package hub

// Rate-limit state of code sessions (DESIGN §26).

// SetRateLimit stores the latest rate_limit_info of a session (JSON).
func (s *Store) SetRateLimit(id, info string) error {
	_, err := s.db.Exec(`UPDATE sessions SET rate_limit=? WHERE id=?`, info, id)
	return err
}

// SetLimited records a rejection: the reset time and window. afterSeq, the seq the
// re-sent messages start after, is kept from the first rejection while the session
// stays limited.
func (s *Store) SetLimited(id string, until int64, typ string, afterSeq int64) error {
	_, err := s.db.Exec(`UPDATE sessions SET limited_seq=CASE WHEN COALESCE(limited_until,0)=0 THEN ? ELSE limited_seq END,
		limited_until=?, limited_type=? WHERE id=?`, afterSeq, until, typ, id)
	return err
}

// ClearLimited ends the limited state.
func (s *Store) ClearLimited(id string) error {
	_, err := s.db.Exec(`UPDATE sessions SET limited_until=0, limited_type='', limited_seq=0 WHERE id=?`, id)
	return err
}

// SetAutoResume flips the per-session auto-resume switch.
func (s *Store) SetAutoResume(id string, on bool) error {
	_, err := s.db.Exec(`UPDATE sessions SET auto_resume=? WHERE id=?`, on, id)
	return err
}

// SetResumeAttempts stores the number of consecutive automatic resumes.
func (s *Store) SetResumeAttempts(id string, n int) error {
	_, err := s.db.Exec(`UPDATE sessions SET resume_attempts=? WHERE id=?`, n, id)
	return err
}

// LimitedSessionIDs lists the code sessions waiting for an automatic resume.
func (s *Store) LimitedSessionIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM sessions WHERE COALESCE(limited_until,0)>0 AND COALESCE(auto_resume,1)=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// EventsDownFrom returns up to n events with seq <= seq, newest first.
func (s *Store) EventsDownFrom(sessionID string, seq int64, n int) ([]StoredEvent, error) {
	rows, err := s.db.Query(`SELECT seq, raw FROM events WHERE session_id=? AND seq<=? ORDER BY seq DESC LIMIT ?`, sessionID, seq, n)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// EchoesAfter returns the user's own messages (the shim's "mado":"echo" events) after
// seq, oldest first.
func (s *Store) EchoesAfter(sessionID string, seq int64) ([]StoredEvent, error) {
	rows, err := s.db.Query(`SELECT seq, raw FROM events WHERE session_id=? AND seq>? AND instr(raw, '"mado":"echo"') > 0 ORDER BY seq`, sessionID, seq)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}
