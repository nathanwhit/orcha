package orch

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nathanwhit/orcha/internal/exec"
)

// fakeTunnelProc is a stand-in for the ssh -N -R process. Wait() blocks until
// the process is "dropped" (drop) or killed; both are idempotent so the
// supervisor and CloseTunnels can race without panicking on a double close.
type fakeTunnelProc struct {
	done     chan struct{}
	killed   atomic.Bool
	closeOne sync.Once
}

func newFakeTunnelProc() *fakeTunnelProc { return &fakeTunnelProc{done: make(chan struct{})} }

func (p *fakeTunnelProc) Stdout() io.Reader     { return nil }
func (p *fakeTunnelProc) Stderr() io.Reader     { return nil }
func (p *fakeTunnelProc) Stdin() io.WriteCloser { return nil }
func (p *fakeTunnelProc) Pid() int              { return 0 }
func (p *fakeTunnelProc) Wait() error           { <-p.done; return nil }
func (p *fakeTunnelProc) drop()                 { p.closeOne.Do(func() { close(p.done) }) }
func (p *fakeTunnelProc) Kill() error           { p.killed.Store(true); p.drop(); return nil }

// A dropped tunnel must be reopened on the same port without any external
// re-trigger — this is the gap that silently broke a long-lived interactive
// manager (every poke is a tmux SendInput that never rebuilds the spec, so
// nothing else ever re-established the tunnel).
func TestTunnelSupervisor_ReopensOnDrop(t *testing.T) {
	o, _ := newTestOrch(t)

	procs := make(chan *fakeTunnelProc, 8)
	var opens atomic.Int32
	open := func() (exec.Process, error) {
		opens.Add(1)
		p := newFakeTunnelProc()
		procs <- p
		return p, nil
	}

	first := newFakeTunnelProc()
	tun := &mcpTunnel{
		base: "http://127.0.0.1:18080", port: 18080, tgtName: "test",
		open: open, proc: first,
		stop: make(chan struct{}), dead: make(chan struct{}),
	}
	go o.superviseTunnel(tun)

	// Drop the live tunnel; the supervisor must reopen on its own.
	first.drop()
	select {
	case <-procs: // a reopened process appeared
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not reopen the tunnel after a drop")
	}
	if got := opens.Load(); got < 1 {
		t.Fatalf("expected at least 1 reopen after the drop, got %d", got)
	}
	if !tun.alive() {
		t.Fatal("tunnel should still be alive (supervised) after a reopen")
	}

	// Shutdown must stop supervision and not reopen again.
	o.tunnelMu.Lock()
	o.tunnels[tun.tgtName] = tun
	o.tunnelMu.Unlock()
	o.CloseTunnels()
	select {
	case <-tun.dead:
	case <-time.After(3 * time.Second):
		t.Fatal("CloseTunnels did not stop the supervisor")
	}
	if tun.alive() {
		t.Fatal("tunnel should be dead after CloseTunnels")
	}
}
