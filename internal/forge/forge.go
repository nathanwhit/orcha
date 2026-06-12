// Package forge abstracts the git + code-host operations the orchestrator needs
// for the PR workflow (push, open/refresh PR). A fake implementation backs
// tests and local development; a real implementation would shell out to git and
// the gh CLI. None of these calls happen inside a DB transaction.
package forge

import (
	"context"
	"errors"
	"sync"

	"github.com/nathanwhit/orcha/internal/exec"
)

// Comment is an issue or review comment observed on a PR.
type Comment struct {
	ExternalID string // stable id for dedup (comment URL, or review key)
	Author     string
	Body       string
	Kind       string // "issue_comment" | "review"
}

// PRState is the live state of a PR on the host.
type PRState struct {
	Number      int
	URL         string
	Status      string // draft | open | merged | closed
	ChecksState string // unknown | pending | passing | failing
	HeadSHA     string
	Title       string // populated by FindOpenPR (for adoption); may be empty elsewhere
}

// OpenResult is returned when a PR is opened.
type OpenResult struct {
	Number  int
	URL     string
	HeadSHA string
}

// Forge is the host/VCS contract.
type Forge interface {
	// RepoExists reports whether the target repository is reachable.
	RepoExists(ctx context.Context, repo string) (bool, error)
	// HasDiff reports whether a workspace path has uncommitted/branch changes
	// worth publishing.
	HasDiff(ctx context.Context, workspacePath string) (bool, error)
	// PushBranch pushes the workspace branch to the repo. force must be
	// explicitly requested and is recorded with a reason by the caller.
	PushBranch(ctx context.Context, repo, workspacePath, branch string, force bool) (headSHA string, err error)
	// CommitAll stages and commits any uncommitted changes in the workspace with
	// message, returning whether a commit was made (false if the tree was clean).
	// Used so a worker that edits files but doesn't commit still yields a diff.
	CommitAll(ctx context.Context, workspacePath, message string) (committed bool, err error)
	// OpenPR opens a pull request.
	OpenPR(ctx context.Context, repo, branch, base, title, body string) (OpenResult, error)
	// GetPRState fetches the current PR state from the host.
	GetPRState(ctx context.Context, repo string, number int) (PRState, error)
	// FindOpenPR returns the open PR whose head is `branch` on `repo`, or nil if
	// there is none. Used to adopt PRs opened outside orcha (e.g. an agent that
	// ran the gh CLI) so they are tracked and monitored like any other.
	FindOpenPR(ctx context.Context, repo, branch string) (*PRState, error)
	// Comment posts an issue/PR comment.
	Comment(ctx context.Context, repo string, number int, body string) error
	// ListComments returns the PR's issue and review comments.
	ListComments(ctx context.Context, repo string, number int) ([]Comment, error)
}

// ErrRepoMissing indicates the target repo is unreachable.
var ErrRepoMissing = errors.New("forge: repository not found")

// Retargetable is an optional Forge capability: return a Forge that runs its
// external commands on a specific executor (e.g. a worker's SSH target, where
// its checkout and gh auth live). The orchestrator uses this so PR operations
// run on the machine that holds the checkout, not on the orchestrator host.
type Retargetable interface {
	OnExecutor(ex exec.Executor) Forge
}

// OnExecutor lets the Fake satisfy Retargetable; it ignores the executor (the
// Fake has no real commands to run) and returns itself.
func (f *Fake) OnExecutor(exec.Executor) Forge { return f }

// ---------------------------------------------------------------------------
// Fake
// ---------------------------------------------------------------------------

// Fake is an in-memory Forge for tests/dev.
type Fake struct {
	mu           sync.Mutex
	repos        map[string]bool
	diffs        map[string]bool     // workspacePath -> has diff
	prs          map[string]*PRState // repo#number -> state
	openByBranch map[string]*PRState // repo\x00branch -> open PR (for FindOpenPR)
	nextNum      int
	Pushes       []PushRecord
	ForcePush    []PushRecord
	Comments     []CommentRecord
	Commits      []CommitRecord
	incoming     []Comment
}

// PushRecord captures a push for assertions.
type PushRecord struct {
	Repo, Branch, WorkspacePath string
	Force                       bool
}

// CommitRecord captures a commit for assertions.
type CommitRecord struct {
	WorkspacePath, Message string
}

// CommentRecord captures a comment for assertions.
type CommentRecord struct {
	Repo   string
	Number int
	Body   string
}

// NewFake creates a Fake forge with all repos/diffs present by default.
func NewFake() *Fake {
	return &Fake{
		repos:        map[string]bool{},
		diffs:        map[string]bool{},
		prs:          map[string]*PRState{},
		openByBranch: map[string]*PRState{},
		nextNum:      100,
	}
}

// SetRepo marks a repo as existing (or not).
func (f *Fake) SetRepo(repo string, exists bool) { f.mu.Lock(); f.repos[repo] = exists; f.mu.Unlock() }

// SetDiff marks whether a workspace path has a diff.
func (f *Fake) SetDiff(path string, has bool) { f.mu.Lock(); f.diffs[path] = has; f.mu.Unlock() }

// SetPRState seeds/overrides the host state for a PR.
func (f *Fake) SetPRState(repo string, number int, st PRState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := st
	f.prs[key(repo, number)] = &cp
}

func key(repo string, n int) string {
	return repo + "#" + itoa(n)
}

func (f *Fake) RepoExists(_ context.Context, repo string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.repos[repo]; ok {
		return v, nil
	}
	return true, nil // default: exists
}

func (f *Fake) HasDiff(_ context.Context, workspacePath string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.diffs[workspacePath]; ok {
		return v, nil
	}
	return true, nil // default: has changes
}

func (f *Fake) CommitAll(_ context.Context, workspacePath, message string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Commits = append(f.Commits, CommitRecord{WorkspacePath: workspacePath, Message: message})
	return true, nil
}

func (f *Fake) PushBranch(_ context.Context, repo, workspacePath, branch string, force bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := PushRecord{Repo: repo, Branch: branch, WorkspacePath: workspacePath, Force: force}
	f.Pushes = append(f.Pushes, rec)
	if force {
		f.ForcePush = append(f.ForcePush, rec)
	}
	return "sha-" + branch + "-" + itoa(len(f.Pushes)), nil
}

func (f *Fake) OpenPR(_ context.Context, repo, branch, base, title, body string) (OpenResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextNum++
	n := f.nextNum
	st := &PRState{Number: n, URL: "https://forge.test/" + repo + "/pull/" + itoa(n), Status: "open", ChecksState: "pending", HeadSHA: "sha-" + branch}
	f.prs[key(repo, n)] = st
	return OpenResult{Number: n, URL: st.URL, HeadSHA: st.HeadSHA}, nil
}

func (f *Fake) GetPRState(_ context.Context, repo string, number int) (PRState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.prs[key(repo, number)]; ok {
		return *st, nil
	}
	return PRState{Number: number, Status: "open", ChecksState: "unknown"}, nil
}

// SetOpenPRByBranch seeds an out-of-band open PR that FindOpenPR will return for
// (repo, branch) — i.e. a PR orcha did not create.
func (f *Fake) SetOpenPRByBranch(repo, branch string, st PRState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := st
	f.openByBranch[repo+"\x00"+branch] = &cp
}

func (f *Fake) FindOpenPR(_ context.Context, repo, branch string) (*PRState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.openByBranch[repo+"\x00"+branch]; ok {
		cp := *st
		return &cp, nil
	}
	return nil, nil
}

func (f *Fake) Comment(_ context.Context, repo string, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Comments = append(f.Comments, CommentRecord{Repo: repo, Number: number, Body: body})
	return nil
}

func (f *Fake) ListComments(_ context.Context, repo string, number int) ([]Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Comment(nil), f.incoming...), nil
}

// SetComments seeds the comments ListComments returns.
func (f *Fake) SetComments(cs ...Comment) { f.mu.Lock(); f.incoming = cs; f.mu.Unlock() }

// itoa avoids importing strconv across many call sites.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
