package store

import (
	"database/sql"
	"errors"

	"github.com/nathanwhit/orcha/internal/model"
)

// CreateSession inserts a new session.
func (s *Store) CreateSession(sess *model.Session) error {
	if sess.ID == "" {
		sess.ID = s.NewID()
	}
	now := s.now()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	sess.UpdatedAt = now
	if sess.Status == "" {
		sess.Status = model.SessionQueued
	}
	if sess.Mode == "" {
		sess.Mode = model.ModeInteractive
	}
	_, err := s.db.Exec(
		`INSERT INTO sessions(id, objective_id, parent_session_id, role, agent, mode,
		   status, title, goal, current_activity, latest_summary, target_id,
		   workspace_id, usage_provider, created_at, started_at, updated_at,
		   completed_at, metadata)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sess.ID, nullStr(sess.ObjectiveID), nullStr(sess.ParentSessionID),
		string(sess.Role), string(sess.Agent), string(sess.Mode), string(sess.Status),
		sess.Title, sess.Goal, sess.CurrentActivity, sess.LatestSummary,
		nullStr(sess.TargetID), nullStr(sess.WorkspaceID), nullStr(sess.UsageProvider),
		sess.CreatedAt, nullTime(sess.StartedAt), sess.UpdatedAt,
		nullTime(sess.CompletedAt), sess.Metadata,
	)
	return err
}

var sessionCols = `id, objective_id, parent_session_id, role, agent, mode, status,
	title, goal, current_activity, latest_summary, target_id, workspace_id,
	usage_provider, created_at, started_at, updated_at, completed_at, metadata`

func scanSession(r rowScanner) (*model.Session, error) {
	var s model.Session
	var obj, parent, target, ws, provider sql.NullString
	var started, completed sql.NullTime
	err := r.Scan(&s.ID, &obj, &parent, &s.Role, &s.Agent, &s.Mode, &s.Status,
		&s.Title, &s.Goal, &s.CurrentActivity, &s.LatestSummary, &target, &ws,
		&provider, &s.CreatedAt, &started, &s.UpdatedAt, &completed, &s.Metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.ObjectiveID = obj.String
	s.ParentSessionID = parent.String
	s.TargetID = target.String
	s.WorkspaceID = ws.String
	s.UsageProvider = provider.String
	s.StartedAt = timePtr(started)
	s.CompletedAt = timePtr(completed)
	return &s, nil
}

// GetSession fetches one session.
func (s *Store) GetSession(id string) (*model.Session, error) {
	row := s.db.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

// ListSessions returns all sessions, newest first.
func (s *Store) ListSessions() ([]*model.Session, error) {
	return s.querySessions(`SELECT ` + sessionCols + ` FROM sessions ORDER BY created_at DESC`)
}

// ListSessionsByObjective returns sessions for one objective.
func (s *Store) ListSessionsByObjective(objectiveID string) ([]*model.Session, error) {
	return s.querySessions(`SELECT `+sessionCols+` FROM sessions WHERE objective_id = ? ORDER BY created_at ASC`, objectiveID)
}

// ListChildSessions returns sessions whose parent is the given session.
func (s *Store) ListChildSessions(parentID string) ([]*model.Session, error) {
	return s.querySessions(`SELECT `+sessionCols+` FROM sessions WHERE parent_session_id = ?`, parentID)
}

func (s *Store) querySessions(q string, args ...any) ([]*model.Session, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// UpdateSessionStatus atomically transitions a session, enforcing the state
// machine inside a transaction. A late completion arriving after cancellation
// fails validation and the terminal status is preserved.
//
// It returns the (possibly unchanged) session and an error. When the
// transition is illegal it returns the current session plus the validation
// error so callers can record the late/ignored completion without mutating
// state.
func (s *Store) UpdateSessionStatus(id string, next model.SessionStatus) (*model.Session, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	row := tx.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if err != nil {
		return nil, err
	}
	if err := model.ValidateSessionTransition(sess.Status, next); err != nil {
		return sess, err
	}
	if sess.Status == next {
		return sess, tx.Commit()
	}

	now := s.now()
	var started any
	if next == model.SessionRunning && sess.StartedAt == nil {
		started = now
	}
	var completed any
	if next.IsTerminal() {
		completed = now
	}
	_, err = tx.Exec(
		`UPDATE sessions SET status = ?,
		   started_at = COALESCE(started_at, ?),
		   completed_at = COALESCE(?, completed_at),
		   updated_at = ? WHERE id = ?`,
		string(next), started, completed, now, id)
	if err != nil {
		return nil, err
	}
	sess.Status = next
	sess.UpdatedAt = now
	if next.IsTerminal() {
		sess.CompletedAt = &now
	}
	return sess, tx.Commit()
}

// UpdateSessionRuntime updates the soft, frequently-changing fields that do not
// affect the state machine (activity line, summary, target/workspace binding).
func (s *Store) UpdateSessionRuntime(id string, fn func(*model.Session)) (*model.Session, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	row := tx.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if err != nil {
		return nil, err
	}
	fn(sess)
	now := s.now()
	sess.UpdatedAt = now
	_, err = tx.Exec(
		`UPDATE sessions SET title = ?, goal = ?, current_activity = ?,
		   latest_summary = ?, target_id = ?, workspace_id = ?,
		   usage_provider = ?, metadata = ?, updated_at = ? WHERE id = ?`,
		sess.Title, sess.Goal, sess.CurrentActivity, sess.LatestSummary,
		nullStr(sess.TargetID), nullStr(sess.WorkspaceID), nullStr(sess.UsageProvider),
		sess.Metadata, now, id)
	if err != nil {
		return nil, err
	}
	return sess, tx.Commit()
}
