// Package policy is mado auto mode's approval policy (DESIGN §18): deterministic deny
// and allow rules over a tool call, and a model classifier for what the rules leave
// open. The hub runs it for router-profile sessions in permission mode auto.
package policy

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Decision is a policy outcome. None means no rule matched (the classifier, or the
// user, decides).
type Decision string

const (
	None  Decision = ""
	Allow Decision = "allow"
	Deny  Decision = "deny"
	Ask   Decision = "ask"
)

// Request is one approval request.
type Request struct {
	Tool  string
	Input json.RawMessage
	CWD   string   // the session's working directory
	Roots []string // the machine's project roots
}

// Config is [auto] allow/deny from hub.toml: RE2 regexes added to the defaults.
// Deny regexes are matched on the whole Bash command and on the file path of the other
// tools; allow regexes on each command of a Bash line (after the separators are split
// and wrappers such as `timeout 60` removed).
type Config struct {
	Allow []string
	Deny  []string
}

// Policy holds the compiled rules.
type Policy struct {
	allow []*regexp.Regexp
	deny  []*regexp.Regexp
}

// New compiles the extra rules.
func New(c Config) (*Policy, error) {
	p := &Policy{}
	for _, s := range c.Allow {
		re, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("[auto] allow %q: %w", s, err)
		}
		p.allow = append(p.allow, re)
	}
	for _, s := range c.Deny {
		re, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("[auto] deny %q: %w", s, err)
		}
		p.deny = append(p.deny, re)
	}
	return p, nil
}

// DenyMessage is what claude is told when a rule (or the classifier) denies a call.
func DenyMessage(rule string) string {
	return "mado policy denied this: " + rule + "; ask the user in your reply if it is needed"
}

// Evaluate runs the deny rules, then the allow rules (DESIGN §18.2 steps 2 and 3). It
// returns Deny or Allow with the rule's name, or None.
func (p *Policy) Evaluate(r Request) (Decision, string) {
	if name := p.denied(r); name != "" {
		return Deny, name
	}
	if name := p.allowed(r); name != "" {
		return Allow, name
	}
	return None, ""
}

// --- deny

// secretPaths: any path (or Bash command text) containing one of these is denied.
var secretPaths = []struct {
	name string
	re   *regexp.Regexp
}{
	{".claude/settings.json", regexp.MustCompile(`\.claude/settings(\.local)?\.json`)},
	{".config/mado/", regexp.MustCompile(`\.config/mado(/|$|[\s"';|&)])`)},
	{".ssh/", regexp.MustCompile(`\.ssh(/|$|[\s"';|&)])`)},
	{"secrets.toml", regexp.MustCompile(`secrets\.toml`)},
	{"*.token", regexp.MustCompile(`[\w*-]\.token($|[^\w.-])`)},
	{".env", regexp.MustCompile(`(^|[\s"'=/:])\.env([\w.-]*)($|[\s"';|&)/])`)},
	{"id_rsa", regexp.MustCompile(`id_rsa`)},
	{"id_ed25519", regexp.MustCompile(`id_ed25519`)},
}

// rawDeny is matched on the whole Bash command text.
var rawDeny = []struct {
	name string
	re   *regexp.Regexp
}{
	{"curl | sh", regexp.MustCompile(`\b(curl|wget)\b[^\n;&]*\|\s*(sudo\s+)?(env\s+)?(ba|z|da|k|fi)?sh\b`)},
	{"curl | sh", regexp.MustCompile(`\b(ba|z|da|k)?sh\b[^\n;&|]*(<\(|\$\(|\x60)\s*(curl|wget)\b`)},
	{"fork bomb", regexp.MustCompile(`:\s*\(\s*\)\s*\{`)},
}

// systemDirs may not be written.
var systemDirs = []string{"/etc", "/usr", "/boot", "/var/lib"}

func underAny(p string, dirs []string) bool {
	for _, d := range dirs {
		d = strings.TrimSuffix(d, "/")
		if d == "" { // the root "/"
			return strings.HasPrefix(p, "/")
		}
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// fileToolPaths are the input fields of the non-Bash tools that name a path.
var fileToolPaths = []string{"file_path", "path", "notebook_path"}

// writeTools change files.
var writeTools = []string{"Write", "Edit", "MultiEdit", "NotebookEdit"}

func (p *Policy) denied(r Request) string {
	if r.Tool == "Bash" {
		return p.deniedBash(bashCommand(r.Input), r.CWD)
	}
	var in map[string]any
	_ = json.Unmarshal(r.Input, &in)
	var paths []string
	for _, k := range fileToolPaths {
		if s, ok := in[k].(string); ok && s != "" {
			paths = append(paths, s)
		}
	}
	if r.Tool == "Glob" {
		if s, ok := in["pattern"].(string); ok {
			paths = append(paths, s)
		}
	}
	for _, ps := range paths {
		for _, sp := range secretPaths {
			if sp.re.MatchString(ps) {
				return "secret path " + sp.name
			}
		}
		for _, re := range p.deny {
			if re.MatchString(ps) {
				return "config deny " + re.String()
			}
		}
		if slices.Contains(writeTools, r.Tool) && underAny(cleanAbs(ps, r.CWD), systemDirs) {
			return "write under " + topDir(cleanAbs(ps, r.CWD))
		}
	}
	return ""
}

func bashCommand(input json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	return in.Command
}

// cleanAbs makes p absolute against cwd and cleans it; "" for a path it cannot place.
func cleanAbs(p, cwd string) string {
	if strings.HasPrefix(p, "/") {
		return path.Clean(p)
	}
	if cwd == "" || strings.HasPrefix(p, "~") || strings.HasPrefix(p, "$") {
		return ""
	}
	return path.Clean(cwd + "/" + p)
}

func topDir(p string) string {
	for _, d := range systemDirs {
		if underAny(p, []string{d}) {
			return d
		}
	}
	return p
}

func (p *Policy) deniedBash(cmd, cwd string) string {
	for _, rd := range rawDeny {
		if rd.re.MatchString(cmd) {
			return rd.name
		}
	}
	for _, sp := range secretPaths {
		if sp.re.MatchString(cmd) {
			return "secret path " + sp.name
		}
	}
	for _, re := range p.deny {
		if re.MatchString(cmd) {
			return "config deny " + re.String()
		}
	}
	return deniedCommands(Split(cmd).Commands, cwd, 0)
}

func deniedCommands(cs []Command, cwd string, depth int) string {
	for _, c := range cs {
		if name := deniedCommand(c, cwd, depth); name != "" {
			return name
		}
	}
	return ""
}

func deniedCommand(c Command, cwd string, depth int) string {
	for _, rd := range c.Redirs {
		if isWriteRedir(rd.Op) {
			if t := cleanAbs(rd.Target.Text, cwd); underAny(t, systemDirs) {
				return "write under " + topDir(t)
			}
		}
	}
	ws, _ := strip(c.Words)
	if len(ws) == 0 {
		return ""
	}
	if depth < 4 {
		// sh -c '...', eval ..., find -exec ...: the inner command counts too.
		if inner, ok := innerCommand(ws); ok {
			if name := deniedCommands(Split(inner).Commands, cwd, depth+1); name != "" {
				return name
			}
			if name := (&Policy{}).deniedBash(inner, cwd); name != "" {
				return name
			}
		}
		for i, w := range ws {
			if ws[0].Text == "find" && (w.Text == "-exec" || w.Text == "-execdir" || w.Text == "-ok" || w.Text == "-okdir") {
				sub := Command{Words: ws[i+1:]}
				if name := deniedCommand(sub, cwd, depth+1); name != "" {
					return name
				}
			}
		}
	}
	for _, r := range wordRules {
		if r.fn(ws) {
			return r.name
		}
	}
	if name := systemWrite(ws, cwd); name != "" {
		return name
	}
	return ""
}

func isWriteRedir(op string) bool {
	return strings.Contains(op, ">")
}

// innerCommand returns the script of `bash -c '...'` and `eval ...`.
func innerCommand(ws []Word) (string, bool) {
	switch ws[0].Text {
	case "bash", "sh", "zsh", "dash", "ksh":
		for i := 1; i < len(ws); i++ {
			if strings.HasPrefix(ws[i].Text, "-") && strings.Contains(ws[i].Text, "c") && !strings.HasPrefix(ws[i].Text, "--") {
				if i+1 < len(ws) {
					return ws[i+1].Text, true
				}
			}
		}
	case "eval":
		return strings.Join(texts(ws[1:]), " "), true
	}
	return "", false
}

func texts(ws []Word) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Text
	}
	return out
}

var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// unsafeAssign are variables that change what a command does beyond its arguments.
var unsafeAssign = regexp.MustCompile(`^(PATH|LD_[A-Z_]*|DYLD_[A-Z_]*|BASH_ENV|ENV|PROMPT_COMMAND|IFS|SHELLOPTS|BASHOPTS|PS4|GIT_[A-Z_]*|PYTHONPATH|PYTHONSTARTUP|NODE_OPTIONS|GOFLAGS|MAKEFLAGS)=`)

// strip removes leading assignments, shell keywords and wrapper commands (env, nohup,
// time, timeout, nice, xargs, exec, command, ...), so the first word is the program
// that runs. wrapped reports a wrapper other than time, timeout, nice or a safe
// assignment, which the allow rules do not accept.
func strip(ws []Word) (out []Word, wrapped bool) {
	i := 0
	for i < len(ws) {
		t := ws[i].Text
		switch {
		case assignRe.MatchString(t):
			if unsafeAssign.MatchString(t) || ws[i].Var {
				wrapped = true
			}
			i++
		case t == "!" || t == "if" || t == "then" || t == "else" || t == "elif" || t == "do" || t == "while" || t == "until":
			wrapped = true
			i++
		case t == "time":
			i++
			for i < len(ws) && strings.HasPrefix(ws[i].Text, "-") {
				i++
			}
		case t == "nice":
			i++
			for i < len(ws) && strings.HasPrefix(ws[i].Text, "-") {
				if ws[i].Text == "-n" {
					i++
				}
				i++
			}
		case t == "timeout":
			i++
			for i < len(ws) && strings.HasPrefix(ws[i].Text, "-") {
				if ws[i].Text == "-s" || ws[i].Text == "-k" || ws[i].Text == "--signal" || ws[i].Text == "--kill-after" {
					i++
				}
				i++
			}
			i++ // the duration
		case t == "env":
			if i == len(ws)-1 {
				return ws[i:], wrapped // bare `env` prints the environment
			}
			wrapped = true
			i++
			for i < len(ws) && (strings.HasPrefix(ws[i].Text, "-") || assignRe.MatchString(ws[i].Text)) {
				if ws[i].Text == "-u" || ws[i].Text == "-C" || ws[i].Text == "-S" {
					i++
				}
				i++
			}
		case t == "nohup" || t == "exec" || t == "builtin" || t == "unbuffer" || t == "caffeinate":
			wrapped = true
			i++
		case t == "command":
			wrapped = true
			i++
			for i < len(ws) && strings.HasPrefix(ws[i].Text, "-") {
				i++
			}
		case t == "stdbuf" || t == "ionice" || t == "chrt" || t == "taskset" || t == "watch":
			wrapped = true
			i++
			for i < len(ws) && strings.HasPrefix(ws[i].Text, "-") {
				if len(ws[i].Text) == 2 { // -c 3, -n 2: the value is the next word
					i++
				}
				i++
			}
			if t == "chrt" || t == "taskset" {
				i++
			}
		case t == "xargs":
			wrapped = true
			i++
			for i < len(ws) && strings.HasPrefix(ws[i].Text, "-") {
				if len(ws[i].Text) == 2 && strings.ContainsRune("IndPLsaE", rune(ws[i].Text[1])) {
					i++
				}
				i++
			}
		default:
			return ws[i:], wrapped
		}
	}
	return nil, wrapped
}

// gitSub returns git's subcommand and its arguments, skipping global options.
func gitSub(ws []Word) (string, []string) {
	if len(ws) == 0 || ws[0].Text != "git" {
		return "", nil
	}
	for i := 1; i < len(ws); i++ {
		t := ws[i].Text
		switch {
		case t == "-C" || t == "-c" || t == "--git-dir" || t == "--work-tree" || t == "--namespace":
			i++
		case strings.HasPrefix(t, "-"):
		default:
			return t, texts(ws[i+1:])
		}
	}
	return "", nil
}

// shortFlag reports a word like -rf that contains the letter (not --long).
func shortFlag(t string, letters string) bool {
	return len(t) > 1 && t[0] == '-' && t[1] != '-' && strings.ContainsAny(t[1:], letters)
}

type wordRule struct {
	name string
	fn   func(ws []Word) bool
}

func first(ws []Word, names ...string) bool { return slices.Contains(names, ws[0].Text) }

func anyArg(ws []Word, pred func(string) bool) bool {
	for _, w := range ws[1:] {
		if pred(w.Text) {
			return true
		}
	}
	return false
}

// wordRules are the deny rules on a command's words (after strip), DESIGN §18.2 step 2.
var wordRules = []wordRule{
	{"sudo", func(ws []Word) bool { return first(ws, "sudo", "doas", "pkexec") }},
	{"su", func(ws []Word) bool { return first(ws, "su") }},
	{"rm -rf", rmDangerous},
	{"git push --force", func(ws []Word) bool {
		sub, args := gitSub(ws)
		return sub == "push" && slices.ContainsFunc(args, func(a string) bool {
			return a == "--force" || strings.HasPrefix(a, "--force-with-lease") || a == "--mirror" ||
				shortFlag(a, "f") || strings.HasPrefix(a, "+")
		})
	}},
	{"git reset --hard", func(ws []Word) bool {
		sub, args := gitSub(ws)
		return sub == "reset" && slices.Contains(args, "--hard")
	}},
	{"git clean -f", func(ws []Word) bool {
		sub, args := gitSub(ws)
		return sub == "clean" && slices.ContainsFunc(args, func(a string) bool { return a == "--force" || shortFlag(a, "f") })
	}},
	{"git checkout -- .", func(ws []Word) bool {
		sub, args := gitSub(ws)
		if sub != "checkout" && sub != "restore" {
			return false
		}
		return slices.ContainsFunc(args, func(a string) bool { return a == "." || a == "./" || a == "*" || a == ":/" })
	}},
	{"chmod -R 777", func(ws []Word) bool {
		return first(ws, "chmod") && anyArg(ws, func(a string) bool { return a == "--recursive" || shortFlag(a, "R") }) &&
			anyArg(ws, func(a string) bool { return a == "777" || a == "0777" || a == "a+rwx" || a == "ugo+rwx" })
	}},
	{"mkfs", func(ws []Word) bool {
		return ws[0].Text == "mkfs" || strings.HasPrefix(ws[0].Text, "mkfs.") || ws[0].Text == "mke2fs" || ws[0].Text == "wipefs"
	}},
	{"dd of=/dev", func(ws []Word) bool {
		return first(ws, "dd") && anyArg(ws, func(a string) bool { return strings.HasPrefix(a, "of=/dev/") })
	}},
	{"shutdown", func(ws []Word) bool { return first(ws, "shutdown", "reboot", "halt", "poweroff") }},
	{"systemctl stop", func(ws []Word) bool {
		return first(ws, "systemctl") && anyArg(ws, func(a string) bool {
			return slices.Contains([]string{"stop", "disable", "mask", "kill", "poweroff", "reboot", "halt", "isolate"}, a)
		})
	}},
	{"kill -9", func(ws []Word) bool {
		if !first(ws, "kill") {
			return false
		}
		for i, w := range ws {
			switch w.Text {
			case "-9", "-KILL", "-SIGKILL", "--signal=KILL", "--signal=9":
				return true
			case "-s", "--signal", "-n":
				if i+1 < len(ws) && slices.Contains([]string{"9", "KILL", "SIGKILL"}, ws[i+1].Text) {
					return true
				}
			}
		}
		return false
	}},
	{"pkill", func(ws []Word) bool { return first(ws, "pkill") }},
	{"killall", func(ws []Word) bool { return first(ws, "killall") }},
	{"tmux kill-server", func(ws []Word) bool {
		return first(ws, "tmux") && anyArg(ws, func(a string) bool { return a == "kill-server" })
	}},
	{"claude login", func(ws []Word) bool {
		return first(ws, "claude") && anyArg(ws, func(a string) bool {
			return a == "login" || a == "/login" || a == "logout" || a == "/logout" || a == "setup-token"
		})
	}},
}

// rmDangerous: a recursive rm of /, a top-level directory, ~, $HOME, ., .., anything
// above the working directory, or *.
func rmDangerous(ws []Word) bool {
	if !first(ws, "rm") {
		return false
	}
	recursive := false
	var targets []Word
	opts := true
	for _, w := range ws[1:] {
		switch {
		case opts && w.Text == "--":
			opts = false
		case opts && (w.Text == "--recursive" || shortFlag(w.Text, "rR")):
			recursive = true
		case opts && strings.HasPrefix(w.Text, "-"):
		default:
			targets = append(targets, w)
		}
	}
	if !recursive {
		return false
	}
	for _, w := range targets {
		t := w.Text
		for strings.HasSuffix(t, "/*") || (len(t) > 1 && strings.HasSuffix(t, "/")) {
			t = strings.TrimSuffix(strings.TrimSuffix(t, "*"), "/")
		}
		switch {
		case t == "" || t == "/" || t == "*" || t == ".*" || t == "." || t == "~" || t == "$HOME" || t == "${HOME}":
			return true
		case t == ".." || strings.HasPrefix(t, "../"):
			return true
		case strings.HasPrefix(t, "~/") && !strings.Contains(t[2:], "/"): // ~/x: a home directory entry
			return true
		case strings.HasPrefix(t, "$HOME/") || strings.HasPrefix(t, "${HOME}/"):
			return true
		case strings.HasPrefix(t, "/"):
			c := path.Clean(t)
			n := strings.Count(c, "/")
			if n <= 1 || (n == 2 && (strings.HasPrefix(c, "/home/") || strings.HasPrefix(c, "/Users/"))) {
				return true
			}
		}
	}
	return false
}

// writeCommands change the paths they are given.
var writeCommands = []string{"cp", "mv", "rm", "rmdir", "touch", "mkdir", "chmod", "chown", "chgrp", "ln", "install",
	"tee", "truncate", "sed", "dd", "rsync", "unlink", "shred"}

// systemWrite: a write command naming a path under a system directory. For cp, mv,
// install, ln and rsync only the destination (the last argument) counts, so copying
// out of /etc stays possible.
func systemWrite(ws []Word, cwd string) string {
	if !slices.Contains(writeCommands, ws[0].Text) {
		return ""
	}
	if ws[0].Text == "sed" && !anyArg(ws, func(a string) bool { return a == "-i" || strings.HasPrefix(a, "-i") || a == "--in-place" }) {
		return ""
	}
	var args []string
	for _, w := range ws[1:] {
		if !strings.HasPrefix(w.Text, "-") {
			args = append(args, w.Text)
		}
	}
	if len(args) == 0 {
		return ""
	}
	switch ws[0].Text {
	case "cp", "mv", "install", "ln", "rsync":
		if !anyArg(ws, func(a string) bool { return a == "-t" || strings.HasPrefix(a, "--target-directory") }) {
			args = args[len(args)-1:]
		}
	case "dd":
		args = nil
		for _, w := range ws[1:] {
			if strings.HasPrefix(w.Text, "of=") {
				args = append(args, strings.TrimPrefix(w.Text, "of="))
			}
		}
	}
	for _, a := range args {
		if t := cleanAbs(a, cwd); underAny(t, systemDirs) {
			return "write under " + topDir(t)
		}
	}
	return ""
}
