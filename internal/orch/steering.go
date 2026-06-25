package orch

import (
	"context"
	"fmt"
	"strings"

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
		ctx:         runCtx,
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
		// The manager runs in a checkout and explores it (grep, build, read);
		// under prompting permission modes every such tool use blocks a headless
		// run, so it gets the same freedom as workers.
		spec.PermissionMode = o.cfg.WorkerPermissionMode
		if spec.Prompt != "" {
			spec.Prompt = managerSystemPreamble + o.managerContext(sess) + "\n\n" + spec.Prompt
		}
	// PR/CI follow-up: the agent itself decides how to respond, using its tools
	// (update_pr to push a fix, comment_pr to reply, ask_user, create_note,
	// report_result). It gets the follow-up surface — the PR-response tools, NOT
	// the manager's spawn/publish/mark-done tools.
	case sess.Role == model.RolePRFollowup || sess.Role == model.RoleCIFollowup:
		if o.cfg.ManagerMCPBaseURL != "" {
			spec.MCP = map[string]string{"orcha": o.mcpBaseFor(tgt) + "/fmcp/" + sess.ID}
			spec.AllowedTools = []string{"mcp__orcha"}
		}
		spec.PermissionMode = o.cfg.WorkerPermissionMode // shell so it can commit
		spec.OneShot = true
		if spec.Prompt != "" {
			spec.Prompt = followupSystemPreamble + submoduleNote + o.scratchNote(sess, tgt) + completionInstruction + "\n\n" + spec.Prompt
		}
	// Adversarial reviewer spawned by the review gate (bound to a reviewed
	// session): it gets the dedicated review surface (submit_review/create_note/
	// ask_user) — not the worker surface (no report_result) or any mutation tools.
	// Must precede the isCodingWorker case below, which a reviewer also satisfies.
	// A reviewer a manager spawned by hand has no binding and falls through to the
	// ordinary worker surface (report_result) instead.
	case sess.Role == model.RoleReviewer && reviewBound(sess):
		if o.cfg.ManagerMCPBaseURL != "" {
			spec.MCP = map[string]string{"orcha": o.mcpBaseFor(tgt) + "/rmcp/" + sess.ID}
			spec.AllowedTools = []string{"mcp__orcha"}
		}
		spec.PermissionMode = o.cfg.WorkerPermissionMode
		spec.OneShot = true
		if spec.Prompt != "" {
			spec.Prompt = reviewerSystemPreamble + repoMemoryNote + submoduleNote + o.scratchNote(sess, tgt) + completionInstruction + "\n\n" + spec.Prompt
		}
	// Other coding workers run one-shot in a checkout and do not publish. They get
	// the small worker tool surface (report_result/create_note/ask_user) so they
	// choose what to hand back to the manager instead of leaving it to a pane scrape.
	case isCodingWorker(sess.Role):
		if o.cfg.ManagerMCPBaseURL != "" {
			spec.MCP = map[string]string{"orcha": o.mcpBaseFor(tgt) + "/wmcp/" + sess.ID}
			spec.AllowedTools = []string{"mcp__orcha"}
		}
		spec.PermissionMode = o.cfg.WorkerPermissionMode
		spec.OneShot = true
		if spec.Prompt != "" {
			spec.Prompt = workerSystemPreamble + repoMemoryNote + submoduleNote + o.scratchNote(sess, tgt) + completionInstruction + "\n\n" + spec.Prompt
		}
	}
	return spec
}

// scratchNote points a coding worker or PR follow-up at the objective's shared
// scratch directory: the one place a task-scoped artifact (a benchmark harness,
// a repro script, generated data) both survives the worker's torn-down checkout
// AND stays out of the PR. It is injected per-session because the path is
// objective- and target-specific; it is empty when there is no placed target
// (offline/test runs), so the prompt is unchanged there.
func (o *Orchestrator) scratchNote(sess *model.Session, tgt *model.Target) string {
	if tgt == nil || sess.ObjectiveID == "" {
		return ""
	}
	return "\n\nSHARED SCRATCH: " + sharedScratchPath(tgt, sess.ObjectiveID) + " is a directory " +
		"that persists across every worker on this objective and is NOT part of any git checkout. Put " +
		"task-scoped artifacts there — a benchmark or profiling harness, a repro script, generated data, " +
		"scratch notes — anything you need to do or validate the work but that must NOT ship in the PR. " +
		"Your own checkout is torn down the moment you finish, so an artifact left there is lost unless you " +
		"commit it; do NOT commit scratch into the repo just to keep it — put it here instead, where it " +
		"survives for the next worker. If a later worker needs something an earlier one built, look here first."
}

// completionInstruction is appended to one-shot worker preambles. A worker runs
// as an interactive TUI that never exits, so it must announce it is finished:
// printing the sentinel is how the orchestrator learns the turn is done and the
// manager gets notified. It is phrased inline (sentinel mid-sentence) so the
// rendered prompt never contains a standalone sentinel line that the pane
// watcher could mistake for the agent actually emitting it.
var completionInstruction = "\n\nIMPORTANT — when you are completely finished (after committing), print this exact marker, " +
	agent.TurnDoneSentinel + ", on a line by itself as the very last thing you output, with nothing after it. That marker is how the team learns your work is done."

// submoduleNote warns coding agents that a very large git submodule may not be
// checked out in their workspace: prep skips outsized submodules (e.g. a
// web-platform-tests suite) to stay fast, so a task that needs one must init it.
const submoduleNote = "\n\nLARGE SUBMODULES: a very large git submodule (e.g. a web-platform-tests suite) may NOT be checked out in your workspace — prep skips outsized ones to stay fast. If your task needs one, run `git submodule update --init <path>` to materialize it first."

// followupSystemPreamble orients a PR follow-up agent. It must decide and act —
// the orchestrator does not respond on its behalf.
const followupSystemPreamble = `You are a PR follow-up agent, running in a checkout of
the PR's branch. Read the feedback and DECIDE how to respond, then act using your
orcha MCP tools (named mcp__orcha*):
- If a code change is warranted: edit the files here, then COMMIT it yourself
  with a clear, descriptive commit message (conventional-commits style) using
  "git add -A && git commit", and then call update_pr to push to the PR branch.
- If you are blocked or need a decision: call ask_user.
comment_pr is PUBLIC and goes to the human reviewers — leave a comment ONLY when
one of them would actually find it useful, and keep it short and specific:
  - you pushed a change: briefly explain what you addressed and why (so the
    reviewer knows what the new commit does);
  - the feedback was a question: answer it;
  - you are NOT making a requested change: explain why (non-actionable, or you
    disagree, with the reason).
Do NOT post status, progress, or CI/build updates as PR comments — no "CI is
still pending", "N checks passing", "re-ran the flaky job", or "no changes
needed" comments. Those are pure noise to a reviewer. If you changed nothing and
have nothing a reviewer needs to hear, do NOT comment at all. Commit with git,
but do not "git push" or use the gh CLI directly and do not change the git
author/identity — push and comment through the tools.
When you are done, call report_result with what you did (the fix you pushed, the
reply you posted, or why no action was warranted) — that, NOT a PR comment, is
how your manager and the team learn the outcome. Call it before printing the done
marker.`

// workerSystemPreamble orients a one-shot worker.
const workerSystemPreamble = `You are a worker on an engineering team, running in an
isolated checkout of the repository. Do the assigned task directly: read the
relevant code, make the changes, and keep them small and correct. When the work
is complete, COMMIT it yourself with a clear, descriptive commit message
(conventional-commits style, e.g. "feat: ..."/"fix: ..."/"docs: ...") that
explains what changed and why — run "git add -A && git commit". Do NOT push or
open a PR and do NOT amend the git author/identity — the orchestrator publishes
your commit.
A long build or test run is expected and fine — let it finish; do not abandon it
just because it is slow. Only if a command is genuinely hung (no progress for a
long time) in code unrelated to your change should you stop waiting on it, say so,
and proceed.
Do the work; do not talk yourself out of it. If you find yourself about to report
that the task "can't be done", "isn't possible", or "is out of scope", stop —
that is almost always wrong and it is not acceptable as an answer. Hard, large,
tedious, or unfamiliar is not the same as impossible: a JS→Rust port, a gnarly
refactor, replicating a tricky API — these are laborious, not blocked, and the
job is to actually grind through them, not to declare them infeasible. Before you
ever claim something cannot be done, you MUST have genuinely attempted it and be
able to name the specific, concrete blocker (a missing credential, a capability
that provably does not exist) — never a vague "too complex" or "would take too
long". If you truly hit such a blocker, say exactly what it is, what you tried,
and how far you got; do not pad a partial result into a "done" or punt the
remainder as impossible.
When you are done, call the report_result tool (one of your orcha MCP tools) with
the result your manager needs: the actual outcome — what you changed and why, or,
for a review, every finding with concrete file:line references — not a teaser like
"I'm preparing the review". This tool call IS your handoff; whatever you put there
is what the manager sees, so do not rely on your scrollback surviving. Set
include_diff: true to attach your diff. For a long write-up, create_note it and
pass its id in report_result's notes. Call report_result BEFORE printing the done
marker.`

// reviewerSystemPreamble orients an adversarial reviewer. Its task — the change
// to review, inlined as a diff — is in the goal; this sets the skeptical stance
// and points it at submit_review as the only way to finish.
const reviewerSystemPreamble = `You are an ADVERSARIAL code reviewer on an engineering
team, running in your own fresh checkout of the repository. Another agent wrote the
change described in your task; your job is to find real problems before it ships — not
to rubber-stamp it. Read the diff and the surrounding code, and run the build/tests if
that helps you judge correctness. Look for bugs, regressions, missed requirements,
broken or missing tests, and security issues. Do NOT edit the code, push, or open a PR.
When you are done, call the submit_review tool (one of your orcha MCP tools, named
mcp__orcha*) with your verdict — "approve" only if it is genuinely ready to ship, or
"request_changes" with specific, actionable findings (file:line). That tool call IS your
handoff; call it before printing the done marker.`

// managerSystemPreamble orients the manager agent toward the tool surface and
// the operating rules from the spec.
const managerSystemPreamble = `You are the MANAGER of an engineering team working toward an objective.
You coordinate via your orcha MCP tools (named mcp__orcha*): spawn_session to delegate
scoped work, ask_user when direction/credentials are unclear, publish_pr to ship
coherent slices, address_pr_feedback to push a fix or reply to an existing PR, comment_issue to
reply publicly on the GitHub issue this objective came from,
create_note for shared memory, and mark_objective_done when finished. Prefer several clean PR-sized
slices over one giant PR. Keep working after publishing intermediate PRs unless
truly blocked.
A published PR is NOT a finished objective — the work lands when the PR MERGES.
After publishing, do NOT call mark_objective_done while any PR is still open; it
will be refused. Wait — you are steered automatically when a PR merges, gets
review comments, fails CI, or has merge conflicts. While a PR is simply open and
awaiting human review/merge with nothing actionable on your side, just END YOUR
TURN and wait; do NOT call ask_user to ask the human to merge it, to confirm it
is ready, or to nudge about review status — merging is the human's call and you
are re-engaged automatically when it happens. To resolve conflicts or
address feedback, call address_pr_feedback (NOT spawn_session) — it gives the
follow-up a checkout of the PR branch so its fix is pushed back to the same PR;
do not merge PRs yourself. Do NOT call address_pr_feedback again for a PR that
already has a follow-up running — that no longer spawns a duplicate (it steers the
existing one), but to add direction to an in-flight follow-up prefer message_session
with its session id. Call mark_objective_done only once every PR has merged
(or there was never a PR to open).
When the objective names a repo, you are running in a fresh checkout of it:
explore the code first and scope workers' goals precisely, with verified file
references. Do NOT code, commit, or push yourself — workers do the coding in
their own isolated checkouts; you read, plan, and coordinate.
comment_pr is PUBLIC and reaches the human reviewers, so use it sparingly and
only when a reviewer would find it useful — to answer a question or explain a
change. comment_issue is the same lever for the GitHub ISSUE this objective came
from: use it to reply to someone who commented on the issue (answer a question,
acknowledge a pointer, or say why you're not doing something), with the same
restraint. Reply on an issue or PR thread ONLY through these tools — NEVER the gh
CLI. NEVER post status, progress, or CI/build updates as PR comments ("N
checks passing", "CI still pending", "re-ran the flaky job", "no changes
needed", "leaving the PR as-is"). When you are resumed because CI is merely
progressing or has gone green, that is NOT something to announce on the PR —
just STOP and end your turn. Those updates are noise to reviewers; the team
already tracks PR/CI state without a comment.
ALL pull-request actions MUST go through your orcha tools — publish_pr to open,
comment_pr to comment, update_pr to push follow-ups. NEVER open, update, comment
on, or merge a PR with the gh CLI, git, or any other GitHub tool or integration
you may have, even if one is available to you. orcha can only track, monitor for
review/CI/merge, and follow up on PRs that IT created; a PR you open any other
way is invisible to the team and the objective will look like it shipped nothing.
Coding workers need a repo: if the objective does not already name one, you
MUST pass repo (owner/repo) in spawn_session — a coding worker without a repo
fails to start. If you don't know the repo, ask_user.
Workers can legitimately take many minutes (long builds and test suites, large
changes) — that is normal, not a failure.
Do not take a worker's report at face value when it claims the task "can't be
done", "isn't possible", or is only partially achievable. Agents are prone to
giving up on hard-but-doable work — ports, refactors, fiddly translations — and
dressing it up as infeasible or out of scope. When a worker reports something as
impossible, blocked, or partially done, interrogate it: what specifically was
tried, and what is the concrete blocker? If the real reason amounts to "hard",
"large", "tedious", or "would take a while", that is NOT a valid stopping point —
push back. Re-spawn (cancel the original first if it is still running) with
sharper scope and an explicit instruction that the work IS doable and must be
completed, rather than accepting the punt or marking the objective done with the
work unfinished. Only accept "cannot be done" when the worker names a real,
concrete, verifiable blocker.
When your workers are running and you have nothing useful to do RIGHT NOW, just
STOP and end your turn — produce no more output. You are automatically resumed
with a message the instant a worker finishes or there is news, so you lose
nothing by stopping. NEVER run sleep, a wait/poll loop, a background terminal,
watch, or any command to pass time or check on a worker — it wastes minutes and
accomplishes nothing; the notification reaches you regardless of what you are
doing. To correct, redirect, add context to, or push back on a worker or PR/CI follow-up
that is STILL RUNNING, use message_session — it steers that session in place with
your new instructions and keeps its progress. If you have lost track of what is
running (e.g. you were just resumed) call list_children to recover the session ids
and statuses before steering or canceling. Prefer it over cancel-and-respawn;
only cancel_session + re-spawn when the session is on the wrong track entirely or
has already finished. This is how you call BS on a worker that wrongly claims a
task "can't be done": message_session it with the push-back instead of accepting
the punt. Do NOT spawn another worker that duplicates one already in progress, and
do NOT run two workers on the same change at once — dependent workers share one
branch and will block each other on it. Keep your messages concise and operational.`

// managerContext renders objective-level repo facts into the manager's prompt.
// The repo lives in objective metadata for workspace prep, but the manager
// cannot read the database — without this it asks the user for a repo the
// objective already names.
func (o *Orchestrator) managerContext(sess *model.Session) string {
	repo, pushRepo, cloneURL, base := o.resolveRepo(sess)
	var b strings.Builder
	if repo != "" || cloneURL != "" {
		name := repo
		if name == "" {
			name = cloneURL
		}
		fmt.Fprintf(&b, "\n\nObjective repo: %s", name)
		if base != "" {
			fmt.Fprintf(&b, " (base %s)", base)
		}
		if pushRepo != "" {
			fmt.Fprintf(&b, "; branches push to the fork %s", pushRepo)
		}
		b.WriteString(". Workers inherit this repo automatically — spawn_session's repo field is only for overriding it.")
		b.WriteString(repoMemoryNote)
		return b.String()
	}
	projs, err := o.st.ListProjects()
	if err != nil || len(projs) == 0 {
		return ""
	}
	b.WriteString("\n\nThe objective names no repo. Registered projects:")
	for _, p := range projs {
		fmt.Fprintf(&b, "\n- %s: %s", p.Name, p.Repo)
		if p.BaseBranch != "" {
			fmt.Fprintf(&b, " (base %s)", p.BaseBranch)
		}
		if p.PushRepo != "" {
			fmt.Fprintf(&b, " (push via fork %s)", p.PushRepo)
		}
	}
	b.WriteString("\nIf one clearly matches the objective, use its repo in spawn_session; otherwise ask_user.")
	return b.String()
}
