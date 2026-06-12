package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nathanwhit/orcha/internal/model"
)

func TestTmuxProvider_InteractiveShellSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	p := NewTmux(TmuxConfig{Kind: model.AgentOther}) // nil Command => default shell

	h, events, err := p.StartSession(context.Background(), Spec{SessionID: "tmux-it-1"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !h.Interactive() {
		t.Fatal("tmux sessions are interactive")
	}

	// Collect events in the background.
	var (
		sawDone = make(chan bool, 1)
		attach  = make(chan string, 1)
	)
	go func() {
		for ev := range events {
			switch {
			case ev.Kind == EventStatus && ev.Metadata != nil:
				if a, ok := ev.Metadata["tmux_attach"].(string); ok {
					select {
					case attach <- a:
					default:
					}
				}
			case ev.Kind == EventDone:
				select {
				case sawDone <- ev.Success:
				default:
				}
			}
		}
	}()

	// The attach command is surfaced so a human can watch the live pane.
	select {
	case a := <-attach:
		if !strings.Contains(a, "tmux attach -t orcha-tmux-it-1") {
			t.Fatalf("attach command = %q", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no attach command surfaced")
	}

	// Steer the live shell via send-keys; the effect is visible in the live
	// pane snapshot (pane output is not streamed into the transcript).
	if err := p.SendInput(h, "echo TMUX-PROVIDER-OK"); err != nil {
		t.Fatalf("send input: %v", err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if screen, err := p.Snapshot(h); err == nil && strings.Contains(screen, "TMUX-PROVIDER-OK") {
			goto steered
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("did not observe steered output in the pane snapshot")
steered:

	// Cancel -> the session ends.
	if err := p.CancelSession(h); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-sawDone:
	case <-time.After(6 * time.Second):
		t.Fatal("no done event after cancel")
	}
}

func TestTmuxProvider_Snapshot(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	p := NewTmux(TmuxConfig{Kind: model.AgentOther})
	h, events, err := p.StartSession(context.Background(), Spec{SessionID: "tmux-snap-1"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() {
		for range events {
		}
	}() // drain
	defer p.CancelSession(h)

	if err := p.SendInput(h, "echo SNAPSHOT-CONTENT"); err != nil {
		t.Fatalf("send: %v", err)
	}
	// The provider implements the Snapshotter capability.
	var snap Snapshotter = p
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		screen, err := snap.Snapshot(h)
		if err == nil && strings.Contains(screen, "SNAPSHOT-CONTENT") {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("snapshot never showed the typed content")
}

// An orchestrator restart must ADOPT a still-live tmux session — the TUI keeps
// its full context — never kill and recreate it.
func TestTmuxProvider_ResumeAdoptsLiveSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const id = "tmux-adopt-1"

	// First process: start a shell session and put a marker on its screen.
	p1 := NewTmux(TmuxConfig{Kind: model.AgentOther})
	h1, ev1, err := p1.StartSession(context.Background(), Spec{SessionID: id})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() {
		for range ev1 {
		}
	}()
	if err := p1.SendInput(h1, "echo ADOPT-MARKER"); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitForScreen(t, p1, h1, "ADOPT-MARKER")

	// "Restart": a fresh provider instance with no in-memory state. Resume must
	// find the live tmux session and adopt it — the marker is still on screen,
	// which a kill-and-recreate would have wiped.
	p2 := NewTmux(TmuxConfig{Kind: model.AgentOther})
	h2, ev2, err := p2.ResumeSession(context.Background(), id, Spec{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	sawDone := make(chan bool, 1)
	adopted := make(chan string, 1)
	go func() {
		for ev := range ev2 {
			switch ev.Kind {
			case EventStatus:
				select {
				case adopted <- ev.Content:
				default:
				}
			case EventDone:
				select {
				case sawDone <- ev.Success:
				default:
				}
			}
		}
	}()
	select {
	case msg := <-adopted:
		if !strings.Contains(msg, "re-adopted") {
			t.Fatalf("status = %q, want re-adoption announcement", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no status event after resume")
	}
	if screen, err := p2.Snapshot(h2); err != nil || !strings.Contains(screen, "ADOPT-MARKER") {
		t.Fatalf("adopted screen lost prior context (err=%v):\n%s", err, screen)
	}

	// The adopted session is fully driveable: steer it and cancel it.
	if err := p2.SendInput(h2, "echo ADOPT-STEER"); err != nil {
		t.Fatalf("send after adopt: %v", err)
	}
	waitForScreen(t, p2, h2, "ADOPT-STEER")
	if err := p2.CancelSession(h2); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-sawDone:
	case <-time.After(6 * time.Second):
		t.Fatal("no done event after cancel")
	}
}

func waitForScreen(t *testing.T, p *TmuxProvider, h Handle, want string) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if screen, err := p.Snapshot(h); err == nil && strings.Contains(screen, want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("screen never showed %q", want)
}

// A blocking startup dialog (like claude's folder trust prompt) is accepted by
// the dialog watcher; the program proceeds without any typed input.
func TestTmuxProvider_AcceptsStartupDialog(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// A stand-in TUI: shows the trust question, blocks on Enter, then proceeds.
	script := `echo "Do you trust the files in this folder?"; read line; echo DIALOG-ACCEPTED; sleep 60`
	p := NewTmux(TmuxConfig{
		Kind:    model.AgentOther,
		Command: func(Spec) []string { return []string{"sh", "-c", script} },
		AcceptDialog: func(screen string) bool {
			return strings.Contains(screen, "Do you trust the files in this folder?") &&
				!strings.Contains(screen, "DIALOG-ACCEPTED")
		},
	})
	h, events, err := p.StartSession(context.Background(), Spec{SessionID: "tmux-dialog-1"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() {
		for range events {
		}
	}()
	defer p.CancelSession(h)
	waitForScreenT(t, p, h, "DIALOG-ACCEPTED", 8*time.Second)
}

// TestTmuxProvider_OneShotCompletesOnSentinel proves the core fix: a one-shot
// interactive session that never exits is still driven to completion when the
// agent prints the done sentinel. Without this a finished worker's TUI sits at
// its prompt forever and the manager is never notified.
func TestTmuxProvider_OneShotCompletesOnSentinel(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// A stand-in worker TUI: an idle shell that never exits on its own.
	p := NewTmux(TmuxConfig{Kind: model.AgentOther})
	h, events, err := p.StartSession(context.Background(), Spec{SessionID: "tmux-oneshot-1", OneShot: true})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var (
		done      = make(chan bool, 1)
		finalText = make(chan string, 1)
	)
	go func() {
		for ev := range events {
			switch ev.Kind {
			case EventText:
				select {
				case finalText <- ev.Content:
				default:
				}
			case EventDone:
				select {
				case done <- ev.Success:
				default:
				}
			}
		}
	}()

	// Let the shell settle, then "finish" by printing the sentinel as output —
	// printf so the (long) command line itself can't match, only the output line.
	time.Sleep(500 * time.Millisecond)
	if err := p.SendInput(h, "printf '%s\\n' "+TurnDoneSentinel); err != nil {
		t.Fatalf("send input: %v", err)
	}

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("one-shot session reported failure, want success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("one-shot session never completed after printing the sentinel")
	}
	// The pane content is surfaced as a final agent message for the handoff.
	select {
	case <-finalText:
	default:
		t.Fatal("no final message captured for the manager handoff")
	}
	// And the tmux session is torn down, not left lingering.
	if alive, _ := tmuxHasSession("orcha-tmux-oneshot-1"); alive {
		t.Fatal("tmux session still alive after completion")
	}
}

func TestPaneSignalsDone(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		want   bool
	}{
		{"plain sentinel line", "doing work\n" + TurnDoneSentinel + "\n\n❯ ", true},
		{"bulleted sentinel", "● " + TurnDoneSentinel + "\n❯ ", true},
		{"prose instruction only", "print " + TurnDoneSentinel + " as the very last line when finished\n❯ ", false},
		{"absent", "still working...\n❯ ", false},
		{"too far up", TurnDoneSentinel + strings.Repeat("\nfiller line that pushes it out of the tail window", 20) + "\n❯ ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := paneSignalsDone(c.screen); got != c.want {
				t.Fatalf("paneSignalsDone = %v, want %v", got, c.want)
			}
		})
	}
}

func TestFinalPaneMessage(t *testing.T) {
	screen := "● Done. Upgraded vite to ^8.\n" +
		"  Committed as dd46176.\n" +
		TurnDoneSentinel + "\n" +
		"\n" +
		"❯ \n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)"
	got := finalPaneMessage(screen)
	if !strings.Contains(got, "Committed as dd46176") {
		t.Fatalf("expected the agent summary, got %q", got)
	}
	if strings.Contains(got, TurnDoneSentinel) || strings.Contains(got, "bypass permissions") || strings.Contains(got, "❯") {
		t.Fatalf("chrome/sentinel leaked into summary: %q", got)
	}
}

func tmuxHasSession(name string) (bool, error) {
	err := exec.Command("tmux", "has-session", "-t", name).Run()
	return err == nil, err
}

func waitForScreenT(t *testing.T, p *TmuxProvider, h Handle, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if screen, err := p.Snapshot(h); err == nil && strings.Contains(screen, want) {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("screen never showed %q", want)
}
