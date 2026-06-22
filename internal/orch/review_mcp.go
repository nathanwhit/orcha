package orch

import (
	"net/http"

	"github.com/nathanwhit/orcha/internal/mcp"
)

// ReviewMCPHandler returns the MCP tool surface for adversarial reviewer
// sessions. A reviewer judges another worker's diff and renders a verdict, so it
// gets submit_review (its handoff) plus create_note and ask_user — but NONE of
// the manager/worker mutation tools (no publish_pr, update_pr, spawn_session, or
// report_result). Mount it under "/rmcp/"; each reviewer connects to
// "/rmcp/<sessionID>".
func (o *Orchestrator) ReviewMCPHandler() http.Handler {
	return mcpServer(
		o.toolSubmitReview(),
		o.toolCreateNote(),
		o.toolAskUser(),
	)
}

func (o *Orchestrator) toolSubmitReview() mcp.Tool {
	return mcp.Tool{
		Name: "submit_review",
		Description: "Submit your verdict on the change you reviewed. THIS is how you finish a review — call " +
			"it once, when done, before the done marker. verdict must be \"approve\" (only if the change is " +
			"genuinely ready to ship) or \"request_changes\" (with specific, actionable findings). On approve " +
			"the PR opens automatically; on request_changes the manager gets your findings and the PR is held.",
		InputSchema: mcpObj(map[string]any{
			"verdict":  map[string]any{"type": "string", "enum": []string{reviewApprove, reviewRequestChanges}},
			"summary":  map[string]any{"type": "string", "description": "your overall assessment of the change"},
			"findings": map[string]any{"type": "array", "items": mcpStr, "description": "specific, actionable problems with file:line refs (required when requesting changes)"},
		}, "verdict", "summary"),
		Handler: o.mcpSubmitReview,
	}
}
