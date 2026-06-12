package orch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/forge"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/store"
)

// forgeFor returns the forge bound to run its git/gh commands on target's
// machine, when the configured forge supports retargeting. A worker's checkout
// and gh auth live on its target, so PR operations must run THERE — running them
// on the orchestrator host fails ("chdir <workspace>: no such file or directory"
// for a remote checkout). A nil target or non-retargetable forge (the Fake) is
// returned unchanged.
func (o *Orchestrator) forgeFor(target *model.Target) forge.Forge {
	if rt, ok := o.forge.(forge.Retargetable); ok && target != nil {
		return rt.OnExecutor(agent.NewExecutor(target))
	}
	return o.forge
}

// forgeForWorkspace binds the forge to the target a workspace lives on.
func (o *Orchestrator) forgeForWorkspace(ws *model.Workspace) forge.Forge {
	if ws == nil || ws.TargetID == "" {
		return o.forge
	}
	tgt, err := o.st.GetTarget(ws.TargetID)
	if err != nil {
		return o.forge
	}
	return o.forgeFor(tgt)
}

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
	spec.Title = normalizePRTitle(spec.Title)
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
	// Run git/gh on the machine that holds the checkout (the workspace's target),
	// not the orchestrator host — otherwise the checkout path does not exist.
	f := o.forgeForWorkspace(ws)

	// --- Mechanical safety checks (no DB transaction held across these) ---
	if ok, err := f.RepoExists(ctx, repo); err != nil {
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
	if _, err := f.CommitAll(ctx, ws.Path, commitMsg); err != nil {
		return nil, err
	}
	if ok, err := f.HasDiff(ctx, ws.Path); err != nil {
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

	// The push goes to origin, whose push URL is the fork in a fork workflow
	// (set during workspace prep). The PR opens against the upstream repo, with
	// the head qualified as fork-owner:branch when the branch lives on a fork.
	pushRepo, _ := ws.Metadata["push_repo"].(string)
	headSHA, err := f.PushBranch(ctx, repo, ws.Path, ws.BranchName, false)
	if err != nil {
		return nil, err
	}
	head := ws.BranchName
	if pushRepo != "" && pushRepo != repo {
		head = repoOwner(pushRepo) + ":" + ws.BranchName
	}
	res, err := f.OpenPR(ctx, repo, head, base, spec.Title, spec.Body)
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
	if pushRepo != "" {
		// Follow-ups need to know the branch lives on the fork.
		pr.Metadata = model.JSONMap{"push_repo": pushRepo}
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
	SessionID     string // follow-up session performing the update
	WorkspaceID   string // PR-branch workspace
	Force         bool
	ForceReason   string
	Title         string
	Body          string
	CommitMessage string // used only if the agent left changes uncommitted
	Comment       string // optional GitHub comment; agent decides content
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
	// Run git/gh on the machine holding the PR-branch checkout (the follow-up
	// workspace's target), not the orchestrator host.
	var fws *model.Workspace
	if spec.WorkspaceID != "" {
		fws, _ = o.st.GetWorkspace(spec.WorkspaceID)
	}
	f := o.forgeForWorkspace(fws)

	// Refresh host state before deciding anything.
	if st, err := f.GetPRState(ctx, pr.Repo, pr.Number); err == nil {
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
	if fws != nil {
		wsPath = fws.Path
	}
	// Fallback: commit any edits the agent left uncommitted (normally the agent
	// commits its own work with its own message, so this is a no-op).
	if wsPath != "" {
		msg := spec.CommitMessage
		if msg == "" {
			msg = "Address PR feedback"
		}
		if _, err := f.CommitAll(ctx, wsPath, msg); err != nil {
			return nil, err
		}
	}
	headSHA, err := f.PushBranch(ctx, pr.Repo, wsPath, pr.Branch, spec.Force)
	if err != nil {
		return nil, err
	}
	pr, _ = o.st.UpdatePR(prID, func(p *model.PullRequest) {
		p.HeadSHA = headSHA
		if t := normalizePRTitle(spec.Title); t != "" {
			p.Title = t
		}
		if spec.Body != "" {
			p.Summary = spec.Body
		}
	})
	if spec.Force {
		o.audit(pr.ObjectiveID, spec.SessionID, "force_push", spec.ForceReason, model.JSONMap{"pr_id": prID})
	}
	if spec.Comment != "" {
		_ = f.Comment(ctx, pr.Repo, pr.Number, spec.Comment)
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
	wasMerged := pr.Status == model.PRMerged
	updated, err := o.st.UpdatePR(prID, func(p *model.PullRequest) {
		p.Status = model.PRStatus(st.Status)
		p.ChecksState = model.ChecksState(st.ChecksState)
		if st.HeadSHA != "" {
			p.HeadSHA = st.HeadSHA
		}
		now := o.st.Now()
		p.LastSyncedAt = &now
	})
	// First time we observe the merge, nudge the manager to wrap up — otherwise
	// a merged PR is silent and the objective never reaches mark_objective_done.
	if err == nil && !wasMerged && updated.Status == model.PRMerged {
		o.notifyManagerOfMerge(updated)
	}
	// An open PR that GitHub reports as CONFLICTING needs a rebase. Record it as
	// actionable feedback (deduped by head SHA, so it fires once per conflicting
	// head and again only if a new push is still conflicting) — ProcessFeedback
	// then spawns a follow-up that rebases and force-updates the PR.
	if err == nil && (updated.Status == model.PROpen || updated.Status == model.PRDraft) &&
		st.Mergeable == "CONFLICTING" && !o.hasActivePRFollowup(updated.ObjectiveID, prID) {
		// No follow-up is currently working this PR. If a prior attempt finished
		// without fixing it (e.g. it couldn't push), its handled feedback would
		// block a retry — clear it so this re-fires.
		_ = o.st.DeleteHandledConflictFeedback(prID)
		_ = o.IngestFeedback(ctx, prID, []model.PRFeedback{{
			Kind:       model.FeedbackConflict,
			ExternalID: "conflict@" + updated.HeadSHA,
			Body: "This PR has merge conflicts with its base branch (" + updated.BaseBranch + "). " +
				"In this PR-branch checkout, fetch the latest base and rebase the PR branch onto it " +
				"(inspect `git remote -v`; the upstream base is " + updated.BaseBranch + "), resolve every " +
				"conflict, re-run the build/tests, commit, then call update_pr with force=true and a short " +
				"reason — a rebase rewrites history so a normal push is rejected.",
			Actionable: true,
		}})
	}
	return updated, err
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// repoOwner returns the owner part of an "owner/repo" identifier.
func repoOwner(repo string) string {
	if i := strings.IndexByte(repo, '/'); i > 0 {
		return repo[:i]
	}
	return repo
}

// agentTitleTags are the leading "[tag]" prefixes stripped from agent-supplied
// PR titles: agents (notably codex) like to brand a title with their own name.
// A PR title should describe the change, not which model wrote it.
var agentTitleTags = map[string]bool{
	"codex": true, "claude": true, "orcha": true, "agent": true,
	"bot": true, "ai": true, "assistant": true,
}

// normalizePRTitle strips leading agent self-branding tags ("[codex] ...",
// "[claude]: ...") and surrounding whitespace, leaving the descriptive title.
// It only removes a known agent-name tag, so legitimate prefixes like "[WIP]"
// or a conventional-commit "feat:" are preserved. Applied to every published or
// updated PR title regardless of what the agent passed.
func normalizePRTitle(title string) string {
	for {
		t := strings.TrimSpace(title)
		if !strings.HasPrefix(t, "[") {
			return t
		}
		end := strings.IndexByte(t, ']')
		if end < 0 {
			return t
		}
		tag := strings.ToLower(strings.TrimSpace(t[1:end]))
		if !agentTitleTags[tag] {
			return t
		}
		// Drop the tag and any following separator (": ", "- ", spaces).
		rest := strings.TrimLeft(t[end+1:], " :-\t")
		if rest == "" {
			return t // tag-only title: keep it rather than emptying the title
		}
		title = rest
	}
}
