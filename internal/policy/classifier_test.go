package policy

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
)

// fakeUpstream answers each request with the next reply: a decide input, "hang",
// "text" (no tool call) or "500".
type fakeUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	replies []string
	reqs    []map[string]any
}

func decideStream(id, toolName, input string) string {
	var b strings.Builder
	ev := func(t, d string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", t, d) }
	ev("message_start", `{"type":"message_start","message":{"id":"`+id+`","type":"message","role":"assistant","model":"deepseek-ai/deepseek-v4-flash","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	if toolName == "" {
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I think it is fine."}}`)
	} else {
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"`+toolName+`","input":{}}}`)
		j, _ := json.Marshal(input)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+string(j)+`}}`)
	}
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":900,"output_tokens":40}}`)
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

func newFakeUpstream(t *testing.T, replies ...string) *fakeUpstream {
	f := &fakeUpstream{replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		n := len(f.reqs)
		reply := "hang"
		if n <= len(f.replies) {
			reply = f.replies[n-1]
		}
		f.mu.Unlock()
		switch reply {
		case "hang":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		case "500":
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"boom"}}`, 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		id := fmt.Sprintf("msg_%d", n)
		if reply == "text" {
			io.WriteString(w, decideStream(id, "", ""))
			return
		}
		io.WriteString(w, decideStream(id, "decide", reply))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) classifier() *Classifier {
	return &Classifier{Upstream: messages.Upstream{BaseURL: f.srv.URL, Key: "k"}, Model: "deepseek-ai/deepseek-v4-flash",
		Timeout: 300 * time.Millisecond}
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

var sampleInput = Input{Tool: "Bash", ToolInput: json.RawMessage(`{"command":"git push origin main"}`), CWD: "/home/u/proj",
	Roots: []string{"/home/u/proj"}, LastUser: "push the branch when the tests pass", Recent: "[tool_use Bash] go test ./..."}

func TestClassifierDecisions(t *testing.T) {
	for _, d := range []Decision{Allow, Deny, Ask} {
		f := newFakeUpstream(t, `{"decision":"`+string(d)+`","reason":"  the user asked\nfor it  "}`)
		res := f.classifier().Decide(context.Background(), "s1", sampleInput)
		if res.Decision != d || res.Reason != "the user asked for it" || len(res.Calls) != 1 || res.Calls[0].MessageID != "msg_1" ||
			res.Calls[0].Usage.InputTokens != 900 || res.Calls[0].Model != "deepseek-ai/deepseek-v4-flash" {
			t.Errorf("%s: %+v", d, res)
		}
	}
	// The request forces the decide tool and carries the context.
	f := newFakeUpstream(t, `{"decision":"allow","reason":"ok"}`)
	f.classifier().Decide(context.Background(), "s1", sampleInput)
	req := f.reqs[0]
	tc, _ := json.Marshal(req["tool_choice"])
	if string(tc) != `{"name":"decide","type":"tool"}` || req["model"] != "deepseek-ai/deepseek-v4-flash" || req["stream"] != true {
		t.Errorf("request %v", req)
	}
	b, _ := json.Marshal(req["messages"])
	for _, want := range []string{"git push origin main", "/home/u/proj", "push the branch when the tests pass", "[tool_use Bash] go test"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("prompt lacks %q: %s", want, b)
		}
	}
	if !strings.Contains(req["system"].(string), "ask when you are unsure") {
		t.Error("system prompt")
	}
}

func TestClassifierFailuresAsk(t *testing.T) {
	cases := []struct {
		name    string
		replies []string
		reason  string
		calls   int // requests made
		usage   int // usage rows
	}{
		{"timeout", []string{"hang", "hang"}, "the classifier failed (timeout)", 2, 0},
		{"malformed json", []string{`{"decision":`, `{"decision":`}, "the classifier failed (malformed answer)", 2, 2},
		{"bad decision", []string{`{"decision":"maybe","reason":"x"}`, "text"}, "the classifier failed (malformed answer)", 2, 2},
		{"upstream 500", []string{"500", "500"}, "the classifier failed (upstream status 500)", 2, 0},
		{"retry succeeds", []string{"hang", `{"decision":"allow","reason":"fine"}`}, "fine", 2, 1},
	}
	for _, c := range cases {
		f := newFakeUpstream(t, c.replies...)
		res := f.classifier().Decide(context.Background(), "s1", sampleInput)
		want := Ask
		if c.name == "retry succeeds" {
			want = Allow
		}
		if res.Decision != want || res.Reason != c.reason || f.count() != c.calls || len(res.Calls) != c.usage {
			t.Errorf("%s: %+v after %d requests", c.name, res, f.count())
		}
	}
}

func TestRecentText(t *testing.T) {
	evs := [][]byte{
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"run the tests\nplease"}]}}`),
		[]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Running them."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + strings.Repeat("x", 1000) + `"}]}}`),
	}
	recent, last := RecentText(evs, 20)
	if last != "run the tests\nplease" {
		t.Errorf("last user %q", last)
	}
	lines := strings.Split(recent, "\n")
	if len(lines) != 4 || lines[0] != "[user] run the tests please" || !strings.HasPrefix(lines[2], "[tool_use Bash] go test") || len([]rune(lines[3])) > 330 {
		t.Errorf("recent:\n%s", recent)
	}
	if r, _ := RecentText(evs, 2); len(strings.Split(r, "\n")) != 2 {
		t.Errorf("n=2: %s", r)
	}
}
