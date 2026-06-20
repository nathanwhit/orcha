package orch

import (
	"context"
	"encoding/json"
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

// loadProbeScript reads the 1-minute load average, core count, and available
// memory in one shell line: "load <l> cores <n> memkb <k>". Missing values
// collapse to empty tokens; parseLoadOutput tolerates that and fails open.
const loadProbeScript = `printf 'load %s cores %s memkb %s\n' ` +
	`"$(cut -d' ' -f1 /proc/loadavg 2>/dev/null)" ` +
	`"$(nproc 2>/dev/null)" ` +
	`"$(grep -m1 '^MemAvailable' /proc/meminfo 2>/dev/null | tr -dc '0-9')"`

// ProbeTargetLoads samples every schedulable target's load and records it, so
// the scheduler can place load-aware. It is a no-op when load-aware scheduling
// is disabled (MaxLoadPerCore <= 0). Probes run concurrently; a failed or
// unreachable probe is skipped (the prior sample ages out on its own).
func (o *Orchestrator) ProbeTargetLoads(ctx context.Context) {
	if o.cfg.MaxLoadPerCore <= 0 {
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
			o.probeTargetLoad(ctx, t)
		}(t)
	}
	wg.Wait()
}

func (o *Orchestrator) probeTargetLoad(ctx context.Context, t *model.Target) {
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.Capture(pctx, agent.NewExecutor(t), exec.Command{Name: "sh", Args: []string{"-c", loadProbeScript}})
	if err != nil {
		return // unreachable / probe failed: leave the prior sample to age out
	}
	perCore, memMB, ok := parseLoadOutput(out)
	if !ok {
		return // unparseable (e.g. a non-Linux host with no /proc): fail open
	}
	_ = o.st.SetTargetLoad(t.ID, perCore, memMB, o.st.Now())
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
