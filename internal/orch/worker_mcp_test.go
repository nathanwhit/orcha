package orch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/nathanwhit/orcha/internal/forge"
	"github.com/nathanwhit/orcha/internal/mcp"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/store"
)

// listTools returns the tool names an MCP handler exposes, via tools/list.
func listTools(t *testing.T, h http.Handler) []string {
	t.Helper()
	req := httptest.NewRequest("POST", "/sess1", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode tools/list: %v (%s)", err, rec.Body.String())
	}
	res, _ := out["result"].(map[string]any)
	raw, _ := res["tools"].([]any)
	var names []string
	for _, x := range raw {
		if m, ok := x.(map[string]any); ok {
			names = append(names, m["name"].(string))
		}
	}
	sort.Strings(names)
	return names
}

// The three MCP surfaces must expose exactly their intended tools — in
// particular a worker/follow-up must NOT see spawn_session, publish_pr,
// mark_objective_done, or cancel_session.
func TestMCPSurfaces_ToolScoping(t *testing.T) {
	o, _ := newTestOrch(t)

	manager := listTools(t, o.ManagerMCPHandler())
	wantManager := []string{"address_pr_feedback", "ask_user", "cancel_session", "comment_pr",
		"create_note", "mark_objective_done", "publish_pr", "spawn_session", "update_pr"}
	if strings.Join(manager, ",") != strings.Join(wantManager, ",") {
		t.Fatalf("manager surface = %v, want %v", manager, wantManager)
	}

	worker := listTools(t, o.WorkerMCPHandler())
	wantWorker := []string{"ask_user", "create_note", "report_result"}
	if strings.Join(worker, ",") != strings.Join(wantWorker, ",") {
		t.Fatalf("worker surface = %v, want %v", worker, wantWorker)
	}

	followup := listTools(t, o.FollowupMCPHandler())
	wantFollowup := []string{"ask_user", "comment_pr", "create_note", "report_result", "update_pr"}
	if strings.Join(followup, ",") != strings.Join(wantFollowup, ",") {
		t.Fatalf("followup surface = %v, want %v", followup, wantFollowup)
	}

	// The escalation tools must never leak into a non-manager surface.
	for _, banned := range []string{"spawn_session", "publish_pr", "mark_objective_done", "cancel_session"} {
		for _, n := range append(append([]string{}, worker...), followup...) {
			if n == banned {
				t.Fatalf("%q must not be exposed to workers/follow-ups", banned)
			}
		}
	}
}

// reportCtx binds a session id to a context the way the MCP server does, so the
// report_result handler resolves the calling worker.
func reportCtx(sessionID string) context.Context {
	return mcp.WithSession(context.Background(), sessionID)
}

// seedWorker creates an objective + a worker session with a ready workspace.
func seedWorker(t *testing.T, o *Orchestrator, st *store.Store) (*model.Session, *model.Workspace) {
	t.Helper()
	obj, mgr, err := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p"})
	if err != nil {
		t.Fatalf("create objective: %v", err)
	}
	w := &model.Session{
		ObjectiveID: obj.ID, ParentSessionID: mgr.ID, Role: model.RoleReviewer,
		Agent: model.AgentCodex, Status: model.SessionRunning, Title: "Review",
	}
	if err := st.CreateSession(w); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	ws := &model.Workspace{
		ObjectiveID: obj.ID, SessionID: w.ID, Kind: model.WorkspaceIsolated,
		Path: "/work/" + w.ID, Status: model.WorkspaceReady,
	}
	if err := st.CreateWorkspace(ws); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := st.UpdateSessionRuntime(w.ID, func(s *model.Session) { s.WorkspaceID = ws.ID }); err != nil {
		t.Fatalf("bind workspace: %v", err)
	}
	w, _ = st.GetSession(w.ID)
	return w, ws
}

func TestReportResult_SetsHandoffSummary(t *testing.T) {
	o, st := newTestOrch(t)
	o.SetForge(forge.NewFake())
	w, _ := seedWorker(t, o, st)

	_, err := o.mcpReportResult(reportCtx(w.ID), map[string]any{
		"summary": "Found 1 medium issue: SSH metadata deletion at Targets.tsx:214.",
	})
	if err != nil {
		t.Fatalf("report_result: %v", err)
	}
	got, _ := st.GetSession(w.ID)
	if !strings.Contains(got.HandoffSummary, "SSH metadata deletion at Targets.tsx:214") {
		t.Fatalf("handoff missing findings: %q", got.HandoffSummary)
	}
}

func TestReportResult_AttachesDiff(t *testing.T) {
	o, st := newTestOrch(t)
	f := forge.NewFake()
	o.SetForge(f)
	w, ws := seedWorker(t, o, st)
	f.SetDiffText(ws.Path, "diff --git a/x b/x\n+added line")

	if _, err := o.mcpReportResult(reportCtx(w.ID), map[string]any{
		"summary":      "Implemented the change.",
		"include_diff": true,
	}); err != nil {
		t.Fatalf("report_result: %v", err)
	}
	got, _ := st.GetSession(w.ID)
	if !strings.Contains(got.HandoffSummary, "Implemented the change.") ||
		!strings.Contains(got.HandoffSummary, "+added line") {
		t.Fatalf("handoff missing summary or diff: %q", got.HandoffSummary)
	}
}

func TestReportResult_IncludeDiffNoChanges(t *testing.T) {
	o, st := newTestOrch(t)
	o.SetForge(forge.NewFake()) // no diff text seeded -> empty diff
	w, _ := seedWorker(t, o, st)

	msg, err := o.mcpReportResult(reportCtx(w.ID), map[string]any{
		"summary":      "Read-only review; no changes.",
		"include_diff": true,
	})
	if err != nil {
		t.Fatalf("report_result: %v", err)
	}
	if !strings.Contains(msg, "no diff was attached") {
		t.Fatalf("expected a no-diff note, got %q", msg)
	}
	got, _ := st.GetSession(w.ID)
	if strings.Contains(got.HandoffSummary, "diff vs base") {
		t.Fatalf("should not have a diff section: %q", got.HandoffSummary)
	}
}

func TestReportResult_InlinesReferencedNotes(t *testing.T) {
	o, st := newTestOrch(t)
	o.SetForge(forge.NewFake())
	w, _ := seedWorker(t, o, st)

	note, err := o.CreateNote(w.ID, "Full review", "Three findings, the worst at api.go:563.")
	if err != nil {
		t.Fatalf("create note: %v", err)
	}
	if _, err := o.mcpReportResult(reportCtx(w.ID), map[string]any{
		"summary": "See the full review note.",
		"notes":   []any{note.ID},
	}); err != nil {
		t.Fatalf("report_result: %v", err)
	}
	got, _ := st.GetSession(w.ID)
	if !strings.Contains(got.HandoffSummary, "Full review") ||
		!strings.Contains(got.HandoffSummary, "api.go:563") {
		t.Fatalf("handoff missing inlined note: %q", got.HandoffSummary)
	}
}

func TestReportResult_RequiresSummary(t *testing.T) {
	o, st := newTestOrch(t)
	o.SetForge(forge.NewFake())
	w, _ := seedWorker(t, o, st)

	if _, err := o.mcpReportResult(reportCtx(w.ID), map[string]any{"summary": "   "}); err == nil {
		t.Fatal("expected an error for an empty summary")
	}
}

func TestRelaySummary_PrefersHandoff(t *testing.T) {
	s := &model.Session{
		HandoffSummary:  "authoritative findings",
		LatestSummary:   "scraped pane noise",
		CurrentActivity: "running build",
	}
	if got := relaySummary(s); got != "authoritative findings" {
		t.Fatalf("relaySummary = %q, want the handoff", got)
	}
	// Falls back to the scrape when no handoff was recorded.
	s.HandoffSummary = ""
	if got := relaySummary(s); got != "scraped pane noise" {
		t.Fatalf("relaySummary fallback = %q, want LatestSummary", got)
	}
	s.LatestSummary = ""
	if got := relaySummary(s); got != "running build" {
		t.Fatalf("relaySummary fallback = %q, want CurrentActivity", got)
	}
}

func TestRelaySummaryLine_FirstLineCapped(t *testing.T) {
	s := &model.Session{HandoffSummary: "\n  headline finding  \n\nlong details below\n"}
	if got := relaySummaryLine(s); got != "headline finding" {
		t.Fatalf("relaySummaryLine = %q, want the first non-empty line", got)
	}
}

func TestTailRunes(t *testing.T) {
	// Keeps the tail (the conclusion), rune-safe: a box-drawing-heavy string is
	// never split mid-character, and the END is what survives.
	s := strings.Repeat("─", 50) + "FINDINGS"
	got := tailRunes(s, 10)
	if !strings.HasSuffix(got, "FINDINGS") {
		t.Fatalf("tailRunes dropped the tail: %q", got)
	}
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("tailRunes should mark truncation: %q", got)
	}
	if []rune(got)[1] == '�' {
		t.Fatalf("tailRunes split a multibyte rune: %q", got)
	}
	// Short input is returned unchanged.
	if got := tailRunes("hi", 10); got != "hi" {
		t.Fatalf("tailRunes mangled short input: %q", got)
	}
}
