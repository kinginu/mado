package hub

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kinginu/mado/internal/chat"
	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/render"
)

// The JSON API for the SPA (DESIGN §17.5). Cookie auth; every non-GET request needs a
// same-origin Origin header and the X-CSRF-Token header (its value is in GET /api/me).

func (h *Hub) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me", h.requireAPI(h.apiMe))
	mux.HandleFunc("GET /api/sessions", h.requireAPI(h.apiSessions))
	mux.HandleFunc("POST /api/sessions", h.requireAPI(h.apiCreateSession))
	mux.HandleFunc("GET /api/sessions/{id}", h.requireAPI(h.apiSession))
	mux.HandleFunc("PATCH /api/sessions/{id}", h.requireAPI(h.apiPatchSession))
	mux.HandleFunc("DELETE /api/sessions/{id}", h.requireAPI(h.apiDeleteSession))
	mux.HandleFunc("GET /api/sessions/{id}/blocks", h.requireAPI(h.apiBlocks))
	mux.HandleFunc("POST /api/sessions/{id}/send", h.requireAPI(h.apiSend))
	mux.HandleFunc("POST /api/sessions/{id}/approve", h.requireAPI(h.apiApprove))
	mux.HandleFunc("POST /api/sessions/{id}/interrupt", h.requireAPI(h.apiInterrupt))
	mux.HandleFunc("POST /api/sessions/{id}/stop", h.requireAPI(h.apiStop))
	mux.HandleFunc("POST /api/sessions/{id}/resume", h.requireAPI(h.apiResume))
	mux.HandleFunc("POST /api/sessions/{id}/plans/{plan_id}", h.requireAPI(h.apiPlanDecision))
	mux.HandleFunc("GET /api/machines", h.requireAPI(h.apiMachines))
	mux.HandleFunc("DELETE /api/machines/{name}", h.requireAdminAPI(h.apiRemoveMachine))
	mux.HandleFunc("GET /api/profiles", h.requireAPI(h.apiProfiles))
	mux.HandleFunc("GET /api/chat/models", h.requireAPI(h.apiChatModels))
	mux.HandleFunc("POST /api/files", h.requireAPI(h.apiUpload))
	mux.HandleFunc("DELETE /api/files/{id}", h.requireAPI(h.apiDeleteFile))
	mux.HandleFunc("GET /files/{id}", h.requireUser(h.handleFile))
	mux.HandleFunc("GET /api/usage", h.requireAPI(h.apiUsage))
	mux.HandleFunc("GET /api/usage.csv", h.requireAPI(h.apiUsageCSV))
	mux.HandleFunc("GET /api/admin/users", h.requireAdminAPI(h.apiAdminUsers))
	mux.HandleFunc("PATCH /api/admin/users/{sub}", h.requireAdminAPI(h.apiAdminPatchUser))
	mux.HandleFunc("GET /api/audit", h.requireAPI(h.apiAudit))
	mux.HandleFunc("GET /api/admin/profiles", h.requireAdminAPI(h.apiAdminProfiles))
	mux.HandleFunc("POST /api/admin/profiles", h.requireAdminAPI(h.apiAdminSaveProfile))
	mux.HandleFunc("POST /api/admin/chat-models", h.requireAdminAPI(h.apiAdminSaveChatModel))
	// Overrides of hub.toml entries (DESIGN §28). Price model ids contain '/', so
	// prices take the model as a query parameter.
	mux.HandleFunc("DELETE /api/admin/profiles/{name}/override", h.requireAdminAPI(h.apiAdminRevertProfile))
	mux.HandleFunc("DELETE /api/admin/chat-models/{name}/override", h.requireAdminAPI(h.apiAdminRevertChatModel))
	mux.HandleFunc("POST /api/admin/prices", h.requireAdminAPI(h.apiAdminSavePrice))
	mux.HandleFunc("DELETE /api/admin/prices/override", h.requireAdminAPI(h.apiAdminRevertPrice))
	mux.HandleFunc("DELETE /api/admin/prices", h.requireAdminAPI(h.apiAdminDeletePrice))
	mux.HandleFunc("POST /api/admin/settings", h.requireAdminAPI(h.apiAdminSaveSetting))
	mux.HandleFunc("DELETE /api/admin/settings/{key}/override", h.requireAdminAPI(h.apiAdminRevertSetting))
	mux.HandleFunc("GET /api/admin/upstream-models", h.requireAdminAPI(h.apiAdminUpstreamModels))
	mux.HandleFunc("GET /api/admin/history", h.requireAdminAPI(h.apiHistory))
	mux.HandleFunc("GET /api/admin/history/{id}", h.requireAdminAPI(h.apiHistoryFile))
	mux.HandleFunc("POST /api/admin/history/{id}/resume", h.requireAdminAPI(h.apiHistoryResume))
	mux.HandleFunc("POST /api/admin/history/rescan", h.requireAdminAPI(h.apiHistoryRescan))
	mux.HandleFunc("POST /api/admin/enroll", h.requireAdminAPI(h.apiEnroll))
	h.inboxRoutes(mux)
	h.fileRoutes(mux) // the file viewer (DESIGN §27)
	// M22 and M23 (DESIGN §25.3, §26.2).
	mux.HandleFunc("POST /api/sessions/{id}/mode", h.requireAPI(h.apiSetMode))
	mux.HandleFunc("POST /api/sessions/{id}/auto_resume", h.requireAPI(h.apiAutoResume))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// userMessage is the text of a userError (or a chat refusal); anything else is a
// generic message so internal errors are not shown.
func userMessage(err error, fallback string) string {
	var ue userError
	var ce chat.UserError
	var be budgetError
	switch {
	case errors.As(err, &ue):
		return string(ue)
	case errors.As(err, &be):
		return string(be)
	case errors.As(err, &ce):
		return string(ce)
	}
	return fallback
}

// requireAPI is requireUser for JSON: 401 instead of a redirect, and the Origin and
// X-CSRF-Token checks on every non-GET request.
func (h *Hub) requireAPI(next func(http.ResponseWriter, *http.Request, *WebSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws := h.webSession(r)
		if ws == nil {
			apiError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !h.sameOrigin(r.Header.Get("Origin")) {
				apiError(w, http.StatusForbidden, "bad origin")
				return
			}
			tok := r.Header.Get("X-CSRF-Token")
			if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(ws.CSRF)) != 1 {
				apiError(w, http.StatusForbidden, "bad csrf token")
				return
			}
		}
		next(w, r, ws)
	}
}

// sameOrigin requires an Origin equal to base_url's scheme and host.
func (h *Hub) sameOrigin(o string) bool {
	if o == "" {
		return false
	}
	ou, err := url.Parse(o)
	bu, _ := url.Parse(h.cfg.BaseURL)
	return err == nil && strings.EqualFold(ou.Host, bu.Host) && ou.Scheme == bu.Scheme
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// apiOwnSession is ownSession with JSON errors.
func (h *Hub) apiOwnSession(w http.ResponseWriter, r *http.Request, ws *WebSession) *Session {
	id := r.PathValue("id")
	if !proto.ValidSessionID(id) {
		apiError(w, http.StatusNotFound, "no such session")
		return nil
	}
	s, err := h.store.Session(id)
	if err != nil || s.OwnerSub != ws.User.Sub {
		apiError(w, http.StatusNotFound, "no such session")
		return nil
	}
	return s
}

func (h *Hub) apiMe(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	writeJSON(w, http.StatusOK, map[string]any{"sub": ws.User.Sub, "email": ws.User.Email, "name": ws.User.Name,
		"is_admin": ws.User.IsAdmin, "csrf": ws.CSRF, "chat_enabled": h.cfg.Chat.Enabled, "currency": h.cfg.Budgets.Currency,
		"concierge": h.cfg.Chat.Enabled && h.conciergeModel() != "",
		"inbox":     h.inbox != nil,
		"budget":    h.userBudgetState(ws.User.Sub)})
}

type apiSessionView struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Title          string `json:"title"`
	State          string `json:"state"`
	LastError      string `json:"last_error,omitempty"`
	CreatedAt      string `json:"created_at"`
	Machine        string `json:"machine,omitempty"`
	MachineID      string `json:"machine_id,omitempty"`
	Online         bool   `json:"online"`
	CWD            string `json:"cwd,omitempty"`
	Profile        string `json:"profile,omitempty"`
	MainModel      string `json:"main_model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	// MadoAuto: permission mode auto on a router profile, run by mado's policy (§18).
	// auto on a subscription profile is Claude Code's own auto mode (§25.1).
	MadoAuto bool   `json:"mado_auto,omitempty"`
	Model    string `json:"model,omitempty"`
	System   string `json:"system,omitempty"`
	Running  bool   `json:"running"`
	Models   string `json:"models,omitempty"` // code: the session list's model column
	// Concierge: a chat session run as mado's concierge (DESIGN §20).
	Concierge bool `json:"concierge,omitempty"`
	// Resumable: a stopped code session with a claude conversation id (POST .../resume).
	Resumable bool `json:"resumable"`
	// ResumedBy and ResumedFrom link a resume chain (DESIGN §21.6).
	ResumedBy   string `json:"resumed_by,omitempty"`
	ResumedFrom string `json:"resumed_from,omitempty"`
	// ProfileKind (code): router or subscription, for the mode labels (DESIGN §25.4).
	ProfileKind string `json:"profile_kind,omitempty"`
	// Rate limits (code, DESIGN §26): the latest rate_limit_info as Claude Code sent
	// it; while limited the reset (unix seconds) and window; the auto-resume switch.
	RateLimit    json.RawMessage `json:"rate_limit,omitempty"`
	LimitedUntil int64           `json:"limited_until"` // always sent: the SPA merges views
	LimitedType  string          `json:"limited_type"`
	AutoResume   bool            `json:"auto_resume"`
}

func (h *Hub) sessionView(s *Session) apiSessionView {
	v := apiSessionView{ID: s.ID, Kind: s.Kind, Title: s.Title, State: s.State, LastError: s.LastError, CreatedAt: s.CreatedAt,
		ResumedBy: s.ResumedBy, ResumedFrom: s.ResumedFrom}
	if s.IsChat() {
		v.Model, v.System, v.Running, v.Online, v.Concierge = s.Model, s.System, h.runner.Running(s.ID), true, s.Concierge
		return v
	}
	v.Machine, v.MachineID, v.Online, v.CWD, v.Profile, v.MainModel, v.PermissionMode =
		s.MachineName, s.MachineID, h.online(s.MachineID), s.CWD, s.ProfileName, s.MainModel, s.PermissionMode
	v.Running = s.State == proto.StateRunning || s.State == proto.StateWaitingApproval
	v.Models = h.sessionModels(s.ID).Short()
	v.MadoAuto = h.madoAuto(s)
	v.Resumable = canResume(s)
	if p, err := h.store.Profile(s.ProfileName); err == nil {
		v.ProfileKind = p.Kind
	}
	if s.RateLimit != "" && json.Valid([]byte(s.RateLimit)) {
		v.RateLimit = json.RawMessage(s.RateLimit)
	}
	v.LimitedUntil, v.LimitedType, v.AutoResume = s.LimitedUntil, s.LimitedType, s.AutoResume
	return v
}

func (h *Hub) apiSessions(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	ss, err := h.store.SessionsByOwner(ws.User.Sub)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	all := r.URL.Query().Get("all") == "1"
	out := make([]apiSessionView, 0, len(ss))
	for _, s := range ss {
		if s.ResumedBy != "" && !all {
			continue // continued by a newer session in the chain (§21.6)
		}
		out = append(out, h.sessionView(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (h *Hub) apiCreateSession(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	var q struct {
		Kind           string `json:"kind"`
		Machine        string `json:"machine"` // id or name
		CWD            string `json:"cwd"`
		Profile        string `json:"profile"`
		MainModel      string `json:"main_model"`
		PermissionMode string `json:"permission_mode"`
		Model          string `json:"model"`
		System         string `json:"system"`
		Title          string `json:"title"`
		Concierge      bool   `json:"concierge"` // with kind chat: mado's concierge (DESIGN §20)
	}
	if !decodeBody(w, r, &q) {
		return
	}
	var id string
	var err error
	switch q.Kind {
	case KindChat:
		if q.Concierge {
			id, err = h.newConciergeSession(ws, q.Title)
		} else {
			id, err = h.newChatSession(ws, q.Model, q.System, q.Title)
		}
	case KindCode, "":
		req := newSessionReq{Project: q.CWD, Profile: q.Profile, Model: q.MainModel, Mode: q.PermissionMode, Title: q.Title}
		if _, merr := h.store.Machine(q.Machine); merr == nil {
			req.MachineID = q.Machine
		} else {
			req.MachineName = q.Machine
		}
		if req.Profile == "" {
			req.Profile = "subscription"
		}
		id, err = h.newSession(r.Context(), ws, req)
	default:
		err = userError(`kind must be "code" or "chat"`)
	}
	if err != nil {
		apiError(w, errStatus(err, http.StatusBadRequest), userMessage(err, "could not create session"))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (h *Hub) apiSession(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return
	}
	out := map[string]any{"session": h.sessionView(s), "usage": h.store.SessionUsage(s.ID)}
	if s.IsChat() {
		out["month_usage"] = h.store.MonthUsage(ws.User.Sub, monthOf(time.Now()))
		fs, _ := h.store.ReferencedSessionFiles(s.ID)
		list := make([]map[string]any, 0, len(fs))
		for _, f := range fs {
			list = append(list, fileJSON(&f))
		}
		out["files"] = list
		out["tools"] = h.chatTools()
		if s.Concierge {
			names := []string{toolListMachines, toolListProjects, toolInspectDir, toolListProfiles, toolPropose, toolSessionStatus}
			if h.inbox != nil {
				names = append(names, inbox.ChatToolNames...)
			}
			out["tools"] = names
			out["spawned"] = h.spawnedViews(s)
		}
		if s.Kind == KindDigest {
			out["tools"] = []string{inbox.ToolProposeTask}
		}
	} else {
		out["models"] = h.sessionModels(s.ID)
		if s.Kind == KindDigest {
			out["tools"] = []string{inbox.ToolProposeTask} // a claude digest job (DESIGN §23.6)
		}
		pending, _ := h.store.PendingApprovals(s.ID)
		out["pending_approvals"] = len(pending)
		if h.autoSwitchable(s) {
			out["auto"] = h.autoView(s)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Hub) apiPatchSession(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return
	}
	var q struct {
		Title *string `json:"title"`
		Model *string `json:"model"` // chat only: the composer's model picker
		Auto  *bool   `json:"auto"`  // router code sessions only: the policy switch (§18.1, §18.6)
	}
	if !decodeBody(w, r, &q) {
		return
	}
	if q.Auto != nil {
		if !h.autoSwitchable(s) {
			apiError(w, http.StatusBadRequest, "only a router-profile code session has the auto switch")
			return
		}
		var err error
		if *q.Auto && s.PermissionMode != "auto" {
			// Mid-session switch (§18.6): the running claude keeps its mode; a resume
			// starts it in acceptEdits through claudeMode.
			err = h.store.EnableSessionAuto(s.ID)
			s.PermissionMode = "auto"
		} else {
			err = h.store.SetSessionAuto(s.ID, *q.Auto)
		}
		if err != nil {
			apiError(w, http.StatusInternalServerError, "internal error")
			return
		}
		h.store.Audit(ws.User.Sub, "session_auto", s.ID+" "+strconv.FormatBool(*q.Auto))
		s.AutoPolicy = *q.Auto
	}
	if q.Model != nil && *q.Model != s.Model {
		if !s.IsChat() {
			apiError(w, http.StatusBadRequest, "only a chat session can switch models")
			return
		}
		if !slices.ContainsFunc(h.chatModels(), func(m chat.Model) bool { return m.Name == *q.Model }) {
			apiError(w, http.StatusBadRequest, "unknown chat model")
			return
		}
		if err := h.store.SetSessionModel(s.ID, *q.Model); err != nil {
			apiError(w, http.StatusInternalServerError, "internal error")
			return
		}
		h.store.Audit(ws.User.Sub, "session_model", s.ID+" "+*q.Model)
		s.Model = *q.Model
	}
	if q.Title != nil {
		t := strings.TrimSpace(*q.Title)
		if utf8.RuneCountInString(t) > 120 || !utf8.ValidString(t) {
			apiError(w, http.StatusBadRequest, "title too long")
			return
		}
		if err := h.store.SetSessionTitle(s.ID, t); err != nil {
			apiError(w, http.StatusInternalServerError, "internal error")
			return
		}
		s.Title = t
	}
	out := map[string]any{"session": h.sessionView(s)}
	if h.autoSwitchable(s) {
		out["auto"] = h.autoView(s)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Hub) apiDeleteSession(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return
	}
	if s.IsChat() {
		h.runner.Interrupt(s.ID)
		h.runner.Wait(s.ID)
	} else if s.State != proto.StateExited && s.State != proto.StateDetached {
		apiError(w, http.StatusConflict, "stop the session first")
		return
	}
	orphans, err := h.store.DeleteSession(s.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, sha := range orphans {
		if err := h.blobs.Remove(sha); err != nil {
			h.log.Warn("remove blob", "err", err)
		}
	}
	h.models.mu.Lock()
	delete(h.models.accs, s.ID)
	h.models.mu.Unlock()
	h.store.Audit(ws.User.Sub, "session_delete", s.ID)
	w.WriteHeader(http.StatusNoContent)
}

// apiBlock is one rendered block (DESIGN §17.5). html is set for user and assistant
// text (goldmark output, sanitized); the client escapes every other field.
type apiBlock struct {
	Seq      int64  `json:"seq"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	HTML     string `json:"html,omitempty"`
	Text     string `json:"text"`
	Detail   string `json:"detail,omitempty"`
	IsError  bool   `json:"is_error"`
	Subagent bool   `json:"subagent"`
	Parent   string `json:"parent,omitempty"`
	ToolID   string `json:"tool_id,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Model    string `json:"model,omitempty"`
	Noise    bool   `json:"noise"`
	Delta    int    `json:"delta,omitempty"`   // thinking_tokens
	FileID   string `json:"file_id,omitempty"` // file
	Mime     string `json:"mime,omitempty"`    // file
	Origin   string `json:"origin,omitempty"`  // file: "generated" for a tool's image
	PlanID   string `json:"plan_id,omitempty"` // plan: the concierge's plan card (DESIGN §20.3)
	Plan     any    `json:"plan,omitempty"`    // plan: its current state
}

func blocksJSON(seq int64, raw []byte) []apiBlock {
	bs := render.Blocks(raw)
	out := make([]apiBlock, 0, len(bs))
	for _, b := range bs {
		a := apiBlock{Seq: seq, Kind: b.Kind, Label: b.Label, Text: b.Text, Detail: b.Detail, IsError: b.IsError,
			Subagent: b.Subagent, Parent: b.Parent, ToolID: b.ToolID, Agent: b.Agent, Model: b.Model, Noise: b.Noise,
			Delta: b.Delta, FileID: b.FileID, Mime: b.Mime, Origin: b.Origin, PlanID: b.PlanID}
		if b.Kind == render.KindUser || b.Kind == render.KindAssistant || b.Kind == render.KindNote {
			a.HTML = string(render.Markdown(b.Text))
		}
		out = append(out, a)
	}
	return out
}

// eventJSON is the SSE form: the JSON list of one event's blocks, "" for none.
func (h *Hub) eventJSON(seq int64, raw []byte) string {
	bs := h.fillPlans(blocksJSON(seq, raw))
	if len(bs) == 0 {
		return ""
	}
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(bs)
	return strings.TrimSpace(buf.String())
}

type apiApproval struct {
	RequestID string `json:"request_id"`
	ToolName  string `json:"tool_name"`
	Summary   string `json:"summary"`
	Detail    string `json:"detail,omitempty"`
	State     string `json:"state"`
	Decision  string `json:"decision,omitempty"`
	// Auto mode (DESIGN §18.3): DecidedBy is "rule", "rule:<name>" or "classifier" for
	// an automatic decision (never a user's sub); Auto the policy outcome (allow, deny,
	// ask); Reason its explanation; AfterSeq where the card goes in the transcript.
	DecidedBy string `json:"decided_by,omitempty"`
	Auto      string `json:"auto,omitempty"`
	Reason    string `json:"reason,omitempty"`
	AfterSeq  int64  `json:"after_seq"`
	// AskUserQuestion (DESIGN §21.2): the questions of the input for the question
	// card, and once answered the answers and notes by question text.
	Questions json.RawMessage   `json:"questions,omitempty"`
	Answers   map[string]string `json:"answers,omitempty"`
	Notes     map[string]string `json:"notes,omitempty"`
}

func (h *Hub) approvalJSON(a Approval) apiApproval {
	v := apiApproval{RequestID: a.RequestID, ToolName: a.ToolName, Summary: render.ToolInputSummary(a.ToolName, json.RawMessage(a.Input)),
		State: a.State, Decision: a.Decision, Auto: a.Auto, Reason: a.Reason, AfterSeq: a.AfterSeq}
	if a.DecidedBy == "rule" || a.DecidedBy == "classifier" || strings.HasPrefix(a.DecidedBy, "rule:") {
		v.DecidedBy = a.DecidedBy
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(a.Input), "", "  ") == nil {
		v.Detail = render.Abbrev(pretty.String(), 8000)
	}
	if a.ToolName == ToolAskUserQuestion {
		if qs, err := askQuestions(a.Input); err == nil {
			v.Questions, _ = json.Marshal(qs)
		}
		var sa storedAnswers
		if a.Answers != "" && json.Unmarshal([]byte(a.Answers), &sa) == nil {
			v.Answers, v.Notes = sa.Answers, sa.Notes
		}
	}
	return v
}

const blocksPage = 1000

func (h *Hub) apiBlocks(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return
	}
	q := r.URL.Query()
	approvals := []apiApproval{}
	if pending, err := h.store.TranscriptApprovals(s.ID); err == nil {
		for _, a := range pending {
			approvals = append(approvals, h.approvalJSON(a))
		}
	}
	// ?before=<seq> (DESIGN §17.14): the newest page before seq, empty or 0 for the end.
	if q.Has("before") {
		before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
		evs, moreBefore, err := h.store.EventsBefore(s.ID, before, blocksPage)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "internal error")
			return
		}
		blocks := []apiBlock{}
		var first, last int64
		for _, e := range evs {
			blocks = append(blocks, h.fillPlans(blocksJSON(e.Seq, e.Raw))...)
		}
		if len(evs) > 0 {
			first, last = evs[0].Seq, evs[len(evs)-1].Seq
		}
		writeJSON(w, http.StatusOK, map[string]any{"blocks": blocks, "first": first, "last": last, "more_before": moreBefore,
			"approvals": approvals})
		return
	}
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	evs, err := h.store.Events(s.ID, max(after, 0), blocksPage)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	blocks := []apiBlock{}
	last := after
	for _, e := range evs {
		blocks = append(blocks, h.fillPlans(blocksJSON(e.Seq, e.Raw))...)
		last = e.Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocks": blocks, "last": last, "more": len(evs) == blocksPage,
		"approvals": approvals})
}

func (h *Hub) apiSend(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return
	}
	var q struct {
		Text    string   `json:"text"`
		FileIDs []string `json:"file_ids"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	// `/new plan [first message]` starts a concierge session from any session (DESIGN §20.1).
	if rest, ok := h.isPlanCommand(q.Text); ok && len(q.FileIDs) == 0 {
		id, err := h.startConcierge(ws, rest)
		if err != nil && id == "" {
			apiError(w, errStatus(err, http.StatusBadRequest), userMessage(err, "could not start the concierge"))
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
		return
	}
	if s.Kind == KindDigest {
		apiError(w, http.StatusBadRequest, "a digest session takes no messages")
		return
	}
	if s.IsChat() {
		seq, err := h.chatSend(ws, s, q.Text, q.FileIDs)
		switch {
		case errors.Is(err, chat.ErrBusy):
			apiError(w, http.StatusConflict, err.Error())
		case err != nil:
			apiError(w, errStatus(err, http.StatusBadRequest), userMessage(err, "could not send"))
		default:
			writeJSON(w, http.StatusAccepted, map[string]any{"seq": seq})
		}
		return
	}
	text := strings.TrimSpace(q.Text)
	if text == "" || len(text) > 200_000 || !utf8.ValidString(text) {
		apiError(w, http.StatusBadRequest, "empty or invalid message")
		return
	}
	if len(q.FileIDs) > 0 {
		apiError(w, http.StatusBadRequest, "code sessions take no attachments")
		return
	}
	if req, isCmd, err := parseNewCommand(text); isCmd {
		id := ""
		if err == nil {
			id, err = h.newSession(r.Context(), ws, req)
		}
		if err != nil {
			apiError(w, errStatus(err, http.StatusBadRequest), userMessage(err, "command failed"))
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
		return
	}
	if err := h.checkProfileBudget(ws.User.Sub, s.ProfileName); err != nil {
		apiError(w, http.StatusPaymentRequired, err.Error())
		return
	}
	if err := h.sendToSession(r.Context(), s, &proto.UserMessage{SessionID: s.ID, Text: text}); err != nil {
		apiError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	h.store.Audit(ws.User.Sub, "message", s.ID+" len="+strconv.Itoa(len(text)))
	writeJSON(w, http.StatusAccepted, map[string]any{})
}

func (h *Hub) apiApprove(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return
	}
	var q struct {
		RequestID string `json:"request_id"`
		Decision  string `json:"decision"`
		// AskUserQuestion only (DESIGN §21.1): {question: answer} and {question: notes}.
		Answers map[string]string `json:"answers"`
		Notes   map[string]string `json:"notes"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	a, status, err := h.decideApproval(r, ws, s, q.RequestID, q.Decision, q.Answers, q.Notes)
	if err != nil {
		apiError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h.approvalJSON(*a))
}

func (h *Hub) apiInterrupt(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return
	}
	if s.IsChat() {
		ok := h.runner.Interrupt(s.ID)
		if ok {
			h.store.Audit(ws.User.Sub, "interrupt", s.ID)
		}
		writeJSON(w, http.StatusOK, map[string]bool{"interrupted": ok})
		return
	}
	if err := h.sendToSession(r.Context(), s, &proto.Interrupt{SessionID: s.ID}); err != nil {
		apiError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	h.store.Audit(ws.User.Sub, "interrupt", s.ID)
	writeJSON(w, http.StatusOK, map[string]bool{"interrupted": true})
}

func (h *Hub) apiStop(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return
	}
	if s.IsChat() {
		apiError(w, http.StatusBadRequest, "a chat session has no process to stop; interrupt or delete it")
		return
	}
	if err := h.sendToSession(r.Context(), s, &proto.StopSession{SessionID: s.ID}); err != nil {
		apiError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	h.store.Audit(ws.User.Sub, "session_stop", s.ID)
	writeJSON(w, http.StatusOK, map[string]any{})
}

// canResume: a stopped code session that still knows its claude conversation and has
// not been continued already (the newest session of a chain is the one to resume).
func canResume(s *Session) bool {
	return !s.IsChat() && s.Kind != KindDigest && s.ClaudeSessionID != "" && s.ResumedBy == "" && (s.State == proto.StateExited || s.State == proto.StateDetached)
}

// apiResume continues a stopped code session's conversation in a new session (new shim,
// current profile settings): `claude --resume <id>` with the old machine, cwd, profile,
// model, mode and title (DESIGN §21.6).
func (h *Hub) apiResume(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return
	}
	if s.IsChat() {
		apiError(w, http.StatusBadRequest, "a chat session cannot be resumed")
		return
	}
	if s.Kind == KindDigest {
		apiError(w, http.StatusBadRequest, "a digest job cannot be resumed")
		return
	}
	id, conflict, err := h.resumeSession(r.Context(), ws, s)
	switch {
	case conflict != "":
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "id": conflict})
	case err != nil:
		apiError(w, errStatus(err, http.StatusBadRequest), userMessage(err, "could not resume"))
	default:
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
	}
}

// resumeSession is the Resume of a stopped code session (DESIGN §21.6), for the API
// and the rate-limit scheduler (§26.2). conflict names the session the conversation
// already continues or runs in.
func (h *Hub) resumeSession(ctx context.Context, ws *WebSession, s *Session) (id, conflict string, err error) {
	if s.ResumedBy != "" {
		// A stale page: the conversation already continues in a newer session.
		return "", s.ResumedBy, userError("this session continues in a newer session")
	}
	if !canResume(s) {
		return "", "", userError("only a stopped session with a conversation id can be resumed")
	}
	if o := h.liveConversation(s); o != "" {
		return "", o, userError("this conversation is already running in another session")
	}
	id, err = h.newSession(ctx, ws, newSessionReq{MachineID: s.MachineID, Project: s.CWD, Profile: s.ProfileName,
		Model: s.MainModel, Mode: s.PermissionMode, Title: s.Title, ResumeClaudeSessionID: s.ClaudeSessionID, ResumedFrom: s.ID})
	if err != nil {
		return "", "", err
	}
	if err := h.store.SetResumedBy(s.ID, id); err != nil {
		h.log.Warn("set resumed_by", "err", err)
	}
	h.store.Audit(ws.User.Sub, "session_resume", s.ID+" -> "+id)
	return id, "", nil
}

// liveConversation is another session of the owner that runs the same claude
// conversation, "" when none.
func (h *Hub) liveConversation(s *Session) string {
	all, err := h.store.SessionsByOwner(s.OwnerSub)
	if err != nil {
		return ""
	}
	for _, o := range all {
		if o.ID != s.ID && !o.IsChat() && o.ClaudeSessionID == s.ClaudeSessionID && o.State != proto.StateExited && o.State != proto.StateDetached {
			return o.ID
		}
	}
	return ""
}

func (h *Hub) apiMachines(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	type mv struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Online        bool     `json:"online"`
		LastSeen      string   `json:"last_seen"`
		Projects      []string `json:"projects"`
		Version       string   `json:"version"`
		ClaudeVersion string   `json:"claude_version"`
		// LiveSessions (admins only) counts the sessions a removal would end (DESIGN §24).
		LiveSessions *int `json:"live_sessions,omitempty"`
	}
	var live map[string]int
	if ws.User.IsAdmin {
		live, _ = h.store.LiveSessionCounts()
	}
	out := []mv{}
	for _, m := range h.machineViews() {
		ps := m.Projects
		if ps == nil {
			ps = []string{}
		}
		v := mv{ID: m.ID, Name: m.Name, Online: m.Online, LastSeen: m.LastSeen, Projects: ps, Version: m.Version, ClaudeVersion: m.ClaudeVersion}
		if live != nil {
			n := live[m.ID]
			v.LiveSessions = &n
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": out, "permission_modes": permissionModes})
}

// apiProfiles is what the new-code-session form needs: per profile the main-model
// choices (default first). Not in the §17.5 list; the SPA needs it for the form.
func (h *Hub) apiProfiles(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	ps, _ := h.store.Profiles()
	type pv struct {
		Name    string     `json:"name"`
		Kind    string     `json:"kind"`
		Main    string     `json:"main"`
		Choices []string   `json:"choices"`
		Aliases []aliasRow `json:"aliases"`
		Roles   []aliasRow `json:"roles"`
		RolesOn bool       `json:"roles_on"`
		Enforce string     `json:"enforce"`
	}
	out := []pv{}
	for _, f := range formProfiles(ps) {
		out = append(out, pv{f.Name, f.Kind, f.Main, f.Choices, f.Aliases, f.Roles, f.RolesOn, f.Enforce})
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

func (h *Hub) apiChatModels(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	type cm struct {
		Name          string `json:"name"`
		Label         string `json:"label"`
		Vision        bool   `json:"vision"`
		Documents     bool   `json:"documents"`
		Default       bool   `json:"default"`
		ContextWindow int    `json:"context_window"`
		MaxOutput     int    `json:"max_output"`
	}
	out := []cm{}
	for i, m := range h.chatModels() {
		out = append(out, cm{m.Name, m.Label, m.Vision, m.NativeDocuments(), i == 0, m.ContextWindow, m.MaxOutput})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out, "enabled": h.cfg.Chat.Enabled,
		"max_file_mb": h.cfg.Chat.MaxFileMB})
}
