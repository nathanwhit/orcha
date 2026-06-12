package exec

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// RunCapture runs a command to completion through an Executor and returns its
// combined stdout+stderr. It is for short command-and-check operations (e.g.
// git plumbing during workspace preparation), not for long-lived streaming
// sessions. The same call works on local and SSH executors.
func RunCapture(ctx context.Context, ex Executor, cmd Command) (string, error) {
	proc, err := ex.Start(ctx, cmd)
	if err != nil {
		return "", err
	}
	var mu sync.Mutex
	var buf strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	drain := func(r io.Reader) {
		defer wg.Done()
		b, _ := io.ReadAll(r)
		mu.Lock()
		buf.Write(b)
		mu.Unlock()
	}
	go drain(proc.Stdout())
	go drain(proc.Stderr())
	wg.Wait()
	waitErr := proc.Wait()
	return buf.String(), waitErr
}

// Capturer is an executor that can run a command and return its stdout cleanly —
// separated from stderr and, crucially for SSH, without allocating a remote tty
// (which would fold interactive-shell noise into stdout). Use for parse-sensitive
// command output like `gh --json` or git plumbing.
type Capturer interface {
	CleanCapture(ctx context.Context, cmd Command) (stdout string, err error)
}

// Capture runs cmd on ex and returns clean, trimmed stdout. When ex implements
// Capturer (LocalExecutor, SSHExecutor) it uses the non-tty clean path; otherwise
// it falls back to the combined-stream RunCapture.
func Capture(ctx context.Context, ex Executor, cmd Command) (string, error) {
	if c, ok := ex.(Capturer); ok {
		return c.CleanCapture(ctx, cmd)
	}
	out, err := RunCapture(ctx, ex, cmd)
	return strings.TrimSpace(out), err
}

// captureStreams drains a process's stdout and stderr separately, waits, and
// returns trimmed stdout plus an error that embeds stderr on failure.
func captureStreams(proc Process, label string) (string, error) {
	var so, se strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b, _ := io.ReadAll(proc.Stdout()); so.WriteString(string(b)) }()
	go func() { defer wg.Done(); b, _ := io.ReadAll(proc.Stderr()); se.WriteString(string(b)) }()
	wg.Wait()
	err := proc.Wait()
	out := strings.TrimSpace(so.String())
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", label, err, strings.TrimSpace(se.String()))
	}
	return out, nil
}
