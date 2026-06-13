package orch

import (
	"context"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// ReclaimWorkspaces removes the on-disk checkout of every workspace that is no
// longer needed and marks it archived, reclaiming disk on long-lived targets
// where per-session checkouts otherwise accumulate forever. It is deliberately
// conservative: a workspace is only reclaimed once its OBJECTIVE is terminal AND
// no non-terminal session still references it. This sidesteps the sharing hazard
// — dependent workers inherit a dependency's checkout, the manager publishes a
// PR from a worker's checkout, follow-ups reuse a PR-branch checkout — by never
// touching a workspace while its objective could still do work. The shared bare
// mirror cache is left in place (it is reused across checkouts and stays small).
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
		if obj.Status == model.ObjectiveActive {
			continue // active objectives may still publish/inherit these checkouts
		}
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
		for _, ws := range wss {
			if !inUse[ws.ID] {
				o.reclaimWorkspace(ctx, ws)
			}
		}
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
