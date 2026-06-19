package store

import (
	"database/sql"
	"errors"
	"strings"

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
		   status, title, goal, current_activity, latest_summary, handoff_summary,
		   target_id, workspace_id, usage_provider, used_tokens, created_at, started_at,
		   updated_at, completed_at, metadata)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sess.ID, nullStr(sess.ObjectiveID), nullStr(sess.ParentSessionID),
		string(sess.Role), string(sess.Agent), string(sess.Mode), string(sess.Status),
		sess.Title, sess.Goal, sess.CurrentActivity, sess.LatestSummary, sess.HandoffSummary,
		nullStr(sess.TargetID), nullStr(sess.WorkspaceID), nullStr(sess.UsageProvider),
		sess.UsedTokens, sess.CreatedAt, nullTime(sess.StartedAt), sess.UpdatedAt,
		nullTime(sess.CompletedAt), sess.Metadata,
	)
	return err
}

var sessionCols = `id, objective_id, parent_session_id, role, agent, mode, status,
	title, goal, current_activity, latest_summary, handoff_summary, target_id, workspace_id,
	usage_provider, used_tokens, created_at, started_at, updated_at, completed_at, metadata`

func scanSession(r rowScanner) (*model.Session, error) {
	var s model.Session
	var obj, parent, target, ws, provider sql.NullString
	var started, completed sql.NullTime
	err := r.Scan(&s.ID, &obj, &parent, &s.Role, &s.Agent, &s.Mode, &s.Status,
		&s.Title, &s.Goal, &s.CurrentActivity, &s.LatestSummary, &s.HandoffSummary, &target, &ws,
		&provider, &s.UsedTokens, &s.CreatedAt, &started, &s.UpdatedAt, &completed, &s.Metadata)
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

// ListSessionsByStatuses returns sessions in any of the given statuses, oldest
// first. Used by the scheduler to find runnable work without loading terminal
// sessions.
func (s *Store) ListSessionsByStatuses(statuses ...model.SessionStatus) ([]*model.Session, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(statuses))
	args := make([]any, len(statuses))
	for i, st := range statuses {
		placeholders[i] = "?"
		args[i] = string(st)
	}
	q := `SELECT ` + sessionCols + ` FROM sessions WHERE status IN (` +
		strings.Join(placeholders, ",") + `) ORDER BY created_at ASC`
	return s.querySessions(q, args...)
}

// CountSessionsByStatuses returns how many sessions are in any of the given
// statuses.
func (s *Store) CountSessionsByStatuses(statuses ...model.SessionStatus) (int, error) {
	if len(statuses) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(statuses))
	args := make([]any, len(statuses))
	for i, st := range statuses {
		placeholders[i] = "?"
		args[i] = string(st)
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE status IN (`+
		strings.Join(placeholders, ",")+`)`, args...).Scan(&n)
	return n, err
}

// CountActiveWorkerSessions counts sessions that consume the scheduler's global
// worker-concurrency budget: starting or running, and NOT managers. Interactive
// managers are long-lived per-objective supervisors that sit idle most of their
// life (waiting on workers or PR events); counting them lets accumulated idle
// managers exhaust the budget and starve new work on an otherwise idle fleet.
// They are still bounded by per-target capacity (a manager is a real process on
// the box) — just not by this logical throughput cap.
func (s *Store) CountActiveWorkerSessions() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE status IN (?,?) AND role <> ?`,
		string(model.SessionStarting), string(model.SessionRunning), string(model.RoleManager)).Scan(&n)
	return n, err
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
		   latest_summary = ?, handoff_summary = ?, target_id = ?, workspace_id = ?,
		   usage_provider = ?, metadata = ?, updated_at = ? WHERE id = ?`,
		sess.Title, sess.Goal, sess.CurrentActivity, sess.LatestSummary, sess.HandoffSummary,
		nullStr(sess.TargetID), nullStr(sess.WorkspaceID), nullStr(sess.UsageProvider),
		sess.Metadata, now, id)
	if err != nil {
		return nil, err
	}
	return sess, tx.Commit()
}

// RequeueInterruptedSessions force-requeues every session left in starting or
// running by a previous process, returning the affected sessions. It runs once
// at startup, before the scheduler: those rows claim a live run exists, but a
// fresh process has none, so the sessions would otherwise be stranded forever.
// This deliberately bypasses the session state machine — the running->queued
// edge is illegal precisely because a live run normally exists, and at boot
// that premise is void. Target slots and locks are intentionally untouched:
// claims persist across restarts and re-acquisition by the same holder is
// idempotent, so a requeued session restarts with the resources it held.
func (s *Store) RequeueInterruptedSessions() ([]*model.Session, error) {
	sessions, err := s.ListSessionsByStatuses(model.SessionStarting, model.SessionRunning)
	if err != nil {
		return nil, err
	}
	now := s.now()
	for _, sess := range sessions {
		if _, err := s.db.Exec(
			`UPDATE sessions SET status = ?, updated_at = ? WHERE id = ?`,
			string(model.SessionQueued), now, sess.ID); err != nil {
			return nil, err
		}
		sess.Status = model.SessionQueued
		sess.UpdatedAt = now
	}
	return sessions, nil
}

// AddSessionTokens increments a session's running token total. It is a small,
// self-contained UPDATE and deliberately does not touch updated_at or the
// session state machine — usage accrues independently of lifecycle changes.
func (s *Store) AddSessionTokens(sessionID string, tokens int64) error {
	if tokens == 0 {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE sessions SET used_tokens = used_tokens + ? WHERE id = ?`,
		tokens, sessionID)
	return err
}

// SessionUsedTokens returns a session's running token total.
func (s *Store) SessionUsedTokens(sessionID string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT used_tokens FROM sessions WHERE id = ?`, sessionID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return n, err
}

// ObjectiveUsage aggregates token usage across all sessions of one objective:
// a grand total plus per-session and per-provider breakdowns. The effective
// provider is sessions.usage_provider when set, otherwise sessions.agent.
func (s *Store) ObjectiveUsage(objectiveID string) (*model.ObjectiveUsage, error) {
	rows, err := s.db.Query(
		`SELECT id, title, role,
		   COALESCE(NULLIF(usage_provider, ''), agent) AS provider, used_tokens
		 FROM sessions WHERE objective_id = ? ORDER BY created_at ASC`, objectiveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &model.ObjectiveUsage{ObjectiveID: objectiveID}
	byProvider := map[string]int64{}
	var providerOrder []string
	for rows.Next() {
		var b model.SessionUsageBreakdown
		if err := rows.Scan(&b.SessionID, &b.Title, &b.Role, &b.Provider, &b.UsedTokens); err != nil {
			return nil, err
		}
		out.Sessions = append(out.Sessions, b)
		out.TotalTokens += b.UsedTokens
		if _, seen := byProvider[b.Provider]; !seen {
			providerOrder = append(providerOrder, b.Provider)
		}
		byProvider[b.Provider] += b.UsedTokens
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, p := range providerOrder {
		out.Providers = append(out.Providers, model.ProviderUsageTotal{
			Provider: p, UsedTokens: byProvider[p],
		})
	}
	return out, nil
}
