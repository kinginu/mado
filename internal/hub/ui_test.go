package hub

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// The SPA shell and its files (DESIGN §17.5, §17.10).
func TestUIShellAndFiles(t *testing.T) {
	x := newM2(t)
	sid := x.session(t, "subscription", "idle", "")
	anon := browser()
	for _, p := range []string{"/", "/s/" + sid, "/new", "/usage", "/settings", "/admin/users", "/profiles", "/history",
		"/history/1", "/audit", "/admin/enroll", "/admin/machines"} {
		resp, err := x.c.Get(x.th.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b := readAll(resp)
		if resp.StatusCode != 200 || !strings.Contains(b, `<script type="module" src="/ui/`+uiVersion+`/app.js">`) ||
			resp.Header.Get("Cache-Control") != "no-store" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("%s: %s %v", p, resp.Status, resp.Header)
		}
		if strings.Contains(b, "{{") || strings.Contains(b, "<script>") {
			t.Errorf("%s: unreplaced placeholder or inline script", p)
		}
		if resp, _ := anon.Get(x.th.srv.URL + p); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Errorf("anonymous %s: %s", p, resp.Status)
		}
	}
	get := func(p string) *http.Response {
		resp, err := anon.Get(x.th.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := get("/ui/" + uiVersion + "/app.js"); r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/javascript") ||
		!strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("app.js: %s %v", r.Status, r.Header)
	}
	if r := get("/ui/" + uiVersion + "/vendor/preact.module.js"); r.StatusCode != 200 {
		t.Errorf("vendor: %s", r.Status)
	}
	if r := get("/ui/0123456789ab/app.css"); r.StatusCode != 200 || r.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("stale version: %s %v", r.Status, r.Header)
	}
	for _, p := range []string{"/ui/" + uiVersion + "/index.html", "/ui/" + uiVersion + "/vendor", "/ui/" + uiVersion + "/vendor/", "/ui/" + uiVersion + "/nope.js"} {
		if r := get(p); r.StatusCode != 404 {
			t.Errorf("%s: %s", p, r.Status)
		}
	}
	// The htmx pages, the server-rendered admin pages and their form routes are gone.
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	for _, p := range []string{"/new", "/s/" + sid + "/send", "/s/" + sid + "/approve", "/s/" + sid + "/stop", "/s/" + sid + "/interrupt",
		"/profiles", "/chat-models", "/history/rescan", "/history/1/resume", "/admin/enroll"} {
		resp, _ := x.c.PostForm(x.th.srv.URL+p, map[string][]string{"csrf": {csrf}, "text": {"x"}})
		if resp.StatusCode < 400 {
			t.Errorf("POST %s still answers: %s", p, resp.Status)
		}
	}
	for _, p := range []string{"/static/htmx.min.js", "/static/app.js", "/static/app.css", "/static/log.js", "/profiles/new",
		"/profiles/subscription", "/chat-models/new"} {
		if r := get(p); r.StatusCode != 404 {
			t.Errorf("%s: %s", p, r.Status)
		}
	}
}

// Every import between the SPA modules is relative, so the files load without an
// import map (an inline script would need a CSP hash), and the version prefix of
// the shell carries over to every module. No module sets inline styles.
func TestUIModulesHaveNoBareImports(t *testing.T) {
	// import/export statements (also minified: `import{a}from"./x.js"`) and import("…").
	reImport := regexp.MustCompile(`(?:(?:^|[;\n])\s*(?:import|export)\b[^;"'\n]*?\bfrom\s*|\bimport\s*\(\s*|(?:^|[;\n])\s*import\s*)["']([^"']+)["']`)
	n := 0
	err := fs.WalkDir(uiFiles, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		b, _ := fs.ReadFile(uiFiles, p)
		src := string(b)
		for _, m := range reImport.FindAllStringSubmatch(src, -1) {
			n++
			if !strings.HasPrefix(m[1], "./") && !strings.HasPrefix(m[1], "../") {
				t.Errorf("%s imports %q", p, m[1])
			}
			if !strings.HasSuffix(m[1], ".js") {
				t.Errorf("%s imports %q without .js", p, m[1])
			}
		}
		if !strings.HasPrefix(p, "vendor/") && (strings.Contains(src, "style=") || strings.Contains(src, ".style.") || strings.Contains(src, "innerHTML =")) {
			t.Errorf("%s sets inline styles or innerHTML", p)
		}
		return nil
	})
	if err != nil || n < 10 {
		t.Fatalf("walk: %v, %d imports", err, n)
	}
}

func readAll(resp *http.Response) string {
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 32<<10)
	for {
		k, err := resp.Body.Read(buf)
		sb.Write(buf[:k])
		if err != nil {
			return sb.String()
		}
	}
}
