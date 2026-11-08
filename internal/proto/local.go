package proto

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
)

// Local frames, spoken as JSON lines on $XDG_RUNTIME_DIR/mado/<session>.sock.
// They are only exchanged between processes of the same user on one machine.
//
// Every connection starts with a LocalHello from the client.
type Local struct {
	T string `json:"t"`

	// hello
	Role    string `json:"role,omitempty"` // "daemon", "approver" or "tool"
	FromSeq int64  `json:"from_seq,omitempty"`
	// Hold: send control frames only until the daemon sends a replay (used by a
	// restarted daemon, which waits for the hub's resync to know where to start).
	Hold bool `json:"hold,omitempty"`

	// start (daemon -> shim): how to spawn claude.
	Spawn *Spawn `json:"spawn,omitempty"`

	// event (shim -> daemon)
	Seq int64           `json:"seq,omitempty"`
	Raw json.RawMessage `json:"raw,omitempty"`

	// state (shim -> daemon)
	State           string `json:"state,omitempty"`
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	Error           string `json:"error,omitempty"`

	// user_message (daemon -> shim)
	Text string `json:"text,omitempty"`

	// approval_request (approver -> shim -> daemon), approval_response (reverse)
	RequestID string          `json:"request_id,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Decision  string          `json:"decision,omitempty"`
	Message   string          `json:"message,omitempty"`
	// UpdatedInput (approval_response, allow): the input claude runs the tool with
	// instead of its own (DESIGN §21.1).
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`

	// tool_call (tool -> shim -> daemon: ToolName, Input) and tool_result (reverse:
	// Text, IsError), M8.
	CallID  string `json:"call_id,omitempty"`
	IsError bool   `json:"is_error,omitempty"`

	// set_mode and mode_result (DESIGN §25.3): RequestID, Mode, and Error on failure.
	Mode string `json:"mode,omitempty"`
	// note (DESIGN §26.2): a mado note with this name and Text. user_message may
	// carry one too (its text in Message), stored just before the message.
	Note string `json:"note,omitempty"`
}

// Local frame types.
const (
	LHello            = "hello"
	LStart            = "start"
	LEvent            = "event"
	LState            = "state"
	LUserMessage      = "user_message"
	LInterrupt        = "interrupt"
	LStop             = "stop"
	LReplay           = "replay"
	LApprovalRequest  = "approval_request"
	LApprovalResponse = "approval_response"
	LToolCall         = "tool_call"
	LToolResult       = "tool_result"
)

// Spawn describes the claude child. Built and validated by the daemon.
type Spawn struct {
	ClaudeBin string            `json:"claude_bin"`
	CWD       string            `json:"cwd"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env,omitempty"`
	// RouterToken is persisted by the shim (mode 0600) so a restarted daemon can
	// re-register the session with its router.
	RouterToken string `json:"router_token,omitempty"`
}

// LineConn reads and writes JSON lines. Writes are serialised.
type LineConn struct {
	mu sync.Mutex
	w  io.Writer
	sc *bufio.Scanner
}

func NewLineConn(rw io.ReadWriter) *LineConn {
	sc := bufio.NewScanner(rw)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	return &LineConn{w: rw, sc: sc}
}

func (c *LineConn) Write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(b)
	return err
}

func (c *LineConn) Read(v any) error {
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	return json.Unmarshal(c.sc.Bytes(), v)
}

// ValidSessionID accepts the hub's UUIDs (and similar safe ids); it guards file paths
// and tmux targets.
func ValidSessionID(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'f' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
