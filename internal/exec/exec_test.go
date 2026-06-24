package exec

import (
	"bufio"
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readAll drains r to EOF. It deliberately ignores the read error and never
// calls t.Fatalf: it runs inside reader goroutines, and t.Fatalf there calls
// runtime.Goexit, which kills the goroutine without sending its result on the
// result channel — turning a clean failure into a deadlock until the test
// timeout. Callers assert on the returned content instead. Drain a process's
// streams fully BEFORE calling Wait(): per the os/exec StdoutPipe contract,
// Wait closes the pipes, so reading after Wait races against ErrClosed.
func readAll(r io.Reader) string {
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestLocal_StreamsStdoutStderr(t *testing.T) {
	l := NewLocal()
	p, err := l.Start(context.Background(), Command{
		Name: "sh", Args: []string{"-c", "echo out-line; echo err-line >&2"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out := make(chan string, 1)
	errc := make(chan string, 1)
	go func() { out <- readAll(p.Stdout()) }()
	go func() { errc <- readAll(p.Stderr()) }()
	// Drain both streams to EOF before Wait (Wait closes the pipes).
	gotOut, gotErr := <-out, <-errc
	if err := p.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !strings.Contains(gotOut, "out-line") {
		t.Fatalf("stdout=%q", gotOut)
	}
	if !strings.Contains(gotErr, "err-line") {
		t.Fatalf("stderr=%q", gotErr)
	}
}

func TestLocal_NonZeroExit(t *testing.T) {
	l := NewLocal()
	p, _ := l.Start(context.Background(), Command{Name: "sh", Args: []string{"-c", "exit 7"}})
	go io.Copy(io.Discard, p.Stdout())
	go io.Copy(io.Discard, p.Stderr())
	err := p.Wait()
	if ExitCode(err) != 7 {
		t.Fatalf("expected exit code 7, got %v (%d)", err, ExitCode(err))
	}
}

func TestLocal_HealthCheck(t *testing.T) {
	if err := NewLocal().HealthCheck(context.Background()); err != nil {
		t.Fatalf("health check: %v", err)
	}
}

// Killing the process must take down the whole process group, including
// grandchildren the agent spawned — not just the immediate child.
func TestLocal_KillTerminatesProcessGroup(t *testing.T) {
	l := NewLocal()
	// Spawn a long-lived grandchild, print its pid, then block.
	p, err := l.Start(context.Background(), Command{
		Name: "sh", Args: []string{"-c", "sleep 60 & echo $!; wait"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sc := bufio.NewScanner(p.Stdout())
	if !sc.Scan() {
		t.Fatal("expected grandchild pid on stdout")
	}
	childPid, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
	if err != nil {
		t.Fatalf("parse child pid: %v", err)
	}

	if err := p.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = p.Wait()

	// The grandchild sleep must be reaped/killed too. Poll until kill(pid, 0)
	// reports ESRCH (no such process).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPid, 0); err == syscall.ESRCH {
			return // success: the whole group is gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild pid %d survived process-group kill", childPid)
}

func TestLocal_ContextCancelKills(t *testing.T) {
	l := NewLocal()
	ctx, cancel := context.WithCancel(context.Background())
	p, err := l.Start(ctx, Command{Name: "sh", Args: []string{"-c", "sleep 60"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	go io.Copy(io.Discard, p.Stdout())
	go io.Copy(io.Discard, p.Stderr())

	done := make(chan error, 1)
	go func() { done <- p.Wait() }()

	cancel()
	select {
	case <-done:
		// canceled process exits (killed) — success
	case <-time.After(3 * time.Second):
		t.Fatal("context cancellation did not kill the process")
	}
}

func TestLocal_StdinSteering(t *testing.T) {
	l := NewLocal()
	p, err := l.Start(context.Background(), Command{
		Name: "sh", Args: []string{"-c", "read line; echo got:$line"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	outc := make(chan string, 1)
	go func() { outc <- readAll(p.Stdout()) }()
	if _, err := io.WriteString(p.Stdin(), "refactor please\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	_ = p.Stdin().Close()
	// Drain stdout to EOF before Wait (Wait closes the pipe).
	got := <-outc
	if err := p.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !strings.Contains(got, "got:refactor please") {
		t.Fatalf("stdin not delivered, stdout=%q", got)
	}
}

// A command carrying Command.Stdin is a feed-and-wait write: the executor must
// deliver the content AND close the pipe so a read-to-EOF command (cat/tee) sees
// EOF and exits. Before the fix the pipe was left open, so this hung forever —
// which is exactly what wedged a manager whose memory seed tee'd a file over SSH.
func TestLocal_StdinClosedForFeedAndWait(t *testing.T) {
	l := NewLocal()
	done := make(chan struct{})
	var out string
	var err error
	go func() {
		defer close(done)
		// `cat` reads stdin to EOF and echoes it; it only exits if it sees EOF.
		out, err = RunCapture(context.Background(), l, Command{Name: "cat", Stdin: "memory contents\n"})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCapture with Command.Stdin hung — stdin pipe was not closed")
	}
	if err != nil {
		t.Fatalf("RunCapture: %v", err)
	}
	if !strings.Contains(out, "memory contents") {
		t.Fatalf("stdin not delivered, stdout=%q", out)
	}
}

// The deadlock that actually bricked prod: an EMPTY feed-and-wait write. With no
// content, Stdin == "" reads as an interactive session and the pipe is left open,
// so `cat`/`tee` block on stdin forever. CloseStdin opts the empty write into the
// write-and-close path so it still sees EOF and exits.
func TestLocal_EmptyStdinClosedWithCloseStdin(t *testing.T) {
	l := NewLocal()
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, err = RunCapture(context.Background(), l, Command{Name: "cat", Stdin: "", CloseStdin: true})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCapture with empty Stdin + CloseStdin hung — pipe was not closed (the manager-wedge deadlock)")
	}
	if err != nil {
		t.Fatalf("RunCapture: %v", err)
	}
}

func TestRemoteCommandRendering(t *testing.T) {
	got := remoteCommand(Command{
		Dir:  "/home/bot/work/sess",
		Env:  []string{"FOO=bar baz"},
		Name: "claude",
		Args: []string{"-p", "do the thing"},
	})
	want := `cd '/home/bot/work/sess' && exec env 'FOO=bar baz' 'claude' '-p' 'do the thing'`
	if got != want {
		t.Fatalf("remoteCommand:\n got=%s\nwant=%s", got, want)
	}
}

func TestShQuote(t *testing.T) {
	cases := map[string]string{
		"":          "''",
		"plain":     "'plain'",
		"a b":       "'a b'",
		"it's fine": `'it'\''s fine'`,
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q)=%s want %s", in, got, want)
		}
	}
}

// TestSSH_Live runs against a real host when ORCHA_SSH_TEST_HOST is set
// (e.g. ORCHA_SSH_TEST_HOST=localhost with Remote Login enabled). It verifies
// streaming, exit codes, and remote process-group cancellation over SSH.
func TestSSH_Live(t *testing.T) {
	host := os.Getenv("ORCHA_SSH_TEST_HOST")
	if host == "" {
		t.Skip("set ORCHA_SSH_TEST_HOST to run the live SSH test")
	}
	s := NewSSH(SSHConfig{Host: host, User: os.Getenv("ORCHA_SSH_TEST_USER")})
	if err := s.HealthCheck(context.Background()); err != nil {
		t.Fatalf("ssh health check: %v", err)
	}
	p, err := s.Start(context.Background(), Command{Name: "sh", Args: []string{"-c", "echo remote-out; exit 0"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	go io.Copy(io.Discard, p.Stderr())
	out := readAll(p.Stdout())
	if err := p.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !strings.Contains(out, "remote-out") {
		t.Fatalf("remote stdout=%q", out)
	}
}

func TestLocalCleanCapture_SeparatesStreams(t *testing.T) {
	// stdout must come back clean — stderr (e.g. a noisy remote .bashrc) must not
	// leak into it, or gh --json parsing would break.
	out, err := NewLocal().CleanCapture(context.Background(),
		Command{Name: "sh", Args: []string{"-c", "echo OUT; echo NOISE >&2"}})
	if err != nil {
		t.Fatalf("clean capture: %v", err)
	}
	if out != "OUT" {
		t.Fatalf("stdout = %q, want just OUT (stderr leaked)", out)
	}
	// On failure, stderr is folded into the error for diagnosis.
	_, err = NewLocal().CleanCapture(context.Background(),
		Command{Name: "sh", Args: []string{"-c", "echo BOOM >&2; exit 3"}})
	if err == nil || !strings.Contains(err.Error(), "BOOM") {
		t.Fatalf("expected error carrying stderr, got %v", err)
	}
}
