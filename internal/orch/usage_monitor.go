package orch

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/tmux"
)

// The usage monitor reads each provider's real subscription usage the same way
// a human does: it drives the provider's own interactive CLI inside a real tmux
// pty, types the usage slash command (`/usage` for claude, `/status` for
// codex), screen-scrapes the rendered panel, and records the weekly usage
// percentage. That percentage is what defaultAgent() balances on and what
// SelectProvider treats as exhausted — so without this loop both run blind.
//
// A pty is required: these are TUI slash commands with no scriptable/JSON
// equivalent (and claude's `-p "/usage"` print-mode shim is going away), so the
// only durable source is the rendered terminal. tmux.Controller runs over the
// same exec.Executor as agents, so a probe works on the local host or a remote
// SSH target.

// usageProbe describes how to read one provider's usage panel from its CLI.
type usageProbe struct {
	kind    model.AgentKind
	launch  []string                          // argv that starts the interactive TUI
	command string                            // the usage slash command to type
	ready   string                            // pane substring proving the TUI finished booting
	parse   func(pane string) (float64, bool) // pane text -> weekly used_percent
}

// SetUsageBin tells the monitor which binary to launch for a provider's usage
// probe (matching the CLI binary the agent provider uses). Defaults to the
// provider kind name ("claude"/"codex") when unset.
func (o *Orchestrator) SetUsageBin(kind model.AgentKind, bin string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.usageBins == nil {
		o.usageBins = map[model.AgentKind]string{}
	}
	o.usageBins[kind] = bin
}

// SyncUsage refreshes the usage picture for every registered provider that has
// a probe. It is the loop body the host's usage-monitor ticker calls. Each
// probe is best-effort: a failure to read one provider's panel is audited and
// skipped, leaving its last-known usage in place rather than blanking it.
func (o *Orchestrator) SyncUsage(ctx context.Context) {
	probes := o.usageProbes()
	if len(probes) == 0 {
		return
	}
	ex, dir := o.usageExecutor()
	for _, p := range probes {
		pct, ok := o.probeUsage(ctx, ex, dir, p)
		if !ok {
			o.audit("", "", "usage_probe_failed",
				string(p.kind)+": could not read usage panel", nil)
			continue
		}
		if err := o.st.SetUsageWindow(string(p.kind), "", pct, usageStateFor(pct)); err != nil {
			continue
		}
		o.audit("", "", "usage_synced",
			fmt.Sprintf("%s weekly usage %.0f%%", p.kind, pct),
			model.JSONMap{"provider": string(p.kind), "used_percent": pct})
	}
}

// usageProbes builds the standard probes for the providers that are actually
// registered, using the configured (or default) binary for each.
func (o *Orchestrator) usageProbes() []usageProbe {
	o.mu.Lock()
	bins := map[model.AgentKind]string{}
	for k, v := range o.usageBins {
		bins[k] = v
	}
	_, hasClaude := o.providers[model.AgentClaude]
	_, hasCodex := o.providers[model.AgentCodex]
	o.mu.Unlock()

	var out []usageProbe
	if hasClaude {
		out = append(out, claudeUsageProbe(binOr(bins[model.AgentClaude], "claude")))
	}
	if hasCodex {
		out = append(out, codexUsageProbe(binOr(bins[model.AgentCodex], "codex")))
	}
	return out
}

// usageExecutor picks where to run a probe. Usage limits are account-wide, so
// any host with the CLI authenticated reports the same weekly percentage; we
// prefer a schedulable target (where the agents themselves run, and so where
// the CLI is logged in) and fall back to the local host.
func (o *Orchestrator) usageExecutor() (exec.Executor, string) {
	if tgts, err := o.st.ListTargets(); err == nil {
		for _, t := range tgts {
			if t.Status == model.TargetOnline {
				return agent.NewExecutor(t), t.WorkRoot
			}
		}
	}
	return agent.NewExecutor(nil), ""
}

// probeUsage drives one provider's CLI in a throwaway tmux session: launch the
// TUI, wait for it to boot, type the usage command, then poll the pane until
// the panel parses. Some TUIs (codex) open a slash-command autocomplete that
// swallows the first Enter, so a second Enter is sent partway through.
func (o *Orchestrator) probeUsage(ctx context.Context, ex exec.Executor, dir string, p usageProbe) (float64, bool) {
	ctrl := tmux.New(ex)
	name := "orcha-usage-" + string(p.kind)
	// Always tear the probe session down, even if ctx is already cancelled.
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = ctrl.KillSession(stop, name)
		cancel()
	}()

	if err := ctrl.NewSession(ctx, name, dir, p.launch); err != nil {
		return 0, false
	}
	if !waitForPane(ctx, ctrl, name, func(pane string) bool {
		return strings.Contains(pane, p.ready)
	}, 30) {
		return 0, false
	}
	// The pane can show its ready marker a beat before the TUI is actually
	// accepting input, so an immediate paste is silently dropped. Settle first.
	if !sleepCtx(ctx, 1500*time.Millisecond) {
		return 0, false
	}
	if err := ctrl.SendKeys(ctx, name, p.command); err != nil {
		return 0, false
	}
	for i := 0; i < 24; i++ {
		if pane, err := ctrl.CapturePane(ctx, name); err == nil {
			if pct, ok := p.parse(pane); ok {
				return pct, true
			}
		}
		if i == 6 {
			// No panel yet: the first keystrokes were likely dropped before the
			// input handler attached. Re-send the command (paste + Enter).
			_ = ctrl.SendKeys(ctx, name, p.command)
		}
		if !sleepCtx(ctx, 500*time.Millisecond) {
			return 0, false
		}
	}
	return 0, false
}

// waitForPane polls a pane until cond holds or the attempt budget runs out.
func waitForPane(ctx context.Context, ctrl *tmux.Controller, name string, cond func(string) bool, tries int) bool {
	for i := 0; i < tries; i++ {
		if pane, err := ctrl.CapturePane(ctx, name); err == nil && cond(pane) {
			return true
		}
		if !sleepCtx(ctx, 500*time.Millisecond) {
			return false
		}
	}
	return false
}

// sleepCtx sleeps for d unless ctx is cancelled first; it reports whether the
// full duration elapsed (true) versus an early cancel (false).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func binOr(bin, fallback string) string {
	if strings.TrimSpace(bin) == "" {
		return fallback
	}
	return bin
}

// usageStateFor maps a weekly used-percent to a scheduling state. Exhausted is
// reserved for genuinely spent windows so SelectProvider stops routing to it;
// the constrained band is informational headroom.
func usageStateFor(usedPercent float64) model.UsageState {
	switch {
	case usedPercent >= 99.5:
		return model.UsageExhausted
	case usedPercent >= 90:
		return model.UsageConstrained
	default:
		return model.UsageOK
	}
}

// ---------------------------------------------------------------------------
// Provider-specific probes + parsers
// ---------------------------------------------------------------------------

// claudeUsageProbe reads the "Current week (all models)" figure from claude's
// /usage panel, which renders e.g.:
//
//	Current week (all models)
//	██████████████          28% used
//	Resets Jun 21 at 6:59pm (America/Los_Angeles)
func claudeUsageProbe(bin string) usageProbe {
	return usageProbe{
		kind:    model.AgentClaude,
		launch:  []string{bin},
		command: "/usage",
		ready:   "? for shortcuts",
		parse:   parseClaudeUsage,
	}
}

var pctUsedRe = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*%\s*used`)

func parseClaudeUsage(pane string) (float64, bool) {
	lines := strings.Split(pane, "\n")
	for i, l := range lines {
		if !strings.Contains(l, "Current week (all models)") {
			continue
		}
		// The percent sits on this line or one of the next couple of rows.
		for j := i; j < len(lines) && j < i+3; j++ {
			if m := pctUsedRe.FindStringSubmatch(lines[j]); m != nil {
				if v, err := strconv.ParseFloat(m[1], 64); err == nil {
					return v, true
				}
			}
		}
	}
	return 0, false
}

// codexUsageProbe reads the first "Weekly limit" figure from codex's /status
// panel, which renders the limit as percent *remaining*, e.g.:
//
//	5h limit:        [██████████████████░░] 90% left (resets 19:18)
//	Weekly limit:    [████████████████████] 98% left (resets 14:18 on 24 Jun)
//
// so weekly used = 100 - left. The first "Weekly limit" line is the primary
// model's; later ones (e.g. a Spark sub-limit) are ignored.
func codexUsageProbe(bin string) usageProbe {
	return usageProbe{
		kind:    model.AgentCodex,
		launch:  []string{bin},
		command: "/status",
		ready:   "Context",
		parse:   parseCodexUsage,
	}
}

var pctLeftRe = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*%\s*left`)

func parseCodexUsage(pane string) (float64, bool) {
	for _, l := range strings.Split(pane, "\n") {
		if !strings.Contains(l, "Weekly limit") {
			continue
		}
		if m := pctLeftRe.FindStringSubmatch(l); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				used := 100 - v
				if used < 0 {
					used = 0
				}
				return used, true
			}
		}
	}
	return 0, false
}
