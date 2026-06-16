package exec

import (
	"context"
	"io"
	"os"
	osexec "os/exec"
	"sync"
	"syscall"
)

// LocalExecutor runs commands as child processes of this host. Each process is
// placed in its own process group so Kill can terminate the whole tree (the
// agent plus anything it spawns) rather than leaking orphans.
type LocalExecutor struct{}

// NewLocal returns a LocalExecutor.
func NewLocal() *LocalExecutor { return &LocalExecutor{} }

// Describe implements Executor.
func (l *LocalExecutor) Describe() string { return "local" }

// Bootstrap is a no-op locally.
func (l *LocalExecutor) Bootstrap(context.Context) error { return nil }

// HealthCheck runs `true` to confirm the host can launch processes.
func (l *LocalExecutor) HealthCheck(ctx context.Context) error {
	p, err := l.Start(ctx, Command{Name: "true"})
	if err != nil {
		return err
	}
	// Drain to avoid a blocked pipe, then wait.
	go io.Copy(io.Discard, p.Stdout()) //nolint:errcheck
	go io.Copy(io.Discard, p.Stderr()) //nolint:errcheck
	return p.Wait()
}

// CleanCapture runs cmd to completion and returns its stdout, separated from
// stderr (folded into the error on failure). Implements Capturer.
func (l *LocalExecutor) CleanCapture(ctx context.Context, cmd Command) (string, error) {
	proc, err := l.Start(ctx, cmd)
	if err != nil {
		return "", err
	}
	return captureStreams(proc, cmd.Name)
}

// Start launches a local process in its own process group.
func (l *LocalExecutor) Start(ctx context.Context, cmd Command) (Process, error) {
	c := osexec.Command(cmd.Name, cmd.Args...)
	c.Dir = cmd.Dir
	if len(cmd.Env) > 0 {
		c.Env = append(os.Environ(), cmd.Env...)
	}
	// New process group: the child becomes its own group leader so we can signal
	// the whole group with kill(-pgid).
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return nil, err
	}
	var stdin io.WriteCloser
	if cmd.Stdin != "" {
		stdin, err = c.StdinPipe()
		if err != nil {
			return nil, err
		}
	} else {
		// Keep an interactive stdin pipe available for steering even when no
		// initial input is supplied.
		stdin, err = c.StdinPipe()
		if err != nil {
			return nil, err
		}
	}

	if err := c.Start(); err != nil {
		return nil, err
	}

	p := &localProcess{cmd: c, stdout: stdout, stderr: stderr, stdin: stdin, pgid: c.Process.Pid}

	if cmd.Stdin != "" {
		go func() {
			_, _ = io.WriteString(stdin, cmd.Stdin)
			// Command.Stdin is the complete input: close the pipe so a read-to-EOF
			// command (tee, cat, git apply) sees EOF and exits. Only the
			// no-initial-stdin branch leaves the pipe open, for steering via Stdin().
			_ = stdin.Close()
		}()
	}

	// Kill the process group if the context is canceled.
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Kill()
		case <-p.done():
		}
	}()

	return p, nil
}

type localProcess struct {
	cmd    *osexec.Cmd
	stdout io.Reader
	stderr io.Reader
	stdin  io.WriteCloser
	pgid   int

	waitOnce sync.Once
	waitErr  error
	finished chan struct{}
	initOnce sync.Once
}

func (p *localProcess) ensureDone() chan struct{} {
	p.initOnce.Do(func() { p.finished = make(chan struct{}) })
	return p.finished
}

func (p *localProcess) done() <-chan struct{} { return p.ensureDone() }

func (p *localProcess) Stdout() io.Reader     { return p.stdout }
func (p *localProcess) Stderr() io.Reader     { return p.stderr }
func (p *localProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *localProcess) Pid() int              { return p.cmd.Process.Pid }

func (p *localProcess) Wait() error {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		if err != nil {
			if ee, ok := err.(*osexec.ExitError); ok {
				p.waitErr = &ExitError{Code: ee.ExitCode(), Err: ee}
			} else {
				p.waitErr = err
			}
		}
		close(p.ensureDone())
	})
	<-p.ensureDone()
	return p.waitErr
}

// Kill terminates the process group. Sending to -pgid signals every process in
// the group, not just the leader.
func (p *localProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	// Negative pid targets the whole group. Ignore ESRCH (already gone).
	err := syscall.Kill(-p.pgid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}
