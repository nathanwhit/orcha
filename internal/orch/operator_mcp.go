package orch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/nathanwhit/orcha/internal/mcp"
	"github.com/nathanwhit/orcha/internal/model"
)

// OperatorMCPHandler returns an HTTP handler exposing the operator tool surface
// over MCP. Mount it under "/omcp/". Unlike the manager/worker/follow-up
// surfaces, this one is NOT bound to a session — it represents a human operator
// (or an outside agent acting on their behalf, e.g. reached over an SSH tunnel)
// driving orcha from the top: create work, inspect state, answer questions, and
// steer a manager.
//
// It deliberately exposes none of the in-objective tools (spawn_session,
// publish_pr, mark_objective_done, …): those are a manager's to decide. The
// operator's job is to start objectives and observe/redirect them, not to do a
// manager's coordination by hand.
func (o *Orchestrator) OperatorMCPHandler() http.Handler {
	return mcpServer(
		o.toolListObjectives(),
		o.toolGetObjective(),
		o.toolListOpenQuestions(),
		o.toolCreateObjective(),
		o.toolMessageManager(),
		o.toolAnswerQuestion(),
		o.toolCancelObjective(),
	)
}

// jsonResult renders a value as indented JSON for an MCP tool result, so the
// calling agent gets structured state it can parse rather than prose.
func jsonResult(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (o *Orchestrator) toolListObjectives() mcp.Tool {
	return mcp.Tool{
		Name:        "list_objectives",
		Description: "List all objectives with their status and rollup counts (the dashboard rows). Start here to see what work exists.",
		InputSchema: mcpObj(map[string]any{}),
		Handler:     o.mcpListObjectives,
	}
}

func (o *Orchestrator) toolGetObjective() mcp.Tool {
	return mcp.Tool{
		Name:        "get_objective",
		Description: "Get one objective in full: its status and outcome summary, its sessions (manager + workers), pull requests, open questions, and notes/artifacts.",
		InputSchema: mcpObj(map[string]any{"objective_id": mcpStr}, "objective_id"),
		Handler:     o.mcpGetObjective,
	}
}

func (o *Orchestrator) toolListOpenQuestions() mcp.Tool {
	return mcp.Tool{
		Name:        "list_open_questions",
		Description: "List every unanswered question a manager has raised across all objectives — the things currently blocked waiting on a human. Answer them with answer_question.",
		InputSchema: mcpObj(map[string]any{}),
		Handler:     o.mcpListOpenQuestions,
	}
}

func (o *Orchestrator) toolCreateObjective() mcp.Tool {
	return mcp.Tool{
		Name:        "create_objective",
		Description: "Create a new objective and start its manager. prompt is the goal in full. For repo work pass repo (owner/repo) or a registered project_id; omit both for a repo-less investigation/research objective.",
		InputSchema: mcpObj(map[string]any{
			"title":       mcpStr,
			"prompt":      mcpStr,
			"repo":        map[string]any{"type": "string", "description": "owner/repo to check out and work in; omit for a repo-less objective"},
			"push_repo":   map[string]any{"type": "string", "description": "fork to push branches to (owner/repo); omit to push to repo itself"},
			"base_branch": map[string]any{"type": "string", "description": "base branch for checkouts (default main)"},
			"project_id":  map[string]any{"type": "string", "description": "a registered project supplying repo/fork/base defaults"},
			"agent":       map[string]any{"type": "string", "enum": []string{"claude", "codex"}, "description": "preferred agent for the manager"},
		}, "title", "prompt"),
		Handler: o.mcpCreateObjective,
	}
}

func (o *Orchestrator) toolMessageManager() mcp.Tool {
	return mcp.Tool{
		Name:        "message_manager",
		Description: "Send a message to an objective's manager — redirect it, add context, or nudge it. This steers the running manager session; it does not create a new objective.",
		InputSchema: mcpObj(map[string]any{"objective_id": mcpStr, "message": mcpStr}, "objective_id", "message"),
		Handler:     o.mcpMessageManager,
	}
}

func (o *Orchestrator) toolAnswerQuestion() mcp.Tool {
	return mcp.Tool{
		Name:        "answer_question",
		Description: "Answer a question a manager raised (from list_open_questions or get_objective). This unblocks the objective so it can proceed.",
		InputSchema: mcpObj(map[string]any{"question_id": mcpStr, "answer": mcpStr}, "question_id", "answer"),
		Handler:     o.mcpAnswerQuestion,
	}
}

func (o *Orchestrator) toolCancelObjective() mcp.Tool {
	return mcp.Tool{
		Name:        "cancel_objective",
		Description: "Cancel an objective and all of its sessions.",
		InputSchema: mcpObj(map[string]any{"objective_id": mcpStr}, "objective_id"),
		Handler:     o.mcpCancelObjective,
	}
}

func (o *Orchestrator) mcpListObjectives(_ context.Context, _ map[string]any) (string, error) {
	rows, err := o.st.DashboardObjectives()
	if err != nil {
		return "", err
	}
	if rows == nil {
		return "[]", nil
	}
	return jsonResult(rows)
}

func (o *Orchestrator) mcpGetObjective(_ context.Context, args map[string]any) (string, error) {
	id := mcp.StringArg(args, "objective_id")
	obj, err := o.st.GetObjective(id)
	if err != nil {
		return "", err
	}
	sessions, _ := o.st.DashboardSessions(id)
	prs, _ := o.st.ListPRsByObjective(id)
	questions, _ := o.st.ListQuestionsByObjective(id)
	artifacts, _ := o.st.ListArtifactsByObjective(id)
	return jsonResult(map[string]any{
		"objective":     obj,
		"sessions":      sessions,
		"pull_requests": prs,
		"questions":     questions,
		"artifacts":     artifacts,
	})
}

func (o *Orchestrator) mcpListOpenQuestions(_ context.Context, _ map[string]any) (string, error) {
	qs, err := o.st.ListOpenQuestions()
	if err != nil {
		return "", err
	}
	if qs == nil {
		return "[]", nil
	}
	return jsonResult(qs)
}

func (o *Orchestrator) mcpCreateObjective(_ context.Context, args map[string]any) (string, error) {
	spec := NewObjectiveSpec{
		Title:      strings.TrimSpace(mcp.StringArg(args, "title")),
		Prompt:     strings.TrimSpace(mcp.StringArg(args, "prompt")),
		Agent:      model.AgentKind(mcp.StringArg(args, "agent")),
		Repo:       mcp.StringArg(args, "repo"),
		PushRepo:   mcp.StringArg(args, "push_repo"),
		BaseBranch: mcp.StringArg(args, "base_branch"),
	}
	// A registered project supplies repo/fork/base defaults; explicit fields win.
	if pid := mcp.StringArg(args, "project_id"); pid != "" {
		p, err := o.st.GetProject(pid)
		if err != nil {
			return "", err
		}
		if spec.Repo == "" {
			spec.Repo = p.Repo
		}
		if spec.PushRepo == "" {
			spec.PushRepo = p.PushRepo
		}
		if spec.BaseBranch == "" {
			spec.BaseBranch = p.BaseBranch
		}
	}
	if spec.Title == "" || spec.Prompt == "" {
		return "", fmt.Errorf("title and prompt are required")
	}
	obj, mgr, err := o.CreateObjective(spec)
	if err != nil {
		return "", err
	}
	return jsonResult(map[string]any{
		"objective_id":       obj.ID,
		"manager_session_id": mgr.ID,
		"status":             string(obj.Status),
		"title":              obj.Title,
	})
}

func (o *Orchestrator) mcpMessageManager(ctx context.Context, args map[string]any) (string, error) {
	id := mcp.StringArg(args, "objective_id")
	obj, err := o.st.GetObjective(id)
	if err != nil {
		return "", err
	}
	if obj.ManagerSessionID == "" {
		return "", fmt.Errorf("objective %s has no manager session", id)
	}
	msg := strings.TrimSpace(mcp.StringArg(args, "message"))
	if msg == "" {
		return "", fmt.Errorf("message is required")
	}
	if err := o.Steer(ctx, obj.ManagerSessionID, msg); err != nil {
		return "", err
	}
	return "message delivered to the manager.", nil
}

func (o *Orchestrator) mcpAnswerQuestion(_ context.Context, args map[string]any) (string, error) {
	id := mcp.StringArg(args, "question_id")
	ans := strings.TrimSpace(mcp.StringArg(args, "answer"))
	if strings.TrimSpace(id) == "" || ans == "" {
		return "", fmt.Errorf("question_id and answer are required")
	}
	q, err := o.AnswerQuestion(id, ans)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("answered question %s; the objective can proceed.", q.ID), nil
}

func (o *Orchestrator) mcpCancelObjective(_ context.Context, args map[string]any) (string, error) {
	id := mcp.StringArg(args, "objective_id")
	if err := o.CancelObjective(id, "canceled by operator"); err != nil {
		return "", err
	}
	return "objective " + id + " canceled.", nil
}
