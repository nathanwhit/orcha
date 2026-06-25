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

// TestPrepareIsolated_FetchesForkHostedBase covers a worker that builds on
// another worker's branch (e.g. validating a published PR): orcha pushes worker
// branches to the fork, never upstream, so the base lives only on the fork and
// must be fetched from there rather than origin.
func TestPrepareIsolated_FetchesForkHostedBase(t *testing.T) {
	bare, _ := seedBare(t) // origin: main = commit A
	root := t.TempDir()
	fork := filepath.Join(root, "fork.git")
	git(t, root, "clone", "--bare", bare, fork) // a fork of origin

	// Push an "implementer" branch to the FORK only (not origin), as orcha does.
	impl := filepath.Join(root, "impl")
	git(t, root, "clone", bare, impl)
	write(t, impl, "feature.txt", "impl work\n")
	git(t, impl, "add", ".")
	git(t, impl, "commit", "-m", "impl change")
	git(t, impl, "push", fork, "HEAD:refs/heads/orcha/impl-abc")
	wantSHA := git(t, impl, "rev-parse", "HEAD")

	// A validator branches off the fork-hosted implementer branch.
	work := filepath.Join(t.TempDir(), "work")
	ex := exec.NewLocal()
	p := New()
	ctx := context.Background()
	ws := filepath.Join(work, "vali")
	if err := p.PrepareIsolated(ctx, ex, Spec{
		WorkRoot: work, RepoURL: bare, Dir: ws,
		Base: "orcha/impl-abc", Branch: "orcha/vali-xyz", PushURL: fork,
	}); err != nil {
		t.Fatalf("PrepareIsolated off fork-hosted base: %v", err)
	}
	if got := git(t, ws, "rev-parse", "HEAD"); got != wantSHA {
		t.Fatalf("HEAD = %s, want fork impl head %s", got, wantSHA)
	}
	if _, err := os.Stat(filepath.Join(ws, "feature.txt")); err != nil {
		t.Fatalf("expected the implementer's feature.txt in the validator checkout: %v", err)
	}
}

// TestPrepareIsolated_ForkBaseWithoutForkNamesOrigin: a base absent from origin
// with no fork to try should still fail with an error that names origin/<base>,
// so the missing ref is obvious.
func TestPrepareIsolated_ForkBaseWithoutForkNamesOrigin(t *testing.T) {
	bare, _ := seedBare(t)
	work := filepath.Join(t.TempDir(), "work")
	err := New().PrepareIsolated(context.Background(), exec.NewLocal(), Spec{
		WorkRoot: work, RepoURL: bare, Dir: filepath.Join(work, "ws"),
		Base: "orcha/impl-missing", Branch: "orcha/vali-1",
	})
	if err == nil {
		t.Fatal("expected failure branching off a base absent from origin with no fork")
	}
	if !strings.Contains(err.Error(), "origin/orcha/impl-missing") {
		t.Fatalf("error should name origin/<base>, got: %v", err)
	}
}

// TestPrepareIsolated_InitsSubmodules: a repo with a submodule (e.g.
// denoland/deno's vendored test fixtures) must come up with the submodule
// CHECKED OUT, not an empty directory, or the build fails. Uses git's
// env-config to allow local-path submodules in the test (production submodules
// are https and unaffected) so the code under test's `submodule update` works.
func TestPrepareIsolated_InitsSubmodules(t *testing.T) {
	hermeticGit(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	bare, seed := seedBare(t)
	root := t.TempDir()

	// A second repo to serve as the submodule.
	subBare := filepath.Join(root, "sub.git")
	git(t, root, "init", "--bare", "-b", "main", subBare)
	subSeed := filepath.Join(root, "subseed")
	git(t, root, "init", "-b", "main", subSeed)
	write(t, subSeed, "lib.txt", "from submodule\n")
	git(t, subSeed, "add", ".")
	git(t, subSeed, "commit", "-m", "sub commit")
	git(t, subSeed, "remote", "add", "origin", subBare)
	git(t, subSeed, "push", "-u", "origin", "main")

	// Wire the submodule into the main repo and publish.
	git(t, seed, "submodule", "add", subBare, "vendor/sub")
	git(t, seed, "commit", "-m", "add submodule")
	git(t, seed, "push", "origin", "main")

	work := filepath.Join(t.TempDir(), "work")
	ws := filepath.Join(work, "ws")
	if err := New().PrepareIsolated(context.Background(), exec.NewLocal(), Spec{
		WorkRoot: work, RepoURL: bare, Dir: ws, Base: "main", Branch: "feat-sub",
	}); err != nil {
		t.Fatalf("prepare with submodule: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, "vendor", "sub", "lib.txt"))
	if err != nil {
		t.Fatalf("submodule not checked out (vendor/sub/lib.txt missing): %v", err)
	}
	if string(got) != "from submodule\n" {
		t.Fatalf("submodule content = %q, want %q", string(got), "from submodule\n")
	}
}

// TestPrepareIsolated_SkipSubmodulesLeavesThemUninitialized: with
// SkipSubmodules set (a manager checkout), the superproject is checked out but
// NO submodule is materialized — that's where prep's minutes go, and a manager
// only reads the superproject source. The repo files are still present.
func TestPrepareIsolated_SkipSubmodulesLeavesThemUninitialized(t *testing.T) {
	hermeticGit(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	bare, seed := seedBare(t)
	root := t.TempDir()

	subBare := filepath.Join(root, "sub.git")
	git(t, root, "init", "--bare", "-b", "main", subBare)
	subSeed := filepath.Join(root, "subseed")
	git(t, root, "init", "-b", "main", subSeed)
	write(t, subSeed, "lib.txt", "from submodule\n")
	git(t, subSeed, "add", ".")
	git(t, subSeed, "commit", "-m", "sub commit")
	git(t, subSeed, "remote", "add", "origin", subBare)
	git(t, subSeed, "push", "-u", "origin", "main")

	git(t, seed, "submodule", "add", subBare, "vendor/sub")
	git(t, seed, "commit", "-m", "add submodule")
	git(t, seed, "push", "origin", "main")

	work := filepath.Join(t.TempDir(), "work")
	ws := filepath.Join(work, "ws")
	if err := New().PrepareIsolated(context.Background(), exec.NewLocal(), Spec{
		WorkRoot: work, RepoURL: bare, Dir: ws, Base: "main", Branch: "mgr",
		SkipSubmodules: true,
	}); err != nil {
		t.Fatalf("prepare (skip submodules): %v", err)
	}

	// Superproject is present...
	if _, err := os.Stat(filepath.Join(ws, "README.md")); err != nil {
		t.Fatalf("superproject should be checked out: %v", err)
	}
	// ...but the submodule working tree was never materialized.
	if _, err := os.Stat(filepath.Join(ws, "vendor", "sub", "lib.txt")); !os.IsNotExist(err) {
		t.Fatalf("submodule should be left uninitialized, but lib.txt is present (err=%v)", err)
	}
}

// TestPrepareIsolated_SkipsOversizedSubmodule: a submodule whose tree is larger
// than maxEagerSubmoduleFiles is left UNINITIALIZED (not checked out), so prep
// doesn't pay to materialize bulk test data (denoland/deno's WPT suite, ~160k
// files) a typical worker never touches — while a small submodule alongside it is
// still checked out as normal. The size is decided generically by file count, not
// by any hardcoded path.
func TestPrepareIsolated_SkipsOversizedSubmodule(t *testing.T) {
	hermeticGit(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	// Lower the ceiling so a tiny test submodule trips the "too large" rule.
	defer func(n int) { maxEagerSubmoduleFiles = n }(maxEagerSubmoduleFiles)
	maxEagerSubmoduleFiles = 2

	bare, seed := seedBare(t)
	root := t.TempDir()

	mkSub := func(name string, files []string) string {
		subBare := filepath.Join(root, name+".git")
		git(t, root, "init", "--bare", "-b", "main", subBare)
		subSeed := filepath.Join(root, name+"seed")
		git(t, root, "init", "-b", "main", subSeed)
		for _, f := range files {
			write(t, subSeed, f, "x\n")
		}
		git(t, subSeed, "add", ".")
		git(t, subSeed, "commit", "-m", "seed")
		git(t, subSeed, "remote", "add", "origin", subBare)
		git(t, subSeed, "push", "-u", "origin", "main")
		return subBare
	}
	smallBare := mkSub("small", []string{"a.txt"})                   // 1 file  -> kept
	largeBare := mkSub("large", []string{"a.txt", "b.txt", "c.txt"}) // 3 files -> skipped (>2)

	git(t, seed, "submodule", "add", smallBare, "vendor/small")
	git(t, seed, "submodule", "add", largeBare, "vendor/large")
	git(t, seed, "commit", "-m", "add submodules")
	git(t, seed, "push", "origin", "main")

	work := filepath.Join(t.TempDir(), "work")
	ws := filepath.Join(work, "ws")
	if err := New().PrepareIsolated(context.Background(), exec.NewLocal(), Spec{
		WorkRoot: work, RepoURL: bare, Dir: ws, Base: "main", Branch: "feat",
	}); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// The small submodule is checked out as usual.
	if _, err := os.Stat(filepath.Join(ws, "vendor", "small", "a.txt")); err != nil {
		t.Fatalf("small submodule should be checked out: %v", err)
	}
	// The oversized one is left uninitialized — its files are never materialized.
	if _, err := os.Stat(filepath.Join(ws, "vendor", "large", "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("oversized submodule should NOT be checked out, but a.txt is present (err=%v)", err)
	}
}

// TestPrepareIsolated_SubmoduleMirrorCache: submodule objects must be served from
// the per-repo mirror cache, not refetched from the network on every prep. After
// a prepare, the cache should hold the submodule as a remote and the prepared
// submodule should carry a git alternate pointing back at the cache (so its
// objects come from local disk). Regression guard for the caching path.
func TestPrepareIsolated_SubmoduleMirrorCache(t *testing.T) {
	hermeticGit(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	bare, seed := seedBare(t)
	root := t.TempDir()

	subBare := filepath.Join(root, "sub.git")
	git(t, root, "init", "--bare", "-b", "main", subBare)
	subSeed := filepath.Join(root, "subseed")
	git(t, root, "init", "-b", "main", subSeed)
	write(t, subSeed, "lib.txt", "from submodule\n")
	git(t, subSeed, "add", ".")
	git(t, subSeed, "commit", "-m", "sub commit")
	git(t, subSeed, "remote", "add", "origin", subBare)
	git(t, subSeed, "push", "-u", "origin", "main")

	git(t, seed, "submodule", "add", subBare, "vendor/sub")
	git(t, seed, "commit", "-m", "add submodule")
	git(t, seed, "push", "origin", "main")

	work := filepath.Join(t.TempDir(), "work")
	ws := filepath.Join(work, "ws")
	if err := New().PrepareIsolated(context.Background(), exec.NewLocal(), Spec{
		WorkRoot: work, RepoURL: bare, Dir: ws, Base: "main", Branch: "feat-sub",
	}); err != nil {
		t.Fatalf("prepare with submodule: %v", err)
	}

	// The cache holds the submodule's objects as a named remote.
	cache := filepath.Join(work, ".orcha-cache", slug(bare)+".git")
	remote := "sub-" + slug(subBare)
	if out := git(t, cache, "remote"); !strings.Contains(out, remote) {
		t.Fatalf("cache should have submodule remote %q, got remotes:\n%s", remote, out)
	}

	// The prepared submodule sources its objects from the cache via an alternate.
	altFile := filepath.Join(ws, ".git", "modules", "vendor", "sub", "objects", "info", "alternates")
	alt, err := os.ReadFile(altFile)
	if err != nil {
		t.Fatalf("submodule should have an objects alternate (mirror reference): %v", err)
	}
	if !strings.Contains(string(alt), cache) {
		t.Fatalf("submodule alternate %q should point at the cache %q", strings.TrimSpace(string(alt)), cache)
	}
}

// TestReconcileSubmodulePins_ResetsDriftToPin: when a submodule's checkout has
// drifted ahead of the commit the superproject pins (as a shallow `submodule
// update` against a cache whose tracking branch is ahead can leave it),
// reconcile must put it back exactly on the recorded pin so the drift can't be
// committed. Regression guard for the wpt/suite submodule-bump leak.
func TestReconcileSubmodulePins_ResetsDriftToPin(t *testing.T) {
	hermeticGit(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	bare, seed := seedBare(t)
	root := t.TempDir()

	subBare := filepath.Join(root, "sub.git")
	git(t, root, "init", "--bare", "-b", "main", subBare)
	subSeed := filepath.Join(root, "subseed")
	git(t, root, "init", "-b", "main", subSeed)
	write(t, subSeed, "lib.txt", "x\n")
	git(t, subSeed, "add", ".")
	git(t, subSeed, "commit", "-m", "X")
	git(t, subSeed, "remote", "add", "origin", subBare)
	git(t, subSeed, "push", "-u", "origin", "main")
	pinX := git(t, subSeed, "rev-parse", "HEAD")

	git(t, seed, "submodule", "add", subBare, "vendor/sub")
	git(t, seed, "commit", "-m", "add submodule")
	git(t, seed, "push", "origin", "main")

	work := filepath.Join(t.TempDir(), "work")
	ws := filepath.Join(work, "ws")
	ex := exec.NewLocal()
	p := New()
	ctx := context.Background()
	spec := Spec{WorkRoot: work, RepoURL: bare, Dir: ws, Base: "main", Branch: "feat-sub"}
	if err := p.PrepareIsolated(ctx, ex, spec); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	subDir := filepath.Join(ws, "vendor", "sub")
	if got := git(t, subDir, "rev-parse", "HEAD"); got != pinX {
		t.Fatalf("submodule prepared at %q, want pin %s", got, pinX)
	}

	// Upstream advances; move the workspace submodule onto the newer tip, the way
	// a drifting shallow update would.
	write(t, subSeed, "lib.txt", "y\n")
	git(t, subSeed, "add", ".")
	git(t, subSeed, "commit", "-m", "Y")
	git(t, subSeed, "push", "origin", "main")
	tipY := git(t, subSeed, "rev-parse", "HEAD")
	git(t, subDir, "fetch", "origin")
	git(t, subDir, "checkout", "--detach", tipY)
	if got := git(t, subDir, "rev-parse", "HEAD"); got != tipY {
		t.Fatalf("drift setup failed: submodule at %q, want tip %s", got, tipY)
	}

	p.reconcileSubmodulePins(ctx, ex, spec)
	if got := git(t, subDir, "rev-parse", "HEAD"); got != pinX {
		t.Fatalf("after reconcile submodule at %q, want pin %s", got, pinX)
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
