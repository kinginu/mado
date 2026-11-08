package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kinginu/mado/internal/oai/oaitest"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/tools"
)

// A chat turn with a tool call on an OpenAI-compatible model (DESIGN §22.3): the
// runner, the messages client and the adapter together.
func TestChatTurnOnOpenAIModel(t *testing.T) {
	fake := oaitest.New(t, func(n int, r oaitest.Request) oaitest.Reply {
		if n == 1 {
			return oaitest.Reply{Chunks: []string{oaitest.Role(), oaitest.Reasoning("search first"),
				oaitest.ToolStart(0, "chatcmpl-tool-77", "web_search"), oaitest.ToolArgs(0, `{"query":`), oaitest.ToolArgs(0, `"mado"}`),
				oaitest.Finish("tool_calls")}, Usage: `{"prompt_tokens":500,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":256}}`}
		}
		return oaitest.Reply{Chunks: []string{oaitest.Role(), oaitest.Text("Found "), oaitest.Text("it."), oaitest.Finish("stop")},
			Usage: `{"prompt_tokens":600,"completion_tokens":4}`}
	})
	m := Model{Name: "local", BaseURL: fake.URL + "/v1", Model: "qwen3.6-27b", Kind: proto.UpstreamOpenAI, TimeoutFirstByte: "180s"}
	if err := m.Normalize(); err != nil {
		t.Fatal(err)
	}
	if m.NativeDocuments() {
		t.Fatal("an openai model takes PDFs as text")
	}
	h := &fakeHost{}
	r := newRunner(h)
	search := &fakeSearch{}
	r.Tools = &tools.Registry{Search: search}
	tu := turn(m, "find mado")
	tu.Key = ""
	if _, err := r.Send(tu); err != nil {
		t.Fatal(err)
	}
	r.Wait("s1")
	if got := strings.Join(h.types(), ","); got != "user,assistant,user,assistant,result/success" {
		t.Fatalf("events %s", got)
	}
	if !strings.Contains(h.raw(1), `"id":"chatcmpl-tool-77"`) || !strings.Contains(h.raw(1), `"model":"qwen3.6-27b"`) ||
		!strings.Contains(h.raw(3), "Found it.") || len(search.qs) != 1 || search.qs[0] != "mado" {
		t.Fatalf("assistant %s / %s, queries %v", h.raw(1), h.raw(3), search.qs)
	}
	reqs := fake.Requests()
	if len(reqs) != 2 || reqs[0].Header.Get("Authorization") != "" {
		t.Fatalf("%d requests", len(reqs))
	}
	var second struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Function struct{ Name string } `json:"function"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(reqs[1].Raw, &second)
	ms := second.Messages
	if len(ms) != 4 || ms[0].Role != "system" || ms[2].Role != "assistant" || ms[3].Role != "tool" ||
		ms[3].ToolCallID != "chatcmpl-tool-77" || len(second.Tools) == 0 || second.Tools[0].Function.Name != "web_search" {
		t.Fatalf("second request %s", reqs[1].Raw)
	}
	if strings.Contains(string(reqs[1].Raw), "search first") {
		t.Fatal("thinking was sent back up")
	}
	// usage: 500 prompt with 256 cached + 600 prompt, as Anthropic counts
	if !strings.Contains(h.raw(4), `"cache_read_input_tokens":256`) {
		t.Fatalf("result %s", h.raw(4))
	}
}

func TestOpenAIModelValidation(t *testing.T) {
	yes := true
	for want, m := range map[string]Model{
		"kind must be":    {Kind: "vertex"},
		"Bearer":          {Kind: proto.UpstreamOpenAI, AuthHeader: "x-api-key"},
		"documents":       {Kind: proto.UpstreamOpenAI, Documents: &yes},
		"timeout_total":   {TimeoutTotal: "forever"},
		"must not exceed": {TimeoutFirstByte: "11m"},
	} {
		m.Name, m.BaseURL, m.Model = "x", "http://127.0.0.1:11434/v1", "llama"
		if err := m.Normalize(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	m := Model{Name: "x", BaseURL: "http://127.0.0.1:11434/v1", Model: "llama", Kind: proto.UpstreamOpenAI, TimeoutTotal: "30m", TimeoutFirstByte: "15m"}
	if err := m.Normalize(); err != nil {
		t.Fatal(err)
	}
	if up := m.Upstream("k"); up.Kind != "openai" || up.FirstByte.Minutes() != 15 || up.Total.Minutes() != 30 || up.Key != "k" {
		t.Fatalf("%+v", up)
	}
}
