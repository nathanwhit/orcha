package store

import (
	"database/sql"
	"errors"

	"github.com/nathanwhit/orcha/internal/model"
)

// CreatePR inserts a pull request.
func (s *Store) CreatePR(p *model.PullRequest) error {
	if p.ID == "" {
		p.ID = s.NewID()
	}
	now := s.now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	if p.Status == "" {
		p.Status = model.PROpen
	}
	if p.ChecksState == "" {
		p.ChecksState = model.ChecksUnknown
	}
	_, err := s.db.Exec(
		`INSERT INTO pull_requests(id, objective_id, created_by_session_id, repo,
		   number, url, branch, base_branch, head_sha, status, checks_state,
		   title, summary, last_synced_at, created_at, updated_at, metadata)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, nullStr(p.ObjectiveID), nullStr(p.CreatedBySessionID), p.Repo,
		p.Number, p.URL, p.Branch, p.BaseBranch, p.HeadSHA, string(p.Status),
		string(p.ChecksState), p.Title, p.Summary, nullTime(p.LastSyncedAt),
		p.CreatedAt, p.UpdatedAt, p.Metadata,
	)
	return err
}

var prCols = `id, objective_id, created_by_session_id, repo, number, url, branch,
	base_branch, head_sha, status, checks_state, title, summary, last_synced_at,
	created_at, updated_at, metadata`

func scanPR(r rowScanner) (*model.PullRequest, error) {
	var p model.PullRequest
	var obj, sess sql.NullString
	var synced sql.NullTime
	err := r.Scan(&p.ID, &obj, &sess, &p.Repo, &p.Number, &p.URL, &p.Branch,
		&p.BaseBranch, &p.HeadSHA, &p.Status, &p.ChecksState, &p.Title, &p.Summary,
		&synced, &p.CreatedAt, &p.UpdatedAt, &p.Metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.ObjectiveID = obj.String
	p.CreatedBySessionID = sess.String
	p.LastSyncedAt = timePtr(synced)
	return &p, nil
}

// GetPR fetches one pull request.
func (s *Store) GetPR(id string) (*model.PullRequest, error) {
	row := s.db.QueryRow(`SELECT `+prCols+` FROM pull_requests WHERE id = ?`, id)
	return scanPR(row)
}

// ListPRs returns all pull requests.
func (s *Store) ListPRs() ([]*model.PullRequest, error) {
	return s.queryPRs(`SELECT ` + prCols + ` FROM pull_requests ORDER BY created_at DESC`)
}

// ListPRsByObjective returns pull requests for one objective.
func (s *Store) ListPRsByObjective(objectiveID string) ([]*model.PullRequest, error) {
	return s.queryPRs(`SELECT `+prCols+` FROM pull_requests WHERE objective_id = ? ORDER BY created_at ASC`, objectiveID)
}

// GetPRByRepoNumber returns the (earliest) PR row for a repo+number, or
// ErrNotFound. Used to avoid recording the same host PR twice.
func (s *Store) GetPRByRepoNumber(repo string, number int) (*model.PullRequest, error) {
	row := s.db.QueryRow(`SELECT `+prCols+` FROM pull_requests WHERE repo = ? AND number = ? ORDER BY created_at ASC LIMIT 1`, repo, number)
	return scanPR(row)
}

// DeduplicatePRs removes rows that duplicate an existing (repo, number),
// keeping the earliest of each. Numbers <= 0 (not yet assigned) are left alone.
// Returns how many rows were deleted. Run at startup to heal any duplicates an
// earlier race recorded.
func (s *Store) DeduplicatePRs() (int, error) {
	res, err := s.db.Exec(`
		DELETE FROM pull_requests
		WHERE number > 0 AND id NOT IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (
					PARTITION BY repo, number ORDER BY created_at ASC, id ASC
				) AS rn
				FROM pull_requests WHERE number > 0
			) WHERE rn = 1
		)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) queryPRs(q string, args ...any) ([]*model.PullRequest, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.PullRequest
	for rows.Next() {
		p, err := scanPR(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePR applies a mutation function and persists the result, refreshing
// updated_at.
func (s *Store) UpdatePR(id string, fn func(*model.PullRequest)) (*model.PullRequest, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	row := tx.QueryRow(`SELECT `+prCols+` FROM pull_requests WHERE id = ?`, id)
	p, err := scanPR(row)
	if err != nil {
		return nil, err
	}
	fn(p)
	now := s.now()
	p.UpdatedAt = now
	_, err = tx.Exec(
		`UPDATE pull_requests SET status = ?, checks_state = ?, head_sha = ?,
		   title = ?, summary = ?, url = ?, number = ?, last_synced_at = ?,
		   updated_at = ?, metadata = ? WHERE id = ?`,
		string(p.Status), string(p.ChecksState), p.HeadSHA, p.Title, p.Summary,
		p.URL, p.Number, nullTime(p.LastSyncedAt), now, p.Metadata, id)
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// ---------------------------------------------------------------------------
// PR feedback
// ---------------------------------------------------------------------------

// RecordFeedback stores a PR feedback item. The unique (pr_id, kind,
// external_id) index dedups re-polled events; a duplicate returns
// (existing? , false, nil) style is simplified to a boolean inserted flag.
func (s *Store) RecordFeedback(f *model.PRFeedback) (inserted bool, err error) {
	if f.ID == "" {
		f.ID = s.NewID()
	}
	if f.CreatedAt.IsZero() {
		f.CreatedAt = s.now()
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO pr_feedback(id, pr_id, kind, external_id, body,
		   actionable, handled, session_id, created_at, metadata)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		f.ID, f.PRID, f.Kind, f.ExternalID, f.Body, boolInt(f.Actionable),
		boolInt(f.Handled), nullStr(f.SessionID), f.CreatedAt, f.Metadata)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UnhandledFeedback returns actionable, unhandled feedback for a PR.
func (s *Store) UnhandledFeedback(prID string) ([]*model.PRFeedback, error) {
	rows, err := s.db.Query(
		`SELECT id, pr_id, kind, external_id, body, actionable, handled, session_id, created_at, metadata
		 FROM pr_feedback WHERE pr_id = ? AND handled = 0 AND actionable = 1 ORDER BY created_at ASC`, prID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.PRFeedback
	for rows.Next() {
		f := &model.PRFeedback{}
		var sess sql.NullString
		if err := rows.Scan(&f.ID, &f.PRID, &f.Kind, &f.ExternalID, &f.Body,
			&f.Actionable, &f.Handled, &sess, &f.CreatedAt, &f.Metadata); err != nil {
			return nil, err
		}
		f.SessionID = sess.String
		out = append(out, f)
	}
	return out, rows.Err()
}

// MarkFeedbackHandled flags a feedback item handled and links the session.
func (s *Store) MarkFeedbackHandled(id, sessionID string) error {
	_, err := s.db.Exec(
		`UPDATE pr_feedback SET handled = 1, session_id = ? WHERE id = ?`,
		nullStr(sessionID), id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
