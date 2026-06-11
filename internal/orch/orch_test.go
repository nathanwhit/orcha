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

// ---- real workspace preparation ----

func tgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := osexec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=orcha", "GIT_AUTHOR_EMAIL=orcha@test",
		"GIT_COMMITTER_NAME=orcha", "GIT_COMMITTER_EMAIL=orcha@test")
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
