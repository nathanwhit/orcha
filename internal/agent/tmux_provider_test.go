package agent

import (
	"context"
	"os/exec"
	"strings"
	"sync"
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
		mu      stringSet
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
			case ev.Kind == EventStdout:
				mu.add(ev.Content)
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

	// Steer the live shell via send-keys.
	if err := p.SendInput(h, "echo TMUX-PROVIDER-OK"); err != nil {
		t.Fatalf("send input: %v", err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if mu.has("TMUX-PROVIDER-OK") {
			goto steered
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("did not observe steered output streamed from the pane")
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

// stringSet is a tiny concurrency-safe substring matcher for streamed lines.
type stringSet struct {
	mu    sync.Mutex
	lines []string
}

func (s *stringSet) add(line string) {
	s.mu.Lock()
	s.lines = append(s.lines, line)
	s.mu.Unlock()
}

func (s *stringSet) has(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
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
