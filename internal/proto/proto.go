// Package proto defines the hub <-> worker WebSocket frames (DESIGN §4) and the
// local JSON-lines frames spoken on the shim's unix socket.
//
// Every hub/worker frame is a flat JSON object: {"t":..., "id":..., "ts":..., ...payload}.
package proto

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Frame type names.
const (
	// worker -> hub
	THello           = "hello"
	TSessionState    = "session_state"
	TEvent           = "event"
	TApprovalRequest = "approval_request"
	TSessions        = "sessions"
	TAck             = "ack"
	// hub -> worker
	TStartSession     = "start_session"
	TUserMessage      = "user_message"
	TInterrupt        = "interrupt"
	TApprovalResponse = "approval_response"
	TStopSession      = "stop_session"
	TListSessions     = "list_sessions"
	TResync           = "resync"
	TGC               = "gc"
	// M3: Claude Code history import (DESIGN §14)
	THistorySession = "history_session" // worker -> hub
	THistoryOffsets = "history_offsets" // hub -> worker
	THistoryRescan  = "history_rescan"  // hub -> worker
	// M8: web tools for code sessions (DESIGN §17.12)
	TToolCall   = "tool_call"   // worker -> hub
	TToolResult = "tool_result" // hub -> worker
)

// Session states.
const (
	StateStarting        = "starting"
	StateRunning         = "running"
	StateWaitingApproval = "waiting_approval"
	StateIdle            = "idle"
	StateDetached        = "detached"
	StateExited          = "exited"
)

// Approval decisions.
const (
	DecisionAllow        = "allow"
	DecisionAllowSession = "allow_session"
	DecisionDeny         = "deny"
)

// Header is embedded in every frame.
type Header struct {
	T  string    `json:"t"`
	ID string    `json:"id"`
	TS time.Time `json:"ts"`
}

func (h *Header) header() *Header { return h }

// Frame is implemented by every frame type.
type Frame interface{ header() *Header }

type Hello struct {
	Header
	Machine       string   `json:"machine"`
	Version       string   `json:"version"`
	ClaudeVersion string   `json:"claude_version"`
	Capabilities  []string `json:"capabilities"`
	Projects      []string `json:"projects"`
	// Roots are the worker's project_roots themselves (M12, DESIGN §20); Projects
	// also lists their child directories. An older worker sends none.
	Roots []string `json:"roots,omitempty"`
	// JobDir is the cwd of job sessions (M16, DESIGN §23.6); set only with the
	// capability jobs.
	JobDir string `json:"job_dir,omitempty"`
}

// CapJobs is the hello capability of a worker that runs job sessions (DESIGN §23.6):
// jobs = true in worker.toml and a subscription login present.
const CapJobs = "jobs"

// Job kinds.
const JobDigest = "digest"

// Job turns a start_session into a job session (M16, DESIGN §23.6): one turn on the
// subscription with no built-in tools, cwd = the worker's job_dir. The hub sends the
// system prompt so the worker needs no knowledge of the job.
type Job struct {
	Kind      string `json:"kind"`                 // "digest"
	MainModel string `json:"main_model,omitempty"` // --model; "" = sonnet
	System    string `json:"system"`               // --append-system-prompt
}

type SessionState struct {
	Header
	SessionID       string `json:"session_id"`
	State           string `json:"state"`
	ClaudeSessionID string `json:"claude_session_id"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	Error           string `json:"error,omitempty"`
}

type Event struct {
	Header
	SessionID string          `json:"session_id"`
	Seq       int64           `json:"seq"`
	Raw       json.RawMessage `json:"raw"`
}

type ApprovalRequest struct {
	Header
	SessionID   string          `json:"session_id"`
	RequestID   string          `json:"request_id"`
	ToolName    string          `json:"tool_name"`
	Input       json.RawMessage `json:"input"`
	Suggestions json.RawMessage `json:"suggestions,omitempty"`
}

type SessionInfo struct {
	SessionID       string `json:"session_id"`
	State           string `json:"state"`
	ClaudeSessionID string `json:"claude_session_id"`
	LastSeq         int64  `json:"last_seq"`
}

type Sessions struct {
	Header
	Sessions []SessionInfo `json:"sessions"`
	// Spools lists session ids that have a spool or token file on disk but no live
	// shim. The hub answers with a gc frame naming the dead ones.
	Spools []string `json:"spools,omitempty"`
}

// GC names sessions the hub considers dead (exited, detached or unknown). The worker
// deletes their spool and token files once they are older than its retention.
type GC struct {
	Header
	SessionIDs []string `json:"session_ids"`
}

type Ack struct {
	Header
	Ref   string `json:"ref"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type StartSession struct {
	Header
	SessionID             string   `json:"session_id"`
	CWD                   string   `json:"cwd"`
	Profile               Profile  `json:"profile"`
	MainModel             string   `json:"main_model"`
	ResumeClaudeSessionID string   `json:"resume_claude_session_id,omitempty"`
	PermissionMode        string   `json:"permission_mode,omitempty"`
	ExtraArgs             []string `json:"extra_args,omitempty"`
	// Refresh re-pushes profile and keys for a session whose shim is still alive
	// (after a worker reconnect); nothing is spawned.
	Refresh bool `json:"refresh,omitempty"`
	// SeqBase: a resumed shim numbers its events after this (the hub's highest seq),
	// so a lost spool never reuses seqs the hub already stored.
	SeqBase int64 `json:"seq_base,omitempty"`
	// FromSeq: the worker relays spool events after this seq once the shim is up.
	FromSeq int64 `json:"from_seq,omitempty"`
	// Tools (M8) are the hub tools the session may call through the mado MCP server
	// (web_search, fetch_url). An old hub sends none, so none are offered.
	Tools []string `json:"tools,omitempty"`
	// Job (M16) makes this a job session; the hub sends it only to workers that
	// advertised CapJobs (an older worker would ignore it and refuse the cwd).
	Job *Job `json:"job,omitempty"`
}

// ToolCall asks the hub to run one of its tools for a code session (M8). Args is
// the tool input claude gave the mado MCP server.
type ToolCall struct {
	Header
	SessionID string          `json:"session_id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
}

// ToolResult answers a ToolCall: the text claude gets, and whether it is an error.
type ToolResult struct {
	Header
	SessionID string `json:"session_id"`
	CallID    string `json:"call_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

type UserMessage struct {
	Header
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
	// Note, when set, makes the shim store a mado note of this name just before the
	// message (DESIGN §26.2, "rate_limit_resume"); NoteText is its text.
	Note     string `json:"note,omitempty"`
	NoteText string `json:"note_text,omitempty"`
}

type Interrupt struct {
	Header
	SessionID string `json:"session_id"`
}

type ApprovalResponse struct {
	Header
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"`
	Message   string `json:"message,omitempty"`
	// UpdatedInput replaces the tool input claude runs with on allow (DESIGN §21.1):
	// the answers to an AskUserQuestion. Absent: the input is unchanged.
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
}

type StopSession struct {
	Header
	SessionID string `json:"session_id"`
	Kill      bool   `json:"kill,omitempty"`
}

type ListSessions struct{ Header }

type Resync struct {
	Header
	SessionID string `json:"session_id"`
	FromSeq   int64  `json:"from_seq"`
}

// HistorySession carries the tail of one Claude Code transcript file (read-only
// import, DESIGN §14). Lines are the raw transcript lines without the newline, in file
// order, starting at byte Offset; End is the byte after the last complete line sent.
// A file is identified by Path (the worker's absolute path); the hub never asks the
// worker to open a path, it only echoes offsets for paths the worker reported.
type HistorySession struct {
	Header
	Path            string   `json:"path"`
	ConfigDir       string   `json:"config_dir"`               // ~/.claude or a CLAUDE_CONFIG_DIR of a mado profile
	DefaultConfig   bool     `json:"default_config,omitempty"` // ConfigDir is the user's own ~/.claude (subscription)
	ClaudeSessionID string   `json:"claude_session_id"`
	CWD             string   `json:"cwd,omitempty"`   // from the first line with a cwd, else decoded from the directory name
	Title           string   `json:"title,omitempty"` // from these lines, if any
	TitleCustom     bool     `json:"title_custom,omitempty"`
	MTime           int64    `json:"mtime"` // unix nanoseconds
	Size            int64    `json:"size"`
	Offset          int64    `json:"offset"`
	End             int64    `json:"end"`
	Reset           bool     `json:"reset,omitempty"` // the file shrank or was replaced: drop what the hub has
	Lines           []string `json:"lines"`
	LineOffsets     []int64  `json:"line_offsets"` // byte offset of each line
}

// HistoryFileState is the hub's view of one transcript file.
type HistoryFileState struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	MTime  int64  `json:"mtime"`
}

// HistoryOffsets tells the worker how far the hub has each file. Sent after hello and
// whenever the hub rejects a history_session that does not continue where it stands.
type HistoryOffsets struct {
	Header
	Files []HistoryFileState `json:"files"`
	// Partial: only these files; the worker keeps its state for the others.
	Partial bool `json:"partial,omitempty"`
}

type HistoryRescan struct{ Header }

// Profile (DESIGN §5). Upstream keys are resolved by the hub before sending.
type Profile struct {
	Name      string              `json:"name"`
	Kind      string              `json:"kind"` // "router" or "subscription"
	Main      string              `json:"main,omitempty"`
	Aliases   map[string]string   `json:"aliases,omitempty"`
	Upstreams map[string]Upstream `json:"upstreams,omitempty"`
	// Roles sets up the two-model split (DESIGN §5, §15). nil means the defaults
	// (enabled, implementer/researcher opus, simple sonnet, mado's prompt).
	Roles *Roles `json:"roles,omitempty"`
}

// Roles is the per-profile role split: three subagent definitions passed with
// --agents and a delegation prompt passed with --append-system-prompt. The model
// fields are Claude Code aliases (fable, opus, sonnet, haiku, inherit); for a router
// profile the alias resolves through the profile's aliases.
type Roles struct {
	Enabled     *bool  `json:"enabled,omitempty"` // nil = true
	Implementer string `json:"implementer,omitempty"`
	Researcher  string `json:"researcher,omitempty"`
	Simple      string `json:"simple,omitempty"`
	Prompt      string `json:"prompt,omitempty"` // "" = mado's default delegation prompt
	// Enforce is how the split is enforced on the main model (DESIGN §16): "off",
	// "edits" (the main model may not use Edit/Write/NotebookEdit) or "all" (nor
	// Bash). "" = "edits".
	Enforce string `json:"enforce,omitempty"`
}

const (
	KindRouter       = "router"
	KindSubscription = "subscription"
)

type Upstream struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	KeyRef  string `json:"key_ref,omitempty"`
	// Key is filled in by the hub from its secrets file. Never stored on the worker.
	Key string `json:"key,omitempty"`
	// AuthHeader is "bearer" (default for non-Anthropic hosts) or "x-api-key".
	AuthHeader string `json:"auth_header,omitempty"`
	// Optional model metadata (DESIGN §5). For the session's main upstream they become
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS and CLAUDE_CODE_MAX_OUTPUT_TOKENS.
	ContextWindow int `json:"context_window,omitempty"`
	MaxOutput     int `json:"max_output,omitempty"`
	// Kind is the upstream's API (DESIGN §22): "" or "anthropic" (Messages API,
	// forwarded as is) or "openai" (chat completions under BaseURL, translated).
	Kind string `json:"kind,omitempty"`
	// TimeoutFirstByte and TimeoutTotal are Go durations; "" = the default
	// (DESIGN §22.1, §22.5).
	TimeoutFirstByte string `json:"timeout_first_byte,omitempty"`
	TimeoutTotal     string `json:"timeout_total,omitempty"`
}

// Upstream kinds.
const (
	UpstreamAnthropic = "anthropic"
	UpstreamOpenAI    = "openai"
)

// Upstream timeout defaults (DESIGN §22.1).
const (
	DefaultTimeoutFirstByte = 60 * time.Second
	DefaultTimeoutTotal     = 10 * time.Minute
)

// ValidUpstreamKind accepts "", anthropic and openai.
func ValidUpstreamKind(k string) bool {
	return k == "" || k == UpstreamAnthropic || k == UpstreamOpenAI
}

// ParseTimeouts parses timeout_first_byte and timeout_total; "" gives 0 (the
// caller's default). Both must be 1s..24h and the first-byte wait no longer than the
// total (the default total when that is unset).
func ParseTimeouts(firstByte, total string) (fb, tot time.Duration, err error) {
	parse := func(name, s string) (time.Duration, error) {
		if s == "" {
			return 0, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil || d < time.Second || d > 24*time.Hour {
			return 0, fmt.Errorf("%s must be a duration from 1s to 24h, such as 180s or 10m", name)
		}
		return d, nil
	}
	if fb, err = parse("timeout_first_byte", firstByte); err != nil {
		return 0, 0, err
	}
	if tot, err = parse("timeout_total", total); err != nil {
		return 0, 0, err
	}
	effTot := tot
	if effTot == 0 {
		effTot = DefaultTimeoutTotal
	}
	if fb > effTot {
		return 0, 0, fmt.Errorf("timeout_first_byte must not exceed timeout_total (%s)", effTot)
	}
	return fb, tot, nil
}

// Redacted returns a copy of the profile without keys, for storage and logs.
func (p Profile) Redacted() Profile {
	q := p
	q.Upstreams = make(map[string]Upstream, len(p.Upstreams))
	for k, u := range p.Upstreams {
		u.Key = ""
		q.Upstreams[k] = u
	}
	return q
}

// NewID returns a random UUIDv4 string.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

var typeOf = map[string]func() Frame{
	THello:            func() Frame { return &Hello{} },
	TSessionState:     func() Frame { return &SessionState{} },
	TEvent:            func() Frame { return &Event{} },
	TApprovalRequest:  func() Frame { return &ApprovalRequest{} },
	TSessions:         func() Frame { return &Sessions{} },
	TAck:              func() Frame { return &Ack{} },
	TStartSession:     func() Frame { return &StartSession{} },
	TUserMessage:      func() Frame { return &UserMessage{} },
	TInterrupt:        func() Frame { return &Interrupt{} },
	TApprovalResponse: func() Frame { return &ApprovalResponse{} },
	TStopSession:      func() Frame { return &StopSession{} },
	TListSessions:     func() Frame { return &ListSessions{} },
	TResync:           func() Frame { return &Resync{} },
	TGC:               func() Frame { return &GC{} },
	THistorySession:   func() Frame { return &HistorySession{} },
	THistoryOffsets:   func() Frame { return &HistoryOffsets{} },
	THistoryRescan:    func() Frame { return &HistoryRescan{} },
	TToolCall:         func() Frame { return &ToolCall{} },
	TToolResult:       func() Frame { return &ToolResult{} },
}

var nameOf = map[string]string{}

func init() {
	for name, mk := range typeOf {
		nameOf[fmt.Sprintf("%T", mk())] = name
	}
}

// Encode fills in t (from the Go type), id (if empty) and ts (if zero) and marshals.
func Encode(f Frame) ([]byte, error) {
	h := f.header()
	name, ok := nameOf[fmt.Sprintf("%T", f)]
	if !ok {
		return nil, fmt.Errorf("proto: unknown frame type %T", f)
	}
	h.T = name
	if h.ID == "" {
		h.ID = NewID()
	}
	if h.TS.IsZero() {
		h.TS = time.Now().UTC()
	}
	return json.Marshal(f)
}

// Decode parses one frame. Unknown types return an error.
func Decode(b []byte) (Frame, error) {
	var h Header
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("proto: %w", err)
	}
	mk, ok := typeOf[h.T]
	if !ok {
		return nil, fmt.Errorf("proto: unknown frame type %q", h.T)
	}
	f := mk()
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("proto: %s: %w", h.T, err)
	}
	return f, nil
}

// HeaderOf exposes the header of a frame.
func HeaderOf(f Frame) *Header { return f.header() }
