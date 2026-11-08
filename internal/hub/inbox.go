package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kinginu/mado/internal/chat"
	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/tools"
)

// The inbox in the hub (DESIGN §23): the scheduler, the digester and its executor,
// the minute tick, retention, and the inbox tools of chat sessions.

type inboxState struct {
	store  *inbox.Store
	sched  *inbox.Scheduler
	digest *inbox.Digester
	ingest ingestLimiter
	stop   chan struct{}
	done   chan struct{}
}

// inboxTick is how often the digest schedule is checked (DESIGN §23.5).
const inboxTick = time.Minute

// setupInbox seeds [[inbox.accounts]] and starts the scheduler and the minute tick
// when [inbox] enabled. With the inbox off, h.inbox stays nil and every inbox route
// answers 404.
func (h *Hub) setupInbox() error {
	c := &h.cfg.Inbox
	if c.Digest.Schedule == nil || c.MaxItemBytes == 0 { // a Config built in code rather than by LoadConfig
		if err := c.applyDefaults(); err != nil {
			return err
		}
	}
	if !c.Enabled {
		return nil
	}
	st := &inbox.Store{DB: h.store.db}
	var seed []inbox.Account
	for _, ac := range c.Accounts {
		a, err := ac.account()
		if err != nil {
			return err
		}
		if err := inbox.Validate(&a, h.secrets.Keys); err != nil {
			return fmt.Errorf("[[inbox.accounts]] %w", err)
		}
		seed = append(seed, a)
	}
	if err := st.SeedConfig(seed); err != nil {
		return err
	}
	if m := c.Digest.Model; m != "" {
		if _, err := h.store.ChatModel(m); err != nil {
			return fmt.Errorf("[inbox.digest] model %q is not a [[chat.models]] name", m)
		}
	}
	st.ResolveOwners()
	st.FailRunningDigests()
	loc, err := c.Digest.Location()
	if err != nil {
		return err
	}
	ib := &inboxState{store: st, stop: make(chan struct{}), done: make(chan struct{})}
	ib.sched = &inbox.Scheduler{Store: st, Keys: h.secrets.Keys, Log: h.log, MaxItemBytes: c.MaxItemBytes,
		Client: func(allowPrivate bool) *http.Client {
			return tools.Guard{AllowPrivate: allowPrivate}.Client(time.Minute)
		}}
	ib.digest = &inbox.Digester{Store: st, Log: h.log, Executor: h.digestExecutor(),
		Cfg: inbox.DigestConfig{Schedule: c.Digest.ScheduleTimes(), Location: loc, Language: c.Digest.Language,
			MaxItems: c.Digest.MaxItems, MaxBytes: c.Digest.MaxBytes},
		Audit: h.store.Audit}
	h.inbox = ib
	ib.sched.Start()
	go func() {
		defer close(ib.done)
		t := time.NewTicker(inboxTick)
		defer t.Stop()
		for {
			select {
			case <-ib.stop:
				return
			case now := <-t.C:
				st.ResolveOwners()
				ib.digest.Tick(now)
			}
		}
	}()
	return nil
}

// closeInbox stops the tick, the pollers and running digests (before the chat
// runner closes, so a chat digest records its failure).
func (h *Hub) closeInbox() {
	ib := h.inbox
	if ib == nil {
		return
	}
	select {
	case <-ib.stop:
		return
	default:
	}
	close(ib.stop)
	<-ib.done
	ib.sched.Stop()
	ib.digest.Stop()
}

// inboxHousekeep deletes read and archived items past retention_days.
func (h *Hub) inboxHousekeep(now time.Time) {
	if h.inbox == nil {
		return
	}
	n, err := h.inbox.store.Retention(now.AddDate(0, 0, -h.cfg.Inbox.RetentionDays))
	if err != nil {
		h.log.Error("housekeeping: inbox retention", "err", err)
	} else if n > 0 {
		h.log.Info("housekeeping: deleted old inbox items", "items", n)
	}
}

// digestExecutor picks the executor (DESIGN §23.5): chat, or claude (a job session,
// §23.6) with chat as the fallback; the digest row says which one ran.
func (h *Hub) digestExecutor() inbox.Executor {
	chat := chatDigestExecutor{h: h}
	if h.cfg.Inbox.Digest.Executor == ExecutorChat {
		return chat
	}
	return inbox.FallbackExecutor{Primary: claudeDigestExecutor{h: h}, Secondary: chat, Log: h.log}
}

// digestModel is the chat executor's model name.
func (h *Hub) digestModel() string {
	if m := h.cfg.Inbox.Digest.Model; m != "" {
		return m
	}
	return h.conciergeModel()
}

// chatDigestExecutor runs a digest as a hub-side session of kind digest on a chat
// model (DESIGN §23.5): the records are the user message, the digest prompt the
// system prompt, propose_task the only tool.
type chatDigestExecutor struct{ h *Hub }

func (chatDigestExecutor) Name() string { return ExecutorChat }

func (e chatDigestExecutor) Run(ctx context.Context, in inbox.DigestInput) (inbox.DigestResult, error) {
	h := e.h
	res := inbox.DigestResult{Executor: ExecutorChat}
	if !h.cfg.Chat.Enabled || h.runner == nil {
		return res, errors.New("the chat executor needs [chat] enabled")
	}
	name := h.digestModel()
	m, err := h.store.ChatModel(name)
	if err != nil {
		return res, fmt.Errorf("no chat model for the digest ([inbox.digest] model %q)", name)
	}
	res.Model = m.Model
	if err := h.checkBudget(in.Owner); err != nil {
		return res, err
	}
	s := Session{ID: proto.NewID(), OwnerSub: in.Owner, Kind: KindDigest, Model: name, State: chat.StateIdle,
		Title: "Inbox digest " + time.Now().In(digestLoc(h)).Format("2006-01-02 15:04")}
	if err := h.store.CreateSession(s); err != nil {
		return res, err
	}
	res.SessionID = s.ID
	if in.OnSession != nil {
		in.OnSession(s.ID)
	}
	h.store.Audit(in.Owner, "session_start", fmt.Sprintf("%s digest %s digest_id=%d", s.ID, name, in.DigestID))
	key := ""
	if m.KeyRef != "" {
		key = h.secrets.Keys[m.KeyRef]
	}
	t := chat.Turn{SessionID: s.ID, Owner: in.Owner, Model: *m, Key: key, Text: in.Message, Prompt: in.System,
		MaxText: 8 << 20}
	if in.Tools != nil {
		t.Tools = in.Tools
	}
	if _, err := h.runner.Send(t); err != nil {
		return res, err
	}
	done := make(chan struct{})
	go func() { h.runner.Wait(s.ID); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		h.runner.Interrupt(s.ID)
		<-done
	}
	res.Cost = h.store.SessionUsage(s.ID).Cost
	if ctx.Err() != nil {
		return res, errors.New("the digest was interrupted or took too long")
	}
	text, err := h.digestOutcome(s.ID)
	res.Text = text
	return res, err
}

func digestLoc(h *Hub) *time.Location {
	loc, err := h.cfg.Inbox.Digest.Location()
	if err != nil {
		return time.UTC
	}
	return loc
}

// digestOutcome reads a finished chat run: the text of the last assistant message,
// or the error the run ended with.
func (h *Hub) digestOutcome(sessionID string) (string, error) {
	evs, err := chatHost{h}.Events(sessionID)
	if err != nil {
		return "", err
	}
	var text, failure, subtype string
	for _, e := range evs {
		var ev struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Message json.RawMessage
		}
		if json.Unmarshal(e.Raw, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			var m struct {
				Content []messages.Block `json:"content"`
			}
			_ = json.Unmarshal(ev.Message, &m)
			var parts []string
			for _, b := range m.Content {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					parts = append(parts, b.Text)
				}
			}
			text = strings.TrimSpace(strings.Join(parts, "\n\n"))
		case "mado_error":
			var m struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(e.Raw, &m)
			failure = m.Message
		case "result":
			subtype = ev.Subtype
		}
	}
	switch {
	case failure != "":
		return "", errors.New(failure)
	case subtype != "success":
		return "", fmt.Errorf("the run ended with %q", subtype)
	}
	return text, nil
}

// --- tools for chat and concierge turns (DESIGN §23.7)

// toolSets offers several tool sets as one; a name is run by the set that defines it.
type toolSets []chat.ToolSet

func (ts toolSets) Defs() []messages.Tool {
	var out []messages.Tool
	for _, t := range ts {
		out = append(out, t.Defs()...)
	}
	return out
}

func (ts toolSets) Run(ctx context.Context, name string, input json.RawMessage) (tools.Result, error) {
	for _, t := range ts {
		if slices.ContainsFunc(t.Defs(), func(d messages.Tool) bool { return d.Name == name }) {
			return t.Run(ctx, name, input)
		}
	}
	return tools.Result{}, tools.Error("unknown tool " + name)
}

// inboxTools are the owner's inbox tools, nil when the inbox is off.
func (h *Hub) inboxTools(owner string) chat.ToolSet {
	if h.inbox == nil {
		return nil
	}
	return inbox.ChatTools{Store: h.inbox.store, Owner: owner}
}

// --- ingest rate limit (DESIGN §23.4): 60 requests per minute per account

type ingestLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

const ingestPerMinute = 60

func (l *ingestLimiter) allow(account string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	cut := now.Add(-time.Minute)
	hs := l.hits[account]
	i := 0
	for i < len(hs) && !hs[i].After(cut) {
		i++
	}
	hs = hs[i:]
	if len(hs) >= ingestPerMinute {
		l.hits[account] = hs
		return false
	}
	l.hits[account] = append(hs, now)
	return true
}
