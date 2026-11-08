package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/kinginu/mado/internal/proto"
)

// HistoryFile is one imported Claude Code transcript (DESIGN §14). Files are keyed by
// (machine, path): the same claude session id can exist on two machines or in two
// config dirs, so lines hang off the hub's own file id.
type HistoryFile struct {
	ID                                                 int64
	MachineID, MachineName, Path, ConfigDir, SessionID string
	CWD, Title                                         string
	DefaultConfig, TitleCustom                         bool
	MTime, Size, Offset, Lines, BadLines               int64
}

func (f HistoryFile) Modified() string { return time.Unix(0, f.MTime).UTC().Format("2006-01-02 15:04") }

const historyCols = `f.id, f.machine_id, COALESCE(m.name,rm.name,''), f.path, f.config_dir, f.claude_session_id, f.cwd, f.title,
	f.default_config, f.title_custom, f.mtime, f.size, f.offset, f.lines, f.bad_lines`

func scanHistory(sc interface{ Scan(...any) error }) (*HistoryFile, error) {
	var f HistoryFile
	err := sc.Scan(&f.ID, &f.MachineID, &f.MachineName, &f.Path, &f.ConfigDir, &f.SessionID, &f.CWD, &f.Title,
		&f.DefaultConfig, &f.TitleCustom, &f.MTime, &f.Size, &f.Offset, &f.Lines, &f.BadLines)
	return &f, err
}

// HistoryStates is what the hub has of a machine's transcripts (sent as history_offsets).
func (s *Store) HistoryStates(machineID string) ([]proto.HistoryFileState, error) {
	rows, err := s.db.Query(`SELECT path, offset, size, mtime FROM history_files WHERE machine_id=?`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.HistoryFileState{}
	for rows.Next() {
		var f proto.HistoryFileState
		if err := rows.Scan(&f.Path, &f.Offset, &f.Size, &f.MTime); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// errHistoryOffset: the frame does not continue where the hub stands.
var errHistoryOffset = errors.New("history frame does not continue the stored offset")

// AppendHistory stores one history_session frame in one transaction. A frame must
// start at the stored offset (or at 0 with reset, or for a new file); otherwise
// nothing changes and errHistoryOffset is returned with the hub's state.
func (s *Store) AppendHistory(machineID string, m *proto.HistorySession) (*proto.HistoryFileState, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id, offset, size, mtime int64
	var title string
	var custom bool
	err = tx.QueryRow(`SELECT id, offset, size, mtime, title, title_custom FROM history_files WHERE machine_id=? AND path=?`,
		machineID, m.Path).Scan(&id, &offset, &size, &mtime, &title, &custom)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if m.Offset != 0 {
			return &proto.HistoryFileState{Path: m.Path}, errHistoryOffset
		}
		res, err := tx.Exec(`INSERT INTO history_files(machine_id, path, config_dir, default_config, claude_session_id, mtime, size, updated_at)
			VALUES(?,?,?,?,?,?,?,?)`, machineID, m.Path, m.ConfigDir, m.DefaultConfig, m.ClaudeSessionID, m.MTime, m.Size, now())
		if err != nil {
			return nil, err
		}
		id, _ = res.LastInsertId()
	case err != nil:
		return nil, err
	case m.Reset && m.Offset == 0:
		if _, err := tx.Exec(`DELETE FROM history_lines WHERE file_id=?`, id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE history_files SET offset=0, lines=0, bad_lines=0, title='', title_custom=0 WHERE id=?`, id); err != nil {
			return nil, err
		}
		title, custom = "", false
	case m.Offset != offset:
		return &proto.HistoryFileState{Path: m.Path, Offset: offset, Size: size, MTime: mtime}, errHistoryOffset
	}
	var added, bad int64
	ins, err := tx.Prepare(`INSERT OR IGNORE INTO history_lines(file_id, off, raw) VALUES(?,?,?)`)
	if err != nil {
		return nil, err
	}
	defer ins.Close()
	for i, raw := range m.Lines {
		res, err := ins.Exec(id, m.LineOffsets[i], raw)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			added++
			if !json.Valid([]byte(raw)) {
				bad++
			}
		}
	}
	// Title: a custom title (/rename) always wins; otherwise the first prompt sticks.
	if m.Title != "" && (m.TitleCustom || (title == "" && !custom)) {
		title, custom = m.Title, m.TitleCustom
	}
	_, err = tx.Exec(`UPDATE history_files SET offset=?, size=?, mtime=?, config_dir=?, default_config=?, claude_session_id=?,
		cwd=CASE WHEN ?<>'' THEN ? ELSE cwd END, title=?, title_custom=?, lines=lines+?, bad_lines=bad_lines+?, updated_at=? WHERE id=?`,
		m.End, m.Size, m.MTime, m.ConfigDir, m.DefaultConfig, m.ClaudeSessionID, m.CWD, m.CWD, title, custom, added, bad, now(), id)
	if err != nil {
		return nil, err
	}
	return nil, tx.Commit()
}

// HistoryFiles lists transcripts, newest first, optionally filtered by a substring of
// the title or cwd.
func (s *Store) HistoryFiles(q string, limit int) ([]*HistoryFile, error) {
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	rows, err := s.db.Query(`SELECT `+historyCols+` FROM history_files f LEFT JOIN machines m ON m.id=f.machine_id LEFT JOIN removed_machines rm ON rm.id=f.machine_id
		WHERE (?='' OR f.title LIKE ? ESCAPE '\' OR f.cwd LIKE ? ESCAPE '\')
		ORDER BY COALESCE(m.name,rm.name), f.mtime DESC LIMIT ?`, q, like, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*HistoryFile
	for rows.Next() {
		f, err := scanHistory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) HistoryFile(id int64) (*HistoryFile, error) {
	return scanHistory(s.db.QueryRow(`SELECT `+historyCols+` FROM history_files f LEFT JOIN machines m ON m.id=f.machine_id LEFT JOIN removed_machines rm ON rm.id=f.machine_id WHERE f.id=?`, id))
}

// HistoryLines returns up to limit lines after byte offset after (-1: from the start).
func (s *Store) HistoryLines(fileID, after int64, limit int) ([]StoredEvent, error) {
	rows, err := s.db.Query(`SELECT off, raw FROM history_lines WHERE file_id=? AND off>? ORDER BY off LIMIT ?`, fileID, after, limit)
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

// HistoryTailStart is the offset to pass to HistoryLines to get the last n lines.
func (s *Store) HistoryTailStart(fileID int64, n int) int64 {
	var off sql.NullInt64
	_ = s.db.QueryRow(`SELECT off FROM history_lines WHERE file_id=? ORDER BY off DESC LIMIT 1 OFFSET ?`, fileID, n).Scan(&off)
	if !off.Valid {
		return -1
	}
	return off.Int64
}

// HistoryTotals is for the list page header.
func (s *Store) HistoryTotals() (files, lines, bytes, bad int64) {
	_ = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(lines),0), COALESCE(SUM(offset),0), COALESCE(SUM(bad_lines),0) FROM history_files`).
		Scan(&files, &lines, &bytes, &bad)
	return
}
