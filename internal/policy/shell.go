package policy

import (
	"strings"
)

// A small POSIX shell lexer, good enough to judge the commands a model runs through
// the Bash tool (DESIGN §18.2). It splits on ; && || | |& & and newlines, keeps quoted
// text together (`echo "a && b"` is one command), separates redirections, skips
// here-document bodies, and parses the inside of $(...), `...`, (...) and <(...) as
// commands of their own, so the deny rules see them. Anything it does not analyse
// fully sets Parsed.Opaque, and an opaque command is never allowed by rule.

// Word is one shell word with its quotes removed.
type Word struct {
	Text string
	// Var: the word contains a $ expansion outside single quotes (its value is unknown).
	Var bool
	// Tilde: the word starts with an unquoted ~ (a home directory path).
	Tilde bool
}

// Redir is one redirection: Op is >, >>, >|, <, <>, &>, &>>, >&, <&, <<< or <<
// (FD prefixes such as 2 are dropped); Target is the word after it.
type Redir struct {
	Op     string
	Target Word
}

// Command is one simple command between separators.
type Command struct {
	Words  []Word
	Redirs []Redir
}

// Texts returns the word texts.
func (c Command) Texts() []string {
	out := make([]string, len(c.Words))
	for i, w := range c.Words {
		out[i] = w.Text
	}
	return out
}

// Parsed is the result of Split.
type Parsed struct {
	Commands []Command
	// Opaque names the first construct that is not analysed (command substitution,
	// a subshell, a brace group, an unterminated quote, ...); "" when there is none.
	Opaque string
}

type frame struct {
	kind    byte // '(' for $( ( <( >(, '`' for backticks
	subst   bool // $(...) or `...`: its output becomes part of the outer word
	inDQ    bool // the frame was opened inside double quotes
	word    strings.Builder
	hasWord bool
	w       Word

	saved       Command // the outer command so far
	savedRedir  string
	savedQuoted bool
}

type lexer struct {
	rs  []rune
	i   int
	out Parsed

	cur        Command
	word       strings.Builder
	hasWord    bool
	w          Word
	pendRedir  string // the operator waiting for its target word
	quotedWord bool   // the current word had quotes or escapes
	heredocs   []heredoc
	stack      []*frame
	inDQ       bool
}

type heredoc struct {
	delim     string
	stripTabs bool
	quoted    bool
}

func (l *lexer) opaque(why string) {
	if l.out.Opaque == "" {
		l.out.Opaque = why
	}
}

// endWord finishes the current word: a redirection target or a command word.
func (l *lexer) endWord() {
	if !l.hasWord {
		return
	}
	l.w.Text = l.word.String()
	if l.pendRedir != "" {
		if l.pendRedir == "<<" || l.pendRedir == "<<-" {
			l.heredocs = append(l.heredocs, heredoc{delim: l.w.Text, stripTabs: l.pendRedir == "<<-", quoted: l.quotedWord})
			l.cur.Redirs = append(l.cur.Redirs, Redir{Op: "<<", Target: l.w})
		} else {
			l.cur.Redirs = append(l.cur.Redirs, Redir{Op: l.pendRedir, Target: l.w})
		}
		l.pendRedir = ""
	} else {
		switch {
		case len(l.cur.Words) == 0 && (l.w.Text == "{" || l.w.Text == "}") && !l.quotedWord:
			l.opaque("a brace group")
		case len(l.cur.Words) > 0 && l.w.Text == "}" && !l.quotedWord:
			l.opaque("a brace group")
		default:
			l.cur.Words = append(l.cur.Words, l.w)
		}
	}
	l.word.Reset()
	l.hasWord = false
	l.w = Word{}
	l.quotedWord = false
}

// endCommand finishes the current simple command.
func (l *lexer) endCommand() {
	l.endWord()
	if l.pendRedir != "" {
		l.opaque("a redirection without a target")
		l.pendRedir = ""
	}
	if len(l.cur.Words) > 0 || len(l.cur.Redirs) > 0 {
		l.out.Commands = append(l.out.Commands, l.cur)
	}
	l.cur = Command{}
}

func (l *lexer) peek(k int) rune {
	if l.i+k < len(l.rs) {
		return l.rs[l.i+k]
	}
	return 0
}

func (l *lexer) add(r rune) {
	l.word.WriteRune(r)
	l.hasWord = true
}

// push opens a nested command list ($(...), `...`, (...), <(...)).
func (l *lexer) push(kind byte, subst bool) {
	f := &frame{kind: kind, subst: subst, inDQ: l.inDQ, hasWord: l.hasWord, w: l.w}
	f.word.WriteString(l.word.String())
	// The text so far belongs to the outer word, which gets an unknown part.
	l.endCommandKeep(f)
	l.stack = append(l.stack, f)
	l.inDQ = false
}

// endCommandKeep ends the inner command list's predecessor without losing the outer
// word: the outer word is saved in the frame and restored by pop.
func (l *lexer) endCommandKeep(f *frame) {
	l.word.Reset()
	l.hasWord = false
	l.w = Word{}
	saved := l.cur
	saved.Words = append([]Word(nil), saved.Words...)
	saved.Redirs = append([]Redir(nil), saved.Redirs...)
	f.saved = saved
	f.savedRedir = l.pendRedir
	f.savedQuoted = l.quotedWord
	l.cur = Command{}
	l.pendRedir = ""
	l.quotedWord = false
}

func (l *lexer) pop() {
	l.endCommand()
	f := l.stack[len(l.stack)-1]
	l.stack = l.stack[:len(l.stack)-1]
	l.cur = f.saved
	l.pendRedir = f.savedRedir
	l.quotedWord = f.savedQuoted
	l.inDQ = f.inDQ
	l.word.Reset()
	l.word.WriteString(f.word.String())
	l.w, l.hasWord = f.w, f.hasWord
	if f.subst {
		l.w.Var = true // the substitution's output is unknown
		l.hasWord = true
	}
}

// Split parses a shell command line.
func Split(cmd string) Parsed {
	l := &lexer{rs: []rune(cmd)}
	l.run()
	if t := strings.TrimRight(cmd, " \t\r\n"); strings.HasSuffix(t, "&&") || strings.HasSuffix(t, "||") || strings.HasSuffix(t, "|") {
		l.opaque("a command line that ends in an operator")
	}
	return l.out
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\r' }

func (l *lexer) run() {
	for ; l.i < len(l.rs); l.i++ {
		c := l.rs[l.i]
		if l.inDQ {
			l.dq(c)
			continue
		}
		switch {
		case c == '\\':
			if l.peek(1) == '\n' {
				l.i++
				continue
			}
			if l.i+1 < len(l.rs) {
				l.i++
				l.add(l.rs[l.i])
				l.quotedWord = true
			}
		case c == '\'':
			j := l.i + 1
			for j < len(l.rs) && l.rs[j] != '\'' {
				j++
			}
			if j >= len(l.rs) {
				l.opaque("an unterminated quote")
			}
			l.word.WriteString(string(l.rs[l.i+1 : min(j, len(l.rs))]))
			l.hasWord, l.quotedWord = true, true
			l.i = j
		case c == '"':
			l.inDQ, l.hasWord, l.quotedWord = true, true, true
		case c == '`':
			if n := len(l.stack); n > 0 && l.stack[n-1].kind == '`' {
				l.pop()
			} else {
				l.opaque("command substitution")
				l.push('`', true)
			}
		case c == '$' && l.peek(1) == '(':
			l.opaque("command substitution")
			l.i++
			l.push('(', true)
		case c == '$':
			l.add(c)
			l.w.Var = true
		case (c == '<' || c == '>') && l.peek(1) == '(':
			l.opaque("process substitution")
			l.endWord()
			l.i++
			l.push('(', false)
		case c == '(':
			l.opaque("a subshell")
			l.endWord()
			l.push('(', false)
		case c == ')':
			if n := len(l.stack); n > 0 && l.stack[n-1].kind == '(' {
				l.pop()
			} else {
				l.opaque("an unbalanced parenthesis")
				l.endCommand()
			}
		case c == '#' && !l.hasWord:
			for l.i+1 < len(l.rs) && l.rs[l.i+1] != '\n' {
				l.i++
			}
		case c == '\n':
			l.endCommand()
			l.readHeredocs()
		case c == ';':
			l.endCommand()
			if l.peek(1) == ';' || l.peek(1) == '&' {
				l.i++
			}
		case c == '|':
			l.endCommand()
			if l.peek(1) == '|' || l.peek(1) == '&' {
				l.i++
			}
		case c == '&':
			switch l.peek(1) {
			case '&':
				l.endCommand()
				l.i++
			case '>':
				l.endWord()
				l.i++
				l.redir("&>")
			default:
				l.endCommand()
			}
		case c == '>' || c == '<':
			// A number right before the operator is a file descriptor, not a word.
			if l.hasWord && !l.quotedWord && !l.w.Var && isDigits(l.word.String()) {
				l.word.Reset()
				l.hasWord = false
			}
			l.endWord()
			op := string(c)
			switch {
			case c == '<' && l.peek(1) == '<' && l.peek(2) == '<':
				op = "<<<"
				l.i += 2
			case c == '<' && l.peek(1) == '<' && l.peek(2) == '-':
				op = "<<-"
				l.i += 2
			case c == '<' && l.peek(1) == '<':
				op = "<<"
				l.i++
			case l.peek(1) == '>' && c == '>':
				op = ">>"
				l.i++
			case l.peek(1) == '&' || l.peek(1) == '|' || (c == '<' && l.peek(1) == '>'):
				op += string(l.peek(1))
				l.i++
			}
			l.redir(op)
		case isSpace(c):
			l.endWord()
		case c == '~' && !l.hasWord:
			l.add(c)
			l.w.Tilde = true
		default:
			l.add(c)
		}
	}
	if l.inDQ {
		l.opaque("an unterminated quote")
	}
	if len(l.stack) > 0 {
		l.opaque("an unterminated substitution")
		for len(l.stack) > 0 {
			l.pop()
		}
	}
	l.endCommand()
	if len(l.heredocs) > 0 {
		l.opaque("an unterminated here-document")
	}
}

func (l *lexer) redir(op string) {
	if l.pendRedir != "" {
		l.opaque("a redirection without a target")
	}
	l.pendRedir = op
	if op == ">&" || op == "<&" {
		// >&2, >&-: a descriptor, not a file.
		j := l.i + 1
		for j < len(l.rs) && isSpace(l.rs[j]) {
			j++
		}
		k := j
		for k < len(l.rs) && (l.rs[k] >= '0' && l.rs[k] <= '9' || l.rs[k] == '-') {
			k++
		}
		if k > j && (k == len(l.rs) || !isWordRune(l.rs[k])) {
			l.cur.Redirs = append(l.cur.Redirs, Redir{Op: op, Target: Word{Text: "&" + string(l.rs[j:k])}})
			l.pendRedir = ""
			l.i = k - 1
		}
	}
}

func isWordRune(r rune) bool {
	return !isSpace(r) && !strings.ContainsRune("\n;&|<>()", r)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// dq handles one rune inside double quotes.
func (l *lexer) dq(c rune) {
	switch {
	case c == '"':
		l.inDQ = false
	case c == '\\' && strings.ContainsRune("\"\\$`\n", l.peek(1)):
		l.i++
		if l.rs[l.i] != '\n' {
			l.add(l.rs[l.i])
		}
	case c == '$' && l.peek(1) == '(':
		l.opaque("command substitution")
		l.i++
		l.push('(', true)
	case c == '`':
		l.opaque("command substitution")
		l.push('`', true)
	case c == '$':
		l.add(c)
		l.w.Var = true
	default:
		l.add(c)
	}
}

// readHeredocs skips the bodies of the here-documents started on the line just ended.
// A body with an unquoted delimiter may expand $(...), so it is opaque then.
func (l *lexer) readHeredocs() {
	for len(l.heredocs) > 0 {
		hd := l.heredocs[0]
		l.heredocs = l.heredocs[1:]
		for {
			start := l.i + 1
			if start > len(l.rs) {
				l.heredocs = append([]heredoc{hd}, l.heredocs...)
				return
			}
			end := start
			for end < len(l.rs) && l.rs[end] != '\n' {
				end++
			}
			line := string(l.rs[start:end])
			l.i = end
			check := line
			if hd.stripTabs {
				check = strings.TrimLeft(line, "\t")
			}
			if check == hd.delim {
				break
			}
			if !hd.quoted && (strings.Contains(line, "$(") || strings.Contains(line, "`")) {
				l.opaque("command substitution in a here-document")
			}
			if end >= len(l.rs) {
				l.heredocs = append([]heredoc{hd}, l.heredocs...)
				return
			}
		}
	}
}
