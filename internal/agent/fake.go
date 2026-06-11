package agent

import (
	"context"
	"sync"

	"github.com/nathanwhit/orcha/internal/model"
)

// FakeProvider is an in-process provider for tests and local dev. It is
// scriptable: each started session runs a Script function that emits events.
// It records cancellations and steering inputs so tests can assert on them.
type FakeProvider struct {
	kind        model.AgentKind
	interactive bool

	// Script is invoked in a goroutine for each session. It should emit events
	// on out and return when finished. ctx is canceled on CancelSession (this
	// models process-group cancellation). inputs delivers steering messages.
	Script func(ctx context.Context, spec Spec, inputs <-chan string, out chan<- Event)

	mu       sync.Mutex
	canceled map[string]bool
	inputs   map[string]chan string
	started  []string
	resumed  []string
}

// NewFake returns a fake provider. If script is nil a default one-shot script
// that emits a greeting and completes successfully is used.
func NewFake(kind model.AgentKind, interactive bool, script func(context.Context, Spec, <-chan string, chan<- Event)) *FakeProvider {
	if script == nil {
		script = defaultScript
	}
	return &FakeProvider{
		kind:        kind,
		interactive: interactive,
		Script:      script,
		canceled:    map[string]bool{},
		inputs:      map[string]chan string{},
	}
}

func defaultScript(ctx context.Context, spec Spec, inputs <-chan string, out chan<- Event) {
	select {
	case out <- Event{Kind: EventText, Source: model.MsgAgent, Content: "working on: " + spec.Goal, Activity: "starting"}:
	case <-ctx.Done():
		return
	}
	select {
	case out <- Event{Kind: EventDone, Success: true, Content: "done"}:
	case <-ctx.Done():
	}
}

type fakeHandle struct {
	id          string
	interactive bool
}

func (h *fakeHandle) ID() string        { return h.id }
func (h *fakeHandle) Interactive() bool { return h.interactive }

// Kind reports the provider kind.
func (f *FakeProvider) Kind() model.AgentKind { return f.kind }

func (f *FakeProvider) start(ctx context.Context, spec Spec, resume bool) (Handle, <-chan Event, error) {
	ctx, cancel := context.WithCancel(ctx)
	in := make(chan string, 8)
	out := make(chan Event, 16)

	f.mu.Lock()
	f.inputs[spec.SessionID] = in
	if resume {
		f.resumed = append(f.resumed, spec.SessionID)
	} else {
		f.started = append(f.started, spec.SessionID)
	}
	f.mu.Unlock()

	// Tie CancelSession to ctx cancellation, modelling process-group kill.
	go func() {
		<-ctx.Done()
	}()

	go func() {
		defer close(out)
		defer cancel()
		f.Script(ctx, spec, in, out)
	}()

	return &fakeHandle{id: "fake-" + spec.SessionID, interactive: f.interactive}, out, nil
}

// StartSession launches a scripted fake session.
func (f *FakeProvider) StartSession(ctx context.Context, spec Spec) (Handle, <-chan Event, error) {
	return f.start(ctx, spec, false)
}

// ResumeSession restarts a fake session from compact context, preserving the
// logical session id.
func (f *FakeProvider) ResumeSession(ctx context.Context, sessionID string, spec Spec) (Handle, <-chan Event, error) {
	spec.SessionID = sessionID
	return f.start(ctx, spec, true)
}

// SendInput delivers a steering message to a running interactive session.
func (f *FakeProvider) SendInput(h Handle, text string) error {
	id := trimFakePrefix(h.ID())
	f.mu.Lock()
	in := f.inputs[id]
	f.mu.Unlock()
	if in == nil {
		return nil
	}
	select {
	case in <- text:
	default:
	}
	return nil
}

// CancelSession records the cancellation. The session id is recovered from the
// handle; tests can query WasCanceled.
func (f *FakeProvider) CancelSession(h Handle) error {
	id := trimFakePrefix(h.ID())
	f.mu.Lock()
	f.canceled[id] = true
	in := f.inputs[id]
	f.mu.Unlock()
	if in != nil {
		// Closing input is a benign signal; scripts may select on ctx instead.
	}
	return nil
}

// WasCanceled reports whether CancelSession was called for a session id.
func (f *FakeProvider) WasCanceled(sessionID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canceled[sessionID]
}

// Resumed returns the session ids that were resumed.
func (f *FakeProvider) Resumed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.resumed...)
}

func trimFakePrefix(id string) string {
	const p = "fake-"
	if len(id) > len(p) && id[:len(p)] == p {
		return id[len(p):]
	}
	return id
}
