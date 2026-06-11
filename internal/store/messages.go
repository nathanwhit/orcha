package store

import (
	"github.com/nathanwhit/orcha/internal/model"
)

// AppendMessage adds a transcript row, assigning the next seq for the session.
// Transcripts are stored separately from session rows and are never joined into
// dashboard queries.
func (s *Store) AppendMessage(m *model.Message) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var maxSeq int64
	// COALESCE handles the first message (no rows yet).
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(seq), 0) FROM session_messages WHERE session_id = ?`,
		m.SessionID).Scan(&maxSeq); err != nil {
		return err
	}
	m.Seq = maxSeq + 1
	if m.ID == "" {
		m.ID = s.NewID()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = s.now()
	}
	_, err = tx.Exec(
		`INSERT INTO session_messages(id, session_id, seq, source, kind, content, metadata, created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		m.ID, m.SessionID, m.Seq, string(m.Source), string(m.Kind), m.Content, m.Metadata, m.CreatedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// MessagesAfter returns transcript rows for a session with seq greater than
// after, ordered oldest-first. This is the incremental cursor used by the
// session view and the streaming endpoint.
func (s *Store) MessagesAfter(sessionID string, after int64, limit int) ([]model.Message, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(
		`SELECT id, session_id, seq, source, kind, content, metadata, created_at
		 FROM session_messages WHERE session_id = ? AND seq > ?
		 ORDER BY seq ASC LIMIT ?`, sessionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Message
	for rows.Next() {
		var m model.Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Seq, &m.Source, &m.Kind,
			&m.Content, &m.Metadata, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LatestSeq returns the highest seq stored for a session (0 if none).
func (s *Store) LatestSeq(sessionID string) (int64, error) {
	var seq int64
	err := s.db.QueryRow(
		`SELECT COALESCE(MAX(seq), 0) FROM session_messages WHERE session_id = ?`,
		sessionID).Scan(&seq)
	return seq, err
}
