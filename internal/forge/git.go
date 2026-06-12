package forge

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/nathanwhit/orcha/internal/exec"
)

// GitForge is a real Forge backed by the `git` and `gh` CLIs. Git handles branch
// pushes and diff detection in a workspace checkout; gh handles repo/PR/comment
// operations against GitHub. Every method shells out — these are the external
// calls the orchestrator deliberately keeps outside DB transactions.
//
// Commands run through Exec, which is the crux for remote targets: a worker's
// checkout lives on its target, and gh is authenticated there, so the forge must
// run git/gh ON THAT TARGET — not on the orchestrator host, where the checkout
// path does not exist. Exec defaults to the local host; OnExecutor rebinds it to
// a workspace's target.
type GitForge struct {
	// GitBin / GHBin allow overriding the executables (default "git"/"gh").
	GitBin string
	GHBin  string
	// Exec runs the commands; nil means the local host.
	Exec exec.Executor
}

// NewGit returns a GitForge using the default git/gh executables on the local
// host.
func NewGit() *GitForge { return &GitForge{GitBin: "git", GHBin: "gh"} }

// OnExecutor returns a copy of the forge that runs its commands on ex (e.g. a
// worker's SSH target). Implements the orchestrator's retargetable forge.
func (g *GitForge) OnExecutor(ex exec.Executor) Forge {
	cp := *g
	cp.Exec = ex
	return &cp
}

func (g *GitForge) executor() exec.Executor {
	if g.Exec != nil {
		return g.Exec
	}
	return exec.NewLocal()
}

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

func (g *GitForge) git(ctx context.Context, dir string, args ...string) (string, error) {
	return exec.Capture(ctx, g.executor(), exec.Command{Name: g.gitBin(), Args: args, Dir: dir})
}

func (g *GitForge) gh(ctx context.Context, args ...string) (string, error) {
	return exec.Capture(ctx, g.executor(), exec.Command{Name: g.ghBin(), Args: args})
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

// CommitAll is a fallback that stages and commits any changes the agent left
// uncommitted. It uses the checkout's inherited git identity (the user's normal
// author), not a synthetic one. Agents are expected to commit their own work
// with their own message; this only catches leftovers.
func (g *GitForge) CommitAll(ctx context.Context, workspacePath, message string) (bool, error) {
	if _, err := g.git(ctx, workspacePath, "add", "-A"); err != nil {
		return false, err
	}
	// `diff --cached --quiet` exits non-zero when there is something staged.
	if _, err := g.git(ctx, workspacePath, "diff", "--cached", "--quiet"); err == nil {
		return false, nil // nothing to commit (the agent already committed)
	}
	if message == "" {
		message = "Apply changes"
	}
	if _, err := g.git(ctx, workspacePath, "commit", "-m", message); err != nil {
		return false, err
	}
	return true, nil
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

// FindOpenPR returns the open PR whose head ref is `branch` on `repo`, or nil if
// none. gh pr list matches headRefName, so it finds PRs opened from a fork too.
func (g *GitForge) FindOpenPR(ctx context.Context, repo, branch string) (*PRState, error) {
	out, err := g.gh(ctx, "pr", "list", "--repo", repo, "--head", branch, "--state", "open",
		"--limit", "1", "--json", "number,url,state,isDraft,headRefOid,title,statusCheckRollup")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Number            int    `json:"number"`
		URL               string `json:"url"`
		State             string `json:"state"`
		IsDraft           bool   `json:"isDraft"`
		HeadRefOid        string `json:"headRefOid"`
		Title             string `json:"title"`
		StatusCheckRollup []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	r := raw[0]
	return &PRState{
		Number:      r.Number,
		URL:         r.URL,
		Status:      ghStatus(r.State, r.IsDraft),
		ChecksState: ghChecks(r.StatusCheckRollup),
		HeadSHA:     r.HeadRefOid,
		Title:       r.Title,
	}, nil
}

// Comment posts a PR comment via gh.
func (g *GitForge) Comment(ctx context.Context, repo string, number int, body string) error {
	_, err := g.gh(ctx, "pr", "comment", strconv.Itoa(number), "--repo", repo, "--body", body)
	return err
}

// ListComments returns the PR's issue comments and review bodies via gh.
func (g *GitForge) ListComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	out, err := g.gh(ctx, "pr", "view", strconv.Itoa(number), "--repo", repo, "--json", "comments,reviews")
	if err != nil {
		return nil, err
	}
	var raw struct {
		Comments []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body string `json:"body"`
			URL  string `json:"url"`
		} `json:"comments"`
		Reviews []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body        string `json:"body"`
			State       string `json:"state"`
			SubmittedAt string `json:"submittedAt"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	var cs []Comment
	for _, c := range raw.Comments {
		cs = append(cs, Comment{ExternalID: c.URL, Author: c.Author.Login, Body: c.Body, Kind: "issue_comment"})
	}
	for _, r := range raw.Reviews {
		if strings.TrimSpace(r.Body) == "" {
			continue // approvals/empty reviews carry no actionable text
		}
		cs = append(cs, Comment{
			ExternalID: "review:" + r.Author.Login + ":" + r.SubmittedAt,
			Author:     r.Author.Login, Body: r.Body, Kind: "review",
		})
	}
	return cs, nil
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
