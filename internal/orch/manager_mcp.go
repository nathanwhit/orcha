package orch

import (
	"context"
	"fmt"
	"net/http"

	"github.com/nathanwhit/orcha/internal/mcp"
	"github.com/nathanwhit/orcha/internal/model"
)

// ManagerMCPHandler returns an HTTP handler exposing the manager tool surface
// over MCP. Mount it under "/mcp/" — each manager session's Claude connects to
// "/mcp/<sessionID>", and tool calls are scoped to that session/objective. This
// is what turns "the manager session runs" into "the manager actually manages":
// its tool calls drive the orchestrator.
func (o *Orchestrator) ManagerMCPHandler() http.Handler {
	s := mcp.NewServer("orcha", "0.1")

	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	str := map[string]any{"type": "string"}

	s.AddTool(mcp.Tool{
		Name:        "spawn_session",
		Description: "Spawn a scoped worker session under this objective. Returns the new session id. Use dependencies to make a session wait for others to succeed.",
		InputSchema: obj(map[string]any{
			"role":          map[string]any{"type": "string", "enum": []string{"implementer", "reviewer", "validator", "researcher", "pr_followup", "ci_followup", "custom"}},
			"title":         str,
			"goal":          str,
			"agent_hint":    map[string]any{"type": "string", "enum": []string{"claude", "codex"}},
			"dependencies":  map[string]any{"type": "array", "items": str},
			"repo":          map[string]any{"type": "string", "description": "override the objective's repo for this worker's checkout (owner/repo)"},
			"base_branch":   map[string]any{"type": "string", "description": "base branch for the checkout (default main)"},
			"target":        map[string]any{"type": "string", "description": "pin this worker to a target machine (name or id), e.g. a remote SSH box"},
			"target_labels": map[string]any{"type": "array", "items": str, "description": "require a target with these labels"},
		}, "role", "title", "goal"),
		Handler: o.mcpSpawnSession,
	})
	s.AddTool(mcp.Tool{
		Name:        "ask_user",
		Description: "Ask the user a question and block on their input. Use when requirements, credentials, setup, or direction are unclear.",
		InputSchema: obj(map[string]any{"question": str, "context": str}, "question"),
		Handler:     o.mcpAskUser,
	})
	s.AddTool(mcp.Tool{
		Name:        "publish_pr",
		Description: "Publish a PR from a worker session's committed changes. The orchestrator verifies mechanical safety, pushes the branch, and opens the PR.",
		InputSchema: obj(map[string]any{
			"session_id":     str,
			"title":          str,
			"body":           str,
			"commit_message": str,
		}, "session_id", "title", "body"),
		Handler: o.mcpPublishPR,
	})
	s.AddTool(mcp.Tool{
		Name:        "update_pr",
		Description: "Push follow-up changes to an existing PR (branch-safe: never pushes to a merged PR).",
		InputSchema: obj(map[string]any{
			"pr_id":        str,
			"session_id":   str,
			"title":        str,
			"body":         str,
			"push_changes": map[string]any{"type": "boolean"},
		}, "pr_id"),
		Handler: o.mcpUpdatePR,
	})
	s.AddTool(mcp.Tool{
		Name:        "comment_pr",
		Description: "Leave a comment on a PR.",
		InputSchema: obj(map[string]any{"pr_id": str, "body": str}, "pr_id", "body"),
		Handler:     o.mcpCommentPR,
	})
	s.AddTool(mcp.Tool{
		Name:        "create_note",
		Description: "Record a note in the objective's shared memory (not stdout).",
		InputSchema: obj(map[string]any{"title": str, "body": str}, "title", "body"),
		Handler:     o.mcpCreateNote,
	})
	s.AddTool(mcp.Tool{
		Name:        "mark_objective_done",
		Description: "Mark the objective complete with a concise summary.",
		InputSchema: obj(map[string]any{"summary": str}, "summary"),
		Handler:     o.mcpMarkDone,
	})
	s.AddTool(mcp.Tool{
		Name:        "cancel_session",
		Description: "Cancel a session (and its children).",
		InputSchema: obj(map[string]any{"session_id": str}, "session_id"),
		Handler:     o.mcpCancelSession,
	})

	return s.Handler()
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
	agentHint := model.AgentKind(mcp.StringArg(args, "agent_hint"))
	meta := model.JSONMap{}
	if repo := mcp.StringArg(args, "repo"); repo != "" {
		meta["repo"] = repo
		if base := mcp.StringArg(args, "base_branch"); base != "" {
			meta["base_branch"] = base
		}
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
		Role:         model.SessionRole(mcp.StringArg(args, "role")),
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
	pr, err := o.UpdatePR(ctx, mcp.StringArg(args, "pr_id"), UpdateSpec{
		SessionID: mcp.StringArg(args, "session_id"),
		Title:     mcp.StringArg(args, "title"),
		Body:      mcp.StringArg(args, "body"),
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("updated PR #%d (head %s)", pr.Number, pr.HeadSHA), nil
}

func (o *Orchestrator) mcpCommentPR(ctx context.Context, args map[string]any) (string, error) {
	if err := o.CommentPR(ctx, mcp.StringArg(args, "pr_id"), mcp.StringArg(args, "body")); err != nil {
		return "", err
	}
	return "comment posted.", nil
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
