// Package store is the SQLite persistence layer. It keeps normalized
// current-state tables and never holds a write transaction across model,
// GitHub, shell, or SSH calls — those happen in the orchestrator between
// short DB operations.
package store

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nathanwhit/orcha/internal/model"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrLockHeld is returned when a lock cannot be acquired because another
// holder owns it.
var ErrLockHeld = errors.New("store: lock already held")

// ErrConflict is returned when a write violates a uniqueness constraint, e.g.
// updating a project's repo to one another project already owns.
var ErrConflict = errors.New("store: conflict")

// Clock returns the current time. It is injectable for deterministic tests.
type Clock func() time.Time

// Store wraps a SQLite database.
type Store struct {
	db    *sql.DB
	now   Clock
	idmu  sync.Mutex
	idGen func() string
}

// Option configures a Store.
type Option func(*Store)

// WithClock overrides the time source.
func WithClock(c Clock) Option { return func(s *Store) { s.now = c } }

// WithIDGen overrides the id generator (useful for deterministic tests).
func WithIDGen(g func() string) Option { return func(s *Store) { s.idGen = g } }

// Open creates a Store backed by the SQLite database at path. Use ":memory:"
// for an ephemeral database. WAL journaling and foreign keys are enabled.
func Open(path string, opts ...Option) (*Store, error) {
	// busy_timeout avoids spurious "database is locked" under concurrency.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single shared in-memory DB requires limiting to one connection; for
	// file-backed WAL we still cap writers to keep things simple and correct.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{
		db:    db,
		now:   time.Now,
		idGen: defaultID,
	}
	for _, o := range opts {
		o(s)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

// migrate brings a pre-existing database up to the current schema. Open only
// runs CREATE TABLE IF NOT EXISTS, which never adds a column to a table that
// already exists, so columns introduced after a table's first release need an
// explicit, idempotent ALTER for older DBs. New DBs already have the column
// from schema.sql and these calls become no-ops.
func (s *Store) migrate() error {
	if err := s.ensureColumn("sessions", "used_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("sessions", "handoff_summary", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("projects", "review_gate", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return s.ensureColumn("projects", "review_guidance", "TEXT NOT NULL DEFAULT ''")
}

// ensureColumn adds a column to a table if it is not already present. Existing
// rows take the column's DEFAULT, so a NOT NULL DEFAULT 0 column backfills to 0.
func (s *Store) ensureColumn(table, column, decl string) error {
	var exists int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&exists)
	if err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	// Table/column identifiers are internal constants, not user input.
	_, err = s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl))
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle for advanced callers/tests.
func (s *Store) DB() *sql.DB { return s.db }

// Now returns the store's current time.
func (s *Store) Now() time.Time { return s.now() }

// NewID returns a fresh unique identifier.
func (s *Store) NewID() string {
	s.idmu.Lock()
	defer s.idmu.Unlock()
	return s.idGen()
}

// ---------------------------------------------------------------------------
// Locks
// ---------------------------------------------------------------------------

// AcquireLock atomically takes a lock identified by key. It returns ErrLockHeld
// if a different session already holds it. Re-acquiring a lock you already hold
// is a successful no-op (idempotent).
func (s *Store) AcquireLock(key string, kind model.LockKind, holder, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var existing string
	err = tx.QueryRow(`SELECT holder_session_id FROM locks WHERE key = ?`, key).Scan(&existing)
	switch {
	case err == nil:
		if existing == holder {
			return tx.Commit() // already ours
		}
		return ErrLockHeld
	case errors.Is(err, sql.ErrNoRows):
		// fall through to insert
	default:
		return err
	}

	_, err = tx.Exec(
		`INSERT INTO locks(key, kind, holder_session_id, acquired_at, reason) VALUES(?,?,?,?,?)`,
		key, string(kind), holder, s.now(), reason,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseLock removes a lock. It is a no-op if the lock does not exist. If
// holder is non-empty, the lock is only released when held by that session.
func (s *Store) ReleaseLock(key, holder string) error {
	var err error
	if holder == "" {
		_, err = s.db.Exec(`DELETE FROM locks WHERE key = ?`, key)
	} else {
		_, err = s.db.Exec(`DELETE FROM locks WHERE key = ? AND holder_session_id = ?`, key, holder)
	}
	return err
}

// ReleaseLocksHeldBy releases every lock owned by a session. Used on
// cancellation so a terminated session never strands a lock.
func (s *Store) ReleaseLocksHeldBy(holder string) error {
	_, err := s.db.Exec(`DELETE FROM locks WHERE holder_session_id = ?`, holder)
	return err
}

// LockHolder returns the current holder of a lock and whether it is held.
func (s *Store) LockHolder(key string) (string, bool, error) {
	var holder string
	err := s.db.QueryRow(`SELECT holder_session_id FROM locks WHERE key = ?`, key).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return holder, true, nil
}

// ---------------------------------------------------------------------------
// Events (audit history)
// ---------------------------------------------------------------------------

// AppendEvent records an audit/history row.
func (s *Store) AppendEvent(e model.Event) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO events(objective_id, session_id, type, summary, data, created_at)
		 VALUES(?,?,?,?,?,?)`,
		nullStr(e.ObjectiveID), nullStr(e.SessionID), e.Type, e.Summary, e.Data, s.now(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EventsAfter returns audit rows with id greater than after, oldest first.
func (s *Store) EventsAfter(after int64, limit int) ([]model.Event, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT id, objective_id, session_id, type, summary, data, created_at
		 FROM events WHERE id > ? ORDER BY id ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		var e model.Event
		var obj, sess sql.NullString
		if err := rows.Scan(&e.ID, &obj, &sess, &e.Type, &e.Summary, &e.Data, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.ObjectiveID = obj.String
		e.SessionID = sess.String
		out = append(out, e)
	}
	return out, rows.Err()
}
