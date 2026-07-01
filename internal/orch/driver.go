package orch

import (
	"context"
	"errors"
	"sync"
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

	// starting tracks sessions with a StartRun in flight. Starting a session does
	// a git checkout (minutes for a big repo), so it runs in the background — but
	// the session row stays queued until StartRun flips it, so without this guard
	// the next tick would launch a second StartRun for the same session. The value
	// is whether the session is a worker, so the worker budget can account for
	// in-flight launches that haven't reached the running count yet.
	startMu  sync.Mutex
	starting map[string]bool
}

// NewScheduler builds a scheduler. interval is the idle tick period;
// maxConcurrent caps simultaneously active (starting+running) WORKER sessions
// across all targets — managers are exempt (see Tick). Per-target capacity still
// applies and counts everything, managers included.
func NewScheduler(o *Orchestrator, interval time.Duration, maxConcurrent int) *Scheduler {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 32
	}
	return &Scheduler{
		o:             o,
		interval:      interval,
		maxConcurrent: maxConcurrent,
		wake:          make(chan struct{}, 1),
		starting:      map[string]bool{},
	}
}

// beginStart marks a session's StartRun as in flight, returning false if one
// already is (so a slow checkout is never started twice while the session row is
// still queued). isWorker feeds the worker-budget accounting.
func (s *Scheduler) beginStart(id string, isWorker bool) bool {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if _, ok := s.starting[id]; ok {
		return false
	}
	s.starting[id] = isWorker
	return true
}

func (s *Scheduler) endStart(id string) {
	s.startMu.Lock()
	delete(s.starting, id)
	s.startMu.Unlock()
}

func (s *Scheduler) isStarting(id string) bool {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	_, ok := s.starting[id]
	return ok
}

// inflightWorkers counts worker StartRuns that have been launched but not yet
// reached the running/starting count CountActiveWorkerSessions sees. Counting
// them keeps the worker cap honest across the async-launch window (it may briefly
// double-count a worker already flipped to running, which only undershoots the
// cap — never oversubscribes it).
func (s *Scheduler) inflightWorkers() int {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	n := 0
	for _, isWorker := range s.starting {
		if isWorker {
			n++
		}
	}
	return n
}

// Wake nudges the scheduler to run a tick promptly (e.g. after a session is
// created or completes).
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default: // a wake is already pending
	}
}

// workspaceGCInterval is how often the scheduler reclaims unneeded checkouts.
// Disk cleanup is not latency-sensitive, so it runs far less often than the
// scheduling tick.
var workspaceGCInterval = 5 * time.Minute

// Run drives the scheduler until ctx is canceled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	gc := time.NewTicker(workspaceGCInterval)
	load := time.NewTicker(loadProbeInterval)
	reconcile := time.NewTicker(orphanReconcileInterval)
	defer t.Stop()
	defer gc.Stop()
	defer load.Stop()
	defer reconcile.Stop()
	// Sweep stale checkouts left by a previous process at startup, then on a tick.
	go s.o.ReclaimWorkspaces(ctx)
	// Compact the bare mirror caches (throttled per target) so the shared mirror
	// doesn't grow unbounded.
	go s.o.MaintainCaches(ctx)
	// Sample target load before the first placements so they are load-aware from
	// the start (no-op when load-aware scheduling is disabled).
	go s.o.ProbeTargetLoads(ctx)
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
		case <-gc.C:
			// Reclaim asynchronously: a remote rm must not stall scheduling.
			go s.o.ReclaimWorkspaces(ctx)
			go s.o.MaintainCaches(ctx) // throttled per target inside

		case <-load.C:
			// Probe asynchronously: a slow/hung SSH probe must not stall scheduling.
			go s.o.ProbeTargetLoads(ctx)
		case <-reconcile.C:
			// Heal sessions stranded "running" with no live run (a watcher that
			// died without emitting terminal). Cheap and DB-only, but run off the
			// scheduling thread for symmetry with the other periodic sweeps.
			go s.ReconcileOrphans(ctx)
		}
	}
}

// Tick performs one scheduling pass and returns how many session starts it
// LAUNCHED this pass. Starting a session does a git checkout that can take
// minutes for a large repo, so StartRun runs in the background — one slow
// checkout must not head-of-line block every other pending session (managers
// included) on the single scheduler thread. A launched session reaches the
// running state shortly after the tick returns; gating (dependencies, the worker
// cap, an already-in-flight start) is still decided synchronously here.
//
// It is exported so tests (and Wake-driven callers) can drive scheduling.
func (s *Scheduler) Tick(ctx context.Context) (int, error) {
	// The global cap bounds concurrent WORKERS only. Interactive managers are
	// excluded from both the running count and the budget gate below: they are
	// long-lived per-objective supervisors, idle most of their life, so gating
	// them behind the worker cap is exactly what made a new manager queue for
	// minutes on an otherwise idle fleet. Managers stay bounded by per-target
	// capacity and the per-objective respawn limit.
	active, err := s.o.st.CountActiveWorkerSessions()
	if err != nil {
		return 0, err
	}
	budget := s.maxConcurrent - active - s.inflightWorkers()

	// queued + waiting_capacity are the runnable-but-not-yet-running states.
	candidates, err := s.o.st.ListSessionsByStatuses(model.SessionQueued, model.SessionWaitingCapacity)
	if err != nil {
		return 0, err
	}

	started := 0
	for _, sess := range candidates {
		if s.isStarting(sess.ID) {
			continue // a StartRun for this session is already running in the background
		}
		isManager := sess.Role == model.RoleManager
		// Workers consume the budget; managers bypass it. Don't break — a worker
		// past the cap is skipped, but a later manager must still get a chance.
		if !isManager && budget <= 0 {
			continue
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

		// Launch the start in the background: the checkout it may do can take
		// minutes, and blocking the scheduler thread on it stalls every other
		// pending session. The in-flight guard keeps a later tick from
		// double-starting this same session while its row is still queued.
		if !s.beginStart(sess.ID, !isManager) {
			continue
		}
		started++
		if !isManager {
			budget--
		}
		go func(id string) {
			defer s.endStart(id)
			if _, err := s.o.StartRun(ctx, id); err != nil {
				// Capacity/lock contention already parked the session
				// waiting_capacity inside StartRun, and a real start failure already
				// marked it failed. Provider exhaustion is the one case left to the
				// caller: StartRun has asked the user, so park it to avoid re-asking
				// every tick.
				if errors.Is(err, ErrProviderExhausted) {
					_, _ = s.o.st.UpdateSessionStatus(id, model.SessionWaitingUser)
				}
			}
			s.Wake() // a slot/lock may have freed up — re-evaluate promptly
		}(sess.ID)
	}
	return started, nil
}

// orphanReconcileInterval is how often the scheduler heals sessions stranded in
// a live status (starting/running) with no live run backing them. It is not
// latency-sensitive — a strand only deadlocks its objective, it does not corrupt
// anything — so it runs far less often than the scheduling tick.
var orphanReconcileInterval = 1 * time.Minute

// orphanGrace is how long a starting/running row may have no live run before the
// reconciler treats it as orphaned. The in-flight (isStarting) guard already
// covers a scheduler-launched StartRun for its whole duration; this grace is the
// margin for a direct StartRun that bypasses beginStart (the operator restart
// endpoint) and for clock skew on updated_at, so a just-launched session whose
// run is still wiring up is never yanked.
var orphanGrace = 90 * time.Second

// ReconcileOrphans requeues sessions whose row says starting/running but which
// have no live run in memory and no StartRun in flight — i.e. the in-memory run
// whose watcher should drive them to a terminal state is gone (a missed tmux
// exit, a provider goroutine that stopped, a re-adopt after restart that later
// lost the session). Such a row is stranded forever: it never reaches terminal,
// so a worker/follow-up's manager waits on a completion that never comes, and the
// "don't spawn a duplicate" guards stop it from replacing the dead child.
//
// RecoverInterrupted only sweeps once at startup (when every live-status row is
// by definition unbacked), so a strand that happens mid-life is never cleaned up
// — this is the periodic counterpart. Requeuing routes the session back through
// StartRun, which re-adopts a still-live tmux, resumes a recoverable conversation,
// or fails cleanly; a clean failure is itself the fix, because it finally
// re-engages the waiting manager.
func (s *Scheduler) ReconcileOrphans(ctx context.Context) {
	sessions, err := s.o.st.ListSessionsByStatuses(model.SessionStarting, model.SessionRunning)
	if err != nil {
		return
	}
	cutoff := s.o.st.Now().Add(-orphanGrace)
	requeued := 0
	for _, sess := range sessions {
		if sess.UpdatedAt.After(cutoff) {
			continue // recently touched; a start may still be registering its run
		}
		s.o.mu.Lock()
		_, live := s.o.runs[sess.ID]
		s.o.mu.Unlock()
		if live {
			continue // a real run is driving it
		}
		if s.isStarting(sess.ID) {
			continue // StartRun in flight (e.g. a multi-minute checkout)
		}
		ok, err := s.o.st.RequeueSession(sess.ID)
		if err != nil || !ok {
			continue // moved to terminal under us, or a transient DB error
		}
		_ = s.o.emit(sess.ID, model.MsgSystem, model.KindStatus,
			"no live process backed this running session; requeued to restart it", nil)
		s.o.audit(sess.ObjectiveID, sess.ID, "session_reconciled",
			"requeued orphaned "+string(sess.Status)+" session with no live run", nil)
		requeued++
	}
	if requeued > 0 {
		s.o.notifyChange()
		s.Wake() // run a tick now so the requeued sessions restart promptly
	}
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
