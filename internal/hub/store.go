package hub

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/proto"
)

const schema = `
CREATE TABLE IF NOT EXISTS users(sub TEXT PRIMARY KEY, email TEXT, name TEXT, created_at TEXT, last_login TEXT);
CREATE TABLE IF NOT EXISTS web_sessions(id_hash TEXT PRIMARY KEY, sub TEXT NOT NULL, csrf TEXT NOT NULL, created_at TEXT, expires_at TEXT);
CREATE TABLE IF NOT EXISTS login_flows(state TEXT PRIMARY KEY, nonce TEXT, verifier TEXT, created_at TEXT);
CREATE TABLE IF NOT EXISTS machines(id TEXT PRIMARY KEY, name TEXT UNIQUE NOT NULL, token_hash TEXT UNIQUE NOT NULL,
  last_seen TEXT, projects_json TEXT DEFAULT '[]', version TEXT DEFAULT '', claude_version TEXT DEFAULT '', created_at TEXT);
CREATE TABLE IF NOT EXISTS removed_machines(id TEXT PRIMARY KEY, name TEXT NOT NULL, token_hash TEXT, removed_at TEXT);
CREATE TABLE IF NOT EXISTS enroll_codes(code_hash TEXT PRIMARY KEY, name TEXT, expires_at TEXT, used INTEGER DEFAULT 0);
CREATE TABLE IF NOT EXISTS profiles(name TEXT PRIMARY KEY, json TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY, machine_id TEXT, owner_sub TEXT, cwd TEXT, profile_name TEXT,
  main_model TEXT, state TEXT, claude_session_id TEXT DEFAULT '', title TEXT DEFAULT '', created_at TEXT, last_error TEXT DEFAULT '');
CREATE TABLE IF NOT EXISTS events(session_id TEXT, seq INTEGER, ts TEXT, raw TEXT, PRIMARY KEY(session_id, seq));
CREATE TABLE IF NOT EXISTS approvals(session_id TEXT, request_id TEXT, tool_name TEXT, input TEXT, state TEXT,
  decision TEXT DEFAULT '', decided_by TEXT DEFAULT '', created_at TEXT, decided_at TEXT DEFAULT '', PRIMARY KEY(session_id, request_id));
CREATE TABLE IF NOT EXISTS session_rules(session_id TEXT, rule TEXT, PRIMARY KEY(session_id, rule));
CREATE TABLE IF NOT EXISTS audit(ts TEXT, user TEXT, action TEXT, detail TEXT);
CREATE INDEX IF NOT EXISTS audit_user ON audit(user, ts);
CREATE TABLE IF NOT EXISTS history_files(id INTEGER PRIMARY KEY, machine_id TEXT NOT NULL, path TEXT NOT NULL,
  config_dir TEXT, default_config INTEGER DEFAULT 0, claude_session_id TEXT, cwd TEXT DEFAULT '', title TEXT DEFAULT '',
  title_custom INTEGER DEFAULT 0, mtime INTEGER, size INTEGER, offset INTEGER DEFAULT 0, lines INTEGER DEFAULT 0,
  bad_lines INTEGER DEFAULT 0, updated_at TEXT, UNIQUE(machine_id, path));
CREATE TABLE IF NOT EXISTS history_lines(file_id INTEGER, off INTEGER, raw TEXT, PRIMARY KEY(file_id, off));
CREATE TABLE IF NOT EXISTS chat_models(name TEXT PRIMARY KEY, json TEXT NOT NULL, source TEXT NOT NULL DEFAULT 'config');
CREATE TABLE IF NOT EXISTS files(id TEXT PRIMARY KEY, owner_sub TEXT NOT NULL, session_id TEXT NOT NULL, name TEXT NOT NULL,
  mime TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, created_at TEXT, origin TEXT NOT NULL DEFAULT 'upload');
CREATE INDEX IF NOT EXISTS files_owner ON files(owner_sub);
CREATE INDEX IF NOT EXISTS files_session ON files(session_id);
CREATE TABLE IF NOT EXISTS usage(id INTEGER PRIMARY KEY, ts TEXT NOT NULL, session_id TEXT, kind TEXT, owner_sub TEXT,
  model TEXT, input INTEGER DEFAULT 0, cache_read INTEGER DEFAULT 0, cache_write INTEGER DEFAULT 0, output INTEGER DEFAULT 0,
  cost_usd REAL DEFAULT 0, message_id TEXT UNIQUE);
CREATE INDEX IF NOT EXISTS usage_owner ON usage(owner_sub, ts);
CREATE INDEX IF NOT EXISTS usage_session ON usage(session_id);
CREATE TABLE IF NOT EXISTS plans(id TEXT PRIMARY KEY, session_id TEXT NOT NULL, json TEXT NOT NULL, state TEXT NOT NULL,
  created_at TEXT, decided_at TEXT DEFAULT '');
CREATE INDEX IF NOT EXISTS plans_session ON plans(session_id);
CREATE TABLE IF NOT EXISTS prices(model TEXT PRIMARY KEY, json TEXT NOT NULL, source TEXT NOT NULL DEFAULT 'config', config_json TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', config_value TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT 'config');
`

// migrations add columns to databases created by earlier versions.
var migrations = []string{
	`ALTER TABLE sessions ADD COLUMN permission_mode TEXT DEFAULT 'default'`,
	`ALTER TABLE profiles ADD COLUMN source TEXT DEFAULT 'config'`,
	`ALTER TABLE users ADD COLUMN is_admin INTEGER DEFAULT 0`,
	`ALTER TABLE sessions ADD COLUMN kind TEXT DEFAULT 'code'`, // DESIGN §17.1
	`ALTER TABLE sessions ADD COLUMN model TEXT DEFAULT ''`,    // chat: a [[chat.models]] name
	`ALTER TABLE sessions ADD COLUMN system TEXT DEFAULT ''`,   // chat: the per-session system prompt
	`ALTER TABLE users ADD COLUMN monthly_budget REAL`,         // NULL: [budgets] default_monthly (§17.6)
	// mado auto mode (§18): the session's policy switch, and per approval the policy
	// outcome (allow, deny, ask; '' outside auto mode), its reason and the transcript
	// position the card belongs after.
	`ALTER TABLE sessions ADD COLUMN auto_policy INTEGER DEFAULT 1`,
	`ALTER TABLE approvals ADD COLUMN auto TEXT DEFAULT ''`,
	`ALTER TABLE approvals ADD COLUMN reason TEXT DEFAULT ''`,
	`ALTER TABLE approvals ADD COLUMN after_seq INTEGER DEFAULT 0`,
	// Code usage (§17.13): a row from an assistant event is provisional until the
	// turn's result replaces it; seq is the event it came from.
	`ALTER TABLE usage ADD COLUMN seq INTEGER DEFAULT 0`,
	`ALTER TABLE usage ADD COLUMN provisional INTEGER DEFAULT 0`,
	// The concierge (§20): a chat session with mado's API as tools, and the code
	// sessions it started (a JSON list of ids).
	`ALTER TABLE sessions ADD COLUMN concierge INTEGER DEFAULT 0`,
	`ALTER TABLE sessions ADD COLUMN spawned TEXT DEFAULT '[]'`,
	// AskUserQuestion answered from mado (§21.1): {"answers":{...},"notes":{...}}.
	`ALTER TABLE approvals ADD COLUMN answers TEXT DEFAULT ''`,
	// Resume chains (§21.6): the session that continues this one, and the one this
	// session continues.
	`ALTER TABLE sessions ADD COLUMN resumed_by TEXT DEFAULT ''`,
	`ALTER TABLE sessions ADD COLUMN resumed_from TEXT DEFAULT ''`,
	// Machine removal (M21): the hub keeps this flag while a worker is connected, so
	// `mado hub remove-machine` can refuse from the database alone.
	`ALTER TABLE machines ADD COLUMN connected INTEGER DEFAULT 0`,
	// Rate-limit auto-resume (M23, §26): the latest rate_limit_info (JSON), the reset
	// time (unix seconds) and window of a rejection, the seq the re-sent messages
	// start after, the per-session switch and the consecutive automatic attempts.
	`ALTER TABLE sessions ADD COLUMN rate_limit TEXT DEFAULT ''`,
	`ALTER TABLE sessions ADD COLUMN limited_until INTEGER DEFAULT 0`,
	`ALTER TABLE sessions ADD COLUMN limited_type TEXT DEFAULT ''`,
	`ALTER TABLE sessions ADD COLUMN limited_seq INTEGER DEFAULT 0`,
	`ALTER TABLE sessions ADD COLUMN auto_resume INTEGER DEFAULT 1`,
	`ALTER TABLE sessions ADD COLUMN resume_attempts INTEGER DEFAULT 0`,
	// Browser overrides of hub.toml entries (M25, §28.1): the hub.toml version of a
	// profile or chat model, kept next to a ui override so the admin can revert.
	`ALTER TABLE profiles ADD COLUMN config_json TEXT DEFAULT ''`,
	`ALTER TABLE chat_models ADD COLUMN config_json TEXT DEFAULT ''`,
}

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, err
		}
	}
	if err := inbox.Migrate(db); err != nil { // DESIGN §23.2
		db.Close()
		return nil, err
	}
	st := &Store{db: db}
	if err := st.backfillResumeChains(); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return st, nil
}

// backfillResumeChains links code sessions resumed before the resume columns existed
// (DESIGN §21.6): within each (owner, claude conversation) group of more than one row,
// every older row is continued by the newest, and the newest continues the row before
// it. Rows already linked are left alone, so this is idempotent.
func (s *Store) backfillResumeChains() error {
	rows, err := s.db.Query(`SELECT owner_sub, claude_session_id FROM sessions
		WHERE COALESCE(kind,'code')='code' AND claude_session_id != ''
		GROUP BY owner_sub, claude_session_id HAVING COUNT(*) > 1`)
	if err != nil {
		return err
	}
	type group struct{ owner, cid string }
	var groups []group
	for rows.Next() {
		var g group
		if err := rows.Scan(&g.owner, &g.cid); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range groups {
		ids, err := s.chainIDs(g.owner, g.cid)
		if err != nil {
			return err
		}
		if len(ids) < 2 {
			continue
		}
		newest, prev := ids[0], ids[1]
		if _, err := s.db.Exec(`UPDATE sessions SET resumed_by=? WHERE owner_sub=? AND claude_session_id=?
			AND COALESCE(kind,'code')='code' AND id!=? AND COALESCE(resumed_by,'')=''`, newest, g.owner, g.cid, newest); err != nil {
			return err
		}
		if _, err := s.db.Exec(`UPDATE sessions SET resumed_from=? WHERE id=? AND COALESCE(resumed_from,'')=''`, prev, newest); err != nil {
			return err
		}
	}
	return nil
}

// chainIDs lists the code sessions of one owner and claude conversation, newest first.
func (s *Store) chainIDs(owner, cid string) ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM sessions WHERE owner_sub=? AND claude_session_id=? AND COALESCE(kind,'code')='code'
		ORDER BY created_at DESC, rowid DESC`, owner, cid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetResumedBy marks a session as continued by another (DESIGN §21.6).
func (s *Store) SetResumedBy(id, by string) error {
	_, err := s.db.Exec(`UPDATE sessions SET resumed_by=? WHERE id=?`, by, id)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// HashToken is the stored form of machine tokens, web session ids and enroll codes.
func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// RandomToken returns n random bytes, base64url without padding.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Store) Audit(user, action, detail string) {
	_, _ = s.db.Exec(`INSERT INTO audit(ts,user,action,detail) VALUES(?,?,?,?)`, now(), user, action, detail)
}

type AuditRow struct{ TS, User, Action, Detail string }

// AuditAll is the hub-wide audit log for admins, newest first.
func (s *Store) AuditAll(limit int) ([]AuditRow, error) {
	rows, err := s.db.Query(`SELECT ts, user, action, detail FROM audit ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var a AuditRow
		if err := rows.Scan(&a.TS, &a.User, &a.Action, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AuditFor returns a user's own audit rows plus the hub's automatic actions on that
// user's sessions (resume, remembered approvals), newest first.
func (s *Store) AuditFor(sub string, limit int) ([]AuditRow, error) {
	rows, err := s.db.Query(`SELECT ts, user, action, detail FROM audit WHERE user=?
		OR (user='' AND substr(detail,1,36) IN (SELECT id FROM sessions WHERE owner_sub=?))
		ORDER BY ts DESC LIMIT ?`, sub, sub, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var a AuditRow
		if err := rows.Scan(&a.TS, &a.User, &a.Action, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- users and web sessions

type User struct {
	Sub, Email, Name string
	IsAdmin          bool
}

// UpsertUser creates the user on first login and refreshes the profile and the
// admin flag on every later one.
func (s *Store) UpsertUser(u User) error {
	_, err := s.db.Exec(`INSERT INTO users(sub,email,name,created_at,last_login,is_admin) VALUES(?,?,?,?,?,?)
		ON CONFLICT(sub) DO UPDATE SET email=excluded.email, name=excluded.name, last_login=excluded.last_login, is_admin=excluded.is_admin`,
		u.Sub, u.Email, u.Name, now(), now(), u.IsAdmin)
	return err
}

func (s *Store) User(sub string) (*User, error) {
	var u User
	err := s.db.QueryRow(`SELECT sub, COALESCE(email,''), COALESCE(name,''), COALESCE(is_admin,0) FROM users WHERE sub=?`, sub).
		Scan(&u.Sub, &u.Email, &u.Name, &u.IsAdmin)
	return &u, err
}

type WebSession struct {
	User User
	CSRF string
}

func (s *Store) CreateWebSession(sub string, ttl time.Duration) (id string, err error) {
	id = RandomToken(32)
	_, err = s.db.Exec(`INSERT INTO web_sessions(id_hash,sub,csrf,created_at,expires_at) VALUES(?,?,?,?,?)`,
		HashToken(id), sub, RandomToken(24), now(), time.Now().Add(ttl).UTC().Format(time.RFC3339Nano))
	return id, err
}

func (s *Store) WebSession(id string) (*WebSession, error) {
	var ws WebSession
	var exp string
	err := s.db.QueryRow(`SELECT w.csrf, w.expires_at, u.sub, COALESCE(u.email,''), COALESCE(u.name,''), COALESCE(u.is_admin,0) FROM web_sessions w JOIN users u ON u.sub=w.sub WHERE w.id_hash=?`,
		HashToken(id)).Scan(&ws.CSRF, &exp, &ws.User.Sub, &ws.User.Email, &ws.User.Name, &ws.User.IsAdmin)
	if err != nil {
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339Nano, exp); err != nil || time.Now().After(t) {
		return nil, errors.New("expired")
	}
	return &ws, nil
}

func (s *Store) DeleteWebSession(id string) {
	_, _ = s.db.Exec(`DELETE FROM web_sessions WHERE id_hash=?`, HashToken(id))
}

func (s *Store) PutLoginFlow(state, nonce, verifier string) error {
	_, _ = s.db.Exec(`DELETE FROM login_flows WHERE created_at < ?`, time.Now().Add(-15*time.Minute).UTC().Format(time.RFC3339Nano))
	_, err := s.db.Exec(`INSERT INTO login_flows(state,nonce,verifier,created_at) VALUES(?,?,?,?)`, state, nonce, verifier, now())
	return err
}

// TakeLoginFlow returns and deletes a flow (single use).
func (s *Store) TakeLoginFlow(state string) (nonce, verifier string, err error) {
	var created string
	err = s.db.QueryRow(`SELECT nonce, verifier, created_at FROM login_flows WHERE state=?`, state).Scan(&nonce, &verifier, &created)
	if err != nil {
		return "", "", err
	}
	_, _ = s.db.Exec(`DELETE FROM login_flows WHERE state=?`, state)
	if t, err := time.Parse(time.RFC3339Nano, created); err != nil || time.Since(t) > 15*time.Minute {
		return "", "", errors.New("login flow expired")
	}
	return nonce, verifier, nil
}

// --- machines and enrollment

type Machine struct {
	ID, Name, LastSeen, Version, ClaudeVersion string
	Projects                                   []string
}

func (s *Store) CreateEnrollCode(name string, ttl time.Duration) (string, error) {
	code := RandomToken(18)
	_, err := s.db.Exec(`INSERT INTO enroll_codes(code_hash,name,expires_at) VALUES(?,?,?)`,
		HashToken(code), name, time.Now().Add(ttl).UTC().Format(time.RFC3339Nano))
	return code, err
}

// Enroll consumes a code and (re)creates the machine with a fresh token.
func (s *Store) Enroll(name, code string) (token string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var codeName, exp string
	var used int
	err = tx.QueryRow(`SELECT name, expires_at, used FROM enroll_codes WHERE code_hash=?`, HashToken(code)).Scan(&codeName, &exp, &used)
	if err != nil {
		return "", errors.New("invalid code")
	}
	t, _ := time.Parse(time.RFC3339Nano, exp)
	if used != 0 || time.Now().After(t) || codeName != name {
		return "", errors.New("invalid code")
	}
	if _, err := tx.Exec(`UPDATE enroll_codes SET used=1 WHERE code_hash=?`, HashToken(code)); err != nil {
		return "", err
	}
	token = RandomToken(32)
	_, err = tx.Exec(`INSERT INTO machines(id,name,token_hash,created_at) VALUES(?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET token_hash=excluded.token_hash`, proto.NewID(), name, HashToken(token), now())
	if err != nil {
		return "", err
	}
	return token, tx.Commit()
}

// SetMachineToken creates the machine name or replaces its token, without an
// enrollment code. `mado local` uses it because the process owns the store (DESIGN §19.2).
func (s *Store) SetMachineToken(name, token string) error {
	if !validName(name) {
		return fmt.Errorf("machine name %q: use [A-Za-z0-9_-]", name)
	}
	_, err := s.db.Exec(`INSERT INTO machines(id,name,token_hash,created_at) VALUES(?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET token_hash=excluded.token_hash`, proto.NewID(), name, HashToken(token), now())
	return err
}

// Machine removal (DESIGN §24). The errors map to HTTP 404 and 409.
var (
	ErrNoMachine        = errors.New("no such machine")
	ErrMachineConnected = errors.New("the worker is connected; stop it first")
)

// terminalStates is the SQL list of ended session states; any other state is live.
const terminalStates = `('exited','detached')`

// MachineRemovedReason is the last_error of sessions ended by a machine removal.
const MachineRemovedReason = "machine removed"

// RemoveMachine deletes a machine whose worker is not connected. Its token stops
// working (the hash moves to removed_machines, which also keeps the name for old
// sessions and history files), and its unused enrollment codes are deleted. Sessions
// that had not ended are marked exited with last_error "machine removed" (as the hub
// does for any session whose worker or shim is gone) and their pending approvals
// expire; their ids are returned so the hub can publish the state. Sessions, audit
// rows and digests are kept. The eligibility check and the delete are one write
// statement, so a worker that connects at the same moment either blocks the removal
// or finds its row gone.
func (s *Store) RemoveMachine(name string) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ts := now()
	res, err := tx.Exec(`INSERT INTO removed_machines(id,name,token_hash,removed_at)
		SELECT m.id, m.name, m.token_hash, ? FROM machines m WHERE m.name=? AND COALESCE(m.connected,0)=0
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, token_hash=excluded.token_hash, removed_at=excluded.removed_at`, ts, name)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var connected int
		if err := tx.QueryRow(`SELECT COALESCE(connected,0) FROM machines WHERE name=?`, name).Scan(&connected); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrNoMachine
			}
			return nil, err
		}
		return nil, ErrMachineConnected
	}
	var id string
	if err := tx.QueryRow(`SELECT id FROM machines WHERE name=?`, name).Scan(&id); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT id FROM sessions WHERE machine_id=? AND state NOT IN `+terminalStates+` ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	var ended []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return nil, err
		}
		ended = append(ended, sid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, sid := range ended {
		if _, err := tx.Exec(`UPDATE sessions SET state=?, last_error=? WHERE id=?`, proto.StateExited, MachineRemovedReason, sid); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE approvals SET state='decided', decision='expired', decided_at=? WHERE session_id=? AND state='pending'`, ts, sid); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM machines WHERE id=?`, id); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM enroll_codes WHERE name=? AND used=0`, name); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ended, nil
}

// LiveSessionCounts is the number of sessions that have not ended, per machine id
// (for the Machines view's removal warning, DESIGN §24).
func (s *Store) LiveSessionCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT COALESCE(machine_id,''), COUNT(*) FROM sessions WHERE state NOT IN ` + terminalStates + ` GROUP BY machine_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// RemovedMachineName is the name of a removed machine whose token this was ("" if none),
// for the log line when its worker still tries to connect.
func (s *Store) RemovedMachineName(token string) string {
	var name string
	_ = s.db.QueryRow(`SELECT name FROM removed_machines WHERE token_hash=?`, HashToken(token)).Scan(&name)
	return name
}

// SetMachineConnected keeps the connected flag. Turning it on reports false when the
// machine no longer exists (removed while the worker was authenticating).
func (s *Store) SetMachineConnected(id string, on bool) bool {
	res, err := s.db.Exec(`UPDATE machines SET connected=? WHERE id=?`, on, id)
	if err != nil {
		return !on
	}
	n, _ := res.RowsAffected()
	return n > 0 || !on
}

// ResetMachinesConnected clears the flags at hub start: no worker is connected yet.
func (s *Store) ResetMachinesConnected() error {
	_, err := s.db.Exec(`UPDATE machines SET connected=0 WHERE connected<>0`)
	return err
}

func (s *Store) MachineByToken(token string) (*Machine, error) {
	var m Machine
	err := s.db.QueryRow(`SELECT id, name FROM machines WHERE token_hash=?`, HashToken(token)).Scan(&m.ID, &m.Name)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) UpdateMachineHello(id string, h *proto.Hello) error {
	pj, _ := json.Marshal(h.Projects)
	_, err := s.db.Exec(`UPDATE machines SET last_seen=?, projects_json=?, version=?, claude_version=? WHERE id=?`,
		now(), string(pj), h.Version, h.ClaudeVersion, id)
	return err
}

func (s *Store) TouchMachine(id string) {
	_, _ = s.db.Exec(`UPDATE machines SET last_seen=? WHERE id=?`, now(), id)
}

func (s *Store) Machines() ([]Machine, error) {
	rows, err := s.db.Query(`SELECT id, name, COALESCE(last_seen,''), projects_json, version, claude_version FROM machines ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Machine
	for rows.Next() {
		var m Machine
		var pj string
		if err := rows.Scan(&m.ID, &m.Name, &m.LastSeen, &pj, &m.Version, &m.ClaudeVersion); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(pj), &m.Projects)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) Machine(id string) (*Machine, error) {
	ms, err := s.Machines()
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.ID == id {
			return &m, nil
		}
	}
	return nil, sql.ErrNoRows
}

// --- profiles

// Profile sources: "config" (hub.toml, re-seeded on every start), "builtin"
// (subscription, read-only) and "ui" (the editor: a new entry, or an override of a
// hub.toml entry that seeding leaves alone, DESIGN §28.1). The same sources are used
// for chat models, prices and settings.
const (
	SourceConfig  = "config"
	SourceBuiltin = "builtin"
	SourceUI      = "ui"
)

// PutProfile stores p as the hub.toml version of its name (tests and older callers).
func (s *Store) PutProfile(p proto.Profile) error { return s.PutProfileFrom(p, SourceConfig) }

func (s *Store) PutProfileFrom(p proto.Profile, source string) error {
	b, _ := json.Marshal(p.Redacted())
	return s.putOverridable(ovProfiles, p.Name, string(b), source)
}

// SeedProfiles stores the hub.toml profiles under the override rule (§28.1).
func (s *Store) SeedProfiles(ps []proto.Profile) error {
	es := make([]seedEntry, 0, len(ps))
	for _, p := range ps {
		b, _ := json.Marshal(p.Redacted())
		es = append(es, seedEntry{p.Name, string(b)})
	}
	return s.seedOverridable(ovProfiles, es)
}

// RevertProfile drops the ui override of a hub.toml profile.
func (s *Store) RevertProfile(name string) (bool, error) {
	return s.revertOverridable(ovProfiles, name)
}

// ProfileSource returns the source of a profile, or "" if it does not exist.
func (s *Store) ProfileSource(name string) string {
	var src string
	_ = s.db.QueryRow(`SELECT COALESCE(source,'config') FROM profiles WHERE name=?`, name).Scan(&src)
	return src
}

func (s *Store) Profile(name string) (*proto.Profile, error) {
	var j string
	if err := s.db.QueryRow(`SELECT json FROM profiles WHERE name=?`, name).Scan(&j); err != nil {
		return nil, err
	}
	var p proto.Profile
	if err := json.Unmarshal([]byte(j), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) Profiles() ([]proto.Profile, error) {
	rows, err := s.db.Query(`SELECT json FROM profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []proto.Profile
	for rows.Next() {
		var j string
		var p proto.Profile
		if err := rows.Scan(&j); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(j), &p) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// --- sessions and events

type Session struct {
	ID, MachineID, OwnerSub, CWD, ProfileName, MainModel, State, ClaudeSessionID, Title, CreatedAt, LastError string
	MachineName, PermissionMode                                                                               string
	// Kind is KindCode or KindChat (DESIGN §17.1). Model and System are chat only.
	Kind, Model, System string
	// AutoPolicy is the mado auto switch (DESIGN §18.1); it only counts for a router
	// session in permission mode auto. New sessions start with it on.
	AutoPolicy bool
	// Concierge marks a chat session run as mado's concierge (DESIGN §20).
	Concierge bool
	// ResumedBy is the session that continues this one, ResumedFrom the one this
	// session continues (DESIGN §21.6); '' when none.
	ResumedBy, ResumedFrom string
	// Rate-limit auto-resume (DESIGN §26): RateLimit is the latest rate_limit_info as
	// JSON; LimitedUntil (unix seconds, 0 = not limited), LimitedType and LimitedSeq
	// describe the current rejection; AutoResume is the switch; ResumeAttempts counts
	// consecutive automatic resumes.
	RateLimit                string
	LimitedUntil, LimitedSeq int64
	LimitedType              string
	AutoResume               bool
	ResumeAttempts           int
}

// Session kinds.
const (
	KindCode = "code"
	KindChat = "chat"
	// KindDigest is an inbox digest run (DESIGN §23.5). Without a machine it is run
	// by the chat runner on the hub and behaves as a chat session that takes no
	// messages; with one (the claude executor, M16) it is a code session.
	KindDigest = "digest"
)

// IsChat reports a session run by the hub's chat runner: a chat session, or a
// digest run by the chat executor.
func (x *Session) IsChat() bool {
	return x.Kind == KindChat || (x.Kind == KindDigest && x.MachineID == "")
}

func (s *Store) CreateSession(x Session) error {
	if x.PermissionMode == "" {
		x.PermissionMode = "default"
	}
	if x.Kind == "" {
		x.Kind = KindCode
	}
	if x.State == "" {
		x.State = proto.StateStarting
	}
	_, err := s.db.Exec(`INSERT INTO sessions(id,machine_id,owner_sub,cwd,profile_name,main_model,state,title,created_at,permission_mode,claude_session_id,kind,model,system,auto_policy,concierge,resumed_from) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`,
		x.ID, x.MachineID, x.OwnerSub, x.CWD, x.ProfileName, x.MainModel, x.State, x.Title, now(), x.PermissionMode, x.ClaudeSessionID,
		x.Kind, x.Model, x.System, x.Concierge, x.ResumedFrom)
	return err
}

const sessionCols = `s.id, s.machine_id, s.owner_sub, s.cwd, s.profile_name, s.main_model, s.state, s.claude_session_id, s.title, s.created_at, s.last_error, COALESCE(m.name,rm.name,''), COALESCE(s.permission_mode,'default'),
	COALESCE(s.kind,'code'), COALESCE(s.model,''), COALESCE(s.system,''), COALESCE(s.auto_policy,1), COALESCE(s.concierge,0),
	COALESCE(s.resumed_by,''), COALESCE(s.resumed_from,''),
	COALESCE(s.rate_limit,''), COALESCE(s.limited_until,0), COALESCE(s.limited_seq,0), COALESCE(s.limited_type,''),
	COALESCE(s.auto_resume,1), COALESCE(s.resume_attempts,0)`

func scanSession(sc interface{ Scan(...any) error }) (*Session, error) {
	var x Session
	err := sc.Scan(&x.ID, &x.MachineID, &x.OwnerSub, &x.CWD, &x.ProfileName, &x.MainModel, &x.State, &x.ClaudeSessionID, &x.Title, &x.CreatedAt, &x.LastError, &x.MachineName, &x.PermissionMode,
		&x.Kind, &x.Model, &x.System, &x.AutoPolicy, &x.Concierge, &x.ResumedBy, &x.ResumedFrom,
		&x.RateLimit, &x.LimitedUntil, &x.LimitedSeq, &x.LimitedType, &x.AutoResume, &x.ResumeAttempts)
	return &x, err
}

// SessionsOnMachine lists the sessions of a machine that have not ended.
func (s *Store) SessionsOnMachine(machineID string) ([]*Session, error) {
	rows, err := s.db.Query(`SELECT `+sessionCols+` FROM sessions s LEFT JOIN machines m ON m.id=s.machine_id LEFT JOIN removed_machines rm ON rm.id=s.machine_id
		WHERE s.machine_id=? AND s.state NOT IN ('exited','detached') ORDER BY s.created_at`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SetSessionState is the hub's own state change (resume), not a worker report.
func (s *Store) SetSessionState(id, state string) {
	_, _ = s.db.Exec(`UPDATE sessions SET state=? WHERE id=?`, state, id)
}

// ContiguousSeq is the highest n such that events 1..n are all stored: the point
// a resync must start from.
func (s *Store) ContiguousSeq(sessionID string) int64 {
	var min int64
	if s.db.QueryRow(`SELECT COALESCE(MIN(seq),0) FROM events WHERE session_id=?`, sessionID).Scan(&min) != nil || min != 1 {
		return 0
	}
	var end int64
	_ = s.db.QueryRow(`SELECT MIN(e.seq) FROM events e WHERE e.session_id=? AND NOT EXISTS
		(SELECT 1 FROM events f WHERE f.session_id=e.session_id AND f.seq=e.seq+1)`, sessionID).Scan(&end)
	return end
}

// MaxSeq is the highest stored seq.
func (s *Store) MaxSeq(sessionID string) int64 {
	var n int64
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM events WHERE session_id=?`, sessionID).Scan(&n)
	return n
}

// HasEvent reports whether one seq is stored.
func (s *Store) HasEvent(sessionID string, seq int64) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id=? AND seq=?`, sessionID, seq).Scan(&n)
	return n > 0
}

// ExpireApprovals closes the pending approvals of a session whose claude is gone.
func (s *Store) ExpireApprovals(sessionID string) {
	_, _ = s.db.Exec(`UPDATE approvals SET state='decided', decision='expired', decided_at=? WHERE session_id=? AND state='pending'`, now(), sessionID)
}

func (s *Store) Session(id string) (*Session, error) {
	return scanSession(s.db.QueryRow(`SELECT `+sessionCols+` FROM sessions s LEFT JOIN machines m ON m.id=s.machine_id LEFT JOIN removed_machines rm ON rm.id=s.machine_id WHERE s.id=?`, id))
}

func (s *Store) SessionsByOwner(sub string) ([]*Session, error) {
	rows, err := s.db.Query(`SELECT `+sessionCols+` FROM sessions s LEFT JOIN machines m ON m.id=s.machine_id LEFT JOIN removed_machines rm ON rm.id=s.machine_id WHERE s.owner_sub=? ORDER BY s.created_at DESC LIMIT 200`, sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// UpdateSessionState only accepts updates from the machine that owns the session.
func (s *Store) UpdateSessionState(machineID string, st *proto.SessionState) (bool, error) {
	res, err := s.db.Exec(`UPDATE sessions SET state=?, claude_session_id=CASE WHEN ?='' THEN claude_session_id ELSE ? END,
		last_error=CASE WHEN ?='' THEN last_error ELSE ? END WHERE id=? AND machine_id=?`,
		st.State, st.ClaudeSessionID, st.ClaudeSessionID, st.Error, st.Error, st.SessionID, machineID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) SetSessionError(id, msg string) {
	_, _ = s.db.Exec(`UPDATE sessions SET state=?, last_error=? WHERE id=?`, proto.StateExited, msg, id)
}

func (s *Store) SessionMachine(id string) (string, error) {
	var m string
	err := s.db.QueryRow(`SELECT machine_id FROM sessions WHERE id=?`, id).Scan(&m)
	return m, err
}

// InsertEvent stores an event; duplicates (resync) report inserted=false.
func (s *Store) InsertEvent(sessionID string, seq int64, raw []byte) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO events(session_id,seq,ts,raw) VALUES(?,?,?,?)`, sessionID, seq, now(), string(raw))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

type StoredEvent struct {
	Seq int64
	Raw []byte
}

func (s *Store) Events(sessionID string, afterSeq int64, limit int) ([]StoredEvent, error) {
	rows, err := s.db.Query(`SELECT seq, raw FROM events WHERE session_id=? AND seq>? ORDER BY seq LIMIT ?`, sessionID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredEvent
	for rows.Next() {
		var e StoredEvent
		var raw string
		if err := rows.Scan(&e.Seq, &raw); err != nil {
			return nil, err
		}
		e.Raw = []byte(raw)
		out = append(out, e)
	}
	return out, rows.Err()
}

// beforeExtendPages caps how far EventsBefore looks back for a user turn (DESIGN §17.14).
const beforeExtendPages = 3

// EventsBefore is the tail-first page (DESIGN §17.14): the newest limit events with
// seq < before (before <= 0 means the end), in ascending order. When older events
// exist and the page does not start at a human user turn, the page is extended
// backwards to the nearest one, looking at most beforeExtendPages pages back; past
// that the page is cut at its original boundary. moreBefore reports older events.
func (s *Store) EventsBefore(sessionID string, before int64, limit int) (evs []StoredEvent, moreBefore bool, err error) {
	if before <= 0 {
		before = 1<<63 - 1
	}
	rows, err := s.db.Query(`SELECT seq, raw FROM events WHERE session_id=? AND seq<? ORDER BY seq DESC LIMIT ?`, sessionID, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	evs, err = scanEvents(rows)
	if err != nil {
		return nil, false, err
	}
	if len(evs) <= limit {
		slices.Reverse(evs)
		return evs, false, nil
	}
	evs = evs[:limit]
	slices.Reverse(evs)
	if humanTurn(evs[0].Raw) {
		return evs, true, nil
	}
	cut, err := s.userTurnBefore(sessionID, evs[0].Seq, beforeExtendPages*limit)
	if err != nil {
		return nil, false, err
	}
	if cut > 0 {
		rows, err := s.db.Query(`SELECT seq, raw FROM events WHERE session_id=? AND seq>=? AND seq<? ORDER BY seq`, sessionID, cut, evs[0].Seq)
		if err != nil {
			return nil, false, err
		}
		older, err := scanEvents(rows)
		if err != nil {
			return nil, false, err
		}
		evs = append(older, evs...)
	}
	err = s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM events WHERE session_id=? AND seq<?)`, sessionID, evs[0].Seq).Scan(&moreBefore)
	return evs, moreBefore, err
}

// userTurnBefore finds where an extended page starts: the seq of the newest human user
// turn among the window events right before seq, or the session's first seq when the
// window reaches the start; 0 when neither is within the window.
func (s *Store) userTurnBefore(sessionID string, seq int64, window int) (int64, error) {
	rows, err := s.db.Query(`SELECT seq, raw FROM events WHERE session_id=? AND seq<? ORDER BY seq DESC LIMIT ?`, sessionID, seq, window)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n, oldest := 0, int64(0)
	for rows.Next() {
		var q int64
		var raw []byte
		if err := rows.Scan(&q, &raw); err != nil {
			return 0, err
		}
		n++
		oldest = q
		if humanTurn(raw) {
			return q, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if n < window {
		return oldest, nil // the window reaches the first event
	}
	return 0, nil
}

// humanTurn reports a stream-json user event typed by a person: the main thread's
// (no parent_tool_use_id) user message whose content is a string, or blocks with text
// (or an attached file) and no tool_result.
func humanTurn(raw []byte) bool {
	if !bytes.Contains(raw, []byte(`"user"`)) {
		return false
	}
	var e struct {
		Type            string  `json:"type"`
		ParentToolUseID *string `json:"parent_tool_use_id"`
		Message         *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Type != "user" || e.Message == nil {
		return false
	}
	if e.ParentToolUseID != nil && *e.ParentToolUseID != "" {
		return false
	}
	var text string
	if json.Unmarshal(e.Message.Content, &text) == nil {
		return true
	}
	var cbs []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(e.Message.Content, &cbs) != nil {
		return false
	}
	human := false
	for _, c := range cbs {
		switch c.Type {
		case "tool_result":
			return false
		case "text", "mado_file", "image":
			human = true
		}
	}
	return human
}

func scanEvents(rows *sql.Rows) ([]StoredEvent, error) {
	defer rows.Close()
	var out []StoredEvent
	for rows.Next() {
		var e StoredEvent
		var raw string
		if err := rows.Scan(&e.Seq, &raw); err != nil {
			return nil, err
		}
		e.Raw = []byte(raw)
		out = append(out, e)
	}
	return out, rows.Err()
}

// UsageEvents is Events narrowed to the events that may carry model usage (assistant
// and result lines, and the mado_spawn marker); the usage package checks the type again.
func (s *Store) UsageEvents(sessionID string, afterSeq int64, limit int) ([]StoredEvent, error) {
	rows, err := s.db.Query(`SELECT seq, raw FROM events WHERE session_id=? AND seq>?
		AND (instr(raw, '"type":"assistant"') > 0 OR instr(raw, '"type":"result"') > 0 OR instr(raw, '"type":"mado_spawn"') > 0)
		ORDER BY seq LIMIT ?`, sessionID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredEvent
	for rows.Next() {
		var e StoredEvent
		var raw string
		if err := rows.Scan(&e.Seq, &raw); err != nil {
			return nil, err
		}
		e.Raw = []byte(raw)
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- approvals

type Approval struct {
	SessionID, RequestID, ToolName, Input, State, Decision string
	// DecidedBy is the user's sub, "rule" (allow for session), "rule:<name>" or
	// "classifier" (DESIGN §18.3). Auto is the auto mode outcome (allow, deny, ask) and
	// Reason its explanation; AfterSeq is the highest event seq when the request came.
	DecidedBy, Auto, Reason string
	AfterSeq                int64
	// Answers is the JSON {"answers":{question: answer},"notes":{question: notes}} of
	// an AskUserQuestion answered in mado (DESIGN §21.1), "" otherwise.
	Answers string
}

const approvalCols = `session_id, request_id, tool_name, input, state, decision, COALESCE(decided_by,''), COALESCE(auto,''),
	COALESCE(reason,''), COALESCE(after_seq,0), COALESCE(answers,'')`

func scanApproval(sc interface{ Scan(...any) error }) (Approval, error) {
	var a Approval
	err := sc.Scan(&a.SessionID, &a.RequestID, &a.ToolName, &a.Input, &a.State, &a.Decision, &a.DecidedBy, &a.Auto, &a.Reason, &a.AfterSeq, &a.Answers)
	return a, err
}

func (s *Store) InsertApproval(a Approval) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO approvals(session_id,request_id,tool_name,input,state,created_at,after_seq) VALUES(?,?,?,?,?,?,?)`,
		a.SessionID, a.RequestID, a.ToolName, a.Input, "pending", now(), a.AfterSeq)
	return err
}

// DecideApproval marks a pending approval decided; false if it was not pending.
func (s *Store) DecideApproval(sessionID, requestID, decision, by string) (bool, error) {
	return s.DecideApprovalAnswers(sessionID, requestID, decision, by, "")
}

// DecideApprovalAnswers is DecideApproval that also records the answers JSON of an
// AskUserQuestion (DESIGN §21.1).
func (s *Store) DecideApprovalAnswers(sessionID, requestID, decision, by, answers string) (bool, error) {
	res, err := s.db.Exec(`UPDATE approvals SET state='decided', decision=?, decided_by=?, decided_at=?, answers=?
		WHERE session_id=? AND request_id=? AND state='pending'`,
		decision, by, now(), answers, sessionID, requestID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DecideAuto is DecideApproval for a decision of the auto mode policy.
func (s *Store) DecideAuto(sessionID, requestID, decision, by, outcome, reason string) (bool, error) {
	res, err := s.db.Exec(`UPDATE approvals SET state='decided', decision=?, decided_by=?, decided_at=?, auto=?, reason=?
		WHERE session_id=? AND request_id=? AND state='pending'`, decision, by, now(), outcome, reason, sessionID, requestID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkAsked records that the policy left a pending approval to the user.
func (s *Store) MarkAsked(sessionID, requestID, reason string) bool {
	res, err := s.db.Exec(`UPDATE approvals SET auto='ask', reason=? WHERE session_id=? AND request_id=? AND state='pending'`,
		reason, sessionID, requestID)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// ReopenApproval undoes DecideApproval when the decision could not be delivered.
func (s *Store) ReopenApproval(sessionID, requestID string) {
	_, _ = s.db.Exec(`UPDATE approvals SET state='pending', decision='', decided_by='', decided_at='', answers='' WHERE session_id=? AND request_id=?`,
		sessionID, requestID)
}

func (s *Store) Approval(sessionID, requestID string) (*Approval, error) {
	a, err := scanApproval(s.db.QueryRow(`SELECT `+approvalCols+` FROM approvals WHERE session_id=? AND request_id=?`, sessionID, requestID))
	return &a, err
}

func (s *Store) PendingApprovals(sessionID string) ([]Approval, error) {
	return s.approvalsWhere(`session_id=? AND state='pending'`, sessionID)
}

// TranscriptApprovals are the cards a reloaded session view shows: the pending ones,
// every one the auto mode policy handled and every question (DESIGN §21.2), whose
// card shows the answers.
func (s *Store) TranscriptApprovals(sessionID string) ([]Approval, error) {
	return s.approvalsWhere(`session_id=? AND (state='pending' OR COALESCE(auto,'')<>'' OR tool_name=?)`, sessionID, ToolAskUserQuestion)
}

func (s *Store) approvalsWhere(where string, args ...any) ([]Approval, error) {
	rows, err := s.db.Query(`SELECT `+approvalCols+` FROM approvals WHERE `+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AutoCounts counts a session's auto mode outcomes.
type AutoCounts struct {
	Allowed int `json:"allowed"`
	Denied  int `json:"denied"`
	Asked   int `json:"asked"`
}

func (s *Store) AutoCounts(sessionID string) AutoCounts {
	var c AutoCounts
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(auto='allow'),0), COALESCE(SUM(auto='deny'),0), COALESCE(SUM(auto='ask'),0)
		FROM approvals WHERE session_id=?`, sessionID).Scan(&c.Allowed, &c.Denied, &c.Asked)
	return c
}

// SetSessionAuto flips the auto mode switch.
func (s *Store) SetSessionAuto(id string, on bool) error {
	_, err := s.db.Exec(`UPDATE sessions SET auto_policy=? WHERE id=?`, on, id)
	return err
}

// EnableSessionAuto turns a running default or acceptEdits session into mado auto
// (DESIGN §18.6): permission_mode becomes auto, so a resume starts claude in
// acceptEdits, and the policy switch is on.
func (s *Store) EnableSessionAuto(id string) error {
	_, err := s.db.Exec(`UPDATE sessions SET permission_mode='auto', auto_policy=1 WHERE id=?`, id)
	return err
}

// SetSessionMode stores a permission mode chosen mid-session (DESIGN §25.3); auto
// turns the policy switch on as well.
func (s *Store) SetSessionMode(id, mode string) error {
	_, err := s.db.Exec(`UPDATE sessions SET permission_mode=?, auto_policy=CASE WHEN ?='auto' THEN 1 ELSE auto_policy END WHERE id=?`,
		mode, mode, id)
	return err
}

// LastEvents returns up to n of a session's newest events, oldest first.
func (s *Store) LastEvents(sessionID string, n int) ([][]byte, error) {
	rows, err := s.db.Query(`SELECT raw FROM (SELECT seq, raw FROM events WHERE session_id=? ORDER BY seq DESC LIMIT ?) ORDER BY seq`, sessionID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, []byte(raw))
	}
	return out, rows.Err()
}

func (s *Store) AddSessionRule(sessionID, rule string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO session_rules(session_id,rule) VALUES(?,?)`, sessionID, rule)
	return err
}

func (s *Store) HasSessionRule(sessionID, rule string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM session_rules WHERE session_id=? AND rule=?`, sessionID, rule).Scan(&n)
	return n > 0
}

// eventType is the top-level "type" of a stored event.
func eventType(raw []byte) string {
	var e struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.Type
}

// UsageBoundary finds the event a code turn ending in the result at seq counts its
// usage from (§17.13): the latest earlier result or mado_spawn marker of the session;
// typ is "" and prev.Seq 0 when there is none. later reports that the next stored
// result of the same process (no marker in between) is already there and accounted
// for this turn as well (a resync filled a gap).
func (s *Store) UsageBoundary(sessionID string, seq int64) (prev StoredEvent, typ string, later bool, err error) {
	const marks = `(instr(raw, '"type":"result"') > 0 OR instr(raw, '"type":"mado_spawn"') > 0)`
	scan := func(q string) (StoredEvent, string, error) {
		rows, err := s.db.Query(q, sessionID, seq)
		if err != nil {
			return StoredEvent{}, "", err
		}
		defer rows.Close()
		for rows.Next() {
			var e StoredEvent
			var raw string
			if err := rows.Scan(&e.Seq, &raw); err != nil {
				return StoredEvent{}, "", err
			}
			// instr also matches the text inside other events; check the real type.
			if t := eventType([]byte(raw)); t == "result" || t == "mado_spawn" {
				e.Raw = []byte(raw)
				return e, t, nil
			}
		}
		return StoredEvent{}, "", rows.Err()
	}
	if _, t, err := scan(`SELECT seq, raw FROM events WHERE session_id=? AND seq>? AND ` + marks + ` ORDER BY seq`); err != nil || t == "result" {
		return StoredEvent{}, "", t == "result", err
	}
	prev, typ, err = scan(`SELECT seq, raw FROM events WHERE session_id=? AND seq<? AND ` + marks + ` ORDER BY seq DESC`)
	return prev, typ, false, err
}
