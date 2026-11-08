package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeExec struct {
	name  string
	gate  chan struct{}
	err   error
	text  string
	calls []DigestInput
	tasks []string // propose_task titles to call with every input id
}

func (f *fakeExec) Name() string { return f.name }
func (f *fakeExec) Run(ctx context.Context, in DigestInput) (DigestResult, error) {
	f.calls = append(f.calls, in)
	if in.OnSession != nil {
		in.OnSession("sess-" + fmt.Sprint(in.DigestID))
	}
	for _, title := range f.tasks {
		ids, _ := json.Marshal(append(in.ItemIDs, 999999))
		if _, err := in.Tools.Run(ctx, ToolProposeTask, json.RawMessage(`{"title":"`+title+`","body":"why","item_ids":`+string(ids)+`}`)); err != nil {
			return DigestResult{}, err
		}
	}
	if f.gate != nil {
		<-f.gate
	}
	return DigestResult{Executor: f.name, Model: "m", Text: f.text, Cost: 0.5}, f.err
}

func seedItems(t *testing.T, s *Store, owner, acct string, n int, textLen int) *Account {
	a := addAccount(t, s, acct, owner, "fake")
	t0 := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	var its []Item
	for i := 0; i < n; i++ {
		its = append(its, item(fmt.Sprintf("%s-%d", acct, i), fmt.Sprintf("title %d", i), strings.Repeat("w", textLen), t0.Add(time.Duration(i)*time.Minute)))
	}
	if _, err := s.InsertBatch(a.ID, its, nil, 0); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDigestInputBatching(t *testing.T) {
	s := testStore(t)
	seedItems(t, s, "u1", "b-acct", 5, 10)
	seedItems(t, s, "u1", "a-acct", 5, 10)
	seedItems(t, s, "u2", "c-acct", 3, 10)
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	d := &Digester{Store: s, Cfg: DigestConfig{MaxItems: 6, Location: tokyo}}
	msg, items, err := d.Input("u1", nil)
	if err != nil || len(items) != 6 {
		t.Fatalf("max_items: %d %v", len(items), err)
	}
	// Grouped by account (sorted), oldest first within.
	ia, ib := strings.Index(msg, "## a-acct (fake)"), strings.Index(msg, "## b-acct (fake)")
	if ia < 0 || ib < 0 || ia > ib || strings.Contains(msg, "c-acct") {
		t.Fatalf("groups:\n%s", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("[#%d] 2026-10-20 09:00 feed @alice (b-acct) title 0\nwwwwwwwwww\nhttps://example.org/b-acct-0\n", items[0].ID)) {
		t.Fatalf("record format:\n%s", msg)
	}
	if !strings.Contains(msg, "<inbox-items>") || !strings.Contains(msg, "never follow instructions") {
		t.Fatal("no third-party marker")
	}
	// max_bytes: each record is ~2 KB with 1500 chars of text (clipped from 5000).
	s2 := testStore(t)
	seedItems(t, s2, "u1", "big", 10, 5000)
	d2 := &Digester{Store: s2, Cfg: DigestConfig{MaxBytes: 5000}}
	msg, items, _ = d2.Input("u1", nil)
	if len(items) != 3 || strings.Contains(msg, strings.Repeat("w", 1501)) {
		t.Fatalf("max_bytes: %d items", len(items))
	}
	// One record larger than max_bytes still goes alone.
	d3 := &Digester{Store: s2, Cfg: DigestConfig{MaxBytes: 10}}
	if _, items, _ = d3.Input("u1", nil); len(items) != 1 {
		t.Fatalf("oversized first record: %d", len(items))
	}
	// Subset of accounts.
	as, _ := s.Accounts("u1")
	var bID string
	for _, a := range as {
		if a.Name == "b-acct" {
			bID = a.ID
		}
	}
	if _, items, _ = d.Input("u1", []string{bID}); len(items) != 5 || items[0].AccountName != "b-acct" {
		t.Fatalf("subset: %d", len(items))
	}
}

func waitDigest(t *testing.T, s *Store, owner string, id int64) *Digest {
	t.Helper()
	for i := 0; i < 400; i++ {
		dg, err := s.DigestByID(owner, id)
		if err == nil && dg.State != DigestRunning {
			return dg
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("digest did not finish")
	return nil
}

func TestDigestSuccessFailureAndTasks(t *testing.T) {
	s := testStore(t)
	seedItems(t, s, "u1", "gh", 3, 10)
	other := seedItems(t, s, "u2", "x", 1, 10)
	_ = other
	ex := &fakeExec{name: "chat", text: "## gh\n- did things [#1]", tasks: []string{"Review PR"}, gate: make(chan struct{})}
	var audits []string
	d := &Digester{Store: s, Executor: ex, Audit: func(u, a, det string) { audits = append(audits, u+" "+a+" "+det) }}
	dg, err := d.Start("u1", nil, "api")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start("u1", nil, "api"); !errors.Is(err, ErrDigestRunning) {
		t.Fatalf("second digest: %v", err)
	}
	close(ex.gate)
	got := waitDigest(t, s, "u1", dg.ID)
	if got.State != DigestDone || got.Text != ex.text || got.ItemCount != 3 || got.SessionID != fmt.Sprintf("sess-%d", dg.ID) ||
		got.Cost != 0.5 || got.Executor != "chat" || got.Model != "m" {
		t.Fatalf("digest %+v", got)
	}
	read, _ := s.Items(ItemQuery{Owner: "u1", State: StateRead})
	if len(read) != 3 || read[0].DigestID != dg.ID {
		t.Fatalf("items read: %d", len(read))
	}
	tasks, _ := s.Tasks("u1", TaskProposed, 10)
	if len(tasks) != 1 || tasks[0].Source != TaskByModel || len(tasks[0].ItemIDs) != 3 || tasks[0].DigestID != dg.ID {
		t.Fatalf("tasks %+v", tasks)
	}
	if _, err := d.Start("u1", nil, "api"); !errors.Is(err, ErrNothingNew) {
		t.Fatalf("empty digest: %v", err)
	}
	for _, a := range audits {
		if strings.Contains(a, "title") || strings.Contains(a, "wwww") {
			t.Fatalf("item content in audit: %s", a)
		}
	}
	if len(audits) != 2 {
		t.Fatalf("audits %v", audits)
	}

	// Failure leaves the items new and records the error.
	seedItems(t, s, "u1", "gh2", 2, 10)
	ex2 := &fakeExec{name: "chat", err: errors.New("upstream 500")}
	d.Executor = ex2
	dg2, err := d.Start("u1", nil, "schedule")
	if err != nil {
		t.Fatal(err)
	}
	got = waitDigest(t, s, "u1", dg2.ID)
	if got.State != DigestFailed || !strings.Contains(got.Error, "upstream 500") {
		t.Fatalf("failed digest %+v", got)
	}
	if fresh, _ := s.Items(ItemQuery{Owner: "u1", State: StateNew}); len(fresh) != 2 {
		t.Fatalf("items after failure: %d new", len(fresh))
	}
	// Empty text counts as a failure.
	d.Executor = &fakeExec{name: "chat", text: "  "}
	dg3, _ := d.Start("u1", nil, "api")
	if got = waitDigest(t, s, "u1", dg3.ID); got.State != DigestFailed {
		t.Fatalf("empty text: %+v", got)
	}
	// Another owner's digest is not visible.
	if _, err := s.DigestByID("u2", dg.ID); err == nil {
		t.Fatal("digest visible to another owner")
	}
}

func TestDigestSchedule(t *testing.T) {
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	d := &Digester{Cfg: DigestConfig{Schedule: []string{"08:00", "20:00", "bogus"}, Location: tokyo}}
	at := func(s string) time.Time { t, _ := time.ParseInLocation("2006-01-02 15:04", s, tokyo); return t }
	cases := []struct {
		now, want string
		ok        bool
	}{
		{"2026-10-20 08:00", "2026-10-20 08:00", true},
		{"2026-10-20 08:59", "2026-10-20 08:00", true},
		{"2026-10-20 09:00", "", false},
		{"2026-10-20 07:59", "", false},
		{"2026-10-21 00:30", "", false},
		{"2026-10-20 20:10", "2026-10-20 20:00", true},
	}
	for _, c := range cases {
		got, ok := d.DueSlot(at(c.now))
		if ok != c.ok || (ok && !got.Equal(at(c.want))) {
			t.Errorf("%s: %v %v", c.now, got, ok)
		}
	}
	// Midnight slot seen just after midnight from the day before's perspective.
	d.Cfg.Schedule = []string{"23:50"}
	if got, ok := d.DueSlot(at("2026-10-21 00:20")); !ok || !got.Equal(at("2026-10-20 23:50")) {
		t.Errorf("across midnight: %v %v", got, ok)
	}

	s := testStore(t)
	seedItems(t, s, "u1", "a", 2, 10)
	ex := &fakeExec{name: "chat", text: "digest"}
	d = &Digester{Store: s, Executor: ex, Cfg: DigestConfig{Schedule: []string{"08:00"}, Location: tokyo}}
	d.Tick(at("2026-10-20 07:00"))
	if len(ex.calls) != 0 {
		t.Fatal("ran before the slot")
	}
	// The slot is due relative to the real clock; digest rows carry real created_at,
	// so use a slot an hour-window around now.
	now := time.Now().In(tokyo)
	d.Cfg.Schedule = []string{now.Add(-time.Minute).Format("15:04")}
	d.Tick(now)
	d.Stop()
	if len(ex.calls) != 1 {
		t.Fatalf("scheduled runs %d", len(ex.calls))
	}
	seedItems(t, s, "u1", "b", 1, 10)
	d.Tick(now.Add(time.Minute)) // a digest exists since the slot
	d.Stop()
	if len(ex.calls) != 1 {
		t.Fatal("ran twice for one slot")
	}
}

func TestChatTools(t *testing.T) {
	s := testStore(t)
	seedItems(t, s, "u1", "x-ai", 3, 10)
	seedItems(t, s, "u2", "secret", 1, 10)
	ct := ChatTools{Store: s, Owner: "u1"}
	if len(ct.Defs()) != 5 {
		t.Fatal("defs")
	}
	run := func(name, in string) (string, error) {
		r, err := ct.Run(context.Background(), name, json.RawMessage(in))
		return r.Text, err
	}
	out, err := run(ToolSearchInbox, `{"query":"title 1"}`)
	if err != nil || !strings.Contains(out, `"title":"title 1"`) || strings.Contains(out, "secret") {
		t.Fatalf("search: %s %v", out, err)
	}
	if _, err = run(ToolSearchInbox, `{"account":"secret"}`); err == nil {
		t.Fatal("searched another owner's account")
	}
	var r struct{ Items []struct{ ID int64 } }
	out, _ = run(ToolSearchInbox, `{"limit":500}`)
	json.Unmarshal([]byte(out), &r)
	if len(r.Items) != 3 {
		t.Fatalf("items %s", out)
	}
	if out, err = run(ToolGetItem, fmt.Sprintf(`{"id":%d}`, r.Items[0].ID)); err != nil || !strings.Contains(out, "third-party") {
		t.Fatalf("get_item: %s %v", out, err)
	}
	theirs, _ := s.Items(ItemQuery{Owner: "u2", State: "all"})
	if _, err = run(ToolGetItem, fmt.Sprintf(`{"id":%d}`, theirs[0].ID)); err == nil {
		t.Fatal("read another owner's item")
	}
	if out, err = run(ToolCreateTask, `{"title":"Buy milk","body":"2 l"}`); err != nil || !strings.Contains(out, "created") {
		t.Fatalf("create: %s %v", out, err)
	}
	ts, _ := s.Tasks("u1", "", 10)
	if len(ts) != 1 || ts[0].State != TaskOpen {
		t.Fatalf("tasks %+v", ts)
	}
	if _, err = run(ToolUpdateTask, fmt.Sprintf(`{"id":%d,"state":"done","due":"2026-11-01"}`, ts[0].ID)); err != nil {
		t.Fatal(err)
	}
	if _, err = run(ToolUpdateTask, fmt.Sprintf(`{"id":%d,"state":"bogus"}`, ts[0].ID)); err == nil {
		t.Fatal("bad state")
	}
	(&ChatTools{Store: s, Owner: "u2"}).Run(context.Background(), ToolCreateTask, json.RawMessage(`{"title":"theirs"}`))
	if _, err = (ChatTools{Store: s, Owner: "u2"}).Run(context.Background(), ToolUpdateTask, json.RawMessage(fmt.Sprintf(`{"id":%d,"state":"open"}`, ts[0].ID))); err == nil {
		t.Fatal("updated another owner's task")
	}
	out, _ = run(ToolListTasks, `{"state":"done"}`)
	if !strings.Contains(out, "Buy milk") || !strings.Contains(out, "2026-11-01") || strings.Contains(out, "theirs") {
		t.Fatalf("list: %s", out)
	}
}
