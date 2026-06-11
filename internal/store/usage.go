package store

import (
	"database/sql"

	"github.com/nathanwhit/orcha/internal/model"
)

// UpsertUsage records the latest usage bucket for a provider/account. There is
// one current bucket per provider+account; older windows are overwritten.
func (s *Store) UpsertUsage(u *model.UsageBucket) error {
	if u.ID == "" {
		u.ID = u.Provider + ":" + u.Account
	}
	u.UpdatedAt = s.now()
	if u.State == "" {
		u.State = model.UsageUnknown
	}
	_, err := s.db.Exec(
		`INSERT INTO usage_buckets(id, provider, account, window_start, window_end,
		   used_tokens, used_percent, state, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   window_start=excluded.window_start, window_end=excluded.window_end,
		   used_tokens=excluded.used_tokens, used_percent=excluded.used_percent,
		   state=excluded.state, updated_at=excluded.updated_at`,
		u.ID, u.Provider, u.Account, u.WindowStart, u.WindowEnd, u.UsedTokens,
		u.UsedPercent, string(u.State), u.UpdatedAt)
	return err
}

// AddUsageTokens increments the current bucket for a provider/account, creating
// it if necessary. State is left untouched (a monitor sets it from real
// provider data); callers that derive exhaustion locally may pass a non-empty
// newState to update it.
func (s *Store) AddUsageTokens(provider, account string, tokens int64, newState model.UsageState) error {
	id := provider + ":" + account
	now := s.now()
	state := string(newState)
	if newState == "" {
		state = string(model.UsageUnknown)
	}
	_, err := s.db.Exec(
		`INSERT INTO usage_buckets(id, provider, account, window_start, window_end,
		   used_tokens, used_percent, state, updated_at)
		 VALUES(?,?,?,?,?,?,NULL,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   used_tokens = used_tokens + excluded.used_tokens,
		   state = CASE WHEN excluded.state = 'unknown' THEN usage_buckets.state ELSE excluded.state END,
		   updated_at = excluded.updated_at`,
		id, provider, account, now, now, tokens, state, now)
	return err
}

// ListUsage returns all current usage buckets.
func (s *Store) ListUsage() ([]*model.UsageBucket, error) {
	rows, err := s.db.Query(
		`SELECT id, provider, account, window_start, window_end, used_tokens,
		   used_percent, state, updated_at FROM usage_buckets ORDER BY provider ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.UsageBucket
	for rows.Next() {
		u := &model.UsageBucket{}
		var pct sql.NullFloat64
		if err := rows.Scan(&u.ID, &u.Provider, &u.Account, &u.WindowStart,
			&u.WindowEnd, &u.UsedTokens, &pct, &u.State, &u.UpdatedAt); err != nil {
			return nil, err
		}
		if pct.Valid {
			v := pct.Float64
			u.UsedPercent = &v
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ProviderState returns the usage state for a provider (across accounts it
// returns the worst state). Unknown if no bucket exists.
func (s *Store) ProviderState(provider string) (model.UsageState, error) {
	buckets, err := s.ListUsage()
	if err != nil {
		return model.UsageUnknown, err
	}
	worst := model.UsageUnknown
	found := false
	for _, b := range buckets {
		if b.Provider != provider {
			continue
		}
		found = true
		if usageSeverity(b.State) > usageSeverity(worst) {
			worst = b.State
		}
	}
	if !found {
		return model.UsageUnknown, nil
	}
	return worst, nil
}

func usageSeverity(s model.UsageState) int {
	switch s {
	case model.UsageOK:
		return 1
	case model.UsageConstrained:
		return 2
	case model.UsageExhausted:
		return 3
	default:
		return 0
	}
}
