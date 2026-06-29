package orch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nathanwhit/orcha/internal/model"
)

func TestParseFreeDiskMB(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		wantMB int
		wantOK bool
	}{
		{"normal", "load 1.0 cores 4 memkb 1048576 freekb 20971520", 20480, true},
		{"zero free is valid", "freekb 0", 0, true},
		{"missing token fails open", "load 1.0 cores 4 memkb 1024", 0, false},
		{"empty token fails open", "load 1.0 cores 4 memkb 1024 freekb", 0, false},
		{"garbage fails open", "nothing here", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mb, ok := parseFreeDiskMB(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if ok && mb != c.wantMB {
				t.Fatalf("freeMB=%d want %d", mb, c.wantMB)
			}
		})
	}
}

// The disk guard must skip a target below the free-disk floor and place on one
// with room, even when the low-disk box has far more free capacity slots.
func TestSelectTarget_SkipsDiskPressuredPicksRoomy(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.MinFreeDiskMB = 10240 // 10 GB floor
	roomy := addTarget(t, st, "roomy", model.TargetLocal, 4)
	big := addTarget(t, st, "big", model.TargetSSH, 16)
	mustSetDisk(t, st, big.ID, 2048)    // 2 GB free — pressured
	mustSetDisk(t, st, roomy.ID, 50000) // ~50 GB free

	got, err := o.SelectTarget(TargetRequest{})
	if err != nil {
		t.Fatalf("SelectTarget: %v", err)
	}
	if got.ID != roomy.ID {
		t.Fatalf("placed on %s, want the roomy target %s", got.Name, roomy.Name)
	}
}

// When every schedulable target is below the floor, placement is refused
// (ErrNoTarget) so the session parks as waiting_capacity instead of filling a
// volume to 100% — backpressure rather than wedging the box.
func TestSelectTarget_AllDiskPressuredReturnsNoTarget(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.MinFreeDiskMB = 10240
	a := addTarget(t, st, "a", model.TargetLocal, 4)
	b := addTarget(t, st, "b", model.TargetSSH, 16)
	mustSetDisk(t, st, a.ID, 500)
	mustSetDisk(t, st, b.ID, 1000)

	if _, err := o.SelectTarget(TargetRequest{}); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("err=%v, want ErrNoTarget", err)
	}
}

// With the disk guard disabled (MinFreeDiskMB<=0), a low-disk reading is ignored.
func TestSelectTarget_DiskGuardDisabledIgnoresDisk(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.MinFreeDiskMB = 0
	big := addTarget(t, st, "big", model.TargetSSH, 16)
	mustSetDisk(t, st, big.ID, 1) // 1 MB free — would be gated if enabled

	got, err := o.SelectTarget(TargetRequest{})
	if err != nil {
		t.Fatalf("SelectTarget: %v", err)
	}
	if got.ID != big.ID {
		t.Fatalf("placed on %s, want %s when the disk guard is off", got.Name, big.Name)
	}
}

// A stale disk sample must not gate a target forever: past loadStaleAfter the
// reading is ignored and the target schedules as if no data exists (fail open).
func TestSelectTarget_StaleDiskFailsOpen(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.MinFreeDiskMB = 10240
	old := st.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	tgt := &model.Target{
		Name: "stale", Kind: model.TargetSSH, Status: model.TargetOnline,
		WorkRoot: "/w", CapacitySessions: 8,
		Metadata: model.JSONMap{"free_disk_mb": 1, "disk_probed_at": old},
	}
	if err := st.CreateTarget(tgt); err != nil {
		t.Fatalf("create target: %v", err)
	}

	got, err := o.SelectTarget(TargetRequest{})
	if err != nil {
		t.Fatalf("stale sample should fail open, got: %v", err)
	}
	if got.ID != tgt.ID {
		t.Fatalf("placed on %s, want %s", got.Name, tgt.Name)
	}
}

// SetTargetDisk persists the sample; targetFreeDiskMB reads it back fresh and
// diskPressured compares it to the floor.
func TestSetTargetDisk_RoundTripAndGate(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.MinFreeDiskMB = 10240
	tgt := addTarget(t, st, "t", model.TargetSSH, 8)

	// Above the floor: fresh sample, not pressured.
	if err := st.SetTargetDisk(tgt.ID, 50000, st.Now()); err != nil {
		t.Fatalf("SetTargetDisk: %v", err)
	}
	reread, _ := st.GetTarget(tgt.ID)
	if free, ok := o.targetFreeDiskMB(reread, st.Now()); !ok || free != 50000 {
		t.Fatalf("free_disk_mb=%d ok=%v, want 50000 true", free, ok)
	}
	if o.diskPressured(reread, st.Now()) {
		t.Fatal("50 GB free must not be disk-pressured under a 10 GB floor")
	}

	// Below the floor: pressured.
	if err := st.SetTargetDisk(tgt.ID, 2048, st.Now()); err != nil {
		t.Fatalf("SetTargetDisk: %v", err)
	}
	reread, _ = st.GetTarget(tgt.ID)
	if !o.diskPressured(reread, st.Now()) {
		t.Fatal("2 GB free must be disk-pressured under a 10 GB floor")
	}
}

// A pr_branch checkout whose PR is terminal (merged/closed) is reclaimed even
// while its objective stays ACTIVE — the disk-full incident, where a closed PR's
// multi-GB checkout sat on the orch host for hours. An OPEN PR's checkout is kept
// (a future follow-up may reuse it), and a checkout whose dir a LIVE session
// still occupies is kept (path-aware, including a finished row that shares a dir
// with a still-running follow-up).
func TestReclaimWorkspaces_ClosedPRBranchCheckout(t *testing.T) {
	o, st := newTestOrch(t)
	tgt := addTarget(t, st, "local", model.TargetLocal, 4)
	obj := &model.Objective{Title: "active", Prompt: "p", Status: model.ObjectiveActive}
	_ = st.CreateObjective(obj)

	mkdir := func(name string) string {
		d := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	// row creates a pr_branch workspace at path owned by a follow-up session in the
	// given status (path may be shared across rows, as real re-preps are).
	row := func(path, branch string, sessStatus model.SessionStatus) *model.Workspace {
		s := &model.Session{ObjectiveID: obj.ID, Role: model.RolePRFollowup, Agent: model.AgentClaude, Status: sessStatus}
		if err := st.CreateSession(s); err != nil {
			t.Fatal(err)
		}
		ws := &model.Workspace{ObjectiveID: obj.ID, TargetID: tgt.ID, SessionID: s.ID,
			Kind: model.WorkspacePRBranch, BranchName: branch, Path: path, Status: model.WorkspaceReady}
		if err := st.CreateWorkspace(ws); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpdateSessionRuntime(s.ID, func(se *model.Session) { se.WorkspaceID = ws.ID }); err != nil {
			t.Fatal(err)
		}
		return ws
	}
	pr := func(num int, branch string, status model.PRStatus) {
		if err := st.CreatePR(&model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo",
			Number: num, Branch: branch, BaseBranch: "main", Status: status}); err != nil {
			t.Fatal(err)
		}
	}

	// Reclaimed: closed PR, finished follow-up.
	closedPath := mkdir("closed")
	closedWS := row(closedPath, "orcha/pr-closed", model.SessionSucceeded)
	pr(1, "orcha/pr-closed", model.PRClosed)
	// Reclaimed: merged PR, finished follow-up.
	mergedPath := mkdir("merged")
	mergedWS := row(mergedPath, "orcha/pr-merged", model.SessionSucceeded)
	pr(2, "orcha/pr-merged", model.PRMerged)
	// Kept: open PR — a future follow-up may reuse the checkout.
	openPath := mkdir("open")
	openWS := row(openPath, "orcha/pr-open", model.SessionSucceeded)
	pr(3, "orcha/pr-open", model.PROpen)
	// Kept (path-aware): closed PR, but two rows share one dir and one follow-up is
	// still running on it — the finished row must NOT reclaim the shared dir.
	sharedPath := mkdir("shared")
	sharedDone := row(sharedPath, "orcha/pr-shared", model.SessionSucceeded)
	_ = row(sharedPath, "orcha/pr-shared", model.SessionRunning)
	pr(4, "orcha/pr-shared", model.PRClosed)

	o.ReclaimWorkspaces(context.Background())

	for name, r := range map[string]struct{ path, id string }{
		"closed-pr": {closedPath, closedWS.ID},
		"merged-pr": {mergedPath, mergedWS.ID},
	} {
		if _, err := os.Stat(r.path); !os.IsNotExist(err) {
			t.Fatalf("%s checkout should be reclaimed, stat err=%v", name, err)
		}
		if ws, _ := st.GetWorkspace(r.id); ws.Status != model.WorkspaceArchived {
			t.Fatalf("%s workspace should be archived, got %s", name, ws.Status)
		}
	}
	for name, kept := range map[string]struct{ path, id string }{
		"open-pr":     {openPath, openWS.ID},
		"shared-live": {sharedPath, sharedDone.ID},
	} {
		if _, err := os.Stat(kept.path); err != nil {
			t.Fatalf("%s checkout must be kept, stat err=%v", name, err)
		}
		if ws, _ := st.GetWorkspace(kept.id); ws.Status != model.WorkspaceReady {
			t.Fatalf("%s workspace must stay ready, got %s", name, ws.Status)
		}
	}
}

func mustSetDisk(t *testing.T, st interface {
	SetTargetDisk(string, int, time.Time) error
	Now() time.Time
}, id string, freeMB int) {
	t.Helper()
	if err := st.SetTargetDisk(id, freeMB, st.Now()); err != nil {
		t.Fatalf("SetTargetDisk: %v", err)
	}
}
