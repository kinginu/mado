package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/render"
	"github.com/kinginu/mado/internal/tools"
)

// toolStream answers with text (optional) and tool_use blocks, like AI&.
func toolStream(id string, text string, calls ...[2]string) string {
	var b strings.Builder
	ev := func(t, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", t, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"`+id+`","type":"message","role":"assistant","model":"org/m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	i := 0
	if text != "" {
		j, _ := json.Marshal(text)
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(j)+`}}`)
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
		i++
	}
	for k, c := range calls {
		idx := fmt.Sprint(i)
		tid := fmt.Sprintf("chatcmpl-tool-%s_%d", id, k)
		ev("content_block_start", `{"type":"content_block_start","index":`+idx+`,"content_block":{"type":"tool_use","id":"`+tid+`","name":"`+c[0]+`","input":{}}}`)
		half := len(c[1]) / 2
		for _, part := range []string{c[1][:half], c[1][half:]} {
			j, _ := json.Marshal(part)
			ev("content_block_delta", `{"type":"content_block_delta","index":`+idx+`,"delta":{"type":"input_json_delta","partial_json":`+string(j)+`}}`)
		}
		ev("content_block_stop", `{"type":"content_block_stop","index":`+idx+`}`)
		i++
	}
	stop := "end_turn"
	if len(calls) > 0 {
		stop = "tool_use"
	}
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`"},"usage":{"input_tokens":100,"output_tokens":10}}`)
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

type fakeSearch struct {
	block chan struct{} // when set, wait for it or the context
	mu    sync.Mutex
	qs    []string
}

func (f *fakeSearch) Search(ctx context.Context, q string) ([]tools.SearchResult, error) {
	f.mu.Lock()
	f.qs = append(f.qs, q)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []tools.SearchResult{{Title: "Sea otter - Wikipedia", URL: "https://en.wikipedia.org/wiki/Sea_otter", Snippet: "The sea otter is a marine mammal."}}, nil
}

type fakeImager struct{}

func (fakeImager) Sizes() []string { return []string{"1024x1024"} }
func (fakeImager) Generate(ctx context.Context, prompt, size string) (*tools.Image, error) {
	return &tools.Image{Data: pngData(), Mime: "image/png", Model: "gpt-image-1",
		Usage: &messages.Usage{InputTokens: 20, OutputTokens: 272}}, nil
}

func blockKinds(h *fakeHost) string {
	var kinds []string
	for i := range h.events {
		for _, b := range render.Blocks([]byte(h.raw(i))) {
			k := b.Kind
			if b.IsError {
				k += "!"
			}
			if b.Origin != "" {
				k += "(" + b.Origin + ")"
			}
			kinds = append(kinds, k)
		}
	}
	return strings.Join(kinds, ",")
}

func TestToolLoop(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "otters hold hands")
	}))
	defer page.Close()
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		switch n {
		case 1: // two calls in one message
			sseWrite(w, toolStream("m1", "Let me look.", [2]string{"web_search", `{"query":"sea otters"}`},
				[2]string{"fetch_url", `{"url":"` + page.URL + `/otters"}`}))
		case 2:
			sseWrite(w, toolStream("m2", "", [2]string{"generate_image", `{"prompt":"an otter","size":"1024x1024"}`},
				[2]string{"fetch_url", `{"url":"http://127.0.0.1:1/"}`}))
		default:
			sseWrite(w, toolStream("m3", "Here you are."))
		}
	})
	h := &fakeHost{}
	r := newRunner(h)
	search := &fakeSearch{}
	r.Tools = &tools.Registry{Search: search, Fetcher: &tools.Fetcher{Guard: tools.Guard{AllowPrivate: true}, MaxChars: 20000}, Imager: fakeImager{}}
	if _, err := r.Send(turn(testModel(up.srv.URL), "tell me about otters")); err != nil {
		t.Fatal(err)
	}
	r.Wait("s1")
	if got := strings.Join(h.types(), ","); got != "user,assistant,user,assistant,user,assistant,result/success" {
		t.Fatalf("events %s", got)
	}
	// Request: custom tools without a type, the guidance sentence.
	req := up.req(0)
	if len(req.Tools) != 3 || req.Tools[0].Name != "web_search" || !strings.Contains(req.System, "You have these tools: web_search, fetch_url, generate_image.") {
		t.Fatalf("request tools %+v system %q", req.Tools, req.System)
	}
	// The results of both calls went back in one user message, in call order.
	msgs := up.req(1).Messages
	last := msgs[len(msgs)-1]
	if len(msgs) != 3 || last.Role != "user" || len(last.Content) != 2 || last.Content[0].Type != "tool_result" ||
		last.Content[0].ToolUseID != "chatcmpl-tool-m1_0" || !strings.Contains(string(last.Content[0].Content), "Sea otter - Wikipedia") ||
		!strings.Contains(string(last.Content[1].Content), "otters hold hands") || last.Content[1].IsError {
		t.Fatalf("second request %+v", msgs)
	}
	if search.qs[0] != "sea otters" {
		t.Errorf("query %v", search.qs)
	}
	// Third request: the image result says only that it was shown, no image block;
	// the second call (fetch of a closed port) is an is_error result, not a crash.
	msgs = up.req(2).Messages
	last = msgs[len(msgs)-1]
	if len(last.Content) != 2 || !strings.Contains(string(last.Content[0].Content), "Image gen1 was generated and is shown to the user.") ||
		!last.Content[1].IsError || !strings.Contains(string(last.Content[1].Content), "Error: fetch failed") {
		t.Fatalf("third request %+v", last)
	}
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "image" || b.Type == "mado_file" {
				t.Fatalf("generated image sent back: %+v", b)
			}
		}
	}
	// Stored: the image as a mado_file after the results; renders as a generated file.
	if !strings.Contains(h.raw(4), `"type":"mado_file","file_id":"gen1"`) || !strings.Contains(h.raw(4), `"origin":"generated"`) {
		t.Fatalf("image event %s", h.raw(4))
	}
	want := "user,assistant,tool_use,tool_use,tool_result,tool_result,tool_use,tool_use,tool_result,tool_result!,file(generated),assistant,result"
	if got := blockKinds(h); got != want {
		t.Errorf("blocks\n got %s\nwant %s", got, want)
	}
	// Usage: three responses and the image.
	if len(h.usage) != 4 || h.usage[2] != "img_gen1 gpt-image-1 20/0/272" {
		t.Errorf("usage %v", h.usage)
	}
	var res struct {
		NumTurns int     `json:"num_turns"`
		Cost     float64 `json:"total_cost_usd"`
	}
	json.Unmarshal([]byte(h.raw(6)), &res)
	if res.NumTurns != 3 || res.Cost != 2 {
		t.Errorf("result %s", h.raw(6))
	}
	// The next turn replays the whole loop (tool pairs intact).
	r.Send(turn(testModel(up.srv.URL), "thanks"))
	r.Wait("s1")
	if msgs := up.req(3).Messages; len(msgs) != 7 || msgs[6].Content[len(msgs[6].Content)-1].Text != "thanks" {
		t.Fatalf("replay %d messages", len(msgs))
	}
}

func TestToolRoundLimit(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		sseWrite(w, toolStream(fmt.Sprint("m", n), "", [2]string{"web_search", `{"query":"again"}`}))
	})
	h := &fakeHost{}
	r := newRunner(h)
	r.MaxToolRounds = 2
	r.Tools = &tools.Registry{Search: &fakeSearch{}}
	r.Send(turn(testModel(up.srv.URL), "loop"))
	r.Wait("s1")
	if got := strings.Join(h.types(), ","); got != "user,assistant,user,assistant,user,assistant,user,system/mado_tool_limit,result/success" {
		t.Fatalf("events %s", got)
	}
	if len(up.reqs) != 3 || !strings.Contains(h.raw(6), "Not run: the limit of 2 tool rounds") {
		t.Fatalf("requests %d, last results %s", len(up.reqs), h.raw(6))
	}
	bs := render.Blocks([]byte(h.raw(7)))
	if len(bs) != 1 || !strings.Contains(bs[0].Text, "stopped after 2 tool rounds") {
		t.Errorf("notice %+v", bs)
	}
}

func TestInterruptDuringTool(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			sseWrite(w, toolStream("m1", "", [2]string{"web_search", `{"query":"slow"}`}, [2]string{"web_search", `{"query":"never"}`}))
			return
		}
		sseWrite(w, toolStream("m2", "ok"))
	})
	h := &fakeHost{}
	r := newRunner(h)
	search := &fakeSearch{block: make(chan struct{})}
	r.Tools = &tools.Registry{Search: search}
	m := testModel(up.srv.URL)
	r.Send(turn(m, "search"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		search.mu.Lock()
		n := len(search.qs)
		search.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.Interrupt("s1")
	r.Wait("s1")
	if got := strings.Join(h.types(), ","); got != "user,assistant,user,result/interrupted" {
		t.Fatalf("events %s", got)
	}
	if strings.Count(h.raw(2), "Interrupted by the user.") != 2 || len(search.qs) != 1 {
		t.Fatalf("results %s, queries %v", h.raw(2), search.qs)
	}
	// The conversation stays valid for the next turn.
	r.Send(turn(m, "never mind"))
	r.Wait("s1")
	msgs := up.req(1).Messages
	if len(msgs) != 3 || msgs[1].Content[0].Type != "tool_use" || msgs[2].Content[0].Type != "tool_result" {
		t.Fatalf("next request %+v", msgs)
	}
}

func TestDanglingToolUseDropped(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) { sseWrite(w, toolStream("m", "ok")) })
	h := &fakeHost{}
	h.Append("s1", []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"q"}]}}`))
	h.Append("s1", []byte(`{"type":"assistant","message":{"id":"a","role":"assistant","content":[{"type":"text","text":"partial"},{"type":"tool_use","id":"t1","name":"web_search","input":{}}],"stop_reason":"interrupted"}}`))
	h.Append("s1", []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"zz","content":"orphan"}]}}`))
	r := newRunner(h)
	r.Send(turn(testModel(up.srv.URL), "again"))
	r.Wait("s1")
	msgs := up.req(0).Messages
	if len(msgs) != 3 || len(msgs[1].Content) != 1 || msgs[1].Content[0].Text != "partial" || len(msgs[2].Content) != 1 ||
		msgs[2].Content[0].Text != "again" {
		t.Fatalf("request %+v", msgs)
	}
}
