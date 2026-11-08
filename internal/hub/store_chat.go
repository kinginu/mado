package hub

import (
	"encoding/json"
	"errors"

	"github.com/kinginu/mado/internal/chat"
)

// --- chat models (DESIGN §17.1): stored like profiles, seeded from hub.toml (§28.1).

// SeedChatModels stores the hub.toml models under the override rule (§28.1): a
// model saved in the editor (source ui) keeps its JSON, even under a hub.toml name.
func (s *Store) SeedChatModels(ms []chat.Model) error {
	es := make([]seedEntry, 0, len(ms))
	for _, m := range ms {
		b, _ := json.Marshal(m)
		es = append(es, seedEntry{m.Name, string(b)})
	}
	return s.seedOverridable(ovChatModels, es)
}

func (s *Store) PutChatModel(m chat.Model, source string) error {
	b, _ := json.Marshal(m)
	return s.putOverridable(ovChatModels, m.Name, string(b), source)
}

// RevertChatModel drops the ui override of a hub.toml chat model.
func (s *Store) RevertChatModel(name string) (bool, error) {
	return s.revertOverridable(ovChatModels, name)
}

// ChatModelSource is "" for an unknown model.
func (s *Store) ChatModelSource(name string) string {
	var src string
	_ = s.db.QueryRow(`SELECT source FROM chat_models WHERE name=?`, name).Scan(&src)
	return src
}

func (s *Store) ChatModel(name string) (*chat.Model, error) {
	var j string
	if err := s.db.QueryRow(`SELECT json FROM chat_models WHERE name=?`, name).Scan(&j); err != nil {
		return nil, err
	}
	var m chat.Model
	if err := json.Unmarshal([]byte(j), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ChatModels lists the models by name.
func (s *Store) ChatModels() ([]chat.Model, error) {
	rows, err := s.db.Query(`SELECT json FROM chat_models ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chat.Model
	for rows.Next() {
		var j string
		var m chat.Model
		if err := rows.Scan(&j); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(j), &m) == nil {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

// --- events appended by the hub itself (chat sessions)

// AppendEvent stores raw as the session's next event and returns its seq. The store
// has one connection, so the MAX+1 is atomic.
func (s *Store) AppendEvent(sessionID string, raw []byte) (int64, error) {
	var seq int64
	err := s.db.QueryRow(`INSERT INTO events(session_id,seq,ts,raw)
		SELECT ?, COALESCE(MAX(seq),0)+1, ?, ? FROM events WHERE session_id=? RETURNING seq`,
		sessionID, now(), string(raw), sessionID).Scan(&seq)
	return seq, err
}

// SetSessionTitle renames a session.
func (s *Store) SetSessionTitle(id, title string) error {
	_, err := s.db.Exec(`UPDATE sessions SET title=? WHERE id=?`, title, id)
	return err
}

// SetSessionModel switches a chat session's model (the next turn uses it).
func (s *Store) SetSessionModel(id, model string) error {
	_, err := s.db.Exec(`UPDATE sessions SET model=? WHERE id=? AND kind='chat'`, model, id)
	return err
}

// DeleteSession removes a session with its events, approvals, rules and file rows,
// and returns the sha256 of blobs no other file row uses (the caller removes them).
// Usage rows stay: they are the accounting record.
func (s *Store) DeleteSession(id string) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT DISTINCT sha256 FROM files WHERE session_id=?`, id)
	if err != nil {
		return nil, err
	}
	var shas []string
	for rows.Next() {
		var h string
		if rows.Scan(&h) == nil {
			shas = append(shas, h)
		}
	}
	rows.Close()
	for _, q := range []string{`DELETE FROM events WHERE session_id=?`, `DELETE FROM approvals WHERE session_id=?`,
		`DELETE FROM session_rules WHERE session_id=?`, `DELETE FROM files WHERE session_id=?`, `DELETE FROM plans WHERE session_id=?`, `DELETE FROM sessions WHERE id=?`} {
		if _, err := tx.Exec(q, id); err != nil {
			return nil, err
		}
	}
	// The session this one continued is shown and resumable again (DESIGN §21.6).
	if _, err := tx.Exec(`UPDATE sessions SET resumed_by='' WHERE resumed_by=?`, id); err != nil {
		return nil, err
	}
	var orphans []string
	for _, h := range shas {
		var n int
		if tx.QueryRow(`SELECT COUNT(*) FROM files WHERE sha256=?`, h).Scan(&n) == nil && n == 0 {
			orphans = append(orphans, h)
		}
	}
	return orphans, tx.Commit()
}

// --- files (DESIGN §17.4)

type FileRow struct {
	ID, OwnerSub, SessionID, Name, Mime, SHA256, CreatedAt, Origin string
	Size                                                           int64
}

const fileCols = `f.id, f.owner_sub, f.session_id, f.name, f.mime, f.size, f.sha256, COALESCE(f.created_at,''), f.origin`

func scanFile(sc interface{ Scan(...any) error }) (*FileRow, error) {
	var f FileRow
	err := sc.Scan(&f.ID, &f.OwnerSub, &f.SessionID, &f.Name, &f.Mime, &f.Size, &f.SHA256, &f.CreatedAt, &f.Origin)
	return &f, err
}

func (s *Store) InsertFile(f FileRow) error {
	_, err := s.db.Exec(`INSERT INTO files(id,owner_sub,session_id,name,mime,size,sha256,created_at,origin) VALUES(?,?,?,?,?,?,?,?,?)`,
		f.ID, f.OwnerSub, f.SessionID, f.Name, f.Mime, f.Size, f.SHA256, now(), f.Origin)
	return err
}

func (s *Store) File(id string) (*FileRow, error) {
	return scanFile(s.db.QueryRow(`SELECT `+fileCols+` FROM files f WHERE id=?`, id))
}

// fileReferencedSQL is true for a file row f that a stored event of its session names.
const fileReferencedSQL = `EXISTS (SELECT 1 FROM events e WHERE e.session_id=f.session_id AND instr(e.raw, '"file_id":"' || f.id || '"') > 0)`

// ReferencedSessionFiles are the session's files that a stored message refers to:
// the attachments the panel lists (an upload that was never sent is not one).
func (s *Store) ReferencedSessionFiles(sessionID string) ([]FileRow, error) {
	rows, err := s.db.Query(`SELECT `+fileCols+` FROM files f WHERE session_id=? AND `+fileReferencedSQL+` ORDER BY created_at`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileRow
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// UnreferencedFilesBefore lists uploads created before cutoff (RFC 3339) that no
// stored message refers to: attached in the composer, then never sent.
func (s *Store) UnreferencedFilesBefore(cutoff string) ([]FileRow, error) {
	rows, err := s.db.Query(`SELECT `+fileCols+` FROM files f WHERE created_at < ? AND NOT `+fileReferencedSQL, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileRow
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// UserFileBytes is the quota measure: every file row of the user.
func (s *Store) UserFileBytes(sub string) int64 {
	var n int64
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(size),0) FROM files WHERE owner_sub=?`, sub).Scan(&n)
	return n
}

// FileReferenced reports whether a stored event of the file's session mentions it.
func (s *Store) FileReferenced(f *FileRow) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id=? AND instr(raw, ?) > 0`,
		f.SessionID, `"file_id":"`+f.ID+`"`).Scan(&n)
	return n > 0
}

// DeleteFile removes a row and reports whether its blob is now unused.
func (s *Store) DeleteFile(f *FileRow) (orphan bool, err error) {
	if _, err := s.db.Exec(`DELETE FROM files WHERE id=?`, f.ID); err != nil {
		return false, err
	}
	var n int
	err = s.db.QueryRow(`SELECT COUNT(*) FROM files WHERE sha256=?`, f.SHA256).Scan(&n)
	return err == nil && n == 0, err
}

// --- usage (DESIGN §17.6)

type UsageRow struct {
	TS, SessionID, Kind, OwnerSub, Model, MessageID string
	Input, CacheRead, CacheWrite, Output            int64
	Cost                                            float64
	// Seq is the event a code row came from; Provisional marks a row from an
	// assistant event, which the turn's result replaces (§17.13).
	Seq         int64
	Provisional bool
}

// Rates are [budgets] currency units per million tokens, used to recompute the cost of a merged row.
type Rates struct{ Input, Output, CacheRead, CacheWrite float64 }

// RecordUsage adds a row, or merges into the row of the same message id: Claude Code
// repeats a response's usage on every event of it, early ones with output 0, so each
// count keeps its largest value and the cost is recomputed from the merged counts.
func (s *Store) RecordUsage(u UsageRow, r Rates) error {
	if u.MessageID == "" {
		return errors.New("usage row without message id")
	}
	if u.TS == "" {
		u.TS = now()
	}
	// One statement, so the cost always matches the merged counts.
	_, err := s.db.Exec(`INSERT INTO usage(ts,session_id,kind,owner_sub,model,input,cache_read,cache_write,output,cost_usd,message_id,seq,provisional)
		VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,(?6*?10+?7*?11+?8*?12+?9*?13)/1e6,?14,?15,?16)
		ON CONFLICT(message_id) DO UPDATE SET input=max(input,excluded.input), cache_read=max(cache_read,excluded.cache_read),
		  cache_write=max(cache_write,excluded.cache_write), output=max(output,excluded.output),
		  cost_usd=(max(input,excluded.input)*?10+max(cache_read,excluded.cache_read)*?11+
		    max(cache_write,excluded.cache_write)*?12+max(output,excluded.output)*?13)/1e6`,
		u.TS, u.SessionID, u.Kind, u.OwnerSub, u.Model, u.Input, u.CacheRead, u.CacheWrite, u.Output,
		r.Input, r.CacheRead, r.CacheWrite, r.Output, u.MessageID, u.Seq, u.Provisional)
	return err
}

// ReplaceTurnUsage writes a code turn's exact rows (from result.modelUsage) and
// deletes the session's provisional rows from events strictly between fromSeq and
// toSeq, which those rows now account for. Rows are keyed by message id, so a
// repeated call changes nothing.
func (s *Store) ReplaceTurnUsage(sessionID string, fromSeq, toSeq int64, rows []UsageRow, rates []Rates) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM usage WHERE session_id=? AND provisional=1 AND seq>? AND seq<?`, sessionID, fromSeq, toSeq); err != nil {
		return err
	}
	for i, u := range rows {
		if u.TS == "" {
			u.TS = now()
		}
		r := rates[i]
		if _, err := tx.Exec(`INSERT INTO usage(ts,session_id,kind,owner_sub,model,input,cache_read,cache_write,output,cost_usd,message_id,seq,provisional)
			VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,(?6*?10+?7*?11+?8*?12+?9*?13)/1e6,?14,?15,0)
			ON CONFLICT(message_id) DO UPDATE SET input=excluded.input, cache_read=excluded.cache_read,
			  cache_write=excluded.cache_write, output=excluded.output, cost_usd=excluded.cost_usd`,
			u.TS, u.SessionID, u.Kind, u.OwnerSub, u.Model, u.Input, u.CacheRead, u.CacheWrite, u.Output,
			r.Input, r.CacheRead, r.CacheWrite, r.Output, u.MessageID, u.Seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UsageTotals sums a session's rows.
type UsageTotals struct {
	Input     int64   `json:"input"`
	CacheRead int64   `json:"cache_read"`
	CacheWr   int64   `json:"cache_write"`
	Output    int64   `json:"output"`
	Cost      float64 `json:"cost"`
	Responses int64   `json:"responses"`
}

func (s *Store) SessionUsage(sessionID string) UsageTotals {
	return s.usageWhere(`session_id=?`, sessionID)
}

// MonthUsage sums a user's rows of one UTC calendar month ("2026-10").
func (s *Store) MonthUsage(sub, month string) UsageTotals {
	return s.usageWhere(`owner_sub=? AND ts>=? AND ts<?`, sub, month, nextMonth(month))
}

func (s *Store) usageWhere(where string, args ...any) UsageTotals {
	var t UsageTotals
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(input),0), COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0),
		COALESCE(SUM(output),0), COALESCE(SUM(cost_usd),0), COUNT(*) FROM usage WHERE `+where, args...).
		Scan(&t.Input, &t.CacheRead, &t.CacheWr, &t.Output, &t.Cost, &t.Responses)
	return t
}

// UsageRows lists rows for tests and the later /usage page.
func (s *Store) UsageRows(sessionID string) ([]UsageRow, error) {
	rows, err := s.db.Query(`SELECT ts, COALESCE(session_id,''), COALESCE(kind,''), COALESCE(owner_sub,''), COALESCE(model,''),
		COALESCE(message_id,''), input, cache_read, cache_write, output, cost_usd, COALESCE(seq,0), COALESCE(provisional,0)
		FROM usage WHERE session_id=? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.TS, &u.SessionID, &u.Kind, &u.OwnerSub, &u.Model, &u.MessageID, &u.Input, &u.CacheRead,
			&u.CacheWrite, &u.Output, &u.Cost, &u.Seq, &u.Provisional); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ResetChatStates marks chat sessions idle after a hub start.
func (s *Store) ResetChatStates() {
	_, _ = s.db.Exec(`UPDATE sessions SET state='idle' WHERE (kind='chat' OR (kind='digest' AND COALESCE(machine_id,'')='')) AND state<>'idle'`)
}
