package orch

import (
	"context"
	"strings"
	"testing"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/forge"
	"github.com/nathanwhit/orcha/internal/mcp"
	"github.com/nathanwhit/orcha/internal/model"
)

// reviewGateFixture wires an objective with a manager, an implementer with a
// workspace + seeded diff, a fake forge, both providers (so a cross-provider
// reviewer can be chosen), and a project for octo/repo with the gate set to
// `gate`. It returns the orchestrator, objective, manager, implementer, and forge.
func reviewGateFixture(t *testing.T, gate bool) (*Orchestrator, *model.Objective, *model.Session, *model.Session, *forge.Fake) {
	t.Helper()
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.RegisterProvider(agent.NewFake(model.AgentCodex, true, nil))
	f := forge.NewFake()
	o.SetForge(f)

	if err := st.UpsertProject(&model.Project{Repo: "octo/repo", ReviewGate: gate}); err != nil {
		t.Fatalf("project: %v", err)
	}
	obj, mgr, err := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p", Repo: "octo/repo"})
	if err != nil {
		t.Fatalf("objective: %v", err)
	}
	impl, err := o.CreateSession(SpawnSpec{
		ObjectiveID: obj.ID, ParentSessionID: mgr.ID, Role: model.RoleImplementer, Agent: model.AgentClaude})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	ws, err := o.PrepareIsolatedWorkspace(context.Background(), impl.ID, "octo/repo", "", "main")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	f.SetDiff(ws.Path, true)
	f.SetDiffText(ws.Path, "DIFF-V1")
	return o, obj, mgr, impl, f
}

func reviewersFor(o *Orchestrator, objID string) []*model.Session {
	all, _ := o.st.ListSessionsByObjective(objID)
	var out []*model.Session
	for _, s := range all {
		if s.Role == model.RoleReviewer {
			out = append(out, s)
		}
	}
	return out
}

// With the gate off, publish_pr opens the PR immediately and spawns no reviewer —
// the feature is strictly opt-in.
func TestReviewGate_OffPublishesDirectly(t *testing.T) {
	o, obj, _, impl, _ := reviewGateFixture(t, false)
	pr, err := o.PublishPR(context.Background(), impl.ID, PublishSpec{Title: "t", Body: "b"})
	if err != nil {
		t.Fatalf("publish with gate off should succeed: %v", err)
	}
	if pr == nil || pr.Status != model.PROpen {
		t.Fatalf("expected an open PR, got %+v", pr)
	}
	if rs := reviewersFor(o, obj.ID); len(rs) != 0 {
		t.Fatalf("gate off must not spawn a reviewer, got %d", len(rs))
	}
}

// With the gate on, the first publish is held: no PR opens, and exactly one
// reviewer is spawned on the OTHER provider, bound to the implementer and the
// diff fingerprint. A second publish of the same diff does not duplicate it.
func TestReviewGate_OnHoldsAndSpawnsAdversary(t *testing.T) {
	o, obj, _, impl, _ := reviewGateFixture(t, true)

	if _, err := o.PublishPR(context.Background(), impl.ID, PublishSpec{Title: "t", Body: "b"}); err == nil {
		t.Fatal("gate on: first publish must be held, not opened")
	}
	if prs, _ := o.st.ListPRsByObjective(obj.ID); len(prs) != 0 {
		t.Fatalf("no PR should open while review is pending, got %d", len(prs))
	}
	rs := reviewersFor(o, obj.ID)
	if len(rs) != 1 {
		t.Fatalf("expected exactly 1 reviewer, got %d", len(rs))
	}
	rv := rs[0]
	if rv.Agent != model.AgentCodex {
		t.Fatalf("reviewer must run on the other provider (codex), got %s", rv.Agent)
	}
	if id, _ := rv.Metadata["reviews_session"].(string); id != impl.ID {
		t.Fatalf("reviewer not bound to implementer: %q", id)
	}
	if fp, _ := rv.Metadata["review_fingerprint"].(string); fp != diffFingerprint("DIFF-V1") {
		t.Fatalf("reviewer fingerprint = %q, want %q", fp, diffFingerprint("DIFF-V1"))
	}
	gotImpl, _ := o.st.GetSession(impl.ID)
	if _, ok := pendingPublishSpec(gotImpl); !ok {
		t.Fatal("publish intent was not stashed on the implementer")
	}
	// Same diff, second attempt: a review is already in flight — do not duplicate it.
	if _, err := o.PublishPR(context.Background(), impl.ID, PublishSpec{Title: "t", Body: "b"}); err == nil {
		t.Fatal("publish should still be held while a review is in flight")
	}
	if rs := reviewersFor(o, obj.ID); len(rs) != 1 {
		t.Fatalf("a second publish of the same diff must not spawn another reviewer, got %d", len(rs))
	}
}

// An approving verdict replays the held publish, opening the PR automatically.
func TestReviewGate_ApproveOpensPR(t *testing.T) {
	o, obj, mgr, impl, f := reviewGateFixture(t, true)
	if _, err := o.PublishPR(context.Background(), impl.ID, PublishSpec{Title: "t", Body: "b", CommitMessage: "c"}); err == nil {
		t.Fatal("first publish should be held")
	}
	rv := reviewersFor(o, obj.ID)[0]
	_ = o.Cancel(mgr.ID, false) // manager terminal: the verdict's steer is a no-op in the test

	ctx := mcp.WithSession(context.Background(), rv.ID)
	msg, err := o.mcpSubmitReview(ctx, map[string]any{"verdict": "approve", "summary": "lgtm"})
	if err != nil {
		t.Fatalf("submit_review approve: %v", err)
	}
	if !strings.Contains(msg, "opened PR") {
		t.Fatalf("approve should report an opened PR, got %q", msg)
	}
	prs, _ := o.st.ListPRsByObjective(obj.ID)
	if len(prs) != 1 || prs[0].Status != model.PROpen {
		t.Fatalf("approve should open exactly one open PR, got %+v", prs)
	}
	if len(f.Pushes) != 1 {
		t.Fatalf("expected exactly one push, got %d", len(f.Pushes))
	}
}

// request_changes holds the PR and records the verdict. A re-publish of the same
// diff returns the cached reject without a new reviewer; changing the diff
// invalidates the verdict and triggers a fresh review.
func TestReviewGate_RequestChangesHoldsThenReReviewsOnNewDiff(t *testing.T) {
	o, obj, mgr, impl, f := reviewGateFixture(t, true)
	if _, err := o.PublishPR(context.Background(), impl.ID, PublishSpec{Title: "t", Body: "b"}); err == nil {
		t.Fatal("first publish should be held")
	}
	rv := reviewersFor(o, obj.ID)[0]
	_ = o.Cancel(mgr.ID, false)

	ctx := mcp.WithSession(context.Background(), rv.ID)
	if _, err := o.mcpSubmitReview(ctx, map[string]any{
		"verdict": "request_changes", "summary": "no", "findings": []any{"bug at foo.go:10"}}); err != nil {
		t.Fatalf("submit_review request_changes: %v", err)
	}
	if prs, _ := o.st.ListPRsByObjective(obj.ID); len(prs) != 0 {
		t.Fatalf("request_changes must not open a PR, got %d", len(prs))
	}
	gotImpl, _ := o.st.GetSession(impl.ID)
	if v, _ := gotImpl.Metadata["review_verdict"].(string); v != reviewRequestChanges {
		t.Fatalf("verdict on implementer = %q, want %q", v, reviewRequestChanges)
	}

	// Same diff: cached reject, no new reviewer.
	if _, err := o.PublishPR(context.Background(), impl.ID, PublishSpec{Title: "t", Body: "b"}); err == nil {
		t.Fatal("re-publish of a rejected diff should stay blocked")
	}
	if rs := reviewersFor(o, obj.ID); len(rs) != 1 {
		t.Fatalf("cached reject must not spawn a new reviewer, got %d", len(rs))
	}

	// Changed diff: the verdict no longer applies — a fresh review is spawned.
	ws, _ := o.st.GetWorkspace(gotImpl.WorkspaceID)
	f.SetDiffText(ws.Path, "DIFF-V2")
	if _, err := o.PublishPR(context.Background(), impl.ID, PublishSpec{Title: "t", Body: "b"}); err == nil {
		t.Fatal("a changed diff should be held for a fresh review")
	}
	if rs := reviewersFor(o, obj.ID); len(rs) != 2 {
		t.Fatalf("a changed diff should spawn a second reviewer, got %d", len(rs))
	}
}

// The reviewer MCP surface exposes exactly submit_review + create_note + ask_user
// — no report_result and none of the manager/worker mutation tools.
func TestReviewMCPSurface_Tools(t *testing.T) {
	o, _ := newTestOrch(t)
	got := listTools(t, o.ReviewMCPHandler())
	want := []string{"ask_user", "create_note", "submit_review"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("review surface = %v, want %v", got, want)
	}
}

// A gate-spawned reviewer (bound to a reviewed session) gets the /rmcp/
// submit_review surface; a reviewer a manager spawned by hand keeps the ordinary
// worker /wmcp/ surface (report_result).
func TestReviewGate_RoutingByBinding(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.ManagerMCPBaseURL = "http://127.0.0.1:0"
	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})

	bound := &model.Session{ObjectiveID: obj.ID, Role: model.RoleReviewer, Agent: model.AgentCodex,
		Goal: "review", Metadata: model.JSONMap{"reviews_session": "impl-1"}}
	if err := st.CreateSession(bound); err != nil {
		t.Fatalf("create bound: %v", err)
	}
	if u := o.buildSpec(bound, nil, nil).MCP["orcha"]; !strings.Contains(u, "/rmcp/") {
		t.Fatalf("a gate reviewer should use the /rmcp/ surface, got %q", u)
	}

	manual := &model.Session{ObjectiveID: obj.ID, Role: model.RoleReviewer, Agent: model.AgentCodex, Goal: "review"}
	if err := st.CreateSession(manual); err != nil {
		t.Fatalf("create manual: %v", err)
	}
	if u := o.buildSpec(manual, nil, nil).MCP["orcha"]; !strings.Contains(u, "/wmcp/") {
		t.Fatalf("a hand-spawned reviewer should use the /wmcp/ surface, got %q", u)
	}
}

// review_gate round-trips through the store: it persists, survives a "remember
// the repo" re-upsert, and can be toggled off via the editing path.
func TestProjectReviewGate_Roundtrip(t *testing.T) {
	_, st := newTestOrch(t)
	if err := st.UpsertProject(&model.Project{Repo: "octo/repo", ReviewGate: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if p, err := st.GetProjectByRepo("octo/repo"); err != nil || !p.ReviewGate {
		t.Fatalf("review_gate should persist true, got %+v err=%v", p, err)
	}
	// Re-registering the repo (the empty-preserving upsert) must not clear it.
	if err := st.UpsertProject(&model.Project{Repo: "octo/repo"}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if p, _ := st.GetProjectByRepo("octo/repo"); !p.ReviewGate {
		t.Fatal("re-registering a repo must not clear the review gate")
	}
	// The editing path can turn it off.
	p, _ := st.GetProjectByRepo("octo/repo")
	p.ReviewGate = false
	if err := st.UpdateProject(p); err != nil {
		t.Fatalf("update: %v", err)
	}
	if p2, _ := st.GetProjectByRepo("octo/repo"); p2.ReviewGate {
		t.Fatal("toggling the gate off should clear review_gate")
	}
}
