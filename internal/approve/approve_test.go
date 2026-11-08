package approve

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The exact sequence Claude Code 2.1.302 sent in the recorded probe run.
const clientScript = `{"jsonrpc":"2.0","id":"server-discover-probe-1","method":"server/discover","params":{}}
{"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}},"jsonrpc":"2.0","id":0}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"method":"tools/list","jsonrpc":"2.0","id":1}
{"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Write","input":{"file_path":"/x/probe.txt","content":"hi\n"},"tool_use_id":"toolu_1"},"_meta":{"claudecode/toolUseId":"toolu_1","progressToken":2}},"jsonrpc":"2.0","id":2}
{"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Bash","input":{"command":"rm -rf /"},"tool_use_id":"toolu_2"}},"jsonrpc":"2.0","id":3}
`

func TestServeProtocol(t *testing.T) {
	pr, pw := io.Pipe()
	var mu sync.Mutex
	var seen []Request
	decide := func(_ context.Context, r Request) Decision {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		return Decision{Allow: r.ToolName == "Write", Message: "no"}
	}
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), strings.NewReader(clientScript), pw, decide); pw.Close() }()

	byID := map[string]map[string]any{}
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad line %s", sc.Bytes())
		}
		id, _ := json.Marshal(m["id"])
		byID[string(id)] = m
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(byID) != 5 {
		t.Fatalf("want 5 replies (no reply to the notification), got %d: %v", len(byID), byID)
	}
	if e := byID[`"server-discover-probe-1"`]["error"].(map[string]any); e["code"].(float64) != -32601 {
		t.Errorf("discover: %v", e)
	}
	if v := byID["0"]["result"].(map[string]any)["protocolVersion"]; v != "2025-11-25" {
		t.Errorf("protocol version not echoed: %v", v)
	}
	tools := byID["1"]["result"].(map[string]any)["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "approve" {
		t.Errorf("tools: %v", tools)
	}
	text := func(id string) map[string]any {
		c := byID[id]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
		if c["type"] != "text" {
			t.Fatalf("content type %v", c["type"])
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(c["text"].(string)), &v); err != nil {
			t.Fatalf("text is not a JSON string: %v", c["text"])
		}
		return v
	}
	allow := text("2")
	if allow["behavior"] != "allow" || allow["updatedInput"].(map[string]any)["file_path"] != "/x/probe.txt" {
		t.Errorf("allow: %v", allow)
	}
	deny := text("3")
	if deny["behavior"] != "deny" || deny["message"] != "no" {
		t.Errorf("deny: %v", deny)
	}
	if len(seen) != 2 {
		t.Errorf("decider saw %+v", seen)
	}
}

func TestMCPConfigShape(t *testing.T) {
	var m map[string]map[string]map[string]any
	if err := json.Unmarshal([]byte(MCPConfig([]string{"/bin/mado", "approve-tool", "--socket", "/s"})), &m); err != nil {
		t.Fatal(err)
	}
	s := m["mcpServers"]["mado"]
	if s["type"] != "stdio" || s["command"] != "/bin/mado" || len(s["args"].([]any)) != 3 {
		t.Fatalf("%v", s)
	}
	if PromptTool != "mcp__mado__approve" {
		t.Fatal(PromptTool)
	}
}

func TestServeHubTools(t *testing.T) {
	script := `{"method":"initialize","params":{"protocolVersion":"2025-11-25"},"jsonrpc":"2.0","id":0}
{"method":"tools/list","jsonrpc":"2.0","id":1}
{"method":"tools/call","params":{"name":"web_search","arguments":{"query":"otters"}},"jsonrpc":"2.0","id":2}
{"method":"tools/call","params":{"name":"generate_image","arguments":{"prompt":"x"}},"jsonrpc":"2.0","id":3}
{"method":"tools/call","params":{"name":"fetch_url","arguments":{"url":"http://10.0.0.1/"}},"jsonrpc":"2.0","id":4}
`
	var mu sync.Mutex
	var calls []string
	call := func(_ context.Context, name string, args json.RawMessage) (string, bool) {
		mu.Lock()
		calls = append(calls, name+" "+string(args))
		mu.Unlock()
		if name == "fetch_url" {
			return "Error: refused", true
		}
		return "1. Sea otter", false
	}
	pr, pw := io.Pipe()
	go func() {
		ServeWith(context.Background(), strings.NewReader(script), pw,
			Options{Decide: func(context.Context, Request) Decision { return Decision{} }, Tools: []string{"web_search", "fetch_url", "generate_image", "web_search"}, Call: call})
		pw.Close()
	}()
	byID := map[string]map[string]any{}
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		var m map[string]any
		json.Unmarshal(sc.Bytes(), &m)
		id, _ := json.Marshal(m["id"])
		byID[string(id)] = m
	}
	var names []string
	for _, tl := range byID["1"]["result"].(map[string]any)["tools"].([]any) {
		tm := tl.(map[string]any)
		names = append(names, tm["name"].(string))
		if tm["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("schema %v", tm)
		}
	}
	if strings.Join(names, ",") != "approve,web_search,fetch_url" {
		t.Fatalf("tools %v", names)
	}
	r2 := byID["2"]["result"].(map[string]any)
	if r2["isError"] != false || r2["content"].([]any)[0].(map[string]any)["text"] != "1. Sea otter" {
		t.Errorf("web_search %v", r2)
	}
	if byID["3"]["error"] == nil {
		t.Errorf("generate_image was callable: %v", byID["3"])
	}
	if r4 := byID["4"]["result"].(map[string]any); r4["isError"] != true {
		t.Errorf("fetch_url %v", r4)
	}
	// The calls run concurrently, so their order is not fixed.
	if len(calls) != 2 || !slices.Contains(calls, `web_search {"query":"otters"}`) {
		t.Errorf("calls %v", calls)
	}
	// Without a caller only approve is offered.
	var out strings.Builder
	Serve(context.Background(), strings.NewReader(`{"method":"tools/list","jsonrpc":"2.0","id":1}`+"\n"), &out, nil)
	if strings.Contains(out.String(), "web_search") {
		t.Errorf("tools without a caller: %s", out.String())
	}
}

// M16: a job's MCP server offers propose_task with the digest's schema and relays it.
func TestServeProposeTask(t *testing.T) {
	script := `{"method":"tools/list","jsonrpc":"2.0","id":1}
{"method":"tools/call","params":{"name":"propose_task","arguments":{"title":"Review","item_ids":[3]}},"jsonrpc":"2.0","id":2}
`
	var got string
	call := func(_ context.Context, name string, args json.RawMessage) (string, bool) {
		got = name + " " + string(args)
		return "Task #1 proposed to the user.", false
	}
	var out strings.Builder
	ServeWith(context.Background(), strings.NewReader(script), &out,
		Options{Decide: func(context.Context, Request) Decision { return Decision{} }, Tools: JobTools, Call: call})
	s := out.String()
	if !strings.Contains(s, `"name":"propose_task"`) || !strings.Contains(s, `"item_ids"`) || !strings.Contains(s, "Task #1 proposed") {
		t.Fatalf("out %s", s)
	}
	if got != `propose_task {"title":"Review","item_ids":[3]}` {
		t.Fatalf("call %q", got)
	}
}
