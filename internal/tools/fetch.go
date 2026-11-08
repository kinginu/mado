package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// Fetch limits (DESIGN §17.7).
const (
	FetchTimeout  = 10 * time.Second
	MaxFetchBytes = 2 << 20
)

// Fetcher is fetch_url: one guarded GET, HTML turned into readable text.
type Fetcher struct {
	Guard    Guard
	MaxChars int
}

// Fetch returns "Title: …\nURL: …\n\n<text>", truncated to MaxChars characters.
func (f *Fetcher) Fetch(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return "", Error("not an absolute URL")
	}
	u.Fragment = ""
	if err := f.Guard.CheckURL(u); err != nil {
		return "", Error(err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", Error("bad URL")
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,application/json;q=0.8,*/*;q=0.5")
	resp, err := f.Guard.Client(FetchTimeout).Do(req)
	if err != nil {
		if ctx.Err() == context.Canceled {
			return "", ctx.Err()
		}
		return "", Error("fetch failed: " + transportError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", Error(fmt.Sprintf("fetch failed: HTTP %d", resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchBytes+1))
	if err != nil && len(data) == 0 {
		return "", Error("fetch failed: could not read the page")
	}
	cut := len(data) > MaxFetchBytes
	if cut {
		data = data[:MaxFetchBytes]
	}
	final := resp.Request.URL
	ct := resp.Header.Get("Content-Type")
	mt, _, _ := mime.ParseMediaType(ct)
	if mt == "" {
		mt = http.DetectContentType(data)
		mt, _, _ = mime.ParseMediaType(mt)
	}
	var title, text string
	switch {
	case mt == "text/html" || mt == "application/xhtml+xml":
		title, text = htmlText(data, ct, final)
	case strings.HasPrefix(mt, "text/") || mt == "application/json" || mt == "application/xml" ||
		strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml"):
		text = decodeText(data, ct)
	default:
		return "", Error("fetch_url reads web pages and text only; this URL is " + mt)
	}
	out := "URL: " + final.String() + "\n"
	if title != "" {
		out = "Title: " + oneLine(title, 300) + "\n" + out
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = "(no readable text on this page)"
	}
	max := f.MaxChars
	if max <= 0 {
		max = DefaultFetchMaxChars
	}
	if n := utf8.RuneCountInString(text); n > max {
		r := []rune(text)
		text = string(r[:max]) + fmt.Sprintf("\n\n[truncated: %d of %d characters shown]", max, n)
	} else if cut {
		text += "\n\n[truncated: the page is larger than 2 MB]"
	}
	return out + "\n" + text, nil
}

func decodeText(data []byte, contentType string) string {
	r, err := charset.NewReader(bytes.NewReader(data), contentType)
	if err != nil {
		return strings.ToValidUTF8(string(data), "�")
	}
	b, _ := io.ReadAll(r)
	return strings.ToValidUTF8(string(b), "�")
}

// htmlText runs readability and renders its article as text; a page readability
// cannot parse falls back to the whole body.
func htmlText(data []byte, contentType string, page *url.URL) (title, text string) {
	utf := decodeText(data, contentType)
	art, err := readability.FromReader(strings.NewReader(utf), page)
	if err == nil && art.Node != nil {
		text = nodeText(art.Node)
		title = art.Title
	}
	if strings.TrimSpace(text) == "" {
		doc, err := html.Parse(strings.NewReader(utf))
		if err != nil {
			return title, ""
		}
		if title == "" {
			title = findTitle(doc)
		}
		text = nodeText(doc)
	}
	return title, text
}

func findTitle(n *html.Node) string {
	if n.Type == html.ElementNode && n.Data == "title" && n.FirstChild != nil {
		return n.FirstChild.Data
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if t := findTitle(c); t != "" {
			return t
		}
	}
	return ""
}

var skipTags = map[string]bool{"script": true, "style": true, "noscript": true, "template": true, "svg": true,
	"iframe": true, "head": true, "nav": true, "form": true, "button": true, "select": true}

var blockTags = map[string]bool{"p": true, "div": true, "section": true, "article": true, "main": true, "header": true,
	"footer": true, "aside": true, "ul": true, "ol": true, "li": true, "pre": true, "blockquote": true, "table": true,
	"tr": true, "br": true, "hr": true, "dl": true, "dt": true, "dd": true, "figure": true, "figcaption": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "address": true, "details": true, "summary": true}

var manyNewlines = regexp.MustCompile(`\n{3,}`)

// nodeText renders an HTML tree as plain text with paragraphs, headings ("## …")
// and list items ("- …") on their own lines.
func nodeText(root *html.Node) string {
	var b strings.Builder
	var walk func(n *html.Node, pre bool)
	walk = func(n *html.Node, pre bool) {
		switch n.Type {
		case html.TextNode:
			if pre {
				b.WriteString(n.Data)
				return
			}
			s := strings.Join(strings.Fields(n.Data), " ")
			if s == "" {
				if strings.TrimSpace(n.Data) == "" && n.Data != "" && b.Len() > 0 && !strings.HasSuffix(b.String(), " ") &&
					!strings.HasSuffix(b.String(), "\n") {
					b.WriteString(" ")
				}
				return
			}
			if startsSpace(n.Data) && b.Len() > 0 && !strings.HasSuffix(b.String(), " ") && !strings.HasSuffix(b.String(), "\n") {
				b.WriteString(" ")
			}
			b.WriteString(s)
			if endsSpace(n.Data) {
				b.WriteString(" ")
			}
			return
		case html.ElementNode:
			if skipTags[n.Data] {
				return
			}
		}
		block := n.Type == html.ElementNode && blockTags[n.Data]
		if block {
			b.WriteString("\n")
			switch n.Data {
			case "h1", "h2", "h3", "h4", "h5", "h6":
				b.WriteString("\n" + strings.Repeat("#", int(n.Data[1]-'0')) + " ")
			case "li":
				b.WriteString("- ")
			case "p", "pre", "blockquote", "table":
				b.WriteString("\n")
			}
		}
		if n.Type == html.ElementNode && n.Data == "pre" {
			pre = true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, pre)
		}
		if block {
			b.WriteString("\n")
		}
	}
	walk(root, false)
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(manyNewlines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

func startsSpace(s string) bool { return s != "" && strings.ContainsRune(" \t\n\r", rune(s[0])) }
func endsSpace(s string) bool   { return s != "" && strings.ContainsRune(" \t\n\r", rune(s[len(s)-1])) }

// HTMLText renders an HTML fragment (a feed item's description, a mail's HTML part)
// as plain text, with the same rules as fetch_url: paragraphs and line breaks kept,
// scripts and styles dropped, entities decoded. Plain text passes through.
func HTMLText(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return strings.TrimSpace(s)
	}
	return nodeText(doc)
}
