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

// supervisePokeCooldown is the minimum gap between supervisor actions on the same
// objective, so a manager that ignores a poke (or a respawn that immediately dies)
// is retried at most once per cooldown rather than every scheduler tick.
var supervisePokeCooldown = 90 * time.Second

// maxManagerSessions caps how many manager sessions an objective may accumulate
// (the original plus respawns). Past this, the supervisor stops respawning and
// asks the user instead of spinning up managers forever.
const maxManagerSessions = 4

// supervisorAction is a decision the supervisor made for one objective. kind is
// "poke" (re-engage the live manager), "respawn" (the objective has no live
// manager — start a fresh one), or "escalate" (too many manager deaths — ask the
// user).
type supervisorAction struct {
	kind        string
	objectiveID string
	prompt      string          // objective prompt, for respawn
	agent       model.AgentKind // manager agent to reuse, for respawn
	manager     *model.Session  // the live manager, for poke
}

// SuperviseIdleObjectives keeps every active objective driven. A manager can
// pause mid-task and end its turn, or its session can go terminal entirely; in
// both cases the objective would otherwise sit ACTIVE with nothing making
// progress. Each tick it re-pokes a live-but-idle manager, respawns a manager for
// an objective that lost one, and (after too many respawns) asks the user. It
// only acts when NO worker is making progress — a non-terminal worker, even one
// on a long quiet build, counts as activity, so a progressing objective is left
// alone.
func (o *Orchestrator) SuperviseIdleObjectives(ctx context.Context) {
	for _, act := range o.superviseDecisions(o.st.Now()) {
		switch act.kind {
		case "poke":
			o.audit(act.objectiveID, act.manager.ID, "manager_repoke",
				"no active workers; re-engaging idle manager", nil)
			_ = o.Steer(ctx, act.manager.ID, o.idleManagerPokeMessage(act.objectiveID))
		case "respawn":
			o.respawnManager(act)
		case "escalate":
			o.escalateManagerDeaths(act.objectiveID)
		}
	}
}

// superviseDecisions returns the actions to take now, recording the action time
// per objective so the cooldown holds. It is separated from the side effects so
// the decision logic is testable without driving real agents.
func (o *Orchestrator) superviseDecisions(now time.Time) []supervisorAction {
	objs, err := o.st.ListObjectives()
	if err != nil {
		return nil
	}
	var out []supervisorAction
	for _, obj := range objs {
		if obj.Status != model.ObjectiveActive {
			continue
		}
		// The rule, shared by both paths: only act when NO worker is active. A
		// running worker means the objective is progressing on its own.
		if o.objectiveHasActiveWorkers(obj.ID, "") {
			continue
		}

		mgr := o.activeManagerFor(obj.ID)
		if mgr == nil {
			// No live manager: nothing can react to worker completions or PR events,
			// so the objective is stuck. Respawn one — unless this objective has
			// already burned through too many managers, in which case ask the user.
			if o.countManagers(obj.ID) >= maxManagerSessions {
				if o.markPoked(obj.ID, now) {
					out = append(out, supervisorAction{kind: "escalate", objectiveID: obj.ID})
				}
				continue
			}
			if !o.markPoked(obj.ID, now) {
				continue
			}
			out = append(out, supervisorAction{
				kind: "respawn", objectiveID: obj.ID, prompt: obj.Prompt,
				agent: o.lastManagerAgent(obj.ID),
			})
			continue
		}

		// Live manager: only poke one that has actually been quiet (a manager
		// streaming output right now has a fresh UpdatedAt and is left alone).
		if now.Sub(mgr.UpdatedAt) < superviseIdleAfter {
			continue
		}
		// Don't timer-poke a manager whose objective has an open PR. Re-engagement
		// there is driven entirely by PR lifecycle events — a merge notifies the
		// manager, and new review/CI feedback spawns a follow-up automatically — so
		// the idle poke has no next action to offer; it only makes the manager
		// re-ask the human to merge or re-state status. (An earlier version carved
		// out failing-CI / unhandled-feedback PRs as still pokeable, but those are
		// handled by the follow-up pipeline too, and a feedback item arriving one
		// tick before its follow-up spawned let a pointless poke through — which is
		// exactly what drove the redundant "can you merge?" asks.)
		if o.objectiveHasOpenPR(obj.ID) {
			continue
		}
		if !o.markPoked(obj.ID, now) {
			continue
		}
		out = append(out, supervisorAction{kind: "poke", objectiveID: obj.ID, manager: mgr})
	}
	return out
}

// objectiveHasOpenPR reports whether the objective has at least one open or
// draft PR. When it does, every next step is driven by PR lifecycle events: a
// merge notifies the manager (notifyManagerOfMerge), and new review/CI feedback
// is turned into a follow-up automatically (ProcessFeedback). The idle timer-
// poke plays no part in any of that, so the supervisor leaves such an objective
// alone and lets the PR machinery steer it the instant its state changes.
//
// This deliberately covers failing-CI and feedback-bearing PRs too: the follow-
// up pipeline owns them, and poking the manager in the gap before a follow-up
// spawns just yields a redundant question.
func (o *Orchestrator) objectiveHasOpenPR(objectiveID string) bool {
	prs, err := o.st.ListPRsByObjective(objectiveID)
	if err != nil {
		return false
	}
	for _, pr := range prs {
		if pr.Status == model.PROpen || pr.Status == model.PRDraft {
			return true
		}
	}
	return false
}

// markPoked records a supervisor action for an objective and reports whether
// enough time has passed since the last one. It returns false (and leaves the
// time unchanged) while still within the cooldown.
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

// countManagers returns how many manager sessions an objective has had (live or
// terminal) — its respawn budget is measured against this.
func (o *Orchestrator) countManagers(objectiveID string) int {
	sessions, err := o.st.ListSessionsByObjective(objectiveID)
	if err != nil {
		return 0
	}
	n := 0
	for _, s := range sessions {
		if s.Role == model.RoleManager {
			n++
		}
	}
	return n
}

// lastManagerAgent returns the agent kind of the objective's most recent manager
// so a respawn reuses the same provider (codex vs claude), falling back to the
// default when there somehow isn't one.
func (o *Orchestrator) lastManagerAgent(objectiveID string) model.AgentKind {
	sessions, err := o.st.ListSessionsByObjective(objectiveID)
	if err != nil {
		return o.defaultAgent()
	}
	var agent model.AgentKind
	var newest time.Time
	for _, s := range sessions {
		if s.Role == model.RoleManager && !s.CreatedAt.Before(newest) {
			agent, newest = s.Agent, s.CreatedAt
		}
	}
	if agent == "" {
		return o.defaultAgent()
	}
	return agent
}

// respawnManager starts a fresh manager for an objective that lost its previous
// one. The new manager is handed the original prompt plus a snapshot of what has
// already happened, so it continues the objective instead of restarting it.
func (o *Orchestrator) respawnManager(act supervisorAction) {
	goal := act.prompt + "\n\n" + o.resumeManagerContext(act.objectiveID)
	mgr, err := o.CreateSession(SpawnSpec{
		ObjectiveID: act.objectiveID,
		Role:        model.RoleManager,
		Agent:       act.agent,
		Mode:        model.ModeInteractive,
		Title:       "Manager (resumed)",
		Goal:        goal,
	})
	if err != nil {
		return
	}
	_ = o.st.SetObjectiveManager(act.objectiveID, mgr.ID)
	o.audit(act.objectiveID, mgr.ID, "manager_respawned",
		"respawned manager for objective with no live manager", nil)
	o.notifyChange() // wake the scheduler to start it promptly
}

// escalateManagerDeaths records a user-facing question when an objective has
// churned through its manager budget — repeatedly respawning would just burn
// tokens, so a human should look.
func (o *Orchestrator) escalateManagerDeaths(objectiveID string) {
	// Idempotent: a stuck objective is escalated every supervisor tick that gets
	// past the cooldown (90s), but the user only needs ONE standing "this is
	// stuck" question — not a fresh one every cooldown until they look. Without
	// this, an objective that can't progress (e.g. an open PR awaiting review, or
	// a manager that can't even start) piled up dozens of identical questions.
	// A supervisor escalation is the only question with no session id (worker and
	// manager asks always carry one), so that's the dedup key.
	if existing, err := o.st.ListQuestionsByObjective(objectiveID); err == nil {
		for _, q := range existing {
			if q.Status == model.QuestionOpen && q.SessionID == "" {
				return
			}
		}
	}
	_ = o.st.CreateQuestion(&model.Question{
		ObjectiveID: objectiveID,
		Priority:    20,
		Question: "This objective's manager has terminated repeatedly and is still not done. " +
			"It may be stuck. Review the work so far and decide whether to keep going, re-scope, or cancel it.",
		Context: o.objectiveStateSnapshot(objectiveID),
	})
	o.audit(objectiveID, "", "manager_respawn_capped",
		"manager respawn budget exhausted; asked the user", nil)
}

// objectiveStateSnapshot renders the worker statuses and PRs for an objective.
// Both the idle re-poke and a manager respawn embed it, since the manager has no
// read tools of its own — the snapshot is how a blind (or brand-new) manager
// learns what already happened.
func (o *Orchestrator) objectiveStateSnapshot(objectiveID string) string {
	var b strings.Builder
	sessions, _ := o.st.ListSessionsByObjective(objectiveID)
	b.WriteString("Worker status:\n")
	wrote := false
	for _, s := range sessions {
		if s.Role == model.RoleManager {
			continue
		}
		wrote = true
		sum := relaySummaryLine(s)
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
	return b.String()
}

// idleManagerPokeMessage is the re-poke text for a live but stalled manager.
func (o *Orchestrator) idleManagerPokeMessage(objectiveID string) string {
	return "No workers are currently running for this objective, so nothing is making progress right now.\n\n" +
		o.objectiveStateSnapshot(objectiveID) +
		"\nDecide the next step now. If the objective is complete and every PR has merged, call " +
		"mark_objective_done. If work remains, spawn the next worker. If you are blocked on a decision only " +
		"the user can make, call ask_user. Do not end your turn without taking one of these actions."
}

// resumeManagerContext frames the state snapshot for a freshly respawned manager.
func (o *Orchestrator) resumeManagerContext(objectiveID string) string {
	return "You are resuming management of an objective that was already in progress; the previous manager " +
		"session ended. Below is the current state — continue from here. Do NOT restart work that is already " +
		"done or re-spawn workers that already ran; pick up the remaining work and drive it to completion.\n\n" +
		o.objectiveStateSnapshot(objectiveID)
}
