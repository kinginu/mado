package worker

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M10 (DESIGN §19.4): defaults that work on a Mac.

// shortTmp is the base of temp dirs that hold sockets: /tmp when it exists, because
// the default TMPDIR on macOS (/var/folders/…) leaves too little of the 103-byte
// socket path limit.
func shortTmp() string {
	if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
		return "/tmp"
	}
	return ""
}

// shortHome is a temp HOME short enough for socket paths (t.TempDir is too long).
func shortHome(t *testing.T) string {
	d, err := os.MkdirTemp(shortTmp(), "mh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestDefaultsWithoutXDGRuntimeDir(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	c := Config{HubURL: "ws://127.0.0.1:8740/ws/worker", MachineName: "mac", ProjectRoots: []string{home}}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.RuntimeDir != filepath.Join(home, ".local/state/mado/run") || c.SpoolDir != filepath.Join(home, ".local/state/mado/spool") {
		t.Fatalf("runtime %s spool %s", c.RuntimeDir, c.SpoolDir)
	}
	if c.TokenFile != filepath.Join(home, ".config/mado/machine.token") || c.TmuxScope != "auto" {
		t.Fatalf("token %s scope %s", c.TokenFile, c.TmuxScope)
	}
	// XDG_RUNTIME_DIR still wins where it exists.
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	c2 := Config{HubURL: c.HubURL, MachineName: "m", ProjectRoots: []string{home}}
	if err := c2.normalize(); err != nil || c2.RuntimeDir != "/run/user/1000/mado" {
		t.Fatalf("xdg: %v %s", err, c2.RuntimeDir)
	}
	// A runtime_dir that would make session sockets longer than macOS allows.
	long := "/Users/someone/" + strings.Repeat("x", MaxSocketPath-socketNameLen-len("/Users/someone/")+1)
	c3 := Config{HubURL: c.HubURL, MachineName: "m", ProjectRoots: []string{home}, RuntimeDir: long}
	if err := c3.normalize(); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("long runtime_dir: %v", err)
	}
	c3.RuntimeDir = long[:len(long)-1]
	if err := c3.normalize(); err != nil {
		t.Fatalf("runtime_dir at the limit: %v", err)
	}
}

func TestBinaryDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	none := func(string) (string, error) { return "", errors.New("not found") }
	onPath := func(n string) (string, error) { return "/opt/homebrew/bin/" + n, nil }
	if _, err := os.Stat("/usr/bin/tmux"); err != nil {
		if got := defaultTmuxBin(onPath); got != "/opt/homebrew/bin/tmux" {
			t.Errorf("tmux on PATH: %s", got)
		}
	}
	if got := defaultClaudeBin(none); got != filepath.Join(home, ".local/bin/claude") {
		t.Errorf("claude fallback: %s", got)
	}
	if got := defaultClaudeBin(onPath); got != "/opt/homebrew/bin/claude" {
		t.Errorf("claude on PATH: %s", got)
	}
	os.MkdirAll(filepath.Join(home, ".local/bin"), 0o700)
	os.WriteFile(filepath.Join(home, ".local/bin/claude"), []byte("#!/bin/sh\n"), 0o700)
	if got := defaultClaudeBin(onPath); got != filepath.Join(home, ".local/bin/claude") {
		t.Errorf("native installer first: %s", got)
	}
}

func TestNewReportsMissingBinaries(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "machine.token")
	os.WriteFile(tok, []byte("t\n"), 0o600)
	self, _ := os.Executable()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct{ tmux, claude, want string }{
		{"", self, "brew install tmux"},
		{filepath.Join(dir, "no-tmux"), self, "brew install tmux"},
		{self, filepath.Join(dir, "no-claude"), "install Claude Code"},
	}
	for _, c := range cases {
		_, err := New(&Config{TokenFile: tok, TmuxBin: c.tmux, ClaudeBin: c.claude}, log)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

func TestLocalConfig(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	path := filepath.Join(home, "worker.toml")
	// No file: the home directory is the project root.
	c, err := LocalConfig(path, "ws://127.0.0.1:8740/ws/worker", "mac", filepath.Join(home, "tok"))
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(home)
	if len(c.ProjectRoots) != 1 || c.ProjectRoots[0] != real || c.MachineName != "mac" || c.TokenFile != filepath.Join(home, "tok") {
		t.Fatalf("%+v", c)
	}
	// A worker.toml keeps its project roots and binaries; the hub side is replaced.
	proj := filepath.Join(home, "src")
	os.MkdirAll(proj, 0o700)
	os.WriteFile(path, []byte(`hub_url = "wss://elsewhere/ws/worker"
machine_name = "other"
project_roots = ["~/src"]
claude_bin = "/opt/claude"
`), 0o600)
	c, err = LocalConfig(path, "ws://127.0.0.1:9/ws/worker", "mac", filepath.Join(home, "tok"))
	if err != nil {
		t.Fatal(err)
	}
	realProj, _ := filepath.EvalSymlinks(proj)
	if c.HubURL != "ws://127.0.0.1:9/ws/worker" || c.MachineName != "mac" || c.ProjectRoots[0] != realProj || c.ClaudeBin != "/opt/claude" {
		t.Fatalf("%+v", c)
	}
	os.WriteFile(path, []byte("bogus = 1\n"), 0o600)
	if _, err := LocalConfig(path, "ws://127.0.0.1:9/ws/worker", "mac", "t"); err == nil {
		t.Error("unknown key accepted")
	}
}
