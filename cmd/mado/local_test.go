package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/hub"
)

// localHome is a temp HOME for `mado local`, short enough for the worker's socket
// paths, with a worker.toml that points tmux and claude at a no-op script (the tmux
// integration itself is covered by the worker tests) and the router at a free port.
func localHome(t *testing.T) string {
	t.Helper()
	base := "" // /tmp when it exists: the default macOS TMPDIR is too long for socket paths
	if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
		base = "/tmp"
	}
	home, err := os.MkdirTemp(base, "ml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	noop := filepath.Join(home, "noop")
	os.WriteFile(noop, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	router := ln.Addr().String()
	ln.Close()
	os.MkdirAll(filepath.Join(home, ".config/mado"), 0o700)
	os.WriteFile(filepath.Join(home, ".config/mado/worker.toml"), []byte(`tmux_bin = "`+noop+`"
claude_bin = "`+noop+`"
tmux_socket = "mado-local-test"
router_listen = "`+router+`"
`), 0o600)
	return home
}

type localRun struct {
	addr  string
	store *hub.Store
	out   bytes.Buffer
	stop  context.CancelFunc
	done  chan error
}

func startLocal(t *testing.T, args ...string) *localRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &localRun{stop: cancel, done: make(chan error, 1)}
	ready := make(chan struct{})
	env := localEnv{out: &r.out, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		openBrowser: func(string) error { t.Error("browser opened with --no-browser"); return nil },
		ready:       func(addr string, s *hub.Store) { r.addr, r.store = addr, s; close(ready) }}
	go func() { r.done <- localMain(ctx, append([]string{"--port", "0", "--no-browser"}, args...), env) }()
	select {
	case <-ready:
	case err := <-r.done:
		t.Fatalf("mado local: %v\n%s", err, r.out.String())
	case <-time.After(10 * time.Second):
		t.Fatal("mado local did not start")
	}
	return r
}

func (r *localRun) waitOnline(t *testing.T) hub.Machine {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ms, _ := r.store.Machines(); len(ms) == 1 && len(ms[0].Projects) > 0 {
			return ms[0]
		}
		time.Sleep(25 * time.Millisecond)
	}
	ms, _ := r.store.Machines()
	t.Fatalf("worker never said hello: %+v", ms)
	return hub.Machine{}
}

func (r *localRun) shutdown(t *testing.T) {
	t.Helper()
	r.stop()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("mado local did not stop")
	}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestLocalFirstRunAndWorker(t *testing.T) {
	home := localHome(t)
	r := startLocal(t)
	m := r.waitOnline(t)
	host, _ := os.Hostname()
	if m.Name != machineNameFrom(host) {
		t.Errorf("machine name %q", m.Name)
	}
	cfgDir := filepath.Join(home, ".config/mado")
	hubTOML, err := os.ReadFile(filepath.Join(cfgDir, "hub.toml"))
	if err != nil || !strings.Contains(string(hubTOML), `listen = "127.0.0.1:0"`) || !strings.Contains(string(hubTOML), `# key_ref = "aiand"`) {
		t.Errorf("hub.toml: %v\n%s", err, hubTOML)
	}
	if mode(t, filepath.Join(cfgDir, "secrets.toml")) != 0o600 || mode(t, filepath.Join(cfgDir, "machine.token")) != 0o600 {
		t.Error("secrets.toml or machine.token is not 0600")
	}
	tok, _ := os.ReadFile(filepath.Join(cfgDir, "machine.token"))
	if strings.Contains(r.out.String(), strings.TrimSpace(string(tok))) {
		t.Error("token printed")
	}
	if !strings.Contains(r.out.String(), filepath.Join(cfgDir, "hub.toml")) || !strings.Contains(r.out.String(), filepath.Join(cfgDir, "secrets.toml")) {
		t.Errorf("paths not printed: %s", r.out.String())
	}
	if mode(t, filepath.Join(home, ".local/state/mado/run")) != 0o700 {
		t.Error("runtime dir not 0700")
	}
	// The local login works on the in-process hub.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, err := c.Post("http://"+r.addr+"/local-login", "", nil); err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Errorf("local login: %v %v", err, resp)
	}
	r.shutdown(t)

	// The second run keeps the files, the token and the single machine row.
	r2 := startLocal(t)
	if m2 := r2.waitOnline(t); m2.ID != m.ID {
		t.Errorf("machine row replaced: %s -> %s", m.ID, m2.ID)
	}
	if strings.Contains(r2.out.String(), "wrote ") {
		t.Errorf("files written again: %s", r2.out.String())
	}
	if tok2, _ := os.ReadFile(filepath.Join(cfgDir, "machine.token")); string(tok2) != string(tok) {
		t.Error("token changed")
	}
	r2.shutdown(t)
}

func TestLocalRefusesNonLoopback(t *testing.T) {
	home := localHome(t)
	p := filepath.Join(home, ".config/mado/hub.toml")
	os.WriteFile(p, []byte("listen = \"0.0.0.0:8740\"\nbase_url = \"http://mac.lan:8740\"\n"), 0o600)
	var out bytes.Buffer
	err := localMain(context.Background(), []string{"--no-browser"}, localEnv{out: &out, log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback listen: %v", err)
	}
}

func TestMachineNameFrom(t *testing.T) {
	for in, want := range map[string]string{"My-MacBook-Pro.local": "My-MacBook-Pro", "": "local", "ttgpu2": "ttgpu2",
		"büro mac": "b-ro-mac", "...": "local"} {
		if got := machineNameFrom(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
