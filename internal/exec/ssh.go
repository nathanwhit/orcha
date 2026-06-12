package exec

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// SSHConfig describes how to reach a remote target over SSH.
type SSHConfig struct {
	User         string
	Host         string
	Port         int
	IdentityFile string
	// BootstrapCmd is run once (over SSH) to prepare the target, e.g. create the
	// work root or activate an environment.
	BootstrapCmd string
	// Options are extra `-o Key=Value` ssh options.
	Options []string
}

// SSHExecutor runs commands on a remote machine over SSH. It reuses
// LocalExecutor to manage the local ssh client process: killing the local ssh
// process group disconnects the channel, and because the remote command runs
// under a forced pty (-tt), the remote process group receives SIGHUP and
// terminates. This gives real remote process-group cancellation without a
// second round-trip.
type SSHExecutor struct {
	cfg   SSHConfig
	local *LocalExecutor
}

// NewSSH returns an SSHExecutor.
func NewSSH(cfg SSHConfig) *SSHExecutor {
	return &SSHExecutor{cfg: cfg, local: NewLocal()}
}

// Describe implements Executor.
func (s *SSHExecutor) Describe() string {
	return "ssh " + s.target()
}

func (s *SSHExecutor) target() string {
	if s.cfg.User != "" {
		return s.cfg.User + "@" + s.cfg.Host
	}
	return s.cfg.Host
}

// sshArgs builds the ssh invocation prefix (without the remote command).
func (s *SSHExecutor) sshArgs(tty bool) []string {
	args := []string{
		"-o", "BatchMode=yes", // never prompt; fail fast if auth is not set up
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10", // don't hang on an unreachable host
	}
	if tty {
		// Force a pty so the remote process group gets SIGHUP on disconnect.
		args = append(args, "-tt")
	}
	if s.cfg.Port != 0 {
		args = append(args, "-p", itoa(s.cfg.Port))
	}
	if s.cfg.IdentityFile != "" {
		args = append(args, "-i", s.cfg.IdentityFile)
	}
	for _, o := range s.cfg.Options {
		args = append(args, "-o", o)
	}
	args = append(args, s.target())
	return args
}

// remoteCommand renders a Command into a single remote shell command string.
func remoteCommand(cmd Command) string {
	var b strings.Builder
	if cmd.Dir != "" {
		fmt.Fprintf(&b, "cd %s && ", shQuote(cmd.Dir))
	}
	b.WriteString("exec ")
	if len(cmd.Env) > 0 {
		b.WriteString("env ")
		for _, e := range cmd.Env {
			b.WriteString(shQuote(e))
			b.WriteByte(' ')
		}
	}
	b.WriteString(shQuote(cmd.Name))
	for _, a := range cmd.Args {
		b.WriteByte(' ')
		b.WriteString(shQuote(a))
	}
	return b.String()
}

// Start launches a remote command via ssh and returns the local ssh process.
func (s *SSHExecutor) Start(ctx context.Context, cmd Command) (Process, error) {
	remote := remoteCommand(cmd)
	args := append(s.sshArgs(true), remote)
	// The remote command carries Dir/Env/Stdin semantics, so the local ssh
	// command needs only the initial stdin forwarded.
	return s.local.Start(ctx, Command{Name: "ssh", Args: args, Stdin: cmd.Stdin})
}

// CleanCapture runs cmd on the remote host WITHOUT allocating a tty, so its
// stdout is free of interactive-shell noise (e.g. a .bashrc that prints
// warnings) and safe to parse — `gh --json`, git plumbing. stderr is folded into
// the error on failure. Implements Capturer.
//
// The non-tty path is fine here because these are short command-and-wait calls
// that exit on their own; the tty in Start exists only so long-lived interactive
// sessions get SIGHUP on disconnect.
func (s *SSHExecutor) CleanCapture(ctx context.Context, cmd Command) (string, error) {
	remote := remoteCommand(cmd)
	args := append(s.sshArgs(false), remote)
	proc, err := s.local.Start(ctx, Command{Name: "ssh", Args: args, Stdin: cmd.Stdin})
	if err != nil {
		return "", err
	}
	return captureStreams(proc, cmd.Name)
}

// HealthCheck runs `true` on the remote host.
func (s *SSHExecutor) HealthCheck(ctx context.Context) error {
	args := append(s.sshArgs(false), "true")
	p, err := s.local.Start(ctx, Command{Name: "ssh", Args: args})
	if err != nil {
		return err
	}
	go io.Copy(io.Discard, p.Stdout()) //nolint:errcheck
	go io.Copy(io.Discard, p.Stderr()) //nolint:errcheck
	return p.Wait()
}

// Bootstrap runs the configured bootstrap command on the remote host.
func (s *SSHExecutor) Bootstrap(ctx context.Context) error {
	if s.cfg.BootstrapCmd == "" {
		return nil
	}
	args := append(s.sshArgs(false), s.cfg.BootstrapCmd)
	p, err := s.local.Start(ctx, Command{Name: "ssh", Args: args})
	if err != nil {
		return err
	}
	go io.Copy(io.Discard, p.Stdout()) //nolint:errcheck
	go io.Copy(io.Discard, p.Stderr()) //nolint:errcheck
	return p.Wait()
}

// shQuote single-quotes a string for safe use in a remote /bin/sh command.
func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	// Wrap in single quotes, escaping embedded single quotes as '\''.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// itoa is a tiny strconv-free int formatter (matches the style used elsewhere).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// OpenReverseTunnel starts a persistent `ssh -N -R` exposing localAddr (e.g.
// the orchestrator's HTTP port) on the target's loopback at remotePort.
// Killing the returned process closes the tunnel. ExitOnForwardFailure makes a
// failed bind fatal instead of a silent no-op tunnel.
func (s *SSHExecutor) OpenReverseTunnel(ctx context.Context, remotePort int, localAddr string) (Process, error) {
	args := []string{
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-R", fmt.Sprintf("127.0.0.1:%d:%s", remotePort, localAddr),
	}
	args = append(args, s.sshArgs(false)...)
	return s.local.Start(ctx, Command{Name: "ssh", Args: args})
}
