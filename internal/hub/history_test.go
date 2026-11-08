package hub

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
)

const histSID = "0f1e2d3c-4b5a-4968-8776-655443322110"

// histFrame builds a history_session from raw lines laid out back to back from offset.
func histFrame(path string, offset int64, lines ...string) *proto.HistorySession {
	m := &proto.HistorySession{Path: path, ConfigDir: "/home/me/.claude", DefaultConfig: true, ClaudeSessionID: histSID,
		CWD: "/proj/a/sub", MTime: time.Now().UnixNano(), Offset: offset}
	off := offset
	for _, l := range lines {
		m.Lines = append(m.Lines, l)
		m.LineOffsets = append(m.LineOffsets, off)
		off += int64(len(l)) + 1
	}
	m.End, m.Size = off, off
	return m
}

func TestHistoryImportAndPages(t *testing.T) {
	x := newM2(t)
	x.fw.send(&proto.Hello{Machine: "box", Capabilities: []string{"history"}, Projects: []string{"/proj/a", "/proj/b"}})
	if o := x.fw.next(proto.THistoryOffsets).(*proto.HistoryOffsets); len(o.Files) != 0 || o.Partial {
		t.Fatalf("first offsets: %+v", o)
	}
	path := "/home/me/.claude/projects/-proj-a-sub/" + histSID + ".jsonl"
	f1 := histFrame(path, 0,
		`{"type":"permission-mode","permissionMode":"default"}`,
		`{"type":"user","message":{"role":"user","content":"fix the <b>flaky</b> test"},"cwd":"/proj/a/sub"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"on it"}]}}`,
		`{"type":"brand-new-entry","payload":{"x":1}}`,
		`this line is not json`,
		"\x00\x00\x00"+`{"type":"assistant","message":{"content":[{"type":"text","text":"after the crash"}]}}`)
	f1.Title = "fix the <b>flaky</b> test"
	x.fw.send(f1)
	time.Sleep(150 * time.Millisecond)
	files, _ := x.th.store.HistoryFiles("", 10)
	if len(files) != 1 || files[0].Lines != 6 || files[0].BadLines != 2 || files[0].Offset != f1.End || files[0].Title != f1.Title {
		t.Fatalf("stored: %+v", files)
	}
	id := strconv.FormatInt(files[0].ID, 10)

	// A frame that does not continue the stored offset is refused with the hub's state.
	x.fw.send(histFrame(path, 3, `{"type":"x"}`))
	o := x.fw.next(proto.THistoryOffsets).(*proto.HistoryOffsets)
	if !o.Partial || len(o.Files) != 1 || o.Files[0].Offset != f1.End {
		t.Fatalf("partial offsets: %+v", o)
	}
	// The tail continues; a custom title replaces the first prompt.
	f2 := histFrame(path, f1.End, `{"type":"custom-title","customTitle":"flaky-fix"}`)
	f2.Title, f2.TitleCustom = "flaky-fix", true
	x.fw.send(f2)
	// A later first-prompt title does not undo it.
	f3 := histFrame(path, f2.End, `{"type":"user","message":{"content":"later prompt"}}`)
	f3.Title = "later prompt"
	x.fw.send(f3)
	time.Sleep(150 * time.Millisecond)
	if f, _ := x.th.store.HistoryFile(files[0].ID); f.Title != "flaky-fix" || f.Lines != 8 {
		t.Fatalf("after tail: %+v", f)
	}
	// Malformed frames are dropped.
	bad := histFrame("relative/path.jsonl", 0, `{}`)
	x.fw.send(bad)
	bad2 := histFrame(path, f3.End, `{}`, `{}`)
	bad2.LineOffsets[1] = bad2.LineOffsets[0]
	x.fw.send(bad2)
	time.Sleep(100 * time.Millisecond)
	if n, _, _, _ := x.th.store.HistoryTotals(); n != 1 {
		t.Fatalf("%d files", n)
	}

	type histList struct {
		Groups []struct {
			MachineID string `json:"machine_id"`
			Online    bool
			Files     []apiHistoryFile
		}
		Count  int
		Totals map[string]int64
	}
	list := func(q string) histList {
		var l histList
		if st := apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/admin/history"+q, nil, &l); st != 200 {
			t.Fatalf("list %s: %d", q, st)
		}
		return l
	}
	if l := list(""); l.Count != 1 || !l.Groups[0].Online || l.Groups[0].Files[0].Title != "flaky-fix" ||
		l.Groups[0].Files[0].CWD != "/proj/a/sub" || l.Totals["files"] != 1 || l.Totals["bad"] != 2 {
		t.Fatalf("list: %+v", l)
	}
	if l := list("?q=nomatch"); l.Count != 0 {
		t.Fatal("search did not filter")
	}
	if l := list("?q=a%2Fsub"); l.Count != 1 {
		t.Fatal("search by cwd failed")
	}
	if l := list("?q=%25"); l.Count != 0 {
		t.Fatal("% is not escaped in search")
	}
	var page struct {
		File   apiHistoryFile
		Online bool
		Blocks []apiBlock
		First  bool
		More   bool
	}
	if st := apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/admin/history/"+id, nil, &page); st != 200 || !page.First || page.More ||
		page.File.BadLines != 2 || !page.File.DefaultConfig || !page.Online {
		t.Fatalf("file: %d %+v", st, page)
	}
	pj, _ := json.Marshal(page.Blocks)
	for _, want := range []string{`"text":"fix the \u003cb\u003eflaky\u003c/b\u003e test"`, "on it", `"label":"brand-new-entry"`,
		`"label":"unparsable"`, "this line is not json", `"html":"\u003cp\u003eafter the crash\u003c/p\u003e"`} {
		if !strings.Contains(string(pj), want) {
			t.Errorf("history blocks lack %s", want)
		}
	}
	if strings.Contains(string(pj), "\u003cb\u003eflaky\u003c/b\u003e test\u003c/p") {
		t.Error("transcript html not escaped")
	}
	if st := apiDo(t, x.c, x.th.srv.URL, "", "GET", "/api/admin/history/999", nil, nil); st != 404 {
		t.Errorf("missing file: %d", st)
	}

	// Reset: the file shrank on the worker; the hub drops what it had.
	f4 := histFrame(path, 0, `{"type":"user","message":{"content":"fresh start"}}`)
	f4.Reset, f4.Title = true, "fresh start"
	x.fw.send(f4)
	time.Sleep(150 * time.Millisecond)
	if f, _ := x.th.store.HistoryFile(files[0].ID); f.Lines != 1 || f.Title != "fresh start" || f.Offset != f4.End || f.BadLines != 0 {
		t.Fatalf("after reset: %+v", f)
	}

	// The next connection gets the stored offsets.
	x.fw.send(&proto.Hello{Machine: "box", Capabilities: []string{"history"}, Projects: []string{"/proj/a"}})
	if o := x.fw.next(proto.THistoryOffsets).(*proto.HistoryOffsets); len(o.Files) != 1 || o.Files[0].Path != path || o.Files[0].Offset != f4.End {
		t.Fatalf("reconnect offsets: %+v", o)
	}

	// Rescan button.
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/admin/history/rescan", map[string]string{"machine": x.mID}, nil); st != 200 {
		t.Fatalf("rescan: %d", st)
	}
	x.fw.next(proto.THistoryRescan)
	if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/admin/history/rescan", map[string]string{"machine": "nope"}, nil); st != 503 {
		t.Errorf("rescan offline: %d", st)
	}

	// Resume in mado: a subscription session resuming that claude session in its cwd.
	var created struct{ ID string }
	code := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/admin/history/"+id+"/resume", nil, &created)
	st := x.fw.next(proto.TStartSession).(*proto.StartSession)
	if st.ResumeClaudeSessionID != histSID || st.CWD != "/proj/a/sub" || st.Profile.Kind != proto.KindSubscription ||
		code != 201 || created.ID != st.SessionID {
		t.Fatalf("resume: %+v %d %+v", st, code, created)
	}
	if s, _ := x.th.store.Session(st.SessionID); s.ClaudeSessionID != histSID || !strings.HasPrefix(s.Title, "resume: ") {
		t.Fatalf("resumed session row: %+v", s)
	}
}

func TestHistoryResumeRules(t *testing.T) {
	x := newM2(t)
	mk := func(path, cwd string, def bool) string {
		f := histFrame(path, 0, `{"type":"user","message":{"content":"hi"}}`)
		f.CWD, f.DefaultConfig = cwd, def
		x.fw.send(f)
		time.Sleep(100 * time.Millisecond)
		fs, _ := x.th.store.HistoryFiles("", 10)
		for _, hf := range fs {
			if hf.Path == path {
				return strconv.FormatInt(hf.ID, 10)
			}
		}
		t.Fatal("not stored")
		return ""
	}
	profileFile := mk("/home/me/.mado/claude-r/projects/-proj-a/"+histSID+".jsonl", "/proj/a", false)
	outside := mk("/home/me/.claude/projects/-etc/"+histSID+".jsonl", "/etc", true)
	sneaky := mk("/home/me/.claude/projects/-proj-ab/"+histSID+".jsonl", "/proj/ab", true)
	dotdot := mk("/home/me/.claude/projects/-proj-a--/"+histSID+".jsonl", "/proj/a/../../etc", true)
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	for name, id := range map[string]string{"profile dir": profileFile, "outside": outside, "prefix": sneaky, "dotdot": dotdot} {
		var e struct{ Error string }
		if st := apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/admin/history/"+id+"/resume", nil, &e); st != 400 || e.Error == "" {
			t.Errorf("%s: resume allowed (%d %q)", name, st, e.Error)
		}
	}
	x.fw.quiet(200 * time.Millisecond)

	// Non-admins get nothing.
	if err := x.th.store.UpsertUser(User{Sub: "plain", Email: "p@example.org"}); err != nil {
		t.Fatal(err)
	}
	sid, _ := x.th.store.CreateWebSession("plain", time.Hour)
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(x.th.srv.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: cookieSession, Value: sid}})
	plain := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if s := status(t, plain, x.th.srv.URL+"/"); s != 200 {
		t.Fatalf("plain user index: %d", s)
	}
	for _, p := range []string{"/api/admin/history", "/api/admin/history/" + outside} {
		if s := status(t, plain, x.th.srv.URL+p); s != http.StatusForbidden {
			t.Errorf("plain user %s: %d", p, s)
		}
	}
	var me struct {
		IsAdmin bool `json:"is_admin"`
	}
	apiDo(t, plain, x.th.srv.URL, "", "GET", "/api/me", nil, &me)
	if me.IsAdmin {
		t.Error("plain user is an admin for the SPA (it would show the admin links)")
	}
}
