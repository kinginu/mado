// Package render turns Claude Code stream-json objects into readable blocks.
// It is shared by the shim (plain text on its tty) and the hub (HTML).
// Parsing is tolerant: anything unexpected becomes a KindRaw block.
package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kinginu/mado/internal/usage"
)

// Block kinds.
const (
	KindUser       = "user"
	KindAssistant  = "assistant"
	KindThinking   = "thinking"
	KindToolUse    = "tool_use"
	KindToolResult = "tool_result"
	KindSystem     = "system"
	KindResult     = "result"
	KindRaw        = "raw"
	// KindStatus is a one-line update about background work (system/task_*,
	// background_tasks_changed). The hub shows it inside the current tool chunk.
	KindStatus = "status"
	// KindFile is an attachment of a chat message (a mado_file content block).
	KindFile = "file"
	// KindThinkingTokens carries a system/thinking_tokens delta. It is never shown
	// as a block; the hub folds it into a per-turn counter.
	KindThinkingTokens = "thinking_tokens"
	// KindPlan is a concierge plan card (system/mado_plan, DESIGN §20.3); the hub
	// adds the plan's current state by PlanID.
	KindPlan = "plan"
	// KindNote is a message mado wrote into a chat for the model (a user event with
	// mado_note, e.g. the outcome of a plan); shown as a notice, not as the user's.
	KindNote = "note"
)

// Block is one readable unit.
type Block struct {
	Kind     string
	Label    string // short heading, e.g. tool name
	Text     string // main body, already abbreviated where needed
	Detail   string // longer content shown collapsed
	IsError  bool
	Subagent bool // from a subagent (parent_tool_use_id set)

	// Grouping hints for the hub (DESIGN §15).
	Parent string // parent_tool_use_id, or the tool_use_id a task status belongs to
	ToolID string // tool_use id (tool_use) or the id it answers (tool_result)
	Agent  string // Agent/Task tool_use: subagent_type
	Model  string // assistant blocks: message.model; Agent tool_use: the model it asked for
	Delta  int    // KindThinkingTokens: estimated_tokens_delta
	Noise  bool   // hidden unless the viewer asks (e.g. [claude-code:unrecognized_model])

	// KindFile: the hub file id and its mime type (DESIGN §17.4); Origin is
	// "generated" for an image a tool made (DESIGN §17.7).
	FileID string
	Mime   string
	Origin string

	PlanID string // KindPlan
}

// IsImage reports a file block the page can show inline.
func (b Block) IsImage() bool { return b.Kind == KindFile && strings.HasPrefix(b.Mime, "image/") }

const (
	maxInput  = 300
	maxResult = 600
	maxDetail = 8000
)

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	FileID    string          `json:"file_id"` // mado_file
	Mime      string          `json:"mime"`    // mado_file
	Origin    string          `json:"origin"`  // mado_file
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Model   string          `json:"model"`
}

type envelope struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	MessageRaw      json.RawMessage `json:"message"` // an object for user/assistant, a string for mado_error
	Message         *message        `json:"-"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	Model           string          `json:"model"`
	CWD             string          `json:"cwd"`
	SessionID       string          `json:"session_id"`
	PermissionMode  string          `json:"permissionMode"`
	Version         string          `json:"claude_code_version"`
	Result          string          `json:"result"`
	IsError         bool            `json:"is_error"`
	DurationMS      float64         `json:"duration_ms"`
	NumTurns        int             `json:"num_turns"`
	TotalCostUSD    float64         `json:"total_cost_usd"`
	MadoCurrency    string          `json:"mado_currency"` // chat results: the cost is in this currency
	Attempt         int             `json:"attempt"`
	MaxRetries      int             `json:"max_retries"`
	ErrorStatus     *int            `json:"error_status"`
	Error           json.RawMessage `json:"error"`
	OpenCC          string          `json:"mado"`
	// system/thinking_tokens and system/task_* fields
	EstimatedDelta int    `json:"estimated_tokens_delta"`
	Dropped        int    `json:"dropped"` // system/mado_trimmed
	Rounds         int    `json:"rounds"`  // system/mado_tool_limit
	PlanID         string `json:"plan_id"` // system/mado_plan
	Plan           *struct {
		Summary string `json:"summary"`
	} `json:"plan"`
	MadoNote     string          `json:"mado_note"` // a user event mado wrote for the model
	ToolUseID    string          `json:"tool_use_id"`
	Description  string          `json:"description"`
	SubagentType string          `json:"subagent_type"`
	Backgrounded bool            `json:"is_backgrounded"`
	Status       string          `json:"status"`
	Summary      string          `json:"summary"`
	Patch        json.RawMessage `json:"patch"`
	Tasks        []struct {
		SubagentType string `json:"subagent_type"`
		Description  string `json:"description"`
	} `json:"tasks"`
	TaskUsage *struct {
		TotalTokens int `json:"total_tokens"`
		ToolUses    int `json:"tool_uses"`
	} `json:"usage"`
}

// Blocks renders one stream-json object. It never fails: bad input yields a raw block.
// Events that carry no user-visible content (rate limit info, partial stream events)
// yield no blocks.
func Blocks(raw []byte) []Block {
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return []Block{rawBlock("unparsable", raw)}
	}
	if len(e.MessageRaw) > 0 && e.MessageRaw[0] == '{' {
		var m message
		if json.Unmarshal(e.MessageRaw, &m) == nil {
			e.Message = &m
		}
	}
	sub := e.ParentToolUseID != nil && *e.ParentToolUseID != ""
	parent := ""
	if sub {
		parent = *e.ParentToolUseID
	}
	model := ""
	var out []Block
	switch e.Type {
	case "system":
		out = systemBlocks(e, raw)
	case "assistant", "user":
		if e.Message == nil {
			return []Block{rawBlock(e.Type, raw)}
		}
		out = messageBlocks(e.Type, e.Message.Content, raw)
		if e.Type == "user" && e.MadoNote != "" {
			for i := range out {
				if out[i].Kind == KindUser {
					out[i].Kind, out[i].Label = KindNote, e.MadoNote
				}
			}
		}
		if e.Type == "assistant" {
			model = e.Message.Model
		}
	case "result":
		out = []Block{resultBlock(e)}
	case "rate_limit_event", "stream_event", "keep_alive", "mado_spawn":
		return nil
	case "mado_stderr", "mado_stdout":
		var l struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &l)
		// The user lives with this per-request diagnostic for router names (§13).
		return []Block{{Kind: KindSystem, Label: "stderr", Text: l.Text, Noise: strings.Contains(l.Text, "[claude-code:unrecognized_model]")}}
	case "mado_error":
		var l struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &l)
		return []Block{{Kind: KindSystem, Label: "error", Text: l.Message, IsError: true}}
	default:
		return []Block{rawBlock(e.Type, raw)}
	}
	for i := range out {
		out[i].Subagent = sub
		if sub {
			out[i].Parent = parent
		}
		if model != "" && out[i].Model == "" {
			out[i].Model = model
		}
	}
	return out
}

func systemBlocks(e envelope, raw []byte) []Block {
	switch e.Subtype {
	case "init":
		parts := []string{"session started"}
		if e.Model != "" {
			parts = append(parts, "model "+e.Model)
		}
		if e.PermissionMode != "" {
			parts = append(parts, "mode "+e.PermissionMode)
		}
		if e.Version != "" {
			parts = append(parts, "claude "+e.Version)
		}
		return []Block{{Kind: KindSystem, Label: "init", Text: strings.Join(parts, " · "), Detail: e.CWD}}
	case "thinking_tokens":
		return []Block{{Kind: KindThinkingTokens, Label: "thinking", Delta: e.EstimatedDelta}}
	case "task_started", "task_progress", "task_updated", "task_notification", "background_tasks_changed":
		return []Block{taskStatus(e)}
	case "mado_trimmed":
		return []Block{{Kind: KindSystem, Label: "trimmed",
			Text: fmt.Sprintf("%d earlier exchange(s) dropped from the model's context to fit its window", e.Dropped)}}
	case "mado_plan":
		b := Block{Kind: KindPlan, Label: "plan", PlanID: e.PlanID}
		if e.Plan != nil {
			b.Text = e.Plan.Summary
		}
		return []Block{b}
	case "mado_tool_limit":
		return []Block{{Kind: KindSystem, Label: "tools", IsError: true,
			Text: fmt.Sprintf("stopped after %d tool rounds (the per-message limit); send a message to continue", e.Rounds)}}
	case "api_retry":
		status := "no response"
		if e.ErrorStatus != nil {
			status = fmt.Sprintf("HTTP %d", *e.ErrorStatus)
		}
		return []Block{{Kind: KindSystem, Label: "retry", IsError: true,
			Text: fmt.Sprintf("API retry %d/%d (%s, %s)", e.Attempt, e.MaxRetries, status, unquote(e.Error))}}
	}
	b := rawBlock("system/"+e.Subtype, raw)
	b.Kind = KindSystem
	return []Block{b}
}

// taskStatus is the one-line form of a background-task event.
func taskStatus(e envelope) Block {
	who := e.SubagentType
	if who == "" {
		who = "task"
	}
	b := Block{Kind: KindStatus, Label: e.Subtype, Parent: e.ToolUseID}
	switch e.Subtype {
	case "task_started":
		b.Text = who + " started: " + e.Description
		if e.Backgrounded {
			b.Text += " (background)"
		}
	case "task_progress":
		b.Text = who + ": " + e.Description
		if u := e.TaskUsage; u != nil {
			b.Text += fmt.Sprintf(" (%d tools, %s tokens)", u.ToolUses, Thousands(u.TotalTokens))
		}
	case "task_updated":
		var p struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(e.Patch, &p)
		b.Text = "task " + p.Status
		if p.Status == "" {
			b.Text = "task updated"
		}
	case "task_notification":
		b.Text = who + " " + e.Status
		if e.Summary != "" {
			b.Text += ": " + Abbrev(strings.Join(strings.Fields(e.Summary), " "), 160)
			b.Detail = detailIfLonger(e.Summary, 160)
		}
	case "background_tasks_changed":
		if len(e.Tasks) == 0 {
			b.Text = "no background tasks"
		} else {
			var ds []string
			for _, t := range e.Tasks {
				ds = append(ds, t.SubagentType+": "+t.Description)
			}
			b.Text = fmt.Sprintf("background tasks (%d): %s", len(e.Tasks), Abbrev(strings.Join(ds, "; "), 200))
		}
	}
	b.Text = Abbrev(b.Text, 300)
	return b
}

// Thousands formats a token count: 950, 12.3k, 1.2M.
func Thousands(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func messageBlocks(role string, content json.RawMessage, raw []byte) []Block {
	// content may be a plain string.
	var s string
	if json.Unmarshal(content, &s) == nil {
		return []Block{textBlock(role, s)}
	}
	var cbs []contentBlock
	if err := json.Unmarshal(content, &cbs); err != nil {
		return []Block{rawBlock(role, raw)}
	}
	var out []Block
	for _, c := range cbs {
		switch c.Type {
		case "text":
			out = append(out, textBlock(role, c.Text))
		case "thinking":
			if strings.TrimSpace(c.Thinking) != "" {
				out = append(out, Block{Kind: KindThinking, Label: "thinking", Text: Abbrev(c.Thinking, 120), Detail: Abbrev(c.Thinking, maxDetail)})
			}
		case "redacted_thinking":
			out = append(out, Block{Kind: KindThinking, Label: "thinking", Text: "(redacted)"})
		case "tool_use", "server_tool_use":
			b := Block{Kind: KindToolUse, Label: c.Name, Text: ToolInputSummary(c.Name, c.Input),
				Detail: Abbrev(prettyJSON(c.Input), maxDetail), ToolID: c.ID}
			if c.Name == "Agent" || c.Name == "Task" {
				var in struct {
					SubagentType string `json:"subagent_type"`
					Model        string `json:"model"`
				}
				_ = json.Unmarshal(c.Input, &in)
				b.Agent, b.Model = in.SubagentType, in.Model
				if b.Agent == "" {
					b.Agent = "general-purpose"
				}
			}
			out = append(out, b)
		case "tool_result":
			text := toolResultText(c.Content)
			out = append(out, Block{Kind: KindToolResult, Label: "result", Text: Abbrev(text, maxResult),
				Detail: detailIfLonger(text, maxResult), IsError: c.IsError, ToolID: c.ToolUseID})
		case "image":
			out = append(out, Block{Kind: KindSystem, Label: role, Text: "(image)"})
		case "mado_file":
			out = append(out, Block{Kind: KindFile, Label: c.Name, Text: c.Name, Detail: c.Mime, FileID: c.FileID, Mime: c.Mime,
				Origin: c.Origin})
		default:
			b, _ := json.Marshal(c)
			out = append(out, rawBlock(role+"/"+c.Type, b))
		}
	}
	return out
}

func textBlock(role, s string) Block {
	k := KindAssistant
	if role == "user" {
		k = KindUser
	}
	return Block{Kind: k, Label: role, Text: s}
}

func resultBlock(e envelope) Block {
	parts := []string{}
	if e.Subtype != "" && e.Subtype != "success" {
		parts = append(parts, e.Subtype)
	} else {
		parts = append(parts, "turn done")
	}
	if e.DurationMS > 0 {
		parts = append(parts, fmt.Sprintf("%.1fs", e.DurationMS/1000))
	}
	if e.NumTurns > 0 {
		parts = append(parts, fmt.Sprintf("%d turns", e.NumTurns))
	}
	switch {
	case e.TotalCostUSD > 0 && e.MadoCurrency != "":
		parts = append(parts, usage.FormatMoney(e.MadoCurrency, e.TotalCostUSD))
	case e.TotalCostUSD > 0:
		parts = append(parts, fmt.Sprintf("$%.4f", e.TotalCostUSD)) // claude's own estimate
	}
	return Block{Kind: KindResult, Label: "result", Text: strings.Join(parts, " · "), IsError: e.IsError,
		Detail: detailIfLonger(e.Result, 0)}
}

func rawBlock(label string, raw []byte) Block {
	return Block{Kind: KindRaw, Label: label, Text: label, Detail: Abbrev(prettyJSON(raw), maxDetail)}
}

// ToolInputSummary picks the most telling field of a tool input.
func ToolInputSummary(tool string, input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return Abbrev(string(input), maxInput)
	}
	if tool == "AskUserQuestion" { // DESIGN §21: the first question, and how many more
		var in struct {
			Questions []struct {
				Question string `json:"question"`
			} `json:"questions"`
		}
		if json.Unmarshal(input, &in) == nil && len(in.Questions) > 0 && in.Questions[0].Question != "" {
			s := in.Questions[0].Question
			if n := len(in.Questions) - 1; n > 0 {
				s += fmt.Sprintf(" (+%d more)", n)
			}
			return Abbrev(s, maxInput)
		}
	}
	for _, k := range []string{"command", "file_path", "pattern", "path", "url", "query", "description", "prompt"} {
		if v, ok := m[k].(string); ok && v != "" {
			s := v
			if k == "pattern" {
				if p, ok := m["path"].(string); ok {
					s += " in " + p
				}
			}
			return Abbrev(s, maxInput)
		}
	}
	return Abbrev(compactJSON(input), maxInput)
}

func toolResultText(c json.RawMessage) string {
	if len(c) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(c, &s) == nil {
		return s
	}
	var parts []contentBlock
	if json.Unmarshal(c, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "text":
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
			case "image":
				b.WriteString("(image)")
			}
		}
		return b.String()
	}
	return compactJSON(c)
}

// Abbrev cuts s to at most n runes, appending an ellipsis when cut.
func Abbrev(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func detailIfLonger(s string, n int) string {
	if utf8.RuneCountInString(s) > n {
		return Abbrev(s, maxDetail)
	}
	return ""
}

func prettyJSON(b []byte) string {
	var out bytes.Buffer
	if json.Indent(&out, b, "", "  ") != nil {
		return string(b)
	}
	return out.String()
}

func compactJSON(b []byte) string {
	var out bytes.Buffer
	if json.Compact(&out, b) != nil {
		return string(b)
	}
	return out.String()
}

func unquote(b json.RawMessage) string {
	var s string
	if json.Unmarshal(b, &s) == nil {
		return s
	}
	return compactJSON(b)
}

// TTY renders blocks as plain text lines for a terminal (the shim's tmux pane).
func TTY(blocks []Block) string {
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Kind == KindThinkingTokens {
			continue
		}
		prefix := ""
		if bl.Subagent {
			prefix = "  │ "
		}
		var head string
		switch bl.Kind {
		case KindUser:
			head = "\x1b[1;36m> you\x1b[0m "
		case KindAssistant:
			head = "\x1b[1m● \x1b[0m"
		case KindThinking:
			head = "\x1b[2m∴ thinking: "
		case KindToolUse:
			head = "\x1b[33m⚙ " + bl.Label + "\x1b[0m "
		case KindToolResult:
			head = "  ↳ "
			if bl.IsError {
				head = "  \x1b[31m↳ error:\x1b[0m "
			}
		case KindSystem, KindStatus:
			head = "\x1b[2m· "
		case KindResult:
			head = "\x1b[32m✓ "
			if bl.IsError {
				head = "\x1b[31m✗ "
			}
		case KindApproval:
			head = "\x1b[35m? approval "
		default:
			head = "\x1b[2m[" + bl.Label + "] "
		}
		text := bl.Text
		if bl.Kind == KindToolResult {
			text = Abbrev(text, 200)
		}
		lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
		for i, l := range lines {
			if i == 0 {
				b.WriteString(prefix + head + l)
			} else {
				b.WriteString("\n" + prefix + "    " + l)
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String()
}

// KindApproval is used by the hub and shim for permission requests (not a stream-json type).
const KindApproval = "approval"
