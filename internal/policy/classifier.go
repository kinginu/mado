package policy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/render"
)

// Classifier is step 4 of DESIGN §18.2: a model decides what the rules leave open. It
// calls one [[chat.models]] entry through internal/messages with the forced tool
// `decide`. Every failure (timeout, transport error, malformed answer)
// is an Ask, never an Allow.
type Classifier struct {
	Upstream messages.Upstream
	Model    string // the upstream model id
	// Timeout bounds one attempt (default 20 s); Retries more attempts follow a
	// failure (default 1; a negative value means none).
	Timeout   time.Duration
	Retries   int
	MaxTokens int // default 4096
	// Client sends the requests; nil makes one with Timeout and no retries of its own.
	Client *messages.Client

	once sync.Once
}

// Input is what the classifier sees of one approval request.
type Input struct {
	Tool      string
	ToolInput json.RawMessage
	CWD       string
	Roots     []string
	LastUser  string // the session's last user message
	Recent    string // the last blocks as plain text (RecentText)
}

// Call is one model response, for the usage rows (kind auto).
type Call struct {
	MessageID string
	Model     string
	Usage     messages.Usage
}

// Result is the classifier's answer. Decision is Allow, Deny or Ask.
type Result struct {
	Decision Decision
	Reason   string
	Calls    []Call
}

const (
	defaultTimeout = 20 * time.Second
	maxReason      = 300
	maxToolInput   = 20000
)

// SystemPrompt is the classifier's instruction.
const SystemPrompt = `You are the approval policy of mado, a tool that runs a coding agent (Claude Code) unattended on a developer's machine. The agent asks before running a tool; you answer with the decide tool.

Approve (allow) routine development work inside the project: reading and searching code, building, testing, linting, formatting, running the project's own scripts, git operations that only touch the local repository, installing nothing system-wide.
Deny anything destructive or irreversible (deleting data outside build outputs, rewriting published git history, pushing to remotes with force), anything that sends data from the machine to an unexpected place (uploads, posting secrets, curl to unknown hosts with local files), anything touching secrets, credentials, keys or tokens, and anything changing system configuration, other users' files, services or the shell environment.
Answer ask when you are unsure, when the call goes outside the project roots in a way that is not plainly harmless, or when the user's last message does not cover it.

Keep the reason short (one sentence) and concrete; it is shown to the user.`

// policySummary is put in front of each request.
const policySummary = `Policy so far: hard deny rules (sudo, rm -rf of broad paths, force pushes, secrets, system directories, ...) and allow rules (read-only commands, builds and tests inside the project) already ran and did not decide this call. Edits inside the working directory are accepted without asking.`

var decideTool = messages.Tool{
	Name:        "decide",
	Description: "Record the decision for the pending tool call.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"decision":{"type":"string","enum":["allow","deny","ask"]},"reason":{"type":"string","description":"one short sentence for the user"}},"required":["decision","reason"]}`),
}

func (c *Classifier) init() {
	c.once.Do(func() {
		if c.Timeout <= 0 {
			c.Timeout = defaultTimeout
		}
		if c.Retries == 0 {
			c.Retries = 1
		}
		if c.MaxTokens <= 0 {
			c.MaxTokens = 4096
		}
		if c.Client == nil {
			cl := messages.NewClient()
			cl.FirstByte, cl.Total, cl.Backoff = c.Timeout, c.Timeout, nil
			c.Client = cl
		}
	})
}

// Decide asks the model about one tool call.
func (c *Classifier) Decide(ctx context.Context, sessionID string, in Input) Result {
	c.init()
	req := messages.Request{Model: c.Model, MaxTokens: c.MaxTokens, System: SystemPrompt,
		Messages:   []messages.Message{{Role: "user", Content: []messages.Block{{Type: "text", Text: prompt(in)}}}},
		Tools:      []messages.Tool{decideTool},
		ToolChoice: &messages.ToolChoice{Type: "tool", Name: "decide"}}
	var res Result
	var lastErr error
	for attempt := 0; attempt <= max(c.Retries, 0); attempt++ {
		resp, err := c.call(ctx, req)
		if resp != nil && (resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0) {
			id := resp.ID
			if id == "" {
				id = "auto_" + randomHex()
			}
			res.Calls = append(res.Calls, Call{MessageID: id, Model: resp.Model, Usage: resp.Usage})
		}
		if err == nil {
			var d Decision
			var reason string
			if d, reason, err = parseDecision(resp); err == nil {
				res.Decision, res.Reason = d, reason
				return res
			}
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	res.Decision, res.Reason = Ask, "the classifier failed ("+failure(lastErr)+")"
	return res
}

func (c *Classifier) call(ctx context.Context, req messages.Request) (*messages.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	ch, err := c.Client.Stream(ctx, c.Upstream, req)
	if err != nil {
		return nil, err
	}
	var resp *messages.Response
	for ev := range ch {
		switch ev.Type {
		case "message_stop":
			resp = ev.Response
		case "error":
			resp, err = ev.Response, ev.Err
		}
	}
	if err == nil && resp == nil {
		err = errors.New("the stream ended early")
	}
	return resp, err
}

var errMalformed = errors.New("malformed answer")

func parseDecision(resp *messages.Response) (Decision, string, error) {
	for _, b := range resp.Content {
		if b.Type != "tool_use" || b.Name != "decide" {
			continue
		}
		var v struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if json.Unmarshal(b.Input, &v) != nil {
			return "", "", errMalformed
		}
		reason := render.Abbrev(strings.Join(strings.Fields(v.Reason), " "), maxReason)
		switch d := Decision(strings.ToLower(strings.TrimSpace(v.Decision))); d {
		case Allow, Deny, Ask:
			if reason == "" {
				reason = "no reason given"
			}
			return d, reason, nil
		}
		return "", "", errMalformed
	}
	return "", "", errMalformed
}

func failure(err error) string {
	switch {
	case err == nil:
		return "no answer"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errMalformed):
		return "malformed answer"
	}
	var ae *messages.APIError
	if errors.As(err, &ae) && ae.Status != 0 {
		return fmt.Sprintf("upstream status %d", ae.Status)
	}
	return "upstream error"
}

func randomHex() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func prompt(in Input) string {
	var b strings.Builder
	b.WriteString(policySummary + "\n\n")
	fmt.Fprintf(&b, "Working directory: %s\nProject roots: %s (and /tmp)\n\n", in.CWD, strings.Join(in.Roots, ", "))
	if in.LastUser != "" {
		fmt.Fprintf(&b, "The user's last message:\n<<<\n%s\n>>>\n\n", render.Abbrev(in.LastUser, 4000))
	}
	if in.Recent != "" {
		fmt.Fprintf(&b, "Recent transcript (oldest first, abbreviated):\n<<<\n%s\n>>>\n\n", in.Recent)
	}
	fmt.Fprintf(&b, "The tool call to decide:\ntool: %s\ninput: %s\n", in.Tool, render.Abbrev(string(in.ToolInput), maxToolInput))
	return b.String()
}

// RecentText renders the last n blocks of a session's events (oldest first) as plain
// text, one abbreviated line per block, for the classifier's context. It also
// returns the text of the last user message.
func RecentText(events [][]byte, n int) (recent, lastUser string) {
	var lines []string
	for _, raw := range events {
		for _, bl := range render.Blocks(raw) {
			if bl.Noise || bl.Kind == render.KindThinkingTokens || bl.Kind == render.KindThinking {
				continue
			}
			text := strings.Join(strings.Fields(bl.Text), " ")
			if text == "" {
				continue
			}
			if bl.Kind == render.KindUser {
				lastUser = bl.Text
			}
			label := bl.Kind
			if bl.Label != "" && bl.Label != bl.Kind {
				label += " " + bl.Label
			}
			if bl.Subagent {
				label = "subagent " + label
			}
			lines = append(lines, "["+label+"] "+render.Abbrev(text, 300))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), lastUser
}
