package orch

import (
	"context"
	"fmt"
	"net/url"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// mcpTunnel is a persistent reverse SSH tunnel that exposes the orchestrator's
// HTTP port (API + MCP tool surface) on a remote target's loopback. Agents on
// that target reach their tools at http://127.0.0.1:<port>/mcp/<session> —
// without it, the configured base URL (typically 127.0.0.1:8080) points at the
// wrong machine from over there, and remote sessions silently have no tools.
type mcpTunnel struct {
	proc exec.Process
	base string
	dead chan struct{}
}

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
// reverse tunnel, opened on first use and reopened if it died.
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
	proc, err := sshEx.OpenReverseTunnel(context.Background(), port, localAddr)
	if err != nil {
		return "", err
	}
	t := &mcpTunnel{
		proc: proc,
		base: fmt.Sprintf("http://127.0.0.1:%d", port),
		dead: make(chan struct{}),
	}
	go func() {
		_ = proc.Wait()
		close(t.dead)
	}()
	o.tunnels[tgt.ID] = t
	o.audit("", "", "mcp_tunnel_opened",
		fmt.Sprintf("%s: remote 127.0.0.1:%d -> %s", tgt.Name, port, localAddr), nil)
	return t.base, nil
	// Note: if a previous orcha process left an orphaned tunnel holding the
	// remote port, this open dies (ExitOnForwardFailure) — but the orphan still
	// forwards to the same local address, so agents keep working through it,
	// and the next ensure retries.
}

// CloseTunnels tears down all reverse tunnels. Call on shutdown — ssh -N
// children do not exit just because the orchestrator did.
func (o *Orchestrator) CloseTunnels() {
	o.tunnelMu.Lock()
	defer o.tunnelMu.Unlock()
	for id, t := range o.tunnels {
		_ = t.proc.Kill()
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
