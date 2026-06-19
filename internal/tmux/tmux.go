// Package tmux drives real, attachable tmux sessions through an exec.Executor,
// so it works the same on a local host or a remote SSH target. Each orchestrator
// session can run as an interactive shell/TUI inside a tmux session that a human
// can `tmux attach -t <name>` to and watch or take over live. Steering is
// `send-keys`; output is streamed with `pipe-pane`; cancellation is
// `kill-session`.
package tmux

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/nathanwhit/orcha/internal/exec"
)

// Controller issues tmux commands via an executor.
type Controller struct {
	ex   exec.Executor
	bin  string
	cols int
	rows int
}

// New returns a Controller. bin defaults to "tmux".
func New(ex exec.Executor) *Controller {
	return &Controller{ex: ex, bin: "tmux", cols: 200, rows: 50}
}

// WithBinary overrides the tmux executable.
func (c *Controller) WithBinary(bin string) *Controller {
	if bin != "" {
		c.bin = bin
	}
	return c
}

// WithSize sets the virtual terminal size of new sessions.
func (c *Controller) WithSize(cols, rows int) *Controller {
	if cols > 0 {
		c.cols = cols
	}
	if rows > 0 {
		c.rows = rows
	}
	return c
}

func (c *Controller) run(ctx context.Context, args ...string) (string, error) {
	out, err := exec.RunCapture(ctx, c.ex, exec.Command{Name: c.bin, Args: args})
	if err != nil {
		return out, fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return out, nil
}

// NewSession creates a detached session named name, starting in dir and running
// command (its argv). An empty command launches the target's default shell — a
// real interactive shell. If a session by that name exists it is replaced.
func (c *Controller) NewSession(ctx context.Context, name, dir string, command []string) error {
	_ = c.KillSession(ctx, name) // ensure a clean slate
	args := []string{"new-session", "-d", "-s", name,
		"-x", itoa(c.cols), "-y", itoa(c.rows)}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	args = append(args, command...) // empty => default shell
	_, err := c.run(ctx, args...)
	return err
}

// HasSession reports whether a session is alive.
func (c *Controller) HasSession(ctx context.Context, name string) (bool, error) {
	_, err := c.run(ctx, "has-session", "-t", name)
	if err != nil {
		// has-session exits non-zero when the session is gone; that is not a
		// transport error, just "false".
		return false, nil
	}
	return true, nil
}

// SendKeys delivers text to the session as a bracketed paste followed by an
// explicit Enter keypress. Bracketed paste (paste-buffer -p) gives the
// receiving program an unambiguous paste boundary, so the Enter that follows
// is a real keystroke — a TUI with paste detection (e.g. the claude CLI)
// submits instead of treating it as a newline glued to the paste. This is a
// protocol-level boundary, not a timing heuristic.
//
// The staging buffer name is unique per call: the tmux server is shared, so a
// fixed name would let concurrent senders paste or delete each other's text.
func (c *Controller) SendKeys(ctx context.Context, name, text string) error {
	buf := fmt.Sprintf("orcha-in-%s-%d", name, bufSeq.Add(1))
	if _, err := c.run(ctx, "set-buffer", "-b", buf, "--", text); err != nil {
		return err
	}
	if _, err := c.run(ctx, "paste-buffer", "-d", "-p", "-b", buf, "-t", name); err != nil {
		return err
	}
	_, err := c.run(ctx, "send-keys", "-t", name, "Enter")
	return err
}

// bufSeq disambiguates concurrent SendKeys staging buffers.
var bufSeq atomic.Int64

// SendRaw sends key names verbatim (e.g. "C-c", "Escape", "Up"), not literal
// text. Use for control sequences.
func (c *Controller) SendRaw(ctx context.Context, name string, keys ...string) error {
	args := append([]string{"send-keys", "-t", name}, keys...)
	_, err := c.run(ctx, args...)
	return err
}

// CapturePane returns the current visible pane text (a screen snapshot). When
// the controller runs over ssh -tt, the local ssh client's "Connection to ...
// closed." notice lands in the captured output (stderr is merged); strip it.
func (c *Controller) CapturePane(ctx context.Context, name string) (string, error) {
	out, err := c.run(ctx, "capture-pane", "-p", "-t", name)
	if err != nil {
		return out, err
	}
	return stripSSHNoise(out), nil
}

// CapturePaneANSI is like CapturePane but preserves the pane's colors and text
// attributes as escape sequences (capture-pane -e), so a terminal emulator in
// the UI can render the screen faithfully. Kept separate from CapturePane:
// callers that parse the pane as plain text (usage scraping, completion-marker
// detection) must not see SGR noise.
func (c *Controller) CapturePaneANSI(ctx context.Context, name string) (string, error) {
	out, err := c.run(ctx, "capture-pane", "-p", "-e", "-t", name)
	if err != nil {
		return out, err
	}
	return stripSSHNoise(out), nil
}

// Size returns the virtual terminal size new sessions are created at. The UI
// sizes its emulator to match so the captured screen renders 1:1.
func (c *Controller) Size() (cols, rows int) { return c.cols, c.rows }

// AttachPTY opens a live, interactive attach to the session over a pty at the
// given size, for the web UI's terminal. It runs `tmux attach` through the same
// executor as every other command, so it lands on the right host (local pty, or
// ssh -tt for a remote target). Requires the executor to support ptys.
func (c *Controller) AttachPTY(ctx context.Context, name string, cols, rows uint16) (exec.PTYProcess, error) {
	starter, ok := c.ex.(exec.PTYStarter)
	if !ok {
		return nil, fmt.Errorf("tmux: executor %T does not support ptys", c.ex)
	}
	// Force TERM: the attach client renders against our pty, which the browser
	// drives with an xterm-compatible emulator (xterm.js). Without this, tmux
	// inherits whatever TERM the orchestrator has — often unset or "dumb" in CI
	// and under systemd — and aborts with "terminal does not support clear".
	return starter.StartPTY(ctx, exec.Command{
		Name: c.bin,
		Args: []string{"attach", "-t", name},
		Env:  []string{"TERM=xterm-256color"},
	}, cols, rows)
}

func stripSSHNoise(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "Connection to ") && strings.HasSuffix(t, " closed.") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// PipePane streams the pane's raw output to a shell command on the target
// (e.g. "cat >> /tmp/log"). Passing an empty command stops piping.
func (c *Controller) PipePane(ctx context.Context, name, shellCmd string) error {
	if shellCmd == "" {
		_, err := c.run(ctx, "pipe-pane", "-t", name)
		return err
	}
	_, err := c.run(ctx, "pipe-pane", "-t", name, "-o", shellCmd)
	return err
}

// KillSession terminates a session and its process group. It is a no-op if the
// session does not exist.
func (c *Controller) KillSession(ctx context.Context, name string) error {
	_, err := c.run(ctx, "kill-session", "-t", name)
	if err != nil {
		return nil // already gone
	}
	return nil
}

// SessionName returns a tmux-safe session name for an orchestrator session id.
// tmux forbids "." and ":" in names.
func SessionName(sessionID string) string {
	r := strings.NewReplacer(".", "-", ":", "-")
	return "orcha-" + r.Replace(sessionID)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
