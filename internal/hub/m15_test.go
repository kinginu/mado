package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/usage"
)

// M15 (DESIGN §23): the inbox in the hub.

const ingestToken = "tok-0123456789abcdef"

type inboxHub struct {
	*chatHub
	sub string
}

// newInboxHub is a chat hub with [inbox] enabled, a push account "angel" of the
// logged-in user seeded from config, and a digest on the chat executor.
func newInboxHub(t *testing.T, mutate func(*Config)) *inboxHub {
	t.Helper()
	llm := newFakeLLM(t)
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := &Config{BaseURL: srv.URL, Listen: "127.0.0.1:0",
		Chat: ChatConfig{Enabled: true, DataDir: filepath.Join(dir, "files"), MaxFileMB: 1, UserQuotaMB: 2, MaxToolRounds: 8,
			Models: []ChatModelConfig{{Name: "glm-5.3", BaseURL: llm.srv.URL, Model: "zai-org/glm-5.3", KeyRef: "aiand", Default: true}}},
		Prices: usage.Prices{{Model: "zai-org/glm-*", Input: 0.6, Output: 2.2, CacheRead: 0.1}},
		Inbox: InboxConfig{Enabled: true, Digest: InboxDigestConfig{Executor: "chat", Timezone: "Asia/Tokyo", Language: "ja"},
			Accounts: []InboxAccountConfig{{Name: "angel", Kind: "push", Owner: "local@localhost", KeyRef: "ingest"}}}}
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Chat.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Inbox.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h, err := New(cfg, store, &Secrets{Keys: map[string]string{"aiand": "sk-test", "ingest": ingestToken}}, Options{LocalLogin: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	handler = h.Handler()
	x := &chatHub{testHub: &testHub{h: h, srv: srv, store: store}, c: browser(), llm: llm, dir: dir}
	x.c.Post(srv.URL+"/local-login", "", nil)
	var me struct {
		CSRF, Sub, Email string
		Inbox            bool
	}
	x.getJSON(t, "/api/me", &me)
	if !me.Inbox {
		t.Fatal("/api/me: inbox false")
	}
	x.csrf = me.CSRF
	return &inboxHub{chatHub: x, sub: me.Sub}
}

func (x *inboxHub) ingest(t *testing.T, account, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", x.srv.URL+"/api/inbox/ingest/"+account, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (x *inboxHub) items(t *testing.T, query string) []inbox.ItemRow {
	t.Helper()
	var r struct{ Items []inbox.ItemRow }
	if st := x.getJSON(t, "/api/inbox/items"+query, &r); st != 200 {
		t.Fatalf("items: %d", st)
	}
	return r.Items
}

func TestInboxConfig(t *testing.T) {
	write := func(body string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "hub.toml")
		os.WriteFile(p, []byte("base_url = \"https://mado.example\"\n"+body), 0o600)
		return LoadConfig(p)
	}
	c, err := write(`
[inbox]
enabled = true
[inbox.digest]
timezone = "Asia/Tokyo"
[[inbox.accounts]]
name = "x-ai"
kind = "rss"
owner = "me@example.org"
interval = "15m"
[inbox.accounts.config]
url = "http://home-twitter-rss.example.ts.net:1200/twitter/home_latest"
allow_private = true
`)
	if err != nil {
		t.Fatal(err)
	}
	d := c.Inbox.Digest
	if c.Inbox.MaxItemBytes != 65536 || c.Inbox.RetentionDays != 90 || d.Executor != "claude" || d.MainModel != "sonnet" ||
		strings.Join(d.ScheduleTimes(), ",") != "08:00,20:00" || d.MaxItems != 400 || d.MaxBytes != 307200 || d.Language != "en" {
		t.Fatalf("defaults %+v", c.Inbox)
	}
	a, err := c.Inbox.Accounts[0].account()
	if err != nil || a.IntervalS != 900 || string(a.Config) != `{"allow_private":true,"url":"http://home-twitter-rss.example.ts.net:1200/twitter/home_latest"}` {
		t.Fatalf("account %+v %s %v", a, a.Config, err)
	}
	c, err = write("[inbox.digest]\nschedule = []\n")
	if err != nil || len(c.Inbox.Digest.ScheduleTimes()) != 0 {
		t.Fatalf("empty schedule: %v %v", c.Inbox.Digest.Schedule, err)
	}
	for _, bad := range []string{
		"[inbox.digest]\nexecutor = \"gpt\"\n",
		"[inbox.digest]\nschedule = [\"8am\"]\n",
		"[inbox.digest]\ntimezone = \"Mars/Base\"\n",
		"[inbox]\nretention_days = -1\n",
		"[[inbox.accounts]]\nname = \"a\"\nkind = \"nope\"\nowner = \"x\"\n",
		"[[inbox.accounts]]\nname = \"a b\"\nkind = \"push\"\nowner = \"x\"\nkey_ref = \"k\"\n",
		"[[inbox.accounts]]\nname = \"a\"\nkind = \"push\"\nkey_ref = \"k\"\n",
		"[[inbox.accounts]]\nname = \"a\"\nkind = \"rss\"\nowner = \"x\"\ninterval = \"30s\"\n[inbox.accounts.config]\nurl = \"https://x/\"\n",
		"[[inbox.accounts]]\nname = \"a\"\nkind = \"rss\"\nowner = \"x\"\n[inbox.accounts.config]\nurl = \"https://x/\"\nbogus = 1\n",
		"[[inbox.accounts]]\nname = \"a\"\nkind = \"push\"\nowner = \"x\"\nkey_ref = \"k\"\n[[inbox.accounts]]\nname = \"a\"\nkind = \"push\"\nowner = \"X\"\nkey_ref = \"k\"\n",
	} {
		if _, err := write(bad); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
	// key_refs are checked against the secrets file at start.
	_, err = New(&Config{BaseURL: "http://127.0.0.1:1", Inbox: InboxConfig{Enabled: true,
		Accounts: []InboxAccountConfig{{Name: "p", Kind: "push", Owner: "x", KeyRef: "missing"}}}},
		mustStore(t), &Secrets{Keys: map[string]string{}}, Options{LocalLogin: true})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing key_ref: %v", err)
	}
}

func mustStore(t *testing.T) *Store {
	s, err := OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestExampleInboxConfig(t *testing.T) {
	c, err := LoadConfig("../../deploy/hub.toml.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Inbox.Accounts) == 0 || c.Inbox.Digest.Timezone == "" {
		t.Fatalf("example [inbox] %+v", c.Inbox)
	}
}

func TestIngest(t *testing.T) {
	x := newInboxHub(t, nil)
	var accts struct{ Accounts []apiInboxAccount }
	x.getJSON(t, "/api/inbox/accounts", &accts)
	if len(accts.Accounts) != 1 || !accts.Accounts[0].Resolved || accts.Accounts[0].OwnerSub != x.sub ||
		accts.Accounts[0].IngestPath == "" || accts.Accounts[0].Editable {
		t.Fatalf("accounts %+v (owner resolved from the e-mail?)", accts.Accounts)
	}
	id := accts.Accounts[0].ID
	if st, _ := x.ingest(t, id, "", `{"text":"x"}`); st != 401 {
		t.Fatalf("no token: %d", st)
	}
	if st, _ := x.ingest(t, id, ingestToken+"x", `{"text":"x"}`); st != 401 {
		t.Fatalf("wrong token: %d", st)
	}
	if st, _ := x.ingest(t, "nope", ingestToken, `{"text":"x"}`); st != 401 {
		t.Fatalf("unknown account: %d", st)
	}
	st, out := x.ingest(t, id, ingestToken, `[{"text":"<b>hello</b> from angel","author":"bob","url":"javascript:alert(1)"},{"text":"two"}]`)
	if st != 200 || out["items_added"] != 2.0 {
		t.Fatalf("ingest: %d %v", st, out)
	}
	// By name, and a repeat is deduplicated.
	if st, out = x.ingest(t, "angel", ingestToken, `{"text":"two"}`); st != 200 || out["items_added"] != 0.0 {
		t.Fatalf("by name: %d %v", st, out)
	}
	if st, _ = x.ingest(t, id, ingestToken, `not json`); st != 400 {
		t.Fatalf("bad body: %d", st)
	}
	big := `{"text":"` + strings.Repeat("a", 300<<10) + `"}`
	if st, _ = x.ingest(t, id, ingestToken, big); st != 413 {
		t.Fatalf("big body: %d", st)
	}
	its := x.items(t, "")
	if len(its) != 2 || its[1].Text != "<b>hello</b> from angel" || its[1].URL != "" || its[1].Author != "bob" {
		t.Fatalf("items %+v", its)
	}
	// Rate limit: 60 per minute per account (6 used above).
	codes := map[int]int{}
	for i := 0; i < 60; i++ {
		st, _ := x.ingest(t, id, ingestToken, fmt.Sprintf(`{"text":"r%d"}`, i))
		codes[st]++
	}
	if codes[429] == 0 || codes[200] > 56 {
		t.Fatalf("rate limit: %v", codes)
	}
	// Ingest needs no cookie and no CSRF, but nothing else under /api/inbox does without them.
	req, _ := http.NewRequest("POST", x.srv.URL+"/api/inbox/tasks", strings.NewReader(`{"title":"x"}`))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("tasks without login: %d", resp.StatusCode)
	}
	// No audit row carries item content.
	rows, _ := x.store.AuditAll(500)
	for _, r := range rows {
		if strings.Contains(r.Detail, "hello") || strings.Contains(r.Detail, "angel</") {
			t.Fatalf("content in audit: %+v", r)
		}
	}
}

func TestInboxAccountsAPIAndOwnership(t *testing.T) {
	x := newInboxHub(t, nil)
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<rss version="2.0"><channel><title>t</title>
			<item><title>Kumamoto fab opens</title><guid>g1</guid><link>https://example.org/1</link><description>半導体の新工場</description></item>
			<item><title>Second</title><guid>g2</guid></item></channel></rss>`))
	}))
	defer feed.Close()
	// The local user is an admin. Make a second, non-admin user.
	bob := x.loginAs(t, "bob-sub", false)
	var bobMe struct{ CSRF string }
	resp, _ := bob.Get(x.srv.URL + "/api/me")
	json.NewDecoder(resp.Body).Decode(&bobMe)
	resp.Body.Close()
	bobDo := func(method, path string, body, v any) int {
		return apiDo(t, bob, x.srv.URL, bobMe.CSRF, method, path, body, v)
	}
	mk := map[string]any{"name": "chips", "kind": "rss", "owner_sub": "bob-sub@example.org", "interval_s": 300,
		"config": map[string]any{"url": feed.URL, "allow_private": true}}
	if st := bobDo("POST", "/api/inbox/accounts", mk, nil); st != 403 {
		t.Fatalf("non-admin create: %d", st)
	}
	var created apiInboxAccount
	if st := x.do(t, "POST", "/api/inbox/accounts", mk, &created); st != 201 || created.OwnerSub != "bob-sub" || !created.Editable {
		t.Fatalf("admin create: %d %+v", st, created)
	}
	bad := map[string]any{"name": "x", "kind": "rss", "config": map[string]any{"url": "ftp://x"}}
	if st := x.do(t, "POST", "/api/inbox/accounts", bad, nil); st != 400 {
		t.Fatalf("bad config: %d", st)
	}
	if st := x.do(t, "POST", "/api/inbox/accounts", map[string]any{"name": "y", "kind": "push", "key_ref": "nope"}, nil); st != 400 {
		t.Fatalf("missing key_ref: %d", st)
	}
	if st := x.do(t, "POST", "/api/inbox/accounts", map[string]any{"name": "z", "kind": "push", "owner_sub": "ghost"}, nil); st != 400 {
		t.Fatalf("unknown owner: %d", st)
	}
	// Bob sees his account, without key_ref; the admin sees all.
	var list struct {
		Accounts []apiInboxAccount
		Kinds    []struct{ Name string }
	}
	resp, _ = bob.Get(x.srv.URL + "/api/inbox/accounts")
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Accounts) != 1 || list.Accounts[0].Name != "chips" || list.Kinds != nil {
		t.Fatalf("bob's accounts %+v", list)
	}
	x.getJSON(t, "/api/inbox/accounts", &list)
	if len(list.Accounts) != 2 || len(list.Kinds) < 2 {
		t.Fatalf("admin's accounts %+v", list)
	}
	// Bob polls his account; the guard is off for it (allow_private).
	var polled map[string]int
	if st := bobDo("POST", "/api/inbox/accounts/"+created.ID+"/poll", nil, &polled); st != 200 || polled["items_added"] != 2 {
		t.Fatalf("poll: %d %v", st, polled)
	}
	// The admin cannot read bob's items; bob can, and can search them.
	if its := x.items(t, "?account="+created.ID); len(its) != 0 {
		t.Fatalf("admin sees bob's items: %d", len(its))
	}
	var r struct{ Items []inbox.ItemRow }
	resp, _ = bob.Get(x.srv.URL + "/api/inbox/items?q=" + "新工場")
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if len(r.Items) != 1 || r.Items[0].Title != "Kumamoto fab opens" {
		t.Fatalf("bob's search %+v", r.Items)
	}
	itemPath := fmt.Sprintf("/api/inbox/items/%d", r.Items[0].ID)
	if st := x.getJSON(t, itemPath, nil); st != 404 {
		t.Fatalf("admin reads bob's item: %d", st)
	}
	if st := x.do(t, "PATCH", itemPath, map[string]string{"state": "archived"}, nil); st != 404 {
		t.Fatalf("admin changes bob's item: %d", st)
	}
	if st := bobDo("PATCH", itemPath, map[string]string{"state": "archived"}, nil); st != 200 {
		t.Fatalf("bob archives: %d", st)
	}
	var changed map[string]int
	bobDo("POST", "/api/inbox/items/state", map[string]any{"all_new": true}, &changed)
	if changed["changed"] != 1 {
		t.Fatalf("mark all read: %v", changed)
	}
	// Edit and delete: admin only, UI rows only.
	if st := bobDo("PATCH", "/api/inbox/accounts/"+created.ID, map[string]any{"enabled": false}, nil); st != 403 {
		t.Fatalf("bob edits: %d", st)
	}
	var upd apiInboxAccount
	if st := x.do(t, "PATCH", "/api/inbox/accounts/"+created.ID, map[string]any{"enabled": false, "interval_s": 600}, &upd); st != 200 ||
		upd.Enabled || upd.IntervalS != 600 {
		t.Fatalf("admin edits: %d %+v", st, upd)
	}
	if st := x.do(t, "PATCH", "/api/inbox/accounts/"+created.ID, map[string]any{"kind": "push"}, nil); st != 400 {
		t.Fatalf("kind change: %d", st)
	}
	angel := ""
	for _, a := range list.Accounts {
		if a.Name == "angel" {
			angel = a.ID
		}
	}
	if st := x.do(t, "DELETE", "/api/inbox/accounts/"+angel, nil, nil); st != 400 {
		t.Fatalf("delete a config account: %d", st)
	}
	if st := x.do(t, "POST", "/api/inbox/accounts/"+angel+"/poll", nil, nil); st != 400 {
		t.Fatalf("poll a push account: %d", st)
	}
	if st := x.do(t, "DELETE", "/api/inbox/accounts/"+created.ID, nil, nil); st != 204 {
		t.Fatalf("delete: %d", st)
	}
	var n int
	x.store.db.QueryRow(`SELECT COUNT(*) FROM inbox_items WHERE account_id=?`, created.ID).Scan(&n)
	if n != 0 {
		t.Fatal("items of a deleted account kept")
	}
	audits, _ := x.store.AuditAll(100)
	var acts []string
	for _, a := range audits {
		if strings.HasPrefix(a.Action, "inbox_account") {
			acts = append(acts, a.Action)
		}
	}
	if strings.Join(acts, ",") != "inbox_account_delete,inbox_account_disable,inbox_account_create" {
		t.Fatalf("audit %v", acts)
	}
}

func TestTasksAPI(t *testing.T) {
	x := newInboxHub(t, nil)
	var task inbox.Task
	if st := x.do(t, "POST", "/api/inbox/tasks", map[string]string{"title": "Reply to Bob", "due": "2026-10-25"}, &task); st != 201 ||
		task.State != "open" || task.Source != "user" {
		t.Fatalf("create: %d %+v", st, task)
	}
	if st := x.do(t, "POST", "/api/inbox/tasks", map[string]string{"title": " "}, nil); st != 400 {
		t.Fatalf("empty title: %d", st)
	}
	if st := x.do(t, "PATCH", fmt.Sprintf("/api/inbox/tasks/%d", task.ID), map[string]string{"state": "done"}, &task); st != 200 || task.State != "done" {
		t.Fatalf("done: %d %+v", st, task)
	}
	if st := x.do(t, "PATCH", fmt.Sprintf("/api/inbox/tasks/%d", task.ID), map[string]string{"state": "later"}, nil); st != 400 {
		t.Fatalf("bad state: %d", st)
	}
	bob := x.loginAs(t, "bob", false)
	resp, _ := bob.Get(x.srv.URL + "/api/inbox/tasks")
	var r struct{ Tasks []inbox.Task }
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if len(r.Tasks) != 0 {
		t.Fatal("bob sees the admin's tasks")
	}
}

func waitDigestState(t *testing.T, x *inboxHub, id int64) apiDigest {
	t.Helper()
	for i := 0; i < 300; i++ {
		var d apiDigest
		x.getJSON(t, fmt.Sprintf("/api/inbox/digests/%d", id), &d)
		if d.Digest != nil && d.State != "running" {
			return d
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("digest still running")
	return apiDigest{}
}

func errorStream(msg string) string {
	return "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"" + msg + "\"}}\n\n"
}

// TestDigestChatExecutor runs a digest end to end on the fake Messages API: the
// model proposes a task, then answers with the digest.
func TestDigestChatExecutor(t *testing.T) {
	x := newInboxHub(t, nil)
	if st := x.do(t, "POST", "/api/inbox/digests", nil, nil); st != 400 {
		t.Fatalf("digest of nothing: %d", st)
	}
	if st, out := x.ingest(t, "angel", ingestToken, `[{"text":"Please review PR #42","author":"carol","kind":"review_requested"},{"text":"FYI: release out"}]`); st != 200 {
		t.Fatalf("ingest %d %v", st, out)
	}
	its := x.items(t, "")
	if len(its) != 2 {
		var raw any
		x.getJSON(t, "/api/inbox/items?state=all", &raw)
		t.Fatalf("items %v", raw)
	}
	ids := []int64{its[0].ID, its[1].ID}
	x.llm.mu.Lock()
	x.llm.script = []string{
		toolStream("msg_t", "tu_1", "propose_task", map[string]any{"title": "Review PR #42", "body": "carol asked", "item_ids": ids}),
		chatStream("msg_d", fmt.Sprintf("## angel\n- carol asks for a review [#%d]\n- release out [#%d, #%d]", ids[1], ids[0], ids[1])),
	}
	x.llm.mu.Unlock()
	var started apiDigest
	if st := x.do(t, "POST", "/api/inbox/digests", map[string]any{}, &started); st != 202 || started.ItemCount != 2 {
		t.Fatalf("start: %d %+v", st, started)
	}
	d := waitDigestState(t, x, started.ID)
	if d.State != "done" || d.Executor != "chat" || d.SessionID == "" || d.Model != "zai-org/glm-5.3" || d.Cost <= 0 {
		t.Fatalf("digest %+v", d.Digest)
	}
	if !strings.Contains(d.HTML, fmt.Sprintf(`<button type="button" class="item-ref" data-item="%d">#%d</button>`, ids[1], ids[1])) {
		t.Fatalf("html %s", d.HTML)
	}
	// The request: digest prompt as system, records as the user message, one tool.
	req := x.llm.req(0)
	sys, _ := req["system"].(string)
	if !strings.Contains(sys, "inbox summarizer") || !strings.Contains(sys, "language: ja") {
		t.Fatalf("system %q", sys)
	}
	toolsJSON, _ := json.Marshal(req["tools"])
	if !strings.Contains(string(toolsJSON), `"propose_task"`) || strings.Contains(string(toolsJSON), "web_search") || strings.Contains(string(toolsJSON), "search_inbox") {
		t.Fatalf("tools %s", toolsJSON)
	}
	if u := lastUserText(req); !strings.Contains(u, "inbox-items") || !strings.Contains(u, "@carol (angel) Please review PR #42") {
		t.Fatalf("user message %q", u)
	}
	// Items read with the digest id; the task proposed.
	read := x.items(t, "?state=read")
	if len(read) != 2 || read[0].DigestID != started.ID {
		t.Fatalf("read items %+v", read)
	}
	var tr struct{ Tasks []inbox.Task }
	x.getJSON(t, "/api/inbox/tasks?state=proposed", &tr)
	if len(tr.Tasks) != 1 || tr.Tasks[0].Title != "Review PR #42" || len(tr.Tasks[0].ItemIDs) != 2 || tr.Tasks[0].DigestID != started.ID {
		t.Fatalf("tasks %+v", tr.Tasks)
	}
	// The session: kind digest, hidden behind a filter in the UI, takes no messages.
	var sv struct {
		Session apiSessionView
		Tools   []string
	}
	x.getJSON(t, "/api/sessions/"+d.SessionID, &sv)
	if sv.Session.Kind != "digest" || len(sv.Tools) != 1 || sv.Tools[0] != "propose_task" {
		t.Fatalf("session %+v", sv)
	}
	if st := x.do(t, "POST", "/api/sessions/"+d.SessionID+"/send", map[string]string{"text": "hi"}, nil); st != 400 {
		t.Fatalf("send to a digest session: %d", st)
	}
	rows, _ := x.store.UsageRows(d.SessionID)
	if len(rows) != 2 || rows[0].Kind != "digest" {
		t.Fatalf("usage rows %+v", rows)
	}

	// A failed run leaves the items new.
	x.ingest(t, "angel", ingestToken, `{"text":"third"}`)
	x.llm.mu.Lock()
	x.llm.script = []string{errorStream("context too long for this test")}
	x.llm.mu.Unlock()
	if st := x.do(t, "POST", "/api/inbox/digests", nil, &started); st != 202 {
		t.Fatalf("start 2: %d", st)
	}
	d = waitDigestState(t, x, started.ID)
	if d.State != "failed" || d.Error == "" || d.SessionID == "" {
		t.Fatalf("failed digest %+v", d.Digest)
	}
	if fresh := x.items(t, "?state=new"); len(fresh) != 1 {
		t.Fatalf("new after failure: %d", len(fresh))
	}
	var list struct {
		Digests []apiDigest
		HasNew  bool `json:"has_new"`
	}
	x.getJSON(t, "/api/inbox/digests", &list)
	if len(list.Digests) != 2 || !list.HasNew || list.Digests[1].Text != "" {
		t.Fatalf("list %+v", list)
	}
	audits, _ := x.store.AuditAll(100)
	var digestAudits int
	for _, a := range audits {
		if a.Action == "inbox_digest" {
			digestAudits++
			if strings.Contains(a.Detail, "review") {
				t.Fatalf("content in audit %+v", a)
			}
		}
	}
	if digestAudits != 4 {
		t.Fatalf("digest audits %d", digestAudits)
	}
}

// TestChatGetsInboxTools: a chat turn offers the inbox tools next to the web
// tools, scoped to the owner.
func TestChatGetsInboxTools(t *testing.T) {
	x := newInboxHub(t, nil)
	x.ingest(t, "angel", ingestToken, `{"text":"the quarterly numbers are in","author":"dana"}`)
	x.llm.mu.Lock()
	x.llm.script = []string{
		toolStream("m1", "tu_1", "search_inbox", map[string]any{"query": "quarterly"}),
		chatStream("m2", "Dana says the numbers are in."),
	}
	x.llm.mu.Unlock()
	sid := x.newChat(t, "", "")
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]string{"text": "what is new?"}, nil); st != 202 {
		t.Fatalf("send: %d", st)
	}
	for i := 0; i < 200 && x.llm.count() < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	x.h.runner.Wait(sid)
	toolsJSON, _ := json.Marshal(x.llm.req(0)["tools"])
	for _, n := range []string{"search_inbox", "get_item", "list_tasks", "create_task", "update_task"} {
		if !strings.Contains(string(toolsJSON), `"`+n+`"`) {
			t.Fatalf("tool %s missing: %s", n, toolsJSON)
		}
	}
	second, _ := json.Marshal(x.llm.req(1)["messages"])
	if !bytes.Contains(second, []byte("quarterly numbers")) || !bytes.Contains(second, []byte("third-party")) {
		t.Fatalf("tool result %s", second)
	}
	var sv struct{ Tools []string }
	x.getJSON(t, "/api/sessions/"+sid, &sv)
	if !strings.Contains(strings.Join(sv.Tools, ","), "search_inbox") {
		t.Fatalf("panel tools %v", sv.Tools)
	}
}

func TestInboxDisabled(t *testing.T) {
	x := newChatHub(t)
	if st := x.getJSON(t, "/api/inbox/items", nil); st != 404 {
		t.Fatalf("items with inbox off: %d", st)
	}
	req, _ := http.NewRequest("POST", x.srv.URL+"/api/inbox/ingest/x", strings.NewReader(`{}`))
	resp, _ := http.DefaultClient.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("ingest with inbox off: %d", resp.StatusCode)
	}
}

func TestDigestHTML(t *testing.T) {
	got := digestHTML("- a [#12]\n- b [#3, #4]\n- <script>x</script> [#x]")
	if !strings.Contains(got, `data-item="12"`) || !strings.Contains(got, `data-item="4"`) || strings.Contains(got, "<script>") ||
		strings.Contains(got, `data-item="x"`) {
		t.Fatal(got)
	}
}
