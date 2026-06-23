package orch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/store"
)

// The manager tool surface. These methods back the manager agent's tools
// (spawn_session, ask_user, publish_pr, update_pr, comment_pr, address_pr_feedback, create_note,
// mark_objective_done, cancel_session). Each acquires the objective_manager
// lock so only one manager mutation per objective happens at a time.

// withManagerLock runs fn while holding the objective_manager lock.
func (o *Orchestrator) withManagerLock(objectiveID, managerSessionID string, fn func() error) error {
	key := managerLockKey(objectiveID)
	if err := o.st.AcquireLock(key, model.LockObjectiveManager, managerSessionID, "manager mutation"); err != nil {
		if errors.Is(err, store.ErrLockHeld) {
			return errors.New("orch: another manager mutation is in progress")
		}
		return err
	}
	defer o.st.ReleaseLock(key, managerSessionID)
	return fn()
}

// SpawnSession is the manager's spawn_session tool: it creates a scoped worker
// session under the objective. Dependencies/hints are recorded in metadata.
func (o *Orchestrator) SpawnSession(managerSessionID string, spec SpawnSpec) (*model.Session, error) {
	mgr, err := o.st.GetSession(managerSessionID)
	if err != nil {
		return nil, err
	}
	spec.ObjectiveID = mgr.ObjectiveID
	spec.ParentSessionID = managerSessionID
	if spec.Agent == "" {
		spec.Agent = o.defaultAgent()
	}
	// Coding workers run one-shot (do the task and finish), which is what drives
	// the worker-complete -> manager-notify handoff.
	if spec.Mode == "" && needsIsolatedWorkspace(spec.Role) {
		spec.Mode = model.ModeNoninteractive
	}
	var out *model.Session
	err = o.withManagerLock(mgr.ObjectiveID, managerSessionID, func() error {
		s, err := o.CreateSession(spec)
		out = s
		return err
	})
	return out, err
}

// AskUser is the manager's ask_user tool: it opens a first-class question that
// notifies the user.
func (o *Orchestrator) AskUser(sessionID, question, contextStr string, priority int) (*model.Question, error) {
	sess, err := o.st.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	q := &model.Question{
		ObjectiveID: sess.ObjectiveID,
		SessionID:   sessionID,
		Priority:    priority,
		Question:    question,
		Context:     contextStr,
	}
	if err := o.st.CreateQuestion(q); err != nil {
		return nil, err
	}
	// Mark the objective as waiting on the user.
	if sess.ObjectiveID != "" {
		if obj, err := o.st.GetObjective(sess.ObjectiveID); err == nil && obj.Status == model.ObjectiveActive {
			_ = o.st.UpdateObjectiveStatus(sess.ObjectiveID, model.ObjectiveWaitingUser, "")
		}
	}
	o.audit(sess.ObjectiveID, sessionID, "ask_user", question, nil)
	return q, nil
}

// AnswerQuestion records a user's answer and, if no other questions remain
// open, returns the objective to active.
func (o *Orchestrator) AnswerQuestion(questionID, answer string) (*model.Question, error) {
	q, err := o.st.AnswerQuestion(questionID, answer)
	if err != nil {
		return nil, err
	}
	if q.SessionID != "" {
		_ = o.emit(q.SessionID, model.MsgUser, model.KindText, "answer: "+answer, model.JSONMap{"question_id": questionID})
	}
	if q.ObjectiveID != "" {
		open, _ := o.st.ListOpenQuestions()
		stillBlocked := false
		for _, oq := range open {
			if oq.ObjectiveID == q.ObjectiveID {
				stillBlocked = true
				break
			}
		}
		if !stillBlocked {
			if obj, err := o.st.GetObjective(q.ObjectiveID); err == nil && obj.Status == model.ObjectiveWaitingUser {
				_ = o.st.UpdateObjectiveStatus(q.ObjectiveID, model.ObjectiveActive, "")
			}
		}
	}
	o.audit(q.ObjectiveID, q.SessionID, "question_answered", answer, model.JSONMap{"question_id": questionID})
	return q, nil
}

// CreateNote is the manager's create_note tool: it records a note artifact in
// shared memory (not stdout).
func (o *Orchestrator) CreateNote(sessionID, title, body string) (*model.Artifact, error) {
	sess, err := o.st.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	a := &model.Artifact{
		ObjectiveID: sess.ObjectiveID,
		SessionID:   sessionID,
		Kind:        model.ArtifactNote,
		Title:       title,
		Summary:     body,
		Visibility:  model.VisibilitySecondary,
	}
	if err := o.st.CreateArtifact(a); err != nil {
		return nil, err
	}
	return a, nil
}

// CommentPR is the manager's comment_pr tool.
func (o *Orchestrator) CommentPR(ctx context.Context, prID, body string) error {
	if o.forge == nil {
		return errors.New("orch: no forge configured")
	}
	pr, err := o.st.GetPR(prID)
	if err != nil {
		return err
	}
	// Tag orcha-posted comments so the feedback monitor never reacts to its own
	// (or the manager's) replies as if they were new user feedback. Agents often
	// copy the marker into their own body (they see it on prior comments), so add
	// it only when absent — otherwise the comment ends with a doubled marker.
	if !strings.Contains(body, orchaBotMarker) {
		body = body + "\n\n" + orchaBotMarker
	}
	if err := o.forge.Comment(ctx, pr.Repo, pr.Number, body); err != nil {
		return err
	}
	o.audit(pr.ObjectiveID, "", "pr_comment", "manager comment", model.JSONMap{"pr_id": prID})
	return nil
}

// MarkObjectiveDone is the manager's mark_objective_done tool. Marking the
// objective succeeded also finalizes the manager session itself — its TUI is a
// long-lived conversation that never exits on its own, so without this the
// manager would sit running forever after declaring the work complete.
func (o *Orchestrator) MarkObjectiveDone(managerSessionID, summary string) error {
	mgr, err := o.st.GetSession(managerSessionID)
	if err != nil {
		return err
	}
	// Capture any PR the manager opened out-of-band (e.g. via the gh CLI rather
	// than publish_pr) before the gate/finalize, so it is tracked and seen here.
	o.AdoptUntrackedPRs(context.Background(), mgr.ObjectiveID)
	// An objective is not done while it has open (unmerged) PRs: the work lands
	// when they MERGE, not when they are opened. Refuse so the manager keeps the
	// objective alive and waits — it is steered automatically when a PR merges,
	// gets review, or needs conflict/CI fixes. This is the guard that stops an
	// objective from going prematurely "succeeded" with an unmerged (or
	// conflicting) PR still outstanding.
	if prs, err := o.st.ListPRsByObjective(mgr.ObjectiveID); err == nil {
		var open []int
		for _, pr := range prs {
			if pr.Status == model.PROpen || pr.Status == model.PRDraft {
				open = append(open, pr.Number)
			}
		}
		if len(open) > 0 {
			return fmt.Errorf("objective is not done: %d open PR(s) %v are not merged yet. "+
				"It completes when its PRs merge — you are steered automatically then. If a PR "+
				"needs review replies, CI fixes, or conflict resolution, call address_pr_feedback "+
				"to push fixes; do NOT mark the objective done now", len(open), open)
		}
	}
	if err := o.withManagerLock(mgr.ObjectiveID, managerSessionID, func() error {
		return o.st.UpdateObjectiveStatus(mgr.ObjectiveID, model.ObjectiveSucceeded, summary)
	}); err != nil {
		return err
	}
	o.audit(mgr.ObjectiveID, managerSessionID, "objective_done", "objective complete: "+summary, nil)
	// The objective is finished, so its shared scratch — throwaway cross-worker
	// artifacts with no value past the objective — can go. Reaping here keeps
	// WorkRoot/scratch from growing without bound.
	o.reapSharedScratch(context.Background(), mgr.ObjectiveID)
	o.finalizeSession(managerSessionID, model.SessionSucceeded)
	return nil
}

// reapSharedScratch removes a finished objective's shared scratch directories.
// The scratch holds throwaway artifacts (harnesses, repro scripts, generated
// data) that have no value once the objective is done, so leaving them would let
// WorkRoot/scratch grow without bound. Best-effort: a failure is audited, not
// surfaced, since the objective has already succeeded. Only shared scratch is
// touched — isolated checkouts are reaped on their own lifecycle.
func (o *Orchestrator) reapSharedScratch(ctx context.Context, objectiveID string) {
	wss, err := o.st.ListWorkspacesByObjective(objectiveID)
	if err != nil {
		return
	}
	for _, ws := range wss {
		if ws.Kind != model.WorkspaceShared || ws.Status == model.WorkspaceArchived {
			continue
		}
		// Defensive: never rm -rf a path that is not a scratch dir.
		if ws.Path == "" || !strings.Contains(ws.Path, "/scratch/") {
			continue
		}
		if o.preparer != nil {
			if tgt, terr := o.st.GetTarget(ws.TargetID); terr == nil {
				ex := agent.NewExecutor(tgt)
				if _, rerr := exec.RunCapture(ctx, ex, exec.Command{Name: "rm", Args: []string{"-rf", ws.Path}}); rerr != nil {
					o.audit(objectiveID, "", "shared_scratch_reap_failed", rerr.Error(), model.JSONMap{"workspace_id": ws.ID})
					continue
				}
			}
		}
		_ = o.st.SetWorkspaceStatus(ws.ID, model.WorkspaceArchived)
	}
}

// finalizeSession drives a session to a terminal status and tears down its live
// run (killing the agent process / tmux session). Used for sessions that do not
// end by a provider-emitted done event — e.g. a manager declaring the objective
// complete. Safe when there is no live run.
func (o *Orchestrator) finalizeSession(sessionID string, status model.SessionStatus) {
	o.mu.Lock()
	r := o.runs[sessionID]
	o.mu.Unlock()
	_, _ = o.st.UpdateSessionStatus(sessionID, status)
	if r != nil {
		// The run's consume loop sees the canceled context and unwinds (releasing
		// locks/slot via cleanupRun); the already-terminal status it set above is
		// preserved because a canceled run does not force its own terminal state.
		_ = r.provider.CancelSession(r.handle)
		r.cancel()
	} else if sess, err := o.st.GetSession(sessionID); err == nil {
		o.releaseSessionLocks(sess)
		o.releaseTargetSlot(sess)
		o.notifyChange()
	}
	_ = o.st.CancelOpenQuestionsBySession(sessionID)
}

// sharedScratchPath is where an objective's shared scratch dir lives on a
// target: a plain directory (no git tree) beside the per-session checkouts.
func sharedScratchPath(tgt *model.Target, objectiveID string) string {
	return tgt.WorkRoot + "/scratch/" + objectiveID
}

// EnsureSharedScratch returns the objective's shared scratch directory on a
// target, creating both the workspace row and the on-disk directory if absent.
// Each objective has one shared scratch per target.
//
// Unlike an isolated checkout — which is a fresh git tree, single-writer, and
// rm -rf'd and re-cloned per session — the shared scratch is plain and is NEVER
// torn down between worker sessions. It is where workers keep task-scoped
// artifacts (a benchmark or profiling harness, a repro script, generated data)
// that must survive across the objective's workers but must NOT ship in any PR.
// Without it such artifacts die with the worker's checkout, so a later worker
// reports "the harness isn't in the tree" and cannot reproduce the result.
func (o *Orchestrator) EnsureSharedScratch(ctx context.Context, objectiveID string, tgt *model.Target) (*model.Workspace, error) {
	// Serialize the find-or-create: workers now start concurrently, and two on the
	// same objective+target would otherwise both miss the lookup and create
	// duplicate shared-scratch rows. The mkdir below is idempotent; the DB row is
	// what needs the guard.
	o.scratchMu.Lock()
	defer o.scratchMu.Unlock()

	ws, err := o.st.SharedScratchFor(objectiveID, tgt.ID)
	if err != nil {
		ws = &model.Workspace{
			ObjectiveID: objectiveID,
			TargetID:    tgt.ID,
			Kind:        model.WorkspaceShared,
			ProjectPath: tgt.WorkRoot,
			VCS:         model.VCSNone,
			Path:        sharedScratchPath(tgt, objectiveID),
			Status:      model.WorkspaceReady,
		}
		if err := o.st.CreateWorkspace(ws); err != nil {
			return nil, err
		}
	}
	// Materialize the directory on the target (idempotent). Gated on a preparer
	// like the isolated checkout is, so offline/test runs don't touch disk.
	if o.preparer != nil {
		ex := agent.NewExecutor(tgt)
		if _, err := exec.RunCapture(ctx, ex, exec.Command{Name: "mkdir", Args: []string{"-p", ws.Path}}); err != nil {
			return ws, fmt.Errorf("orch: mkdir shared scratch: %w", err)
		}
	}
	return ws, nil
}
