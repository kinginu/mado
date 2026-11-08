package hub

import (
	"path/filepath"
	"testing"

	"github.com/kinginu/mado/internal/proto"
)

const resumeCID = "44444444-4444-4444-8444-444444444444"

func TestSessionResume(t *testing.T) {
	x := newM2(t)
	base := x.th.srv.URL
	csrf := apiCSRF(t, x.c, base)
	mk := func(owner, kind, state, cid, mode string) string {
		id := proto.NewID()
		if err := x.th.store.CreateSession(Session{ID: id, MachineID: x.mID, OwnerSub: owner, CWD: "/proj/a/sub", ProfileName: "subscription",
			MainModel: "sonnet", Title: "old title", PermissionMode: mode, Kind: kind, ClaudeSessionID: cid, State: state}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	post := func(id string, out any) int {
		return apiDo(t, x.c, base, csrf, "POST", "/api/sessions/"+id+"/resume", nil, out)
	}

	// Refused cases.
	for name, id := range map[string]string{
		"running":   mk("local", KindCode, proto.StateRunning, "55555555-5555-4555-8555-555555555555", "default"),
		"chat":      mk("local", KindChat, proto.StateExited, "66666666-6666-4666-8666-666666666666", "default"),
		"other":     mk("someone-else", KindCode, proto.StateExited, "77777777-7777-4777-8777-777777777777", "default"),
		"no claude": mk("local", KindCode, proto.StateExited, "", "default"),
	} {
		var out struct{ Error string }
		if st := post(id, &out); st != 400 && st != 404 || out.Error == "" {
			t.Errorf("%s: %d %+v", name, st, out)
		}
	}

	// The only sessions so far are refused ones; none was started.
	id := mk("local", KindCode, proto.StateExited, resumeCID, "acceptEdits")
	var got struct {
		Session apiSessionView
	}
	if st := apiDo(t, x.c, base, "", "GET", "/api/sessions/"+id, nil, &got); st != 200 || !got.Session.Resumable {
		t.Fatalf("detail: %d %+v", st, got.Session)
	}
	var out struct{ ID string }
	if st := post(id, &out); st != 201 || out.ID == "" {
		t.Fatalf("resume: %d %+v", st, out)
	}
	start := x.fw.next(proto.TStartSession).(*proto.StartSession)
	if start.SessionID != out.ID || start.ResumeClaudeSessionID != resumeCID || start.CWD != "/proj/a/sub" ||
		start.PermissionMode != "acceptEdits" || start.Profile.Kind != proto.KindSubscription {
		t.Fatalf("start frame %+v", start)
	}
	ns, _ := x.th.store.Session(out.ID)
	if ns.ClaudeSessionID != resumeCID || ns.Title != "old title" || ns.MachineID != x.mID || ns.MainModel != "sonnet" ||
		ns.ProfileName != "subscription" || ns.PermissionMode != "acceptEdits" || ns.OwnerSub != "local" {
		t.Fatalf("new row %+v", ns)
	}

	// The old row is continued by the new one: hidden from the list, not resumable, and
	// a second resume points at the new session.
	old, _ := x.th.store.Session(id)
	if old.ResumedBy != out.ID || ns.ResumedFrom != id || old.ResumedFrom != "" || ns.ResumedBy != "" {
		t.Fatalf("chain: old %+v new %+v", old, ns)
	}
	listed := func(q string) map[string]apiSessionView {
		var list struct{ Sessions []apiSessionView }
		if st := apiDo(t, x.c, base, "", "GET", "/api/sessions"+q, nil, &list); st != 200 {
			t.Fatalf("list%s: %d", q, st)
		}
		m := map[string]apiSessionView{}
		for _, v := range list.Sessions {
			m[v.ID] = v
		}
		return m
	}
	if l := listed(""); l[id].ID != "" || l[out.ID].ResumedFrom != id {
		t.Fatalf("list shows the continued row or lacks resumed_from: %+v", l)
	}
	if l := listed("?all=1"); l[id].ResumedBy != out.ID || l[id].Resumable {
		t.Fatalf("all=1: %+v", l[id])
	}
	if st := apiDo(t, x.c, base, "", "GET", "/api/sessions/"+id, nil, &got); st != 200 || got.Session.Resumable || got.Session.ResumedBy != out.ID {
		t.Fatalf("continued detail: %d %+v", st, got.Session)
	}
	var dup struct {
		Error, ID string
	}
	if st := post(id, &dup); st != 409 || dup.ID != out.ID {
		t.Fatalf("resume of a continued session: %d %+v", st, dup)
	}
	// The new session is live, so resuming another session of the same conversation
	// is a 409 naming it.
	other := mk("local", KindCode, proto.StateExited, resumeCID, "default")
	if st := post(other, &dup); st != 409 || dup.ID != out.ID {
		t.Fatalf("resume while live: %d %+v", st, dup)
	}
	if err := x.th.store.SetResumedBy(other, out.ID); err != nil { // keep the chain tidy for the rest
		t.Fatal(err)
	}
	// Once the new session has stopped, it is the one to resume; the chain grows.
	x.th.store.SetSessionState(out.ID, proto.StateExited)
	var out2 struct{ ID string }
	if st := post(out.ID, &out2); st != 201 {
		t.Fatalf("resume after exit: %d", st)
	}
	x.fw.next(proto.TStartSession)
	if s2, _ := x.th.store.Session(out.ID); s2.ResumedBy != out2.ID {
		t.Fatalf("second link: %+v", s2)
	}
	rows, _ := x.th.store.AuditFor("local", 50)
	n := 0
	for _, r := range rows {
		if r.Action == "session_resume" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("session_resume audit rows: %d", n)
	}

	// Deleting the newest session brings back the one it continued, resumable again;
	// that session's own resumed_from is left alone.
	x.th.store.SetSessionState(out2.ID, proto.StateExited)
	if st := apiDo(t, x.c, base, csrf, "DELETE", "/api/sessions/"+out2.ID, nil, nil); st != 204 {
		t.Fatalf("delete: %d", st)
	}
	if s2, _ := x.th.store.Session(out.ID); s2.ResumedBy != "" || s2.ResumedFrom != id {
		t.Fatalf("after delete: %+v", s2)
	}
	if l := listed(""); !l[out.ID].Resumable || l[id].ID != "" {
		t.Fatalf("after delete list: %+v", l)
	}
}

func TestResumeChainBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id, owner, kind, cid string) {
		if err := st.CreateSession(Session{ID: id, MachineID: "m", OwnerSub: owner, CWD: "/p", ProfileName: "subscription",
			Title: "same", Kind: kind, ClaudeSessionID: cid, State: proto.StateExited}); err != nil {
			t.Fatal(err)
		}
	}
	mk("a1", "u", KindCode, resumeCID)
	mk("a2", "u", KindCode, resumeCID)
	mk("a3", "u", KindCode, resumeCID)
	mk("b1", "u", KindCode, "55555555-5555-4555-8555-555555555555") // alone
	mk("c1", "v", KindCode, resumeCID)                              // another owner
	mk("d1", "u", KindChat, resumeCID)                              // chat rows never chain
	mk("e1", "u", KindCode, "")
	mk("e2", "u", KindCode, "")
	st.Close()
	for i := 0; i < 2; i++ { // the second open must change nothing
		if st, err = OpenStore(path); err != nil {
			t.Fatal(err)
		}
		want := map[string][2]string{"a1": {"a3", ""}, "a2": {"a3", ""}, "a3": {"", "a2"}, "b1": {}, "c1": {}, "d1": {}, "e1": {}, "e2": {}}
		for id, w := range want {
			s, err := st.Session(id)
			if err != nil {
				t.Fatal(err)
			}
			if s.ResumedBy != w[0] || s.ResumedFrom != w[1] {
				t.Errorf("open %d: %s by=%q from=%q, want %q", i, id, s.ResumedBy, s.ResumedFrom, w)
			}
		}
		st.Close()
	}
}
