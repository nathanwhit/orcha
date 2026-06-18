// Package workspace prepares real git checkouts for sessions. Preparation runs
// through an exec.Executor, so it works the same on a local host or a remote
// SSH target (the checkout lives wherever the executor runs).
//
// Freshness is the central guarantee: every prepared workspace is based on the
// latest upstream. A per-target bare mirror cache gives build/cache locality and
// fast clones, but the isolated checkout always re-fetches from the real origin
// and branches off the freshly-fetched base — never a stale local copy.
package workspace

import (
	"context"
	"fmt"
	"strings"

	"github.com/nathanwhit/orcha/internal/exec"
)

// Preparer creates git checkouts via an executor.
type Preparer struct {
	// GitBin is the git executable on the target (default "git").
	GitBin string
	// CacheSubdir is the directory under the work root holding bare mirror
	// caches (default ".orcha-cache").
	CacheSubdir string
}

// New returns a Preparer with defaults.
func New() *Preparer { return &Preparer{GitBin: "git", CacheSubdir: ".orcha-cache"} }

func (p *Preparer) git() string {
	if p.GitBin == "" {
		return "git"
	}
	return p.GitBin
}

func (p *Preparer) cacheSub() string {
	if p.CacheSubdir == "" {
		return ".orcha-cache"
	}
	return p.CacheSubdir
}

// Spec describes a checkout to prepare.
type Spec struct {
	WorkRoot string // target-local root, e.g. /home/bot/work
	RepoURL  string // clone source: a git URL or a local path (tests use a bare repo path)
	Dir      string // target-local checkout directory
	Base     string // base branch to branch from / update against (e.g. "main")
	Branch   string // branch to create (isolated) or check out (PR follow-up)
	// PushURL, when set, is where branch pushes go — the fork in a fork
	// workflow. origin's FETCH url stays RepoURL (the upstream freshness
	// guarantee), while its PUSH url becomes PushURL, so a plain
	// `git push origin <branch>` lands on the fork.
	PushURL string
}

// PrepareIsolated creates a fresh isolated checkout with a new Branch based on
// the latest origin/Base. Steps: ensure & refresh the mirror cache, clone from
// the cache for speed, re-point origin at the real repo, fetch fresh, and create
// the branch off the freshly-fetched base.
func (p *Preparer) PrepareIsolated(ctx context.Context, ex exec.Executor, spec Spec) error {
	if err := p.base(ctx, ex, spec); err != nil {
		return err
	}
	// New branch off the freshly-fetched base.
	base := spec.Base
	if base == "" {
		base = "HEAD"
	}
	start, err := p.isolatedStartPoint(ctx, ex, spec, base)
	if err != nil {
		return err
	}
	return p.checkoutBranch(ctx, ex, spec, start)
}

// checkoutBranch creates spec.Branch at start and initializes submodules. Every
// prepare path funnels through here so the tree is always complete however the
// start point was resolved (an isolated base, or a PR head off the fork). Repos
// like denoland/deno keep test fixtures and vendored deps in submodules; without
// the submodule step those paths are empty and the build fails. It runs after
// the checkout so .gitmodules reflects this ref, and after base() restored the
// real origin so relative submodule URLs (../foo) resolve against upstream, not
// the local cache. A repo with no .gitmodules makes the submodule step a no-op.
func (p *Preparer) checkoutBranch(ctx context.Context, ex exec.Executor, spec Spec, start string) error {
	if _, err := p.run(ctx, ex, "", "-C", spec.Dir, "checkout", "-B", spec.Branch, start); err != nil {
		return fmt.Errorf("workspace: check out %s at %s: %w", spec.Branch, start, err)
	}
	if _, err := p.run(ctx, ex, "", "-C", spec.Dir, "submodule", "update", "--init", "--recursive", "--checkout"); err != nil {
		return fmt.Errorf("workspace: init submodules for %s: %w", spec.Branch, err)
	}
	return nil
}

// isolatedStartPoint resolves the revision a fresh isolated branch starts from.
// The base is normally an upstream branch on origin (e.g. main). But orcha's
// own worker branches are pushed to the fork (PushURL), never upstream — so a
// worker asked to build on another worker's branch (e.g. validating a published
// PR's branch) has a base that lives only on the fork. Prefer origin when it
// has the base; otherwise, when a fork is configured, fetch the base from there
// and start from the fetched head. Without a fork to fall back to, keep
// origin/<base> so the resulting checkout error names exactly what was missing.
func (p *Preparer) isolatedStartPoint(ctx context.Context, ex exec.Executor, spec Spec, base string) (string, error) {
	if base == "HEAD" {
		return "HEAD", nil
	}
	if _, err := p.run(ctx, ex, "", "-C", spec.Dir, "rev-parse", "--verify", "--quiet", "origin/"+base); err == nil {
		return "origin/" + base, nil
	}
	if spec.PushURL != "" {
		if _, err := p.run(ctx, ex, "", "-C", spec.Dir, "fetch", spec.PushURL, base); err != nil {
			return "", fmt.Errorf("workspace: base %q is not on origin and could not be fetched from the fork: %w", base, err)
		}
		return "FETCH_HEAD", nil
	}
	return "origin/" + base, nil
}

// PreparePRBranch creates a checkout tracking an existing PR branch at its
// fresh head, so follow-up work updates the correct branch. In a fork workflow
// the PR branch lives on the fork (PushURL), not upstream, so it is fetched
// from there.
func (p *Preparer) PreparePRBranch(ctx context.Context, ex exec.Executor, spec Spec) error {
	if err := p.base(ctx, ex, spec); err != nil {
		return err
	}
	if spec.PushURL != "" {
		if _, err := p.run(ctx, ex, "", "-C", spec.Dir, "fetch", spec.PushURL, spec.Branch); err != nil {
			return fmt.Errorf("workspace: fetch PR branch %s from fork: %w", spec.Branch, err)
		}
		return p.checkoutBranch(ctx, ex, spec, "FETCH_HEAD")
	}
	return p.checkoutBranch(ctx, ex, spec, "origin/"+spec.Branch)
}

// base performs the shared, freshness-guaranteeing steps up to (but not
// including) the final branch checkout.
func (p *Preparer) base(ctx context.Context, ex exec.Executor, spec Spec) error {
	cacheParent := join(spec.WorkRoot, p.cacheSub())
	cache := join(cacheParent, slug(spec.RepoURL)+".git")

	// Ensure the work root and cache parent exist on the target.
	if _, err := exec.RunCapture(ctx, ex, exec.Command{Name: "mkdir", Args: []string{"-p", cacheParent}}); err != nil {
		return fmt.Errorf("workspace: mkdir cache root: %w", err)
	}

	// Ensure the bare mirror cache exists (clone once), then refresh it so the
	// cache itself tracks upstream.
	if _, err := p.run(ctx, ex, "", "-C", cache, "rev-parse", "--git-dir"); err != nil {
		if _, err := p.run(ctx, ex, "", "clone", "--mirror", spec.RepoURL, cache); err != nil {
			return fmt.Errorf("workspace: mirror clone: %w", err)
		}
	}
	if _, err := p.run(ctx, ex, "", "-C", cache, "fetch", "--prune", "origin"); err != nil {
		// A fetch failure on an existing cache is not fatal on its own — the
		// per-workspace fetch below still pulls fresh from the real origin — but
		// surface it for visibility on first failure.
		_ = err
	}

	// Fresh isolated checkout: remove any stale dir, clone from the cache.
	if _, err := exec.RunCapture(ctx, ex, exec.Command{Name: "rm", Args: []string{"-rf", spec.Dir}}); err != nil {
		return fmt.Errorf("workspace: clean dir: %w", err)
	}
	if _, err := p.run(ctx, ex, "", "clone", cache, spec.Dir); err != nil {
		return fmt.Errorf("workspace: clone from cache: %w", err)
	}

	// Re-point origin at the real repo and fetch fresh from it. This is the
	// freshness guarantee: origin/* now reflects the real upstream, not the
	// possibly-stale cache.
	if _, err := p.run(ctx, ex, "", "-C", spec.Dir, "remote", "set-url", "origin", spec.RepoURL); err != nil {
		return fmt.Errorf("workspace: set origin: %w", err)
	}
	// Fork workflow: fetch from upstream, push to the fork.
	if spec.PushURL != "" {
		if _, err := p.run(ctx, ex, "", "-C", spec.Dir, "remote", "set-url", "--push", "origin", spec.PushURL); err != nil {
			return fmt.Errorf("workspace: set push url: %w", err)
		}
	}
	if _, err := p.run(ctx, ex, "", "-C", spec.Dir, "fetch", "--prune", "origin"); err != nil {
		return fmt.Errorf("workspace: fetch upstream: %w", err)
	}
	return nil
}

// run executes a git subcommand and returns combined output.
func (p *Preparer) run(ctx context.Context, ex exec.Executor, dir string, args ...string) (string, error) {
	out, err := exec.RunCapture(ctx, ex, exec.Command{Dir: dir, Name: p.git(), Args: args})
	if err != nil {
		return out, fmt.Errorf("%s: %w", strings.TrimSpace(out), err)
	}
	return out, nil
}

// join concatenates path segments with "/" (paths are target-local POSIX).
func join(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimRight(p, "/")
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	return strings.Join(cleaned, "/")
}

// slug turns a repo URL/path into a filesystem-safe cache name.
func slug(s string) string {
	s = strings.TrimSuffix(s, ".git")
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			prevDash = false
		default:
			// Collapse any run of separators into a single dash.
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "repo"
	}
	return out
}
