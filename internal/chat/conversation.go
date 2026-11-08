package chat

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/kinginu/mado/internal/files"
	"github.com/kinginu/mado/internal/messages"
)

// turnGroup is one user turn: the user's message and everything up to the next one
// (assistant answers, tool results). Trimming drops whole groups.
type turnGroup struct {
	startSeq int64
	msgs     []messages.Message
	tokens   int // estimate
}

type conversation struct {
	groups  []turnGroup
	cutoff  int64 // events below this seq are out of the context (mado_trimmed)
	lastSeq int64
	// lastKnown is input+output tokens of the latest response, and afterKnown the
	// estimate for what came after it; 0 when no response carried usage.
	lastKnown, afterKnown int
}

type storedEnvelope struct {
	Type      string         `json:"type"`
	Subtype   string         `json:"subtype"`
	BeforeSeq int64          `json:"before_seq"`
	Message   *storedMessage `json:"message"`
	Parent    *string        `json:"parent_tool_use_id"`
}

type storedMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Usage   *messages.Usage `json:"usage"`
}

type storedBlock struct {
	messages.Block
	FileID string `json:"file_id"`
	Mime   string `json:"mime"`
	Origin string `json:"origin"`
}

// conversation turns the stored events into request messages, expanding attachments.
func (r *Runner) conversation(ctx context.Context, t *Turn, evs []StoredEvent) *conversation {
	m := t.Model
	c := &conversation{}
	var envs []storedEnvelope
	seqs := []int64{}
	for _, e := range evs {
		c.lastSeq = e.Seq
		var env storedEnvelope
		if json.Unmarshal(e.Raw, &env) != nil {
			continue
		}
		if env.Type == "system" && env.Subtype == "mado_trimmed" && env.BeforeSeq > c.cutoff {
			c.cutoff = env.BeforeSeq
		}
		envs = append(envs, env)
		seqs = append(seqs, e.Seq)
	}
	for i, env := range envs {
		seq := seqs[i]
		if seq < c.cutoff || env.Message == nil || (env.Parent != nil && *env.Parent != "") {
			continue
		}
		if env.Type != "user" && env.Type != "assistant" {
			continue
		}
		var blocks []storedBlock
		if json.Unmarshal(env.Message.Content, &blocks) != nil {
			var s string
			if json.Unmarshal(env.Message.Content, &s) != nil || s == "" {
				continue
			}
			blocks = []storedBlock{{Block: messages.Block{Type: "text", Text: s}}}
		}
		msg := messages.Message{Role: env.Type}
		est := 0
		startsTurn := false
		for _, b := range blocks {
			switch b.Type {
			case "mado_file":
				if b.Origin == OriginGenerated {
					continue // shown to the user; the model got a tool_result
				}
				startsTurn = true
				at, err := r.Host.Attachment(t.Owner, t.SessionID, b.FileID)
				if err != nil {
					msg.Content = append(msg.Content, messages.Block{Type: "text", Text: "[file " + b.Name + " is no longer available]"})
					continue
				}
				bs, n, err := r.expand(ctx, m, at)
				if err != nil {
					msg.Content = append(msg.Content, messages.Block{Type: "text", Text: "[file " + b.Name + ": " + err.Error() + "]"})
					continue
				}
				msg.Content = append(msg.Content, bs...)
				est += n
			case "text":
				if b.Text == "" {
					continue
				}
				if env.Type == "user" {
					startsTurn = true
				}
				msg.Content = append(msg.Content, b.Block)
				est += len(b.Text) / 3
			case "tool_use":
				blk := b.Block
				if len(blk.Input) == 0 {
					blk.Input = json.RawMessage("{}")
				}
				msg.Content = append(msg.Content, blk)
				est += len(blk.Input) / 3
			case "tool_result":
				msg.Content = append(msg.Content, b.Block)
				est += len(b.Content) / 3
			case "image":
				msg.Content = append(msg.Content, b.Block)
				est += imageTokens
				// thinking and redacted_thinking are dropped on replay
			}
		}
		if env.Type == "assistant" && env.Message.Usage != nil {
			u := env.Message.Usage
			if k := int(u.TotalInput() + u.OutputTokens); k > 0 {
				c.lastKnown, c.afterKnown = k, 0
			}
		} else if c.lastKnown > 0 {
			c.afterKnown += est
		}
		if len(msg.Content) == 0 {
			continue
		}
		if startsTurn || len(c.groups) == 0 {
			c.groups = append(c.groups, turnGroup{startSeq: seq})
		}
		g := &c.groups[len(c.groups)-1]
		g.msgs = append(g.msgs, msg)
		g.tokens += est
	}
	return c
}

// estimate is the expected prompt size of the next call.
func (c *conversation) estimate() int {
	if c.lastKnown > 0 {
		return c.lastKnown + c.afterKnown
	}
	n := 0
	for _, g := range c.groups {
		n += g.tokens
	}
	return n
}

// trimToFit drops the oldest turns until the estimate fits the model's window minus
// its output budget and a margin (max_tokens itself is never reduced). The current
// turn always stays. It returns the number of dropped turns.
func (c *conversation) trimToFit(m Model) int {
	budget := m.ContextWindow - m.MaxOutput - trimMargin
	n := 0
	for c.estimate() > budget && len(c.groups) > 1 {
		c.dropOne()
		n++
	}
	return n
}

// drop removes up to n more of the oldest turns, keeping the current one.
func (c *conversation) drop(n int) int {
	d := 0
	for d < n && len(c.groups) > 1 {
		c.dropOne()
		d++
	}
	return d
}

func (c *conversation) dropOne() {
	g := c.groups[0]
	c.groups = c.groups[1:]
	if c.lastKnown > 0 {
		c.lastKnown = max(0, c.lastKnown-g.tokens)
	}
	c.cutoff = c.groups[0].startSeq
}

// messages flattens the groups, merging consecutive messages of one role (a failed
// turn leaves two user messages in a row), and drops tool calls without results and
// results without calls (an interrupted stream, a trimmed turn), which the API
// refuses.
func (c *conversation) messages() []messages.Message {
	var flat []messages.Message
	for _, g := range c.groups {
		flat = append(flat, g.msgs...)
	}
	out := mergeRoles(flat)
	answered := map[string]bool{}
	for i := 0; i+1 < len(out); i++ {
		if out[i].Role != "assistant" || out[i+1].Role != "user" {
			continue
		}
		uses := map[string]bool{}
		for _, b := range out[i].Content {
			if b.Type == "tool_use" {
				uses[b.ID] = true
			}
		}
		for _, b := range out[i+1].Content {
			if b.Type == "tool_result" && uses[b.ToolUseID] {
				answered[b.ToolUseID] = true
			}
		}
	}
	var kept []messages.Message
	for _, m := range out {
		var content []messages.Block
		for _, b := range m.Content {
			if (b.Type == "tool_use" && !answered[b.ID]) || (b.Type == "tool_result" && !answered[b.ToolUseID]) {
				continue
			}
			content = append(content, b)
		}
		if len(content) > 0 {
			kept = append(kept, messages.Message{Role: m.Role, Content: content})
		}
	}
	out = mergeRoles(kept)
	// The API wants the conversation to start with the user.
	for len(out) > 0 && out[0].Role != "user" {
		out = out[1:]
	}
	return out
}

func mergeRoles(in []messages.Message) []messages.Message {
	var out []messages.Message
	for _, m := range in {
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			out[n-1].Content = append(out[n-1].Content, m.Content...)
			continue
		}
		out = append(out, messages.Message{Role: m.Role, Content: append([]messages.Block(nil), m.Content...)})
	}
	return out
}

// expand turns an attachment into request blocks and estimates their tokens.
func (r *Runner) expand(ctx context.Context, m Model, at *Attachment) ([]messages.Block, int, error) {
	data, err := os.ReadFile(at.Path)
	if err != nil {
		return nil, 0, UserError("file " + at.Name + " is no longer available")
	}
	switch files.ClassOf(at.Mime) {
	case files.ClassImage:
		if !m.Vision {
			return nil, 0, UserError("this model cannot see images")
		}
		mt, _, _ := strings.Cut(at.Mime, ";")
		return []messages.Block{{Type: "image", Source: &messages.Source{Type: "base64", MediaType: mt,
			Data: base64.StdEncoding.EncodeToString(data)}}}, imageTokens, nil
	case files.ClassPDF:
		if m.NativeDocuments() {
			return []messages.Block{{Type: "document", Title: at.Name, Source: &messages.Source{Type: "base64",
				MediaType: "application/pdf", Data: base64.StdEncoding.EncodeToString(data)}}}, documentTokens, nil
		}
		text, err := r.pdf.text(ctx, at)
		if err != nil {
			return nil, 0, UserError(at.Name + ": " + err.Error())
		}
		return docBlock(at.Name, text), len(text) / 3, nil
	}
	return docBlock(at.Name, string(data)), len(data) / 3, nil
}

func docBlock(name, text string) []messages.Block {
	name = strings.NewReplacer(`"`, "'", "\n", " ", "\r", " ", "<", "", ">", "").Replace(name)
	return []messages.Block{{Type: "text", Text: fmt.Sprintf("<document name=\"%s\">\n%s\n</document>", name, text)}}
}

// pdfCache keeps extracted PDF text by sha256: a document is re-sent on every turn.
type pdfCache struct {
	mu sync.Mutex
	m  map[string]string
}

const pdfCacheEntries = 32

func (p *pdfCache) text(ctx context.Context, at *Attachment) (string, error) {
	p.mu.Lock()
	t, ok := p.m[at.SHA256]
	p.mu.Unlock()
	if ok && at.SHA256 != "" {
		return t, nil
	}
	t, err := files.PDFText(ctx, at.Path)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	if p.m == nil || len(p.m) >= pdfCacheEntries {
		p.m = map[string]string{}
	}
	p.m[at.SHA256] = t
	p.mu.Unlock()
	return t, nil
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
