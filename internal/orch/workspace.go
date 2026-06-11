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
	if baseRef == "" {
		baseRef = "main"
	}
	if cloneURL == "" {
		cloneURL = cloneURLFor(repo)
	}
	branch := "orcha/" + roleShort(sess.Role) + "-" + sessionID[:min(8, len(sessionID))]
	dir := fmt.Sprintf("%s/%s", target.WorkRoot, sessionID)

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
		Metadata:    model.JSONMap{"repo": repo, "clone_url": cloneURL},
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
		ex := agent.NewExecutor(target)
		perr := o.preparer.PrepareIsolated(ctx, ex, workspace.Spec{
			WorkRoot: target.WorkRoot, RepoURL: cloneURL, Dir: dir, Base: baseRef, Branch: branch,
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
