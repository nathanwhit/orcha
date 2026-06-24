package orch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/store"
)

// SpawnSpec describes a session the manager (or user) wants created.
type SpawnSpec struct {
	ObjectiveID     string
	ParentSessionID string
	Role            model.SessionRole
	Agent           model.AgentKind // preferred provider
	Mode            model.SessionMode
	Title           string
	Goal            string
	Prompt          string
	WorkspaceID     string
	Target          TargetRequest
	// Dependencies are session ids that must succeed before this session is
	// eligible to run. The scheduler enforces them.
	Dependencies []string
	Metadata     model.JSONMap
}

// CreateSession persists a new queued session. It does not start it.
func (o *Orchestrator) CreateSession(spec SpawnSpec) (*model.Session, error) {
	if len(spec.Dependencies) > 0 {
		if spec.Metadata == nil {
			spec.Metadata = model.JSONMap{}
		}
		deps := make([]any, len(spec.Dependencies))
		for i, d := range spec.Dependencies {
			deps[i] = d
		}
		spec.Metadata["depends_on"] = deps
	}
	sess := &model.Session{
		ObjectiveID:     spec.ObjectiveID,
		ParentSessionID: spec.ParentSessionID,
		Role:            spec.Role,
		Agent:           spec.Agent,
		Mode:            spec.Mode,
		Status:          model.SessionQueued,
		Title:           spec.Title,
		Goal:            spec.Goal,
		WorkspaceID:     spec.WorkspaceID,
		UsageProvider:   string(spec.Agent),
		Metadata:        spec.Metadata,
	}
	if sess.Mode == "" {
		sess.Mode = model.ModeInteractive
	}
	if err := o.st.CreateSession(sess); err != nil {
		return nil, err
	}
	o.audit(spec.ObjectiveID, sess.ID, "session_created", "created "+string(spec.Role)+" session", nil)
	o.notifyChange() // new runnable work
	return sess, nil
}

// run is an in-flight session driver.
type run struct {
	sessionID   string
	provider    agent.Provider
	handle      agent.Handle
	ctx         context.Context // the run's lifetime; canceled on shutdown/cancel
	cancel      context.CancelFunc
	interactive bool
	done        chan struct{}
	finalize    sync.Once
	spec        agent.Spec
}

// Run is the public handle to a started session run.
type Run struct{ r *run }

// Wait blocks until the session run's event loop finishes.
func (r *Run) Wait() { <-r.r.done }

// StartRun begins driving a session through its provider. It resolves the
// provider (with usage-aware fallback), acquires the workspace lock if the
// session has a workspace, claims a target slot if not already placed, marks the
// session running, and consumes the provider event stream in the background.
func (o *Orchestrator) StartRun(ctx context.Context, sessionID string) (*Run, error) {
	sess, err := o.st.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if sess.Status.IsTerminal() {
		return nil, fmt.Errorf("orch: cannot start terminal session %s", sessionID)
	}

	// Resolve provider with fallback; ask the user if all are exhausted.
	kind, err := o.SelectProvider(sess.Agent)
	if err != nil {
		if errors.Is(err, ErrProviderExhausted) {
			o.askProviderExhausted(sess)
		}
		return nil, err
	}
	prov, ok := o.provider(kind)
	if !ok {
		return nil, ErrNoProvider
	}

	// Place on a target first so the workspace checkout lands on the same machine
	// the agent will run on.
	if sess.TargetID == "" {
		if _, err := o.PlaceSession(sessionID, o.targetRequestFor(sess)); err != nil {
			_, _ = o.st.UpdateSessionStatus(sessionID, model.SessionWaitingCapacity)
			return nil, err
		}
		sess, _ = o.st.GetSession(sessionID)
	}
	var tgt *model.Target
	if sess.TargetID != "" {
		tgt, _ = o.st.GetTarget(sess.TargetID)
	}

	// Auto-prepare an isolated checkout for coding sessions that don't have one
	// (e.g. workers the manager spawned), on the session's target.
	if err := o.ensureWorkspace(ctx, sess, tgt); err != nil {
		o.releaseTargetSlot(sess)
		// Record WHY in the transcript before failing — the manager is
		// re-prompted on child failure and needs the reason to react.
		_ = o.emit(sessionID, model.MsgSystem, model.KindError, err.Error(), nil)
		_, _ = o.st.UpdateSessionStatus(sessionID, model.SessionFailed)
		return nil, err
	}
	sess, _ = o.st.GetSession(sessionID)

	// Acquire workspace lock (single writer per workspace).
	if sess.WorkspaceID != "" {
		if err := o.st.AcquireLock(workspaceLockKey(sess.WorkspaceID), model.LockWorkspace, sessionID, "session run"); err != nil {
			o.releaseTargetSlot(sess)
			if errors.Is(err, store.ErrLockHeld) {
				// Not a capacity shortage: another session (typically a sibling
				// sharing this inherited workspace) holds the single-writer lock.
				// Record the real reason so the dashboard does not read as "no
				// machine free" when the targets are idle. The scheduler retries
				// this session when the lock is released.
				_, _ = o.st.UpdateSessionStatus(sessionID, model.SessionWaitingCapacity)
				_, _ = o.st.UpdateSessionRuntime(sessionID, func(s *model.Session) {
					s.CurrentActivity = "waiting for workspace (in use by another session)"
				})
			}
			return nil, err
		}
	}

	if _, err := o.st.UpdateSessionStatus(sessionID, model.SessionStarting); err != nil {
		o.releaseSessionLocks(sess)
		o.releaseTargetSlot(sess)
		return nil, err
	}

	var ws *model.Workspace
	if sess.WorkspaceID != "" {
		ws, _ = o.st.GetWorkspace(sess.WorkspaceID)
	}

	// Seed repo-wide memory into the checkout (as .orcha/MEMORY.md) before the
	// agent starts, so it reads past learnings on its first turn.
	o.seedRepoMemory(ctx, sess, ws, tgt)

	// Materialize the objective's shared scratch dir so a coding worker / follow-up
	// has a persistent, non-git place for task artifacts (harnesses, repro scripts)
	// that must outlive its torn-down checkout but must NOT ship in a PR. The path
	// is injected into the prompt by buildSpec.
	if tgt != nil && sess.ObjectiveID != "" && isCodingWorker(sess.Role) {
		if _, serr := o.EnsureSharedScratch(ctx, sess.ObjectiveID, tgt); serr != nil {
			o.audit(sess.ObjectiveID, sessionID, "shared_scratch_failed", serr.Error(), nil)
		}
	}

	spec := o.buildSpec(sess, ws, tgt)

	runCtx, cancel := context.WithCancel(ctx)

	// A fresh interactive start delivers its opening prompt as a positional argv
	// argument, which is size-bounded and fragile for big inputs. Move an oversized
	// one to a file on the target and hand the agent a short bootstrap pointing at
	// it. Resume paths don't re-pass the prompt (it's already in the conversation),
	// so only externalize a cold start.
	if pid, _ := sess.Metadata["provider_session_id"].(string); pid == "" {
		if tmuxName, _ := sess.Metadata["tmux_session"].(string); tmuxName == "" {
			spec = o.externalizeLargePrompt(runCtx, sess, tgt, spec)
		}
	}
	// A session whose prior run captured a durable provider-side handle — a
	// provider conversation id, or a tmux session that outlived the process —
	// RESUMES instead of starting cold. After an orchestrator restart this is
	// what keeps a manager's context (and its knowledge of already-spawned
	// workers) intact rather than re-planning from scratch.
	var (
		handle agent.Handle
		events <-chan agent.Event
	)
	pid, _ := sess.Metadata["provider_session_id"].(string)
	tmuxName, _ := sess.Metadata["tmux_session"].(string)
	if pid != "" || tmuxName != "" {
		handle, events, err = prov.ResumeSession(runCtx, sessionID, spec)
	} else {
		handle, events, err = prov.StartSession(runCtx, spec)
	}
	if err != nil {
		cancel()
		o.releaseSessionLocks(sess)
		o.releaseTargetSlot(sess)
		// Record WHY before flipping to failed. A start failure (e.g. the remote
		// `tmux new-session` erroring on a flaky SSH target) otherwise leaves a
		// session with zero transcript and no DB reason — an undiagnosable black
		// box. This is exactly what made repeated manager-launch failures on a
		// degraded host impossible to explain after the fact.
		_ = o.emit(sessionID, model.MsgSystem, model.KindError, "failed to start session: "+err.Error(), nil)
		_, _ = o.st.UpdateSessionStatus(sessionID, model.SessionFailed)
		return nil, err
	}

	r := &run{
		sessionID:   sessionID,
		provider:    prov,
		handle:      handle,
		ctx:         runCtx,
		cancel:      cancel,
		interactive: handle.Interactive(),
		done:        make(chan struct{}),
		spec:        spec,
	}
	o.mu.Lock()
	o.runs[sessionID] = r
	o.mu.Unlock()

	if _, err := o.st.UpdateSessionStatus(sessionID, model.SessionRunning); err != nil {
		// Lost a race with cancellation; tear down.
		cancel()
	}

	go o.consume(r, events)
	return &Run{r: r}, nil
}

// maxInlinePromptBytes is the largest opening prompt passed inline as a positional
// argument. Above it the prompt is written to a file and replaced by a short
// bootstrap that points the agent at it.
//
// The binding limit is NOT the kernel's ~128KB MAX_ARG_STRLEN but tmux's own
// command-length cap, which is far lower: a remote `tmux new-session` was measured
// (on the prod SSH box) to start failing with "command too long" between 12KB and
// 16KB of total command — and a ~17KB reviewer prompt did exactly that in prod,
// because 32KB let it through inline. 8KB keeps the assembled command (agent flags
// + the --mcp-config JSON + worst-case shell-escaping inflation + the prompt) well
// under tmux's cap; anything larger is externalized to a file instead.
const maxInlinePromptBytes = 8 * 1024

// externalizeLargePrompt moves an oversized opening prompt off the command line.
// The interactive providers pass the first prompt as a positional argv argument,
// which is bounded (~128KB per arg on Linux, less over SSH after escaping) and
// fragile for big inputs. When the prompt is large, write it to a file on the
// target — delivered over a pipe (stdin), which has no such limit — and hand the
// agent a short bootstrap that points at it. Small prompts are left inline,
// exactly as before. A no-op without a real target/preparer (offline/test runs),
// and it falls back to the inline prompt if the write fails, so a session never
// launches with no task at all.
func (o *Orchestrator) externalizeLargePrompt(ctx context.Context, sess *model.Session, tgt *model.Target, spec agent.Spec) agent.Spec {
	if tgt == nil || o.preparer == nil || len(spec.Prompt) <= maxInlinePromptBytes {
		return spec
	}
	abs := promptFilePath(sess.ID)
	ex := agent.NewExecutor(tgt)
	if err := writeWorkspaceFile(ctx, ex, tgt.WorkRoot, abs, spec.Prompt); err != nil {
		o.audit(sess.ObjectiveID, sess.ID, "prompt_externalize_failed", err.Error(), nil)
		return spec
	}
	o.audit(sess.ObjectiveID, sess.ID, "prompt_externalized",
		fmt.Sprintf("opening prompt (%d bytes) written to %s", len(spec.Prompt), abs),
		model.JSONMap{"path": abs})
	spec.Prompt = promptBootstrap(abs)
	return spec
}

// promptFilePath is the per-session file an externalized opening prompt is written
// to on the target. /tmp is used (not the checkout) so it never risks landing in a
// PR; the provider already writes its pane log there.
func promptFilePath(sessionID string) string {
	return "/tmp/orcha-prompt-" + sessionID + ".md"
}

// promptBootstrap is the short inline prompt that replaces an externalized one: it
// tells the agent where its real instructions are and to read them first.
func promptBootstrap(absPath string) string {
	return "Your full instructions for this session were too large to pass inline, so they have been written to a file on this machine:\n\n" +
		absPath + "\n\nRead that entire file now (run `cat " + absPath + "`) before doing anything else — it contains your complete task. Then carry it out."
}

// consume drains a provider's event stream into the transcript and drives
// terminal state. It is the only place raw stdout/stderr is persisted (into the
// transcript, never an artifact).
func (o *Orchestrator) consume(r *run, events <-chan agent.Event) {
	defer close(r.done)
	defer o.cleanupRun(r)

	for ev := range events {
		switch ev.Kind {
		case agent.EventText, agent.EventToolCall, agent.EventToolResult:
			_ = o.emit(r.sessionID, ev.Source, msgKind(ev.Kind), ev.Content, ev.Metadata)
			o.RecordProgress(r.sessionID)
			// Keep the latest agent prose as the session's summary — it is what a
			// parent manager is handed when this session finishes. Without this
			// LatestSummary is never written and the handoff says nothing useful.
			if ev.Kind == agent.EventText && ev.Source == model.MsgAgent && strings.TrimSpace(ev.Content) != "" {
				// Keep the TAIL, rune-safe: this is a fallback for when a worker did
				// not call report_result, and the meaningful conclusion sits at the
				// end of the message (or the bottom of a scraped TUI pane). Byte
				// slicing here previously split multibyte box-drawing chars and, worse,
				// kept the noisy build-log preamble while dropping the findings.
				summary := tailRunes(ev.Content, 2000)
				_, _ = o.st.UpdateSessionRuntime(r.sessionID, func(s *model.Session) {
					s.LatestSummary = summary
				})
			}
		case agent.EventStdout:
			_ = o.emit(r.sessionID, model.MsgStdout, model.KindText, ev.Content, ev.Metadata)
		case agent.EventStderr:
			_ = o.emit(r.sessionID, model.MsgStderr, model.KindText, ev.Content, ev.Metadata)
		case agent.EventProgress:
			// Live progress scraped from an interactive TUI pane: a settled output
			// line and/or the current activity. Record it as forward progress (so a
			// busy worker is known-alive and the no-progress guard isn't tripped),
			// stream the line into the transcript for a live view, and reflect the
			// activity — but do NOT touch LatestSummary (that is the agent's own
			// final message, set on EventText).
			if strings.TrimSpace(ev.Content) != "" {
				_ = o.emit(r.sessionID, ev.Source, model.KindText, ev.Content, ev.Metadata)
				o.RecordProgress(r.sessionID)
			}
			if ev.Activity != "" {
				o.RecordProgress(r.sessionID)
				_, _ = o.st.UpdateSessionRuntime(r.sessionID, func(s *model.Session) {
					s.CurrentActivity = ev.Activity
				})
			}
		case agent.EventStatus:
			if ev.Activity != "" {
				_, _ = o.st.UpdateSessionRuntime(r.sessionID, func(s *model.Session) {
					s.CurrentActivity = ev.Activity
				})
			}
		case agent.EventUsage:
			o.recordUsage(r.sessionID, ev.UsedTokens)
		case agent.EventError:
			_ = o.emit(r.sessionID, model.MsgSystem, model.KindError, ev.Content, ev.Metadata)
			if trip := o.CheckError(r.sessionID, ev.Content); trip != nil {
				_ = o.PauseSession(r.sessionID, trip.Error())
				r.cancel()
				return
			}
		case agent.EventDone:
			// A failure done that arrives after the run's context was canceled is
			// not the agent failing: it is the orchestrator shutting down (leave
			// the session running so restart recovery resumes it) or an explicit
			// cancel (which already set the terminal status). Marking it failed
			// here buried live sessions in a terminal state on a plain restart.
			if !ev.Success && r.ctx.Err() != nil {
				return
			}
			o.finishRun(r, ev.Success)
			return
		}
		if ev.Activity != "" && ev.Kind != agent.EventStatus {
			_, _ = o.st.UpdateSessionRuntime(r.sessionID, func(s *model.Session) {
				s.CurrentActivity = ev.Activity
			})
		}
		// Capture durable provider-side handles into session metadata: a Codex
		// thread id (so resume preserves the conversation) and the tmux session /
		// attach command (so the UI can show "attach with ...").
		if ev.Metadata != nil {
			o.persistSessionMeta(r.sessionID, ev.Metadata,
				"provider_session_id", "tmux_session", "tmux_attach")
		}
	}
	// Stream ended without an explicit done event (e.g. canceled). Finalization
	// happens in cleanupRun; do not force a terminal transition here so a
	// canceled status is preserved.
}

// finishRun applies the terminal status from a done event. If the session was
// already canceled, the transition is rejected and recorded as an ignored late
// completion — canceled work can never be resurrected.
func (o *Orchestrator) finishRun(r *run, success bool) {
	child, _ := o.st.GetSession(r.sessionID)
	// A CI follow-up that "succeeded" but never advanced its PR's head did not land
	// a fix — the PR is still red. The success flag comes from the agent printing a
	// done marker, which it does even when it gave up or its push failed (observed:
	// a follow-up reported "Blocked... could not push" yet was marked succeeded). So
	// for a CI follow-up, require proof it pushed (the PR head moved); otherwise
	// record a failure, which routes it to the manager below. PR follow-ups may
	// legitimately just comment, so they are exempt.
	if success && child != nil && child.Role == model.RoleCIFollowup && !o.followupAdvancedPR(child) {
		_ = o.emit(r.sessionID, model.MsgSystem, model.KindStatus,
			"CI follow-up finished without advancing the PR (no fix pushed) — recording as failed", nil)
		success = false
	}
	next := model.SessionSucceeded
	if !success {
		next = model.SessionFailed
	}
	defer func() { _ = o.st.CancelOpenQuestionsBySession(r.sessionID) }()
	if _, err := o.st.UpdateSessionStatus(r.sessionID, next); err != nil {
		// Terminal already (likely canceled): record and ignore.
		_ = o.emit(r.sessionID, model.MsgSystem, model.KindStatus,
			"ignored late completion ("+string(next)+"): "+err.Error(), nil)
		objID := ""
		if child != nil {
			objID = child.ObjectiveID
		}
		o.audit(objID, r.sessionID, "late_completion_ignored", string(next), nil)
		return
	}
	_ = o.emit(r.sessionID, model.MsgSystem, model.KindStatus, "session "+string(next), nil)
	// Fold any edits the agent made to .orcha/MEMORY.md back into repo-wide
	// memory before the manager re-engages, so it sees the latest learnings. Only
	// on success — a failed run's notes are unreliable.
	if success && child != nil {
		o.mergeBackRepoMemory(child)
	}
	// PR/CI follow-ups are spawned parentless (notifyManagerOfChild ignores them);
	// route their outcome to the objective's manager explicitly, so a failed
	// follow-up reaches the supervisor instead of stranding a still-red PR.
	if child != nil && (child.Role == model.RoleCIFollowup || child.Role == model.RolePRFollowup) {
		o.notifyManagerOfFollowup(child, success)
		return
	}
	o.notifyManagerOfChild(r.sessionID, success)
}

// notifyManagerOfChild re-prompts the objective's manager when a worker it
// spawned finishes, so the manager can review, publish a PR, spawn follow-on
// work, or mark the objective done. This closes the worker -> manager handoff
// loop that makes the team self-driving.
func (o *Orchestrator) notifyManagerOfChild(childID string, success bool) {
	child, err := o.st.GetSession(childID)
	if err != nil || child.Role == model.RoleManager || child.ParentSessionID == "" {
		return
	}
	mgr, err := o.st.GetSession(child.ParentSessionID)
	if err != nil || mgr.Role != model.RoleManager || mgr.Status.IsTerminal() {
		return
	}
	// A reviewer's outcome is delivered by submit_review (approve auto-opens the
	// PR, request_changes hands the findings to the manager). The generic
	// "succeeded — publish it" message below would be wrong for a reviewer, so
	// route it through the review-specific notifier instead.
	if child.Role == model.RoleReviewer {
		o.notifyManagerOfReview(child, mgr, success)
		return
	}
	summary := relaySummary(child)
	var msg string
	switch {
	case !success:
		msg = fmt.Sprintf(
			"Worker session %s (%q) FAILED. Summary: %s\n"+
				"Do NOT publish. Decide how to proceed: inspect the failure, re-scope and "+
				"re-spawn the work, or ask_user if you are blocked.",
			child.ID, child.Title, summary)
	case o.hasPendingDependents(child.ObjectiveID, child.ID):
		// Another worker depends on this one (e.g. a validator/reviewer continuing
		// its branch). The slice is not finished — publishing now would ship
		// unvalidated work. Tell the manager to wait, not publish.
		msg = fmt.Sprintf(
			"Worker session %s (%q) succeeded. Summary: %s\n"+
				"Dependent work (e.g. validation/review) is still running on top of this slice — "+
				"do NOT publish it yet. Wait; you are notified when the dependents finish.",
			child.ID, child.Title, summary)
	default:
		msg = fmt.Sprintf(
			"Worker session %s (%q) succeeded. Summary: %s\n"+
				"Its checkout holds the changes and any dependent work is done. If they look right, "+
				"publish a PR with publish_pr(session_id=%q, title=..., body=...). Then spawn any "+
				"follow-on work or mark_objective_done when the objective is complete.",
			child.ID, child.Title, summary, child.ID)
	}
	outcome := "succeeded"
	if !success {
		outcome = "failed"
	}
	o.audit(child.ObjectiveID, mgr.ID, "manager_notified", "worker "+outcome, model.JSONMap{"child": child.ID})
	_ = o.Steer(context.Background(), mgr.ID, msg)
}

// CompletionAllowed reports whether a tmux session may be treated as finished
// when its pane goes idle. It is false while the session has an open question —
// the agent called ask_user and is waiting on the answer, not done. Wired into
// the tmux providers' CompletionGate so a waiting worker isn't killed (which
// would drop the user's pending answer).
func (o *Orchestrator) CompletionAllowed(sessionID string) bool {
	return !o.st.HasOpenQuestionBySession(sessionID)
}

// hasPendingDependents reports whether any non-terminal session for the
// objective depends on sessionID — i.e. work that continues this one's branch is
// still queued or running, so its slice is not finished.
func (o *Orchestrator) hasPendingDependents(objectiveID, sessionID string) bool {
	sessions, err := o.st.ListSessionsByObjective(objectiveID)
	if err != nil {
		return false
	}
	for _, s := range sessions {
		if s.Status.IsTerminal() {
			continue
		}
		for _, dep := range dependencyIDs(s) {
			if dep == sessionID {
				return true
			}
		}
	}
	return false
}

// cleanupRun releases locks always and the target slot exactly once per run —
// but only when the session has reached a terminal state. A paused
// (waiting_user) or steering-resumed session keeps its slot, since it still
// occupies the machine and will resume in place.
func (o *Orchestrator) cleanupRun(r *run) {
	r.finalize.Do(func() {
		o.mu.Lock()
		delete(o.runs, r.sessionID)
		o.mu.Unlock()
		sess, err := o.st.GetSession(r.sessionID)
		if err != nil {
			return
		}
		o.releaseSessionLocks(sess)
		if sess.Status.IsTerminal() {
			o.releaseTargetSlot(sess)
			o.notifyChange() // freed a slot / unblocked dependents
		}
	})
}

func (o *Orchestrator) releaseSessionLocks(sess *model.Session) {
	_ = o.st.ReleaseLocksHeldBy(sess.ID)
}

// Cancel terminates a session: it kills the process group via the provider,
// cancels the run context, marks the session canceled, releases locks/slot, and
// (per policy) cancels child sessions. A late completion arriving afterward is
// ignored by the state machine.
func (o *Orchestrator) Cancel(sessionID string, cancelChildren bool) error {
	sess, err := o.st.GetSession(sessionID)
	if err != nil {
		return err
	}
	if sess.Status.IsTerminal() {
		return nil // already done; nothing to cancel
	}

	o.mu.Lock()
	r := o.runs[sessionID]
	o.mu.Unlock()

	if _, err := o.st.UpdateSessionStatus(sessionID, model.SessionCanceled); err != nil {
		return err
	}
	if r != nil {
		_ = r.provider.CancelSession(r.handle) // kill process group
		r.cancel()
		// The run's cleanupRun (finalize.Once) will release locks and the slot
		// now that the status is terminal — avoiding a double slot release.
	} else {
		// No live run; release directly since no cleanup will fire.
		o.releaseSessionLocks(sess)
		o.releaseTargetSlot(sess)
		o.notifyChange()
	}
	_ = o.st.CancelOpenQuestionsBySession(sessionID) // nobody can act on answers now
	_ = o.emit(sessionID, model.MsgSystem, model.KindStatus, "session canceled", nil)
	o.audit(sess.ObjectiveID, sessionID, "session_canceled", "canceled by request", nil)

	if cancelChildren {
		children, _ := o.st.ListChildSessions(sessionID)
		for _, c := range children {
			if !c.Status.IsTerminal() {
				_ = o.Cancel(c.ID, true)
			}
		}
	}
	return nil
}

// persistSessionMeta merges the named string keys from an event's metadata into
// the session metadata, skipping no-ops. Used to durably record provider-side
// handles (Codex thread id, tmux session/attach command).
func (o *Orchestrator) persistSessionMeta(sessionID string, evMeta model.JSONMap, keys ...string) {
	updates := map[string]string{}
	for _, k := range keys {
		if v, ok := evMeta[k].(string); ok && v != "" {
			updates[k] = v
		}
	}
	if len(updates) == 0 {
		return
	}
	sess, err := o.st.GetSession(sessionID)
	if err != nil {
		return
	}
	changed := false
	for k, v := range updates {
		if existing, _ := sess.Metadata[k].(string); existing != v {
			changed = true
		}
	}
	if !changed {
		return
	}
	_, _ = o.st.UpdateSessionRuntime(sessionID, func(s *model.Session) {
		if s.Metadata == nil {
			s.Metadata = model.JSONMap{}
		}
		for k, v := range updates {
			s.Metadata[k] = v
		}
	})
}

// SessionScreen returns the current visible terminal screen for a live session
// when its provider supports snapshots (e.g. tmux capture-pane). ok is false
// when the session is not running or the provider has no screen.
func (o *Orchestrator) SessionScreen(sessionID string) (screen agent.Screen, ok bool, err error) {
	o.mu.Lock()
	r := o.runs[sessionID]
	o.mu.Unlock()
	if r == nil {
		return agent.Screen{}, false, nil
	}
	snap, isSnap := r.provider.(agent.Snapshotter)
	if !isSnap {
		return agent.Screen{}, false, nil
	}
	s, err := snap.Snapshot(r.handle)
	if err != nil {
		return agent.Screen{}, true, err
	}
	return s, true, nil
}

// SessionAttach opens a live, interactive pty attached to a running session so
// the UI can drive it like a real terminal. ok is false when the session is not
// running or its provider has no attach capability. The caller owns the returned
// process and must Close it when the viewer disconnects.
func (o *Orchestrator) SessionAttach(sessionID string, cols, rows uint16) (exec.PTYProcess, bool, error) {
	o.mu.Lock()
	r := o.runs[sessionID]
	o.mu.Unlock()
	if r == nil {
		return nil, false, nil
	}
	att, isAtt := r.provider.(agent.Attacher)
	if !isAtt {
		return nil, false, nil
	}
	p, err := att.AttachPTY(r.handle, cols, rows)
	if err != nil {
		return nil, true, err
	}
	return p, true, nil
}

func msgKind(k agent.EventKind) model.MessageKind {
	switch k {
	case agent.EventToolCall:
		return model.KindToolCall
	case agent.EventToolResult:
		return model.KindToolResult
	default:
		return model.KindText
	}
}

// RecoverInterrupted requeues sessions orphaned by a previous process — rows
// still marked starting/running at boot, when no in-memory runs exist. Call it
// once at startup, before the scheduler runs. Each requeued session restarts
// through the normal scheduling path; if a provider conversation id was
// captured during the prior run, StartRun resumes that conversation instead of
// starting cold.
func (o *Orchestrator) RecoverInterrupted() int {
	// Heal questions stranded by earlier versions (or crashes): open ones whose
	// asking session or objective is already terminal can never be acted on.
	if n, err := o.st.SweepStaleQuestions(); err == nil && n > 0 {
		o.audit("", "", "questions_swept", fmt.Sprintf("closed %d stale open question(s)", n), nil)
	}
	// Heal duplicate PR rows a prior adoption race may have recorded.
	if n, err := o.st.DeduplicatePRs(); err == nil && n > 0 {
		o.audit("", "", "prs_deduped", fmt.Sprintf("removed %d duplicate PR row(s)", n), nil)
	}
	sessions, err := o.st.RequeueInterruptedSessions()
	if err != nil || len(sessions) == 0 {
		return 0
	}
	for _, sess := range sessions {
		_ = o.emit(sess.ID, model.MsgSystem, model.KindStatus,
			"orchestrator restarted; session requeued", nil)
		o.audit(sess.ObjectiveID, sess.ID, "session_recovered",
			"requeued after orchestrator restart", nil)
	}
	o.notifyChange()
	return len(sessions)
}
