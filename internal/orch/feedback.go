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
			wasMerged := pr.Status == model.PRMerged
			updated, _ := o.st.UpdatePR(prID, func(p *model.PullRequest) { p.Status = model.PRMerged })
			f.Actionable = false
			if !wasMerged && updated != nil {
				o.notifyManagerOfMerge(updated)
			}
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
	// Fork workflow: the PR branch lives on the fork recorded at publish time.
	pushRepo, _ := pr.Metadata["push_repo"].(string)
	pushURL := ""
	if pushRepo != "" {
		pushURL = cloneURLFor(pushRepo)
	}
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
		Metadata:    prWorkspaceMeta(pr.Repo, prID, cloneURL, pushRepo),
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
			PushURL: pushURL,
		}); perr != nil {
			_ = o.st.SetWorkspaceStatus(ws.ID, model.WorkspaceFailed)
			return nil, perr
		}
	}
	_ = o.st.SetWorkspaceStatus(ws.ID, model.WorkspaceReady)

	goal := fmt.Sprintf("Address feedback on PR #%d (%q) in repo %s.\n"+
		"Use orcha pr_id=%q for your tool calls (update_pr / comment_pr take pr_id).\n"+
		"This checkout is the PR branch %q with origin set.\n\nFeedback:\n%s\n\nPrior PR summary: %s",
		pr.Number, pr.Title, pr.Repo, prID, pr.Branch, strings.Join(checkLogs, "\n"), pr.Summary)

	sess, err := o.CreateSession(SpawnSpec{
		ObjectiveID: pr.ObjectiveID,
		Role:        role,
		Agent:       o.defaultAgent(),
		Mode:        model.ModeNoninteractive, // one-shot: do the fix and finish
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

// orchaBotMarker tags orcha's own PR comments so the monitor doesn't react to
// them as if they were user feedback.
const orchaBotMarker = "<!-- orcha-bot -->"

// notifyManagerOfMerge re-prompts the objective's manager when one of its PRs is
// merged (detected by the PR monitor or an explicit feedback ingest). A merged
// PR is the signal a slice has actually landed — without this nudge the manager
// sits idle after publishing and the objective never reaches mark_objective_done,
// so merging a PR appears to do nothing. When every PR is merged and no workers
// remain, the message tells the manager to finish; otherwise to keep going.
func (o *Orchestrator) notifyManagerOfMerge(pr *model.PullRequest) {
	mgr := o.activeManagerFor(pr.ObjectiveID)
	if mgr == nil {
		return
	}
	prs, _ := o.st.ListPRsByObjective(pr.ObjectiveID)
	openPRs := 0
	for _, p := range prs {
		if p.Status == model.PROpen || p.Status == model.PRDraft {
			openPRs++
		}
	}
	msg := fmt.Sprintf("PR #%d (%q) was merged.", pr.Number, pr.Title)
	if openPRs == 0 && !o.objectiveHasActiveWorkers(pr.ObjectiveID, mgr.ID) {
		msg += " All PRs for this objective are now merged and no workers are running. " +
			"If the objective is complete, call mark_objective_done now. If more work remains, spawn it."
	} else {
		msg += " Keep going with the remaining work; call mark_objective_done once everything is shipped."
	}
	o.audit(pr.ObjectiveID, mgr.ID, "manager_notified_merge",
		fmt.Sprintf("PR #%d merged", pr.Number), model.JSONMap{"pr_id": pr.ID})
	_ = o.Steer(context.Background(), mgr.ID, msg)
}

// activeManagerFor returns the objective's live (non-terminal) manager session,
// or nil if there isn't one.
func (o *Orchestrator) activeManagerFor(objectiveID string) *model.Session {
	sessions, err := o.st.ListSessionsByObjective(objectiveID)
	if err != nil {
		return nil
	}
	for _, s := range sessions {
		if s.Role == model.RoleManager && !s.Status.IsTerminal() {
			return s
		}
	}
	return nil
}

// objectiveHasActiveWorkers reports whether any non-manager session for the
// objective is still active (excluding the given manager).
func (o *Orchestrator) objectiveHasActiveWorkers(objectiveID, managerID string) bool {
	sessions, err := o.st.ListSessionsByObjective(objectiveID)
	if err != nil {
		return false
	}
	for _, s := range sessions {
		if s.ID == managerID || s.Role == model.RoleManager {
			continue
		}
		if !s.Status.IsTerminal() {
			return true
		}
	}
	return false
}

// SyncPRFeedback polls the host for new comments on a PR, ingests actionable
// ones (deduped), refreshes PR state, and spawns follow-up sessions. This is
// what a PR monitor calls on a tick or what the API triggers on demand.
func (o *Orchestrator) SyncPRFeedback(ctx context.Context, prID string) ([]*model.Session, error) {
	if o.forge == nil {
		return nil, nil
	}
	pr, err := o.RefreshPR(ctx, prID) // also picks up merged/closed/checks
	if err != nil {
		pr, err = o.st.GetPR(prID)
		if err != nil {
			return nil, err
		}
	}
	comments, err := o.forge.ListComments(ctx, pr.Repo, pr.Number)
	if err != nil {
		return nil, err
	}
	var items []model.PRFeedback
	for _, c := range comments {
		if strings.Contains(c.Body, orchaBotMarker) {
			continue // our own reply
		}
		items = append(items, model.PRFeedback{
			Kind: model.FeedbackKind(c.Kind), ExternalID: c.ExternalID, Body: c.Body, Actionable: true,
		})
	}
	if len(items) > 0 {
		if err := o.IngestFeedback(ctx, prID, items); err != nil {
			return nil, err
		}
	}
	return o.ProcessFeedback(ctx, prID)
}

// SyncOpenPRs runs a feedback sync for every open/draft PR. A PR monitor loop
// calls this on a tick. It first adopts any out-of-band PRs on active objectives
// so a PR an agent opened with the gh CLI (instead of publish_pr) still gets
// monitored, merge-detected, and followed up.
func (o *Orchestrator) SyncOpenPRs(ctx context.Context) {
	if objs, err := o.st.ListObjectives(); err == nil {
		for _, obj := range objs {
			if obj.Status == model.ObjectiveActive {
				o.AdoptUntrackedPRs(ctx, obj.ID)
			}
		}
	}
	prs, err := o.st.ListPRs()
	if err != nil {
		return
	}
	for _, pr := range prs {
		if pr.Status == model.PROpen || pr.Status == model.PRDraft {
			_, _ = o.SyncPRFeedback(ctx, pr.ID)
		}
	}
}

// AdoptUntrackedPRs scans an objective's workspace branches for open PRs orcha
// did not create — ones an agent opened out-of-band (e.g. by running the gh CLI
// instead of publish_pr) — and records them so they are tracked, monitored for
// review/CI/merge, and followed up like any PR orcha opened itself. An agent
// with a shell can always reach for git/gh, so rather than try to forbid that,
// we make it not matter. Returns how many were adopted.
func (o *Orchestrator) AdoptUntrackedPRs(ctx context.Context, objectiveID string) int {
	if o.forge == nil {
		return 0
	}
	wss, err := o.st.ListWorkspacesByObjective(objectiveID)
	if err != nil {
		return 0
	}
	tracked, _ := o.st.ListPRsByObjective(objectiveID)
	knownBranch := map[string]bool{}
	for _, pr := range tracked {
		knownBranch[pr.Branch] = true
	}
	adopted := 0
	for _, ws := range wss {
		if ws.BranchName == "" || ws.VCS != model.VCSGit || knownBranch[ws.BranchName] {
			continue
		}
		repo := ws.ProjectPath
		if r, ok := ws.Metadata["repo"].(string); ok && r != "" {
			repo = r
		}
		st, err := o.forge.FindOpenPR(ctx, repo, ws.BranchName)
		if err != nil || st == nil {
			continue
		}
		pr := &model.PullRequest{
			ObjectiveID:        objectiveID,
			CreatedBySessionID: ws.SessionID,
			Repo:               repo,
			Number:             st.Number,
			URL:                st.URL,
			Branch:             ws.BranchName,
			BaseBranch:         ws.BaseRef,
			HeadSHA:            st.HeadSHA,
			Status:             model.PRStatus(st.Status),
			ChecksState:        model.ChecksState(st.ChecksState),
			Title:              normalizePRTitle(st.Title),
		}
		if pushRepo, _ := ws.Metadata["push_repo"].(string); pushRepo != "" {
			pr.Metadata = model.JSONMap{"push_repo": pushRepo}
		}
		if err := o.st.CreatePR(pr); err != nil {
			continue
		}
		knownBranch[ws.BranchName] = true
		adopted++
		o.audit(objectiveID, ws.SessionID, "pr_adopted",
			fmt.Sprintf("adopted out-of-band PR #%d on %s", st.Number, ws.BranchName),
			model.JSONMap{"pr_id": pr.ID, "url": st.URL})
	}
	if adopted > 0 {
		o.notifyChange()
	}
	return adopted
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

// prWorkspaceMeta builds a PR-branch workspace's metadata; push_repo is only
// present for fork PRs.
func prWorkspaceMeta(repo, prID, cloneURL, pushRepo string) model.JSONMap {
	m := model.JSONMap{"repo": repo, "pr_id": prID, "clone_url": cloneURL}
	if pushRepo != "" {
		m["push_repo"] = pushRepo
	}
	return m
}
