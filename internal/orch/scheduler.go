package orch

import (
	"fmt"

	"github.com/nathanwhit/orcha/internal/model"
)

// TargetRequest expresses the constraints used to pick a target.
type TargetRequest struct {
	RequiredLabels []string
	ProjectPath    string // for build/cache locality preference
	PinnedTargetID string // explicit user pinning
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
				if t.AvailableSessions <= 0 {
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

	var best *model.Target
	for _, t := range targets {
		if !t.Status.CanSchedule() { // draining/offline/disabled excluded
			continue
		}
		if t.AvailableSessions <= 0 {
			continue
		}
		if !hasLabels(t, req.RequiredLabels) {
			continue
		}
		// Prefer the target with the most free capacity (spread load), with a
		// small bonus for cache locality if the project lives there.
		if best == nil || score(t, req) > score(best, req) {
			best = t
		}
	}
	if best == nil {
		return nil, ErrNoTarget
	}
	return best, nil
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
	// Atomic claim — enforces capacity and draining at the store layer.
	if err := o.st.ClaimTargetSlot(target.ID); err != nil {
		return nil, err
	}
	if _, err := o.st.UpdateSessionRuntime(sessionID, func(s *model.Session) {
		s.TargetID = target.ID
	}); err != nil {
		// Roll back the claim so we never leak capacity.
		_ = o.st.ReleaseTargetSlot(target.ID)
		return nil, err
	}
	o.audit(sess.ObjectiveID, sessionID, "session_placed",
		fmt.Sprintf("placed on %s", target.Name), model.JSONMap{"target_id": target.ID})
	return target, nil
}

// releaseTargetSlot frees the capacity a session held, if any.
func (o *Orchestrator) releaseTargetSlot(sess *model.Session) {
	if sess.TargetID != "" {
		_ = o.st.ReleaseTargetSlot(sess.TargetID)
	}
}
