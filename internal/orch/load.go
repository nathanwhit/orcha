package orch

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// loadProbeInterval is how often the driver samples each schedulable target's
// load. Frequent enough to notice a box starting to thrash before the scheduler
// piles more on, cheap enough to be one tiny command per target.
var loadProbeInterval = 20 * time.Second

// loadStaleAfter bounds how long a load sample is trusted. Past this the sample
// is ignored and the target schedules as if no data exists (fail open), so a
// target whose probe goes silent (SSH down, host rebooting) is never permanently
// gated off by a single high reading.
const loadStaleAfter = 90 * time.Second

// probeScript reads the 1-minute load average, core count, available memory, and
// free disk on the work-root filesystem in one shell line:
// "load <l> cores <n> memkb <k> freekb <f>". Missing values collapse to empty
// tokens; the parsers tolerate that and fail open per-metric. The work root is
// single-quoted so an unusual path can't break the script.
func probeScript(workRoot string) string {
	wr := workRoot
	if wr == "" {
		wr = "."
	}
	return `printf 'load %s cores %s memkb %s freekb %s\n' ` +
		`"$(cut -d' ' -f1 /proc/loadavg 2>/dev/null)" ` +
		`"$(nproc 2>/dev/null)" ` +
		`"$(grep -m1 '^MemAvailable' /proc/meminfo 2>/dev/null | tr -dc '0-9')" ` +
		`"$(df -Pk ` + shQuote(wr) + ` 2>/dev/null | awk 'NR==2{print $4}')"`
}

// shQuote single-quotes a shell argument.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// healthProbeEnabled reports whether any scheduler gate needs target health
// samples. Load-aware scheduling needs load; the disk guard needs free disk.
// When neither is on, probing is skipped entirely.
func (o *Orchestrator) healthProbeEnabled() bool {
	return o.cfg.MaxLoadPerCore > 0 || o.cfg.MinFreeDiskMB > 0
}

// ProbeTargetLoads samples every schedulable target's load and free disk and
// records them, so the scheduler can place load- and disk-aware. It is a no-op
// when neither gate is enabled. Probes run concurrently; a failed or unreachable
// probe is skipped (the prior samples age out on their own).
func (o *Orchestrator) ProbeTargetLoads(ctx context.Context) {
	if !o.healthProbeEnabled() {
		return
	}
	targets, err := o.st.ListTargets()
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, t := range targets {
		if !t.Status.CanSchedule() {
			continue
		}
		wg.Add(1)
		go func(t *model.Target) {
			defer wg.Done()
			o.probeTarget(ctx, t)
		}(t)
	}
	wg.Wait()
}

func (o *Orchestrator) probeTarget(ctx context.Context, t *model.Target) {
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.Capture(pctx, agent.NewExecutor(t), exec.Command{Name: "sh", Args: []string{"-c", probeScript(t.WorkRoot)}})
	if err != nil {
		return // unreachable / probe failed: leave the prior samples to age out
	}
	now := o.st.Now()
	if o.cfg.MaxLoadPerCore > 0 {
		if perCore, memMB, ok := parseLoadOutput(out); ok {
			_ = o.st.SetTargetLoad(t.ID, perCore, memMB, now)
		}
	}
	if o.cfg.MinFreeDiskMB > 0 {
		if freeMB, ok := parseFreeDiskMB(out); ok {
			_ = o.st.SetTargetDisk(t.ID, freeMB, now)
			if freeMB < o.cfg.MinFreeDiskMB {
				o.onDiskPressure(ctx, t, freeMB)
			}
		}
	}
}

// parseLoadOutput parses the "load <l> cores <n> memkb <k>" probe line into a
// per-core load and available megabytes. ok is false unless both load and a
// positive core count were found, so a partial/garbled reading fails open.
func parseLoadOutput(out string) (perCore float64, memAvailMB int, ok bool) {
	fields := strings.Fields(out)
	var load float64
	var cores, memKB int
	var haveLoad, haveCores bool
	for i := 0; i+1 < len(fields); i++ {
		switch fields[i] {
		case "load":
			if f, err := strconv.ParseFloat(fields[i+1], 64); err == nil && f >= 0 {
				load, haveLoad = f, true
			}
		case "cores":
			if n, err := strconv.Atoi(fields[i+1]); err == nil && n > 0 {
				cores, haveCores = n, true
			}
		case "memkb":
			if n, err := strconv.Atoi(fields[i+1]); err == nil && n >= 0 {
				memKB = n
			}
		}
	}
	if !haveLoad || !haveCores {
		return 0, 0, false
	}
	return load / float64(cores), memKB / 1024, true
}

// parseFreeDiskMB extracts the "freekb <k>" token (free 1K-blocks on the work
// root, from df) and returns it in MB. ok is false when the token is absent or
// unparseable, so a missing/garbled df fails open (no gate, no false alert).
func parseFreeDiskMB(out string) (int, bool) {
	fields := strings.Fields(out)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "freekb" {
			if n, err := strconv.Atoi(fields[i+1]); err == nil && n >= 0 {
				return n / 1024, true
			}
			return 0, false
		}
	}
	return 0, false
}

// targetLoadPerCore returns a target's recent per-core load, or ok=false when
// there is no fresh sample (missing or older than loadStaleAfter).
func (o *Orchestrator) targetLoadPerCore(t *model.Target, now time.Time) (float64, bool) {
	if t.Metadata == nil {
		return 0, false
	}
	lpc, ok := toFloat(t.Metadata["load_per_core"])
	if !ok {
		return 0, false
	}
	at, ok := t.Metadata["load_probed_at"].(string)
	if !ok {
		return 0, false
	}
	ts, err := time.Parse(time.RFC3339, at)
	if err != nil || now.Sub(ts) > loadStaleAfter {
		return 0, false
	}
	return lpc, true
}

// overloaded reports whether a target should be skipped for new placement
// because its recent per-core load is at or above the configured ceiling. It
// fails open: disabled gate, or no fresh sample, means not overloaded.
func (o *Orchestrator) overloaded(t *model.Target, now time.Time) bool {
	if o.cfg.MaxLoadPerCore <= 0 {
		return false
	}
	lpc, ok := o.targetLoadPerCore(t, now)
	if !ok {
		return false
	}
	return lpc >= o.cfg.MaxLoadPerCore
}

// targetFreeDiskMB returns a target's recent work-root free disk in MB, or
// ok=false when there is no fresh sample (missing or older than loadStaleAfter).
func (o *Orchestrator) targetFreeDiskMB(t *model.Target, now time.Time) (int, bool) {
	if t.Metadata == nil {
		return 0, false
	}
	f, ok := toFloat(t.Metadata["free_disk_mb"])
	if !ok {
		return 0, false
	}
	at, ok := t.Metadata["disk_probed_at"].(string)
	if !ok {
		return 0, false
	}
	ts, err := time.Parse(time.RFC3339, at)
	if err != nil || now.Sub(ts) > loadStaleAfter {
		return 0, false
	}
	return int(f), true
}

// diskPressured reports whether a target should be skipped for new placement
// because its work-root free disk is below the configured floor. It fails open:
// disabled gate, or no fresh sample, means not pressured (a target whose probe
// goes silent is never permanently gated off by one stale low reading).
func (o *Orchestrator) diskPressured(t *model.Target, now time.Time) bool {
	if o.cfg.MinFreeDiskMB <= 0 {
		return false
	}
	free, ok := o.targetFreeDiskMB(t, now)
	if !ok {
		return false
	}
	return free < o.cfg.MinFreeDiskMB
}

// diskAlertCooldown bounds how often one target re-alerts on disk pressure, so a
// box that lingers below the floor doesn't spam the event log / push channel
// every probe interval. Reclaim still runs every pressured probe (see below).
const diskAlertCooldown = 10 * time.Minute

// onDiskPressure fires when a probe finds a target below the free-disk floor. It
// kicks an immediate reclaim every time (idempotent — ReclaimWorkspaces
// self-serializes via TryLock — so the box keeps shedding spent checkouts as PRs
// close), and emits a cooldown-deduped, push-notified alert. This is the safety
// net that turns a checkout leak into graceful degradation instead of a 100% fill
// that wedges the orchestrator's own database (see Config.MinFreeDiskMB).
func (o *Orchestrator) onDiskPressure(ctx context.Context, t *model.Target, freeMB int) {
	go o.ReclaimWorkspaces(ctx)

	now := o.st.Now()
	o.diskAlertMu.Lock()
	last := o.lastDiskAlert[t.ID]
	fresh := last.IsZero() || now.Sub(last) >= diskAlertCooldown
	if fresh {
		o.lastDiskAlert[t.ID] = now
	}
	o.diskAlertMu.Unlock()
	if !fresh {
		return
	}
	o.audit("", "", "disk_pressure",
		fmt.Sprintf("%s low on disk: %d MB free (floor %d MB) — gating new placement and reclaiming checkouts", t.Name, freeMB, o.cfg.MinFreeDiskMB),
		model.JSONMap{"target_id": t.ID, "free_disk_mb": freeMB, "floor_mb": o.cfg.MinFreeDiskMB})
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
