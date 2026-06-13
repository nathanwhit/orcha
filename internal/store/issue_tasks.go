package store

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
