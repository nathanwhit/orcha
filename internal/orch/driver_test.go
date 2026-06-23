package orch

import (
	"context"
	"testing"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/store"
)

// blockingFake is a provider whose sessions run until canceled, so the
// scheduler's "running" accounting is observable and deterministic.
func blockingFake(kind model.AgentKind) *agent.FakeProvider {
	return agent.NewFake(kind, true, func(ctx context.Context, spec agent.Spec, in <-chan string, out chan<- agent.Event) {
		out <- agent.Event{Kind: agent.EventText, Source: model.MsgAgent, Content: "running"}
		<-ctx.Done()
	})
}

func queuedSession(t *testing.T, st *store.Store, objID string, deps ...string) *model.Session {
	t.Helper()
	s := &model.Session{ObjectiveID: objID, Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionQueued}
	if len(deps) > 0 {
		anyDeps := make([]any, len(deps))
		for i, d := range deps {
			anyDeps[i] = d
		}
		s.Metadata = model.JSONMap{"depends_on": anyDeps}
	}
	if err := st.CreateSession(s); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s
}

func TestScheduler_StartsQueuedSession(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(blockingFake(model.AgentClaude))
	sch := NewScheduler(o, time.Second, 8)

	s := queuedSession(t, st, "")
	started, err := sch.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if started != 1 {
		t.Fatalf("started=%d, want 1", started)
	}
	// StartRun runs in the background (its checkout must not block scheduling), so
	// the session reaches running shortly after the tick.
	waitFor(t, func() bool { rs, _ := st.GetSession(s.ID); return rs.Status == model.SessionRunning })
	_ = o.Cancel(s.ID, false)
}

func TestScheduler_RespectsMaxConcurrent(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 8)
	o.RegisterProvider(blockingFake(model.AgentClaude))
	sch := NewScheduler(o, time.Second, 1) // global cap of 1

	a := queuedSession(t, st, "")
	b := queuedSession(t, st, "")
	started, _ := sch.Tick(context.Background())
	if started != 1 {
		t.Fatalf("started=%d, want 1 (global cap)", started)
	}
	// One reaches running; the other was never attempted (budget gate), so it
	// stays queued.
	waitFor(t, func() bool { active, _ := st.CountSessionsByStatuses(model.SessionRunning); return active == 1 })
	bs, _ := st.GetSession(b.ID)
	if bs.Status != model.SessionQueued {
		t.Fatalf("second session status=%s, want queued", bs.Status)
	}
	_ = o.Cancel(a.ID, false)

	// After the first finishes, a later tick starts the second.
	waitFor(t, func() bool { s, _ := st.GetSession(a.ID); return s.Status.IsTerminal() })
	started, _ = sch.Tick(context.Background())
	if started != 1 {
		t.Fatalf("second pass started=%d, want 1", started)
	}
	_ = o.Cancel(b.ID, false)
}

// Managers must not be gated by the worker concurrency cap: they are long-lived
// supervisors that sit idle most of their life, and gating them is what made a
// new manager queue for minutes behind running workers on an idle fleet. Workers
// still respect the cap.
func TestScheduler_ManagersExemptFromWorkerCap(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 8)
	o.RegisterProvider(blockingFake(model.AgentClaude))
	sch := NewScheduler(o, time.Second, 1) // worker cap of 1

	newManager := func() *model.Session {
		m := &model.Session{Role: model.RoleManager, Agent: model.AgentClaude,
			Mode: model.ModeInteractive, Status: model.SessionQueued}
		if err := st.CreateSession(m); err != nil {
			t.Fatalf("create manager: %v", err)
		}
		return m
	}
	m1, m2 := newManager(), newManager()
	w := queuedSession(t, st, "") // one worker

	// Worker cap is 1, yet both managers AND the one worker start this tick.
	started, _ := sch.Tick(context.Background())
	if started != 3 {
		t.Fatalf("started=%d, want 3 (1 worker + 2 managers, managers exempt)", started)
	}
	for _, s := range []*model.Session{m1, m2, w} {
		s := s
		waitFor(t, func() bool { got, _ := st.GetSession(s.ID); return got.Status == model.SessionRunning })
	}

	// The worker cap is still enforced: a second worker waits behind the one
	// already running (managers don't count toward the budget).
	w2 := queuedSession(t, st, "")
	if started, _ := sch.Tick(context.Background()); started != 0 {
		t.Fatalf("second worker should wait on the worker cap, started=%d", started)
	}
	if got, _ := st.GetSession(w2.ID); got.Status != model.SessionQueued {
		t.Fatalf("second worker status=%s, want queued", got.Status)
	}
	for _, s := range []*model.Session{m1, m2, w, w2} {
		_ = o.Cancel(s.ID, false)
	}
}

func TestScheduler_RespectsTargetCapacity(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 1) // single slot
	o.RegisterProvider(blockingFake(model.AgentClaude))
	sch := NewScheduler(o, time.Second, 8)

	a := queuedSession(t, st, "")
	b := queuedSession(t, st, "")
	if _, err := sch.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The target has a single slot. Both pass the worker-cap gate and launch, but
	// the slot claim is atomic in the store, so exactly one wins and runs while the
	// other is parked waiting_capacity. Which one wins is a goroutine race, so
	// assert the aggregate.
	waitFor(t, func() bool {
		running, _ := st.CountSessionsByStatuses(model.SessionRunning)
		waiting, _ := st.CountSessionsByStatuses(model.SessionWaitingCapacity)
		return running == 1 && waiting == 1
	})
	_ = o.Cancel(a.ID, false)
	_ = o.Cancel(b.ID, false)
}

func TestScheduler_DependencyGating(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 8)
	o.RegisterProvider(blockingFake(model.AgentClaude))
	sch := NewScheduler(o, time.Second, 8)

	// A is still in flight; B depends on A and must not start.
	a := queuedSession(t, st, "")
	b := queuedSession(t, st, "", a.ID)
	if _, err := sch.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if bs, _ := st.GetSession(b.ID); bs.Status != model.SessionQueued {
		t.Fatalf("B should remain queued while A runs, got %s", bs.Status)
	}

	// Mark A succeeded, then B becomes eligible.
	_ = o.Cancel(a.ID, false)
	// Preset a succeeded dependency to make the readiness deterministic.
	done := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionSucceeded}
	_ = st.CreateSession(done)
	c := queuedSession(t, st, "", done.ID)
	started, _ := sch.Tick(context.Background())
	if started < 1 {
		t.Fatalf("expected dependent C to start once its dep succeeded, started=%d", started)
	}
	waitFor(t, func() bool { cs, _ := st.GetSession(c.ID); return cs.Status == model.SessionRunning })
	_ = o.Cancel(c.ID, false)
}

func TestScheduler_DependencyFailureCancelsDependent(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 8)
	o.RegisterProvider(blockingFake(model.AgentClaude))
	sch := NewScheduler(o, time.Second, 8)

	failed := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionFailed}
	_ = st.CreateSession(failed)
	b := queuedSession(t, st, "", failed.ID)

	if _, err := sch.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	bs, _ := st.GetSession(b.ID)
	if bs.Status != model.SessionCanceled {
		t.Fatalf("dependent of a failed session should be canceled, got %s", bs.Status)
	}
}

func TestScheduler_SkipsWaitingUser(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 8)
	o.RegisterProvider(blockingFake(model.AgentClaude))
	sch := NewScheduler(o, time.Second, 8)

	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionWaitingUser}
	_ = st.CreateSession(s)
	started, _ := sch.Tick(context.Background())
	if started != 0 {
		t.Fatalf("waiting_user sessions must not auto-start, started=%d", started)
	}
	if rs, _ := st.GetSession(s.ID); rs.Status != model.SessionWaitingUser {
		t.Fatalf("status changed to %s", rs.Status)
	}
}

func TestScheduler_ProviderExhaustedParksSession(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.ProviderFallback = nil
	addTarget(t, st, "local", model.TargetLocal, 8)
	o.RegisterProvider(blockingFake(model.AgentClaude))
	_ = st.UpsertUsage(&model.UsageBucket{Provider: string(model.AgentClaude), State: model.UsageExhausted, WindowStart: st.Now(), WindowEnd: st.Now()})
	sch := NewScheduler(o, time.Second, 8)

	s := queuedSession(t, st, "")
	if _, err := sch.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The start is attempted in the background, finds every provider exhausted, asks
	// the user, and parks the session waiting_user so the loop doesn't respin.
	waitFor(t, func() bool { rs, _ := st.GetSession(s.ID); return rs.Status == model.SessionWaitingUser })
	qs, _ := st.ListOpenQuestions()
	if len(qs) == 0 {
		t.Fatal("exhaustion should open a question for the user")
	}
}

// The Run loop starts queued work and stops cleanly on context cancel.
func TestScheduler_RunLoopSelfDrives(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 8)
	o.RegisterProvider(blockingFake(model.AgentClaude))
	sch := NewScheduler(o, 20*time.Millisecond, 8)

	ctx, cancel := context.WithCancel(context.Background())
	go sch.Run(ctx)

	s := queuedSession(t, st, "")
	sch.Wake()
	waitFor(t, func() bool { rs, _ := st.GetSession(s.ID); return rs.Status == model.SessionRunning })

	cancel()
	_ = o.Cancel(s.ID, false)
}

// A restart leaves sessions marked starting/running with no live run; recovery
// requeues them and the scheduler restarts them — resuming the provider
// conversation when one was captured.
func TestRecoverInterruptedRequeuesAndRestarts(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))

	obj := &model.Objective{Title: "obj", Prompt: "p", Status: model.ObjectiveActive}
	if err := st.CreateObjective(obj); err != nil {
		t.Fatalf("objective: %v", err)
	}
	// Simulate a session orphaned by a dead process: marked running, no run,
	// with a provider conversation id captured during the prior run.
	s := &model.Session{
		ObjectiveID: obj.ID, Role: model.RoleImplementer, Agent: model.AgentClaude,
		Status: model.SessionQueued, Goal: "do the thing",
		Metadata: model.JSONMap{"provider_session_id": "prior-conversation"},
	}
	if err := st.CreateSession(s); err != nil {
		t.Fatalf("session: %v", err)
	}
	if _, err := st.UpdateSessionStatus(s.ID, model.SessionStarting); err != nil {
		t.Fatalf("starting: %v", err)
	}
	if _, err := st.UpdateSessionStatus(s.ID, model.SessionRunning); err != nil {
		t.Fatalf("running: %v", err)
	}

	if n := o.RecoverInterrupted(); n != 1 {
		t.Fatalf("recovered %d sessions, want 1", n)
	}
	got, _ := st.GetSession(s.ID)
	if got.Status != model.SessionQueued {
		t.Fatalf("status after recovery = %s, want queued", got.Status)
	}

	// The scheduler now restarts it like any queued session.
	sched := NewScheduler(o, time.Second, 4)
	started, err := sched.Tick(context.Background())
	if err != nil || started != 1 {
		t.Fatalf("tick started=%d err=%v, want 1 started", started, err)
	}
	waitFor(t, func() bool {
		fresh, _ := st.GetSession(s.ID)
		return fresh.Status == model.SessionRunning || fresh.Status.IsTerminal()
	})
}

// A session can be stranded "running" mid-life when its watcher dies without
// emitting a terminal event (a missed tmux exit, a re-adopt-after-restart that
// later loses the session). Startup recovery never sees it (the process did not
// restart), so the periodic reconciler must requeue it — while leaving genuinely
// live runs and in-flight starts untouched.
func TestReconcileOrphans_RequeuesStrandedRunningSession(t *testing.T) {
	defer func(g time.Duration) { orphanGrace = g }(orphanGrace)
	orphanGrace = 0 // treat any unbacked running row as an orphan regardless of age

	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(blockingFake(model.AgentClaude))
	sch := NewScheduler(o, time.Second, 8)

	obj := &model.Objective{Title: "obj", Prompt: "p", Status: model.ObjectiveActive}
	if err := st.CreateObjective(obj); err != nil {
		t.Fatalf("objective: %v", err)
	}

	// (1) A genuinely live session: StartRun registers a run in o.runs whose
	// watcher drives it. The reconciler must leave it alone.
	live := queuedSession(t, st, obj.ID)
	if _, err := o.StartRun(context.Background(), live.ID); err != nil {
		t.Fatalf("start live: %v", err)
	}
	waitFor(t, func() bool { s, _ := st.GetSession(live.ID); return s.Status == model.SessionRunning })

	// (2) The orphan: marked running, but no run was ever registered for it.
	orphan := queuedSession(t, st, obj.ID)
	mustRun(t, st, orphan.ID)

	// (3) A start in flight: a running row covered by the in-flight guard (its
	// StartRun's run is still wiring up). Must also be left alone.
	inflight := queuedSession(t, st, obj.ID)
	mustRun(t, st, inflight.ID)
	if !sch.beginStart(inflight.ID, true) {
		t.Fatal("beginStart should mark the session in flight")
	}

	sch.ReconcileOrphans(context.Background())

	if s, _ := st.GetSession(orphan.ID); s.Status != model.SessionQueued {
		t.Fatalf("orphan status = %s, want queued (requeued)", s.Status)
	}
	if s, _ := st.GetSession(live.ID); s.Status != model.SessionRunning {
		t.Fatalf("live status = %s, want running (untouched)", s.Status)
	}
	if s, _ := st.GetSession(inflight.ID); s.Status != model.SessionRunning {
		t.Fatalf("in-flight status = %s, want running (untouched)", s.Status)
	}

	_ = o.Cancel(live.ID, false)
}

func mustRun(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.UpdateSessionStatus(id, model.SessionStarting); err != nil {
		t.Fatalf("starting: %v", err)
	}
	if _, err := st.UpdateSessionStatus(id, model.SessionRunning); err != nil {
		t.Fatalf("running: %v", err)
	}
}

// An orchestrator shutdown must not bury live sessions: the provider's
// failure done event (emitted when the run context is canceled) used to mark
// them failed — terminal — so restart recovery never resumed them.
func TestShutdownLeavesRunningSessionsRecoverable(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	// A provider that models the tmux watcher: it emits a failure done when
	// its context is canceled (e.g. the process is shutting down).
	script := func(ctx context.Context, spec agent.Spec, in <-chan string, out chan<- agent.Event) {
		select {
		case out <- agent.Event{Kind: agent.EventStatus, Activity: "running"}:
		case <-ctx.Done():
		}
		<-ctx.Done()
		out <- agent.Event{Kind: agent.EventDone, Success: false, Content: "tmux session canceled"}
	}
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, script))

	s := &model.Session{Role: model.RoleManager, Agent: model.AgentClaude, Status: model.SessionQueued, Goal: "g"}
	if err := st.CreateSession(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run, err := o.StartRun(ctx, s.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, func() bool {
		fresh, _ := st.GetSession(s.ID)
		return fresh.Status == model.SessionRunning
	})

	cancel() // the shutdown
	run.Wait()

	fresh, _ := st.GetSession(s.ID)
	if fresh.Status != model.SessionRunning {
		t.Fatalf("status after shutdown = %s, want running (recoverable)", fresh.Status)
	}
	// And recovery picks it up.
	if n := o.RecoverInterrupted(); n != 1 {
		t.Fatalf("recovered %d, want 1", n)
	}

	// An EXPLICIT cancel still lands on canceled, not running.
	s2 := &model.Session{Role: model.RoleManager, Agent: model.AgentClaude, Status: model.SessionQueued, Goal: "g"}
	_ = st.CreateSession(s2)
	if _, err := o.StartRun(context.Background(), s2.ID); err != nil {
		t.Fatalf("start2: %v", err)
	}
	waitFor(t, func() bool {
		fresh, _ := st.GetSession(s2.ID)
		return fresh.Status == model.SessionRunning
	})
	if err := o.Cancel(s2.ID, false); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitFor(t, func() bool {
		fresh, _ := st.GetSession(s2.ID)
		return fresh.Status == model.SessionCanceled
	})
}
