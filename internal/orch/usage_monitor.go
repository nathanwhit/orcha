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

// usageReading is one parsed usage panel: the weekly used-percent and, when the
// panel named one, the instant the window resets (zero otherwise).
type usageReading struct {
	usedPercent float64
	resetAt     time.Time
}

// usageProbe describes how to read one provider's usage panel from its CLI.
type usageProbe struct {
	kind    model.AgentKind
	launch  []string                               // argv that starts the interactive TUI
	command string                                 // the usage slash command to type
	ready   string                                 // pane substring proving the TUI finished booting
	parse   func(pane string) (usageReading, bool) // pane text -> weekly usage reading
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
		r, ok := o.probeUsage(ctx, ex, dir, p)
		if !ok {
			o.audit("", "", "usage_probe_failed",
				string(p.kind)+": could not read usage panel", nil)
			continue
		}
		if err := o.st.SetUsageWindow(string(p.kind), "", r.usedPercent, r.resetAt, usageStateFor(r.usedPercent)); err != nil {
			continue
		}
		data := model.JSONMap{"provider": string(p.kind), "used_percent": r.usedPercent}
		if !r.resetAt.IsZero() {
			data["resets_at"] = r.resetAt.Format(time.RFC3339)
		}
		o.audit("", "", "usage_synced",
			fmt.Sprintf("%s weekly usage %.0f%%", p.kind, r.usedPercent), data)
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
func (o *Orchestrator) probeUsage(ctx context.Context, ex exec.Executor, dir string, p usageProbe) (usageReading, bool) {
	ctrl := tmux.New(ex)
	name := "orcha-usage-" + string(p.kind)
	// Always tear the probe session down, even if ctx is already cancelled.
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = ctrl.KillSession(stop, name)
		cancel()
	}()

	if err := ctrl.NewSession(ctx, name, dir, p.launch, nil); err != nil {
		return usageReading{}, false
	}
	// Wait for the TUI to boot, clearing any blocking startup dialog (folder
	// trust, codex's update nudge) along the way — without this the probe sits
	// behind the prompt until it times out, which silently starves the
	// load-balancer of all usage data.
	if !waitForReady(ctx, ctrl, name, p.ready, 60) {
		return usageReading{}, false
	}
	// The pane can show its ready marker a beat before the TUI is actually
	// accepting input, so an immediate paste is silently dropped. Settle first.
	if !sleepCtx(ctx, 1500*time.Millisecond) {
		return usageReading{}, false
	}
	if err := ctrl.SendKeys(ctx, name, p.command); err != nil {
		return usageReading{}, false
	}
	for i := 0; i < 24; i++ {
		if pane, err := ctrl.CapturePane(ctx, name); err == nil {
			if r, ok := p.parse(pane); ok {
				return r, true
			}
		}
		if i == 6 {
			// No panel yet: the first keystrokes were likely dropped before the
			// input handler attached. Re-send the command (paste + Enter).
			_ = ctrl.SendKeys(ctx, name, p.command)
		}
		if !sleepCtx(ctx, 500*time.Millisecond) {
			return usageReading{}, false
		}
	}
	return usageReading{}, false
}

// waitForReady polls a pane until the ready marker appears, dismissing any known
// blocking startup dialog (shared with the live agent watchdog) it sees first.
func waitForReady(ctx context.Context, ctrl *tmux.Controller, name, ready string, tries int) bool {
	for i := 0; i < tries; i++ {
		if pane, err := ctrl.CapturePane(ctx, name); err == nil {
			if strings.Contains(pane, ready) {
				return true
			}
			if keys, ok := agent.DismissStartupDialog(pane); ok {
				for _, k := range keys {
					_ = ctrl.SendRaw(ctx, name, k)
				}
			}
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

func parseClaudeUsage(pane string) (usageReading, bool) {
	lines := strings.Split(pane, "\n")
	for i, l := range lines {
		if !strings.Contains(l, "Current week (all models)") {
			continue
		}
		// The percent — and a couple rows below it, the "Resets …" line — sit in
		// the block opened by this header.
		for j := i; j < len(lines) && j < i+4; j++ {
			m := pctUsedRe.FindStringSubmatch(lines[j])
			if m == nil {
				continue
			}
			v, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				continue
			}
			r := usageReading{usedPercent: v}
			for k := j; k < len(lines) && k < j+3; k++ {
				if reset, ok := parseClaudeReset(lines[k], time.Now()); ok {
					r.resetAt = reset
					break
				}
			}
			return r, true
		}
	}
	return usageReading{}, false
}

// parseClaudeReset reads claude's weekly reset line into the next future reset
// instant. The day-to-time separator varies — "Jun 21 at 6:59pm" and
// "Jun 22, 2am" both occur — and a named zone ("America/Los_Angeles", "UTC")
// may or may not be present; it is honoured when it loads, else the host's local
// zone is used.
var claudeResetRe = regexp.MustCompile(`Resets\s+([A-Za-z]{3,9})\s+(\d{1,2})(?:\s+at\s+|,\s*)(\d{1,2})(?::(\d{2}))?\s*([AaPp][Mm])(?:\s*\(([^)]+)\))?`)

func parseClaudeReset(line string, now time.Time) (time.Time, bool) {
	m := claudeResetRe.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, false
	}
	mon, ok := parseMonth(m[1])
	if !ok {
		return time.Time{}, false
	}
	day, _ := strconv.Atoi(m[2])
	hour, _ := strconv.Atoi(m[3])
	min := 0
	if m[4] != "" {
		min, _ = strconv.Atoi(m[4])
	}
	hour = to24Hour(hour, m[5])
	loc := time.Local
	if m[6] != "" {
		if l, err := time.LoadLocation(strings.TrimSpace(m[6])); err == nil {
			loc = l
		}
	}
	return nextOccurrence(now, mon, day, hour, min, loc), true
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
		// The welcome banner ("OpenAI Codex (vX.Y.Z)") is the stable proof the TUI
		// finished booting and is accepting input — present once any startup
		// dialog is cleared, across model/footer changes.
		ready: "OpenAI Codex",
		parse: parseCodexUsage,
	}
}

var pctLeftRe = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*%\s*left`)

func parseCodexUsage(pane string) (usageReading, bool) {
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
				r := usageReading{usedPercent: used}
				if reset, ok := parseCodexReset(l, time.Now()); ok {
					r.resetAt = reset
				}
				return r, true
			}
		}
	}
	return usageReading{}, false
}

// parseCodexReset reads the reset clause codex appends to a limit line —
// "(resets 21:18 on 24 Jun)" — into the next future reset instant, interpreted
// in the host's local zone (codex renders local time and names no zone).
var codexResetRe = regexp.MustCompile(`resets\s+(\d{1,2}):(\d{2})\s+on\s+(\d{1,2})\s+([A-Za-z]{3,9})`)

func parseCodexReset(line string, now time.Time) (time.Time, bool) {
	m := codexResetRe.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, false
	}
	hour, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(m[3])
	mon, ok := parseMonth(m[4])
	if !ok {
		return time.Time{}, false
	}
	return nextOccurrence(now, mon, day, hour, min, time.Local), true
}

// to24Hour converts a 12-hour clock reading plus "am"/"pm" to 24-hour.
func to24Hour(hour12 int, meridiem string) int {
	h := hour12 % 12
	if strings.EqualFold(meridiem, "pm") {
		h += 12
	}
	return h
}

// parseMonth maps a (possibly full) English month name to time.Month.
func parseMonth(s string) (time.Month, bool) {
	if len(s) < 3 {
		return 0, false
	}
	for m := time.January; m <= time.December; m++ {
		if strings.EqualFold(m.String()[:3], s[:3]) {
			return m, true
		}
	}
	return 0, false
}

// nextOccurrence resolves a year-less month/day/time (the panels omit the year)
// to its next occurrence at or after now, in loc — rolling to next year when
// that calendar date has already passed this year.
func nextOccurrence(now time.Time, mon time.Month, day, hour, min int, loc *time.Location) time.Time {
	year := now.In(loc).Year()
	t := time.Date(year, mon, day, hour, min, 0, 0, loc)
	if t.Before(now) {
		t = time.Date(year+1, mon, day, hour, min, 0, 0, loc)
	}
	return t
}
