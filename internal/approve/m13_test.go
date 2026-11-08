package approve

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/kinginu/mado/internal/proto"
)

// resultOf decodes the one text block of a toolResult.
func resultOf(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	text := v["content"].([]any)[0].(map[string]any)["text"].(string)
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("not JSON: %s", text)
	}
	return m
}

const askInput = `{"questions":[{"question":"Which one?","header":"Pick","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}],"multiSelect":false}]}`

func TestToolResultUpdatedInput(t *testing.T) {
	r := Request{ToolName: "AskUserQuestion", Input: json.RawMessage(askInput)}
	// Without updated_input the original input goes back (unchanged behaviour).
	m := resultOf(t, toolResult(r, Decision{Allow: true}))
	in := m["updatedInput"].(map[string]any)
	if m["behavior"] != "allow" || in["answers"] != nil || len(in["questions"].([]any)) != 1 {
		t.Fatalf("plain allow: %v", m)
	}
	// With it, claude gets the answers.
	upd := `{"questions":[],"answers":{"Which one?":"B"},"annotations":{"Which one?":{"notes":"why not"}}}`
	m = resultOf(t, toolResult(r, Decision{Allow: true, UpdatedInput: json.RawMessage(upd)}))
	in = m["updatedInput"].(map[string]any)
	if in["answers"].(map[string]any)["Which one?"] != "B" || in["annotations"].(map[string]any)["Which one?"].(map[string]any)["notes"] != "why not" {
		t.Fatalf("answered: %v", m)
	}
	// Invalid JSON is ignored rather than sent to claude.
	m = resultOf(t, toolResult(r, Decision{Allow: true, UpdatedInput: json.RawMessage(`{"x":`)}))
	if m["updatedInput"].(map[string]any)["questions"] == nil {
		t.Fatalf("bad updated input used: %v", m)
	}
	// A deny never carries an input.
	m = resultOf(t, toolResult(r, Decision{Allow: false, Message: "The user dismissed the question in mado.", UpdatedInput: json.RawMessage(upd)}))
	if m["behavior"] != "deny" || m["updatedInput"] != nil || m["message"] != "The user dismissed the question in mado." {
		t.Fatalf("deny: %v", m)
	}
}

// The shim's answer reaches the decider with its updated_input.
func TestSocketDeciderUpdatedInput(t *testing.T) {
	dir, err := os.MkdirTemp("", "mado-ap")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	upd := json.RawMessage(`{"questions":[],"answers":{"Which one?":"A, B"}}`)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		lc := proto.NewLineConn(c)
		for {
			var m proto.Local
			if lc.Read(&m) != nil {
				return
			}
			if m.T == proto.LApprovalRequest {
				_ = lc.Write(proto.Local{T: proto.LApprovalResponse, RequestID: m.RequestID, Decision: proto.DecisionAllow, UpdatedInput: upd})
			}
		}
	}()
	d := SocketDecider(sock)(context.Background(), Request{ToolName: "AskUserQuestion", Input: json.RawMessage(askInput)})
	if !d.Allow || string(d.UpdatedInput) != string(upd) {
		t.Fatalf("decision %+v", d)
	}
}
