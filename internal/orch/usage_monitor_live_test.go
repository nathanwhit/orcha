package orch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/model"
)

// TestSyncUsage_Live drives the real claude and codex CLIs through tmux and
// asserts the monitor records a plausible weekly percentage for each. It needs
// real authenticated CLIs + tmux on the host, so it is gated behind
// ORCHA_LIVE_USAGE=1 and skipped by default / in CI.
//
//	ORCHA_LIVE_USAGE=1 go test ./internal/orch/ -run TestSyncUsage_Live -v -timeout 120s
func TestSyncUsage_Live(t *testing.T) {
	if os.Getenv("ORCHA_LIVE_USAGE") != "1" {
		t.Skip("set ORCHA_LIVE_USAGE=1 to run the live usage probe")
	}
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.RegisterProvider(agent.NewFake(model.AgentCodex, false, nil))
	o.SetUsageBin(model.AgentClaude, "claude")
	o.SetUsageBin(model.AgentCodex, "codex")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	o.SyncUsage(ctx)

	buckets, err := st.ListUsage()
	if err != nil {
		t.Fatalf("ListUsage: %v", err)
	}
	seen := map[string]bool{}
	for _, b := range buckets {
		seen[b.Provider] = true
		if b.UsedPercent == nil {
			t.Errorf("%s: used_percent is nil (probe did not record a percentage)", b.Provider)
			continue
		}
		if *b.UsedPercent < 0 || *b.UsedPercent > 100 {
			t.Errorf("%s: used_percent %v out of range", b.Provider, *b.UsedPercent)
		}
		t.Logf("%s weekly used = %.1f%% (state=%s)", b.Provider, *b.UsedPercent, b.State)
	}
	for _, p := range []string{string(model.AgentClaude), string(model.AgentCodex)} {
		if !seen[p] {
			t.Errorf("no usage bucket recorded for %s", p)
		}
	}
}
