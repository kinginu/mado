package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sseMsg is one SSE event; data is JSON (DESIGN §17.5).
type sseMsg struct {
	event, id, data string
}

type broker struct {
	mu   sync.Mutex
	subs map[string]map[chan sseMsg]bool
}

func newBroker() *broker { return &broker{subs: map[string]map[chan sseMsg]bool{}} }

func (b *broker) subscribe(session string) chan sseMsg {
	ch := make(chan sseMsg, 256)
	b.mu.Lock()
	if b.subs[session] == nil {
		b.subs[session] = map[chan sseMsg]bool{}
	}
	b.subs[session][ch] = true
	b.mu.Unlock()
	return ch
}

func (b *broker) unsubscribe(session string, ch chan sseMsg) {
	b.mu.Lock()
	delete(b.subs[session], ch)
	if len(b.subs[session]) == 0 {
		delete(b.subs, session)
	}
	b.mu.Unlock()
}

// publish never blocks; a subscriber that falls behind is dropped and reconnects.
func (b *broker) publish(session string, m sseMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[session] {
		select {
		case ch <- m:
		default:
			delete(b.subs[session], ch)
			close(ch)
		}
	}
}

func writeSSE(w http.ResponseWriter, m sseMsg) error {
	var sb strings.Builder
	if m.event != "" {
		fmt.Fprintf(&sb, "event: %s\n", m.event)
	}
	if m.id != "" {
		fmt.Fprintf(&sb, "id: %s\n", m.id)
	}
	for _, l := range strings.Split(m.data, "\n") {
		fmt.Fprintf(&sb, "data: %s\n", strings.TrimSuffix(l, "\r"))
	}
	sb.WriteString("\n")
	_, err := w.Write([]byte(sb.String()))
	return err
}

// handleSSE streams blocks after ?after=<seq> (or Last-Event-ID), then live ones.
// Every event is JSON: `block` carries the block list of one stored event (the shape of
// /api/sessions/{id}/blocks, id = seq), `approval`, `state`, `models` and `delta`. The
// HTML mode of the htmx page is gone with M6; ?format=json is accepted and ignored.
func (h *Hub) handleSSE(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.ownSession(w, r, ws)
	if s == nil {
		return
	}
	approvalMsg := func(a Approval) sseMsg {
		b, _ := json.Marshal(h.approvalJSON(a))
		return sseMsg{event: "approval", data: string(b)}
	}
	var after int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	} else if v := r.URL.Query().Get("after"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)

	ch := h.broker.subscribe(s.ID)
	defer h.broker.unsubscribe(s.ID, ch)
	// Pending approvals first: one may have arrived between page render and this
	// connect (its live publish then had no subscriber). The SPA keys cards by
	// request id, so a card it already shows is replaced, not doubled.
	if pending, err := h.store.PendingApprovals(s.ID); err == nil {
		for _, a := range pending {
			if writeSSE(w, approvalMsg(a)) != nil {
				return
			}
		}
	}
	// Catch up from the store after subscribing, so nothing falls in the gap.
	for {
		evs, err := h.store.Events(s.ID, after, 500)
		if err != nil {
			return
		}
		for _, e := range evs {
			if data := h.eventJSON(e.Seq, e.Raw); data != "" {
				if writeSSE(w, sseMsg{event: "block", id: strconv.FormatInt(e.Seq, 10), data: data}) != nil {
					return
				}
			}
			after = e.Seq
		}
		if len(evs) < 500 {
			break
		}
	}
	_ = rc.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
		case m, ok := <-ch:
			if !ok {
				return
			}
			if m.id != "" {
				if seq, _ := strconv.ParseInt(m.id, 10, 64); seq <= after {
					continue // already sent during catch-up
				}
			}
			if writeSSE(w, m) != nil {
				return
			}
		}
		_ = rc.Flush()
	}
}
