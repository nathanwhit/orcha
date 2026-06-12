package orch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nathanwhit/orcha/internal/model"
)

// superviseIdleAfter is how long a manager must be quiet (no progress) before the
// supervisor considers the objective stalled. A manager actively producing output
// advances its UpdatedAt via streamed progress, so this only trips on a manager
// that genuinely paused.
var superviseIdleAfter = 90 * time.Second

// supervisePokeCooldown is the minimum gap between re-pokes of the same objective,
// so a manager that ignores a poke is nudged again at most once per cooldown
// rather than every scheduler tick.
var supervisePokeCooldown = 90 * time.Second

// SuperviseIdleObjectives re-engages the manager of any active objective that has
// gone idle. "Idle" means no worker is making progress: a manager paused
// mid-task (or one that never got a nudge because the triggering event was
// dropped) would otherwise leave the objective ACTIVE forever. The supervisor
// pokes only when NO worker is active — a running worker, even one quietly
// grinding through a long build, is non-terminal and counts as activity, so an
// objective that is genuinely progressing is left alone. It pokes a LIVE manager
// only; re-spawning a manager that already went terminal is handled separately.
func (o *Orchestrator) SuperviseIdleObjectives(ctx context.Context) {
	for _, mgr := range o.idleManagersToPoke(o.st.Now()) {
		o.audit(mgr.ObjectiveID, mgr.ID, "manager_repoke",
			"no active workers; re-engaging idle manager", nil)
		_ = o.Steer(ctx, mgr.ID, o.idleManagerPokeMessage(mgr.ObjectiveID))
	}
}

// idleManagersToPoke returns the live managers of active objectives that should
// be re-poked now, recording the poke time for each so the cooldown holds. It is
// separated from the Steer side effect so the decision logic is testable.
func (o *Orchestrator) idleManagersToPoke(now time.Time) []*model.Session {
	objs, err := o.st.ListObjectives()
	if err != nil {
		return nil
	}
	var out []*model.Session
	for _, obj := range objs {
		if obj.Status != model.ObjectiveActive {
			continue
		}
		mgr := o.activeManagerFor(obj.ID)
		if mgr == nil {
			continue // no live manager to poke (terminal-manager respawn is separate)
		}
		// The rule: only re-poke when NO worker is active.
		if o.objectiveHasActiveWorkers(obj.ID, mgr.ID) {
			continue
		}
		// Don't poke a manager that's actively working right now (mid tool-call,
		// streaming output): its UpdatedAt is fresh. Only a manager quiet for
		// superviseIdleAfter is treated as stalled.
		if now.Sub(mgr.UpdatedAt) < superviseIdleAfter {
			continue
		}
		if !o.markPoked(obj.ID, now) {
			continue // poked too recently
		}
		out = append(out, mgr)
	}
	return out
}

// markPoked records a re-poke for an objective and reports whether enough time
// has passed since the last one. It returns false (and does not update the time)
// when still within the cooldown.
func (o *Orchestrator) markPoked(objectiveID string, now time.Time) bool {
	o.pokeMu.Lock()
	defer o.pokeMu.Unlock()
	if o.lastPoke == nil {
		o.lastPoke = map[string]time.Time{}
	}
	if last, ok := o.lastPoke[objectiveID]; ok && now.Sub(last) < supervisePokeCooldown {
		return false
	}
	o.lastPoke[objectiveID] = now
	return true
}

// idleManagerPokeMessage builds the re-poke text. Because the manager has no
// read tools, the poke carries the state snapshot inline: every worker's role,
// status, and latest summary, plus the objective's PRs. This is what lets a blind
// manager decide whether to spawn more work or finish.
func (o *Orchestrator) idleManagerPokeMessage(objectiveID string) string {
	var b strings.Builder
	b.WriteString("No workers are currently running for this objective, so nothing is making progress right now.\n\n")

	sessions, _ := o.st.ListSessionsByObjective(objectiveID)
	b.WriteString("Worker status:\n")
	wrote := false
	for _, s := range sessions {
		if s.Role == model.RoleManager {
			continue
		}
		wrote = true
		sum := firstNonEmpty(s.LatestSummary, s.CurrentActivity)
		if sum == "" {
			sum = "(no summary reported)"
		}
		fmt.Fprintf(&b, "- %q [%s] %s: %s\n", s.Title, s.Role, s.Status, sum)
	}
	if !wrote {
		b.WriteString("- (no workers spawned yet)\n")
	}

	if prs, _ := o.st.ListPRsByObjective(objectiveID); len(prs) > 0 {
		b.WriteString("\nPull requests:\n")
		for _, p := range prs {
			fmt.Fprintf(&b, "- PR #%d [%s, checks=%s]: %q\n", p.Number, p.Status, p.ChecksState, p.Title)
		}
	}

	b.WriteString("\nDecide the next step now. If the objective is complete and every PR has merged, " +
		"call mark_objective_done. If work remains, spawn the next worker. If you are blocked on a " +
		"decision only the user can make, call ask_user. Do not end your turn without taking one of these actions.")
	return b.String()
}
