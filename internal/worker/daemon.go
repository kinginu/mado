// Package worker is the per-machine daemon: it keeps an outbound WebSocket to the hub,
// owns the tmux server, discovers shims through their unix sockets, relays frames and
// runs the loopback model router.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/router"
	"github.com/kinginu/mado/internal/shim"
)

// Version is set by the main package.
var Version = "dev"

type Daemon struct {
	cfg    *Config
	log    *slog.Logger
	token  string
	tmux   Tmux
	router *router.Server

	claudeVersion string

	hubMu sync.Mutex
	hub   *websocket.Conn

	history *historyShipper // nil unless claude_history

	mu       sync.Mutex
	sessions map[string]*session
	starting map[string]bool // startSession in progress; discover keeps off
}

type session struct {
	id          string
	lc          *proto.LineConn
	conn        net.Conn
	state       string
	claudeSID   string
	hubSeq      int64 // last seq handed to the hub
	routerToken string
}

func New(cfg *Config, log *slog.Logger) (*Daemon, error) {
	if cfg.TmuxBin == "" {
		return nil, errors.New("tmux not found; install it (brew install tmux) or set tmux_bin")
	}
	if _, err := exec.LookPath(cfg.TmuxBin); err != nil {
		return nil, fmt.Errorf("tmux not found at %s; install it (brew install tmux) or set tmux_bin", cfg.TmuxBin)
	}
	if _, err := exec.LookPath(cfg.ClaudeBin); err != nil {
		return nil, fmt.Errorf("claude not found at %s; install Claude Code (https://code.claude.com/docs/en/setup) or set claude_bin", cfg.ClaudeBin)
	}
	b, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("machine token: %w (run `mado enroll` first)", err)
	}
	if fi, err := os.Stat(cfg.TokenFile); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must not be readable by group/others", cfg.TokenFile)
	}
	d := &Daemon{cfg: cfg, log: log, token: strings.TrimSpace(string(b)),
		tmux:     Tmux{Bin: cfg.TmuxBin, Socket: cfg.TmuxSocket, SystemdRun: UseScope(cfg.TmuxScope, os.Getenv, exec.LookPath), Log: log.Warn},
		sessions: map[string]*session{}, starting: map[string]bool{}}
	if out, err := exec.Command(cfg.ClaudeBin, "--version").Output(); err == nil {
		d.claudeVersion = strings.TrimSpace(string(out))
	}
	return d, nil
}

func (d *Daemon) Run(ctx context.Context) error {
	dirs := []string{d.cfg.RuntimeDir, d.cfg.SpoolDir}
	if d.cfg.Jobs {
		dirs = append(dirs, d.cfg.JobDir)
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	rs, err := router.Listen(d.cfg.RouterListen, router.New(d.log))
	if err != nil {
		return fmt.Errorf("router on %s: %w", d.cfg.RouterListen, err)
	}
	defer rs.Close()
	defer func() {
		// Leave the shims running; just hang up on them.
		d.mu.Lock()
		for _, s := range d.sessions {
			s.conn.Close()
		}
		d.mu.Unlock()
	}()
	d.router = rs
	d.log.Info("router listening", "addr", rs.Addr, "tmux_scope", d.tmux.SystemdRun != "")
	if strings.HasSuffix(d.cfg.RouterListen, ":0") {
		d.log.Warn("router_listen uses a random port: router sessions break when the daemon restarts")
	}

	if d.cfg.ClaudeHistory {
		d.history = newHistoryShipper(d)
		go d.history.run(ctx)
	}
	d.discover(ctx)
	backoff := backoffInitial
	for {
		up, err := d.connectOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		backoff = resetBackoff(backoff, up)
		d.log.Warn("hub connection ended", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = growBackoff(backoff)
	}
}

const (
	backoffInitial = time.Second
	backoffMax     = 30 * time.Second
	// A connection that stayed up this long counts as healthy and resets the backoff.
	backoffHealthyUp = 5 * time.Second
)

// resetBackoff returns the initial backoff when the last connection was up long enough.
func resetBackoff(cur, up time.Duration) time.Duration {
	if up >= backoffHealthyUp {
		return backoffInitial
	}
	return cur
}

// growBackoff doubles the backoff, capped at backoffMax.
func growBackoff(cur time.Duration) time.Duration {
	cur *= 2
	if cur > backoffMax {
		cur = backoffMax
	}
	return cur
}

// connectOnce runs one hub connection and returns how long it was established.
func (d *Daemon) connectOnce(ctx context.Context) (up time.Duration, err error) {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+d.token)
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	c, _, err := websocket.Dial(dctx, d.cfg.HubURL, &websocket.DialOptions{HTTPHeader: hdr})
	cancel()
	if err != nil {
		return 0, err
	}
	connected := time.Now()
	defer func() { up = time.Since(connected) }()
	defer c.CloseNow()
	c.SetReadLimit(16 << 20) // a digest's records arrive as one user_message (≤ 4 MiB of text)
	d.hubMu.Lock()
	d.hub = c
	d.hubMu.Unlock()
	defer func() {
		d.hubMu.Lock()
		d.hub = nil
		d.hubMu.Unlock()
	}()
	d.log.Info("connected to hub", "url", d.cfg.HubURL)

	caps := []string{"router", "subscription", "approve", "spool", "gc", proto.CapFS}
	caps = append(caps, proto.CapFSRead) // the file viewer (DESIGN §27)
	if d.history != nil {
		caps = append(caps, "history")
	}
	jobDir := ""
	if d.cfg.JobsReady(runtime.GOOS) {
		caps, jobDir = append(caps, proto.CapJobs), d.cfg.JobDir
	} else if d.cfg.Jobs {
		d.log.Warn("jobs = true but no subscription login found; not offering jobs", "credentials", filepath.Join(d.cfg.ClaudeHome, ".credentials.json"))
	}
	if err := d.send(&proto.Hello{Machine: d.cfg.MachineName, Version: Version, ClaudeVersion: d.claudeVersion,
		Capabilities: caps, Projects: ListProjects(d.cfg.ProjectRoots), Roots: d.cfg.ProjectRoots, JobDir: jobDir}); err != nil {
		return 0, err
	}
	d.discover(ctx)
	// The hub answers with profile refreshes, resyncs (from its own last contiguous
	// seq) and resumes for sessions that should be alive here but have no shim.
	if err := d.send(d.sessionsFrame()); err != nil {
		return 0, err
	}

	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-t.C:
				ictx, icancel := context.WithTimeout(pctx, 15*time.Second)
				err := c.Ping(ictx)
				icancel()
				if err != nil {
					c.CloseNow()
					return
				}
			}
		}
	}()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return 0, err
		}
		f, err := proto.Decode(b)
		if err != nil {
			d.log.Warn("bad hub frame", "err", err)
			continue
		}
		go d.handleHub(ctx, f)
	}
}

func (d *Daemon) send(f proto.Frame) error {
	b, err := proto.Encode(f)
	if err != nil {
		return err
	}
	d.hubMu.Lock()
	defer d.hubMu.Unlock()
	if d.hub == nil {
		return errors.New("hub not connected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return d.hub.Write(ctx, websocket.MessageText, b)
}

func (d *Daemon) ack(ref string, err error) {
	a := &proto.Ack{Ref: ref, OK: err == nil}
	if err != nil {
		a.Error = err.Error()
	}
	_ = d.send(a)
}

func (d *Daemon) sessionsFrame() *proto.Sessions {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := &proto.Sessions{Sessions: []proto.SessionInfo{}}
	for _, s := range d.sessions {
		f.Sessions = append(f.Sessions, proto.SessionInfo{SessionID: s.id, State: s.state, ClaudeSessionID: s.claudeSID, LastSeq: s.hubSeq})
	}
	f.Spools = SpoolIDs(d.cfg.SpoolDir, d.liveLocked)
	return f
}

// liveLocked: a shim is attached or being started. d.mu must be held.
func (d *Daemon) liveLocked(id string) bool {
	_, ok := d.sessions[id]
	return ok || d.starting[id]
}

func (d *Daemon) live(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.liveLocked(id)
}

func (d *Daemon) handleHub(ctx context.Context, f proto.Frame) {
	ref := proto.HeaderOf(f).ID
	switch m := f.(type) {
	case *proto.StartSession:
		if m.Refresh {
			d.ack(ref, d.refreshSession(m))
			return
		}
		d.ack(ref, d.startSession(ctx, m))
	case *proto.UserMessage:
		d.ack(ref, d.toShim(m.SessionID, proto.Local{T: proto.LUserMessage, Text: m.Text, Note: m.Note, Message: m.NoteText}))
	case *proto.SetPermissionMode:
		// DESIGN §25.3: the shim answers with mode_result; without a shim we answer.
		err := errors.New("permission mode " + m.Mode + " is not allowed")
		if proto.ClaudeModes[m.Mode] {
			err = d.toShim(m.SessionID, proto.Local{T: proto.LSetMode, RequestID: m.RequestID, Mode: m.Mode})
		}
		if err != nil {
			_ = d.send(&proto.ModeResult{SessionID: m.SessionID, RequestID: m.RequestID, Mode: m.Mode, Error: err.Error()})
		}
	case *proto.AppendNote:
		d.ack(ref, d.toShim(m.SessionID, proto.Local{T: proto.LNote, Note: m.Note, Text: m.Text}))
	case *proto.Interrupt:
		d.ack(ref, d.toShim(m.SessionID, proto.Local{T: proto.LInterrupt}))
	case *proto.StopSession:
		d.ack(ref, d.toShim(m.SessionID, proto.Local{T: proto.LStop}))
	case *proto.ApprovalResponse:
		switch m.Decision {
		case proto.DecisionAllow, proto.DecisionAllowSession, proto.DecisionDeny:
		default:
			d.ack(ref, errors.New("bad decision"))
			return
		}
		d.ack(ref, d.toShim(m.SessionID, proto.Local{T: proto.LApprovalResponse, RequestID: m.RequestID, Decision: m.Decision, Message: m.Message,
			UpdatedInput: m.UpdatedInput}))
	case *proto.ToolResult:
		if err := d.toShim(m.SessionID, proto.Local{T: proto.LToolResult, CallID: m.CallID, Text: m.Content, IsError: m.IsError}); err != nil {
			d.log.Info("tool result for a session without a shim", "session", m.SessionID)
		}
	case *proto.ListSessions:
		d.discover(ctx)
		_ = d.send(d.sessionsFrame())
	case *proto.Resync:
		d.ack(ref, d.toShim(m.SessionID, proto.Local{T: proto.LReplay, FromSeq: m.FromSeq}))
	case *proto.HistoryOffsets:
		if d.history != nil {
			d.history.offsets(m)
		}
	case *proto.HistoryRescan:
		if d.history != nil {
			d.history.poke()
		}
	case *proto.FSInspect:
		_ = d.send(d.fsInspect(ctx, m))
	case *proto.FSPrepare:
		_ = d.send(d.fsPrepare(ctx, m))
	case *proto.FSRead:
		_ = d.send(d.fsRead(m))
	case *proto.GC:
		if ids := CollectSpools(d.cfg.SpoolDir, m.SessionIDs, d.live, d.cfg.Retention(), time.Now()); len(ids) > 0 {
			d.log.Info("spools collected", "sessions", len(ids))
		}
	default:
		d.ack(ref, fmt.Errorf("unexpected frame %s", proto.HeaderOf(f).T))
	}
}

func (d *Daemon) toShim(id string, m proto.Local) error {
	d.mu.Lock()
	s := d.sessions[id]
	d.mu.Unlock()
	if s == nil {
		return errors.New("no such session on this machine")
	}
	return s.lc.Write(m)
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// refreshSession re-registers a live session's router token with the profile and
// keys the hub pushes after a reconnect. Nothing is spawned.
func (d *Daemon) refreshSession(m *proto.StartSession) error {
	d.mu.Lock()
	s := d.sessions[m.SessionID]
	d.mu.Unlock()
	if s == nil {
		return errors.New("no live shim for this session")
	}
	if m.Profile.Kind != proto.KindRouter {
		return nil
	}
	if err := router.Validate(m.Profile); err != nil {
		return err
	}
	if s.routerToken == "" {
		return errors.New("session has no persisted router token")
	}
	d.router.Register(s.routerToken, s.id, m.Profile)
	d.log.Info("router profile refreshed", "session", s.id, "profile", m.Profile.Name)
	return nil
}

func (d *Daemon) startSession(ctx context.Context, m *proto.StartSession) error {
	if !proto.ValidSessionID(m.SessionID) {
		return errors.New("bad session id")
	}
	if ctx.Err() != nil {
		return errors.New("worker is shutting down")
	}
	if m.SeqBase < 0 || m.FromSeq < 0 {
		return errors.New("bad seq")
	}
	d.mu.Lock()
	_, exists := d.sessions[m.SessionID]
	busy := d.starting[m.SessionID]
	if !exists && !busy {
		d.starting[m.SessionID] = true
	}
	d.mu.Unlock()
	if exists || busy {
		return errors.New("session already running")
	}
	defer func() {
		d.mu.Lock()
		delete(d.starting, m.SessionID)
		d.mu.Unlock()
	}()
	sock := shim.SockPath(d.cfg.RuntimeDir, m.SessionID)
	params := SpawnParams{ClaudeBin: d.cfg.ClaudeBin, ShimBin: d.cfg.ShimBin, SocketPath: sock,
		ClaudeConfigBase: d.cfg.ClaudeConfigBase}
	if d.cfg.Jobs {
		params.JobDir = d.cfg.JobDir
	}
	var token string
	if m.Profile.Kind == proto.KindRouter {
		token = randomToken()
		params.RouterURL, params.RouterToken = d.router.URL(), token
	}
	spawn, err := BuildSpawn(m, d.cfg.ProjectRoots, params)
	if err != nil {
		return err
	}
	spawn.RouterToken = token
	if token != "" {
		d.router.Register(token, m.SessionID, m.Profile)
	}
	fail := func(err error) error {
		if token != "" {
			d.router.Unregister(token)
		}
		return err
	}
	argv := ShimArgv(d.cfg.ShimBin, m.SessionID, d.cfg.RuntimeDir, d.cfg.SpoolDir, m.ResumeClaudeSessionID, m.SeqBase)
	if err := d.tmux.StartWindow(ctx, WindowName(m.SessionID), argv); err != nil {
		return fail(err)
	}
	var s *session
	for i := 0; i < 100; i++ {
		if s, err = d.attach(ctx, m.SessionID, m.FromSeq, false, token); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return fail(fmt.Errorf("shim did not come up: %w", err))
	}
	if err := s.lc.Write(proto.Local{T: proto.LStart, Spawn: spawn}); err != nil {
		return fail(err)
	}
	d.log.Info("session started", "session", m.SessionID, "profile", m.Profile.Name, "cwd", spawn.CWD,
		"resume", m.ResumeClaudeSessionID)
	return nil
}

// ShimArgv is the command line of the tmux window for a session.
func ShimArgv(shimBin, sessionID, runtimeDir, spoolDir, resumeID string, seqBase int64) []string {
	argv := []string{shimBin, "shim", "--session", sessionID, "--runtime-dir", runtimeDir, "--spool-dir", spoolDir}
	if resumeID != "" {
		argv = append(argv, "--resume", resumeID)
	}
	if seqBase > 0 {
		argv = append(argv, "--seq-base", fmt.Sprint(seqBase))
	}
	return argv
}

// discover attaches to every shim socket in the runtime dir that we are not attached to.
func (d *Daemon) discover(ctx context.Context) {
	ents, err := os.ReadDir(d.cfg.RuntimeDir)
	if err != nil {
		return
	}
	windows := map[string]bool{}
	for _, w := range d.tmux.Windows(ctx) {
		windows[w] = true
	}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".sock")
		if !ok || !proto.ValidSessionID(id) {
			continue
		}
		d.mu.Lock()
		_, have := d.sessions[id]
		have = have || d.starting[id]
		d.mu.Unlock()
		if have {
			continue
		}
		// Hold: the hub's resync tells the shim where to start.
		if _, err := d.attach(ctx, id, 0, true, shim.ReadToken(d.cfg.SpoolDir, id)); err != nil {
			d.log.Info("stale shim socket", "session", id, "err", err, "window", windows[WindowName(id)])
		}
	}
}

// attach connects to a shim as the daemon and starts relaying.
func (d *Daemon) attach(ctx context.Context, id string, fromSeq int64, hold bool, token string) (*session, error) {
	sock := shim.SockPath(d.cfg.RuntimeDir, id)
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil, err
	}
	lc := proto.NewLineConn(c)
	if err := lc.Write(proto.Local{T: proto.LHello, Role: "daemon", FromSeq: fromSeq, Hold: hold}); err != nil {
		c.Close()
		return nil, err
	}
	s := &session{id: id, lc: lc, conn: c, state: proto.StateStarting, hubSeq: fromSeq, routerToken: token}
	d.mu.Lock()
	if old, ok := d.sessions[id]; ok {
		d.mu.Unlock()
		c.Close()
		return old, nil
	}
	d.sessions[id] = s
	d.mu.Unlock()
	if hold {
		// A shim that outlived the previous daemon: its claude still uses the token it
		// was given. Accept it now; the hub's profile refresh fills in the upstreams.
		if token != "" {
			d.router.Reserve(token, id)
		}
		d.log.Info("attached to running shim", "session", id, "router_token", s.routerToken != "")
	}
	go d.relay(ctx, s)
	return s, nil
}

// relay forwards shim frames to the hub until the shim goes away.
func (d *Daemon) relay(ctx context.Context, s *session) {
	defer func() {
		s.conn.Close()
		d.mu.Lock()
		delete(d.sessions, s.id)
		d.mu.Unlock()
		if s.routerToken != "" {
			d.router.Unregister(s.routerToken)
		}
		if ctx.Err() != nil {
			return // shutting down: the shims may die with us; leave them resumable
		}
		if s.state != proto.StateExited && s.state != proto.StateDetached {
			// Lost without saying goodbye (tmux server gone, shim killed). Tell the
			// hub what is still alive; it resumes the missing session (DESIGN §8).
			d.log.Warn("shim lost", "session", s.id, "state", s.state)
			_ = d.send(d.sessionsFrame())
		}
	}()
	for {
		var m proto.Local
		if err := s.lc.Read(&m); err != nil {
			return
		}
		switch m.T {
		case proto.LEvent:
			if err := d.send(&proto.Event{SessionID: s.id, Seq: m.Seq, Raw: m.Raw}); err == nil {
				d.mu.Lock()
				if m.Seq > s.hubSeq {
					s.hubSeq = m.Seq
				}
				d.mu.Unlock()
			}
		case proto.LState:
			d.mu.Lock()
			s.state, s.claudeSID = m.State, m.ClaudeSessionID
			d.mu.Unlock()
			_ = d.send(&proto.SessionState{SessionID: s.id, State: m.State, ClaudeSessionID: m.ClaudeSessionID,
				ExitCode: m.ExitCode, Error: m.Error})
		case proto.LApprovalRequest:
			input := m.Input
			if len(input) == 0 || !json.Valid(input) {
				input = json.RawMessage("{}")
			}
			_ = d.send(&proto.ApprovalRequest{SessionID: s.id, RequestID: m.RequestID, ToolName: m.ToolName, Input: input})
		case proto.LModeResult:
			_ = d.send(&proto.ModeResult{SessionID: s.id, RequestID: m.RequestID, Mode: m.Mode, OK: m.Error == "", Error: m.Error})
		case proto.LToolCall:
			args := m.Input
			if len(args) == 0 || !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			if err := d.send(&proto.ToolCall{SessionID: s.id, CallID: m.CallID, Name: m.ToolName, Args: args}); err != nil {
				_ = s.lc.Write(proto.Local{T: proto.LToolResult, CallID: m.CallID, Text: "mado: the hub is not connected", IsError: true})
			}
		}
	}
}
