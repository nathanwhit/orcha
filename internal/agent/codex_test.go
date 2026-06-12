package agent

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nathanwhit/orcha/internal/mcp"
	"github.com/nathanwhit/orcha/internal/model"
)

func TestCodexArgs_FreshVsResume(t *testing.T) {
	fresh := codexArgs("", nil, "", "acceptEdits", nil)
	if strings.Join(fresh, " ") != "exec --json --skip-git-repo-check --sandbox workspace-write" {
		t.Fatalf("fresh args = %v", fresh)
	}
	resume := codexArgs("o3", nil, "thread-123", "acceptEdits", nil)
	got := strings.Join(resume, " ")
	want := "exec resume --json --skip-git-repo-check --sandbox workspace-write --model o3 thread-123 -"
	if got != want {
		t.Fatalf("resume args =\n got %q\nwant %q", got, want)
	}
}

func TestCodexArgs_BypassPermissions(t *testing.T) {
	args := strings.Join(codexArgs("", nil, "", "bypassPermissions", nil), " ")
	if !strings.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("bypass mode missing the dangerous flag: %q", args)
	}
	if strings.Contains(args, "--sandbox workspace-write") {
		t.Fatalf("bypass mode should not also set a sandbox: %q", args)
	}
}

func TestCodexArgs_MCP(t *testing.T) {
	mcp := map[string]string{"orcha": "http://127.0.0.1:18080/mcp/sess-1"}
	// MCP override is present and quoted as a TOML string.
	want := `-c mcp_servers.orcha.url="http://127.0.0.1:18080/mcp/sess-1"`
	fresh := strings.Join(codexArgs("", nil, "", "bypassPermissions", mcp), " ")
	if !strings.Contains(fresh, want) {
		t.Fatalf("fresh args missing MCP override:\n got %q\nwant substring %q", fresh, want)
	}
	// On resume, the MCP flag must precede the positional SESSION_ID/PROMPT so it
	// parses as an option, not a prompt.
	resume := codexArgs("", nil, "thread-9", "bypassPermissions", mcp)
	ci, ti := indexOf(resume, "-c"), indexOf(resume, "thread-9")
	if ci < 0 || ti < 0 || ci > ti {
		t.Fatalf("MCP flag must come before the positional thread id: %v", resume)
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

func TestCodexParser_RealSchema(t *testing.T) {
	p := codexParser{}

	// thread.started surfaces the resumable thread id as provider_session_id.
	evs := p.Parse(`{"type":"thread.started","thread_id":"019eb865-0665-7d52-afd3-98b7a7f5fc2f"}`)
	if len(evs) != 1 || evs[0].Metadata["provider_session_id"] != "019eb865-0665-7d52-afd3-98b7a7f5fc2f" {
		t.Fatalf("thread.started: %+v", evs)
	}

	// agent_message item -> text event.
	evs = p.Parse(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"OK"}}`)
	if len(evs) != 1 || evs[0].Kind != EventText || evs[0].Content != "OK" {
		t.Fatalf("agent_message: %+v", evs)
	}

	// turn.completed usage -> usage event.
	evs = p.Parse(`{"type":"turn.completed","usage":{"input_tokens":30360,"output_tokens":5}}`)
	if len(evs) != 1 || evs[0].Kind != EventUsage || evs[0].UsedTokens != 30365 {
		t.Fatalf("usage: %+v", evs)
	}

	// non-JSON preserved as stdout.
	if evs := p.Parse("plain text"); len(evs) != 1 || evs[0].Kind != EventStdout {
		t.Fatalf("plain: %+v", evs)
	}
}

// TestCodex_LiveResumePreservesContext proves that `codex exec resume <thread>`
// continues the same conversation — the mechanism the provider uses for
// context-preserving steering. Gated behind ORCHA_CODEX_LIVE=1 (spends tokens).
func TestCodex_LiveResumePreservesContext(t *testing.T) {
	if os.Getenv("ORCHA_CODEX_LIVE") != "1" {
		t.Skip("set ORCHA_CODEX_LIVE=1 to run the live Codex resume test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Turn 1: establish context and capture the thread id.
	c1 := osexec.CommandContext(ctx, "codex", codexArgs("", nil, "", "bypassPermissions", nil)...)
	c1.Stdin = strings.NewReader("Remember the secret number 42. Just acknowledge.")
	out1, err := c1.CombinedOutput()
	if err != nil {
		t.Fatalf("turn 1: %v\n%s", err, out1)
	}
	var threadID string
	for _, line := range strings.Split(string(out1), "\n") {
		for _, ev := range (codexParser{}).Parse(line) {
			if id, ok := ev.Metadata["provider_session_id"].(string); ok {
				threadID = id
			}
		}
	}
	if threadID == "" {
		t.Fatalf("no thread id captured from turn 1:\n%s", out1)
	}
	t.Logf("thread id: %s", threadID)

	// Turn 2: resume and ask for the remembered value.
	c2 := osexec.CommandContext(ctx, "codex", codexArgs("", nil, threadID, "bypassPermissions", nil)...)
	c2.Stdin = bytes.NewBufferString("What was the secret number? Reply with just the number.")
	out2, err := c2.CombinedOutput()
	if err != nil {
		t.Fatalf("turn 2 (resume): %v\n%s", err, out2)
	}
	if !strings.Contains(string(out2), "42") {
		t.Fatalf("resumed session lost context (no 42):\n%s", out2)
	}
	t.Log("resume preserved context (recalled 42)")
}

// TestLiveCodexTmuxMCP proves a codex session driven through orcha's own tmux
// provider can reach the orcha MCP tool surface and actually call a tool — the
// capability a codex manager needs to spawn workers and publish PRs. It serves
// orcha's real MCP server, launches codex via NewTmuxCodex wired with that
// server, and asserts the tool was invoked with the expected argument. Gated
// behind ORCHA_CODEX_LIVE=1 (spends tokens; needs codex auth + tmux).
func TestLiveCodexTmuxMCP(t *testing.T) {
	if os.Getenv("ORCHA_CODEX_LIVE") != "1" {
		t.Skip("set ORCHA_CODEX_LIVE=1 to run the live Codex MCP test")
	}
	if _, err := osexec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	// Orcha's real MCP server with one recording tool.
	var (
		mu      sync.Mutex
		gotNote string
	)
	srv := mcp.NewServer("orcha", "test")
	srv.AddTool(mcp.Tool{
		Name:        "report_status",
		Description: "Report a short status note back to the orchestrator.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"note": map[string]any{"type": "string"},
		}, "required": []any{"note"}},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			mu.Lock()
			gotNote = mcp.StringArg(args, "note")
			mu.Unlock()
			return "recorded", nil
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	p := NewTmuxCodex(CodexConfig{})
	spec := Spec{
		SessionID:      "codex-mcp-live-1",
		PermissionMode: "bypassPermissions",
		Workspace:      &model.Workspace{Path: t.TempDir()},
		MCP:            map[string]string{"orcha": ts.URL + "/" + "codex-mcp-live-1"},
		Prompt: "You have an MCP server named orcha. Call its tool report_status " +
			"with the argument note set to exactly CODEX-MCP-OK. After it returns, stop.",
	}
	h, events, err := p.StartSession(ctx, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.CancelSession(h)
	go func() {
		for range events {
		}
	}()

	deadline := time.Now().Add(2*time.Minute + 30*time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := gotNote
		mu.Unlock()
		if n != "" {
			if n != "CODEX-MCP-OK" {
				t.Fatalf("orcha tool called with note %q, want CODEX-MCP-OK", n)
			}
			t.Log("codex tmux session called the orcha MCP tool")
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("codex never called the orcha MCP tool within the deadline")
}
