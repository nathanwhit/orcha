package orch

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nathanwhit/orcha/internal/agent"
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

	// Acquire workspace lock (single writer per workspace).
	if sess.WorkspaceID != "" {
		if err := o.st.AcquireLock(workspaceLockKey(sess.WorkspaceID), model.LockWorkspace, sessionID, "session run"); err != nil {
			if errors.Is(err, store.ErrLockHeld) {
				_, _ = o.st.UpdateSessionStatus(sessionID, model.SessionWaitingCapacity)
			}
			return nil, err
		}
	}

	// Place on a target if not already placed.
	if sess.TargetID == "" {
		if _, err := o.PlaceSession(sessionID, TargetRequest{}); err != nil {
			o.releaseSessionLocks(sess)
			_, _ = o.st.UpdateSessionStatus(sessionID, model.SessionWaitingCapacity)
			return nil, err
		}
		sess, _ = o.st.GetSession(sessionID)
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
	var tgt *model.Target
	if sess.TargetID != "" {
		tgt, _ = o.st.GetTarget(sess.TargetID)
	}

	spec := o.buildSpec(sess, ws, tgt)

	runCtx, cancel := context.WithCancel(ctx)
	handle, events, err := prov.StartSession(runCtx, spec)
	if err != nil {
		cancel()
		o.releaseSessionLocks(sess)
		o.releaseTargetSlot(sess)
		_, _ = o.st.UpdateSessionStatus(sessionID, model.SessionFailed)
		return nil, err
	}

	r := &run{
		sessionID:   sessionID,
		provider:    prov,
		handle:      handle,
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
		case agent.EventStdout:
			_ = o.emit(r.sessionID, model.MsgStdout, model.KindText, ev.Content, ev.Metadata)
		case agent.EventStderr:
			_ = o.emit(r.sessionID, model.MsgStderr, model.KindText, ev.Content, ev.Metadata)
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
	next := model.SessionSucceeded
	if !success {
		next = model.SessionFailed
	}
	if _, err := o.st.UpdateSessionStatus(r.sessionID, next); err != nil {
		// Terminal already (likely canceled): record and ignore.
		_ = o.emit(r.sessionID, model.MsgSystem, model.KindStatus,
			"ignored late completion ("+string(next)+"): "+err.Error(), nil)
		sess, _ := o.st.GetSession(r.sessionID)
		objID := ""
		if sess != nil {
			objID = sess.ObjectiveID
		}
		o.audit(objID, r.sessionID, "late_completion_ignored", string(next), nil)
		return
	}
	_ = o.emit(r.sessionID, model.MsgSystem, model.KindStatus, "session "+string(next), nil)
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
