package hub

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kinginu/mado/internal/chat"
	"github.com/kinginu/mado/internal/files"
	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/tools"
)

// setupChat seeds [[chat.models]] and creates the runner (DESIGN §17.1, §17.3).
func (h *Hub) setupChat() error {
	if h.cfg.Chat.DataDir == "" { // a Config built in code rather than by LoadConfig
		if err := h.cfg.Chat.applyDefaults(); err != nil {
			return err
		}
	}
	ms, err := h.cfg.Chat.ChatModels()
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.KeyRef != "" {
			if _, ok := h.secrets.Keys[m.KeyRef]; !ok {
				return fmt.Errorf("chat model %s: key_ref %q not in secrets file", m.Name, m.KeyRef)
			}
		}
	}
	if err := h.store.SeedChatModels(ms); err != nil {
		return err
	}
	if err := h.cfg.Chat.Tools.Validate(h.secrets.Keys); err != nil {
		return err
	}
	h.tools = tools.New(h.cfg.Chat.Tools, h.secrets.Keys)
	h.blobs = files.Blobs{Dir: h.cfg.Chat.DataDir}
	h.runner = chat.NewRunner(chatHost{h})
	h.runner.MaxToolRounds = h.cfg.Chat.MaxToolRounds
	h.runner.Tools = h.tools
	h.runner.Currency = h.cfg.Budgets.Currency
	// A hub that stopped mid-turn left chat sessions "running"; no runner survives it.
	h.store.ResetChatStates()
	return nil
}

// Close stops the housekeeping, running plans and chat turns (each stores its partial answer).
func (h *Hub) Close() {
	h.stopHousekeeping()
	h.rl.close()
	h.closeInbox()
	if h.planCancel != nil {
		h.planCancel()
		h.planWG.Wait()
	}
	if h.runner != nil {
		h.runner.Close()
	}
}

// chatTools are the tools a chat turn offers, for the session panel.
func (h *Hub) chatTools() []string {
	n := append([]string{}, h.tools.Names()...)
	if h.inbox != nil {
		n = append(n, inbox.ChatToolNames...)
	}
	return n
}

// chatModels lists the models for the form: the default first, then by name.
func (h *Hub) chatModels() []chat.Model {
	if !h.cfg.Chat.Enabled {
		return nil
	}
	ms, _ := h.store.ChatModels()
	def := -1
	for i, m := range ms {
		if m.Default {
			def = i
			break
		}
	}
	if def > 0 {
		m := ms[def]
		ms = append([]chat.Model{m}, slices.Delete(ms, def, def+1)...)
	}
	return ms
}

// newChatSession creates a chat session (no machine, cwd or profile).
func (h *Hub) newChatSession(ws *WebSession, model, system, title string) (string, error) {
	return h.createChat(ws, model, system, title, false)
}

// createChat creates a chat session; concierge marks mado's concierge (DESIGN §20).
func (h *Hub) createChat(ws *WebSession, model, system, title string, concierge bool) (string, error) {
	if !h.cfg.Chat.Enabled {
		return "", userError("chat is disabled on this hub")
	}
	if err := h.checkBudget(ws.User.Sub); err != nil {
		return "", err
	}
	if model == "" {
		if ms := h.chatModels(); len(ms) > 0 {
			model = ms[0].Name
		}
	}
	if _, err := h.store.ChatModel(model); err != nil {
		return "", userError("unknown chat model")
	}
	if len(system) > chat.MaxSystemBytes || !utf8.ValidString(system) {
		return "", userError("system prompt: at most 4000 bytes of text")
	}
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) > 120 {
		return "", userError("title too long")
	}
	s := Session{ID: proto.NewID(), OwnerSub: ws.User.Sub, Kind: KindChat, Model: model, System: strings.TrimSpace(system),
		Title: title, State: chat.StateIdle, Concierge: concierge}
	if err := h.store.CreateSession(s); err != nil {
		return "", userError("could not create session")
	}
	kind := " chat "
	if concierge {
		kind = " concierge "
	}
	h.store.Audit(ws.User.Sub, "session_start", s.ID+kind+model)
	return s.ID, nil
}

// chatSend runs one user message in a chat session.
func (h *Hub) chatSend(ws *WebSession, s *Session, text string, fileIDs []string) (int64, error) {
	if !h.cfg.Chat.Enabled {
		return 0, userError("chat is disabled on this hub")
	}
	if err := h.checkBudget(s.OwnerSub); err != nil {
		return 0, err
	}
	seq, err := h.chatTurn(s, text, fileIDs, "")
	if err != nil {
		return 0, err
	}
	if s.Title == "" {
		title := strings.Join(strings.Fields(text), " ")
		if r := []rune(title); len(r) > 60 {
			title = string(r[:60]) + "…"
		}
		if title != "" {
			_ = h.store.SetSessionTitle(s.ID, title)
		}
	}
	h.store.Audit(ws.User.Sub, "message", fmt.Sprintf("%s len=%d files=%d", s.ID, len(text), len(fileIDs))) // never the content
	return seq, nil
}

// chatTurn hands one message to the runner; a concierge session gets its own tools
// and prompt (DESIGN §20). note marks a message mado wrote for the model.
func (h *Hub) chatTurn(s *Session, text string, fileIDs []string, note string) (int64, error) {
	m, err := h.store.ChatModel(s.Model)
	if err != nil {
		return 0, userError("this session's model " + s.Model + " is no longer configured")
	}
	key := ""
	if m.KeyRef != "" {
		key = h.secrets.Keys[m.KeyRef]
	}
	t := chat.Turn{SessionID: s.ID, Owner: s.OwnerSub, Model: *m, Key: key, System: s.System, Text: text, FileIDs: fileIDs, Note: note}
	if s.Concierge {
		t.Tools = conciergeTools{h: h, sessionID: s.ID, owner: s.OwnerSub}
		t.Prompt = chat.ConciergeSystem(time.Now())
	}
	if it := h.inboxTools(s.OwnerSub); it != nil { // DESIGN §23.7
		base := t.Tools
		if base == nil {
			base = h.tools
		}
		t.Tools = toolSets{base, it}
	}
	return h.runner.Send(t)
}

// chatHost is the runner's view of the hub.
type chatHost struct{ h *Hub }

func (c chatHost) Events(sessionID string) ([]chat.StoredEvent, error) {
	var out []chat.StoredEvent
	var after int64
	for {
		evs, err := c.h.store.Events(sessionID, after, 1000)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			out = append(out, chat.StoredEvent{Seq: e.Seq, Raw: e.Raw})
			after = e.Seq
		}
		if len(evs) < 1000 {
			return out, nil
		}
	}
}

func (c chatHost) Append(sessionID string, raw []byte) (int64, error) {
	seq, err := c.h.store.AppendEvent(sessionID, raw)
	if err != nil {
		c.h.log.Error("append chat event", "session", sessionID, "err", err)
		return 0, err
	}
	c.h.publishEvent(sessionID, seq, raw)
	return seq, nil
}

func (c chatHost) Delta(sessionID string, seq int64, text string) {
	b, _ := json.Marshal(map[string]any{"seq": seq, "text": text})
	c.h.broker.publish(sessionID, sseMsg{event: "delta", data: string(b)})
}

func (c chatHost) SetState(sessionID, state string) {
	c.h.store.SetSessionState(sessionID, state)
	c.h.publishState(sessionID, state)
}

func (c chatHost) Attachment(owner, sessionID, fileID string) (*chat.Attachment, error) {
	f, err := c.h.store.File(fileID)
	if err != nil || f.OwnerSub != owner || f.SessionID != sessionID {
		return nil, fmt.Errorf("no such file")
	}
	p, err := c.h.blobs.Path(f.SHA256)
	if err != nil {
		return nil, err
	}
	return &chat.Attachment{ID: f.ID, Name: f.Name, Mime: f.Mime, SHA256: f.SHA256, Size: f.Size, Path: p}, nil
}

func (c chatHost) RecordUsage(t *chat.Turn, messageID, model string, u messages.Usage) float64 {
	r := c.h.rates(model, false)
	kind := KindChat
	if s, err := c.h.store.Session(t.SessionID); err == nil && s.Kind == KindDigest {
		kind = KindDigest // DESIGN §23.5: usage rows of kind digest
	}
	row := UsageRow{SessionID: t.SessionID, Kind: kind, OwnerSub: t.Owner, Model: model, MessageID: messageID,
		Input: u.InputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens, Output: u.OutputTokens}
	if err := c.h.store.RecordUsage(row, r); err != nil {
		c.h.log.Error("record usage", "session", t.SessionID, "err", err)
	}
	return (float64(row.Input)*r.Input + float64(row.CacheRead)*r.CacheRead + float64(row.CacheWrite)*r.CacheWrite +
		float64(row.Output)*r.Output) / 1e6
}

func (c chatHost) StoreImage(t *chat.Turn, name, mimeType string, data []byte) (*chat.Attachment, error) {
	h := c.h
	if used := h.store.UserFileBytes(t.Owner); used+int64(len(data)) > int64(h.cfg.Chat.UserQuotaMB)<<20 {
		return nil, chat.UserError(fmt.Sprintf("the image would exceed your %d MB file quota", h.cfg.Chat.UserQuotaMB))
	}
	sha, err := h.blobs.Put(data)
	if err != nil {
		h.log.Error("store generated image", "err", err)
		return nil, err
	}
	f := FileRow{ID: proto.NewID(), OwnerSub: t.Owner, SessionID: t.SessionID, Name: name, Mime: mimeType,
		Size: int64(len(data)), SHA256: sha, Origin: chat.OriginGenerated}
	if err := h.store.InsertFile(f); err != nil {
		return nil, err
	}
	p, _ := h.blobs.Path(sha)
	return &chat.Attachment{ID: f.ID, Name: name, Mime: mimeType, SHA256: sha, Size: f.Size, Path: p}, nil
}
