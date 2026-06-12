package agent

import (
	"bytes"
	"context"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
	"time"
)

func TestCodexArgs_FreshVsResume(t *testing.T) {
	fresh := codexArgs("", nil, "", "acceptEdits")
	if strings.Join(fresh, " ") != "exec --json --skip-git-repo-check --sandbox workspace-write" {
		t.Fatalf("fresh args = %v", fresh)
	}
	resume := codexArgs("o3", nil, "thread-123", "acceptEdits")
	got := strings.Join(resume, " ")
	want := "exec resume --json --skip-git-repo-check --sandbox workspace-write --model o3 thread-123 -"
	if got != want {
		t.Fatalf("resume args =\n got %q\nwant %q", got, want)
	}
}

func TestCodexArgs_BypassPermissions(t *testing.T) {
	args := strings.Join(codexArgs("", nil, "", "bypassPermissions"), " ")
	if !strings.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("bypass mode missing the dangerous flag: %q", args)
	}
	if strings.Contains(args, "--sandbox workspace-write") {
		t.Fatalf("bypass mode should not also set a sandbox: %q", args)
	}
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
	c1 := osexec.CommandContext(ctx, "codex", codexArgs("", nil, "", "bypassPermissions")...)
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
	c2 := osexec.CommandContext(ctx, "codex", codexArgs("", nil, threadID, "bypassPermissions")...)
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
