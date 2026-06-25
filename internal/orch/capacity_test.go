package orch

import (
	"errors"
	"testing"

	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/store"
)

// A manager is exempt from per-target worker capacity: it places on a target
// whose worker slots are full, while a worker requesting the same full target is
// refused. This is what stops a pool of idle managers from filling a box and
// starving real work.
func TestSelectTarget_ManagerIgnoresFullCapacity(t *testing.T) {
	o, st := newTestOrch(t)
	tgt := addTarget(t, st, "only", model.TargetLocal, 1)
	// Consume the single worker slot.
	if err := st.ClaimTargetSlot(tgt.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// A worker can't place — the box is full.
	if _, err := o.SelectTarget(TargetRequest{}); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("worker on full target: err=%v, want ErrNoTarget", err)
	}
	// A manager places anyway.
	got, err := o.SelectTarget(TargetRequest{IgnoreWorkerCapacity: true})
	if err != nil {
		t.Fatalf("manager on full target: %v", err)
	}
	if got.ID != tgt.ID {
		t.Fatalf("manager placed on %s, want %s", got.Name, tgt.Name)
	}
}

// Placing a manager must not debit a target's worker capacity, and releasing it
// must not credit one back — otherwise capacity drifts and the box oversubscribes
// real workers. A worker placed on the same target debits and credits normally.
func TestPlaceSession_ManagerDoesNotConsumeWorkerSlot(t *testing.T) {
	o, st := newTestOrch(t)
	tgt := addTarget(t, st, "box", model.TargetLocal, 2)

	mgr := &model.Session{Role: model.RoleManager, Agent: model.AgentClaude, Status: model.SessionQueued}
	if err := st.CreateSession(mgr); err != nil {
		t.Fatalf("create mgr: %v", err)
	}
	if _, err := o.PlaceSession(mgr.ID, o.targetRequestFor(mgr)); err != nil {
		t.Fatalf("place mgr: %v", err)
	}
	if avail := mustAvail(t, st, tgt.ID); avail != 2 {
		t.Fatalf("after manager placement available=%d, want 2 (unchanged)", avail)
	}

	worker := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionQueued}
	if err := st.CreateSession(worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	if _, err := o.PlaceSession(worker.ID, o.targetRequestFor(worker)); err != nil {
		t.Fatalf("place worker: %v", err)
	}
	if avail := mustAvail(t, st, tgt.ID); avail != 1 {
		t.Fatalf("after worker placement available=%d, want 1", avail)
	}

	// Release both: the manager is a no-op, the worker credits its slot back.
	mgr, _ = st.GetSession(mgr.ID)
	o.releaseTargetSlot(mgr)
	if avail := mustAvail(t, st, tgt.ID); avail != 1 {
		t.Fatalf("after manager release available=%d, want 1 (no credit)", avail)
	}
	worker, _ = st.GetSession(worker.ID)
	o.releaseTargetSlot(worker)
	if avail := mustAvail(t, st, tgt.ID); avail != 2 {
		t.Fatalf("after worker release available=%d, want 2", avail)
	}
}

// ReconcileTargetSlots rebuilds available_sessions from live occupancy: it counts
// non-terminal workers, ignores managers (capacity-exempt) and terminal rows, and
// heals a drifted counter — the migration that frees the slots managers held
// under the old accounting.
func TestReconcileTargetSlots(t *testing.T) {
	_, st := newTestOrch(t)
	tgt := addTarget(t, st, "box", model.TargetLocal, 4)

	bind := func(role model.SessionRole, status model.SessionStatus) {
		s := &model.Session{Role: role, Agent: model.AgentClaude, Status: status, TargetID: tgt.ID}
		if err := st.CreateSession(s); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	bind(model.RoleImplementer, model.SessionRunning)   // counts
	bind(model.RoleReviewer, model.SessionStarting)     // counts
	bind(model.RoleManager, model.SessionRunning)       // exempt — does not count
	bind(model.RoleImplementer, model.SessionSucceeded) // terminal — does not count

	// Drift the counter to a wrong value (e.g. managers had claimed under the old
	// accounting): available 4 -> 0.
	for i := 0; i < 4; i++ {
		_ = st.ClaimTargetSlot(tgt.ID)
	}
	if avail := mustAvail(t, st, tgt.ID); avail != 0 {
		t.Fatalf("precondition: available=%d, want 0", avail)
	}

	n, err := st.ReconcileTargetSlots()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("corrected %d targets, want 1", n)
	}
	// 4 capacity - 2 live workers = 2; manager + terminal worker excluded.
	if avail := mustAvail(t, st, tgt.ID); avail != 2 {
		t.Fatalf("after reconcile available=%d, want 2", avail)
	}

	// Idempotent: a second pass corrects nothing.
	if n, err := st.ReconcileTargetSlots(); err != nil || n != 0 {
		t.Fatalf("second reconcile n=%d err=%v, want 0 nil", n, err)
	}
}

func mustAvail(t *testing.T, st *store.Store, targetID string) int {
	t.Helper()
	tgt, err := st.GetTarget(targetID)
	if err != nil {
		t.Fatalf("get target: %v", err)
	}
	return tgt.AvailableSessions
}
