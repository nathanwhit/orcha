package store

import (
	"database/sql"
	"time"
)

// ObjectiveRow is a dense dashboard row. It deliberately contains only small
// scalar columns and aggregate counts — never transcript content — so the
// initial dashboard load stays fast regardless of how large session logs grow.
type ObjectiveRow struct {
	ID             string    `json:"id"`
	Status         string    `json:"status"`
	Title          string    `json:"title"`
	Repo           string    `json:"repo"`
	ActiveSessions int       `json:"active_sessions"`
	PRCount        int       `json:"pr_count"`
	OpenQuestions  int       `json:"open_questions"`
	NeedsUser      bool      `json:"needs_user"`
	LatestActivity string    `json:"latest_activity"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// DashboardObjectives returns the dense objective list for the main dashboard.
// It performs only small aggregate queries and never selects message content.
func (s *Store) DashboardObjectives() ([]ObjectiveRow, error) {
	rows, err := s.db.Query(`
		SELECT
		  o.id, o.status, o.title, o.updated_at,
		  COALESCE((SELECT COUNT(*) FROM sessions se
		            WHERE se.objective_id = o.id
		              AND se.status IN ('queued','starting','running','waiting_user','waiting_capacity')), 0) AS active_sessions,
		  COALESCE((SELECT COUNT(*) FROM pull_requests p WHERE p.objective_id = o.id), 0) AS pr_count,
		  COALESCE((SELECT COUNT(*) FROM questions q WHERE q.objective_id = o.id AND q.status = 'open'), 0) AS open_questions,
		  COALESCE(
		    (SELECT p.repo FROM pull_requests p WHERE p.objective_id = o.id ORDER BY p.created_at ASC LIMIT 1),
		    json_extract(o.metadata, '$.repo'),
		    '') AS repo,
		  COALESCE((SELECT se.current_activity FROM sessions se
		            WHERE se.objective_id = o.id AND se.current_activity <> ''
		            ORDER BY se.updated_at DESC LIMIT 1), '') AS latest_activity
		FROM objectives o
		ORDER BY o.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectiveRow
	for rows.Next() {
		var r ObjectiveRow
		var repo, activity sql.NullString
		if err := rows.Scan(&r.ID, &r.Status, &r.Title, &r.UpdatedAt,
			&r.ActiveSessions, &r.PRCount, &r.OpenQuestions, &repo, &activity); err != nil {
			return nil, err
		}
		r.Repo = repo.String
		r.LatestActivity = activity.String
		r.NeedsUser = r.OpenQuestions > 0 || r.Status == "waiting_user"
		out = append(out, r)
	}
	return out, rows.Err()
}

// SessionRow is a compact session row for dashboards. No transcript content.
type SessionRow struct {
	ID              string    `json:"id"`
	ObjectiveID     string    `json:"objective_id"`
	Status          string    `json:"status"`
	Role            string    `json:"role"`
	Agent           string    `json:"agent"`
	Mode            string    `json:"mode"`
	Title           string    `json:"title"`
	TargetID        string    `json:"target_id"`
	CurrentActivity string    `json:"current_activity"`
	UsedTokens      int64     `json:"used_tokens"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// DashboardSessions returns compact session rows, optionally filtered by
// objective. It selects no message/transcript columns.
func (s *Store) DashboardSessions(objectiveID string) ([]SessionRow, error) {
	q := `SELECT id, COALESCE(objective_id,''), status, role, agent, mode, title,
	         COALESCE(target_id,''), current_activity, used_tokens, updated_at
	      FROM sessions`
	var args []any
	if objectiveID != "" {
		q += ` WHERE objective_id = ?`
		args = append(args, objectiveID)
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(&r.ID, &r.ObjectiveID, &r.Status, &r.Role, &r.Agent,
			&r.Mode, &r.Title, &r.TargetID, &r.CurrentActivity, &r.UsedTokens, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
