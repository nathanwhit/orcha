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

// SendKeys types text into the session followed by Enter (literal, so special
// characters are not interpreted as key names).
func (c *Controller) SendKeys(ctx context.Context, name, text string) error {
	if _, err := c.run(ctx, "send-keys", "-t", name, "-l", text); err != nil {
		return err
	}
	_, err := c.run(ctx, "send-keys", "-t", name, "Enter")
	return err
}

// SendRaw sends key names verbatim (e.g. "C-c", "Escape", "Up"), not literal
// text. Use for control sequences.
func (c *Controller) SendRaw(ctx context.Context, name string, keys ...string) error {
	args := append([]string{"send-keys", "-t", name}, keys...)
	_, err := c.run(ctx, args...)
	return err
}

// CapturePane returns the current visible pane text (a screen snapshot).
func (c *Controller) CapturePane(ctx context.Context, name string) (string, error) {
	return c.run(ctx, "capture-pane", "-p", "-t", name)
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
