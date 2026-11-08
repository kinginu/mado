package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/inbox/push"
	"github.com/kinginu/mado/internal/render"
)

// The inbox API (DESIGN §23.8). Items, digests and tasks are the caller's own;
// accounts are listed to their owner and to admins, and only admins change them.

func (h *Hub) inboxRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/inbox/accounts", h.requireAPI(h.inboxOn(h.apiInboxAccounts)))
	mux.HandleFunc("POST /api/inbox/accounts", h.requireAdminAPI(h.inboxOn(h.apiInboxCreateAccount)))
	mux.HandleFunc("PATCH /api/inbox/accounts/{id}", h.requireAdminAPI(h.inboxOn(h.apiInboxPatchAccount)))
	mux.HandleFunc("DELETE /api/inbox/accounts/{id}", h.requireAdminAPI(h.inboxOn(h.apiInboxDeleteAccount)))
	mux.HandleFunc("POST /api/inbox/accounts/{id}/poll", h.requireAPI(h.inboxOn(h.apiInboxPoll)))
	mux.HandleFunc("GET /api/inbox/items", h.requireAPI(h.inboxOn(h.apiInboxItems)))
	mux.HandleFunc("GET /api/inbox/items/{id}", h.requireAPI(h.inboxOn(h.apiInboxItem)))
	mux.HandleFunc("PATCH /api/inbox/items/{id}", h.requireAPI(h.inboxOn(h.apiInboxPatchItem)))
	mux.HandleFunc("POST /api/inbox/items/state", h.requireAPI(h.inboxOn(h.apiInboxItemsState)))
	mux.HandleFunc("GET /api/inbox/digests", h.requireAPI(h.inboxOn(h.apiInboxDigests)))
	mux.HandleFunc("GET /api/inbox/digests/{id}", h.requireAPI(h.inboxOn(h.apiInboxDigest)))
	mux.HandleFunc("POST /api/inbox/digests", h.requireAPI(h.inboxOn(h.apiInboxStartDigest)))
	mux.HandleFunc("GET /api/inbox/tasks", h.requireAPI(h.inboxOn(h.apiInboxTasks)))
	mux.HandleFunc("POST /api/inbox/tasks", h.requireAPI(h.inboxOn(h.apiInboxCreateTask)))
	mux.HandleFunc("PATCH /api/inbox/tasks/{id}", h.requireAPI(h.inboxOn(h.apiInboxPatchTask)))
	// No cookie, no CSRF: a bearer token per account (DESIGN §23.4).
	mux.HandleFunc("POST /api/inbox/ingest/{account}", h.handleIngest)
}

func (h *Hub) inboxOn(next func(http.ResponseWriter, *http.Request, *WebSession)) func(http.ResponseWriter, *http.Request, *WebSession) {
	return func(w http.ResponseWriter, r *http.Request, ws *WebSession) {
		if h.inbox == nil {
			apiError(w, http.StatusNotFound, "the inbox is disabled on this hub")
			return
		}
		next(w, r, ws)
	}
}

// --- accounts

type apiInboxAccount struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	OwnerSub   string          `json:"owner_sub"`
	Owner      string          `json:"owner"` // name or e-mail for display
	Resolved   bool            `json:"owner_resolved"`
	Config     json.RawMessage `json:"config"`
	KeyRef     string          `json:"key_ref,omitempty"` // admins only
	Enabled    bool            `json:"enabled"`
	IntervalS  int             `json:"interval_s"`
	Source     string          `json:"source"`
	Editable   bool            `json:"editable"`
	Pollable   bool            `json:"pollable"`
	LastRun    string          `json:"last_run"`
	LastOK     string          `json:"last_ok"`
	LastError  string          `json:"last_error"`
	BackoffS   int             `json:"backoff_s"`
	CreatedAt  string          `json:"created_at"`
	NewItems   int             `json:"new_items"`
	TotalItems int             `json:"total_items"`
	IngestPath string          `json:"ingest_path,omitempty"`
}

func (h *Hub) accountView(a *inbox.Account, admin bool) apiInboxAccount {
	v := apiInboxAccount{ID: a.ID, Name: a.Name, Kind: a.Kind, OwnerSub: a.OwnerSub, Owner: a.OwnerSub, Config: a.Config,
		Enabled: a.Enabled, IntervalS: a.IntervalS, Source: a.Source, Editable: a.Source == inbox.SourceUI,
		LastRun: a.LastRun, LastOK: a.LastOK, LastError: a.LastError, BackoffS: a.BackoffS, CreatedAt: a.CreatedAt}
	if u, err := h.store.User(a.OwnerSub); err == nil {
		v.Resolved = true
		v.Owner = u.Email
		if u.Name != "" {
			v.Owner = u.Name
		}
	}
	if admin {
		v.KeyRef = a.KeyRef
	}
	if k, ok := inbox.KindByName(a.Kind); ok {
		_, err := k.New(*a, "", inbox.Env{})
		v.Pollable = !errors.Is(err, inbox.ErrPassive)
		if errors.Is(err, inbox.ErrPassive) {
			v.IngestPath = "/api/inbox/ingest/" + a.ID
		}
	}
	v.NewItems, v.TotalItems = h.inbox.store.ItemCounts(a.ID)
	return v
}

func (h *Hub) apiInboxAccounts(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	h.inbox.store.ResolveOwners()
	owner := ws.User.Sub
	if ws.User.IsAdmin && r.URL.Query().Get("mine") != "1" {
		owner = ""
	}
	as, err := h.inbox.store.Accounts(owner)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := []apiInboxAccount{}
	for _, a := range as {
		out = append(out, h.accountView(a, ws.User.IsAdmin))
	}
	resp := map[string]any{"accounts": out}
	if ws.User.IsAdmin {
		type kindView struct {
			Name     string          `json:"name"`
			Template json.RawMessage `json:"template"`
		}
		kinds := []kindView{}
		for _, n := range inbox.Kinds() {
			k, _ := inbox.KindByName(n)
			t := json.RawMessage(`{}`)
			if tp, ok := k.(inbox.Templater); ok {
				t = tp.Template()
			}
			kinds = append(kinds, kindView{n, t})
		}
		resp["kinds"], resp["keys"] = kinds, h.keyNames()
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveUser finds a user by sub or e-mail.
func (h *Hub) resolveUser(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if u, err := h.store.User(s); err == nil {
		return u.Sub, true
	}
	var sub string
	if h.store.db.QueryRow(`SELECT sub FROM users WHERE lower(email)=lower(?) ORDER BY created_at LIMIT 1`, s).Scan(&sub) == nil {
		return sub, true
	}
	return "", false
}

type accountBody struct {
	Name      *string          `json:"name"`
	Kind      *string          `json:"kind"`
	OwnerSub  *string          `json:"owner_sub"`
	Config    *json.RawMessage `json:"config"`
	KeyRef    *string          `json:"key_ref"`
	Enabled   *bool            `json:"enabled"`
	IntervalS *int             `json:"interval_s"`
}

func (h *Hub) apiInboxCreateAccount(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	var q accountBody
	if !decodeBody(w, r, &q) {
		return
	}
	a := &inbox.Account{Enabled: true, Source: inbox.SourceUI}
	if q.Name != nil {
		a.Name = *q.Name
	}
	if q.Kind != nil {
		a.Kind = *q.Kind
	}
	if q.Config != nil {
		a.Config = *q.Config
	}
	if q.KeyRef != nil {
		a.KeyRef = *q.KeyRef
	}
	if q.Enabled != nil {
		a.Enabled = *q.Enabled
	}
	if q.IntervalS != nil {
		a.IntervalS = *q.IntervalS
	}
	owner := ws.User.Sub
	if q.OwnerSub != nil && *q.OwnerSub != "" {
		sub, ok := h.resolveUser(*q.OwnerSub)
		if !ok {
			apiError(w, http.StatusBadRequest, "owner_sub: no such user (a sub or the e-mail of a user who has logged in)")
			return
		}
		owner = sub
	}
	a.OwnerSub = owner
	if err := inbox.Validate(a, h.secrets.Keys); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.inbox.store.CreateAccount(a); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.store.Audit(ws.User.Sub, "inbox_account_create", fmt.Sprintf("%s %s %s owner=%s", a.ID, a.Name, a.Kind, a.OwnerSub))
	h.inbox.sched.Sync()
	writeJSON(w, http.StatusCreated, h.accountView(a, true))
}

// adminAccount loads the account of the path for an admin edit.
func (h *Hub) pathAccount(w http.ResponseWriter, r *http.Request) *inbox.Account {
	a, err := h.inbox.store.Account(r.PathValue("id"))
	if err != nil {
		apiError(w, http.StatusNotFound, "no such account")
		return nil
	}
	return a
}

func (h *Hub) apiInboxPatchAccount(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	a := h.pathAccount(w, r)
	if a == nil {
		return
	}
	if a.Source != inbox.SourceUI {
		apiError(w, http.StatusBadRequest, "this account is defined in hub.toml; edit it there")
		return
	}
	var q accountBody
	if !decodeBody(w, r, &q) {
		return
	}
	if q.Kind != nil && *q.Kind != a.Kind {
		apiError(w, http.StatusBadRequest, "the kind of an account cannot change; create a new account")
		return
	}
	if q.OwnerSub != nil && *q.OwnerSub != "" && *q.OwnerSub != a.OwnerSub {
		if sub, ok := h.resolveUser(*q.OwnerSub); !ok || sub != a.OwnerSub {
			apiError(w, http.StatusBadRequest, "the owner of an account cannot change; create a new account")
			return
		}
	}
	was := a.Enabled
	if q.Name != nil {
		a.Name = *q.Name
	}
	if q.Config != nil {
		a.Config = *q.Config
	}
	if q.KeyRef != nil {
		a.KeyRef = *q.KeyRef
	}
	if q.Enabled != nil {
		a.Enabled = *q.Enabled
	}
	if q.IntervalS != nil {
		a.IntervalS = *q.IntervalS
	}
	if err := inbox.Validate(a, h.secrets.Keys); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.inbox.store.UpdateAccount(a); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	action := "inbox_account_update"
	if was && !a.Enabled {
		action = "inbox_account_disable"
	} else if !was && a.Enabled {
		action = "inbox_account_enable"
	}
	h.store.Audit(ws.User.Sub, action, fmt.Sprintf("%s %s %s", a.ID, a.Name, a.Kind))
	h.inbox.sched.Sync()
	writeJSON(w, http.StatusOK, h.accountView(a, true))
}

func (h *Hub) apiInboxDeleteAccount(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	a := h.pathAccount(w, r)
	if a == nil {
		return
	}
	if a.Source != inbox.SourceUI {
		apiError(w, http.StatusBadRequest, "this account is defined in hub.toml; remove it there")
		return
	}
	_, total := h.inbox.store.ItemCounts(a.ID)
	if err := h.inbox.store.DeleteAccount(a.ID); err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.store.Audit(ws.User.Sub, "inbox_account_delete", fmt.Sprintf("%s %s %s items=%d", a.ID, a.Name, a.Kind, total))
	h.inbox.sched.Sync()
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) apiInboxPoll(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	a := h.pathAccount(w, r)
	if a == nil {
		return
	}
	if a.OwnerSub != ws.User.Sub && !ws.User.IsAdmin {
		apiError(w, http.StatusNotFound, "no such account")
		return
	}
	n, err := h.inbox.sched.PollNow(r.Context(), a.ID)
	switch {
	case errors.Is(err, inbox.ErrBusy):
		apiError(w, http.StatusConflict, err.Error())
	case errors.Is(err, inbox.ErrPassive):
		apiError(w, http.StatusBadRequest, "a "+a.Kind+" account has nothing to poll")
	case err != nil:
		apiError(w, http.StatusBadGateway, "poll failed: "+inbox.ClipUTF8(err.Error(), 300))
	default:
		writeJSON(w, http.StatusOK, map[string]int{"items_added": n})
	}
}

// --- items

// listTextChars bounds an item's text in the list; GET /api/inbox/items/{id} has all.
const listTextChars = 2000

func (h *Hub) apiInboxItems(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	q := r.URL.Query()
	iq := inbox.ItemQuery{Owner: ws.User.Sub, Account: q.Get("account"), State: q.Get("state"), Kind: q.Get("kind"), Q: q.Get("q")}
	if iq.State == "" {
		iq.State = inbox.StateNew
	}
	if iq.State != "all" && !inbox.ValidItemState(iq.State) {
		apiError(w, http.StatusBadRequest, "state must be new, read, archived or all")
		return
	}
	iq.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	iq.Limit, _ = strconv.Atoi(q.Get("limit"))
	if iq.Limit <= 0 {
		iq.Limit = 50
	}
	iq.Limit = min(iq.Limit, 200)
	rows, err := h.inbox.store.Items(iq)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := []*inbox.ItemRow{}
	for _, it := range rows {
		if len(it.Text) > listTextChars {
			it.Text, it.Truncated = inbox.ClipUTF8(it.Text, listTextChars), true
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "more": len(rows) == iq.Limit})
}

func (h *Hub) pathItem(w http.ResponseWriter, r *http.Request, ws *WebSession) *inbox.ItemRow {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	it, err := h.inbox.store.Item(ws.User.Sub, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such item")
		return nil
	}
	return it
}

func (h *Hub) apiInboxItem(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	if it := h.pathItem(w, r, ws); it != nil {
		writeJSON(w, http.StatusOK, it)
	}
}

func (h *Hub) apiInboxPatchItem(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	it := h.pathItem(w, r, ws)
	if it == nil {
		return
	}
	var q struct {
		State string `json:"state"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	if _, err := h.inbox.store.SetItemState(ws.User.Sub, []int64{it.ID}, q.State); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	it.State = q.State
	writeJSON(w, http.StatusOK, it)
}

func (h *Hub) apiInboxItemsState(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	var q struct {
		IDs     []int64 `json:"ids"`
		State   string  `json:"state"`
		AllNew  bool    `json:"all_new"` // every new item (of account) becomes state "read"
		Account string  `json:"account"`
	}
	if !decodeBody(w, r, &q) {
		return
	}
	var n int
	var err error
	switch {
	case q.AllNew:
		n, err = h.inbox.store.MarkAllRead(ws.User.Sub, q.Account)
	case len(q.IDs) > 1000:
		err = errors.New("at most 1000 ids")
	default:
		n, err = h.inbox.store.SetItemState(ws.User.Sub, q.IDs, q.State)
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"changed": n})
}

// --- digests

type apiDigest struct {
	*inbox.Digest
	HTML     string   `json:"html,omitempty"`
	Accounts []string `json:"accounts"`
}

var itemRefRe = regexp.MustCompile(`\[(#\d+(?:\s*,\s*#\d+)*)\]`)
var itemNumRe = regexp.MustCompile(`#(\d+)`)

// digestHTML renders a digest's Markdown and turns the [#id] references into
// buttons that open the item (DESIGN §23.8).
func digestHTML(text string) string {
	out := string(render.Markdown(text))
	return itemRefRe.ReplaceAllStringFunc(out, func(m string) string {
		inner := itemNumRe.ReplaceAllString(m[1:len(m)-1], `<button type="button" class="item-ref" data-item="$1">#$1</button>`)
		return `<span class="item-refs">` + inner + `</span>`
	})
}

func (h *Hub) digestView(d *inbox.Digest, full bool) apiDigest {
	v := apiDigest{Digest: d, Accounts: []string{}}
	for _, id := range d.AccountIDs {
		if a, err := h.inbox.store.Account(id); err == nil {
			v.Accounts = append(v.Accounts, a.Name)
		}
	}
	if full && d.Text != "" {
		v.HTML = digestHTML(d.Text)
	}
	return v
}

func (h *Hub) apiInboxDigests(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	ds, err := h.inbox.store.Digests(ws.User.Sub, 50)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := []apiDigest{}
	for i, d := range ds {
		if i > 0 {
			d.Text = "" // the list carries the latest digest's text only
		}
		out = append(out, h.digestView(d, i == 0))
	}
	newItems, _ := h.inbox.store.Items(inbox.ItemQuery{Owner: ws.User.Sub, State: inbox.StateNew, Limit: 1})
	writeJSON(w, http.StatusOK, map[string]any{"digests": out, "running": h.inbox.digest.Running(ws.User.Sub),
		"has_new": len(newItems) > 0, "executor": h.cfg.Inbox.Digest.Executor, "schedule": h.cfg.Inbox.Digest.ScheduleTimes(),
		"timezone": digestLoc(h).String()})
}

func (h *Hub) apiInboxDigest(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	d, err := h.inbox.store.DigestByID(ws.User.Sub, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such digest")
		return
	}
	writeJSON(w, http.StatusOK, h.digestView(d, true))
}

func (h *Hub) apiInboxStartDigest(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	var q struct {
		Accounts []string `json:"accounts"`
	}
	if r.ContentLength != 0 && !decodeBody(w, r, &q) {
		return
	}
	for _, id := range q.Accounts {
		if a, err := h.inbox.store.Account(id); err != nil || a.OwnerSub != ws.User.Sub {
			apiError(w, http.StatusBadRequest, "no such account: "+id)
			return
		}
	}
	d, err := h.inbox.digest.Start(ws.User.Sub, q.Accounts, "api")
	switch {
	case errors.Is(err, inbox.ErrDigestRunning):
		apiError(w, http.StatusConflict, err.Error())
	case errors.Is(err, inbox.ErrNothingNew):
		apiError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		apiError(w, http.StatusInternalServerError, "could not start the digest")
	default:
		writeJSON(w, http.StatusAccepted, h.digestView(d, false))
	}
}

// --- tasks

func (h *Hub) apiInboxTasks(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	state := r.URL.Query().Get("state")
	if state != "" && !inbox.ValidTaskState(state) {
		apiError(w, http.StatusBadRequest, "state must be proposed, open, done or dismissed")
		return
	}
	ts, err := h.inbox.store.Tasks(ws.User.Sub, state, 500)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if ts == nil {
		ts = []*inbox.Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": ts})
}

func (h *Hub) apiInboxCreateTask(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	var q struct {
		Title, Body, Due string
	}
	if !decodeBody(w, r, &q) {
		return
	}
	t := &inbox.Task{OwnerSub: ws.User.Sub, Title: q.Title, Body: q.Body, Due: q.Due, State: inbox.TaskOpen, Source: inbox.TaskByUser}
	if err := h.inbox.store.InsertTask(t); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (h *Hub) apiInboxPatchTask(w http.ResponseWriter, r *http.Request, ws *WebSession) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	t, err := h.inbox.store.TaskByID(ws.User.Sub, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such task")
		return
	}
	var q struct {
		State, Title, Body, Due *string
	}
	if !decodeBody(w, r, &q) {
		return
	}
	inbox.ApplyTaskPatch(t, q.State, q.Title, q.Body, q.Due)
	if err := h.inbox.store.SaveTask(t); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// --- ingest (DESIGN §23.4)

// ingestAccount finds a push account by id, or by name when one push account has it.
func (h *Hub) ingestAccount(ref string) *inbox.Account {
	if a, err := h.inbox.store.Account(ref); err == nil {
		return a
	}
	as, _ := h.inbox.store.AccountsNamed(ref)
	var found *inbox.Account
	for _, a := range as {
		if a.Kind == "push" {
			if found != nil {
				return nil // ambiguous: use the id
			}
			found = a
		}
	}
	return found
}

func (h *Hub) handleIngest(w http.ResponseWriter, r *http.Request) {
	if h.inbox == nil {
		apiError(w, http.StatusNotFound, "the inbox is disabled on this hub")
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	a := h.ingestAccount(r.PathValue("account"))
	want := ""
	if a != nil && a.Kind == "push" && a.Enabled && a.KeyRef != "" {
		want = h.secrets.Keys[a.KeyRef]
	}
	if !ok || !push.CheckToken(tok, want) {
		time.Sleep(200 * time.Millisecond)
		apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !h.inbox.ingest.allow(a.ID, time.Now()) {
		w.Header().Set("Retry-After", "60")
		apiError(w, http.StatusTooManyRequests, "at most 60 requests per minute")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, push.MaxBody))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, "the body is larger than 256 KiB")
		return
	}
	items, err := push.Items(bytes.TrimSpace(body), time.Now())
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := h.inbox.store.InsertBatch(a.ID, items, nil, h.cfg.Inbox.MaxItemBytes)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"items_added": n})
}
