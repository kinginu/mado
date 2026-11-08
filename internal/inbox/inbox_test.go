package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE users(sub TEXT PRIMARY KEY, email TEXT, name TEXT, created_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil { // idempotent
		t.Fatal(err)
	}
	return &Store{DB: db}
}

// fakeKind is a test kind whose collectors are scripted per account name.
type fakeKind struct{}

type fakeScript struct {
	mu     sync.Mutex
	polls  int
	fail   error
	pages  [][]Item
	block  chan struct{} // when set, Poll waits on it
	cursor []string
}

var scripts sync.Map // account name -> *fakeScript

func (fakeKind) Name() string { return "fake" }
func (fakeKind) Validate(c json.RawMessage, keyRef string, keys map[string]string) error {
	var v struct {
		AllowPrivate bool `json:"allow_private"`
	}
	return DecodeStrict(c, &v)
}
func (fakeKind) New(a Account, key string, env Env) (Collector, error) {
	v, _ := scripts.LoadOrStore(a.Name, &fakeScript{})
	return v.(*fakeScript), nil
}

func (f *fakeScript) Poll(ctx context.Context, cursor json.RawMessage) ([]Item, json.RawMessage, bool, error) {
	f.mu.Lock()
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, nil, false, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	f.cursor = append(f.cursor, string(cursor))
	if f.fail != nil {
		return nil, nil, false, f.fail
	}
	if len(f.pages) == 0 {
		return nil, nil, false, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	next, _ := json.Marshal(map[string]int{"page": f.polls})
	return page, next, len(f.pages) > 0, nil
}

type passiveKind struct{}

func (passiveKind) Name() string                                              { return "passive" }
func (passiveKind) Validate(json.RawMessage, string, map[string]string) error { return nil }
func (passiveKind) New(Account, string, Env) (Collector, error)               { return nil, ErrPassive }

func init() {
	Register(fakeKind{})
	Register(passiveKind{})
}

func addAccount(t *testing.T, s *Store, name, owner, kind string) *Account {
	t.Helper()
	a := &Account{Name: name, Kind: kind, OwnerSub: owner, Enabled: true, IntervalS: 60}
	if err := Validate(a, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAccount(a); err != nil {
		t.Fatal(err)
	}
	return a
}

func item(id, title, text string, ts time.Time) Item {
	return Item{ExtID: id, Kind: "feed", Author: "alice", Title: title, Text: text, URL: "https://example.org/" + id, TS: ts,
		Raw: json.RawMessage(`{"id":"` + id + `"}`)}
}

func TestFTS5Available(t *testing.T) {
	s := testStore(t)
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='inbox_fts' AND sql LIKE '%fts5%'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("fts5 table: %d %v", n, err)
	}
}

func TestInsertBatchDedupeSearchRetention(t *testing.T) {
	s := testStore(t)
	a := addAccount(t, s, "x-ai", "u1", "fake")
	b := addAccount(t, s, "other", "u2", "fake")
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	items := []Item{
		item("1", "Kumamoto fab", "半導体の新工場が稼働", t0),
		item("2", "", "HBM4 sampling begins", t0.Add(time.Hour)),
		{ExtID: "3", Author: "bob", Text: strings.Repeat("é", 50000), URL: "javascript:alert(1)", Raw: json.RawMessage(`not json`)},
	}
	n, err := s.InsertBatch(a.ID, items, json.RawMessage(`{"c":1}`), 1000)
	if err != nil || n != 3 {
		t.Fatalf("insert: %d %v", n, err)
	}
	// Same ext ids again: nothing new, cursor still saved.
	n, err = s.InsertBatch(a.ID, items[:2], json.RawMessage(`{"c":2}`), 1000)
	if err != nil || n != 0 {
		t.Fatalf("dedupe: %d %v", n, err)
	}
	if acc, _ := s.Account(a.ID); string(acc.Cursor) != `{"c":2}` {
		t.Fatalf("cursor %s", acc.Cursor)
	}
	s.InsertBatch(b.ID, []Item{item("1", "Kumamoto fab", "someone else's", t0)}, nil, 0)

	all, _ := s.Items(ItemQuery{Owner: "u1", State: "all"})
	if len(all) != 3 {
		t.Fatalf("owner items %d", len(all))
	}
	third := all[0] // no ts: stored as now, so newest
	if len(third.Text) > 1000 || third.URL != "" || !strings.HasPrefix(third.Text, "é") {
		t.Fatalf("normalized: len %d url %q", len(third.Text), third.URL)
	}
	var raw string
	s.DB.QueryRow(`SELECT raw FROM inbox_items WHERE id=?`, third.ID).Scan(&raw)
	if raw != "{}" {
		t.Fatalf("invalid raw kept: %s", raw)
	}

	search := func(owner, q string) []string {
		rows, err := s.Items(ItemQuery{Owner: owner, State: "all", Q: q})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.ExtID)
		}
		return out
	}
	if got := search("u1", "kumamoto"); len(got) != 1 || got[0] != "1" {
		t.Errorf("word: %v", got)
	}
	if got := search("u1", "新工場"); len(got) != 1 || got[0] != "1" {
		t.Errorf("japanese: %v", got)
	}
	if got := search("u1", "bob"); len(got) != 1 || got[0] != "3" {
		t.Errorf("author: %v", got)
	}
	if got := search("u1", "工場 HB"); len(got) != 0 {
		t.Errorf("AND across items: %v", got)
	}
	if got := search("u1", `"); DROP TABLE inbox_items; --`); len(got) != 0 {
		t.Errorf("injection: %v", got)
	}
	if got := search("u2", "kumamoto"); len(got) != 1 {
		t.Errorf("owner scope: %v", got)
	}
	// Update keeps the index in step.
	s.DB.Exec(`UPDATE inbox_items SET title='renamed' WHERE ext_id='1' AND account_id=?`, a.ID)
	if got := search("u1", "kumamoto"); len(got) != 0 {
		t.Errorf("stale index after update: %v", got)
	}
	if got := search("u1", "renamed"); len(got) != 1 {
		t.Errorf("update not indexed: %v", got)
	}

	// States: other owners' ids are ignored.
	ids := []int64{all[0].ID, all[1].ID}
	if n, _ := s.SetItemState("u2", ids, StateRead); n != 0 {
		t.Fatal("changed another owner's items")
	}
	if n, _ := s.SetItemState("u1", ids, StateArchived); n != 2 {
		t.Fatal("state not set")
	}
	if _, err := s.SetItemState("u1", ids, "bogus"); err == nil {
		t.Fatal("bad state accepted")
	}
	// Retention: read/archived older than the cutoff go, new stays.
	s.DB.Exec(`UPDATE inbox_items SET ts='2026-01-01T00:00:00Z'`)
	n, _ = s.Retention(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if n != 2 {
		t.Fatalf("retention deleted %d", n)
	}
	if left, _ := s.Items(ItemQuery{Owner: "u1", State: "all"}); len(left) != 1 || left[0].State != StateNew {
		t.Fatalf("left %v", left)
	}
	var fts int
	s.DB.QueryRow(`SELECT COUNT(*) FROM inbox_fts WHERE inbox_fts MATCH '"kumamoto"'`).Scan(&fts)
	if fts != 1 { // only u2's item remains
		t.Fatalf("fts rows after delete: %d", fts)
	}
	// Deleting an account deletes its items.
	s.DeleteAccount(b.ID)
	var cnt int
	s.DB.QueryRow(`SELECT COUNT(*) FROM inbox_items WHERE account_id=?`, b.ID).Scan(&cnt)
	if cnt != 0 {
		t.Fatal("items of a deleted account kept")
	}
}

func TestItemsPagingAndFilters(t *testing.T) {
	s := testStore(t)
	a := addAccount(t, s, "a", "u1", "fake")
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var its []Item
	for i := 0; i < 7; i++ {
		its = append(its, item(fmt.Sprint(i), "t", "x", t0.Add(time.Duration(i%3)*time.Minute)))
	}
	s.InsertBatch(a.ID, its, nil, 0)
	var seen []int64
	var before int64
	for {
		page, _ := s.Items(ItemQuery{Owner: "u1", State: "all", Limit: 3, Before: before})
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			seen = append(seen, r.ID)
		}
		before = page[len(page)-1].ID
	}
	if len(seen) != 7 {
		t.Fatalf("paged %v", seen)
	}
	dup := map[int64]bool{}
	for _, id := range seen {
		if dup[id] {
			t.Fatalf("duplicate in pages %v", seen)
		}
		dup[id] = true
	}
}

func TestSeedConfigAndResolveOwners(t *testing.T) {
	s := testStore(t)
	cfg := []Account{{Name: "x-ai", Kind: "fake", OwnerSub: "Me@Example.org", Enabled: true, IntervalS: 900}}
	if err := s.SeedConfig(cfg); err != nil {
		t.Fatal(err)
	}
	id := ConfigID("Me@Example.org", "x-ai")
	s.InsertBatch(id, []Item{item("1", "a", "b", time.Now())}, json.RawMessage(`{"k":1}`), 0)
	if n := s.ResolveOwners(); n != 0 {
		t.Fatal("resolved without a user")
	}
	s.DB.Exec(`INSERT INTO users(sub,email,created_at) VALUES('sub-1','me@example.org','2026')`)
	if n := s.ResolveOwners(); n != 1 {
		t.Fatalf("resolved %d", n)
	}
	if rows, _ := s.Items(ItemQuery{Owner: "sub-1", State: "all"}); len(rows) != 1 {
		t.Fatal("items not visible to the resolved owner")
	}
	// Re-seed keeps the cursor and the resolved owner.
	cfg[0].IntervalS = 120
	s.SeedConfig(cfg)
	a, _ := s.Account(id)
	if a.OwnerSub != "sub-1" || string(a.Cursor) != `{"k":1}` || a.IntervalS != 120 || a.Source != SourceConfig {
		t.Fatalf("after reseed %+v cursor %s", a, a.Cursor)
	}
	// Dropped from config: a disabled UI row.
	s.SeedConfig(nil)
	a, _ = s.Account(id)
	if a.Source != SourceUI || a.Enabled {
		t.Fatalf("dropped row %+v", a)
	}
}

func newSched(s *Store) *Scheduler {
	return &Scheduler{Store: s, FirstDelay: func() time.Duration { return 10 * time.Millisecond }}
}

func script(name string) *fakeScript {
	f := &fakeScript{}
	scripts.Store(name, f)
	return f
}

func TestSchedulerPagesCursorAndBackoff(t *testing.T) {
	s := testStore(t)
	a := addAccount(t, s, "paged", "u1", "fake")
	sc := script("paged")
	t0 := time.Now().Add(-time.Hour)
	sc.pages = [][]Item{{item("1", "a", "", t0)}, {item("2", "b", "", t0)}, {item("2", "b", "", t0), item("3", "c", "", t0)}}
	sch := newSched(s)
	n, err := sch.PollNow(context.Background(), a.ID)
	if err != nil || n != 3 || sc.polls != 3 {
		t.Fatalf("poll: %d %v polls %d", n, err, sc.polls)
	}
	// Each page saw the cursor stored after the previous one.
	if sc.cursor[1] != `{"page":1}` || sc.cursor[2] != `{"page":2}` {
		t.Fatalf("cursors %v", sc.cursor)
	}
	acc, _ := s.Account(a.ID)
	if string(acc.Cursor) != `{"page":3}` || acc.LastOK == "" || acc.LastError != "" || acc.BackoffS != 0 {
		t.Fatalf("account after ok %+v", acc)
	}
	// Failures double the backoff up to an hour; success resets it.
	sc.fail = errors.New("boom")
	want := []int{60, 120, 240}
	for _, w := range want {
		sch.PollNow(context.Background(), a.ID)
		acc, _ = s.Account(a.ID)
		if acc.BackoffS != w || acc.LastError != "boom" {
			t.Fatalf("backoff %d want %d (%q)", acc.BackoffS, w, acc.LastError)
		}
	}
	s.DB.Exec(`UPDATE inbox_accounts SET backoff_s=3000 WHERE id=?`, a.ID)
	sch.PollNow(context.Background(), a.ID)
	if acc, _ = s.Account(a.ID); acc.BackoffS != 3600 {
		t.Fatalf("cap %d", acc.BackoffS)
	}
	sc.fail = nil
	sch.PollNow(context.Background(), a.ID)
	if acc, _ = s.Account(a.ID); acc.BackoffS != 0 || acc.LastError != "" {
		t.Fatalf("reset %+v", acc)
	}
	p := addAccount(t, s, "pas", "u1", "passive")
	if _, err := sch.PollNow(context.Background(), p.ID); !errors.Is(err, ErrPassive) {
		t.Fatalf("passive poll: %v", err)
	}
}

func TestSchedulerFailedBatchKeepsCursor(t *testing.T) {
	s := testStore(t)
	a := addAccount(t, s, "crashy", "u1", "fake")
	// A cursor that is not JSON fails the transaction: neither items nor cursor stored.
	if _, err := s.InsertBatch(a.ID, []Item{item("1", "a", "", time.Now())}, json.RawMessage(`{bad`), 0); err == nil {
		t.Fatal("bad cursor stored")
	}
	if rows, _ := s.Items(ItemQuery{Owner: "u1", State: "all"}); len(rows) != 0 {
		t.Fatal("items stored without their cursor")
	}
}

func TestSchedulerLoopsJitterAndExclusivity(t *testing.T) {
	s := testStore(t)
	a := addAccount(t, s, "looped", "u1", "fake")
	addAccount(t, s, "passive-one", "u1", "passive")
	off := addAccount(t, s, "off", "u1", "fake")
	off.Enabled = false
	s.UpdateAccount(off)
	sc := script("looped")
	block := make(chan struct{})
	sc.mu.Lock()
	sc.block = block
	sc.mu.Unlock()
	var delays atomic.Int32
	sch := &Scheduler{Store: s, FirstDelay: func() time.Duration { delays.Add(1); return 5 * time.Millisecond }}
	sch.Start()
	defer sch.Stop()
	if got := len(sch.loops); got != 1 {
		t.Fatalf("loops %d (passive and disabled accounts must not run)", got)
	}
	// The loop's poll is blocked: Poll now is refused meanwhile.
	deadline := time.Now().Add(2 * time.Second)
	for {
		sch.mu.Lock()
		busy := sch.polling[a.ID]
		sch.mu.Unlock()
		if busy || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := sch.PollNow(context.Background(), a.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent poll: %v", err)
	}
	close(block)
	// An edit restarts the loop (a new jitter).
	a.IntervalS = 120
	s.UpdateAccount(a)
	sch.Sync()
	for i := 0; i < 200 && delays.Load() < 2; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if delays.Load() < 2 {
		t.Fatalf("loop not restarted: %d delays", delays.Load())
	}
	// Disable: the loop stops.
	a.Enabled = false
	s.UpdateAccount(a)
	sch.Sync()
	if len(sch.loops) != 0 {
		t.Fatal("disabled account still looping")
	}
}

func TestDefaultJitterRange(t *testing.T) {
	sch := &Scheduler{}
	for i := 0; i < 200; i++ {
		d := sch.firstDelay()
		if d < 5*time.Second || d >= 30*time.Second {
			t.Fatalf("jitter %v", d)
		}
	}
}
