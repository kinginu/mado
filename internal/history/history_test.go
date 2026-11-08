package history

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sid = "0f1e2d3c-4b5a-4968-8776-655443322110"

// synthetic is shaped like a real transcript (Claude Code 2.1.302): metadata entries,
// a command wrapper, the first real prompt, unknown entry types, a bad line, and a
// truncated last line that is still being written.
var synthetic = strings.Join([]string{
	`{"type":"permission-mode","permissionMode":"default","sessionId":"` + sid + `"}`,
	`{"type":"user","isMeta":true,"message":{"role":"user","content":"Caveat: local commands"},"cwd":"/home/me/my app"}`,
	`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
	`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"  fix the\nflaky test  "}]},"cwd":"/home/me/other"}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`,
	`{"type":"brand-new-entry","payload":{"x":1}}`,
	`this line is not json`,
	``,
	`{"type":"attachment","attachment":{"type":"skill_listing"}}`,
	`{"type":"custom-title","customTitle":"flaky-test-fix","sessionId":"` + sid + `"}`,
}, "\n") + "\n" + `{"type":"assistant","message":{"role":"assis`

func writeTranscript(t *testing.T, root, projDir, content string) string {
	t.Helper()
	dir := filepath.Join(root, "projects", projDir)
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// collect reads a tail and checks that batches chain: each starts where the previous
// one ended, every line lies inside its batch, and the last batch ends at the result.
func collect(t *testing.T, path string, off int64, maxLine, batch int) ([]Line, int, int64) {
	t.Helper()
	var all []Line
	batches := 0
	prevEnd := off
	end, err := ReadTail(path, off, maxLine, batch, func(ls []Line, bend int64) error {
		batches++
		if ls[0].Off < prevEnd {
			t.Errorf("batch %d starts at %d, before the previous end %d", batches, ls[0].Off, prevEnd)
		}
		last := ls[len(ls)-1]
		if bend <= last.Off || bend < last.Off+int64(len(last.Raw)) {
			t.Errorf("batch %d ends at %d, not past its last line at %d", batches, bend, last.Off)
		}
		prevEnd = bend
		all = append(all, ls...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if batches > 0 && prevEnd != end {
		t.Errorf("last batch ends at %d, ReadTail returned %d", prevEnd, end)
	}
	return all, batches, end
}

func TestScanAndSyntheticTranscript(t *testing.T) {
	root := t.TempDir()
	p := writeTranscript(t, root, "-home-me-my-app", synthetic)
	// Things Scan must skip.
	os.WriteFile(filepath.Join(root, "projects", "-home-me-my-app", "notes.jsonl"), []byte("{}\n"), 0o600)
	os.MkdirAll(filepath.Join(root, "projects", "-home-me-my-app", sid, "subagents"), 0o700)
	os.WriteFile(filepath.Join(root, "projects", "-home-me-my-app", sid, "subagents", "agent-1.jsonl"), []byte("{}\n"), 0o600)
	os.Symlink(p, filepath.Join(root, "projects", "-home-me-my-app", "11111111-1111-4111-8111-111111111111.jsonl"))

	fs := Scan([]string{root, filepath.Join(root, "missing")})
	if len(fs) != 1 || fs[0].Path != p || fs[0].SessionID != sid || fs[0].ProjectDir != "-home-me-my-app" || fs[0].Size != int64(len(synthetic)) {
		t.Fatalf("scan: %+v", fs)
	}
	if DecodeProjectDir("-home-me-my-app") != "/home/me/my/app" {
		t.Error("decode")
	}

	lines, _, end := collect(t, p, 0, 1<<20, 1<<20)
	if len(lines) != 9 { // 10 lines minus the empty one; the truncated tail is held back
		t.Fatalf("%d lines", len(lines))
	}
	if want := int64(strings.LastIndex(synthetic, "\n") + 1); end != want {
		t.Fatalf("end %d, want %d", end, want)
	}
	for _, l := range lines {
		if !bytes.Equal(l.Raw, []byte(synthetic[l.Off:l.Off+int64(len(l.Raw))])) {
			t.Fatalf("offset %d does not point at its line", l.Off)
		}
	}
	if string(lines[6].Raw) != "this line is not json" {
		t.Errorf("bad line not kept raw: %q", lines[6].Raw)
	}
	var m Meta
	m.Scan(lines[:5])
	if m.CWD != "/home/me/my app" || m.Title != "fix the flaky test" || m.TitleCustom {
		t.Fatalf("meta from the first lines: %+v", m)
	}
	m.Scan(lines[5:])
	if m.Title != "flaky-test-fix" || !m.TitleCustom || m.CWD != "/home/me/my app" {
		t.Fatalf("meta: %+v", m)
	}

	// The writer finishes the last line: the next read starts at end and gets only it.
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`tant","content":[]}}` + "\n")
	f.Close()
	more, _, end2 := collect(t, p, end, 1<<20, 1<<20)
	if len(more) != 1 || more[0].Off != end || !json.Valid(more[0].Raw) {
		t.Fatalf("tail: %+v", more)
	}
	if fi, _ := os.Stat(p); end2 != fi.Size() {
		t.Fatalf("end2 %d size %d", end2, fi.Size())
	}
}

func TestReadTailBatchesAndOversize(t *testing.T) {
	root := t.TempDir()
	big := `{"type":"user","message":{"content":"` + strings.Repeat("x", 5000) + `"}}`
	content := `{"a":1}` + "\n" + big + "\n" + `{"b":2}` + "\r\n" + `{"c":3}` + "\n"
	p := writeTranscript(t, root, "-p", content)
	lines, _, end := collect(t, p, 0, 1000, 1<<20)
	if len(lines) != 4 || end != int64(len(content)) {
		t.Fatalf("%d lines end %d", len(lines), end)
	}
	var o struct {
		Type  string
		Bytes int
	}
	if json.Unmarshal(lines[1].Raw, &o); o.Type != "mado_oversize" || o.Bytes != len(big) {
		t.Fatalf("oversize placeholder: %s", lines[1].Raw)
	}
	if string(lines[2].Raw) != `{"b":2}` || lines[3].Off != int64(len(content)-8) {
		t.Fatalf("after the big line: %q at %d", lines[2].Raw, lines[3].Off)
	}
	// Small batches: every line in order, none lost.
	small, batches, _ := collect(t, p, 0, 1<<20, 10)
	if len(small) != 4 || batches != 4 {
		t.Fatalf("%d lines in %d batches", len(small), batches)
	}
}

// The recorded stream-json fixture is not a transcript, but its lines are the same
// kind of objects; they all come through unchanged.
func TestRecordedFixtureLines(t *testing.T) {
	b, err := os.ReadFile("../render/testdata/write_tool.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	p := writeTranscript(t, root, "-x", string(b))
	lines, _, end := collect(t, p, 0, 1<<20, 1<<20)
	if len(lines) != 6 || end != int64(len(b)) {
		t.Fatalf("%d lines, end %d of %d", len(lines), end, len(b))
	}
	var m Meta
	m.Scan(lines)
	if m.CWD == "" {
		t.Error("no cwd from system/init")
	}
}
