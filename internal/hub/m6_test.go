package hub

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/chat"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/usage"
)

// fakeLLM is a Messages API upstream. hold, when set, keeps a stream open after its
// first text delta until released.
type fakeLLM struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []map[string]any
	hold chan struct{}
	// script, when not empty, is answered in order (one SSE body per request).
	script []string
}

func chatStream(id, text string) string {
	var b strings.Builder
	ev := func(t, d string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", t, d) }
	ev("message_start", `{"type":"message_start","message":{"id":"`+id+`","type":"message","role":"assistant","model":"zai-org/glm-5.3","content":[],"usage":{"input_tokens":72,"output_tokens":1}}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	for _, w := range strings.SplitAfter(text, " ") {
		j, _ := json.Marshal(w)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(j)+`}}`)
	}
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1000000,"cache_read_input_tokens":1000000,"output_tokens":500000}}`)
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

func newFakeLLM(t *testing.T) *fakeLLM {
	f := &fakeLLM{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		n := len(f.reqs)
		hold := f.hold
		var scripted string
		if len(f.script) > 0 {
			scripted, f.script = f.script[0], f.script[1:]
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		body := chatStream(fmt.Sprintf("msg_%d", n), "**Hello** from the model")
		if scripted != "" {
			io.WriteString(w, scripted)
			return
		}
		if hold != nil {
			cut := strings.Index(body, `"text":"from `)
			cut = strings.LastIndex(body[:cut], "event:")
			io.WriteString(w, body[:cut])
			w.(http.Flusher).Flush()
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
			io.WriteString(w, body[cut:])
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLLM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeLLM) req(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[i]
}

type chatHub struct {
	*testHub
	c    *http.Client
	csrf string
	llm  *fakeLLM
	dir  string
}

func newChatHub(t *testing.T) *chatHub {
	t.Helper()
	llm := newFakeLLM(t)
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	vision := true
	cfg := &Config{BaseURL: srv.URL, Listen: "127.0.0.1:0",
		Chat: ChatConfig{Enabled: true, DataDir: filepath.Join(dir, "files"), MaxFileMB: 1, UserQuotaMB: 2, MaxToolRounds: 8,
			Models: []ChatModelConfig{
				{Name: "glm-5.3", Label: "GLM 5.3", BaseURL: llm.srv.URL, Model: "zai-org/glm-5.3", KeyRef: "aiand", Default: true},
				{Name: "glm-flash", BaseURL: llm.srv.URL, Model: "zai-org/glm-5.3-flash", KeyRef: "aiand", Vision: vision},
			}},
		Prices: usage.Prices{{Model: "zai-org/glm-*", Input: 0.6, Output: 2.2, CacheRead: 0.1}}}
	if err := cfg.Chat.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h, err := New(cfg, store, &Secrets{Keys: map[string]string{"aiand": "sk-test"}}, Options{LocalLogin: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	handler = h.Handler()
	x := &chatHub{testHub: &testHub{h: h, srv: srv, store: store}, c: browser(), llm: llm, dir: dir}
	x.c.Post(srv.URL+"/local-login", "", nil)
	var me struct{ CSRF string }
	x.getJSON(t, "/api/me", &me)
	x.csrf = me.CSRF
	return x
}

func (x *chatHub) getJSON(t *testing.T, path string, v any) int {
	t.Helper()
	resp, err := x.c.Get(x.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v != nil {
		json.NewDecoder(resp.Body).Decode(v)
	}
	return resp.StatusCode
}

// do sends a JSON API request with the Origin and CSRF headers.
func (x *chatHub) do(t *testing.T, method, path string, body any, v any) int {
	t.Helper()
	return apiDo(t, x.c, x.srv.URL, x.csrf, method, path, body, v)
}

func (x *chatHub) upload(t *testing.T, sid, name string, data []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("session_id", sid)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", x.srv.URL+"/api/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", x.srv.URL)
	req.Header.Set("X-CSRF-Token", x.csrf)
	resp, err := x.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (x *chatHub) newChat(t *testing.T, model, system string) string {
	t.Helper()
	var out struct{ ID string }
	if st := x.do(t, "POST", "/api/sessions", map[string]any{"kind": "chat", "model": model, "system": system}, &out); st != 201 {
		t.Fatalf("create chat: %d", st)
	}
	return out.ID
}

func TestAPIAuthOriginCSRF(t *testing.T) {
	x := newChatHub(t)
	if st := (&chatHub{testHub: x.testHub, c: browser()}).getJSON(t, "/api/me", nil); st != 401 {
		t.Errorf("anonymous /api/me: %d", st)
	}
	body := `{"kind":"chat"}`
	for _, c := range []struct {
		name, origin, csrf string
		want               int
	}{
		{"no origin", "", x.csrf, 403},
		{"foreign origin", "https://evil.example", x.csrf, 403},
		{"no csrf", x.srv.URL, "", 403},
		{"wrong csrf", x.srv.URL, "nope", 403},
		{"ok", x.srv.URL, x.csrf, 201},
	} {
		req, _ := http.NewRequest("POST", x.srv.URL+"/api/sessions", strings.NewReader(body))
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.csrf != "" {
			req.Header.Set("X-CSRF-Token", c.csrf)
		}
		resp, _ := x.c.Do(req)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d", c.name, resp.StatusCode)
		}
	}
}

func TestChatSessionEndToEnd(t *testing.T) {
	x := newChatHub(t)
	var models struct {
		Models []struct {
			Name      string
			Default   bool
			Vision    bool
			Documents bool
		}
	}
	x.getJSON(t, "/api/chat/models", &models)
	if len(models.Models) != 2 || models.Models[0].Name != "glm-5.3" || !models.Models[0].Default ||
		!models.Models[1].Vision || !models.Models[1].Documents {
		t.Fatalf("models %+v", models)
	}
	if st := x.do(t, "POST", "/api/sessions", map[string]any{"kind": "chat", "model": "nope"}, nil); st != 400 {
		t.Errorf("unknown model: %d", st)
	}
	if st := x.do(t, "POST", "/api/sessions", map[string]any{"kind": "chat", "system": strings.Repeat("x", 4001)}, nil); st != 400 {
		t.Errorf("long system prompt: %d", st)
	}
	sid := x.newChat(t, "", "Answer in haiku.")
	var sv struct {
		Session apiSessionView
	}
	x.getJSON(t, "/api/sessions/"+sid, &sv)
	if sv.Session.Kind != "chat" || sv.Session.Model != "glm-5.3" || sv.Session.System != "Answer in haiku." || sv.Session.State != "idle" {
		t.Fatalf("session %+v", sv.Session)
	}

	// Live viewer in JSON mode.
	sseReq, _ := http.NewRequest("GET", x.srv.URL+"/s/"+sid+"/events?format=json", nil)
	sseResp, err := x.c.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	defer sseResp.Body.Close()
	events := make(chan [2]string, 100)
	go func() {
		sc := bufio.NewScanner(sseResp.Body)
		ev := ""
		for sc.Scan() {
			l := sc.Text()
			if v, ok := strings.CutPrefix(l, "event: "); ok {
				ev = v
			} else if v, ok := strings.CutPrefix(l, "data: "); ok {
				events <- [2]string{ev, v}
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)

	// Upload a text file; the client's content type is ignored.
	st, f := x.upload(t, sid, "notes.md", []byte("# Notes\nremember the milk"))
	if st != 201 || f["mime"] != "text/markdown; charset=utf-8" {
		t.Fatalf("upload %d %v", st, f)
	}
	fid := f["id"].(string)
	var sent struct{ Seq int64 }
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "summarise", "file_ids": []string{fid}}, &sent); st != 202 || sent.Seq != 1 {
		t.Fatalf("send %d %+v", st, sent)
	}
	x.h.runner.Wait(sid)

	// The request: bearer key, system with the session's prompt, the document first.
	req := x.llm.req(0)
	if !strings.HasSuffix(req["system"].(string), "Answer in haiku.") || req["model"] != "zai-org/glm-5.3" || req["max_tokens"].(float64) != 32000 {
		t.Fatalf("request %v", req)
	}
	content := req["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if !strings.Contains(content[0].(map[string]any)["text"].(string), `<document name="notes.md">`) {
		t.Fatalf("content %v", content)
	}

	// Blocks JSON.
	var bl struct {
		Blocks []apiBlock
		Last   int64
	}
	x.getJSON(t, "/api/sessions/"+sid+"/blocks?after=0", &bl)
	var kinds []string
	for _, b := range bl.Blocks {
		kinds = append(kinds, b.Kind)
	}
	if strings.Join(kinds, ",") != "file,user,assistant,result" || bl.Last != 3 {
		t.Fatalf("blocks %v last %d", kinds, bl.Last)
	}
	if bl.Blocks[0].FileID != fid || strings.TrimSpace(bl.Blocks[2].HTML) != "<p><strong>Hello</strong> from the model</p>" || bl.Blocks[2].Seq != 2 {
		t.Fatalf("blocks %+v", bl.Blocks)
	}
	x.getJSON(t, "/api/sessions/"+sid+"/blocks?after=2", &bl)
	if len(bl.Blocks) != 1 || bl.Blocks[0].Kind != "result" {
		t.Fatalf("after=2: %+v", bl.Blocks)
	}

	// Usage row with the price: 1M input × 0.6 + 1M cache read × 0.1 + 0.5M output × 2.2 = 1.8.
	rows, _ := x.store.UsageRows(sid)
	if len(rows) != 1 || rows[0].Kind != "chat" || rows[0].Input != 1_000_000 || rows[0].Output != 500_000 || rows[0].OwnerSub != "local" ||
		rows[0].Cost < 1.799 || rows[0].Cost > 1.801 || rows[0].MessageID != "msg_1" {
		t.Fatalf("usage %+v", rows)
	}
	var detail struct {
		Usage UsageTotals
		Files []map[string]any
	}
	x.getJSON(t, "/api/sessions/"+sid, &detail)
	if detail.Usage.Output != 500_000 || len(detail.Files) != 1 {
		t.Errorf("detail %+v", detail)
	}

	// SSE saw deltas, the blocks as JSON and the state changes.
	var got []string
	deltaText := ""
	timeout := time.After(3 * time.Second)
	for done := false; !done; {
		select {
		case e := <-events:
			got = append(got, e[0])
			switch e[0] {
			case "delta":
				var d struct {
					Seq  int64
					Text string
				}
				json.Unmarshal([]byte(e[1]), &d)
				if d.Seq != 2 {
					t.Errorf("delta seq %d", d.Seq)
				}
				deltaText += d.Text
			case "block":
				var bs []apiBlock
				if json.Unmarshal([]byte(e[1]), &bs) != nil || len(bs) == 0 {
					t.Errorf("block not JSON: %s", e[1])
				}
				if bs[0].Kind == "result" {
					done = true
				}
			case "state":
				if !strings.HasPrefix(e[1], `{"state":`) {
					t.Errorf("state not JSON: %s", e[1])
				}
			}
		case <-timeout:
			t.Fatalf("SSE events %v", got)
		}
	}
	if deltaText != "**Hello** from the model" {
		t.Errorf("deltas %q (events %v)", deltaText, got)
	}

	// Title from the first message; PATCH renames.
	x.getJSON(t, "/api/sessions/"+sid, &sv)
	if sv.Session.Title != "summarise" {
		t.Errorf("title %q", sv.Session.Title)
	}
	if st := x.do(t, "PATCH", "/api/sessions/"+sid, map[string]any{"title": "Milk"}, &sv); st != 200 || sv.Session.Title != "Milk" {
		t.Errorf("patch %d %+v", st, sv.Session)
	}

	// The SPA shell is served for the session's URL.
	if st := status(t, x.c, x.srv.URL+"/s/"+sid); st != 200 {
		t.Errorf("shell: %d", st)
	}

	// Referenced files cannot be deleted; unreferenced ones can.
	if st := x.do(t, "DELETE", "/api/files/"+fid, nil, nil); st != 409 {
		t.Errorf("delete referenced file: %d", st)
	}
	_, f2 := x.upload(t, sid, "spare.txt", []byte("spare"))
	if st := x.do(t, "DELETE", "/api/files/"+f2["id"].(string), nil, nil); st != 204 {
		t.Errorf("delete unreferenced file: %d", st)
	}

	// Deleting the session removes events, file rows and the blob; usage stays.
	sha := ""
	if fr, err := x.store.File(fid); err == nil {
		sha = fr.SHA256
	}
	blob, _ := x.h.blobs.Path(sha)
	if st := x.do(t, "DELETE", "/api/sessions/"+sid, nil, nil); st != 204 {
		t.Fatalf("delete session: %d", st)
	}
	if _, err := x.store.Session(sid); err != sql.ErrNoRows {
		t.Error("session still there")
	}
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Error("blob still there")
	}
	if evs, _ := x.store.Events(sid, 0, 10); len(evs) != 0 {
		t.Error("events still there")
	}
	if rows, _ := x.store.UsageRows(sid); len(rows) != 1 {
		t.Error("usage rows removed")
	}
}

func TestChatBusyInterruptAndImages(t *testing.T) {
	x := newChatHub(t)
	sid := x.newChat(t, "glm-5.3", "")
	// An image to a model without vision is refused before anything is stored.
	st, f := x.upload(t, sid, "cat.png", pngData(t))
	if st != 201 {
		t.Fatalf("upload png %d %v", st, f)
	}
	var e struct{ Error string }
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "look", "file_ids": []string{f["id"].(string)}}, &e); st != 400 || e.Error != "this model cannot see images" {
		t.Fatalf("image to blind model: %d %q", st, e.Error)
	}
	if x.store.MaxSeq(sid) != 0 {
		t.Fatal("stored after refusal")
	}

	x.llm.mu.Lock()
	x.llm.hold = make(chan struct{})
	x.llm.mu.Unlock()
	defer close(x.llm.hold)
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "hi"}, nil); st != 202 {
		t.Fatalf("send %d", st)
	}
	for deadline := time.Now().Add(3 * time.Second); x.llm.count() == 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "again"}, &e); st != 409 {
		t.Errorf("concurrent send: %d %v", st, e)
	}
	var sv struct{ Session apiSessionView }
	x.getJSON(t, "/api/sessions/"+sid, &sv)
	if !sv.Session.Running || sv.Session.State != "running" {
		t.Errorf("not running: %+v", sv.Session)
	}
	time.Sleep(100 * time.Millisecond) // let the first delta arrive
	var ir struct{ Interrupted bool }
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/interrupt", nil, &ir); st != 200 || !ir.Interrupted {
		t.Fatalf("interrupt %d %+v", st, ir)
	}
	x.h.runner.Wait(sid)
	var bl struct{ Blocks []apiBlock }
	x.getJSON(t, "/api/sessions/"+sid+"/blocks", &bl)
	last := bl.Blocks[len(bl.Blocks)-1]
	if last.Kind != "result" || !strings.HasPrefix(last.Text, "interrupted") || bl.Blocks[len(bl.Blocks)-2].Text != "**Hello** " {
		t.Fatalf("after interrupt %+v", bl.Blocks)
	}
	x.getJSON(t, "/api/sessions/"+sid, &sv)
	if sv.Session.Running || sv.Session.State != "idle" {
		t.Errorf("still running: %+v", sv.Session)
	}
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/stop", nil, nil); st != 400 {
		t.Errorf("stop on chat: %d", st)
	}
}

func pngData(t *testing.T) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestFileUploadRulesAndServing(t *testing.T) {
	x := newChatHub(t)
	sid := x.newChat(t, "", "")
	for _, c := range []struct {
		name string
		data []byte
		want int
	}{
		{"x.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 415},
		{"x.html", []byte("<html></html>"), 415},
		{"evil.txt", []byte("<html><script>alert(1)</script>"), 415},
		{"big.txt", bytes.Repeat([]byte("a"), 1<<20+1), 413},
	} {
		if st, out := x.upload(t, sid, c.name, c.data); st != c.want {
			t.Errorf("%s: %d %v", c.name, st, out)
		}
	}
	// Quota 2 MB: two 0.9 MB PDFs-as-text fit, the third does not.
	chunk := bytes.Repeat([]byte("b"), 900<<10)
	for i, want := range []int{201, 201, 507} {
		if st, out := x.upload(t, sid, fmt.Sprintf("part%d.txt", i), append(chunk, byte('0'+i))); st != want {
			t.Errorf("quota upload %d: %d %v", i, st, out)
		}
	}
	// Code sessions take no files.
	code := proto.NewID()
	x.store.CreateSession(Session{ID: code, OwnerSub: "local", Kind: KindCode, ProfileName: "subscription"})
	if st, _ := x.upload(t, code, "a.txt", []byte("x")); st != 400 {
		t.Errorf("upload to code session: %d", st)
	}
	_, f := x.upload(t, sid, "", []byte("x"))
	if f["error"] == nil {
		t.Error("nameless upload accepted")
	}

	st, f := x.upload(t, sid, `../../we"ird.md`, []byte("# hi"))
	if st != 201 || f["name"] != `we"ird.md` {
		t.Fatalf("name cleaning %d %v", st, f)
	}
	id := f["id"].(string)
	resp, _ := x.c.Get(x.srv.URL + "/files/" + id)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "# hi" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		resp.Header.Get("Content-Type") != "text/markdown; charset=utf-8" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("serve %d %v %q", resp.StatusCode, resp.Header, b)
	}
	// Another user gets 404, an admin may read it.
	other := x.loginAs(t, "u2", false)
	if r, _ := other.Get(x.srv.URL + "/files/" + id); r.StatusCode != 404 {
		t.Errorf("other user: %d", r.StatusCode)
	}
	if r, _ := other.Get(x.srv.URL + "/api/sessions/" + sid); r.StatusCode != 404 {
		t.Errorf("other user's session view: %d", r.StatusCode)
	}
	admin := x.loginAs(t, "u3", true)
	if r, _ := admin.Get(x.srv.URL + "/files/" + id); r.StatusCode != 200 {
		t.Errorf("admin: %d", r.StatusCode)
	}
}

// loginAs creates a user with a web session cookie.
func (x *chatHub) loginAs(t *testing.T, sub string, admin bool) *http.Client {
	x.store.UpsertUser(User{Sub: sub, Email: sub + "@example.org", IsAdmin: admin})
	id, err := x.store.CreateWebSession(sub, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c := browser()
	u, _ := url.Parse(x.srv.URL)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: cookieSession, Value: id, Path: "/"}})
	return c
}

func TestCodeUsageRowsDedupAndCost(t *testing.T) {
	x := newM2(t)
	setPrices(t, x.th.h, usage.Prices{{Model: "zai-org/glm-5.3", Input: 1, Output: 1}})
	sid := x.session(t, "r", proto.StateIdle, "")
	ev := func(seq int64, id string, out int) {
		x.fw.send(&proto.Event{SessionID: sid, Seq: seq, Raw: json.RawMessage(fmt.Sprintf(
			`{"type":"assistant","message":{"id":"%s","model":"zai-org/glm-5.3","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":1000000,"output_tokens":%d}}}`, id, out))})
	}
	ev(1, "m1", 0)
	ev(2, "m1", 1000000) // same response, the final output count
	ev(3, "m2", 5)
	for deadline := time.Now().Add(5 * time.Second); x.th.store.MaxSeq(sid) < 3 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	rows, _ := x.th.store.UsageRows(sid)
	if len(rows) != 2 || rows[0].MessageID != "m1" || rows[0].Output != 1000000 || rows[0].Cost != 2 || rows[0].Kind != "code" {
		t.Fatalf("rows %+v", rows)
	}
	// A subscription session costs 0 whatever the prices say.
	sub := x.session(t, "subscription", proto.StateIdle, "")
	x.fw.send(&proto.Event{SessionID: sub, Seq: 1, Raw: json.RawMessage(`{"type":"assistant","message":{"id":"s1","model":"zai-org/glm-5.3","content":[],"usage":{"input_tokens":1000000,"output_tokens":1}}}`)})
	for deadline := time.Now().Add(5 * time.Second); x.th.store.MaxSeq(sub) < 1 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if rows, _ := x.th.store.UsageRows(sub); len(rows) != 1 || rows[0].Cost != 0 {
		t.Fatalf("subscription rows %+v", rows)
	}
}

func TestChatConfigAndEditor(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hub.toml")
	os.WriteFile(cfgPath, []byte(`base_url = "http://127.0.0.1:1"
[chat]
enabled = true
data_dir = "`+filepath.Join(dir, "files")+`"
[[chat.models]]
name = "glm-5.3"
base_url = "https://api.aiand.com"
model = "zai-org/glm-5.3"
key_ref = "aiand"
[[chat.models]]
name = "flash"
base_url = "https://api.aiand.com"
model = "zai-org/glm-5.3-flash"
vision = true
default = true
[[prices]]
model = "zai-org/glm-5.3"
input = 0.6
output = 2.2
`), 0o600)
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := cfg.Chat.ChatModels()
	if cfg.Chat.MaxFileMB != 20 || cfg.Chat.UserQuotaMB != 1024 || cfg.Chat.MaxToolRounds != 8 || len(ms) != 2 ||
		ms[0].MaxOutput != 32000 || !ms[1].NativeDocuments() || ms[0].NativeDocuments() || len(cfg.Prices) != 1 {
		t.Fatalf("config %+v %+v", cfg.Chat, ms)
	}
	store, _ := OpenStore(filepath.Join(dir, "hub.db"))
	defer store.Close()
	store.PutChatModel(ms[0], SourceUI) // an editor override of a hub.toml name (§28.1)
	store.PutChatModel(chatModelNamed("old"), SourceConfig)
	store.PutChatModel(chatModelNamed("mine"), SourceUI)
	if _, err := New(cfg, store, &Secrets{Keys: map[string]string{}}, Options{LocalLogin: true}); err == nil {
		t.Fatal("missing key_ref accepted")
	}
	h, err := New(cfg, store, &Secrets{Keys: map[string]string{"aiand": "k"}}, Options{LocalLogin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	got := map[string]string{}
	all, _ := store.ChatModels()
	for _, m := range all {
		got[m.Name] = store.ChatModelSource(m.Name)
	}
	if fmt.Sprint(got) != "map[flash:config glm-5.3:ui mine:ui]" {
		t.Fatalf("seeded %v", got)
	}
	if ms := h.chatModels(); ms[0].Name != "flash" {
		t.Errorf("default not first: %v", ms)
	}
	if _, err := ParseChatModel([]byte(`{"name":"x","base_url":"https://a","model":"m","key":"sk-1"}`), h.secrets); err == nil {
		t.Error("inline key accepted")
	}
	if _, err := ParseChatModel([]byte(`{"name":"x","base_url":"https://a","model":"m","key_ref":"nope"}`), h.secrets); err == nil {
		t.Error("unknown key_ref accepted")
	}

	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	h.cfg.BaseURL = srv.URL
	c := browser()
	c.Post(srv.URL+"/local-login", "", nil)
	csrf := apiCSRF(t, c, srv.URL)
	var list struct {
		ChatModels []apiChatModelRow `json:"chat_models"`
	}
	apiDo(t, c, srv.URL, csrf, "GET", "/api/admin/profiles", nil, &list)
	if len(list.ChatModels) != 3 || list.ChatModels[0].Name != "flash" || !list.ChatModels[0].Default || !list.ChatModels[0].Editable ||
		list.ChatModels[0].OverridesConfig || !list.ChatModels[1].OverridesConfig ||
		list.ChatModels[2].Name != "mine" || !list.ChatModels[2].Editable {
		t.Errorf("chat models: %+v", list.ChatModels)
	}
	post := func(js, orig string) int {
		return apiDo(t, c, srv.URL, csrf, "POST", "/api/admin/chat-models", map[string]string{"json": js, "original": orig}, nil)
	}
	if st := post(`{"name":"flash","base_url":"https://a","model":"m","default":true}`, "flash"); st != 200 || store.ChatModelSource("flash") != SourceUI {
		t.Errorf("override a config model: %d", st)
	}
	if st := post(`{"name":"new1","base_url":"https://a","model":"m","default":true}`, ""); st != 422 {
		t.Errorf("second default: %d", st)
	}
	if st := post(`{"name":"new1","base_url":"https://a","model":"m","vision":true}`, ""); st != 200 {
		t.Errorf("save ui model: %d", st)
	}
	if m, err := store.ChatModel("new1"); err != nil || !m.NativeDocuments() || store.ChatModelSource("new1") != SourceUI {
		t.Errorf("saved %+v %v", m, err)
	}
}

func chatModelNamed(n string) (m chat.Model) {
	m = chat.Model{Name: n, BaseURL: "https://x", Model: "m"}
	m.Normalize()
	return m
}

func TestSessionKindMigration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	// The M5 sessions table, without kind, model and system.
	db.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY, machine_id TEXT, owner_sub TEXT, cwd TEXT, profile_name TEXT,
  main_model TEXT, state TEXT, claude_session_id TEXT DEFAULT '', title TEXT DEFAULT '', created_at TEXT, last_error TEXT DEFAULT '',
  permission_mode TEXT DEFAULT 'default')`)
	db.Exec(`INSERT INTO sessions(id,machine_id,owner_sub,cwd,profile_name,main_model,state,created_at) VALUES('s1','m','dev','/p','subscription','fable','idle','2026-10-01T00:00:00Z')`)
	db.Close()
	st, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.Session("s1")
	if err != nil || s.Kind != KindCode || s.IsChat() || s.Model != "" {
		t.Fatalf("migrated %+v %v", s, err)
	}
}

func TestExampleChatConfig(t *testing.T) {
	c, err := LoadConfig("../../deploy/hub.toml.example")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := c.Chat.ChatModels()
	if err != nil || !c.Chat.Enabled || len(ms) != 2 || !ms[0].Default || ms[0].NativeDocuments() || !ms[1].NativeDocuments() {
		t.Fatalf("example chat models %+v %v", ms, err)
	}
}

// The composer's model picker: PATCH {model} switches a chat session's model.
func TestChatModelSwitch(t *testing.T) {
	x := newChatHub(t)
	sid := x.newChat(t, "glm-5.3", "")
	var sv struct{ Session apiSessionView }
	if st := x.do(t, "PATCH", "/api/sessions/"+sid, map[string]any{"model": "glm-flash"}, &sv); st != 200 || sv.Session.Model != "glm-flash" {
		t.Fatalf("switch: %d %+v", st, sv.Session)
	}
	if st := x.do(t, "PATCH", "/api/sessions/"+sid, map[string]any{"model": "nope"}, nil); st != 400 {
		t.Errorf("unknown model: %d", st)
	}
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "hi"}, nil); st != 202 {
		t.Fatalf("send: %d", st)
	}
	x.h.runner.Wait(sid)
	if m := x.llm.req(0)["model"]; m != "zai-org/glm-5.3-flash" {
		t.Errorf("upstream model %v", m)
	}
}

// A turn's result replaces its provisional rows with the exact counts from
// modelUsage, per claude process (§17.13).
func TestCodeUsageResultReplacesProvisional(t *testing.T) {
	x := newM2(t)
	setPrices(t, x.th.h, usage.Prices{{Model: "org/A", Input: 100, Output: 1000, CacheRead: 10}})
	sid := x.session(t, "r", proto.StateIdle, "")
	var seq int64
	send := func(raw string) {
		seq++
		x.fw.send(&proto.Event{SessionID: sid, Seq: seq, Raw: json.RawMessage(raw)})
		for deadline := time.Now().Add(5 * time.Second); x.th.store.MaxSeq(sid) < seq && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
	}
	spawn := func() { send(`{"type":"mado_spawn","pid":"1"}`) }
	assistant := func(id string) {
		send(`{"type":"assistant","message":{"id":"` + id + `","model":"org/A","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":10620,"cache_read_input_tokens":0,"output_tokens":0}}}`)
	}
	result := func(in, cache, out int) {
		send(fmt.Sprintf(`{"type":"result","subtype":"success","usage":{"input_tokens":1},"modelUsage":{"oc-a":{"inputTokens":%d,"cacheReadInputTokens":%d,"cacheCreationInputTokens":0,"outputTokens":%d}}}`, in, cache, out))
	}
	type want struct {
		id             string
		in, cache, out int64
		cost           float64
		provisional    bool
	}
	check := func(step string, ws ...want) {
		t.Helper()
		rows, _ := x.th.store.UsageRows(sid)
		if len(rows) != len(ws) {
			t.Fatalf("%s: rows %+v", step, rows)
		}
		for i, w := range ws {
			r := rows[i]
			cost := (float64(r.Input)*100 + float64(r.CacheRead)*10 + float64(r.Output)*1000) / 1e6
			if r.MessageID != w.id || r.Input != w.in || r.CacheRead != w.cache || r.Output != w.out || r.Provisional != w.provisional ||
				r.Model != "org/A" || math.Abs(r.Cost-cost) > 1e-9 {
				t.Fatalf("%s: row %d %+v, want %+v", step, i, r, w)
			}
		}
	}
	turn := func(n int) string { return fmt.Sprintf("turn:%s:%d:oc-a", sid, n) }

	spawn()         // 1
	assistant("m1") // 2
	assistant("m1") // 3, same response
	assistant("m2") // 4
	check("live", want{"m1", 10620, 0, 0, 0, true}, want{"m2", 10620, 0, 0, 0, true})
	result(444, 15104, 266) // 5
	check("turn 1", want{turn(5), 444, 15104, 266, 0, false})
	assistant("m3")         // 6
	result(777, 22528, 310) // 7: cumulative
	check("turn 2", want{turn(5), 444, 15104, 266, 0, false}, want{turn(7), 333, 7424, 44, 0, false})
	// A resumed process counts from zero again, even past the old totals.
	spawn()                  // 8
	assistant("m4")          // 9
	result(1000, 30000, 400) // 10
	check("resume", want{turn(5), 444, 15104, 266, 0, false}, want{turn(7), 333, 7424, 44, 0, false},
		want{turn(10), 1000, 30000, 400, 0, false})
	// A process that died mid-turn keeps its provisional row.
	assistant("m5") // 11
	spawn()         // 12
	result(5, 0, 1) // 13
	rows, _ := x.th.store.UsageRows(sid)
	if len(rows) != 5 || rows[3].MessageID != "m5" || !rows[3].Provisional || rows[4].MessageID != turn(13) || rows[4].Input != 5 {
		t.Fatalf("crash: %+v", rows)
	}
	// Without a marker (an older shim) a smaller cumulative count means a new process.
	result(2, 0, 1) // 14
	rows, _ = x.th.store.UsageRows(sid)
	if len(rows) != 6 || rows[5].Input != 2 || rows[5].Output != 1 {
		t.Fatalf("no marker: %+v", rows)
	}
	// An unchanged total adds nothing.
	result(2, 0, 1) // 15
	if rows, _ := x.th.store.UsageRows(sid); len(rows) != 6 {
		t.Fatalf("no change: %+v", rows)
	}
	if u := x.th.store.SessionUsage(sid); u.Input != 444+333+1000+10620+5+2 || u.Output != 266+44+400+1+1 {
		t.Fatalf("totals %+v", u)
	}
}

func TestUsageBoundary(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ins := func(seq int64, raw string) {
		if _, err := st.InsertEvent("s", seq, json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	ins(1, `{"type":"mado_spawn","pid":"1"}`)
	ins(2, `{"type":"user","message":{"role":"user","content":"{\"type\":\"result\"} in a text"}}`)
	ins(3, `{"type":"result","modelUsage":{}}`)
	ins(4, `{"type":"user","message":{"role":"user","content":"{\"type\":\"mado_spawn\"}"}}`)
	ins(6, `{"type":"result","modelUsage":{}}`)
	ins(8, `{"type":"mado_spawn","pid":"2"}`)
	ins(9, `{"type":"result","modelUsage":{}}`)
	for _, c := range []struct {
		seq, prev int64
		typ       string
		later     bool
	}{{3, 0, "", true}, {5, 0, "", true}, {6, 3, "result", false}, {7, 6, "result", false},
		{9, 8, "mado_spawn", false}, {1, 0, "", true}} {
		prev, typ, later, err := st.UsageBoundary("s", c.seq)
		if err != nil || prev.Seq != c.prev || typ != c.typ || later != c.later {
			t.Fatalf("seq %d: %d %q %v %v", c.seq, prev.Seq, typ, later, err)
		}
	}
}
