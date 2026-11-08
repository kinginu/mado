// Package usage computes which models did the work in a session (DESIGN §15): API
// responses per message.model, split into the main model and subagents, the number
// of Agent tool calls, and token counts. It reads stream-json events, so it works on
// the hub's stored events and on a worker's spool alike.
package usage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
)

// Row is one model in one role.
type Row struct {
	Model     string // message.model as the upstream reported it
	Subagent  bool   // parent_tool_use_id set
	Responses int    // distinct message ids (one API response is often several events)
	Events    int    // assistant events ("turns" in the UI)
	Input     int64  // input + cache read + cache write tokens, from message.usage
	Output    int64
}

// TokenRow is Claude Code's own count from result.modelUsage, keyed by the model
// name Claude Code asked for (for a router profile: the router name).
type TokenRow struct {
	Name                              string
	Input, CacheRead, CacheWrite, Out int64
}

// Stats is the summary.
type Stats struct {
	Rows       []Row
	AgentCalls int
	Tokens     []TokenRow
}

type rowKey struct {
	model string
	sub   bool
}

type msgUsage struct {
	key           rowKey
	input, output int64
}

// Acc accumulates events. Adding the same event twice does not change the result
// for assistant events (keyed by message id) or results (cumulative values).
type Acc struct {
	events map[rowKey]int
	anon   map[rowKey]int // assistant events without a message id
	msgs   map[string]*msgUsage
	tools  map[string]bool
	calls  int
	last   map[string]TokenRow // latest cumulative modelUsage per name
	base   map[string]TokenRow // carried over from an earlier claude process
}

func New() *Acc {
	return &Acc{events: map[rowKey]int{}, anon: map[rowKey]int{}, msgs: map[string]*msgUsage{}, tools: map[string]bool{},
		last: map[string]TokenRow{}, base: map[string]TokenRow{}}
}

type event struct {
	Type            string  `json:"type"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Message         *struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content json.RawMessage
		Usage   *struct {
			Input      int64 `json:"input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			Output     int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ModelUsage map[string]struct {
		Input      int64 `json:"inputTokens"`
		Output     int64 `json:"outputTokens"`
		CacheRead  int64 `json:"cacheReadInputTokens"`
		CacheWrite int64 `json:"cacheCreationInputTokens"`
	} `json:"modelUsage"`
}

// Add feeds one stream-json object. Anything it does not understand is ignored.
func (a *Acc) Add(raw []byte) {
	var e event
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	switch e.Type {
	case "assistant":
		if e.Message == nil {
			return
		}
		k := rowKey{e.Message.Model, e.ParentToolUseID != nil && *e.ParentToolUseID != ""}
		if k.model == "" {
			k.model = "(unknown)"
		}
		var blocks []struct {
			Type, ID, Name string
		}
		_ = json.Unmarshal(e.Message.Content, &blocks)
		for _, b := range blocks {
			if b.Type == "tool_use" && (b.Name == "Agent" || b.Name == "Task") && !a.tools[b.ID] {
				if b.ID != "" {
					a.tools[b.ID] = true
				}
				a.calls++
			}
		}
		if e.Message.ID == "" {
			a.anon[k]++
			a.events[k]++
			return
		}
		m := a.msgs[e.Message.ID]
		if m == nil {
			m = &msgUsage{key: k}
			a.msgs[e.Message.ID] = m
		}
		a.events[k]++
		if u := e.Message.Usage; u != nil {
			// Streaming repeats the usage on every event of a response; keep the largest.
			m.input = max(m.input, u.Input+u.CacheRead+u.CacheWrite)
			m.output = max(m.output, u.Output)
		}
	case "mado_spawn":
		// A new claude process (DESIGN §17.13): its modelUsage starts from zero.
		for name, l := range a.last {
			b := a.base[name]
			a.base[name] = TokenRow{Name: name, Input: b.Input + l.Input, CacheRead: b.CacheRead + l.CacheRead,
				CacheWrite: b.CacheWrite + l.CacheWrite, Out: b.Out + l.Out}
		}
		a.last = map[string]TokenRow{}
	case "result":
		for name, u := range e.ModelUsage {
			cur := TokenRow{Name: name, Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Out: u.Output}
			prev, seen := a.last[name]
			// modelUsage is cumulative per claude process; a smaller value means a new
			// process (resume), so the earlier total is kept as a base.
			if seen && cur.Input+cur.CacheRead+cur.CacheWrite+cur.Out < prev.Input+prev.CacheRead+prev.CacheWrite+prev.Out {
				b := a.base[name]
				a.base[name] = TokenRow{Name: name, Input: b.Input + prev.Input, CacheRead: b.CacheRead + prev.CacheRead,
					CacheWrite: b.CacheWrite + prev.CacheWrite, Out: b.Out + prev.Out}
			}
			a.last[name] = cur
		}
	}
}

// Stats returns the summary: main rows first, then by responses.
func (a *Acc) Stats() Stats {
	rows := map[rowKey]*Row{}
	get := func(k rowKey) *Row {
		if rows[k] == nil {
			rows[k] = &Row{Model: k.model, Subagent: k.sub}
		}
		return rows[k]
	}
	for _, m := range a.msgs {
		r := get(m.key)
		r.Responses++
		r.Input += m.input
		r.Output += m.output
	}
	for k, n := range a.anon {
		get(k).Responses += n
	}
	for k, n := range a.events {
		get(k).Events = n
	}
	s := Stats{AgentCalls: a.calls}
	for _, r := range rows {
		s.Rows = append(s.Rows, *r)
	}
	slices.SortFunc(s.Rows, func(x, y Row) int {
		switch {
		case x.Subagent != y.Subagent:
			if x.Subagent {
				return 1
			}
			return -1
		case x.Responses != y.Responses:
			return y.Responses - x.Responses
		case x.Events != y.Events:
			return y.Events - x.Events
		}
		return strings.Compare(x.Model, y.Model)
	})
	for name, l := range a.last {
		b := a.base[name]
		s.Tokens = append(s.Tokens, TokenRow{Name: name, Input: b.Input + l.Input, CacheRead: b.CacheRead + l.CacheRead,
			CacheWrite: b.CacheWrite + l.CacheWrite, Out: b.Out + l.Out})
	}
	slices.SortFunc(s.Tokens, func(x, y TokenRow) int { return strings.Compare(x.Name, y.Name) })
	return s
}

// ShortModel drops an org prefix: "deepseek-ai/deepseek-v4-flash" -> "deepseek-v4-flash".
func ShortModel(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 && i+1 < len(m) {
		return m[i+1:]
	}
	return m
}

// Short is the session-list column: "glm-5.3 ×7, deepseek-v4-pro ×12 (sub)". The
// count is assistant events (turns as Claude Code streams them); several events can
// belong to one API response.
func (s Stats) Short() string {
	var parts []string
	for _, r := range s.Rows {
		p := fmt.Sprintf("%s ×%d", ShortModel(r.Model), r.Events)
		if r.Subagent {
			p += " (sub)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

// Role names a row's role.
func (r Row) Role() string {
	if r.Subagent {
		return "subagent"
	}
	return "main"
}

// Table is the terminal form (`mado session-models`).
func (s Stats) Table() string {
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tMODEL\tTURNS\tRESPONSES\tINPUT\tOUTPUT")
	for _, r := range s.Rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\n", r.Role(), r.Model, r.Events, r.Responses, r.Input, r.Output)
	}
	tw.Flush()
	fmt.Fprintf(&sb, "Agent tool calls: %d\n", s.AgentCalls)
	if len(s.Tokens) > 0 {
		sb.WriteString("Claude Code's token count (result.modelUsage, by requested name):\n")
		tw = tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tINPUT\tCACHE READ\tCACHE WRITE\tOUTPUT")
		for _, t := range s.Tokens {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\n", t.Name, t.Input, t.CacheRead, t.CacheWrite, t.Out)
		}
		tw.Flush()
	}
	return sb.String()
}

// ReadJSONL feeds every line of r: a spool line ({"seq","ts","raw"}) contributes its
// raw event, any other JSON line is taken as a stream-json event itself.
func ReadJSONL(r io.Reader) (*Acc, error) {
	a := New()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var sp struct {
			Raw json.RawMessage `json:"raw"`
		}
		if json.Unmarshal(line, &sp) == nil && len(sp.Raw) > 0 && sp.Raw[0] == '{' {
			a.Add(sp.Raw)
		} else {
			a.Add(line)
		}
	}
	return a, sc.Err()
}
