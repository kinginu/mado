package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kinginu/mado/internal/proto"
)

// oidcLogin runs the full code + PKCE flow against the fake IdP.
func oidcLogin(t *testing.T, th *testHub, idp *fakeIdP, sub, email string, groups []string) (*http.Client, int) {
	t.Helper()
	c := browser()
	resp, err := c.Get(th.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if sc := q.Get("scope"); sc != "openid profile email groups" {
		t.Errorf("scopes %q", sc)
	}
	code := RandomToken(8)
	idp.mu.Lock()
	idp.codes[code] = fakeGrant{q.Get("code_challenge"), q.Get("nonce"), sub, email, groups}
	idp.mu.Unlock()
	resp, err = c.Get(th.srv.URL + "/callback?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	return c, resp.StatusCode
}

func status(t *testing.T, c *http.Client, u string) int {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func body(t *testing.T, c *http.Client, u string) string {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestRolesAndAutoCreate(t *testing.T) {
	idp := newFakeIdP(t)
	th := newTestHub(t, idp.srv.URL, false)
	th.h.cfg.OIDC.Scopes = []string{"openid", "profile", "email", "groups"}
	th.h.cfg.Allow = AllowConfig{} // no allowlist: the IdP decides who gets in
	th.h.auth = NewOIDCAuth(th.h.cfg.OIDC)

	if _, err := th.store.User("newbie"); err == nil {
		t.Fatal("user exists before login")
	}
	plain, st := oidcLogin(t, th, idp, "newbie", "newbie@example.org", []string{"users"})
	if st != http.StatusSeeOther {
		t.Fatalf("first login: %d", st)
	}
	if u, err := th.store.User("newbie"); err != nil || u.IsAdmin || u.Email != "newbie@example.org" {
		t.Fatalf("auto-created user: %+v %v", u, err)
	}
	admin, _ := oidcLogin(t, th, idp, "boss", "boss@example.org", []string{"users", DefaultAdminGroup})
	if u, _ := th.store.User("boss"); !u.IsAdmin {
		t.Fatal("admin group did not make an admin")
	}
	// The admin API is gated; the pages are SPA routes whose data comes from it.
	acsrf, pcsrf := apiCSRF(t, admin, th.srv.URL), apiCSRF(t, plain, th.srv.URL)
	for _, p := range []string{"/api/admin/profiles", "/api/admin/history", "/api/admin/users"} {
		if s := apiDo(t, admin, th.srv.URL, acsrf, "GET", p, nil, nil); s != 200 {
			t.Errorf("admin %s: %d", p, s)
		}
		if s := apiDo(t, plain, th.srv.URL, pcsrf, "GET", p, nil, nil); s != http.StatusForbidden {
			t.Errorf("user %s: %d", p, s)
		}
	}
	for _, p := range []string{"/api/admin/profiles", "/api/admin/chat-models", "/api/admin/enroll", "/api/admin/history/rescan"} {
		if s := apiDo(t, plain, th.srv.URL, pcsrf, "POST", p, map[string]string{"name": "x", "json": `{"name":"x","kind":"subscription"}`}, nil); s != http.StatusForbidden {
			t.Errorf("user POST %s: %d", p, s)
		}
	}
	// Web enrollment for admins.
	var enr struct{ Command, Code string }
	if s := apiDo(t, admin, th.srv.URL, acsrf, "POST", "/api/admin/enroll", map[string]string{"name": "box2"}, &enr); s != 200 ||
		!strings.HasPrefix(enr.Command, "mado enroll --hub "+th.srv.URL+" --name box2 --code ") || enr.Code == "" {
		t.Fatalf("enroll: %d %+v", s, enr)
	}
	if s := apiDo(t, admin, th.srv.URL, acsrf, "POST", "/api/admin/enroll", map[string]string{"name": "bad name"}, nil); s != 422 {
		t.Errorf("bad machine name: %d", s)
	}
	// Audit: admins see everyone, users only themselves.
	audit := func(c *http.Client, q string) string {
		var out struct {
			Rows []struct{ User, Action, Detail string }
		}
		apiDo(t, c, th.srv.URL, "", "GET", "/api/audit"+q, nil, &out)
		b, _ := json.Marshal(out.Rows)
		return string(b)
	}
	if a := audit(admin, ""); !strings.Contains(a, "newbie@example.org") || !strings.Contains(a, "enroll_code") {
		t.Errorf("admin audit lacks hub-wide rows: %s", a)
	}
	if a := audit(admin, "?mine=1"); strings.Contains(a, "newbie@example.org") || !strings.Contains(a, "enroll_code") {
		t.Errorf("admin audit, only mine: %s", a)
	}
	if a := audit(plain, ""); strings.Contains(a, "boss@example.org") || !strings.Contains(a, "newbie@example.org") {
		t.Errorf("user audit: %s", a)
	}
	// The flag is recomputed at every login.
	oidcLogin(t, th, idp, "boss", "boss@example.org", []string{"users"})
	if u, _ := th.store.User("boss"); u.IsAdmin {
		t.Fatal("admin flag survived leaving the group")
	}
	if s := apiDo(t, admin, th.srv.URL, acsrf, "GET", "/api/admin/profiles", nil, nil); s != http.StatusForbidden {
		t.Errorf("old admin web session still admin: %d", s)
	}
	// A non-empty allowlist narrows.
	th.h.cfg.Allow = AllowConfig{Subs: []string{"boss"}}
	if _, st := oidcLogin(t, th, idp, "newbie", "newbie@example.org", nil); st != http.StatusForbidden {
		t.Errorf("allowlist did not narrow: %d", st)
	}
}

func TestParseGroups(t *testing.T) {
	if g := parseGroups(json.RawMessage(`["a","b"]`)); len(g) != 2 {
		t.Error(g)
	}
	if g := parseGroups(json.RawMessage(`"a"`)); len(g) != 1 {
		t.Error(g)
	}
	if g := parseGroups(json.RawMessage(`{"a":1}`)); g != nil {
		t.Error(g)
	}
}

// --- worker-side helpers

type m2 struct {
	th  *testHub
	c   *http.Client
	fw  *fakeWorker
	mID string
}

func newM2(t *testing.T) *m2 {
	th := newTestHub(t, "", true)
	th.h.secrets.Keys["k"] = "the-key"
	if err := th.store.PutProfile(proto.Profile{Name: "r", Kind: proto.KindRouter, Main: "oc-a",
		Upstreams: map[string]proto.Upstream{"oc-a": {BaseURL: "http://up:1", Model: "org/A", KeyRef: "k"}}}); err != nil {
		t.Fatal(err)
	}
	c := browser()
	c.Post(th.srv.URL+"/local-login", "", nil)
	code, _ := th.store.CreateEnrollCode("box", time.Minute)
	tok, _ := th.store.Enroll("box", code)
	x := &m2{th: th, c: c}
	x.fw = x.connect(t, tok)
	x.fw.send(&proto.Hello{Machine: "box", Projects: []string{"/proj/a", "/proj/b"}})
	time.Sleep(100 * time.Millisecond)
	ms, _ := th.store.Machines()
	x.mID = ms[0].ID
	return x
}

func (x *m2) connect(t *testing.T, tok string) *fakeWorker {
	wsURL := "ws" + strings.TrimPrefix(x.th.srv.URL, "http") + "/ws/worker"
	wc, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wc.CloseNow() })
	fw := &fakeWorker{t: t, c: wc, ch: make(chan proto.Frame, 64)}
	go func() {
		for {
			_, b, err := wc.Read(context.Background())
			if err != nil {
				return
			}
			if f, err := proto.Decode(b); err == nil {
				fw.ch <- f
			}
		}
	}()
	return fw
}

// quiet asserts no frame arrives for a moment.
func (w *fakeWorker) quiet(d time.Duration) {
	w.t.Helper()
	select {
	case f := <-w.ch:
		w.t.Fatalf("unexpected frame %s %+v", proto.HeaderOf(f).T, f)
	case <-time.After(d):
	}
}

func (x *m2) session(t *testing.T, profile, state, claudeSID string) string {
	id := proto.NewID()
	if err := x.th.store.CreateSession(Session{ID: id, MachineID: x.mID, OwnerSub: "local", CWD: "/proj/a",
		ProfileName: profile, MainModel: "oc-a", Title: "t", PermissionMode: "acceptEdits"}); err != nil {
		t.Fatal(err)
	}
	if state != proto.StateStarting || claudeSID != "" {
		x.th.store.UpdateSessionState(x.mID, &proto.SessionState{SessionID: id, State: state, ClaudeSessionID: claudeSID})
	}
	return id
}

func TestReconcileRefreshResyncResume(t *testing.T) {
	x := newM2(t)
	live := x.session(t, "r", proto.StateIdle, "11111111-1111-4111-8111-111111111111")
	lost := x.session(t, "r", proto.StateRunning, "22222222-2222-4222-8222-222222222222")
	detached := x.session(t, "r", proto.StateDetached, "33333333-3333-4333-8333-333333333333")
	starting := x.session(t, "subscription", proto.StateStarting, "")
	noID := x.session(t, "subscription", proto.StateIdle, "")
	for _, seq := range []int64{1, 2, 4} { // a gap at 3
		x.th.store.InsertEvent(live, seq, []byte(`{"type":"x"}`))
	}
	for _, seq := range []int64{1, 2, 3, 5} {
		x.th.store.InsertEvent(lost, seq, []byte(`{"type":"x"}`))
	}
	x.th.store.InsertApproval(Approval{SessionID: lost, RequestID: "old", ToolName: "Bash", Input: "{}"})

	x.fw.send(&proto.Sessions{Sessions: []proto.SessionInfo{{SessionID: live, State: proto.StateIdle}}})
	got := map[string][]proto.Frame{}
	for i := 0; i < 3; i++ {
		select {
		case f := <-x.fw.ch:
			sid := ""
			switch m := f.(type) {
			case *proto.StartSession:
				sid = m.SessionID
			case *proto.Resync:
				sid = m.SessionID
			}
			got[sid] = append(got[sid], f)
		case <-time.After(5 * time.Second):
			t.Fatalf("got only %v", got)
		}
	}
	x.fw.quiet(300 * time.Millisecond)
	if len(got[live]) != 2 {
		t.Fatalf("live session frames: %+v", got[live])
	}
	ref := got[live][0].(*proto.StartSession)
	if !ref.Refresh || ref.Profile.Upstreams["oc-a"].Key != "the-key" {
		t.Fatalf("refresh frame: %+v", ref)
	}
	if rs := got[live][1].(*proto.Resync); rs.FromSeq != 2 {
		t.Fatalf("resync from %d, want 2 (the gap)", rs.FromSeq)
	}
	res := got[lost][0].(*proto.StartSession)
	if res.Refresh || res.ResumeClaudeSessionID != "22222222-2222-4222-8222-222222222222" || res.SeqBase != 5 ||
		res.FromSeq != 3 || res.PermissionMode != "acceptEdits" || res.Profile.Upstreams["oc-a"].Key != "the-key" {
		t.Fatalf("resume frame: %+v", res)
	}
	if len(got[detached])+len(got[starting])+len(got[noID]) != 0 {
		t.Fatalf("frames for sessions that must be left alone: %v", got)
	}
	if s, _ := x.th.store.Session(lost); s.State != proto.StateStarting {
		t.Errorf("resumed session state %s", s.State)
	}
	if p, _ := x.th.store.PendingApprovals(lost); len(p) != 0 {
		t.Errorf("stale approval still pending")
	}
	if s, _ := x.th.store.Session(noID); s.State != proto.StateExited || s.LastError == "" {
		t.Errorf("session without claude id: %+v", s)
	}
	// A second list right away does not resume again (rate limit), even though the
	// session is still "starting" with a claude id.
	x.fw.send(&proto.Sessions{Sessions: []proto.SessionInfo{{SessionID: live}}})
	x.fw.next(proto.TStartSession)
	x.fw.next(proto.TResync)
	x.fw.quiet(300 * time.Millisecond)
	// A failed resume (ack error) ends the session.
	x.fw.send(&proto.Ack{Ref: res.ID, OK: false, Error: "cwd is gone"})
	time.Sleep(100 * time.Millisecond)
	if s, _ := x.th.store.Session(lost); s.State != proto.StateExited {
		t.Errorf("failed resume left state %s", s.State)
	}
}

func TestLiveGapTriggersResync(t *testing.T) {
	x := newM2(t)
	sid := x.session(t, "subscription", proto.StateRunning, "")
	x.fw.send(&proto.Event{SessionID: sid, Seq: 1, Raw: json.RawMessage(`{"type":"a"}`)})
	x.fw.send(&proto.Event{SessionID: sid, Seq: 2, Raw: json.RawMessage(`{"type":"a"}`)})
	x.fw.quiet(200 * time.Millisecond)
	x.fw.send(&proto.Event{SessionID: sid, Seq: 4, Raw: json.RawMessage(`{"type":"a"}`)})
	rs := x.fw.next(proto.TResync).(*proto.Resync)
	if rs.SessionID != sid || rs.FromSeq != 2 {
		t.Fatalf("resync %+v", rs)
	}
	// The worker answers from its spool; the gap closes.
	x.fw.send(&proto.Event{SessionID: sid, Seq: 3, Raw: json.RawMessage(`{"type":"a"}`)})
	x.fw.send(&proto.Event{SessionID: sid, Seq: 4, Raw: json.RawMessage(`{"type":"a"}`)})
	time.Sleep(100 * time.Millisecond)
	if c := x.th.store.ContiguousSeq(sid); c != 4 {
		t.Fatalf("contiguous %d", c)
	}
}

func TestContiguousSeq(t *testing.T) {
	th := newTestHub(t, "", true)
	if th.store.ContiguousSeq("s") != 0 {
		t.Fatal("empty")
	}
	for _, s := range []int64{2, 3} {
		th.store.InsertEvent("s", s, []byte("{}"))
	}
	if th.store.ContiguousSeq("s") != 0 {
		t.Fatal("missing 1")
	}
	th.store.InsertEvent("s", 1, []byte("{}"))
	th.store.InsertEvent("s", 7, []byte("{}"))
	if c, m := th.store.ContiguousSeq("s"), th.store.MaxSeq("s"); c != 3 || m != 7 {
		t.Fatalf("contiguous %d max %d", c, m)
	}
}

// send posts a composer message through the API; it returns the status and the
// answer ({id} for a /new command, {error} on a refusal).
func (x *m2) send(t *testing.T, sid, text string) (int, map[string]string) {
	t.Helper()
	out := map[string]string{}
	st := apiDo(t, x.c, x.th.srv.URL, apiCSRF(t, x.c, x.th.srv.URL), "POST", "/api/sessions/"+sid+"/send", map[string]string{"text": text}, &out)
	return st, out
}

func TestNewCommandAndInterrupt(t *testing.T) {
	x := newM2(t)
	sid := x.session(t, "subscription", proto.StateIdle, "")

	for _, bad := range []string{
		"/new box /proj/a/../a", "/new box /proj/a/", "/new box /etc", "/new nosuch /proj/a",
		"/new box /proj/a nosuch", "/new box", "/new box /proj/a r extra", "/new box;id /proj/a",
	} {
		st, out := x.send(t, sid, bad)
		if st != http.StatusBadRequest || out["id"] != "" || out["error"] == "" || strings.Contains(out["error"], "/etc") {
			t.Errorf("%q: %d %v", bad, st, out)
		}
	}
	st, out := x.send(t, sid, "/new box /proj/a")
	start := x.fw.next(proto.TStartSession).(*proto.StartSession)
	if st != http.StatusCreated || start.CWD != "/proj/a" || start.Profile.Name != "subscription" || out["id"] != start.SessionID {
		t.Fatalf("start %+v, answer %d %v", start, st, out)
	}
	st, out = x.send(t, sid, "/new box /proj/b r")
	start = x.fw.next(proto.TStartSession).(*proto.StartSession)
	if start.CWD != "/proj/b" || start.MainModel != "oc-a" || start.Profile.Upstreams["oc-a"].Key != "the-key" || out["id"] != start.SessionID {
		t.Fatalf("router /new: %+v %d %v", start, st, out)
	}
	x.fw.quiet(200 * time.Millisecond) // the bad commands sent nothing
	// A normal message still goes through.
	if st, _ := x.send(t, sid, "/newish text"); st != http.StatusAccepted {
		t.Fatalf("message: %d", st)
	}
	if um := x.fw.next(proto.TUserMessage).(*proto.UserMessage); um.Text != "/newish text" {
		t.Fatal(um)
	}

	if st := apiDo(t, x.c, x.th.srv.URL, apiCSRF(t, x.c, x.th.srv.URL), "POST", "/api/sessions/"+sid+"/interrupt", nil, nil); st != 200 {
		t.Fatalf("interrupt: %d", st)
	}
	if in := x.fw.next(proto.TInterrupt).(*proto.Interrupt); in.SessionID != sid {
		t.Fatal(in)
	}
	var audit json.RawMessage
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/audit", nil, &audit)
	if a := string(audit); !strings.Contains(a, `"interrupt"`) || strings.Contains(a, "/newish text") {
		t.Errorf("audit: %s", a)
	}
}

func TestSSESendsPendingApprovalFirst(t *testing.T) {
	x := newM2(t)
	sid := x.session(t, "subscription", proto.StateRunning, "")
	x.fw.send(&proto.Event{SessionID: sid, Seq: 1, Raw: json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`)})
	time.Sleep(100 * time.Millisecond)
	// The blocks are loaded (after=1), then the approval arrives before SSE connects.
	x.fw.send(&proto.ApprovalRequest{SessionID: sid, RequestID: "late", ToolName: "Bash", Input: json.RawMessage(`{"command":"ls"}`)})
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", x.th.srv.URL+"/s/"+sid+"/events?after=1", nil)
	resp, err := x.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	var first []string
	for sc.Scan() && sc.Text() != "" {
		first = append(first, sc.Text())
	}
	joined := strings.Join(first, "\n")
	if !strings.HasPrefix(joined, "event: approval") || !strings.Contains(joined, `"request_id":"late"`) || !strings.Contains(joined, `"state":"pending"`) {
		t.Fatalf("first SSE event: %s", joined)
	}
}

func TestParseProfile(t *testing.T) {
	sec := &Secrets{Keys: map[string]string{"k": "v"}}
	good := `{"name":"p1","kind":"router","main":"oc-a","aliases":{"haiku":"oc-a"},
	  "upstreams":{"oc-a":{"base_url":"http://h:8000","model":"zai-org/GLM-5.1","key_ref":"k","context_window":131072,"max_output":32000}}}`
	p, err := ParseProfile([]byte(good), sec)
	if err != nil || p.Upstreams["oc-a"].ContextWindow != 131072 || p.Upstreams["oc-a"].MaxOutput != 32000 {
		t.Fatalf("%+v %v", p, err)
	}
	for name, bad := range map[string]string{
		"slash in router name": strings.Replace(good, `"oc-a"`, `"oc/a"`, -1),
		"inline key":           strings.Replace(good, `"key_ref":"k"`, `"key":"sk-123"`, 1),
		"unknown key_ref":      strings.Replace(good, `"key_ref":"k"`, `"key_ref":"nope"`, 1),
		"unknown field":        strings.Replace(good, `"kind"`, `"extra":1,"kind"`, 1),
		"bad name":             strings.Replace(good, `"p1"`, `"p 1"`, 1),
		"file url":             strings.Replace(good, `http://h:8000`, `file:///etc`, 1),
		"tiny window":          strings.Replace(good, `131072`, `5`, 1),
		"alias to nothing":     strings.Replace(good, `"haiku":"oc-a"`, `"haiku":"oc-b"`, 1),
		"bad kind":             strings.Replace(good, `"router"`, `"shell"`, 1),
		"trailing":             good + `{}`,
		"subscription extras":  `{"name":"s2","kind":"subscription","main":"x"}`,
	} {
		if _, err := ParseProfile([]byte(bad), sec); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestProfileEditor(t *testing.T) {
	x := newM2(t) // the local user is an admin
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	post := func(orig, js string) (int, string) {
		var out struct{ Name, Error string }
		st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/admin/profiles", map[string]string{"original": orig, "json": js}, &out)
		return st, out.Name + out.Error
	}
	js := `{"name":"mine","kind":"router","main":"oc-a","upstreams":{"oc-a":{"base_url":"http://h:1","model":"o/m","key_ref":"k"}}}`
	if st, msg := post("", js); st != 200 || msg != "mine" {
		t.Fatalf("create: %d %s", st, msg)
	}
	if x.th.store.ProfileSource("mine") != SourceUI {
		t.Fatal("not saved as ui")
	}
	if st, msg := post("", js); st != http.StatusUnprocessableEntity || !strings.Contains(msg, "already exists") {
		t.Errorf("duplicate create: %d %s", st, msg)
	}
	if st, _ := post("mine", strings.Replace(js, "o/m", "o/m2", 1)); st != 200 {
		t.Errorf("edit: %d", st)
	}
	if p, _ := x.th.store.Profile("mine"); p.Upstreams["oc-a"].Model != "o/m2" {
		t.Errorf("edit not stored: %+v", p)
	}
	if st, msg := post("mine", strings.Replace(js, `"mine"`, `"other"`, 1)); st != 422 || !strings.Contains(msg, "renaming") {
		t.Errorf("rename: %d %s", st, msg)
	}
	if st, msg := post("", strings.Replace(js, `"oc-a"`, `"oc/a"`, -1)); st != http.StatusUnprocessableEntity || !strings.Contains(msg, "not allowed") {
		t.Errorf("slash name: %d %s", st, msg)
	}
	if st, msg := post("", `{"name":"x","kind":"router","key":"sk"}`); st != 422 || !strings.Contains(msg, "invalid JSON") {
		t.Errorf("inline key: %d %s", st, msg)
	}
	// A config profile takes an override (§28.1); the built-in one is read-only.
	if st, msg := post("r", `{"name":"r","kind":"subscription"}`); st != 200 || x.th.store.ProfileSource("r") != SourceUI {
		t.Errorf("config profile override: %d %s", st, msg)
	}
	if st, _ := post("subscription", `{"name":"subscription","kind":"subscription"}`); st != http.StatusUnprocessableEntity {
		t.Errorf("builtin overwritten: %d", st)
	}
	var list struct {
		Profiles []apiProfileRow
		Keys     []string
		Template string `json:"profile_template"`
	}
	var raw json.RawMessage
	apiDo(t, x.c, x.th.srv.URL, csrf, "GET", "/api/admin/profiles", nil, &raw)
	json.Unmarshal(raw, &list)
	src := map[string]string{}
	for _, p := range list.Profiles {
		src[p.Name] = p.Source + fmt.Sprint(p.Editable)
		if p.Name == "mine" && !strings.Contains(p.JSON, `"key_ref": "k"`) {
			t.Errorf("mine JSON: %s", p.JSON)
		}
	}
	if fmt.Sprint(src) != "map[mine:uitrue r:uitrue subscription:builtinfalse]" || fmt.Sprint(list.Keys) != "[k]" || list.Template == "" {
		t.Errorf("list: %v %v", src, list.Keys)
	}
	if strings.Contains(string(raw), "the-key") {
		t.Fatal("a key value reached the API")
	}
}

func TestReconcileSendsGCHint(t *testing.T) {
	x := newM2(t)
	exited := x.session(t, "subscription", proto.StateExited, "")
	detached := x.session(t, "subscription", proto.StateDetached, "")
	idle := x.session(t, "subscription", proto.StateIdle, "55555555-5555-4555-8555-555555555555")
	live := x.session(t, "subscription", proto.StateRunning, "")
	unknown := proto.NewID()
	// A session of another machine is "unknown" on this one.
	other := proto.NewID()
	x.th.store.CreateSession(Session{ID: other, MachineID: "elsewhere", OwnerSub: "local", CWD: "/x", ProfileName: "subscription"})

	x.fw.send(&proto.Sessions{Sessions: []proto.SessionInfo{{SessionID: live}},
		Spools: []string{exited, detached, idle, live, unknown, other, "bad"}})
	gc := x.fw.next(proto.TGC).(*proto.GC)
	got := map[string]bool{}
	for _, id := range gc.SessionIDs {
		got[id] = true
	}
	if len(got) != 4 || !got[exited] || !got[detached] || !got[unknown] || !got[other] {
		t.Fatalf("gc hint %v", gc.SessionIDs)
	}
	if got[idle] || got[live] {
		t.Fatal("gc hint names a session that is alive or resumable")
	}
	// No spools, no hint.
	x.fw.send(&proto.Sessions{Sessions: []proto.SessionInfo{{SessionID: live}}})
	x.fw.next(proto.TResync)
	x.fw.quiet(300 * time.Millisecond)
}
