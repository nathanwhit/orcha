package orch

import (
	"context"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// ReclaimWorkspaces removes the on-disk checkout of every workspace that is no
// longer needed and marks it archived, reclaiming disk on long-lived targets
// where per-session checkouts otherwise accumulate forever. The shared bare
// mirror cache is left in place (it is reused across checkouts and stays small).
//
// There are two passes, both gated on "no non-terminal session references the
// workspace":
//
//   - When the OBJECTIVE is terminal, every checkout it owns is reclaimed.
//   - While the objective is still ACTIVE, a finished worker's PRIVATE isolated
//     checkout is reclaimed once a PR has been published from its branch (see
//     reclaimSpentWorkerCheckouts). This matters because objectives can stay
//     active for hours/days while spawning many multi-GB worker checkouts; the
//     old objective-only gating let every finished worker's checkout sit on disk
//     for the whole objective and filled the target. Anything that could still be
//     read/written within an active objective — the manager's own checkout, a
//     not-yet-published worker checkout the manager may still publish from, a
//     checkout a pending dependent will inherit, or a shared/pr_branch checkout —
//     is left alone.
//
// It runs under a TryLock so overlapping ticks don't pile up, and is idempotent:
// already-archived workspaces are skipped, so each dir is removed at most once.
func (o *Orchestrator) ReclaimWorkspaces(ctx context.Context) {
	if !o.gcMu.TryLock() {
		return // a reclaim pass is already running
	}
	defer o.gcMu.Unlock()

	objs, err := o.st.ListObjectives()
	if err != nil {
		return
	}
	for _, obj := range objs {
		sessions, err := o.st.ListSessionsByObjective(obj.ID)
		if err != nil {
			continue
		}
		inUse := map[string]bool{}
		for _, s := range sessions {
			if s.WorkspaceID != "" && !s.Status.IsTerminal() {
				inUse[s.WorkspaceID] = true
			}
		}
		wss, err := o.st.ListWorkspacesByObjective(obj.ID)
		if err != nil {
			continue
		}
		if obj.Status != model.ObjectiveActive {
			for _, ws := range wss {
				if !inUse[ws.ID] {
					o.reclaimWorkspace(ctx, ws)
				}
			}
			continue
		}
		o.reclaimSpentWorkerCheckouts(ctx, obj, sessions, wss, inUse)
	}
}

// reclaimSpentWorkerCheckouts reclaims a finished worker's private isolated
// checkout mid-objective, once it is provably done being needed. The trace that
// motivated this (see reclaim.go history / disk-full incident) found exactly four
// ways an isolated checkout can still be touched while its objective is active —
// each is excluded here:
//
//   - The manager's own long-lived checkout (RoleManager, holds its lock for life).
//   - A not-yet-published worker checkout the manager may still publish a PR from.
//     Once a PR exists for the branch the checkout's job is done: follow-ups run in
//     a separate pr_branch workspace, never the isolated one.
//   - A checkout a pending dependent will inherit (dependents inherit a Ready
//     isolated checkout, and run only after their dependency is terminal).
//   - A shared or pr_branch checkout (only WorkspaceIsolated is ever reclaimed here).
func (o *Orchestrator) reclaimSpentWorkerCheckouts(ctx context.Context, obj *model.Objective, sessions []*model.Session, wss []*model.Workspace, inUse map[string]bool) {
	// Branches/sessions a PR has already been published from — the signal that an
	// isolated worker checkout has served its purpose.
	publishedBranch := map[string]bool{}
	publishedBySession := map[string]bool{}
	if prs, err := o.st.ListPRsByObjective(obj.ID); err == nil {
		for _, pr := range prs {
			if pr.Branch != "" {
				publishedBranch[pr.Branch] = true
			}
			if pr.CreatedBySessionID != "" {
				publishedBySession[pr.CreatedBySessionID] = true
			}
		}
	}
	// Sessions a still-live session depends on — a pending dependent will inherit
	// that predecessor's checkout when it runs, so it must be kept.
	neededByPendingDependent := map[string]bool{}
	for _, s := range sessions {
		if s.Status.IsTerminal() {
			continue
		}
		for _, dep := range dependencyIDs(s) {
			neededByPendingDependent[dep] = true
		}
	}

	for _, ws := range wss {
		if inUse[ws.ID] || ws.Kind != model.WorkspaceIsolated || ws.SessionID == "" {
			continue
		}
		if neededByPendingDependent[ws.SessionID] {
			continue
		}
		owner, err := o.st.GetSession(ws.SessionID)
		if err != nil || owner == nil || owner.Role == model.RoleManager || !owner.Status.IsTerminal() {
			continue
		}
		spent := publishedBySession[ws.SessionID] || (ws.BranchName != "" && publishedBranch[ws.BranchName])
		if !spent {
			continue
		}
		o.reclaimWorkspace(ctx, ws)
	}
}

// reclaimWorkspace removes one workspace's checkout dir on its target and marks
// it archived. Already-archived/preparing workspaces are skipped. On a reachable
// target whose rm fails (a transient SSH hiccup) the workspace is left for a
// later pass to retry rather than marked archived; when the target itself is gone
// there is nothing to remove, so it is archived to stop retrying.
func (o *Orchestrator) reclaimWorkspace(ctx context.Context, ws *model.Workspace) {
	if ws.Status != model.WorkspaceReady && ws.Status != model.WorkspaceFailed {
		return
	}
	if ws.Path == "" || ws.TargetID == "" {
		_ = o.st.SetWorkspaceStatus(ws.ID, model.WorkspaceArchived)
		return
	}
	tgt, err := o.st.GetTarget(ws.TargetID)
	if err != nil {
		// Target is gone — the dir is unreachable. Archive to stop retrying.
		_ = o.st.SetWorkspaceStatus(ws.ID, model.WorkspaceArchived)
		return
	}
	ex := agent.NewExecutor(tgt)
	if _, err := exec.RunCapture(ctx, ex, exec.Command{
		Name: "rm", Args: []string{"-rf", ws.Path},
	}); err != nil {
		// Reachable target but rm failed; leave it for the next pass to retry.
		return
	}
	_ = o.st.SetWorkspaceStatus(ws.ID, model.WorkspaceArchived)
	o.audit(ws.ObjectiveID, "", "workspace_reclaimed",
		"removed checkout "+ws.Path, model.JSONMap{"workspace_id": ws.ID, "target_id": ws.TargetID})
}
