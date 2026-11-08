package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/approve"
	"github.com/kinginu/mado/internal/proto"
)

// The test binary doubles as a fake `claude` and as the approve-tool MCP server.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "approve-tool" {
		sock := os.Args[3]
		if err := approve.Serve(context.Background(), os.Stdin, os.Stdout, approve.SocketDecider(sock)); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("MADO_FAKE") == "claude" {
		os.Exit(fakeClaude())
	}
	if os.Getenv("MADO_FAKE") == "control" {
		os.Exit(fakeControl())
	}
	os.Exit(m.Run())
}

// fakeClaude replays the recorded fixture. Between the tool_use and the tool_result it
// asks the --permission-prompt-tool server, exactly like the real CLI.
func fakeClaude() int {
	fixture, err := os.ReadFile(os.Getenv("MADO_FIXTURE"))
	if err != nil {
		return 2
	}
	var lines []string
	for _, l := range strings.Split(string(fixture), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	var mcpCfg, promptTool string
	for i, a := range os.Args {
		if a == "--mcp-config" && i+1 < len(os.Args) {
			mcpCfg = os.Args[i+1]
		}
		if a == "--permission-prompt-tool" && i+1 < len(os.Args) {
			promptTool = os.Args[i+1]
		}
	}
	if promptTool != approve.PromptTool {
		fmt.Println(`{"type":"result","subtype":"error","is_error":true,"result":"no prompt tool"}`)
		return 1
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(mcpCfg), &cfg); err != nil {
		return 3
	}
	srv := cfg.MCPServers["mado"]
	mcp := exec.Command(srv.Command, srv.Args...)
	min, _ := mcp.StdinPipe()
	mout, _ := mcp.StdoutPipe()
	if err := mcp.Start(); err != nil {
		return 4
	}
	mr := bufio.NewScanner(mout)
	call := func(id int, method string, params any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		min.Write(append(b, '\n'))
		for mr.Scan() {
			var m map[string]any
			json.Unmarshal(mr.Bytes(), &m)
			if m["id"] == float64(id) {
				return m
			}
		}
		return nil
	}
	call(0, "initialize", map[string]any{"protocolVersion": "2025-11-25"})
	call(1, "tools/list", map[string]any{})

	in := bufio.NewScanner(os.Stdin)
	first := true
	id := 2
	for in.Scan() {
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil || msg.Type != "user" {
			fmt.Println(`{"type":"result","subtype":"error_bad_input","is_error":true}`)
			continue
		}
		if first {
			fmt.Println(lines[0])
			first = false
		}
		fmt.Println(lines[1]) // assistant tool_use Write
		var tu struct {
			Message struct {
				Content []struct {
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		json.Unmarshal([]byte(lines[1]), &tu)
		c := tu.Message.Content[0]
		resp := call(id, "tools/call", map[string]any{"name": "approve",
			"arguments": map[string]any{"tool_name": c.Name, "input": c.Input, "tool_use_id": c.ID}})
		id++
		text := resp["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		var dec struct {
			Behavior, Message string
			UpdatedInput      json.RawMessage `json:"updatedInput"`
		}
		json.Unmarshal([]byte(text), &dec)
		if dec.Behavior == "allow" {
			fmt.Fprintln(os.Stderr, "fake claude: updatedInput "+string(dec.UpdatedInput))
			fmt.Println(lines[2])
		} else {
			b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user",
				"content": []any{map[string]any{"type": "tool_result", "tool_use_id": c.ID, "is_error": true, "content": dec.Message}}}})
			fmt.Println(string(b))
		}
		fmt.Println(lines[3]) // rate limit event
		fmt.Println(lines[4]) // assistant "done"
		fmt.Println(lines[5]) // result
		fmt.Fprintln(os.Stderr, "fake claude: turn finished")
	}
	min.Close()
	mcp.Wait()
	return 0
}

type daemon struct {
	t  *testing.T
	lc *proto.LineConn
	c  net.Conn
	ch chan proto.Local
}

func dialDaemon(t *testing.T, sock string, from int64) *daemon {
	t.Helper()
	var c net.Conn
	var err error
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{t: t, lc: proto.NewLineConn(c), c: c, ch: make(chan proto.Local, 100)}
	if err := d.lc.Write(proto.Local{T: proto.LHello, Role: "daemon", FromSeq: from}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			var m proto.Local
			if d.lc.Read(&m) != nil {
				close(d.ch)
				return
			}
			d.ch <- m
		}
	}()
	t.Cleanup(func() { c.Close() })
	return d
}

// until reads frames until pred matches; it returns all frames read.
func (d *daemon) until(what string, pred func(proto.Local) bool) []proto.Local {
	d.t.Helper()
	var got []proto.Local
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-d.ch:
			if !ok {
				d.t.Fatalf("connection closed waiting for %s; got %+v", what, got)
			}
			got = append(got, m)
			if pred(m) {
				return got
			}
		case <-timeout:
			d.t.Fatalf("timeout waiting for %s; got %+v", what, got)
		}
	}
}

func rawType(m proto.Local) string {
	var e struct {
		Type string `json:"type"`
	}
	json.Unmarshal(m.Raw, &e)
	return e.Type
}

func TestShimEndToEndWithFakeClaude(t *testing.T) {
	dir := sockDir(t)
	sid := proto.NewID()
	sock, spoolPath := SockPath(dir, sid), SpoolPath(dir, sid)
	fixture, _ := filepath.Abs("../render/testdata/write_tool.jsonl")
	self, _ := os.Executable()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tty := &syncWriter{b: &strings.Builder{}}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{SessionID: sid, RuntimeDir: dir, TTY: tty, Linger: 5 * time.Second})
	}()

	d := dialDaemon(t, sock, 0)
	d.until("starting state", func(m proto.Local) bool { return m.T == proto.LState && m.State == proto.StateStarting })
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--strict-mcp-config", "--mcp-config", approve.MCPConfig([]string{self, "approve-tool", "--socket", sock}),
		"--permission-prompt-tool", approve.PromptTool}
	if err := d.lc.Write(proto.Local{T: proto.LStart, Spawn: &proto.Spawn{ClaudeBin: self, CWD: dir, Args: args,
		Env: map[string]string{"MADO_FAKE": "claude", "MADO_FIXTURE": fixture}}}); err != nil {
		t.Fatal(err)
	}
	d.until("idle", func(m proto.Local) bool { return m.T == proto.LState && m.State == proto.StateIdle })

	// Turn 1: allowed.
	d.lc.Write(proto.Local{T: proto.LUserMessage, Text: "create probe.txt"})
	got := d.until("approval", func(m proto.Local) bool { return m.T == proto.LApprovalRequest })
	req := got[len(got)-1]
	if req.ToolName != "Write" || !strings.Contains(string(req.Input), "probe.txt") || req.ToolUseID == "" {
		t.Fatalf("approval request: %+v", req)
	}
	// The updated input (DESIGN §21.1) reaches claude in place of its own.
	d.lc.Write(proto.Local{T: proto.LApprovalResponse, RequestID: req.RequestID, Decision: proto.DecisionAllow,
		UpdatedInput: json.RawMessage(`{"file_path":"/x/answered.txt"}`)})
	got = append(got, d.until("result", func(m proto.Local) bool { return m.T == proto.LEvent && rawType(m) == "result" })...)
	updatedSeen := false
	var types []string
	var claudeSID string
	for _, m := range got {
		if m.T == proto.LEvent && rawType(m) != "mado_stderr" {
			types = append(types, rawType(m))
		}
		if m.T == proto.LEvent && strings.Contains(string(m.Raw), `updatedInput {\"file_path\":\"/x/answered.txt\"}`) {
			updatedSeen = true
		}
		if m.T == proto.LState && m.ClaudeSessionID != "" {
			claudeSID = m.ClaudeSessionID
		}
	}
	if strings.Join(types, ",") != "user,system,assistant,user,rate_limit_event,assistant,result" {
		t.Fatalf("event order: %v", types)
	}
	if !updatedSeen {
		t.Errorf("the updated input did not reach claude: %+v", got)
	}
	if claudeSID != "ef4d08a3-0fb3-45bf-927f-28034415014c" {
		t.Errorf("claude session id not tracked: %q", claudeSID)
	}
	d.until("idle after result", func(m proto.Local) bool { return m.T == proto.LState && m.State == proto.StateIdle })

	// Turn 2: denied; the deny message must reach claude.
	d.lc.Write(proto.Local{T: proto.LUserMessage, Text: "again"})
	got = d.until("approval 2", func(m proto.Local) bool { return m.T == proto.LApprovalRequest })
	d.lc.Write(proto.Local{T: proto.LApprovalResponse, RequestID: got[len(got)-1].RequestID, Decision: proto.DecisionDeny, Message: "not now"})
	got = d.until("result 2", func(m proto.Local) bool { return m.T == proto.LEvent && rawType(m) == "result" })
	deniedSeen := false
	for _, m := range got {
		if m.T == proto.LEvent && strings.Contains(string(m.Raw), `"not now"`) && strings.Contains(string(m.Raw), `"is_error":true`) {
			deniedSeen = true
		}
	}
	if !deniedSeen {
		t.Fatalf("deny message did not reach claude: %+v", got)
	}

	// A second daemon connection resyncs from seq 3.
	d2 := dialDaemon(t, sock, 3)
	got = d2.until("seq 4", func(m proto.Local) bool { return m.T == proto.LEvent && m.Seq == 4 })
	for _, m := range got {
		if m.T == proto.LEvent && m.Seq <= 3 {
			t.Fatalf("resync sent seq %d", m.Seq)
		}
	}
	d2.c.Close()

	// Stop: stdin closes, claude exits 0, the shim exits once the daemon caught up.
	d.lc.Write(proto.Local{T: proto.LStop})
	got = d.until("exited", func(m proto.Local) bool { return m.T == proto.LState && m.State == proto.StateExited })
	if ec := got[len(got)-1].ExitCode; ec == nil || *ec != 0 {
		t.Fatalf("exit code %v", ec)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shim did not exit")
	}

	entries, err := ReadSpool(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range entries {
		if e.Seq != int64(i+1) || !json.Valid(e.Raw) {
			t.Fatalf("spool entry %d: %+v", i, e)
		}
	}
	if len(entries) < 14 {
		t.Fatalf("spool too short: %d", len(entries))
	}
	if fi, _ := os.Stat(spoolPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("spool mode %v", fi.Mode())
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket not removed")
	}
	out := tty.String()
	for _, s := range []string{"⚙ Write", "? approval Write", "turn done", "fake claude: turn finished"} {
		if !strings.Contains(out, s) {
			t.Errorf("tty lacks %q", s)
		}
	}
}

func TestSpoolSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	sid := proto.NewID()
	spoolPath := SpoolPath(dir, sid)
	os.WriteFile(spoolPath, []byte(`{"seq":1,"ts":"2026-10-18T00:00:00Z","raw":{"type":"a"}}
{"seq":2,"ts":"2026-10-18T00:00:00Z","raw":{"type":"b"}}
{"seq":3,"ts":"2026-10-18T00:00:0`), 0o600) // torn tail
	s := &shim{}
	if err := s.loadSpool(spoolPath); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 2 || s.lastSeqLocked() != 2 {
		t.Fatalf("%+v", s.events)
	}
}

func TestChildEnvStripsCredentials(t *testing.T) {
	env := childEnv([]string{"PATH=/bin", "ANTHROPIC_API_KEY=x", "CLAUDE_CONFIG_DIR=/a", "HOME=/h"},
		map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:1"})
	joined := strings.Join(env, " ")
	if strings.Contains(joined, "ANTHROPIC_API_KEY") || strings.Contains(joined, "CLAUDE_CONFIG_DIR") ||
		!strings.Contains(joined, "ANTHROPIC_BASE_URL=http://127.0.0.1:1") || !strings.Contains(joined, "PATH=/bin") {
		t.Fatal(joined)
	}
}

func TestRejectsBadSessionID(t *testing.T) {
	if err := Run(context.Background(), Config{SessionID: "../../etc", RuntimeDir: t.TempDir()}); err == nil {
		t.Fatal("accepted")
	}
}

func writeSpool(t *testing.T, path string, n int) {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, `{"seq":%d,"ts":"2026-10-18T00:00:00Z","raw":{"type":"e%d"}}`+"\n", i, i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func dialHold(t *testing.T, sock string) *daemon {
	t.Helper()
	var c net.Conn
	var err error
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{t: t, lc: proto.NewLineConn(c), c: c, ch: make(chan proto.Local, 100)}
	d.lc.Write(proto.Local{T: proto.LHello, Role: "daemon", Hold: true})
	go func() {
		for {
			var m proto.Local
			if d.lc.Read(&m) != nil {
				close(d.ch)
				return
			}
			d.ch <- m
		}
	}()
	t.Cleanup(func() { c.Close() })
	return d
}

// A restarted daemon attaches with hold: nothing but control frames until the hub's
// resync arrives as a replay, which then starts exactly after from_seq.
func TestHoldUntilReplay(t *testing.T) {
	dir := sockDir(t)
	sid := proto.NewID()
	writeSpool(t, SpoolPath(dir, sid), 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Config{SessionID: sid, RuntimeDir: dir})
	d := dialHold(t, SockPath(dir, sid))
	d.until("state", func(m proto.Local) bool { return m.T == proto.LState })
	select {
	case m := <-d.ch:
		t.Fatalf("held daemon got %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
	d.lc.Write(proto.Local{T: proto.LReplay, FromSeq: 3})
	got := d.until("seq 5", func(m proto.Local) bool { return m.T == proto.LEvent && m.Seq == 5 })
	if len(got) != 2 || got[0].Seq != 4 {
		t.Fatalf("replay sent %+v", got)
	}
}

// startCat runs /bin/cat as a stand-in claude (stdin open, dies on SIGINT).
func startCat(t *testing.T, cfg Config) (*daemon, context.CancelCauseFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	d := dialDaemon(t, SockPath(cfg.RuntimeDir, cfg.SessionID), 0)
	d.lc.Write(proto.Local{T: proto.LStart, Spawn: &proto.Spawn{ClaudeBin: "/bin/cat", CWD: cfg.RuntimeDir, RouterToken: "tok-123"}})
	d.until("idle", func(m proto.Local) bool { return m.T == proto.LState && m.State == proto.StateIdle })
	return d, cancel, done
}

func TestTokenPersistedAndRemoved(t *testing.T) {
	dir, spool := sockDir(t), t.TempDir()
	sid := proto.NewID()
	d, cancel, done := startCat(t, Config{SessionID: sid, RuntimeDir: dir, SpoolDir: spool})
	if ReadToken(spool, sid) != "tok-123" {
		t.Fatal("token not persisted")
	}
	if fi, _ := os.Stat(TokenPath(spool, sid)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %v", fi.Mode())
	}
	if _, err := os.Stat(SpoolPath(spool, sid)); err != nil {
		t.Fatal("spool not in spool dir")
	}
	os.Chmod(TokenPath(spool, sid), 0o644)
	if ReadToken(spool, sid) != "" {
		t.Fatal("a group-readable token file was trusted")
	}
	d.lc.Write(proto.Local{T: proto.LStop})
	d.until("exited", func(m proto.Local) bool { return m.T == proto.LState && m.State == proto.StateExited })
	cancel(nil)
	<-done
	if _, err := os.Stat(TokenPath(spool, sid)); !os.IsNotExist(err) {
		t.Fatal("token file left behind")
	}
}

func TestDetachReportsDetached(t *testing.T) {
	dir := sockDir(t)
	d, cancel, done := startCat(t, Config{SessionID: proto.NewID(), RuntimeDir: dir})
	cancel(ErrDetach)
	got := d.until("detached", func(m proto.Local) bool { return m.T == proto.LState && m.State == proto.StateDetached })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got = append(got, d.drain()...)
	for _, m := range got {
		if m.T == proto.LState && m.State == proto.StateExited {
			t.Fatalf("detach reported exited: %+v", got)
		}
	}
}

func TestLostReportsNothing(t *testing.T) {
	dir := sockDir(t)
	d, cancel, done := startCat(t, Config{SessionID: proto.NewID(), RuntimeDir: dir})
	cancel(ErrLost)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, m := range d.drain() {
		if m.T == proto.LState && (m.State == proto.StateExited || m.State == proto.StateDetached) {
			t.Fatalf("lost shim reported %s", m.State)
		}
	}
}

func TestSeqBaseAndResume(t *testing.T) {
	dir := sockDir(t)
	sid, resume := proto.NewID(), "ef4d08a3-0fb3-45bf-927f-28034415014c"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Config{SessionID: sid, RuntimeDir: dir, SeqBase: 41, ResumeID: resume})
	d := dialDaemon(t, SockPath(dir, sid), 41)
	got := d.until("state", func(m proto.Local) bool { return m.T == proto.LState })
	if got[len(got)-1].ClaudeSessionID != resume {
		t.Fatalf("resume id not reported before claude speaks: %+v", got)
	}
	d.lc.Write(proto.Local{T: proto.LStart, Spawn: &proto.Spawn{ClaudeBin: "/bin/cat", CWD: dir}})
	d.lc.Write(proto.Local{T: proto.LUserMessage, Text: "x"})
	ev := d.until("event", func(m proto.Local) bool { return m.T == proto.LEvent })
	if e := ev[len(ev)-1]; e.Seq != 42 {
		t.Fatalf("first event after seq base 41 has seq %d", e.Seq)
	}
	if err := Run(context.Background(), Config{SessionID: proto.NewID(), RuntimeDir: dir, ResumeID: "--x"}); err == nil {
		t.Fatal("bad resume id accepted")
	}
}

func TestResumeArgs(t *testing.T) {
	id := "ef4d08a3-0fb3-45bf-927f-28034415014c"
	if a, _ := resumeArgs([]string{"-p"}, id); strings.Join(a, " ") != "-p --resume "+id {
		t.Fatal(a)
	}
	if a, _ := resumeArgs([]string{"-p", "--resume", id}, id); len(a) != 3 {
		t.Fatal(a)
	}
	if _, err := resumeArgs([]string{"--resume", "00000000-0000-4000-8000-000000000000"}, id); err == nil {
		t.Fatal("mismatched --resume accepted")
	}
	if _, err := resumeArgs([]string{"--resume", id}, ""); err == nil {
		t.Fatal("--resume without the shim flag accepted")
	}
}

// drain returns what arrives within half a second.
func (d *daemon) drain() []proto.Local {
	var got []proto.Local
	for {
		select {
		case m, ok := <-d.ch:
			if !ok {
				return got
			}
			got = append(got, m)
		case <-time.After(500 * time.Millisecond):
			return got
		}
	}
}

// A hub tool call from the MCP server goes to the daemon as a control frame and the
// daemon's answer comes back to the caller (DESIGN §17.12).
func TestToolCallRelay(t *testing.T) {
	dir := sockDir(t)
	sid := proto.NewID()
	d, cancel, done := startCat(t, Config{SessionID: sid, RuntimeDir: dir})
	defer func() { cancel(nil); <-done }()
	sock := SockPath(dir, sid)
	type answer struct {
		text  string
		isErr bool
	}
	got := make(chan answer, 1)
	go func() {
		text, isErr := approve.SocketToolCaller(sock, 5*time.Second)(context.Background(), "web_search", json.RawMessage(`{"query":"otters"}`))
		got <- answer{text, isErr}
	}()
	frames := d.until("tool_call", func(m proto.Local) bool { return m.T == proto.LToolCall })
	call := frames[len(frames)-1]
	if call.ToolName != "web_search" || string(call.Input) != `{"query":"otters"}` || call.CallID == "" {
		t.Fatalf("call %+v", call)
	}
	d.lc.Write(proto.Local{T: proto.LToolResult, CallID: "someone-else", Text: "wrong"})
	d.lc.Write(proto.Local{T: proto.LToolResult, CallID: call.CallID, Text: "1. Sea otter", IsError: false})
	if a := <-got; a.text != "1. Sea otter" || a.isErr {
		t.Fatalf("answer %+v", a)
	}
	// No answer: the caller gives claude an error result after its timeout.
	text, isErr := approve.SocketToolCaller(sock, 200*time.Millisecond)(context.Background(), "fetch_url", nil)
	if !isErr || !strings.Contains(text, "did not answer within 200ms") {
		t.Fatalf("timeout answer %q %v", text, isErr)
	}
}

// sockDir is a temp dir short enough for unix socket paths: under /tmp when it
// exists, since t.TempDir under the default macOS TMPDIR exceeds the 103-byte limit.
func sockDir(t *testing.T) string {
	base := ""
	if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
		base = "/tmp"
	}
	d, err := os.MkdirTemp(base, "ms")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}
