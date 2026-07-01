package orch

import (
	"fmt"
	"time"

	"github.com/nathanwhit/orcha/internal/model"
)

// TargetRequest expresses the constraints used to pick a target.
type TargetRequest struct {
	RequiredLabels []string
	ProjectPath    string // for build/cache locality preference
	PinnedTargetID string // explicit user pinning
	// IgnoreWorkerCapacity places this session without consuming, or being gated
	// by, a target's per-session worker capacity. Set for interactive managers:
	// they are long-lived per-objective supervisors that sit idle most of their
	// life, so charging them the same slots workers need lets accumulated idle
	// managers fill a target and starve real work — a fleet with N total slots
	// could otherwise hold at most N managers and zero workers. This mirrors the
	// existing exemption from the global worker-concurrency cap
	// (CountActiveWorkerSessions / Scheduler.Tick). Managers remain bounded by
	// the load gate and the per-objective respawn limit.
	IgnoreWorkerCapacity bool
}

// SelectTarget picks a schedulable target satisfying the request. It considers:
// user pinning, status (only online), required labels, and free capacity. It
// returns ErrNoTarget when nothing fits. Selection does NOT claim a slot — the
// caller claims atomically via the store once a target is chosen.
func (o *Orchestrator) SelectTarget(req TargetRequest) (*model.Target, error) {
	targets, err := o.st.ListTargets()
	if err != nil {
		return nil, err
	}

	// Explicit pin wins, but still must be schedulable with capacity.
	if req.PinnedTargetID != "" {
		for _, t := range targets {
			if t.ID == req.PinnedTargetID {
				if !t.Status.CanSchedule() {
					return nil, fmt.Errorf("%w: pinned target %s is %s", ErrNoTarget, t.Name, t.Status)
				}
				if !req.IgnoreWorkerCapacity && t.AvailableSessions <= 0 {
					return nil, fmt.Errorf("%w: pinned target %s is at capacity", ErrNoTarget, t.Name)
				}
				if !hasLabels(t, req.RequiredLabels) {
					return nil, fmt.Errorf("%w: pinned target %s lacks required labels", ErrNoTarget, t.Name)
				}
				return t, nil
			}
		}
		return nil, fmt.Errorf("%w: pinned target %s not found", ErrNoTarget, req.PinnedTargetID)
	}

	now := o.st.Now()
	var best *model.Target
	for _, t := range targets {
		if !t.Status.CanSchedule() { // draining/offline/disabled excluded
			continue
		}
		if !req.IgnoreWorkerCapacity && t.AvailableSessions <= 0 {
			continue
		}
		if !hasLabels(t, req.RequiredLabels) {
			continue
		}
		// Load gate: a saturated box stops accepting new work even with free slots,
		// so the scheduler spreads to a quieter target (or waits) instead of piling
		// on and thrashing. Fails open when load-aware scheduling is off or the
		// sample is missing/stale.
		if o.overloaded(t, now) {
			continue
		}
		// Disk gate: a box low on work-root disk stops accepting new checkouts, so a
		// leak parks work (waiting_capacity) instead of filling the volume to 100% —
		// which on the orch host wedges the DB it shares. Fails open when the disk
		// guard is off or the sample is missing/stale.
		if o.diskPressured(t, now) {
			continue
		}
		// Prefer the target with the most free capacity (spread load) and a small
		// cache-locality bonus, less a penalty for how loaded it currently is.
		if best == nil || o.targetScore(t, req, now) > o.targetScore(best, req, now) {
			best = t
		}
	}
	if best == nil {
		return nil, ErrNoTarget
	}
	return best, nil
}

// targetScore ranks a candidate target: capacity spread + cache locality, minus
// a penalty for current per-core load so the less-loaded target wins a close
// race. The load penalty is deliberately small — it breaks ties and biases
// spreading, but does not override the cache-locality bonus (a cold cache means
// a full checkout + build, which is far costlier than a modestly busier box).
func (o *Orchestrator) targetScore(t *model.Target, req TargetRequest, now time.Time) int {
	s := score(t, req)
	if o.cfg.MaxLoadPerCore <= 0 {
		return s // load-aware scheduling disabled: capacity + cache only
	}
	if lpc, ok := o.targetLoadPerCore(t, now); ok {
		s -= int(lpc * 20)
	}
	return s
}

// ErrNoTarget is returned when no schedulable target satisfies a request.
var ErrNoTarget = errTarget("no schedulable target available")

type errTarget string

func (e errTarget) Error() string { return "orch: " + string(e) }

func hasLabels(t *model.Target, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := map[string]bool{}
	for _, l := range t.Labels {
		have[l] = true
	}
	for _, r := range required {
		if !have[r] {
			return false
		}
	}
	return true
}

func score(t *model.Target, req TargetRequest) int {
	s := t.AvailableSessions * 10
	if req.ProjectPath != "" {
		if cache, ok := t.Metadata["cached_projects"].([]any); ok {
			for _, c := range cache {
				if c == req.ProjectPath {
					s += 100 // build/cache locality bonus
				}
			}
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// Provider selection with usage-aware fallback
// ---------------------------------------------------------------------------

// SelectProvider chooses an agent provider for a session, honoring usage
// exhaustion. If the preferred provider is exhausted it tries the configured
// fallback chain. If nothing usable remains it returns ErrProviderExhausted so
// the caller can ask the user instead of hammering an exhausted provider.
func (o *Orchestrator) SelectProvider(preferred model.AgentKind) (model.AgentKind, error) {
	candidates := append([]model.AgentKind{preferred}, o.cfg.ProviderFallback...)
	seen := map[model.AgentKind]bool{}
	for _, kind := range candidates {
		if seen[kind] {
			continue
		}
		seen[kind] = true
		if _, ok := o.provider(kind); !ok {
			continue // not registered
		}
		state, err := o.st.ProviderState(string(kind))
		if err != nil {
			return "", err
		}
		if state == model.UsageExhausted {
			continue // skip exhausted providers; never retry them
		}
		return kind, nil
	}
	return "", ErrProviderExhausted
}

// ErrProviderExhausted indicates every candidate provider is exhausted.
var ErrProviderExhausted = errTarget("all candidate providers are exhausted")

// PlaceSession selects a target, atomically claims a slot, and binds the
// session to it. On success the session's target_id is set and a target slot is
// reserved (released when the session reaches a terminal state). It returns
// store.ErrNoCapacity / ErrNoTarget so the scheduler can mark the session
// waiting_capacity.
func (o *Orchestrator) PlaceSession(sessionID string, req TargetRequest) (*model.Target, error) {
	sess, err := o.st.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if sess.Status.IsTerminal() {
		return nil, fmt.Errorf("orch: cannot place terminal session %s", sessionID)
	}
	target, err := o.SelectTarget(req)
	if err != nil {
		return nil, err
	}
	// Managers don't consume a worker slot, so they bind to the target without
	// claiming one. SelectTarget already excluded draining/offline targets, so
	// the atomic claim — whose job is to stop concurrent schedulers
	// oversubscribing capacity — has nothing to enforce for a manager. Keyed on
	// the durable session role (not req) so it can never disagree with
	// releaseTargetSlot, which must skip the matching release.
	claimsSlot := !sessionExemptFromCapacity(sess)
	if claimsSlot {
		// Atomic claim — enforces capacity and draining at the store layer.
		if err := o.st.ClaimTargetSlot(target.ID); err != nil {
			return nil, err
		}
	}
	if _, err := o.st.UpdateSessionRuntime(sessionID, func(s *model.Session) {
		s.TargetID = target.ID
	}); err != nil {
		// Roll back the claim so we never leak capacity.
		if claimsSlot {
			_ = o.st.ReleaseTargetSlot(target.ID)
		}
		return nil, err
	}
	o.audit(sess.ObjectiveID, sessionID, "session_placed",
		fmt.Sprintf("placed on %s", target.Name), model.JSONMap{"target_id": target.ID})
	return target, nil
}

// releaseTargetSlot frees the capacity a session held, if any. A session exempt
// from worker capacity (a manager) never claimed a slot, so it must not release
// one either — crediting capacity that was never debited would let the target
// oversubscribe real workers.
func (o *Orchestrator) releaseTargetSlot(sess *model.Session) {
	if sessionExemptFromCapacity(sess) {
		return
	}
	if sess.TargetID != "" {
		_ = o.st.ReleaseTargetSlot(sess.TargetID)
	}
}

// sessionExemptFromCapacity reports whether a session is placed without
// consuming a target's per-session worker capacity. Interactive managers are
// long-lived supervisors that sit idle most of their life; see
// TargetRequest.IgnoreWorkerCapacity for the full rationale. Claim, release, and
// the slot reconcile all key off this single predicate so they stay in lockstep.
func sessionExemptFromCapacity(sess *model.Session) bool {
	return sess.Role == model.RoleManager
}
