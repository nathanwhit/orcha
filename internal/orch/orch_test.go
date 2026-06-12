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
