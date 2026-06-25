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
		o.toolCommentIssue(),
		o.toolAddressPRFeedback(),
		o.toolListChildren(),
		o.toolMessageSession(),
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
	// A tool with no required fields gets a nil variadic slice, which marshals to
	// JSON `null`. JSON Schema's "required" must be an array, and strict MCP
	// clients (Claude Code's Zod validation) reject the ENTIRE tools/list when any
	// tool has `"required": null` — silently leaving the agent with no tools. Emit
	// an empty array instead.
	if required == nil {
		required = []string{}
	}
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
		Description: "Push follow-up changes to an existing PR (branch-safe: never pushes to a merged PR). After a rebase (which rewrites history) set force=true, or the push is rejected as non-fast-forward. Pass title and/or body to edit the PR's title/description on the host (gh pr edit) — you can retitle/rewrite the description without any code change.",
		InputSchema: mcpObj(map[string]any{
			"pr_id":          mcpStr,
			"session_id":     mcpStr,
			"title":          map[string]any{"type": "string", "description": "new PR title; updates it on the host. Omit to leave the title unchanged"},
			"body":           map[string]any{"type": "string", "description": "new PR description; updates it on the host. Omit to leave the description unchanged"},
			"commit_message": map[string]any{"type": "string", "description": "used only if you left changes uncommitted; prefer committing yourself with git"},
			"force":          map[string]any{"type": "boolean", "description": "force-push (--force-with-lease); required after a rebase or any history rewrite"},
			"force_reason":   map[string]any{"type": "string", "description": "why a force push is needed (e.g. 'rebased onto main to resolve conflicts')"},
		}, "pr_id"),
		Handler: o.mcpUpdatePR,
	}
}

func (o *Orchestrator) toolCommentPR() mcp.Tool {
	return mcp.Tool{
		Name: "comment_pr",
		Description: "Leave a PUBLIC comment on a PR, visible to the human reviewers. Use only when a " +
			"reviewer would find it useful — to answer a question or explain a change you pushed. Do NOT " +
			"use it for status, progress, or CI/build updates (e.g. \"N checks passing\", \"CI pending\", " +
			"\"no changes needed\"); those are noise. pr_id accepts the Orcha pr_id or the GitHub PR number.",
		InputSchema: mcpObj(map[string]any{"pr_id": mcpStr, "body": mcpStr}, "pr_id", "body"),
		Handler:     o.mcpCommentPR,
	}
}

func (o *Orchestrator) toolCommentIssue() mcp.Tool {
	return mcp.Tool{
		Name: "comment_issue",
		Description: "Leave a PUBLIC comment, as the bot, on the GitHub ISSUE this objective is working — " +
			"visible to the issue reporter and watchers. Use it to reply to someone who commented on the issue: " +
			"answer a question, acknowledge a pointer, or say why you're not doing something. Same discipline as " +
			"comment_pr — NOT for status, progress, or CI/build noise; keep it short and specific. The target is " +
			"the issue this objective was created from, so you do NOT pass a number. This is the ONLY way to " +
			"reply on the issue — never use the gh CLI.",
		InputSchema: mcpObj(map[string]any{"body": mcpStr}, "body"),
		Handler:     o.mcpCommentIssue,
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

func (o *Orchestrator) toolListChildren() mcp.Tool {
	return mcp.Tool{
		Name: "list_children",
		Description: "List the worker and PR/CI follow-up sessions under this objective, with their " +
			"session ids, roles, statuses, and current activity. Use it to recover a session id you need " +
			"for message_session, cancel_session, or publish_pr — e.g. after you were resumed and lost track " +
			"of what is running. Set include_finished: true to also see completed/failed/canceled sessions.",
		InputSchema: mcpObj(map[string]any{
			"include_finished": map[string]any{"type": "boolean", "description": "include terminal (finished/failed/canceled) sessions; default false"},
		}),
		Handler: o.mcpListChildren,
	}
}

func (o *Orchestrator) toolMessageSession() mcp.Tool {
	return mcp.Tool{
		Name: "message_session",
		Description: "Steer one of your running child sessions — a worker or a PR/CI follow-up — in place, " +
			"WITHOUT canceling and respawning it. The message is delivered mid-flight: the session picks up " +
			"your new direction and keeps its work and context. Use this to redirect, add context, correct " +
			"course, or push back (e.g. a worker that wrongly claims a task can't be done) whenever the " +
			"existing session can still reach the right answer — it is almost always better than cancel_session + " +
			"spawn_session, which throws away progress. session_id is the id returned by spawn_session or " +
			"address_pr_feedback.",
		InputSchema: mcpObj(map[string]any{"session_id": mcpStr, "message": mcpStr}, "session_id", "message"),
		Handler:     o.mcpMessageSession,
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

func (o *Orchestrator) mcpCommentIssue(ctx context.Context, args map[string]any) (string, error) {
	objID := o.callerObjective(ctx)
	if objID == "" {
		return "", fmt.Errorf("no objective bound to request")
	}
	if err := o.CommentIssue(ctx, objID, mcp.StringArg(args, "body")); err != nil {
		return "", err
	}
	return "comment posted on the issue.", nil
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
	instructions := mcp.StringArg(args, "instructions")
	// If a follow-up is already working this PR, steer it with the new
	// instructions rather than spawning another. Two follow-ups on one PR-branch
	// checkout stomp each other and the team loses track of which is authoritative;
	// this is the bug where repeated address_pr_feedback calls pile up duplicate
	// workers. A fresh follow-up spawns naturally once the prior one finishes.
	if existing := o.activePRFollowup(pr.ObjectiveID, pr.ID); existing != nil {
		if err := o.Steer(ctx, existing.ID, "Additional instructions for this PR follow-up:\n"+instructions); err != nil {
			return "", err
		}
		return fmt.Sprintf("a %s (session %s) is already working PR #%d; steered it with your new instructions instead of spawning a duplicate.",
			existing.Role, existing.ID, pr.Number), nil
	}
	sess, err := o.spawnPRFollowup(ctx, pr, role, instructions)
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

func (o *Orchestrator) mcpListChildren(ctx context.Context, args map[string]any) (string, error) {
	mgr, err := o.managerSession(ctx)
	if err != nil {
		return "", err
	}
	includeFinished := mcp.BoolArg(args, "include_finished")
	sessions, err := o.st.ListSessionsByObjective(mgr.ObjectiveID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	n := 0
	for _, s := range sessions {
		if s.ID == mgr.ID || s.Role == model.RoleManager {
			continue // children only, not the manager(s)
		}
		if s.Status.IsTerminal() && !includeFinished {
			continue
		}
		n++
		fmt.Fprintf(&b, "\n- %s [%s] %s", s.ID, s.Status, s.Role)
		if s.Title != "" {
			fmt.Fprintf(&b, " %q", s.Title)
		}
		if prID, _ := s.Metadata["pr_id"].(string); prID != "" {
			fmt.Fprintf(&b, " (pr_id %s)", prID)
		}
		if act := strings.TrimSpace(s.CurrentActivity); act != "" {
			fmt.Fprintf(&b, " — %s", act)
		}
	}
	if n == 0 {
		if includeFinished {
			return "This objective has no child sessions yet.", nil
		}
		return "No running child sessions. (Pass include_finished: true to see completed ones.)", nil
	}
	return fmt.Sprintf("%d child session(s):%s", n, b.String()), nil
}

func (o *Orchestrator) mcpMessageSession(ctx context.Context, args map[string]any) (string, error) {
	mgr, err := o.managerSession(ctx)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(mcp.StringArg(args, "session_id"))
	if id == "" {
		return "", fmt.Errorf("session_id is required")
	}
	msg := strings.TrimSpace(mcp.StringArg(args, "message"))
	if msg == "" {
		return "", fmt.Errorf("message is required")
	}
	target, err := o.st.GetSession(id)
	if err != nil {
		return "", err
	}
	// Confine steering to this manager's own children: same objective, not the
	// manager itself, not a peer manager.
	if target.ID == mgr.ID {
		return "", fmt.Errorf("cannot message yourself; this tool steers your child sessions")
	}
	if target.ObjectiveID != mgr.ObjectiveID {
		return "", fmt.Errorf("session %s is not part of this objective", id)
	}
	if target.Role == model.RoleManager {
		return "", fmt.Errorf("cannot steer another manager session")
	}
	if target.Status.IsTerminal() {
		return "", fmt.Errorf("session %s already finished (%s) — spawn or address_pr_feedback a new one instead of steering", id, target.Status)
	}
	if err := o.Steer(ctx, id, msg); err != nil {
		return "", err
	}
	return fmt.Sprintf("delivered your message to %s session %s; it will act on it in place.", target.Role, id), nil
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
