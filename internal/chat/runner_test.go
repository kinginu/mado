package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/render"
	"github.com/kinginu/mado/internal/usage"
)

type delta struct {
	seq  int64
	text string
}

type fakeHost struct {
	mu     sync.Mutex
	events []StoredEvent
	deltas []delta
	states []string
	files  map[string]*Attachment
	usage  []string
}

func (h *fakeHost) Events(string) ([]StoredEvent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]StoredEvent(nil), h.events...), nil
}

func (h *fakeHost) Append(_ string, raw []byte) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !json.Valid(raw) {
		return 0, errors.New("invalid json")
	}
	seq := int64(len(h.events) + 1)
	h.events = append(h.events, StoredEvent{Seq: seq, Raw: raw})
	return seq, nil
}

func (h *fakeHost) Delta(_ string, seq int64, text string) {
	h.mu.Lock()
	h.deltas = append(h.deltas, delta{seq, text})
	h.mu.Unlock()
}

func (h *fakeHost) SetState(_, state string) {
	h.mu.Lock()
	h.states = append(h.states, state)
	h.mu.Unlock()
}

func (h *fakeHost) Attachment(owner, session, id string) (*Attachment, error) {
	if owner != "u1" || session != "s1" {
		return nil, errors.New("not yours")
	}
	if a := h.files[id]; a != nil {
		return a, nil
	}
	return nil, errors.New("no such file")
}

func (h *fakeHost) RecordUsage(t *Turn, id, model string, u messages.Usage) float64 {
	h.mu.Lock()
	h.usage = append(h.usage, fmt.Sprintf("%s %s %d/%d/%d", id, model, u.InputTokens, u.CacheReadInputTokens, u.OutputTokens))
	h.mu.Unlock()
	return 0.5
}

func (h *fakeHost) StoreImage(t *Turn, name, mime string, data []byte) (*Attachment, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.files == nil {
		h.files = map[string]*Attachment{}
	}
	id := fmt.Sprintf("gen%d", len(h.files)+1)
	a := &Attachment{ID: id, Name: name, Mime: mime, Size: int64(len(data))}
	h.files[id] = a
	return a, nil
}

func (h *fakeHost) types() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, e := range h.events {
		var v struct{ Type, Subtype string }
		json.Unmarshal(e.Raw, &v)
		if v.Subtype != "" {
			v.Type += "/" + v.Subtype
		}
		out = append(out, v.Type)
	}
	return out
}

func (h *fakeHost) raw(i int) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.events[i].Raw)
}

// streamOf answers like AI&: thinking, then the text in pieces, usage in message_delta.
func streamOf(id string, pieces ...string) string {
	var b strings.Builder
	ev := func(t, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", t, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"`+id+`","type":"message","role":"assistant","model":"org/m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
	ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"secret plan"}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
	for _, p := range pieces {
		j, _ := json.Marshal(p)
		ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":`+string(j)+`}}`)
	}
	ev("content_block_stop", `{"type":"content_block_stop","index":1}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":120,"cache_read_input_tokens":30,"output_tokens":42}}`)
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []messages.Request
	auth []string
	fn   func(w http.ResponseWriter, r *http.Request, n int)
}

func newUpstream(t *testing.T, fn func(w http.ResponseWriter, r *http.Request, n int)) *upstream {
	u := &upstream{fn: fn}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req messages.Request
		json.NewDecoder(r.Body).Decode(&req)
		u.mu.Lock()
		u.reqs = append(u.reqs, req)
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		n := len(u.reqs)
		u.mu.Unlock()
		u.fn(w, r, n)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) req(i int) messages.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reqs[i]
}

func sseWrite(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, body)
}

func newRunner(h *fakeHost) *Runner {
	r := NewRunner(h)
	c := messages.NewClient()
	c.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
	r.Client = c
	r.DeltaEvery = 0
	r.Now = func() time.Time { return time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC) }
	return r
}

func testModel(base string) Model {
	m := Model{Name: "glm", BaseURL: base, Model: "org/m", KeyRef: "k"}
	if err := m.Normalize(); err != nil {
		panic(err)
	}
	return m
}

func turn(m Model, text string, files ...string) Turn {
	return Turn{SessionID: "s1", Owner: "u1", Model: m, Key: "the-key", System: "Be brief.", Text: text, FileIDs: files}
}

func TestTurnStoredInStreamJSONShape(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		sseWrite(w, streamOf(fmt.Sprintf("msg_%d", n), "Hel", "lo ", "there"))
	})
	h := &fakeHost{}
	r := newRunner(h)
	m := testModel(up.srv.URL)
	seq, err := r.Send(turn(m, "hi"))
	if err != nil || seq != 1 {
		t.Fatal(seq, err)
	}
	r.Wait("s1")
	if got := strings.Join(h.types(), ","); got != "user,assistant,result/success" {
		t.Fatalf("events %s", got)
	}
	// Request: default system + the session's, max_tokens = max_output, bearer key.
	req := up.req(0)
	if !strings.HasPrefix(req.System, "You are mado") || !strings.Contains(req.System, "2026-10-18") || !strings.HasSuffix(req.System, "Be brief.") ||
		req.MaxTokens != DefaultMaxOutput || !req.Stream || req.Model != "org/m" || len(req.Messages) != 1 || up.auth[0] != "Bearer the-key" {
		t.Fatalf("request %+v auth %v", req, up.auth)
	}
	if len(req.Tools) != 0 || strings.Contains(req.System, "tools") {
		t.Errorf("tools offered without a registry: %+v", req.Tools)
	}
	// Deltas carry the seq the assistant event got, and add up to its text.
	var dt strings.Builder
	for _, d := range h.deltas {
		if d.seq != 2 {
			t.Errorf("delta seq %d", d.seq)
		}
		dt.WriteString(d.text)
	}
	if dt.String() != "Hello there" {
		t.Errorf("deltas %q", dt.String())
	}
	// The stored events render with the existing renderer.
	var kinds []string
	for i := range h.events {
		for _, b := range render.Blocks([]byte(h.raw(i))) {
			kinds = append(kinds, b.Kind+":"+b.Text)
		}
	}
	want := "user:hi|thinking:secret plan|assistant:Hello there|result:turn done · 1 turns · $0.5000"
	if got := strings.Join(kinds, "|"); got != want {
		t.Errorf("blocks\n got %s\nwant %s", got, want)
	}
	// And count in the usage package like a code session's events.
	acc := usage.New()
	for i := range h.events {
		acc.Add([]byte(h.raw(i)))
	}
	st := acc.Stats()
	if len(st.Rows) != 1 || st.Rows[0].Model != "org/m" || st.Rows[0].Responses != 1 || st.Rows[0].Input != 150 || st.Rows[0].Output != 42 {
		t.Errorf("usage %+v", st.Rows)
	}
	if len(h.usage) != 1 || h.usage[0] != "msg_1 org/m 120/30/42" {
		t.Errorf("usage rows %v", h.usage)
	}
	if strings.Join(h.states, ",") != "running,idle" {
		t.Errorf("states %v", h.states)
	}
	var ev struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal([]byte(h.raw(1)), &ev)
	if ev.SessionID != "s1" {
		t.Errorf("session id %q", ev.SessionID)
	}

	// Second turn: history replayed without the thinking block.
	if _, err := r.Send(turn(m, "and?")); err != nil {
		t.Fatal(err)
	}
	r.Wait("s1")
	req = up.req(1)
	if len(req.Messages) != 3 || req.Messages[1].Role != "assistant" || len(req.Messages[1].Content) != 1 ||
		req.Messages[1].Content[0].Type != "text" || req.Messages[1].Content[0].Text != "Hello there" {
		t.Fatalf("replay %+v", req.Messages)
	}
}

func TestBusyAndInterrupt(t *testing.T) {
	release := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		full := streamOf("msg_x", "partial ", "answer", " never")
		cut := strings.Index(full, `"text":" never"`)
		cut = strings.LastIndex(full[:cut], "event:")
		sseWrite(w, full[:cut])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	h := &fakeHost{}
	r := newRunner(h)
	m := testModel(up.srv.URL)
	if _, err := r.Send(turn(m, "go")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.deltas)
		h.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := r.Send(turn(m, "again")); !errors.Is(err, ErrBusy) {
		t.Fatalf("second send: %v", err)
	}
	if !r.Interrupt("s1") {
		t.Fatal("nothing to interrupt")
	}
	r.Wait("s1")
	if got := strings.Join(h.types(), ","); got != "user,assistant,result/interrupted" {
		t.Fatalf("events %s", got)
	}
	var a struct {
		Message struct {
			StopReason string `json:"stop_reason"`
			ID         string
			Content    []messages.Block
		}
	}
	json.Unmarshal([]byte(h.raw(1)), &a)
	text := ""
	for _, c := range a.Message.Content {
		text += c.Text
	}
	if a.Message.StopReason != "interrupted" || text != "partial answer" || a.Message.ID != "msg_x" {
		t.Fatalf("partial %s", h.raw(1))
	}
	if r.Running("s1") || r.Interrupt("s1") {
		t.Error("still running")
	}
}

func TestUpstreamErrorStored(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.WriteHeader(500)
		io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)
	})
	h := &fakeHost{}
	r := newRunner(h)
	r.Send(turn(testModel(up.srv.URL), "hi"))
	r.Wait("s1")
	if got := strings.Join(h.types(), ","); got != "user,mado_error,result/error_during_execution" {
		t.Fatalf("events %s", got)
	}
	bs := render.Blocks([]byte(h.raw(1)))
	if !bs[0].IsError || !strings.Contains(bs[0].Text, "boom") || strings.Contains(bs[0].Text, "the-key") {
		t.Fatalf("error block %+v", bs)
	}
	if len(up.reqs) != 3 {
		t.Errorf("attempts %d", len(up.reqs))
	}
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func pngData() []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	return b.Bytes()
}

// minimalPDF is a one-page PDF with the text "Quarterly figures".
func minimalPDF() []byte {
	stream := "BT /F1 24 Tf 72 720 Td (Quarterly figures) Tj ET"
	objs := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	var offs []int
	for i, o := range objs {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	return b.Bytes()
}

func TestAttachments(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) { sseWrite(w, streamOf(fmt.Sprint("m", n), "ok")) })
	dir := t.TempDir()
	h := &fakeHost{files: map[string]*Attachment{
		"txt": {ID: "txt", Name: `notes".md`, Mime: "text/markdown; charset=utf-8", Path: writeFile(t, dir, "a", []byte("# Notes\nhello"))},
		"img": {ID: "img", Name: "cat.png", Mime: "image/png", Path: writeFile(t, dir, "b", pngData())},
		"pdf": {ID: "pdf", Name: "q3.pdf", Mime: "application/pdf", SHA256: "aa", Path: writeFile(t, dir, "c", minimalPDF())},
		"big": {ID: "big", Name: "big.txt", Mime: "text/plain; charset=utf-8", Path: writeFile(t, dir, "d", bytes.Repeat([]byte("x"), 300_000))},
	}}
	r := newRunner(h)
	blind := testModel(up.srv.URL)

	// A model without vision: the image is refused before anything is stored.
	if _, err := r.Send(turn(blind, "look", "img")); err == nil || err.Error() != "this model cannot see images" {
		t.Fatalf("image to a blind model: %v", err)
	}
	if _, err := r.Send(turn(blind, "x", "nope")); err == nil {
		t.Fatal("unknown file accepted")
	}
	small := blind
	small.ContextWindow, small.MaxOutput = 150_000, 1000
	if _, err := r.Send(turn(small, "x", "big")); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("oversized attachment: %v", err)
	}
	if len(h.types()) != 0 {
		t.Fatalf("stored after refusal: %v", h.types())
	}

	// Text and PDF (as extracted text) before the user's text; stored as mado_file.
	if _, err := r.Send(turn(blind, "summarise", "txt", "pdf")); err != nil {
		t.Fatal(err)
	}
	r.Wait("s1")
	if strings.Contains(h.raw(0), "base64") || !strings.Contains(h.raw(0), `"type":"mado_file"`) {
		t.Fatalf("stored user event %s", h.raw(0))
	}
	c := up.req(0).Messages[0].Content
	if len(c) != 3 || !strings.HasPrefix(c[0].Text, "<document name=\"notes'.md\">\n# Notes\nhello\n</document>") ||
		!strings.Contains(c[1].Text, "Quarterly figures") || c[2].Text != "summarise" {
		t.Fatalf("expanded %+v", c)
	}

	// A vision model gets image and native document blocks.
	seeing := blind
	seeing.Vision, seeing.Documents = true, nil
	seeing.Normalize()
	h2 := &fakeHost{files: h.files}
	r2 := newRunner(h2)
	if _, err := r2.Send(turn(seeing, "look", "img", "pdf")); err != nil {
		t.Fatal(err)
	}
	r2.Wait("s1")
	c = up.req(1).Messages[0].Content
	if len(c) != 3 || c[0].Type != "image" || c[0].Source.MediaType != "image/png" || c[1].Type != "document" ||
		c[1].Source.MediaType != "application/pdf" || c[1].Title != "q3.pdf" {
		t.Fatalf("vision blocks %+v", c)
	}
}

// history builds stored events: n finished turns with a large usage on the last.
func history(h *fakeHost, n int, lastInput int64) {
	for i := 1; i <= n; i++ {
		h.Append("s1", []byte(fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"question %d"}]}}`, i)))
		in := int64(10)
		if i == n {
			in = lastInput
		}
		h.Append("s1", []byte(fmt.Sprintf(`{"type":"assistant","message":{"id":"a%d","role":"assistant","model":"org/m","content":[{"type":"text","text":"answer %d"}],"usage":{"input_tokens":%d,"output_tokens":5}}}`, i, i, in)))
		h.Append("s1", []byte(`{"type":"result","subtype":"success"}`))
	}
}

func TestTrimming(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) { sseWrite(w, streamOf(fmt.Sprint("m", n), "ok")) })
	h := &fakeHost{}
	history(h, 4, 9000)
	r := newRunner(h)
	m := testModel(up.srv.URL)
	m.ContextWindow, m.MaxOutput = 10_000, 1000 // budget 7000 < 9005 known
	if _, err := r.Send(turn(m, "next")); err != nil {
		t.Fatal(err)
	}
	r.Wait("s1")
	ty := h.types()
	if ty[13] != "system/mado_trimmed" || ty[14] != "assistant" {
		t.Fatalf("events %v", ty)
	}
	// The estimate falls by the dropped turns' own estimates only, so every old turn
	// goes and the new one stays.
	msgs := up.req(0).Messages
	if len(msgs) != 1 || msgs[0].Content[0].Text != "next" {
		t.Fatalf("request after trim %+v", msgs)
	}
	var tr struct {
		Dropped   int64 `json:"dropped"`
		BeforeSeq int64 `json:"before_seq"`
	}
	json.Unmarshal([]byte(h.raw(13)), &tr)
	if tr.Dropped != 4 || tr.BeforeSeq != 13 {
		t.Fatalf("trim event %s", h.raw(13))
	}
	bs := render.Blocks([]byte(h.raw(13)))
	if !strings.Contains(bs[0].Text, "4 earlier") {
		t.Errorf("notice %+v", bs)
	}
	// The cut persists: the next turn does not send the dropped turns again.
	r.Send(turn(m, "more"))
	r.Wait("s1")
	if msgs := up.req(1).Messages; len(msgs) != 3 || msgs[0].Content[0].Text != "next" {
		t.Fatalf("second request %+v", msgs)
	}
}

func TestContextOverflowDropsTwoAndRetriesOnce(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.WriteHeader(400)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 1100000 tokens > 1048576 maximum","code":"invalid_value","param":"messages"},"request_id":"r"}`)
			return
		}
		sseWrite(w, streamOf("ok", "fine"))
	})
	h := &fakeHost{}
	history(h, 3, 10)
	r := newRunner(h)
	r.Send(turn(testModel(up.srv.URL), "next"))
	r.Wait("s1")
	if len(up.reqs) != 2 || len(up.req(0).Messages) != 7 || len(up.req(1).Messages) != 3 {
		t.Fatalf("requests %d: %d then %d messages", len(up.reqs), len(up.req(0).Messages), len(up.req(1).Messages))
	}
	if got := strings.Join(h.types()[10:], ","); got != "system/mado_trimmed,assistant,result/success" {
		t.Fatalf("events %s", got)
	}
}

func TestValidateModels(t *testing.T) {
	ms := []Model{{Name: "a", BaseURL: "https://x", Model: "m", Vision: true}, {Name: "b", BaseURL: "https://y", Model: "n", Default: true}}
	if err := ValidateModels(ms); err != nil {
		t.Fatal(err)
	}
	if !ms[0].NativeDocuments() || ms[1].NativeDocuments() || ms[0].MaxOutput != 32000 || ms[0].Label != "a" {
		t.Fatalf("defaults %+v", ms)
	}
	f := false
	m := Model{Name: "c", BaseURL: "https://x", Model: "m", Vision: true, Documents: &f}
	if m.Normalize() != nil || m.NativeDocuments() {
		t.Fatal("explicit documents=false ignored")
	}
	for _, bad := range []Model{{Name: "a/b", BaseURL: "https://x", Model: "m"}, {Name: "a", BaseURL: "ftp://x", Model: "m"},
		{Name: "a", BaseURL: "https://x"}, {Name: "a", BaseURL: "https://x", Model: "m", AuthHeader: "basic"},
		{Name: "a", BaseURL: "https://x", Model: "m", ContextWindow: 4000, MaxOutput: 3000}} {
		if bad.Normalize() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	two := []Model{{Name: "a", BaseURL: "https://x", Model: "m", Default: true}, {Name: "b", BaseURL: "https://x", Model: "m", Default: true}}
	if ValidateModels(two) == nil || ValidateModels([]Model{ms[0], ms[0]}) == nil {
		t.Error("two defaults or duplicate names accepted")
	}
}
