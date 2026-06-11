package store

import (
	"database/sql"
	"errors"

	"github.com/nathanwhit/orcha/internal/model"
)

// CreateWorkspace inserts a new workspace.
func (s *Store) CreateWorkspace(w *model.Workspace) error {
	if w.ID == "" {
		w.ID = s.NewID()
	}
	now := s.now()
	if w.CreatedAt.IsZero() {
		w.CreatedAt = now
	}
	w.UpdatedAt = now
	if w.Status == "" {
		w.Status = model.WorkspacePreparing
	}
	_, err := s.db.Exec(
		`INSERT INTO workspaces(id, objective_id, session_id, target_id, kind,
		   project_path, vcs, path, base_ref, base_sha, branch_name, status,
		   created_at, updated_at, metadata)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.ID, nullStr(w.ObjectiveID), nullStr(w.SessionID), w.TargetID, string(w.Kind),
		w.ProjectPath, string(w.VCS), w.Path, w.BaseRef, w.BaseSHA, nullStr(w.BranchName),
		string(w.Status), w.CreatedAt, w.UpdatedAt, w.Metadata,
	)
	return err
}

var workspaceCols = `id, objective_id, session_id, target_id, kind, project_path,
	vcs, path, base_ref, base_sha, branch_name, status, created_at, updated_at, metadata`

func scanWorkspace(r rowScanner) (*model.Workspace, error) {
	var w model.Workspace
	var obj, sess, branch sql.NullString
	err := r.Scan(&w.ID, &obj, &sess, &w.TargetID, &w.Kind, &w.ProjectPath, &w.VCS,
		&w.Path, &w.BaseRef, &w.BaseSHA, &branch, &w.Status, &w.CreatedAt, &w.UpdatedAt, &w.Metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	w.ObjectiveID = obj.String
	w.SessionID = sess.String
	w.BranchName = branch.String
	return &w, nil
}

// GetWorkspace fetches one workspace.
func (s *Store) GetWorkspace(id string) (*model.Workspace, error) {
	row := s.db.QueryRow(`SELECT `+workspaceCols+` FROM workspaces WHERE id = ?`, id)
	return scanWorkspace(row)
}

// ListWorkspacesByObjective returns workspaces for an objective.
func (s *Store) ListWorkspacesByObjective(objectiveID string) ([]*model.Workspace, error) {
	rows, err := s.db.Query(`SELECT `+workspaceCols+` FROM workspaces WHERE objective_id = ?`, objectiveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SharedScratchFor returns the objective's shared scratch workspace on a target,
// if one exists. Each objective may have one shared scratch per target.
func (s *Store) SharedScratchFor(objectiveID, targetID string) (*model.Workspace, error) {
	row := s.db.QueryRow(
		`SELECT `+workspaceCols+` FROM workspaces
		 WHERE objective_id = ? AND target_id = ? AND kind = ? LIMIT 1`,
		objectiveID, targetID, string(model.WorkspaceShared))
	return scanWorkspace(row)
}

// SetWorkspaceStatus updates a workspace's lifecycle status.
func (s *Store) SetWorkspaceStatus(id string, status model.WorkspaceStatus) error {
	_, err := s.db.Exec(`UPDATE workspaces SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), s.now(), id)
	return err
}
