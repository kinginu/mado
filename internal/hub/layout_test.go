package hub

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
)

// A smoke check of the phone layout without a browser (DESIGN §7 "Mobile first",
// §17.5): the SPA shell, the login page and the stylesheet both use. It does not
// replace looking at the pages (DESIGN §17.10, §17.11 have the screenshot runs).
func TestPhoneLayoutSmoke(t *testing.T) {
	x := newM2(t)
	sid := x.session(t, "subscription", proto.StateIdle, "")
	x.fw.send(&proto.Event{SessionID: sid, Seq: 1, Raw: []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`)})
	time.Sleep(50 * time.Millisecond)
	pages := map[string]string{
		"shell": body(t, x.c, x.th.srv.URL+"/s/"+sid),
		"login": body(t, browser(), x.th.srv.URL+"/login"),
	}
	reViewport := regexp.MustCompile(`<meta name="viewport" content="width=device-width, initial-scale=1[^"]*">`)
	reWidthAttr := regexp.MustCompile(`(?i)\swidth="(\d+)"`)
	for name, p := range pages {
		if !reViewport.MatchString(p) {
			t.Errorf("%s: no viewport meta", name)
		}
		if strings.Contains(p, " style=") {
			t.Errorf("%s: inline style (CSP forbids it, and it escapes this check)", name)
		}
		for _, m := range reWidthAttr.FindAllStringSubmatch(p, -1) {
			if n, _ := strconv.Atoi(m[1]); n > 400 {
				t.Errorf("%s: width=%s attribute", name, m[1])
			}
		}
		if !strings.Contains(p, `href="/ui/`+uiVersion+`/app.css"`) {
			t.Errorf("%s: no stylesheet", name)
		}
	}

	reFixed := regexp.MustCompile(`(?:^|[;\s])(width|min-width|flex-basis)\s*:\s*(\d+(?:\.\d+)?)px`)
	for _, sheet := range []string{"/ui/" + uiVersion + "/app.css"} {
		rules := cssRules(body(t, x.c, x.th.srv.URL+sheet))
		for sel, decl := range rules {
			for _, m := range reFixed.FindAllStringSubmatch(decl, -1) {
				if n, _ := strconv.ParseFloat(m[2], 64); n > 400 {
					t.Errorf("%s: %s { %s: %spx } is wider than a phone", sheet, sel, m[1], m[2])
				}
			}
		}
		if !strings.Contains(rules["pre"], "pre-wrap") || !strings.Contains(rules["body"], "overflow-x: hidden") {
			t.Errorf("%s: long lines may scroll sideways: pre %q body %q", sheet, rules["pre"], rules["body"])
		}
		// Wide tables and code scroll inside their box, never the page.
		for _, sel := range []string{".table-wrap", "pre", ".md pre"} {
			if !strings.Contains(rules[sel], "overflow-x: auto") {
				t.Errorf("%s: %s does not scroll sideways inside its box: %q", sheet, sel, rules[sel])
			}
		}
		if !strings.Contains(rules[".table-wrap"], "max-width: 100%") {
			t.Errorf("%s: .table-wrap is not capped at the column width: %q", sheet, rules[".table-wrap"])
		}
	}
	css := body(t, x.c, x.th.srv.URL+"/ui/"+uiVersion+"/app.css")
	for _, want := range []string{"@media (max-width: 900px)", "@media (prefers-color-scheme: dark)", ".app.drawer-open .sidebar", ".app.sheet-open .panel"} {
		if !strings.Contains(css, want) {
			t.Errorf("ui/app.css lacks %q", want)
		}
	}
}

// cssRules maps each selector (of top-level and @media rules) to its declarations.
// Good enough for our own stylesheet; not a CSS parser.
func cssRules(css string) map[string]string {
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`).FindAllStringSubmatch(css, -1) {
		decl := strings.TrimSpace(m[2])
		for _, sel := range strings.Split(m[1], ",") {
			sel = strings.TrimSpace(sel)
			if i := strings.LastIndex(sel, "\n"); i >= 0 {
				sel = strings.TrimSpace(sel[i:])
			}
			out[sel] += decl + ";"
		}
	}
	return out
}

func TestCSSRulesHelper(t *testing.T) {
	r := cssRules("/* c */ .a, .b { width: 500px; }\n@media (x) { .c { max-width: 900px } }\n.composer{position:sticky;bottom:0}")
	if r[".a"] != "width: 500px;;" || r[".b"] != r[".a"] || !strings.Contains(r[".c"], "max-width") || !strings.Contains(r[".composer"], "sticky") {
		t.Fatalf("%q", r)
	}
}
