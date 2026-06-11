package forge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=orcha", "GIT_AUTHOR_EMAIL=orcha@test",
		"GIT_COMMITTER_NAME=orcha", "GIT_COMMITTER_EMAIL=orcha@test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// setupRepo creates a bare "remote" seeded with a main branch and returns a
// fresh clone (the workspace) plus the bare path.
func setupRepo(t *testing.T) (workDir, bareDir string) {
	t.Helper()
	root := t.TempDir()
	bareDir = filepath.Join(root, "remote.git")
	mustGit(t, root, "init", "--bare", "-b", "main", bareDir)

	seed := filepath.Join(root, "seed")
	mustGit(t, root, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, seed, "add", ".")
	mustGit(t, seed, "commit", "-m", "initial")
	mustGit(t, seed, "remote", "add", "origin", bareDir)
	mustGit(t, seed, "push", "-u", "origin", "main")

	workDir = filepath.Join(root, "work")
	mustGit(t, root, "clone", bareDir, workDir)
	return workDir, bareDir
}

func TestGitForge_HasDiff(t *testing.T) {
	work, _ := setupRepo(t)
	g := NewGit()
	ctx := context.Background()

	// Clean main, equal to origin/main -> nothing to publish.
	if has, err := g.HasDiff(ctx, work); err != nil || has {
		t.Fatalf("clean checkout: has=%v err=%v", has, err)
	}

	// Uncommitted change -> dirty -> has diff.
	mustGit(t, work, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if has, err := g.HasDiff(ctx, work); err != nil || !has {
		t.Fatalf("dirty tree: has=%v err=%v", has, err)
	}

	// Commit it -> clean tree but ahead of origin/main -> still has diff.
	mustGit(t, work, "add", ".")
	mustGit(t, work, "commit", "-m", "add feature")
	if has, err := g.HasDiff(ctx, work); err != nil || !has {
		t.Fatalf("ahead-of-base: has=%v err=%v", has, err)
	}
}

func TestGitForge_PushBranch(t *testing.T) {
	work, bare := setupRepo(t)
	g := NewGit()
	ctx := context.Background()

	mustGit(t, work, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "commit", "-m", "c1")

	sha, err := g.PushBranch(ctx, "owner/repo", work, "feature", false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	localSha := mustGit(t, work, "rev-parse", "feature")
	if sha != localSha {
		t.Fatalf("returned sha %s != local %s", sha, localSha)
	}
	// The branch is now on the remote at that sha.
	remote := mustGit(t, bare, "rev-parse", "feature")
	if remote != sha {
		t.Fatalf("remote sha %s != pushed %s", remote, sha)
	}

	// Amend (rewrite history) and force-push.
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "commit", "-am", "c1-amended", "--amend")
	sha2, err := g.PushBranch(ctx, "owner/repo", work, "feature", true)
	if err != nil {
		t.Fatalf("force push: %v", err)
	}
	if sha2 == sha {
		t.Fatal("amended sha should differ")
	}
	if remote := mustGit(t, bare, "rev-parse", "feature"); remote != sha2 {
		t.Fatalf("remote not updated by force push: %s != %s", remote, sha2)
	}
}

func TestGhStatusMapping(t *testing.T) {
	cases := []struct {
		state string
		draft bool
		want  string
	}{
		{"OPEN", false, "open"},
		{"OPEN", true, "draft"},
		{"MERGED", false, "merged"},
		{"CLOSED", false, "closed"},
	}
	for _, c := range cases {
		if got := ghStatus(c.state, c.draft); got != c.want {
			t.Errorf("ghStatus(%s,%v)=%s want %s", c.state, c.draft, got, c.want)
		}
	}
}

type rollupItem = struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

func TestGhChecksMapping(t *testing.T) {
	if got := ghChecks(nil); got != "unknown" {
		t.Errorf("empty rollup = %s want unknown", got)
	}
	passing := []rollupItem{{Status: "COMPLETED", Conclusion: "SUCCESS"}}
	if got := ghChecks(passing); got != "passing" {
		t.Errorf("passing = %s", got)
	}
	failing := []rollupItem{{Status: "COMPLETED", Conclusion: "SUCCESS"}, {Status: "COMPLETED", Conclusion: "FAILURE"}}
	if got := ghChecks(failing); got != "failing" {
		t.Errorf("failing = %s", got)
	}
	pending := []rollupItem{{Status: "IN_PROGRESS"}}
	if got := ghChecks(pending); got != "pending" {
		t.Errorf("pending = %s", got)
	}
}

func TestURLParsing(t *testing.T) {
	url := "https://github.com/owner/repo/pull/4242"
	if got := lastURL("Creating pull request...\n" + url); got != url {
		t.Errorf("lastURL=%s", got)
	}
	if got := prNumberFromURL(url); got != 4242 {
		t.Errorf("prNumberFromURL=%d", got)
	}
	if got := prNumberFromURL(url + "/"); got != 4242 {
		t.Errorf("prNumberFromURL trailing slash=%d", got)
	}
}

// TestGitForge_GHLive exercises the real gh CLI read-only against a public repo.
// Gated behind ORCHA_GH_LIVE=1 (requires gh auth + network).
func TestGitForge_GHLive(t *testing.T) {
	if os.Getenv("ORCHA_GH_LIVE") != "1" {
		t.Skip("set ORCHA_GH_LIVE=1 to run the live gh test")
	}
	g := NewGit()
	ctx := context.Background()
	if ok, err := g.RepoExists(ctx, "cli/cli"); err != nil || !ok {
		t.Fatalf("cli/cli should exist: ok=%v err=%v", ok, err)
	}
	if ok, err := g.RepoExists(ctx, "orcha-nope/definitely-not-a-real-repo-xyz"); err != nil || ok {
		t.Fatalf("nonexistent repo: ok=%v err=%v", ok, err)
	}
}
