package proto

import "fmt"

// Permission mode changes of a running session (DESIGN §25.3) and transcript notes
// written by the hub into a live session (§26.2).

// Frame types.
const (
	TSetPermissionMode = "set_permission_mode" // hub -> worker
	TModeResult        = "mode_result"         // worker -> hub
	TAppendNote        = "append_note"         // hub -> worker
)

// SetPermissionMode asks the session's claude to switch to Mode (claude's name:
// default, acceptEdits or auto) through the stream-json control protocol. RequestID
// comes back in the ModeResult.
type SetPermissionMode struct {
	Header
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	Mode      string `json:"mode"`
}

// ModeResult is claude's answer to a SetPermissionMode: OK when claude switched, else
// Error (no shim, claude not running, no answer in time, or claude's own error).
type ModeResult struct {
	Header
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	Mode      string `json:"mode"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

// AppendNote makes the shim store a mado note in the session's transcript: a user
// event with "mado":"note" and "mado_note":Note whose text is Text. claude never
// sees it.
type AppendNote struct {
	Header
	SessionID string `json:"session_id"`
	Note      string `json:"note"`
	Text      string `json:"text"`
}

// Local frame types for the same (DESIGN §25.3, §26.2). LSetMode (daemon -> shim)
// carries RequestID and Mode; LModeResult (shim -> daemon) RequestID, Mode and Error
// ("" = success); LNote (daemon -> shim) Note and Text.
const (
	LSetMode    = "set_mode"
	LModeResult = "mode_result"
	LNote       = "note"
)

// Claude permission modes a hub may set on a running claude (never the ones that skip
// the prompts).
var ClaudeModes = map[string]bool{"default": true, "acceptEdits": true, "auto": true}

func init() {
	typeOf[TSetPermissionMode] = func() Frame { return &SetPermissionMode{} }
	typeOf[TModeResult] = func() Frame { return &ModeResult{} }
	typeOf[TAppendNote] = func() Frame { return &AppendNote{} }
	for _, name := range []string{TSetPermissionMode, TModeResult, TAppendNote} {
		nameOf[fmt.Sprintf("%T", typeOf[name]())] = name
	}
}
