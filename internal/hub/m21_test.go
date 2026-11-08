package hub

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kinginu/mado/internal/proto"
)

// M21: machine removal (DESIGN §24).

func enrollMachine(t *testing.T, st *Store, name string) string {
	t.Helper()
	code, err := st.CreateEnrollCode(name, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.Enroll(name, code)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func machineID(t *testing.T, st *Store, name string) string {
	t.Helper()
	ms, _ := st.Machines()
	for _, m := range ms {
		if m.Name == name {
			return m.ID
		}
	}
	t.Fatalf("no machine %s", name)
	return ""
}

func codeSession(t *testing.T, st *Store, mID, owner, state string) string {
	t.Helper()
	id := proto.NewID()
	if err := st.CreateSession(Session{ID: id, MachineID: mID, OwnerSub: owner, CWD: "/proj/a", ProfileName: "subscription",
		Title: "old work", PermissionMode: "default"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateSessionState(mID, &proto.SessionState{SessionID: id, State: state}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRemoveMachineStore(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tok := enrollMachine(t, st, "box")
	enrollMachine(t, st, "other")
	mID := machineID(t, st, "box")
	sid := codeSession(t, st, mID, "u", proto.StateIdle)
	pending, _ := st.CreateEnrollCode("box", time.Minute)

	done := codeSession(t, st, mID, "u", proto.StateDetached)
	otherSid := codeSession(t, st, machineID(t, st, "other"), "u", proto.StateRunning)
	if err := st.InsertApproval(Approval{SessionID: sid, RequestID: "r1", ToolName: "Bash", Input: `{"command":"ls"}`, State: "pending"}); err != nil {
		t.Fatal(err)
	}

	if _, err := st.RemoveMachine("nope"); !errors.Is(err, ErrNoMachine) {
		t.Fatalf("unknown name: %v", err)
	}
	st.SetMachineConnected(mID, true)
	if _, err := st.RemoveMachine("box"); !errors.Is(err, ErrMachineConnected) {
		t.Fatalf("connected: %v", err)
	}
	if s, _ := st.Session(sid); s.State != proto.StateIdle {
		t.Fatalf("a refused removal touched the session: %s", s.State)
	}
	if c, _ := st.LiveSessionCounts(); c[mID] != 1 {
		t.Fatalf("live counts: %v", c)
	}
	// Offline with a live session: removed, and the session ends.
	st.SetMachineConnected(mID, false)
	ended, err := st.RemoveMachine("box")
	if err != nil || len(ended) != 1 || ended[0] != sid {
		t.Fatalf("offline with a live session: %v %v", ended, err)
	}
	if s, _ := st.Session(sid); s.State != proto.StateExited || s.LastError != MachineRemovedReason {
		t.Fatalf("live session after removal: %s %q", s.State, s.LastError)
	}
	if s, _ := st.Session(done); s.State != proto.StateDetached || s.LastError != "" {
		t.Fatalf("ended session touched: %s %q", s.State, s.LastError)
	}
	if s, _ := st.Session(otherSid); s.State != proto.StateRunning {
		t.Fatalf("other machine's session touched: %s", s.State)
	}
	if a, _ := st.Approval(sid, "r1"); a.State != "decided" || a.Decision != "expired" {
		t.Fatalf("pending approval: %+v", a)
	}

	// Gone from the list, token rejected, the name known for the log line.
	if ms, _ := st.Machines(); len(ms) != 1 || ms[0].Name != "other" {
		t.Fatalf("machines after removal: %+v", ms)
	}
	if _, err := st.MachineByToken(tok); err == nil {
		t.Fatal("old token still valid")
	}
	if n := st.RemovedMachineName(tok); n != "box" {
		t.Fatalf("removed name %q", n)
	}
	if st.SetMachineConnected(mID, true) {
		t.Fatal("a removed machine can be marked connected")
	}
	// History survives with the machine's name.
	if s, err := st.Session(sid); err != nil || s.MachineName != "box" || s.Title != "old work" {
		t.Fatalf("old session: %+v %v", s, err)
	}
	if ss, _ := st.SessionsByOwner("u"); len(ss) != 3 {
		t.Fatalf("session list: %+v", ss)
	}
	// The pending code went with it; a new code re-enrolls the name.
	if _, err := st.Enroll("box", pending); err == nil {
		t.Fatal("pending enroll code survived the removal")
	}
	tok2 := enrollMachine(t, st, "box")
	if m, err := st.MachineByToken(tok2); err != nil || m.Name != "box" || m.ID == mID {
		t.Fatalf("re-enrolled: %+v %v", m, err)
	}
	if _, err := st.MachineByToken(tok); err == nil {
		t.Fatal("old token valid after re-enroll")
	}
	if s, _ := st.Session(sid); s.MachineName != "box" {
		t.Fatalf("old session after re-enroll: %q", s.MachineName)
	}
	// Removing it again after the re-enrollment works too, with nothing to end.
	if ended, err := st.RemoveMachine("box"); err != nil || len(ended) != 0 {
		t.Fatal(ended, err)
	}
}

func TestRemoveMachineAPI(t *testing.T) {
	idp := newFakeIdP(t)
	th := newTestHub(t, idp.srv.URL, false)
	th.h.cfg.OIDC.Scopes = []string{"openid", "profile", "email", "groups"}
	th.h.cfg.Allow = AllowConfig{}
	th.h.auth = NewOIDCAuth(th.h.cfg.OIDC)
	plain, _ := oidcLogin(t, th, idp, "newbie", "newbie@example.org", []string{"users"})
	admin, _ := oidcLogin(t, th, idp, "boss", "boss@example.org", []string{"users", DefaultAdminGroup})
	acsrf, pcsrf := apiCSRF(t, admin, th.srv.URL), apiCSRF(t, plain, th.srv.URL)
	del := func(c *http.Client, csrf, name string) (int, string) {
		var out struct{ Error string }
		s := apiDo(t, c, th.srv.URL, csrf, "DELETE", "/api/machines/"+name, nil, &out)
		return s, out.Error
	}

	tok := enrollMachine(t, th.store, "box")
	mID := machineID(t, th.store, "box")
	x := &m2{th: th}
	fw := x.connect(t, tok)
	waitFor(t, func() bool { return th.h.online(mID) })

	if s, _ := del(plain, pcsrf, "box"); s != http.StatusForbidden {
		t.Fatalf("non-admin: %d", s)
	}
	if s, _ := del(admin, acsrf, "missing"); s != http.StatusNotFound {
		t.Fatalf("unknown machine: %d", s)
	}
	if s, msg := del(admin, acsrf, "box"); s != http.StatusConflict || !strings.Contains(msg, "connected") {
		t.Fatalf("online worker: %d %q", s, msg)
	}
	// The CLI's path: the store sees the connected flag the hub keeps.
	if _, err := th.store.RemoveMachine("box"); !errors.Is(err, ErrMachineConnected) {
		t.Fatalf("store while connected: %v", err)
	}
	// A live session does not change the answer while the worker is connected.
	sid := codeSession(t, th.store, mID, "boss", proto.StateIdle)
	if s, msg := del(admin, acsrf, "box"); s != http.StatusConflict || !strings.Contains(msg, "connected") {
		t.Fatalf("online worker with a live session: %d %q", s, msg)
	}
	fw.c.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return !th.h.online(mID) })

	// The admin's machine list carries the count the confirm dialog warns about.
	var lc struct {
		Machines []struct {
			Name         string
			LiveSessions *int `json:"live_sessions"`
		}
	}
	apiDo(t, admin, th.srv.URL, "", "GET", "/api/machines", nil, &lc)
	if len(lc.Machines) != 1 || lc.Machines[0].LiveSessions == nil || *lc.Machines[0].LiveSessions != 1 {
		t.Fatalf("admin machine list: %+v", lc)
	}
	lc.Machines = nil
	apiDo(t, plain, th.srv.URL, "", "GET", "/api/machines", nil, &lc)
	if len(lc.Machines) != 1 || lc.Machines[0].LiveSessions != nil {
		t.Fatalf("non-admin machine list: %+v", lc)
	}

	// Offline with a live session: 204, the count in a header, the session ended and
	// an open view told so.
	events := th.h.broker.subscribe(sid)
	defer th.h.broker.unsubscribe(sid, events)
	req, _ := http.NewRequest("DELETE", th.srv.URL+"/api/machines/box", nil)
	req.Header.Set("Origin", th.srv.URL)
	req.Header.Set("X-CSRF-Token", acsrf)
	resp, err := admin.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("X-Mado-Ended-Sessions") != "1" {
		t.Fatalf("remove: %d %q", resp.StatusCode, resp.Header.Get("X-Mado-Ended-Sessions"))
	}
	select {
	case ev := <-events:
		if ev.event != "state" || !strings.Contains(ev.data, proto.StateExited) {
			t.Fatalf("event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no state event for the ended session")
	}
	if s, _ := th.store.Session(sid); s.State != proto.StateExited || s.LastError != MachineRemovedReason {
		t.Fatalf("session after removal: %s %q", s.State, s.LastError)
	}
	rows, _ := th.store.AuditAll(50)
	found := false
	for _, r := range rows {
		found = found || (r.Action == "machine.remove" && r.User == "boss" && r.Detail == "box ended_sessions=1")
	}
	if !found {
		t.Fatalf("no audit row: %+v", rows)
	}
	var ml struct{ Machines []struct{ Name string } }
	apiDo(t, admin, th.srv.URL, "", "GET", "/api/machines", nil, &ml)
	if len(ml.Machines) != 0 {
		t.Fatalf("still listed: %+v", ml)
	}
	// The old session still opens, with the machine's name.
	var sv struct{ Session struct{ Machine string } }
	if s := apiDo(t, admin, th.srv.URL, "", "GET", "/api/sessions/"+sid, nil, &sv); s != 200 || sv.Session.Machine != "box" {
		t.Fatalf("old session: %d %+v", s, sv)
	}
	// The worker's old token is refused at the websocket.
	wsURL := "ws" + strings.TrimPrefix(th.srv.URL, "http") + "/ws/worker"
	_, resp, err = websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token at the websocket: %v %v", err, resp)
	}
	if s, _ := del(admin, acsrf, "box"); s != http.StatusNotFound {
		t.Fatalf("second removal: %d", s)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
