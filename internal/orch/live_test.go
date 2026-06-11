package orch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/forge"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/workspace"
)

// TestLive_OneShotPR exercises the real write path end to end against a real
// GitHub repo: prepare a fresh checkout (workspace backend), have Claude make a
// small README enhancement (agent), then push the branch and open a PR (forge
// backend). Gated behind ORCHA_LIVE_PR=1 because it creates a real PR.
//
//	ORCHA_LIVE_PR=1 ORCHA_LIVE_REPO=nathanwhit/aged go test ./internal/orch/ -run TestLive_OneShotPR -v
func TestLive_OneShotPR(t *testing.T) {
	if os.Getenv("ORCHA_LIVE_PR") != "1" {
		t.Skip("set ORCHA_LIVE_PR=1 to create a real PR")
	}
	repo := os.Getenv("ORCHA_LIVE_REPO")
	if repo == "" {
		repo = "nathanwhit/aged"
	}
	ctx := context.Background()
	ex := exec.NewLocal()
	root := t.TempDir()
	work := root + "/work"
	dir := work + "/checkout"
	branch := fmt.Sprintf("orcha/readme-oneshot-%d", time.Now().Unix())
	cloneURL := "https://github.com/" + repo + ".git"

	// 1) Fresh checkout off the latest main (workspace backend).
	if err := workspace.New().PrepareIsolated(ctx, ex, workspace.Spec{
		WorkRoot: work, RepoURL: cloneURL, Dir: dir, Base: "main", Branch: branch,
	}); err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	t.Logf("prepared fresh checkout on branch %s", branch)

	// 2) Claude makes a small README enhancement (edits only; no shell).
	prompt := "Read README.md in this repository and make ONE small, genuinely useful " +
		"enhancement — improve clarity, fix a typo, or tighten wording. Edit README.md " +
		"directly with the Edit tool. Keep the change small and safe. Do not run any " +
		"shell or git commands. When finished, reply with a one-line summary."
	cl := osexec.CommandContext(ctx, "claude", "-p", prompt,
		"--permission-mode", "acceptEdits",
		"--allowedTools", "Read", "Edit", "Write", "Glob", "Grep")
	cl.Dir = dir
	out, err := cl.CombinedOutput()
	if err != nil {
		t.Fatalf("claude edit: %v\n%s", err, out)
	}
	t.Logf("claude: %s", strings.TrimSpace(string(out)))

	// 3) Confirm there is a real diff (mechanical safety), then commit it.
	g := forge.NewGit()
	if has, err := g.HasDiff(ctx, dir); err != nil || !has {
		t.Fatalf("expected a README diff from the agent: has=%v err=%v", has, err)
	}
	tgit(t, dir, "add", "-A")
	tgit(t, dir, "commit", "-m", "docs: small README enhancement\n\nAutomated one-shot via orcha.")

	// 4) Push the branch and open the PR (forge backend).
	sha, err := g.PushBranch(ctx, repo, dir, branch, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	t.Logf("pushed %s at %s", branch, sha)

	res, err := g.OpenPR(ctx, repo, branch, "main",
		"docs: small README enhancement",
		"A small README improvement, opened end-to-end by **orcha**:\n\n"+
			"- fresh checkout prepared off latest `main`\n"+
			"- change authored by Claude (edit-only)\n"+
			"- branch pushed and PR opened by the git+gh forge backend\n")
	if err != nil {
		t.Fatalf("open PR: %v", err)
	}
	if res.Number == 0 {
		t.Fatalf("expected a PR number, got %+v", res)
	}
	t.Logf("opened PR #%d: %s", res.Number, res.URL)
}

// TestLive_ManagerToolCalling drives a real manager: Claude connects to the MCP
// server over HTTP and calls spawn_session, which must create a worker in the
// orchestrator. Gated behind ORCHA_LIVE_MANAGER=1 (spends tokens).
//
//	ORCHA_LIVE_MANAGER=1 go test ./internal/orch/ -run TestLive_ManagerToolCalling -v
func TestLive_ManagerToolCalling(t *testing.T) {
	if os.Getenv("ORCHA_LIVE_MANAGER") != "1" {
		t.Skip("set ORCHA_LIVE_MANAGER=1 to run the live manager tool-calling test")
	}
	o, st := newTestOrch(t)
	addTarget(t, st, "local", model.TargetLocal, 4)
	o.RegisterProvider(agent.NewClaude(agent.ClaudeConfig{}))

	// Stand up the manager MCP surface and point the orchestrator at it.
	mux := http.NewServeMux()
	mux.Handle("/mcp/", http.StripPrefix("/mcp", o.ManagerMCPHandler()))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	o.cfg.ManagerMCPBaseURL = srv.URL

	obj, mgr, err := o.CreateObjective(NewObjectiveSpec{
		Title:  "tiny",
		Prompt: "Use your spawn_session tool to create exactly ONE implementer worker with title 'hello' and goal 'print hello world'. Then stop. Do nothing else.",
		Agent:  model.AgentClaude,
	})
	if err != nil {
		t.Fatalf("create objective: %v", err)
	}

	run, err := o.StartRun(context.Background(), mgr.ID)
	if err != nil {
		t.Fatalf("start manager: %v", err)
	}
	defer func() { _ = o.Cancel(mgr.ID, true); _ = run }()

	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		sessions, _ := st.ListSessionsByObjective(obj.ID)
		for _, s := range sessions {
			if s.Role == model.RoleImplementer {
				t.Logf("manager spawned worker %s (%q) via MCP tool call", s.ID, s.Title)
				return // success: the live tool call reached the orchestrator
			}
		}
		time.Sleep(time.Second)
	}

	// Dump the manager transcript to debug a protocol/handshake issue.
	msgs, _ := st.MessagesAfter(mgr.ID, 0, 200)
	for _, m := range msgs {
		t.Logf("[%s/%s] %s", m.Source, m.Kind, truncate(m.Content, 200))
	}
	t.Fatal("manager did not spawn a worker via MCP within the deadline")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
