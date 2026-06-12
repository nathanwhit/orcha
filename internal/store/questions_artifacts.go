package store

import (
	"database/sql"
	"errors"

	"github.com/nathanwhit/orcha/internal/model"
)

// ---------------------------------------------------------------------------
// Questions
// ---------------------------------------------------------------------------

// CreateQuestion inserts a user question.
func (s *Store) CreateQuestion(q *model.Question) error {
	if q.ID == "" {
		q.ID = s.NewID()
	}
	if q.CreatedAt.IsZero() {
		q.CreatedAt = s.now()
	}
	if q.Status == "" {
		q.Status = model.QuestionOpen
	}
	_, err := s.db.Exec(
		`INSERT INTO questions(id, objective_id, session_id, status, priority,
		   question, context, answer, created_at, answered_at, metadata)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		q.ID, nullStr(q.ObjectiveID), nullStr(q.SessionID), string(q.Status),
		q.Priority, q.Question, q.Context, q.Answer, q.CreatedAt,
		nullTime(q.AnsweredAt), q.Metadata)
	return err
}

var questionCols = `id, objective_id, session_id, status, priority, question,
	context, answer, created_at, answered_at, metadata`

func scanQuestion(r rowScanner) (*model.Question, error) {
	var q model.Question
	var obj, sess sql.NullString
	var answered sql.NullTime
	err := r.Scan(&q.ID, &obj, &sess, &q.Status, &q.Priority, &q.Question,
		&q.Context, &q.Answer, &q.CreatedAt, &answered, &q.Metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	q.ObjectiveID = obj.String
	q.SessionID = sess.String
	q.AnsweredAt = timePtr(answered)
	return &q, nil
}

// GetQuestion fetches one question.
func (s *Store) GetQuestion(id string) (*model.Question, error) {
	row := s.db.QueryRow(`SELECT `+questionCols+` FROM questions WHERE id = ?`, id)
	return scanQuestion(row)
}

// ListOpenQuestions returns open questions, highest priority first. This backs
// the needs-user queue and is a small-row query.
func (s *Store) ListOpenQuestions() ([]*model.Question, error) {
	return s.queryQuestions(
		`SELECT ` + questionCols + ` FROM questions WHERE status = 'open' ORDER BY priority DESC, created_at ASC`)
}

// ListQuestionsByObjective returns all questions for an objective.
func (s *Store) ListQuestionsByObjective(objectiveID string) ([]*model.Question, error) {
	return s.queryQuestions(`SELECT `+questionCols+` FROM questions WHERE objective_id = ? ORDER BY created_at ASC`, objectiveID)
}

func (s *Store) queryQuestions(q string, args ...any) ([]*model.Question, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Question
	for rows.Next() {
		qq, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, qq)
	}
	return out, rows.Err()
}

// AnswerQuestion records an answer and marks the question answered. Only open
// questions may be answered.
func (s *Store) AnswerQuestion(id, answer string) (*model.Question, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	row := tx.QueryRow(`SELECT `+questionCols+` FROM questions WHERE id = ?`, id)
	q, err := scanQuestion(row)
	if err != nil {
		return nil, err
	}
	if q.Status != model.QuestionOpen {
		return q, errors.New("store: question is not open")
	}
	now := s.now()
	q.Status = model.QuestionAnswered
	q.Answer = answer
	q.AnsweredAt = &now
	_, err = tx.Exec(
		`UPDATE questions SET status = ?, answer = ?, answered_at = ? WHERE id = ?`,
		string(model.QuestionAnswered), answer, now, id)
	if err != nil {
		return nil, err
	}
	return q, tx.Commit()
}

// ---------------------------------------------------------------------------
// Artifacts
// ---------------------------------------------------------------------------

// CreateArtifact inserts a durable output.
func (s *Store) CreateArtifact(a *model.Artifact) error {
	if a.ID == "" {
		a.ID = s.NewID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = s.now()
	}
	if a.Visibility == "" {
		a.Visibility = model.VisibilitySecondary
	}
	_, err := s.db.Exec(
		`INSERT INTO artifacts(id, objective_id, session_id, kind, title, summary,
		   uri, visibility, created_at, metadata)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		a.ID, nullStr(a.ObjectiveID), nullStr(a.SessionID), string(a.Kind),
		a.Title, a.Summary, a.URI, string(a.Visibility), a.CreatedAt, a.Metadata)
	return err
}

// ListArtifactsByObjective returns artifacts for an objective, primary first.
func (s *Store) ListArtifactsByObjective(objectiveID string) ([]*model.Artifact, error) {
	rows, err := s.db.Query(
		`SELECT id, objective_id, session_id, kind, title, summary, uri, visibility, created_at, metadata
		 FROM artifacts WHERE objective_id = ?
		 ORDER BY CASE visibility WHEN 'primary' THEN 0 WHEN 'secondary' THEN 1 ELSE 2 END, created_at DESC`,
		objectiveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Artifact
	for rows.Next() {
		a := &model.Artifact{}
		var obj, sess sql.NullString
		if err := rows.Scan(&a.ID, &obj, &sess, &a.Kind, &a.Title, &a.Summary,
			&a.URI, &a.Visibility, &a.CreatedAt, &a.Metadata); err != nil {
			return nil, err
		}
		a.ObjectiveID = obj.String
		a.SessionID = sess.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// CancelOpenQuestionsByObjective closes every open question on an objective —
// a canceled objective must not keep demanding the user's attention.
func (s *Store) CancelOpenQuestionsByObjective(objectiveID string) error {
	_, err := s.db.Exec(
		`UPDATE questions SET status = ? WHERE objective_id = ? AND status = ?`,
		string(model.QuestionCanceled), objectiveID, string(model.QuestionOpen))
	return err
}
