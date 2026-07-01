package orch

import (
	"context"
	"fmt"
	"strings"
	"time"

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
//
// It also reclaims a pr_branch checkout once its PR is TERMINAL (merged/closed),
// even while the objective is still active. A terminal PR never gets another
// follow-up, so its checkout is spent — and a follow-up always pushes its commits
// to the PR, so a terminal PR's branch has nothing unpushed to lose. This is the
// exact disk-full incident: a closed PR's multi-GB pr_branch checkout sat on the
// orch host for hours because the objective stayed active. An OPEN PR's checkout
// is kept (a new follow-up may reuse it). pr_branch reclaim is PATH-aware: many
// workspace rows across re-preps share one dir, so a dir a live session still
// occupies is never yanked. Shared/scratch checkouts are never reclaimed here.
func (o *Orchestrator) reclaimSpentCheckouts(ctx context.Context, obj *model.Objective, sessions []*model.Session, wss []*model.Workspace, inUse map[string]bool) {
	// Branches/sessions a PR has already been published from — the signal that an
	// isolated worker checkout has served its purpose.
	publishedBranch := map[string]bool{}
	publishedBySession := map[string]bool{}
	// Branches whose PR is terminal (merged/closed): their pr_branch checkouts are
	// spent the moment the PR closes, even mid-objective.
	terminalPRBranch := map[string]bool{}
	if prs, err := o.st.ListPRsByObjective(obj.ID); err == nil {
		for _, pr := range prs {
			if pr.Branch != "" {
				publishedBranch[pr.Branch] = true
				if pr.Status == model.PRMerged || pr.Status == model.PRClosed {
					terminalPRBranch[pr.Branch] = true
				}
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
	// Paths a live (non-terminal) session still occupies. pr_branch checkouts are
	// shared by PATH across re-preps (many rows, one on-disk dir), so reclaim must
	// key on the path, not just the workspace id, or it could rm a dir out from
	// under a running follow-up that holds a different row on the same path.
	inUsePaths := map[string]bool{}
	for _, ws := range wss {
		if inUse[ws.ID] && ws.Path != "" {
			inUsePaths[ws.Path] = true
		}
	}

	for _, ws := range wss {
		if inUse[ws.ID] || ws.SessionID == "" {
			continue
		}
		if neededByPendingDependent[ws.SessionID] {
			continue
		}
		switch ws.Kind {
		case model.WorkspaceIsolated:
			owner, err := o.st.GetSession(ws.SessionID)
			if err != nil || owner == nil || !owner.Status.IsTerminal() {
				continue
			}
			// Only roles that author their own branch/PR need the publish gate — the
			// manager might still publish from them. Reviewers, validators and dead
			// managers never publish, so a terminal one is immediately spent. But a
			// reviewer is told to commit any fix it makes, and that commit lives ONLY
			// on this ephemeral checkout's branch — never pushed or merged — so
			// reclaiming a checkout whose branch advanced past its base would silently
			// destroy it. Keep it instead; disk is the cheaper loss. (Published-PR
			// checkouts are exempt: their commits are safely on the PR, which is the
			// whole point of the publish gate.)
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
		case model.WorkspacePRBranch:
			// Reclaim only when the PR this branch belongs to is terminal
			// (merged/closed) and no live session still occupies the shared dir. A
			// follow-up pushes its commits to the PR, so a terminal PR's checkout has
			// nothing unpushed to lose; an open PR's is kept for a future follow-up.
			// (checkoutHasUnmergedCommits is deliberately NOT used here: a pr_branch's
			// BaseSHA is the pre-follow-up PR head, so it reads "has commits" after any
			// successful pushed follow-up — the terminal-PR gate is the safe signal.)
			if ws.BranchName == "" || !terminalPRBranch[ws.BranchName] {
				continue
			}
			if inUsePaths[ws.Path] {
				continue
			}
			o.reclaimWorkspace(ctx, ws)
		}
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

// cacheGCInterval is the minimum time between bare-mirror gc runs on one target.
// gc repacks a multi-GB mirror, and the mirror only grows slowly, so it runs far
// less often than checkout reclaim. Throttled per target.
var cacheGCInterval = 6 * time.Hour

// MaintainCaches runs `git gc` on each schedulable target's bare mirror caches so
// the shared mirror — fetched into on every checkout — does not grow without
// bound. It was observed at 21G (~a third of a 74G orch-host disk) because gc
// never ran: reclaim deliberately leaves the mirror in place (it's reused), but
// "left in place" silently became "never compacted". gc takes the repo's own
// gc.pid lock, so it is safe to run while checkouts clone from / fetch into the
// same mirror; the default prune grace avoids racing a just-created object. It is
// throttled per target via cacheGCInterval and a no-op without a real preparer.
func (o *Orchestrator) MaintainCaches(ctx context.Context) {
	if o.preparer == nil {
		return // no real checkouts (e.g. unit tests): no mirror to maintain
	}
	sub := o.preparer.CacheSubdir
	if sub == "" {
		sub = ".orcha-cache"
	}
	targets, err := o.st.ListTargets()
	if err != nil {
		return
	}
	now := o.st.Now()
	for _, t := range targets {
		if !t.Status.CanSchedule() || t.WorkRoot == "" {
			continue
		}
		o.cacheGCMu.Lock()
		last := o.lastCacheGC[t.ID]
		due := last.IsZero() || now.Sub(last) >= cacheGCInterval
		if due {
			o.lastCacheGC[t.ID] = now
		}
		o.cacheGCMu.Unlock()
		if !due {
			continue
		}
		go o.gcTargetCaches(ctx, t, sub)
	}
}

// gcTargetCaches runs `git gc` on every bare mirror under a target's cache dir.
// One generous-timeout shell command per target; failures are non-fatal (the
// mirror just stays larger until the next pass).
func (o *Orchestrator) gcTargetCaches(ctx context.Context, t *model.Target, sub string) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cacheDir := strings.TrimRight(t.WorkRoot, "/") + "/" + sub
	// Glob the mirrors but quote the dir prefix so an unusual work root can't break
	// the loop; gc --quiet keeps a clean run silent.
	script := fmt.Sprintf(`for d in %s/*.git; do [ -d "$d" ] || continue; git -C "$d" gc --quiet 2>/dev/null; done`, shQuote(cacheDir))
	if _, err := exec.RunCapture(cctx, agent.NewExecutor(t), exec.Command{Name: "sh", Args: []string{"-c", script}}); err != nil {
		return
	}
	o.audit("", "", "cache_gc", "compacted bare mirror cache on "+t.Name, model.JSONMap{"target_id": t.ID})
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
