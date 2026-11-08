package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kinginu/mado/internal/proto"
)

// The concierge's file system frames (DESIGN §20.2, §20.3). Every path is checked
// against the project roots after resolving symlinks, so neither ".." nor a symlink
// leads out of them. git runs with a clean environment and a timeout, and only the
// tail of its output goes back.

const (
	fsInspectTimeout = 18 * time.Second // the hub waits 20 s
	fsQuickTimeout   = 18 * time.Second // mkdir, git_init
	fsGitTimeout     = 10 * time.Minute // clone, worktree
	fsMaxEntries     = 200
	fsReadmeBytes    = 2048
	fsOutputTail     = 2048
)

// ResolveInside resolves p (which need not exist yet) and checks that it stays inside
// one of roots. The longest existing prefix is resolved with EvalSymlinks; the
// components after it do not exist, so they cannot be symlinks.
func ResolveInside(p string, roots []string) (string, error) {
	if !proto.CleanAbs(p) {
		return "", errors.New("path must be absolute and clean (no .., no trailing /)")
	}
	existing, rest := p, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", errors.New("path has no existing parent")
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("path: %w", err)
	}
	resolved := filepath.Join(real, rest)
	for _, r := range roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rr, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return resolved, nil
		}
	}
	return "", errors.New("path is outside the project roots")
}

func fsReply(reqID string, data any, err error) *proto.FSResult {
	if err != nil {
		return &proto.FSResult{RequestID: reqID, Error: err.Error()}
	}
	b, merr := json.Marshal(data)
	if merr != nil {
		return &proto.FSResult{RequestID: reqID, Error: "could not encode the result"}
	}
	return &proto.FSResult{RequestID: reqID, OK: true, Data: b}
}

func (d *Daemon) fsInspect(ctx context.Context, m *proto.FSInspect) *proto.FSResult {
	ctx, cancel := context.WithTimeout(ctx, fsInspectTimeout)
	defer cancel()
	switch m.Kind {
	case proto.InspectListProjects:
		return fsReply(m.RequestID, ListProjectDirs(d.cfg.ProjectRoots), nil)
	case proto.InspectDir:
		v, err := InspectDir(ctx, m.Path, d.cfg.ProjectRoots)
		return fsReply(m.RequestID, v, err)
	}
	return fsReply(m.RequestID, nil, errors.New("unknown inspect kind"))
}

func (d *Daemon) fsPrepare(ctx context.Context, m *proto.FSPrepare) *proto.FSResult {
	out, err := Prepare(ctx, m, d.cfg.ProjectRoots)
	if err != nil {
		d.log.Info("prepare step failed", "action", m.Action, "path", m.Path, "err", err)
	} else {
		d.log.Info("prepare step done", "action", m.Action, "path", m.Path)
	}
	return fsReply(m.RequestID, map[string]string{"output": out}, err)
}

// ProjectDir is one top-level directory of a root.
type ProjectDir struct {
	Name  string `json:"name"`
	IsGit bool   `json:"is_git"`
	MTime string `json:"mtime"`
}

// RootListing is list_projects for one root: its directories, newest first.
type RootListing struct {
	Root string       `json:"root"`
	Dirs []ProjectDir `json:"dirs"`
	More int          `json:"more,omitempty"` // directories left out after the first 200
}

// ListProjectDirs lists the non-hidden directories of every root (a symlink only
// when it resolves inside the root), at most 200 per root, most recently changed first.
func ListProjectDirs(roots []string) map[string]any {
	out := []RootListing{}
	for _, r := range roots {
		rl := RootListing{Root: r, Dirs: []ProjectDir{}}
		ents, _ := os.ReadDir(r)
		type dir struct {
			ProjectDir
			t time.Time
		}
		var ds []dir
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			full := filepath.Join(r, e.Name())
			if e.Type()&os.ModeSymlink != 0 {
				if _, err := ResolveInside(full, []string{r}); err != nil {
					continue
				}
			}
			fi, err := os.Stat(full)
			if err != nil || !fi.IsDir() {
				continue
			}
			_, gerr := os.Lstat(filepath.Join(full, ".git"))
			ds = append(ds, dir{ProjectDir{Name: e.Name(), IsGit: gerr == nil, MTime: fi.ModTime().UTC().Format(time.RFC3339)}, fi.ModTime()})
		}
		sort.SliceStable(ds, func(i, j int) bool { return ds[i].t.After(ds[j].t) })
		for i, x := range ds {
			if i >= fsMaxEntries {
				rl.More = len(ds) - fsMaxEntries
				break
			}
			rl.Dirs = append(rl.Dirs, x.ProjectDir)
		}
		out = append(out, rl)
	}
	return map[string]any{"roots": out}
}

// Entry is one directory entry of inspect.
type Entry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir,omitempty"`
	Link bool   `json:"symlink,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// GitSummary describes the repository a directory belongs to.
type GitSummary struct {
	Root      string     `json:"root"`
	Branch    string     `json:"branch"`
	Upstream  string     `json:"upstream,omitempty"`
	Dirty     int        `json:"dirty"`
	Worktrees []Worktree `json:"worktrees,omitempty"`
}

// Worktree is one line of `git worktree list`.
type Worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head,omitempty"`
}

// Inspection is the inspect result.
type Inspection struct {
	Path      string      `json:"path"`
	Entries   []Entry     `json:"entries"`
	More      int         `json:"more,omitempty"`
	Readme    string      `json:"readme,omitempty"`
	ReadmeOf  string      `json:"readme_file,omitempty"`
	Git       *GitSummary `json:"git,omitempty"`
	Languages []string    `json:"languages,omitempty"`
}

// InspectDir looks at one directory inside the roots.
func InspectDir(ctx context.Context, p string, roots []string) (*Inspection, error) {
	real, err := ResolveInside(p, roots)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, errors.New("no such directory")
	}
	if !fi.IsDir() {
		return nil, errors.New("not a directory")
	}
	ents, err := os.ReadDir(real)
	if err != nil {
		return nil, errors.New("cannot read the directory")
	}
	sort.SliceStable(ents, func(i, j int) bool {
		di, dj := ents[i].IsDir(), ents[j].IsDir()
		if di != dj {
			return di
		}
		return ents[i].Name() < ents[j].Name()
	})
	v := &Inspection{Path: p, Entries: []Entry{}}
	for i, e := range ents {
		if i >= fsMaxEntries {
			v.More = len(ents) - fsMaxEntries
			break
		}
		x := Entry{Name: e.Name(), Dir: e.IsDir(), Link: e.Type()&os.ModeSymlink != 0}
		if e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				x.Size = info.Size()
			}
		}
		v.Entries = append(v.Entries, x)
	}
	for _, e := range ents {
		if e.Type().IsRegular() && strings.HasPrefix(strings.ToLower(e.Name()), "readme") {
			if text, err := readHead(filepath.Join(real, e.Name()), fsReadmeBytes); err == nil {
				v.Readme, v.ReadmeOf = text, e.Name()
			}
			break
		}
	}
	v.Git = gitSummary(ctx, real)
	v.Languages = guessLanguages(real)
	return v, nil
}

// readHead reads up to n bytes and cuts at a rune boundary.
func readHead(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(n)))
	if err != nil {
		return "", err
	}
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b), nil
}

func gitSummary(ctx context.Context, dir string) *GitSummary {
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	// fsmonitor could run a command named in the repository's config.
	git := func(args ...string) (string, error) {
		out, err := runGit(ctx, dir, append([]string{"-c", "core.fsmonitor=false"}, args...)...)
		return strings.TrimSpace(out), err
	}
	top, err := git("rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return nil
	}
	g := &GitSummary{Root: top}
	g.Branch, _ = git("rev-parse", "--abbrev-ref", "HEAD")
	if up, err := git("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
		g.Upstream = up
	}
	if st, err := git("status", "--porcelain"); err == nil && st != "" {
		g.Dirty = strings.Count(st, "\n") + 1
	}
	if wl, err := git("worktree", "list", "--porcelain"); err == nil {
		var cur *Worktree
		for _, l := range strings.Split(wl, "\n") {
			switch {
			case strings.HasPrefix(l, "worktree "):
				g.Worktrees = append(g.Worktrees, Worktree{Path: strings.TrimPrefix(l, "worktree ")})
				cur = &g.Worktrees[len(g.Worktrees)-1]
			case cur != nil && strings.HasPrefix(l, "branch "):
				cur.Branch = strings.TrimPrefix(strings.TrimPrefix(l, "branch "), "refs/heads/")
			case cur != nil && strings.HasPrefix(l, "HEAD "):
				cur.Head = abbrevHash(strings.TrimPrefix(l, "HEAD "))
			}
		}
		if len(g.Worktrees) > 50 {
			g.Worktrees = g.Worktrees[:50]
		}
	}
	return g
}

func abbrevHash(h string) string {
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

var langByExt = map[string]string{
	".go": "Go", ".py": "Python", ".rs": "Rust", ".js": "JavaScript", ".mjs": "JavaScript", ".ts": "TypeScript",
	".tsx": "TypeScript", ".jsx": "JavaScript", ".c": "C", ".h": "C/C++", ".cc": "C++", ".cpp": "C++", ".hpp": "C++",
	".cu": "CUDA", ".java": "Java", ".kt": "Kotlin", ".swift": "Swift", ".rb": "Ruby", ".php": "PHP", ".cs": "C#",
	".sh": "Shell", ".lua": "Lua", ".zig": "Zig", ".hs": "Haskell", ".ml": "OCaml", ".ex": "Elixir", ".scala": "Scala",
	".dart": "Dart", ".r": "R", ".jl": "Julia", ".md": "Markdown", ".tex": "TeX", ".html": "HTML", ".css": "CSS",
	".sql": "SQL", ".proto": "Protobuf", ".tf": "Terraform", ".vue": "Vue", ".svelte": "Svelte", ".ipynb": "Jupyter",
}

var langByFile = map[string]string{
	"go.mod": "Go", "Cargo.toml": "Rust", "package.json": "JavaScript", "pyproject.toml": "Python",
	"requirements.txt": "Python", "CMakeLists.txt": "C/C++", "Gemfile": "Ruby", "pom.xml": "Java",
	"build.gradle": "Java", "Package.swift": "Swift", "mix.exs": "Elixir", "Makefile": "Make", "Dockerfile": "Docker",
}

var skipDirs = map[string]bool{"node_modules": true, "vendor": true, "target": true, "build": true, "dist": true,
	"__pycache__": true, ".venv": true, "venv": true, "third_party": true}

// guessLanguages counts file names two levels deep (at most 3000 entries) and
// returns the five most common languages, marker files counting double.
func guessLanguages(dir string) []string {
	counts := map[string]int{}
	seen := 0
	var walk func(d string, depth int)
	walk = func(d string, depth int) {
		ents, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range ents {
			if seen >= 3000 {
				return
			}
			seen++
			n := e.Name()
			if strings.HasPrefix(n, ".") {
				continue
			}
			if e.IsDir() {
				if depth < 2 && !skipDirs[n] {
					walk(filepath.Join(d, n), depth+1)
				}
				continue
			}
			if l, ok := langByFile[n]; ok {
				counts[l] += 2
			}
			if l, ok := langByExt[strings.ToLower(filepath.Ext(n))]; ok {
				counts[l]++
			}
		}
	}
	walk(dir, 0)
	type lc struct {
		l string
		n int
	}
	var ls []lc
	for l, n := range counts {
		ls = append(ls, lc{l, n})
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].n > ls[j].n || ls[i].n == ls[j].n && ls[i].l < ls[j].l })
	var out []string
	for i, x := range ls {
		if i >= 5 {
			break
		}
		out = append(out, x.l+" ("+strconv.Itoa(x.n)+")")
	}
	return out
}

// gitEnv is the clean environment git runs in: no router token, no API keys, no
// prompts; HOME and the ssh agent so the user's git config and keys still work.
func gitEnv() []string {
	env := []string{"LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes"}
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin"
	}
	env = append(env, "PATH="+path)
	for _, k := range []string{"HOME", "USER", "LOGNAME", "SSH_AUTH_SOCK", "TMPDIR"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// tailWriter keeps the last n bytes written.
type tailWriter struct {
	n int
	b []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*t.n {
		t.b = append([]byte(nil), t.b[len(t.b)-t.n:]...)
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	b := t.b
	if len(b) > t.n {
		b = b[len(b)-t.n:]
	}
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[1:]
	}
	return string(bytes.TrimSpace(b))
}

// runGit runs git in dir and returns stdout (inspect) or, on failure, an error with the
// tail of stderr.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	var out bytes.Buffer
	errs := &tailWriter{n: fsOutputTail}
	cmd.Stdout, cmd.Stderr = &out, errs
	if err := cmd.Run(); err != nil {
		return "", gitError(ctx, err, errs.String())
	}
	return out.String(), nil
}

// runGitTail runs git and returns the tail of its combined output.
func runGitTail(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	tail := &tailWriter{n: fsOutputTail}
	cmd.Stdout, cmd.Stderr = tail, tail
	err := cmd.Run()
	if err != nil {
		return tail.String(), gitError(ctx, err, tail.String())
	}
	return tail.String(), nil
}

func gitError(ctx context.Context, err error, tail string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = errors.New("timed out")
	}
	if tail != "" {
		return fmt.Errorf("git: %v: %s", err, tail)
	}
	return fmt.Errorf("git: %v", err)
}

// existingParent is the nearest existing directory at or above p.
func existingParent(p string) string {
	for {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

func emptyOrMissing(p string) error {
	ents, err := os.ReadDir(p)
	if errors.Is(err, os.ErrNotExist) {
		if _, lerr := os.Lstat(p); lerr == nil {
			return errors.New(p + " exists and is not a directory")
		}
		return nil
	}
	if err != nil {
		return errors.New(p + " exists and is not a directory")
	}
	if len(ents) > 0 {
		return errors.New(p + " already exists and is not empty")
	}
	return nil
}

// Prepare runs one step of an approved plan and returns git's output tail.
func Prepare(ctx context.Context, m *proto.FSPrepare, roots []string) (string, error) {
	timeout := fsQuickTimeout
	if m.Action == proto.ActionClone || m.Action == proto.ActionWorktree {
		timeout = fsGitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	path, err := ResolveInside(m.Path, roots)
	if err != nil {
		return "", err
	}
	needGit := func() error {
		if _, err := exec.LookPath("git"); err != nil {
			return errors.New("git is not installed on this machine")
		}
		return nil
	}
	switch m.Action {
	case proto.ActionMkdir:
		if fi, err := os.Stat(path); err == nil {
			if !fi.IsDir() {
				return "", errors.New(m.Path + " exists and is not a directory")
			}
			return "already exists", nil
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return "", fmt.Errorf("mkdir: %v", errors.Unwrap(err))
		}
		return "created", nil
	case proto.ActionGitInit:
		if err := needGit(); err != nil {
			return "", err
		}
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			return "", errors.New(m.Path + " exists and is not a directory")
		}
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			return "already a git repository", nil
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return "", fmt.Errorf("mkdir: %v", errors.Unwrap(err))
		}
		return runGitTail(ctx, path, "init")
	case proto.ActionClone:
		if !proto.ValidGitURL(m.URL) {
			return "", errors.New("url must be https://, ssh:// or git@host:path")
		}
		if m.Branch != "" && !proto.ValidRefName(m.Branch) {
			return "", errors.New("bad branch name")
		}
		if err := needGit(); err != nil {
			return "", err
		}
		if err := emptyOrMissing(path); err != nil {
			return "", err
		}
		args := []string{"clone"}
		if m.Branch != "" {
			args = append(args, "--branch", m.Branch)
		}
		args = append(args, "--", m.URL, path)
		return runGitTail(ctx, existingParent(filepath.Dir(path)), args...)
	case proto.ActionWorktree:
		if !proto.ValidRefName(m.Branch) || (m.Base != "" && !proto.ValidRefName(m.Base)) {
			return "", errors.New("worktree needs a plain branch name (and base, if given)")
		}
		if err := needGit(); err != nil {
			return "", err
		}
		repo, err := ResolveInside(m.Repo, roots)
		if err != nil {
			return "", fmt.Errorf("repo: %w", err)
		}
		if fi, err := os.Stat(repo); err != nil || !fi.IsDir() {
			return "", errors.New("repo " + m.Repo + " is not a directory")
		}
		if _, err := runGit(ctx, repo, "rev-parse", "--git-dir"); err != nil {
			return "", errors.New("repo " + m.Repo + " is not a git repository")
		}
		if _, err := os.Lstat(path); err == nil {
			return "", errors.New(m.Path + " already exists")
		}
		if _, err := runGit(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+m.Branch); err == nil {
			return runGitTail(ctx, repo, "worktree", "add", path, m.Branch)
		}
		args := []string{"worktree", "add", "-b", m.Branch, path}
		if m.Base != "" {
			args = append(args, m.Base)
		}
		return runGitTail(ctx, repo, args...)
	}
	return "", errors.New("unknown action " + m.Action)
}
