package render

import (
	"bytes"
	"html/template"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// md renders user and assistant text for the hub. The renderer stays safe: goldmark's
// unsafe option is off (dangerous link URLs are dropped), and raw HTML is written out
// escaped instead of passed through. Tables get a scroll wrapper, links open in a new tab,
// file paths become viewer links (paths.go).
var md = goldmark.New(
	// Cell alignment as an align attribute: a style attribute would break the hub's CSP.
	goldmark.WithExtensions(extension.NewTable(extension.WithTableCellAlignMethod(extension.TableCellAlignAttribute)), extension.Strikethrough, extension.TaskList, extension.Linkify),
	goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(mdTransformer{}, 100))),
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(mdRenderer{}, 100))),
)

// Markdown renders s (CommonMark + GFM tables, strikethrough, task lists, autolinks) to
// HTML that is safe to embed in a page. On a renderer error the text is shown escaped.
func Markdown(s string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(s), &buf); err != nil {
		return template.HTML("<p>" + template.HTMLEscapeString(s) + "</p>")
	}
	return template.HTML(strings.TrimRight(buf.String(), "\n"))
}

// kindTableWrap is a block node around each table, rendered as a scroll container.
var kindTableWrap = ast.NewNodeKind("TableWrap")

type tableWrap struct{ ast.BaseBlock }

func (n *tableWrap) Kind() ast.NodeKind            { return kindTableWrap }
func (n *tableWrap) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

type mdTransformer struct{}

func (mdTransformer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	linkPaths(doc, reader)
	var tables []ast.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case ast.KindLink, ast.KindAutoLink:
			n.SetAttributeString("rel", []byte("noopener noreferrer"))
			n.SetAttributeString("target", []byte("_blank"))
		case east.KindTable:
			tables = append(tables, n)
		}
		return ast.WalkContinue, nil
	})
	for _, t := range tables {
		w := &tableWrap{}
		t.Parent().ReplaceChild(t.Parent(), t, w)
		w.AppendChild(w, t)
	}
}

type mdRenderer struct{}

func (mdRenderer) RegisterFuncs(r renderer.NodeRendererFuncRegisterer) {
	r.Register(kindFilePath, renderFilePath)
	r.Register(ast.KindCodeSpan, renderCodeSpan)
	r.Register(kindTableWrap, func(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			_, _ = w.WriteString(`<div class="table-wrap">`)
		} else {
			_, _ = w.WriteString("</div>\n")
		}
		return ast.WalkContinue, nil
	})
	r.Register(ast.KindRawHTML, func(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			segs := node.(*ast.RawHTML).Segments
			for i := range segs.Len() {
				seg := segs.At(i)
				_, _ = w.WriteString(template.HTMLEscapeString(string(seg.Value(source))))
			}
		}
		return ast.WalkSkipChildren, nil
	})
	r.Register(ast.KindHTMLBlock, func(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
		n := node.(*ast.HTMLBlock)
		if entering {
			_, _ = w.WriteString(`<pre class="html-src">`)
			for i := range n.Lines().Len() {
				line := n.Lines().At(i)
				_, _ = w.WriteString(template.HTMLEscapeString(string(line.Value(source))))
			}
			if n.HasClosure() {
				_, _ = w.WriteString(template.HTMLEscapeString(string(n.ClosureLine.Value(source))))
			}
			_, _ = w.WriteString("</pre>\n")
		}
		return ast.WalkContinue, nil
	})
}
