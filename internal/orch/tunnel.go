package orch

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// mcpTunnel is a persistent reverse SSH tunnel that exposes the orchestrator's
// HTTP port (API + MCP tool surface) on a remote target's loopback. Agents on
// that target reach their tools at http://127.0.0.1:<port>/mcp/<session> —
// without it, the configured base URL (typically 127.0.0.1:8080) points at the
// wrong machine from over there, and remote sessions silently have no tools.
//
// The tunnel is supervised: if the ssh -N -R process dies (a network blip, a
// dropped sshd connection), a goroutine reopens it on the SAME fixed port. The
// port is constant, so the URLs baked into already-running agents stay valid
// across reopens. This matters most for a long-lived interactive manager, whose
// every poke is a tmux SendInput that never rebuilds its spec — so nothing else
// would ever re-establish the tunnel for it.
type mcpTunnel struct {
	base    string
	port    int
	tgtName string
	// open dials a fresh reverse tunnel on the same port. The supervisor calls it
	// to reopen after a drop; injecting it (rather than the executor) keeps the
	// supervision loop unit-testable with a fake process.
	open func() (exec.Process, error)

	mu   sync.Mutex // guards proc across supervisor reopens and CloseTunnels
	proc exec.Process

	stop     chan struct{} // closed by CloseTunnels to end supervision
	stopOnce sync.Once
	dead     chan struct{} // closed once supervision has fully stopped
}

// alive reports whether the tunnel is still supervised (not torn down). A
// momentarily-dead ssh process counts as alive: the supervisor will reopen it
// on the same port, so callers must not spawn a duplicate.
func (t *mcpTunnel) alive() bool {
	select {
	case <-t.dead:
		return false
	default:
		return true
	}
}

// mcpBaseFor returns the MCP base URL reachable FROM a session's target.
// Local sessions use the configured base directly; SSH targets get a managed
// reverse tunnel, opened on first use and supervised thereafter.
func (o *Orchestrator) mcpBaseFor(tgt *model.Target) string {
	base := o.cfg.ManagerMCPBaseURL
	if base == "" || tgt == nil || tgt.Kind != model.TargetSSH {
		return base
	}
	tunneled, err := o.ensureMCPTunnel(tgt)
	if err != nil {
		// Fall back to the configured base: likely broken from the remote side,
		// but the failure is recorded and the session still starts.
		o.audit("", "", "mcp_tunnel_failed", tgt.Name+": "+err.Error(), nil)
		return base
	}
	return tunneled
}

func (o *Orchestrator) ensureMCPTunnel(tgt *model.Target) (string, error) {
	o.tunnelMu.Lock()
	defer o.tunnelMu.Unlock()
	if t := o.tunnels[tgt.ID]; t != nil && t.alive() {
		return t.base, nil
	}
	sshEx, ok := agent.NewExecutor(tgt).(*exec.SSHExecutor)
	if !ok {
		return o.cfg.ManagerMCPBaseURL, nil
	}
	localAddr, err := hostPortOf(o.cfg.ManagerMCPBaseURL)
	if err != nil {
		return "", err
	}
	port := o.cfg.MCPTunnelPort
	open := func() (exec.Process, error) {
		return sshEx.OpenReverseTunnel(context.Background(), port, localAddr)
	}
	proc, err := open()
	if err != nil {
		return "", err
	}
	t := &mcpTunnel{
		base:    fmt.Sprintf("http://127.0.0.1:%d", port),
		port:    port,
		tgtName: tgt.Name,
		open:    open,
		proc:    proc,
		stop:    make(chan struct{}),
		dead:    make(chan struct{}),
	}
	go o.superviseTunnel(t)
	o.tunnels[tgt.ID] = t
	o.audit("", "", "mcp_tunnel_opened",
		fmt.Sprintf("%s: remote 127.0.0.1:%d -> %s", tgt.Name, port, localAddr), nil)
	return t.base, nil
	// Note: if a previous orcha process left an orphaned tunnel holding the
	// remote port, this open dies (ExitOnForwardFailure) — but the orphan still
	// forwards to the same local address, so agents keep working through it, and
	// the supervisor's reopen attempts defer to it until it is gone.
}

// superviseTunnel keeps a tunnel up for its whole lifetime. When the ssh process
// exits unexpectedly it reopens on the SAME port (so baked agent URLs stay
// valid), backing off on repeated failure until the port frees or the network
// returns. It exits only when CloseTunnels signals stop.
func (o *Orchestrator) superviseTunnel(t *mcpTunnel) {
	defer close(t.dead)
	backoff := time.Second
	for {
		t.mu.Lock()
		proc := t.proc
		t.mu.Unlock()

		// Wait for the process to exit, but stay interruptible by shutdown: a
		// reopen may have just swapped in a fresh proc, and CloseTunnels closes
		// stop before killing — without this select the supervisor could block in
		// Wait on a proc CloseTunnels never sees, and never reap.
		waited := make(chan struct{})
		go func() { _ = proc.Wait(); close(waited) }()
		select {
		case <-t.stop:
			_ = proc.Kill()
			return
		case <-waited:
		}

		// Did the process exit because we are shutting down, or did it drop?
		select {
		case <-t.stop:
			return
		default:
		}

		o.audit("", "", "mcp_tunnel_died",
			fmt.Sprintf("%s: reverse tunnel on 127.0.0.1:%d dropped; reopening", t.tgtName, t.port), nil)

		// Reopen on the same port, attempting immediately (a clean drop frees the
		// port right away). Back off only between FAILED retries: the remote sshd
		// may still hold the listener (ExitOnForwardFailure makes a held port
		// fatal), or the network may be briefly gone.
		for {
			next, err := t.open()
			if err == nil {
				t.mu.Lock()
				t.proc = next
				t.mu.Unlock()
				o.audit("", "", "mcp_tunnel_reopened",
					fmt.Sprintf("%s: remote 127.0.0.1:%d reopened", t.tgtName, t.port), nil)
				backoff = time.Second
				break
			}
			select {
			case <-t.stop:
				return
			case <-time.After(backoff):
			}
			backoff = minDur(backoff*2, 30*time.Second)
		}
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// CloseTunnels tears down all reverse tunnels. Call on shutdown — ssh -N
// children do not exit just because the orchestrator did.
func (o *Orchestrator) CloseTunnels() {
	o.tunnelMu.Lock()
	defer o.tunnelMu.Unlock()
	for id, t := range o.tunnels {
		t.stopOnce.Do(func() { close(t.stop) }) // tell the supervisor not to reopen
		t.mu.Lock()
		_ = t.proc.Kill()
		t.mu.Unlock()
		delete(o.tunnels, id)
	}
}

// hostPortOf extracts "host:port" from a URL, defaulting the port by scheme.
func hostPortOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("orch: no host in MCP base URL %q", rawURL)
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	if u.Scheme == "https" {
		return u.Host + ":443", nil
	}
	return u.Host + ":80", nil
}
