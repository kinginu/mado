package oai

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		gi, _ := json.MarshalIndent(g, "", "  ")
		wi, _ := json.MarshalIndent(w, "", "  ")
		t.Fatalf("got\n%s\nwant\n%s", gi, wi)
	}
}

// A Claude Code-shaped request: system blocks with cache_control, thinking, tools,
// a tool_use/tool_result round trip with a parallel call, an image, metadata.
const claudeRequest = `{
  "model": "oc-strong",
  "max_tokens": 32000,
  "stream": true,
  "temperature": 1,
  "top_k": 40,
  "stop_sequences": ["</done>"],
  "metadata": {"user_id": "user_abc"},
  "thinking": {"type": "enabled", "budget_tokens": 8000},
  "context_management": {"edits": []},
  "system": [
    {"type": "text", "text": "x-anthropic-billing-header: cc"},
    {"type": "text", "text": "You are Claude Code.", "cache_control": {"type": "ephemeral"}}
  ],
  "tools": [
    {"name": "Bash", "description": "Run a command", "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}},
    {"name": "Read", "input_schema": {"type": "object", "properties": {"file_path": {"type": "string"}}}, "cache_control": {"type": "ephemeral"}},
    {"type": "web_search_20250305", "name": "web_search", "max_uses": 5}
  ],
  "tool_choice": {"type": "auto"},
  "messages": [
    {"role": "user", "content": "list the files"},
    {"role": "assistant", "content": [
      {"type": "thinking", "thinking": "I should run ls", "signature": "sig"},
      {"type": "text", "text": "Running two commands."},
      {"type": "tool_use", "id": "call_abc", "name": "Bash", "input": {"command": "ls"}},
      {"type": "tool_use", "id": "chatcmpl-tool-9928b2", "name": "Read", "input": {"file_path": "/a.png"}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "call_abc", "content": "a.go\nb.go"},
      {"type": "tool_result", "tool_use_id": "chatcmpl-tool-9928b2", "content": [
        {"type": "text", "text": "the image:"},
        {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBO"}}
      ]},
      {"type": "text", "text": "<system-reminder>hi</system-reminder>", "cache_control": {"type": "ephemeral"}}
    ]},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "call_2", "name": "Bash", "input": {"command": "false"}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "call_2", "is_error": true, "content": "exit 1"}]},
    {"role": "user", "content": [
      {"type": "text", "text": "what is this?"},
      {"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "/9j/"}},
      {"type": "document", "title": "notes.txt", "source": {"type": "text", "media_type": "text/plain", "data": "remember milk"}}
    ]}
  ]
}`

const wantRequest = `{
  "model": "zai-org/GLM-5.1",
  "max_tokens": 32000,
  "temperature": 1,
  "stop": ["</done>"],
  "user": "user_abc",
  "stream": true,
  "stream_options": {"include_usage": true},
  "tool_choice": "auto",
  "tools": [
    {"type": "function", "function": {"name": "Bash", "description": "Run a command", "parameters": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}},
    {"type": "function", "function": {"name": "Read", "parameters": {"type": "object", "properties": {"file_path": {"type": "string"}}}}}
  ],
  "messages": [
    {"role": "system", "content": "x-anthropic-billing-header: cc\n\nYou are Claude Code."},
    {"role": "user", "content": "list the files"},
    {"role": "assistant", "content": "Running two commands.", "tool_calls": [
      {"id": "call_abc", "type": "function", "function": {"name": "Bash", "arguments": "{\"command\":\"ls\"}"}},
      {"id": "chatcmpl-tool-9928b2", "type": "function", "function": {"name": "Read", "arguments": "{\"file_path\":\"/a.png\"}"}}
    ]},
    {"role": "tool", "tool_call_id": "call_abc", "content": "a.go\nb.go"},
    {"role": "tool", "tool_call_id": "chatcmpl-tool-9928b2", "content": "the image:\n[image omitted]"},
    {"role": "user", "content": "<system-reminder>hi</system-reminder>"},
    {"role": "assistant", "content": null, "tool_calls": [
      {"id": "call_2", "type": "function", "function": {"name": "Bash", "arguments": "{\"command\":\"false\"}"}}
    ]},
    {"role": "tool", "tool_call_id": "call_2", "content": "Error: exit 1"},
    {"role": "user", "content": [
      {"type": "text", "text": "what is this?"},
      {"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,/9j/"}},
      {"type": "text", "text": "<document name=\"notes.txt\">\nremember milk\n</document>"}
    ]}
  ]
}`

func TestTranslateRequest(t *testing.T) {
	out, err := TranslateRequest(context.Background(), []byte(claudeRequest), "zai-org/GLM-5.1")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, out, wantRequest)
}

func TestTranslateRequestToolChoice(t *testing.T) {
	tools := `"tools":[{"name":"decide","input_schema":{"type":"object"}}]`
	for _, c := range []struct{ in, want string }{
		{`{"type":"auto"}`, `"auto"`},
		{`{"type":"any"}`, `"required"`},
		{`{"type":"none"}`, `"none"`},
		{`{"type":"tool","name":"decide"}`, `{"type":"function","function":{"name":"decide"}}`},
	} {
		body := `{"model":"m","max_tokens":5,"system":"be brief","messages":[{"role":"user","content":"hi"}],` + tools + `,"tool_choice":` + c.in + `}`
		out, err := TranslateRequest(context.Background(), []byte(body), "")
		if err != nil {
			t.Fatal(err)
		}
		var o struct {
			ToolChoice json.RawMessage `json:"tool_choice"`
			Messages   []struct{ Role string }
			Stream     *bool           `json:"stream"`
			Options    json.RawMessage `json:"stream_options"`
		}
		_ = json.Unmarshal(out, &o)
		jsonEqual(t, o.ToolChoice, c.want)
		if len(o.Messages) != 2 || o.Messages[0].Role != "system" || o.Stream != nil || o.Options != nil {
			t.Fatalf("%s", out)
		}
	}
	// no tools: tool_choice is dropped (servers reject it alone); parallel use off
	out, _ := TranslateRequest(context.Background(), []byte(`{"model":"m","messages":[],"tool_choice":{"type":"any"}}`), "")
	if strings.Contains(string(out), "tool_choice") {
		t.Fatalf("%s", out)
	}
	out, _ = TranslateRequest(context.Background(), []byte(`{"model":"m","messages":[],`+tools+`,"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`), "")
	if !strings.Contains(string(out), `"parallel_tool_calls":false`) {
		t.Fatalf("%s", out)
	}
	if _, err := TranslateRequest(context.Background(), []byte(`[1]`), ""); err == nil {
		t.Fatal("non-object accepted")
	}
}

func TestEstimateTokens(t *testing.T) {
	out, _ := TranslateRequest(context.Background(), []byte(claudeRequest), "")
	if n := EstimateTokens(context.Background(), []byte(claudeRequest)); n != len(out)/4 || n < 100 {
		t.Fatalf("estimate %d, translated %d bytes", n, len(out))
	}
	if n := EstimateTokens(context.Background(), []byte(`not json at all`)); n != 3 {
		t.Fatalf("estimate %d", n)
	}
}

// collect runs Stream and returns the events as "type data" lines.
func collect(t *testing.T, sse string) []string {
	t.Helper()
	var out []string
	err := Stream(context.Background(), strings.NewReader(sse), "zai-org/GLM-5.1", func(ev string, data []byte) error {
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("event %s: %v", ev, err)
		}
		if m["type"] != ev {
			t.Fatalf("event %s carries type %v", ev, m["type"])
		}
		if ev == "message_start" { // the id is random when the server has none
			msg := m["message"].(map[string]any)
			if id, _ := msg["id"].(string); id == "" {
				t.Fatal("no message id")
			}
			msg["id"] = "ID"
			data, _ = json.Marshal(m)
		}
		out = append(out, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func checkEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d events, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		jsonEqual(t, []byte(got[i]), want[i])
	}
}

const msgStart = `{"type":"message_start","message":{"id":"ID","type":"message","role":"assistant","model":"zai-org/GLM-5.1","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`

// vLLM's shape: role chunk, content chunks, finish chunk, usage-only chunk, [DONE].
func TestStreamText(t *testing.T) {
	sse := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"glm","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"glm","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}

: keep-alive

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"glm","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"glm","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":2,"total_tokens":122,"prompt_tokens_details":{"cached_tokens":96}}}

data: [DONE]

`
	checkEvents(t, collect(t, sse), msgStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":24,"cache_read_input_tokens":96,"output_tokens":2}}`,
		`{"type":"message_stop"}`)
}

// reasoning_content (DeepSeek, vLLM) and reasoning (Ollama) become one thinking block.
func TestStreamReasoning(t *testing.T) {
	for _, field := range []string{"reasoning_content", "reasoning"} {
		sse := `data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","` + field + `":"think"}}]}
data: {"id":"x","choices":[{"index":0,"delta":{"` + field + `":"ing"}}]}
data: {"id":"x","choices":[{"index":0,"delta":{"content":"Hi"}}]}
data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}
data: [DONE]
`
		checkEvents(t, collect(t, sse), msgStart,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"ing"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"input_tokens":5,"cache_read_input_tokens":0,"output_tokens":9}}`,
			`{"type":"message_stop"}`)
	}
}

// Parallel tool calls with index, as vLLM streams them: id and name in the first
// fragment of each index, arguments in pieces. The second call has no id (some
// servers) and gets call_<n>; a third repeats the first id and is renamed.
func TestStreamParallelToolCalls(t *testing.T) {
	sse := `data: {"id":"c","choices":[{"index":0,"delta":{"role":"assistant","content":"Two calls."}}]}
data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"chatcmpl-tool-aa","type":"function","function":{"name":"Bash","arguments":""}}]}}]}
data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\": "}}]}}]}
data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}
data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/a\"}"}}]}}]}
data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"chatcmpl-tool-aa","function":{"name":"Glob","arguments":{"pattern":"*.go"}}}]}}]}
data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}
data: {"id":"c","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":30}}
data: [DONE]
`
	checkEvents(t, collect(t, sse), msgStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Two calls."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"chatcmpl-tool-aa","name":"Bash","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\": "}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"Read","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/a\"}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"call_2","name":"Glob","input":{}}}`,
		`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"pattern\":\"*.go\"}"}}`,
		`{"type":"content_block_stop","index":3}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":50,"cache_read_input_tokens":0,"output_tokens":30}}`,
		`{"type":"message_stop"}`)
}

// Ollama-style: one complete tool call per chunk and finish_reason "stop".
func TestStreamToolCallWithStopFinish(t *testing.T) {
	sse := `data: {"id":"chatcmpl-9","choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"id":"call_x1","index":0,"type":"function","function":{"name":"decide","arguments":"{\"decision\":\"allow\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-9","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-9","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4}}

data: [DONE]

`
	ev := collect(t, sse)
	if len(ev) != 6 || !strings.Contains(ev[1], `"id":"call_x1"`) || !strings.Contains(ev[4], `"stop_reason":"tool_use"`) {
		t.Fatalf("%s", strings.Join(ev, "\n"))
	}
}

func TestStreamErrors(t *testing.T) {
	// error mid-stream after content: the message ends normally with end_turn
	sse := `data: {"id":"e","choices":[{"index":0,"delta":{"content":"partial"}}]}
data: {"error":{"message":"engine died","type":"InternalServerError","code":500}}
`
	checkEvents(t, collect(t, sse), msgStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`,
		`{"type":"message_stop"}`)

	// an error before any content: an error event only
	checkEvents(t, collect(t, `data: {"object":"error","message":"The model is overloaded","type":"BadRequestError","code":503}

`), `{"type":"error","error":{"type":"api_error","message":"The model is overloaded"}}`)

	// a stream that ends without [DONE] after a role chunk: message_start, then the error
	checkEvents(t, collect(t, `data: {"id":"e","choices":[{"index":0,"delta":{"role":"assistant"}}]}
`), msgStart, `{"type":"error","error":{"type":"api_error","message":"upstream stream ended early"}}`)

	// a malformed chunk after content ends the message
	ev := collect(t, `data: {"id":"e","choices":[{"index":0,"delta":{"content":"ok"}}]}
data: {"id":
`)
	if len(ev) != 6 || !strings.Contains(ev[4], "end_turn") {
		t.Fatalf("%s", strings.Join(ev, "\n"))
	}

	// EOF without [DONE] but with a finish_reason is a normal end
	ev = collect(t, `data: {"id":"e","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"length"}]}
`)
	if len(ev) != 6 || !strings.Contains(ev[4], "max_tokens") {
		t.Fatalf("%s", strings.Join(ev, "\n"))
	}

	// llama.cpp's error: line
	checkEvents(t, collect(t, `error: {"code":500,"message":"slot unavailable","type":"server_error"}
`), `{"type":"error","error":{"type":"api_error","message":"slot unavailable"}}`)

	// an empty answer is still an answer
	ev = collect(t, "data: {\"id\":\"e\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\ndata: [DONE]\n")
	if len(ev) != 3 || !strings.Contains(ev[1], "end_turn") {
		t.Fatalf("%s", strings.Join(ev, "\n"))
	}
}

func TestTranslateResponse(t *testing.T) {
	body := `{"id":"chatcmpl-7","object":"chat.completion","model":"glm","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"hmm","content":"Calling.","tool_calls":[{"id":"call_q","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}},{"type":"function","function":{"name":"Read","arguments":"not json"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":10}}}`
	out, err := TranslateResponse([]byte(body), "zai-org/GLM-5.1")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, out, `{"id":"chatcmpl-7","type":"message","role":"assistant","model":"zai-org/GLM-5.1","stop_reason":"tool_use","stop_sequence":null,
	  "content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Calling."},
	    {"type":"tool_use","id":"call_q","name":"Bash","input":{"command":"ls"}},
	    {"type":"tool_use","id":"call_1","name":"Read","input":{}}],
	  "usage":{"input_tokens":20,"cache_read_input_tokens":10,"output_tokens":7}}`)
}

func TestErrorBody(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"object":"error","message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens.","type":"BadRequestError","code":400}`,
			`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: This model's maximum context length is 32768 tokens. However, you requested 40000 tokens."}}`},
		{404, `{"error":{"message":"model \"qwen\" not found, try pulling it first","type":"api_error"}}`,
			`{"type":"error","error":{"type":"not_found_error","message":"model \"qwen\" not found, try pulling it first"}}`},
		{401, `{"error":"unauthorized"}`, `{"type":"error","error":{"type":"authentication_error","message":"unauthorized"}}`},
		{422, `{"detail":[{"loc":["body","messages"],"msg":"field required"}]}`,
			`{"type":"error","error":{"type":"invalid_request_error","message":"[{\"loc\":[\"body\",\"messages\"],\"msg\":\"field required\"}]"}}`},
		{503, `upstream connect error`, `{"type":"error","error":{"type":"overloaded_error","message":"upstream connect error"}}`},
		{429, ``, `{"type":"error","error":{"type":"rate_limit_error","message":"Too Many Requests"}}`},
	} {
		jsonEqual(t, ErrorBody(c.status, []byte(c.body)), c.want)
	}
}
