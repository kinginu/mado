package proto

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// The concierge's file system frames (DESIGN §20.2, §20.3). The hub asks the worker to
// look at a directory (fs_inspect) or to run one prepare step of an approved plan
// (fs_prepare); the worker answers with fs_result. Both sides check every path
// against the worker's project roots.

// Frame types.
const (
	TFSInspect = "fs_inspect" // hub -> worker
	TFSPrepare = "fs_prepare" // hub -> worker
	TFSResult  = "fs_result"  // worker -> hub
)

// CapFS is the hello capability of a worker that answers the fs frames.
const CapFS = "fs"

// Inspect kinds.
const (
	InspectListProjects = "list_projects"
	InspectDir          = "inspect"
)

// Prepare actions (exactly these four).
const (
	ActionMkdir    = "mkdir"
	ActionGitInit  = "git_init"
	ActionClone    = "clone"
	ActionWorktree = "worktree"
)

// FSInspect asks for a listing. Path is empty for list_projects (every root).
type FSInspect struct {
	Header
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"`
	Path      string `json:"path,omitempty"`
}

// FSPrepare runs one prepare step. Repo is the worktree's repository, URL the clone
// source; Branch and Base are plain ref names.
type FSPrepare struct {
	Header
	RequestID string `json:"request_id"`
	Action    string `json:"action"`
	Path      string `json:"path"`
	Repo      string `json:"repo,omitempty"`
	URL       string `json:"url,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Base      string `json:"base,omitempty"`
}

// FSResult answers either frame: Data on success, Error otherwise.
type FSResult struct {
	Header
	RequestID string          `json:"request_id"`
	OK        bool            `json:"ok"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
	Code      string          `json:"code,omitempty"` // fs_read errors: one of the FSCode values
}

// The file viewer's frame (DESIGN §27.2). The hub asks for one file of a code
// session; the worker resolves the path ("~", relative to CWD), checks it against the
// project roots, the session cwd and the worker user's home, and answers fs_result
// with an FSFile (Bytes only for a download) or an error with a Code.
const (
	TFSRead   = "fs_read" // hub -> worker
	CapFSRead = "fs_read" // hello capability of a worker that answers fs_read
)

// Sizes of fs_read: the view and the download caps.
const (
	FSViewMax     = 1 << 20
	FSDownloadMax = 16 << 20
)

// fs_read error codes.
const (
	FSCodeNotFound  = "not_found"
	FSCodeForbidden = "forbidden"
	FSCodeTooLarge  = "too_large" // Data carries an FSFile with Size
	FSCodeNotFile   = "not_file"
)

// FSRead asks for one file. Download wants the bytes whatever their type.
type FSRead struct {
	Header
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	CWD       string `json:"cwd"`
	MaxBytes  int64  `json:"max_bytes"`
	Download  bool   `json:"download,omitempty"`
}

// FSFile is the data of a successful fs_read. Text is set for valid UTF-8 text when
// viewing; Binary marks a file shown only as a download; Bytes is the content of a
// download (base64 in JSON).
type FSFile struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Modified string `json:"modified,omitempty"`
	Mime     string `json:"mime,omitempty"`
	Text     string `json:"text,omitempty"`
	Binary   bool   `json:"binary,omitempty"`
	Bytes    []byte `json:"data,omitempty"`
}

func init() {
	typeOf[TFSRead] = func() Frame { return &FSRead{} }
	nameOf[fmt.Sprintf("%T", typeOf[TFSRead]())] = TFSRead
}

func init() {
	typeOf[TFSInspect] = func() Frame { return &FSInspect{} }
	typeOf[TFSPrepare] = func() Frame { return &FSPrepare{} }
	typeOf[TFSResult] = func() Frame { return &FSResult{} }
	for _, name := range []string{TFSInspect, TFSPrepare, TFSResult} {
		nameOf[fmt.Sprintf("%T", typeOf[name]())] = name
	}
}

// CleanAbs reports a path that is absolute, clean (no "..", no "//", no trailing
// slash) and free of control characters.
func CleanAbs(p string) bool {
	if p == "" || len(p) > 4096 || !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Inside reports that p is a clean absolute path equal to or below one of roots
// (lexically; the worker resolves symlinks as well).
func Inside(p string, roots []string) bool {
	if !CleanAbs(p) {
		return false
	}
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, strings.TrimSuffix(r, "/")+"/") {
			return true
		}
	}
	return false
}

var refChars = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+-]*$`)

// ValidRefName accepts a plain branch name such as main, feature/x or v1.2: no
// leading "-", no "..", "//", "@{", trailing "/" or "." or ".lock", at most 200 bytes.
func ValidRefName(s string) bool {
	if s == "" || len(s) > 200 || !refChars.MatchString(s) {
		return false
	}
	if strings.Contains(s, "..") || strings.Contains(s, "//") || strings.HasSuffix(s, "/") ||
		strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") || strings.Contains(s, "/.") {
		return false
	}
	return true
}

var scpURL = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._~/-]+$`)

// ValidGitURL accepts https://host/path (no credentials, query or fragment),
// ssh://[user@]host[:port]/path and the scp form git@host:path.
func ValidGitURL(s string) bool {
	if s == "" || len(s) > 2000 || strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t\r\n\\\"'`") {
		return false
	}
	if scpURL.MatchString(s) {
		return !strings.HasPrefix(s[strings.Index(s, ":")+1:], "-")
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path == "" || u.Path == "/" {
		return false
	}
	if strings.HasPrefix(u.Hostname(), "-") {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.User == nil
	case "ssh":
		_, hasPass := u.User.Password()
		return !hasPass
	}
	return false
}
