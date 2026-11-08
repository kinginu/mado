// Package shim runs inside a tmux window. It owns one `claude -p` child in stream-json
// mode, spools every event to $XDG_RUNTIME_DIR/mado/<session>.spool, serves the
// spool and live events on <session>.sock, and prints a readable log to its tty.
package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/render"
)

// Termination causes, set by the caller through context.WithCancelCause.
var (
	// ErrDetach: the user stopped the shim on purpose (Ctrl-C in the tmux window).
	// The hub marks the session detached and never resumes it by itself.
	ErrDetach = errors.New("shim detached by the user")
	// ErrLost: the shim was killed from outside (tmux server gone, shutdown). The
	// session stays resumable: no exited/detached state is reported.
	ErrLost = errors.New("shim terminated by a signal")
)

type Config struct {
	SessionID  string
	RuntimeDir string // socket
	SpoolDir   string // spool and token file; default RuntimeDir
	// ResumeID is the claude session id this shim resumes (`--resume`). The spawn
	// spec must carry the same id; it is added if missing.
	ResumeID string
	// SeqBase: number events after this seq even if the spool is gone.
	SeqBase int64
	TTY     io.Writer     // readable log; os.Stdout in a tmux window
	Linger  time.Duration // how long to wait for the daemon after claude exits
	Log     *slog.Logger
	// ControlTimeout is how long a control request (set_permission_mode, DESIGN
	// §25.3) waits for claude's control_response; default 5 s.
	ControlTimeout time.Duration
}

// SpoolEntry is one line of the spool file.
type SpoolEntry struct {
	Seq int64           `json:"seq"`
	TS  time.Time       `json:"ts"`
	Raw json.RawMessage `json:"raw"`
}

// SockPath is the shim's unix socket.
func SockPath(runtimeDir, sessionID string) string {
	return filepath.Join(runtimeDir, sessionID+".sock")
}

// SpoolPath is the event spool.
func SpoolPath(spoolDir, sessionID string) string {
	return filepath.Join(spoolDir, sessionID+".spool")
}

// TokenPath holds the session's router bearer token (mode 0600) while claude runs.
func TokenPath(spoolDir, sessionID string) string {
	return filepath.Join(spoolDir, sessionID+".token")
}

// ReadToken returns the persisted router token of a session, or "".
func ReadToken(spoolDir, sessionID string) string {
	p := TokenPath(spoolDir, sessionID)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeToken(path, tok string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type shim struct {
	cfg   Config
	log   *slog.Logger
	spool *os.File

	mu        sync.Mutex
	base      int64 // SeqBase
	ending    error // ErrDetach / ErrLost once termination started
	events    []SpoolEntry
	changed   chan struct{} // closed and replaced on every append / state change
	state     string
	claudeSID string
	exitCode  *int
	exitErr   string
	daemons   map[*daemonConn]bool
	pending   map[string]*pendingApproval
	calls     map[string]*proto.LineConn // tool calls waiting for the hub (M8)
	stdin     io.WriteCloser
	proc      *os.Process
	started   bool
	stdinDone bool
	exited    chan struct{}
	// controls are the control requests waiting for claude's control_response, by
	// request id (DESIGN §25.3).
	controls map[string]chan controlResponse
	// stdinMu keeps lines written to claude's stdin whole.
	stdinMu sync.Mutex
}

type daemonConn struct {
	lc     *proto.LineConn
	hold   bool        // control frames only, until a replay
	cursor int64       // last seq sent
	ctrl   []ctrlFrame // queued state / approval frames
	reset  chan int64
}

// ctrlFrame is sent after every event up to and including seq "after".
type ctrlFrame struct {
	f     proto.Local
	after int64
}

type pendingApproval struct {
	req proto.Local
	lc  *proto.LineConn
}

// Run serves until claude has exited and the daemon has caught up (or Linger passes),
// or ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	if !proto.ValidSessionID(cfg.SessionID) {
		return fmt.Errorf("invalid session id %q", cfg.SessionID)
	}
	if cfg.TTY == nil {
		cfg.TTY = io.Discard
	}
	if cfg.ControlTimeout == 0 {
		cfg.ControlTimeout = 5 * time.Second
	}
	if cfg.Linger == 0 {
		cfg.Linger = 10 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := os.MkdirAll(cfg.RuntimeDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(cfg.RuntimeDir, 0o700); err != nil {
		return err
	}
	if cfg.SpoolDir == "" {
		cfg.SpoolDir = cfg.RuntimeDir
	}
	if err := os.MkdirAll(cfg.SpoolDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(cfg.SpoolDir, 0o700); err != nil {
		return err
	}
	if cfg.ResumeID != "" && !validUUID(cfg.ResumeID) {
		return fmt.Errorf("invalid resume id %q", cfg.ResumeID)
	}
	sockPath, spoolPath := SockPath(cfg.RuntimeDir, cfg.SessionID), SpoolPath(cfg.SpoolDir, cfg.SessionID)
	s := &shim{cfg: cfg, log: cfg.Log, changed: make(chan struct{}), state: proto.StateStarting, base: cfg.SeqBase,
		claudeSID: cfg.ResumeID, daemons: map[*daemonConn]bool{}, pending: map[string]*pendingApproval{}, calls: map[string]*proto.LineConn{}, exited: make(chan struct{}),
		controls: map[string]chan controlResponse{}}
	if err := s.loadSpool(spoolPath); err != nil {
		return err
	}
	f, err := os.OpenFile(spoolPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	s.spool = f

	ln, err := listenUnix(sockPath)
	if err != nil {
		return err
	}
	defer func() { ln.Close(); os.Remove(sockPath) }()
	go s.acceptLoop(ln)

	defer os.Remove(TokenPath(cfg.SpoolDir, cfg.SessionID))
	resume := ""
	if cfg.ResumeID != "" {
		resume = " · resuming " + cfg.ResumeID
	}
	fmt.Fprintf(cfg.TTY, "\x1b[2mmado shim · session %s%s · waiting for the daemon\x1b[0m\n", cfg.SessionID, resume)
	linger := cfg.Linger
	select {
	case <-ctx.Done():
		cause := context.Cause(ctx)
		if !errors.Is(cause, ErrDetach) {
			cause = ErrLost
		}
		s.mu.Lock()
		s.ending = cause
		if cause == ErrDetach {
			s.state = proto.StateDetached
			s.broadcastStateLocked()
		}
		s.mu.Unlock()
		fmt.Fprintf(cfg.TTY, "\x1b[2m%v; ending claude\x1b[0m\n", cause)
		s.terminate()
		linger = 3 * time.Second
	case <-s.exited:
	}
	// claude is gone: give the daemon time to drain the tail of the spool.
	deadline := time.After(linger)
	for {
		s.mu.Lock()
		caughtUp := false
		last := s.lastSeqLocked()
		for d := range s.daemons {
			if d.cursor >= last && len(d.ctrl) == 0 {
				caughtUp = true
			}
		}
		ch := s.changed
		s.mu.Unlock()
		if caughtUp {
			time.Sleep(500 * time.Millisecond) // let the daemon read the final state frame
			return nil
		}
		select {
		case <-ch:
		case <-time.After(time.Second):
		case <-deadline:
			return nil
		}
	}
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return false
			}
		}
	}
	return true
}

func listenUnix(path string) (net.Listener, error) {
	if _, err := os.Lstat(path); err == nil {
		// A live shim owns it? Then refuse; otherwise it is stale.
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("socket %s is in use by another shim", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func (s *shim) loadSpool(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	for sc.Scan() {
		var e SpoolEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || (len(s.events) > 0 && e.Seq <= s.events[len(s.events)-1].Seq) {
			continue // torn last line after a crash
		}
		s.events = append(s.events, e)
	}
	return sc.Err()
}

func (s *shim) lastSeqLocked() int64 {
	if len(s.events) == 0 || s.events[len(s.events)-1].Seq < s.base {
		return s.base
	}
	return s.events[len(s.events)-1].Seq
}

func (s *shim) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// appendEvent spools one stream-json object and wakes the subscribers.
func (s *shim) appendEvent(raw json.RawMessage) {
	s.mu.Lock()
	e := SpoolEntry{Seq: s.lastSeqLocked() + 1, TS: time.Now().UTC(), Raw: raw}
	line, _ := json.Marshal(e)
	if _, err := s.spool.Write(append(line, '\n')); err != nil {
		s.log.Error("spool write", "err", err)
	}
	s.events = append(s.events, e)
	s.trackStateLocked(raw)
	s.notifyLocked()
	s.mu.Unlock()
	fmt.Fprint(s.cfg.TTY, render.TTY(render.Blocks(raw)))
}

// trackStateLocked follows the turn lifecycle from the stream.
func (s *shim) trackStateLocked(raw json.RawMessage) {
	var e struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		OpenCC    string `json:"mado"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	prev, prevSID := s.state, s.claudeSID
	if e.SessionID != "" {
		s.claudeSID = e.SessionID
	}
	switch {
	case s.ending != nil:
		// detached or lost: the reported state is final.
	case e.Type == "user" && e.OpenCC == "echo":
		s.state = proto.StateRunning
	case e.Type == "result":
		if len(s.pending) == 0 {
			s.state = proto.StateIdle
		}
	}
	if s.state != prev || s.claudeSID != prevSID {
		s.broadcastStateLocked()
	}
}

func (s *shim) stateFrameLocked() proto.Local {
	return proto.Local{T: proto.LState, State: s.state, ClaudeSessionID: s.claudeSID, ExitCode: s.exitCode, Error: s.exitErr}
}

func (s *shim) broadcastStateLocked() {
	s.enqueueLocked(s.stateFrameLocked())
}

// enqueueLocked queues a control frame for every daemon connection.
func (s *shim) enqueueLocked(f proto.Local) {
	after := s.lastSeqLocked()
	for d := range s.daemons {
		d.ctrl = append(d.ctrl, ctrlFrame{f, after})
	}
	s.notifyLocked()
}

func (s *shim) setState(st string) {
	s.mu.Lock()
	if s.state != st && s.state != proto.StateExited && s.ending == nil {
		s.state = st
		s.broadcastStateLocked()
		s.notifyLocked()
	}
	s.mu.Unlock()
}

func (s *shim) acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *shim) handle(c net.Conn) {
	defer c.Close()
	if !sameUser(c) {
		s.log.Warn("rejected socket peer with a different uid")
		return
	}
	lc := proto.NewLineConn(c)
	var hello proto.Local
	if err := lc.Read(&hello); err != nil || hello.T != proto.LHello {
		return
	}
	switch hello.Role {
	case "daemon":
		s.serveDaemon(lc, hello.FromSeq, hello.Hold)
	case "approver":
		s.serveApprover(lc)
	case "tool":
		s.serveTool(lc)
	}
}

func (s *shim) serveDaemon(lc *proto.LineConn, fromSeq int64, hold bool) {
	d := &daemonConn{lc: lc, cursor: fromSeq, hold: hold, reset: make(chan int64, 1)}
	s.mu.Lock()
	s.daemons[d] = true
	d.ctrl = append(d.ctrl, ctrlFrame{s.stateFrameLocked(), 0})
	for _, p := range s.pending {
		d.ctrl = append(d.ctrl, ctrlFrame{p.req, 0})
	}
	s.notifyLocked()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.daemons, d)
		s.mu.Unlock()
	}()
	done := make(chan struct{})
	defer close(done)
	go s.pump(d, done)
	for {
		var m proto.Local
		if err := lc.Read(&m); err != nil {
			return
		}
		switch m.T {
		case proto.LStart:
			if err := s.start(m.Spawn); err != nil {
				s.log.Error("start claude", "err", err)
				s.mu.Lock()
				s.exitErr = err.Error()
				s.mu.Unlock()
				s.appendEvent(localEvent("mado_error", "message", "failed to start claude: "+err.Error()))
				s.markExited(-1)
			}
		case proto.LUserMessage:
			if m.Note != "" {
				s.appendEvent(noteEvent(m.Note, m.Message))
			}
			s.sendUser(m.Text)
		case proto.LNote:
			if m.Note != "" {
				s.appendEvent(noteEvent(m.Note, m.Text))
			}
		case proto.LSetMode:
			go s.setMode(m.RequestID, m.Mode)
		case proto.LInterrupt:
			s.signal(syscall.SIGINT)
		case proto.LStop:
			// End the turn if one is in flight, then end stdin so claude exits cleanly.
			s.mu.Lock()
			busy := s.state == proto.StateRunning || s.state == proto.StateWaitingApproval
			s.mu.Unlock()
			if busy {
				s.signal(syscall.SIGINT)
			}
			s.closeStdin()
		case proto.LReplay:
			select {
			case d.reset <- m.FromSeq:
			default:
			}
			s.mu.Lock()
			s.notifyLocked()
			s.mu.Unlock()
		case proto.LApprovalResponse:
			s.answer(m)
		case proto.LToolResult:
			s.answerTool(m)
		}
	}
}

// pump streams spool entries after d.cursor, then live ones, interleaving control
// frames at the position they were raised.
func (s *shim) pump(d *daemonConn, done <-chan struct{}) {
	sendEvent := func(e SpoolEntry) bool {
		if err := d.lc.Write(proto.Local{T: proto.LEvent, Seq: e.Seq, Raw: e.Raw}); err != nil {
			return false
		}
		s.mu.Lock()
		d.cursor = e.Seq
		s.notifyLocked()
		s.mu.Unlock()
		return true
	}
	for {
		select {
		case seq := <-d.reset:
			s.mu.Lock()
			d.cursor, d.hold = seq, false
			s.mu.Unlock()
		default:
		}
		s.mu.Lock()
		ctrl := d.ctrl
		d.ctrl = nil
		var batch []SpoolEntry
		for i := len(s.events) - 1; i >= 0 && s.events[i].Seq > d.cursor && !d.hold; i-- {
			batch = append(batch, s.events[i])
		}
		ch := s.changed
		s.mu.Unlock()
		// batch is newest first.
		for _, c := range ctrl {
			for len(batch) > 0 && batch[len(batch)-1].Seq <= c.after {
				if !sendEvent(batch[len(batch)-1]) {
					return
				}
				batch = batch[:len(batch)-1]
			}
			if err := d.lc.Write(c.f); err != nil {
				return
			}
		}
		for i := len(batch) - 1; i >= 0; i-- {
			if !sendEvent(batch[i]) {
				return
			}
		}
		if len(batch) > 0 || len(ctrl) > 0 {
			continue
		}
		select {
		case <-ch:
		case <-done:
			return
		}
	}
}

func (s *shim) serveApprover(lc *proto.LineConn) {
	var req proto.Local
	if err := lc.Read(&req); err != nil || req.T != proto.LApprovalRequest || req.RequestID == "" {
		return
	}
	s.mu.Lock()
	s.pending[req.RequestID] = &pendingApproval{req: req, lc: lc}
	if s.ending == nil {
		s.state = proto.StateWaitingApproval
		s.broadcastStateLocked()
	}
	s.enqueueLocked(req)
	s.mu.Unlock()
	fmt.Fprint(s.cfg.TTY, render.TTY([]render.Block{{Kind: render.KindApproval, Label: req.ToolName,
		Text: req.ToolName + ": " + render.ToolInputSummary(req.ToolName, req.Input) + " (answer in the mado web UI)"}}))
	// Block until the approver goes away (answered or cancelled by claude).
	var sink proto.Local
	for lc.Read(&sink) == nil {
	}
	s.mu.Lock()
	if _, still := s.pending[req.RequestID]; still {
		delete(s.pending, req.RequestID)
		if len(s.pending) == 0 && s.state == proto.StateWaitingApproval && s.ending == nil {
			s.state = proto.StateRunning
			s.broadcastStateLocked()
		}
	}
	s.mu.Unlock()
}

// serveTool relays one hub tool call from the mado MCP server to the daemon and
// waits for the answer; the MCP server enforces the timeout. Calls are not kept
// for a daemon that attaches later: they are short, and the caller times out.
func (s *shim) serveTool(lc *proto.LineConn) {
	var req proto.Local
	if err := lc.Read(&req); err != nil || req.T != proto.LToolCall || req.CallID == "" {
		return
	}
	s.mu.Lock()
	if len(s.daemons) == 0 {
		s.mu.Unlock()
		_ = lc.Write(proto.Local{T: proto.LToolResult, CallID: req.CallID, Text: "mado: the worker daemon is not connected", IsError: true})
		return
	}
	s.calls[req.CallID] = lc
	s.enqueueLocked(proto.Local{T: proto.LToolCall, CallID: req.CallID, ToolName: req.ToolName, Input: req.Input})
	s.mu.Unlock()
	var sink proto.Local
	for lc.Read(&sink) == nil {
	}
	s.mu.Lock()
	delete(s.calls, req.CallID)
	s.mu.Unlock()
}

func (s *shim) answerTool(m proto.Local) {
	s.mu.Lock()
	lc := s.calls[m.CallID]
	delete(s.calls, m.CallID)
	s.mu.Unlock()
	if lc != nil {
		_ = lc.Write(proto.Local{T: proto.LToolResult, CallID: m.CallID, Text: m.Text, IsError: m.IsError})
	}
}

func (s *shim) answer(m proto.Local) {
	s.mu.Lock()
	p, ok := s.pending[m.RequestID]
	if ok {
		delete(s.pending, m.RequestID)
		if len(s.pending) == 0 && s.state == proto.StateWaitingApproval && s.ending == nil {
			s.state = proto.StateRunning
			s.broadcastStateLocked()
		}
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = p.lc.Write(proto.Local{T: proto.LApprovalResponse, RequestID: m.RequestID, Decision: m.Decision, Message: m.Message,
		UpdatedInput: m.UpdatedInput})
	fmt.Fprintf(s.cfg.TTY, "\x1b[35m  ↳ %s\x1b[0m\n", m.Decision)
}

// start spawns claude once.
func (s *shim) start(sp *proto.Spawn) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.mu.Unlock()
	if sp == nil || sp.ClaudeBin == "" {
		return errors.New("empty spawn spec")
	}
	args, err := resumeArgs(sp.Args, s.cfg.ResumeID)
	if err != nil {
		return err
	}
	if sp.RouterToken != "" {
		if err := writeToken(TokenPath(s.cfg.SpoolDir, s.cfg.SessionID), sp.RouterToken); err != nil {
			return fmt.Errorf("persist router token: %w", err)
		}
	}
	cmd := exec.Command(sp.ClaudeBin, args...)
	cmd.Dir = sp.CWD
	cmd.Env = childEnv(os.Environ(), sp.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.mu.Lock()
	s.stdin, s.proc = stdin, cmd.Process
	s.mu.Unlock()
	// Marks a new claude process in the stream: result.modelUsage counts from here (§17.13).
	s.appendEvent(localEvent("mado_spawn", "pid", fmt.Sprint(cmd.Process.Pid)))
	fmt.Fprintf(s.cfg.TTY, "\x1b[2mstarted %s (pid %d) in %s\x1b[0m\n", filepath.Base(sp.ClaudeBin), cmd.Process.Pid, sp.CWD)
	s.setState(proto.StateIdle)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 64<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			if json.Valid(line) && line[0] == '{' {
				if s.controlAnswer(line) {
					continue // a control_response is not a session event (DESIGN §25.3)
				}
				s.appendEvent(append(json.RawMessage(nil), line...))
			} else {
				s.appendEvent(localEvent("mado_stdout", "text", string(line)))
			}
		}
	}()
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			s.appendEvent(localEvent("mado_stderr", "text", sc.Text()))
		}
	}()
	go func() {
		wg.Wait()
		err := cmd.Wait()
		code := 0
		if err != nil {
			code = -1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
		}
		fmt.Fprintf(s.cfg.TTY, "\x1b[2mclaude exited with code %d\x1b[0m\n", code)
		s.markExited(code)
	}()
	return nil
}

func (s *shim) markExited(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == proto.StateExited {
		return
	}
	s.exitCode = &code
	if s.ending == nil {
		s.state = proto.StateExited
		s.broadcastStateLocked()
	}
	s.notifyLocked()
	close(s.exited)
}

// childEnv drops inherited endpoint/credential variables and adds the profile's.
func childEnv(base []string, add map[string]string) []string {
	out := make([]string, 0, len(base)+len(add))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "ANTHROPIC_") || k == "CLAUDE_CONFIG_DIR" || k == "CLAUDE_CODE_OAUTH_TOKEN" ||
			k == "API_TIMEOUT_MS" || k == "CLAUDE_CODE_ATTRIBUTION_HEADER" ||
			k == "CLAUDE_CODE_MAX_CONTEXT_TOKENS" || k == "CLAUDE_CODE_MAX_OUTPUT_TOKENS" || k == "INVOCATION_ID" {
			continue
		}
		if _, override := add[k]; override {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range add {
		out = append(out, k+"="+v)
	}
	return out
}

func (s *shim) sendUser(text string) {
	msg := map[string]any{"type": "user", "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "text", "text": text}}}}
	line, _ := json.Marshal(msg)
	s.mu.Lock()
	w, closed := s.stdin, s.stdinDone
	s.mu.Unlock()
	if w == nil || closed {
		s.appendEvent(localEvent("mado_error", "message", "claude is not running; message dropped"))
		return
	}
	echo := map[string]any{"type": "user", "mado": "echo", "message": msg["message"]}
	eb, _ := json.Marshal(echo)
	s.appendEvent(eb)
	if err := s.writeStdin(w, line); err != nil {
		s.appendEvent(localEvent("mado_error", "message", "write to claude failed: "+err.Error()))
	}
}

// writeStdin writes one JSON line to claude; concurrent writers do not interleave.
func (s *shim) writeStdin(w io.Writer, line []byte) error {
	s.stdinMu.Lock()
	defer s.stdinMu.Unlock()
	_, err := w.Write(append(line, '\n'))
	return err
}

// noteEvent is a mado note in the transcript (DESIGN §26.2): a user event claude never
// sees, rendered as a notice.
func noteEvent(note, text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"type": "user", "mado": "note", "mado_note": note,
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}})
	return b
}

// controlResponse is claude's answer to a control request.
type controlResponse struct {
	Subtype string `json:"subtype"`
	ID      string `json:"request_id"`
	Error   string `json:"error"`
}

// controlAnswer hands a control_response line to the request waiting for it and
// reports whether the line was one (it is then not a session event).
func (s *shim) controlAnswer(line []byte) bool {
	if !strings.Contains(string(line), `"control_response"`) {
		return false
	}
	var e struct {
		Type     string          `json:"type"`
		Response controlResponse `json:"response"`
	}
	if json.Unmarshal(line, &e) != nil || e.Type != "control_response" {
		return false
	}
	s.mu.Lock()
	ch := s.controls[e.Response.ID]
	delete(s.controls, e.Response.ID)
	s.mu.Unlock()
	if ch != nil {
		ch <- e.Response
	}
	return true
}

// setMode asks claude to switch its permission mode through the stream-json control
// protocol (DESIGN §25.3) and reports the outcome to the daemons as mode_result.
func (s *shim) setMode(reqID, mode string) {
	errMsg := s.controlRequest(mode)
	if errMsg == "" {
		fmt.Fprintf(s.cfg.TTY, "\x1b[2mpermission mode is now %s\x1b[0m\n", mode)
	} else {
		fmt.Fprintf(s.cfg.TTY, "\x1b[2mpermission mode %s not applied: %s\x1b[0m\n", mode, errMsg)
	}
	s.mu.Lock()
	s.enqueueLocked(proto.Local{T: proto.LModeResult, RequestID: reqID, Mode: mode, Error: errMsg})
	s.mu.Unlock()
}

// controlRequest sends set_permission_mode and waits for the answer; "" is success.
func (s *shim) controlRequest(mode string) string {
	if !proto.ClaudeModes[mode] {
		return "permission mode " + mode + " is not allowed"
	}
	s.mu.Lock()
	w, closed := s.stdin, s.stdinDone
	id := proto.NewID()
	ch := make(chan controlResponse, 1)
	if w != nil && !closed {
		s.controls[id] = ch
	}
	s.mu.Unlock()
	if w == nil || closed {
		return "claude is not running"
	}
	line, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": id,
		"request": map[string]any{"subtype": "set_permission_mode", "mode": mode}})
	if err := s.writeStdin(w, line); err != nil {
		s.mu.Lock()
		delete(s.controls, id)
		s.mu.Unlock()
		return "write to claude failed: " + err.Error()
	}
	select {
	case r := <-ch:
		if r.Subtype == "success" {
			return ""
		}
		if r.Error == "" {
			r.Error = "claude refused the request"
		}
		return r.Error
	case <-time.After(s.cfg.ControlTimeout):
	case <-s.exited:
	}
	s.mu.Lock()
	delete(s.controls, id)
	s.mu.Unlock()
	return "claude did not answer the control request"
}

func (s *shim) signal(sig syscall.Signal) {
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if p != nil {
		_ = p.Signal(sig)
	}
}

func (s *shim) closeStdin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin != nil && !s.stdinDone {
		s.stdinDone = true
		_ = s.stdin.Close()
	}
}

// terminate ends claude gracefully when the shim itself is told to stop.
func (s *shim) terminate() {
	s.mu.Lock()
	started := s.proc != nil
	s.mu.Unlock()
	if !started {
		return
	}
	s.signal(syscall.SIGINT)
	s.closeStdin()
	select {
	case <-s.exited:
	case <-time.After(10 * time.Second):
		s.signal(syscall.SIGTERM)
	}
}

// resumeArgs makes sure claude's argv resumes exactly the shim's --resume id.
func resumeArgs(args []string, id string) ([]string, error) {
	for i, a := range args {
		if a == "--resume" || a == "-r" {
			if id == "" || i+1 >= len(args) || args[i+1] != id {
				return nil, errors.New("spawn spec --resume does not match the shim's --resume")
			}
			return args, nil
		}
	}
	if id == "" {
		return args, nil
	}
	return append(append([]string(nil), args...), "--resume", id), nil
}

func localEvent(typ, key, val string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"type": typ, key: val})
	return b
}

// ReadSpool reads a spool file (used by tools and tests).
func ReadSpool(path string) ([]SpoolEntry, error) {
	s := &shim{}
	if err := s.loadSpool(path); err != nil {
		return nil, err
	}
	return s.events, nil
}

// Main is `mado shim`: flags, signals, Run. Ctrl-C in the tmux window is a
// deliberate detach; SIGHUP (tmux server or window gone) and SIGTERM (shutdown)
// leave the session resumable by the hub.
func Main(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("shim", flag.ContinueOnError)
	sid := fs.String("session", "", "session id")
	dir := fs.String("runtime-dir", "", "runtime dir for the socket (default $XDG_RUNTIME_DIR/mado)")
	spool := fs.String("spool-dir", "", "spool and token dir (default: the runtime dir)")
	resume := fs.String("resume", "", "claude session id to resume")
	seqBase := fs.Int64("seq-base", 0, "number events after this seq")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		x := os.Getenv("XDG_RUNTIME_DIR")
		if x == "" {
			return errors.New("XDG_RUNTIME_DIR unset")
		}
		*dir = filepath.Join(x, "mado")
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	sigs := make(chan os.Signal, 1)
	signal.Reset(os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		if sig := <-sigs; sig == os.Interrupt {
			cancel(ErrDetach)
		} else {
			cancel(ErrLost)
		}
	}()
	return Run(ctx, Config{SessionID: *sid, RuntimeDir: *dir, SpoolDir: *spool, ResumeID: *resume,
		SeqBase: *seqBase, TTY: os.Stdout, Log: log})
}
