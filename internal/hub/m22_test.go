package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
)

type modeAnswer struct {
	Applied bool   `json:"applied"`
	Restart bool   `json:"restart"`
	Error   string `json:"error"`
	Session struct {
		PermissionMode string `json:"permission_mode"`
		MadoAuto       bool   `json:"mado_auto"`
		ProfileKind    string `json:"profile_kind"`
	}
	Auto map[string]any
}

// setMode posts a mode change; reply, when set, plays the worker: it gets the frame
// and answers it.
func (x *m2) setMode(t *testing.T, sid, mode string, reply func(*proto.SetPermissionMode)) (int, modeAnswer, *proto.SetPermissionMode) {
	t.Helper()
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	got := make(chan *proto.SetPermissionMode, 1)
	if reply != nil {
		go func() {
			f := x.fw.next(proto.TSetPermissionMode).(*proto.SetPermissionMode)
			got <- f
			reply(f)
		}()
	}
	var out modeAnswer
	st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions/"+sid+"/mode", map[string]string{"mode": mode}, &out)
	var f *proto.SetPermissionMode
	if reply != nil {
		select {
		case f = <-got:
		case <-time.After(5 * time.Second):
			t.Fatal("no set_permission_mode frame")
		}
	}
	return st, out, f
}

func TestSetModeLive(t *testing.T) {
	x := newM2(t)
	ok := func(f *proto.SetPermissionMode) {
		x.fw.send(&proto.ModeResult{SessionID: f.SessionID, RequestID: f.RequestID, Mode: f.Mode, OK: true})
	}

	// A router session: auto is mado auto, so claude is told acceptEdits.
	r := x.session(t, "r", proto.StateIdle, "11111111-1111-4111-8111-111111111111")
	x.th.store.SetSessionMode(r, "default")
	sse := x.th.h.broker.subscribe(r)
	defer x.th.h.broker.unsubscribe(r, sse)
	st, out, f := x.setMode(t, r, "auto", ok)
	if st != 200 || !out.Applied || out.Restart || f.Mode != "acceptEdits" || f.SessionID != r ||
		out.Session.PermissionMode != "auto" || !out.Session.MadoAuto || out.Session.ProfileKind != "router" || out.Auto["enabled"] != true {
		t.Fatalf("router auto: %d %+v %+v", st, out, f)
	}
	if s, _ := x.th.store.Session(r); s.PermissionMode != "auto" || !s.AutoPolicy {
		t.Fatalf("stored %+v", s)
	}
	select {
	case m := <-sse:
		if m.event != "session" || !strings.Contains(m.data, `"permission_mode":"auto"`) {
			t.Errorf("sse %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Error("no session event published")
	}
	st, out, f = x.setMode(t, r, "default", ok)
	if st != 200 || !out.Applied || f.Mode != "default" || out.Session.MadoAuto {
		t.Fatalf("router default: %d %+v %+v", st, out, f)
	}

	// A subscription session: auto is claude's own auto mode, the hub stays out.
	sub := x.session(t, "subscription", proto.StateRunning, "22222222-2222-4222-8222-222222222222")
	st, out, f = x.setMode(t, sub, "auto", ok)
	if st != 200 || !out.Applied || f.Mode != "auto" || out.Session.MadoAuto || out.Auto != nil || out.Session.ProfileKind != "subscription" {
		t.Fatalf("subscription auto: %d %+v %+v", st, out, f)
	}
	if s, _ := x.th.store.Session(sub); s.PermissionMode != "auto" || x.th.h.autoOn(s) {
		t.Fatalf("subscription stored %+v", s)
	}

	// claude refuses or the worker is too old: stored, restart offered.
	st, out, _ = x.setMode(t, sub, "acceptEdits", func(f *proto.SetPermissionMode) {
		x.fw.send(&proto.ModeResult{SessionID: f.SessionID, RequestID: f.RequestID, Mode: f.Mode, Error: "claude did not answer the control request"})
	})
	if st != 200 || out.Applied || !out.Restart || !strings.Contains(out.Error, "did not answer") {
		t.Fatalf("refused: %d %+v", st, out)
	}
	if s, _ := x.th.store.Session(sub); s.PermissionMode != "acceptEdits" {
		t.Fatalf("not stored on refusal: %+v", s)
	}
	st, out, _ = x.setMode(t, sub, "default", func(f *proto.SetPermissionMode) {
		x.fw.send(&proto.Ack{Ref: f.ID, OK: false, Error: "unexpected frame set_permission_mode"})
	})
	if st != 200 || out.Applied || !out.Restart {
		t.Fatalf("old worker: %d %+v", st, out)
	}

	// The audit has old -> new.
	var audit struct {
		Rows []struct{ Action, Detail string }
	}
	apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/audit", nil, &audit)
	var details []string
	for _, row := range audit.Rows {
		if row.Action == "session_mode" {
			details = append(details, row.Detail)
		}
	}
	joined := strings.Join(details, "|")
	for _, want := range []string{r + " default -> auto", r + " auto -> default", sub + " acceptEdits -> auto"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit lacks %q: %s", want, joined)
		}
	}
}

func TestSetModeStoppedAndRefused(t *testing.T) {
	x := newM2(t)
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	// A stopped session: stored for the next Resume, no frame, no restart button.
	stopped := x.session(t, "subscription", proto.StateExited, "33333333-3333-4333-8333-333333333333")
	st, out, _ := x.setMode(t, stopped, "default", nil)
	if st != 200 || out.Applied || out.Restart {
		t.Fatalf("stopped: %d %+v", st, out)
	}
	x.fw.quiet(200 * time.Millisecond)
	s, _ := x.th.store.Session(stopped)
	prof, _ := x.th.store.Profile("subscription")
	if s.PermissionMode != "default" || claudeMode(s, prof) != "default" {
		t.Fatalf("stopped stored %+v", s)
	}
	// Bad modes, chat sessions and other owners' sessions are refused.
	chatID := proto.NewID()
	x.th.store.CreateSession(Session{ID: chatID, OwnerSub: "local", Kind: KindChat, Model: "m", Title: "t"})
	other := proto.NewID()
	x.th.store.CreateSession(Session{ID: other, MachineID: x.mID, OwnerSub: "someone", CWD: "/proj/a", ProfileName: "subscription", Title: "t"})
	for _, c := range []struct {
		id, mode string
		want     int
	}{{stopped, "bypassPermissions", 400}, {stopped, "plan", 400}, {stopped, "", 400}, {chatID, "auto", 400}, {other, "auto", 404}} {
		var e map[string]any
		if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions/"+c.id+"/mode", map[string]string{"mode": c.mode}, &e); st != c.want {
			t.Errorf("%s %q: %d %v", c.id, c.mode, st, e)
		}
	}
}
