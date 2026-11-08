package hub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kinginu/mado/internal/chat"
	"github.com/kinginu/mado/internal/messages"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/render"
	"github.com/kinginu/mado/internal/tools"
)

// The concierge (DESIGN §20): a chat session whose tools are mado's own API. It looks
// at machines and directories freely; every write (mkdir, git init, clone, worktree,
// new sessions) runs only from a plan the user approved on its card.

// Concierge tool names (DESIGN §20.2).
const (
	toolListMachines  = "list_machines"
	toolListProjects  = "list_projects"
	toolInspectDir    = "inspect_dir"
	toolListProfiles  = "list_profiles"
	toolPropose       = "propose"
	toolSessionStatus = "session_status"
)

// Plan states.
const (
	PlanProposed = "proposed"
	PlanRunning  = "running"
	PlanDone     = "done"
	PlanFailed   = "failed"
	PlanRejected = "rejected"
)

// Step states.
const (
	stepPending = "pending"
	stepRunning = "running"
	stepDone    = "done"
	stepFailed  = "failed"
	stepSkipped = "skipped"
)

// Timeouts: the worker gets 18 s for an inspection and a quick step, 10 min for
// clone and worktree; the hub waits a little longer so the worker's own answer wins.
const (
	fsInspectWait  = 20 * time.Second
	fsQuickWait    = 20 * time.Second
	fsGitWait      = 10*time.Minute + 30*time.Second
	startAckWait   = 2 * time.Minute
	maxPlanPrepare = 20
	maxPlanSession = 10
	maxFirstPrompt = 100_000
)

// Plan is what the concierge proposes (DESIGN §20.3).
type Plan struct {
	Summary  string        `json:"summary"`
	Prepare  []PrepareStep `json:"prepare"`
	Sessions []PlanSession `json:"sessions"`
}

// PrepareStep is one of mkdir, git_init, clone, worktree.
type PrepareStep struct {
	Machine string `json:"machine"`
	Action  string `json:"action"`
	Path    string `json:"path"`
	Repo    string `json:"repo,omitempty"`
	URL     string `json:"url,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Base    string `json:"base,omitempty"`
}

// PlanSession is a code session the plan starts.
type PlanSession struct {
	Machine     string `json:"machine"`
	CWD         string `json:"cwd"`
	Profile     string `json:"profile"`
	MainModel   string `json:"main_model"`
	Mode        string `json:"mode"`
	Title       string `json:"title"`
	FirstPrompt string `json:"first_prompt"`
}

// planStep is the card's view of one step.
type planStep struct {
	Kind      string `json:"kind"` // prepare | session
	Label     string `json:"label"`
	State     string `json:"state"`
	Output    string `json:"output,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// planRecord is a stored plan with its progress.
type planRecord struct {
	ID        string     `json:"id"`
	SessionID string     `json:"-"`
	State     string     `json:"state"`
	Plan      Plan       `json:"plan"`
	Steps     []planStep `json:"steps"`
	Error     string     `json:"error,omitempty"`
	CreatedAt string     `json:"created_at"`
	DecidedAt string     `json:"decided_at,omitempty"`
}

func (p *PrepareStep) label() string {
	switch p.Action {
	case proto.ActionClone:
		s := "clone " + p.URL + " into " + p.Path
		if p.Branch != "" {
			s += " (branch " + p.Branch + ")"
		}
		return p.Machine + ": " + s
	case proto.ActionWorktree:
		s := "worktree " + p.Path + " of " + p.Repo + " on branch " + p.Branch
		if p.Base != "" {
			s += " from " + p.Base
		}
		return p.Machine + ": " + s
	}
	return p.Machine + ": " + p.Action + " " + p.Path
}

func (s *PlanSession) label() string {
	return fmt.Sprintf("session %q on %s in %s (%s, %s)", s.Title, s.Machine, s.CWD, s.Profile, s.Mode)
}

// --- store

type planDoc struct {
	Plan  Plan       `json:"plan"`
	Steps []planStep `json:"steps"`
	Error string     `json:"error,omitempty"`
}

func (s *Store) InsertPlan(p *planRecord) error {
	b, _ := json.Marshal(planDoc{p.Plan, p.Steps, p.Error})
	_, err := s.db.Exec(`INSERT INTO plans(id,session_id,json,state,created_at) VALUES(?,?,?,?,?)`, p.ID, p.SessionID, string(b), p.State, now())
	return err
}

func (s *Store) Plan(id string) (*planRecord, error) {
	var p planRecord
	var j string
	err := s.db.QueryRow(`SELECT id, session_id, json, state, COALESCE(created_at,''), COALESCE(decided_at,'') FROM plans WHERE id=?`, id).
		Scan(&p.ID, &p.SessionID, &j, &p.State, &p.CreatedAt, &p.DecidedAt)
	if err != nil {
		return nil, err
	}
	var d planDoc
	if err := json.Unmarshal([]byte(j), &d); err != nil {
		return nil, err
	}
	p.Plan, p.Steps, p.Error = d.Plan, d.Steps, d.Error
	return &p, nil
}

// SavePlan stores the progress and state.
func (s *Store) SavePlan(p *planRecord) error {
	b, _ := json.Marshal(planDoc{p.Plan, p.Steps, p.Error})
	_, err := s.db.Exec(`UPDATE plans SET json=?, state=? WHERE id=?`, string(b), p.State, p.ID)
	return err
}

// ClaimPlan moves a proposed plan of the session to state; false when it was not
// proposed (any more), so two clicks cannot both run it.
func (s *Store) ClaimPlan(id, sessionID, state string) (bool, error) {
	res, err := s.db.Exec(`UPDATE plans SET state=?, decided_at=? WHERE id=? AND session_id=? AND state=?`, state, now(), id, sessionID, PlanProposed)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ProposedPlans are the session's plans still waiting for a decision.
func (s *Store) ProposedPlans(sessionID string) []string {
	rows, err := s.db.Query(`SELECT id FROM plans WHERE session_id=? AND state=?`, sessionID, PlanProposed)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// RunningPlans are plans a stopped hub left running.
func (s *Store) RunningPlans() []string {
	rows, err := s.db.Query(`SELECT id FROM plans WHERE state=?`, PlanRunning)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// Spawned lists the code sessions a concierge session started.
func (s *Store) Spawned(sessionID string) []string {
	var j string
	_ = s.db.QueryRow(`SELECT COALESCE(spawned,'[]') FROM sessions WHERE id=?`, sessionID).Scan(&j)
	var out []string
	_ = json.Unmarshal([]byte(j), &out)
	return out
}

func (s *Store) AddSpawned(sessionID, id string) error {
	ids := append(s.Spawned(sessionID), id)
	b, _ := json.Marshal(ids)
	_, err := s.db.Exec(`UPDATE sessions SET spawned=? WHERE id=?`, string(b), sessionID)
	return err
}

// LastProfile is the profile of the user's newest code session, "" for none.
func (s *Store) LastProfile(sub string) string {
	var p string
	_ = s.db.QueryRow(`SELECT COALESCE(profile_name,'') FROM sessions WHERE owner_sub=? AND COALESCE(kind,'code')='code'
		AND COALESCE(profile_name,'')<>'' ORDER BY created_at DESC LIMIT 1`, sub).Scan(&p)
	return p
}

// --- setup, sessions, turns

// setupConcierge checks [concierge] model and fails the plans a stopped hub left running.
func (h *Hub) setupConcierge() error {
	if err := h.cfg.Concierge.applyDefaults(); err != nil {
		return err
	}
	// [concierge] model is the hub.toml value of the concierge.model setting (§28.3).
	if err := h.store.SeedSetting(SettingConciergeModel, h.cfg.Concierge.Model); err != nil {
		return err
	}
	if st := h.store.Setting(SettingConciergeModel); st.Value != "" {
		if _, err := h.store.ChatModel(st.Value); err != nil {
			if st.Source != SourceUI {
				return fmt.Errorf("[concierge] model %q is not a [[chat.models]] name", st.Value)
			}
			h.log.Warn("the concierge model set in the editor is not a chat model; the chat default is used", "model", st.Value)
		}
	}
	for _, id := range h.store.RunningPlans() {
		p, err := h.store.Plan(id)
		if err != nil {
			continue
		}
		for i := range p.Steps {
			switch p.Steps[i].State {
			case stepRunning:
				p.Steps[i].State, p.Steps[i].Output = stepFailed, "the hub stopped while this step ran; check the machine"
			case stepPending:
				p.Steps[i].State = stepSkipped
			}
		}
		p.State, p.Error = PlanFailed, "the hub restarted while the plan ran"
		_ = h.store.SavePlan(p)
	}
	return nil
}

// conciergeModel is the concierge.model setting ([concierge] model or its editor
// override, DESIGN §28.3) when it names a chat model, else the chat default.
func (h *Hub) conciergeModel() string {
	if m := h.store.Setting(SettingConciergeModel).Value; m != "" {
		if _, err := h.store.ChatModel(m); err == nil {
			return m
		}
	}
	if ms := h.chatModels(); len(ms) > 0 {
		return ms[0].Name
	}
	return ""
}

// newConciergeSession creates a concierge chat session (DESIGN §20.1).
func (h *Hub) newConciergeSession(ws *WebSession, title string) (string, error) {
	return h.createChat(ws, h.conciergeModel(), "", title, true)
}

// conciergeTools are the tools of one concierge turn.
type conciergeTools struct {
	h         *Hub
	sessionID string
	owner     string
}

// isPlanCommand: `/new plan [first message]` starts a concierge session. With text
// after it, a machine named "plan" keeps `/new plan <project>` a code command.
func (h *Hub) isPlanCommand(text string) (rest string, ok bool) {
	f := strings.Fields(text)
	if len(f) < 2 || f[0] != "/new" || f[1] != "plan" {
		return "", false
	}
	if len(f) > 2 {
		if ms, _ := h.store.Machines(); slices.ContainsFunc(ms, func(m Machine) bool { return m.Name == "plan" }) {
			return "", false
		}
	}
	_, after, _ := strings.Cut(text, "plan")
	return strings.TrimSpace(after), true
}

// startConcierge is `/new plan …` and the Plan tab: a new concierge session and,
// when given, its first message.
func (h *Hub) startConcierge(ws *WebSession, first string) (string, error) {
	id, err := h.newConciergeSession(ws, "")
	if err != nil {
		return "", err
	}
	if first != "" {
		s, err := h.store.Session(id)
		if err == nil {
			_, err = h.chatSend(ws, s, first, nil)
		}
		if err != nil {
			return id, err
		}
	}
	return id, nil
}

// --- worker round trips

type workerInfo struct {
	m      Machine
	online bool
	fs     bool
	roots  []string
}

// workerInfoByName finds a machine by name with its live roots and capabilities.
func (h *Hub) workerInfoByName(name string) (*workerInfo, error) {
	ms, _ := h.store.Machines()
	for _, m := range ms {
		if m.Name == name {
			return h.workerInfoOf(m), nil
		}
	}
	return nil, tools.Error("unknown machine " + name + "; list_machines shows the machines")
}

func (h *Hub) workerInfoOf(m Machine) *workerInfo {
	wi := &workerInfo{m: m}
	h.mu.Lock()
	wc := h.workers[m.ID]
	if wc != nil {
		wi.online, wi.fs, wi.roots = true, slices.Contains(wc.caps, proto.CapFS), wc.roots
	}
	h.mu.Unlock()
	if len(wi.roots) == 0 {
		wi.roots = topLevel(m.Projects)
	}
	return wi
}

// topLevel derives the roots from an older worker's project list (roots and their
// children): the entries not below another entry.
func topLevel(ps []string) []string {
	var out []string
	for _, p := range ps {
		if !slices.ContainsFunc(ps, func(q string) bool { return q != p && strings.HasPrefix(p, strings.TrimSuffix(q, "/")+"/") }) {
			out = append(out, p)
		}
	}
	return out
}

func (wi *workerInfo) needFS() error {
	if !wi.online {
		return tools.Error("machine " + wi.m.Name + " is offline")
	}
	if !wi.fs {
		return tools.Error("the worker on " + wi.m.Name + " is too old for this (no fs capability); update mado there")
	}
	return nil
}

type fsWaiter struct {
	machineID string
	ch        chan *proto.FSResult
}

// fsCall sends an fs frame and waits for its fs_result.
func (h *Hub) fsCall(ctx context.Context, machineID, reqID string, f proto.Frame, wait time.Duration) (json.RawMessage, error) {
	r, err := h.fsRequest(ctx, machineID, reqID, f, wait)
	if err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, tools.Error(r.Error)
	}
	return r.Data, nil
}

// fsRequest sends an fs frame and returns its fs_result, failed or not.
func (h *Hub) fsRequest(ctx context.Context, machineID, reqID string, f proto.Frame, wait time.Duration) (*proto.FSResult, error) {
	wc := h.worker(machineID)
	if wc == nil {
		return nil, tools.Error("the machine is offline")
	}
	w := fsWaiter{machineID: machineID, ch: make(chan *proto.FSResult, 1)}
	h.mu.Lock()
	h.fsWaits[reqID] = w
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.fsWaits, reqID)
		h.mu.Unlock()
	}()
	if err := wc.send(ctx, f); err != nil {
		return nil, tools.Error("could not reach the machine")
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case r := <-w.ch:
		return r, nil
	case <-t.C:
		return nil, tools.Error(fmt.Sprintf("the machine did not answer within %s", wait.Round(time.Second)))
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// onFSResult hands a result to its waiter; results from another machine are dropped.
func (h *Hub) onFSResult(wc *workerConn, m *proto.FSResult) {
	h.mu.Lock()
	w, ok := h.fsWaits[m.RequestID]
	h.mu.Unlock()
	if !ok || w.machineID != wc.machineID {
		return
	}
	select {
	case w.ch <- m:
	default:
	}
}

// --- tools

func (c conciergeTools) Defs() []messages.Tool {
	obj := func(props map[string]any, required ...string) json.RawMessage {
		if required == nil {
			required = []string{}
		}
		b, _ := json.Marshal(map[string]any{"type": "object", "properties": props, "required": required})
		return b
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	step := map[string]any{"type": "object", "properties": map[string]any{
		"machine": str("Machine name."),
		"action":  map[string]any{"type": "string", "enum": []string{"mkdir", "git_init", "clone", "worktree"}},
		"path":    str("Absolute path inside a root of the machine: the directory to create, initialise, clone into, or the new worktree."),
		"repo":    str("worktree only: the existing repository."),
		"url":     str("clone only: https://, ssh:// or git@host:path URL."),
		"branch":  str("clone: the branch to check out (optional). worktree: the branch of the new worktree (created when it does not exist)."),
		"base":    str("worktree only: the commit or branch a new branch starts from (optional, default HEAD)."),
	}, "required": []string{"machine", "action", "path"}}
	sess := map[string]any{"type": "object", "properties": map[string]any{
		"machine":      str("Machine name."),
		"cwd":          str("Absolute working directory inside a root (created by a prepare step or already there)."),
		"profile":      str("Profile name from list_profiles; empty = the one the user used last."),
		"main_model":   str("Main model; empty = the profile's default."),
		"mode":         map[string]any{"type": "string", "enum": []string{"", "default", "acceptEdits", "auto"}, "description": "Permission mode; empty = auto on every profile (router: mado's rules and classifier decide; subscription: Claude Code's own auto mode)."},
		"title":        str("Short session title."),
		"first_prompt": str("The first message of the session: the goal, the constraints and the facts found, asking to discuss the design with the user before implementing."),
	}, "required": []string{"machine", "cwd", "title", "first_prompt"}}
	plan := map[string]any{"type": "object", "properties": map[string]any{
		"summary":  str("One line: what the plan does."),
		"prepare":  map[string]any{"type": "array", "items": step, "description": "Steps run in order before the sessions start."},
		"sessions": map[string]any{"type": "array", "items": sess},
	}, "required": []string{"summary", "prepare", "sessions"}}
	return []messages.Tool{
		{Name: toolListMachines, Description: "List the online machines: name, project roots, Claude Code version, number of live sessions.",
			InputSchema: obj(map[string]any{})},
		{Name: toolListProjects, Description: "List the top-level directories of each project root of a machine (name, is_git, mtime), newest first, at most 200 per root.",
			InputSchema: obj(map[string]any{"machine": str("Machine name.")}, "machine")},
		{Name: toolInspectDir, Description: "Look at one directory inside a root: entries (dirs first, at most 200), the start of its README, the git state (branch, upstream, dirty files, worktrees) and the languages guessed from file names.",
			InputSchema: obj(map[string]any{"machine": str("Machine name."), "path": str("Absolute path inside a root.")}, "machine", "path")},
		{Name: toolListProfiles, Description: "List the profiles a session can use (kind, main model, the models behind its aliases) and which one the user used last.",
			InputSchema: obj(map[string]any{})},
		{Name: toolPropose, Description: "Propose a plan: prepare steps (mkdir, git_init, clone, worktree) and the sessions to start. The user sees it as a card and approves or rejects it; nothing runs before that. Returns the plan id.",
			InputSchema: obj(map[string]any{"plan": plan}, "plan")},
		{Name: toolSessionStatus, Description: "The state of a session this concierge started (or of all of them when session_id is empty): state, title, machine, cwd, the last messages abbreviated, pending approvals, cost.",
			InputSchema: obj(map[string]any{"session_id": str("Session id; empty = every session this concierge started.")})},
	}
}

func textResult(v any) tools.Result {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return tools.Result{Text: strings.TrimSpace(buf.String())}
}

func (c conciergeTools) Run(ctx context.Context, name string, input json.RawMessage) (tools.Result, error) {
	h := c.h
	var in struct {
		Machine   string          `json:"machine"`
		Path      string          `json:"path"`
		SessionID string          `json:"session_id"`
		Plan      json.RawMessage `json:"plan"`
	}
	if len(input) > 0 && json.Unmarshal(input, &in) != nil {
		return tools.Result{}, tools.Error("the tool input is not a JSON object")
	}
	switch name {
	case toolListMachines:
		return textResult(h.listMachines()), nil
	case toolListProjects:
		wi, err := h.workerInfoByName(in.Machine)
		if err != nil {
			return tools.Result{}, err
		}
		if err := wi.needFS(); err != nil {
			return tools.Result{}, err
		}
		id := proto.NewID()
		data, err := h.fsCall(ctx, wi.m.ID, id, &proto.FSInspect{RequestID: id, Kind: proto.InspectListProjects}, fsInspectWait)
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{Text: string(data)}, nil
	case toolInspectDir:
		wi, err := h.workerInfoByName(in.Machine)
		if err != nil {
			return tools.Result{}, err
		}
		if !proto.Inside(in.Path, wi.roots) {
			return tools.Result{}, tools.Error("path must be an absolute, clean path inside a root of " + wi.m.Name + ": " + strings.Join(wi.roots, ", "))
		}
		if err := wi.needFS(); err != nil {
			return tools.Result{}, err
		}
		id := proto.NewID()
		data, err := h.fsCall(ctx, wi.m.ID, id, &proto.FSInspect{RequestID: id, Kind: proto.InspectDir, Path: in.Path}, fsInspectWait)
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{Text: string(data)}, nil
	case toolListProfiles:
		return textResult(h.listProfiles(c.owner)), nil
	case toolPropose:
		return h.propose(c.sessionID, c.owner, in.Plan)
	case toolSessionStatus:
		return h.sessionStatus(c.sessionID, c.owner, in.SessionID)
	}
	return tools.Result{}, tools.Error("unknown tool " + name)
}

func (h *Hub) listMachines() map[string]any {
	ms, _ := h.store.Machines()
	type mv struct {
		Name          string   `json:"name"`
		Roots         []string `json:"roots"`
		ClaudeVersion string   `json:"claude_version"`
		Sessions      int      `json:"live_sessions"`
		CanPrepare    bool     `json:"can_prepare"`
	}
	online := []mv{}
	offline := []string{}
	for _, m := range ms {
		wi := h.workerInfoOf(m)
		if !wi.online {
			offline = append(offline, m.Name)
			continue
		}
		ss, _ := h.store.SessionsOnMachine(m.ID)
		online = append(online, mv{m.Name, wi.roots, m.ClaudeVersion, len(ss), wi.fs})
	}
	return map[string]any{"machines": online, "offline": offline}
}

// defaultMode is the plan's mode default (DESIGN §20.3): auto on every profile since
// M22 (§25.2); mado auto on a router profile, claude's own on a subscription one.
func defaultMode(_ *proto.Profile) string {
	return "auto"
}

func (h *Hub) listProfiles(owner string) map[string]any {
	ps, _ := h.store.Profiles()
	type pv struct {
		Name        string            `json:"name"`
		Kind        string            `json:"kind"`
		Main        string            `json:"main"`
		MainModel   string            `json:"main_model,omitempty"`
		Aliases     map[string]string `json:"aliases,omitempty"` // alias -> upstream model
		DefaultMode string            `json:"default_mode"`
	}
	out := []pv{}
	for _, f := range formProfiles(ps) {
		v := pv{Name: f.Name, Kind: f.Kind, Main: f.Main}
		for i := range ps {
			if ps[i].Name == f.Name {
				v.DefaultMode = defaultMode(&ps[i])
			}
		}
		for _, a := range f.Aliases {
			if a.Name == "main" {
				v.MainModel = a.Model
				continue
			}
			if a.Model != "" {
				if v.Aliases == nil {
					v.Aliases = map[string]string{}
				}
				v.Aliases[a.Name] = a.Model
			}
		}
		out = append(out, v)
	}
	return map[string]any{"profiles": out, "last_used": h.defaultProfile(owner)}
}

// defaultProfile is the user's last used profile when it still exists, else subscription.
func (h *Hub) defaultProfile(owner string) string {
	if p := h.store.LastProfile(owner); p != "" {
		if _, err := h.store.Profile(p); err == nil {
			return p
		}
	}
	return "subscription"
}

// validatePlan checks a plan against the machines' roots and fills the defaults
// (DESIGN §20.3). The worker checks every path again.
func (h *Hub) validatePlan(owner string, p *Plan) error {
	bad := func(format string, a ...any) error { return tools.Error(fmt.Sprintf(format, a...)) }
	p.Summary = strings.TrimSpace(p.Summary)
	if p.Summary == "" || utf8.RuneCountInString(p.Summary) > 300 {
		return bad("summary must be one line of 1..300 characters")
	}
	if len(p.Prepare) > maxPlanPrepare || len(p.Sessions) > maxPlanSession {
		return bad("at most %d prepare steps and %d sessions", maxPlanPrepare, maxPlanSession)
	}
	if len(p.Prepare)+len(p.Sessions) == 0 {
		return bad("the plan has nothing to do")
	}
	infos := map[string]*workerInfo{}
	machine := func(name string) (*workerInfo, error) {
		if wi, ok := infos[name]; ok {
			return wi, nil
		}
		wi, err := h.workerInfoByName(name)
		if err != nil {
			return nil, err
		}
		if !wi.online {
			return nil, bad("machine %s is offline", name)
		}
		infos[name] = wi
		return wi, nil
	}
	for i := range p.Prepare {
		st := &p.Prepare[i]
		n := i + 1
		wi, err := machine(st.Machine)
		if err != nil {
			return bad("prepare %d: %v", n, err)
		}
		if err := wi.needFS(); err != nil {
			return bad("prepare %d: %v", n, err)
		}
		if !proto.Inside(st.Path, wi.roots) {
			return bad("prepare %d: path %q must be an absolute, clean path inside a root of %s (%s)", n, st.Path, st.Machine, strings.Join(wi.roots, ", "))
		}
		switch st.Action {
		case proto.ActionMkdir, proto.ActionGitInit:
			if st.Repo != "" || st.URL != "" || st.Branch != "" || st.Base != "" {
				return bad("prepare %d: %s takes only machine and path", n, st.Action)
			}
		case proto.ActionClone:
			if !proto.ValidGitURL(st.URL) {
				return bad("prepare %d: url must be https://host/path (no credentials), ssh://host/path or git@host:path", n)
			}
			if st.Branch != "" && !proto.ValidRefName(st.Branch) {
				return bad("prepare %d: branch must be a plain ref name", n)
			}
			if st.Repo != "" || st.Base != "" {
				return bad("prepare %d: clone takes machine, path, url and branch", n)
			}
		case proto.ActionWorktree:
			if !proto.Inside(st.Repo, wi.roots) {
				return bad("prepare %d: repo %q must be inside a root of %s", n, st.Repo, st.Machine)
			}
			if !proto.ValidRefName(st.Branch) {
				return bad("prepare %d: worktree needs branch, a plain ref name", n)
			}
			if st.Base != "" && !proto.ValidRefName(st.Base) {
				return bad("prepare %d: base must be a plain ref name", n)
			}
			if st.URL != "" {
				return bad("prepare %d: worktree takes no url", n)
			}
		default:
			return bad("prepare %d: action must be mkdir, git_init, clone or worktree", n)
		}
	}
	for i := range p.Sessions {
		ps := &p.Sessions[i]
		n := i + 1
		wi, err := machine(ps.Machine)
		if err != nil {
			return bad("session %d: %v", n, err)
		}
		if !proto.Inside(ps.CWD, wi.roots) {
			return bad("session %d: cwd %q must be an absolute, clean path inside a root of %s (%s)", n, ps.CWD, ps.Machine, strings.Join(wi.roots, ", "))
		}
		if ps.Profile == "" {
			ps.Profile = h.defaultProfile(owner)
		}
		prof, err := h.store.Profile(ps.Profile)
		if err != nil {
			return bad("session %d: unknown profile %q; list_profiles shows them", n, ps.Profile)
		}
		if ps.Mode == "" {
			ps.Mode = defaultMode(prof)
		}
		if !slices.Contains(permissionModes, ps.Mode) {
			return bad("session %d: mode must be default, acceptEdits or auto", n)
		}
		if ps.MainModel != "" {
			if _, err := pickMain(*prof, ps.MainModel); err != nil {
				return bad("session %d: main_model must be one of %s", n, strings.Join(mainChoices(*prof), ", "))
			}
		}
		ps.Title = strings.TrimSpace(ps.Title)
		if ps.Title == "" {
			parts := strings.Split(ps.CWD, "/")
			ps.Title = parts[len(parts)-1]
		}
		if utf8.RuneCountInString(ps.Title) > 120 {
			return bad("session %d: title too long", n)
		}
		ps.FirstPrompt = strings.TrimSpace(ps.FirstPrompt)
		if len(ps.FirstPrompt) > maxFirstPrompt || !utf8.ValidString(ps.FirstPrompt) {
			return bad("session %d: first_prompt must be text of at most %d bytes", n, maxFirstPrompt)
		}
	}
	return nil
}

// decodePlan reads the propose input strictly; a plan sent as a JSON string (some
// models do that) is accepted too.
func decodePlan(raw json.RawMessage) (*Plan, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			raw = json.RawMessage(s)
		}
	}
	if len(raw) == 0 || raw[0] != '{' {
		return nil, tools.Error("plan must be an object with summary, prepare and sessions")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p Plan
	if err := dec.Decode(&p); err != nil {
		msg := err.Error()
		if strings.HasPrefix(msg, "json: unknown field") {
			return nil, tools.Error("plan: " + strings.TrimPrefix(msg, "json: "))
		}
		return nil, tools.Error("plan is not valid JSON of the documented shape")
	}
	return &p, nil
}

func newPlanSteps(p *Plan) []planStep {
	var steps []planStep
	for i := range p.Prepare {
		steps = append(steps, planStep{Kind: "prepare", Label: p.Prepare[i].label(), State: stepPending})
	}
	for i := range p.Sessions {
		steps = append(steps, planStep{Kind: "session", Label: p.Sessions[i].label(), State: stepPending})
	}
	return steps
}

// propose validates and stores a plan and returns the card event (DESIGN §20.3).
// An earlier plan of the session that is still waiting is replaced.
func (h *Hub) propose(sessionID, owner string, raw json.RawMessage) (tools.Result, error) {
	p, err := decodePlan(raw)
	if err != nil {
		return tools.Result{}, err
	}
	if err := h.validatePlan(owner, p); err != nil {
		return tools.Result{}, err
	}
	rec := &planRecord{ID: proto.NewID(), SessionID: sessionID, State: PlanProposed, Plan: *p, Steps: newPlanSteps(p)}
	for _, old := range h.store.ProposedPlans(sessionID) {
		if ok, _ := h.store.ClaimPlan(old, sessionID, PlanRejected); ok {
			if o, err := h.store.Plan(old); err == nil {
				o.Error = "replaced by a newer plan"
				for i := range o.Steps {
					o.Steps[i].State = stepSkipped
				}
				_ = h.store.SavePlan(o)
				h.publishPlan(o)
			}
		}
	}
	if err := h.store.InsertPlan(rec); err != nil {
		return tools.Result{}, tools.Error("could not store the plan")
	}
	ev, _ := json.Marshal(map[string]any{"type": "system", "subtype": "mado_plan", "plan_id": rec.ID, "plan": rec.Plan,
		"state": PlanProposed, "session_id": sessionID})
	return tools.Result{Text: "plan_id: " + rec.ID + "\nThe plan is shown to the user as a card with Approve and Reject. Nothing has run yet. " +
		"End your turn now with one short sentence; mado sends you the outcome when the user decides.", Event: ev}, nil
}

// sessionStatus answers for one or all of the sessions this concierge started; only
// the owner's own sessions.
func (h *Hub) sessionStatus(conciergeID, owner, id string) (tools.Result, error) {
	ids := h.store.Spawned(conciergeID)
	if id != "" {
		ids = []string{id}
	}
	if len(ids) == 0 {
		return tools.Result{Text: "This concierge has not started any session yet."}, nil
	}
	var out []map[string]any
	for _, sid := range ids {
		s, err := h.store.Session(sid)
		if !proto.ValidSessionID(sid) || err != nil || s.OwnerSub != owner {
			if id != "" {
				return tools.Result{}, tools.Error("no such session")
			}
			continue
		}
		pending, _ := h.store.PendingApprovals(s.ID)
		u := h.store.SessionUsage(s.ID)
		lastUser, lastAsst := h.lastTexts(s.ID)
		v := map[string]any{"session_id": s.ID, "link": "/s/" + s.ID, "title": s.Title, "state": s.State, "kind": s.Kind,
			"last_user_text": lastUser, "last_assistant_text": lastAsst, "pending_approvals": len(pending),
			"cost": fmt.Sprintf("%.4g %s", u.Cost, h.cfg.Budgets.Currency)}
		if !s.IsChat() {
			v["machine"], v["cwd"], v["profile"] = s.MachineName, s.CWD, s.ProfileName
		}
		if s.LastError != "" {
			v["error"] = s.LastError
		}
		out = append(out, v)
	}
	return textResult(map[string]any{"sessions": out, "month_to_date_cost": fmt.Sprintf("%.4g %s",
		h.store.MonthUsage(owner, monthOf(time.Now())).Cost, h.cfg.Budgets.Currency)}), nil
}

// lastTexts are the newest user and assistant texts of a session, abbreviated.
func (h *Hub) lastTexts(sessionID string) (user, asst string) {
	evs, _, err := h.store.EventsBefore(sessionID, 0, 200)
	if err != nil {
		return "", ""
	}
	for i := len(evs) - 1; i >= 0 && (user == "" || asst == ""); i-- {
		for _, b := range render.Blocks(evs[i].Raw) {
			if b.Subagent {
				continue
			}
			switch {
			case b.Kind == render.KindUser && user == "":
				user = render.Abbrev(strings.Join(strings.Fields(b.Text), " "), 400)
			case b.Kind == render.KindAssistant && asst == "":
				asst = render.Abbrev(strings.Join(strings.Fields(b.Text), " "), 600)
			}
		}
	}
	return user, asst
}

// --- the card, decisions and execution

// planJSON is the card's view.
func planJSON(p *planRecord) map[string]any {
	return map[string]any{"id": p.ID, "state": p.State, "plan": p.Plan, "steps": p.Steps, "error": p.Error}
}

func (h *Hub) publishPlan(p *planRecord) {
	b, _ := json.Marshal(planJSON(p))
	h.broker.publish(p.SessionID, sseMsg{event: "plan", data: string(b)})
}

// fillPlans gives each plan block the plan's current state.
func (h *Hub) fillPlans(bs []apiBlock) []apiBlock {
	for i := range bs {
		if bs[i].Kind == render.KindPlan && bs[i].PlanID != "" {
			if p, err := h.store.Plan(bs[i].PlanID); err == nil {
				bs[i].Plan = planJSON(p)
			}
		}
	}
	return bs
}

// apiPlanDecision is POST /api/sessions/{id}/plans/{plan_id} {"decision"}.
func (h *Hub) apiPlanDecision(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return
	}
	var q struct {
		Decision string `json:"decision"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	if q.Decision != "approve" && q.Decision != "reject" {
		apiError(w, http.StatusBadRequest, `decision must be "approve" or "reject"`)
		return
	}
	planID := r.PathValue("plan_id")
	p, err := h.store.Plan(planID)
	if !proto.ValidSessionID(planID) || err != nil || p.SessionID != s.ID || !s.Concierge {
		apiError(w, http.StatusNotFound, "no such plan")
		return
	}
	state := PlanRunning
	if q.Decision == "reject" {
		state = PlanRejected
	}
	ok, err := h.store.ClaimPlan(p.ID, s.ID, state)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		apiError(w, http.StatusConflict, "this plan was already decided")
		return
	}
	p.State = state
	h.store.Audit(ws.User.Sub, "plan_"+q.Decision, s.ID+" "+p.ID)
	if state == PlanRejected {
		for i := range p.Steps {
			p.Steps[i].State = stepSkipped
		}
		_ = h.store.SavePlan(p)
		h.appendNote(s.ID, "The user rejected plan "+p.ID+". Nothing was run.")
	}
	h.publishPlan(p)
	view, _ := json.Marshal(planJSON(p)) // before runPlan starts changing p
	if state == PlanRunning {
		h.planWG.Add(1)
		go func() {
			defer h.planWG.Done()
			h.runPlan(ws.User, s.ID, p)
		}()
	}
	writeJSON(w, http.StatusOK, json.RawMessage(view))
}

// appendNote stores a mado note for the model without running a turn.
func (h *Hub) appendNote(sessionID, text string) {
	raw, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user",
		"content": []any{messages.Block{Type: "text", Text: text}}}, "parent_tool_use_id": nil, "session_id": sessionID,
		"mado_note": "plan"})
	_, _ = chatHost{h}.Append(sessionID, raw)
}

// runPlan executes an approved plan in order: the prepare steps on the workers, then
// the sessions; the first failure stops it. The card follows over SSE and the model
// gets the outcome as a note, then a turn to answer with the links or a better plan.
func (h *Hub) runPlan(user User, conciergeID string, p *planRecord) {
	ctx := h.planCtx
	save := func() {
		_ = h.store.SavePlan(p)
		h.publishPlan(p)
	}
	fail := func(i int, msg string) {
		p.Steps[i].State, p.Steps[i].Output = stepFailed, msg
		for j := i + 1; j < len(p.Steps); j++ {
			p.Steps[j].State = stepSkipped
		}
		p.State = PlanFailed
		p.Error = fmt.Sprintf("step %d of %d (%s): %s", i+1, len(p.Steps), p.Steps[i].Label, msg)
		save()
	}
	ws := &WebSession{User: user}
	failed := false
	for i := range p.Plan.Prepare {
		st := p.Plan.Prepare[i]
		p.Steps[i].State = stepRunning
		save()
		wi, err := h.workerInfoByName(st.Machine)
		if err == nil {
			err = wi.needFS()
		}
		if err == nil && (!proto.Inside(st.Path, wi.roots) || (st.Repo != "" && !proto.Inside(st.Repo, wi.roots))) {
			err = tools.Error("the path is no longer inside the machine's roots")
		}
		var data json.RawMessage
		if err == nil {
			wait := fsQuickWait
			if st.Action == proto.ActionClone || st.Action == proto.ActionWorktree {
				wait = fsGitWait
			}
			id := proto.NewID()
			data, err = h.fsCall(ctx, wi.m.ID, id, &proto.FSPrepare{RequestID: id, Action: st.Action, Path: st.Path, Repo: st.Repo,
				URL: st.URL, Branch: st.Branch, Base: st.Base}, wait)
		}
		if err != nil {
			fail(i, planErr(err))
			failed = true
			break
		}
		var out struct {
			Output string `json:"output"`
		}
		_ = json.Unmarshal(data, &out)
		p.Steps[i].State, p.Steps[i].Output = stepDone, out.Output
		save()
	}
	if !failed {
		for k := range p.Plan.Sessions {
			i := len(p.Plan.Prepare) + k
			ps := p.Plan.Sessions[k]
			p.Steps[i].State = stepRunning
			save()
			id, err := h.startPlanSession(ctx, ws, ps)
			if id != "" {
				p.Steps[i].SessionID = id
				_ = h.store.AddSpawned(conciergeID, id)
			}
			if err != nil {
				fail(i, planErr(err))
				failed = true
				break
			}
			p.Steps[i].State = stepDone
			save()
		}
	}
	if !failed {
		p.State = PlanDone
		save()
	}
	if ctx.Err() != nil {
		return // the hub is shutting down; a plan left running is failed at the next start
	}
	h.conciergeNote(conciergeID, planOutcome(p))
}

func planErr(err error) string {
	var te tools.Error
	if errors.As(err, &te) {
		return te.Error()
	}
	if msg := userMessage(err, ""); msg != "" {
		return msg
	}
	if errors.Is(err, context.Canceled) {
		return "the hub is shutting down"
	}
	return "failed"
}

// startPlanSession starts one session through newSession, waits for the worker to
// have it running, and sends the first prompt.
func (h *Hub) startPlanSession(ctx context.Context, ws *WebSession, ps PlanSession) (string, error) {
	ack := make(chan string, 1)
	id, err := h.newSession(ctx, ws, newSessionReq{MachineName: ps.Machine, Project: ps.CWD, Profile: ps.Profile,
		Model: ps.MainModel, Mode: ps.Mode, Title: ps.Title, InsideRoot: true, Ack: ack})
	if err != nil {
		return "", err
	}
	t := time.NewTimer(startAckWait)
	defer t.Stop()
	select {
	case msg := <-ack:
		if msg != "" {
			return id, userError("the machine refused the session: " + msg)
		}
	case <-t.C:
		return id, userError("the session did not start within 2 minutes")
	case <-ctx.Done():
		return id, ctx.Err()
	}
	if ps.FirstPrompt == "" {
		return id, nil
	}
	s, err := h.store.Session(id)
	if err != nil {
		return id, err
	}
	if err := h.sendToSession(ctx, s, &proto.UserMessage{SessionID: id, Text: ps.FirstPrompt}); err != nil {
		return id, userError("the session started but the first prompt could not be sent: " + err.Error())
	}
	h.store.Audit(ws.User.Sub, "message", fmt.Sprintf("%s len=%d", id, len(ps.FirstPrompt)))
	return id, nil
}

// planOutcome is the text the model (and the notice in the transcript) gets.
func planOutcome(p *planRecord) string {
	var b strings.Builder
	if p.State == PlanDone {
		fmt.Fprintf(&b, "Plan %s was approved and ran.\n", p.ID)
	} else {
		fmt.Fprintf(&b, "Plan %s was approved and failed at %s\n", p.ID, p.Error)
	}
	for _, st := range p.Steps {
		mark := map[string]string{stepDone: "✓", stepFailed: "✗", stepSkipped: "– not run:"}[st.State]
		line := mark + " " + st.Label
		if st.SessionID != "" && st.State == stepDone {
			line += " → [/s/" + st.SessionID + "](/s/" + st.SessionID + ")"
		}
		if st.State == stepFailed && st.Output != "" {
			line += ": " + st.Output
		}
		b.WriteString("\n- " + line)
	}
	return b.String()
}

// conciergeNote stores the outcome for the model and runs a turn on it once the
// session is free. Over the budget, or when the session is gone, the note is only stored.
func (h *Hub) conciergeNote(sessionID, text string) {
	for i := 0; i < 120; i++ {
		h.runner.Wait(sessionID)
		s, err := h.store.Session(sessionID)
		if err != nil {
			return
		}
		if h.checkBudget(s.OwnerSub) != nil {
			h.appendNote(sessionID, text)
			return
		}
		_, err = h.chatTurn(s, text, nil, "plan")
		if err == nil {
			return
		}
		if !errors.Is(err, chat.ErrBusy) {
			h.log.Warn("concierge note turn", "session", sessionID, "err", err)
			h.appendNote(sessionID, text)
			return
		}
		select {
		case <-h.planCtx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	h.appendNote(sessionID, text)
}

// spawnedViews are the right panel's list of the sessions a concierge started.
func (h *Hub) spawnedViews(s *Session) []map[string]any {
	out := []map[string]any{}
	for _, id := range h.store.Spawned(s.ID) {
		x, err := h.store.Session(id)
		if errors.Is(err, sql.ErrNoRows) || err != nil || x.OwnerSub != s.OwnerSub {
			continue
		}
		out = append(out, map[string]any{"id": x.ID, "title": x.Title, "state": x.State, "machine": x.MachineName})
	}
	return out
}

// planState is the hub's state for running plans.
type planState struct {
	planCtx    context.Context
	planCancel context.CancelFunc
	planWG     sync.WaitGroup
}
