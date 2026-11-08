// Package history reads Claude Code's JSONL transcripts for the read-only import
// (DESIGN §14). The transcript format is internal to Claude Code: everything here is
// tolerant, a bad line never fails a file, and the raw lines are kept verbatim.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

var reUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// File is one transcript on disk.
type File struct {
	Path       string
	ConfigDir  string // the Claude Code config dir it belongs to
	ProjectDir string // the mangled cwd directory name under projects/
	SessionID  string
	Size       int64
	MTime      int64 // unix nanoseconds
}

// Scan lists <configDir>/projects/*/<uuid>.jsonl for each config dir. Subagent
// transcripts in deeper directories are not included. Symlinks are skipped.
func Scan(configDirs []string) []File {
	var out []File
	for _, cd := range configDirs {
		projs, err := os.ReadDir(filepath.Join(cd, "projects"))
		if err != nil {
			continue
		}
		for _, p := range projs {
			if !p.IsDir() {
				continue
			}
			dir := filepath.Join(cd, "projects", p.Name())
			ents, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				id, ok := strings.CutSuffix(e.Name(), ".jsonl")
				if !ok || !reUUID.MatchString(id) || !e.Type().IsRegular() {
					continue
				}
				fi, err := e.Info()
				if err != nil {
					continue
				}
				out = append(out, File{Path: filepath.Join(dir, e.Name()), ConfigDir: cd, ProjectDir: p.Name(),
					SessionID: id, Size: fi.Size(), MTime: fi.ModTime().UnixNano()})
			}
		}
	}
	return out
}

// DecodeProjectDir turns Claude Code's directory name for a cwd back into a path. The
// mangling replaces every non-alphanumeric character with '-', so this is a guess
// ("-home-me-my-app" may be /home/me/my-app); the cwd field in the lines is preferred.
func DecodeProjectDir(name string) string {
	if !strings.HasPrefix(name, "-") {
		return name
	}
	return strings.ReplaceAll(name, "-", "/")
}

// Line is one complete transcript line and its byte offset in the file.
type Line struct {
	Off int64
	Raw []byte
}

// OversizeLine replaces a line longer than the limit; the original is not shipped.
func OversizeLine(n int64) []byte {
	b, _ := json.Marshal(map[string]any{"type": "mado_oversize", "bytes": n})
	return b
}

// ReadTail reads complete lines from offset on and calls emit with batches of at most
// batchBytes (a single longer line forms its own batch), each with the offset just past
// its last line, so one batch ends where the next begins. Lines longer than maxLine are
// replaced by OversizeLine. A last line without a newline is not returned: the file is
// probably being written. It returns the offset just past the last complete line.
func ReadTail(path string, offset int64, maxLine, batchBytes int, emit func([]Line, int64) error) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	r := bufio.NewReaderSize(f, 256<<10)
	pos := offset
	var batch []Line
	size := 0
	// flush hands over the batch; end is the offset just past its last line.
	flush := func(end int64) error {
		if len(batch) == 0 {
			return nil
		}
		err := emit(batch, end)
		batch, size = nil, 0
		return err
	}
	for {
		line, n, complete, err := readLine(r, maxLine)
		if !complete {
			if ferr := flush(pos); ferr != nil {
				return pos, ferr
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return pos, err
			}
			return pos, nil
		}
		start := pos
		pos += n
		if line == nil {
			line = OversizeLine(n - 1)
		}
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		if size > 0 && size+len(line) > batchBytes {
			if err := flush(start); err != nil {
				return start, err
			}
		}
		batch = append(batch, Line{Off: start, Raw: line})
		size += len(line)
	}
}

// readLine reads up to and including '\n'. It returns the line without the newline,
// the number of bytes consumed and whether a newline was found. A line longer than
// max is consumed but returned as nil.
func readLine(r *bufio.Reader, max int) ([]byte, int64, bool, error) {
	var buf []byte
	var n int64
	over := false
	for {
		chunk, err := r.ReadSlice('\n')
		n += int64(len(chunk))
		if !over {
			if len(buf)+len(chunk) > max+1 {
				over, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err == nil {
			if over {
				return nil, n, true, nil
			}
			return buf[:len(buf)-1], n, true, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, n, false, err
	}
}

// Meta is what the lines of a transcript tell about it.
type Meta struct {
	CWD         string
	Title       string
	TitleCustom bool // from a custom-title entry (/rename), which wins over the first prompt
}

type entry struct {
	Type        string `json:"type"`
	CWD         string `json:"cwd"`
	CustomTitle string `json:"customTitle"`
	IsMeta      bool   `json:"isMeta"`
	Message     *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// Scan updates m from more lines (in file order). Bad lines are ignored.
func (m *Meta) Scan(lines []Line) {
	for _, l := range lines {
		var e entry
		if json.Unmarshal(l.Raw, &e) != nil {
			continue
		}
		if m.CWD == "" && e.CWD != "" {
			m.CWD = e.CWD
		}
		switch e.Type {
		case "custom-title":
			if t := cleanTitle(e.CustomTitle); t != "" {
				m.Title, m.TitleCustom = t, true
			}
		case "user":
			if m.Title == "" && !e.IsMeta && e.Message != nil {
				m.Title = cleanTitle(promptText(e.Message.Content))
			}
		}
	}
}

// promptText is the typed text of a user entry, or "" for tool results and the
// wrappers Claude Code puts around slash commands and their output.
func promptText(c json.RawMessage) string {
	var s string
	if json.Unmarshal(c, &s) != nil {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(c, &parts) != nil {
			return ""
		}
		for _, p := range parts {
			if p.Type == "text" {
				s = p.Text
				break
			}
		}
	}
	if t := strings.TrimSpace(s); strings.HasPrefix(t, "<") {
		return ""
	}
	return s
}

func cleanTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 120 {
		s = string([]rune(s)[:120]) + "…"
	}
	return s
}
