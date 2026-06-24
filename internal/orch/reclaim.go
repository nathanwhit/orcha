package orch

import (
	"context"
	"fmt"
	"strings"

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
//   - While the objective is still ACTIVE, a finished session's PRIVATE isolated
//     checkout is reclaimed once it is provably spent (see reclaimSpentCheckouts).
//     This matters because objectives can stay active for hours/days while
//     spawning many multi-GB checkouts; the old objective-only gating let every
//     finished checkout sit on disk for the whole objective and filled the target.
//     A reviewer/validator/dead-manager checkout is spent as soon as it goes
//     terminal; a worker checkout the manager may still publish from is kept until
//     a PR exists for its branch. Anything that could still be read/written within
//     an active objective — the live manager's checkout, a not-yet-published
//     worker checkout, a checkout a pending dependent will inherit, or a
//     shared/pr_branch checkout — is left alone.
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
		o.reclaimSpentCheckouts(ctx, obj, sessions, wss, inUse)
	}
}

// reclaimSpentCheckouts reclaims a finished session's private isolated checkout
// mid-objective, once it is provably done being needed. The trace that motivated
// this (see reclaim.go history / disk-full incident) found exactly four ways an
// isolated checkout can still be touched while its objective is active — each is
// excluded here:
//
//   - The live manager's own long-lived checkout. An active manager is never
//     terminal, so the IsTerminal gate below already excludes it; a *dead*
//     manager's checkout is spent (a respawned manager always gets a fresh
//     checkout via CreateSession, never inherits the old one), so it is reclaimed.
//   - A not-yet-published worker checkout the manager may still publish a PR from.
//     Once a PR exists for the branch the checkout's job is done: follow-ups run in
//     a separate pr_branch workspace, never the isolated one. This publish gate
//     applies ONLY to roles that author a branch/PR (see rolePublishesPR);
//     reviewers, validators and dead managers never publish, so theirs are
//     reclaimed the moment they go terminal — these are the bulk of the churn that
//     filled the box (a single failed reviewer's WPT + build checkout was 47G).
//   - A checkout a pending dependent will inherit (dependents inherit a Ready
//     isolated checkout, and run only after their dependency is terminal).
//   - A shared or pr_branch checkout (only WorkspaceIsolated is ever reclaimed here).
func (o *Orchestrator) reclaimSpentCheckouts(ctx context.Context, obj *model.Objective, sessions []*model.Session, wss []*model.Workspace, inUse map[string]bool) {
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
		if err != nil || owner == nil || !owner.Status.IsTerminal() {
			continue
		}
		// Only roles that author their own branch/PR need the publish gate — the
		// manager might still publish from them. Reviewers, validators and dead
		// managers never publish, so a terminal one is immediately spent. But a
		// reviewer is told to commit any fix it makes, and that commit lives ONLY on
		// this ephemeral checkout's branch — never pushed or merged — so reclaiming
		// a checkout whose branch advanced past its base would silently destroy it.
		// Keep it instead; disk is the cheaper loss. (Published-PR checkouts are
		// exempt: their commits are safely on the PR, which is the whole point of the
		// publish gate.)
		if rolePublishesPR(owner.Role) {
			spent := publishedBySession[ws.SessionID] || (ws.BranchName != "" && publishedBranch[ws.BranchName])
			if !spent {
				continue
			}
		} else if o.checkoutHasUnmergedCommits(ctx, ws) {
			o.audit(ws.ObjectiveID, ws.SessionID, "reclaim_skipped_unmerged",
				"kept "+string(owner.Role)+" checkout: branch has commits not on any published PR",
				model.JSONMap{"workspace_id": ws.ID, "branch": ws.BranchName})
			continue
		}
		o.reclaimWorkspace(ctx, ws)
	}
}

// checkoutHasUnmergedCommits reports whether ws's branch has commits beyond the
// base it was cut from — work that exists ONLY in this checkout. A reviewer that
// "fixes and commits" leaves the fix on its review branch, never pushed or merged,
// so this is the guard against reclaiming (and thus destroying) it. It returns
// false when there is provably nothing to lose (the dir is gone, or HEAD is still
// the base) and true otherwise, including on a transient probe failure with the
// dir still present — keeping a checkout costs disk; dropping one costs the work.
func (o *Orchestrator) checkoutHasUnmergedCommits(ctx context.Context, ws *model.Workspace) bool {
	if ws.BaseSHA == "" || ws.Path == "" || ws.TargetID == "" {
		return false // nothing to compare against — preserve prior reclaim behavior
	}
	tgt, err := o.st.GetTarget(ws.TargetID)
	if err != nil {
		return false // target gone → the dir is unreachable, nothing to keep
	}
	ex := agent.NewExecutor(tgt)
	// One round-trip that distinguishes "gone/not-a-repo" (reclaim) from a real
	// HEAD (compare) from an unreachable target (keep, via RunCapture error).
	script := fmt.Sprintf(`if [ ! -e %q ]; then echo GONE; elif [ ! -d %q/.git ]; then echo NOGIT; else git -C %q rev-parse HEAD; fi`,
		ws.Path, ws.Path, ws.Path)
	out, err := exec.RunCapture(ctx, ex, exec.Command{Name: "sh", Args: []string{"-c", script}})
	if err != nil {
		return true // could not probe a present checkout — don't risk destroying work
	}
	head := strings.TrimSpace(out)
	switch head {
	case "GONE", "NOGIT", "":
		return false
	default:
		return head != ws.BaseSHA
	}
}

// rolePublishesPR reports whether a session of this role authors a branch the
// manager may still publish a PR from after the session finishes. Such a checkout
// must be kept until a PR exists for its branch; all other roles' checkouts are
// spent the moment they go terminal.
func rolePublishesPR(role model.SessionRole) bool {
	switch role {
	case model.RoleImplementer, model.RoleCustom, model.RoleResearcher:
		return true
	default:
		return false
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
