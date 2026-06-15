package orch

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/nathanwhit/orcha/internal/mcp"
	"github.com/nathanwhit/orcha/internal/model"
)

// ManagerMCPHandler returns an HTTP handler exposing the manager tool surface
// over MCP. Mount it under "/mcp/" — each manager session's Claude connects to
// "/mcp/<sessionID>", and tool calls are scoped to that session/objective. This
// is what turns "the manager session runs" into "the manager actually manages":
// its tool calls drive the orchestrator.
//
// The manager gets the full surface. Workers get strict subsets (see
// WorkerMCPHandler / FollowupMCPHandler) so a worker cannot, say, mark the
// objective done or spawn more workers — those are the manager's to decide.
func (o *Orchestrator) ManagerMCPHandler() http.Handler {
	return mcpServer(
		o.toolSpawnSession(),
		o.toolAskUser(),
		o.toolPublishPR(),
		o.toolUpdatePR(),
		o.toolCommentPR(),
		o.toolAddressPRFeedback(),
		o.toolCreateNote(),
		o.toolMarkDone(),
		o.toolCancelSession(),
	)
}

// mcpServer builds an MCP HTTP handler exposing exactly the given tools.
func mcpServer(tools ...mcp.Tool) http.Handler {
	s := mcp.NewServer("orcha", "0.1")
	for _, t := range tools {
		s.AddTool(t)
	}
	return s.Handler()
}

// mcpObj/mcpStr are tiny JSON-schema builders shared by the tool constructors.
func mcpObj(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var mcpStr = map[string]any{"type": "string"}

func (o *Orchestrator) toolSpawnSession() mcp.Tool {
	return mcp.Tool{
		Name:        "spawn_session",
		Description: "Spawn a scoped worker session under this objective. Returns the new session id. Use dependencies to make a session wait for others to succeed. To address feedback or push a fix to an existing PR, use address_pr_feedback instead — not spawn_session.",
		InputSchema: mcpObj(map[string]any{
			"role":          map[string]any{"type": "string", "enum": []string{"implementer", "reviewer", "validator", "researcher", "custom"}},
			"title":         mcpStr,
			"goal":          mcpStr,
			"agent_hint":    map[string]any{"type": "string", "enum": []string{"claude", "codex"}},
			"dependencies":  map[string]any{"type": "array", "items": mcpStr},
			"repo":          map[string]any{"type": "string", "description": "override the objective's repo for this worker's checkout (owner/repo, the upstream)"},
			"push_repo":     map[string]any{"type": "string", "description": "fork to push branches to (owner/repo); omit to push to repo itself"},
			"base_branch":   map[string]any{"type": "string", "description": "base branch for the checkout (default main)"},
			"target":        map[string]any{"type": "string", "description": "pin this worker to a target machine (name or id), e.g. a remote SSH box"},
			"target_labels": map[string]any{"type": "array", "items": mcpStr, "description": "require a target with these labels"},
		}, "role", "title", "goal"),
		Handler: o.mcpSpawnSession,
	}
}

func (o *Orchestrator) toolAskUser() mcp.Tool {
	return mcp.Tool{
		Name:        "ask_user",
		Description: "Ask the user a question and block on their input. Use when requirements, credentials, setup, or direction are unclear.",
		InputSchema: mcpObj(map[string]any{"question": mcpStr, "context": mcpStr}, "question"),
		Handler:     o.mcpAskUser,
	}
}

func (o *Orchestrator) toolPublishPR() mcp.Tool {
	return mcp.Tool{
		Name:        "publish_pr",
		Description: "Publish a PR from a worker session's committed changes. The orchestrator verifies mechanical safety, pushes the branch, and opens the PR.",
		InputSchema: mcpObj(map[string]any{
			"session_id":     mcpStr,
			"title":          mcpStr,
			"body":           mcpStr,
			"commit_message": mcpStr,
		}, "session_id", "title", "body"),
		Handler: o.mcpPublishPR,
	}
}

func (o *Orchestrator) toolUpdatePR() mcp.Tool {
	return mcp.Tool{
		Name:        "update_pr",
		Description: "Push follow-up changes to an existing PR (branch-safe: never pushes to a merged PR). After a rebase (which rewrites history) set force=true, or the push is rejected as non-fast-forward.",
		InputSchema: mcpObj(map[string]any{
			"pr_id":          mcpStr,
			"session_id":     mcpStr,
			"title":          mcpStr,
			"body":           mcpStr,
			"commit_message": map[string]any{"type": "string", "description": "used only if you left changes uncommitted; prefer committing yourself with git"},
			"force":          map[string]any{"type": "boolean", "description": "force-push (--force-with-lease); required after a rebase or any history rewrite"},
			"force_reason":   map[string]any{"type": "string", "description": "why a force push is needed (e.g. 'rebased onto main to resolve conflicts')"},
		}, "pr_id"),
		Handler: o.mcpUpdatePR,
	}
}

func (o *Orchestrator) toolCommentPR() mcp.Tool {
	return mcp.Tool{
		Name:        "comment_pr",
		Description: "Leave a comment on a PR. pr_id accepts the Orcha pr_id or the GitHub PR number.",
		InputSchema: mcpObj(map[string]any{"pr_id": mcpStr, "body": mcpStr}, "pr_id", "body"),
		Handler:     o.mcpCommentPR,
	}
}

func (o *Orchestrator) toolAddressPRFeedback() mcp.Tool {
	return mcp.Tool{
		Name: "address_pr_feedback",
		Description: "Spawn a follow-up worker to address review feedback or push a fix to an existing PR. " +
			"Use this for any PR follow-up instead of spawn_session: the worker gets a checkout of the PR " +
			"branch and pushes its fix back to the same PR. pr_id accepts the Orcha pr_id or the GitHub PR number.",
		InputSchema: mcpObj(map[string]any{
			"pr_id":        mcpStr,
			"instructions": map[string]any{"type": "string", "description": "what the follow-up should do: the review comments to address or the fix to make"},
			"role":         map[string]any{"type": "string", "enum": []string{"pr_followup", "ci_followup"}, "description": "ci_followup for CI/check failures; pr_followup (default) for review feedback"},
		}, "pr_id", "instructions"),
		Handler: o.mcpAddressPRFeedback,
	}
}

func (o *Orchestrator) toolCreateNote() mcp.Tool {
	return mcp.Tool{
		Name:        "create_note",
		Description: "Record a note in the objective's shared memory (not stdout).",
		InputSchema: mcpObj(map[string]any{"title": mcpStr, "body": mcpStr}, "title", "body"),
		Handler:     o.mcpCreateNote,
	}
}

func (o *Orchestrator) toolMarkDone() mcp.Tool {
	return mcp.Tool{
		Name:        "mark_objective_done",
		Description: "Mark the objective complete with a concise summary.",
		InputSchema: mcpObj(map[string]any{"summary": mcpStr}, "summary"),
		Handler:     o.mcpMarkDone,
	}
}

func (o *Orchestrator) toolCancelSession() mcp.Tool {
	return mcp.Tool{
		Name:        "cancel_session",
		Description: "Cancel a session (and its children).",
		InputSchema: mcpObj(map[string]any{"session_id": mcpStr}, "session_id"),
		Handler:     o.mcpCancelSession,
	}
}

// managerSession resolves the calling manager session from the request context.
func (o *Orchestrator) managerSession(ctx context.Context) (*model.Session, error) {
	id := mcp.SessionFromContext(ctx)
	if id == "" {
		return nil, fmt.Errorf("no manager session bound to request")
	}
	return o.st.GetSession(id)
}

func (o *Orchestrator) mcpSpawnSession(ctx context.Context, args map[string]any) (string, error) {
	mgr, err := o.managerSession(ctx)
	if err != nil {
		return "", err
	}
	role := model.SessionRole(mcp.StringArg(args, "role"))
	// PR/CI follow-ups need a PR-branch checkout that spawn_session cannot
	// provision; routing them through address_pr_feedback is the only way they get
	// one (without it the follow-up commits to a stranded branch and update_pr has
	// nothing to push). Block them here so the enum can't be worked around.
	if role == model.RolePRFollowup || role == model.RoleCIFollowup {
		return "", fmt.Errorf("use address_pr_feedback (not spawn_session) for PR follow-ups: it provisions the PR-branch checkout the follow-up must push its fix from")
	}
	agentHint := model.AgentKind(mcp.StringArg(args, "agent_hint"))
	meta := model.JSONMap{}
	if repo := mcp.StringArg(args, "repo"); repo != "" {
		meta["repo"] = repo
		if base := mcp.StringArg(args, "base_branch"); base != "" {
			meta["base_branch"] = base
		}
	}
	if pushRepo := mcp.StringArg(args, "push_repo"); pushRepo != "" {
		meta["push_repo"] = pushRepo
	}
	if target := mcp.StringArg(args, "target"); target != "" {
		meta["pinned_target"] = target
	}
	if labels := mcp.StringsArg(args, "target_labels"); len(labels) > 0 {
		anyLabels := make([]any, len(labels))
		for i, l := range labels {
			anyLabels[i] = l
		}
		meta["target_labels"] = anyLabels
	}
	if len(meta) == 0 {
		meta = nil
	}
	sess, err := o.SpawnSession(mgr.ID, SpawnSpec{
		Role:         role,
		Title:        mcp.StringArg(args, "title"),
		Goal:         mcp.StringArg(args, "goal"),
		Agent:        agentHint,
		Dependencies: mcp.StringsArg(args, "dependencies"),
		Metadata:     meta,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("spawned %s session %s (%q). It will start when scheduled.", sess.Role, sess.ID, sess.Title), nil
}

func (o *Orchestrator) mcpAskUser(ctx context.Context, args map[string]any) (string, error) {
	mgr, err := o.managerSession(ctx)
	if err != nil {
		return "", err
	}
	q, err := o.AskUser(mgr.ID, mcp.StringArg(args, "question"), mcp.StringArg(args, "context"), 5)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("asked the user (question %s); the objective is now waiting on their answer.", q.ID), nil
}

func (o *Orchestrator) mcpPublishPR(ctx context.Context, args map[string]any) (string, error) {
	pr, err := o.PublishPR(ctx, mcp.StringArg(args, "session_id"), PublishSpec{
		Title:         mcp.StringArg(args, "title"),
		Body:          mcp.StringArg(args, "body"),
		CommitMessage: mcp.StringArg(args, "commit_message"),
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("opened PR #%d: %s", pr.Number, pr.URL), nil
}

func (o *Orchestrator) mcpUpdatePR(ctx context.Context, args map[string]any) (string, error) {
	pr, err := o.resolvePR(ctx, mcp.StringArg(args, "pr_id"))
	if err != nil {
		return "", err
	}
	// Default the pushing session to the caller (e.g. a follow-up pushing its own
	// checkout), so the agent need not know its own session/workspace id.
	sessionID := mcp.StringArg(args, "session_id")
	if sessionID == "" {
		sessionID = mcp.SessionFromContext(ctx)
	}
	workspaceID := ""
	if s, err := o.st.GetSession(sessionID); err == nil {
		workspaceID = s.WorkspaceID
	}
	force := mcp.BoolArg(args, "force")
	reason := mcp.StringArg(args, "force_reason")
	if force && reason == "" {
		// UpdatePR requires a reason with a force push; supply a sensible default
		// so an agent that force-pushes after a rebase isn't blocked on wording.
		reason = "force update after history rewrite (e.g. rebase to resolve conflicts)"
	}
	updated, err := o.UpdatePR(ctx, pr.ID, UpdateSpec{
		SessionID:     sessionID,
		WorkspaceID:   workspaceID,
		Title:         mcp.StringArg(args, "title"),
		Body:          mcp.StringArg(args, "body"),
		CommitMessage: mcp.StringArg(args, "commit_message"),
		Force:         force,
		ForceReason:   reason,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("updated PR #%d (head %s)", updated.Number, updated.HeadSHA), nil
}

func (o *Orchestrator) mcpCommentPR(ctx context.Context, args map[string]any) (string, error) {
	pr, err := o.resolvePR(ctx, mcp.StringArg(args, "pr_id"))
	if err != nil {
		return "", err
	}
	if err := o.CommentPR(ctx, pr.ID, mcp.StringArg(args, "body")); err != nil {
		return "", err
	}
	return "comment posted.", nil
}

func (o *Orchestrator) mcpAddressPRFeedback(ctx context.Context, args map[string]any) (string, error) {
	pr, err := o.resolvePR(ctx, mcp.StringArg(args, "pr_id"))
	if err != nil {
		return "", err
	}
	role := model.RolePRFollowup
	if mcp.StringArg(args, "role") == string(model.RoleCIFollowup) {
		role = model.RoleCIFollowup
	}
	sess, err := o.spawnPRFollowup(ctx, pr, role, mcp.StringArg(args, "instructions"))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("spawned %s session %s for PR #%d on a checkout of branch %q; it will push its fix back to the PR.",
		role, sess.ID, pr.Number, pr.Branch), nil
}

// resolvePR resolves a pr_id tool argument to a PR. It accepts either the
// internal Orcha PR id (a UUID) or the GitHub PR number (e.g. "17"): the number
// is the identifier agents see on GitHub and in PR comments, so accepting it
// avoids the confusing "store: not found" an agent hits when it passes the number
// it knows. A bare number is resolved within the caller's objective.
func (o *Orchestrator) resolvePR(ctx context.Context, idArg string) (*model.PullRequest, error) {
	idArg = strings.TrimSpace(idArg)
	if idArg == "" {
		return nil, fmt.Errorf("pr_id is required")
	}
	if pr, err := o.st.GetPR(idArg); err == nil {
		return pr, nil
	}
	objID := o.callerObjective(ctx)
	if n, convErr := strconv.Atoi(idArg); convErr == nil && n > 0 && objID != "" {
		prs, _ := o.st.ListPRsByObjective(objID)
		var match *model.PullRequest
		for _, pr := range prs {
			if pr.Number != n {
				continue
			}
			// Prefer an open/draft PR if a number somehow appears more than once.
			if match == nil || pr.Status == model.PROpen || pr.Status == model.PRDraft {
				match = pr
			}
		}
		if match != nil {
			return match, nil
		}
	}
	// Help the agent recover by listing the objective's PRs and their ids.
	if objID != "" {
		if prs, _ := o.st.ListPRsByObjective(objID); len(prs) > 0 {
			var b strings.Builder
			for _, pr := range prs {
				fmt.Fprintf(&b, "\n  #%d -> pr_id %s (%s)", pr.Number, pr.ID, pr.Status)
			}
			return nil, fmt.Errorf("no PR found for pr_id %q; this objective's PRs:%s", idArg, b.String())
		}
	}
	return nil, fmt.Errorf("no PR found for pr_id %q (pass the Orcha pr_id or the GitHub PR number)", idArg)
}

// callerObjective returns the objective of the session bound to the MCP request,
// or "" if none is bound.
func (o *Orchestrator) callerObjective(ctx context.Context) string {
	id := mcp.SessionFromContext(ctx)
	if id == "" {
		return ""
	}
	if s, err := o.st.GetSession(id); err == nil {
		return s.ObjectiveID
	}
	return ""
}

func (o *Orchestrator) mcpCreateNote(ctx context.Context, args map[string]any) (string, error) {
	mgr, err := o.managerSession(ctx)
	if err != nil {
		return "", err
	}
	a, err := o.CreateNote(mgr.ID, mcp.StringArg(args, "title"), mcp.StringArg(args, "body"))
	if err != nil {
		return "", err
	}
	return "note recorded (" + a.ID + ").", nil
}

func (o *Orchestrator) mcpMarkDone(ctx context.Context, args map[string]any) (string, error) {
	mgr, err := o.managerSession(ctx)
	if err != nil {
		return "", err
	}
	if err := o.MarkObjectiveDone(mgr.ID, mcp.StringArg(args, "summary")); err != nil {
		return "", err
	}
	return "objective marked done.", nil
}

func (o *Orchestrator) mcpCancelSession(ctx context.Context, args map[string]any) (string, error) {
	id := mcp.StringArg(args, "session_id")
	if err := o.Cancel(id, true); err != nil {
		return "", err
	}
	return "canceled session " + id + ".", nil
}
