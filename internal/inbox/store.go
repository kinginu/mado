package inbox

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Schema is the inbox's part of the hub database (DESIGN §23.2). The full-text index
// is an external-content FTS5 table kept in step by triggers; the trigram tokenizer
// matches substrings, so Japanese text without spaces is searchable (3+ characters).
const Schema = `
CREATE TABLE IF NOT EXISTS inbox_accounts(
  id TEXT PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL, owner_sub TEXT NOT NULL, config TEXT NOT NULL,
  key_ref TEXT DEFAULT '', enabled INTEGER DEFAULT 1, interval_s INTEGER DEFAULT 900, source TEXT DEFAULT 'ui',
  cursor TEXT DEFAULT '{}', last_run TEXT, last_ok TEXT, last_error TEXT DEFAULT '', backoff_s INTEGER DEFAULT 0,
  created_at TEXT, UNIQUE(owner_sub, name));
CREATE TABLE IF NOT EXISTS inbox_items(
  id INTEGER PRIMARY KEY, account_id TEXT NOT NULL, ext_id TEXT NOT NULL, ts TEXT NOT NULL,
  author TEXT DEFAULT '', title TEXT DEFAULT '', text TEXT DEFAULT '', url TEXT DEFAULT '', thread_key TEXT DEFAULT '',
  kind TEXT DEFAULT '', raw TEXT NOT NULL, state TEXT DEFAULT 'new', digest_id INTEGER, created_at TEXT,
  UNIQUE(account_id, ext_id));
CREATE INDEX IF NOT EXISTS inbox_items_acct ON inbox_items(account_id, state, ts);
CREATE INDEX IF NOT EXISTS inbox_items_ts ON inbox_items(ts);
CREATE VIRTUAL TABLE IF NOT EXISTS inbox_fts USING fts5(title, text, author, content='inbox_items', content_rowid='id',
  tokenize='trigram');
CREATE TRIGGER IF NOT EXISTS inbox_items_ai AFTER INSERT ON inbox_items BEGIN
  INSERT INTO inbox_fts(rowid, title, text, author) VALUES (new.id, new.title, new.text, new.author);
END;
CREATE TRIGGER IF NOT EXISTS inbox_items_ad AFTER DELETE ON inbox_items BEGIN
  INSERT INTO inbox_fts(inbox_fts, rowid, title, text, author) VALUES ('delete', old.id, old.title, old.text, old.author);
END;
CREATE TRIGGER IF NOT EXISTS inbox_items_au AFTER UPDATE OF title, text, author ON inbox_items BEGIN
  INSERT INTO inbox_fts(inbox_fts, rowid, title, text, author) VALUES ('delete', old.id, old.title, old.text, old.author);
  INSERT INTO inbox_fts(rowid, title, text, author) VALUES (new.id, new.title, new.text, new.author);
END;
CREATE TABLE IF NOT EXISTS inbox_digests(
  id INTEGER PRIMARY KEY, owner_sub TEXT NOT NULL, created_at TEXT, since TEXT, until TEXT, account_ids TEXT NOT NULL,
  executor TEXT NOT NULL, model TEXT DEFAULT '', session_id TEXT DEFAULT '', text TEXT DEFAULT '',
  item_count INTEGER DEFAULT 0, cost REAL DEFAULT 0, state TEXT NOT NULL, error TEXT DEFAULT '');
CREATE INDEX IF NOT EXISTS inbox_digests_owner ON inbox_digests(owner_sub, created_at);
CREATE TABLE IF NOT EXISTS inbox_tasks(
  id INTEGER PRIMARY KEY, owner_sub TEXT NOT NULL, title TEXT NOT NULL, body TEXT DEFAULT '', state TEXT NOT NULL,
  source TEXT NOT NULL, digest_id INTEGER, item_ids TEXT DEFAULT '[]', due TEXT, created_at TEXT, updated_at TEXT);
CREATE INDEX IF NOT EXISTS inbox_tasks_owner ON inbox_tasks(owner_sub, state);
`

// Migrate creates the inbox tables in the hub database.
func Migrate(db *sql.DB) error {
	_, err := db.Exec(Schema)
	return err
}

// Store is the inbox's view of the hub database.
type Store struct {
	DB  *sql.DB
	Now func() time.Time
}

// ErrNotFound: no such row for this owner.
var ErrNotFound = errors.New("not found")

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) stamp() string { return s.now().UTC().Format(time.RFC3339Nano) }

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ConfigID is the stable id of an account seeded from hub.toml: the same owner (as
// written) and name give the same row across restarts.
func ConfigID(owner, name string) string {
	h := sha256.Sum256([]byte(owner + "\x00" + name))
	return "cfg_" + hex.EncodeToString(h[:10])
}

// --- accounts

const accountCols = `id, name, kind, owner_sub, config, COALESCE(key_ref,''), COALESCE(enabled,1), COALESCE(interval_s,900),
	COALESCE(source,'ui'), COALESCE(cursor,'{}'), COALESCE(last_run,''), COALESCE(last_ok,''), COALESCE(last_error,''),
	COALESCE(backoff_s,0), COALESCE(created_at,'')`

func scanAccount(sc interface{ Scan(...any) error }) (*Account, error) {
	var a Account
	var cfg, cur string
	err := sc.Scan(&a.ID, &a.Name, &a.Kind, &a.OwnerSub, &cfg, &a.KeyRef, &a.Enabled, &a.IntervalS, &a.Source, &cur,
		&a.LastRun, &a.LastOK, &a.LastError, &a.BackoffS, &a.CreatedAt)
	a.Config, a.Cursor = json.RawMessage(cfg), json.RawMessage(cur)
	return &a, err
}

// Account returns one account.
func (s *Store) Account(id string) (*Account, error) {
	a, err := scanAccount(s.DB.QueryRow(`SELECT `+accountCols+` FROM inbox_accounts WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// Accounts lists the accounts of one owner, every account when owner is "".
func (s *Store) Accounts(owner string) ([]*Account, error) {
	q, args := `SELECT `+accountCols+` FROM inbox_accounts`, []any{}
	if owner != "" {
		q, args = q+` WHERE owner_sub=?`, append(args, owner)
	}
	return s.accounts(q+` ORDER BY owner_sub, name`, args...)
}

// AccountsNamed lists the accounts with a name (any owner).
func (s *Store) AccountsNamed(name string) ([]*Account, error) {
	return s.accounts(`SELECT `+accountCols+` FROM inbox_accounts WHERE name=?`, name)
}

func (s *Store) accounts(q string, args ...any) ([]*Account, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateAccount inserts a new account (id assigned when empty).
func (s *Store) CreateAccount(a *Account) error {
	if a.ID == "" {
		a.ID = randomID()
	}
	if a.Source == "" {
		a.Source = SourceUI
	}
	if len(a.Config) == 0 {
		a.Config = json.RawMessage(`{}`)
	}
	a.CreatedAt = s.stamp()
	_, err := s.DB.Exec(`INSERT INTO inbox_accounts(id,name,kind,owner_sub,config,key_ref,enabled,interval_s,source,cursor,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,'{}',?)`, a.ID, a.Name, a.Kind, a.OwnerSub, string(a.Config), a.KeyRef, a.Enabled, a.IntervalS,
		a.Source, a.CreatedAt)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("an account named %s already exists for this owner", a.Name)
	}
	return err
}

// UpdateAccount saves name, config, key_ref, enabled and interval. A changed config
// keeps the cursor: the kind decides what an old cursor means.
func (s *Store) UpdateAccount(a *Account) error {
	_, err := s.DB.Exec(`UPDATE inbox_accounts SET name=?, config=?, key_ref=?, enabled=?, interval_s=? WHERE id=?`,
		a.Name, string(a.Config), a.KeyRef, a.Enabled, a.IntervalS, a.ID)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("an account named %s already exists for this owner", a.Name)
	}
	return err
}

// DeleteAccount removes an account with its items.
func (s *Store) DeleteAccount(id string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM inbox_items WHERE account_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM inbox_accounts WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SeedConfig writes the [[inbox.accounts]] of hub.toml (source config). Rows keep
// their cursor and run state; the owner is only replaced while it is still
// unresolved. Config rows no longer in hub.toml become disabled UI rows, so an admin
// can delete them with their items.
func (s *Store) SeedConfig(as []Account) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keep := map[string]bool{}
	for _, a := range as {
		a.ID = ConfigID(a.OwnerSub, a.Name)
		keep[a.ID] = true
		if len(a.Config) == 0 {
			a.Config = json.RawMessage(`{}`)
		}
		if _, err := tx.Exec(`INSERT INTO inbox_accounts(id,name,kind,owner_sub,config,key_ref,enabled,interval_s,source,cursor,created_at)
			VALUES(?,?,?,?,?,?,?,?,'config','{}',?)
			ON CONFLICT(id) DO UPDATE SET name=excluded.name, kind=excluded.kind, config=excluded.config,
			key_ref=excluded.key_ref, enabled=excluded.enabled, interval_s=excluded.interval_s, source='config'`,
			a.ID, a.Name, a.Kind, a.OwnerSub, string(a.Config), a.KeyRef, a.Enabled, a.IntervalS, s.stamp()); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("[[inbox.accounts]] %s: an account of that name already exists for this owner", a.Name)
			}
			return err
		}
	}
	rows, err := tx.Query(`SELECT id FROM inbox_accounts WHERE source='config'`)
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && !keep[id] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	for _, id := range gone {
		if _, err := tx.Exec(`UPDATE inbox_accounts SET source='ui', enabled=0 WHERE id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ResolveOwners replaces an owner written as an e-mail address by the sub of the
// user with that address, once that user exists (DESIGN §23.9). It returns the
// number of accounts resolved.
func (s *Store) ResolveOwners() int {
	res, err := s.DB.Exec(`UPDATE inbox_accounts SET owner_sub=(SELECT u.sub FROM users u
		WHERE lower(u.email)=lower(inbox_accounts.owner_sub) ORDER BY u.created_at LIMIT 1)
		WHERE instr(owner_sub,'@')>0 AND NOT EXISTS (SELECT 1 FROM users v WHERE v.sub=inbox_accounts.owner_sub)
		AND EXISTS (SELECT 1 FROM users w WHERE lower(w.email)=lower(inbox_accounts.owner_sub))`)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// SaveRun records the outcome of a poll.
func (s *Store) SaveRun(id string, pollErr string, backoff int) error {
	t := s.stamp()
	if pollErr == "" {
		_, err := s.DB.Exec(`UPDATE inbox_accounts SET last_run=?, last_ok=?, last_error='', backoff_s=0 WHERE id=?`, t, t, id)
		return err
	}
	_, err := s.DB.Exec(`UPDATE inbox_accounts SET last_run=?, last_error=?, backoff_s=? WHERE id=?`, t, pollErr, backoff, id)
	return err
}

// --- items

// ItemRow is a stored item with its account.
type ItemRow struct {
	ID          int64  `json:"id"`
	AccountID   string `json:"account_id"`
	AccountName string `json:"account"`
	AccountKind string `json:"account_kind"`
	ExtID       string `json:"-"`
	TS          string `json:"ts"`
	Author      string `json:"author"`
	Title       string `json:"title"`
	Text        string `json:"text"`
	URL         string `json:"url"`
	ThreadKey   string `json:"thread_key,omitempty"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	DigestID    int64  `json:"digest_id,omitempty"`
	CreatedAt   string `json:"created_at"`
	Truncated   bool   `json:"text_truncated,omitempty"`
}

// ClipUTF8 cuts s to at most n bytes on a rune boundary.
func ClipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func cleanText(s string, n int) string {
	return ClipUTF8(strings.ToValidUTF8(s, "�"), n)
}

// SafeURL keeps http(s) URLs only, so the UI never links anything else.
func SafeURL(s string) string {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(s) > 2048 {
		return ""
	}
	return s
}

// normalize applies the stored form's limits to one item.
func normalize(it Item, maxText int, now time.Time) (Item, string) {
	it.ExtID = strings.ToValidUTF8(it.ExtID, "?")
	if it.ExtID == "" {
		h := sha256.Sum256(append([]byte(it.Title+"\x00"+it.Text+"\x00"), it.Raw...))
		it.ExtID = "sha256:" + hex.EncodeToString(h[:])
	} else if len(it.ExtID) > 512 {
		h := sha256.Sum256([]byte(it.ExtID))
		it.ExtID = "sha256:" + hex.EncodeToString(h[:])
	}
	it.Kind = cleanText(it.Kind, 64)
	it.Author = cleanText(it.Author, 256)
	it.Title = cleanText(it.Title, 1024)
	it.Text = cleanText(it.Text, maxText)
	it.ThreadKey = cleanText(it.ThreadKey, 512)
	it.URL = SafeURL(it.URL)
	if it.TS.IsZero() {
		it.TS = now
	}
	raw := string(it.Raw)
	if raw == "" || !json.Valid(it.Raw) {
		raw = "{}"
	}
	if len(raw) > MaxRawBytes {
		b, _ := json.Marshal(map[string]any{"truncated": true, "raw_prefix": ClipUTF8(raw, MaxRawBytes-1024)})
		raw = string(b)
	}
	return it, raw
}

// InsertBatch stores a poll's items (duplicates ignored) and the account's next
// cursor in one transaction, so a crash re-fetches rather than loses. It returns the
// number of new items.
func (s *Store) InsertBatch(accountID string, items []Item, cursor json.RawMessage, maxText int) (int, error) {
	if maxText <= 0 {
		maxText = DefaultMaxItem
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := s.now()
	stamp := now.UTC().Format(time.RFC3339Nano)
	added := 0
	for _, it := range items {
		it, raw := normalize(it, maxText, now)
		res, err := tx.Exec(`INSERT OR IGNORE INTO inbox_items(account_id,ext_id,ts,author,title,text,url,thread_key,kind,raw,state,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,'new',?)`, accountID, it.ExtID, it.TS.UTC().Format(time.RFC3339), it.Author, it.Title,
			it.Text, it.URL, it.ThreadKey, it.Kind, raw, stamp)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	if cursor != nil {
		if !json.Valid(cursor) {
			return 0, errors.New("the collector returned a cursor that is not JSON")
		}
		if _, err := tx.Exec(`UPDATE inbox_accounts SET cursor=? WHERE id=?`, string(cursor), accountID); err != nil {
			return 0, err
		}
	}
	return added, tx.Commit()
}

const itemCols = `i.id, i.account_id, a.name, a.kind, i.ext_id, i.ts, COALESCE(i.author,''), COALESCE(i.title,''),
	COALESCE(i.text,''), COALESCE(i.url,''), COALESCE(i.thread_key,''), COALESCE(i.kind,''), COALESCE(i.state,'new'),
	COALESCE(i.digest_id,0), COALESCE(i.created_at,'')`

func scanItem(sc interface{ Scan(...any) error }) (*ItemRow, error) {
	var r ItemRow
	err := sc.Scan(&r.ID, &r.AccountID, &r.AccountName, &r.AccountKind, &r.ExtID, &r.TS, &r.Author, &r.Title, &r.Text,
		&r.URL, &r.ThreadKey, &r.Kind, &r.State, &r.DigestID, &r.CreatedAt)
	return &r, err
}

// ItemQuery filters the owner's items. Before is an item id: rows older than it.
type ItemQuery struct {
	Owner, Account, State, Kind, Q, Since string
	Before                                int64
	Limit                                 int
	Oldest                                bool // oldest first (digest input)
	Accounts                              []string
}

// Items lists the owner's items, newest first (by the item's own time).
func (s *Store) Items(q ItemQuery) ([]*ItemRow, error) {
	if q.Owner == "" {
		return nil, ErrNotFound
	}
	where := []string{"a.owner_sub=?"}
	args := []any{q.Owner}
	if q.Account != "" {
		where, args = append(where, "i.account_id=?"), append(args, q.Account)
	}
	if len(q.Accounts) > 0 {
		where = append(where, "i.account_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(q.Accounts)), ",")+")")
		for _, a := range q.Accounts {
			args = append(args, a)
		}
	}
	if q.State != "" && q.State != "all" {
		where, args = append(where, "i.state=?"), append(args, q.State)
	}
	if q.Kind != "" {
		where, args = append(where, "i.kind=?"), append(args, q.Kind)
	}
	if q.Since != "" {
		where, args = append(where, "i.ts>=?"), append(args, q.Since)
	}
	if q.Before > 0 {
		where = append(where, "(i.ts < (SELECT ts FROM inbox_items WHERE id=?) OR (i.ts = (SELECT ts FROM inbox_items WHERE id=?) AND i.id < ?))")
		args = append(args, q.Before, q.Before, q.Before)
	}
	if strings.TrimSpace(q.Q) != "" {
		w, a := matchClause(q.Q)
		where, args = append(where, w), append(args, a...)
	}
	order := "i.ts DESC, i.id DESC"
	if q.Oldest {
		order = "i.ts ASC, i.id ASC"
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit)
	rows, err := s.DB.Query(`SELECT `+itemCols+` FROM inbox_items i JOIN inbox_accounts a ON a.id=i.account_id
		WHERE `+strings.Join(where, " AND ")+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ItemRow
	for rows.Next() {
		r, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// matchClause turns a search box into SQL: each word must match title, text or
// author. Words of three or more characters go through the FTS index (as quoted
// phrases, so no FTS syntax reaches it); shorter ones, which the trigram tokenizer
// cannot match, fall back to LIKE.
func matchClause(q string) (string, []any) {
	var fts []string
	var parts []string
	var args []any
	for _, w := range strings.Fields(q) {
		if len(w) > 200 {
			w = ClipUTF8(w, 200)
		}
		if utf8.RuneCountInString(w) >= 3 {
			fts = append(fts, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
			continue
		}
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(w) + "%"
		parts = append(parts, `(i.title LIKE ? ESCAPE '\' OR i.text LIKE ? ESCAPE '\' OR i.author LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	if len(fts) > 0 {
		parts = append([]string{"i.id IN (SELECT rowid FROM inbox_fts WHERE inbox_fts MATCH ?)"}, parts...)
		args = append([]any{strings.Join(fts, " ")}, args...)
	}
	if len(parts) == 0 {
		return "1", nil
	}
	return strings.Join(parts, " AND "), args
}

// Item returns one of the owner's items.
func (s *Store) Item(owner string, id int64) (*ItemRow, error) {
	r, err := scanItem(s.DB.QueryRow(`SELECT `+itemCols+` FROM inbox_items i JOIN inbox_accounts a ON a.id=i.account_id
		WHERE i.id=? AND a.owner_sub=?`, id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ValidItemState: new, read or archived.
func ValidItemState(s string) bool { return s == StateNew || s == StateRead || s == StateArchived }

// SetItemState changes the state of the owner's items among ids; it returns how
// many changed.
func (s *Store) SetItemState(owner string, ids []int64, state string) (int, error) {
	if !ValidItemState(state) {
		return 0, errors.New("state must be new, read or archived")
	}
	n := 0
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(len(ids), start+500)]
		args := []any{state}
		for _, id := range chunk {
			args = append(args, id)
		}
		args = append(args, owner)
		res, err := s.DB.Exec(`UPDATE inbox_items SET state=? WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`)
			AND account_id IN (SELECT id FROM inbox_accounts WHERE owner_sub=?)`, args...)
		if err != nil {
			return n, err
		}
		k, _ := res.RowsAffected()
		n += int(k)
	}
	return n, nil
}

// MarkAllRead marks the owner's new items read (of one account when account is set).
func (s *Store) MarkAllRead(owner, account string) (int, error) {
	q := `UPDATE inbox_items SET state='read' WHERE state='new' AND account_id IN (SELECT id FROM inbox_accounts WHERE owner_sub=?)`
	args := []any{owner}
	if account != "" {
		q, args = q+` AND account_id=?`, append(args, account)
	}
	res, err := s.DB.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ItemCounts are the new and total items of an account.
func (s *Store) ItemCounts(accountID string) (newCount, total int) {
	_ = s.DB.QueryRow(`SELECT COALESCE(SUM(state='new'),0), COUNT(*) FROM inbox_items WHERE account_id=?`, accountID).Scan(&newCount, &total)
	return
}

// Retention deletes read and archived items whose time is before cutoff; new items
// are kept however old. It returns the number deleted.
func (s *Store) Retention(cutoff time.Time) (int, error) {
	res, err := s.DB.Exec(`DELETE FROM inbox_items WHERE state IN ('read','archived') AND ts < ?`, cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// OwnersWithNew lists the owners that have new items.
func (s *Store) OwnersWithNew() ([]string, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT a.owner_sub FROM inbox_items i JOIN inbox_accounts a ON a.id=i.account_id WHERE i.state='new'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if rows.Scan(&o) == nil {
			out = append(out, o)
		}
	}
	return out, rows.Err()
}

// --- digests

// Digest is a row of inbox_digests.
type Digest struct {
	ID         int64    `json:"id"`
	OwnerSub   string   `json:"-"`
	CreatedAt  string   `json:"created_at"`
	Since      string   `json:"since"`
	Until      string   `json:"until"`
	AccountIDs []string `json:"account_ids"`
	Executor   string   `json:"executor"`
	Model      string   `json:"model"`
	SessionID  string   `json:"session_id"`
	Text       string   `json:"text"`
	ItemCount  int      `json:"item_count"`
	Cost       float64  `json:"cost"`
	State      string   `json:"state"`
	Error      string   `json:"error"`
}

// Digest states.
const (
	DigestRunning = "running"
	DigestDone    = "done"
	DigestFailed  = "failed"
)

const digestCols = `id, owner_sub, COALESCE(created_at,''), COALESCE(since,''), COALESCE(until,''), account_ids, executor,
	COALESCE(model,''), COALESCE(session_id,''), COALESCE(text,''), COALESCE(item_count,0), COALESCE(cost,0), state, COALESCE(error,'')`

func scanDigest(sc interface{ Scan(...any) error }) (*Digest, error) {
	var d Digest
	var ids string
	err := sc.Scan(&d.ID, &d.OwnerSub, &d.CreatedAt, &d.Since, &d.Until, &ids, &d.Executor, &d.Model, &d.SessionID, &d.Text,
		&d.ItemCount, &d.Cost, &d.State, &d.Error)
	_ = json.Unmarshal([]byte(ids), &d.AccountIDs)
	if d.AccountIDs == nil {
		d.AccountIDs = []string{}
	}
	return &d, err
}

// InsertDigest creates a running digest row.
func (s *Store) InsertDigest(d *Digest) error {
	ids, _ := json.Marshal(d.AccountIDs)
	d.CreatedAt, d.State = s.stamp(), DigestRunning
	return s.DB.QueryRow(`INSERT INTO inbox_digests(owner_sub,created_at,since,until,account_ids,executor,model,item_count,state)
		VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`, d.OwnerSub, d.CreatedAt, d.Since, d.Until, string(ids), d.Executor, d.Model,
		d.ItemCount, d.State).Scan(&d.ID)
}

// SetDigestSession records the session a running digest runs in.
func (s *Store) SetDigestSession(id int64, sessionID string) {
	_, _ = s.DB.Exec(`UPDATE inbox_digests SET session_id=? WHERE id=?`, sessionID, id)
}

// FinishDigest stores a successful digest and marks its items read, in one
// transaction. Only items still new are taken.
func (s *Store) FinishDigest(d *Digest, itemIDs []int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE inbox_digests SET state='done', text=?, executor=?, model=?, session_id=?, cost=?, error=? WHERE id=?`,
		d.Text, d.Executor, d.Model, d.SessionID, d.Cost, d.Error, d.ID); err != nil {
		return err
	}
	for start := 0; start < len(itemIDs); start += 500 {
		chunk := itemIDs[start:min(len(itemIDs), start+500)]
		args := []any{d.ID}
		for _, id := range chunk {
			args = append(args, id)
		}
		args = append(args, d.OwnerSub)
		if _, err := tx.Exec(`UPDATE inbox_items SET state='read', digest_id=? WHERE state='new' AND id IN (`+
			strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`) AND account_id IN (SELECT id FROM inbox_accounts WHERE owner_sub=?)`,
			args...); err != nil {
			return err
		}
	}
	d.State = DigestDone
	return tx.Commit()
}

// FailDigest marks a digest failed; its items stay new.
func (s *Store) FailDigest(d *Digest, msg string) error {
	d.State, d.Error = DigestFailed, msg
	_, err := s.DB.Exec(`UPDATE inbox_digests SET state='failed', error=?, executor=?, model=?, session_id=?, cost=? WHERE id=?`,
		msg, d.Executor, d.Model, d.SessionID, d.Cost, d.ID)
	return err
}

// FailRunningDigests marks digests a stopped hub left running as failed.
func (s *Store) FailRunningDigests() {
	_, _ = s.DB.Exec(`UPDATE inbox_digests SET state='failed', error='the hub stopped while the digest ran' WHERE state='running'`)
}

// DigestByID returns one of the owner's digests.
func (s *Store) DigestByID(owner string, id int64) (*Digest, error) {
	d, err := scanDigest(s.DB.QueryRow(`SELECT `+digestCols+` FROM inbox_digests WHERE id=? AND owner_sub=?`, id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

// Digests lists the owner's digests, newest first.
func (s *Store) Digests(owner string, limit int) ([]*Digest, error) {
	rows, err := s.DB.Query(`SELECT `+digestCols+` FROM inbox_digests WHERE owner_sub=? ORDER BY id DESC LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Digest
	for rows.Next() {
		d, err := scanDigest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DigestSince reports whether the owner has a digest created at or after t.
func (s *Store) DigestSince(owner string, t time.Time) bool {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM inbox_digests WHERE owner_sub=? AND created_at>=?`, owner,
		t.UTC().Format(time.RFC3339Nano)).Scan(&n)
	return n > 0
}

// --- tasks

// Task is a row of inbox_tasks.
type Task struct {
	ID        int64   `json:"id"`
	OwnerSub  string  `json:"-"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	State     string  `json:"state"`
	Source    string  `json:"source"`
	DigestID  int64   `json:"digest_id,omitempty"`
	ItemIDs   []int64 `json:"item_ids"`
	Due       string  `json:"due,omitempty"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// ValidTaskState: proposed, open, done or dismissed.
func ValidTaskState(s string) bool {
	return s == TaskProposed || s == TaskOpen || s == TaskDone || s == TaskDismissed
}

const taskCols = `id, owner_sub, title, COALESCE(body,''), state, source, COALESCE(digest_id,0), COALESCE(item_ids,'[]'),
	COALESCE(due,''), COALESCE(created_at,''), COALESCE(updated_at,'')`

func scanTask(sc interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	var ids string
	err := sc.Scan(&t.ID, &t.OwnerSub, &t.Title, &t.Body, &t.State, &t.Source, &t.DigestID, &ids, &t.Due, &t.CreatedAt, &t.UpdatedAt)
	_ = json.Unmarshal([]byte(ids), &t.ItemIDs)
	if t.ItemIDs == nil {
		t.ItemIDs = []int64{}
	}
	return &t, err
}

// Task limits.
const (
	MaxTaskTitle = 300
	MaxTaskBody  = 4000
)

// CleanTask checks and trims a task's text fields.
func CleanTask(t *Task) error {
	t.Title = strings.TrimSpace(strings.ToValidUTF8(t.Title, ""))
	t.Body = strings.TrimSpace(strings.ToValidUTF8(t.Body, ""))
	if t.Title == "" {
		return errors.New("a task needs a title")
	}
	if len(t.Title) > MaxTaskTitle || len(t.Body) > MaxTaskBody {
		return fmt.Errorf("task title at most %d bytes, body at most %d", MaxTaskTitle, MaxTaskBody)
	}
	if t.Due != "" {
		if _, err := time.Parse("2006-01-02", t.Due); err != nil {
			if _, err := time.Parse(time.RFC3339, t.Due); err != nil {
				return errors.New("due must be a date (YYYY-MM-DD) or an RFC 3339 time")
			}
		}
	}
	return nil
}

// InsertTask stores a task. Item ids that are not the owner's are dropped.
func (s *Store) InsertTask(t *Task) error {
	if err := CleanTask(t); err != nil {
		return err
	}
	if !ValidTaskState(t.State) {
		return errors.New("bad task state")
	}
	t.ItemIDs = s.ownItems(t.OwnerSub, t.ItemIDs)
	ids, _ := json.Marshal(t.ItemIDs)
	t.CreatedAt = s.stamp()
	t.UpdatedAt = t.CreatedAt
	var digest any
	if t.DigestID > 0 {
		digest = t.DigestID
	}
	var due any
	if t.Due != "" {
		due = t.Due
	}
	return s.DB.QueryRow(`INSERT INTO inbox_tasks(owner_sub,title,body,state,source,digest_id,item_ids,due,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?) RETURNING id`, t.OwnerSub, t.Title, t.Body, t.State, t.Source, digest, string(ids), due,
		t.CreatedAt, t.UpdatedAt).Scan(&t.ID)
}

func (s *Store) ownItems(owner string, ids []int64) []int64 {
	out := []int64{}
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] || len(out) >= 50 {
			continue
		}
		seen[id] = true
		if _, err := s.Item(owner, id); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// TaskByID returns one of the owner's tasks.
func (s *Store) TaskByID(owner string, id int64) (*Task, error) {
	t, err := scanTask(s.DB.QueryRow(`SELECT `+taskCols+` FROM inbox_tasks WHERE id=? AND owner_sub=?`, id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// SaveTask writes a changed task back.
func (s *Store) SaveTask(t *Task) error {
	if err := CleanTask(t); err != nil {
		return err
	}
	if !ValidTaskState(t.State) {
		return errors.New("state must be proposed, open, done or dismissed")
	}
	t.UpdatedAt = s.stamp()
	var due any
	if t.Due != "" {
		due = t.Due
	}
	_, err := s.DB.Exec(`UPDATE inbox_tasks SET title=?, body=?, state=?, due=?, updated_at=? WHERE id=? AND owner_sub=?`,
		t.Title, t.Body, t.State, due, t.UpdatedAt, t.ID, t.OwnerSub)
	return err
}

// Tasks lists the owner's tasks: proposed first, then open, done, dismissed; newest
// first within a state. state "" lists every state.
func (s *Store) Tasks(owner, state string, limit int) ([]*Task, error) {
	q, args := `SELECT `+taskCols+` FROM inbox_tasks WHERE owner_sub=?`, []any{owner}
	if state != "" {
		q, args = q+` AND state=?`, append(args, state)
	}
	args = append(args, limit)
	rows, err := s.DB.Query(q+` ORDER BY CASE state WHEN 'proposed' THEN 0 WHEN 'open' THEN 1 WHEN 'done' THEN 2 ELSE 3 END, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
