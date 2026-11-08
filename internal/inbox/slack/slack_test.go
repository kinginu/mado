package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/inbox"
)

const (
	general = "C01ABCDEF2"
	parent  = "1792433100.000300"
	token   = "xoxp-1111-2222-3333-abcdef"
)

// now is 2026-10-20T00:00:00Z; the fixture messages are about six hours older.
var now = time.Unix(1792454400, 0).UTC()

type msg = map[string]any

// fake is a Slack Web API built from the fixtures in testdata. It honours oldest
// (exclusive) for history and replies and refuses every method that is not a read.
type fake struct {
	t      *testing.T
	mu     sync.Mutex
	pages  [][]msg // general's history, newest first
	next   string  // general's page-1 next_cursor
	dms    map[string][]msg
	reps   []msg // the thread, parent first
	info   map[string]json.RawMessage
	users  map[string]json.RawMessage
	list   json.RawMessage
	auth   json.RawMessage
	calls  []string
	limits []int // Retry-After values to answer with 429, consumed per call
}

func load(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, dms: map[string][]msg{}}
	var p1, p2 struct {
		Messages         []msg `json:"messages"`
		ResponseMetadata struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	load(t, "conversations.history."+general+".page1.json", &p1)
	load(t, "conversations.history."+general+".page2.json", &p2)
	f.pages, f.next = [][]msg{p1.Messages, p2.Messages}, p1.ResponseMetadata.NextCursor
	for _, id := range []string{"D0DMDMDM01", "G0MPIMMP01"} {
		var h struct {
			Messages []msg `json:"messages"`
		}
		load(t, "conversations.history."+id+".json", &h)
		f.dms[id] = h.Messages
	}
	var r struct {
		Messages []msg `json:"messages"`
	}
	load(t, "conversations.replies."+general+"."+parent+".json", &r)
	f.reps = r.Messages
	load(t, "conversations.info.json", &f.info)
	load(t, "users.info.json", &f.users)
	load(t, "conversations.list.json", &f.list)
	load(t, "auth.test.json", &f.auth)
	return f
}

func after(ms []msg, oldest string, keepParent string) []msg {
	out := []msg{}
	for _, m := range ms {
		ts, _ := m["ts"].(string)
		if ts == keepParent || oldest == "" || tsLess(oldest, ts) {
			out = append(out, m)
		}
	}
	return out
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	q := r.URL.Query()
	f.calls = append(f.calls, method+" "+q.Get("channel")+q.Get("user")+" "+q.Get("cursor"))
	if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+token {
		f.t.Errorf("%s %s with auth %q", r.Method, method, r.Header.Get("Authorization"))
		w.WriteHeader(400)
		return
	}
	if len(f.limits) > 0 {
		ra := f.limits[0]
		f.limits = f.limits[1:]
		if ra >= 0 {
			w.Header().Set("Retry-After", fmt.Sprint(ra))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
	}
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(v)
	}
	page := func(ms []msg, more bool, next string) map[string]any {
		return map[string]any{"ok": true, "messages": ms, "has_more": more, "response_metadata": map[string]any{"next_cursor": next}}
	}
	switch method {
	case "auth.test":
		w.Write(f.auth)
	case "conversations.list":
		if q.Get("types") != "im,mpim" {
			f.t.Errorf("conversations.list types %q", q.Get("types"))
		}
		w.Write(f.list)
	case "conversations.info":
		if b, ok := f.info[q.Get("channel")]; ok {
			w.Write(b)
		} else {
			reply(map[string]any{"ok": false, "error": "channel_not_found"})
		}
	case "users.info":
		if b, ok := f.users[q.Get("user")]; ok {
			w.Write(b)
		} else {
			reply(map[string]any{"ok": false, "error": "user_not_found"})
		}
	case "conversations.history":
		if q.Get("limit") != "200" || q.Get("inclusive") != "false" || q.Get("oldest") == "" {
			f.t.Errorf("history query %v", q)
		}
		ch, oldest := q.Get("channel"), q.Get("oldest")
		switch {
		case ch == general && q.Get("cursor") == "":
			reply(page(after(f.pages[0], oldest, ""), true, f.next))
		case ch == general && q.Get("cursor") == f.next:
			reply(page(after(f.pages[1], oldest, ""), false, ""))
		case f.dms[ch] != nil:
			reply(page(after(f.dms[ch], oldest, ""), false, ""))
		default:
			reply(map[string]any{"ok": false, "error": "channel_not_found"})
		}
	case "conversations.replies":
		if q.Get("channel") != general || q.Get("ts") != parent {
			reply(map[string]any{"ok": false, "error": "thread_not_found"})
			return
		}
		reply(page(after(f.reps, q.Get("oldest"), parent), false, ""))
	default:
		f.t.Errorf("unexpected method %s", method)
		reply(map[string]any{"ok": false, "error": "unknown_method"})
	}
}

func (f *fake) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

func count(calls []string, method string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, method+" ") {
			n++
		}
	}
	return n
}

func setup(t *testing.T, config string) (*fake, inbox.Collector) {
	t.Helper()
	f := newFake(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	old := apiBase
	apiBase = srv.URL + "/api/"
	t.Cleanup(func() { apiBase = old })
	a := inbox.Account{Name: "slack-test", Kind: "slack", KeyRef: "slack-test", Config: json.RawMessage(config)}
	if err := inbox.Validate(&a, map[string]string{"slack-test": token}); err != nil {
		t.Fatal(err)
	}
	c, err := Kind{}.New(a, token, inbox.Env{HTTP: srv.Client(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func byExt(items []inbox.Item) map[string]inbox.Item {
	m := map[string]inbox.Item{}
	for _, it := range items {
		m[it.ExtID] = it
	}
	return m
}

func TestValidate(t *testing.T) {
	k := Kind{}
	keys := map[string]string{"ws": token, "bot": "xoxb-1-2-3", "junk": "hello"}
	good := []string{
		`{"channels":["C01ABCDEF2"]}`,
		`{"dms":true}`,
		`{"channels":["C01ABCDEF2","G0MPIMMP01","D0DMDMDM01"],"dms":true,"replies":false,"initial_hours":72}`,
	}
	for _, g := range good {
		if err := k.Validate(json.RawMessage(g), "ws", keys); err != nil {
			t.Errorf("%s: %v", g, err)
		}
	}
	bad := []string{
		`{}`,
		`{"channels":[]}`,
		`{"channels":["general"]}`,
		`{"channels":["c01abcdef2"]}`,
		`{"channels":["C0123"]}`,
		`{"channels":["U01AAAAAA0"]}`,
		`{"channels":["C01ABCDEF2","C01ABCDEF2"]}`,
		`{"channels":["C01ABCDEF2"],"initial_hours":-1}`,
		`{"channels":["C01ABCDEF2"],"initial_hours":100000}`,
		`{"channels":["C01ABCDEF2"],"workspace":"x"}`,
	}
	for _, b := range bad {
		if k.Validate(json.RawMessage(b), "ws", keys) == nil {
			t.Errorf("%s accepted", b)
		}
	}
	cfg := json.RawMessage(`{"dms":true}`)
	if k.Validate(cfg, "", keys) == nil {
		t.Error("missing key_ref accepted")
	}
	if k.Validate(cfg, "nope", keys) == nil {
		t.Error("unknown key_ref accepted")
	}
	if err := k.Validate(cfg, "bot", keys); err == nil || !strings.Contains(err.Error(), "bot token") {
		t.Errorf("bot token: %v", err)
	}
	if k.Validate(cfg, "junk", keys) == nil {
		t.Error("non-token accepted")
	}
	if err := k.Validate(cfg, "anything", nil); err != nil {
		t.Errorf("nil keys (config load) must defer the key check: %v", err)
	}
	if _, ok := inbox.KindByName("slack"); !ok {
		t.Error("slack not registered")
	}
	if err := k.Validate(k.Template(), "ws", keys); err != nil {
		t.Errorf("template: %v", err)
	}
}

// TestPollTwice drives the fixtures through Poll: the first poll pages history,
// fetches the thread once and the DMs; the second yields nothing and calls only
// history; a third sees exactly the new message and the new reply.
func TestPollTwice(t *testing.T) {
	f, c := setup(t, `{"channels":["C01ABCDEF2"],"dms":true}`)
	items, cur, more, err := c.Poll(context.Background(), nil)
	if err != nil || more {
		t.Fatalf("poll 1: more %v, err %v", more, err)
	}
	calls := f.take()
	if count(calls, "auth.test") != 1 || count(calls, "conversations.list") != 1 || count(calls, "conversations.replies") != 1 || count(calls, "conversations.info") != 1 {
		t.Errorf("poll 1 calls: %v", calls)
	}
	if count(calls, "conversations.history") != 4 { // general ×2 pages, DM, MPIM
		t.Errorf("poll 1 history calls: %v", calls)
	}
	if count(calls, "users.info") != 3 { // alice, bob, the missing user once
		t.Errorf("poll 1 users.info: %v", calls)
	}
	want := []string{ // oldest first
		general + ":1792432900.000100", // file only
		general + ":1792433000.000200", // ci-bot
		general + ":" + parent,
		general + ":1792433160.000310",
		general + ":1792433220.000320",
		general + ":1792433300.000500",
		"D0DMDMDM01:1792433400.000400",
		"G0MPIMMP01:1792433450.000450",
	}
	if len(items) != len(want) {
		for _, it := range items {
			t.Log(it.ExtID, it.Kind, it.Text)
		}
		t.Fatalf("poll 1: %d items, want %d", len(items), len(want))
	}
	for i, w := range want {
		if items[i].ExtID != w {
			t.Errorf("item %d: %s, want %s", i, items[i].ExtID, w)
		}
	}
	m := byExt(items)
	p := m[general+":"+parent]
	if p.Kind != "message" || p.Author != "Bob Builder" || p.Title != "#general" || p.ThreadKey != general+":"+parent {
		t.Errorf("parent: %+v", p)
	}
	if p.Text != "Anyone seen #general lately? <3 & thanks" {
		t.Errorf("parent text %q", p.Text)
	}
	if p.URL != "https://madotest.slack.com/archives/C01ABCDEF2/p1792433100000300" {
		t.Errorf("parent url %q", p.URL)
	}
	if !p.TS.Equal(time.Unix(1792433100, 300000).UTC()) {
		t.Errorf("parent ts %v", p.TS)
	}
	r1 := m[general+":1792433160.000310"]
	if r1.Kind != "reply" || r1.Author != "alice" || r1.ThreadKey != general+":"+parent || r1.Text != "@here yes, in #general" {
		t.Errorf("reply 1: %+v", r1)
	}
	if r1.URL != "https://madotest.slack.com/archives/C01ABCDEF2/p1792433160000310?thread_ts="+parent+"&cid=C01ABCDEF2" {
		t.Errorf("reply url %q", r1.URL)
	}
	r2 := m[general+":1792433220.000320"]
	if r2.Author != "U03CCCCCC" || r2.Text != "mail bob@example.com or @oncall" {
		t.Errorf("reply 2: %+v", r2)
	}
	m5 := m[general+":1792433300.000500"]
	if m5.Text != "Deploy done, see run 5 (https://example.com/run/5) cc @Bob Builder and https://example.com/raw" {
		t.Errorf("links/mentions: %q", m5.Text)
	}
	bot := m[general+":1792433000.000200"]
	if bot.Author != "ci-bot" || bot.Text != "Build #12 passed" || bot.Kind != "message" {
		t.Errorf("bot: %+v", bot)
	}
	file := m[general+":1792432900.000100"]
	if file.Text != "(files: report.pdf)" || strings.Contains(string(file.Raw), "url_private") || !strings.Contains(string(file.Raw), `"report.pdf"`) || !strings.Contains(string(file.Raw), `"channel":"C01ABCDEF2"`) {
		t.Errorf("file: %q raw %s", file.Text, file.Raw)
	}
	d := m["D0DMDMDM01:1792433400.000400"]
	if d.Kind != "dm" || d.Title != "DM Bob Builder" || d.Author != "Bob Builder" || d.Text != "hi @alice, lunch?" {
		t.Errorf("dm: %+v", d)
	}
	g := m["G0MPIMMP01:1792433450.000450"]
	if g.Kind != "dm" || g.Title != "DM alice, bob, inu" || g.Author != "alice" {
		t.Errorf("mpim: %+v", g)
	}
	var cc Cursor
	if err := json.Unmarshal(cur, &cc); err != nil {
		t.Fatal(err)
	}
	if cc.Channels[general] != "1792433300.000500" || cc.Threads[general+":"+parent] != "1792433220.000320" || cc.URL != "https://madotest.slack.com" || cc.Team != "Mado Test Workspace" || len(cc.Pending) != 0 || len(cc.DMs) != 2 {
		t.Errorf("cursor: %s", cur)
	}

	// Poll 2: nothing new; caches hold, the thread is not fetched again.
	items, cur2, more, err := c.Poll(context.Background(), cur)
	if err != nil || more || len(items) != 0 {
		t.Fatalf("poll 2: %d items, more %v, err %v", len(items), more, err)
	}
	calls = f.take()
	if len(calls) != count(calls, "conversations.history") || len(calls) != 3 {
		t.Errorf("poll 2 calls: %v", calls)
	}

	// Poll 3: a new top-level message and a new reply on the known thread.
	f.mu.Lock()
	f.pages[0] = append([]msg{{"type": "message", "user": "U01AAAAAA", "text": "new one", "ts": "1792440000.000600"}}, f.pages[0]...)
	for _, pm := range f.pages[0] {
		if pm["ts"] == parent {
			pm["reply_count"], pm["latest_reply"] = 3, "1792439000.000330"
		}
	}
	f.reps = append(f.reps, msg{"type": "message", "user": "U02BBBBBB", "text": "late reply", "ts": "1792439000.000330", "thread_ts": parent})
	f.mu.Unlock()
	items, _, more, err = c.Poll(context.Background(), cur2)
	if err != nil || more || len(items) != 2 {
		t.Fatalf("poll 3: %d items, more %v, err %v", len(items), more, err)
	}
	if items[0].ExtID != general+":1792439000.000330" || items[0].Kind != "reply" || items[1].ExtID != general+":1792440000.000600" {
		t.Errorf("poll 3 items: %s %s", items[0].ExtID, items[1].ExtID)
	}
	if calls = f.take(); count(calls, "conversations.replies") != 1 {
		t.Errorf("poll 3 calls: %v", calls)
	}
}

func TestDMsOffAndRepliesOff(t *testing.T) {
	f, c := setup(t, `{"channels":["C01ABCDEF2"],"replies":false}`)
	items, _, _, err := c.Poll(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.take()
	if count(calls, "conversations.list") != 0 || count(calls, "conversations.replies") != 0 {
		t.Errorf("calls: %v", calls)
	}
	for _, it := range items {
		if it.Kind != "message" || !strings.HasPrefix(it.ExtID, general+":") {
			t.Errorf("unexpected item %s %s", it.ExtID, it.Kind)
		}
	}
	if len(items) != 4 {
		t.Errorf("%d items", len(items))
	}
}

func TestDMsOnly(t *testing.T) {
	f, c := setup(t, `{"dms":true}`)
	items, _, _, err := c.Poll(context.Background(), nil)
	if err != nil || len(items) != 2 {
		t.Fatalf("%d items, err %v", len(items), err)
	}
	if calls := f.take(); count(calls, "conversations.info") != 0 {
		t.Errorf("calls: %v", calls)
	}
}

// TestMissingChannel: a bad channel is reported at the end of the round while the
// others are collected.
func TestMissingChannel(t *testing.T) {
	_, c := setup(t, `{"channels":["C0MISSING9","C01ABCDEF2"]}`)
	items, cur, more, err := c.Poll(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "C0MISSING9") || !strings.Contains(err.Error(), "channel_not_found") || more {
		t.Fatalf("err %v more %v", err, more)
	}
	if len(items) != 6 {
		t.Errorf("%d items", len(items))
	}
	var cc Cursor
	json.Unmarshal(cur, &cc)
	if len(cc.Errors) != 0 || cc.Channels[general] == "" {
		t.Errorf("cursor %s", cur)
	}
}

func TestRateLimit(t *testing.T) {
	var slept []time.Duration
	old := sleep
	sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	defer func() { sleep = old }()

	// A short Retry-After is waited out once and the call retried.
	f, c := setup(t, `{"channels":["C01ABCDEF2"]}`)
	f.limits = []int{-1, 3} // auth.test passes, the first info call is limited
	items, _, more, err := c.Poll(context.Background(), nil)
	if err != nil || more || len(items) != 6 || len(slept) != 1 || slept[0] != 3*time.Second {
		t.Fatalf("short: %d items, more %v, err %v, slept %v", len(items), more, err, slept)
	}

	// A second 429 in the same Poll: the partial batch comes back with more.
	slept = nil
	f, c = setup(t, `{"channels":["C01ABCDEF2"],"dms":true}`)
	f.limits = []int{-1, -1, -1, -1, -1, -1, 5, 5} // then a 429 twice, somewhere in the general channel
	items, cur, more, err := c.Poll(context.Background(), nil)
	if err != nil || !more || len(slept) != 1 {
		t.Fatalf("twice: %d items, more %v, err %v, slept %v", len(items), more, err, slept)
	}
	var cc Cursor
	json.Unmarshal(cur, &cc)
	if len(cc.Pending) == 0 {
		t.Fatalf("pending lost: %s", cur)
	}
	first := len(items)
	items, cur, more, err = c.Poll(context.Background(), cur)
	if err != nil || more {
		t.Fatalf("resume: more %v err %v", more, err)
	}
	cc = Cursor{}
	json.Unmarshal(cur, &cc)
	if first+len(items) < 8 || len(cc.Pending) != 0 {
		t.Errorf("resume: %d + %d items, cursor %s", first, len(items), cur)
	}

	// A long Retry-After ends the Poll at once without "more"; the round resumes later.
	slept = nil
	f, c = setup(t, `{"channels":["C01ABCDEF2"]}`)
	f.limits = []int{120}
	items, cur, more, err = c.Poll(context.Background(), nil)
	if err != nil || more || len(items) != 0 || len(slept) != 0 {
		t.Fatalf("long: %d items, more %v, err %v, slept %v", len(items), more, err, slept)
	}
	if items, _, _, err = c.Poll(context.Background(), cur); err != nil || len(items) != 6 {
		t.Fatalf("after long: %d items, err %v", len(items), err)
	}
}

func TestBadToken(t *testing.T) {
	f, c := setup(t, `{"channels":["C01ABCDEF2"]}`)
	f.auth = json.RawMessage(`{"ok":false,"error":"invalid_auth"}`)
	if _, _, _, err := c.Poll(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("err %v", err)
	}
}

func TestRewrite(t *testing.T) {
	p := &poll{cur: Cursor{Users: map[string]cached{"U1AAAAAAA": {N: "alice"}}, Names: map[string]cached{"C1AAAAAAA": {N: "#random"}}}}
	cases := map[string]string{
		"<@U1AAAAAAA> hi":                       "@alice hi",
		"<@U9ZZZZZZZ|zed> hi":                   "@zed hi",
		"<@U9ZZZZZZZ>":                          "@U9ZZZZZZZ",
		"<#C1AAAAAAA>":                          "#random",
		"<#C1AAAAAAA|chan>":                     "#chan",
		"<#C2BBBBBBB>":                          "#C2BBBBBBB",
		"<!channel> <!everyone>":                "@channel @everyone",
		"<!date^1392734382^{date}|Feb 18>":      "Feb 18",
		"<https://a.example/x?y=1&amp;z=2>":     "https://a.example/x?y=1&z=2",
		"<https://a.example|A>":                 "A (https://a.example)",
		"<https://a.example|https://a.example>": "https://a.example",
		"<mailto:x@y.z|x@y.z>":                  "x@y.z",
		"a &lt;b&gt; &amp;amp;":                 "a <b> &amp;",
		"plain *bold* _it_":                     "plain *bold* _it_",
	}
	for in, want := range cases {
		if got := p.rewrite(in); got != want {
			t.Errorf("rewrite(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTS(t *testing.T) {
	if !tsLess("1792433100.000300", "1792433100.000301") || tsLess("1792433100.1", "1792433100.000300") || !tsLess("999999999.9", "1000000000.0") {
		t.Error("tsLess")
	}
	if got := tsFromTime(time.Unix(1792433100, 300000)); got != "1792433100.000300" {
		t.Error(got)
	}
	if mpimName("mpdm-alice--bob--inu-1") != "alice, bob, inu" {
		t.Error(mpimName("mpdm-alice--bob--inu-1"))
	}
}
