package store

import (
	"database/sql"
	"errors"

	"github.com/nathanwhit/orcha/internal/model"
)

// CreateObjective inserts a new objective. ID/timestamps are filled if empty.
func (s *Store) CreateObjective(o *model.Objective) error {
	if o.ID == "" {
		o.ID = s.NewID()
	}
	now := s.now()
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	}
	o.UpdatedAt = now
	if o.Status == "" {
		o.Status = model.ObjectiveActive
	}
	_, err := s.db.Exec(
		`INSERT INTO objectives(id, title, prompt, status, manager_session_id,
		   created_at, updated_at, completed_at, summary, metadata)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.Title, o.Prompt, string(o.Status), nullStr(o.ManagerSessionID),
		o.CreatedAt, o.UpdatedAt, nullTime(o.CompletedAt), o.Summary, o.Metadata,
	)
	return err
}

// GetObjective fetches one objective.
func (s *Store) GetObjective(id string) (*model.Objective, error) {
	row := s.db.QueryRow(
		`SELECT id, title, prompt, status, manager_session_id, created_at,
		   updated_at, completed_at, summary, metadata
		 FROM objectives WHERE id = ?`, id)
	return scanObjective(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanObjective(r rowScanner) (*model.Objective, error) {
	var o model.Objective
	var mgr sql.NullString
	var completed sql.NullTime
	err := r.Scan(&o.ID, &o.Title, &o.Prompt, &o.Status, &mgr, &o.CreatedAt,
		&o.UpdatedAt, &completed, &o.Summary, &o.Metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	o.ManagerSessionID = mgr.String
	o.CompletedAt = timePtr(completed)
	return &o, nil
}

// ListObjectives returns all objectives, newest first.
func (s *Store) ListObjectives() ([]*model.Objective, error) {
	rows, err := s.db.Query(
		`SELECT id, title, prompt, status, manager_session_id, created_at,
		   updated_at, completed_at, summary, metadata
		 FROM objectives ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Objective
	for rows.Next() {
		o, err := scanObjective(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetObjectiveManager records the manager session id.
func (s *Store) SetObjectiveManager(objectiveID, sessionID string) error {
	_, err := s.db.Exec(
		`UPDATE objectives SET manager_session_id = ?, updated_at = ? WHERE id = ?`,
		sessionID, s.now(), objectiveID)
	return err
}

// UpdateObjectiveTitle replaces an objective's title. Used by asynchronous
// title generation after an objective is created with a provisional title; the
// polling dashboard picks the new value up on its next refresh.
func (s *Store) UpdateObjectiveTitle(id, title string) error {
	_, err := s.db.Exec(
		`UPDATE objectives SET title = ?, updated_at = ? WHERE id = ?`,
		title, s.now(), id)
	return err
}

// UpdateObjectiveStatus transitions an objective, enforcing the state machine.
func (s *Store) UpdateObjectiveStatus(id string, next model.ObjectiveStatus, summary string) error {
	o, err := s.GetObjective(id)
	if err != nil {
		return err
	}
	if err := model.ValidateObjectiveTransition(o.Status, next); err != nil {
		return err
	}
	now := s.now()
	var completed any
	if next.IsTerminal() {
		completed = now
	}
	if summary == "" {
		summary = o.Summary
	}
	_, err = s.db.Exec(
		`UPDATE objectives SET status = ?, summary = ?, completed_at = COALESCE(?, completed_at), updated_at = ? WHERE id = ?`,
		string(next), summary, completed, now, id)
	return err
}
