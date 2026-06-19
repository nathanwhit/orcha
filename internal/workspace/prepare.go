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
// Base. Steps: ensure & refresh the mirror cache, clone from the cache for
// speed, re-point origin at the real repo, fetch fresh, then create Branch off
// the freshly-resolved base.
func (p *Preparer) PrepareIsolated(ctx context.Context, ex exec.Executor, spec Spec) error {
	if err := p.base(ctx, ex, spec); err != nil {
		return err
	}
	base := spec.Base
	if base == "" {
		base = "HEAD"
	}
	return p.checkoutNewBranch(ctx, ex, spec, base)
}

// PreparePRBranch creates a checkout tracking an existing PR branch at its fresh
// head, so follow-up work updates the correct branch — the PR head is just the
// branch's start point.
func (p *Preparer) PreparePRBranch(ctx context.Context, ex exec.Executor, spec Spec) error {
	if err := p.base(ctx, ex, spec); err != nil {
		return err
	}
	return p.checkoutNewBranch(ctx, ex, spec, spec.Branch)
}

// checkoutNewBranch (re)creates spec.Branch pointing at startRef and initializes
// submodules. Both callers share it: an isolated checkout starts from its base,
// a PR-branch checkout from the PR head — in each case startRef is resolved from
// wherever it actually lives (see resolveRef).
//
// Submodules are initialized after the checkout (so .gitmodules reflects this
// ref) and after base() restored the real origin (so relative submodule URLs
// like ../foo resolve against upstream, not the local mirror cache). Repos like
// denoland/deno keep test fixtures and vendored deps in submodules; without this
// the tree is missing those paths and the build fails. A repo with no
// .gitmodules makes the submodule step a no-op.
func (p *Preparer) checkoutNewBranch(ctx context.Context, ex exec.Executor, spec Spec, startRef string) error {
	start, err := p.resolveRef(ctx, ex, spec, startRef)
	if err != nil {
		return err
	}
	if _, err := p.run(ctx, ex, "", "-C", spec.Dir, "checkout", "-B", spec.Branch, start); err != nil {
		return fmt.Errorf("workspace: check out %s at %s: %w", spec.Branch, start, err)
	}
	return p.updateSubmodules(ctx, ex, spec)
}

// submoduleJobs is how many submodules `submodule update` clones in parallel.
// Most of the cost is per-submodule network setup, so a handful of jobs shortens
// wall time on repos with several submodules; harmless for repos with one.
const submoduleJobs = "8"

// updateSubmodules checks out the tree's submodules, sourcing their objects from
// the per-repo mirror cache instead of refetching them from the network on every
// prep. Repos like denoland/deno pin large submodules (the WPT suite especially);
// since base() wipes and re-clones the workspace each prep, an uncached
// `submodule update` re-downloads all of them every time.
//
// Steps: (1) resolve the submodules' URLs into .git/config without touching the
// network (submodule init), (2) warm the same bare cache base() maintains with
// each submodule's objects (added as extra remotes), then (3) run submodule
// update with --reference <cache> so every submodule clone takes its objects
// from the local mirror via git alternates. Plain --reference degrades
// gracefully — a submodule the cache doesn't have yet is fetched from its origin
// as before — so correctness never depends on the cache being warm, only speed.
// A repo with no .gitmodules makes this a fast no-op.
func (p *Preparer) updateSubmodules(ctx context.Context, ex exec.Executor, spec Spec) error {
	// Resolve submodule URLs into .git/config (no network). Relative URLs resolve
	// against origin, which base() already re-pointed at the real upstream.
	if _, err := p.run(ctx, ex, "", "-C", spec.Dir, "submodule", "init"); err != nil {
		return fmt.Errorf("workspace: submodule init for %s: %w", spec.Branch, err)
	}
	urls := p.submoduleURLs(ctx, ex, spec)

	cache := p.cachePath(spec)
	for _, u := range urls {
		// Best-effort: a warm failure just falls back to a network fetch below.
		p.warmSubmoduleMirror(ctx, ex, cache, u)
	}

	args := []string{"-C", spec.Dir, "submodule", "update", "--init", "--recursive",
		"--checkout", "--jobs", submoduleJobs}
	if len(urls) > 0 {
		// The cache holds the submodule objects (warmed above); reference it so the
		// per-submodule clones source from local disk. Workspaces are wiped each
		// prep while the cache persists, so the alternate this leaves behind stays
		// valid for the workspace's lifetime.
		args = append(args, "--reference", cache)
	}
	if _, err := p.run(ctx, ex, "", args...); err != nil {
		return fmt.Errorf("workspace: init submodules for %s: %w", spec.Branch, err)
	}
	return nil
}

// submoduleURLs returns the resolved (absolute) URL of every initialized
// submodule, read from .git/config after `submodule init`. Returns nil for a
// repo with no submodules (the --get-regexp exits non-zero on no match, which is
// the no-submodule case, not a failure).
func (p *Preparer) submoduleURLs(ctx context.Context, ex exec.Executor, spec Spec) []string {
	out, err := p.run(ctx, ex, "", "-C", spec.Dir, "config", "--get-regexp", `^submodule\..*\.url$`)
	if err != nil {
		return nil
	}
	var urls []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		// Each line is "submodule.<name>.url <value>".
		if i := strings.IndexByte(line, ' '); i > 0 {
			if u := strings.TrimSpace(line[i+1:]); u != "" {
				urls = append(urls, u)
			}
		}
	}
	return urls
}

// warmSubmoduleMirror ensures the per-repo mirror cache holds one submodule's
// objects, so a later `submodule update --reference <cache>` serves them from
// local disk. The submodule is added as an extra remote in the same bare cache
// base() maintains (named by a slug of its URL) and fetched into the shared
// object store. Best-effort: any error is swallowed, leaving the update to fetch
// that submodule from the network as before.
func (p *Preparer) warmSubmoduleMirror(ctx context.Context, ex exec.Executor, cache, url string) {
	remote := "sub-" + slug(url)
	// Add the remote once; if it already exists (a prior prep, or a concurrent
	// one) skip straight to the fetch.
	if _, err := p.run(ctx, ex, "", "-C", cache, "remote", "get-url", remote); err != nil {
		if _, err := p.run(ctx, ex, "", "-C", cache, "remote", "add", remote, url); err != nil {
			return
		}
	}
	// Objects (not refs) are what the alternate needs; --no-tags keeps the cache's
	// ref namespace tidy.
	_, _ = p.run(ctx, ex, "", "-C", cache, "fetch", "--no-tags", remote)
}

// cachePath is the per-repo bare mirror cache directory for spec's RepoURL.
func (p *Preparer) cachePath(spec Spec) string {
	return join(spec.WorkRoot, p.cacheSub(), slug(spec.RepoURL)+".git")
}

// resolveRef turns a branch name into a checkable start point: origin/<ref> when
// origin has it (e.g. main), otherwise the head fetched from the fork (PushURL).
// orcha's own worker branches and, in a fork workflow, PR heads are pushed to
// the fork and never reach upstream, so a ref missing from origin is sought on
// the fork. With no fork to try it returns origin/<ref> so a failed checkout
// still names the missing ref. "HEAD" passes through (a base-less checkout).
func (p *Preparer) resolveRef(ctx context.Context, ex exec.Executor, spec Spec, ref string) (string, error) {
	if ref == "HEAD" {
		return "HEAD", nil
	}
	if _, err := p.run(ctx, ex, "", "-C", spec.Dir, "rev-parse", "--verify", "--quiet", "origin/"+ref); err == nil {
		return "origin/" + ref, nil
	}
	if spec.PushURL != "" {
		if _, err := p.run(ctx, ex, "", "-C", spec.Dir, "fetch", spec.PushURL, ref); err != nil {
			return "", fmt.Errorf("workspace: ref %q is not on origin and could not be fetched from the fork: %w", ref, err)
		}
		return "FETCH_HEAD", nil
	}
	return "origin/" + ref, nil
}

// base performs the shared, freshness-guaranteeing steps up to (but not
// including) the final branch checkout.
func (p *Preparer) base(ctx context.Context, ex exec.Executor, spec Spec) error {
	cacheParent := join(spec.WorkRoot, p.cacheSub())
	cache := p.cachePath(spec)

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
