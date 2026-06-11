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
	reloaded, _ := st.GetSession(s.ID)
	if reloaded.Status != model.SessionRunning {
		t.Fatalf("session status=%s, want running", reloaded.Status)
	}
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
	// One running, the other still queued (not even attempted).
	active, _ := st.CountSessionsByStatuses(model.SessionRunning)
	if active != 1 {
		t.Fatalf("active=%d, want 1", active)
	}
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

func TestScheduler_RespectsTargetCapacity(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 1) // single slot
	o.RegisterProvider(blockingFake(model.AgentClaude))
	sch := NewScheduler(o, time.Second, 8)

	a := queuedSession(t, st, "")
	b := queuedSession(t, st, "")
	started, _ := sch.Tick(context.Background())
	if started != 1 {
		t.Fatalf("started=%d, want 1 (target capacity)", started)
	}
	bs, _ := st.GetSession(b.ID)
	if bs.Status != model.SessionWaitingCapacity {
		t.Fatalf("second session status=%s, want waiting_capacity", bs.Status)
	}
	_ = o.Cancel(a.ID, false)
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
	if cs, _ := st.GetSession(c.ID); cs.Status != model.SessionRunning {
		t.Fatalf("C status=%s, want running", cs.Status)
	}
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
	started, _ := sch.Tick(context.Background())
	if started != 0 {
		t.Fatalf("exhausted provider should not start work, started=%d", started)
	}
	// The session is parked (so the loop doesn't respin) and the user is asked.
	if rs, _ := st.GetSession(s.ID); rs.Status != model.SessionWaitingUser {
		t.Fatalf("session should be parked waiting_user, got %s", rs.Status)
	}
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
