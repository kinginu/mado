package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kinginu/mado/internal/proto"
)

// The file viewer (DESIGN §27.2): fs_read reads one file of a code session for the
// hub. A path may name a file inside the project roots, the session cwd or the worker
// user's home directory, after "~" expansion, joining a relative path to the cwd and
// resolving symlinks.

// fsReadError is a refusal with the code the hub maps to an HTTP status.
type fsReadError struct {
	code string
	msg  string
	size int64
}

func (e *fsReadError) Error() string { return e.msg }

func readErr(code, msg string) error { return &fsReadError{code: code, msg: msg} }

// insideDir reports that p is dir or below it (both clean and absolute).
func insideDir(p, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// ExpandPath turns the path as written in the transcript into a clean absolute path:
// "~" and "~/x" are below home, a relative path is below cwd.
func ExpandPath(p, cwd, home string) (string, error) {
	if p == "" || len(p) > 4096 || strings.ContainsAny(p, "\x00\r\n") {
		return "", readErr(proto.FSCodeNotFound, "no such file")
	}
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		if home == "" {
			return "", readErr(proto.FSCodeForbidden, "outside the allowed directories")
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	case strings.HasPrefix(p, "~"):
		return "", readErr(proto.FSCodeForbidden, "outside the allowed directories") // ~user
	case !filepath.IsAbs(p):
		if !proto.CleanAbs(cwd) {
			return "", readErr(proto.FSCodeForbidden, "a relative path needs the session's cwd")
		}
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p), nil
}

// ResolveReadable expands p and checks it against the allowed directories (roots, cwd,
// home): lexically first, so a path outside them is refused before anything is looked
// up, then again after EvalSymlinks, so no symlink leads out. It returns the resolved
// path and its FileInfo, a regular file not inside a .git/objects directory.
func ResolveReadable(p, cwd, home string, roots []string) (string, os.FileInfo, error) {
	full, err := ExpandPath(p, cwd, home)
	if err != nil {
		return "", nil, err
	}
	var dirs []string
	for _, r := range roots {
		dirs = append(dirs, filepath.Clean(r))
	}
	if proto.CleanAbs(cwd) {
		dirs = append(dirs, cwd)
	}
	if home != "" {
		dirs = append(dirs, filepath.Clean(home))
	}
	var real []string // the allowed directories with symlinks resolved
	for _, d := range dirs {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			real = append(real, r)
		}
	}
	allowed := func(p string, ds []string) bool {
		for _, d := range ds {
			if insideDir(p, d) {
				return true
			}
		}
		return false
	}
	if !allowed(full, dirs) && !allowed(full, real) {
		return "", nil, readErr(proto.FSCodeForbidden, "outside the allowed directories")
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, readErr(proto.FSCodeNotFound, "no such file")
		}
		return "", nil, readErr(proto.FSCodeForbidden, "cannot read the file")
	}
	if !allowed(resolved, real) {
		return "", nil, readErr(proto.FSCodeForbidden, "outside the allowed directories")
	}
	if strings.Contains(resolved+"/", "/.git/objects/") {
		return "", nil, readErr(proto.FSCodeForbidden, "inside .git/objects")
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", nil, readErr(proto.FSCodeNotFound, "no such file")
	}
	if fi.IsDir() {
		return "", nil, readErr(proto.FSCodeNotFile, "a directory, not a file")
	}
	if !fi.Mode().IsRegular() {
		return "", nil, readErr(proto.FSCodeNotFile, "not a regular file")
	}
	return resolved, fi, nil
}

// textLike reports a type the viewer shows as text when the bytes are UTF-8.
func textLike(mime, path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return true
	}
	m, _, _ := strings.Cut(mime, ";")
	return strings.HasPrefix(m, "text/") || strings.HasSuffix(m, "json") || strings.HasSuffix(m, "xml") || strings.HasSuffix(m, "javascript")
}

// ReadFile answers fs_read. maxBytes is capped at proto.FSDownloadMax.
func ReadFile(m *proto.FSRead, home string, roots []string) (*proto.FSFile, error) {
	maxBytes := m.MaxBytes
	if maxBytes <= 0 {
		maxBytes = proto.FSViewMax
	}
	maxBytes = min(maxBytes, proto.FSDownloadMax)
	resolved, fi, err := ResolveReadable(m.Path, m.CWD, home, roots)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxBytes {
		return nil, &fsReadError{code: proto.FSCodeTooLarge, msg: fmt.Sprintf("too large (%d bytes, at most %d)", fi.Size(), maxBytes), size: fi.Size()}
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, readErr(proto.FSCodeForbidden, "cannot read the file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, readErr(proto.FSCodeForbidden, "cannot read the file")
	}
	if int64(len(b)) > maxBytes { // it grew since the stat
		return nil, &fsReadError{code: proto.FSCodeTooLarge, msg: "too large", size: int64(len(b))}
	}
	out := &proto.FSFile{Path: resolved, Size: int64(len(b)), Modified: fi.ModTime().UTC().Format(time.RFC3339),
		Mime: http.DetectContentType(b)}
	switch {
	case m.Download:
		out.Bytes = b
	case utf8.Valid(b) && textLike(out.Mime, resolved):
		out.Text = string(b)
	default:
		out.Binary = true
	}
	return out, nil
}

func (d *Daemon) fsRead(m *proto.FSRead) *proto.FSResult {
	home, _ := os.UserHomeDir()
	f, err := ReadFile(m, home, d.cfg.ProjectRoots)
	if err != nil {
		r := &proto.FSResult{RequestID: m.RequestID, Error: err.Error(), Code: proto.FSCodeForbidden}
		var re *fsReadError
		if errors.As(err, &re) {
			r.Code = re.code
			if re.size > 0 {
				r.Data, _ = json.Marshal(proto.FSFile{Size: re.size})
			}
		}
		d.log.Info("file read refused", "session", m.SessionID, "path", m.Path, "err", err)
		return r
	}
	return fsReply(m.RequestID, f, nil)
}
