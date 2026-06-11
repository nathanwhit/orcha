package orch

import (
	"fmt"

	"github.com/nathanwhit/orcha/internal/model"
)

// PrepareIsolatedWorkspace creates an isolated workspace for an implementation
// session on a target. Each implementation session gets its own isolated
// workspace (single writer). The session is bound to the workspace.
func (o *Orchestrator) PrepareIsolatedWorkspace(sessionID, repo, projectPath, baseRef string) (*model.Workspace, error) {
	sess, err := o.st.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	target, err := o.SelectTarget(TargetRequest{ProjectPath: projectPath})
	if err != nil {
		return nil, err
	}
	branch := "orcha/" + roleShort(sess.Role) + "-" + sessionID[:min(8, len(sessionID))]
	ws := &model.Workspace{
		ObjectiveID: sess.ObjectiveID,
		SessionID:   sessionID,
		TargetID:    target.ID,
		Kind:        model.WorkspaceIsolated,
		ProjectPath: projectPath,
		VCS:         model.VCSGit,
		Path:        fmt.Sprintf("%s/%s", target.WorkRoot, sessionID),
		BaseRef:     baseRef,
		BranchName:  branch,
		Status:      model.WorkspaceReady,
		Metadata:    model.JSONMap{"repo": repo},
	}
	if err := o.st.CreateWorkspace(ws); err != nil {
		return nil, err
	}
	if _, err := o.st.UpdateSessionRuntime(sessionID, func(s *model.Session) {
		s.WorkspaceID = ws.ID
	}); err != nil {
		return nil, err
	}
	return ws, nil
}

func roleShort(r model.SessionRole) string {
	if len(r) > 4 {
		return string(r)[:4]
	}
	return string(r)
}
