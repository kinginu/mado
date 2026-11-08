package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/shim"
)

// M16 (DESIGN §23.6): job sessions.

func jobFrame(dir string) *proto.StartSession {
	return &proto.StartSession{SessionID: proto.NewID(), CWD: dir,
		Profile: proto.Profile{Name: "subscription", Kind: proto.KindSubscription}, PermissionMode: "default",
		Tools: []string{"propose_task", "web_search"},
		Job:   &proto.Job{Kind: proto.JobDigest, MainModel: "sonnet", System: "You are mado's inbox summarizer."}}
}

func TestBuildSpawnJob(t *testing.T) {
	jobDir, root := t.TempDir(), t.TempDir()
	p := SpawnParams{ClaudeBin: "/c", ShimBin: "/oc", SocketPath: "/run/x.sock", ClaudeConfigBase: t.TempDir(), JobDir: jobDir}
	sp, err := BuildSpawn(jobFrame(jobDir), []string{root}, p)
	if err != nil {
		t.Fatal(err)
	}
	a := sp.Args
	joined := strings.Join(a, " ")
	i := slices.Index(a, "--tools")
	if i < 0 || a[i+1] != "" {
		t.Fatalf(`no --tools "": %q`, a)
	}
	for _, flag := range []string{"--agents", "--settings", "--forward-subagent-text", "--resume"} {
		if slices.Contains(a, flag) {
			t.Errorf("job argv has %s: %q", flag, a)
		}
	}
	want := map[string]string{"--model": "sonnet", "--permission-mode": "default", "--allowedTools": "mcp__mado__propose_task",
		"--max-turns": "12", "--append-system-prompt": "You are mado's inbox summarizer.", "--permission-prompt-tool": "mcp__mado__approve"}
	for k, v := range want {
		if got, _ := argAfter(a, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if !strings.Contains(joined, `"args":["approve-tool","--socket","/run/x.sock","--tools","propose_task"]`) || strings.Contains(joined, "web_search") {
		t.Errorf("mcp config: %s", joined)
	}
	if len(sp.Env) != 0 {
		t.Errorf("job env %v (the subscription login needs the default config dir)", sp.Env)
	}
	if real, _ := filepath.EvalSymlinks(jobDir); sp.CWD != real {
		t.Errorf("cwd %s", sp.CWD)
	}

	// A model from the frame, a default, and every refusal.
	f := jobFrame(jobDir)
	f.Job.MainModel, f.MainModel = "", ""
	if sp, err := BuildSpawn(f, nil, p); err != nil || func() string { v, _ := argAfter(sp.Args, "--model"); return v }() != "sonnet" {
		t.Fatalf("default model: %v", err)
	}
	bad := map[string]func(*proto.StartSession, *SpawnParams){
		"jobs off":      func(f *proto.StartSession, p *SpawnParams) { p.JobDir = "" },
		"cwd in a root": func(f *proto.StartSession, p *SpawnParams) { f.CWD = root },
		"cwd below": func(f *proto.StartSession, p *SpawnParams) {
			os.Mkdir(filepath.Join(jobDir, "x"), 0o700)
			f.CWD = filepath.Join(jobDir, "x")
		},
		"router profile":  func(f *proto.StartSession, p *SpawnParams) { f.Profile.Kind = proto.KindRouter },
		"acceptEdits":     func(f *proto.StartSession, p *SpawnParams) { f.PermissionMode = "acceptEdits" },
		"empty system":    func(f *proto.StartSession, p *SpawnParams) { f.Job.System = " " },
		"nul in system":   func(f *proto.StartSession, p *SpawnParams) { f.Job.System = "a\x00b" },
		"resume":          func(f *proto.StartSession, p *SpawnParams) { f.ResumeClaudeSessionID = proto.NewID() },
		"extra args":      func(f *proto.StartSession, p *SpawnParams) { f.ExtraArgs = []string{"--max-turns", "99"} },
		"other kind":      func(f *proto.StartSession, p *SpawnParams) { f.Job.Kind = "shell" },
		"bad model":       func(f *proto.StartSession, p *SpawnParams) { f.Job.MainModel = "oc-strong" },
		"model injection": func(f *proto.StartSession, p *SpawnParams) { f.Job.MainModel = "--dangerously-skip-permissions" },
	}
	for name, mut := range bad {
		f, q := jobFrame(jobDir), p
		mut(f, &q)
		if _, err := BuildSpawn(f, []string{root}, q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A code session never gets propose_task, even if a hub asks.
	cf := startFrame(root, proto.Profile{Name: "subscription", Kind: proto.KindSubscription})
	cf.Tools = []string{"propose_task"}
	sp, err = BuildSpawn(cf, []string{root}, p)
	if err != nil || strings.Contains(strings.Join(sp.Args, " "), "propose_task") {
		t.Fatalf("code session with propose_task: %v %q", err, sp.Args)
	}
}

func TestJobsConfig(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	p := filepath.Join(home, "worker.toml")
	os.WriteFile(p, []byte("hub_url = \"ws://h/ws/worker\"\nmachine_name = \"m\"\nproject_roots = [\"/tmp\"]\nclaude_bin = \"/usr/bin/true\"\njobs = true\n"), 0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Jobs || c.JobDir != filepath.Join(home, ".local/state/mado/jobs") {
		t.Fatalf("jobs %v job_dir %s", c.Jobs, c.JobDir)
	}
	if c.JobsReady("linux") {
		t.Error("jobs ready without credentials on linux")
	}
	if !c.JobsReady("darwin") {
		t.Error("jobs not ready on darwin (keychain)")
	}
	os.MkdirAll(c.ClaudeHome, 0o700)
	os.WriteFile(filepath.Join(c.ClaudeHome, ".credentials.json"), []byte("{}"), 0o600)
	if !c.JobsReady("linux") {
		t.Error("jobs not ready with credentials")
	}
	c.Jobs = false
	if c.JobsReady("darwin") || c.JobsReady("linux") {
		t.Error("jobs ready with jobs = false")
	}
}

// fakeJobClaude is a `claude -p` for a digest job: it checks the job argv, lists the
// mado MCP server's tools, proposes a task for every [#id] in the records through
// it, then answers with the digest. MADO_JOB_MODE=limit behaves like the usage
// limit: api_retry, then exit 2 without a result.
func fakeJobClaude() int {
	args := os.Args[1:]
	argAt := func(flag string) (string, bool) {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1], true
			}
		}
		return "", false
	}
	emit := func(v map[string]any) {
		v["session_id"] = "0b5c3c1e-5a4f-4c8e-9a51-1c1f3f2c9d10"
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	if v, ok := argAt("--tools"); !ok || v != "" || slices.Contains(args, "--agents") {
		emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "not a job argv"})
		return 1
	}
	cfgJSON, _ := argAt("--mcp-config")
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal([]byte(cfgJSON), &cfg) != nil {
		return 3
	}
	srv := cfg.MCPServers["mado"]
	mcp := exec.Command(srv.Command, srv.Args...)
	min, _ := mcp.StdinPipe()
	mout, _ := mcp.StdoutPipe()
	if err := mcp.Start(); err != nil {
		return 4
	}
	defer func() { min.Close(); mcp.Wait() }()
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
	list := call(1, "tools/list", map[string]any{})
	lb, _ := json.Marshal(list)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64*1024), 16<<20)
	for in.Scan() {
		var msg struct {
			Message struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil || len(msg.Message.Content) == 0 {
			continue
		}
		text := msg.Message.Content[0].Text
		model, _ := argAt("--model")
		emit(map[string]any{"type": "system", "subtype": "init", "model": model, "tools": []string{"mcp__mado__propose_task"}})
		if os.Getenv("MADO_JOB_MODE") == "limit" {
			emit(map[string]any{"type": "system", "subtype": "api_retry", "attempt": 1, "error_status": 429})
			return 2
		}
		if !strings.Contains(string(lb), `"propose_task"`) || !strings.Contains(string(lb), `"item_ids"`) {
			emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "no propose_task: " + string(lb)})
			continue
		}
		var ids []int64
		for _, m := range regexp.MustCompile(`\[#(\d+)\]`).FindAllStringSubmatch(text, -1) {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			ids = append(ids, n)
		}
		input := map[string]any{"title": "Review PR #42", "body": "carol asked", "item_ids": ids}
		emit(map[string]any{"type": "assistant", "message": map[string]any{"id": "msg_1", "model": "claude-sonnet-4-5", "role": "assistant",
			"content": []any{map[string]any{"type": "tool_use", "id": "tu_1", "name": "mcp__mado__propose_task", "input": input}},
			"usage":   map[string]any{"input_tokens": 100, "output_tokens": 0}}})
		resp := call(2, "tools/call", map[string]any{"name": "propose_task", "arguments": input})
		res, _ := resp["result"].(map[string]any)
		content, _ := res["content"].([]any)
		toolText := ""
		if len(content) > 0 {
			toolText, _ = content[0].(map[string]any)["text"].(string)
		}
		emit(map[string]any{"type": "user", "message": map[string]any{"role": "user",
			"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": toolText}}}})
		digest := fmt.Sprintf("## angel\n- carol asks for a review %v (%s)", ids, toolText)
		emit(map[string]any{"type": "assistant", "message": map[string]any{"id": "msg_2", "model": "claude-sonnet-4-5", "role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": digest}}, "usage": map[string]any{"input_tokens": 150, "output_tokens": 0}}})
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": digest, "num_turns": 2,
			"modelUsage": map[string]any{"claude-sonnet-4-5": map[string]any{"inputTokens": 250, "outputTokens": 80, "cacheReadInputTokens": 0, "cacheCreationInputTokens": 0}}})
	}
	return 0
}

// TestJobThroughShim runs the real job argv in a real shim with the fake claude:
// the propose_task call goes claude → MCP server → shim → daemon (here the test) and
// the answer comes back into the digest; stop ends claude with code 0. With the
// usage-limit mode claude exits 2 without a result.
func TestJobThroughShim(t *testing.T) {
	for _, mode := range []string{"ok", "limit"} {
		t.Run(mode, func(t *testing.T) {
			dir, err := os.MkdirTemp(shortTmp(), "mj")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			jobDir := filepath.Join(dir, "jobs")
			os.Mkdir(jobDir, 0o700)
			self, _ := os.Executable()
			sid := proto.NewID()
			sock := shim.SockPath(dir, sid)
			f := jobFrame(jobDir)
			f.SessionID = sid
			sp, err := BuildSpawn(f, nil, SpawnParams{ClaudeBin: self, ShimBin: self, SocketPath: sock, JobDir: jobDir})
			if err != nil {
				t.Fatal(err)
			}
			sp.Env["MADO_FAKE"] = "jobclaude"
			sp.Env["MADO_JOB_MODE"] = mode
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- shim.Run(ctx, shim.Config{SessionID: sid, RuntimeDir: dir, TTY: discard{}, Linger: 5 * time.Second})
			}()
			var c net.Conn
			for i := 0; i < 100; i++ {
				if c, err = net.Dial("unix", sock); err == nil {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			lc := proto.NewLineConn(c)
			lc.Write(proto.Local{T: proto.LHello, Role: "daemon"})
			ch := make(chan proto.Local, 100)
			go func() {
				for {
					var m proto.Local
					if lc.Read(&m) != nil {
						close(ch)
						return
					}
					ch <- m
				}
			}()
			until := func(what string, pred func(proto.Local) bool) proto.Local {
				t.Helper()
				timeout := time.After(15 * time.Second)
				for {
					select {
					case m, ok := <-ch:
						if !ok {
							t.Fatalf("closed waiting for %s", what)
						}
						if pred(m) {
							return m
						}
					case <-timeout:
						t.Fatalf("timeout waiting for %s", what)
					}
				}
			}
			lc.Write(proto.Local{T: proto.LStart, Spawn: sp})
			until("idle", func(m proto.Local) bool { return m.T == proto.LState && m.State == proto.StateIdle })
			lc.Write(proto.Local{T: proto.LUserMessage, Text: "<inbox-items>\n[#7] review please\n[#9] fyi\n</inbox-items>"})
			if mode == "limit" {
				m := until("exited", func(m proto.Local) bool { return m.T == proto.LState && m.State == proto.StateExited })
				if m.ExitCode == nil || *m.ExitCode != 2 {
					t.Fatalf("exit %v", m.ExitCode)
				}
				return
			}
			call := until("tool_call", func(m proto.Local) bool { return m.T == proto.LToolCall })
			if call.ToolName != "propose_task" || !strings.Contains(string(call.Input), `"item_ids":[7,9]`) {
				t.Fatalf("tool call %+v %s", call, call.Input)
			}
			lc.Write(proto.Local{T: proto.LToolResult, CallID: call.CallID, Text: "Task #1 proposed to the user."})
			res := until("result", func(m proto.Local) bool {
				return m.T == proto.LEvent && strings.Contains(string(m.Raw), `"type":"result"`)
			})
			var r struct {
				Result  string `json:"result"`
				IsError bool   `json:"is_error"`
			}
			json.Unmarshal(res.Raw, &r)
			if r.IsError || !strings.Contains(r.Result, "Task #1 proposed") || !strings.Contains(r.Result, "[7 9]") {
				t.Fatalf("result %s", res.Raw)
			}
			lc.Write(proto.Local{T: proto.LStop})
			m := until("exited", func(m proto.Local) bool { return m.T == proto.LState && m.State == proto.StateExited })
			if m.ExitCode == nil || *m.ExitCode != 0 {
				t.Fatalf("exit %v", m.ExitCode)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("shim did not end")
			}
		})
	}
}

type discard struct{}

func (discard) Write(b []byte) (int, error) { return len(b), nil }
