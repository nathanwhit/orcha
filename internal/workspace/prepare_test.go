package workspace

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nathanwhit/orcha/internal/exec"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	hermeticGit(t)
	cmd := osexec.Command("git", args...)
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

// seedBare creates a bare remote with one commit on main and returns its path
// plus a seed working clone used to advance upstream later.
func seedBare(t *testing.T) (bare, seed string) {
	t.Helper()
	root := t.TempDir()
	bare = filepath.Join(root, "remote.git")
	git(t, root, "init", "--bare", "-b", "main", bare)
	seed = filepath.Join(root, "seed")
	git(t, root, "init", "-b", "main", seed)
	write(t, seed, "README.md", "A\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-m", "commit A")
	git(t, seed, "remote", "add", "origin", bare)
	git(t, seed, "push", "-u", "origin", "main")
	return bare, seed
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPrepareIsolated_BranchesOffFreshUpstream is the central guarantee: a
// workspace prepared after upstream advances is based on the *new* commit, even
// though the cache was first populated earlier.
func TestPrepareIsolated_BranchesOffFreshUpstream(t *testing.T) {
	bare, seed := seedBare(t)
	work := filepath.Join(t.TempDir(), "work")
	ex := exec.NewLocal()
	p := New()
	ctx := context.Background()

	// First workspace: based on commit A.
	ws1 := filepath.Join(work, "ws1")
	if err := p.PrepareIsolated(ctx, ex, Spec{WorkRoot: work, RepoURL: bare, Dir: ws1, Base: "main", Branch: "feat-1"}); err != nil {
		t.Fatalf("prepare ws1: %v", err)
	}
	if got := git(t, ws1, "rev-parse", "--abbrev-ref", "HEAD"); got != "feat-1" {
		t.Fatalf("ws1 on branch %q, want feat-1", got)
	}

	// Advance upstream: commit B is pushed to origin/main after the cache exists.
	write(t, seed, "B.txt", "B\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-m", "commit B")
	git(t, seed, "push", "origin", "main")
	bSha := git(t, seed, "rev-parse", "HEAD")

	// Second workspace: must include commit B (fresh upstream), not the stale
	// cache state.
	ws2 := filepath.Join(work, "ws2")
	if err := p.PrepareIsolated(ctx, ex, Spec{WorkRoot: work, RepoURL: bare, Dir: ws2, Base: "main", Branch: "feat-2"}); err != nil {
		t.Fatalf("prepare ws2: %v", err)
	}
	if got := git(t, ws2, "rev-parse", "origin/main"); got != bSha {
		t.Fatalf("ws2 origin/main=%s, want fresh %s", got, bSha)
	}
	// B must be reachable from the new branch's HEAD.
	if out := git(t, ws2, "merge-base", "--is-ancestor", bSha, "HEAD"); out != "" {
		t.Fatalf("commit B not in ws2 history: %s", out)
	}
	if got := git(t, ws2, "rev-parse", "HEAD"); got != bSha {
		t.Fatalf("ws2 HEAD=%s, want %s (branched off fresh base)", got, bSha)
	}

	// Workspaces are isolated directories.
	if _, err := os.Stat(ws1); err != nil {
		t.Fatalf("ws1 missing: %v", err)
	}
}

func TestPreparePRBranch_ChecksOutExistingBranch(t *testing.T) {
	bare, seed := seedBare(t)
	// Create a PR branch upstream with its own commit.
	git(t, seed, "checkout", "-b", "pr-7")
	write(t, seed, "fix.txt", "fix\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-m", "pr fix")
	git(t, seed, "push", "-u", "origin", "pr-7")
	prSha := git(t, seed, "rev-parse", "HEAD")

	work := filepath.Join(t.TempDir(), "work")
	ex := exec.NewLocal()
	ws := filepath.Join(work, "followup")
	if err := New().PreparePRBranch(context.Background(), ex, Spec{
		WorkRoot: work, RepoURL: bare, Dir: ws, Branch: "pr-7",
	}); err != nil {
		t.Fatalf("prepare pr branch: %v", err)
	}
	if got := git(t, ws, "rev-parse", "--abbrev-ref", "HEAD"); got != "pr-7" {
		t.Fatalf("on branch %q, want pr-7", got)
	}
	if got := git(t, ws, "rev-parse", "HEAD"); got != prSha {
		t.Fatalf("HEAD=%s, want PR head %s", got, prSha)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"https://github.com/owner/repo.git": "https-github-com-owner-repo",
		"owner/repo":                        "owner-repo",
		"/tmp/x/remote.git":                 "tmp-x-remote",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q)=%q want %q", in, got, want)
		}
	}
}

// Fork workflow: the checkout bases off upstream but pushes to the fork, and a
// PR-branch checkout fetches the branch from the fork.
func TestPrepare_ForkWorkflow(t *testing.T) {
	ctx := context.Background()
	ex := exec.NewLocal()

	upstream, _ := seedBare(t)
	root := t.TempDir()
	fork := filepath.Join(root, "fork.git")
	git(t, root, "clone", "--bare", upstream, fork)

	work := filepath.Join(root, "work")
	dir := filepath.Join(work, "ws1")
	p := New()
	if err := p.PrepareIsolated(ctx, ex, Spec{
		WorkRoot: work, RepoURL: upstream, Dir: dir, Base: "main", Branch: "orcha/feat",
		PushURL: fork,
	}); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// origin fetches from upstream but pushes to the fork.
	if got := git(t, dir, "remote", "get-url", "origin"); got != upstream {
		t.Fatalf("fetch url = %q, want upstream %q", got, upstream)
	}
	if got := git(t, dir, "remote", "get-url", "--push", "origin"); got != fork {
		t.Fatalf("push url = %q, want fork %q", got, fork)
	}

	// A commit pushed via plain `git push origin` lands on the FORK, not upstream.
	git(t, dir, "commit", "--allow-empty", "-m", "feat: fork test")
	git(t, dir, "push", "origin", "orcha/feat")
	if out := git(t, fork, "branch", "--list", "orcha/feat"); !strings.Contains(out, "orcha/feat") {
		t.Fatalf("branch missing on fork: %q", out)
	}
	if out := git(t, upstream, "branch", "--list", "orcha/feat"); strings.Contains(out, "orcha/feat") {
		t.Fatal("branch leaked to upstream")
	}

	// A PR-branch checkout for a follow-up fetches the branch from the fork.
	prDir := filepath.Join(work, "pr1")
	if err := p.PreparePRBranch(ctx, ex, Spec{
		WorkRoot: work, RepoURL: upstream, Dir: prDir, Branch: "orcha/feat",
		PushURL: fork,
	}); err != nil {
		t.Fatalf("prepare PR branch: %v", err)
	}
	if head := git(t, prDir, "log", "-1", "--format=%s"); !strings.Contains(head, "fork test") {
		t.Fatalf("PR checkout head = %q, want the fork commit", head)
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
