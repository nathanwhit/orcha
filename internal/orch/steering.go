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

	// Re-read the session so we pick up any provider_session_id captured during
	// the prior run (used to resume the same conversation/thread).
	if fresh, err := o.st.GetSession(sess.ID); err == nil {
		sess = fresh
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
	spec := agent.Spec{
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
	switch {
	// Manager: full tool surface. The MCP base is per-target: remote sessions
	// reach the orchestrator through a managed reverse tunnel on their own
	// loopback (the configured base points at the wrong machine from there).
	case sess.Role == model.RoleManager && o.cfg.ManagerMCPBaseURL != "":
		spec.MCP = map[string]string{"orcha": o.mcpBaseFor(tgt) + "/mcp/" + sess.ID}
		spec.AllowedTools = []string{"mcp__orcha"}
		spec.PermissionMode = "default"
		if spec.Prompt != "" {
			spec.Prompt = managerSystemPreamble + "\n\n" + spec.Prompt
		}
	// PR/CI follow-up: the agent itself decides how to respond, using its tools
	// (update_pr to push a fix, comment_pr to reply, ask_user, create_note).
	case sess.Role == model.RolePRFollowup || sess.Role == model.RoleCIFollowup:
		if o.cfg.ManagerMCPBaseURL != "" {
			spec.MCP = map[string]string{"orcha": o.mcpBaseFor(tgt) + "/mcp/" + sess.ID}
			spec.AllowedTools = []string{"mcp__orcha"}
		}
		spec.PermissionMode = o.cfg.WorkerPermissionMode // shell so it can commit
		if spec.Prompt != "" {
			spec.Prompt = followupSystemPreamble + "\n\n" + spec.Prompt
		}
	// Other coding workers run one-shot in a checkout and do not publish.
	case isCodingWorker(sess.Role):
		spec.PermissionMode = o.cfg.WorkerPermissionMode
		if spec.Prompt != "" {
			spec.Prompt = workerSystemPreamble + "\n\n" + spec.Prompt
		}
	}
	return spec
}

// followupSystemPreamble orients a PR follow-up agent. It must decide and act —
// the orchestrator does not respond on its behalf.
const followupSystemPreamble = `You are a PR follow-up agent, running in a checkout of
the PR's branch. Read the feedback and DECIDE how to respond, then act using your
orcha tools (mcp__orcha__*):
- If a code change is warranted: edit the files here, then COMMIT it yourself
  with a clear, descriptive commit message (conventional-commits style) using
  "git add -A && git commit", and then call update_pr to push to the PR branch.
- To reply to the reviewer: call comment_pr with a clear, specific message.
- If the feedback is a question: answer it with comment_pr.
- If it is non-actionable or you disagree: explain why with comment_pr.
- If you are blocked or need a decision: call ask_user.
Always leave at least a comment so the reviewer knows the outcome. Commit with
git, but do not "git push" or use the gh CLI directly and do not change the git
author/identity — push and comment through the tools.`

// workerSystemPreamble orients a one-shot worker.
const workerSystemPreamble = `You are a worker on an engineering team, running in an
isolated checkout of the repository. Do the assigned task directly: read the
relevant code, make the changes, and keep them small and correct. When the work
is complete, COMMIT it yourself with a clear, descriptive commit message
(conventional-commits style, e.g. "feat: ..."/"fix: ..."/"docs: ...") that
explains what changed and why — run "git add -A && git commit". Do NOT push or
open a PR and do NOT amend the git author/identity — the orchestrator publishes
your commit. Finish with a brief summary of what you changed.`

// managerSystemPreamble orients the manager agent toward the tool surface and
// the operating rules from the spec.
const managerSystemPreamble = `You are the MANAGER of an engineering team working toward an objective.
You coordinate via your orcha tools (mcp__orcha__*): spawn_session to delegate
scoped work, ask_user when direction/credentials are unclear, publish_pr to ship
coherent slices, comment_pr/update_pr for follow-ups, create_note for shared
memory, and mark_objective_done when finished. Prefer several clean PR-sized
slices over one giant PR. Keep working after publishing intermediate PRs unless
truly blocked. You work from summaries; spawn workers to do the actual coding.
Coding workers run in fresh isolated checkouts: if the objective does not
already name a repo, you MUST pass repo (owner/repo) in spawn_session — a
coding worker without a repo fails to start. If you don't know the repo,
ask_user. Keep your messages concise and operational.`
