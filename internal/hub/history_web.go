package hub

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kinginu/mado/internal/proto"
)

// History API (DESIGN §14, §17.11). Machines have no owner yet, so imported
// transcripts are visible to admins only. TODO: a machine owner (or per-user
// enrollment) so each user sees the history of their own machines.

const historyPage = 300

type apiHistoryFile struct {
	ID            int64  `json:"id"`
	MachineID     string `json:"machine_id"`
	Machine       string `json:"machine"`
	SessionID     string `json:"session_id"`
	Title         string `json:"title"`
	CWD           string `json:"cwd"`
	ConfigDir     string `json:"config_dir"`
	DefaultConfig bool   `json:"default_config"`
	Modified      string `json:"modified"`
	Lines         int64  `json:"lines"`
	BadLines      int64  `json:"bad_lines"`
}

func historyFileJSON(f *HistoryFile) apiHistoryFile {
	return apiHistoryFile{ID: f.ID, MachineID: f.MachineID, Machine: f.MachineName, SessionID: f.SessionID, Title: f.Title,
		CWD: f.CWD, ConfigDir: f.ConfigDir, DefaultConfig: f.DefaultConfig, Modified: f.Modified(), Lines: f.Lines, BadLines: f.BadLines}
}

type apiHistoryGroup struct {
	MachineID string           `json:"machine_id"`
	Machine   string           `json:"machine"`
	Online    bool             `json:"online"`
	Files     []apiHistoryFile `json:"files"`
}

func (h *Hub) apiHistory(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) > 200 {
		apiError(w, http.StatusBadRequest, "search text too long")
		return
	}
	files, err := h.store.HistoryFiles(q, 2000)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	groups := []*apiHistoryGroup{}
	for _, f := range files {
		if len(groups) == 0 || groups[len(groups)-1].MachineID != f.MachineID {
			groups = append(groups, &apiHistoryGroup{MachineID: f.MachineID, Machine: f.MachineName, Online: h.online(f.MachineID)})
		}
		g := groups[len(groups)-1]
		g.Files = append(g.Files, historyFileJSON(f))
	}
	nf, nl, nb, bad := h.store.HistoryTotals()
	type mv struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Online bool   `json:"online"`
	}
	machines := []mv{}
	for _, m := range h.machineViews() {
		machines = append(machines, mv{m.ID, m.Name, m.Online})
	}
	writeJSON(w, http.StatusOK, map[string]any{"q": q, "groups": groups, "count": len(files), "machines": machines,
		"totals": map[string]int64{"files": nf, "lines": nl, "bytes": nb, "bad": bad}})
}

func (h *Hub) apiHistoryFileOf(w http.ResponseWriter, r *http.Request) *HistoryFile {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil && id > 0 {
		if f, err := h.store.HistoryFile(id); err == nil {
			return f
		}
	}
	apiError(w, http.StatusNotFound, "no such transcript")
	return nil
}

// apiHistoryFile pages a transcript as blocks (the session view's JSON shape, seq =
// byte offset of the line): ?after=<offset> continues, ?tail=1 gives the last page.
func (h *Hub) apiHistoryFile(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	f := h.apiHistoryFileOf(w, r)
	if f == nil {
		return
	}
	after := int64(-1)
	if a, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64); err == nil && a >= 0 {
		after = a
	}
	if r.URL.Query().Get("tail") == "1" {
		after = h.store.HistoryTailStart(f.ID, historyPage)
	}
	lines, err := h.store.HistoryLines(f.ID, after, historyPage)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	blocks := []apiBlock{}
	last := after
	for _, l := range lines {
		// A crash can leave a run of NUL bytes before an entry (seen in real
		// transcripts after power loss); the stored line keeps them.
		blocks = append(blocks, blocksJSON(l.Seq, bytes.TrimLeft(l.Raw, "\x00"))...)
		last = l.Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": historyFileJSON(f), "online": h.online(f.MachineID),
		"blocks": blocks, "first": after < 0, "last": last, "more": len(lines) == historyPage})
}

// apiHistoryResume starts a mado session that resumes the transcript's claude
// session on its machine. Only for transcripts of the user's own ~/.claude, which is
// what the subscription profile uses (a profile's CLAUDE_CONFIG_DIR has no login).
func (h *Hub) apiHistoryResume(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	f := h.apiHistoryFileOf(w, r)
	if f == nil {
		return
	}
	if !f.DefaultConfig {
		apiError(w, http.StatusBadRequest, "only transcripts from ~/.claude can be resumed (subscription profile)")
		return
	}
	title := f.Title
	if title == "" {
		title = f.SessionID[:8]
	}
	if r := []rune("resume: " + title); len(r) > 120 {
		title = string(r[:119]) + "…"
	} else {
		title = string(r)
	}
	id, err := h.newSession(r.Context(), ws, newSessionReq{MachineID: f.MachineID, Project: f.CWD, Profile: proto.KindSubscription,
		Mode: "default", Title: title, ResumeClaudeSessionID: f.SessionID})
	if err != nil {
		apiError(w, errStatus(err, http.StatusBadRequest), userMessage(err, "could not start"))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (h *Hub) apiHistoryRescan(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	var q struct {
		Machine string `json:"machine"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	wc := h.worker(q.Machine)
	if wc == nil {
		apiError(w, http.StatusServiceUnavailable, "machine is offline")
		return
	}
	if err := wc.send(r.Context(), &proto.HistoryRescan{}); err != nil {
		apiError(w, http.StatusServiceUnavailable, "could not reach the machine")
		return
	}
	h.store.Audit(ws.User.Sub, "history_rescan", q.Machine)
	writeJSON(w, http.StatusOK, map[string]any{})
}
