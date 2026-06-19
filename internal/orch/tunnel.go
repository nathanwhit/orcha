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

// Tunnel supervision timing. Vars (not consts) so tests can shrink them. A
// reopened tunnel must stay up at least tunnelHealthyAfter to count as a real
// reopen; one that dies faster never actually bound — almost always the remote
// port is still held by an orphaned tunnel from a previous process, which
// ExitOnForwardFailure turns into an exit within ~a second. In that case the
// supervisor backs off (up to tunnelMaxBackoff) instead of relaunching every
// second. The old code reset the backoff on every launch (treating ssh having
// *started* as a successful reopen), so a permanently-held port hot-looped at
// ~1/s and buried the event log — ~100k died/reopened events/day was observed,
// which also drowned the orchestrator's own logs.
var (
	tunnelHealthyAfter  = 5 * time.Second
	tunnelReopenBackoff = time.Second
	tunnelMaxBackoff    = 30 * time.Second
)

// superviseTunnel keeps a tunnel up for its whole lifetime. When the ssh process
// exits it reopens on the SAME port (so baked agent URLs stay valid). A drop
// after a healthy run reopens promptly; a tunnel that never bound (the remote
// port is held) is retried with exponential backoff. It exits only when
// CloseTunnels signals stop.
func (o *Orchestrator) superviseTunnel(t *mcpTunnel) {
	defer close(t.dead)
	backoff := tunnelReopenBackoff
	for {
		t.mu.Lock()
		proc := t.proc
		t.mu.Unlock()

		// Time how long this tunnel stays up so a genuine blip (reopen promptly)
		// can be told apart from a never-bound tunnel (back off). Stay interruptible
		// by shutdown: a reopen may have swapped in a fresh proc, and CloseTunnels
		// closes stop before killing — without this select the supervisor could
		// block in Wait on a proc CloseTunnels never sees, and never reap.
		start := time.Now()
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

		if time.Since(start) >= tunnelHealthyAfter {
			// A drop after a healthy run is a genuine blip: reopen at once, fresh
			// backoff.
			backoff = tunnelReopenBackoff
			o.audit("", "", "mcp_tunnel_died",
				fmt.Sprintf("%s: reverse tunnel on 127.0.0.1:%d dropped; reopening", t.tgtName, t.port), nil)
		} else {
			// It died before it was healthy: it never bound (the remote port is held
			// — ExitOnForwardFailure). Pause before retrying so a stuck port can't
			// spin the supervisor (and the event log) at the flap rate.
			o.audit("", "", "mcp_tunnel_died",
				fmt.Sprintf("%s: reverse tunnel on 127.0.0.1:%d did not bind (remote port held?); retrying in %s",
					t.tgtName, t.port, backoff), nil)
			select {
			case <-t.stop:
				return
			case <-time.After(backoff):
			}
			backoff = minDur(backoff*2, tunnelMaxBackoff)
		}

		// Reopen on the same port. open() only launches ssh; whether it actually
		// bound is judged next iteration by how long the process survives.
		next, err := t.open()
		if err != nil {
			select {
			case <-t.stop:
				return
			case <-time.After(backoff):
			}
			backoff = minDur(backoff*2, tunnelMaxBackoff)
			continue
		}
		t.mu.Lock()
		t.proc = next
		t.mu.Unlock()
		o.audit("", "", "mcp_tunnel_reopened",
			fmt.Sprintf("%s: remote 127.0.0.1:%d reopened", t.tgtName, t.port), nil)
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
