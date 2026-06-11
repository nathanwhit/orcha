package store

import (
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"
)

func defaultID() string { return uuid.NewString() }

// nullStr converts an empty string to NULL for nullable columns.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullTime converts a nil/zero time pointer to NULL.
func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}

// timePtr converts a nullable time column into a *time.Time.
func timePtr(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}
	t := nt.Time
	return &t
}

// joinLabels/splitLabels persist string slices as comma-separated text. Label
// values must not contain commas (enforced by callers/validation upstream).
func joinLabels(labels []string) any {
	if len(labels) == 0 {
		return nil
	}
	return strings.Join(labels, ",")
}

func splitLabels(ns sql.NullString) []string {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	return strings.Split(ns.String, ",")
}
