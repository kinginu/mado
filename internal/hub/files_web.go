package hub

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kinginu/mado/internal/files"
	"github.com/kinginu/mado/internal/proto"
)

func fileJSON(f *FileRow) map[string]any {
	return map[string]any{"id": f.ID, "name": f.Name, "mime": f.Mime, "size": f.Size, "created_at": f.CreatedAt,
		"origin": f.Origin, "url": "/files/" + f.ID}
}

// cleanFileName keeps the base name, printable, at most 200 bytes.
func cleanFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	for len(name) > 200 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "." || name == "/" {
		return ""
	}
	return name
}

// apiUpload is POST /api/files: multipart with one `file` and the `session_id` of
// one of the user's chat sessions (DESIGN §17.4).
func (h *Hub) apiUpload(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	if !h.cfg.Chat.Enabled {
		apiError(w, http.StatusForbidden, "chat is disabled on this hub")
		return
	}
	limit := int64(h.cfg.Chat.MaxFileMB) << 20
	r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		apiError(w, http.StatusBadRequest, "expected multipart/form-data")
		return
	}
	var sessionID, name string
	var data []byte
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("files are limited to %d MB", h.cfg.Chat.MaxFileMB))
			return
		}
		switch part.FormName() {
		case "session_id":
			b, _ := io.ReadAll(io.LimitReader(part, 64))
			sessionID = strings.TrimSpace(string(b))
		case "file":
			if data != nil {
				apiError(w, http.StatusBadRequest, "one file per upload")
				return
			}
			name = cleanFileName(part.FileName())
			data, err = io.ReadAll(io.LimitReader(part, limit+1))
			if err != nil {
				apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("files are limited to %d MB", h.cfg.Chat.MaxFileMB))
				return
			}
			if int64(len(data)) > limit {
				apiError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("files are limited to %d MB", h.cfg.Chat.MaxFileMB))
				return
			}
		}
		part.Close()
	}
	if data == nil || name == "" {
		apiError(w, http.StatusBadRequest, "no file")
		return
	}
	s, err := h.store.Session(sessionID)
	if !proto.ValidSessionID(sessionID) || err != nil || s.OwnerSub != ws.User.Sub {
		apiError(w, http.StatusNotFound, "no such session")
		return
	}
	if !s.IsChat() {
		apiError(w, http.StatusBadRequest, "files go to chat sessions only")
		return
	}
	mimeType, _, err := files.Classify(name, data) // the client's Content-Type is ignored
	if err != nil {
		apiError(w, http.StatusUnsupportedMediaType, userMessage(err, err.Error()))
		return
	}
	if used := h.store.UserFileBytes(ws.User.Sub); used+int64(len(data)) > int64(h.cfg.Chat.UserQuotaMB)<<20 {
		apiError(w, http.StatusInsufficientStorage, fmt.Sprintf("your files would exceed the %d MB quota; delete some sessions first", h.cfg.Chat.UserQuotaMB))
		return
	}
	sha, err := h.blobs.Put(data)
	if err != nil {
		h.log.Error("store upload", "err", err)
		apiError(w, http.StatusInternalServerError, "could not store the file")
		return
	}
	f := FileRow{ID: proto.NewID(), OwnerSub: ws.User.Sub, SessionID: s.ID, Name: name, Mime: mimeType, Size: int64(len(data)),
		SHA256: sha, Origin: "upload"}
	if err := h.store.InsertFile(f); err != nil {
		apiError(w, http.StatusInternalServerError, "could not store the file")
		return
	}
	h.store.Audit(ws.User.Sub, "file_upload", s.ID+" "+f.ID+" size="+strconv.FormatInt(f.Size, 10))
	stored, _ := h.store.File(f.ID)
	writeJSON(w, http.StatusCreated, fileJSON(stored))
}

// apiDeleteFile removes a file no stored message refers to yet.
func (h *Hub) apiDeleteFile(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	f, err := h.store.File(r.PathValue("id"))
	if err != nil || f.OwnerSub != ws.User.Sub {
		apiError(w, http.StatusNotFound, "no such file")
		return
	}
	if h.store.FileReferenced(f) {
		apiError(w, http.StatusConflict, "the file is part of a sent message; delete the session instead")
		return
	}
	orphan, err := h.store.DeleteFile(f)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if orphan {
		_ = h.blobs.Remove(f.SHA256)
	}
	h.store.Audit(ws.User.Sub, "file_delete", f.SessionID+" "+f.ID)
	w.WriteHeader(http.StatusNoContent)
}

// handleFile serves a file to its owner and to admins. Images and PDFs are inline,
// everything else is an attachment; nosniff and the stored, sniffed type keep the
// browser from reinterpreting it.
func (h *Hub) handleFile(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	f, err := h.store.File(r.PathValue("id"))
	if err != nil || (f.OwnerSub != ws.User.Sub && !ws.User.IsAdmin) {
		http.NotFound(w, r)
		return
	}
	data, err := h.blobs.Read(f.SHA256)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	disp := "attachment"
	if c := files.ClassOf(f.Mime); c == files.ClassImage || c == files.ClassPDF {
		disp = "inline"
	}
	hd := w.Header()
	hd.Set("Content-Type", f.Mime)
	hd.Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": f.Name}))
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cache-Control", "private, max-age=86400")
	hd.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
