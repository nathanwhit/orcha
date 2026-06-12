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
	// ResumeCommand builds the argv used when a logical session must be
	// recreated because its tmux session died (e.g. claude --continue picks the
	// conversation back up in the same checkout). Nil falls back to Command —
	// a cold start.
	ResumeCommand func(spec Spec) []string
	// AcceptDialog reports whether the screen shows a blocking startup dialog
	// that should be accepted with Enter (e.g. claude's "Do you trust the files
	// in this folder?"). The opening prompt is passed as a positional argument,
	// which the TUI queues and submits itself once unblocked — so accepting a
	// dialog can never eat the prompt.
	AcceptDialog func(screen string) bool
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
	return &TmuxProvider{cfg: cfg, sessions: map[string]*tmuxSession{}}
}

// Kind implements Provider.
func (p *TmuxProvider) Kind() model.AgentKind { return p.cfg.Kind }

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

	dir := workDirFor(spec)
	ensureDir(ctx, ex, dir)
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

	// Accept known blocking startup dialogs (e.g. the folder trust prompt a
	// fresh checkout always triggers). The opening prompt rides in argv, so
	// pressing Enter on a dialog cannot eat it — the TUI submits the queued
	// prompt itself once unblocked.
	if p.cfg.AcceptDialog != nil {
		go p.watchDialogs(runCtx, ctrl, name)
	}

	return &tmuxHandle{sessionID: spec.SessionID}, out, nil
}

// watchDialogs polls the pane during startup and presses Enter whenever the
// configured blocking dialog is visible. Bounded: these dialogs are a startup
// phenomenon, so watching stops after a couple of minutes (covering slow
// remote cold starts) — it is a watchdog for a known screen state, not a
// delivery delay.
func (p *TmuxProvider) watchDialogs(ctx context.Context, ctrl *tmux.Controller, name string) {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
		screen, err := ctrl.CapturePane(ctx, name)
		if err != nil {
			continue
		}
		if p.cfg.AcceptDialog(screen) {
			_ = ctrl.SendRaw(ctx, name, "Enter")
		}
	}
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
		dir := workDirFor(spec)
		ensureDir(ctx, ex, dir)
		runCtx, cancel := context.WithCancel(ctx)
		if err := ctrl.NewSession(runCtx, name, dir, p.cfg.ResumeCommand(spec)); err != nil {
			cancel()
			return nil, nil, err
		}
		out := p.arm(runCtx, cancel, ctrl, name, spec,
			"tmux session "+name+" recreated, resuming the prior conversation — attach: "+attachCommand(spec.Target, name))
		if p.cfg.AcceptDialog != nil {
			go p.watchDialogs(runCtx, ctrl, name)
		}
		// The resumed conversation already contains the prompt; don't repass it.
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
	//
	// A long-lived conversational session (a manager) ends only when its tmux
	// session goes away (cancel/shutdown). A one-shot session (a worker) runs an
	// interactive TUI that never exits when its task is done — so we additionally
	// watch the pane: the completion sentinel the agent prints, or (as a safety
	// net against a non-compliant agent) a pane that has gone quiescent, both
	// count as the turn being complete.
	go func() {
		defer close(out)
		success := true
		completed := false // turn finished on its own (sentinel/quiescent), vs canceled/external
		var finalScreen string
		var lastScreen string
		var quietSince time.Time
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
					break wait // TUI exited on its own
				}
				if !spec.OneShot {
					continue
				}
				screen, err := ctrl.CapturePane(context.Background(), name)
				if err != nil {
					continue
				}
				if paneSignalsDone(screen) {
					success, completed, finalScreen = true, true, screen
					break wait
				}
				// Quiescence fallback: a one-shot agent that has stopped redrawing
				// the pane for a sustained window has finished its turn (a working
				// TUI animates a ticking spinner every second).
				if screen == lastScreen {
					if quietSince.IsZero() {
						quietSince = time.Now()
					} else if time.Since(quietSince) >= tmuxIdleComplete {
						success, completed, finalScreen = true, true, screen
						break wait
					}
				} else {
					lastScreen, quietSince = screen, time.Time{}
				}
			}
		}
		// On a real completion, surface the agent's final message into the
		// transcript (the manager's notification summary draws from it) and tear
		// the TUI down so it doesn't linger holding the slot.
		if completed {
			if msg := finalPaneMessage(finalScreen); msg != "" {
				out <- Event{Kind: EventText, Source: model.MsgAgent, Content: msg}
			}
			_ = ctrl.KillSession(context.Background(), name)
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

// tmuxIdleComplete is how long a one-shot agent's pane must stay byte-identical
// before the provider concludes the turn is done without a sentinel. Generous
// on purpose: it is a safety net, not the primary signal, and a working TUI
// never stays static this long (its spinner/elapsed timer ticks every second).
const tmuxIdleComplete = 120 * time.Second

// paneSignalsDone reports whether the captured pane shows the completion
// sentinel near the bottom. It scans only the last handful of non-empty lines
// (the agent's final output sits just above the input box) and requires the
// line to be essentially just the sentinel — so the instruction that asks for
// it (a long prose line, higher up and scrolled off once output grows) is not
// mistaken for the agent actually emitting it.
func paneSignalsDone(screen string) bool {
	lines := strings.Split(screen, "\n")
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < 12; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		seen++
		// Allow a short prefix the TUI may add (e.g. a "● " bullet or box edge)
		// but reject the long prose instruction line that merely mentions it.
		if strings.Contains(t, TurnDoneSentinel) && len(t) <= len(TurnDoneSentinel)+6 {
			return true
		}
	}
	return false
}

// finalPaneMessage extracts a best-effort summary of the agent's final output
// from a captured pane: the trailing content lines, with the input box, the
// bottom chrome, and the completion sentinel stripped. It is heuristic — a TUI
// capture is not structured — but gives the manager real context instead of
// "interactive tmux session ...".
func finalPaneMessage(screen string) string {
	lines := strings.Split(screen, "\n")
	// Drop everything from the input prompt down (the box + footer chrome).
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "❯") || strings.HasPrefix(t, "│ >") || strings.Contains(t, "bypass permissions on") {
			lines = lines[:i]
			break
		}
	}
	var kept []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || strings.Contains(t, TurnDoneSentinel) {
			continue
		}
		kept = append(kept, strings.TrimRight(ln, " "))
	}
	if len(kept) > 25 {
		kept = kept[len(kept)-25:]
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
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
