package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/kinginu/mado/internal/proto"
)

// Changing the permission mode of a running session (DESIGN §25.3): the hub stores
// the new mode and asks the live claude to switch through the stream-json control
// protocol (set_permission_mode). When claude cannot be told, the mode applies on the
// next start and the panel offers "Restart to apply".

// modeWait is how long the API waits for the worker's mode_result; the shim itself
// gives claude 5 s.
const modeWait = 8 * time.Second

// modeWaiters are the set_permission_mode requests waiting for their mode_result.
type modeWaiters struct {
	mu sync.Mutex
	m  map[string]chan proto.ModeResult
}

func (w *modeWaiters) add(id string) chan proto.ModeResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = map[string]chan proto.ModeResult{}
	}
	ch := make(chan proto.ModeResult, 1)
	w.m[id] = ch
	return ch
}

func (w *modeWaiters) done(id string, r proto.ModeResult) bool {
	w.mu.Lock()
	ch := w.m[id]
	delete(w.m, id)
	w.mu.Unlock()
	if ch == nil {
		return false
	}
	ch <- r
	return true
}

func (w *modeWaiters) drop(id string) {
	w.mu.Lock()
	delete(w.m, id)
	w.mu.Unlock()
}

// onModeResult delivers a worker's mode_result to the API call waiting for it.
func (h *Hub) onModeResult(wc *workerConn, m *proto.ModeResult) {
	if !h.ownsSession(wc.machineID, m.SessionID) {
		return
	}
	h.modes.done(m.RequestID, *m)
}

// modeAck turns a refused set_permission_mode (an older worker that does not know the
// frame answers with an error ack) into a failed result.
func (h *Hub) modeAck(m *proto.Ack) {
	if !m.OK {
		h.modes.done(m.Ref, proto.ModeResult{RequestID: m.Ref, Error: m.Error})
	}
}

// liveStates: the session's claude process should be there to talk to.
var liveStates = map[string]bool{proto.StateIdle: true, proto.StateRunning: true, proto.StateWaitingApproval: true}

// applyMode asks the session's claude to switch to mode (claude's name) and waits for
// the answer.
func (h *Hub) applyMode(ctx context.Context, s *Session, mode string) error {
	wc := h.worker(s.MachineID)
	if wc == nil {
		return errors.New("machine is offline")
	}
	id := proto.NewID()
	ch := h.modes.add(id)
	defer h.modes.drop(id)
	// The frame id is the request id, so an error ack from an older worker finds it.
	if err := wc.send(ctx, &proto.SetPermissionMode{Header: proto.Header{ID: id}, SessionID: s.ID, RequestID: id, Mode: mode}); err != nil {
		return errors.New("could not reach the machine")
	}
	t := time.NewTimer(modeWait)
	defer t.Stop()
	select {
	case r := <-ch:
		if r.OK {
			return nil
		}
		if r.Error == "" {
			r.Error = "claude did not switch"
		}
		return errors.New(r.Error)
	case <-t.C:
		return errors.New("no answer from the machine")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// apiSetMode is POST /api/sessions/{id}/mode {"mode": "default"|"acceptEdits"|"auto"}
// (DESIGN §25.3). It answers {"applied": true} when the live claude switched,
// {"applied": false, "restart": true} when it could not be told (the mode applies on
// the next start), and {"applied": false, "restart": false} for a stopped session,
// whose Resume starts in the new mode.
func (h *Hub) apiSetMode(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return
	}
	if s.IsChat() || s.Kind == KindDigest {
		apiError(w, http.StatusBadRequest, "only a code session has a permission mode")
		return
	}
	var q struct {
		Mode string `json:"mode"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	if !slices.Contains(permissionModes, q.Mode) {
		apiError(w, http.StatusBadRequest, "mode must be default, acceptEdits or auto")
		return
	}
	old := s.PermissionMode
	if err := h.store.SetSessionMode(s.ID, q.Mode); err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.PermissionMode = q.Mode
	if q.Mode == "auto" {
		s.AutoPolicy = true
	}
	if old != q.Mode {
		h.store.Audit(ws.User.Sub, "session_mode", s.ID+" "+old+" -> "+q.Mode)
	}
	out := map[string]any{"applied": false, "restart": false}
	if liveStates[s.State] {
		prof, _ := h.store.Profile(s.ProfileName)
		err := h.applyMode(r.Context(), s, claudeMode(s, prof))
		if err == nil {
			out["applied"] = true
		} else {
			out["restart"], out["error"] = true, err.Error()
			h.log.Info("permission mode not applied live", "session", s.ID, "mode", q.Mode, "err", err)
		}
	}
	h.publishSession(s)
	out["session"] = h.sessionView(s)
	if h.autoSwitchable(s) {
		out["auto"] = h.autoView(s)
	}
	writeJSON(w, http.StatusOK, out)
}

// publishSession sends the session's view to its open pages (SSE event "session"),
// so the panel follows changes made elsewhere (DESIGN §25.3, §26.1).
func (h *Hub) publishSession(s *Session) {
	j, _ := json.Marshal(h.sessionView(s))
	h.broker.publish(s.ID, sseMsg{event: "session", data: string(j)})
}
