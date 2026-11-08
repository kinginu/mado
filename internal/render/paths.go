package render

import (
	"html/template"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// File paths in transcript text (DESIGN §27.1). A path-like token in a text node or an
// inline code span becomes <a class="fpath" href="#" data-path="…">; the SPA opens it
// in the file viewer. Links, autolinks, images and fenced or indented code blocks are
// left alone (code blocks have no text nodes, their lines are written as is).

// A path-like token, matched after the surrounding punctuation is stripped:
//
//	prefixed: starts with "/", "~/", "./" or "../", then path characters
//	relative: segment(/segment)+ whose last segment ends in .ext, ext 1 to 8 letters
//	          or digits starting with a letter
//
// Path characters are letters, digits and ._+@%~- (no spaces, no ":" so no URL scheme
// and no Windows drive). "//" anywhere is refused (protocol-relative URLs).
var (
	prefixedPath = regexp.MustCompile(`^(?:/|~/|\./|\.\./)[\p{L}\p{N}._+@%~/-]+$`)
	relativePath = regexp.MustCompile(`^[\p{L}\p{N}._+@%-]+(?:/[\p{L}\p{N}._+@%-]+)*/[\p{L}\p{N}._+@%-]*\.[A-Za-z][A-Za-z0-9]{0,7}$`)
	lineSuffix   = regexp.MustCompile(`:\d+(?:[:-]\d+)?$`)
)

const (
	pathLeadTrim  = "([{<\"'`"
	pathTrailTrim = ".,;:)]}>\"'`"
)

// PathLike reports whether s (already stripped of surrounding punctuation) is a path
// the transcript links.
func PathLike(s string) bool {
	if len(s) < 2 || len(s) > 4096 || !strings.Contains(s, "/") || strings.Contains(s, "//") {
		return false
	}
	return prefixedPath.MatchString(s) || relativePath.MatchString(s)
}

// pathDelim splits tokens: white space, and CJK text (U+3000 and above), which is
// written next to a path without a space.
func pathDelim(r rune) bool { return unicode.IsSpace(r) || r >= 0x3000 }

// FindPaths returns the byte ranges [start, end) of the linked paths in s.
func FindPaths(s string) [][2]int {
	var out [][2]int
	i := 0
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if pathDelim(r) {
			i += n
			continue
		}
		j := i
		for j < len(s) {
			r, n := utf8.DecodeRuneInString(s[j:])
			if pathDelim(r) {
				break
			}
			j += n
		}
		if a, b, ok := pathIn(s[i:j]); ok {
			out = append(out, [2]int{i + a, i + b})
		}
		i = j
	}
	return out
}

// pathIn strips the punctuation around one token (and a :line or :line:col suffix) and
// reports the range of the path inside it.
func pathIn(tok string) (int, int, bool) {
	a, b := 0, len(tok)
	for a < b && strings.IndexByte(pathLeadTrim, tok[a]) >= 0 {
		a++
	}
	trail := func() {
		for b > a && strings.IndexByte(pathTrailTrim, tok[b-1]) >= 0 {
			b--
		}
	}
	trail()
	if loc := lineSuffix.FindStringIndex(tok[a:b]); loc != nil {
		b = a + loc[0]
		trail()
	}
	if b <= a || !PathLike(tok[a:b]) {
		return 0, 0, false
	}
	return a, b, true
}

// kindFilePath is the inline node around a linked path; Path is its text.
var kindFilePath = ast.NewNodeKind("FilePath")

type filePath struct {
	ast.BaseInline
	Path string
}

func (n *filePath) Kind() ast.NodeKind            { return kindFilePath }
func (n *filePath) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

func fpathOpen(p string) string {
	return `<a class="fpath" href="#" data-path="` + template.HTMLEscapeString(p) + `">`
}

// linkPaths wraps the paths in the document's text nodes.
func linkPaths(doc *ast.Document, reader text.Reader) {
	source := reader.Source()
	var texts []*ast.Text
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case ast.KindLink, ast.KindAutoLink, ast.KindImage, ast.KindCodeBlock, ast.KindFencedCodeBlock, ast.KindHTMLBlock, ast.KindRawHTML:
			return ast.WalkSkipChildren, nil
		case ast.KindText:
			texts = append(texts, n.(*ast.Text))
		}
		return ast.WalkContinue, nil
	})
	for _, t := range texts {
		if t.Parent() == nil {
			continue // merged into the text before it
		}
		// Unmatched delimiters (~, _, *) end up as separate text nodes; join the
		// adjacent ones so a path such as ~/a_b/c.md is one token.
		for !t.SoftLineBreak() && !t.HardLineBreak() {
			next, ok := t.NextSibling().(*ast.Text)
			if !ok || t.Segment.Len() == 0 || !t.Merge(next, source) {
				break
			}
			t.Parent().RemoveChild(t.Parent(), next)
		}
		seg := t.Segment
		if seg.Padding != 0 {
			continue
		}
		v := string(seg.Value(source))
		hits := FindPaths(v)
		if len(hits) == 0 {
			continue
		}
		parent, at := t.Parent(), 0
		mk := func(s, e int) *ast.Text {
			x := ast.NewTextSegment(text.NewSegment(seg.Start+s, seg.Start+e))
			x.SetRaw(t.IsRaw())
			return x
		}
		for _, h := range hits {
			if h[0] > at {
				parent.InsertBefore(parent, t, mk(at, h[0]))
			}
			fp := &filePath{Path: v[h[0]:h[1]]}
			fp.AppendChild(fp, mk(h[0], h[1]))
			parent.InsertBefore(parent, t, fp)
			at = h[1]
		}
		// The original node keeps the rest and its line break flags.
		t.Segment = text.NewSegment(seg.Start+at, seg.Stop)
	}
}

// renderFilePath writes the link around its text child.
func renderFilePath(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		_, _ = w.WriteString(fpathOpen(node.(*filePath).Path))
	} else {
		_, _ = w.WriteString("</a>")
	}
	return ast.WalkContinue, nil
}

// renderCodeSpan is goldmark's code span renderer with linked paths inside: its
// children are text nodes and file path nodes.
func renderCodeSpan(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		_, _ = w.WriteString("</code>")
		return ast.WalkContinue, nil
	}
	_, _ = w.WriteString("<code>")
	write := func(t *ast.Text) {
		v := string(t.Segment.Value(source))
		if strings.HasSuffix(v, "\n") {
			v = v[:len(v)-1] + " "
		}
		_, _ = w.WriteString(template.HTMLEscapeString(v))
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch x := c.(type) {
		case *ast.Text:
			write(x)
		case *filePath:
			_, _ = w.WriteString(fpathOpen(x.Path))
			if t, ok := x.FirstChild().(*ast.Text); ok {
				write(t)
			}
			_, _ = w.WriteString("</a>")
		}
	}
	return ast.WalkSkipChildren, nil
}
