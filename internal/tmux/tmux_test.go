package tmux

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	oexec "github.com/nathanwhit/orcha/internal/exec"
)

func tmuxAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

// Drive a real interactive shell inside tmux: start it, type a command via
// send-keys, and read it back from the live pane.
func TestTmux_InteractiveShell(t *testing.T) {
	tmuxAvailable(t)
	c := New(oexec.NewLocal())
	ctx := context.Background()
	name := SessionName("test-shell-" + sanitize(t.Name()))
	t.Cleanup(func() { _ = c.KillSession(ctx, name) })

	// Empty command => the target's default interactive shell.
	if err := c.NewSession(ctx, name, "", nil, nil); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	if ok, _ := c.HasSession(ctx, name); !ok {
		t.Fatal("session should be alive")
	}

	if err := c.SendKeys(ctx, name, "echo MARKER-ONE"); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	if !waitForPane(t, c, name, "MARKER-ONE") {
		t.Fatal("did not see MARKER-ONE in the live pane")
	}

	// Steer again — a second command into the same live shell.
	if err := c.SendKeys(ctx, name, "echo MARKER-TWO"); err != nil {
		t.Fatalf("send-keys 2: %v", err)
	}
	if !waitForPane(t, c, name, "MARKER-TWO") {
		t.Fatal("did not see MARKER-TWO after steering")
	}

	// Cancellation kills the session.
	if err := c.KillSession(ctx, name); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if ok, _ := c.HasSession(ctx, name); ok {
		t.Fatal("session should be gone after kill")
	}
}

func TestTmux_PipePaneStreamsOutput(t *testing.T) {
	tmuxAvailable(t)
	c := New(oexec.NewLocal())
	ctx := context.Background()
	name := SessionName("test-pipe-" + sanitize(t.Name()))
	t.Cleanup(func() { _ = c.KillSession(ctx, name) })

	logPath := t.TempDir() + "/pane.log"
	if err := c.NewSession(ctx, name, "", nil, nil); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	if err := c.PipePane(ctx, name, "cat >> "+logPath); err != nil {
		t.Fatalf("pipe-pane: %v", err)
	}
	if err := c.SendKeys(ctx, name, "echo STREAMED-LINE"); err != nil {
		t.Fatalf("send-keys: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := readFile(logPath)
		if strings.Contains(b, "STREAMED-LINE") {
			return // pipe-pane delivered the output to the log
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("pipe-pane did not stream output to the log")
}

func TestSessionName_Sanitizes(t *testing.T) {
	got := SessionName("a.b:c")
	if strings.ContainsAny(got, ".:") {
		t.Fatalf("session name not sanitized: %q", got)
	}
	if got != "orcha-a-b-c" {
		t.Fatalf("got %q", got)
	}
}

func waitForPane(t *testing.T, c *Controller, name, want string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := c.CapturePane(context.Background(), name)
		if strings.Contains(out, want) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func sanitize(s string) string {
	return strings.NewReplacer("/", "-", " ", "-").Replace(s)
}

func readFile(path string) (string, error) {
	b, err := exec.Command("cat", path).Output()
	return string(b), err
}
