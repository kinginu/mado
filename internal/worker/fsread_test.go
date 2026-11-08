package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kinginu/mado/internal/proto"
)

func readCode(err error) string {
	var re *fsReadError
	if errors.As(err, &re) {
		return re.code
	}
	if err == nil {
		return ""
	}
	return "other"
}

func TestReadFileAllowList(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "root")
	cwd := filepath.Join(base, "work")
	other := filepath.Join(base, "other")
	for _, d := range []string{home, root, cwd, other, filepath.Join(root, "p", ".git", "objects", "ab"), filepath.Join(home, "dir")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, "notes.md"), "# Notes\n")
	write(filepath.Join(root, "p", "main.go"), "package main\n")
	write(filepath.Join(root, "p", ".git", "config"), "[core]\n")
	write(filepath.Join(root, "p", ".git", "objects", "ab", "cdef"), "x")
	write(filepath.Join(cwd, "README.md"), "hello")
	write(filepath.Join(cwd, "img.png"), "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	write(filepath.Join(cwd, "big.txt"), strings.Repeat("a", 2048))
	write(filepath.Join(other, "secret.txt"), "no")
	if err := os.Symlink(filepath.Join(other, "secret.txt"), filepath.Join(cwd, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "p", "main.go"), filepath.Join(cwd, "in.go")); err != nil {
		t.Fatal(err)
	}
	roots := []string{root}

	cases := []struct {
		path, code, resolved string
	}{
		{"~/notes.md", "", filepath.Join(home, "notes.md")},
		{filepath.Join(home, "notes.md"), "", filepath.Join(home, "notes.md")},
		{"README.md", "", filepath.Join(cwd, "README.md")},
		{"./README.md", "", filepath.Join(cwd, "README.md")},
		{filepath.Join(root, "p", "main.go"), "", filepath.Join(root, "p", "main.go")},
		{"../root/p/.git/config", "", filepath.Join(root, "p", ".git", "config")},
		{"in.go", "", filepath.Join(root, "p", "main.go")}, // a symlink to another allowed dir
		{"../root/p/.git/objects/ab/cdef", proto.FSCodeForbidden, ""},
		{filepath.Join(other, "secret.txt"), proto.FSCodeForbidden, ""},
		{"../other/secret.txt", proto.FSCodeForbidden, ""},
		{"~/../other/secret.txt", proto.FSCodeForbidden, ""},
		{"~root/x", proto.FSCodeForbidden, ""},
		{"escape.txt", proto.FSCodeForbidden, ""}, // symlink escape
		{"/etc/hostname", proto.FSCodeForbidden, ""},
		{"missing.md", proto.FSCodeNotFound, ""},
		{"~/dir", proto.FSCodeNotFile, ""},
		{"big.txt", proto.FSCodeTooLarge, ""},
	}
	for _, c := range cases {
		f, err := ReadFile(&proto.FSRead{Path: c.path, CWD: cwd, MaxBytes: 1024}, home, roots)
		if got := readCode(err); got != c.code {
			t.Errorf("%s: code %q (%v), want %q", c.path, got, err, c.code)
			continue
		}
		if c.code == "" && f.Path != c.resolved {
			t.Errorf("%s: resolved %q, want %q", c.path, f.Path, c.resolved)
		}
	}

	// Text, binary and download.
	f, err := ReadFile(&proto.FSRead{Path: "README.md", CWD: cwd}, home, roots)
	if err != nil || f.Text != "hello" || f.Binary || f.Size != 5 || f.Modified == "" || !strings.HasPrefix(f.Mime, "text/plain") {
		t.Errorf("README.md: %+v %v", f, err)
	}
	f, err = ReadFile(&proto.FSRead{Path: "img.png", CWD: cwd}, home, roots)
	if err != nil || !f.Binary || f.Text != "" || f.Bytes != nil || f.Mime != "image/png" {
		t.Errorf("img.png: %+v %v", f, err)
	}
	f, err = ReadFile(&proto.FSRead{Path: "img.png", CWD: cwd, Download: true, MaxBytes: proto.FSDownloadMax}, home, roots)
	if err != nil || len(f.Bytes) != 16 || f.Binary {
		t.Errorf("img.png download: %+v %v", f, err)
	}
	f, err = ReadFile(&proto.FSRead{Path: "big.txt", CWD: cwd, Download: true, MaxBytes: 4096}, home, roots)
	if err != nil || len(f.Bytes) != 2048 {
		t.Errorf("big.txt download: %v", err)
	}
	// The too_large error carries the size for the hub's message.
	_, err = ReadFile(&proto.FSRead{Path: "big.txt", CWD: cwd, MaxBytes: 1024}, home, roots)
	var re *fsReadError
	if !errors.As(err, &re) || re.size != 2048 {
		t.Errorf("too large: %v", err)
	}
	// A relative path without a cwd is refused.
	if _, err := ReadFile(&proto.FSRead{Path: "README.md"}, home, roots); readCode(err) != proto.FSCodeForbidden {
		t.Errorf("relative without cwd: %v", err)
	}
}

func TestExpandPath(t *testing.T) {
	cases := map[string]string{"~": "/h", "~/a/../b": "/h/b", "x/y.md": "/c/x/y.md", "../z": "/z", "/abs//p/": "/abs/p"}
	for in, want := range cases {
		got, err := ExpandPath(in, "/c", "/h")
		if err != nil || got != want {
			t.Errorf("ExpandPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
