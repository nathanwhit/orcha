package orch

import (
	"context"
	"time"

	"github.com/nathanwhit/orcha/internal/model"
)

// Scheduler is the driver loop that makes the system self-driving: it
// repeatedly finds runnable queued sessions and starts them, respecting
// dependencies, a global concurrency cap, target capacity, locks, and provider
// usage. The actual placement/lock/provider work lives in StartRun; the
// scheduler only decides what is eligible and how many to start.
type Scheduler struct {
	o             *Orchestrator
	interval      time.Duration
	maxConcurrent int
	wake          chan struct{}
}

// NewScheduler builds a scheduler. interval is the idle tick period;
// maxConcurrent caps simultaneously active (starting+running) sessions across
// all targets (target capacity still applies per target).
func NewScheduler(o *Orchestrator, interval time.Duration, maxConcurrent int) *Scheduler {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 8
	}
	return &Scheduler{
		o:             o,
		interval:      interval,
		maxConcurrent: maxConcurrent,
		wake:          make(chan struct{}, 1),
	}
}

// Wake nudges the scheduler to run a tick promptly (e.g. after a session is
// created or completes).
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default: // a wake is already pending
	}
}

// Run drives the scheduler until ctx is canceled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		_, _ = s.Tick(ctx)
		// Re-engage any active objective that has gone idle (no worker making
		// progress) so a paused manager continues instead of stalling forever.
		// The poke's own cooldown keeps this from firing every tick.
		s.o.SuperviseIdleObjectives(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// Tick performs one scheduling pass and returns how many sessions it started.
// It is exported so tests (and Wake-driven callers) can drive scheduling
// deterministically.
func (s *Scheduler) Tick(ctx context.Context) (int, error) {
	active, err := s.o.st.CountSessionsByStatuses(model.SessionStarting, model.SessionRunning)
	if err != nil {
		return 0, err
	}
	budget := s.maxConcurrent - active
	if budget <= 0 {
		return 0, nil
	}

	// queued + waiting_capacity are the runnable-but-not-yet-running states.
	candidates, err := s.o.st.ListSessionsByStatuses(model.SessionQueued, model.SessionWaitingCapacity)
	if err != nil {
		return 0, err
	}

	started := 0
	for _, sess := range candidates {
		if budget <= 0 {
			break
		}
		// Dependency gating.
		ready, blocked := s.o.dependencyState(sess)
		if blocked {
			// A prerequisite failed/canceled: this work can never run as planned.
			_ = s.o.Cancel(sess.ID, true)
			_ = s.o.emit(sess.ID, model.MsgSystem, model.KindStatus,
				"canceled: a dependency did not succeed", nil)
			continue
		}
		if !ready {
			continue // dependencies still in flight
		}

		_, err := s.o.StartRun(ctx, sess.ID)
		if err == nil {
			started++
			budget--
			continue
		}
		// StartRun already recorded the appropriate state for capacity/lock
		// contention (waiting_capacity) — just leave those for a later tick.
		// Provider exhaustion is terminal for this tick: it has already asked the
		// user, so park the session to avoid re-asking every tick.
		if err == ErrProviderExhausted {
			_, _ = s.o.st.UpdateSessionStatus(sess.ID, model.SessionWaitingUser)
		}
	}
	return started, nil
}

// dependencyState reports whether a session's declared dependencies are all
// satisfied (ready) and whether any dependency reached a non-success terminal
// state (blocked). A session with no dependencies is always ready.
func (o *Orchestrator) dependencyState(sess *model.Session) (ready, blocked bool) {
	deps := dependencyIDs(sess)
	if len(deps) == 0 {
		return true, false
	}
	allSucceeded := true
	for _, id := range deps {
		dep, err := o.st.GetSession(id)
		if err != nil {
			// Unknown dependency: treat as unmet rather than silently ready.
			allSucceeded = false
			continue
		}
		switch {
		case dep.Status == model.SessionSucceeded:
			// satisfied
		case dep.Status == model.SessionFailed || dep.Status == model.SessionCanceled:
			return false, true
		default:
			allSucceeded = false
		}
	}
	return allSucceeded, false
}

// dependencyIDs extracts depends_on from session metadata, tolerating both
// []string and the []any shape produced by a JSON round-trip.
func dependencyIDs(sess *model.Session) []string {
	if sess.Metadata == nil {
		return nil
	}
	raw, ok := sess.Metadata["depends_on"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
