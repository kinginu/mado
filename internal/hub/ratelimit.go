package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/render"
)

// Rate-limit auto-resume (DESIGN §26). Claude Code reports the subscription's usage
// windows as rate_limit_event; a rejection ends the turn. The hub records it, and once
// the window resets it sends the user's messages from the limited time again (or asks
// claude to continue), resuming a stopped session first.

// Clock is the time source of the scheduler; tests inject a fake one.
type Clock interface {
	Now() time.Time
	// AfterFunc runs f in its own goroutine after d; stop cancels it.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

const (
	// resumeDelay is how long after the reported reset the resume fires (plus jitter).
	resumeDelay  = 90 * time.Second
	resumeJitter = 30 * time.Second
	// A reset already past fires 10 to 40 s from now (hub start, late arming).
	resumeSoon = 10 * time.Second
	// resumeRetry is the wait when the session is busy, starting or its machine is
	// offline at the time.
	resumeRetry = time.Minute
	// maxAutoResumes caps consecutive automatic resumes of one session.
	maxAutoResumes = 6
)

// continueText is sent when the rejection came mid-turn (no message of the user's to
// send again).
const continueText = "The usage limit has reset. Continue the work you were doing when the limit was hit; do not repeat finished steps."

// Note names and texts (DESIGN §26.2).
const (
	noteResume     = "rate_limit_resume"
	noteResumeText = "Resumed automatically after the usage limit reset."
	noteGaveUp     = "rate_limit_gave_up"
)

type rateLimits struct {
	clock Clock
	// idleWait and idlePoll: how long, and how often, a resumed session is watched for
	// idle before the text is sent.
	idleWait, idlePoll time.Duration
	jitter             func(time.Duration) time.Duration

	mu       sync.Mutex
	timers   map[string]func() bool // session id -> cancel
	rejected map[string]bool        // a rejection in the session's current turn
	closed   bool
}

func newRateLimits(c Clock) *rateLimits {
	if c == nil {
		c = realClock{}
	}
	return &rateLimits{clock: c, idleWait: 3 * time.Minute, idlePoll: 2 * time.Second,
		jitter: func(d time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(d) + 1)) },
		timers: map[string]func() bool{}, rejected: map[string]bool{}}
}

func (r *rateLimits) cancel(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if stop := r.timers[id]; stop != nil {
		stop()
		delete(r.timers, id)
	}
}

func (r *rateLimits) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for id, stop := range r.timers {
		stop()
		delete(r.timers, id)
	}
}

// armAt schedules f for session id at t (now + 10..40 s when t is past), replacing
// an earlier timer.
func (r *rateLimits) armAt(id string, t time.Time, f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if stop := r.timers[id]; stop != nil {
		stop()
	}
	d := t.Sub(r.clock.Now())
	if d <= 0 {
		d = resumeSoon + r.jitter(resumeJitter)
	}
	r.timers[id] = r.clock.AfterFunc(d, func() {
		r.mu.Lock()
		delete(r.timers, id)
		closed := r.closed
		r.mu.Unlock()
		if !closed {
			f()
		}
	})
}

// armRateLimits arms a timer for every session waiting for an automatic resume (hub
// start).
func (h *Hub) armRateLimits() {
	ids, err := h.store.LimitedSessionIDs()
	if err != nil {
		h.log.Warn("rate limits", "err", err)
		return
	}
	for _, id := range ids {
		if s, err := h.store.Session(id); err == nil {
			h.armResume(s)
		}
	}
}

// armResume schedules the automatic resume of a limited session: 90 s after the
// reset plus 0..30 s of jitter.
func (h *Hub) armResume(s *Session) {
	if s.LimitedUntil <= 0 || !s.AutoResume {
		return
	}
	t := time.Unix(s.LimitedUntil, 0).Add(resumeDelay + h.rl.jitter(resumeJitter))
	id := s.ID
	h.rl.armAt(id, t, func() { h.autoResume(id) })
}

// rateLimitInfo is the part of rate_limit_event the hub reads.
type rateLimitInfo struct {
	Status        string `json:"status"`
	ResetsAt      int64  `json:"resetsAt"`
	RateLimitType string `json:"rateLimitType"`
}

// rateLimitEvent follows a code session's events (DESIGN §26.1): rate_limit_event
// stores the windows and, on a rejection, the limited state; a result without a
// rejection in its turn resets the attempt counter.
func (h *Hub) rateLimitEvent(sessionID string, seq int64, raw []byte) {
	isRL := bytes.Contains(raw, []byte(`"rate_limit_event"`))
	isResult := bytes.Contains(raw, []byte(`"result"`))
	if !isRL && !isResult {
		return
	}
	var e struct {
		Type string          `json:"type"`
		Info json.RawMessage `json:"rate_limit_info"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	switch e.Type {
	case "result":
		h.rl.mu.Lock()
		rejected := h.rl.rejected[sessionID]
		delete(h.rl.rejected, sessionID)
		h.rl.mu.Unlock()
		if !rejected {
			if s, err := h.store.Session(sessionID); err == nil && s.ResumeAttempts > 0 {
				_ = h.store.SetResumeAttempts(sessionID, 0)
			}
		}
		return
	case "rate_limit_event":
	default:
		return
	}
	s, err := h.store.Session(sessionID)
	if err != nil || s.IsChat() || s.Kind == KindDigest || len(e.Info) == 0 {
		return
	}
	// Router (AI&) sessions are never auto-resumed (DESIGN §26.4).
	if p, err := h.store.Profile(s.ProfileName); err != nil || p.Kind != proto.KindSubscription {
		return
	}
	var info rateLimitInfo
	if json.Unmarshal(e.Info, &info) != nil {
		return
	}
	_ = h.store.SetRateLimit(s.ID, string(e.Info))
	s.RateLimit = string(e.Info)
	switch {
	case info.Status == "rejected" && info.ResetsAt > 0:
		h.rl.mu.Lock()
		h.rl.rejected[s.ID] = true
		h.rl.mu.Unlock()
		after := s.LimitedSeq
		if s.LimitedUntil == 0 {
			after = h.rejectionStart(s.ID, seq)
		}
		if err := h.store.SetLimited(s.ID, info.ResetsAt, info.RateLimitType, after); err != nil {
			h.log.Warn("rate limit", "session", s.ID, "err", err)
			return
		}
		s.LimitedUntil, s.LimitedType, s.LimitedSeq = info.ResetsAt, info.RateLimitType, after
		h.store.Audit("", "session_rate_limited", s.ID+" "+info.RateLimitType)
		h.armResume(s)
	case info.Status != "rejected" && s.LimitedUntil > 0:
		// The window has room again (the user's own message went through): nothing to
		// resume any more.
		h.rl.cancel(s.ID)
		_ = h.store.ClearLimited(s.ID)
		s.LimitedUntil, s.LimitedType, s.LimitedSeq = 0, "", 0
	}
	h.publishSession(s)
}

// rejectionStart is the seq the re-sent messages start after: when the rejected turn
// was started by a message of the user's and claude produced nothing before the
// rejection, that message was lost too, so it counts (the seq before its echo);
// otherwise (a rejection mid-turn) the rejection itself.
func (h *Hub) rejectionStart(sessionID string, seq int64) int64 {
	evs, err := h.store.EventsDownFrom(sessionID, seq-1, 200)
	if err != nil {
		return seq
	}
	for _, ev := range evs {
		var e struct {
			Type    string `json:"type"`
			Mado    string `json:"mado"`
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(ev.Raw, &e) != nil {
			continue
		}
		switch {
		case e.Type == "user" && e.Mado == "echo":
			return ev.Seq - 1
		case e.Type == "result", e.Type == "assistant" && e.Message.Model != "<synthetic>", e.Type == "user" && e.Mado == "":
			return seq // claude was at work (an answer, a tool result) or the turn had ended
		}
	}
	return seq
}

// resumeText is what the automatic resume sends: the user's own messages after the
// rejection, verbatim and joined by blank lines, or the continue text.
func (h *Hub) resumeText(s *Session) (text, reason string) {
	evs, err := h.store.EchoesAfter(s.ID, s.LimitedSeq)
	if err != nil {
		return continueText, "continue"
	}
	var parts []string
	for _, ev := range evs {
		for _, b := range render.Blocks(ev.Raw) {
			if b.Kind == render.KindUser && strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	if len(parts) == 0 {
		return continueText, "continue"
	}
	return strings.Join(parts, "\n\n"), "messages"
}

// autoResume runs when a limited session's window has reset (DESIGN §26.2).
func (h *Hub) autoResume(id string) {
	s, err := h.store.Session(id)
	if err != nil || s.LimitedUntil == 0 {
		return
	}
	now := h.rl.clock.Now()
	if now.Before(time.Unix(s.LimitedUntil, 0)) {
		h.armResume(s) // the limit moved later meanwhile
		return
	}
	prof, perr := h.store.Profile(s.ProfileName)
	if !s.AutoResume || s.IsChat() || s.Kind == KindDigest || perr != nil || prof.Kind != proto.KindSubscription {
		h.clearLimited(s)
		return
	}
	retry := func() { h.rl.armAt(id, now.Add(resumeRetry), func() { h.autoResume(id) }) }
	if s.ResumeAttempts >= maxAutoResumes {
		h.clearLimited(s)
		h.sessionNote(s, noteGaveUp, "Automatic resume stopped after 6 attempts in a row; send a message to continue.")
		h.store.Audit("", "session_auto_resume", s.ID+" gave_up")
		return
	}
	switch {
	case s.State == proto.StateIdle:
		if !h.online(s.MachineID) {
			retry()
			return
		}
		text, reason := h.resumeText(s)
		ctx, cancel := context.WithTimeout(context.Background(), codeToolTimeout)
		err := h.sendToSession(ctx, s, &proto.UserMessage{SessionID: s.ID, Text: text, Note: noteResume, NoteText: noteResumeText})
		cancel()
		if err != nil {
			retry()
			return
		}
		h.clearLimited(s)
		_ = h.store.SetResumeAttempts(s.ID, s.ResumeAttempts+1)
		h.store.Audit("", "session_auto_resume", s.ID+" "+reason)
	case s.State == proto.StateRunning || s.State == proto.StateWaitingApproval || s.State == proto.StateStarting:
		retry() // a turn runs (the user may have continued by hand) or claude starts
	case canResume(s) && h.liveConversation(s) == "":
		if !h.online(s.MachineID) {
			retry()
			return
		}
		text, reason := h.resumeText(s)
		h.clearLimited(s)
		nid, _, err := h.resumeSession(context.Background(), &WebSession{User: User{Sub: s.OwnerSub}}, s)
		if err != nil {
			h.sessionNote(s, noteGaveUp, "Automatic resume after the usage limit failed: "+userMessage(err, "could not resume")+".")
			return
		}
		_ = h.store.SetResumeAttempts(nid, s.ResumeAttempts+1)
		h.store.Audit("", "session_auto_resume", s.ID+" "+reason+" -> "+nid)
		go h.sendWhenIdle(s, nid, text)
	default:
		// Ended for good, continued elsewhere, or the conversation already runs in
		// another session: nothing to resume here.
		h.clearLimited(s)
	}
}

// sendWhenIdle waits for a resumed session to report idle, then sends the text; it
// gives up after idleWait with a note on the old session.
func (h *Hub) sendWhenIdle(old *Session, newID, text string) {
	deadline := time.Now().Add(h.rl.idleWait)
	for time.Now().Before(deadline) {
		s, err := h.store.Session(newID)
		if err != nil {
			break
		}
		if s.State == proto.StateIdle {
			ctx, cancel := context.WithTimeout(context.Background(), codeToolTimeout)
			err := h.sendToSession(ctx, s, &proto.UserMessage{SessionID: s.ID, Text: text, Note: noteResume, NoteText: noteResumeText})
			cancel()
			if err == nil {
				return
			}
			break
		}
		if s.State == proto.StateExited || s.State == proto.StateDetached {
			break
		}
		time.Sleep(h.rl.idlePoll)
	}
	h.sessionNote(old, noteGaveUp, "Automatic resume after the usage limit: the resumed session did not become ready; send the message again there.")
}

func (h *Hub) clearLimited(s *Session) {
	h.rl.cancel(s.ID)
	_ = h.store.ClearLimited(s.ID)
	s.LimitedUntil, s.LimitedType, s.LimitedSeq = 0, "", 0
	h.publishSession(s)
}

// sessionNote leaves a mado note in a session's transcript: through the shim while it
// runs (so seqs stay the shim's), else stored by the hub after the last event.
func (h *Hub) sessionNote(s *Session, note, text string) {
	if liveStates[s.State] && h.online(s.MachineID) {
		ctx, cancel := context.WithTimeout(context.Background(), codeToolTimeout)
		err := h.sendToSession(ctx, s, &proto.AppendNote{SessionID: s.ID, Note: note, Text: text})
		cancel()
		if err == nil {
			return
		}
	}
	raw, _ := json.Marshal(map[string]any{"type": "user", "mado": "note", "mado_note": note,
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}})
	seq := h.store.MaxSeq(s.ID) + 1
	if ok, err := h.store.InsertEvent(s.ID, seq, raw); err == nil && ok {
		h.publishEvent(s.ID, seq, raw)
	}
}

// apiAutoResume is POST /api/sessions/{id}/auto_resume {"on": bool} (DESIGN §26.2).
func (h *Hub) apiAutoResume(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return
	}
	if s.IsChat() || s.Kind == KindDigest {
		apiError(w, http.StatusBadRequest, "only a code session resumes after a usage limit")
		return
	}
	var q struct {
		On *bool `json:"on"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	if q.On == nil {
		apiError(w, http.StatusBadRequest, `"on" is required`)
		return
	}
	if err := h.store.SetAutoResume(s.ID, *q.On); err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.AutoResume = *q.On
	if *q.On {
		h.armResume(s)
	} else {
		h.rl.cancel(s.ID)
	}
	h.store.Audit(ws.User.Sub, "session_auto_resume_switch", s.ID+" "+map[bool]string{true: "on", false: "off"}[*q.On])
	h.publishSession(s)
	writeJSON(w, http.StatusOK, map[string]any{"session": h.sessionView(s)})
}
