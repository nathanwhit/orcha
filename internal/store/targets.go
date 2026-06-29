package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nathanwhit/orcha/internal/model"
)

// ErrNoCapacity is returned when a target cannot accept another session.
var ErrNoCapacity = errors.New("store: target has no free capacity")

// ErrTargetNotSchedulable is returned when a target's status forbids new
// sessions (draining, offline, or disabled).
var ErrTargetNotSchedulable = errors.New("store: target is not schedulable")

// CreateTarget inserts a new target. AvailableSessions defaults to capacity.
func (s *Store) CreateTarget(t *model.Target) error {
	if t.ID == "" {
		t.ID = s.NewID()
	}
	if t.Status == "" {
		t.Status = model.TargetOnline
	}
	if t.CapacitySessions <= 0 {
		t.CapacitySessions = 1
	}
	if t.AvailableSessions == 0 {
		t.AvailableSessions = t.CapacitySessions
	}
	_, err := s.db.Exec(
		`INSERT INTO targets(id, name, kind, status, host, user, work_root, labels,
		   capacity_sessions, available_sessions, cpu_summary, memory_summary,
		   disk_summary, last_seen_at, metadata)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, string(t.Kind), string(t.Status), nullStr(t.Host), nullStr(t.User),
		t.WorkRoot, joinLabels(t.Labels), t.CapacitySessions, t.AvailableSessions,
		t.CPUSummary, t.MemorySummary, t.DiskSummary, nullTime(t.LastSeenAt), t.Metadata,
	)
	return err
}

// UpdateTarget updates a target's mutable operator configuration and preserves
// scheduling state, health summaries, and last-seen data. Capacity changes keep
// currently used slots accounted for: available becomes max(new-used, 0).
func (s *Store) UpdateTarget(id string, t *model.Target) (*model.Target, error) {
	if t.CapacitySessions < 1 {
		return nil, fmt.Errorf("store: capacity_sessions must be at least 1")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	row := tx.QueryRow(`SELECT `+targetCols+` FROM targets WHERE id = ?`, id)
	old, err := scanTarget(row)
	if err != nil {
		return nil, err
	}
	used := old.CapacitySessions - old.AvailableSessions
	if used < 0 {
		used = 0
	}
	available := t.CapacitySessions - used
	if available < 0 {
		available = 0
	}

	if _, err := tx.Exec(
		`UPDATE targets
		 SET name = ?, host = ?, user = ?, work_root = ?, labels = ?,
		     capacity_sessions = ?, available_sessions = ?, metadata = ?
		 WHERE id = ?`,
		t.Name, nullStr(t.Host), nullStr(t.User), t.WorkRoot, joinLabels(t.Labels),
		t.CapacitySessions, available, t.Metadata, id,
	); err != nil {
		return nil, err
	}
	row = tx.QueryRow(`SELECT `+targetCols+` FROM targets WHERE id = ?`, id)
	updated, err := scanTarget(row)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

var targetCols = `id, name, kind, status, host, user, work_root, labels,
	capacity_sessions, available_sessions, cpu_summary, memory_summary,
	disk_summary, last_seen_at, metadata`

func scanTarget(r rowScanner) (*model.Target, error) {
	var t model.Target
	var host, user, labels sql.NullString
	var lastSeen sql.NullTime
	err := r.Scan(&t.ID, &t.Name, &t.Kind, &t.Status, &host, &user, &t.WorkRoot,
		&labels, &t.CapacitySessions, &t.AvailableSessions, &t.CPUSummary,
		&t.MemorySummary, &t.DiskSummary, &lastSeen, &t.Metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Host = host.String
	t.User = user.String
	t.Labels = splitLabels(labels)
	t.LastSeenAt = timePtr(lastSeen)
	return &t, nil
}

// GetTarget fetches one target.
func (s *Store) GetTarget(id string) (*model.Target, error) {
	row := s.db.QueryRow(`SELECT `+targetCols+` FROM targets WHERE id = ?`, id)
	return scanTarget(row)
}

// ListTargets returns all targets.
func (s *Store) ListTargets() ([]*model.Target, error) {
	rows, err := s.db.Query(`SELECT ` + targetCols + ` FROM targets ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetTargetStatus changes a target's scheduling status (online, draining,
// offline, disabled).
func (s *Store) SetTargetStatus(id string, status model.TargetStatus) error {
	res, err := s.db.Exec(`UPDATE targets SET status = ? WHERE id = ?`, string(status), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkTargetSeen sets a target's status and stamps last_seen_at (after a
// successful health check).
func (s *Store) MarkTargetSeen(id string, status model.TargetStatus) error {
	_, err := s.db.Exec(`UPDATE targets SET status = ?, last_seen_at = ? WHERE id = ?`,
		string(status), s.now(), id)
	return err
}

// SetTargetLoad records a target's latest sampled load into its metadata
// (load_per_core, mem_available_mb, load_probed_at) and stamps last_seen_at,
// preserving any other metadata keys. Used by the load-aware scheduler's probe.
func (s *Store) SetTargetLoad(id string, loadPerCore float64, memAvailMB int, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var md model.JSONMap
	err = tx.QueryRow(`SELECT metadata FROM targets WHERE id = ?`, id).Scan(&md)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if md == nil {
		md = model.JSONMap{}
	}
	md["load_per_core"] = loadPerCore
	md["mem_available_mb"] = memAvailMB
	md["load_probed_at"] = at.UTC().Format(time.RFC3339)

	if _, err := tx.Exec(
		`UPDATE targets SET metadata = ?, last_seen_at = ? WHERE id = ?`, md, at, id,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// SetTargetDisk records a target's latest sampled work-root free disk into its
// metadata (free_disk_mb, disk_probed_at) and stamps last_seen_at, preserving any
// other metadata keys. Used by the disk-guard probe.
func (s *Store) SetTargetDisk(id string, freeDiskMB int, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var md model.JSONMap
	err = tx.QueryRow(`SELECT metadata FROM targets WHERE id = ?`, id).Scan(&md)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if md == nil {
		md = model.JSONMap{}
	}
	md["free_disk_mb"] = freeDiskMB
	md["disk_probed_at"] = at.UTC().Format(time.RFC3339)

	if _, err := tx.Exec(
		`UPDATE targets SET metadata = ?, last_seen_at = ? WHERE id = ?`, md, at, id,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimTargetSlot atomically reserves one session slot on a target. It enforces
// the scheduling status (only online targets accept new sessions) and capacity
// limits in a single transaction, so concurrent schedulers cannot oversubscribe
// a target or place work on a draining one.
func (s *Store) ClaimTargetSlot(targetID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var status string
	var avail int
	err = tx.QueryRow(
		`SELECT status, available_sessions FROM targets WHERE id = ?`, targetID,
	).Scan(&status, &avail)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !model.TargetStatus(status).CanSchedule() {
		return fmt.Errorf("%w: status=%s", ErrTargetNotSchedulable, status)
	}
	if avail <= 0 {
		return ErrNoCapacity
	}
	if _, err := tx.Exec(
		`UPDATE targets SET available_sessions = available_sessions - 1 WHERE id = ?`,
		targetID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseTargetSlot returns a session slot to a target, never exceeding
// capacity.
func (s *Store) ReleaseTargetSlot(targetID string) error {
	_, err := s.db.Exec(
		`UPDATE targets
		 SET available_sessions = MIN(capacity_sessions, available_sessions + 1)
		 WHERE id = ?`, targetID)
	return err
}

// ReconcileTargetSlots recomputes every target's available_sessions from actual
// occupancy: capacity minus the non-terminal, capacity-consuming (non-manager)
// sessions bound to it. available_sessions is otherwise maintained incrementally
// — debited on claim, credited at terminal — so a crash between claim and the
// runtime update, or a change in WHAT counts against capacity (managers became
// exempt), can drift it and permanently shrink a target's usable slots. Run once
// at startup, before the scheduler, to heal that drift. It is idempotent.
// Returns the number of targets whose counter it corrected.
func (s *Store) ReconcileTargetSlots() (int, error) {
	targets, err := s.ListTargets()
	if err != nil {
		return 0, err
	}
	corrected := 0
	for _, t := range targets {
		var used int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM sessions
			 WHERE target_id = ? AND role <> ? AND status NOT IN (?,?,?)`,
			t.ID, string(model.RoleManager),
			string(model.SessionSucceeded), string(model.SessionFailed), string(model.SessionCanceled),
		).Scan(&used); err != nil {
			return corrected, err
		}
		avail := t.CapacitySessions - used
		if avail < 0 {
			avail = 0
		}
		if avail == t.AvailableSessions {
			continue
		}
		if _, err := s.db.Exec(
			`UPDATE targets SET available_sessions = ? WHERE id = ?`, avail, t.ID); err != nil {
			return corrected, err
		}
		corrected++
	}
	return corrected, nil
}
