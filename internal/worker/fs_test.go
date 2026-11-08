package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/hub"
	"github.com/kinginu/mado/internal/proto"
)

// M12 (DESIGN §20): the worker side of the concierge.

// trueBin is true(1): /bin/true on Linux, /usr/bin/true on macOS.
func trueBin(t *testing.T) string {
	p, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1) on PATH")
	}
	return p
}

func realTemp(t *testing.T) string {
	d, err := os.MkdirTemp(shortTmp(), "mado-fs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	r, _ := filepath.EvalSymlinks(d)
	return r
}

func TestResolveInside(t *testing.T) {
	base := realTemp(t)
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	os.MkdirAll(filepath.Join(root, "a"), 0o755)
	os.MkdirAll(outside, 0o755)
	os.Symlink(outside, filepath.Join(root, "escape"))
	os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "inner"))
	os.Symlink(filepath.Join(base, "nowhere"), filepath.Join(root, "dangling"))
	roots := []string{root}
	for p, want := range map[string]string{
		root:                                    root,
		filepath.Join(root, "a"):                filepath.Join(root, "a"),
		filepath.Join(root, "new/deeper"):       filepath.Join(root, "new/deeper"),
		filepath.Join(root, "inner", "x"):       filepath.Join(root, "a", "x"),
		filepath.Join(root, "escape"):           "",
		filepath.Join(root, "escape", "new"):    "",
		filepath.Join(root, "dangling", "new"):  "",
		root + "/../outside":                    "",
		outside:                                 "",
		"relative/path":                         "",
		filepath.Join(root, "a") + "/":          "",
		filepath.Join(base, "rootx", "sibling"): "",
	} {
		got, err := ResolveInside(p, roots)
		if want == "" {
			if err == nil {
				t.Errorf("%s accepted as %s", p, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%s: %s %v (want %s)", p, got, err, want)
		}
	}
}

func needGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
}

// gitRepo makes a repository with one commit on main.
func gitRepo(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\nA small Go tool.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"checkout", "-q", "-b", "main"}, {"add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

func TestPrepareAndInspect(t *testing.T) {
	needGit(t)
	base := realTemp(t)
	root := filepath.Join(base, "root")
	os.MkdirAll(root, 0o755)
	os.MkdirAll(filepath.Join(base, "outside"), 0o755)
	os.Symlink(filepath.Join(base, "outside"), filepath.Join(root, "escape"))
	roots := []string{root}
	ctx := context.Background()
	run := func(m proto.FSPrepare) (string, error) { return Prepare(ctx, &m, roots) }

	if out, err := run(proto.FSPrepare{Action: "mkdir", Path: root + "/p/q"}); err != nil || out != "created" {
		t.Fatalf("mkdir %q %v", out, err)
	}
	if out, err := run(proto.FSPrepare{Action: "mkdir", Path: root + "/p/q"}); err != nil || out != "already exists" {
		t.Errorf("mkdir again %q %v", out, err)
	}
	if _, err := run(proto.FSPrepare{Action: "mkdir", Path: root + "/escape/evil"}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("mkdir through a symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "outside", "evil")); err == nil {
		t.Error("created outside the root")
	}
	if _, err := run(proto.FSPrepare{Action: "mkdir", Path: root + "/../x"}); err == nil {
		t.Error(".. accepted")
	}
	if _, err := run(proto.FSPrepare{Action: "git_init", Path: root + "/p/q"}); err != nil {
		t.Fatalf("git_init %v", err)
	}
	if out, err := run(proto.FSPrepare{Action: "git_init", Path: root + "/p/q"}); err != nil || out != "already a git repository" {
		t.Errorf("git_init again %q %v", out, err)
	}
	// clone: the URL and the target are checked before git runs.
	if _, err := run(proto.FSPrepare{Action: "clone", Path: root + "/c", URL: "file:///etc"}); err == nil || !strings.Contains(err.Error(), "url") {
		t.Errorf("file url: %v", err)
	}
	if _, err := run(proto.FSPrepare{Action: "clone", Path: root + "/p", URL: "https://example.invalid/o/r.git"}); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("clone into a full dir: %v", err)
	}
	if _, err := run(proto.FSPrepare{Action: "rm", Path: root + "/p"}); err == nil {
		t.Error("unknown action ran")
	}

	repo := filepath.Join(root, "repo")
	gitRepo(t, repo)
	os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("x"), 0o644)
	if _, err := run(proto.FSPrepare{Action: "worktree", Repo: repo, Path: root + "/wt-foo", Branch: "foo", Base: "main"}); err != nil {
		t.Fatalf("worktree %v", err)
	}
	if _, err := run(proto.FSPrepare{Action: "worktree", Repo: repo, Path: root + "/wt-foo", Branch: "bar"}); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("worktree onto an existing path: %v", err)
	}
	// An existing branch is checked out, not created.
	exec.Command("git", "-C", repo, "branch", "existing").Run()
	if _, err := run(proto.FSPrepare{Action: "worktree", Repo: repo, Path: root + "/wt-existing", Branch: "existing"}); err != nil {
		t.Fatalf("worktree of an existing branch %v", err)
	}
	if _, err := run(proto.FSPrepare{Action: "worktree", Repo: root + "/p", Path: root + "/wt-x", Branch: "x"}); err == nil {
		t.Error("worktree of a directory that is not a repository")
	}
	if _, err := run(proto.FSPrepare{Action: "worktree", Repo: repo, Path: root + "/wt-y", Branch: "--orphan"}); err == nil {
		t.Error("option as branch")
	}

	v, err := InspectDir(ctx, repo, roots)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.Readme, "A small Go tool") || v.ReadmeOf != "README.md" || v.Git == nil || v.Git.Branch != "main" ||
		v.Git.Dirty != 1 || len(v.Git.Worktrees) != 3 || len(v.Languages) == 0 || !strings.HasPrefix(v.Languages[0], "Go") {
		t.Fatalf("inspect %+v git %+v", v, v.Git)
	}
	if v.Entries[0].Name != ".git" || !v.Entries[0].Dir {
		t.Errorf("dirs first: %+v", v.Entries)
	}
	wt, err := InspectDir(ctx, root+"/wt-foo", roots)
	if err != nil || wt.Git == nil || wt.Git.Branch != "foo" {
		t.Errorf("worktree inspect %+v %v", wt, err)
	}
	if _, err := InspectDir(ctx, root+"/escape", roots); err == nil {
		t.Error("inspected through a symlink out of the root")
	}
	if _, err := InspectDir(ctx, repo+"/main.go", roots); err == nil {
		t.Error("inspected a file")
	}
	l := ListProjectDirs(roots)["roots"].([]RootListing)
	var names []string
	for _, d := range l[0].Dirs {
		names = append(names, fmt.Sprintf("%s:%v", d.Name, d.IsGit))
	}
	got := strings.Join(names, ",")
	if !strings.Contains(got, "repo:true") || !strings.Contains(got, "wt-foo:true") || !strings.Contains(got, "p:false") || strings.Contains(got, "escape") {
		t.Errorf("list_projects %s", got)
	}
}

func TestGitEnvIsClean(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "secret")
	t.Setenv("GIT_DIR", "/elsewhere")
	env := strings.Join(gitEnv(), "\n")
	if strings.Contains(env, "secret") || strings.Contains(env, "GIT_DIR") || !strings.Contains(env, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("env %s", env)
	}
}

// scriptedLLM answers Messages API requests from a list of SSE bodies.
type scriptedLLM struct {
	srv    *httptest.Server
	mu     sync.Mutex
	script []string
	reqs   []string
}

func sseMessage(id string, blocks ...string) string {
	var b strings.Builder
	ev := func(t, d string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", t, d) }
	ev("message_start", `{"type":"message_start","message":{"id":"`+id+`","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	stop := "end_turn"
	for i, bl := range blocks {
		if name, input, ok := strings.Cut(bl, " "); ok && !strings.HasPrefix(bl, "text:") {
			stop = "tool_use"
			ev("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"%s-%d","name":"%s","input":{}}}`, i, id, i, name))
			j, _ := json.Marshal(input)
			ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, i, j))
		} else {
			ev("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, i))
			j, _ := json.Marshal(strings.TrimPrefix(bl, "text:"))
			ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, i, j))
		}
		ev("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
	}
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`"},"usage":{"input_tokens":1,"output_tokens":1}}`)
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

func newScriptedLLM(t *testing.T) *scriptedLLM {
	l := &scriptedLLM{}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.reqs = append(l.reqs, string(b))
		body := sseMessage(fmt.Sprintf("msg_%d", len(l.reqs)), "text:ok")
		if len(l.script) > 0 {
			body, l.script = l.script[0], l.script[1:]
		}
		l.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
	t.Cleanup(l.srv.Close)
	return l
}

func (l *scriptedLLM) add(bodies ...string) {
	l.mu.Lock()
	l.script = append(l.script, bodies...)
	l.mu.Unlock()
}

func (l *scriptedLLM) requests() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.reqs...)
}

// TestConciergeFramesEndToEnd runs a real hub and a real daemon (fake claude, no tmux)
// and drives a concierge conversation whose model looks at the machine, proposes a
// plan, and whose approval makes the worker create directories, a repository and a
// worktree. A second plan tries to write through a symlink out of the root.
func TestConciergeFramesEndToEnd(t *testing.T) {
	needGit(t)
	base := realTemp(t)
	for _, d := range []string{"run", "spool", "proj", "cfg", "outside"} {
		os.MkdirAll(filepath.Join(base, d), 0o700)
	}
	proj := filepath.Join(base, "proj")
	repo := filepath.Join(proj, "repo")
	gitRepo(t, repo)
	os.Symlink(filepath.Join(base, "outside"), filepath.Join(proj, "link"))
	t.Setenv("MADO_FAKE", "claude")

	llm := newScriptedLLM(t)
	store, err := hub.OpenStore(filepath.Join(base, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { handler.ServeHTTP(w, q) }))
	t.Cleanup(srv.Close)
	hcfg := &hub.Config{BaseURL: srv.URL, Listen: "127.0.0.1:0", Chat: hub.ChatConfig{Enabled: true, DataDir: filepath.Join(base, "files"),
		MaxFileMB: 1, UserQuotaMB: 1, MaxToolRounds: 8, Models: []hub.ChatModelConfig{{Name: "cheap", BaseURL: llm.srv.URL, Model: "m"}}},
		Concierge: hub.ConciergeConfig{Model: "cheap"}}
	h, err := hub.New(hcfg, store, &hub.Secrets{Keys: map[string]string{}},
		hub.Options{LocalLogin: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	handler = h.Handler()

	code, _ := store.CreateEnrollCode("box", time.Minute)
	tok, _ := store.Enroll("box", code)
	tokFile := filepath.Join(base, "cfg", "machine.token")
	os.WriteFile(tokFile, []byte(tok), 0o600)
	self, _ := os.Executable()
	cfg := &Config{HubURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/worker", MachineName: "box",
		TokenFile: tokFile, ProjectRoots: []string{proj}, ClaudeBin: self, TmuxBin: trueBin(t),
		TmuxSocket: "mado-fs", RouterListen: freePort(t), RuntimeDir: filepath.Join(base, "run"),
		SpoolDir: filepath.Join(base, "spool"), ClaudeConfigBase: filepath.Join(base, "cfg"), ShimBin: self}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	d, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	jar, _ := cookiejar.New(nil)
	web := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, err := web.Post(srv.URL+"/local-login", "", nil); err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("local login: %v", err)
	}
	getJSON := func(path string, v any) {
		resp, err := web.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(v)
	}
	var me struct{ CSRF string }
	getJSON("/api/me", &me)
	api := func(method, path string, body, v any) int {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Origin", srv.URL)
		req.Header.Set("X-CSRF-Token", me.CSRF)
		resp, err := web.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if v != nil {
			json.NewDecoder(resp.Body).Decode(v)
		}
		return resp.StatusCode
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for i := 0; i < 500; i++ {
			if cond() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timeout: %s", what)
	}
	waitFor("machine online", func() bool {
		var v struct{ Machines []struct{ Online bool } }
		getJSON("/api/machines", &v)
		return len(v.Machines) == 1 && v.Machines[0].Online
	})

	var created struct{ ID string }
	if st := api("POST", "/api/sessions", map[string]any{"kind": "chat", "concierge": true}, &created); st != 201 {
		t.Fatalf("create %d", st)
	}
	sid := created.ID
	planJSON := func(steps ...map[string]any) string {
		b, _ := json.Marshal(map[string]any{"plan": map[string]any{"summary": "demo", "prepare": steps, "sessions": []any{}}})
		return string(b)
	}
	newDir := filepath.Join(proj, "new", "sub")
	wt := filepath.Join(proj, "wt-x")
	llm.add(
		sseMessage("a1", `list_projects {"machine":"box"}`),
		sseMessage("a2", `inspect_dir {"machine":"box","path":"`+repo+`"}`),
		sseMessage("a3", "propose "+planJSON(
			map[string]any{"machine": "box", "action": "mkdir", "path": newDir},
			map[string]any{"machine": "box", "action": "git_init", "path": newDir},
			map[string]any{"machine": "box", "action": "worktree", "repo": repo, "path": wt, "branch": "x", "base": "main"})),
		sseMessage("a4", "text:Approve the plan to go on."))
	if st := api("POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "set me up"}, nil); st != 202 {
		t.Fatalf("send %d", st)
	}
	type planView struct {
		ID    string
		State string
		Error string
		Steps []struct{ State, Output string }
	}
	plans := func() []planView {
		var r struct {
			Blocks []struct {
				Kind string
				Plan *planView
			}
		}
		getJSON("/api/sessions/"+sid+"/blocks?before=0", &r)
		var out []planView
		for _, b := range r.Blocks {
			if b.Kind == "plan" && b.Plan != nil {
				out = append(out, *b.Plan)
			}
		}
		return out
	}
	waitFor("the plan card", func() bool { return len(plans()) == 1 && len(llm.requests()) >= 4 })
	reqs := llm.requests()
	if !strings.Contains(reqs[1], `\"name\":\"repo\"`) || !strings.Contains(reqs[1], `\"is_git\":true`) || strings.Contains(reqs[1], `\"name\":\"link\"`) {
		t.Errorf("list_projects result: %s", reqs[1])
	}
	if !strings.Contains(reqs[2], "A small Go tool") || !strings.Contains(reqs[2], `\"branch\":\"main\"`) {
		t.Errorf("inspect_dir result: %s", reqs[2])
	}
	p := plans()[0]
	llm.add(sseMessage("a5", "text:Done: the worktree is ready."))
	if st := api("POST", "/api/sessions/"+sid+"/plans/"+p.ID, map[string]any{"decision": "approve"}, nil); st != 200 {
		t.Fatalf("approve %d", st)
	}
	waitFor("plan done", func() bool { ps := plans(); return len(ps) == 1 && ps[0].State == "done" })
	if fi, err := os.Stat(filepath.Join(newDir, ".git")); err != nil || !fi.IsDir() {
		t.Errorf("git_init: %v", err)
	}
	out, err := exec.Command("git", "-C", wt, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(out)) != "x" {
		t.Errorf("worktree branch %q %v", out, err)
	}
	waitFor("the outcome reaches the model", func() bool { return len(llm.requests()) >= 5 })
	if r := llm.requests()[4]; !strings.Contains(r, "was approved and ran") {
		t.Errorf("outcome note: %s", r)
	}

	// A path that is inside the root on paper but leads out through a symlink: the hub
	// accepts it lexically, the worker refuses it, nothing is written outside.
	evil := filepath.Join(proj, "link", "evil")
	llm.add(sseMessage("b1", "propose "+planJSON(map[string]any{"machine": "box", "action": "mkdir", "path": evil})),
		sseMessage("b2", "text:Approve."), sseMessage("b3", "text:That failed."))
	waitFor("idle", func() bool {
		var v struct{ Session struct{ Running bool } }
		getJSON("/api/sessions/"+sid, &v)
		return !v.Session.Running
	})
	api("POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "and one more"}, nil)
	waitFor("second card", func() bool { return len(plans()) == 2 })
	p = plans()[1]
	api("POST", "/api/sessions/"+sid+"/plans/"+p.ID, map[string]any{"decision": "approve"}, nil)
	waitFor("plan failed", func() bool { ps := plans(); return len(ps) == 2 && ps[1].State == "failed" })
	if ps := plans(); !strings.Contains(ps[1].Error, "outside the project roots") {
		t.Errorf("symlink escape: %+v", ps[1])
	}
	if _, err := os.Stat(filepath.Join(base, "outside", "evil")); err == nil {
		t.Error("the worker wrote outside its roots")
	}
}
