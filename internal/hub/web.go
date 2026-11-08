package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/roles"
)

// render serves a server-rendered page; only the login page is one (DESIGN §17.11).
func (h *Hub) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		h.log.Error("template", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// ownSession loads a session and enforces owner-only visibility (DESIGN §2).
func (h *Hub) ownSession(w http.ResponseWriter, r *http.Request, ws *WebSession) *Session {
	id := r.PathValue("id")
	if !proto.ValidSessionID(id) {
		http.NotFound(w, r)
		return nil
	}
	s, err := h.store.Session(id)
	if err != nil || s.OwnerSub != ws.User.Sub {
		http.NotFound(w, r)
		return nil
	}
	return s
}

type machineView struct {
	Machine
	Online bool
}

func (h *Hub) machineViews() []machineView {
	ms, _ := h.store.Machines()
	out := make([]machineView, 0, len(ms))
	for _, m := range ms {
		out = append(out, machineView{Machine: m, Online: h.online(m.ID)})
	}
	return out
}

// subscriptionModels are the main models a subscription session may use; the first
// is the default.
var subscriptionModels = []string{"fable", "opus", "sonnet", "haiku"}

// mainChoices lists the main models a session of p may start on, default first:
// the profile's main, then its other upstream names (DESIGN §15).
func mainChoices(p proto.Profile) []string {
	if p.Kind == proto.KindSubscription {
		return subscriptionModels
	}
	out := []string{p.Main}
	names := make([]string, 0, len(p.Upstreams))
	for n := range p.Upstreams {
		if n != p.Main {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return append(out, names...)
}

// pickMain resolves the requested main model: "" means the profile's default, any
// other value must be one of mainChoices.
func pickMain(p proto.Profile, model string) (string, error) {
	cs := mainChoices(p)
	if model == "" {
		return cs[0], nil
	}
	if !slices.Contains(cs, model) {
		return "", userError("main model must be the profile's main or one of its upstreams")
	}
	return model, nil
}

type aliasRow struct{ Name, Alias, Upstream, Model string }

// formProfile is one profile as the new-session form shows it.
type formProfile struct {
	Name, Kind, Main string
	Choices          []string
	Aliases          []aliasRow // alias -> upstream -> upstream model
	Roles            []aliasRow // role agent -> alias -> upstream -> model
	RolesOn          bool
	Enforce          string // roles.enforce, defaults applied
}

func formProfiles(ps []proto.Profile) []formProfile {
	var out []formProfile
	for _, p := range ps {
		fp := formProfile{Name: p.Name, Kind: p.Kind, Choices: mainChoices(p)}
		fp.Main = fp.Choices[0]
		resolve := func(alias string) (string, string) {
			if p.Kind == proto.KindSubscription {
				return "", ""
			}
			if alias == "inherit" {
				return "(main model)", ""
			}
			up := p.Aliases[alias]
			if up == "" {
				up = p.Main // the worker falls back to the main model
			}
			return up, p.Upstreams[up].Model
		}
		up, m := resolve("fable")
		if p.Kind == proto.KindRouter {
			up, m = p.Main, p.Upstreams[p.Main].Model
		}
		fp.Aliases = append(fp.Aliases, aliasRow{"main", fp.Main, up, m})
		for _, a := range []string{"fable", "opus", "sonnet", "haiku"} {
			up, m := resolve(a)
			fp.Aliases = append(fp.Aliases, aliasRow{a, a, up, m})
		}
		r, on := roles.Effective(p.Roles)
		fp.RolesOn = on
		fp.Enforce = r.Enforce
		for _, x := range []struct{ n, a string }{{"implementer", r.Implementer}, {"researcher", r.Researcher}, {"simple", r.Simple}} {
			up, m := resolve(x.a)
			fp.Roles = append(fp.Roles, aliasRow{x.n, x.a, up, m})
		}
		out = append(out, fp)
	}
	return out
}

// permissionModes offered by the form, default first. auto is back since M5 (the
// user's choice, DESIGN §16); dontAsk and bypassPermissions, which skip the approval
// prompts, are never offered (DESIGN §15).
var permissionModes = []string{"default", "acceptEdits", "auto"}

// newSessionReq is a validated-by-newSession request; every field comes either from a
// <select> of allowlisted values or from the /new command, and is matched exactly.
type newSessionReq struct {
	MachineID, MachineName  string // one of the two
	Project, Profile, Model string
	Mode, Title             string
	// ResumeClaudeSessionID starts the session as `claude --resume <id>` (from /history).
	// The project may then be any directory inside a listed project, since the
	// transcript's cwd need not be a top-level project.
	ResumeClaudeSessionID string
	// ResumedFrom is the stopped session a Resume continues (DESIGN §21.6).
	ResumedFrom string
	// InsideRoot lets a concierge plan start in any directory inside the machine's
	// roots, not only a listed project (DESIGN §20.3); the worker checks it again.
	InsideRoot bool
	// Ack, when set, gets the worker's answer to start_session: "" when the session
	// runs, else the error. Then the channel is closed or left.
	Ack chan string
}

type userError string

func (e userError) Error() string { return string(e) }

func (h *Hub) newSession(ctx context.Context, ws *WebSession, q newSessionReq) (string, error) {
	title := strings.TrimSpace(q.Title)
	if utf8.RuneCountInString(title) > 120 {
		return "", userError("title too long")
	}
	mode := q.Mode
	if mode == "" {
		mode = "auto" // DESIGN §25.2: auto is the default on every profile
	}
	if !slices.Contains(permissionModes, mode) {
		return "", userError("bad permission mode")
	}
	var m *Machine
	ms, _ := h.store.Machines()
	for i := range ms {
		if (q.MachineID != "" && ms[i].ID == q.MachineID) || (q.MachineID == "" && q.MachineName != "" && ms[i].Name == q.MachineName) {
			m = &ms[i]
		}
	}
	if m == nil {
		return "", userError("unknown machine")
	}
	// cwd is chosen from the worker-reported allowlist, never typed (DESIGN §9).
	if !slices.Contains(m.Projects, q.Project) && !((q.ResumeClaudeSessionID != "" || q.InsideRoot) && insideAny(q.Project, m.Projects)) {
		return "", userError("project is not in the machine's allowlist")
	}
	if q.ResumeClaudeSessionID != "" && !proto.ValidSessionID(q.ResumeClaudeSessionID) {
		return "", userError("bad claude session id")
	}
	prof, err := h.store.Profile(q.Profile)
	if err != nil {
		return "", userError("unknown profile")
	}
	model, err := pickMain(*prof, q.Model)
	if err != nil {
		return "", err
	}
	if err := h.checkProfileBudget(ws.User.Sub, prof.Name); err != nil {
		return "", err
	}
	wc := h.worker(m.ID)
	if wc == nil {
		return "", userError("machine is offline")
	}
	if title == "" {
		parts := strings.Split(strings.TrimRight(q.Project, "/"), "/")
		title = parts[len(parts)-1]
	}
	s := Session{ID: proto.NewID(), MachineID: m.ID, OwnerSub: ws.User.Sub, CWD: q.Project, ProfileName: prof.Name,
		MainModel: model, Title: title, PermissionMode: mode, ClaudeSessionID: q.ResumeClaudeSessionID,
		ResumedFrom: q.ResumedFrom}
	if err := h.store.CreateSession(s); err != nil {
		return "", userError("could not create session")
	}
	frame := &proto.StartSession{Header: proto.Header{ID: proto.NewID()}, SessionID: s.ID, CWD: q.Project,
		Profile: h.resolveProfile(*prof), MainModel: model, PermissionMode: claudeMode(&s, prof), ResumeClaudeSessionID: q.ResumeClaudeSessionID,
		Tools: h.tools.CodeNames()}
	h.mu.Lock()
	h.starts[frame.ID] = s.ID
	if q.Ack != nil {
		h.acks[frame.ID] = q.Ack
	}
	if q.ResumeClaudeSessionID != "" {
		// "starting" with a claude id looks like a stalled resume to reconcile; give
		// this start the same grace period as an automatic resume.
		h.resumed[s.ID] = time.Now()
	}
	h.mu.Unlock()
	if err := wc.send(ctx, frame); err != nil {
		h.store.SetSessionError(s.ID, "could not reach machine")
		h.mu.Lock()
		delete(h.acks, frame.ID)
		h.mu.Unlock()
		if q.Ack != nil {
			q.Ack <- "could not reach machine"
		}
	}
	detail := s.ID + " " + m.Name + " " + q.Project + " " + prof.Name + " " + model
	if q.ResumeClaudeSessionID != "" {
		detail += " resume=" + q.ResumeClaudeSessionID
	}
	h.store.Audit(ws.User.Sub, "session_start", detail)
	return s.ID, nil
}

// insideAny: p is a clean absolute path equal to or below one of dirs.
func insideAny(p string, dirs []string) bool {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/") {
			return true
		}
	}
	return false
}

// parseNewCommand parses `/new <machine> <project> [profile] [mode]`. Tokens may be double
// quoted so a project path can contain spaces (`/new box "/src/my app"`). It only
// splits; every token is then matched exactly against allowlists by newSession.
func parseNewCommand(text string) (req newSessionReq, isCmd bool, err error) {
	if f := strings.Fields(text); len(f) == 0 || f[0] != "/new" {
		return req, false, nil
	}
	f, err := splitCommand(text)
	if err != nil {
		return req, true, err
	}
	if len(f) < 3 || len(f) > 5 {
		return req, true, userError(`usage: /new <machine> <project> [profile] [default|acceptEdits|auto] (quote a path with spaces: "/a b")`)
	}
	req = newSessionReq{MachineName: f[1], Project: f[2], Profile: "subscription"}
	if len(f) >= 4 {
		req.Profile = f[3]
	}
	if len(f) == 5 {
		if !slices.Contains(permissionModes, f[4]) {
			return req, true, userError("bad permission mode: use default, acceptEdits or auto")
		}
		req.Mode = f[4]
	}
	if !validName(req.MachineName) {
		return req, true, userError("unknown machine")
	}
	if !validName(req.Profile) {
		return req, true, userError("unknown profile")
	}
	return req, true, nil
}

// splitCommand splits on white space outside double quotes. Inside quotes, \" and \\
// stand for " and \; nothing else is special. An unterminated quote is an error.
func splitCommand(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inTok, inQuote := false, false
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\'):
			i++
			cur.WriteRune(rs[i])
		case c == '"':
			inQuote, inTok = !inQuote, true
		case !inQuote && unicode.IsSpace(c):
			if inTok {
				out = append(out, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteRune(c)
			inTok = true
		}
	}
	if inQuote {
		return nil, userError("unterminated quote")
	}
	if inTok {
		out = append(out, cur.String())
	}
	return out, nil
}

// apiAudit is the audit log: admins see the hub-wide log (?mine=1 narrows it to
// their own rows), users their own rows and the hub's actions on their sessions.
func (h *Hub) apiAudit(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	all := ws.User.IsAdmin && r.URL.Query().Get("mine") != "1"
	var rows []AuditRow
	var err error
	if all {
		rows, err = h.store.AuditAll(1000)
	} else {
		rows, err = h.store.AuditFor(ws.User.Sub, 500)
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	type row struct {
		TS     string `json:"ts"`
		User   string `json:"user"`
		Action string `json:"action"`
		Detail string `json:"detail"`
	}
	out := make([]row, 0, len(rows))
	for _, a := range rows {
		out = append(out, row{a.TS, a.User, a.Action, a.Detail})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": out, "all": all})
}

// apiEnroll is the web form of `mado hub enroll <name>` (admins only): a one-time
// code, valid 15 minutes.
func (h *Hub) apiEnroll(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	var q struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	if !validName(q.Name) {
		apiError(w, http.StatusUnprocessableEntity, "machine name: use 1-64 of [A-Za-z0-9_-]")
		return
	}
	code, err := h.store.CreateEnrollCode(q.Name, 15*time.Minute)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.store.Audit(ws.User.Sub, "enroll_code", q.Name)
	writeJSON(w, http.StatusOK, map[string]any{"name": q.Name, "code": code, "hub": h.cfg.BaseURL, "minutes": 15,
		"command": "mado enroll --hub " + h.cfg.BaseURL + " --name " + q.Name + " --code " + code})
}

// apiRemoveMachine is DELETE /api/machines/{name} (admins only, DESIGN §24): refused
// with 409 while the worker is connected. Sessions on the machine that had not ended
// are marked exited ("machine removed"); the 204 carries their number in the
// X-Mado-Ended-Sessions header, and open session views get the state event.
func (h *Hub) apiRemoveMachine(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	name := r.PathValue("name")
	if !validName(name) {
		apiError(w, http.StatusNotFound, ErrNoMachine.Error())
		return
	}
	// The hub's own view of liveness first; the store checks its connected flag too.
	err := ErrNoMachine
	ms, _ := h.store.Machines()
	for _, m := range ms {
		if m.Name == name {
			err = nil
			if h.online(m.ID) {
				err = ErrMachineConnected
			}
		}
	}
	var ended []string
	if err == nil {
		ended, err = h.store.RemoveMachine(name)
	}
	switch {
	case err == nil:
	case errors.Is(err, ErrNoMachine):
		apiError(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, ErrMachineConnected):
		apiError(w, http.StatusConflict, err.Error())
		return
	default:
		h.log.Warn("remove machine", "machine", name, "err", err)
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.endRemovedSessions(ended)
	h.store.Audit(ws.User.Sub, "machine.remove", fmt.Sprintf("%s ended_sessions=%d", name, len(ended)))
	h.log.Info("machine removed", "machine", name, "ended_sessions", len(ended), "by", ws.User.Sub)
	w.Header().Set("X-Mado-Ended-Sessions", strconv.Itoa(len(ended)))
	w.WriteHeader(http.StatusNoContent)
}

// endRemovedSessions tells open views (and a digest job waiting on one) that the
// sessions a machine removal ended are over.
func (h *Hub) endRemovedSessions(ids []string) {
	for _, id := range ids {
		h.publishState(id, proto.StateExited)
		if job := h.jobs.get(id); job != nil {
			job.note(jobNote{exited: true, err: MachineRemovedReason})
		}
	}
}

// decideApproval forwards the user's decision to the worker and records it (POST
// /api/sessions/{id}/approve). A decision only counts once the machine has it: while
// the machine is offline, or when the send fails, the approval stays pending and the
// answer is 503 (DESIGN §17.11), so the card is still there after a reload.
//
// An AskUserQuestion (DESIGN §21.1) is answered with allow plus answers (and optional
// notes), which become the updated_input claude runs the tool with; allow without
// answers and allow_session are refused for it, and deny dismisses the question.
func (h *Hub) decideApproval(r *http.Request, ws *WebSession, s *Session, reqID, decision string, answers, notes map[string]string) (*Approval, int, error) {
	switch decision {
	case proto.DecisionAllow, proto.DecisionAllowSession, proto.DecisionDeny:
	default:
		return nil, http.StatusBadRequest, errors.New("bad decision")
	}
	a, err := h.store.Approval(s.ID, reqID)
	if err != nil {
		return nil, http.StatusNotFound, errors.New("no such approval")
	}
	if a.State != "pending" {
		return nil, http.StatusConflict, errors.New("already decided")
	}
	question := a.ToolName == ToolAskUserQuestion
	var updated json.RawMessage
	stored := ""
	switch {
	case question && decision == proto.DecisionAllowSession:
		return nil, http.StatusBadRequest, errors.New("a question cannot be allowed for the session; answer it")
	case question && decision == proto.DecisionAllow:
		var err error
		if updated, stored, err = answerQuestion(a.Input, answers, notes); err != nil {
			return nil, http.StatusBadRequest, err
		}
	case len(answers) > 0 || len(notes) > 0:
		return nil, http.StatusBadRequest, errors.New("answers are only for allowing an AskUserQuestion")
	}
	if !h.online(s.MachineID) {
		return nil, http.StatusServiceUnavailable, errors.New("machine is offline; the approval stays pending")
	}
	// Claim it first, so two viewers cannot both answer.
	ok, err := h.store.DecideApprovalAnswers(s.ID, reqID, decision, ws.User.Sub, stored)
	if err != nil || !ok {
		return nil, http.StatusConflict, errors.New("already decided")
	}
	msg := ""
	if decision == proto.DecisionDeny {
		msg = "The user denied this in mado."
		if question {
			msg = dismissMessage
		}
	}
	if err := h.sendToSession(r.Context(), s, &proto.ApprovalResponse{SessionID: s.ID, RequestID: reqID, Decision: decision, Message: msg,
		UpdatedInput: updated}); err != nil {
		h.store.ReopenApproval(s.ID, reqID)
		return nil, http.StatusServiceUnavailable, errors.New("could not reach the machine; the approval stays pending")
	}
	if decision == proto.DecisionAllowSession {
		_ = h.store.AddSessionRule(s.ID, approvalRule(a.ToolName, json.RawMessage(a.Input)))
	}
	h.store.Audit(ws.User.Sub, "approval", s.ID+" "+a.ToolName+" "+decision)
	a.State, a.Decision, a.Answers = "decided", decision, stored
	return a, http.StatusOK, nil
}
