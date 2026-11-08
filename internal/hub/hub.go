// Package hub is the web UI and the endpoint workers connect to.
package hub

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/kinginu/mado/internal/chat"
	"github.com/kinginu/mado/internal/files"
	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/policy"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/tools"
	"github.com/kinginu/mado/internal/usage"
)

//go:embed templates/login.html
var templateFS embed.FS

type Options struct {
	// LocalLogin offers POST /local-login (DESIGN §19.3); the caller checks
	// LocalLoginAllowed first.
	LocalLogin bool
	// LocalMode is set by `mado local`: the hub serves loopback only, so the not-https
	// cookie warning is not logged.
	LocalMode bool
	Auth      Authenticator // nil: OIDC from config
	Log       *slog.Logger
	// ResumeEvery limits automatic resumes of one session (default 30 s), so a
	// resume that dies at once cannot loop.
	ResumeEvery time.Duration
	// Clock drives the rate-limit auto-resume timers (DESIGN §26); nil = real time.
	Clock Clock
}

type Hub struct {
	cfg           *Config
	store         *Store
	secrets       *Secrets
	auth          Authenticator
	log           *slog.Logger
	tmpl          *template.Template
	localLogin    bool
	resumeEvery   time.Duration
	secureCookies bool
	broker        *broker
	models        modelCache
	runner        *chat.Runner
	tools         *tools.Registry // [chat.tools], for chat and code sessions
	blobs         files.Blobs
	policy        *policy.Policy // [auto] rules (DESIGN §18)
	classMu       sync.RWMutex
	classifier    *policy.Classifier // nil: no classifier; guarded by classMu (DESIGN §28.3)
	unpriced      sync.Map           // models without a [[prices]] entry, logged once
	pricesMu      sync.Mutex
	prices        usage.Prices       // cache of the prices table; nil = load (DESIGN §28.2)
	upModels      upstreamModelCache // GET /api/admin/upstream-models (DESIGN §28.6)
	stop, done    chan struct{}      // housekeeping
	planState                        // running concierge plans (DESIGN §20)
	inbox         *inboxState        // nil: [inbox] off (DESIGN §23)
	jobs          jobState           // running digest jobs (DESIGN §23.6)
	modes         modeWaiters        // set_permission_mode requests (DESIGN §25.3)
	rl            *rateLimits        // rate-limit auto-resume (DESIGN §26)

	mu      sync.Mutex
	workers map[string]*workerConn // machine id -> connection
	starts  map[string]string      // start_session frame id -> session id
	resumed map[string]time.Time   // session id -> last automatic resume
	resyncs map[string]time.Time   // session id -> last gap-triggered resync
	fsWaits map[string]fsWaiter    // fs request id -> its waiter (DESIGN §20.2)
	acks    map[string]chan string // start_session frame id -> "" or the worker's error
}

type workerConn struct {
	machineID string
	c         *websocket.Conn
	mu        sync.Mutex
	// From hello, guarded by Hub.mu: the capabilities and the project roots.
	caps, roots []string
	jobDir      string // with capability jobs: the cwd of job sessions (DESIGN §23.6)
}

func (w *workerConn) send(ctx context.Context, f proto.Frame) error {
	b, err := proto.Encode(f)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return w.c.Write(ctx, websocket.MessageText, b)
}

func New(cfg *Config, store *Store, secrets *Secrets, opt Options) (*Hub, error) {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if cfg.OIDC.AdminGroup == "" {
		cfg.OIDC.AdminGroup = DefaultAdminGroup
	}
	if err := cfg.Budgets.applyDefaults(); err != nil {
		return nil, err
	}
	if opt.ResumeEvery == 0 {
		opt.ResumeEvery = 30 * time.Second
	}
	h := &Hub{cfg: cfg, store: store, secrets: secrets, auth: opt.Auth, log: opt.Log, localLogin: opt.LocalLogin, resumeEvery: opt.ResumeEvery,
		broker: newBroker(), workers: map[string]*workerConn{}, starts: map[string]string{},
		resumed: map[string]time.Time{}, resyncs: map[string]time.Time{}, fsWaits: map[string]fsWaiter{},
		acks: map[string]chan string{}, rl: newRateLimits(opt.Clock)}
	h.planCtx, h.planCancel = context.WithCancel(context.Background())
	if err := store.ResetMachinesConnected(); err != nil { // no worker is connected yet (M21)
		return nil, err
	}
	if h.auth == nil {
		h.auth = NewOIDCAuth(cfg.OIDC)
	}
	h.secureCookies = strings.HasPrefix(cfg.BaseURL, "https://")
	if !h.secureCookies && !opt.LocalMode {
		h.log.Warn("base_url is not https: cookies are not marked Secure")
	}
	t, err := template.New("").ParseFS(templateFS, "templates/login.html")
	if err != nil {
		return nil, err
	}
	h.tmpl = t
	// Seed profiles from config; the built-in subscription profile always exists.
	if err := store.PutProfileFrom(proto.Profile{Name: "subscription", Kind: proto.KindSubscription}, SourceBuiltin); err != nil {
		return nil, err
	}
	ps := make([]proto.Profile, 0, len(cfg.Profiles))
	for _, pc := range cfg.Profiles {
		p, err := pc.toProto()
		if err != nil {
			return nil, err
		}
		for name, u := range p.Upstreams {
			if u.KeyRef != "" {
				if _, ok := secrets.Keys[u.KeyRef]; !ok {
					return nil, fmt.Errorf("profile %s upstream %s: key_ref %q not in secrets file", p.Name, name, u.KeyRef)
				}
			}
		}
		ps = append(ps, p)
	}
	// hub.toml entries are seeded under the override rule (DESIGN §28.1).
	if err := store.SeedProfiles(ps); err != nil {
		return nil, err
	}
	if err := store.SeedPrices(cfg.Prices); err != nil {
		return nil, err
	}
	if err := h.setupChat(); err != nil {
		return nil, err
	}
	if err := h.setupAuto(); err != nil {
		return nil, err
	}
	if err := h.setupConcierge(); err != nil {
		return nil, err
	}
	if err := h.setupInbox(); err != nil {
		return nil, err
	}
	h.startHousekeeping()
	h.armRateLimits()
	return h, nil
}

func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /login", h.handleLogin)
	mux.HandleFunc("GET /callback", h.handleCallback)
	mux.HandleFunc("POST /local-login", h.handleLocalLogin)
	mux.HandleFunc("POST /logout", h.handleLogout)
	mux.HandleFunc("POST /enroll", h.handleEnroll)
	mux.HandleFunc("GET /ws/worker", h.handleWorkerWS)
	h.uiRoutes(mux)
	mux.HandleFunc("GET /s/{id}/events", h.requireUser(h.handleSSE))
	h.apiRoutes(mux)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// --- enrollment

func (h *Hub) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Code string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || !validName(req.Name) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tok, err := h.store.Enroll(req.Name, req.Code)
	if err != nil {
		h.store.Audit("", "enroll_failed", req.Name)
		time.Sleep(500 * time.Millisecond)
		http.Error(w, "invalid code", http.StatusForbidden)
		return
	}
	h.store.Audit("", "enroll", req.Name)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": tok})
}

// --- worker websocket

func (h *Hub) handleWorkerWS(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || tok == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	m, err := h.store.MachineByToken(tok)
	if err != nil {
		if name := h.store.RemovedMachineName(tok); name != "" {
			h.log.Warn("worker rejected: its machine was removed; enroll it again to reconnect", "machine", name)
		}
		time.Sleep(200 * time.Millisecond)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Workers are not browsers; the Origin check of the library is for browsers.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(64 << 20)
	wc := &workerConn{machineID: m.ID, c: c}
	// The connected flag (M21) is set and cleared under h.mu, so a replaced connection
	// cannot clear the flag of the one that replaced it. Setting it fails when the
	// machine was removed while this worker authenticated.
	h.mu.Lock()
	if !h.store.SetMachineConnected(m.ID, true) {
		h.mu.Unlock()
		h.log.Warn("worker rejected: its machine was removed; enroll it again to reconnect", "machine", m.Name)
		c.Close(websocket.StatusPolicyViolation, "machine removed")
		return
	}
	if old := h.workers[m.ID]; old != nil {
		old.c.Close(websocket.StatusPolicyViolation, "replaced by a new connection")
	}
	h.workers[m.ID] = wc
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.workers[m.ID] == wc {
			delete(h.workers, m.ID)
			h.store.SetMachineConnected(m.ID, false)
		}
		h.mu.Unlock()
	}()
	h.log.Info("worker connected", "machine", m.Name)
	ctx := r.Context()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			h.log.Info("worker disconnected", "machine", m.Name, "err", err)
			return
		}
		f, err := proto.Decode(b)
		if err != nil {
			h.log.Warn("bad worker frame", "machine", m.Name, "err", err)
			continue
		}
		h.handleWorkerFrame(ctx, wc, f)
	}
}

func (h *Hub) worker(machineID string) *workerConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.workers[machineID]
}

func (h *Hub) online(machineID string) bool { return h.worker(machineID) != nil }

// ownsSession checks that a frame's session belongs to the sending machine.
func (h *Hub) ownsSession(machineID, sessionID string) bool {
	m, err := h.store.SessionMachine(sessionID)
	return err == nil && m == machineID
}

func (h *Hub) handleWorkerFrame(ctx context.Context, wc *workerConn, f proto.Frame) {
	switch m := f.(type) {
	case *proto.Hello:
		_ = h.store.UpdateMachineHello(wc.machineID, m)
		var roots []string
		for _, r := range m.Roots {
			if proto.CleanAbs(r) {
				roots = append(roots, r)
			}
		}
		jobDir := ""
		if slices.Contains(m.Capabilities, proto.CapJobs) && proto.CleanAbs(m.JobDir) {
			jobDir = m.JobDir
		}
		h.mu.Lock()
		wc.caps, wc.roots, wc.jobDir = m.Capabilities, roots, jobDir
		h.mu.Unlock()
		if slices.Contains(m.Capabilities, "history") {
			if states, err := h.store.HistoryStates(wc.machineID); err == nil {
				_ = wc.send(ctx, &proto.HistoryOffsets{Files: states})
			}
		}
	case *proto.HistorySession:
		h.onHistory(ctx, wc, m)
	case *proto.Sessions:
		h.store.TouchMachine(wc.machineID)
		h.reconcile(ctx, wc, m)
	case *proto.SessionState:
		ok, err := h.store.UpdateSessionState(wc.machineID, m)
		if err == nil && ok {
			h.publishState(m.SessionID, m.State)
			h.jobExit(m)
		}
	case *proto.Event:
		if !h.ownsSession(wc.machineID, m.SessionID) || m.Seq <= 0 || !json.Valid(m.Raw) {
			return
		}
		inserted, err := h.store.InsertEvent(m.SessionID, m.Seq, m.Raw)
		if err != nil {
			h.log.Error("insert event", "err", err)
			return
		}
		if inserted {
			h.publishEvent(m.SessionID, m.Seq, m.Raw)
			h.recordCodeUsage(m.SessionID, m.Seq, m.Raw)
			h.rateLimitEvent(m.SessionID, m.Seq, m.Raw)
			h.jobEvent(m.SessionID, m.Raw)
			if m.Seq > 1 && !h.store.HasEvent(m.SessionID, m.Seq-1) {
				h.resyncGap(ctx, wc, m.SessionID)
			}
		}
	case *proto.ApprovalRequest:
		if !h.ownsSession(wc.machineID, m.SessionID) || m.RequestID == "" {
			return
		}
		if h.jobApproval(ctx, wc, m) {
			return
		}
		h.onApprovalRequest(ctx, wc, m)
	case *proto.ToolCall:
		if !h.ownsSession(wc.machineID, m.SessionID) || m.CallID == "" || len(m.CallID) > 64 {
			return
		}
		if m.Name == inbox.ToolProposeTask {
			go h.onJobToolCall(ctx, wc, m)
			return
		}
		go h.onToolCall(ctx, wc, m)
	case *proto.FSResult:
		h.onFSResult(wc, m)
	case *proto.ModeResult:
		h.onModeResult(wc, m)
	case *proto.Ack:
		h.modeAck(m)
		h.mu.Lock()
		sid, isStart := h.starts[m.Ref]
		delete(h.starts, m.Ref)
		ack := h.acks[m.Ref]
		delete(h.acks, m.Ref)
		h.mu.Unlock()
		if ack != nil {
			ack <- m.Error // buffered; the waiter may have given up
		}
		if !m.OK {
			h.log.Warn("worker refused frame", "ref", m.Ref, "err", m.Error)
			if isStart {
				h.store.SetSessionError(sid, m.Error)
				h.publishState(sid, proto.StateExited+": "+m.Error)
			}
		}
	}
}

// codeToolTimeout leaves the worker's 30 s wait room for the answer to travel.
const codeToolTimeout = 25 * time.Second

// onToolCall runs a web tool for a code session (DESIGN §17.12). Only the tools the
// hub offers code sessions are run; searches cost no tokens, so no budget applies.
func (h *Hub) onToolCall(ctx context.Context, wc *workerConn, m *proto.ToolCall) {
	res := &proto.ToolResult{SessionID: m.SessionID, CallID: m.CallID}
	if !slices.Contains(h.tools.CodeNames(), m.Name) {
		res.Content, res.IsError = "mado: the tool "+m.Name+" is not enabled on this hub", true
	} else {
		tctx, cancel := context.WithTimeout(ctx, codeToolTimeout)
		out, err := h.tools.Run(tctx, m.Name, m.Args)
		cancel()
		switch {
		case err == nil:
			res.Content = out.Text
		case errors.As(err, new(tools.Error)):
			res.Content, res.IsError = "Error: "+err.Error(), true
		default:
			res.Content, res.IsError = "Error: the tool failed or took too long", true
		}
	}
	if err := wc.send(ctx, res); err != nil {
		h.log.Warn("tool result not delivered", "session", m.SessionID, "err", err)
	}
}

func approvalRule(tool string, input json.RawMessage) string {
	if tool == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &in)
		return "Bash:" + in.Command
	}
	return tool
}

// onApprovalRequest stores a request and runs the policy (DESIGN §18.2): a question
// (AskUserQuestion, §21.3) always waits for the user; an earlier "allow for session"
// answers it at once; in mado auto mode the rules and the classifier may answer it;
// otherwise it waits for the user.
func (h *Hub) onApprovalRequest(ctx context.Context, wc *workerConn, m *proto.ApprovalRequest) {
	a := Approval{SessionID: m.SessionID, RequestID: m.RequestID, ToolName: m.ToolName, Input: string(m.Input),
		AfterSeq: h.store.MaxSeq(m.SessionID)}
	if err := h.store.InsertApproval(a); err != nil {
		h.log.Error("insert approval", "err", err)
		return
	}
	s, err := h.store.Session(m.SessionID)
	auto := err == nil && h.autoOn(s)
	// A question is for the user (DESIGN §21.3): no session rule, rule or classifier
	// answers it, not even an "allow for session" given before M13.
	if m.ToolName == ToolAskUserQuestion {
		if auto {
			h.autoAsk(s, a, reasonQuestion)
			return
		}
		a.State = "pending"
		h.publishApproval(m.SessionID, a)
		return
	}
	if h.store.HasSessionRule(m.SessionID, approvalRule(m.ToolName, m.Input)) {
		decided := false
		if auto {
			decided, _ = h.store.DecideAuto(m.SessionID, m.RequestID, proto.DecisionAllowSession, "rule", string(policy.Allow), "allowed for this session earlier")
		} else {
			decided, _ = h.store.DecideApproval(m.SessionID, m.RequestID, proto.DecisionAllowSession, "rule")
		}
		if decided {
			_ = wc.send(ctx, &proto.ApprovalResponse{SessionID: m.SessionID, RequestID: m.RequestID, Decision: proto.DecisionAllow})
			a.State, a.Decision, a.DecidedBy = "decided", "allow_session (remembered)", "rule"
			if auto {
				a.Auto, a.Reason = string(policy.Allow), "allowed for this session earlier"
			}
			h.publishApproval(m.SessionID, a)
			return
		}
	}
	if auto {
		h.autoApproval(s, a)
		return
	}
	a.State = "pending"
	h.publishApproval(m.SessionID, a)
}

// sendToSession sends a frame to the machine that runs the session.
func (h *Hub) sendToSession(ctx context.Context, s *Session, f proto.Frame) error {
	wc := h.worker(s.MachineID)
	if wc == nil {
		return errors.New("machine is offline")
	}
	return wc.send(ctx, f)
}

// resolveProfile adds the upstream keys from the secrets file.
func (h *Hub) resolveProfile(p proto.Profile) proto.Profile {
	if p.Kind != proto.KindRouter {
		return p
	}
	r := p
	r.Upstreams = make(map[string]proto.Upstream, len(p.Upstreams))
	for k, u := range p.Upstreams {
		if u.KeyRef != "" {
			u.Key = h.secrets.Keys[u.KeyRef]
		}
		r.Upstreams[k] = u
	}
	return r
}

// resumable states: claude was alive when we last heard of it.
var resumable = map[string]bool{proto.StateRunning: true, proto.StateIdle: true, proto.StateWaitingApproval: true}

// reconcile runs whenever a worker lists its live sessions (on connect, and when a
// shim is lost). Live sessions get their profile and keys again (the worker keeps
// them in memory only) and a resync from the hub's last contiguous seq. Sessions that
// should be alive but have no shim are resumed with `shim --resume` (DESIGN §8).
func (h *Hub) reconcile(ctx context.Context, wc *workerConn, m *proto.Sessions) {
	live := map[string]bool{}
	for _, s := range m.Sessions {
		live[s.SessionID] = true
	}
	ss, err := h.store.SessionsOnMachine(wc.machineID)
	if err != nil {
		h.log.Error("reconcile", "err", err)
		return
	}
	for _, s := range ss {
		if s.Kind == KindDigest {
			// A job runs one turn for its executor; it is never resumed, and a live
			// one nobody waits for (the hub restarted) is ended.
			job := h.jobs.get(s.ID)
			switch {
			case live[s.ID] && job == nil:
				_ = wc.send(ctx, &proto.StopSession{SessionID: s.ID})
			case live[s.ID]:
			case job == nil && s.State != proto.StateExited && s.State != proto.StateDetached:
				h.store.SetSessionError(s.ID, "the digest job ended without the hub")
				h.publishState(s.ID, proto.StateExited)
			case job != nil && resumable[s.State]:
				job.note(jobNote{exited: true, err: "the job's shim was lost"})
			}
			if live[s.ID] {
				_ = wc.send(ctx, &proto.Resync{SessionID: s.ID, FromSeq: h.store.ContiguousSeq(s.ID)})
			}
			continue
		}
		if live[s.ID] {
			if prof, err := h.store.Profile(s.ProfileName); err == nil && prof.Kind == proto.KindRouter {
				_ = wc.send(ctx, &proto.StartSession{SessionID: s.ID, CWD: s.CWD, Profile: h.resolveProfile(*prof),
					MainModel: s.MainModel, Refresh: true})
			}
			_ = wc.send(ctx, &proto.Resync{SessionID: s.ID, FromSeq: h.store.ContiguousSeq(s.ID)})
			continue
		}
		// starting without a claude id: a new session the worker is still bringing up.
		// starting with one: a resume that never came up (retried after resumeEvery).
		if !resumable[s.State] && !(s.State == proto.StateStarting && s.ClaudeSessionID != "") {
			continue
		}
		if s.ClaudeSessionID == "" {
			h.store.SetSessionError(s.ID, "the session was lost before claude reported its session id")
			h.publishState(s.ID, proto.StateExited)
			continue
		}
		h.mu.Lock()
		last, seen := h.resumed[s.ID]
		ok := !seen || time.Since(last) > h.resumeEvery
		if ok {
			h.resumed[s.ID] = time.Now()
		}
		h.mu.Unlock()
		if !ok {
			continue
		}
		h.resume(ctx, wc, s)
	}
	if dead := h.deadSpools(wc.machineID, m.Spools, live); len(dead) > 0 {
		_ = wc.send(ctx, &proto.GC{SessionIDs: dead})
	}
}

// deadSpools picks the spools the worker may delete: sessions that ended (exited,
// detached) or that the hub does not know on this machine. Live shims never qualify.
func (h *Hub) deadSpools(machineID string, spools []string, live map[string]bool) []string {
	var dead []string
	for _, id := range spools {
		if live[id] || !proto.ValidSessionID(id) {
			continue
		}
		s, err := h.store.Session(id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			continue // a database error is not "unknown"
		}
		if err != nil || s.MachineID != machineID || s.State == proto.StateExited || s.State == proto.StateDetached {
			dead = append(dead, id)
		}
	}
	return dead
}

func (h *Hub) resume(ctx context.Context, wc *workerConn, s *Session) {
	prof, err := h.store.Profile(s.ProfileName)
	if err != nil {
		h.store.SetSessionError(s.ID, "cannot resume: profile "+s.ProfileName+" is gone")
		return
	}
	h.store.ExpireApprovals(s.ID) // the claude that asked is gone
	mode := claudeMode(s, prof)
	frame := &proto.StartSession{Header: proto.Header{ID: proto.NewID()}, SessionID: s.ID, CWD: s.CWD,
		Profile: h.resolveProfile(*prof), MainModel: s.MainModel, PermissionMode: mode,
		ResumeClaudeSessionID: s.ClaudeSessionID, SeqBase: h.store.MaxSeq(s.ID), FromSeq: h.store.ContiguousSeq(s.ID),
		Tools: h.tools.CodeNames()}
	h.mu.Lock()
	h.starts[frame.ID] = s.ID
	h.mu.Unlock()
	h.store.SetSessionState(s.ID, proto.StateStarting)
	h.publishState(s.ID, proto.StateStarting+" (resuming)")
	if err := wc.send(ctx, frame); err != nil {
		h.log.Warn("resume send", "session", s.ID, "err", err)
		return
	}
	h.store.Audit("", "session_resume", s.ID+" "+s.ClaudeSessionID)
	h.log.Info("resuming session", "session", s.ID, "claude_session", s.ClaudeSessionID)
}

// resyncGap asks for missing events at most every few seconds per session.
func (h *Hub) resyncGap(ctx context.Context, wc *workerConn, sessionID string) {
	h.mu.Lock()
	last := h.resyncs[sessionID]
	ok := time.Since(last) > 5*time.Second
	if ok {
		h.resyncs[sessionID] = time.Now()
	}
	h.mu.Unlock()
	if ok {
		_ = wc.send(ctx, &proto.Resync{SessionID: sessionID, FromSeq: h.store.ContiguousSeq(sessionID)})
	}
}

// validHistoryFrame checks the shape of a history_session; the content of the lines is
// opaque (stored raw, rendered tolerantly).
func validHistoryFrame(m *proto.HistorySession) bool {
	if m.Path == "" || len(m.Path) > 4096 || !filepath.IsAbs(m.Path) || len(m.ConfigDir) > 4096 ||
		!proto.ValidSessionID(m.ClaudeSessionID) || len(m.CWD) > 4096 || len(m.Title) > 1024 ||
		m.Offset < 0 || m.End < m.Offset || len(m.Lines) != len(m.LineOffsets) {
		return false
	}
	prev := m.Offset - 1
	for _, off := range m.LineOffsets {
		if off <= prev || off >= m.End {
			return false
		}
		prev = off
	}
	return true
}

func (h *Hub) onHistory(ctx context.Context, wc *workerConn, m *proto.HistorySession) {
	if !validHistoryFrame(m) {
		h.log.Warn("bad history frame", "machine", wc.machineID, "path", m.Path)
		return
	}
	st, err := h.store.AppendHistory(wc.machineID, m)
	if errors.Is(err, errHistoryOffset) {
		// The worker's view is off (hub restored, frame lost): tell it where we stand.
		_ = wc.send(ctx, &proto.HistoryOffsets{Files: []proto.HistoryFileState{*st}, Partial: true})
		return
	}
	if err != nil {
		h.log.Error("store history", "err", err)
	}
}
