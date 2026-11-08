package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/oai/oaitest"
	"github.com/kinginu/mado/internal/proto"
)

// A profile with fable on a local OpenAI-compatible server and opus on an Anthropic one.
func openaiProfile(local, aiand string) proto.Profile {
	return proto.Profile{
		Name: "local-mix", Kind: proto.KindRouter, Main: "local-glm",
		Aliases: map[string]string{"fable": "local-glm", "opus": "ai-ds"},
		Upstreams: map[string]proto.Upstream{
			"local-glm": {BaseURL: local + "/v1", Model: "zai-org/GLM-5.1-FP8", Kind: proto.UpstreamOpenAI, TimeoutFirstByte: "180s"},
			"ai-ds":     {BaseURL: aiand, Model: "deepseek-v4-pro", Key: "aiand-key"},
		},
	}
}

func frontFor(t *testing.T, p proto.Profile) *httptest.Server {
	t.Helper()
	if err := Validate(p); err != nil {
		t.Fatal(err)
	}
	r := New(nil)
	r.Register("tok", "s1", p)
	front := httptest.NewServer(r)
	t.Cleanup(front.Close)
	return front
}

// End to end: Claude Code's request through the router to a fake vLLM, a turn with
// a tool call, then the tool result going back with the same id.
func TestOpenAIUpstreamEndToEnd(t *testing.T) {
	fake := oaitest.New(t, func(n int, r oaitest.Request) oaitest.Reply {
		if n == 1 {
			return oaitest.Reply{Chunks: []string{oaitest.Role(), oaitest.Reasoning("check"), oaitest.Text("Looking."),
				oaitest.ToolStart(0, "chatcmpl-tool-1a2b", "Bash"), oaitest.ToolArgs(0, `{"command":`), oaitest.ToolArgs(0, `"ls"}`),
				oaitest.ToolStart(1, "call_x", "Read"), oaitest.ToolArgs(1, `{"file_path":"/a"}`), oaitest.Finish("tool_calls")},
				Usage: `{"prompt_tokens":900,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":768}}`}
		}
		return oaitest.Reply{Chunks: []string{oaitest.Role(), oaitest.Text("Done."), oaitest.Finish("stop")},
			Usage: `{"prompt_tokens":950,"completion_tokens":3}`}
	})
	front := frontFor(t, openaiProfile(fake.URL, "http://127.0.0.1:1"))
	c := messages.NewClient()
	c.Backoff = nil
	up := messages.Upstream{BaseURL: front.URL, Key: "tok"}
	req := messages.Request{Model: "local-glm", MaxTokens: 32000, System: "You are Claude Code.",
		Messages: []messages.Message{{Role: "user", Content: []messages.Block{{Type: "text", Text: "list"}}}},
		Tools:    []messages.Tool{{Name: "Bash", InputSchema: json.RawMessage(`{"type":"object"}`)}, {Name: "Read", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	resp := final(t, c, up, req)
	if len(resp.Content) != 4 || resp.Content[0].Type != "thinking" || resp.Content[0].Thinking != "check" ||
		resp.Content[1].Text != "Looking." || resp.Content[2].ID != "chatcmpl-tool-1a2b" || string(resp.Content[2].Input) != `{"command":"ls"}` ||
		resp.Content[3].ID != "call_x" || resp.StopReason != "tool_use" || resp.Model != "zai-org/GLM-5.1-FP8" {
		t.Fatalf("response %+v", resp)
	}
	if u := resp.Usage; u.InputTokens != 132 || u.CacheReadInputTokens != 768 || u.OutputTokens != 40 {
		t.Fatalf("usage %+v", u)
	}
	seen := fake.Requests()[0]
	if seen.Path != "/v1/chat/completions" || seen.Body["model"] != "zai-org/GLM-5.1-FP8" || seen.Header.Get("Authorization") != "" ||
		seen.Header.Get("Anthropic-Version") != "" || seen.Body["stream"] != true {
		t.Fatalf("upstream saw %s %v auth=%q", seen.Path, seen.Body, seen.Header.Get("Authorization"))
	}

	// the next turn carries the tool results; the ids round-trip unchanged
	req.Messages = append(req.Messages, messages.Message{Role: "assistant", Content: resp.Content},
		messages.Message{Role: "user", Content: []messages.Block{
			{Type: "tool_result", ToolUseID: "chatcmpl-tool-1a2b", Content: json.RawMessage(`"a.go"`)},
			{Type: "tool_result", ToolUseID: "call_x", Content: json.RawMessage(`[{"type":"text","text":"x"}]`)}}})
	resp = final(t, c, up, req)
	if resp.Text() != "Done." || resp.StopReason != "end_turn" {
		t.Fatalf("second %+v", resp)
	}
	var body struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(fake.Requests()[1].Raw, &body)
	ms := body.Messages
	if len(ms) != 5 || ms[2].Role != "assistant" || len(ms[2].ToolCalls) != 2 || ms[2].ToolCalls[0].ID != "chatcmpl-tool-1a2b" ||
		ms[3].ToolCallID != "chatcmpl-tool-1a2b" || ms[4].ToolCallID != "call_x" {
		t.Fatalf("second request %s", fake.Requests()[1].Raw)
	}
}

func final(t *testing.T, c *messages.Client, up messages.Upstream, req messages.Request) *messages.Response {
	t.Helper()
	ch, err := c.Stream(context.Background(), up, req)
	if err != nil {
		t.Fatal(err)
	}
	var resp *messages.Response
	for e := range ch {
		switch e.Type {
		case "message_stop":
			resp = e.Response
		case "error":
			t.Fatalf("stream error %v", e.Err)
		}
	}
	if resp == nil {
		t.Fatal("no message_stop")
	}
	return resp
}

func TestOpenAIUpstreamNonStreamingCountTokensAndErrors(t *testing.T) {
	fake := oaitest.New(t, func(n int, r oaitest.Request) oaitest.Reply {
		switch n {
		case 1:
			return oaitest.Reply{Message: `{"id":"chatcmpl-n","object":"chat.completion","model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`}
		default:
			return oaitest.Reply{Status: 400, ErrorBody: `{"object":"error","message":"This model's maximum context length is 8192 tokens.","type":"BadRequestError","code":400}`}
		}
	})
	p := openaiProfile(fake.URL, "http://127.0.0.1:1")
	u := p.Upstreams["local-glm"]
	u.Key = "local-key"
	p.Upstreams["local-glm"] = u
	front := frontFor(t, p)
	body := `{"model":"local-glm","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`

	resp := post(t, front.URL+"/v1/messages", "tok", body, nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"text":"hi"`) || !strings.Contains(string(b), `"model":"zai-org/GLM-5.1-FP8"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if got := fake.Requests()[0].Header.Get("Authorization"); got != "Bearer local-key" {
		t.Fatalf("auth %q", got)
	}

	resp = post(t, front.URL+"/v1/messages/count_tokens", "tok", body, nil)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var ct struct {
		InputTokens int `json:"input_tokens"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(b, &ct) != nil || ct.InputTokens < 10 || len(fake.Requests()) != 1 {
		t.Fatalf("count_tokens %d %s", resp.StatusCode, b)
	}

	resp = post(t, front.URL+"/v1/messages", "tok", body, nil)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), `"type":"invalid_request_error"`) || !strings.Contains(string(b), "prompt is too long") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

// A server that sends its headers and then nothing: the first-byte timeout answers 504
// before anything reached Claude Code.
func TestOpenAIUpstreamFirstByteTimeout(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	fake := oaitest.New(t, func(n int, r oaitest.Request) oaitest.Reply {
		return oaitest.Reply{Hold: hold, Chunks: []string{oaitest.Text("late")}}
	})
	p := openaiProfile(fake.URL, "http://127.0.0.1:1")
	u := p.Upstreams["local-glm"]
	u.TimeoutFirstByte = "1s"
	p.Upstreams["local-glm"] = u
	front := frontFor(t, p)
	start := time.Now()
	resp := post(t, front.URL+"/v1/messages", "tok", `{"model":"local-glm","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout || !strings.Contains(string(b), "no response within 1s") || time.Since(start) > 5*time.Second {
		t.Fatalf("%d %s after %s", resp.StatusCode, b, time.Since(start))
	}
}

func TestValidateKindAndTimeouts(t *testing.T) {
	base := func(mut func(*proto.Upstream)) proto.Profile {
		p := openaiProfile("http://127.0.0.1:8000", "https://api.aiand.com")
		u := p.Upstreams["local-glm"]
		mut(&u)
		p.Upstreams["local-glm"] = u
		return p
	}
	ok := []func(*proto.Upstream){
		func(u *proto.Upstream) {},
		func(u *proto.Upstream) { u.Kind = proto.UpstreamAnthropic; u.TimeoutFirstByte = "" },
		func(u *proto.Upstream) { u.Kind = ""; u.TimeoutTotal = "30m" },
		func(u *proto.Upstream) { u.AuthHeader = "bearer"; u.KeyRef = "" },
	}
	for i, m := range ok {
		if err := Validate(base(m)); err != nil {
			t.Errorf("ok %d: %v", i, err)
		}
	}
	bad := map[string]func(*proto.Upstream){
		"kind":       func(u *proto.Upstream) { u.Kind = "gemini" },
		"Bearer":     func(u *proto.Upstream) { u.AuthHeader = "x-api-key" },
		"1s to 24h":  func(u *proto.Upstream) { u.TimeoutTotal = "soon" },
		"1s to 24h ": func(u *proto.Upstream) { u.TimeoutFirstByte = "100ms" },
		"not exceed": func(u *proto.Upstream) { u.TimeoutFirstByte = "20m" },
	}
	for want, m := range bad {
		if err := Validate(base(m)); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%s: %v", want, err)
		}
	}
}
