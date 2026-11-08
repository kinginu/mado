package messages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// recorded is the shape of a real AI& stream (GLM-5.3, 2026-10-18), reduced: the
// message_start input count is wrong (72) and message_delta has the real one (168),
// the tool id is not toolu_-prefixed, cache_creation_input_tokens is absent.
const recorded = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"zai-org/glm-5.3","content":[],"stop_reason":null,"usage":{"input_tokens":72,"cache_read_input_tokens":0,"output_tokens":1}}}

event: ping
data: {"type":"ping"}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":", world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"chatcmpl-tool-9928b27491265d9d_05mm6pg","name":"web_search","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"query\": \"mado"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":" windows\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":168,"cache_read_input_tokens":32,"output_tokens":41}}

event: message_stop
data: {"type":"message_stop"}

`

func fastClient() *Client {
	c := NewClient()
	c.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
	c.FirstByte = 2 * time.Second
	c.Total = 10 * time.Second
	return c
}

func collect(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var out []Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-timeout:
			t.Fatal("stream did not end")
		}
	}
}

func sse(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	io.WriteString(w, body)
}

func TestStreamRecorded(t *testing.T) {
	var got http.Header
	var gotBody Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		got = r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&gotBody)
		sse(w, recorded)
	}))
	defer srv.Close()
	ch, err := fastClient().Stream(context.Background(), Upstream{BaseURL: srv.URL + "/", Key: "sekret"},
		Request{Model: "zai-org/glm-5.3", MaxTokens: 100, Messages: []Message{{Role: "user", Content: []Block{{Type: "text", Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, ch)
	if got.Get("Authorization") != "Bearer sekret" || got.Get("X-Api-Key") != "" || got.Get("anthropic-version") != Version {
		t.Errorf("headers %v", got)
	}
	if !gotBody.Stream {
		t.Error("stream not set")
	}
	var types []string
	var text strings.Builder
	for _, e := range evs {
		types = append(types, e.Type)
		if e.Type == "content_block_delta" && e.Delta.Type == "text_delta" {
			text.WriteString(e.Delta.Text)
		}
	}
	if types[0] != "message_start" || types[len(types)-1] != "message_stop" || text.String() != "Hello, world" {
		t.Fatalf("events %v text %q", types, text.String())
	}
	r := evs[len(evs)-1].Response
	if r.ID != "msg_1" || r.Model != "zai-org/glm-5.3" || r.StopReason != "tool_use" || r.Text() != "Hello, world" || len(r.Content) != 3 {
		t.Fatalf("response %+v", r)
	}
	if r.Content[0].Type != "thinking" || r.Content[0].Thinking != "hmm" {
		t.Errorf("thinking %+v", r.Content[0])
	}
	tu := r.Content[2]
	var in struct{ Query string }
	if tu.ID != "chatcmpl-tool-9928b27491265d9d_05mm6pg" || json.Unmarshal(tu.Input, &in) != nil || in.Query != "mado windows" {
		t.Errorf("tool_use %+v %s", tu, tu.Input)
	}
	// message_delta wins over message_start; the absent cache_creation stays 0.
	if u := r.Usage; u.InputTokens != 168 || u.OutputTokens != 41 || u.CacheReadInputTokens != 32 || u.CacheCreationInputTokens != 0 {
		t.Errorf("usage %+v", u)
	}
}

func TestUsageFallsBackToMessageStart(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"id":"m","role":"assistant","model":"x","usage":{"input_tokens":50,"cache_read_input_tokens":7}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sse(w, body) }))
	defer srv.Close()
	ch, err := fastClient().Stream(context.Background(), Upstream{BaseURL: srv.URL}, Request{Model: "x"})
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, ch)
	u := evs[len(evs)-1].Response.Usage
	if u.InputTokens != 50 || u.CacheReadInputTokens != 7 || u.OutputTokens != 3 {
		t.Fatalf("usage %+v", u)
	}
}

func TestRetries(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.WriteHeader(503)
			io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
		case 2:
			// a stream that fails before its first block is retried as well
			sse(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"a\"}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"later\"}}\n\n")
		default:
			sse(w, recorded)
		}
	}))
	defer srv.Close()
	ch, err := fastClient().Stream(context.Background(), Upstream{BaseURL: srv.URL}, Request{Model: "x"})
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, ch)
	starts := 0
	for _, e := range evs {
		if e.Type == "message_start" {
			starts++
		}
	}
	if n.Load() != 3 || starts != 1 || evs[len(evs)-1].Type != "message_stop" {
		t.Fatalf("attempts %d starts %d last %s", n.Load(), starts, evs[len(evs)-1].Type)
	}
}

func TestNoRetryOn400AndGiveUpAfterThree(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.Header.Get("X-Case") == "400" {
			w.WriteHeader(400)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 300000 tokens > 200000 maximum","code":null,"param":null},"request_id":"r1"}`)
			return
		}
		w.WriteHeader(429)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer srv.Close()
	c := fastClient()
	c.HTTP.Transport = headerRT{c.HTTP.Transport, "400"}
	_, err := c.Stream(context.Background(), Upstream{BaseURL: srv.URL}, Request{Model: "x"})
	if !IsContextOverflow(err) || n.Load() != 1 {
		t.Fatalf("400: %v after %d attempts", err, n.Load())
	}
	n.Store(0)
	_, err = fastClient().Stream(context.Background(), Upstream{BaseURL: srv.URL}, Request{Model: "x"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 429 || ae.Message != "slow down" || n.Load() != 3 || IsContextOverflow(err) {
		t.Fatalf("429: %v after %d attempts", err, n.Load())
	}
}

type headerRT struct {
	rt  http.RoundTripper
	val string
}

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Case", h.val)
	return h.rt.RoundTrip(r)
}

func TestErrorAfterBlockIsNotRetried(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		cut := strings.Index(recorded, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}")
		sse(w, recorded[:cut]+"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"gone\"}}\n\n")
	}))
	defer srv.Close()
	ch, err := fastClient().Stream(context.Background(), Upstream{BaseURL: srv.URL}, Request{Model: "x"})
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, ch)
	last := evs[len(evs)-1]
	if n.Load() != 1 || last.Type != "error" || last.Response.Text() != "Hello, world" || !strings.Contains(last.Err.Error(), "gone") {
		t.Fatalf("attempts %d last %+v", n.Load(), last)
	}
}

func TestCancelGivesPartial(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cut := strings.Index(recorded, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}")
		sse(w, recorded[:cut])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := fastClient().Stream(ctx, Upstream{BaseURL: srv.URL}, Request{Model: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var last Event
	for e := range ch {
		if e.Type == "content_block_delta" && e.Delta.Text == ", world" {
			cancel()
		}
		last = e
	}
	if last.Type != "error" || !errors.Is(last.Err, context.Canceled) || last.Response.Text() != "Hello, world" || last.Response.ID != "msg_1" {
		t.Fatalf("last %+v", last)
	}
}

func TestFirstByteTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()
	c := fastClient()
	c.FirstByte = 50 * time.Millisecond
	c.Backoff = nil
	_, err := c.Stream(context.Background(), Upstream{BaseURL: srv.URL}, Request{Model: "x"})
	if err == nil || !strings.Contains(err.Error(), "no response within") {
		t.Fatalf("err %v", err)
	}
}

func TestParseErrorBodyShapes(t *testing.T) {
	for _, c := range []struct{ body, typ, msg string }{
		{`{"type":"error","error":{"type":"not_found_error","message":"no"},"request_id":"x"}`, "not_found_error", "no"},
		{`{"error":{"message":"The model does not exist","type":"NotFoundError","code":404}}`, "NotFoundError", "The model does not exist"},
		{`{"object":"error","message":"route not found","code":404}`, "", "route not found"},
		{`{"error":"plain"}`, "", "plain"},
		{`not json`, "", "not json"},
	} {
		e := parseErrorBody(404, []byte(c.body))
		if e.Type != c.typ || e.Message != c.msg {
			t.Errorf("%s -> %+v", c.body, e)
		}
	}
}

func TestAuthHeader(t *testing.T) {
	for _, c := range []struct{ base, explicit, want string }{
		{"https://api.anthropic.com", "", AuthAPIKey},
		{"https://API.anthropic.com/", "", AuthAPIKey},
		{"https://api.aiand.com", "", AuthBearer},
		{"https://api.anthropic.com", "bearer", AuthBearer},
		{"http://gpu1:8000", "x-api-key", AuthAPIKey},
	} {
		if got := AuthHeader(c.base, c.explicit); got != c.want {
			t.Errorf("%s %q: %s", c.base, c.explicit, got)
		}
	}
	h := http.Header{}
	SetAuth(h, "https://api.anthropic.com", "", "k")
	if h.Get("X-Api-Key") != "k" || h.Get("Authorization") != "" {
		t.Error(h)
	}
	h = http.Header{}
	SetAuth(h, "https://x", "", "")
	if len(h) != 0 {
		t.Error("empty key set a header")
	}
}

func TestErrorDoesNotLeakKey(t *testing.T) {
	_, err := fastClient().Stream(context.Background(), Upstream{BaseURL: "http://127.0.0.1:1", Key: "sekret-key"}, Request{Model: "x"})
	if err == nil || strings.Contains(fmt.Sprint(err), "sekret") {
		t.Fatalf("err %v", err)
	}
}
