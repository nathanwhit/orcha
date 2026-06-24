package orch

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// Project-wide memory. Workers run in fresh isolated checkouts, so a CLAUDE.md
// they write dies with the workspace — there is no memory that survives across
// objectives. This wires the store's repo_memory through the workspace: a small
// set of linked markdown files (an index plus topic files) is SEEDED into each
// checkout under .orcha/memory/ before the agent starts, the agent reads and
// edits them like any files, and they are MERGED BACK into the store file-by-file
// when the session finishes. Keyed by repo, so every objective on the same repo
// shares one growing memory.
//
// A directory (rather than one monolithic file) keeps the index small and
// readable and makes concurrency cheap: two workers adding different topic files
// touch different paths and never conflict; only edits to the SAME file fall back
// to a 3-way merge.

const (
	repoMemoryDir       = ".orcha/memory"         // the checkout-visible memory directory
	repoMemoryBaseDir   = ".orcha/.memory-base"   // seeded snapshot — the 3-way merge base
	repoMemoryOursDir   = ".orcha/.memory-ours"   // store content, written only for a 3-way merge
	repoMemoryTheirsDir = ".orcha/.memory-theirs" // agent's content, written for a 3-way merge
	repoMemoryIndex     = "MEMORY.md"             // index file, relative to repoMemoryDir
	repoMemoryRoot      = ".orcha"                // excluded from git locally
	gitLocalExclude     = ".git/info/exclude"
	// repoMemoryGitignore lives inside .orcha/ and ignores everything under it.
	// .git/info/exclude hides .orcha/ from git, but tools that walk the tree via
	// .gitignore (deno fmt/lint, prettier, ripgrep, …) don't read info/exclude —
	// they'd reformat the memory markdown out from under the agent. A `*` here is
	// inside the already-excluded dir, so it fixes those tools without touching
	// the agent's diff.
	repoMemoryGitignore = ".orcha/.gitignore"
)

// repoMemoryScaffold seeds a brand-new repo's index so the agent sees the memory
// directory exists and how it is meant to be used. It round-trips: if the agent
// leaves it untouched, merge-back is a no-op; if it adds to it (or creates topic
// files), they persist.
const repoMemoryScaffold = `# Project memory

Index of durable, repo-wide notes for agents working on this repository. Memory
is a small set of linked markdown files — this index plus one file per topic
(architecture, gotchas, the build/test commands that actually work, conventions).
It is shared across tasks and persists after this one ends.

Read the entries relevant to your task before you start. As you learn something
worth keeping, add or update a topic file and link it from here. Keep files
focused and concise; prune anything that goes stale.

<!-- Entries (add as you create topic files), e.g.:
- [architecture](architecture.md) — how the pieces fit together
- [build & test](build-and-test.md) — the commands that actually work
- [gotchas](gotchas.md) — sharp edges and surprises
-->
`

// repoMemoryNote is appended to a coding worker's (and the manager's) preamble so
// the agent knows the memory directory exists, to start from the index, and to
// keep it current. Only included when a checkout with a repo was actually seeded.
const repoMemoryNote = "\n\nPROJECT MEMORY: the " + repoMemoryDir + "/ directory in your checkout is durable, " +
	"repo-wide memory left by past agents. Start at " + repoMemoryDir + "/" + repoMemoryIndex + " (the index) and " +
	"read the linked topic files relevant to your task before you begin. As you work, if you learn something durable " +
	"that would save a future agent time on this repo (architecture, a gotcha, the build/test commands that actually " +
	"work, a convention), add or correct a topic file there and link it from the index — keep it concise. It persists " +
	"across objectives and is local-only: it is never committed or included in your PR, so edit it freely."

// repoMemoryKey normalizes a repo/clone-url pair into the single key under which
// memory is stored. The github https URL and the "owner/repo" short form fold to
// the same key so an objective and its workers always agree.
func repoMemoryKey(repo, cloneURL string) string {
	s := strings.ToLower(strings.TrimSpace(repo))
	if s == "" {
		s = strings.ToLower(strings.TrimSpace(cloneURL))
	}
	if s == "" {
		return ""
	}
	s = strings.TrimSuffix(s, ".git")
	if i := strings.Index(s, "github.com"); i >= 0 {
		s = strings.TrimLeft(s[i+len("github.com"):], "/:")
	}
	return s
}

// safeRel rejects paths that escape the memory directory or are absolute, so an
// agent-created filename can never write outside .orcha/memory/.
func safeRel(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// seedRepoMemory writes the repo's stored memory files into a freshly-prepared
// checkout under .orcha/memory/ (plus a base snapshot for the merge-back) and
// excludes .orcha/ from git so it never leaks into the agent's diff. It no-ops
// when there is no preparer (offline/tests), no real checkout, or no repo key.
func (o *Orchestrator) seedRepoMemory(ctx context.Context, sess *model.Session, ws *model.Workspace, tgt *model.Target) {
	if o.preparer == nil || ws == nil || ws.Path == "" || tgt == nil {
		return
	}
	repo, _, cloneURL, _ := o.resolveRepo(sess)
	key := repoMemoryKey(repo, cloneURL)
	if key == "" {
		return
	}
	files, err := o.st.ListRepoMemoryFiles(key)
	if err != nil {
		return
	}

	seed := map[string]string{}
	for _, f := range files {
		if safeRel(f.Path) {
			seed[f.Path] = f.Content
		}
	}
	if len(seed) == 0 {
		seed[repoMemoryIndex] = repoMemoryScaffold // brand-new repo: lay down the index
	}

	ex := agent.NewExecutor(tgt)
	for p, content := range seed {
		if err := writeMemoryFile(ctx, ex, ws.Path, repoMemoryDir, p, content); err != nil {
			return
		}
		// Base snapshot, identical to what we seeded, so merge-back can tell what
		// the agent changed and 3-way merge against it if the store advanced.
		_ = writeMemoryFile(ctx, ex, ws.Path, repoMemoryBaseDir, p, content)
	}
	// Keep the whole .orcha/ dir out of `git status`/diffs locally (the checkout is
	// fresh each run, so a one-time append never accumulates duplicates).
	_, _ = exec.RunCapture(ctx, ex, exec.Command{Name: "tee", Args: []string{"-a", gitLocalExclude}, Dir: ws.Path, Stdin: repoMemoryRoot + "/\n"})
	// And keep gitignore-respecting tools (deno fmt/lint, prettier, rg) from
	// walking into the memory files — they don't honor .git/info/exclude, only
	// real .gitignore files. This one sits inside the excluded .orcha/, so it
	// never reaches the agent's diff.
	_, _ = exec.RunCapture(ctx, ex, exec.Command{Name: "tee", Args: []string{repoMemoryGitignore}, Dir: ws.Path, Stdin: "*\n"})
}

// mergeBackRepoMemory folds a finished session's edits to .orcha/memory/ back
// into the store, file by file. Each file is handled independently: untouched
// files are skipped, a file the agent created or edited is stored (3-way merged
// only if a concurrent session changed the same path meanwhile), and a file the
// agent deleted is removed unless a concurrent session changed it. The whole
// read-merge-write is serialized by repoMemMu.
func (o *Orchestrator) mergeBackRepoMemory(sess *model.Session) {
	if o.preparer == nil || sess == nil || sess.WorkspaceID == "" {
		return
	}
	repo, _, cloneURL, _ := o.resolveRepo(sess)
	key := repoMemoryKey(repo, cloneURL)
	if key == "" {
		return
	}
	ws, err := o.st.GetWorkspace(sess.WorkspaceID)
	if err != nil || ws == nil || ws.Path == "" {
		return
	}
	var tgt *model.Target
	if ws.TargetID != "" {
		tgt, _ = o.st.GetTarget(ws.TargetID)
	}
	if tgt == nil {
		return
	}
	ctx := context.Background()
	ex := agent.NewExecutor(tgt)

	theirs := listMemoryFiles(ctx, ex, ws.Path, repoMemoryDir)
	base := listMemoryFiles(ctx, ex, ws.Path, repoMemoryBaseDir)
	if len(theirs) == 0 && len(base) == 0 {
		return // nothing was seeded (e.g. workspace reclaimed) — nothing to merge
	}

	o.repoMemMu.Lock()
	defer o.repoMemMu.Unlock()

	ours := map[string]string{}
	if rows, err := o.st.ListRepoMemoryFiles(key); err == nil {
		for _, f := range rows {
			ours[f.Path] = f.Content
		}
	}

	// Only paths the agent could have touched: those present now and those seeded.
	paths := map[string]struct{}{}
	for p := range theirs {
		paths[p] = struct{}{}
	}
	for p := range base {
		paths[p] = struct{}{}
	}

	changed := 0
	for p := range paths {
		t, hasT := theirs[p]
		b, hasB := base[p]
		o2, hasO := ours[p]
		switch {
		case !hasT && hasB:
			// Agent deleted a seeded file. Honor it only if the store was not
			// changed concurrently, so one worker's delete can't drop another's edit.
			if !hasO || eq(o2, b) {
				_ = o.st.DeleteRepoMemoryFile(key, p)
				changed++
			}
		case hasT && !hasB:
			// New file the agent created.
			if !hasO {
				_ = o.st.UpsertRepoMemoryFile(key, p, t)
				changed++
			} else if !eq(o2, t) {
				// A concurrent session created the same path: 3-way against empty base.
				_ = o.st.UpsertRepoMemoryFile(key, p, o.mergeMemoryFile(ctx, ex, ws.Path, sess, p, o2, "", t))
				changed++
			}
		case hasT && hasB:
			if eq(t, b) {
				continue // unchanged by the agent; leave any concurrent store update intact
			}
			if !hasO || eq(o2, b) {
				_ = o.st.UpsertRepoMemoryFile(key, p, t) // no concurrent change; agent's version wins
				changed++
			} else if !eq(o2, t) {
				_ = o.st.UpsertRepoMemoryFile(key, p, o.mergeMemoryFile(ctx, ex, ws.Path, sess, p, o2, b, t))
				changed++
			}
		}
	}
	if changed > 0 {
		o.audit(sess.ObjectiveID, sess.ID, "repo_memory_updated", fmt.Sprintf("%s (%d file(s))", key, changed), nil)
	}
}

// mergeMemoryFile 3-way merges one file (git merge-file) when both the store and
// the agent changed it since seeding. All three inputs are written from the same
// (trimmed) strings into controlled paths — never the agent's on-disk file —
// so trailing-whitespace differences can't manufacture a spurious EOF conflict.
// On a hard merge failure it keeps the existing store content rather than risk
// losing it; a real conflict (both sides kept, with markers) is recorded for a
// human/next agent to reconcile.
func (o *Orchestrator) mergeMemoryFile(ctx context.Context, ex exec.Executor, wsPath string, sess *model.Session, rel, ours, base, theirs string) string {
	if err := writeMemoryFile(ctx, ex, wsPath, repoMemoryOursDir, rel, ours); err != nil {
		return ours
	}
	if err := writeMemoryFile(ctx, ex, wsPath, repoMemoryBaseDir, rel, base); err != nil {
		return ours
	}
	if err := writeMemoryFile(ctx, ex, wsPath, repoMemoryTheirsDir, rel, theirs); err != nil {
		return ours
	}
	out, mErr := exec.Capture(ctx, ex, exec.Command{
		Name: "git",
		Args: []string{"merge-file", "-p", "--diff3",
			repoMemoryOursDir + "/" + rel, repoMemoryBaseDir + "/" + rel, repoMemoryTheirsDir + "/" + rel},
		Dir: wsPath,
	})
	if exec.ExitCode(mErr) < 0 {
		o.audit(sess.ObjectiveID, sess.ID, "repo_memory_merge_failed", rel, nil)
		return ours
	}
	if strings.Contains(out, "<<<<<<<") {
		o.audit(sess.ObjectiveID, sess.ID, "repo_memory_merge_conflict", "kept both sides with markers in "+rel, nil)
	}
	return out
}

// eq compares two file contents ignoring surrounding whitespace (cat/Capture
// trims, so an exact byte compare would spuriously differ).
func eq(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) }

// listMemoryFiles returns the files under a checkout directory as a map of
// path-relative-to-dir -> content. A missing directory yields an empty map.
func listMemoryFiles(ctx context.Context, ex exec.Executor, wsPath, dir string) map[string]string {
	out := map[string]string{}
	listing, err := exec.Capture(ctx, ex, exec.Command{Name: "find", Args: []string{dir, "-type", "f"}, Dir: wsPath})
	if err != nil {
		return out
	}
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rel := strings.TrimPrefix(line, dir+"/")
		if rel == line || !safeRel(rel) {
			continue
		}
		content, err := readWorkspaceFile(ctx, ex, wsPath, line)
		if err != nil {
			continue
		}
		out[rel] = content
	}
	return out
}

// writeMemoryFile writes content to baseDir/rel within the checkout, creating the
// parent directory first.
func writeMemoryFile(ctx context.Context, ex exec.Executor, wsPath, baseDir, rel, content string) error {
	full := baseDir + "/" + rel
	if parent := path.Dir(full); parent != "." {
		if _, err := exec.RunCapture(ctx, ex, exec.Command{Name: "mkdir", Args: []string{"-p", parent}, Dir: wsPath}); err != nil {
			return err
		}
	}
	return writeWorkspaceFile(ctx, ex, wsPath, full, content)
}

// writeWorkspaceFile writes exact content to a checkout-relative path via the
// target's executor, piping the content through `tee`'s stdin. Content goes over
// stdin (not an env var or argv) because memory grows unbounded and those have
// hard size limits (~128KB per env string on Linux, ARG_MAX over SSH); a pipe
// does not. The executor delivers Command.Stdin and closes it, so tee sees EOF and
// exits — over SSH this runs without a pty so the EOF actually reaches the remote
// tee (a forced -tt pty would swallow it and hang). Works local or over SSH.
func writeWorkspaceFile(ctx context.Context, ex exec.Executor, dir, rel, content string) error {
	// CloseStdin so even empty content (a memory file an agent blanked) sends EOF
	// and tee exits — without it an empty Stdin reads as interactive, the pipe is
	// left open, and tee hangs forever (over SSH the -tt pty swallows the EOF). A
	// single 0-byte memory row hanging here on resume wedged every manager once.
	_, err := exec.RunCapture(ctx, ex, exec.Command{Name: "tee", Args: []string{rel}, Dir: dir, Stdin: content, CloseStdin: true})
	return err
}

// readWorkspaceFile returns the contents of a checkout-relative path, or an
// error if it does not exist.
func readWorkspaceFile(ctx context.Context, ex exec.Executor, dir, rel string) (string, error) {
	return exec.Capture(ctx, ex, exec.Command{Name: "cat", Args: []string{rel}, Dir: dir})
}
