package orch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/forge"
	"github.com/nathanwhit/orcha/internal/mcp"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/store"
	"github.com/nathanwhit/orcha/internal/workspace"
)

func newTestOrch(t *testing.T) (*Orchestrator, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	o := New(st, Config{Guards: DefaultGuards(), ProviderFallback: []model.AgentKind{model.AgentClaude, model.AgentCodex}})
	return o, st
}

func addTarget(t *testing.T, st *store.Store, name string, kind model.TargetKind, cap int) *model.Target {
	t.Helper()
	tgt := &model.Target{Name: name, Kind: kind, Status: model.TargetOnline, WorkRoot: "/work/" + name, CapacitySessions: cap}
	if err := st.CreateTarget(tgt); err != nil {
		t.Fatalf("create target: %v", err)
	}
	return tgt
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

// ---- guards ----

func TestSameErrorLoopPauses(t *testing.T) {
	o, st := newTestOrch(t)
	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionRunning}
	_ = st.CreateSession(s)

	var tripped bool
	for i := 0; i < o.cfg.Guards.MaxSameErrorRetries; i++ {
		if err := o.CheckError(s.ID, "boom: same error"); err != nil {
			tripped = true
			_ = o.PauseSession(s.ID, err.Error())
		}
	}
	if !tripped {
		t.Fatal("repeated identical error should trip the guard")
	}
	reloaded, _ := st.GetSession(s.ID)
	if reloaded.Status != model.SessionWaitingUser {
		t.Fatalf("session should be paused (waiting_user), got %s", reloaded.Status)
	}
	// A question is opened so the user/manager can give direction.
	qs, _ := st.ListOpenQuestions()
	if len(qs) == 0 {
		t.Fatal("a guard pause should open a question")
	}
}

func TestNoProgressLoopPauses(t *testing.T) {
	o, st := newTestOrch(t)
	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionRunning}
	_ = st.CreateSession(s)

	var tripped bool
	for i := 0; i < o.cfg.Guards.MaxNoProgressTurns; i++ {
		if err := o.CheckNoProgress(s.ID); err != nil {
			tripped = true
			_ = o.PauseSession(s.ID, err.Error())
		}
	}
	if !tripped {
		t.Fatal("no-progress turns should trip the guard")
	}
	// Progress resets the counter.
	o.RecordProgress(s.ID)
	if err := o.CheckNoProgress(s.ID); err != nil {
		t.Fatal("counter should reset after progress")
	}
}

// ---- usage exhaustion ----

func TestUsageExhaustion_SwitchesProvider(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.RegisterProvider(agent.NewFake(model.AgentCodex, true, nil))

	// Mark the preferred provider exhausted.
	_ = st.UpsertUsage(&model.UsageBucket{Provider: string(model.AgentClaude), State: model.UsageExhausted, WindowStart: st.Now(), WindowEnd: st.Now()})

	kind, err := o.SelectProvider(model.AgentClaude)
	if err != nil {
		t.Fatalf("expected fallback, got error %v", err)
	}
	if kind != model.AgentCodex {
		t.Fatalf("expected fallback to codex, got %s", kind)
	}
}

func TestUsageExhaustion_AsksUserWhenNoFallback(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.ProviderFallback = nil
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	_ = st.UpsertUsage(&model.UsageBucket{Provider: string(model.AgentClaude), State: model.UsageExhausted, WindowStart: st.Now(), WindowEnd: st.Now()})

	if _, err := o.SelectProvider(model.AgentClaude); err != ErrProviderExhausted {
		t.Fatalf("expected ErrProviderExhausted, got %v", err)
	}
}

// ---- steering ----

func TestInteractiveSteering_ReachesSession(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 2)

	// Script echoes any steering input back into the transcript as agent text.
	echo := func(ctx context.Context, spec agent.Spec, inputs <-chan string, out chan<- agent.Event) {
		out <- agent.Event{Kind: agent.EventText, Source: model.MsgAgent, Content: "ready"}
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-inputs:
				out <- agent.Event{Kind: agent.EventText, Source: model.MsgAgent, Content: "ack:" + msg}
			}
		}
	}
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, echo))

	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionQueued}
	_ = st.CreateSession(s)
	if _, err := o.StartRun(context.Background(), s.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := o.Steer(context.Background(), s.ID, "please refactor"); err != nil {
		t.Fatalf("steer: %v", err)
	}
	waitFor(t, func() bool {
		msgs, _ := st.MessagesAfter(s.ID, 0, 100)
		for _, m := range msgs {
			if m.Content == "ack:please refactor" {
				return true
			}
		}
		return false
	})
	_ = o.Cancel(s.ID, false)
}

func TestNonInteractiveSteering_CancelsAndResumes(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 2)

	script := func(ctx context.Context, spec agent.Spec, inputs <-chan string, out chan<- agent.Event) {
		out <- agent.Event{Kind: agent.EventText, Source: model.MsgAgent, Content: "phase"}
		<-ctx.Done() // honor process-group cancellation
	}
	fake := agent.NewFake(model.AgentCodex, false, script) // non-interactive
	o.RegisterProvider(fake)

	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentCodex, Status: model.SessionQueued}
	_ = st.CreateSession(s)
	if _, err := o.StartRun(context.Background(), s.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, func() bool { s2, _ := st.GetSession(s.ID); return s2.Status == model.SessionRunning })

	if err := o.Steer(context.Background(), s.ID, "change direction"); err != nil {
		t.Fatalf("steer: %v", err)
	}
	// Non-interactive steering cancels the current process and resumes, while
	// preserving the logical session identity (not terminal).
	if !fake.WasCanceled(s.ID) {
		t.Fatal("non-interactive steering should cancel the current process")
	}
	resumed := fake.Resumed()
	found := false
	for _, id := range resumed {
		if id == s.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("session should have been resumed, resumed=%v", resumed)
	}
	reloaded, _ := st.GetSession(s.ID)
	if reloaded.Status.IsTerminal() {
		t.Fatalf("session identity must be preserved (non-terminal), got %s", reloaded.Status)
	}
	_ = o.Cancel(s.ID, false)
}

// ---- remote target ----

func TestRemoteTarget_StreamsLogs(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "remote", model.TargetSSH, 1)

	script := func(ctx context.Context, spec agent.Spec, inputs <-chan string, out chan<- agent.Event) {
		for i := 0; i < 3; i++ {
			out <- agent.Event{Kind: agent.EventStdout, Content: "build log line"}
		}
		out <- agent.Event{Kind: agent.EventDone, Success: true}
	}
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, script))

	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionQueued}
	_ = st.CreateSession(s)
	run, err := o.StartRun(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	run.Wait()

	msgs, _ := st.MessagesAfter(s.ID, 0, 100)
	var stdout int
	for _, m := range msgs {
		if m.Source == model.MsgStdout {
			stdout++
		}
	}
	if stdout != 3 {
		t.Fatalf("expected 3 streamed stdout log rows, got %d", stdout)
	}
	reloaded, _ := st.GetSession(s.ID)
	if reloaded.Status != model.SessionSucceeded {
		t.Fatalf("session should succeed, got %s", reloaded.Status)
	}
}

func TestRemoteCancel_KillsProcessGroup(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "remote", model.TargetSSH, 1)

	blocked := make(chan struct{})
	script := func(ctx context.Context, spec agent.Spec, inputs <-chan string, out chan<- agent.Event) {
		out <- agent.Event{Kind: agent.EventText, Source: model.MsgAgent, Content: "running"}
		close(blocked)
		<-ctx.Done() // models a long-running remote process group
	}
	fake := agent.NewFake(model.AgentClaude, true, script)
	o.RegisterProvider(fake)

	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionQueued}
	_ = st.CreateSession(s)
	run, err := o.StartRun(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	<-blocked

	if err := o.Cancel(s.ID, false); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	run.Wait()

	if !fake.WasCanceled(s.ID) {
		t.Fatal("cancel should kill the remote process group")
	}
	reloaded, _ := st.GetSession(s.ID)
	if reloaded.Status != model.SessionCanceled {
		t.Fatalf("session should be canceled, got %s", reloaded.Status)
	}
	// Slot must be released back to the target.
	tgt, _ := st.ListTargets()
	if tgt[0].AvailableSessions != tgt[0].CapacitySessions {
		t.Fatalf("target slot not released: avail=%d cap=%d", tgt[0].AvailableSessions, tgt[0].CapacitySessions)
	}
}

// A completion that arrives after cancellation must not resurrect the session.
func TestLateCompletion_DoesNotResurrect(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 1)

	release := make(chan struct{})
	started := make(chan struct{})
	script := func(ctx context.Context, spec agent.Spec, inputs <-chan string, out chan<- agent.Event) {
		out <- agent.Event{Kind: agent.EventText, Source: model.MsgAgent, Content: "go"}
		close(started)
		<-release // ignores ctx on purpose: a late completion racing cancellation
		out <- agent.Event{Kind: agent.EventDone, Success: true}
	}
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, script))

	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionQueued}
	_ = st.CreateSession(s)
	run, _ := o.StartRun(context.Background(), s.ID)
	<-started

	_ = o.Cancel(s.ID, false)
	close(release) // now the (late) success completion fires
	run.Wait()

	reloaded, _ := st.GetSession(s.ID)
	if reloaded.Status != model.SessionCanceled {
		t.Fatalf("late completion resurrected canceled session to %s", reloaded.Status)
	}
}

// ---- PRs ----

func TestObjective_OpensTwoIndependentPRs(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetForge(forge.NewFake())

	obj, _, err := o.CreateObjective(NewObjectiveSpec{Title: "Broad work", Prompt: "do two things"})
	if err != nil {
		t.Fatalf("create objective: %v", err)
	}

	publish := func(title string) *model.PullRequest {
		s, err := o.CreateSession(SpawnSpec{ObjectiveID: obj.ID, Role: model.RoleImplementer, Agent: model.AgentClaude})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		if _, err := o.PrepareIsolatedWorkspace(context.Background(), s.ID, "octo/repo", "", "main"); err != nil {
			t.Fatalf("workspace: %v", err)
		}
		pr, err := o.PublishPR(context.Background(), s.ID, PublishSpec{Title: title, Body: "b", CommitMessage: "c"})
		if err != nil {
			t.Fatalf("publish %s: %v", title, err)
		}
		return pr
	}

	pr1 := publish("First slice")
	// Publishing the first PR must not block the objective.
	reloaded, _ := st.GetObjective(obj.ID)
	if reloaded.Status != model.ObjectiveActive {
		t.Fatalf("objective should still be active after first PR, got %s", reloaded.Status)
	}
	pr2 := publish("Second slice")

	if pr1.ID == pr2.ID || pr1.Number == pr2.Number {
		t.Fatal("two distinct PRs expected")
	}
	prs, _ := st.ListPRsByObjective(obj.ID)
	if len(prs) != 2 {
		t.Fatalf("expected 2 PRs under objective, got %d", len(prs))
	}
}

func TestPublishPR_RejectsWhenNoDiff(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 2)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	f := forge.NewFake()
	o.SetForge(f)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	s, _ := o.CreateSession(SpawnSpec{ObjectiveID: obj.ID, Role: model.RoleImplementer, Agent: model.AgentClaude})
	ws, _ := o.PrepareIsolatedWorkspace(context.Background(), s.ID, "octo/repo", "", "main")
	f.SetDiff(ws.Path, false) // mechanical safety: no diff -> refuse to publish

	if _, err := o.PublishPR(context.Background(), s.ID, PublishSpec{Title: "t", Body: "b"}); err == nil {
		t.Fatal("publish should fail when workspace has no diff")
	}
}

// CommentPR tags comments with the orcha-bot marker, but agents frequently copy
// that marker into their own body (they see it on prior comments). The marker
// must not be doubled.
func TestCommentPR_DoesNotDoubleMarker(t *testing.T) {
	o, st := newTestOrch(t)
	f := forge.NewFake()
	o.SetForge(f)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 9, Branch: "b",
		BaseBranch: "main", Status: model.PROpen}
	_ = st.CreatePR(pr)

	// Body already carries the marker (as an agent's would).
	if err := o.CommentPR(context.Background(), pr.ID, "addressed the review\n\n"+orchaBotMarker); err != nil {
		t.Fatalf("comment: %v", err)
	}
	// A clean body gets the marker appended exactly once.
	if err := o.CommentPR(context.Background(), pr.ID, "explained the new commit"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	for _, c := range f.Comments {
		if n := strings.Count(c.Body, orchaBotMarker); n != 1 {
			t.Fatalf("comment has %d markers, want exactly 1: %q", n, c.Body)
		}
	}
}

func TestUpdatePR_MergedCannotPush(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	f := forge.NewFake()
	o.SetForge(f)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 7, Branch: "b",
		BaseBranch: "main", Status: model.PROpen, ChecksState: model.ChecksPassing}
	_ = st.CreatePR(pr)
	// Host reports the PR is merged.
	f.SetPRState("octo/repo", 7, forge.PRState{Number: 7, Status: "merged", ChecksState: "passing"})

	if _, err := o.UpdatePR(context.Background(), pr.ID, UpdateSpec{SessionID: "x"}); err == nil {
		t.Fatal("must not push to a merged PR")
	}
	if len(f.Pushes) != 0 {
		t.Fatalf("no push should have happened, got %d", len(f.Pushes))
	}
}

func TestUpdatePR_ClosedCreatesManagerDecision(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	f := forge.NewFake()
	o.SetForge(f)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 9, Branch: "b",
		BaseBranch: "main", Status: model.PROpen}
	_ = st.CreatePR(pr)
	f.SetPRState("octo/repo", 9, forge.PRState{Number: 9, Status: "closed"})

	if _, err := o.UpdatePR(context.Background(), pr.ID, UpdateSpec{SessionID: "x"}); err == nil {
		t.Fatal("closed PR should not push")
	}
	// A manager decision point (question) must be opened.
	qs, _ := st.ListQuestionsByObjective(obj.ID)
	if len(qs) == 0 {
		t.Fatal("closed PR should create a manager decision question")
	}
}

func TestUpdatePR_ForcePushRequiresReason(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetForge(forge.NewFake())
	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 3, Branch: "b", BaseBranch: "main", Status: model.PROpen}
	_ = st.CreatePR(pr)
	if _, err := o.UpdatePR(context.Background(), pr.ID, UpdateSpec{SessionID: "x", Force: true}); err == nil {
		t.Fatal("force push without a reason must be rejected")
	}
}

// ---- feedback ----

func TestPRFeedback_SpawnsFollowupWhileWorkerRuns(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetForge(forge.NewFake())

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})

	// An unrelated worker is busy.
	worker := &model.Session{ObjectiveID: obj.ID, Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionRunning}
	_ = st.CreateSession(worker)

	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 11, Branch: "feature",
		BaseBranch: "main", HeadSHA: "sha1", Status: model.PROpen}
	_ = st.CreatePR(pr)

	// CI failure feedback arrives.
	if err := o.IngestFeedback(context.Background(), pr.ID, []model.PRFeedback{
		{Kind: model.FeedbackCheckFailure, ExternalID: "run-1", Body: "tests failed", Actionable: true},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	spawned, err := o.ProcessFeedback(context.Background(), pr.ID)
	if err != nil {
		t.Fatalf("process feedback: %v", err)
	}
	if len(spawned) != 1 {
		t.Fatalf("expected 1 follow-up session, got %d", len(spawned))
	}
	fu := spawned[0]
	if fu.Role != model.RoleCIFollowup {
		t.Fatalf("expected ci_followup role, got %s", fu.Role)
	}
	if fu.Metadata["pr_id"] != pr.ID {
		t.Fatal("follow-up must be attached to the PR")
	}
	// The follow-up uses a PR-branch workspace.
	ws, _ := st.GetWorkspace(fu.WorkspaceID)
	if ws.Kind != model.WorkspacePRBranch || ws.BranchName != pr.Branch {
		t.Fatalf("follow-up should use the PR branch workspace, got %+v", ws)
	}
	// The unrelated worker continues running — feedback didn't wait on it.
	w2, _ := st.GetSession(worker.ID)
	if w2.Status != model.SessionRunning {
		t.Fatalf("unrelated worker should still be running, got %s", w2.Status)
	}

	// Re-polling the same external event is deduped (no second follow-up).
	_ = o.IngestFeedback(context.Background(), pr.ID, []model.PRFeedback{
		{Kind: model.FeedbackCheckFailure, ExternalID: "run-1", Body: "tests failed", Actionable: true},
	})
	again, _ := o.ProcessFeedback(context.Background(), pr.ID)
	if len(again) != 0 {
		t.Fatalf("duplicate feedback should not spawn another follow-up, got %d", len(again))
	}
}

func TestAdoptUntrackedPR(t *testing.T) {
	o, st := newTestOrch(t)
	f := forge.NewFake()
	o.SetForge(f)

	obj, mgr, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p", Repo: "octo/repo"})
	// The manager's checkout — where an agent could run gh out-of-band.
	ws := &model.Workspace{ObjectiveID: obj.ID, SessionID: mgr.ID,
		Kind: model.WorkspaceIsolated, ProjectPath: "octo/repo", VCS: model.VCSGit,
		BranchName: "orcha/mana-abc", BaseRef: "main", Status: model.WorkspaceReady,
		Metadata: model.JSONMap{"repo": "octo/repo"}}
	_ = st.CreateWorkspace(ws)
	_, _ = st.UpdateSessionRuntime(mgr.ID, func(s *model.Session) { s.WorkspaceID = ws.ID })

	// An agent opened a PR out-of-band on that branch, with a [codex] title.
	f.SetOpenPRByBranch("octo/repo", "orcha/mana-abc", forge.PRState{
		Number: 55, URL: "https://forge.test/octo/repo/pull/55", Status: "open",
		ChecksState: "pending", HeadSHA: "deadbeef", Title: "[codex] add ci"})

	if n := o.AdoptUntrackedPRs(context.Background(), obj.ID); n != 1 {
		t.Fatalf("expected to adopt 1 out-of-band PR, got %d", n)
	}
	prs, _ := st.ListPRsByObjective(obj.ID)
	if len(prs) != 1 || prs[0].Number != 55 || prs[0].Branch != "orcha/mana-abc" {
		t.Fatalf("adopted PR not tracked correctly: %+v", prs)
	}
	if prs[0].Title != "add ci" {
		t.Fatalf("adopted PR title should be cleaned of the [codex] tag, got %q", prs[0].Title)
	}
	// Idempotent: a second scan adopts nothing (it is now tracked).
	if n := o.AdoptUntrackedPRs(context.Background(), obj.ID); n != 0 {
		t.Fatalf("re-scan should adopt 0, got %d", n)
	}
}

func TestPRMerge_NotifiesManagerToWrapUp(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetForge(forge.NewFake())

	obj, mgr, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	_, _ = st.UpdateSessionStatus(mgr.ID, model.SessionRunning)
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 7, Branch: "feature",
		BaseBranch: "main", HeadSHA: "sha1", Status: model.PROpen, Title: "do the thing"}
	_ = st.CreatePR(pr)

	// The PR merges (observed as a feedback event).
	if err := o.IngestFeedback(context.Background(), pr.ID, []model.PRFeedback{
		{Kind: model.FeedbackMerged, ExternalID: "merge-1"},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// The manager was steered: a merge nudge is in its transcript, and since this
	// was the only PR and there are no workers, it must point at mark_objective_done.
	msgs, _ := st.MessagesAfter(mgr.ID, 0, 50)
	var steer string
	for _, m := range msgs {
		if m.Source == model.MsgUser && strings.Contains(m.Content, "was merged") {
			steer = m.Content
		}
	}
	if steer == "" {
		t.Fatal("manager was not notified that the PR merged")
	}
	if !strings.Contains(steer, "mark_objective_done") {
		t.Fatalf("with all PRs merged and no workers, the nudge should mention mark_objective_done; got %q", steer)
	}

	// Re-observing the same merge does not re-notify (no transition).
	_ = o.IngestFeedback(context.Background(), pr.ID, []model.PRFeedback{
		{Kind: model.FeedbackMerged, ExternalID: "merge-2"},
	})
	msgs2, _ := st.MessagesAfter(mgr.ID, 0, 50)
	count := 0
	for _, m := range msgs2 {
		if strings.Contains(m.Content, "was merged") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("merge should notify exactly once, got %d", count)
	}
}

func TestDependentWorker_InheritsPredecessorWorkspace(t *testing.T) {
	o, st := newTestOrch(t)
	tgt := addTarget(t, st, "local", model.TargetLocal, 4)
	// A preparer must be installed for ensureWorkspace to act; the inheritance
	// path never invokes it (no real clone happens here).
	o.SetWorkspacePreparer(workspace.New())

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p", Repo: "octo/repo"})

	// An implementer that already produced a ready isolated checkout.
	impl := &model.Session{ObjectiveID: obj.ID, Role: model.RoleImplementer, Agent: model.AgentClaude,
		Status: model.SessionSucceeded, TargetID: tgt.ID}
	_ = st.CreateSession(impl)
	ws := &model.Workspace{ObjectiveID: obj.ID, SessionID: impl.ID, TargetID: tgt.ID,
		Kind: model.WorkspaceIsolated, ProjectPath: "octo/repo", VCS: model.VCSGit,
		Path: tgt.WorkRoot + "/" + impl.ID, BranchName: "orcha/impl-x", BaseRef: "main", Status: model.WorkspaceReady}
	_ = st.CreateWorkspace(ws)
	_, _ = st.UpdateSessionRuntime(impl.ID, func(s *model.Session) { s.WorkspaceID = ws.ID })

	// A validator that depends on the implementer.
	val := &model.Session{ObjectiveID: obj.ID, Role: model.RoleValidator, Agent: model.AgentClaude,
		Status: model.SessionQueued, Metadata: model.JSONMap{"depends_on": []any{impl.ID}}}
	_ = st.CreateSession(val)

	// Placement pins it to the predecessor's target so the checkout is local.
	if req := o.targetRequestFor(val); req.PinnedTargetID != tgt.ID {
		t.Fatalf("dependent should pin to the predecessor's target %s, got %q", tgt.ID, req.PinnedTargetID)
	}

	// And it inherits the implementer's workspace instead of cloning a clean tree.
	_, _ = st.UpdateSessionRuntime(val.ID, func(s *model.Session) { s.TargetID = tgt.ID })
	val, _ = st.GetSession(val.ID)
	if err := o.ensureWorkspace(context.Background(), val, tgt); err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	got, _ := st.GetSession(val.ID)
	if got.WorkspaceID != ws.ID {
		t.Fatalf("validator should continue the implementer's branch (workspace %s), got %s", ws.ID, got.WorkspaceID)
	}
}

// ---- real workspace preparation ----

func tgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	hermeticGit(t)
	c := osexec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=orcha", "GIT_AUTHOR_EMAIL=orcha@test",
		"GIT_COMMITTER_NAME=orcha", "GIT_COMMITTER_EMAIL=orcha@test",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// The orchestrator, with a real preparer installed, materializes an isolated
// workspace as a real git checkout branched off fresh upstream.
func TestOrch_PreparesRealWorkspace(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetWorkspacePreparer(workspace.New())

	// Seed a bare "remote" with a commit on main.
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	tgit(t, root, "init", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	tgit(t, root, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, seed, "add", ".")
	tgit(t, seed, "commit", "-m", "initial")
	tgit(t, seed, "remote", "add", "origin", bare)
	tgit(t, seed, "push", "-u", "origin", "main")
	mainSha := tgit(t, seed, "rev-parse", "HEAD")

	// Target work root is a writable temp dir.
	tgt := &model.Target{Name: "local", Kind: model.TargetLocal, Status: model.TargetOnline,
		WorkRoot: filepath.Join(root, "work"), CapacitySessions: 2}
	if err := st.CreateTarget(tgt); err != nil {
		t.Fatalf("target: %v", err)
	}

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	sess, _ := o.CreateSession(SpawnSpec{ObjectiveID: obj.ID, Role: model.RoleImplementer, Agent: model.AgentClaude})

	// cloneURL is the bare repo path; base is main.
	ws, err := o.PrepareIsolatedWorkspace(context.Background(), sess.ID, "owner/repo", bare, "main")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if ws.Status != model.WorkspaceReady {
		t.Fatalf("workspace status=%s, want ready", ws.Status)
	}
	// It is a real checkout on the session branch, based on fresh upstream.
	if got := tgit(t, ws.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != ws.BranchName {
		t.Fatalf("checkout on branch %q, want %q", got, ws.BranchName)
	}
	if got := tgit(t, ws.Path, "rev-parse", "origin/main"); got != mainSha {
		t.Fatalf("origin/main=%s, want fresh %s", got, mainSha)
	}
	// The session is bound to the workspace.
	reloaded, _ := st.GetSession(sess.ID)
	if reloaded.WorkspaceID != ws.ID {
		t.Fatal("session not bound to workspace")
	}
}

// ---- manager tool-calling (MCP) ----

func mcpCall(t *testing.T, h http.Handler, sessionID, tool string, args map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	req := httptest.NewRequest("POST", "/mcp/"+sessionID, bytes.NewReader(body))
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()
	// Mount the same way main does: strip the /mcp prefix.
	http.StripPrefix("/mcp", h).ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	res, _ := out["result"].(map[string]any)
	if res == nil {
		t.Fatalf("no result: %s", rec.Body.String())
	}
	return res
}

func mcpText(res map[string]any) (string, bool) {
	isErr, _ := res["isError"].(bool)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		return "", isErr
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text, isErr
}

// The manager's tool calls, arriving over MCP, drive the orchestrator: spawning
// workers, asking the user, and marking the objective done.
func TestManagerMCP_DrivesOrchestrator(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	h := o.ManagerMCPHandler()

	obj, mgr, err := o.CreateObjective(NewObjectiveSpec{Title: "Broad work", Prompt: "do it", Agent: model.AgentClaude})
	if err != nil {
		t.Fatalf("create objective: %v", err)
	}

	// spawn_session -> a worker appears under the objective.
	res := mcpCall(t, h, mgr.ID, "spawn_session", map[string]any{
		"role": "implementer", "title": "Port module", "goal": "port it",
	})
	text, isErr := mcpText(res)
	if isErr {
		t.Fatalf("spawn_session errored: %s", text)
	}
	sessions, _ := st.ListSessionsByObjective(obj.ID)
	var worker *model.Session
	for _, s := range sessions {
		if s.Role == model.RoleImplementer {
			worker = s
		}
	}
	if worker == nil {
		t.Fatal("spawn_session did not create an implementer worker")
	}
	if worker.ParentSessionID != mgr.ID {
		t.Fatal("worker not parented to the manager")
	}

	// ask_user -> a question is opened and the objective waits.
	res = mcpCall(t, h, mgr.ID, "ask_user", map[string]any{"question": "Which DB?", "context": "need a choice"})
	if _, isErr := mcpText(res); isErr {
		t.Fatal("ask_user errored")
	}
	qs, _ := st.ListOpenQuestions()
	if len(qs) != 1 {
		t.Fatalf("expected 1 open question, got %d", len(qs))
	}
	if ro, _ := st.GetObjective(obj.ID); ro.Status != model.ObjectiveWaitingUser {
		t.Fatalf("objective should be waiting_user, got %s", ro.Status)
	}

	// create_note -> shared-memory artifact.
	res = mcpCall(t, h, mgr.ID, "create_note", map[string]any{"title": "decision", "body": "use postgres"})
	if _, isErr := mcpText(res); isErr {
		t.Fatal("create_note errored")
	}
	arts, _ := st.ListArtifactsByObjective(obj.ID)
	if len(arts) == 0 {
		t.Fatal("create_note did not record an artifact")
	}

	// mark_objective_done -> objective succeeds.
	res = mcpCall(t, h, mgr.ID, "mark_objective_done", map[string]any{"summary": "shipped"})
	if _, isErr := mcpText(res); isErr {
		t.Fatal("mark_objective_done errored")
	}
	if ro, _ := st.GetObjective(obj.ID); ro.Status != model.ObjectiveSucceeded {
		t.Fatalf("objective should be succeeded, got %s", ro.Status)
	}
}

// Dependencies passed by the manager are honored by the scheduler.
func TestManagerMCP_SpawnWithDependencies(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	h := o.ManagerMCPHandler()

	_, mgr, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p", Agent: model.AgentClaude})
	r1 := mcpCall(t, h, mgr.ID, "spawn_session", map[string]any{"role": "implementer", "title": "A", "goal": "a"})
	a, _ := mcpText(r1)
	// Extract the session id from the response text "spawned ... session <id> ...".
	var aID string
	for _, f := range strings.Fields(a) {
		if len(f) == 36 { // uuid
			aID = f
		}
	}
	if aID == "" {
		t.Fatalf("could not find session id in %q", a)
	}
	mcpCall(t, h, mgr.ID, "spawn_session", map[string]any{
		"role": "implementer", "title": "B", "goal": "b", "dependencies": []any{aID},
	})

	// Find B and confirm its declared dependency.
	sessions, _ := st.ListSessions()
	var b *model.Session
	for _, s := range sessions {
		if s.Title == "B" {
			b = s
		}
	}
	if b == nil {
		t.Fatal("B not created")
	}
	if deps := dependencyIDs(b); len(deps) != 1 || deps[0] != aID {
		t.Fatalf("B dependencies=%v, want [%s]", deps, aID)
	}
}

// ---- live tmux terminal panel ----

func TestOrch_SessionScreen_TmuxPanel(t *testing.T) {
	if _, err := osexec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewTmuxShell(model.AgentClaude))

	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionQueued}
	_ = st.CreateSession(s)
	run, err := o.StartRun(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = o.Cancel(s.ID, false); _ = run }()

	// Drive the live shell and read its screen back through the orchestrator.
	waitFor(t, func() bool {
		rs, _ := st.GetSession(s.ID)
		return rs.Status == model.SessionRunning
	})
	// Steer through the orchestrator (interactive send-keys).
	_ = o.Steer(context.Background(), s.ID, "echo PANEL-VISIBLE")

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		screen, ok, err := o.SessionScreen(s.ID)
		if err == nil && ok && strings.Contains(screen, "PANEL-VISIBLE") {
			// The attach command is also recorded on the session.
			rs, _ := st.GetSession(s.ID)
			if a, _ := rs.Metadata["tmux_attach"].(string); !strings.Contains(a, "tmux attach -t orcha-") {
				t.Fatalf("attach command not recorded: %q", a)
			}
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("orchestrator SessionScreen never showed the live panel content")
}

// A coding worker (e.g. one the manager spawned) auto-gets a fresh isolated
// checkout on its target when the objective has a repo and a preparer is set.
func TestOrch_AutoPreparesWorkspaceForWorker(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetWorkspacePreparer(workspace.New())

	// Seed a bare remote.
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	tgit(t, root, "init", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	tgit(t, root, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, seed, "add", ".")
	tgit(t, seed, "commit", "-m", "initial")
	tgit(t, seed, "remote", "add", "origin", bare)
	tgit(t, seed, "push", "-u", "origin", "main")

	tgt := &model.Target{Name: "local", Kind: model.TargetLocal, Status: model.TargetOnline,
		WorkRoot: filepath.Join(root, "work"), CapacitySessions: 2}
	_ = st.CreateTarget(tgt)

	// Objective carries the repo (clone_url overrides to the local bare path).
	_, mgr, err := o.CreateObjective(NewObjectiveSpec{
		Title: "port it", Prompt: "do it", Agent: model.AgentClaude,
		Repo: "owner/repo", CloneURL: bare,
	})
	if err != nil {
		t.Fatalf("create objective: %v", err)
	}

	// Manager spawns an implementer (no explicit workspace).
	worker, err := o.SpawnSession(mgr.ID, SpawnSpec{Role: model.RoleImplementer, Title: "w", Goal: "g"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if worker.WorkspaceID != "" {
		t.Fatal("worker should not have a workspace until it starts")
	}

	// Starting the worker auto-prepares its checkout.
	run, err := o.StartRun(context.Background(), worker.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	run.Wait()

	reloaded, _ := st.GetSession(worker.ID)
	if reloaded.WorkspaceID == "" {
		t.Fatal("worker did not get an auto-prepared workspace")
	}
	ws, _ := st.GetWorkspace(reloaded.WorkspaceID)
	if ws.Kind != model.WorkspaceIsolated || ws.Status != model.WorkspaceReady {
		t.Fatalf("workspace not ready/isolated: %+v", ws)
	}
	// It's a real checkout on the worker's branch.
	if got := tgit(t, ws.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != ws.BranchName {
		t.Fatalf("checkout branch %q != %q", got, ws.BranchName)
	}
}

// A manager session never gets an auto workspace (it works from summaries).
func TestOrch_ManagerGetsNoWorkspace(t *testing.T) {
	if !needsIsolatedWorkspace(model.RoleImplementer) || needsIsolatedWorkspace(model.RoleManager) {
		t.Fatal("role classification wrong")
	}
}

// ---- remote targets ----

func TestOrch_RegisterLocalTarget_DoctorGatesStatus(t *testing.T) {
	o, _ := newTestOrch(t)
	tgt, rep, err := o.RegisterTarget(context.Background(), &model.Target{
		Name: "box", Kind: model.TargetLocal, WorkRoot: t.TempDir(), CapacitySessions: 2,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Connectivity, git, and a writable work root are always satisfiable locally;
	// tmux and an agent CLI depend on the host. Status must agree with the doctor.
	if rep.OK != (tgt.Status == model.TargetOnline) {
		t.Fatalf("status %s disagrees with doctor.OK=%v (missing=%v)", tgt.Status, rep.OK, rep.Missing)
	}
	if tgt.LastSeenAt == nil {
		t.Fatal("last_seen_at should be stamped")
	}
	// Connectivity and git must pass locally.
	byName := map[string]Check{}
	for _, c := range rep.Checks {
		byName[c.Name] = c
	}
	if !byName["connectivity"].OK || !byName["git"].OK || !byName["workroot"].OK {
		t.Fatalf("basic local checks failed: %+v", rep.Checks)
	}
}

func TestOrch_TargetPinning_PlacesOnNamedTarget(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(blockingFake(model.AgentClaude))
	t1 := addTarget(t, st, "alpha", model.TargetLocal, 2)
	t2 := addTarget(t, st, "beta", model.TargetLocal, 2)
	_ = t1

	// Pin the session to "beta" by name.
	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionQueued,
		Metadata: model.JSONMap{"pinned_target": "beta"}}
	_ = st.CreateSession(s)

	if _, err := o.StartRun(context.Background(), s.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	reloaded, _ := st.GetSession(s.ID)
	if reloaded.TargetID != t2.ID {
		t.Fatalf("session placed on %s, want pinned target beta (%s)", reloaded.TargetID, t2.ID)
	}
	_ = o.Cancel(s.ID, false)
}

// TestLive_RemoteTmux runs a real tmux session on a remote SSH host.
// Gated behind ORCHA_SSH_TEST_HOST (Remote Login enabled / a reachable box).
func TestLive_RemoteTmux(t *testing.T) {
	host := os.Getenv("ORCHA_SSH_TEST_HOST")
	if host == "" {
		t.Skip("set ORCHA_SSH_TEST_HOST to run the live remote-tmux test")
	}
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewTmuxShell(model.AgentClaude))

	tgt, rep, err := o.RegisterTarget(context.Background(), &model.Target{
		Name: "remote", Kind: model.TargetSSH, Host: host, User: os.Getenv("ORCHA_SSH_TEST_USER"),
		WorkRoot: "/tmp/orcha-work", CapacitySessions: 2,
	})
	if err != nil {
		t.Fatalf("register remote: %v", err)
	}
	if tgt.Status != model.TargetOnline {
		t.Fatalf("remote target offline (doctor missing %v): %+v", rep.Missing, tgt)
	}

	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionQueued,
		Metadata: model.JSONMap{"pinned_target": "remote"}}
	_ = st.CreateSession(s)
	run, err := o.StartRun(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = o.Cancel(s.ID, false); _ = run }()

	waitFor(t, func() bool { rs, _ := st.GetSession(s.ID); return rs.Status == model.SessionRunning })
	_ = o.Steer(context.Background(), s.ID, "echo REMOTE-TMUX-OK")
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		screen, ok, _ := o.SessionScreen(s.ID)
		if ok && strings.Contains(screen, "REMOTE-TMUX-OK") {
			rs, _ := st.GetSession(s.ID)
			t.Logf("remote tmux attach: %v", rs.Metadata["tmux_attach"])
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("did not see output from remote tmux pane")
}

// ---- autonomy: worker completion notifies the manager ----

func TestOrch_WorkerCompletionNotifiesManager(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)

	managerGot := make(chan string, 4)
	script := func(ctx context.Context, spec agent.Spec, inputs <-chan string, out chan<- agent.Event) {
		if spec.Role == model.RoleManager {
			out <- agent.Event{Kind: agent.EventText, Source: model.MsgAgent, Content: "manager ready"}
			for {
				select {
				case <-ctx.Done():
					return
				case m := <-inputs:
					managerGot <- m
				}
			}
		}
		// worker: do the task and finish.
		out <- agent.Event{Kind: agent.EventText, Source: model.MsgAgent, Content: "worker did the thing"}
		out <- agent.Event{Kind: agent.EventDone, Success: true}
	}
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, script))

	_, mgr, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "do it", Agent: model.AgentClaude})
	if _, err := o.StartRun(context.Background(), mgr.ID); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	waitFor(t, func() bool { m, _ := st.GetSession(mgr.ID); return m.Status == model.SessionRunning })

	// Manager spawns a worker; it should be one-shot (noninteractive).
	worker, err := o.SpawnSession(mgr.ID, SpawnSpec{Role: model.RoleImplementer, Title: "do X", Goal: "g"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if worker.Mode != model.ModeNoninteractive {
		t.Fatalf("coding worker should be one-shot, got mode %s", worker.Mode)
	}

	run, err := o.StartRun(context.Background(), worker.ID)
	if err != nil {
		t.Fatalf("start worker: %v", err)
	}
	run.Wait() // worker completes -> finishRun -> notifies manager

	select {
	case msg := <-managerGot:
		if !strings.Contains(msg, "publish_pr") || !strings.Contains(msg, worker.ID) {
			t.Fatalf("manager notification missing publish guidance / worker id: %q", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("manager was not notified of worker completion")
	}
	_ = o.Cancel(mgr.ID, true)
}

func TestPublishPR_CommitsUncommittedChanges(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 2)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	f := forge.NewFake()
	o.SetForge(f)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	s, _ := o.CreateSession(SpawnSpec{ObjectiveID: obj.ID, Role: model.RoleImplementer, Agent: model.AgentClaude})
	ws := &model.Workspace{ObjectiveID: obj.ID, SessionID: s.ID, TargetID: "t", Kind: model.WorkspaceIsolated,
		ProjectPath: "owner/repo", VCS: model.VCSGit, Path: "/w/x", BranchName: "orcha/x", Status: model.WorkspaceReady}
	_ = st.CreateWorkspace(ws)
	_, _ = st.UpdateSessionRuntime(s.ID, func(se *model.Session) { se.WorkspaceID = ws.ID })

	if _, err := o.PublishPR(context.Background(), s.ID, PublishSpec{Title: "add health", Body: "b", CommitMessage: "feat: health"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(f.Commits) != 1 || f.Commits[0].Message != "feat: health" {
		t.Fatalf("expected a commit before push, got %+v", f.Commits)
	}
	if len(f.Pushes) != 1 {
		t.Fatalf("expected one push, got %d", len(f.Pushes))
	}
}

// ---- PR feedback: sync spawns a follow-up; the agent responds via tools ----

func TestSyncPRFeedback_SpawnsFollowupForUserComment(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	f := forge.NewFake()
	o.SetForge(f)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "owner/repo", Number: 7, Branch: "feature",
		BaseBranch: "main", HeadSHA: "sha1", Status: model.PROpen}
	_ = st.CreatePR(pr)

	// A user review comment, plus an orcha bot comment that must be ignored.
	f.SetComments(
		forge.Comment{ExternalID: "c1", Author: "human", Body: "Please rename Foo to Bar.", Kind: "issue_comment"},
		forge.Comment{ExternalID: "c2", Author: "human", Body: "looks good " + "<!-- orcha-bot -->", Kind: "issue_comment"},
	)

	spawned, err := o.SyncPRFeedback(context.Background(), pr.ID)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(spawned) != 1 {
		t.Fatalf("expected 1 follow-up for the user comment (bot comment ignored), got %d", len(spawned))
	}
	fu := spawned[0]
	if fu.Role != model.RolePRFollowup {
		t.Fatalf("expected pr_followup, got %s", fu.Role)
	}
	if fu.Mode != model.ModeNoninteractive {
		t.Fatalf("follow-up should be one-shot, got %s", fu.Mode)
	}
	if fu.Metadata["pr_id"] != pr.ID {
		t.Fatal("follow-up not attached to the PR")
	}

	// Re-syncing the same comments spawns nothing new (deduped + bot-skipped).
	again, _ := o.SyncPRFeedback(context.Background(), pr.ID)
	if len(again) != 0 {
		t.Fatalf("re-sync should not respawn, got %d", len(again))
	}
}

// hermeticGit makes git invocations during this test — including ones made by
// the code under test, which inherits the process env — ignore the developer's
// global/system git config. Commit signing in particular (e.g. via the
// 1Password SSH agent) hangs tests on an authorization prompt or fails them
// when the agent is locked.
func hermeticGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// A question is open only while its asking session can receive the answer:
// terminal transitions close the session's open questions, and the startup
// sweep heals rows written before that invariant existed.
func TestQuestionsCloseWithTheirSession(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))

	obj := &model.Objective{Title: "o", Prompt: "p", Status: model.ObjectiveActive}
	_ = st.CreateObjective(obj)
	s := &model.Session{ObjectiveID: obj.ID, Role: model.RoleManager, Agent: model.AgentClaude, Status: model.SessionQueued, Goal: "g"}
	_ = st.CreateSession(s)
	q := &model.Question{ObjectiveID: obj.ID, SessionID: s.ID, Question: "which repo?"}
	_ = st.CreateQuestion(q)

	if err := o.Cancel(s.ID, false); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, _ := st.GetQuestion(q.ID)
	if got.Status != model.QuestionCanceled {
		t.Fatalf("question after session cancel = %s, want canceled", got.Status)
	}

	// Historical stale row: open question on an already-terminal objective.
	obj2 := &model.Objective{Title: "o2", Prompt: "p", Status: model.ObjectiveActive}
	_ = st.CreateObjective(obj2)
	q2 := &model.Question{ObjectiveID: obj2.ID, Question: "stale?"}
	_ = st.CreateQuestion(q2)
	_ = st.UpdateObjectiveStatus(obj2.ID, model.ObjectiveCanceled, "")
	if n, err := st.SweepStaleQuestions(); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want 1 closed", n, err)
	}
	got2, _ := st.GetQuestion(q2.ID)
	if got2.Status != model.QuestionCanceled {
		t.Fatalf("stale question after sweep = %s, want canceled", got2.Status)
	}
}

// The manager's prompt must carry the objective's repo facts: the repo lives
// in objective metadata for workspace prep, and a manager that can't see it
// asks the user for a repo the objective already names.
func TestManagerPromptCarriesObjectiveRepo(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.ManagerMCPBaseURL = "http://127.0.0.1:0"

	obj, mgr, err := o.CreateObjective(NewObjectiveSpec{
		Title: "t", Prompt: "upgrade the ui", Agent: model.AgentClaude,
		Repo: "nathanwhit/orcha", PushRepo: "fork/orcha", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = obj
	sess, _ := st.GetSession(mgr.ID)
	spec := o.buildSpec(sess, nil, nil)
	for _, want := range []string{"Objective repo: nathanwhit/orcha", "base main", "fork/orcha"} {
		if !strings.Contains(spec.Prompt, want) {
			t.Fatalf("manager prompt missing %q:\n%s", want, spec.Prompt)
		}
	}

	// Repo-less objective: registered projects are offered instead.
	_ = st.UpsertProject(&model.Project{Name: "orcha", Repo: "nathanwhit/orcha", BaseBranch: "main"})
	_, mgr2, err := o.CreateObjective(NewObjectiveSpec{Title: "t2", Prompt: "p", Agent: model.AgentClaude})
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	sess2, _ := st.GetSession(mgr2.ID)
	spec2 := o.buildSpec(sess2, nil, nil)
	if !strings.Contains(spec2.Prompt, "Registered projects:") ||
		!strings.Contains(spec2.Prompt, "nathanwhit/orcha") {
		t.Fatalf("repo-less manager prompt missing project hints:\n%s", spec2.Prompt)
	}
}

// A coding worker's prompt points it at the objective's shared scratch dir, so
// task artifacts (harnesses, repro scripts) have a home that survives its
// torn-down checkout without polluting the PR. The note is omitted with no
// target (offline/test path), leaving the prompt unchanged.
func TestBuildSpec_InjectsSharedScratch(t *testing.T) {
	o, _ := newTestOrch(t)
	tgt := &model.Target{ID: "tg1", WorkRoot: "/home/bot/work"}
	wantPath := "/home/bot/work/scratch/obj1"

	for _, role := range []model.SessionRole{model.RoleImplementer, model.RolePRFollowup} {
		sess := &model.Session{ID: "s1", ObjectiveID: "obj1", Role: role, Goal: "do the thing"}
		spec := o.buildSpec(sess, nil, tgt)
		if !strings.Contains(spec.Prompt, wantPath) {
			t.Fatalf("%s prompt missing shared scratch path %q:\n%s", role, wantPath, spec.Prompt)
		}
		if !strings.Contains(spec.Prompt, "SHARED SCRATCH") {
			t.Fatalf("%s prompt missing scratch guidance", role)
		}
	}

	// No placed target → no scratch note.
	sess := &model.Session{ID: "s2", ObjectiveID: "obj1", Role: model.RoleImplementer, Goal: "do the thing"}
	if got := o.buildSpec(sess, nil, nil); strings.Contains(got.Prompt, "SHARED SCRATCH") {
		t.Fatalf("scratch note should be absent without a target:\n%s", got.Prompt)
	}
}

// EnsureSharedScratch is idempotent: one shared workspace per objective+target,
// returned again (not duplicated) on a second call, at the deterministic path.
func TestEnsureSharedScratch_Idempotent(t *testing.T) {
	o, st := newTestOrch(t)
	tgt := addTarget(t, st, "local", model.TargetLocal, 4)

	a, err := o.EnsureSharedScratch(context.Background(), "obj1", tgt)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	b, err := o.EnsureSharedScratch(context.Background(), "obj1", tgt)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if a.ID != b.ID {
		t.Fatalf("expected the same shared scratch workspace, got %s vs %s", a.ID, b.ID)
	}
	if want := tgt.WorkRoot + "/scratch/obj1"; a.Path != want {
		t.Fatalf("scratch path = %q, want %q", a.Path, want)
	}
	if a.Kind != model.WorkspaceShared {
		t.Fatalf("scratch kind = %q, want %q", a.Kind, model.WorkspaceShared)
	}
}

// Finishing an objective reaps its shared scratch (archives the row; the dir is
// rm -rf'd on the target when a preparer is installed) so WorkRoot/scratch does
// not grow without bound. Isolated checkouts are left to their own lifecycle.
func TestReapSharedScratch_OnDone(t *testing.T) {
	o, st := newTestOrch(t)
	tgt := addTarget(t, st, "local", model.TargetLocal, 4)

	scratch, err := o.EnsureSharedScratch(context.Background(), "obj1", tgt)
	if err != nil {
		t.Fatalf("ensure scratch: %v", err)
	}
	iso := &model.Workspace{
		ObjectiveID: "obj1", TargetID: tgt.ID, Kind: model.WorkspaceIsolated,
		Path: tgt.WorkRoot + "/sess1", Status: model.WorkspaceReady,
	}
	if err := st.CreateWorkspace(iso); err != nil {
		t.Fatalf("create isolated ws: %v", err)
	}

	o.reapSharedScratch(context.Background(), "obj1")

	if got, _ := st.GetWorkspace(scratch.ID); got.Status != model.WorkspaceArchived {
		t.Fatalf("shared scratch status = %q, want archived", got.Status)
	}
	if got, _ := st.GetWorkspace(iso.ID); got.Status != model.WorkspaceReady {
		t.Fatalf("isolated workspace should be untouched, status = %q", got.Status)
	}
}

// A manager whose objective names a repo runs in its own fresh checkout —
// grounded managers scope work from the code instead of asking about it.
func TestManagerGetsCheckout(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetWorkspacePreparer(workspace.New())

	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	tgit(t, root, "init", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	tgit(t, root, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "code.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, seed, "add", ".")
	tgit(t, seed, "commit", "-m", "initial")
	tgit(t, seed, "remote", "add", "origin", bare)
	tgit(t, seed, "push", "-u", "origin", "main")

	tgt := &model.Target{Name: "local", Kind: model.TargetLocal, Status: model.TargetOnline,
		WorkRoot: filepath.Join(root, "work"), CapacitySessions: 2}
	if err := st.CreateTarget(tgt); err != nil {
		t.Fatalf("target: %v", err)
	}

	_, mgr, err := o.CreateObjective(NewObjectiveSpec{
		Title: "t", Prompt: "p", Agent: model.AgentClaude, Repo: "owner/x", CloneURL: bare,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := o.StartRun(context.Background(), mgr.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	sess, _ := st.GetSession(mgr.ID)
	if sess.WorkspaceID == "" {
		t.Fatal("manager has no workspace")
	}
	ws, err := st.GetWorkspace(sess.WorkspaceID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "code.go")); err != nil {
		t.Fatalf("manager checkout missing repo content: %v", err)
	}
}

func TestForgeFor_BindsToTargetExecutor(t *testing.T) {
	o, st := newTestOrch(t)
	o.SetForge(forge.NewGit())
	// A retargetable forge runs its git/gh on the workspace's machine.
	ssh := addTarget(t, st, "vultr", model.TargetSSH, 4)
	gf, ok := o.forgeFor(ssh).(*forge.GitForge)
	if !ok || gf.Exec == nil {
		t.Fatalf("forgeFor(ssh) should bind an executor, got %T", o.forgeFor(ssh))
	}
	if d := gf.Exec.Describe(); !strings.Contains(d, "ssh") {
		t.Fatalf("ssh target should bind an ssh executor, got %q", d)
	}
	// The Fake forge is not retargetable — returned unchanged.
	o.SetForge(forge.NewFake())
	if _, isGit := o.forgeFor(ssh).(*forge.GitForge); isGit {
		t.Fatal("fake forge should not become a GitForge")
	}
}

func TestManagerNotify_WaitsForPendingDependents(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	obj, mgr, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	_, _ = st.UpdateSessionStatus(mgr.ID, model.SessionRunning)

	impl := &model.Session{ObjectiveID: obj.ID, ParentSessionID: mgr.ID, Role: model.RoleImplementer,
		Agent: model.AgentClaude, Status: model.SessionSucceeded, Title: "impl"}
	_ = st.CreateSession(impl)
	val := &model.Session{ObjectiveID: obj.ID, ParentSessionID: mgr.ID, Role: model.RoleValidator,
		Agent: model.AgentClaude, Status: model.SessionQueued, Title: "validate",
		Metadata: model.JSONMap{"depends_on": []any{impl.ID}}}
	_ = st.CreateSession(val)

	lastMgrMsg := func() string {
		msgs, _ := st.MessagesAfter(mgr.ID, 0, 100)
		last := ""
		for _, m := range msgs {
			if m.Source == model.MsgUser {
				last = m.Content
			}
		}
		return last
	}

	// Implementer done but its dependent validator is still pending -> wait.
	if !o.hasPendingDependents(obj.ID, impl.ID) {
		t.Fatal("a queued validator depending on the implementer should count as pending")
	}
	o.notifyManagerOfChild(impl.ID, true)
	if msg := lastMgrMsg(); !strings.Contains(msg, "do NOT publish") {
		t.Fatalf("with a pending dependent, manager should be told to wait; got %q", msg)
	}

	// Validator finishes -> the slice is complete; the publish branch is taken.
	for _, s := range []model.SessionStatus{model.SessionStarting, model.SessionRunning, model.SessionSucceeded} {
		if _, err := st.UpdateSessionStatus(val.ID, s); err != nil {
			t.Fatalf("advance validator to %s: %v", s, err)
		}
	}
	if o.hasPendingDependents(obj.ID, impl.ID) {
		t.Fatal("no dependents should be pending once the validator succeeded")
	}
}

func TestMarkObjectiveDone_GatedOnOpenPRs(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetForge(forge.NewFake())
	obj, mgr, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})

	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "o/r", Number: 5, Branch: "b", Status: model.PROpen}
	_ = st.CreatePR(pr)

	// An open (unmerged) PR blocks completion.
	if err := o.MarkObjectiveDone(mgr.ID, "done"); err == nil {
		t.Fatal("mark_objective_done should be refused while a PR is open")
	}
	if cur, _ := st.GetObjective(obj.ID); cur.Status == model.ObjectiveSucceeded {
		t.Fatal("objective must not be succeeded while a PR is open")
	}

	// Once the PR merges, completion is allowed.
	_, _ = st.UpdatePR(pr.ID, func(p *model.PullRequest) { p.Status = model.PRMerged })
	if err := o.MarkObjectiveDone(mgr.ID, "done"); err != nil {
		t.Fatalf("mark_objective_done should succeed once the PR merged: %v", err)
	}
	if cur, _ := st.GetObjective(obj.ID); cur.Status != model.ObjectiveSucceeded {
		t.Fatalf("objective should be succeeded once PRs merged, got %s", cur.Status)
	}
}

func TestConflictingPR_SpawnsRebaseFollowup(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	f := forge.NewFake()
	o.SetForge(f)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 9, Branch: "orcha/impl-x",
		BaseBranch: "main", HeadSHA: "sha0", Status: model.PROpen}
	_ = st.CreatePR(pr)

	// GitHub reports the open PR as conflicting.
	f.SetPRState("octo/repo", 9, forge.PRState{Number: 9, Status: "open", HeadSHA: "sha1", Mergeable: "CONFLICTING"})

	if _, err := o.RefreshPR(context.Background(), pr.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	spawned, err := o.ProcessFeedback(context.Background(), pr.ID)
	if err != nil {
		t.Fatalf("process feedback: %v", err)
	}
	if len(spawned) != 1 {
		t.Fatalf("a conflicting PR should spawn one rebase follow-up, got %d", len(spawned))
	}
	if spawned[0].Role != model.RolePRFollowup || !strings.Contains(spawned[0].Goal, "merge conflicts") {
		t.Fatalf("follow-up should be a PR follow-up tasked to rebase, got role=%s goal=%q", spawned[0].Role, spawned[0].Goal)
	}

	// While that follow-up is still active, re-observing the conflict does NOT
	// dispatch a duplicate.
	_, _ = o.RefreshPR(context.Background(), pr.ID)
	again, _ := o.ProcessFeedback(context.Background(), pr.ID)
	if len(again) != 0 {
		t.Fatalf("a duplicate follow-up must not spawn while one is active, got %d", len(again))
	}

	// The follow-up finishes WITHOUT resolving it (e.g. it couldn't push) and the
	// PR is still conflicting -> a retry is dispatched.
	for _, s := range []model.SessionStatus{model.SessionStarting, model.SessionRunning, model.SessionFailed} {
		_, _ = st.UpdateSessionStatus(spawned[0].ID, s)
	}
	_, _ = o.RefreshPR(context.Background(), pr.ID)
	retry, _ := o.ProcessFeedback(context.Background(), pr.ID)
	if len(retry) != 1 {
		t.Fatalf("a finished-but-unresolved conflict should re-dispatch, got %d", len(retry))
	}
}

func TestSupervisor_RepokesIdleManagerOnlyWhenNoWorkers(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))

	obj, mgr, err := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	if err != nil {
		t.Fatalf("create objective: %v", err)
	}

	// A freshly-active manager (UpdatedAt just now) is NOT yet considered stalled.
	if acts := o.superviseDecisions(st.Now()); len(acts) != 0 {
		t.Fatalf("a just-active manager should not be acted on, got %d", len(acts))
	}

	// Once it has been quiet past the idle threshold and no worker is running,
	// the idle manager is poked.
	future := st.Now().Add(superviseIdleAfter + time.Minute)
	acts := o.superviseDecisions(future)
	if len(acts) != 1 || acts[0].kind != "poke" || acts[0].manager.ID != mgr.ID {
		t.Fatalf("expected idle manager %s to be poked, got %+v", mgr.ID, acts)
	}

	// Cooldown: an immediate re-check does not act again.
	if again := o.superviseDecisions(future); len(again) != 0 {
		t.Fatalf("cooldown should suppress a second poke, got %d", len(again))
	}
	// After the cooldown elapses, an objective still idle is poked again.
	later := future.Add(supervisePokeCooldown + time.Second)
	if again := o.superviseDecisions(later); len(again) != 1 || again[0].kind != "poke" {
		t.Fatalf("after cooldown, a still-idle manager should be poked again, got %+v", again)
	}

	// With an active worker, the objective is making progress -> never acted on,
	// even long after the manager went quiet.
	w := &model.Session{ObjectiveID: obj.ID, Role: model.RoleImplementer,
		Agent: model.AgentClaude, Status: model.SessionRunning}
	if err := st.CreateSession(w); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	evenLater := later.Add(supervisePokeCooldown + time.Second)
	if again := o.superviseDecisions(evenLater); len(again) != 0 {
		t.Fatalf("an objective with an active worker must not be acted on, got %d", len(again))
	}
}

// An idle manager whose only outstanding work is a healthy open PR is waiting on
// a human to merge — it must NOT be poked (that just makes it re-ask "can you
// merge?"). But an open PR that is failing CI or carries unhandled feedback is
// actionable, so the poke still fires.
func TestSupervisor_DoesNotPokeWhenAwaitingHumanMerge(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))

	obj, mgr, err := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	if err != nil {
		t.Fatalf("create objective: %v", err)
	}
	future := st.Now().Add(superviseIdleAfter + time.Minute)

	// Sanity: with no PR, an idle manager IS poked (latent work to drive).
	if acts := o.superviseDecisions(future); len(acts) != 1 || acts[0].kind != "poke" {
		t.Fatalf("expected a poke with no PR, got %+v", acts)
	}

	// A healthy open PR awaiting merge suppresses the poke.
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 12, Branch: "feature",
		BaseBranch: "main", HeadSHA: "sha1", Status: model.PROpen, ChecksState: model.ChecksPassing}
	_ = st.CreatePR(pr)
	cooled := future.Add(supervisePokeCooldown + time.Second)
	if acts := o.superviseDecisions(cooled); len(acts) != 0 {
		t.Fatalf("idle manager awaiting a human merge must not be poked, got %+v", acts)
	}

	// Even unhandled feedback on the PR does NOT resume the timer-poke: the
	// follow-up pipeline (ProcessFeedback) owns that work, and poking the manager
	// in the gap before the follow-up spawns just yields a redundant "can you
	// merge?" question. The manager is re-engaged by PR events, not the timer.
	if err := o.IngestFeedback(context.Background(), pr.ID, []model.PRFeedback{
		{Kind: model.FeedbackReviewComment, ExternalID: "c-1", Body: "please rename", Actionable: true},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	cooled2 := cooled.Add(supervisePokeCooldown + time.Second)
	if acts := o.superviseDecisions(cooled2); len(acts) != 0 {
		t.Fatalf("an open PR must suppress the poke regardless of feedback, got %+v", acts)
	}

	// Once the PR merges there is no open PR left, so a still-active objective is
	// pokeable again (latent unfinished work with nothing else driving it).
	if _, err := st.UpdatePR(pr.ID, func(p *model.PullRequest) { p.Status = model.PRMerged }); err != nil {
		t.Fatalf("merge pr: %v", err)
	}
	cooled3 := cooled2.Add(supervisePokeCooldown + time.Second)
	if acts := o.superviseDecisions(cooled3); len(acts) != 1 || acts[0].kind != "poke" || acts[0].manager.ID != mgr.ID {
		t.Fatalf("with the PR merged and the objective still active, the manager should be pokeable, got %+v", acts)
	}
}

func TestSupervisor_RespawnsManagerThenEscalates(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))

	obj, mgr, err := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "do the thing"})
	if err != nil {
		t.Fatalf("create objective: %v", err)
	}

	// The manager session terminated, the objective is still active, and no worker
	// is running -> nothing can drive it. The supervisor decides to respawn.
	terminateSession(t, st, mgr.ID, model.SessionSucceeded)
	acts := o.superviseDecisions(st.Now())
	if len(acts) != 1 || acts[0].kind != "respawn" {
		t.Fatalf("a manager-less active objective should respawn, got %+v", acts)
	}

	// Executing it brings up a fresh live manager carrying the resume context.
	o.respawnManager(acts[0])
	live := o.activeManagerFor(obj.ID)
	if live == nil {
		t.Fatal("respawn should create a live manager")
	}
	if live.Title != "Manager (resumed)" || !strings.Contains(live.Goal, "do the thing") {
		t.Fatalf("resumed manager missing prompt/title: title=%q", live.Title)
	}

	// Burn through the manager budget: terminate the respawn and add managers until
	// the cap is reached, all terminal so none is live.
	terminateSession(t, st, live.ID, model.SessionFailed)
	for o.countManagers(obj.ID) < maxManagerSessions {
		m, mErr := o.CreateSession(SpawnSpec{ObjectiveID: obj.ID, Role: model.RoleManager,
			Agent: model.AgentClaude, Mode: model.ModeInteractive, Title: "Manager"})
		if mErr != nil {
			t.Fatalf("seed manager: %v", mErr)
		}
		terminateSession(t, st, m.ID, model.SessionFailed)
	}

	// Past the cooldown, with the budget exhausted, it escalates instead of
	// respawning forever.
	future := st.Now().Add(supervisePokeCooldown + time.Minute)
	acts = o.superviseDecisions(future)
	if len(acts) != 1 || acts[0].kind != "escalate" {
		t.Fatalf("an exhausted manager budget should escalate, got %+v", acts)
	}
	o.escalateManagerDeaths(obj.ID)
	if qs, _ := st.ListQuestionsByObjective(obj.ID); len(qs) == 0 {
		t.Fatal("escalation should record an open question for the objective")
	}
}

// terminateSession drives a (queued) session through the legal lifecycle to a
// terminal state: queued -> starting -> running -> final.
func terminateSession(t *testing.T, st *store.Store, id string, final model.SessionStatus) {
	t.Helper()
	for _, s := range []model.SessionStatus{model.SessionStarting, model.SessionRunning, final} {
		if _, err := st.UpdateSessionStatus(id, s); err != nil {
			t.Fatalf("transition %s -> %s: %v", id, s, err)
		}
	}
}

func TestReclaimWorkspaces(t *testing.T) {
	o, st := newTestOrch(t)
	tgt := addTarget(t, st, "local", model.TargetLocal, 4)

	mkdir := func(name string) string {
		d := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	newWS := func(objID, path string) *model.Workspace {
		ws := &model.Workspace{ObjectiveID: objID, TargetID: tgt.ID,
			Kind: model.WorkspaceIsolated, Path: path, Status: model.WorkspaceReady}
		if err := st.CreateWorkspace(ws); err != nil {
			t.Fatal(err)
		}
		return ws
	}

	// Terminal objective with no live sessions -> its checkout is reclaimed.
	doneObj := &model.Objective{Title: "done", Prompt: "p", Status: model.ObjectiveSucceeded}
	_ = st.CreateObjective(doneObj)
	donePath := mkdir("done")
	doneWS := newWS(doneObj.ID, donePath)

	// Active objective -> left alone (it may still publish/inherit the checkout).
	activeObj := &model.Objective{Title: "active", Prompt: "p", Status: model.ObjectiveActive}
	_ = st.CreateObjective(activeObj)
	activePath := mkdir("active")
	activeWS := newWS(activeObj.ID, activePath)

	// Terminal objective but a non-terminal session still references the
	// workspace -> left alone (safety against the sharing hazard).
	busyObj := &model.Objective{Title: "busy", Prompt: "p", Status: model.ObjectiveSucceeded}
	_ = st.CreateObjective(busyObj)
	busyPath := mkdir("busy")
	busyWS := newWS(busyObj.ID, busyPath)
	if err := st.CreateSession(&model.Session{ObjectiveID: busyObj.ID, Role: model.RoleImplementer,
		Agent: model.AgentClaude, Status: model.SessionRunning, WorkspaceID: busyWS.ID}); err != nil {
		t.Fatal(err)
	}

	o.ReclaimWorkspaces(context.Background())

	// Reclaimed: dir gone, status archived.
	if _, err := os.Stat(donePath); !os.IsNotExist(err) {
		t.Fatalf("terminal objective's checkout should be removed, stat err=%v", err)
	}
	if ws, _ := st.GetWorkspace(doneWS.ID); ws.Status != model.WorkspaceArchived {
		t.Fatalf("reclaimed workspace should be archived, got %s", ws.Status)
	}
	// Kept: active objective.
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active objective's checkout must be kept, stat err=%v", err)
	}
	if ws, _ := st.GetWorkspace(activeWS.ID); ws.Status != model.WorkspaceReady {
		t.Fatalf("active workspace must stay ready, got %s", ws.Status)
	}
	// Kept: in-use by a live session.
	if _, err := os.Stat(busyPath); err != nil {
		t.Fatalf("in-use checkout must be kept, stat err=%v", err)
	}
	if ws, _ := st.GetWorkspace(busyWS.ID); ws.Status != model.WorkspaceReady {
		t.Fatalf("in-use workspace must stay ready, got %s", ws.Status)
	}
}

func TestFailingChecksPR_SpawnsCIFollowup(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	f := forge.NewFake()
	o.SetForge(f)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 9, Branch: "orcha/impl-x",
		BaseBranch: "main", HeadSHA: "sha0", Status: model.PROpen}
	_ = st.CreatePR(pr)

	// GitHub reports the open PR's checks as failing.
	f.SetPRState("octo/repo", 9, forge.PRState{Number: 9, Status: "open", HeadSHA: "sha1", ChecksState: "failing"})

	if _, err := o.RefreshPR(context.Background(), pr.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	spawned, err := o.ProcessFeedback(context.Background(), pr.ID)
	if err != nil {
		t.Fatalf("process feedback: %v", err)
	}
	if len(spawned) != 1 {
		t.Fatalf("a failing-CI PR should spawn one CI follow-up, got %d", len(spawned))
	}
	if spawned[0].Role != model.RoleCIFollowup || !strings.Contains(spawned[0].Goal, "checks are failing") {
		t.Fatalf("follow-up should be a CI follow-up tasked to fix checks, got role=%s goal=%q", spawned[0].Role, spawned[0].Goal)
	}

	// While that follow-up is still active, re-observing the failure does NOT
	// dispatch a duplicate.
	_, _ = o.RefreshPR(context.Background(), pr.ID)
	again, _ := o.ProcessFeedback(context.Background(), pr.ID)
	if len(again) != 0 {
		t.Fatalf("a duplicate follow-up must not spawn while one is active, got %d", len(again))
	}

	// The follow-up pushes a new head that is still red -> a fresh failure
	// (new head SHA) re-dispatches.
	for _, s := range []model.SessionStatus{model.SessionStarting, model.SessionRunning, model.SessionSucceeded} {
		_, _ = st.UpdateSessionStatus(spawned[0].ID, s)
	}
	f.SetPRState("octo/repo", 9, forge.PRState{Number: 9, Status: "open", HeadSHA: "sha2", ChecksState: "failing"})
	_, _ = o.RefreshPR(context.Background(), pr.ID)
	retry, _ := o.ProcessFeedback(context.Background(), pr.ID)
	if len(retry) != 1 {
		t.Fatalf("a new failing head should re-dispatch a CI follow-up, got %d", len(retry))
	}
}

func TestUpdatePR_ForcePushViaMCP(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	f := forge.NewFake()
	o.SetForge(f)
	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})

	ws := &model.Workspace{ObjectiveID: obj.ID, Kind: model.WorkspacePRBranch, ProjectPath: "octo/repo",
		VCS: model.VCSGit, BranchName: "orcha/impl-x", Path: "/tmp/pr-3", Status: model.WorkspaceReady}
	_ = st.CreateWorkspace(ws)
	fu := &model.Session{ObjectiveID: obj.ID, Role: model.RolePRFollowup, Agent: model.AgentClaude,
		Status: model.SessionRunning, WorkspaceID: ws.ID}
	_ = st.CreateSession(fu)
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 3, Branch: "orcha/impl-x",
		BaseBranch: "main", Status: model.PROpen}
	_ = st.CreatePR(pr)

	// A rebase follow-up force-pushes its rewritten branch via update_pr.
	ctx := mcp.WithSession(context.Background(), fu.ID)
	if _, err := o.mcpUpdatePR(ctx, map[string]any{"pr_id": pr.ID, "force": true}); err != nil {
		t.Fatalf("update_pr force: %v", err)
	}
	if len(f.ForcePush) == 0 {
		t.Fatal("update_pr with force=true should have force-pushed the branch")
	}
}

// recordUsage must increment the session's running token total while still
// crediting the provider usage bucket — the per-session counter is additive,
// not a replacement for the scheduling buckets.
func TestRecordUsage_IncrementsSessionAndProviderBucket(t *testing.T) {
	o, st := newTestOrch(t)
	s := &model.Session{Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionRunning}
	if err := st.CreateSession(s); err != nil {
		t.Fatalf("create session: %v", err)
	}

	o.recordUsage(s.ID, 120)

	// Session total reflects the usage.
	got, err := st.SessionUsedTokens(s.ID)
	if err != nil {
		t.Fatalf("session tokens: %v", err)
	}
	if got != 120 {
		t.Fatalf("session used=%d, want 120", got)
	}

	// Provider bucket behavior is unchanged: the claude bucket still got 120.
	buckets, _ := st.ListUsage()
	var providerTotal int64
	for _, b := range buckets {
		if b.Provider == string(model.AgentClaude) {
			providerTotal += b.UsedTokens
		}
	}
	if providerTotal != 120 {
		t.Fatalf("provider bucket=%d, want 120", providerTotal)
	}

	// A second call accumulates on both counters.
	o.recordUsage(s.ID, 30)
	if got, _ := st.SessionUsedTokens(s.ID); got != 150 {
		t.Fatalf("session used=%d after second call, want 150", got)
	}

	// Non-positive token counts are ignored on both counters.
	o.recordUsage(s.ID, 0)
	o.recordUsage(s.ID, -5)
	if got, _ := st.SessionUsedTokens(s.ID); got != 150 {
		t.Fatalf("session used=%d after no-op calls, want 150", got)
	}
}

// ---- PR follow-up wiring (manager-spawned follow-ups & id resolution) ----

// update_pr/comment_pr must accept the GitHub PR number, not just the internal
// Orcha pr_id — agents naturally pass the number they see on GitHub. Regression
// for "store: not found" when a follow-up passed pr_id="17".
func TestUpdatePR_AcceptsGitHubNumber(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	f := forge.NewFake()
	o.SetForge(f)
	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})

	ws := &model.Workspace{ObjectiveID: obj.ID, Kind: model.WorkspacePRBranch, ProjectPath: "octo/repo",
		VCS: model.VCSGit, BranchName: "orcha/impl-x", Path: "/tmp/pr-7", Status: model.WorkspaceReady}
	_ = st.CreateWorkspace(ws)
	fu := &model.Session{ObjectiveID: obj.ID, Role: model.RolePRFollowup, Agent: model.AgentClaude,
		Status: model.SessionRunning, WorkspaceID: ws.ID}
	_ = st.CreateSession(fu)
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 7, Branch: "orcha/impl-x",
		BaseBranch: "main", Status: model.PROpen}
	_ = st.CreatePR(pr)

	// Pass the GitHub number, not the UUID.
	ctx := mcp.WithSession(context.Background(), fu.ID)
	if _, err := o.mcpUpdatePR(ctx, map[string]any{"pr_id": "7"}); err != nil {
		t.Fatalf("update_pr by GitHub number: %v", err)
	}
	if len(f.Pushes) == 0 {
		t.Fatal("update_pr by GitHub number should have pushed the branch")
	}
}

// A bare unknown number returns a helpful error listing the objective's PRs.
func TestResolvePR_UnknownNumberListsPRs(t *testing.T) {
	o, st := newTestOrch(t)
	o.SetForge(forge.NewFake())
	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	mgr := &model.Session{ObjectiveID: obj.ID, Role: model.RoleManager, Status: model.SessionRunning}
	_ = st.CreateSession(mgr)
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 7, Branch: "b", BaseBranch: "main", Status: model.PROpen}
	_ = st.CreatePR(pr)

	ctx := mcp.WithSession(context.Background(), mgr.ID)
	_, err := o.resolvePR(ctx, "999")
	if err == nil {
		t.Fatal("unknown number should error")
	}
	if !strings.Contains(err.Error(), "#7") || !strings.Contains(err.Error(), pr.ID) {
		t.Fatalf("error should list the objective's PRs, got: %v", err)
	}
}

// spawn_session must refuse PR/CI follow-up roles: it can't provision the
// PR-branch checkout those roles need, so they must go through
// address_pr_feedback. This is the root-cause guard for the stranded-commit bug.
func TestSpawnSession_RejectsFollowupRoles(t *testing.T) {
	o, st := newTestOrch(t)
	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	mgr := &model.Session{ObjectiveID: obj.ID, Role: model.RoleManager, Status: model.SessionRunning}
	_ = st.CreateSession(mgr)

	ctx := mcp.WithSession(context.Background(), mgr.ID)
	for _, role := range []string{"pr_followup", "ci_followup"} {
		_, err := o.mcpSpawnSession(ctx, map[string]any{"role": role, "title": "t", "goal": "g"})
		if err == nil {
			t.Fatalf("spawn_session must reject role %q", role)
		}
		if !strings.Contains(err.Error(), "address_pr_feedback") {
			t.Fatalf("rejection should point at address_pr_feedback, got: %v", err)
		}
	}
}

// address_pr_feedback provisions the PR-branch workspace and attaches the
// follow-up to the PR, accepting the GitHub number for pr_id.
func TestAddressPRFeedback_ProvisionsPRBranchWorkspace(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetForge(forge.NewFake())
	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	mgr := &model.Session{ObjectiveID: obj.ID, Role: model.RoleManager, Status: model.SessionRunning}
	_ = st.CreateSession(mgr)
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 5, Branch: "feature",
		BaseBranch: "main", HeadSHA: "sha1", Status: model.PROpen}
	_ = st.CreatePR(pr)

	ctx := mcp.WithSession(context.Background(), mgr.ID)
	if _, err := o.mcpAddressPRFeedback(ctx, map[string]any{"pr_id": "5", "instructions": "preserve unchanged fields"}); err != nil {
		t.Fatalf("address_pr_feedback: %v", err)
	}

	sessions, _ := st.ListSessionsByObjective(obj.ID)
	var fu *model.Session
	for _, s := range sessions {
		if s.Role == model.RolePRFollowup {
			fu = s
		}
	}
	if fu == nil {
		t.Fatal("address_pr_feedback should spawn a pr_followup session")
	}
	if fu.Metadata["pr_id"] != pr.ID {
		t.Fatalf("follow-up must be attached to the PR, got pr_id=%v", fu.Metadata["pr_id"])
	}
	if !strings.Contains(fu.Goal, "preserve unchanged fields") || !strings.Contains(fu.Goal, pr.ID) {
		t.Fatalf("goal should carry the instructions and the pr_id, got: %q", fu.Goal)
	}
	ws, _ := st.GetWorkspace(fu.WorkspaceID)
	if ws == nil || ws.Kind != model.WorkspacePRBranch || ws.BranchName != pr.Branch {
		t.Fatalf("follow-up should use a PR-branch checkout, got %+v", ws)
	}
}

// A second address_pr_feedback for a PR that already has a live follow-up must
// NOT spawn another follow-up — it steers the existing one in place. This is the
// fix for managers piling up duplicate follow-up workers on a single PR.
func TestAddressPRFeedback_ReusesActiveFollowup(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	fake := agent.NewFake(model.AgentClaude, true, nil)
	o.RegisterProvider(fake)
	o.SetForge(forge.NewFake())
	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	mgr := &model.Session{ObjectiveID: obj.ID, Role: model.RoleManager, Status: model.SessionRunning}
	_ = st.CreateSession(mgr)
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 9, Branch: "feature",
		BaseBranch: "main", HeadSHA: "sha1", Status: model.PROpen}
	_ = st.CreatePR(pr)

	ctx := mcp.WithSession(context.Background(), mgr.ID)
	if _, err := o.mcpAddressPRFeedback(ctx, map[string]any{"pr_id": "9", "instructions": "first pass"}); err != nil {
		t.Fatalf("first address_pr_feedback: %v", err)
	}
	msg, err := o.mcpAddressPRFeedback(ctx, map[string]any{"pr_id": "9", "instructions": "also handle the edge case"})
	if err != nil {
		t.Fatalf("second address_pr_feedback: %v", err)
	}
	if !strings.Contains(msg, "steered") {
		t.Fatalf("second call should report steering the existing follow-up, got: %q", msg)
	}

	var followups []*model.Session
	sessions, _ := st.ListSessionsByObjective(obj.ID)
	for _, s := range sessions {
		if s.Role == model.RolePRFollowup {
			followups = append(followups, s)
		}
	}
	if len(followups) != 1 {
		t.Fatalf("expected exactly one follow-up after a repeat call, got %d", len(followups))
	}
	if resumed := fake.Resumed(); len(resumed) != 1 || resumed[0] != followups[0].ID {
		t.Fatalf("the existing follow-up should have been steered (resumed), got resumed=%v", resumed)
	}
}

// list_children surfaces the objective's worker/follow-up session ids so the
// manager can recover them for message_session/cancel_session, excludes the
// manager itself, and hides terminal sessions unless asked.
func TestListChildren_ListsAndFilters(t *testing.T) {
	o, st := newTestOrch(t)
	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	mgr := &model.Session{ObjectiveID: obj.ID, Role: model.RoleManager, Status: model.SessionRunning}
	_ = st.CreateSession(mgr)
	running := &model.Session{ObjectiveID: obj.ID, Role: model.RoleImplementer, Status: model.SessionRunning, Title: "do the thing"}
	_ = st.CreateSession(running)
	done := &model.Session{ObjectiveID: obj.ID, Role: model.RoleReviewer, Status: model.SessionSucceeded, Title: "old review"}
	_ = st.CreateSession(done)
	ctx := mcp.WithSession(context.Background(), mgr.ID)

	out, err := o.mcpListChildren(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("list_children: %v", err)
	}
	if !strings.Contains(out, running.ID) {
		t.Fatalf("running child should be listed, got: %q", out)
	}
	if strings.Contains(out, mgr.ID) {
		t.Fatalf("the manager must not list itself, got: %q", out)
	}
	if strings.Contains(out, done.ID) {
		t.Fatalf("terminal child should be hidden by default, got: %q", out)
	}

	full, err := o.mcpListChildren(ctx, map[string]any{"include_finished": true})
	if err != nil {
		t.Fatalf("list_children include_finished: %v", err)
	}
	if !strings.Contains(full, done.ID) || !strings.Contains(full, running.ID) {
		t.Fatalf("include_finished should list both children, got: %q", full)
	}
}

// message_session lets the manager steer a running child in place, and confines
// that power to its own non-manager children.
func TestMessageSession_SteersChildAndGuards(t *testing.T) {
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	fake := agent.NewFake(model.AgentClaude, true, nil)
	o.RegisterProvider(fake)
	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	mgr := &model.Session{ObjectiveID: obj.ID, Role: model.RoleManager, Status: model.SessionRunning}
	_ = st.CreateSession(mgr)
	worker := &model.Session{ObjectiveID: obj.ID, Role: model.RoleImplementer, Agent: model.AgentClaude, Status: model.SessionRunning}
	_ = st.CreateSession(worker)
	ctx := mcp.WithSession(context.Background(), mgr.ID)

	// Happy path: steer the worker in place.
	out, err := o.mcpMessageSession(ctx, map[string]any{"session_id": worker.ID, "message": "that port IS doable — grind through it"})
	if err != nil {
		t.Fatalf("message_session: %v", err)
	}
	if !strings.Contains(out, worker.ID) {
		t.Fatalf("reply should name the steered session, got: %q", out)
	}
	if resumed := fake.Resumed(); len(resumed) != 1 || resumed[0] != worker.ID {
		t.Fatalf("worker should have been steered (resumed), got resumed=%v", resumed)
	}

	// Guard: cannot steer yourself.
	if _, err := o.mcpMessageSession(ctx, map[string]any{"session_id": mgr.ID, "message": "x"}); err == nil {
		t.Fatal("message_session must reject steering the manager itself")
	}

	// Guard: cannot steer a session in another objective.
	other, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "y", Prompt: "q"})
	stranger := &model.Session{ObjectiveID: other.ID, Role: model.RoleImplementer, Status: model.SessionRunning}
	_ = st.CreateSession(stranger)
	if _, err := o.mcpMessageSession(ctx, map[string]any{"session_id": stranger.ID, "message": "x"}); err == nil {
		t.Fatal("message_session must reject a session outside the objective")
	}

	// Guard: cannot steer a finished session.
	doneWorker := &model.Session{ObjectiveID: obj.ID, Role: model.RoleImplementer, Status: model.SessionSucceeded}
	_ = st.CreateSession(doneWorker)
	if _, err := o.mcpMessageSession(ctx, map[string]any{"session_id": doneWorker.ID, "message": "x"}); err == nil {
		t.Fatal("message_session must reject a terminal session")
	}
}

// UpdatePR must refuse to push when no checkout holds the PR branch, rather than
// running git in the orchestrator's own cwd. This is what turned a manager-spawned
// follow-up's stranded commit into a silent no-op.
func TestUpdatePR_NoCheckoutRefuses(t *testing.T) {
	o, st := newTestOrch(t)
	f := forge.NewFake()
	o.SetForge(f)
	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	pr := &model.PullRequest{ObjectiveID: obj.ID, Repo: "octo/repo", Number: 4, Branch: "b",
		BaseBranch: "main", Status: model.PROpen}
	_ = st.CreatePR(pr)

	_, err := o.UpdatePR(context.Background(), pr.ID, UpdateSpec{SessionID: "x"})
	if err == nil {
		t.Fatal("update_pr with no PR-branch checkout must error")
	}
	if !strings.Contains(err.Error(), "no PR-branch checkout") {
		t.Fatalf("error should explain the missing checkout, got: %v", err)
	}
	if len(f.Pushes) != 0 {
		t.Fatalf("no push should have happened, got %d", len(f.Pushes))
	}
}
