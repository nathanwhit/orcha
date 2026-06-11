package agent

import (
	"context"
	"testing"
	"time"

	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// shProvider builds a ProcessProvider that runs a fixed shell script, for
// exercising the real subprocess machinery without an LLM.
func shProvider(t *testing.T, script string, interactive bool) *ProcessProvider {
	t.Helper()
	return NewProcessProvider(ProcessConfig{
		Kind:        model.AgentOther,
		Interactive: interactive,
		Build: func(spec Spec) exec.Command {
			return exec.Command{Name: "sh", Args: []string{"-c", script}}
		},
	})
}

func collect(t *testing.T, events <-chan Event) []Event {
	t.Helper()
	var out []Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatal("timed out collecting events")
		}
	}
}

func TestProcessProvider_StreamsAndCompletes(t *testing.T) {
	p := shProvider(t, "echo line1; echo line2", false)
	_, events, err := p.StartSession(context.Background(), Spec{SessionID: "s1"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	evs := collect(t, events)

	var stdout int
	var done *Event
	for i := range evs {
		switch evs[i].Kind {
		case EventStdout:
			stdout++
		case EventDone:
			done = &evs[i]
		}
	}
	if stdout != 2 {
		t.Fatalf("expected 2 stdout events, got %d (%+v)", stdout, evs)
	}
	if done == nil || !done.Success {
		t.Fatalf("expected successful done event, got %+v", done)
	}
}

func TestProcessProvider_NonZeroExitFails(t *testing.T) {
	p := shProvider(t, "echo oops >&2; exit 3", false)
	_, events, _ := p.StartSession(context.Background(), Spec{SessionID: "s2"})
	evs := collect(t, events)
	for _, e := range evs {
		if e.Kind == EventDone {
			if e.Success {
				t.Fatal("expected failure done event for non-zero exit")
			}
			return
		}
	}
	t.Fatal("no done event")
}

func TestProcessProvider_InteractiveSteeringViaStdin(t *testing.T) {
	// A persistent process that echoes each stdin line — models an interactive
	// agent you steer by sending messages to the running session.
	p := shProvider(t, "while read l; do echo \"ack:$l\"; done", true)
	h, events, err := p.StartSession(context.Background(), Spec{SessionID: "s3"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !h.Interactive() {
		t.Fatal("provider should report interactive")
	}

	got := make(chan string, 4)
	go func() {
		for ev := range events {
			if ev.Kind == EventStdout {
				got <- ev.Content
			}
		}
	}()

	if err := p.SendInput(h, "refactor"); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case line := <-got:
		if line != "ack:refactor" {
			t.Fatalf("steering not delivered, got %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no steered response")
	}
	_ = p.CancelSession(h)
}

func TestProcessProvider_CancelKillsProcess(t *testing.T) {
	p := shProvider(t, "sleep 60", false)
	h, events, err := p.StartSession(context.Background(), Spec{SessionID: "s4"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan *Event, 1)
	go func() {
		for ev := range events {
			if ev.Kind == EventDone {
				e := ev
				done <- &e
			}
		}
	}()
	if err := p.CancelSession(h); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case d := <-done:
		if d.Success {
			t.Fatal("canceled process should not report success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not terminate the process")
	}
}

func TestExecutorForTarget(t *testing.T) {
	if _, ok := ExecutorForTarget(Spec{}).(*exec.LocalExecutor); !ok {
		t.Fatal("nil target should select local executor")
	}
	local := &model.Target{Kind: model.TargetLocal}
	if _, ok := ExecutorForTarget(Spec{Target: local}).(*exec.LocalExecutor); !ok {
		t.Fatal("local target should select local executor")
	}
	ssh := &model.Target{Kind: model.TargetSSH, Host: "h", User: "u"}
	if _, ok := ExecutorForTarget(Spec{Target: ssh}).(*exec.SSHExecutor); !ok {
		t.Fatal("ssh target should select ssh executor")
	}
}
