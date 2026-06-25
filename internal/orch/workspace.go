package orch

import (
	"context"
	"fmt"
	"strings"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/workspace"
)

// PrepareIsolatedWorkspace creates an isolated workspace for an implementation
// session on a target. Each implementation session gets its own isolated
// workspace (single writer). When a workspace preparer is installed, the
// checkout is materialized on the target as a fresh git clone branched off the
// latest upstream base; otherwise only the workspace row is recorded.
//
// repo is the code-host identifier (e.g. "owner/repo"); cloneURL is the git
// source (a URL, or a local path in tests). If cloneURL is empty it is derived
// from repo.
func (o *Orchestrator) PrepareIsolatedWorkspace(ctx context.Context, sessionID, repo, cloneURL, baseRef string) (*model.Workspace, error) {
	sess, err := o.st.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	target, err := o.SelectTarget(TargetRequest{ProjectPath: repo})
	if err != nil {
		return nil, err
	}
	return o.prepareIsolatedOn(ctx, sess, target, repo, "", cloneURL, baseRef)
}

// prepareIsolatedOn prepares an isolated workspace for a session on a specific
// target — used when the session has already been placed, so the checkout lands
// on the same machine the agent runs on. pushRepo, when set, is the fork the
// branch will be pushed to (fork workflow): the checkout fetches from repo but
// pushes to pushRepo.
func (o *Orchestrator) prepareIsolatedOn(ctx context.Context, sess *model.Session, target *model.Target, repo, pushRepo, cloneURL, baseRef string) (*model.Workspace, error) {
	sessionID := sess.ID
	if baseRef == "" {
		baseRef = "main"
	}
	if cloneURL == "" {
		cloneURL = cloneURLFor(repo)
	}
	branch := "orcha/" + roleShort(sess.Role) + "-" + sessionID[:min(8, len(sessionID))]
	dir := fmt.Sprintf("%s/%s", target.WorkRoot, sessionID)

	wsMeta := model.JSONMap{"repo": repo, "clone_url": cloneURL}
	if pushRepo != "" {
		wsMeta["push_repo"] = pushRepo
	}
	ws := &model.Workspace{
		ObjectiveID: sess.ObjectiveID,
		SessionID:   sessionID,
		TargetID:    target.ID,
		Kind:        model.WorkspaceIsolated,
		ProjectPath: repo,
		VCS:         model.VCSGit,
		Path:        dir,
		BaseRef:     baseRef,
		BranchName:  branch,
		Status:      model.WorkspacePreparing,
		Metadata:    wsMeta,
	}
	if err := o.st.CreateWorkspace(ws); err != nil {
		return nil, err
	}
	if _, err := o.st.UpdateSessionRuntime(sessionID, func(s *model.Session) {
		s.WorkspaceID = ws.ID
	}); err != nil {
		return nil, err
	}

	// Materialize the real checkout (target-local, fresh upstream). This is an
	// external operation and runs outside any DB transaction.
	if o.preparer != nil {
		pushURL := ""
		if pushRepo != "" {
			pushURL = cloneURLFor(pushRepo)
		}
		ex := agent.NewExecutor(target)
		perr := o.preparer.PrepareIsolated(ctx, ex, workspace.Spec{
			WorkRoot: target.WorkRoot, RepoURL: cloneURL, Dir: dir, Base: baseRef, Branch: branch,
			PushURL: pushURL,
			// A manager only reads the superproject to scope work; it never builds or
			// runs the suites that live in submodules. Submodule materialization is
			// the bulk of prep time (minutes on denoland/deno) and it all sits on the
			// manager's startup critical path, so skip it — the manager comes up in
			// seconds. Coding workers still get full submodules.
			SkipSubmodules: sess.Role == model.RoleManager,
		})
		if perr != nil {
			_ = o.st.SetWorkspaceStatus(ws.ID, model.WorkspaceFailed)
			o.audit(sess.ObjectiveID, sessionID, "workspace_prepare_failed", perr.Error(), model.JSONMap{"workspace_id": ws.ID})
			return ws, perr
		}
	}
	_ = o.st.SetWorkspaceStatus(ws.ID, model.WorkspaceReady)
	ws.Status = model.WorkspaceReady
	return ws, nil
}

// cloneURLFor derives a git clone URL from a repo identifier. A value that
// already looks like a URL or absolute path is returned unchanged; "owner/repo"
// becomes an https GitHub URL (git's credential helper / gh auth handles creds).
func cloneURLFor(repo string) string {
	if strings.Contains(repo, "://") || strings.Contains(repo, "@") || strings.HasPrefix(repo, "/") {
		return repo
	}
	return "https://github.com/" + repo + ".git"
}

func roleShort(r model.SessionRole) string {
	if len(r) > 4 {
		return string(r)[:4]
	}
	return string(r)
}

// needsIsolatedWorkspace reports whether a role does code work that should get
// its own fresh checkout. Managers work from summaries; PR follow-ups get a
// PR-branch workspace via the feedback path instead.
func needsIsolatedWorkspace(role model.SessionRole) bool {
	switch role {
	case model.RoleImplementer, model.RoleReviewer, model.RoleValidator, model.RoleCustom:
		return true
	}
	return false
}

// isCodingWorker reports whether a role runs one-shot in a checkout with edit
// permissions: the isolated-workspace roles plus PR/CI follow-ups (which get a
// PR-branch workspace instead).
func isCodingWorker(role model.SessionRole) bool {
	if needsIsolatedWorkspace(role) {
		return true
	}
	switch role {
	case model.RolePRFollowup, model.RoleCIFollowup:
		return true
	}
	return false
}

// ensureWorkspace auto-prepares an isolated checkout for a coding session on its
// already-chosen target, if it has none yet. The repo is taken from the session
// metadata (a spawn override) or inherited from the objective. It is a no-op
// when no preparer is configured, the role doesn't need a checkout, or no repo
// is known — so non-code work and offline/test runs are unaffected.
//
// target must be the session's placed target so the checkout and the agent run
// on the same machine.
func (o *Orchestrator) ensureWorkspace(ctx context.Context, sess *model.Session, target *model.Target) error {
	if sess.WorkspaceID != "" || o.preparer == nil || target == nil {
		return nil
	}
	repo, pushRepo, cloneURL, base := o.resolveRepo(sess)

	// The manager gets a checkout too, when there is one to give: grounded in
	// the actual code it scopes work precisely (real file references) instead
	// of asking the user things the repo answers. Unlike coding workers, no
	// repo is not an error — it runs from a scratch dir and can ask_user. Its
	// checkout skips submodules (prepareIsolatedOn): they're the bulk of prep
	// time and a manager reads only the superproject, so this keeps manager
	// startup to seconds instead of minutes.
	if sess.Role == model.RoleManager {
		if repo == "" && cloneURL == "" {
			return nil
		}
		_, err := o.prepareIsolatedOn(ctx, sess, target, repo, pushRepo, cloneURL, base)
		return err
	}

	if !needsIsolatedWorkspace(sess.Role) {
		return nil
	}

	// A worker that depends on prior work continues that work's checkout instead
	// of cloning a clean tree off the base. Without this a validator/fixer spawned
	// after an implementer gets a fresh branch off main, never sees the
	// implementer's commits, and reports "nothing changed" — so the manager
	// thinks the work failed and cancels/re-spawns. Inheriting the predecessor's
	// workspace keeps the chain on one coherent branch. The dependent was placed
	// on the predecessor's target (targetRequestFor pins it), so the checkout is
	// local; dependencies run in order, so there is no concurrent writer.
	if dep := o.dependencyWorkspace(sess); dep != nil && (sess.TargetID == "" || sess.TargetID == dep.TargetID) {
		if _, err := o.st.UpdateSessionRuntime(sess.ID, func(s *model.Session) {
			s.WorkspaceID = dep.ID
		}); err != nil {
			return err
		}
		o.audit(sess.ObjectiveID, sess.ID, "workspace_inherited",
			"continuing "+dep.BranchName+" from a dependency", model.JSONMap{"workspace_id": dep.ID})
		return nil
	}

	if repo == "" && cloneURL == "" {
		// A coding worker with nothing to clone must fail loudly here: the
		// fallback would be an empty scratch dir it can't do its task in (and
		// historically, the orchestrator's own cwd — the operator's live repo).
		return fmt.Errorf("orch: %s session has no repo to work on: set repo on the objective or pass repo in spawn_session", sess.Role)
	}
	_, err := o.prepareIsolatedOn(ctx, sess, target, repo, pushRepo, cloneURL, base)
	return err
}

// dependencyWorkspace returns the workspace a dependent session should continue:
// the first dependency (depends_on) that has a ready isolated checkout. This is
// how sequential workers (implement -> validate -> fix) build on each other's
// work rather than each starting from a clean tree. Returns nil when the session
// has no dependency with a usable workspace (so it gets its own fresh checkout).
func (o *Orchestrator) dependencyWorkspace(sess *model.Session) *model.Workspace {
	for _, depID := range dependencyIDs(sess) {
		dep, err := o.st.GetSession(depID)
		if err != nil || dep.WorkspaceID == "" {
			continue
		}
		// Never continue the manager's checkout: it is long-lived and holds its
		// workspace lock for its whole life, so a dependent would block forever.
		if dep.Role == model.RoleManager {
			continue
		}
		ws, err := o.st.GetWorkspace(dep.WorkspaceID)
		if err != nil {
			continue
		}
		if ws.Kind == model.WorkspaceIsolated && ws.Status == model.WorkspaceReady {
			return ws
		}
	}
	return nil
}

// resolveRepo finds the repo/push-repo/clone-url/base for a session: a
// per-session override in its metadata wins, otherwise the objective's
// defaults. pushRepo is the fork branches are pushed to (fork workflow);
// empty means pushes go to repo itself.
func (o *Orchestrator) resolveRepo(sess *model.Session) (repo, pushRepo, cloneURL, base string) {
	repo, _ = sess.Metadata["repo"].(string)
	pushRepo, _ = sess.Metadata["push_repo"].(string)
	cloneURL, _ = sess.Metadata["clone_url"].(string)
	base, _ = sess.Metadata["base_branch"].(string)
	if sess.ObjectiveID == "" {
		return repo, pushRepo, cloneURL, base
	}
	obj, err := o.st.GetObjective(sess.ObjectiveID)
	if err != nil {
		return repo, pushRepo, cloneURL, base
	}
	objRepo, _ := obj.Metadata["repo"].(string)
	// Fill the repo source from the objective when the session names none.
	if repo == "" && cloneURL == "" {
		repo = objRepo
		cloneURL, _ = obj.Metadata["clone_url"].(string)
	}
	// push_repo (the fork orcha pushes worker branches to) and base_branch are
	// objective-wide settings, independent of which repo a worker checks out.
	// Inherit them when the session left them unset and is working on the
	// objective's repo. Without this, a worker that overrides only base_branch
	// (e.g. to build on another worker's fork-hosted branch) redundantly passing
	// the same repo would silently lose the fork and look for the base on origin.
	if repo == "" || objRepo == "" || repo == objRepo {
		if pushRepo == "" {
			pushRepo, _ = obj.Metadata["push_repo"].(string)
		}
		if base == "" {
			base, _ = obj.Metadata["base_branch"].(string)
		}
	}
	return repo, pushRepo, cloneURL, base
}
