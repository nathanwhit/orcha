package agent

import (
	"context"
	"regexp"
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
	// DismissDialogs enables the startup-dialog watchdog: while the TUI boots it
	// clears known blocking prompts (folder trust, codex's update nudge) via
	// DismissStartupDialog. The opening prompt is passed as a positional argument,
	// which the TUI queues and submits itself once unblocked — so dismissing a
	// dialog can never eat the prompt. Leave false for a plain shell, which has no
	// such dialogs and must not receive phantom keystrokes.
	DismissDialogs bool
	// ExecutorFor selects the executor (local/SSH); defaults to ExecutorForTarget.
	ExecutorFor func(spec Spec) exec.Executor
	// CompletionGate, when set, vetoes quiescence-based completion: it is consulted
	// only on the idle-pane fallback (never on the explicit sentinel) and must
	// return true for the turn to be treated as done. The orchestrator wires it to
	// "this session has no open question" so a worker that asked the user and is
	// idle waiting for the answer is not mistaken for finished.
	CompletionGate func(sessionID string) bool
	// MaxIdleWithBgWork bounds how long a one-shot pane that has gone static but
	// still reports live background shells (a build the agent yielded its turn to
	// await) is tolerated before the no-sentinel backstop reaps it. Zero uses
	// defaultMaxIdleWithBgWork. It must exceed the longest real build+wait: the
	// footer's shell count resets the quiescence clock the moment a shell exits,
	// so this only ever fires on a genuinely stuck session (work that finished,
	// left an immortal shell, and never printed the completion sentinel).
	MaxIdleWithBgWork time.Duration
	TmuxBin           string
	Cols, Rows        int
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
	if cfg.MaxIdleWithBgWork <= 0 {
		cfg.MaxIdleWithBgWork = defaultMaxIdleWithBgWork
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

	// Clear known blocking startup dialogs (e.g. the folder trust prompt a fresh
	// checkout always triggers). The opening prompt rides in argv, so dismissing
	// a dialog cannot eat it — the TUI submits the queued prompt itself once
	// unblocked.
	if p.cfg.DismissDialogs {
		go p.watchDialogs(runCtx, ctrl, name)
	}

	return &tmuxHandle{sessionID: spec.SessionID}, out, nil
}

// watchDialogs polls the pane during startup and clears any known blocking
// dialog the moment it appears (see DismissStartupDialog for the catalogue).
// Bounded: these dialogs are a startup phenomenon, so watching stops after a
// couple of minutes (covering slow remote cold starts) — it is a watchdog for a
// known screen state, not a delivery delay.
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
		if keys, ok := DismissStartupDialog(screen); ok {
			for _, k := range keys {
				_ = ctrl.SendRaw(ctx, name, k)
			}
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
		if p.cfg.DismissDialogs {
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
		streamer := newPaneStreamer()
		var lastActivity string
		ticker := time.NewTicker(tmuxPollInterval)
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
				screen, err := ctrl.CapturePane(context.Background(), name)
				if err != nil {
					continue
				}

				// Stream the pane's settled output and current activity into the
				// transcript/live view so a worker's progress is visible without
				// attaching. Best-effort: drop under backpressure so a slow consumer
				// never stalls the watcher (and never blocks completion detection).
				newLines, activity := streamer.next(screen)
				for _, ln := range newLines {
					trySend(out, Event{Kind: EventProgress, Source: model.MsgAgent,
						Content: ln, Metadata: model.JSONMap{"stream": true}})
				}
				if activity != "" && activity != lastActivity {
					lastActivity = activity
					trySend(out, Event{Kind: EventProgress, Source: model.MsgAgent, Activity: activity})
				}

				if !spec.OneShot {
					continue
				}
				if paneSignalsDone(screen) {
					success, completed, finalScreen = true, true, screen
					break wait
				}
				// Quiescence fallback: a one-shot agent that has stopped redrawing
				// the WHOLE pane (spinner/elapsed timer included) for a sustained
				// window has finished its turn. Compared on the full screen on
				// purpose — a long-running build still ticks its timer, so it never
				// looks quiescent and is never mistaken for done.
				if screen == lastScreen {
					if quietSince.IsZero() {
						quietSince = time.Now()
					} else {
						// An agent that yielded its turn to await a background build
						// goes byte-identical too — it stops ticking its spinner while
						// it is not its turn — so a static pane is NOT proof of "done".
						// If the footer still reports live background shells, wait far
						// longer before the no-sentinel backstop fires, so we don't kill
						// in-flight work (the explicit sentinel above still finishes the
						// turn instantly regardless of any background shells).
						idleWindow := tmuxIdleComplete
						if paneShowsLiveBackgroundWork(screen) {
							idleWindow = p.cfg.MaxIdleWithBgWork
						}
						if time.Since(quietSince) >= idleWindow {
							// A quiescent pane means "finished" only if the session isn't
							// blocked waiting on the user. A worker that called ask_user and
							// is idle at the prompt awaiting an answer looks identical to a
							// done one — completing it here would kill it and drop the answer.
							if p.cfg.CompletionGate == nil || p.cfg.CompletionGate(spec.SessionID) {
								success, completed, finalScreen = true, true, screen
								break wait
							}
							quietSince = time.Time{} // gated (awaiting the user): keep waiting
						}
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
var tmuxIdleComplete = 120 * time.Second

// defaultMaxIdleWithBgWork is the fallback for TmuxConfig.MaxIdleWithBgWork: the
// quiescence window applied when the pane still shows live background work (see
// paneShowsLiveBackgroundWork). An agent that kicked off a long build and yielded
// its turn to await it sits byte-identical — the spinner only ticks while it is
// the agent's turn — so at tmuxIdleComplete it looks done and gets reaped
// mid-build (the bug this guards against). While background shells are reported
// we hold off until this far larger window, kept bounded so a leaked/never-exiting
// shell can't pin the session slot forever. It only governs the no-sentinel
// backstop: an agent that prints the completion sentinel is finished immediately
// regardless of background shells — the intended escape hatch for genuinely-done
// work that left a shell (e.g. a dev server) running. Default is generous because
// the footer's shell count resets the clock the instant a build's shell exits, so
// in practice this fires only on a truly stuck session; deployments with builds
// longer than this can raise it via -idle-bg-work-timeout.
const defaultMaxIdleWithBgWork = 4 * time.Hour

// bgWorkRe matches the Claude Code status-bar segment reporting background
// shells still alive: the "2 shells" in a footer like
// "… · PR #35186 · 2 shells · ↓ to manage", or the "2 shells still running" on
// its churn line. The count does not animate, so the pane stays byte-identical.
var bgWorkRe = regexp.MustCompile(`(?i)\b\d+\s+shells?\b`)

// paneShowsLiveBackgroundWork reports whether a (static) captured pane indicates
// the agent still has background shells running. Only the footer region is
// inspected, and the match must sit on a status-bar line (a "·" separator or the
// "still running" churn text) so prose that merely mentions shells cannot trip
// it. A false negative only means a faster reap (back to the old behaviour); a
// false positive only delays the no-sentinel backstop — neither kills the turn,
// which the sentinel always ends regardless.
func paneShowsLiveBackgroundWork(screen string) bool {
	lines := strings.Split(screen, "\n")
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < 12; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		seen++
		if !bgWorkRe.MatchString(t) {
			continue
		}
		if strings.Contains(t, "·") || strings.Contains(t, "still running") || strings.Contains(t, "to manage") {
			return true
		}
	}
	return false
}

// tmuxPollInterval is how often the watcher captures the pane (for streaming,
// done detection, and liveness). A var so tests can speed it up.
var tmuxPollInterval = 2 * time.Second

// trySend delivers a best-effort event without blocking: if the channel buffer
// is full (a slow consumer), the event is dropped rather than stalling the
// caller. Only for progress/stream events, never for terminal ones.
func trySend(out chan Event, ev Event) {
	select {
	case out <- ev:
	default:
	}
}

// paneStreamer turns successive pane snapshots into a low-noise stream of the
// agent's settled output. A line is emitted only once it is "settled" — present
// in two consecutive snapshots (so a half-rendered line is not streamed as a
// fragment) — and only once ever (a rolling memory de-dupes redraws and the
// static lines a TUI keeps on screen). This is what makes streaming readable
// rather than the per-redraw noise that made raw pane streaming useless.
type paneStreamer struct {
	prev   []string        // content lines from the previous snapshot
	recent map[string]bool // lines already emitted (de-dupe)
	order  []string        // FIFO of recent keys, to bound the memory
}

func newPaneStreamer() *paneStreamer {
	return &paneStreamer{recent: map[string]bool{}}
}

// next returns the newly-settled content lines since the last snapshot and the
// current activity line (the bottom-most settled content line).
func (s *paneStreamer) next(screen string) (newLines []string, activity string) {
	cur := paneContentLines(screen)
	prevSet := make(map[string]bool, len(s.prev))
	for _, l := range s.prev {
		prevSet[l] = true
	}
	for _, l := range cur {
		if prevSet[l] && !s.recent[l] { // settled (in both snapshots) and unseen
			newLines = append(newLines, l)
			s.recent[l] = true
			s.order = append(s.order, l)
			if len(s.order) > 240 { // bound memory; old lines may re-emit, acceptably rare
				delete(s.recent, s.order[0])
				s.order = s.order[1:]
			}
		}
	}
	s.prev = cur
	for i := len(cur) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(cur[i]); t != "" {
			activity = t
			break
		}
	}
	return newLines, activity
}

// paneContentLines extracts the agent's actual output from a TUI pane: it strips
// the trailing live region (input box, footer, spinner/elapsed timer) and any
// chrome (separators, the input prompt, footer hints) so what remains is the
// scrollback content — tool calls, messages, command output.
func paneContentLines(screen string) []string {
	raw := strings.Split(screen, "\n")
	// Strip the trailing live block (footer + input box + separators).
	cut := 0
	for i := len(raw) - 1; i >= 0; i-- {
		t := strings.TrimSpace(raw[i])
		if t == "" || isPaneChrome(t) {
			continue
		}
		cut = i + 1
		break
	}
	var out []string
	for _, ln := range raw[:cut] {
		t := strings.TrimSpace(ln)
		if t == "" || isPaneChrome(t) {
			continue
		}
		out = append(out, strings.TrimRight(ln, " "))
	}
	return out
}

// isPaneChrome reports whether a trimmed pane line is interface chrome rather
// than agent output: the input prompt, the footer/hints, the working spinner, or
// a separator rule. Agent message/tool lines (claude's "● ", codex's "• "/"└ ")
// are deliberately NOT matched.
func isPaneChrome(t string) bool {
	if t == "" {
		return true
	}
	// Input prompt / box borders.
	if strings.HasPrefix(t, "❯") || strings.HasPrefix(t, "›") || strings.HasPrefix(t, "│ >") {
		return true
	}
	// Working spinner / elapsed-time glyphs (codex "◦"/"✻", claude shows these too).
	if strings.HasPrefix(t, "◦") || strings.HasPrefix(t, "✻") {
		return true
	}
	// Footer hints and status strings.
	for _, marker := range []string{
		"esc to interrupt", "bypass permissions", "for agents", "default · ~",
		"/ps to view", "to view transcript", "shift+tab", "ctrl+o to",
	} {
		if strings.Contains(t, marker) {
			return true
		}
	}
	// A horizontal rule / separator (mostly box-drawing or dashes).
	rule := true
	for _, r := range t {
		if r != '─' && r != '-' && r != '═' && r != ' ' {
			rule = false
			break
		}
	}
	return rule && len(t) >= 3
}

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
