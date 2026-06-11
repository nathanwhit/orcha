package orch

import (
	"context"
	"errors"
	"fmt"

	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/store"
)

// PublishSpec carries the agent-supplied PR content.
type PublishSpec struct {
	Title         string
	Body          string
	CommitMessage string // from the agent, never a generic backend template
	BaseBranch    string
}

// ErrUnsafePublish indicates a mechanical safety check failed before any push.
var ErrUnsafePublish = errors.New("orch: unsafe to publish PR")

// PublishPR turns a session's workspace changes into a pull request. It runs the
// spec's mechanical-safety checks before pushing, acquires the PR branch lock
// (one updater per branch), pushes, opens the PR, and records the row. The
// objective is NOT blocked — publishing returns and work continues.
func (o *Orchestrator) PublishPR(ctx context.Context, sessionID string, spec PublishSpec) (*model.PullRequest, error) {
	if o.forge == nil {
		return nil, errors.New("orch: no forge configured")
	}
	sess, err := o.st.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if sess.WorkspaceID == "" {
		return nil, fmt.Errorf("%w: session has no workspace", ErrUnsafePublish)
	}
	ws, err := o.st.GetWorkspace(sess.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace missing", ErrUnsafePublish)
	}
	if ws.BranchName == "" {
		return nil, fmt.Errorf("%w: workspace has no branch", ErrUnsafePublish)
	}
	repo := ws.ProjectPath
	if r, ok := ws.Metadata["repo"].(string); ok && r != "" {
		repo = r
	}

	// --- Mechanical safety checks (no DB transaction held across these) ---
	if ok, err := o.forge.RepoExists(ctx, repo); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%w: repo %s not found", ErrUnsafePublish, repo)
	}
	// Commit any edits the worker made but didn't commit (acceptEdits agents can
	// edit but not run git), so there's a diff to publish.
	commitMsg := spec.CommitMessage
	if commitMsg == "" {
		commitMsg = "orcha: " + spec.Title
	}
	if _, err := o.forge.CommitAll(ctx, ws.Path, commitMsg); err != nil {
		return nil, err
	}
	if ok, err := o.forge.HasDiff(ctx, ws.Path); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%w: no diff in workspace", ErrUnsafePublish)
	}

	base := spec.BaseBranch
	if base == "" {
		base = ws.BaseRef
	}
	if base == "" {
		base = "main"
	}

	// Branch lock: one updater per PR branch. Keyed by repo+branch for new PRs.
	lockKey := "pr_branch:" + repo + ":" + ws.BranchName
	if err := o.st.AcquireLock(lockKey, model.LockPRBranch, sessionID, "publish"); err != nil {
		if errors.Is(err, store.ErrLockHeld) {
			return nil, fmt.Errorf("%w: branch lock unavailable", ErrUnsafePublish)
		}
		return nil, err
	}
	defer o.st.ReleaseLock(lockKey, sessionID)

	headSHA, err := o.forge.PushBranch(ctx, repo, ws.Path, ws.BranchName, false)
	if err != nil {
		return nil, err
	}
	res, err := o.forge.OpenPR(ctx, repo, ws.BranchName, base, spec.Title, spec.Body)
	if err != nil {
		return nil, err
	}

	pr := &model.PullRequest{
		ObjectiveID:        sess.ObjectiveID,
		CreatedBySessionID: sessionID,
		Repo:               repo,
		Number:             res.Number,
		URL:                res.URL,
		Branch:             ws.BranchName,
		BaseBranch:         base,
		HeadSHA:            firstNonEmpty(res.HeadSHA, headSHA),
		Status:             model.PROpen,
		ChecksState:        model.ChecksPending,
		Title:              spec.Title,
		Summary:            spec.Body,
	}
	if err := o.st.CreatePR(pr); err != nil {
		return nil, err
	}
	// Record a primary artifact pointing at the PR (not stdout).
	_ = o.st.CreateArtifact(&model.Artifact{
		ObjectiveID: sess.ObjectiveID,
		SessionID:   sessionID,
		Kind:        model.ArtifactPullRequest,
		Title:       spec.Title,
		Summary:     spec.Body,
		URI:         res.URL,
		Visibility:  model.VisibilityPrimary,
	})
	o.audit(sess.ObjectiveID, sessionID, "pr_published",
		fmt.Sprintf("opened PR #%d", res.Number), model.JSONMap{"pr_id": pr.ID, "url": res.URL})
	return pr, nil
}

// UpdateSpec carries a follow-up update.
type UpdateSpec struct {
	SessionID   string // follow-up session performing the update
	WorkspaceID string // PR-branch workspace
	Force       bool
	ForceReason string
	Title       string
	Body        string
	Comment     string // optional GitHub comment; agent decides content
}

// UpdatePR pushes follow-up changes to an existing PR, enforcing branch safety:
// it refreshes host state first, never pushes to a merged PR, raises a manager
// decision for a closed PR, and otherwise pushes under the branch lock. Force
// push requires an explicit reason which is recorded.
func (o *Orchestrator) UpdatePR(ctx context.Context, prID string, spec UpdateSpec) (*model.PullRequest, error) {
	if o.forge == nil {
		return nil, errors.New("orch: no forge configured")
	}
	pr, err := o.st.GetPR(prID)
	if err != nil {
		return nil, err
	}

	// Refresh host state before deciding anything.
	if st, err := o.forge.GetPRState(ctx, pr.Repo, pr.Number); err == nil {
		pr, _ = o.st.UpdatePR(prID, func(p *model.PullRequest) {
			p.Status = model.PRStatus(st.Status)
			p.ChecksState = model.ChecksState(st.ChecksState)
			if st.HeadSHA != "" {
				p.HeadSHA = st.HeadSHA
			}
			now := o.st.Now()
			p.LastSyncedAt = &now
		})
	}

	decision := model.EvaluatePush(pr.Status)
	if !decision.Allowed {
		if decision.NeedsManagerDecision {
			// Closed PR -> create a manager decision point (a question), do NOT push.
			_ = o.st.CreateQuestion(&model.Question{
				ObjectiveID: pr.ObjectiveID,
				SessionID:   spec.SessionID,
				Priority:    15,
				Question:    fmt.Sprintf("PR #%d is closed. Open a new PR with the follow-up changes?", pr.Number),
				Context:     decision.Reason,
			})
			o.audit(pr.ObjectiveID, spec.SessionID, "pr_closed_decision", decision.Reason, model.JSONMap{"pr_id": prID})
		}
		return pr, fmt.Errorf("%w: %s", ErrUnsafePublish, decision.Reason)
	}
	if spec.Force && spec.ForceReason == "" {
		return pr, errors.New("orch: force push requires an explicit reason")
	}

	// One updater per PR branch, keyed by PR id.
	lockKey := prBranchLockKey(prID)
	if err := o.st.AcquireLock(lockKey, model.LockPRBranch, spec.SessionID, "follow-up update"); err != nil {
		if errors.Is(err, store.ErrLockHeld) {
			return pr, fmt.Errorf("%w: another updater holds the branch", ErrUnsafePublish)
		}
		return nil, err
	}
	defer o.st.ReleaseLock(lockKey, spec.SessionID)

	wsPath := ""
	if spec.WorkspaceID != "" {
		if ws, err := o.st.GetWorkspace(spec.WorkspaceID); err == nil {
			wsPath = ws.Path
		}
	}
	headSHA, err := o.forge.PushBranch(ctx, pr.Repo, wsPath, pr.Branch, spec.Force)
	if err != nil {
		return nil, err
	}
	pr, _ = o.st.UpdatePR(prID, func(p *model.PullRequest) {
		p.HeadSHA = headSHA
		if spec.Title != "" {
			p.Title = spec.Title
		}
		if spec.Body != "" {
			p.Summary = spec.Body
		}
	})
	if spec.Force {
		o.audit(pr.ObjectiveID, spec.SessionID, "force_push", spec.ForceReason, model.JSONMap{"pr_id": prID})
	}
	if spec.Comment != "" {
		_ = o.forge.Comment(ctx, pr.Repo, pr.Number, spec.Comment)
		o.audit(pr.ObjectiveID, spec.SessionID, "pr_comment", "left comment", model.JSONMap{"pr_id": prID})
	}
	o.audit(pr.ObjectiveID, spec.SessionID, "pr_updated",
		fmt.Sprintf("pushed follow-up to PR #%d", pr.Number), model.JSONMap{"pr_id": prID})
	return pr, nil
}

// RefreshPR syncs a PR's host state into the store.
func (o *Orchestrator) RefreshPR(ctx context.Context, prID string) (*model.PullRequest, error) {
	if o.forge == nil {
		return o.st.GetPR(prID)
	}
	pr, err := o.st.GetPR(prID)
	if err != nil {
		return nil, err
	}
	st, err := o.forge.GetPRState(ctx, pr.Repo, pr.Number)
	if err != nil {
		return nil, err
	}
	return o.st.UpdatePR(prID, func(p *model.PullRequest) {
		p.Status = model.PRStatus(st.Status)
		p.ChecksState = model.ChecksState(st.ChecksState)
		if st.HeadSHA != "" {
			p.HeadSHA = st.HeadSHA
		}
		now := o.st.Now()
		p.LastSyncedAt = &now
	})
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
