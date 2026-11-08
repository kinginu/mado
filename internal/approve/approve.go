// Package approve is the stdio MCP server that Claude Code calls as
// --permission-prompt-tool mcp__mado__approve. It is hand-rolled JSON-RPC 2.0
// over newline-delimited stdio. Besides approve it can offer the hub's web tools
// (web_search, fetch_url, DESIGN §17.12), whose calls go to the hub through the
// shim and the worker daemon, and, in a job session, the digest's propose_task
// (DESIGN §23.6), relayed the same way.
//
// Contract (docs/claude-code-facts.md §3): tools/call arguments are
// {"tool_name":..., "input":{...}, "tool_use_id":...}; the result is one text
// content block holding {"behavior":"allow","updatedInput":{...}} or
// {"behavior":"deny","message":"..."}.
package approve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/tools"
)

const (
	ServerName = "mado"
	ToolName   = "approve"
	// PromptTool is the value for --permission-prompt-tool.
	PromptTool = "mcp__" + ServerName + "__" + ToolName
)

// JobTools are the hub tools only job sessions get (DESIGN §23.6): the digest's
// propose_task. The worker passes them only for a start_session with a job.
var JobTools = []string{inbox.ToolProposeTask}

// HubToolDef is the definition the MCP server lists for a hub tool, and whether the
// name is one it may offer (a code tool or a job tool).
func HubToolDef(name string) (messages.Tool, bool) {
	switch {
	case slices.Contains(tools.CodeTools, name):
		return tools.Definition(name, nil), true
	case name == inbox.ToolProposeTask:
		return inbox.ProposeTaskDef(), true
	}
	return messages.Tool{}, false
}

// MCPToolName is the name claude knows one of this server's tools by.
func MCPToolName(tool string) string { return "mcp__" + ServerName + "__" + tool }

// Request is what Claude Code sends us.
type Request struct {
	ToolName  string          `json:"tool_name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

// Decision is the answer from the human (via shim, daemon and hub).
type Decision struct {
	Allow   bool
	Message string
	// UpdatedInput, when set on allow, is the input claude runs the tool with
	// instead of the original (the answers to an AskUserQuestion, DESIGN §21.1).
	UpdatedInput json.RawMessage
}

// Decider asks for a decision. It may block for a long time.
type Decider func(ctx context.Context, r Request) Decision

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var toolSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"tool_name":   map[string]any{"type": "string"},
		"input":       map[string]any{"type": "object"},
		"tool_use_id": map[string]any{"type": "string"},
	},
	"required": []string{"tool_name", "input"},
}

// ToolCaller runs one of the hub's tools and returns the text for claude.
type ToolCaller func(ctx context.Context, name string, args json.RawMessage) (text string, isError bool)

// Options configure the server: the approval decider, and the hub tools to offer
// with the function that runs them.
type Options struct {
	Decide Decider
	Tools  []string
	Call   ToolCaller
}

// Serve runs the MCP server with the approve tool only until in reaches EOF.
func Serve(ctx context.Context, in io.Reader, out io.Writer, decide Decider) error {
	return ServeWith(ctx, in, out, Options{Decide: decide})
}

// ServeWith runs the MCP server until in reaches EOF.
func ServeWith(ctx context.Context, in io.Reader, out io.Writer, o Options) error {
	decide := o.Decide
	extra := map[string]bool{}
	list := []any{map[string]any{
		"name":        ToolName,
		"description": "mado permission prompt: forwards the request to the user's browser.",
		"inputSchema": toolSchema,
	}}
	if o.Call != nil {
		for _, n := range o.Tools {
			d, ok := HubToolDef(n)
			if !ok || extra[n] {
				continue
			}
			extra[n] = true
			list = append(list, map[string]any{"name": d.Name, "description": d.Description, "inputSchema": d.InputSchema})
		}
	}
	conn := proto.NewLineConn(struct {
		io.Reader
		io.Writer
	}{in, out})
	var wg sync.WaitGroup
	defer wg.Wait()
	reply := func(id json.RawMessage, result any, e *rpcError) {
		_ = conn.Write(rpcMsg{JSONRPC: "2.0", ID: id, Result: result, Error: e})
	}
	for {
		var m rpcMsg
		if err := conn.Read(&m); err != nil {
			if err == io.EOF {
				return nil
			}
			if _, ok := err.(*json.SyntaxError); ok {
				reply(json.RawMessage("null"), nil, &rpcError{-32700, "parse error"})
				continue
			}
			return err
		}
		if len(m.ID) == 0 { // notification
			continue
		}
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			reply(m.ID, map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": ServerName, "version": "1"},
			}, nil)
		case "ping":
			reply(m.ID, map[string]any{}, nil)
		case "tools/list":
			reply(m.ID, map[string]any{"tools": list}, nil)
		case "tools/call":
			var tc struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(m.Params, &tc); err == nil && extra[tc.Name] {
				wg.Add(1)
				go func(id json.RawMessage, name string, args json.RawMessage) {
					defer wg.Done()
					text, isErr := o.Call(ctx, name, args)
					reply(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr}, nil)
				}(m.ID, tc.Name, tc.Arguments)
				continue
			}
			var p struct {
				Name      string  `json:"name"`
				Arguments Request `json:"arguments"`
			}
			if err := json.Unmarshal(m.Params, &p); err != nil || p.Name != ToolName {
				reply(m.ID, nil, &rpcError{-32602, "unknown tool or bad arguments"})
				continue
			}
			wg.Add(1)
			go func(id json.RawMessage, r Request) {
				defer wg.Done()
				d := decide(ctx, r)
				reply(id, toolResult(r, d), nil)
			}(m.ID, p.Arguments)
		default:
			reply(m.ID, nil, &rpcError{-32601, "method not found: " + m.Method})
		}
	}
}

func toolResult(r Request, d Decision) map[string]any {
	var v map[string]any
	if d.Allow {
		input := r.Input
		if len(d.UpdatedInput) > 0 && json.Valid(d.UpdatedInput) {
			input = d.UpdatedInput
		}
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		v = map[string]any{"behavior": "allow", "updatedInput": input}
	} else {
		msg := d.Message
		if msg == "" {
			msg = "Denied by the user in mado."
		}
		v = map[string]any{"behavior": "deny", "message": msg}
	}
	b, _ := json.Marshal(v)
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}
}

// SocketDecider asks the shim over its unix socket.
func SocketDecider(sockPath string) Decider {
	return func(ctx context.Context, r Request) Decision {
		d, err := askShim(ctx, sockPath, r)
		if err != nil {
			return Decision{Allow: false, Message: "mado: approval channel failed: " + err.Error()}
		}
		return d
	}
}

func askShim(ctx context.Context, sockPath string, r Request) (Decision, error) {
	var dl net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	c, err := dl.DialContext(dctx, "unix", sockPath)
	cancel()
	if err != nil {
		return Decision{}, err
	}
	defer c.Close()
	go func() { <-ctx.Done(); c.Close() }()
	lc := proto.NewLineConn(c)
	if err := lc.Write(proto.Local{T: proto.LHello, Role: "approver"}); err != nil {
		return Decision{}, err
	}
	reqID := proto.NewID()
	if err := lc.Write(proto.Local{T: proto.LApprovalRequest, RequestID: reqID, ToolName: r.ToolName,
		Input: r.Input, ToolUseID: r.ToolUseID}); err != nil {
		return Decision{}, err
	}
	for {
		var resp proto.Local
		if err := lc.Read(&resp); err != nil {
			return Decision{}, err
		}
		if resp.T != proto.LApprovalResponse || resp.RequestID != reqID {
			continue
		}
		switch resp.Decision {
		case proto.DecisionAllow, proto.DecisionAllowSession:
			return Decision{Allow: true, UpdatedInput: resp.UpdatedInput}, nil
		case proto.DecisionDeny:
			return Decision{Allow: false, Message: resp.Message}, nil
		default:
			return Decision{}, fmt.Errorf("bad decision %q", resp.Decision)
		}
	}
}

// ToolTimeout is how long the MCP server waits for the hub's answer to a tool call.
const ToolTimeout = 30 * time.Second

// SocketToolCaller relays tool calls to the hub through the shim's socket. Every
// failure, including no answer within timeout, is an error result for claude.
func SocketToolCaller(sockPath string, timeout time.Duration) ToolCaller {
	return func(ctx context.Context, name string, args json.RawMessage) (string, bool) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		text, isErr, err := callShim(ctx, sockPath, name, args)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Sprintf("mado: the hub did not answer within %s", timeout), true
			}
			return "mado: tool channel failed: " + err.Error(), true
		}
		return text, isErr
	}
}

func callShim(ctx context.Context, sockPath, name string, args json.RawMessage) (string, bool, error) {
	var dl net.Dialer
	c, err := dl.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return "", false, err
	}
	defer c.Close()
	go func() { <-ctx.Done(); c.Close() }()
	lc := proto.NewLineConn(c)
	if len(args) == 0 || !json.Valid(args) {
		args = json.RawMessage("{}")
	}
	callID := proto.NewID()
	if err := lc.Write(proto.Local{T: proto.LHello, Role: "tool"}); err != nil {
		return "", false, err
	}
	if err := lc.Write(proto.Local{T: proto.LToolCall, CallID: callID, ToolName: name, Input: args}); err != nil {
		return "", false, err
	}
	for {
		var resp proto.Local
		if err := lc.Read(&resp); err != nil {
			if ctx.Err() != nil {
				return "", false, ctx.Err()
			}
			return "", false, err
		}
		if resp.T == proto.LToolResult && resp.CallID == callID {
			return resp.Text, resp.IsError, nil
		}
	}
}

// MCPConfig returns the inline --mcp-config JSON for a stdio server running argv.
func MCPConfig(argv []string) string {
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		ServerName: map[string]any{"type": "stdio", "command": argv[0], "args": argv[1:]},
	}})
	return string(b)
}
