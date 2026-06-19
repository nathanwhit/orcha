package exec

import (
	"context"
	"io"
	"os"
	osexec "os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// PTYProcess is a command attached to a pseudo-terminal: a single bidirectional
// byte stream (the pty master) plus terminal resize. Unlike the pipe-based
// Process, it gives the child a real controlling tty, so line editing, signals
// (Ctrl-C), and full-screen TUIs work — what a live interactive terminal needs.
type PTYProcess interface {
	io.ReadWriteCloser
	// Resize sets the pty window size in character cells.
	Resize(cols, rows uint16) error
	// Wait blocks until the underlying process exits.
	Wait() error
}

// PTYStarter is an optional Executor capability: launch a command attached to a
// pseudo-terminal. Both LocalExecutor and SSHExecutor implement it, so an
// interactive attach lands on the right host transparently.
type PTYStarter interface {
	StartPTY(ctx context.Context, cmd Command, cols, rows uint16) (PTYProcess, error)
}

// StartPTY launches a local process attached to a pty of the given size. pty.Start
// makes the child a session leader with the pty as its controlling terminal, so
// it is its own process-group leader and Kill(-pid) tears down its whole tree.
func (l *LocalExecutor) StartPTY(ctx context.Context, cmd Command, cols, rows uint16) (PTYProcess, error) {
	c := osexec.Command(cmd.Name, cmd.Args...)
	c.Dir = cmd.Dir
	if len(cmd.Env) > 0 {
		c.Env = append(os.Environ(), cmd.Env...)
	}
	f, err := pty.StartWithSize(c, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	p := &ptyProcess{cmd: c, f: f, pid: c.Process.Pid}
	// Tear the process group down if the caller's context is canceled.
	go func() {
		<-ctx.Done()
		_ = p.Close()
	}()
	return p, nil
}

// StartPTY for SSH runs `ssh -tt <host> <remote cmd>` under a local pty: the
// local pty gives the ssh client a controlling tty, and -tt forces a remote pty
// for the command. Reuses the same arg/quoting machinery as Start.
func (s *SSHExecutor) StartPTY(ctx context.Context, cmd Command, cols, rows uint16) (PTYProcess, error) {
	args := append(s.sshArgs(true), remoteCommand(cmd))
	return s.local.StartPTY(ctx, Command{Name: "ssh", Args: args}, cols, rows)
}

type ptyProcess struct {
	cmd *osexec.Cmd
	f   *os.File // pty master
	pid int

	waitOnce sync.Once
	waitErr  error
}

func (p *ptyProcess) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *ptyProcess) Write(b []byte) (int, error) { return p.f.Write(b) }

func (p *ptyProcess) Resize(cols, rows uint16) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: cols, Rows: rows})
}

// Close kills the process group and closes the pty master. Idempotent: a second
// call (e.g. from the context watcher after the client hangs up) is harmless.
func (p *ptyProcess) Close() error {
	if p.cmd.Process != nil {
		if err := syscall.Kill(-p.pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
	}
	return p.f.Close()
}

func (p *ptyProcess) Wait() error {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		if err != nil {
			if ee, ok := err.(*osexec.ExitError); ok {
				p.waitErr = &ExitError{Code: ee.ExitCode(), Err: ee}
			} else {
				p.waitErr = err
			}
		}
	})
	return p.waitErr
}

// ensure the interfaces are satisfied at compile time.
var (
	_ PTYStarter = (*LocalExecutor)(nil)
	_ PTYStarter = (*SSHExecutor)(nil)
	_ PTYProcess = (*ptyProcess)(nil)
)
