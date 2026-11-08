package worker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestSpoolGC(t *testing.T) {
	dir := t.TempDir()
	const (
		dead    = "11111111-1111-4111-8111-111111111111"
		fresh   = "22222222-2222-4222-8222-222222222222"
		live    = "33333333-3333-4333-8333-333333333333"
		running = "44444444-4444-4444-8444-444444444444"
	)
	old := time.Now().Add(-8 * 24 * time.Hour)
	write := func(name string, mtime time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
	}
	write(dead+".spool", old)
	write(dead+".token", old)
	write(fresh+".spool", time.Now().Add(-time.Hour))
	write(live+".spool", old)
	write(running+".spool", old)
	write("not-a-session.spool", old)
	write("notes.txt", old)

	ids := SpoolIDs(dir, func(id string) bool { return id == live })
	slices.Sort(ids)
	if want := []string{dead, fresh, running}; !slices.Equal(ids, want) {
		t.Fatalf("SpoolIDs = %v, want %v", ids, want)
	}
	// The hub names dead, fresh and live (it raced a resume) as dead; running is not
	// in the hint. Only dead goes: fresh is within retention, live is attached again.
	removed := CollectSpools(dir, []string{dead, fresh, live, "../../etc/passwd"},
		func(id string) bool { return id == live }, DefaultSpoolRetention, time.Now())
	if !slices.Equal(removed, []string{dead}) {
		t.Fatalf("removed %v", removed)
	}
	for name, want := range map[string]bool{dead + ".spool": false, dead + ".token": false, fresh + ".spool": true,
		live + ".spool": true, running + ".spool": true, "notes.txt": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", name, err == nil, want)
		}
	}
	// A shorter retention catches the fresh one.
	if r := CollectSpools(dir, []string{fresh}, nil, time.Minute, time.Now()); !slices.Equal(r, []string{fresh}) {
		t.Fatalf("short retention removed %v", r)
	}
}

func TestSpoolRetentionConfig(t *testing.T) {
	c := Config{HubURL: "ws://h/ws/worker", MachineName: "m", ProjectRoots: []string{"/tmp"}, RuntimeDir: "/tmp/r"}
	if err := c.normalize(); err != nil || c.Retention() != DefaultSpoolRetention {
		t.Fatalf("default: %v %v", err, c.Retention())
	}
	c.SpoolRetention = "48h"
	if err := c.normalize(); err != nil || c.Retention() != 48*time.Hour {
		t.Fatalf("48h: %v %v", err, c.Retention())
	}
	for _, bad := range []string{"7d", "1m", "-1h"} {
		c.SpoolRetention = bad
		if err := c.normalize(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The real daemon reports an old orphan spool in its sessions list and deletes it on
// the hub's gc hint.
func TestDaemonCollectsDeadSpools(t *testing.T) {
	r := newRig(t, newUpstream(t))
	orphan := "66666666-6666-4666-8666-666666666666"
	p := filepath.Join(r.cfg.SpoolDir, orphan+".spool")
	os.WriteFile(p, []byte("{}\n"), 0o600)
	old := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(p, old, old)
	keep := filepath.Join(r.cfg.SpoolDir, "77777777-7777-4777-8777-777777777777.spool")
	os.WriteFile(keep, []byte("{}\n"), 0o600) // unknown too, but fresh
	r.startDaemon()
	r.waitFor("orphan spool removed", func() bool { _, err := os.Stat(p); return os.IsNotExist(err) })
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("fresh spool was removed")
	}
}
