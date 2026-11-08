package worker

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kinginu/mado/internal/hub"
)

const shipSID = "0f1e2d3c-4b5a-4968-8776-655443322110"

// The real daemon ships ~/.claude and per-profile transcripts, holds back a truncated
// last line, sends only the tail after a change, and starts over when a file shrinks.
func TestHistoryShipping(t *testing.T) {
	r := newRig(t, newUpstream(t))
	home := filepath.Join(filepath.Dir(r.cfg.SpoolDir), "claude-home")
	r.cfg.ClaudeHistory, r.cfg.ClaudeHome = true, home
	write := func(root, proj, content string) string {
		dir := filepath.Join(root, "projects", proj)
		os.MkdirAll(dir, 0o700)
		p := filepath.Join(dir, shipSID+".jsonl")
		os.WriteFile(p, []byte(content), 0o600)
		return p
	}
	fixture, err := os.ReadFile("../render/testdata/write_tool.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	mine := write(home, "-home-me-proj", `{"type":"user","message":{"content":"first prompt"},"cwd":"/home/me/proj"}`+"\n"+
		`{"type":"weird-new-type"}`+"\n"+"not json\n"+`{"type":"assistant","message":{"content":[{"type":"te`)
	prof := write(filepath.Join(r.cfg.ClaudeConfigBase, "claude-r"), "-x", string(fixture))
	// Bigger than several frames (historyBatchBytes), so frames must chain.
	var big strings.Builder
	for i := 0; big.Len() < 3*historyBatchBytes+1000; i++ {
		big.WriteString(`{"type":"assistant","n":` + strconv.Itoa(i) + `,"pad":"` + strings.Repeat("y", 900) + `"}` + "\n")
	}
	bigPath := write(home, "-big", big.String())

	r.startDaemon()
	files := func() map[string]*hub.HistoryFile {
		fs, _ := r.store.HistoryFiles("", 100)
		out := map[string]*hub.HistoryFile{}
		for _, f := range fs {
			out[f.Path] = f
		}
		return out
	}
	r.waitFor("three transcripts", func() bool { return len(files()) == 3 })
	r.waitFor("the big one complete", func() bool { return files()[bigPath].Offset == int64(big.Len()) })
	if b := files()[bigPath]; b.Lines != int64(strings.Count(big.String(), "\n")) || b.BadLines != 0 {
		t.Fatalf("big transcript: %+v", b)
	}
	fs := files()
	m, p := fs[mine], fs[prof]
	if m.Lines != 3 || m.BadLines != 1 || m.Title != "first prompt" || m.CWD != "/home/me/proj" || !m.DefaultConfig {
		t.Fatalf("mine: %+v", m)
	}
	if p.Lines != 6 || p.DefaultConfig || p.Offset != int64(len(fixture)) {
		t.Fatalf("profile transcript: %+v", p)
	}
	before := m.Offset

	// Finish the truncated line and add one; a rescan ships only the tail.
	f, _ := os.OpenFile(mine, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`xt","text":"done"}]}}` + "\n" + `{"type":"custom-title","customTitle":"renamed"}` + "\n")
	f.Close()
	rescan := func() {
		ms, _ := r.store.Machines()
		if code, out := r.api("POST", "/api/admin/history/rescan", map[string]string{"machine": ms[0].ID}); code != 200 {
			t.Fatalf("rescan %d %s", code, out)
		}
	}
	rescan()
	r.waitFor("tail", func() bool { return files()[mine].Lines == 5 })
	if m := files()[mine]; m.Title != "renamed" || m.Offset <= before {
		t.Fatalf("after tail: %+v", m)
	}
	if !strings.Contains(r.get("/api/admin/history/"+itoa(files()[mine].ID)), "done") {
		t.Fatal("tail line not rendered")
	}

	// The file is replaced by a shorter one: the hub's copy starts over.
	os.WriteFile(mine, []byte(`{"type":"user","message":{"content":"new"}}`+"\n"), 0o600)
	rescan()
	r.waitFor("reset", func() bool { m := files()[mine]; return m.Lines == 1 && m.Title == "new" })

}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
