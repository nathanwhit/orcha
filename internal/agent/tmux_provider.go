package agent

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/tmux"
)

// TmuxConfig configures a tmux-backed interactive provider.
type TmuxConfig struct {
	Kind model.AgentKind
	// Command builds the interactive argv run inside the tmux pane. An empty
	// result launches the target's default shell — a real interactive shell.
	Command func(spec Spec) []string
	// SendInitialPrompt types the session prompt into the TUI after StartDelay
	// (give a TUI time to come up before typing).
	SendInitialPrompt bool
	StartDelay        time.Duration
	// ResumeCommand builds the argv used when a logical session must be
	// recreated because its tmux session died (e.g. claude --continue picks the
	// conversation back up in the same checkout). Nil falls back to Command
	// with the opening prompt retyped — a cold start.
	ResumeCommand func(spec Spec) []string
	// ExecutorFor selects the executor (local/SSH); defaults to ExecutorForTarget.
	ExecutorFor func(spec Spec) exec.Executor
	TmuxBin     string
	Cols, Rows  int
}

// TmuxProvider runs each session as an interactive program inside a real,
// attachable tmux session. Steering types into the live pane (send-keys); the
// live view is Snapshot (capture-pane) or `tmux attach -t <name>` (printed as
// a status event); pipe-pane keeps a raw log file on the target. Pane output
// is deliberately NOT streamed into the transcript — a TUI redraws constantly,
// so per-line events are unreadable fragments.
type TmuxProvider struct {
	cfg TmuxConfig

	mu       sync.Mutex
	sessions map[string]*tmuxSession
}

// NewTmux builds a tmux provider.
func NewTmux(cfg TmuxConfig) *TmuxProvider {
	if cfg.ExecutorFor == nil {
		cfg.ExecutorFor = ExecutorForTarget
	}
	if cfg.StartDelay == 0 {
		cfg.StartDelay = 1500 * time.Millisecond
	}
	return &TmuxProvider{cfg: cfg, sessions: map[string]*tmuxSession{}}
}

// Kind implements Provider.
func (p *TmuxProvider) Kind() model.AgentKind { return p.cfg.Kind }

// NewTmuxAgent builds a tmux provider that runs an agent's interactive TUI
// (e.g. ["codex"] or ["claude"]) in an attachable session and types the opening
// prompt into it. binArgs is the argv of the interactive program.
func NewTmuxAgent(kind model.AgentKind, binArgs ...string) *TmuxProvider {
	return NewTmux(TmuxConfig{
		Kind:              kind,
		Command:           func(Spec) []string { return binArgs },
		SendInitialPrompt: true,
	})
}

// NewTmuxShell builds a tmux provider whose sessions are the target's default
// interactive shell — steered entirely via send-keys.
func NewTmuxShell(kind model.AgentKind) *TmuxProvider {
	return NewTmux(TmuxConfig{Kind: kind})
}

type tmuxSession struct {
	name   string
	ctrl   *tmux.Controller
	cancel context.CancelFunc
}

type tmuxHandle struct{ sessionID string }

func (h *tmuxHandle) ID() string        { return "tmux-" + h.sessionID }
func (h *tmuxHandle) Interactive() bool { return true }

// StartSession launches the interactive program in a fresh tmux session.
func (p *TmuxProvider) StartSession(ctx context.Context, spec Spec) (Handle, <-chan Event, error) {
	ex := p.cfg.ExecutorFor(spec)
	ctrl := tmux.New(ex).WithBinary(p.cfg.TmuxBin).WithSize(p.cfg.Cols, p.cfg.Rows)
	name := tmux.SessionName(spec.SessionID)

	dir := ""
	if spec.Workspace != nil {
		dir = spec.Workspace.Path
	}
	var command []string
	if p.cfg.Command != nil {
		command = p.cfg.Command(spec)
	}

	runCtx, cancel := context.WithCancel(ctx)
	if err := ctrl.NewSession(runCtx, name, dir, command); err != nil {
		cancel()
		return nil, nil, err
	}
	out := p.arm(runCtx, cancel, ctrl, name, spec,
		"tmux session "+name+" — attach: "+attachCommand(spec.Target, name))

	// Type the opening prompt once the program has had time to start.
	if p.cfg.SendInitialPrompt && spec.Prompt != "" {
		prompt := spec.Prompt
		go func() {
			select {
			case <-runCtx.Done():
				return
			case <-time.After(p.cfg.StartDelay):
			}
			_ = ctrl.SendKeys(runCtx, name, prompt)
		}()
	}

	return &tmuxHandle{sessionID: spec.SessionID}, out, nil
}

// ResumeSession resumes a logical session, e.g. after an orchestrator restart.
// tmux sessions outlive the orchestrator process, so if the session is still
// alive it is ADOPTED as-is: the running TUI keeps its full conversation and
// nothing is typed into it. Only when the tmux session is gone is a new one
// created — via ResumeCommand (continuing the prior agent conversation) when
// configured, else a cold start.
func (p *TmuxProvider) ResumeSession(ctx context.Context, sessionID string, spec Spec) (Handle, <-chan Event, error) {
	spec.SessionID = sessionID
	ex := p.cfg.ExecutorFor(spec)
	ctrl := tmux.New(ex).WithBinary(p.cfg.TmuxBin).WithSize(p.cfg.Cols, p.cfg.Rows)
	name := tmux.SessionName(sessionID)

	if alive, _ := ctrl.HasSession(ctx, name); alive {
		runCtx, cancel := context.WithCancel(ctx)
		out := p.arm(runCtx, cancel, ctrl, name, spec,
			"re-adopted live tmux session "+name+" — attach: "+attachCommand(spec.Target, name))
		return &tmuxHandle{sessionID: sessionID}, out, nil
	}

	if p.cfg.ResumeCommand != nil {
		dir := ""
		if spec.Workspace != nil {
			dir = spec.Workspace.Path
		}
		runCtx, cancel := context.WithCancel(ctx)
		if err := ctrl.NewSession(runCtx, name, dir, p.cfg.ResumeCommand(spec)); err != nil {
			cancel()
			return nil, nil, err
		}
		out := p.arm(runCtx, cancel, ctrl, name, spec,
			"tmux session "+name+" recreated, resuming the prior conversation — attach: "+attachCommand(spec.Target, name))
		// The resumed conversation already contains the prompt; don't retype it.
		return &tmuxHandle{sessionID: sessionID}, out, nil
	}
	return p.StartSession(ctx, spec)
}

// arm wires up a (new or adopted) live tmux session: registers it, keeps the
// raw pane log piping, announces the attach command, and starts the watcher
// that emits the terminal event when the session ends.
//
// The transcript does NOT get per-line pane output: a TUI redraws constantly,
// so streamed lines are unreadable fragments. The live view is Snapshot / tmux
// attach; the pipe-pane log file is the forensic record.
func (p *TmuxProvider) arm(runCtx context.Context, cancel context.CancelFunc, ctrl *tmux.Controller, name string, spec Spec, announce string) chan Event {
	logPath := "/tmp/orcha-tmux-" + sanitizeID(spec.SessionID) + ".log"
	_ = ctrl.PipePane(runCtx, name, "cat >> "+logPath) // -o: no-op if already piping

	sess := &tmuxSession{name: name, ctrl: ctrl, cancel: cancel}
	p.mu.Lock()
	p.sessions[spec.SessionID] = sess
	p.mu.Unlock()

	out := make(chan Event, 64)

	// Announce the attachable session up front so a human can watch it live.
	out <- Event{
		Kind:     EventStatus,
		Source:   model.MsgSystem,
		Content:  announce,
		Activity: "interactive tmux session " + name,
		Metadata: model.JSONMap{"tmux_session": name, "tmux_attach": attachCommand(spec.Target, name), "tmux_log": logPath},
	}

	// Watch for the session to end, then emit the single terminal event and
	// close the channel.
	go func() {
		defer close(out)
		success := true
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
	wait:
		for {
			select {
			case <-runCtx.Done():
				success = false
				break wait
			case <-ticker.C:
				if alive, _ := ctrl.HasSession(context.Background(), name); !alive {
					break wait
				}
			}
		}
		p.teardown(spec.SessionID)
		msg := "tmux session ended"
		if !success {
			msg = "tmux session canceled"
		}
		out <- Event{Kind: EventDone, Success: success, Content: msg}
	}()

	return out
}

// SendInput types a steering message into the live pane.
func (p *TmuxProvider) SendInput(h Handle, text string) error {
	th, ok := h.(*tmuxHandle)
	if !ok {
		return nil
	}
	p.mu.Lock()
	sess := p.sessions[th.sessionID]
	p.mu.Unlock()
	if sess == nil {
		return nil
	}
	return sess.ctrl.SendKeys(context.Background(), sess.name, text)
}

// CancelSession kills the tmux session and stops streaming.
func (p *TmuxProvider) CancelSession(h Handle) error {
	th, ok := h.(*tmuxHandle)
	if !ok {
		return nil
	}
	p.mu.Lock()
	sess := p.sessions[th.sessionID]
	p.mu.Unlock()
	if sess == nil {
		return nil
	}
	_ = sess.ctrl.KillSession(context.Background(), sess.name)
	sess.cancel()
	return nil
}

// Snapshot returns the current visible pane content for a live session, so the
// UI can render the terminal panel. Implements Snapshotter.
func (p *TmuxProvider) Snapshot(h Handle) (string, error) {
	th, ok := h.(*tmuxHandle)
	if !ok {
		return "", nil
	}
	p.mu.Lock()
	sess := p.sessions[th.sessionID]
	p.mu.Unlock()
	if sess == nil {
		return "", nil
	}
	return sess.ctrl.CapturePane(context.Background(), sess.name)
}

func (p *TmuxProvider) teardown(sessionID string) {
	p.mu.Lock()
	sess := p.sessions[sessionID]
	delete(p.sessions, sessionID)
	p.mu.Unlock()
	if sess != nil {
		sess.cancel()
	}
}

// attachCommand returns the command a human runs to watch/take over the session.
func attachCommand(t *model.Target, name string) string {
	if t != nil && t.Kind == model.TargetSSH {
		host := t.Host
		if t.User != "" {
			host = t.User + "@" + t.Host
		}
		return "ssh -t " + host + " tmux attach -t " + name
	}
	return "tmux attach -t " + name
}

func sanitizeID(s string) string {
	return strings.NewReplacer(".", "-", ":", "-", "/", "-").Replace(s)
}
