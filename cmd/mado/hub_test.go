package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/hub"
	"github.com/kinginu/mado/internal/proto"
)

// `mado hub remove-machine` (DESIGN §24) works on the database, like `hub enroll`.
func TestHubRemoveMachine(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hub.db")
	cfg := filepath.Join(dir, "hub.toml")
	os.WriteFile(cfg, []byte("base_url = \"https://mado.example.org\"\ndb_path = \""+db+"\"\n"), 0o600)
	st, err := hub.OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := st.CreateEnrollCode("old-box", time.Minute)
	tok, _ := st.Enroll("old-box", code)
	m, _ := st.MachineByToken(tok)
	sid := proto.NewID()
	st.CreateSession(hub.Session{ID: sid, MachineID: m.ID, OwnerSub: "u", CWD: "/p", ProfileName: "subscription", PermissionMode: "default"})
	st.UpdateSessionState(m.ID, &proto.SessionState{SessionID: sid, State: proto.StateIdle})
	st.SetMachineConnected(m.ID, true)
	st.Close()

	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := runHubRemoveMachine(args, &out)
		return out.String(), err
	}
	if _, err := run(); err == nil {
		t.Fatal("no name accepted")
	}
	if _, err := run("missing", "--config", cfg); err == nil || !strings.Contains(err.Error(), "no such machine") {
		t.Fatalf("unknown name: %v", err)
	}
	if _, err := run("old-box", "--config", cfg); err == nil || !strings.Contains(err.Error(), "connected") {
		t.Fatalf("connected worker: %v", err)
	}
	st, _ = hub.OpenStore(db)
	st.SetMachineConnected(m.ID, false)
	st.Close()
	// Offline with a live session: removed, and the session ends.
	out, err := run("--config", cfg, "old-box")
	if err != nil || !strings.Contains(out, `removed machine "old-box"`) || !strings.Contains(out, "ended 1 session(s)") || !strings.Contains(out, sid) {
		t.Fatalf("remove: %q %v", out, err)
	}
	st, _ = hub.OpenStore(db)
	defer st.Close()
	if _, err := st.MachineByToken(tok); err == nil {
		t.Fatal("token still valid")
	}
	if s, _ := st.Session(sid); s.State != proto.StateExited || s.LastError != hub.MachineRemovedReason {
		t.Fatalf("session after removal: %s %q", s.State, s.LastError)
	}
	rows, _ := st.AuditAll(10)
	if len(rows) == 0 || rows[0].Action != "machine.remove" || rows[0].User != "cli" || rows[0].Detail != "old-box ended_sessions=1" {
		t.Fatalf("audit: %+v", rows)
	}
}
