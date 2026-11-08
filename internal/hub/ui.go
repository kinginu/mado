package hub

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"time"
)

// The SPA (DESIGN §17.5, §17.10). The shell is served for the client-side routes;
// the modules, the stylesheet and the vendored libraries live under /ui/<version>/,
// where version is a hash of every embedded file. The version sits in the path, not
// in ?v=, so that the relative imports between modules carry it too.

//go:embed ui
var uiEmbed embed.FS

var (
	uiFiles   fs.FS
	uiVersion string
	uiShell   []byte
)

func init() {
	var err error
	uiFiles, err = fs.Sub(uiEmbed, "ui")
	if err != nil {
		panic(err)
	}
	var names []string
	_ = fs.WalkDir(uiFiles, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return err
	})
	sort.Strings(names)
	sum := sha256.New()
	for _, n := range names {
		b, _ := fs.ReadFile(uiFiles, n)
		sum.Write([]byte(n + "\x00"))
		sum.Write(b)
	}
	uiVersion = hex.EncodeToString(sum.Sum(nil))[:12]
	shell, err := fs.ReadFile(uiFiles, "index.html")
	if err != nil {
		panic(err)
	}
	uiShell = bytes.ReplaceAll(shell, []byte("{{VERSION}}"), []byte(uiVersion))
}

// shellRoutes are the SPA's pages (ui/app.js routes the same list).
var shellRoutes = []string{"GET /{$}", "GET /s/{id}", "GET /new", "GET /usage", "GET /settings", "GET /admin/users",
	"GET /profiles", "GET /history", "GET /history/{id}", "GET /audit", "GET /admin/enroll", "GET /admin/machines", "GET /inbox"}

func (h *Hub) uiRoutes(mux *http.ServeMux) {
	for _, p := range shellRoutes {
		mux.HandleFunc(p, h.requireUser(h.handleShell))
	}
	mux.HandleFunc("GET /ui/{ver}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		p := r.PathValue("path")
		b, err := fs.ReadFile(uiFiles, p) // a directory or a missing file is an error
		if p == "index.html" || err != nil {
			http.NotFound(w, r)
			return
		}
		if r.PathValue("ver") == uiVersion {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// A shell from before a deploy: serve the current file, uncached.
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.ServeContent(w, r, path.Base(p), time.Time{}, bytes.NewReader(b))
	})
}

func (h *Hub) handleShell(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(uiShell)
}
