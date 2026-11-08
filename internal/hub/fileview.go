package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/render"
)

// The file viewer (DESIGN §27): a path in a code session's transcript opens in the
// right panel. The hub forwards the path with the session's cwd to the session's
// machine (fs_read) and renders Markdown with the transcript's renderer; the worker
// decides which files may be read.

const (
	fileViewWait     = 20 * time.Second
	fileDownloadWait = 60 * time.Second // 16 MiB over the worker's link
)

func (h *Hub) fileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sessions/{id}/file", h.requireAPI(h.apiFileView))
	mux.HandleFunc("GET /api/sessions/{id}/file/download", h.requireAPI(h.apiFileDownload))
}

// apiFile is the view's JSON. Kind is markdown (HTML set), text (Text set) or binary.
type apiFile struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Modified string `json:"modified,omitempty"`
	Mime     string `json:"mime,omitempty"`
	Kind     string `json:"kind"`
	HTML     string `json:"html,omitempty"`
	Text     string `json:"text,omitempty"`
}

// fileKind: markdown for .md/.markdown text, text for other text, binary otherwise.
func fileKind(f *proto.FSFile) string {
	switch {
	case f.Binary:
		return "binary"
	case strings.EqualFold(path.Ext(f.Path), ".md") || strings.EqualFold(path.Ext(f.Path), ".markdown"):
		return "markdown"
	}
	return "text"
}

// readSessionFile checks the session and the request, asks the worker and writes the
// error response itself; nil means it did.
func (h *Hub) readSessionFile(w http.ResponseWriter, r *http.Request, ws *WebSession, download bool) (*Session, *proto.FSFile) {
	s := h.apiOwnSession(w, r, ws)
	if s == nil {
		return nil, nil
	}
	if s.IsChat() || s.MachineID == "" {
		apiError(w, http.StatusBadRequest, "files can be opened in code sessions only")
		return nil, nil
	}
	p := r.URL.Query().Get("path")
	if p == "" || len(p) > 4096 || strings.ContainsFunc(p, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
		apiError(w, http.StatusBadRequest, "path is missing or invalid")
		return nil, nil
	}
	h.mu.Lock()
	wc := h.workers[s.MachineID]
	canRead := wc != nil && slices.Contains(wc.caps, proto.CapFSRead)
	h.mu.Unlock()
	switch {
	case wc == nil:
		apiError(w, http.StatusServiceUnavailable, "the machine is offline")
		return nil, nil
	case !canRead:
		apiError(w, http.StatusServiceUnavailable, "the worker on this machine is too old to show files; update mado there")
		return nil, nil
	}
	maxBytes, wait := int64(proto.FSViewMax), fileViewWait
	if download {
		maxBytes, wait = proto.FSDownloadMax, fileDownloadWait
	}
	id := proto.NewID()
	ctx, cancel := context.WithTimeout(r.Context(), wait+time.Second)
	defer cancel()
	res, err := h.fsRequest(ctx, s.MachineID, id, &proto.FSRead{RequestID: id, SessionID: s.ID, Path: p, CWD: s.CWD,
		MaxBytes: maxBytes, Download: download}, wait)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, err.Error())
		return nil, nil
	}
	if !res.OK {
		switch res.Code {
		case proto.FSCodeNotFound:
			apiError(w, http.StatusNotFound, "no such file")
		case proto.FSCodeForbidden:
			msg := "outside the allowed directories"
			if res.Error != "" {
				msg = res.Error
			}
			apiError(w, http.StatusForbidden, msg)
		case proto.FSCodeTooLarge:
			var f proto.FSFile
			_ = json.Unmarshal(res.Data, &f)
			msg := fmt.Sprintf("too large to view (%d KB); download instead", (f.Size+1023)/1024)
			if download {
				msg = fmt.Sprintf("too large to download (%d KB, at most %d MB)", (f.Size+1023)/1024, proto.FSDownloadMax>>20)
			}
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": msg, "size": f.Size})
		case proto.FSCodeNotFile:
			apiError(w, http.StatusBadRequest, res.Error)
		default:
			apiError(w, http.StatusBadGateway, "the machine could not read the file: "+res.Error)
		}
		return nil, nil
	}
	var f proto.FSFile
	if err := json.Unmarshal(res.Data, &f); err != nil {
		apiError(w, http.StatusBadGateway, "the machine sent a bad reply")
		return nil, nil
	}
	return s, &f
}

// apiFileView: GET /api/sessions/{id}/file?path=… (DESIGN §27.2).
func (h *Hub) apiFileView(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s, f := h.readSessionFile(w, r, ws, false)
	if f == nil {
		return
	}
	out := apiFile{Path: f.Path, Size: f.Size, Modified: f.Modified, Mime: f.Mime, Kind: fileKind(f)}
	switch out.Kind {
	case "markdown":
		out.HTML = string(render.Markdown(f.Text))
	case "text":
		out.Text = f.Text
	}
	h.store.Audit(ws.User.Sub, "file_view", s.ID+" "+f.Path)
	writeJSON(w, http.StatusOK, out)
}

// apiFileDownload: GET /api/sessions/{id}/file/download?path=… streams the bytes as an
// attachment.
func (h *Hub) apiFileDownload(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	s, f := h.readSessionFile(w, r, ws, true)
	if f == nil {
		return
	}
	h.store.Audit(ws.User.Sub, "file_download", s.ID+" "+f.Path)
	name := path.Base(f.Path)
	hd := w.Header()
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	hd.Set("Content-Length", strconv.Itoa(len(f.Bytes)))
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cache-Control", "no-store")
	_, _ = w.Write(f.Bytes)
}
