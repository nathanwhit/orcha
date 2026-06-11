// Package exec runs commands on a target and streams their output. It provides
// a LocalExecutor (subprocess with its own process group) and an SSHExecutor
// (the same machinery wrapped in ssh, so a process-group kill of the local ssh
// client tears down the remote process group via SIGHUP).
//
// This is the execution substrate the agent providers run on: a session is a
// command launched through an Executor chosen from its target.
package exec

import (
	"context"
	"io"
)

// Command describes a process to run.
type Command struct {
	Dir   string   // working directory (target-local path)
	Env   []string // extra KEY=VALUE entries, appended to the inherited env
	Name  string   // executable
	Args  []string // arguments
	Stdin string   // optional stdin written once then closed
}

// Process is a running command. Callers read Stdout/Stderr concurrently and
// then call Wait. Kill terminates the whole process group.
type Process interface {
	// Stdout/Stderr are line-streamable readers. They reach EOF when the
	// process closes them (typically at exit).
	Stdout() io.Reader
	Stderr() io.Reader
	// Stdin writes to the process's standard input; nil if not piped.
	Stdin() io.WriteCloser
	// Wait blocks until the process exits, returning a non-nil error for a
	// non-zero exit (an *ExitError) or other failure.
	Wait() error
	// Kill terminates the process group (SIGKILL). Idempotent.
	Kill() error
	// Pid is the local process id (the ssh client pid for SSHExecutor).
	Pid() int
}

// Executor launches commands on a target.
type Executor interface {
	// Start launches a command and returns a Process.
	Start(ctx context.Context, cmd Command) (Process, error)
	// HealthCheck verifies the executor can run a trivial command (used for
	// target readiness / bootstrap verification).
	HealthCheck(ctx context.Context) error
	// Bootstrap prepares the target before first use (no-op for local).
	Bootstrap(ctx context.Context) error
	// Describe returns a short human-readable location, e.g. "local" or
	// "ssh bot@host".
	Describe() string
}

// ExitError reports a non-zero process exit.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return "process exited with non-zero status"
}

// ExitCode extracts the exit code from a Wait error, or 0 if the error is nil,
// or -1 if it is some other failure.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*ExitError); ok {
		return ee.Code
	}
	return -1
}
