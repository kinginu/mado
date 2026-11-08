package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/tools"
)

// Host is what the runner needs from the hub. Append stores an event as the session's
// next seq and publishes it to viewers; Delta publishes unsaved streaming text.
type Host interface {
	Events(sessionID string) ([]StoredEvent, error)
	Append(sessionID string, raw []byte) (int64, error)
	Delta(sessionID string, seq int64, text string)
	SetState(sessionID, state string)
	// Attachment loads a file the owner uploaded to this session.
	Attachment(owner, sessionID, fileID string) (*Attachment, error)
	// RecordUsage writes the usage row of one response and returns its cost.
	RecordUsage(t *Turn, messageID, model string, u messages.Usage) float64
	// StoreImage stores a generated image as a file of the session's owner (origin
	// generated) and returns it.
	StoreImage(t *Turn, name, mime string, data []byte) (*Attachment, error)
}

// StoredEvent is one stored stream-json line.
type StoredEvent struct {
	Seq int64
	Raw []byte
}

// Attachment is a stored file.
type Attachment struct {
	ID, Name, Mime, SHA256 string
	Size                   int64
	Path                   string // the blob on disk
}

// Turn is one user message to run.
type Turn struct {
	SessionID, Owner string
	Model            Model
	Key              string // resolved from Model.KeyRef
	System           string // the session's own system prompt
	Text             string
	FileIDs          []string
	// Tools, when set, replace the hub's tool registry for this turn, and Prompt
	// replaces DefaultSystem (the concierge, DESIGN §20).
	Tools  ToolSet
	Prompt string
	// Note marks a message mado writes into the conversation for the model (the
	// outcome of a plan); it is stored with "mado_note" and shown as a notice.
	Note string
	// MaxText raises the MaxTextBytes limit for a message mado writes itself (the
	// inbox digest's records, DESIGN §23.5); 0 keeps the default.
	MaxText int
}

// ToolSet is a set of tools a turn offers; *tools.Registry is one.
type ToolSet interface {
	Defs() []messages.Tool
	Run(ctx context.Context, name string, input json.RawMessage) (tools.Result, error)
}

// UserError is a refusal shown to the user as is.
type UserError string

func (e UserError) Error() string { return string(e) }

// ErrBusy: a turn is already running in this session (the API answers 409).
var ErrBusy = errors.New("a reply is still being written in this session")

// Limits.
const (
	MaxTextBytes   = 200_000
	MaxSystemBytes = 4000
	MaxFiles       = 20
	trimMargin     = 2000
	imageTokens    = 1600 // estimate per image block
	documentTokens = 3000 // estimate per native PDF block
)

// Session states the runner reports.
const (
	StateRunning = "running"
	StateIdle    = "idle"
)

// Runner runs chat turns, one at a time per session.
type Runner struct {
	Host          Host
	Client        *messages.Client
	MaxToolRounds int
	// Tools are offered to the model when it has any enabled (DESIGN §17.7).
	Tools *tools.Registry
	// Currency tags the cost in result events ([budgets] currency, DESIGN §17.11).
	Currency string
	Now      func() time.Time
	// DeltaEvery coalesces streamed text into one delta event per interval, so a fast
	// stream does not overflow the SSE subscribers.
	DeltaEvery time.Duration

	running sync.Map // session id -> *active
	pdf     pdfCache
}

type active struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewRunner has the defaults.
func NewRunner(h Host) *Runner {
	return &Runner{Host: h, Client: messages.DefaultClient, MaxToolRounds: 8, Now: time.Now, DeltaEvery: 50 * time.Millisecond}
}

// Running reports whether a turn runs in the session.
func (r *Runner) Running(sessionID string) bool {
	_, ok := r.running.Load(sessionID)
	return ok
}

// Interrupt cancels the running turn; false if there is none.
func (r *Runner) Interrupt(sessionID string) bool {
	v, ok := r.running.Load(sessionID)
	if ok {
		v.(*active).cancel()
	}
	return ok
}

// Wait blocks until the session's running turn (if any) has ended.
func (r *Runner) Wait(sessionID string) {
	if v, ok := r.running.Load(sessionID); ok {
		<-v.(*active).done
	}
}

// Close interrupts every running turn and waits for them.
func (r *Runner) Close() {
	r.running.Range(func(k, v any) bool {
		v.(*active).cancel()
		<-v.(*active).done
		return true
	})
}

// Send stores the user message and starts the turn. It returns the user event's seq.
// Refusals (busy, a model that cannot see images, attachments that do not fit) happen
// before anything is stored.
func (r *Runner) Send(t Turn) (int64, error) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &active{cancel: cancel, done: make(chan struct{})}
	if _, busy := r.running.LoadOrStore(t.SessionID, a); busy {
		cancel()
		return 0, ErrBusy
	}
	seq, err := r.accept(ctx, &t)
	if err != nil {
		r.running.Delete(t.SessionID)
		cancel()
		close(a.done)
		return 0, err
	}
	r.Host.SetState(t.SessionID, StateRunning)
	go func() {
		defer close(a.done)
		defer r.running.Delete(t.SessionID)
		defer cancel()
		r.run(ctx, &t)
		r.Host.SetState(t.SessionID, StateIdle)
	}()
	return seq, nil
}

// madoFile is the stored form of an attachment in a user event (never base64).
// Origin "generated" marks an image a tool made; it is shown, never sent back.
type madoFile struct {
	Type   string `json:"type"` // mado_file
	FileID string `json:"file_id"`
	Name   string `json:"name"`
	Mime   string `json:"mime"`
	Origin string `json:"origin,omitempty"`
}

// OriginGenerated marks a file made by generate_image.
const OriginGenerated = "generated"

func (r *Runner) accept(ctx context.Context, t *Turn) (int64, error) {
	t.Text = strings.TrimSpace(t.Text)
	limit := MaxTextBytes
	if t.MaxText > 0 {
		limit = t.MaxText
	}
	if (t.Text == "" && len(t.FileIDs) == 0) || len(t.Text) > limit || !utf8.ValidString(t.Text) {
		return 0, UserError("empty or invalid message")
	}
	if len(t.FileIDs) > MaxFiles {
		return 0, UserError(fmt.Sprintf("at most %d files per message", MaxFiles))
	}
	var content []any
	var tokens int
	seen := map[string]bool{}
	for _, id := range t.FileIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		at, err := r.Host.Attachment(t.Owner, t.SessionID, id)
		if err != nil {
			return 0, UserError("unknown file")
		}
		_, est, err := r.expand(ctx, t.Model, at) // refuses images for a blind model
		if err != nil {
			return 0, err
		}
		tokens += est
		content = append(content, madoFile{Type: "mado_file", FileID: at.ID, Name: at.Name, Mime: at.Mime})
	}
	if limit := t.Model.ContextWindow / 2; tokens > limit {
		return 0, UserError(fmt.Sprintf("the attachments are too long for this model (about %d tokens, the limit is %d)", tokens, limit))
	}
	if t.Text != "" {
		content = append(content, messages.Block{Type: "text", Text: t.Text})
	}
	ev := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil, "session_id": t.SessionID}
	if t.Note != "" {
		ev["mado_note"] = t.Note
	}
	raw, _ := json.Marshal(ev)
	return r.Host.Append(t.SessionID, raw)
}

// toolSet is the turn's tools: its own set, else the registry (nil: none).
func (r *Runner) toolSet(t *Turn) ToolSet {
	if t.Tools != nil {
		return t.Tools
	}
	if r.Tools == nil {
		return nil
	}
	return r.Tools
}

// run is the turn: call, store, run the tools the answer asks for, and call again
// until the model answers without tools or max_tool_rounds is reached.
func (r *Runner) run(ctx context.Context, t *Turn) {
	start := r.Now()
	var total messages.Usage
	var cost float64
	calls, toolRounds := 0, 0
	subtype, isErr := "success", false
	retried := false
	var defs []messages.Tool
	if ts := r.toolSet(t); ts != nil {
		defs = ts.Defs()
	}
	limit := max(1, r.MaxToolRounds)
	for {
		evs, err := r.Host.Events(t.SessionID)
		if err != nil {
			r.fail(t, "could not read the session: "+err.Error())
			subtype, isErr = "error_during_execution", true
			break
		}
		conv := r.conversation(ctx, t, evs)
		if n := conv.trimToFit(t.Model); n > 0 {
			r.storeTrim(t, conv, n)
		}
		req := messages.Request{Model: t.Model.Model, MaxTokens: t.Model.MaxOutput, System: r.system(t, defs),
			Messages: conv.messages(), Tools: defs}
		nextSeq := conv.lastSeq + 1 // the seq the assistant event will get
		resp, err := r.call(ctx, t, req, nextSeq)
		if err != nil && messages.IsContextOverflow(err) && !retried {
			retried = true
			if n := conv.drop(2); n > 0 {
				r.storeTrim(t, conv, n)
				continue
			}
		}
		calls++
		if resp != nil && (err == nil || len(resp.Content) > 0) {
			if err != nil {
				resp.StopReason = "error"
				if ctx.Err() != nil {
					resp.StopReason = "interrupted"
				}
			}
			cost += r.storeAssistant(t, resp)
			total.Add(resp.Usage)
		}
		if err != nil {
			if ctx.Err() != nil {
				subtype = "interrupted"
			} else {
				r.fail(t, err.Error())
				subtype, isErr = "error_during_execution", true
			}
			break
		}
		uses := toolUses(resp.Content)
		if resp.StopReason != "tool_use" || len(uses) == 0 {
			break
		}
		if toolRounds >= limit {
			// Answer the calls so the conversation stays valid, and say why.
			r.storeToolResults(t, uses, nil, fmt.Sprintf("Not run: the limit of %d tool rounds per message was reached.", limit))
			raw, _ := json.Marshal(map[string]any{"type": "system", "subtype": "mado_tool_limit", "rounds": limit,
				"session_id": t.SessionID})
			_, _ = r.Host.Append(t.SessionID, raw)
			break
		}
		toolRounds++
		c, interrupted := r.runTools(ctx, t, uses)
		cost += c
		if interrupted {
			subtype = "interrupted"
			break
		}
	}
	raw, _ := json.Marshal(map[string]any{"type": "result", "subtype": subtype, "is_error": isErr,
		"duration_ms": r.Now().Sub(start).Milliseconds(), "num_turns": calls, "usage": total,
		"total_cost_usd": cost, "mado_currency": r.Currency, "session_id": t.SessionID})
	_, _ = r.Host.Append(t.SessionID, raw)
}

func toolUses(content []messages.Block) []messages.Block {
	var out []messages.Block
	for _, b := range content {
		if b.Type == "tool_use" && b.ID != "" {
			out = append(out, b)
		}
	}
	return out
}

// toolTimeout bounds one call (image generation is the slow one).
const toolTimeout = 4 * time.Minute

type toolOutcome struct {
	text    string
	isError bool
	file    *madoFile
}

// runTools runs the calls of one answer one after another and stores their results
// as one user event. A tool error becomes an is_error result; an interrupt answers
// the remaining calls as interrupted. It returns the cost of generated images.
func (r *Runner) runTools(ctx context.Context, t *Turn, uses []messages.Block) (float64, bool) {
	outs := make([]toolOutcome, len(uses))
	var cost float64
	var events []json.RawMessage
	ts := r.toolSet(t)
	for i, u := range uses {
		if ctx.Err() != nil {
			outs[i] = toolOutcome{text: "Interrupted by the user.", isError: true}
			continue
		}
		var res tools.Result
		var err error = tools.Error("unknown tool " + u.Name)
		tctx, cancel := context.WithTimeout(ctx, toolTimeout)
		if ts != nil {
			res, err = ts.Run(tctx, u.Name, u.Input)
		}
		cancel()
		if err == nil && len(res.Event) > 0 {
			events = append(events, res.Event)
		}
		switch {
		case err != nil && ctx.Err() != nil:
			outs[i] = toolOutcome{text: "Interrupted by the user.", isError: true}
		case err != nil:
			outs[i] = toolOutcome{text: "Error: " + toolError(err), isError: true}
		case res.Image != nil:
			o, c := r.storeImage(t, res.Image)
			outs[i] = o
			cost += c
		default:
			outs[i] = toolOutcome{text: res.Text}
		}
	}
	r.storeToolResults(t, uses, outs, "")
	for _, ev := range events {
		_, _ = r.Host.Append(t.SessionID, ev)
	}
	return cost, ctx.Err() != nil
}

func toolError(err error) string {
	var te tools.Error
	if errors.As(err, &te) {
		return te.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the tool took too long"
	}
	return "the tool failed"
}

// storeImage keeps a generated image as the owner's file and prices it by the
// backend's token count. The model learns only that the user has seen it.
func (r *Runner) storeImage(t *Turn, img *tools.Image) (toolOutcome, float64) {
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp"}[img.Mime]
	name := "image-" + r.Now().UTC().Format("20060102-150405") + ext
	at, err := r.Host.StoreImage(t, name, img.Mime, img.Data)
	if err != nil {
		msg := "could not store the image"
		var ue UserError
		if errors.As(err, &ue) {
			msg = ue.Error()
		}
		return toolOutcome{text: "Error: " + msg, isError: true}, 0
	}
	var cost float64
	if img.Usage != nil {
		cost = r.Host.RecordUsage(t, "img_"+at.ID, img.Model, *img.Usage)
	}
	return toolOutcome{text: "Image " + at.ID + " was generated and is shown to the user.",
		file: &madoFile{Type: "mado_file", FileID: at.ID, Name: at.Name, Mime: at.Mime, Origin: OriginGenerated}}, cost
}

// storeToolResults stores one user event with a tool_result per call (outs[i], or
// text for every call when outs is nil), followed by the generated images.
func (r *Runner) storeToolResults(t *Turn, uses []messages.Block, outs []toolOutcome, text string) {
	var content []any
	var images []any
	for i, u := range uses {
		o := toolOutcome{text: text, isError: true}
		if outs != nil {
			o = outs[i]
		}
		c, _ := json.Marshal(o.text)
		content = append(content, messages.Block{Type: "tool_result", ToolUseID: u.ID, Content: c, IsError: o.isError})
		if o.file != nil {
			images = append(images, *o.file)
		}
	}
	content = append(content, images...)
	raw, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil, "session_id": t.SessionID})
	_, _ = r.Host.Append(t.SessionID, raw)
}

func (r *Runner) fail(t *Turn, msg string) {
	raw, _ := json.Marshal(map[string]any{"type": "mado_error", "message": msg, "session_id": t.SessionID})
	_, _ = r.Host.Append(t.SessionID, raw)
}

func (r *Runner) storeTrim(t *Turn, c *conversation, n int) {
	raw, _ := json.Marshal(map[string]any{"type": "system", "subtype": "mado_trimmed", "dropped": n,
		"before_seq": c.cutoff, "session_id": t.SessionID})
	if seq, err := r.Host.Append(t.SessionID, raw); err == nil {
		c.lastSeq = seq
	}
}

// storeAssistant stores the response as a stream-json assistant event and records its
// usage; it returns the cost.
func (r *Runner) storeAssistant(t *Turn, resp *messages.Response) float64 {
	if resp.ID == "" {
		resp.ID = "mado_" + newID()
	}
	if resp.Model == "" {
		resp.Model = t.Model.Model
	}
	content := resp.Content
	if content == nil {
		content = []messages.Block{}
	}
	raw, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{
		"id": resp.ID, "type": "message", "role": "assistant", "model": resp.Model, "content": content,
		"stop_reason": resp.StopReason, "usage": resp.Usage}, "parent_tool_use_id": nil, "session_id": t.SessionID})
	_, _ = r.Host.Append(t.SessionID, raw)
	return r.Host.RecordUsage(t, resp.ID, resp.Model, resp.Usage)
}

// call streams one request, publishing text deltas, and returns the response (partial
// on error).
func (r *Runner) call(ctx context.Context, t *Turn, req messages.Request, seq int64) (*messages.Response, error) {
	up := t.Model.Upstream(t.Key)
	ch, err := r.Client.Stream(ctx, up, req)
	if err != nil {
		return nil, err
	}
	var pending strings.Builder
	last := time.Time{}
	flush := func() {
		if pending.Len() > 0 {
			r.Host.Delta(t.SessionID, seq, pending.String())
			pending.Reset()
			last = time.Now()
		}
	}
	var resp *messages.Response
	var serr error
	for e := range ch {
		switch e.Type {
		case "content_block_delta":
			if e.Delta.Type == "text_delta" && e.Delta.Text != "" {
				pending.WriteString(e.Delta.Text)
				if time.Since(last) >= r.DeltaEvery {
					flush()
				}
			}
		case "content_block_stop":
			flush()
		case "message_stop":
			resp = e.Response
		case "error":
			resp, serr = e.Response, e.Err
		}
	}
	flush()
	if resp == nil && serr == nil {
		serr = errors.New("upstream: stream ended without a message")
	}
	return resp, serr
}

func (r *Runner) system(t *Turn, defs []messages.Tool) string {
	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
	}
	s := DefaultSystem(r.Now(), names)
	if t.Prompt != "" {
		s = t.Prompt
	}
	if strings.TrimSpace(t.System) != "" {
		s += "\n\n" + t.System
	}
	return s
}

// DefaultSystem is mado's system prompt for chat sessions. The tool guidance is
// there only when tools are offered.
func DefaultSystem(now time.Time, tools []string) string {
	s := "You are mado, a helpful assistant in a private, self-hosted chat.\n" +
		"Today's date is " + now.UTC().Format("2006-01-02 (Monday)") + ".\n" +
		"Answer in the language the user writes in.\n" +
		"You may use Markdown: headings, lists, tables and fenced code blocks with a language tag.\n" +
		"Attached files appear as <document name=\"...\"> blocks before the user's message; refer to them by name."
	if len(tools) > 0 {
		s += "\nYou have these tools: " + strings.Join(tools, ", ") + ". Use a tool only when it helps answer the question, " +
			"say what you found, and name the pages you used with their URLs."
	}
	return s
}
