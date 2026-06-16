package store

import (
	"github.com/nathanwhit/orcha/internal/model"
)

// ListRepoMemoryFiles returns every memory file for a repo key, ordered by path
// (so the index, "MEMORY.md", and topic files come back deterministically). An
// unseen repo returns an empty slice, not an error.
func (s *Store) ListRepoMemoryFiles(repo string) ([]*model.RepoMemoryFile, error) {
	rows, err := s.db.Query(
		`SELECT repo, path, content, updated_at FROM repo_memory WHERE repo = ? ORDER BY path ASC`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.RepoMemoryFile
	for rows.Next() {
		var m model.RepoMemoryFile
		if err := rows.Scan(&m.Repo, &m.Path, &m.Content, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// UpsertRepoMemoryFile writes one memory file for a repo key, inserting or
// replacing the row for that (repo, path).
func (s *Store) UpsertRepoMemoryFile(repo, path, content string) error {
	_, err := s.db.Exec(
		`INSERT INTO repo_memory(repo, path, content, updated_at) VALUES(?,?,?,?)
		 ON CONFLICT(repo, path) DO UPDATE SET content=excluded.content, updated_at=excluded.updated_at`,
		repo, path, content, s.now())
	return err
}

// DeleteRepoMemoryFile removes one memory file — used when an agent deletes it
// from the checkout and no concurrent session changed it.
func (s *Store) DeleteRepoMemoryFile(repo, path string) error {
	_, err := s.db.Exec(`DELETE FROM repo_memory WHERE repo = ? AND path = ?`, repo, path)
	return err
}

// ListAllRepoMemory returns every repo's memory files, newest first within each
// repo — for the dashboard and operator surface to inspect what the team has
// learned.
func (s *Store) ListAllRepoMemory() ([]*model.RepoMemoryFile, error) {
	rows, err := s.db.Query(`SELECT repo, path, content, updated_at FROM repo_memory ORDER BY repo ASC, path ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.RepoMemoryFile
	for rows.Next() {
		var m model.RepoMemoryFile
		if err := rows.Scan(&m.Repo, &m.Path, &m.Content, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}
