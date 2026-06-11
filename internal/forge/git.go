package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// GitForge is a real Forge backed by the local `git` and `gh` CLIs. Git handles
// branch pushes and diff detection in a workspace checkout; gh handles
// repo/PR/comment operations against GitHub. Every method shells out — these are
// the external calls the orchestrator deliberately keeps outside DB
// transactions.
type GitForge struct {
	// GitBin / GHBin allow overriding the executables (default "git"/"gh").
	GitBin string
	GHBin  string
}

// NewGit returns a GitForge using the default git/gh executables.
func NewGit() *GitForge { return &GitForge{GitBin: "git", GHBin: "gh"} }

func (g *GitForge) gitBin() string {
	if g.GitBin == "" {
		return "git"
	}
	return g.GitBin
}

func (g *GitForge) ghBin() string {
	if g.GHBin == "" {
		return "gh"
	}
	return g.GHBin
}

// run executes a command in dir and returns trimmed stdout, or an error that
// includes stderr for diagnosis.
func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()),
			fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (g *GitForge) git(ctx context.Context, dir string, args ...string) (string, error) {
	return run(ctx, dir, g.gitBin(), args...)
}

func (g *GitForge) gh(ctx context.Context, args ...string) (string, error) {
	return run(ctx, "", g.ghBin(), args...)
}

// RepoExists checks reachability via `gh repo view`.
func (g *GitForge) RepoExists(ctx context.Context, repo string) (bool, error) {
	_, err := g.gh(ctx, "repo", "view", repo, "--json", "name")
	if err != nil {
		// gh exits non-zero for a missing/unauthorized repo; treat as "absent"
		// rather than a hard error so the publish check can report it cleanly.
		if strings.Contains(err.Error(), "Could not resolve") ||
			strings.Contains(err.Error(), "not found") ||
			strings.Contains(err.Error(), "404") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// HasDiff reports whether the workspace has anything worth publishing: either
// uncommitted working-tree changes, or commits on the current branch that are
// ahead of its base (the remote-tracking upstream, falling back to the remote's
// default branch).
func (g *GitForge) HasDiff(ctx context.Context, workspacePath string) (bool, error) {
	status, err := g.git(ctx, workspacePath, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if status != "" {
		return true, nil // dirty working tree
	}
	base, err := g.resolveBase(ctx, workspacePath)
	if err != nil || base == "" {
		// No base to compare against (e.g. a brand-new repo): nothing provably
		// publishable beyond a clean tree.
		return false, nil
	}
	count, err := g.git(ctx, workspacePath, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return false, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(count))
	return n > 0, nil
}

// resolveBase finds a ref to diff the branch against: the current branch's
// upstream if set, else the remote's default branch (origin/HEAD).
func (g *GitForge) resolveBase(ctx context.Context, dir string) (string, error) {
	if up, err := g.git(ctx, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil && up != "" {
		return up, nil
	}
	if head, err := g.git(ctx, dir, "symbolic-ref", "refs/remotes/origin/HEAD"); err == nil && head != "" {
		// e.g. "refs/remotes/origin/main" -> "origin/main"
		return strings.TrimPrefix(head, "refs/remotes/"), nil
	}
	return "", nil
}

// PushBranch pushes the workspace's branch to origin. force is only set when the
// caller has explicitly chosen and recorded a force push.
func (g *GitForge) PushBranch(ctx context.Context, repo, workspacePath, branch string, force bool) (string, error) {
	args := []string{"push", "origin", branch}
	if force {
		// --force-with-lease is safer than --force: it refuses to clobber
		// unexpected remote updates.
		args = []string{"push", "--force-with-lease", "origin", branch}
	}
	if _, err := g.git(ctx, workspacePath, args...); err != nil {
		return "", err
	}
	// Resolve the pushed head SHA.
	sha, err := g.git(ctx, workspacePath, "rev-parse", branch)
	if err != nil {
		return "", err
	}
	return sha, nil
}

// OpenPR opens a pull request via gh and returns its number/url/head.
func (g *GitForge) OpenPR(ctx context.Context, repo, branch, base, title, body string) (OpenResult, error) {
	out, err := g.gh(ctx, "pr", "create",
		"--repo", repo, "--head", branch, "--base", base,
		"--title", title, "--body", body)
	if err != nil {
		return OpenResult{}, err
	}
	url := lastURL(out)
	num := prNumberFromURL(url)
	res := OpenResult{Number: num, URL: url}
	// Best-effort head SHA from the freshly-created PR.
	if num > 0 {
		if st, err := g.GetPRState(ctx, repo, num); err == nil {
			res.HeadSHA = st.HeadSHA
		}
	}
	return res, nil
}

// GetPRState fetches the PR's status and aggregate checks from gh.
func (g *GitForge) GetPRState(ctx context.Context, repo string, number int) (PRState, error) {
	out, err := g.gh(ctx, "pr", "view", strconv.Itoa(number), "--repo", repo,
		"--json", "number,url,state,isDraft,headRefOid,statusCheckRollup")
	if err != nil {
		return PRState{}, err
	}
	var raw struct {
		Number            int    `json:"number"`
		URL               string `json:"url"`
		State             string `json:"state"` // OPEN | CLOSED | MERGED
		IsDraft           bool   `json:"isDraft"`
		HeadRefOid        string `json:"headRefOid"`
		StatusCheckRollup []struct {
			Status     string `json:"status"`     // QUEUED|IN_PROGRESS|COMPLETED
			Conclusion string `json:"conclusion"` // SUCCESS|FAILURE|...
			State      string `json:"state"`      // SUCCESS|FAILURE|PENDING (statuses)
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return PRState{}, err
	}
	return PRState{
		Number:      raw.Number,
		URL:         raw.URL,
		Status:      ghStatus(raw.State, raw.IsDraft),
		ChecksState: ghChecks(raw.StatusCheckRollup),
		HeadSHA:     raw.HeadRefOid,
	}, nil
}

// Comment posts a PR comment via gh.
func (g *GitForge) Comment(ctx context.Context, repo string, number int, body string) error {
	_, err := g.gh(ctx, "pr", "comment", strconv.Itoa(number), "--repo", repo, "--body", body)
	return err
}

// ---- helpers ----

func ghStatus(state string, isDraft bool) string {
	switch strings.ToUpper(state) {
	case "MERGED":
		return "merged"
	case "CLOSED":
		return "closed"
	default:
		if isDraft {
			return "draft"
		}
		return "open"
	}
}

func ghChecks(rollup []struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}) string {
	if len(rollup) == 0 {
		return "unknown"
	}
	anyPending, anyFailing := false, false
	for _, c := range rollup {
		// Check runs use Status/Conclusion; commit statuses use State.
		concl := strings.ToUpper(c.Conclusion)
		st := strings.ToUpper(c.Status)
		state := strings.ToUpper(c.State)
		switch {
		case concl == "FAILURE" || concl == "TIMED_OUT" || concl == "CANCELLED" || state == "FAILURE" || state == "ERROR":
			anyFailing = true
		case st != "" && st != "COMPLETED":
			anyPending = true
		case state == "PENDING":
			anyPending = true
		}
	}
	switch {
	case anyFailing:
		return "failing"
	case anyPending:
		return "pending"
	default:
		return "passing"
	}
}

// lastURL returns the last whitespace-separated token that looks like a URL.
func lastURL(out string) string {
	fields := strings.Fields(out)
	for i := len(fields) - 1; i >= 0; i-- {
		if strings.HasPrefix(fields[i], "http://") || strings.HasPrefix(fields[i], "https://") {
			return fields[i]
		}
	}
	return strings.TrimSpace(out)
}

// prNumberFromURL extracts the trailing number from a .../pull/<n> URL.
func prNumberFromURL(url string) int {
	url = strings.TrimRight(url, "/")
	i := strings.LastIndex(url, "/")
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(url[i+1:])
	return n
}

// Ensure GitForge satisfies the Forge interface.
var _ Forge = (*GitForge)(nil)
