package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/nathanwhit/orcha/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mkSession(t *testing.T, st *Store, status model.SessionStatus) *model.Session {
	t.Helper()
	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: status}
	if err := st.CreateSession(s); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s
}

func TestUpdateSessionStatus_LateCompletionCannotResurrect(t *testing.T) {
	st := newTestStore(t)
	s := mkSession(t, st, model.SessionRunning)

	if _, err := st.UpdateSessionStatus(s.ID, model.SessionCanceled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// A late success completion must be rejected and the status must stay canceled.
	got, err := st.UpdateSessionStatus(s.ID, model.SessionSucceeded)
	if err == nil {
		t.Fatal("expected late completion to be rejected")
	}
	if got.Status != model.SessionCanceled {
		t.Fatalf("status changed to %s; canceled must be preserved", got.Status)
	}
	reloaded, _ := st.GetSession(s.ID)
	if reloaded.Status != model.SessionCanceled {
		t.Fatalf("persisted status is %s, want canceled", reloaded.Status)
	}
}

func TestUpdateSessionStatus_IllegalTransitionRejected(t *testing.T) {
	st := newTestStore(t)
	s := mkSession(t, st, model.SessionQueued)
	if _, err := st.UpdateSessionStatus(s.ID, model.SessionSucceeded); err == nil {
		t.Fatal("queued->succeeded should be illegal")
	}
}

func TestWorkspaceLock_PreventsConcurrentWriters(t *testing.T) {
	st := newTestStore(t)
	a := mkSession(t, st, model.SessionRunning)
	b := mkSession(t, st, model.SessionRunning)
	key := "workspace:ws1"

	if err := st.AcquireLock(key, model.LockWorkspace, a.ID, "write"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := st.AcquireLock(key, model.LockWorkspace, b.ID, "write"); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second writer should be blocked, got %v", err)
	}
	// Same holder re-acquire is a no-op.
	if err := st.AcquireLock(key, model.LockWorkspace, a.ID, "write"); err != nil {
		t.Fatalf("re-acquire by holder: %v", err)
	}
	// Release frees it.
	if err := st.ReleaseLock(key, a.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := st.AcquireLock(key, model.LockWorkspace, b.ID, "write"); err != nil {
		t.Fatalf("b should acquire after release: %v", err)
	}
}

func TestPRBranchLock_PreventsConcurrentPush(t *testing.T) {
	st := newTestStore(t)
	a := mkSession(t, st, model.SessionRunning)
	b := mkSession(t, st, model.SessionRunning)
	key := "pr_branch:pr1"
	if err := st.AcquireLock(key, model.LockPRBranch, a.ID, "push"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := st.AcquireLock(key, model.LockPRBranch, b.ID, "push"); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second updater must be blocked, got %v", err)
	}
}

func TestReleaseLocksHeldBy(t *testing.T) {
	st := newTestStore(t)
	a := mkSession(t, st, model.SessionRunning)
	_ = st.AcquireLock("workspace:w", model.LockWorkspace, a.ID, "")
	_ = st.AcquireLock("pr_branch:p", model.LockPRBranch, a.ID, "")
	if err := st.ReleaseLocksHeldBy(a.ID); err != nil {
		t.Fatalf("release all: %v", err)
	}
	if _, held, _ := st.LockHolder("workspace:w"); held {
		t.Error("workspace lock should be released")
	}
	if _, held, _ := st.LockHolder("pr_branch:p"); held {
		t.Error("pr branch lock should be released")
	}
}

func TestTargetDraining_PreventsNewSessions(t *testing.T) {
	st := newTestStore(t)
	tgt := &model.Target{Name: "t", Kind: model.TargetLocal, Status: model.TargetOnline,
		WorkRoot: "/w", CapacitySessions: 2}
	if err := st.CreateTarget(tgt); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := st.ClaimTargetSlot(tgt.ID); err != nil {
		t.Fatalf("claim while online: %v", err)
	}
	if err := st.SetTargetStatus(tgt.ID, model.TargetDraining); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// Draining: existing slot still held, but no new claims.
	if err := st.ClaimTargetSlot(tgt.ID); !errors.Is(err, ErrTargetNotSchedulable) {
		t.Fatalf("draining target should reject new sessions, got %v", err)
	}
}

func TestTargetCapacity_LimitsScheduling(t *testing.T) {
	st := newTestStore(t)
	tgt := &model.Target{Name: "t", Kind: model.TargetLocal, Status: model.TargetOnline,
		WorkRoot: "/w", CapacitySessions: 2}
	_ = st.CreateTarget(tgt)
	if err := st.ClaimTargetSlot(tgt.ID); err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if err := st.ClaimTargetSlot(tgt.ID); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if err := st.ClaimTargetSlot(tgt.ID); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("third claim should fail with no capacity, got %v", err)
	}
	// Releasing returns a slot; capacity cap prevents over-release.
	_ = st.ReleaseTargetSlot(tgt.ID)
	_ = st.ReleaseTargetSlot(tgt.ID)
	_ = st.ReleaseTargetSlot(tgt.ID) // extra release must not exceed capacity
	reloaded, _ := st.GetTarget(tgt.ID)
	if reloaded.AvailableSessions != 2 {
		t.Fatalf("available=%d, want 2 (capped)", reloaded.AvailableSessions)
	}
}

// The dashboard query must return only small rows — never transcript content,
// no matter how large the transcript grows.
func TestDashboard_NoTranscriptBlobs(t *testing.T) {
	st := newTestStore(t)
	obj := &model.Objective{Title: "Big", Prompt: "p"}
	_ = st.CreateObjective(obj)
	s := &model.Session{ObjectiveID: obj.ID, Role: model.RoleImplementer,
		Agent: model.AgentClaude, Status: model.SessionRunning, CurrentActivity: "compiling"}
	_ = st.CreateSession(s)

	huge := strings.Repeat("X", 200_000)
	for i := 0; i < 50; i++ {
		if err := st.AppendMessage(&model.Message{SessionID: s.ID, Source: model.MsgStdout,
			Kind: model.KindText, Content: huge}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	objRows, err := st.DashboardObjectives()
	if err != nil {
		t.Fatalf("dashboard objectives: %v", err)
	}
	sessRows, err := st.DashboardSessions("")
	if err != nil {
		t.Fatalf("dashboard sessions: %v", err)
	}
	// Assert no row carries the huge blob anywhere in its fields.
	for _, r := range objRows {
		if strings.Contains(r.LatestActivity, huge) || strings.Contains(r.Title, huge) {
			t.Fatal("objective dashboard row leaked transcript content")
		}
	}
	for _, r := range sessRows {
		if strings.Contains(r.CurrentActivity, huge) || strings.Contains(r.Title, huge) {
			t.Fatal("session dashboard row leaked transcript content")
		}
	}
	// The transcript itself is reachable via the separate, paginated path.
	msgs, _ := st.MessagesAfter(s.ID, 0, 10)
	if len(msgs) != 10 {
		t.Fatalf("expected paginated transcript fetch of 10, got %d", len(msgs))
	}
}

func TestMessagesAfter_Incremental(t *testing.T) {
	st := newTestStore(t)
	s := mkSession(t, st, model.SessionRunning)
	for i := 0; i < 5; i++ {
		_ = st.AppendMessage(&model.Message{SessionID: s.ID, Source: model.MsgAgent, Kind: model.KindText, Content: "m"})
	}
	first, _ := st.MessagesAfter(s.ID, 0, 2)
	if len(first) != 2 || first[0].Seq != 1 || first[1].Seq != 2 {
		t.Fatalf("bad first page: %+v", first)
	}
	next, _ := st.MessagesAfter(s.ID, first[1].Seq, 100)
	if len(next) != 3 || next[0].Seq != 3 {
		t.Fatalf("bad second page: %+v", next)
	}
}

func TestObjectiveTerminalIsFinal(t *testing.T) {
	st := newTestStore(t)
	obj := &model.Objective{Title: "x", Prompt: "p"}
	_ = st.CreateObjective(obj)
	if err := st.UpdateObjectiveStatus(obj.ID, model.ObjectiveSucceeded, "done"); err != nil {
		t.Fatalf("succeed: %v", err)
	}
	if err := st.UpdateObjectiveStatus(obj.ID, model.ObjectiveActive, ""); err == nil {
		t.Fatal("succeeded objective should not reactivate")
	}
}

func TestDeduplicatePRs(t *testing.T) {
	st := newTestStore(t)
	// Two rows for the same host PR (a prior adoption race), plus a distinct one.
	_ = st.CreatePR(&model.PullRequest{Repo: "o/r", Number: 7, Branch: "b", Status: model.PROpen})
	_ = st.CreatePR(&model.PullRequest{Repo: "o/r", Number: 7, Branch: "b", Status: model.PROpen})
	_ = st.CreatePR(&model.PullRequest{Repo: "o/r", Number: 8, Branch: "c", Status: model.PROpen})
	n, err := st.DeduplicatePRs()
	if err != nil {
		t.Fatalf("dedupe: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected to remove 1 duplicate, removed %d", n)
	}
	all, _ := st.ListPRs()
	if len(all) != 2 {
		t.Fatalf("expected 2 PRs after dedupe, got %d", len(all))
	}
	if pr, err := st.GetPRByRepoNumber("o/r", 7); err != nil || pr == nil {
		t.Fatalf("the surviving #7 should be findable, got %v %v", pr, err)
	}
}
