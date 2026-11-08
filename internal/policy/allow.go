package policy

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

// allowTools pass without a check (DESIGN §18.2 step 3); the deny rules ran first, so
// a Read of a secret is already refused.
var allowTools = []string{"Read", "Glob", "Grep", "WebFetch", "mcp__mado__web_search", "mcp__mado__fetch_url",
	"TodoWrite", "Task", "Agent"}

type allowRule struct {
	re *regexp.Regexp
	// read: the command only reads, so absolute paths outside the roots are fine
	// unless they point into a home directory or /proc. Otherwise every path the
	// command names must lie under a project root or /tmp.
	read bool
	// reject refuses a matching command that uses a dangerous option.
	reject func(ws []string) bool
}

func hasArg(ws []string, pred func(string) bool) bool { return slices.ContainsFunc(ws[1:], pred) }

func argIn(names ...string) func(string) bool {
	return func(a string) bool {
		for _, n := range names {
			if a == n || strings.HasPrefix(a, n+"=") {
				return true
			}
		}
		return false
	}
}

var sedWrite = regexp.MustCompile(`(^|[/;}\d$])\s*[wWe](\s|$)`)

// allowRules are matched on the words of each command joined by single spaces.
var allowRules = []allowRule{
	{re: regexp.MustCompile(`^git (status|diff|log|show|branch|rev-parse|remote -v|stash list)( |$)`), read: true,
		reject: func(ws []string) bool { return hasArg(ws, argIn("--output", "--ext-diff", "--textconv")) }},
	{re: regexp.MustCompile(`^git (add|commit)( |$)`)},
	{re: regexp.MustCompile(`^(ls|cat|head|tail|wc|pwd|echo|which|stat|file|du|df|diff)( |$)`), read: true},
	{re: regexp.MustCompile(`^env$`), read: true},
	{re: regexp.MustCompile(`^date( |$)`), read: true, reject: func(ws []string) bool { return hasArg(ws, argIn("-s", "--set")) }},
	{re: regexp.MustCompile(`^tree( |$)`), read: true, reject: func(ws []string) bool { return hasArg(ws, argIn("-o")) }},
	{re: regexp.MustCompile(`^(grep|egrep|fgrep)( |$)`), read: true},
	{re: regexp.MustCompile(`^rg( |$)`), read: true, reject: func(ws []string) bool { return hasArg(ws, argIn("--pre", "--pre-glob")) }},
	{re: regexp.MustCompile(`^find( |$)`), read: true, reject: func(ws []string) bool {
		return hasArg(ws, argIn("-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls"))
	}},
	{re: regexp.MustCompile(`^fd( |$)`), read: true, reject: func(ws []string) bool {
		return hasArg(ws, argIn("-x", "--exec", "-X", "--exec-batch"))
	}},
	{re: regexp.MustCompile(`^awk( |$)`), read: true, reject: func(ws []string) bool {
		return hasArg(ws, func(a string) bool {
			return strings.ContainsAny(a, "|>") || strings.Contains(a, "system") || strings.Contains(a, "getline")
		})
	}},
	{re: regexp.MustCompile(`^sed -n( |$)`), read: true, reject: func(ws []string) bool {
		return hasArg(ws, func(a string) bool { return strings.HasPrefix(a, "-i") || a == "--in-place" || sedWrite.MatchString(a) })
	}},
	{re: regexp.MustCompile(`^go (build|test|vet|run|fmt|generate)( |$)`)},
	{re: regexp.MustCompile(`^go mod tidy$`)},
	{re: regexp.MustCompile(`^make( [a-z-]+)?$`)},
	{re: regexp.MustCompile(`^python3? (-I )?-m pytest( |$)`)},
	{re: regexp.MustCompile(`^pytest( |$)`)},
	{re: regexp.MustCompile(`^cargo (build|test|check|fmt|clippy)( |$)`)},
	{re: regexp.MustCompile(`^npm (test|run [a-z:-]+)( |$)`)},
	{re: regexp.MustCompile(`^mkdir -p( |$)`)},
	{re: regexp.MustCompile(`^(cp|mv|touch)( |$)`)},
	{re: regexp.MustCompile(`^cd [^ ]+$`)},
}

func (p *Policy) allowed(r Request) string {
	if r.Tool != "Bash" {
		if slices.Contains(allowTools, r.Tool) {
			return r.Tool
		}
		if editInProject(r) {
			return "edit-in-project"
		}
		return ""
	}
	parsed := Split(bashCommand(r.Input))
	if parsed.Opaque != "" || len(parsed.Commands) == 0 {
		return ""
	}
	okDirs := append([]string{"/tmp"}, r.Roots...)
	cwd := r.CWD
	var names []string
	for _, c := range parsed.Commands {
		for _, rd := range c.Redirs {
			if !redirOK(rd, cwd, okDirs) {
				return ""
			}
		}
		ws, wrapped := strip(c.Words)
		if len(ws) == 0 {
			if len(c.Words) > 0 {
				return "" // only assignments: they change later commands
			}
			continue
		}
		if wrapped {
			return ""
		}
		ts := texts(ws)
		line := strings.Join(ts, " ")
		rule, ok := p.match(line, ts)
		if !ok {
			return ""
		}
		if !pathsOK(ws, cwd, okDirs, rule.read) {
			return ""
		}
		if ts[0] == "cd" {
			cwd = cleanAbs(ts[1], cwd)
		}
		names = appendUnique(names, commandName(ts))
	}
	return strings.Join(names, ", ")
}

// editInProject allows a file edit under a project root or /tmp (DESIGN §18.6). A
// session switched to mado auto mid-session may still run claude in default, where
// such edits reach the hub; in acceptEdits they never ask. The deny rules (secret
// paths, system directories) ran first.
func editInProject(r Request) bool {
	if !slices.Contains(writeTools, r.Tool) {
		return false
	}
	var in map[string]any
	if json.Unmarshal(r.Input, &in) != nil {
		return false
	}
	n := 0
	for _, k := range fileToolPaths {
		v, ok := in[k]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" || strings.HasPrefix(s, "~") || strings.Contains(s, "$") {
			return false
		}
		ap := cleanAbs(s, r.CWD)
		if ap == "" || !underAny(ap, append([]string{"/tmp"}, r.Roots...)) {
			return false
		}
		n++
	}
	return n > 0
}

func (p *Policy) match(line string, ts []string) (allowRule, bool) {
	for _, r := range allowRules {
		if r.re.MatchString(line) {
			if r.reject != nil && r.reject(ts) {
				return r, false
			}
			return r, true
		}
	}
	for _, re := range p.allow {
		if re.MatchString(line) {
			return allowRule{re: re}, true
		}
	}
	return allowRule{}, false
}

// commandName is the decision's label: the program and, for tools with
// subcommands, the subcommand ("git status", "go test", "make check").
func commandName(ts []string) string {
	switch ts[0] {
	case "git", "go", "cargo", "npm", "make":
		if len(ts) > 1 && !strings.HasPrefix(ts[1], "-") {
			return ts[0] + " " + ts[1]
		}
	}
	return ts[0]
}

func appendUnique(xs []string, x string) []string {
	if slices.Contains(xs, x) {
		return xs
	}
	return append(xs, x)
}

var devOK = []string{"/dev/null", "/dev/stdout", "/dev/stderr"}

func redirOK(rd Redir, cwd string, okDirs []string) bool {
	t := rd.Target
	if strings.HasPrefix(t.Text, "&") || rd.Op == "<<" || rd.Op == "<<<" {
		return true
	}
	if t.Var || t.Tilde {
		return false
	}
	p := cleanAbs(t.Text, cwd)
	if p == "" {
		return false
	}
	if slices.Contains(devOK, p) {
		return true
	}
	if strings.Contains(rd.Op, ">") {
		return underAny(p, okDirs)
	}
	return readOK(p, okDirs)
}

// homeLike are places a read-only command may not look at without a check: other
// people's and the user's own files outside the projects, and process environments.
var homeLike = []string{"/home", "/root", "/Users", "/var/home", "/proc"}

func readOK(p string, okDirs []string) bool {
	return underAny(p, okDirs) || !underAny(p, homeLike)
}

// pathsOK checks every argument that names a path (an absolute path, a path with a
// slash, `..`, or the value of --flag=path).
func pathsOK(ws []Word, cwd string, okDirs []string, read bool) bool {
	for _, w := range ws[1:] {
		if w.Var || w.Tilde {
			return false
		}
		t := w.Text
		if strings.HasPrefix(t, "-") {
			if i := strings.IndexByte(t, '='); i > 0 {
				t = t[i+1:]
			} else {
				continue
			}
		}
		if strings.HasPrefix(t, "~") {
			return false
		}
		if !strings.HasPrefix(t, "/") && !strings.Contains(t, "/") && t != ".." {
			continue
		}
		p := cleanAbs(t, cwd)
		if p == "" {
			return false
		}
		if strings.HasPrefix(t, "/") && slices.Contains(devOK, p) {
			continue
		}
		if read {
			if !readOK(p, okDirs) {
				return false
			}
		} else if !underAny(p, okDirs) {
			return false
		}
	}
	return true
}
