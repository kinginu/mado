package messages

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/oai/oaitest"
	"github.com/kinginu/mado/internal/proto"
)

func openaiUp(url string) Upstream {
	return Upstream{BaseURL: url + "/v1", Kind: proto.UpstreamOpenAI}
}

func drain(t *testing.T, ch <-chan Event) (*Response, []string, error) {
	t.Helper()
	var resp *Response
	var types []string
	var err error
	for e := range ch {
		types = append(types, e.Type)
		switch e.Type {
		case "message_stop":
			resp = e.Response
		case "error":
			resp, err = e.Response, e.Err
		}
	}
	return resp, types, err
}

func TestOpenAIStreamWithForcedTool(t *testing.T) {
	fake := oaitest.New(t, func(n int, r oaitest.Request) oaitest.Reply {
		if n == 1 {
			return oaitest.Reply{Status: 503, ErrorBody: `{"error":{"message":"loading model","type":"server_error"}}`}
		}
		return oaitest.Reply{Chunks: []string{oaitest.Role(), oaitest.ToolStart(0, "call_9", "decide"),
			oaitest.ToolArgs(0, `{"decision":"allow",`), oaitest.ToolArgs(0, `"reason":"read-only"}`), oaitest.Finish("tool_calls")},
			Usage: `{"prompt_tokens":40,"completion_tokens":12}`}
	})
	c := fastClient()
	up := openaiUp(fake.URL)
	up.Key = "k"
	ch, err := c.Stream(context.Background(), up, Request{Model: "qwen3:8b", MaxTokens: 256, System: "decide",
		Messages:   []Message{{Role: "user", Content: []Block{{Type: "text", Text: "ls?"}}}},
		Tools:      []Tool{{Name: "decide", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: &ToolChoice{Type: "tool", Name: "decide"}})
	if err != nil {
		t.Fatal(err)
	}
	resp, types, err := drain(t, ch)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(types, ","); got != "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop" {
		t.Fatalf("events %s", got)
	}
	if len(resp.Content) != 1 || resp.Content[0].ID != "call_9" || string(resp.Content[0].Input) != `{"decision":"allow","reason":"read-only"}` ||
		resp.StopReason != "tool_use" || resp.Usage.InputTokens != 40 || resp.Usage.OutputTokens != 12 {
		t.Fatalf("%+v", resp)
	}
	reqs := fake.Requests()
	if len(reqs) != 2 || reqs[1].Path != "/v1/chat/completions" || reqs[1].Header.Get("Authorization") != "Bearer k" ||
		reqs[1].Header.Get("Anthropic-Version") != "" || reqs[1].Body["model"] != "qwen3:8b" {
		t.Fatalf("requests %+v", reqs)
	}
	tc, _ := json.Marshal(reqs[1].Body["tool_choice"])
	if string(tc) != `{"function":{"name":"decide"},"type":"function"}` {
		t.Fatalf("tool_choice %s", tc)
	}
}

func TestOpenAIErrors(t *testing.T) {
	fake := oaitest.New(t, func(n int, r oaitest.Request) oaitest.Reply {
		switch n {
		case 1:
			return oaitest.Reply{Status: 400, ErrorBody: `{"object":"error","message":"This model's maximum context length is 4096 tokens.","code":400}`}
		case 2: // nothing but a role chunk, then the stream ends: retried
			return oaitest.Reply{Chunks: []string{oaitest.Role()}, NoDone: true}
		default:
			return oaitest.Reply{Chunks: []string{oaitest.Text("fine")}, NoDone: true} // no finish: ends as end_turn
		}
	})
	c := fastClient()
	_, err := c.Stream(context.Background(), openaiUp(fake.URL), Request{Model: "m", MaxTokens: 5})
	if !IsContextOverflow(err) {
		t.Fatalf("err %v", err)
	}
	ch, err := c.Stream(context.Background(), openaiUp(fake.URL), Request{Model: "m", MaxTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	resp, _, err := drain(t, ch)
	if err != nil || resp.Text() != "fine" || resp.StopReason != "end_turn" || len(fake.Requests()) != 3 {
		t.Fatalf("%v %+v after %d requests", err, resp, len(fake.Requests()))
	}
}

// The first-byte timeout counts to the first body byte: the fake sends its headers
// at once and then holds the body, as vLLM does during a long prefill.
func TestOpenAIFirstByteTimeoutAfterHeaders(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	fake := oaitest.New(t, func(n int, r oaitest.Request) oaitest.Reply {
		return oaitest.Reply{Hold: hold, Chunks: []string{oaitest.Text("late")}}
	})
	c := fastClient()
	c.Backoff = nil
	up := openaiUp(fake.URL)
	up.FirstByte = 100 * time.Millisecond // overrides the client's
	ch, err := c.Stream(context.Background(), up, Request{Model: "m", MaxTokens: 5})
	if err != nil { // the headers came, so the failure arrives as an error event
		t.Fatal(err)
	}
	_, _, err = drain(t, ch)
	if err == nil || !strings.Contains(err.Error(), "no response within 100ms") {
		t.Fatalf("err %v", err)
	}
}
