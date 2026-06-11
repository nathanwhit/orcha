package orch

import (
	"context"
	"fmt"
	"strings"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/workspace"
)

// IngestFeedback records observed PR feedback (deduped) and reacts to lifecycle
// events. merged/closed events update PR status; actionable comment/review/check
// events are left for ProcessFeedback to turn into follow-up sessions. This is
// what a PR monitor calls after polling the host.
func (o *Orchestrator) IngestFeedback(ctx context.Context, prID string, items []model.PRFeedback) error {
	pr, err := o.st.GetPR(prID)
	if err != nil {
		return err
	}
	for i := range items {
		f := items[i]
		f.PRID = prID
		switch f.Kind {
		case model.FeedbackMerged:
			_, _ = o.st.UpdatePR(prID, func(p *model.PullRequest) { p.Status = model.PRMerged })
			f.Actionable = false
		case model.FeedbackClosed:
			_, _ = o.st.UpdatePR(prID, func(p *model.PullRequest) { p.Status = model.PRClosed })
			f.Actionable = false
		}
		if _, err := o.st.RecordFeedback(&f); err != nil {
			return err
		}
	}
	o.audit(pr.ObjectiveID, "", "pr_feedback_ingested",
		fmt.Sprintf("ingested %d feedback items for PR #%d", len(items), pr.Number), model.JSONMap{"pr_id": prID})
	return nil
}

// ProcessFeedback creates follow-up sessions for actionable, unhandled feedback
// on a PR. Each follow-up is attached to the PR, gets a PR-branch workspace, and
// is handed the exact comments/check logs plus prior summaries. This runs
// independently of other implementation sessions — PR feedback never waits for
// unrelated work to finish. It returns the created sessions.
func (o *Orchestrator) ProcessFeedback(ctx context.Context, prID string) ([]*model.Session, error) {
	pr, err := o.st.GetPR(prID)
	if err != nil {
		return nil, err
	}
	pending, err := o.st.UnhandledFeedback(prID)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}

	// A PR that is merged/closed produces no push-style follow-up.
	if pr.Status == model.PRMerged {
		for _, f := range pending {
			_ = o.st.MarkFeedbackHandled(f.ID, "")
		}
		return nil, nil
	}

	role := model.RolePRFollowup
	var checkLogs []string
	for _, f := range pending {
		if f.Kind == model.FeedbackCheckFailure {
			role = model.RoleCIFollowup
		}
		checkLogs = append(checkLogs, string(f.Kind)+": "+f.Body)
	}

	// PR-branch workspace for the follow-up. Pick a schedulable target.
	target, err := o.SelectTarget(TargetRequest{})
	if err != nil {
		return nil, err
	}
	cloneURL := cloneURLFor(pr.Repo)
	dir := fmt.Sprintf("%s/pr-%d", target.WorkRoot, pr.Number)
	ws := &model.Workspace{
		ObjectiveID: pr.ObjectiveID,
		TargetID:    target.ID,
		Kind:        model.WorkspacePRBranch,
		ProjectPath: pr.Repo,
		VCS:         model.VCSGit,
		Path:        dir,
		BaseRef:     pr.Branch,
		BaseSHA:     pr.HeadSHA,
		BranchName:  pr.Branch,
		Status:      model.WorkspacePreparing,
		Metadata:    model.JSONMap{"repo": pr.Repo, "pr_id": prID, "clone_url": cloneURL},
	}
	if err := o.st.CreateWorkspace(ws); err != nil {
		return nil, err
	}
	// Materialize the PR-branch checkout at its fresh head so follow-up work
	// updates the correct branch.
	if o.preparer != nil {
		ex := agent.NewExecutor(target)
		if perr := o.preparer.PreparePRBranch(ctx, ex, workspace.Spec{
			WorkRoot: target.WorkRoot, RepoURL: cloneURL, Dir: dir, Branch: pr.Branch,
		}); perr != nil {
			_ = o.st.SetWorkspaceStatus(ws.ID, model.WorkspaceFailed)
			return nil, perr
		}
	}
	_ = o.st.SetWorkspaceStatus(ws.ID, model.WorkspaceReady)

	goal := fmt.Sprintf("Address feedback on PR #%d (%s).\n\nFeedback:\n%s\n\nPrior PR summary: %s",
		pr.Number, pr.Title, strings.Join(checkLogs, "\n"), pr.Summary)

	sess, err := o.CreateSession(SpawnSpec{
		ObjectiveID: pr.ObjectiveID,
		Role:        role,
		Agent:       o.defaultAgent(),
		Mode:        model.ModeInteractive,
		Title:       fmt.Sprintf("Follow-up: PR #%d", pr.Number),
		Goal:        goal,
		WorkspaceID: ws.ID,
		Metadata:    model.JSONMap{"pr_id": prID},
	})
	if err != nil {
		return nil, err
	}
	for _, f := range pending {
		_ = o.st.MarkFeedbackHandled(f.ID, sess.ID)
	}
	o.audit(pr.ObjectiveID, sess.ID, "followup_spawned",
		fmt.Sprintf("spawned %s for PR #%d", role, pr.Number), model.JSONMap{"pr_id": prID})
	return []*model.Session{sess}, nil
}

// defaultAgent returns any registered provider kind (preferring claude) for
// auto-spawned sessions.
func (o *Orchestrator) defaultAgent() model.AgentKind {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.providers[model.AgentClaude]; ok {
		return model.AgentClaude
	}
	for k := range o.providers {
		return k
	}
	return model.AgentClaude
}
