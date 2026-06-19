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
	hermeticGit(t)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=orcha", "GIT_AUTHOR_EMAIL=orcha@test",
		"GIT_COMMITTER_NAME=orcha", "GIT_COMMITTER_EMAIL=orcha@test",
		// Hermetic: ignore the developer's global/system git config. Commit
		// signing in particular (e.g. via the 1Password SSH agent) makes tests
		// hang on an authorization prompt or fail when the agent is locked.
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
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

func TestGitForge_Diff(t *testing.T) {
	work, _ := setupRepo(t)
	g := NewGit()
	ctx := context.Background()

	// Clean checkout equal to origin/main -> empty diff.
	if d, err := g.Diff(ctx, work); err != nil || d != "" {
		t.Fatalf("clean checkout: diff=%q err=%v", d, err)
	}

	// A committed change on a feature branch shows up vs the base, with the
	// changed file named in the leading --stat (so a truncated diff still
	// identifies it) and the added line in the patch body.
	mustGit(t, work, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "commit", "-m", "add feature")

	d, err := g.Diff(ctx, work)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(d, "feature.txt") {
		t.Fatalf("diff should name the changed file:\n%s", d)
	}
	if !strings.Contains(d, "+hello world") {
		t.Fatalf("diff should include the added line:\n%s", d)
	}

	// An uncommitted edit is also captured (working tree vs base).
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("hello world\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err = g.Diff(ctx, work)
	if err != nil {
		t.Fatalf("diff after edit: %v", err)
	}
	if !strings.Contains(d, "+more") {
		t.Fatalf("diff should include the uncommitted line:\n%s", d)
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

func TestParseLatestAssignment(t *testing.T) {
	// Real-world shape from denoland/deno#35348: a human (nathanwhit) assigned the
	// bot, but the event's `actor` is reported as the bot itself. Attribution must
	// come from `assigner`, else an allowlisted human's assignment is dropped.
	payload := `[
		{"id":26971020917,"event":"assigned",
		 "actor":{"login":"nathanwhitbot"},
		 "assignee":{"login":"nathanwhitbot"},
		 "assigner":{"login":"nathanwhit"}}
	]`
	who, id, err := parseLatestAssignment(payload, "nathanwhitbot")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if who != "nathanwhit" {
		t.Errorf("assigner = %q, want nathanwhit", who)
	}
	if id != "26971020917" {
		t.Errorf("eventID = %q, want 26971020917", id)
	}

	// Latest matching assignment wins; non-matching assignee and other event types
	// are ignored. Missing assigner falls back to actor.
	multi := `[
		{"id":1,"event":"labeled","actor":{"login":"x"}},
		{"id":2,"event":"assigned","actor":{"login":"a"},"assignee":{"login":"someoneelse"},"assigner":{"login":"a"}},
		{"id":3,"event":"assigned","actor":{"login":"carol"},"assignee":{"login":"bot"}},
		{"id":4,"event":"assigned","actor":{"login":"bot"},"assignee":{"login":"bot"},"assigner":{"login":"bob"}}
	]`
	who, id, err = parseLatestAssignment(multi, "bot")
	if err != nil {
		t.Fatalf("parse multi: %v", err)
	}
	if who != "bob" || id != "4" {
		t.Errorf("multi = (%q,%q), want (bob,4)", who, id)
	}

	// No matching assignment yields an empty attribution (caller cannot authorize).
	who, id, err = parseLatestAssignment(`[{"id":5,"event":"closed","actor":{"login":"z"}}]`, "bot")
	if err != nil {
		t.Fatalf("parse none: %v", err)
	}
	if who != "" || id != "" {
		t.Errorf("no-match = (%q,%q), want empty", who, id)
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

// CommitAll must use the checkout's inherited git identity, not a synthetic
// "orcha" author (regression: follow-up commits showed up authored by orcha).
func TestGitForge_CommitAll_UsesInheritedIdentity(t *testing.T) {
	work, _ := setupRepo(t)
	// The checkout has its own identity configured (as a real clone would).
	mustGit(t, work, "config", "user.name", "Real Dev")
	mustGit(t, work, "config", "user.email", "real@dev.example")

	if err := os.WriteFile(filepath.Join(work, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	committed, err := NewGit().CommitAll(context.Background(), work, "feat: add new.txt")
	if err != nil || !committed {
		t.Fatalf("CommitAll: committed=%v err=%v", committed, err)
	}
	author := mustGit(t, work, "log", "-1", "--pretty=%an <%ae>")
	if author != "Real Dev <real@dev.example>" {
		t.Fatalf("commit author = %q, want the inherited identity (not orcha)", author)
	}
	msg := mustGit(t, work, "log", "-1", "--pretty=%s")
	if msg != "feat: add new.txt" {
		t.Fatalf("commit message = %q, want the provided one", msg)
	}
	// A clean tree commits nothing.
	again, _ := NewGit().CommitAll(context.Background(), work, "noop")
	if again {
		t.Fatal("CommitAll on a clean tree should not commit")
	}
}

// CommitAll must not let a submodule whose checkout drifted ahead of its pin
// leak that pointer bump into the catch-all commit (regression: a shallow
// submodule update left tests/wpt/suite ahead of the recorded commit and
// `git add -A` swept the bump into a deno PR).
func TestGitForge_CommitAll_DoesNotSweepSubmoduleDrift(t *testing.T) {
	hermeticGit(t)
	root := t.TempDir()
	ctx := context.Background()
	writeFile := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Submodule upstream: commit X (the pin to keep) then a newer commit Y.
	subBare := filepath.Join(root, "sub.git")
	mustGit(t, root, "init", "--bare", "-b", "main", subBare)
	subSeed := filepath.Join(root, "sub-seed")
	mustGit(t, root, "init", "-b", "main", subSeed)
	writeFile(subSeed, "s.txt", "x\n")
	mustGit(t, subSeed, "add", ".")
	mustGit(t, subSeed, "commit", "-m", "X")
	mustGit(t, subSeed, "remote", "add", "origin", subBare)
	mustGit(t, subSeed, "push", "-u", "origin", "main")
	pinX := mustGit(t, subSeed, "rev-parse", "HEAD")
	writeFile(subSeed, "s.txt", "y\n")
	mustGit(t, subSeed, "add", ".")
	mustGit(t, subSeed, "commit", "-m", "Y")
	mustGit(t, subSeed, "push", "origin", "main")
	tipY := mustGit(t, subSeed, "rev-parse", "HEAD")

	// Superproject pins the submodule at X.
	work := filepath.Join(root, "work")
	mustGit(t, root, "init", "-b", "main", work)
	// CommitAll commits through the forge (inheriting only the process env, which
	// is hermetic here), so the checkout needs its own identity like a real clone.
	mustGit(t, work, "config", "user.name", "Real Dev")
	mustGit(t, work, "config", "user.email", "real@dev.example")
	writeFile(work, "README.md", "hi\n")
	mustGit(t, work, "add", ".")
	mustGit(t, work, "commit", "-m", "initial")
	mustGit(t, work, "-c", "protocol.file.allow=always", "submodule", "add", subBare, "vendor/sub")
	mustGit(t, work, "-C", "vendor/sub", "checkout", "--detach", pinX)
	mustGit(t, work, "add", ".gitmodules", "vendor/sub")
	mustGit(t, work, "commit", "-m", "pin sub at X")

	// Drift the submodule's working checkout to the newer commit (uncommitted),
	// alongside a legitimate file change the agent left for CommitAll to commit.
	mustGit(t, work, "-C", "vendor/sub", "checkout", "--detach", tipY)
	writeFile(work, "feature.txt", "f\n")

	committed, err := NewGit().CommitAll(ctx, work, "orcha: add feature")
	if err != nil || !committed {
		t.Fatalf("CommitAll: committed=%v err=%v", committed, err)
	}
	gitlink := mustGit(t, work, "ls-tree", "HEAD", "vendor/sub")
	if strings.Contains(gitlink, tipY) {
		t.Fatalf("submodule bump was swept into the commit: %q (pointer at tip %s)", gitlink, tipY)
	}
	if !strings.Contains(gitlink, pinX) {
		t.Fatalf("submodule pointer = %q, want it left at the pin %s", gitlink, pinX)
	}
	// The legitimate change still landed.
	if files := mustGit(t, work, "show", "--name-only", "--pretty=format:", "HEAD"); !strings.Contains(files, "feature.txt") {
		t.Fatalf("expected feature.txt in the commit, got: %q", files)
	}
}

// hermeticGit makes git invocations during this test — including ones made by
// the code under test, which inherits the process env — ignore the developer's
// global/system git config. Commit signing in particular (e.g. via the
// 1Password SSH agent) hangs tests on an authorization prompt or fails them
// when the agent is locked.
func hermeticGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}
