package orch

import (
	"context"
	"errors"

	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/store"
)

// The manager tool surface. These methods back the manager agent's tools
// (spawn_session, ask_user, publish_pr, update_pr, comment_pr, create_note,
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
	if err := o.forge.Comment(ctx, pr.Repo, pr.Number, body); err != nil {
		return err
	}
	o.audit(pr.ObjectiveID, "", "pr_comment", "manager comment", model.JSONMap{"pr_id": prID})
	return nil
}

// MarkObjectiveDone is the manager's mark_objective_done tool.
func (o *Orchestrator) MarkObjectiveDone(managerSessionID, summary string) error {
	mgr, err := o.st.GetSession(managerSessionID)
	if err != nil {
		return err
	}
	return o.withManagerLock(mgr.ObjectiveID, managerSessionID, func() error {
		return o.st.UpdateObjectiveStatus(mgr.ObjectiveID, model.ObjectiveSucceeded, summary)
	})
}

// EnsureSharedScratch returns the objective's shared scratch workspace on a
// target, creating it if absent. Each objective may have one shared scratch per
// target.
func (o *Orchestrator) EnsureSharedScratch(objectiveID, targetID string) (*model.Workspace, error) {
	if ws, err := o.st.SharedScratchFor(objectiveID, targetID); err == nil {
		return ws, nil
	}
	tgt, err := o.st.GetTarget(targetID)
	if err != nil {
		return nil, err
	}
	ws := &model.Workspace{
		ObjectiveID: objectiveID,
		TargetID:    targetID,
		Kind:        model.WorkspaceShared,
		ProjectPath: tgt.WorkRoot,
		VCS:         model.VCSNone,
		Path:        tgt.WorkRoot + "/scratch/" + objectiveID,
		Status:      model.WorkspaceReady,
	}
	if err := o.st.CreateWorkspace(ws); err != nil {
		return nil, err
	}
	return ws, nil
}
