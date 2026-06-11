package agent

import (
	"bufio"
	"context"
	"io"
	"sync"

	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// CommandBuilder turns a session Spec into the command line to run. This is
// where an agent CLI is parameterized (e.g. the interactive
// `claude --input-format stream-json --output-format stream-json` invocation).
// Builders set argv plus Dir/Env only; the initial prompt is delivered over
// stdin by the provider so a session is one durable, steerable process. The
// Dir/Env are interpreted by the chosen executor (local or SSH).
type CommandBuilder func(spec Spec) exec.Command

// LineParser maps one stdout line from the agent process into zero or more
// events. A fresh parser is created per session via NewParser, so it may keep
// per-session state. Stderr lines are always surfaced as EventStderr.
type LineParser interface {
	Parse(line string) []Event
	// Done flushes any final events when the stream ends. exitErr is the
	// process exit error (nil on success).
	Done(exitErr error) []Event
}

// ProcessProvider is a real agent.Provider backed by an OS process launched
// through an Executor (local or SSH). It streams the process's stdout/stderr as
// events, supports steering via stdin, and cancels by killing the process
// group.
type ProcessProvider struct {
	kind        model.AgentKind
	interactive bool
	build       CommandBuilder
	newParser   func() LineParser
	encodeInput func(text string) string
	// executorFor selects an Executor for a session's target. Overridable for
	// tests; defaults to ExecutorForTarget.
	executorFor func(spec Spec) exec.Executor

	mu    sync.Mutex
	procs map[string]exec.Process
}

// ProcessConfig configures a ProcessProvider.
type ProcessConfig struct {
	Kind        model.AgentKind
	Interactive bool
	Build       CommandBuilder
	// NewParser creates a per-session stdout parser. If nil, a raw line parser
	// that emits each stdout line as EventStdout is used.
	NewParser func() LineParser
	// EncodeInput frames a message for the agent's stdin. It is used both for
	// the initial prompt and for live steering, so a single encoding (e.g. a
	// stream-json user message) covers both. If nil, the text is sent verbatim
	// followed by a newline.
	EncodeInput func(text string) string
	// ExecutorFor overrides target executor selection (tests).
	ExecutorFor func(spec Spec) exec.Executor
}

// NewProcessProvider builds a ProcessProvider.
func NewProcessProvider(cfg ProcessConfig) *ProcessProvider {
	newParser := cfg.NewParser
	if newParser == nil {
		newParser = func() LineParser { return rawParser{} }
	}
	execFor := cfg.ExecutorFor
	if execFor == nil {
		execFor = ExecutorForTarget
	}
	encode := cfg.EncodeInput
	if encode == nil {
		encode = func(text string) string { return text + "\n" }
	}
	return &ProcessProvider{
		kind:        cfg.Kind,
		interactive: cfg.Interactive,
		build:       cfg.Build,
		newParser:   newParser,
		encodeInput: encode,
		executorFor: execFor,
		procs:       map[string]exec.Process{},
	}
}

// Kind implements Provider.
func (p *ProcessProvider) Kind() model.AgentKind { return p.kind }

type processHandle struct {
	sessionID   string
	pid         int
	interactive bool
}

func (h *processHandle) ID() string        { return "proc-" + h.sessionID }
func (h *processHandle) Interactive() bool { return h.interactive }

// StartSession launches the agent process and streams its output.
func (p *ProcessProvider) StartSession(ctx context.Context, spec Spec) (Handle, <-chan Event, error) {
	return p.launch(ctx, spec)
}

// ResumeSession relaunches the agent from compact context, preserving the
// logical session id. For a one-shot CLI this is a fresh process carrying the
// prior summary in its prompt/context.
func (p *ProcessProvider) ResumeSession(ctx context.Context, sessionID string, spec Spec) (Handle, <-chan Event, error) {
	spec.SessionID = sessionID
	return p.launch(ctx, spec)
}

func (p *ProcessProvider) launch(ctx context.Context, spec Spec) (Handle, <-chan Event, error) {
	ex := p.executorFor(spec)
	cmd := p.build(spec)
	proc, err := ex.Start(ctx, cmd)
	if err != nil {
		return nil, nil, err
	}

	// A session is one-shot when the provider is non-interactive (e.g. codex
	// exec) OR the specific session is marked noninteractive (e.g. a worker that
	// should do its task and finish). One-shot sessions get stdin closed after
	// the prompt so the agent sees EOF, completes, and exits (-> EventDone).
	// Interactive sessions keep stdin open so they stay steerable.
	oneShot := !p.interactive || spec.Mode == model.ModeNoninteractive

	if spec.Prompt != "" && proc.Stdin() != nil {
		initial := spec.Prompt
		if spec.CompactContext != "" {
			initial = spec.CompactContext + "\n\n" + spec.Prompt
		}
		framed := p.encodeInput(initial)
		go func() {
			_, _ = io.WriteString(proc.Stdin(), framed)
			if oneShot {
				_ = proc.Stdin().Close()
			}
		}()
	} else if oneShot && proc.Stdin() != nil {
		_ = proc.Stdin().Close()
	}

	p.mu.Lock()
	p.procs[spec.SessionID] = proc
	p.mu.Unlock()

	out := make(chan Event, 64)
	parser := p.newParser()

	var wg sync.WaitGroup
	wg.Add(2)
	// stdout -> parsed events
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(proc.Stdout())
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			for _, ev := range parser.Parse(sc.Text()) {
				out <- ev
			}
		}
	}()
	// stderr -> EventStderr
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(proc.Stderr())
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			out <- Event{Kind: EventStderr, Source: model.MsgStderr, Content: sc.Text()}
		}
	}()

	go func() {
		wg.Wait() // pipes fully drained
		exitErr := proc.Wait()
		for _, ev := range parser.Done(exitErr) {
			out <- ev
		}
		p.mu.Lock()
		delete(p.procs, spec.SessionID)
		p.mu.Unlock()
		out <- Event{Kind: EventDone, Success: exitErr == nil, Content: doneMsg(exitErr)}
		close(out)
	}()

	return &processHandle{sessionID: spec.SessionID, pid: proc.Pid(), interactive: p.interactive && !oneShot}, out, nil
}

// SendInput writes a steering line to the process stdin.
func (p *ProcessProvider) SendInput(h Handle, text string) error {
	ph, ok := h.(*processHandle)
	if !ok {
		return nil
	}
	p.mu.Lock()
	proc := p.procs[ph.sessionID]
	p.mu.Unlock()
	if proc == nil || proc.Stdin() == nil {
		return nil
	}
	// Steering an interactive session is just another framed message to the
	// running process's stdin — no cancel/resume needed.
	_, err := io.WriteString(proc.Stdin(), p.encodeInput(text))
	return err
}

// CancelSession kills the process group.
func (p *ProcessProvider) CancelSession(h Handle) error {
	ph, ok := h.(*processHandle)
	if !ok {
		return nil
	}
	p.mu.Lock()
	proc := p.procs[ph.sessionID]
	p.mu.Unlock()
	if proc == nil {
		return nil
	}
	return proc.Kill()
}

func doneMsg(err error) string {
	if err == nil {
		return "process exited 0"
	}
	return "process exited with error: " + err.Error()
}

// rawParser emits each stdout line verbatim as a stdout event.
type rawParser struct{}

func (rawParser) Parse(line string) []Event {
	return []Event{{Kind: EventStdout, Source: model.MsgStdout, Content: line}}
}
func (rawParser) Done(error) []Event { return nil }

// ExecutorForTarget chooses an executor based on a session's target. Local
// targets (or a nil target) run on this host; SSH targets run remotely.
func ExecutorForTarget(spec Spec) exec.Executor {
	return NewExecutor(spec.Target)
}

// NewExecutor builds the executor for a target. A nil or local target runs on
// this host; an SSH target runs remotely.
func NewExecutor(t *model.Target) exec.Executor {
	if t == nil || t.Kind != model.TargetSSH {
		return exec.NewLocal()
	}
	cfg := exec.SSHConfig{User: t.User, Host: t.Host}
	if t.Metadata != nil {
		if v, ok := t.Metadata["ssh_port"].(float64); ok {
			cfg.Port = int(v)
		}
		if v, ok := t.Metadata["identity_file"].(string); ok {
			cfg.IdentityFile = v
		}
		if v, ok := t.Metadata["bootstrap"].(string); ok {
			cfg.BootstrapCmd = v
		}
	}
	return exec.NewSSH(cfg)
}
