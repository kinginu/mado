package hub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/policy"
	"github.com/kinginu/mado/internal/proto"
)

func TestClaudeMode(t *testing.T) {
	router := &proto.Profile{Kind: proto.KindRouter}
	sub := &proto.Profile{Kind: proto.KindSubscription}
	cases := []struct {
		mode string
		prof *proto.Profile
		want string
	}{
		{"auto", router, "acceptEdits"},
		{"auto", sub, "auto"},
		{"acceptEdits", router, "acceptEdits"},
		{"default", router, "default"},
		{"dontAsk", sub, "default"},
		{"plan", router, "default"},
		{"", router, "default"},
	}
	for _, c := range cases {
		if got := claudeMode(&Session{PermissionMode: c.mode}, c.prof); got != c.want {
			t.Errorf("%s on %s: %s, want %s", c.mode, c.prof.Kind, got, c.want)
		}
	}
}

// fakeClassifier answers each request with the next decide input.
type fakeClassifier struct {
	srv     *httptest.Server
	mu      sync.Mutex
	replies []string
	n       int
}

func newFakeClassifier(t *testing.T) *fakeClassifier {
	f := &fakeClassifier{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.n++
		reply := `{"decision":"ask","reason":"no reply queued"}`
		if len(f.replies) > 0 {
			reply, f.replies = f.replies[0], f.replies[1:]
		}
		id := "cls_" + string(rune('0'+f.n))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, toolUseStream(id, "decide", reply))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeClassifier) queue(replies ...string) {
	f.mu.Lock()
	f.replies = append(f.replies, replies...)
	f.mu.Unlock()
}

func (f *fakeClassifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// autoSession creates a running router session in permission mode auto.
func (x *m2) autoSession(t *testing.T, profile string) string {
	id := proto.NewID()
	if err := x.th.store.CreateSession(Session{ID: id, MachineID: x.mID, OwnerSub: "local", CWD: "/proj/a",
		ProfileName: profile, MainModel: "oc-a", Title: "t", PermissionMode: "auto"}); err != nil {
		t.Fatal(err)
	}
	x.th.store.UpdateSessionState(x.mID, &proto.SessionState{SessionID: id, State: proto.StateRunning, ClaudeSessionID: "44444444-4444-4444-8444-444444444444"})
	return id
}

func bashReq(sid, rid, cmd string) *proto.ApprovalRequest {
	in, _ := json.Marshal(map[string]string{"command": cmd})
	return &proto.ApprovalRequest{SessionID: sid, RequestID: rid, ToolName: "Bash", Input: in}
}

func TestAutoPipeline(t *testing.T) {
	x := newM2(t)
	fc := newFakeClassifier(t)
	x.th.h.classifier = &policy.Classifier{Upstream: messages.Upstream{BaseURL: fc.srv.URL}, Model: "deepseek-ai/deepseek-v4-flash",
		Timeout: 3 * time.Second}
	sid := x.autoSession(t, "r")
	x.fw.send(&proto.Event{SessionID: sid, Seq: 1, Raw: json.RawMessage(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"push it"}]}}`)})
	time.Sleep(50 * time.Millisecond)

	answer := func(rid string) *proto.ApprovalResponse {
		t.Helper()
		r := x.fw.next(proto.TApprovalResponse).(*proto.ApprovalResponse)
		if r.RequestID != rid {
			t.Fatalf("answer for %s, want %s", r.RequestID, rid)
		}
		return r
	}
	stored := func(rid string) *Approval {
		t.Helper()
		a, err := x.th.store.Approval(sid, rid)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	// 1. A session rule answers before anything else, even a deny rule's command.
	x.th.store.AddSessionRule(sid, "Bash:sudo make install")
	x.fw.send(bashReq(sid, "r1", "sudo make install"))
	if r := answer("r1"); r.Decision != proto.DecisionAllow {
		t.Fatalf("session rule: %+v", r)
	}
	if a := stored("r1"); a.DecidedBy != "rule" || a.Auto != "allow" {
		t.Errorf("session rule stored %+v", a)
	}
	// 2. Deny rules: answered with the policy message; the classifier is not asked.
	x.fw.send(bashReq(sid, "r2", "git push --force origin main"))
	if r := answer("r2"); r.Decision != proto.DecisionDeny ||
		r.Message != "mado policy denied this: git push --force; ask the user in your reply if it is needed" {
		t.Fatalf("deny: %+v", r)
	}
	if a := stored("r2"); a.DecidedBy != "rule:git push --force" || a.Auto != "deny" || a.Reason != "git push --force" {
		t.Errorf("deny stored %+v", a)
	}
	// 3. Allow rules.
	x.fw.send(bashReq(sid, "r3", "go test ./... && git status"))
	if r := answer("r3"); r.Decision != proto.DecisionAllow {
		t.Fatalf("allow: %+v", r)
	}
	if a := stored("r3"); a.DecidedBy != "rule:go test, git status" {
		t.Errorf("allow stored %+v", a)
	}
	if fc.count() != 0 {
		t.Fatalf("the classifier ran for a rule decision: %d", fc.count())
	}
	// 4. The classifier: allow, deny, ask.
	fc.queue(`{"decision":"allow","reason":"the user asked to push"}`, `{"decision":"deny","reason":"uploads a private key"}`,
		`{"decision":"ask","reason":"not sure about this host"}`)
	x.fw.send(bashReq(sid, "r4", "git push origin main"))
	if r := answer("r4"); r.Decision != proto.DecisionAllow {
		t.Fatalf("classifier allow: %+v", r)
	}
	if a := stored("r4"); a.DecidedBy != "classifier" || a.Reason != "the user asked to push" {
		t.Errorf("classifier allow stored %+v", a)
	}
	x.fw.send(bashReq(sid, "r5", "scp key.pem far:"))
	if r := answer("r5"); r.Decision != proto.DecisionDeny || !strings.Contains(r.Message, "denied this: classifier: uploads a private key;") {
		t.Fatalf("classifier deny: %+v", r)
	}
	x.fw.send(bashReq(sid, "r6", "ssh far uptime"))
	x.fw.quiet(300 * time.Millisecond)
	if a := stored("r6"); a.State != "pending" || a.Auto != "ask" || a.Reason != "classifier: not sure about this host" {
		t.Fatalf("classifier ask stored %+v", a)
	}
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	var dec map[string]any
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions/"+sid+"/approve", map[string]string{"request_id": "r6", "decision": "deny"}, &dec); st != 200 {
		t.Fatalf("user decision %d %v", st, dec)
	}
	if r := answer("r6"); r.Decision != proto.DecisionDeny {
		t.Fatalf("user deny %+v", r)
	}
	// Usage rows of kind auto, one per classifier call, paid by the owner.
	rows, _ := x.th.store.UsageRows(sid)
	n := 0
	for _, u := range rows {
		if u.Kind == KindAuto && u.OwnerSub == "local" && u.Input == 100 && u.Output == 20 {
			n++
		}
	}
	if n != 3 {
		t.Errorf("auto usage rows %d of %+v", n, rows)
	}
	// Audit rows, visible to the owner.
	var audit struct {
		Rows []struct{ Action, Detail string }
	}
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/audit", nil, &audit)
	var auto []string
	for _, r := range audit.Rows {
		if r.Action == "approval_auto" {
			auto = append(auto, strings.TrimPrefix(r.Detail, sid+" "))
		}
	}
	joined := strings.Join(auto, "|")
	for _, want := range []string{"Bash deny rule:git push --force", "Bash allow rule:go test, git status", "Bash allow classifier", "Bash deny classifier", "Bash ask"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit lacks %q: %s", want, joined)
		}
	}
	// The detail and the blocks show the counts and every auto card.
	var d struct {
		Session struct {
			MadoAuto bool `json:"mado_auto"`
		}
		Auto map[string]any
	}
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/sessions/"+sid, nil, &d)
	if !d.Session.MadoAuto || d.Auto["enabled"] != true || d.Auto["allowed"] != 3.0 || d.Auto["denied"] != 2.0 || d.Auto["asked"] != 1.0 {
		t.Errorf("detail %+v", d)
	}
	var bl struct{ Approvals []apiApproval }
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/sessions/"+sid+"/blocks", nil, &bl)
	if len(bl.Approvals) != 6 || bl.Approvals[1].DecidedBy != "rule:git push --force" || bl.Approvals[1].AfterSeq != 1 || bl.Approvals[5].Auto != "ask" {
		t.Errorf("blocks approvals %+v", bl.Approvals)
	}
}

func TestAutoSwitchAndRulesOnly(t *testing.T) {
	x := newM2(t)
	sid := x.autoSession(t, "r")
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	// No classifier: what no rule decides goes to the user, marked as asked.
	x.fw.send(bashReq(sid, "a1", "git push origin main"))
	x.fw.quiet(200 * time.Millisecond)
	if a, _ := x.th.store.Approval(sid, "a1"); a.State != "pending" || a.Auto != "ask" || !strings.Contains(a.Reason, "no classifier") {
		t.Fatalf("rules only %+v", a)
	}
	// Switch off: the hub stops deciding; claude is not restarted.
	var out struct {
		Auto map[string]any
	}
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "PATCH", "/api/sessions/"+sid, map[string]bool{"auto": false}, &out); st != 200 || out.Auto["enabled"] != false {
		t.Fatalf("switch off %d %+v", st, out)
	}
	x.fw.send(bashReq(sid, "a2", "git status"))
	x.fw.quiet(200 * time.Millisecond)
	if a, _ := x.th.store.Approval(sid, "a2"); a.State != "pending" || a.Auto != "" {
		t.Fatalf("switched off %+v", a)
	}
	apiDo(t, x.c, x.th.srv.URL, csrf, "PATCH", "/api/sessions/"+sid, map[string]bool{"auto": true}, nil)
	x.fw.send(bashReq(sid, "a3", "git status"))
	if r := x.fw.next(proto.TApprovalResponse).(*proto.ApprovalResponse); r.RequestID != "a3" || r.Decision != proto.DecisionAllow {
		t.Fatalf("switched on %+v", r)
	}
	// Only a router code session has the switch; a subscription auto session is
	// Claude Code's own auto mode and the hub never decides for it.
	subAuto := proto.NewID()
	x.th.store.CreateSession(Session{ID: subAuto, MachineID: x.mID, OwnerSub: "local", CWD: "/proj/a", ProfileName: "subscription",
		MainModel: "opus", Title: "t", PermissionMode: "auto"})
	subEdits := proto.NewID()
	x.th.store.CreateSession(Session{ID: subEdits, MachineID: x.mID, OwnerSub: "local", CWD: "/proj/a", ProfileName: "subscription",
		MainModel: "opus", Title: "t", PermissionMode: "acceptEdits"})
	chatID := proto.NewID()
	x.th.store.CreateSession(Session{ID: chatID, OwnerSub: "local", Kind: KindChat, Model: "m", Title: "t"})
	for _, id := range []string{subAuto, subEdits, chatID} {
		var e map[string]string
		if st := apiDo(t, x.c, x.th.srv.URL, csrf, "PATCH", "/api/sessions/"+id, map[string]bool{"auto": true}, &e); st != 400 {
			t.Errorf("switch on %s: %d", id, st)
		}
	}
	x.fw.send(bashReq(subAuto, "s1", "git status"))
	x.fw.quiet(200 * time.Millisecond)
	if a, _ := x.th.store.Approval(subAuto, "s1"); a.State != "pending" || a.Auto != "" {
		t.Errorf("subscription auto %+v", a)
	}
	var audit json.RawMessage
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/audit", nil, &audit)
	if !strings.Contains(string(audit), `"session_auto"`) {
		t.Errorf("audit %s", audit)
	}
}

// A router session started in default or acceptEdits is switched to mado auto while
// it runs (DESIGN §18.6): the mode becomes auto in the DB, claude is left alone, and
// the policy decides from the next request on, edits in the project included.
func TestAutoSwitchOnMidSession(t *testing.T) {
	x := newM2(t)
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	sid := x.session(t, "r", proto.StateRunning, "55555555-5555-4555-8555-555555555555") // acceptEdits
	type view struct {
		Session struct {
			PermissionMode string `json:"permission_mode"`
			MadoAuto       bool   `json:"mado_auto"`
		}
		Auto map[string]any
	}
	var before view
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/sessions/"+sid, nil, &before)
	if before.Auto == nil || before.Auto["enabled"] != false || before.Session.MadoAuto {
		t.Fatalf("before %+v", before)
	}
	// Not in auto: the hub does not decide.
	x.fw.send(bashReq(sid, "b0", "git status"))
	x.fw.quiet(200 * time.Millisecond)
	if a, _ := x.th.store.Approval(sid, "b0"); a.State != "pending" || a.Auto != "" {
		t.Fatalf("before switch %+v", a)
	}
	var out view
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "PATCH", "/api/sessions/"+sid, map[string]bool{"auto": true}, &out); st != 200 ||
		out.Auto["enabled"] != true || !out.Session.MadoAuto || out.Session.PermissionMode != "auto" {
		t.Fatalf("switch on %d %+v", st, out)
	}
	if s, _ := x.th.store.Session(sid); s.PermissionMode != "auto" || !s.AutoPolicy {
		t.Fatalf("stored %+v", s)
	}
	// claude is not restarted or told anything.
	x.fw.quiet(200 * time.Millisecond)
	// An edit in the project (claude may still be in default) and a Bash rule allow.
	in, _ := json.Marshal(map[string]string{"file_path": "/proj/a/main.go", "old_string": "a", "new_string": "b"})
	x.fw.send(&proto.ApprovalRequest{SessionID: sid, RequestID: "b1", ToolName: "Edit", Input: in})
	if r := x.fw.next(proto.TApprovalResponse).(*proto.ApprovalResponse); r.RequestID != "b1" || r.Decision != proto.DecisionAllow {
		t.Fatalf("edit %+v", r)
	}
	if a, _ := x.th.store.Approval(sid, "b1"); a.DecidedBy != "rule:edit-in-project" {
		t.Errorf("edit decided by %q", a.DecidedBy)
	}
	x.fw.send(bashReq(sid, "b2", "git status"))
	if r := x.fw.next(proto.TApprovalResponse).(*proto.ApprovalResponse); r.RequestID != "b2" || r.Decision != proto.DecisionAllow {
		t.Fatalf("bash %+v", r)
	}
	// A resume maps the stored auto to acceptEdits.
	s, _ := x.th.store.Session(sid)
	prof, _ := x.th.store.Profile("r")
	if m := claudeMode(s, prof); m != "acceptEdits" {
		t.Errorf("resume mode %q", m)
	}
	// Off again: the mode stays auto, the policy stops.
	apiDo(t, x.c, x.th.srv.URL, csrf, "PATCH", "/api/sessions/"+sid, map[string]bool{"auto": false}, &out)
	if s, _ := x.th.store.Session(sid); s.PermissionMode != "auto" || s.AutoPolicy || out.Auto["enabled"] != false {
		t.Fatalf("switch off %+v %+v", s, out)
	}
	// A default session too; and the audit has both switches.
	def := proto.NewID()
	x.th.store.CreateSession(Session{ID: def, MachineID: x.mID, OwnerSub: "local", CWD: "/proj/a", ProfileName: "r",
		MainModel: "oc-a", Title: "t", PermissionMode: "default"})
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "PATCH", "/api/sessions/"+def, map[string]bool{"auto": true}, &out); st != 200 || out.Session.PermissionMode != "auto" {
		t.Fatalf("default switch on %d %+v", st, out)
	}
	var audit json.RawMessage
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/audit", nil, &audit)
	if !strings.Contains(string(audit), sid+" true") || !strings.Contains(string(audit), def+" true") {
		t.Errorf("audit %s", audit)
	}
}

func TestAutoResumeAndNewCommand(t *testing.T) {
	x := newM2(t)
	sid := x.autoSession(t, "r")
	subAuto := proto.NewID()
	x.th.store.CreateSession(Session{ID: subAuto, MachineID: x.mID, OwnerSub: "local", CWD: "/proj/a", ProfileName: "subscription",
		MainModel: "opus", Title: "t", PermissionMode: "auto"})
	x.th.store.UpdateSessionState(x.mID, &proto.SessionState{SessionID: subAuto, State: proto.StateIdle, ClaudeSessionID: "55555555-5555-4555-8555-555555555555"})
	x.fw.send(&proto.Sessions{})
	modes := map[string]string{}
	for range 2 {
		st := x.fw.next(proto.TStartSession).(*proto.StartSession)
		modes[st.SessionID] = st.PermissionMode
	}
	if modes[sid] != "acceptEdits" || modes[subAuto] != "auto" {
		t.Errorf("resume modes %v", modes)
	}
	if s, _ := x.th.store.Session(sid); s.PermissionMode != "auto" {
		t.Errorf("stored mode %q", s.PermissionMode)
	}

	from := x.session(t, "subscription", proto.StateIdle, "")
	st, out := x.send(t, from, "/new box /proj/b r auto")
	start := x.fw.next(proto.TStartSession).(*proto.StartSession)
	if st != http.StatusCreated || start.PermissionMode != "acceptEdits" || start.SessionID != out["id"] {
		t.Fatalf("/new auto: %d %v %+v", st, out, start)
	}
	if s, _ := x.th.store.Session(out["id"]); s.PermissionMode != "auto" || !s.AutoPolicy {
		t.Errorf("/new stored %+v", s)
	}
	st, out = x.send(t, from, "/new box /proj/b subscription acceptEdits")
	start = x.fw.next(proto.TStartSession).(*proto.StartSession)
	if st != http.StatusCreated || start.PermissionMode != "acceptEdits" {
		t.Fatalf("/new acceptEdits: %d %v", st, out)
	}
	for _, bad := range []string{"/new box /proj/b r plan", "/new box /proj/b r bypassPermissions", "/new box /proj/b r auto x"} {
		if st, out := x.send(t, from, bad); st != http.StatusBadRequest || out["error"] == "" {
			t.Errorf("%q: %d %v", bad, st, out)
		}
	}
}

func TestAutoConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(auto string) error {
		p := filepath.Join(dir, "hub.toml")
		os.WriteFile(p, []byte("base_url = \"https://mado.example\"\n"+auto), 0o600)
		_, err := LoadConfig(p)
		return err
	}
	for _, good := range []string{"", "[auto]\nclassifier = \"deepseek-v4-flash\"\nallow = ['^git fetch( |$)']\ndeny = ['docker ']\nmax_classifier_per_turn = 5\n"} {
		if err := write(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	for _, bad := range []string{"[auto]\nallow = ['(']\n", "[auto]\ndeny = ['[x']\n",
		"[auto]\nclassifier = \"no spaces allowed\"\n", "[auto]\nunknown = 1\n"} {
		if err := write(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// The classifier must name a chat model.
	th := newTestHub(t, "", true)
	cfg := *th.h.cfg
	cfg.Auto = AutoConfig{Classifier: "nosuch"}
	if _, err := New(&cfg, th.store, &Secrets{Keys: map[string]string{}}, Options{LocalLogin: true}); err == nil || !strings.Contains(err.Error(), "classifier") {
		t.Errorf("unknown classifier: %v", err)
	}
	cfg.Chat.Models = []ChatModelConfig{{Name: "dsv4flash", BaseURL: "http://up:1", Model: "deepseek-ai/deepseek-v4-flash"}}
	cfg.Chat.Enabled = true
	cfg.Auto = AutoConfig{Classifier: "dsv4flash"}
	h, err := New(&cfg, th.store, &Secrets{Keys: map[string]string{}}, Options{LocalLogin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.classifier == nil || h.classifier.Model != "deepseek-ai/deepseek-v4-flash" {
		t.Errorf("classifier %+v", h.classifier)
	}
}
