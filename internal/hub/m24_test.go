package hub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
)

// M24: the file viewer (DESIGN §27).

type fileResp struct {
	status int
	header http.Header
	body   []byte
}

// fileGet requests path in the background, answers the fs_read the worker gets with
// reply (when not nil) and returns the response and the frame.
func (x *conciergeHub) fileGet(t *testing.T, c *http.Client, path string, reply func(*proto.FSRead) *proto.FSResult) (fileResp, *proto.FSRead) {
	t.Helper()
	ch := make(chan fileResp, 1)
	go func() {
		resp, err := c.Get(x.srv.URL + path)
		if err != nil {
			ch <- fileResp{}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		ch <- fileResp{resp.StatusCode, resp.Header, b}
	}()
	var m *proto.FSRead
	if reply != nil {
		m = x.fw.next(proto.TFSRead).(*proto.FSRead)
		r := reply(m)
		r.RequestID = m.RequestID
		x.fw.send(r)
	}
	select {
	case r := <-ch:
		return r, m
	case <-time.After(5 * time.Second):
		t.Fatalf("GET %s: no answer", path)
	}
	return fileResp{}, nil
}

func fsOK(t *testing.T, f proto.FSFile) *proto.FSResult {
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return &proto.FSResult{OK: true, Data: b}
}

func TestFileViewAPI(t *testing.T) {
	x := newConciergeHub(t)
	sid := proto.NewID()
	if err := x.store.CreateSession(Session{ID: sid, MachineID: x.mID, OwnerSub: "local", CWD: "/proj/a",
		ProfileName: "r", MainModel: "oc-a", Title: "code", State: proto.StateIdle}); err != nil {
		t.Fatal(err)
	}
	base := "/api/sessions/" + sid + "/file?path="

	// The worker has fs but not fs_read yet.
	if r, _ := x.fileGet(t, x.c, base+"README.md", nil); r.status != 503 || !strings.Contains(string(r.body), "too old") {
		t.Fatalf("old worker: %d %s", r.status, r.body)
	}
	x.fw.send(&proto.Hello{Machine: "box", Capabilities: []string{proto.CapFS, proto.CapFSRead}, Projects: []string{"/proj"}, Roots: []string{"/proj"}})
	time.Sleep(100 * time.Millisecond)

	// Markdown: rendered, with live path links; the frame carries the session's cwd.
	r, m := x.fileGet(t, x.c, base+url.QueryEscape("docs/a.md"), func(m *proto.FSRead) *proto.FSResult {
		return fsOK(t, proto.FSFile{Path: "/proj/a/docs/a.md", Size: 30, Modified: "2026-10-30T00:00:00Z", Mime: "text/plain; charset=utf-8",
			Text: "# Title\n\nSee `internal/x.go`.\n"})
	})
	if m.Path != "docs/a.md" || m.CWD != "/proj/a" || m.SessionID != sid || m.MaxBytes != proto.FSViewMax || m.Download {
		t.Errorf("frame %+v", m)
	}
	var v map[string]any
	if r.status != 200 || json.Unmarshal(r.body, &v) != nil {
		t.Fatalf("markdown: %d %s", r.status, r.body)
	}
	if v["kind"] != "markdown" || v["path"] != "/proj/a/docs/a.md" || v["size"] != float64(30) || v["text"] != nil ||
		!strings.Contains(v["html"].(string), "<h1>Title</h1>") || !strings.Contains(v["html"].(string), `data-path="internal/x.go"`) {
		t.Errorf("markdown view %v", v)
	}

	// Text and binary.
	r, _ = x.fileGet(t, x.c, base+"/proj/a/main.go", func(*proto.FSRead) *proto.FSResult {
		return fsOK(t, proto.FSFile{Path: "/proj/a/main.go", Size: 13, Mime: "text/plain; charset=utf-8", Text: "package main\n"})
	})
	v = nil
	if json.Unmarshal(r.body, &v); r.status != 200 || v["kind"] != "text" || v["text"] != "package main\n" || v["html"] != nil {
		t.Errorf("text: %d %s", r.status, r.body)
	}
	r, _ = x.fileGet(t, x.c, base+"/proj/a/x.png", func(*proto.FSRead) *proto.FSResult {
		return fsOK(t, proto.FSFile{Path: "/proj/a/x.png", Size: 9, Mime: "image/png", Binary: true})
	})
	v = nil
	if json.Unmarshal(r.body, &v); r.status != 200 || v["kind"] != "binary" || v["text"] != nil {
		t.Errorf("binary: %d %s", r.status, r.body)
	}

	// Errors.
	errs := []struct {
		res    *proto.FSResult
		status int
		msg    string
	}{
		{&proto.FSResult{Code: proto.FSCodeNotFound, Error: "no such file"}, 404, "no such file"},
		{&proto.FSResult{Code: proto.FSCodeForbidden, Error: "outside the allowed directories"}, 403, "outside the allowed directories"},
		{&proto.FSResult{Code: proto.FSCodeTooLarge, Error: "too large", Data: json.RawMessage(`{"path":"","size":2097152}`)}, 413, "too large to view (2048 KB); download instead"},
		{&proto.FSResult{Code: proto.FSCodeNotFile, Error: "a directory, not a file"}, 400, "a directory"},
	}
	for _, e := range errs {
		r, _ := x.fileGet(t, x.c, base+"x", func(*proto.FSRead) *proto.FSResult { return e.res })
		if r.status != e.status || !strings.Contains(string(r.body), e.msg) {
			t.Errorf("%s: %d %s", e.res.Code, r.status, r.body)
		}
	}

	// Download: the bytes as an attachment.
	r, m = x.fileGet(t, x.c, "/api/sessions/"+sid+"/file/download?path="+url.QueryEscape("~/notes.md"), func(*proto.FSRead) *proto.FSResult {
		return fsOK(t, proto.FSFile{Path: "/home/u/notes.md", Size: 4, Mime: "text/plain; charset=utf-8", Bytes: []byte("# hi")})
	})
	if !m.Download || m.MaxBytes != proto.FSDownloadMax || m.Path != "~/notes.md" {
		t.Errorf("download frame %+v", m)
	}
	if r.status != 200 || string(r.body) != "# hi" || r.header.Get("Content-Disposition") != `attachment; filename=notes.md` ||
		r.header.Get("X-Content-Type-Options") != "nosniff" || r.header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("download: %d %q %v", r.status, r.body, r.header)
	}

	// Audit rows with the session and the resolved path.
	rows, _ := x.store.AuditFor("local", 50)
	var got []string
	for _, a := range rows {
		if strings.HasPrefix(a.Action, "file_") {
			got = append(got, a.Action+" "+a.Detail)
		}
	}
	want := []string{"file_download " + sid + " /home/u/notes.md", "file_view " + sid + " /proj/a/x.png",
		"file_view " + sid + " /proj/a/main.go", "file_view " + sid + " /proj/a/docs/a.md"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("audit %q", got)
	}

	// Not the owner, a chat session, a bad path, an offline machine: no frame.
	other := x.loginAs(t, "someone", false)
	if r, _ := x.fileGet(t, other, base+"x.md", nil); r.status != 404 {
		t.Errorf("other user: %d", r.status)
	}
	chat := x.newChat(t, "", "")
	if r, _ := x.fileGet(t, x.c, "/api/sessions/"+chat+"/file?path=x.md", nil); r.status != 400 {
		t.Errorf("chat session: %d %s", r.status, r.body)
	}
	if r, _ := x.fileGet(t, x.c, base, nil); r.status != 400 {
		t.Errorf("empty path: %d", r.status)
	}
	x.fw.c.CloseNow()
	time.Sleep(100 * time.Millisecond)
	if r, _ := x.fileGet(t, x.c, base+"x.md", nil); r.status != 503 || !strings.Contains(string(r.body), "offline") {
		t.Errorf("offline: %d %s", r.status, r.body)
	}
}
