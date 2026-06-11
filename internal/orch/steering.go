package orch

import (
	"context"
	"fmt"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/model"
)

// Steer delivers a user/manager message to a session.
//
// For interactive sessions with a live run, this is simply send_input to the
// running process. For non-interactive sessions (or when there is no live
// interactive run), it follows the spec's steering protocol: cancel the current
// process, record the steering message, and resume with compact context while
// preserving the logical session identity.
func (o *Orchestrator) Steer(ctx context.Context, sessionID, text string) error {
	sess, err := o.st.GetSession(sessionID)
	if err != nil {
		return err
	}
	if sess.Status.IsTerminal() {
		return fmt.Errorf("orch: cannot steer terminal session %s", sessionID)
	}

	// Record the steering message in the transcript regardless of path.
	_ = o.emit(sessionID, model.MsgUser, model.KindText, text, model.JSONMap{"steer": true})

	o.mu.Lock()
	r := o.runs[sessionID]
	o.mu.Unlock()

	if r != nil && r.interactive {
		o.audit(sess.ObjectiveID, sessionID, "steer_interactive", "sent input to live session", nil)
		return r.provider.SendInput(r.handle, text)
	}

	// Non-interactive path: cancel the current process, then resume with compact
	// context. The session row (and thus its identity) is preserved — we never
	// transition it to a terminal status here.
	o.resumeWithSteer(ctx, sess, text, r)
	return nil
}

// resumeWithSteer implements the non-interactive steering protocol.
func (o *Orchestrator) resumeWithSteer(ctx context.Context, sess *model.Session, text string, r *run) {
	if r != nil {
		_ = r.provider.CancelSession(r.handle) // cancel current process
		r.cancel()
		<-r.done // wait for the prior run to fully unwind (releases its locks)
	}

	prov, ok := o.provider(sess.Agent)
	if !ok {
		return
	}

	// Re-acquire the workspace lock (idempotent for the same holder).
	if sess.WorkspaceID != "" {
		_ = o.st.AcquireLock(workspaceLockKey(sess.WorkspaceID), model.LockWorkspace, sess.ID, "steer resume")
	}

	var ws *model.Workspace
	if sess.WorkspaceID != "" {
		ws, _ = o.st.GetWorkspace(sess.WorkspaceID)
	}
	var tgt *model.Target
	if sess.TargetID != "" {
		tgt, _ = o.st.GetTarget(sess.TargetID)
	}

	spec := o.buildSpec(sess, ws, tgt)
	// Fold the steering message into the compact context handed to the resumed
	// process so it sees the new direction.
	spec.CompactContext += "\n\nSTEER: " + text

	runCtx, cancel := context.WithCancel(ctx)
	handle, events, err := prov.ResumeSession(runCtx, sess.ID, spec)
	if err != nil {
		cancel()
		o.audit(sess.ObjectiveID, sess.ID, "steer_resume_failed", err.Error(), nil)
		return
	}
	nr := &run{
		sessionID:   sess.ID,
		provider:    prov,
		handle:      handle,
		cancel:      cancel,
		interactive: handle.Interactive(),
		done:        make(chan struct{}),
		spec:        spec,
	}
	o.mu.Lock()
	o.runs[sess.ID] = nr
	o.mu.Unlock()
	o.audit(sess.ObjectiveID, sess.ID, "steer_resume", "canceled and resumed with steer", nil)
	go o.consume(nr, events)
}

// buildSpec assembles the agent.Spec handed to a provider from a session and
// its bound resources. The context is summary-only.
func (o *Orchestrator) buildSpec(sess *model.Session, ws *model.Workspace, tgt *model.Target) agent.Spec {
	return agent.Spec{
		SessionID:      sess.ID,
		Role:           sess.Role,
		Mode:           sess.Mode,
		Goal:           sess.Goal,
		Prompt:         sess.Goal,
		CompactContext: o.compactContext(sess.ObjectiveID),
		Workspace:      ws,
		Target:         tgt,
		Metadata:       sess.Metadata,
	}
}
