package orch

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/nathanwhit/orcha/internal/mcp"
	"github.com/nathanwhit/orcha/internal/model"
)

// Bounds on what a single report_result handoff carries. The handoff is stored on
// the session row and relayed verbatim to the manager, so it must be generous
// enough to hold real findings yet bounded so a runaway diff can't bloat the row
// or the manager's context.
const (
	maxHandoffText = 6000 // the worker's authored summary
	maxHandoffNote = 1200 // each referenced note's inlined body
	maxHandoffDiff = 8000 // the attached diff (stat + patch)
)

// WorkerMCPHandler returns the MCP tool surface for one-shot coding workers
// (implementer/reviewer/validator/researcher/custom). It is deliberately a SMALL
// subset of the manager surface: a worker may report its result, record a
// shared-memory note, and ask the user — but it cannot spawn sessions,
// publish/merge PRs, or mark the objective done (those are the manager's to
// decide). Mount it under "/wmcp/"; each worker connects to "/wmcp/<sessionID>".
func (o *Orchestrator) WorkerMCPHandler() http.Handler {
	return mcpServer(
		o.toolReportResult(),
		o.toolCreateNote(),
		o.toolAskUser(),
	)
}

// FollowupMCPHandler returns the MCP tool surface for PR/CI follow-up workers.
// A follow-up acts on an EXISTING PR, so it gets the PR-response tools on top of
// the base worker surface — but, like any worker, NOT spawn_session, publish_pr,
// mark_objective_done, or cancel_session (which previously leaked in because
// follow-ups shared the full manager surface). Mount it under "/fmcp/".
func (o *Orchestrator) FollowupMCPHandler() http.Handler {
	return mcpServer(
		o.toolUpdatePR(),
		o.toolCommentPR(),
		o.toolCreateNote(),
		o.toolAskUser(),
		o.toolReportResult(),
	)
}

// toolReportResult builds the report_result tool. Workers (coding and follow-up)
// call it to author exactly what is relayed to the manager.
func (o *Orchestrator) toolReportResult() mcp.Tool {
	return mcp.Tool{
		Name: "report_result",
		Description: "Hand your result back to the manager. THIS is what the manager sees when you finish — " +
			"put the conclusion that matters here (review findings with file:line refs, what you changed and why, " +
			"or why you couldn't). Set include_diff to attach your diff. Call this once, when you are done, " +
			"before printing the done marker.",
		InputSchema: mcpObj(map[string]any{
			"summary":      map[string]any{"type": "string", "description": "the result to relay to the manager (the full findings/outcome, not a teaser)"},
			"include_diff": map[string]any{"type": "boolean", "description": "attach your checkout's diff vs base (committed + uncommitted) so the manager can see the actual change"},
			"notes":        map[string]any{"type": "array", "items": mcpStr, "description": "ids of notes you created with create_note to inline into the handoff (for long content kept in shared memory)"},
		}, "summary"),
		Handler: o.mcpReportResult,
	}
}

// mcpReportResult records the worker-authored handoff that is relayed to the
// manager when the session finishes. The worker chooses exactly what to relay:
// its summary, optionally the checkout's diff, and optionally the bodies of notes
// it stashed in shared memory. Stored in HandoffSummary, which is preferred over
// the scraped LatestSummary everywhere a worker's result is surfaced.
func (o *Orchestrator) mcpReportResult(ctx context.Context, args map[string]any) (string, error) {
	id := mcp.SessionFromContext(ctx)
	if id == "" {
		return "", fmt.Errorf("no session bound to request")
	}
	sess, err := o.st.GetSession(id)
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(mcp.StringArg(args, "summary"))
	if summary == "" {
		return "", fmt.Errorf("summary is required")
	}

	var b strings.Builder
	b.WriteString(truncateRunes(summary, maxHandoffText))

	// Inline the bodies of any referenced notes so the content actually reaches
	// the manager (shared-memory notes are not otherwise folded into its context).
	if noteIDs := mcp.StringsArg(args, "notes"); len(noteIDs) > 0 {
		if notes := o.collectNotes(sess.ObjectiveID, noteIDs); notes != "" {
			b.WriteString("\n\nReferenced notes:\n")
			b.WriteString(notes)
		}
	}

	diffAttached := false
	if mcp.BoolArg(args, "include_diff") {
		if diff := o.workerDiff(ctx, sess); diff != "" {
			b.WriteString("\n\n--- Changes (diff vs base) ---\n")
			b.WriteString(truncateRunes(diff, maxHandoffDiff))
			diffAttached = true
		}
	}

	handoff := b.String()
	if _, err := o.st.UpdateSessionRuntime(id, func(s *model.Session) {
		s.HandoffSummary = handoff
	}); err != nil {
		return "", err
	}
	o.audit(sess.ObjectiveID, id, "worker_reported", "worker recorded its result handoff",
		model.JSONMap{"include_diff": diffAttached})

	msg := "result recorded; it will be relayed to your manager when you finish."
	if mcp.BoolArg(args, "include_diff") && !diffAttached {
		msg += " (no diff was attached — nothing to compare against the base)"
	}
	return msg, nil
}

// workerDiff returns the session's checkout diff vs base, run on the worker's
// target (where the checkout lives). Empty on any failure — a missing diff must
// never block a worker from reporting.
func (o *Orchestrator) workerDiff(ctx context.Context, sess *model.Session) string {
	if o.forge == nil || sess.WorkspaceID == "" {
		return ""
	}
	ws, err := o.st.GetWorkspace(sess.WorkspaceID)
	if err != nil || ws.Path == "" {
		return ""
	}
	diff, err := o.forgeForWorkspace(ws).Diff(ctx, ws.Path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(diff)
}

// relaySummary is what gets handed to the manager when a worker's result is
// surfaced in full (the completion notification). It prefers the worker-authored
// handoff (report_result), falling back to the scraped last output / activity for
// a worker that finished without reporting.
func relaySummary(s *model.Session) string {
	switch {
	case strings.TrimSpace(s.HandoffSummary) != "":
		return s.HandoffSummary
	case strings.TrimSpace(s.LatestSummary) != "":
		return s.LatestSummary
	default:
		return s.CurrentActivity
	}
}

// relaySummaryLine is the compact one-line form of relaySummary, for context
// listings and status snapshots that show one worker per line. It keeps the first
// non-empty line so a multi-paragraph handoff doesn't blow out a one-line view.
func relaySummaryLine(s *model.Session) string {
	full := relaySummary(s)
	for _, ln := range strings.Split(full, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return truncateRunes(t, 280)
		}
	}
	return ""
}

// collectNotes renders the referenced notes (matched by id within the objective)
// as a bounded, inlined list. Unknown ids are skipped.
func (o *Orchestrator) collectNotes(objectiveID string, ids []string) string {
	if objectiveID == "" {
		return ""
	}
	arts, err := o.st.ListArtifactsByObjective(objectiveID)
	if err != nil {
		return ""
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var b strings.Builder
	for _, a := range arts {
		if !want[a.ID] {
			continue
		}
		fmt.Fprintf(&b, "- %s:\n%s\n", a.Title, truncateRunes(strings.TrimSpace(a.Summary), maxHandoffNote))
	}
	return b.String()
}
