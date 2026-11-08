package render

import (
	"strings"
	"testing"
)

func TestPathLike(t *testing.T) {
	yes := []string{
		"/home/me/work/mado/DESIGN.md", "~/work/notes/HANDOFF_NOTES.md",
		"internal/hub/api.go", "./run.sh", "../x/y", "/etc/hosts", "~/a", ".github/workflows/ci.yml",
		"a/b.c", "src/foo_bar+baz@2.tsx", "docs/v1.2/notes.markdown", "x/.env", "data/model.h5",
	}
	no := []string{
		"https://example.com/a.md", "http://x/y.go", "//cdn.example.com/x.js", "a/b", "and/or", "1.2/3",
		"v1.2/3.4", "1/2.5x", "10/30", `C:\Users\x.txt`, "C:/Users/x.txt", "/", "~/", "~", "./", "a/b.toolongext",
		"foo.md", "/a//b", "mailto:x/y.md", "a/b.c d", "--config=/etc/x.toml", "key=a/b.md",
	}
	for _, s := range yes {
		if !PathLike(s) {
			t.Errorf("PathLike(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if PathLike(s) {
			t.Errorf("PathLike(%q) = true, want false", s)
		}
	}
}

func TestFindPaths(t *testing.T) {
	cases := map[string][]string{
		"see internal/hub/api.go, then (~/x/y.md).":      {"internal/hub/api.go", "~/x/y.md"},
		"at internal/hub/api.go:123: and a.go/b.go:4:5.": {"internal/hub/api.go", "a.go/b.go"},
		`"/etc/hosts" and '/tmp/x'`:                      {"/etc/hosts", "/tmp/x"},
		"`./run.sh`":                                     {"./run.sh"},
		"[/a/b] {/c/d} </e/f>":                           {"/a/b", "/c/d", "/e/f"},
		"https://go.dev/doc/x.html and 1.2/3 and a/b":    nil,
		"~/work/DESIGN.mdを更新しました、/tmp/x.txt。":            {"~/work/DESIGN.md", "/tmp/x.txt"},
		"two\tpaths\n/a/b.go\u00a0/c/d.go":               {"/a/b.go", "/c/d.go"},
	}
	for in, want := range cases {
		var got []string
		for _, h := range FindPaths(in) {
			got = append(got, in[h[0]:h[1]])
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("FindPaths(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMarkdownLinksPaths(t *testing.T) {
	in := "Read ~/work/notes/HANDOFF_NOTES.md and `internal/hub/api.go`.\n" +
		"Also `cat ./a/b.md | wc` and **/etc/hosts**.\n\n" +
		"```\n/etc/passwd in a fence\n```\n\n" +
		"    /indented/code.go\n\n" +
		"[the doc](https://example.com/x/y.md), [/a/b.md](/a/b.md), https://go.dev/x/y.go, <https://h/z.md>\n\n" +
		"| file |\n|---|\n| docs/x.md |\n\n" +
		"# Heading /h/x.md\n\n" +
		"quote: \"/q/\\\"x.md\" and /x/a&b.md\n"
	out := string(Markdown(in))
	wants := []string{
		`Read <a class="fpath" href="#" data-path="~/work/notes/HANDOFF_NOTES.md">~/work/notes/HANDOFF_NOTES.md</a> and `,
		`<code><a class="fpath" href="#" data-path="internal/hub/api.go">internal/hub/api.go</a></code>.`,
		`<code>cat <a class="fpath" href="#" data-path="./a/b.md">./a/b.md</a> | wc</code>`,
		`<strong><a class="fpath" href="#" data-path="/etc/hosts">/etc/hosts</a></strong>`,
		"<pre><code>/etc/passwd in a fence\n</code></pre>",
		"<pre><code>/indented/code.go\n</code></pre>",
		`<td><a class="fpath" href="#" data-path="docs/x.md">docs/x.md</a></td>`,
		`<h1>Heading <a class="fpath" href="#" data-path="/h/x.md">/h/x.md</a></h1>`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	if n := strings.Count(out, `class="fpath"`); n != 6 {
		t.Errorf("%d fpath links, want 6:\n%s", n, out)
	}
	for _, bad := range []string{`data-path="https`, `data-path="/a/b.md"`, `data-path="/q/`, `data-path="/x/a`, `data-path="/etc/passwd`} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q:\n%s", bad, out)
		}
	}
}

// The attribute value is escaped; nothing a path may contain closes it.
func TestMarkdownPathEscaping(t *testing.T) {
	out := string(Markdown("/a/b%22onclick.md and `/x/\"><script>.md`"))
	if strings.Contains(out, "<script") || strings.Contains(out, `"onclick`) {
		t.Fatalf("unsafe output: %s", out)
	}
	if !strings.Contains(out, `data-path="/a/b%22onclick.md"`) {
		t.Errorf("output lacks the %%-path: %s", out)
	}
}
