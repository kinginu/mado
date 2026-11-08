package proto

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEncodeIsFlatAndTyped(t *testing.T) {
	b, err := Encode(&Event{SessionID: "s1", Seq: 7, Raw: json.RawMessage(`{"type":"assistant"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["t"] != "event" || m["session_id"] != "s1" || m["seq"].(float64) != 7 {
		t.Fatalf("bad frame: %s", b)
	}
	if _, ok := m["id"].(string); !ok {
		t.Fatalf("missing id: %s", b)
	}
	if _, err := time.Parse(time.RFC3339, m["ts"].(string)); err != nil {
		t.Fatalf("ts not rfc3339: %v", err)
	}
	if raw := m["raw"].(map[string]any); raw["type"] != "assistant" {
		t.Fatalf("raw not verbatim: %s", b)
	}
}

func TestRoundTripAllTypes(t *testing.T) {
	code := 3
	frames := []Frame{
		&Hello{Machine: "gpu1", Version: "v", ClaudeVersion: "2.1.302", Capabilities: []string{"router"}, Projects: []string{"/a"}},
		&SessionState{SessionID: "s", State: StateExited, ClaudeSessionID: "c", ExitCode: &code},
		&Event{SessionID: "s", Seq: 1, Raw: json.RawMessage(`{"a":1}`)},
		&ApprovalRequest{SessionID: "s", RequestID: "r", ToolName: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
		&Sessions{Sessions: []SessionInfo{{SessionID: "s", State: StateIdle}}},
		&Ack{Ref: "x", OK: false, Error: "nope"},
		&StartSession{SessionID: "s", CWD: "/a", MainModel: "oc-strong", Profile: Profile{Name: "p", Kind: KindRouter,
			Upstreams: map[string]Upstream{"oc-strong": {BaseURL: "http://h:1", Model: "m", Key: "k"}}}},
		&UserMessage{SessionID: "s", Text: "hi\nthere"},
		&Interrupt{SessionID: "s"},
		&ApprovalResponse{SessionID: "s", RequestID: "r", Decision: DecisionAllowSession},
		&StopSession{SessionID: "s"},
		&ListSessions{},
		&Resync{SessionID: "s", FromSeq: 42},
		&Sessions{Sessions: []SessionInfo{}, Spools: []string{"a"}},
		&GC{SessionIDs: []string{"a", "b"}},
		&HistorySession{Path: "/h/.claude/projects/-x/s.jsonl", ClaudeSessionID: "s", Offset: 0, End: 9,
			Lines: []string{"not json \u0000 <&>"}, LineOffsets: []int64{0}, Title: "t", TitleCustom: true},
		&HistoryOffsets{Files: []HistoryFileState{{Path: "/p", Offset: 5, Size: 6, MTime: 7}}, Partial: true},
		&HistoryRescan{},
		&StartSession{SessionID: "s", CWD: "/a", Profile: Profile{Name: "sub", Kind: KindSubscription}, Tools: []string{"web_search", "fetch_url"}},
		&ToolCall{SessionID: "s", CallID: "c1", Name: "web_search", Args: json.RawMessage(`{"query":"otters"}`)},
		&ToolResult{SessionID: "s", CallID: "c1", Content: "1. Sea otter\n   https://x", IsError: true},
	}
	for _, f := range frames {
		b, err := Encode(f)
		if err != nil {
			t.Fatal(err)
		}
		g, err := Decode(b)
		if err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		b2, _ := json.Marshal(g)
		if !bytes.Equal(b, b2) {
			t.Fatalf("round trip differs:\n%s\n%s", b, b2)
		}
		if HeaderOf(g).T == "" {
			t.Fatal("empty t")
		}
	}
}

// An old peer decodes the M8 frames as unknown (it logs and skips them), and an old
// start_session without tools offers none.
func TestToolFramesWire(t *testing.T) {
	b, _ := Encode(&ToolCall{SessionID: "s", CallID: "c", Name: "fetch_url", Args: json.RawMessage(`{"url":"https://x"}`)})
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["t"] != "tool_call" || m["call_id"] != "c" || m["name"] != "fetch_url" || m["args"].(map[string]any)["url"] != "https://x" {
		t.Fatalf("tool_call %s", b)
	}
	b, _ = Encode(&ToolResult{SessionID: "s", CallID: "c", Content: "ok"})
	if !bytes.Contains(b, []byte(`"t":"tool_result"`)) || bytes.Contains(b, []byte("is_error")) {
		t.Fatalf("tool_result %s", b)
	}
	f, err := Decode([]byte(`{"t":"start_session","id":"1","session_id":"s","cwd":"/a","profile":{"name":"p","kind":"subscription"}}`))
	if err != nil || len(f.(*StartSession).Tools) != 0 {
		t.Fatalf("old start_session: %v %+v", err, f)
	}
}

func TestDecodeRejectsUnknown(t *testing.T) {
	if _, err := Decode([]byte(`{"t":"rm_rf","id":"1"}`)); err == nil {
		t.Fatal("want error")
	}
	if _, err := Decode([]byte(`not json`)); err == nil {
		t.Fatal("want error")
	}
}

func TestRedactedDropsKeys(t *testing.T) {
	p := Profile{Upstreams: map[string]Upstream{"a": {Key: "secret"}}}
	r := p.Redacted()
	if r.Upstreams["a"].Key != "" || p.Upstreams["a"].Key != "secret" {
		t.Fatal("redaction wrong or mutated original")
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "secret") {
		t.Fatal("key leaked")
	}
}

func TestLineConn(t *testing.T) {
	var buf bytes.Buffer
	c := NewLineConn(&buf)
	if err := c.Write(Local{T: LEvent, Seq: 3, Raw: json.RawMessage(`{"x":"a\nb"}`)}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("not one line: %q", buf.String())
	}
	var l Local
	if err := c.Read(&l); err != nil || l.Seq != 3 || l.T != LEvent {
		t.Fatalf("%v %+v", err, l)
	}
}
