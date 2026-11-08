package oai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Emit receives one Anthropic SSE event: its type and its JSON data.
type Emit func(event string, data []byte) error

// WriteSSE writes one event in the Anthropic wire format.
func WriteSSE(w io.Writer, event string, data []byte) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

// chat-completions chunk, as vLLM, Ollama, llama.cpp and LM Studio send it.
type oChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Model   string          `json:"model"`
	Choices []oChoice       `json:"choices"`
	Usage   *oUsage         `json:"usage"`
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"` // {"object":"error","message":...}
}

type oChoice struct {
	Index        int     `json:"index"`
	Delta        *oDelta `json:"delta"`   // stream
	Message      *oDelta `json:"message"` // non-stream
	FinishReason *string `json:"finish_reason"`
}

type oDelta struct {
	Content          *string        `json:"content"`
	ReasoningContent *string        `json:"reasoning_content"` // DeepSeek, vLLM
	Reasoning        *string        `json:"reasoning"`         // Ollama, newer vLLM
	ToolCalls        []oToolCallDel `json:"tool_calls"`
}

type oToolCallDel struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"` // a string; some servers send an object
	} `json:"function"`
}

func (t oToolCallDel) args() string {
	a := t.Function.Arguments
	if len(a) == 0 || string(a) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(a, &s) == nil {
		return s
	}
	return string(a)
}

type oUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// anthropicUsage maps chat-completions usage: cached tokens are cache reads, the
// uncached input is prompt minus cached.
func (u *oUsage) anthropic() map[string]int64 {
	if u == nil {
		return map[string]int64{"input_tokens": 0, "output_tokens": 0}
	}
	var cached int64
	if u.PromptTokensDetails != nil {
		cached = min(u.PromptTokensDetails.CachedTokens, u.PromptTokens)
	}
	return map[string]int64{"input_tokens": u.PromptTokens - cached, "cache_read_input_tokens": cached,
		"output_tokens": u.CompletionTokens}
}

// StopReason maps a finish_reason.
func StopReason(finish string, toolUse bool) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	}
	if toolUse { // some servers say "stop" after a tool call
		return "tool_use"
	}
	return "end_turn"
}

// MessageID is the server's id or a synthesised msg_ one.
func MessageID(id string) string {
	if id != "" {
		return id
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "msg_" + hex.EncodeToString(b[:])
}

type toolState struct {
	id, name string
	block    int // Anthropic block index; -1 until opened
	pending  strings.Builder
}

type translator struct {
	emit    Emit
	model   string
	started bool
	next    int    // next Anthropic block index
	cur     string // "", text, thinking, tool
	curTool *toolState
	tools   map[int]*toolState
	ids     map[string]bool
	blocks  int // blocks emitted
	toolUse bool
	finish  string
	usage   *oUsage
}

func (t *translator) send(event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return t.emit(event, b)
}

func (t *translator) start(c *oChunk) error {
	if t.started {
		return nil
	}
	t.started = true
	model := t.model
	if model == "" && c != nil {
		model = c.Model
	}
	id := ""
	if c != nil {
		id = c.ID
	}
	return t.send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": MessageID(id), "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int64{"input_tokens": 0, "output_tokens": 0}}})
}

func (t *translator) closeCur() error {
	if t.cur == "" {
		return nil
	}
	idx := t.next - 1
	t.cur, t.curTool = "", nil
	return t.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

func (t *translator) open(kind string, block map[string]any) error {
	if err := t.closeCur(); err != nil {
		return err
	}
	t.cur = kind
	t.blocks++
	idx := t.next
	t.next++
	return t.send("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": block})
}

func (t *translator) delta(d map[string]any) error {
	return t.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": t.next - 1, "delta": d})
}

func (t *translator) text(kind, s string) error {
	if s == "" {
		return nil
	}
	if t.cur != kind {
		var b map[string]any
		if kind == "thinking" {
			b = map[string]any{"type": "thinking", "thinking": ""}
		} else {
			b = map[string]any{"type": "text", "text": ""}
		}
		if err := t.open(kind, b); err != nil {
			return err
		}
	}
	if kind == "thinking" {
		return t.delta(map[string]any{"type": "thinking_delta", "thinking": s})
	}
	return t.delta(map[string]any{"type": "text_delta", "text": s})
}

// errOutOfOrder: a tool-call fragment for a block that was already closed.
var errOutOfOrder = errors.New("tool call fragment out of order")

func (t *translator) toolCall(tc oToolCallDel, pos int) error {
	key := pos
	if tc.Index != nil {
		key = *tc.Index
	}
	st := t.tools[key]
	if st == nil {
		st = &toolState{block: -1}
		t.tools[key] = st
	}
	if st.id == "" && tc.ID != "" {
		st.id = tc.ID
	}
	if st.name == "" && tc.Function.Name != "" {
		st.name = tc.Function.Name
	}
	args := tc.args()
	if st.block < 0 {
		st.pending.WriteString(args)
		if st.name == "" {
			return nil // wait for the name before opening the block
		}
		return t.openTool(st)
	}
	if t.curTool != st {
		if args == "" {
			return nil
		}
		return errOutOfOrder
	}
	if args == "" {
		return nil
	}
	return t.delta(map[string]any{"type": "input_json_delta", "partial_json": args})
}

func (t *translator) openTool(st *toolState) error {
	id := st.id
	if id == "" || t.ids[id] {
		for n := len(t.ids); ; n++ {
			id = fmt.Sprintf("call_%d", n)
			if !t.ids[id] {
				break
			}
		}
	}
	t.ids[id] = true
	st.id = id
	name := st.name
	if name == "" {
		name = "unknown"
	}
	if err := t.open("tool", map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}); err != nil {
		return err
	}
	st.block = t.next - 1
	t.curTool = st
	t.toolUse = true
	if p := st.pending.String(); p != "" {
		st.pending.Reset()
		return t.delta(map[string]any{"type": "input_json_delta", "partial_json": p})
	}
	return nil
}

// chunk handles one parsed chunk. done is set by finish handling elsewhere.
func (t *translator) chunk(c *oChunk) error {
	if err := t.start(c); err != nil {
		return err
	}
	if c.Usage != nil {
		t.usage = c.Usage
	}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue
		}
		d := ch.Delta
		if d == nil {
			d = ch.Message
		}
		if d != nil {
			r := d.ReasoningContent
			if r == nil || *r == "" {
				r = d.Reasoning
			}
			if r != nil {
				if err := t.text("thinking", *r); err != nil {
					return err
				}
			}
			if d.Content != nil {
				if err := t.text("text", *d.Content); err != nil {
					return err
				}
			}
			for i, tc := range d.ToolCalls {
				if err := t.toolCall(tc, i); err != nil {
					return err
				}
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			t.finish = *ch.FinishReason
		}
	}
	return nil
}

// end closes the message. A normal end (finish_reason or [DONE]) or an abnormal one
// after content gives message_delta + message_stop; an abnormal end before any block
// gives an error event.
func (t *translator) end(normal bool, errType, errMsg string) error {
	if !normal && t.blocks == 0 {
		if errType == "" {
			errType = "api_error"
		}
		if errMsg == "" {
			errMsg = "upstream stream ended before any content"
		}
		return t.send("error", map[string]any{"type": "error", "error": map[string]string{"type": errType, "message": errMsg}})
	}
	// tool calls still waiting for a name
	for _, st := range t.tools {
		if st.block < 0 {
			if err := t.openTool(st); err != nil {
				return err
			}
		}
	}
	if err := t.start(nil); err != nil {
		return err
	}
	if err := t.closeCur(); err != nil {
		return err
	}
	sr := "end_turn"
	if normal {
		sr = StopReason(t.finish, t.toolUse)
	}
	if err := t.send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": sr, "stop_sequence": nil}, "usage": t.usage.anthropic()}); err != nil {
		return err
	}
	return t.send("message_stop", map[string]any{"type": "message_stop"})
}

// Stream reads a chat-completions SSE body and emits the Anthropic event sequence
// (DESIGN §22.2). model names the message (the configured upstream model, which the
// hub's usage and prices key on); "" takes the server's. It returns an error only when
// emit fails or ctx ends; upstream trouble becomes events.
func Stream(ctx context.Context, r io.Reader, model string, emit Emit) error {
	t := &translator{emit: emit, model: model, tools: map[int]*toolState{}, ids: map[string]bool{}}
	br := bufio.NewReaderSize(r, 64<<10)
	evType := ""
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 16<<20 {
			return t.end(false, "", "upstream SSE line too long")
		}
		line = bytes.TrimRight(line, "\r\n")
		var data []byte
		isErr := false
		switch {
		case len(line) == 0:
			evType = ""
		case line[0] == ':':
		case bytes.HasPrefix(line, []byte("event:")):
			evType = string(bytes.TrimSpace(line[6:]))
		case bytes.HasPrefix(line, []byte("data:")):
			data = bytes.TrimSpace(line[5:])
			isErr = evType == "error"
		case bytes.HasPrefix(line, []byte("error:")): // llama.cpp
			data, isErr = bytes.TrimSpace(line[6:]), true
		}
		if len(data) > 0 {
			if string(data) == "[DONE]" {
				return t.end(true, "", "")
			}
			var c oChunk
			if json.Unmarshal(data, &c) != nil {
				return t.end(false, "", "malformed upstream chunk")
			}
			if isErr || len(c.Error) > 0 && string(c.Error) != "null" || c.Object == "error" {
				e := parseError(http.StatusInternalServerError, data)
				return t.end(false, e.Type, e.Message)
			}
			if err := t.chunk(&c); err != nil {
				if errors.Is(err, errOutOfOrder) {
					return t.end(false, "", "upstream tool call fragments out of order")
				}
				return err
			}
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// EOF without [DONE]: a finish_reason makes it a normal end
			if t.finish != "" {
				return t.end(true, "", "")
			}
			if rerr != io.EOF {
				return t.end(false, "", "upstream stream broken: "+rerr.Error())
			}
			return t.end(false, "", "upstream stream ended early")
		}
	}
}

// TranslateResponse turns a non-streaming chat.completion into an Anthropic message.
func TranslateResponse(body []byte, model string) ([]byte, error) {
	var c oChunk
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("upstream answer is not JSON")
	}
	if model == "" {
		model = c.Model
	}
	content := []map[string]any{}
	finish := ""
	toolUse := false
	ids := map[string]bool{}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue
		}
		if ch.FinishReason != nil {
			finish = *ch.FinishReason
		}
		m := ch.Message
		if m == nil {
			m = ch.Delta
		}
		if m == nil {
			continue
		}
		r := m.ReasoningContent
		if r == nil || *r == "" {
			r = m.Reasoning
		}
		if r != nil && *r != "" {
			content = append(content, map[string]any{"type": "thinking", "thinking": *r})
		}
		if m.Content != nil && *m.Content != "" {
			content = append(content, map[string]any{"type": "text", "text": *m.Content})
		}
		for _, tc := range m.ToolCalls {
			id := tc.ID
			if id == "" || ids[id] {
				for n := len(ids); ; n++ {
					if id = fmt.Sprintf("call_%d", n); !ids[id] {
						break
					}
				}
			}
			ids[id] = true
			var input json.RawMessage = []byte("{}")
			if a := tc.args(); a != "" && json.Valid([]byte(a)) {
				input = json.RawMessage(a)
			}
			content = append(content, map[string]any{"type": "tool_use", "id": id, "name": tc.Function.Name, "input": input})
			toolUse = true
		}
	}
	return json.Marshal(map[string]any{"id": MessageID(c.ID), "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": StopReason(finish, toolUse), "stop_sequence": nil, "usage": c.Usage.anthropic()})
}
