package proto

import "testing"

func TestFSFramesRoundTrip(t *testing.T) {
	for _, f := range []Frame{&FSInspect{RequestID: "r", Kind: InspectDir, Path: "/p"},
		&FSPrepare{RequestID: "r", Action: ActionWorktree, Path: "/p/w", Repo: "/p/r", Branch: "b", Base: "main"},
		&FSResult{RequestID: "r", OK: true, Data: []byte(`{"a":1}`)}} {
		b, err := Encode(f)
		if err != nil {
			t.Fatal(err)
		}
		g, err := Decode(b)
		if err != nil || HeaderOf(g).T != HeaderOf(f).T {
			t.Fatalf("%T: %v %v", f, g, err)
		}
	}
}

func TestInsideAndNames(t *testing.T) {
	roots := []string{"/home/u/work", "/srv"}
	for p, want := range map[string]bool{"/home/u/work": true, "/home/u/work/x/y": true, "/srv/a": true,
		"/home/u/workshop": false, "/home/u/work/../etc": false, "/home/u/work/": false, "home/u/work/x": false,
		"/etc": false, "": false, "/home/u/work//x": false, "/home/u/work/a\nb": false} {
		if Inside(p, roots) != want {
			t.Errorf("Inside(%q) = %v", p, !want)
		}
	}
	for s, want := range map[string]bool{"main": true, "feature/x-1": true, "v1.2": true, "-b": false, "a..b": false,
		"a//b": false, "a/": false, "a.lock": false, "a b": false, "a@{u}": false, "a/.b": false, "": false, "a.": false} {
		if ValidRefName(s) != want {
			t.Errorf("ValidRefName(%q) = %v", s, !want)
		}
	}
	for s, want := range map[string]bool{"https://github.com/o/r.git": true, "git@github.com:o/r.git": true,
		"ssh://git@host:2222/o/r.git": true, "ssh://host/r": true,
		"http://github.com/o/r": false, "file:///etc": false, "https://u:p@github.com/o/r": false, "https://tok@github.com/o/r": false,
		"ssh://u:p@host/r": false, "-uhttps://x/y": false, "https://github.com": false, "https://h/r?x=1": false,
		"git@host:-oProxyCommand=x": false, "/local/repo": false, "ext::sh -c x": false, "https://h/a b": false} {
		if ValidGitURL(s) != want {
			t.Errorf("ValidGitURL(%q) = %v", s, !want)
		}
	}
}
