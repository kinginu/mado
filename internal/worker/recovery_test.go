package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/approve"
	"github.com/kinginu/mado/internal/hub"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/shim"
)

// The test binary doubles as `mado shim`, as the approve-tool and as a fake claude.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "shim" {
		if err := shim.Main(os.Args[2:], slog.New(slog.NewTextHandler(os.Stderr, nil))); err != nil {
			fmt.Fprintln(os.Stderr, "shim:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "approve-tool" {
		o := approve.Options{Decide: approve.SocketDecider(os.Args[3])}
		if len(os.Args) > 5 && os.Args[4] == "--tools" {
			o.Tools = strings.Split(os.Args[5], ",")
			o.Call = approve.SocketToolCaller(os.Args[3], approve.ToolTimeout)
		}
		_ = approve.ServeWith(context.Background(), os.Stdin, os.Stdout, o)
		os.Exit(0)
	}
	if os.Getenv("MADO_FAKE") == "claude" {
		os.Exit(fakeClaude())
	}
	if os.Getenv("MADO_FAKE") == "jobclaude" {
		os.Exit(fakeJobClaude())
	}
	os.Exit(m.Run())
}

// fakeClaude speaks stream-json. Each user message becomes one turn that asks the
// router (with the session's bearer, retrying like the real CLI does on 5xx and
// connection errors) and answers "reply to <text>: <upstream text>". A message that
// starts with "slow" waits 1.5 s first, so a test can restart the daemon mid-turn.
func fakeClaude() int {
	var resume, model string
	for i, a := range os.Args {
		if a == "--version" {
			fmt.Println("9.9.9 (fake claude)")
			return 0
		}
		if a == "--resume" && i+1 < len(os.Args) {
			resume = os.Args[i+1]
		}
		if a == "--model" && i+1 < len(os.Args) {
			model = os.Args[i+1]
		}
	}
	if f, err := os.OpenFile(os.Getenv("MADO_FAKE_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, strings.ReplaceAll(strings.Join(os.Args[1:], " "), "\n", `\n`)) // one line per start
		f.Close()
	}
	sid := resume
	if sid == "" {
		sid = proto.NewID()
	}
	emit := func(v map[string]any) {
		v["session_id"] = sid
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	in := bufio.NewScanner(os.Stdin)
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
		emit(map[string]any{"type": "system", "subtype": "init", "model": model, "resumed": resume != "",
			"ctx": os.Getenv("CLAUDE_CODE_MAX_CONTEXT_TOKENS")})
		if strings.HasPrefix(text, "slow") {
			time.Sleep(1500 * time.Millisecond)
		}
		reply := askRouter(model, text)
		emit(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": "reply to " + text + ": " + reply}}}})
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": reply})
	}
	return 0
}

func askRouter(model, text string) string {
	base := os.Getenv("ANTHROPIC_BASE_URL")
	if base == "" {
		return "no router"
	}
	body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 16,
		"messages": []any{map[string]any{"role": "user", "content": text}}})
	for i := 0; i < 150; i++ {
		req, _ := http.NewRequest("POST", base+"/v1/messages", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+os.Getenv("ANTHROPIC_AUTH_TOKEN"))
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				var out struct {
					Content []struct{ Text string } `json:"content"`
				}
				if json.Unmarshal(b, &out) == nil && len(out.Content) > 0 {
					return out.Content[0].Text
				}
			}
			if resp.StatusCode < 500 {
				return fmt.Sprintf("router error %d", resp.StatusCode)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "router unreachable"
}

type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth []string
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Model string }
		json.NewDecoder(r.Body).Decode(&in)
		u.mu.Lock()
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"upstream %s says hi"}]}`, in.Model)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) calls() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.auth...)
}

type rig struct {
	t       *testing.T
	store   *hub.Store
	hubURL  string
	web     *http.Client
	cfg     *Config
	tmux    Tmux
	fakeLog string

	cancel context.CancelFunc
	done   chan struct{}
}

func freePort(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func newRig(t *testing.T, up *upstream) *rig {
	if _, err := os.Stat("/usr/bin/tmux"); err != nil {
		t.Skip("no tmux")
	}
	if os.Getenv("MADO_NO_TMUX_TESTS") != "" {
		t.Skip("MADO_NO_TMUX_TESTS set")
	}
	base, err := os.MkdirTemp("", "mado-it")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	for _, d := range []string{"run", "spool", "proj", "cfg", "tmux"} {
		os.MkdirAll(filepath.Join(base, d), 0o700)
	}
	// Isolate the tmux server: its own socket dir and name, and never the user's config.
	t.Setenv("TMUX_TMPDIR", filepath.Join(base, "tmux"))
	t.Setenv("MADO_FAKE", "claude")
	r := &rig{t: t, fakeLog: filepath.Join(base, "claude-argv.log")}
	t.Setenv("MADO_FAKE_LOG", r.fakeLog)

	store, err := hub.OpenStore(filepath.Join(base, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { handler.ServeHTTP(w, q) }))
	t.Cleanup(srv.Close)
	hcfg := &hub.Config{BaseURL: srv.URL, Listen: "127.0.0.1:0", Profiles: []hub.ProfileConfig{{
		Name: "r", Kind: "router", Main: "oc-a",
		Upstreams: map[string]hub.UpstreamConfig{"oc-a": {BaseURL: up.srv.URL, Model: "org/A", KeyRef: "k", ContextWindow: 65536}},
	}}}
	h, err := hub.New(hcfg, store, &hub.Secrets{Keys: map[string]string{"k": "upstream-key"}},
		hub.Options{LocalLogin: true, ResumeEvery: time.Second, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	handler = h.Handler()
	r.store, r.hubURL = store, srv.URL

	code, _ := store.CreateEnrollCode("box", time.Minute)
	tok, err := store.Enroll("box", code)
	if err != nil {
		t.Fatal(err)
	}
	tokFile := filepath.Join(base, "cfg", "machine.token")
	os.WriteFile(tokFile, []byte(tok), 0o600)
	self, _ := os.Executable()
	r.cfg = &Config{HubURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/worker", MachineName: "box",
		TokenFile: tokFile, ProjectRoots: []string{filepath.Join(base, "proj")}, ClaudeBin: self,
		TmuxSocket: "mado-it", RouterListen: freePort(t), RuntimeDir: filepath.Join(base, "run"),
		SpoolDir: filepath.Join(base, "spool"), ClaudeConfigBase: filepath.Join(base, "cfg"), ShimBin: self}
	if err := r.cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	r.tmux = Tmux{Bin: r.cfg.TmuxBin, Socket: r.cfg.TmuxSocket}
	t.Cleanup(func() {
		r.stopDaemon()
		r.tmux.run(context.Background(), "kill-server")
		// Shims end on the SIGHUP within a few seconds; do not leave them behind.
		for i := 0; i < 200; i++ {
			socks, _ := filepath.Glob(filepath.Join(r.cfg.RuntimeDir, "*.sock"))
			if len(socks) == 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	jar, _ := cookiejar.New(nil)
	r.web = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, err := r.web.Post(srv.URL+"/local-login", "", nil); err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("local login: %v", err)
	}
	return r
}

func (r *rig) startDaemon() {
	r.t.Helper()
	d, err := New(r.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	go func() { d.Run(ctx); close(r.done) }()
	r.waitFor("machine online", func() bool {
		ms, _ := r.store.Machines()
		return len(ms) == 1 && len(ms[0].Projects) > 0 && r.machineOnline()
	})
}

func (r *rig) stopDaemon() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
	r.cancel = nil
	// The hub notices the closed WebSocket.
	r.waitFor("machine offline", func() bool { return !r.machineOnline() })
}

func (r *rig) get(path string) string {
	resp, err := r.web.Get(r.hubURL + path)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// machineOnline reads the machine's state from the JSON API (the root serves the SPA shell).
func (r *rig) machineOnline() bool {
	var v struct {
		Machines []struct {
			Online bool `json:"online"`
		} `json:"machines"`
	}
	if json.Unmarshal([]byte(r.get("/api/machines")), &v) != nil {
		return false
	}
	return len(v.Machines) == 1 && v.Machines[0].Online
}

// api sends a JSON request with the web-session cookie, the CSRF token and the same-origin Origin.
func (r *rig) api(method, path string, body any) (int, []byte) {
	r.t.Helper()
	var me struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal([]byte(r.get("/api/me")), &me); err != nil || me.CSRF == "" {
		r.t.Fatal("no csrf token from /api/me")
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, r.hubURL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", r.hubURL)
	req.Header.Set("X-CSRF-Token", me.CSRF)
	resp, err := r.web.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// newSession creates a code session on the rig's machine and returns its id.
func (r *rig) newSession(machineID, cwd string) string {
	r.t.Helper()
	code, out := r.api("POST", "/api/sessions", map[string]string{
		"kind": "code", "machine": machineID, "cwd": cwd, "profile": "r", "main_model": "oc-a"})
	var v struct {
		ID string `json:"id"`
	}
	json.Unmarshal(out, &v)
	if code != http.StatusCreated || !proto.ValidSessionID(v.ID) {
		r.t.Fatalf("new session: %d %s", code, out)
	}
	return v.ID
}

func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (r *rig) events(sid string) string {
	evs, _ := r.store.Events(sid, 0, 10000)
	var sb strings.Builder
	for _, e := range evs {
		sb.Write(e.Raw)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func (r *rig) state(sid string) string {
	s, err := r.store.Session(sid)
	if err != nil {
		return ""
	}
	return s.State
}

func (r *rig) say(sid, text string) {
	r.t.Helper()
	if code, out := r.api("POST", "/api/sessions/"+sid+"/send", map[string]string{"text": text}); code/100 != 2 {
		r.t.Fatalf("send %q: %d %s", text, code, out)
	}
}

func (r *rig) contiguous(sid string) {
	r.t.Helper()
	if c, m := r.store.ContiguousSeq(sid), r.store.MaxSeq(sid); c != m || m == 0 {
		r.t.Fatalf("events not contiguous: contiguous %d, max %d", c, m)
	}
}

// TestRecovery drives the whole M2 recovery path with a real hub, the real daemon, a
// real tmux server and the fake claude:
//
//	A. the daemon dies mid-turn and restarts: the turn's events still reach the hub,
//	   and the restarted router serves the old claude's token with the refreshed key;
//	B. the tmux server is killed under a running daemon: the window comes back with
//	   `shim --resume <claude session id>`;
//	C. a reboot (daemon down, tmux gone, runtime dir wiped): the next daemon start
//	   recreates the window with --resume.
func TestRecovery(t *testing.T) {
	up := newUpstream(t)
	r := newRig(t, up)
	r.startDaemon()
	ms, _ := r.store.Machines()
	project := r.cfg.ProjectRoots[0]
	sid := r.newSession(ms[0].ID, project)
	r.waitFor("idle", func() bool { return r.state(sid) == proto.StateIdle })
	r.say(sid, "one")
	r.waitFor("turn one", func() bool { return strings.Contains(r.events(sid), "reply to one: upstream org/A says hi") })
	r.waitFor("idle after one", func() bool { return r.state(sid) == proto.StateIdle })
	s, _ := r.store.Session(sid)
	claudeSID := s.ClaudeSessionID
	if claudeSID == "" {
		t.Fatal("no claude session id")
	}
	if tok := shim.ReadToken(r.cfg.SpoolDir, sid); tok == "" {
		t.Fatal("router token not persisted")
	} else if fi, _ := os.Stat(shim.TokenPath(r.cfg.SpoolDir, sid)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", fi.Mode())
	}

	// A: daemon restart mid-turn.
	r.say(sid, "slow two")
	time.Sleep(200 * time.Millisecond)
	r.stopDaemon()
	time.Sleep(300 * time.Millisecond)
	r.startDaemon()
	r.waitFor("turn two after daemon restart", func() bool {
		return strings.Contains(r.events(sid), "reply to slow two: upstream org/A says hi")
	})
	r.waitFor("idle after two", func() bool { return r.state(sid) == proto.StateIdle })
	r.contiguous(sid)
	r.say(sid, "three")
	r.waitFor("turn three", func() bool { return strings.Contains(r.events(sid), "reply to three: upstream") })
	for _, a := range up.calls() {
		if a != "Bearer upstream-key" {
			t.Fatalf("upstream saw %q", a)
		}
	}
	if n := len(up.calls()); n != 3 {
		t.Fatalf("upstream calls %d, want 3", n)
	}
	r.waitFor("idle after three", func() bool { return r.state(sid) == proto.StateIdle })

	// B: tmux server killed under a live daemon.
	before := r.store.MaxSeq(sid)
	if _, err := r.tmux.run(context.Background(), "kill-server"); err != nil {
		t.Fatal(err)
	}
	r.waitFor("window recreated with --resume", func() bool {
		return strings.Contains(r.tmux.StartCommands(context.Background())[WindowName(sid)], "--resume "+claudeSID)
	})
	r.waitFor("idle after resume", func() bool { return r.state(sid) == proto.StateIdle })
	r.say(sid, "four")
	r.waitFor("turn four", func() bool { return strings.Contains(r.events(sid), "reply to four: upstream") })
	if !strings.Contains(r.events(sid), `"resumed":true`) {
		t.Fatal("turn four did not run in a resumed claude")
	}
	if r.store.MaxSeq(sid) <= before {
		t.Fatal("no new events after resume")
	}
	r.contiguous(sid)
	r.waitFor("idle after four", func() bool { return r.state(sid) == proto.StateIdle })

	// C: reboot. The daemon goes first, then tmux and the runtime dir (tmpfs).
	r.stopDaemon()
	r.tmux.run(context.Background(), "kill-server")
	r.waitFor("old shim gone", func() bool {
		_, err := os.Stat(shim.SockPath(r.cfg.RuntimeDir, sid))
		return os.IsNotExist(err)
	})
	os.RemoveAll(r.cfg.RuntimeDir)
	if st := r.state(sid); st != proto.StateIdle {
		t.Fatalf("hub state after the reboot = %q; want idle (resumable)", st)
	}
	r.startDaemon()
	r.waitFor("window recreated after reboot", func() bool {
		return strings.Contains(r.tmux.StartCommands(context.Background())[WindowName(sid)], "--resume "+claudeSID)
	})
	r.waitFor("idle after reboot", func() bool { return r.state(sid) == proto.StateIdle })
	r.say(sid, "five")
	r.waitFor("turn five", func() bool { return strings.Contains(r.events(sid), "reply to five: upstream") })
	r.contiguous(sid)

	argv, _ := os.ReadFile(r.fakeLog)
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	if len(lines) != 3 || strings.Contains(lines[0], "--resume") ||
		!strings.Contains(lines[1], "--resume "+claudeSID) || !strings.Contains(lines[2], "--resume "+claudeSID) {
		t.Fatalf("claude was started %d times:\n%s", len(lines), argv)
	}
	// context_window of the main upstream reached claude's env.
	if !strings.Contains(r.events(sid), `"ctx":"65536"`) {
		t.Fatal("CLAUDE_CODE_MAX_CONTEXT_TOKENS not set from context_window")
	}
}

// A user's Ctrl-C in the tmux window detaches: the hub must not resume it.
func TestDetachIsNotResumed(t *testing.T) {
	up := newUpstream(t)
	r := newRig(t, up)
	r.startDaemon()
	ms, _ := r.store.Machines()
	sid := r.newSession(ms[0].ID, r.cfg.ProjectRoots[0])
	r.waitFor("idle", func() bool { return r.state(sid) == proto.StateIdle })
	r.say(sid, "hi")
	r.waitFor("turn", func() bool { return strings.Contains(r.events(sid), "reply to hi") })
	r.waitFor("idle again", func() bool { return r.state(sid) == proto.StateIdle })
	out, err := r.tmux.run(context.Background(), "list-panes", "-a", "-F", "#{pane_pid}")
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(out))
	if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	r.waitFor("detached", func() bool { return r.state(sid) == proto.StateDetached })
	time.Sleep(1500 * time.Millisecond)
	if st := r.state(sid); st != proto.StateDetached {
		t.Fatalf("state %q after detach", st)
	}
	if strings.Contains(r.tmux.StartCommands(context.Background())[WindowName(sid)], "--resume") {
		t.Fatal("a detached session was resumed")
	}
}
