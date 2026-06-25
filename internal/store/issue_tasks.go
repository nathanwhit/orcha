package store

import (
	"database/sql"
	"errors"
)

// RecordIssueTask claims an issue trigger for processing, returning whether this
// call is the one that claimed it. The unique (repo, number, external_id) index
// makes the claim atomic: a concurrent poll or a post-restart re-poll sees
// inserted=false and skips, so each @-mention or assignment spawns at most one
// objective. The objective id is filled in afterward via SetIssueTaskObjective.
func (s *Store) RecordIssueTask(repo string, number int, externalID string) (inserted bool, err error) {
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO issue_tasks(id, repo, number, external_id, created_at)
		 VALUES(?,?,?,?,?)`,
		s.NewID(), repo, number, externalID, s.Now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ActiveObjectiveForIssue returns the id of a non-terminal objective already
// tracking (repo, number), or "" if none. The issue-trigger path uses it to
// avoid spawning a duplicate objective when an issue that is already being worked
// is re-triggered (a re-assignment or new @-mention mints a fresh trigger event).
// Terminal objectives (succeeded/failed/canceled) are ignored, so a later
// re-trigger can start fresh work once the prior attempt is done.
func (s *Store) ActiveObjectiveForIssue(repo string, number int) (string, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT o.id FROM issue_tasks t JOIN objectives o ON o.id = t.objective_id
		 WHERE t.repo = ? AND t.number = ? AND o.status IN ('active','waiting_user')
		 ORDER BY o.created_at DESC LIMIT 1`, repo, number).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// IssueForObjective returns the (repo, number) of the issue that produced the
// objective, or ok=false when the objective wasn't triggered from an issue. Only
// the trigger that minted the objective has its objective_id set
// (SetIssueTaskObjective); routed follow-up comments leave objective_id empty, so
// this resolves to the single source issue.
func (s *Store) IssueForObjective(objectiveID string) (repo string, number int, ok bool, err error) {
	if objectiveID == "" {
		return "", 0, false, nil
	}
	err = s.db.QueryRow(
		`SELECT repo, number FROM issue_tasks WHERE objective_id = ? ORDER BY created_at LIMIT 1`,
		objectiveID).Scan(&repo, &number)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return repo, number, true, nil
}

// SetIssueTaskObjective records which objective a claimed trigger produced.
func (s *Store) SetIssueTaskObjective(repo string, number int, externalID, objectiveID string) error {
	_, err := s.db.Exec(
		`UPDATE issue_tasks SET objective_id = ? WHERE repo = ? AND number = ? AND external_id = ?`,
		objectiveID, repo, number, externalID)
	return err
}

// DeleteIssueTask drops a claim, so a later poll retries. Used to roll back when
// objective creation fails after the claim was recorded.
func (s *Store) DeleteIssueTask(repo string, number int, externalID string) error {
	_, err := s.db.Exec(
		`DELETE FROM issue_tasks WHERE repo = ? AND number = ? AND external_id = ?`,
		repo, number, externalID)
	return err
}
