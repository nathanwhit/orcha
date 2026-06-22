package store

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/nathanwhit/orcha/internal/model"
)

// UpsertProject registers a repository, keyed by repo: registering a repo that
// already exists updates it in place (empty fields keep their old values), so
// creating an objective with a typed repo "remembers" it for next time.
func (s *Store) UpsertProject(p *model.Project) error {
	now := s.Now()
	if existing, err := s.GetProjectByRepo(p.Repo); err == nil {
		p.ID = existing.ID
		p.CreatedAt = existing.CreatedAt
		if p.Name == "" {
			p.Name = existing.Name
		}
		if p.PushRepo == "" {
			p.PushRepo = existing.PushRepo
		}
		if p.CloneURL == "" {
			p.CloneURL = existing.CloneURL
		}
		if p.BaseBranch == "" {
			p.BaseBranch = existing.BaseBranch
		}
		p.UpdatedAt = now
		_, err := s.db.Exec(
			`UPDATE projects SET name=?, push_repo=?, clone_url=?, base_branch=?, updated_at=? WHERE id=?`,
			p.Name, p.PushRepo, p.CloneURL, p.BaseBranch, now, p.ID)
		return err
	}
	if p.ID == "" {
		p.ID = s.NewID()
	}
	if p.Name == "" {
		p.Name = p.Repo
	}
	p.CreatedAt, p.UpdatedAt = now, now
	_, err := s.db.Exec(
		`INSERT INTO projects(id, name, repo, push_repo, clone_url, base_branch, review_gate, review_guidance, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Repo, p.PushRepo, p.CloneURL, p.BaseBranch, b2i(p.ReviewGate), p.ReviewGuidance, now, now)
	return err
}

// UpdateProject performs a full update of an existing project by id, setting
// every editable field explicitly so empty fields actually clear — unlike
// UpsertProject, which is keyed by repo and preserves empty values. This is the
// editing path: it can also change the repo. If name is empty it defaults to the
// repo (consistent with UpsertProject). It returns ErrNotFound when no project
// has the given id, and ErrConflict when the new repo collides with another
// project (the repo column is UNIQUE).
func (s *Store) UpdateProject(p *model.Project) error {
	if p.Name == "" {
		p.Name = p.Repo
	}
	p.UpdatedAt = s.Now()
	res, err := s.db.Exec(
		`UPDATE projects SET name=?, repo=?, push_repo=?, clone_url=?, base_branch=?, review_gate=?, review_guidance=?, updated_at=? WHERE id=?`,
		p.Name, p.Repo, p.PushRepo, p.CloneURL, p.BaseBranch, b2i(p.ReviewGate), p.ReviewGuidance, p.UpdatedAt, p.ID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrConflict
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

var projectCols = `id, name, repo, push_repo, clone_url, base_branch, review_gate, review_guidance, created_at, updated_at`

func scanProject(r rowScanner) (*model.Project, error) {
	var p model.Project
	var reviewGate int64 // SQLite stores the bool as 0/1; scan via int then convert
	err := r.Scan(&p.ID, &p.Name, &p.Repo, &p.PushRepo, &p.CloneURL, &p.BaseBranch, &reviewGate, &p.ReviewGuidance, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.ReviewGate = reviewGate != 0
	return &p, nil
}

// b2i maps a bool to the 0/1 SQLite stores for it.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetProject fetches a project by id.
func (s *Store) GetProject(id string) (*model.Project, error) {
	return scanProject(s.db.QueryRow(`SELECT `+projectCols+` FROM projects WHERE id = ?`, id))
}

// GetProjectByRepo fetches a project by its repo identifier.
func (s *Store) GetProjectByRepo(repo string) (*model.Project, error) {
	return scanProject(s.db.QueryRow(`SELECT `+projectCols+` FROM projects WHERE repo = ?`, repo))
}

// ListProjects returns all registered projects, alphabetically by name.
func (s *Store) ListProjects() ([]*model.Project, error) {
	rows, err := s.db.Query(`SELECT ` + projectCols + ` FROM projects ORDER BY name COLLATE NOCASE ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteProject removes a project from the registry. Objectives that used it
// are unaffected (they carry their own repo metadata).
func (s *Store) DeleteProject(id string) error {
	_, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	return err
}
