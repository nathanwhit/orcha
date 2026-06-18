package orch

import (
	"testing"

	"github.com/nathanwhit/orcha/internal/model"
)

// claudePane and codexPane are real captures from `/usage` and `/status` driven
// through tmux, trimmed to the relevant rows, so the parsers are tested against
// the actual rendered layout (box-drawing, progress bars, alignment) rather
// than an idealized string.
const claudePane = `  Current session
  ██████                                             12% used
  Resets 8:29pm (America/Los_Angeles)
  Current week (all models)
  ██████████████                                     28% used
  Resets Jun 21 at 6:59pm (America/Los_Angeles)
  Current week (Sonnet only)
                                                     0% used`

const codexPane = `  5h limit:                    [██████████████████░░] 90% left (resets 19:18)
  Weekly limit:                [████████████████████] 98% left (resets 14:18 on 24 Jun)
  GPT-5.3-Codex-Spark limit:
  5h limit:                    [████████████████████] 100% left (resets 22:56)
  Weekly limit:                [████████████████████] 100% left (resets 17:56 on 24 Jun)`

func TestParseClaudeUsage(t *testing.T) {
	got, ok := parseClaudeUsage(claudePane)
	if !ok {
		t.Fatal("expected to parse claude usage")
	}
	// The weekly (all models) figure, not the session (12%) or Sonnet (0%) lines.
	if got != 28 {
		t.Fatalf("got %v%% used, want 28", got)
	}
}

func TestParseClaudeUsage_NoPanel(t *testing.T) {
	if _, ok := parseClaudeUsage("just a boot screen\n? for shortcuts"); ok {
		t.Fatal("expected no parse when the usage panel is absent")
	}
}

func TestParseCodexUsage(t *testing.T) {
	got, ok := parseCodexUsage(codexPane)
	if !ok {
		t.Fatal("expected to parse codex usage")
	}
	// First "Weekly limit" is the primary model: 98% left => 2% used. The Spark
	// weekly (100% left) must be ignored.
	if got != 2 {
		t.Fatalf("got %v%% used, want 2", got)
	}
}

func TestParseCodexUsage_NoPanel(t *testing.T) {
	if _, ok := parseCodexUsage("OpenAI Codex\nContext 0% used"); ok {
		t.Fatal("expected no parse when the status panel is absent")
	}
}

func TestUsageStateFor(t *testing.T) {
	cases := []struct {
		pct  float64
		want model.UsageState
	}{
		{2, model.UsageOK},
		{28, model.UsageOK},
		{89.9, model.UsageOK},
		{90, model.UsageConstrained},
		{98, model.UsageConstrained},
		{99.5, model.UsageExhausted},
		{100, model.UsageExhausted},
	}
	for _, c := range cases {
		if got := usageStateFor(c.pct); got != c.want {
			t.Errorf("usageStateFor(%v) = %s, want %s", c.pct, got, c.want)
		}
	}
}

// TestSetUsageWindow_PreservesTokens guards the reason SetUsageWindow exists
// instead of UpsertUsage: a monitor refresh must update the rate-limit picture
// without zeroing the independently-accumulated token counter.
func TestSetUsageWindow_PreservesTokens(t *testing.T) {
	_, st := newTestOrch(t)
	if err := st.AddUsageTokens(string(model.AgentClaude), "", 1234, ""); err != nil {
		t.Fatalf("AddUsageTokens: %v", err)
	}
	if err := st.SetUsageWindow(string(model.AgentClaude), "", 28, model.UsageOK); err != nil {
		t.Fatalf("SetUsageWindow: %v", err)
	}
	buckets, err := st.ListUsage()
	if err != nil {
		t.Fatalf("ListUsage: %v", err)
	}
	var found bool
	for _, b := range buckets {
		if b.Provider != string(model.AgentClaude) {
			continue
		}
		found = true
		if b.UsedTokens != 1234 {
			t.Errorf("used_tokens = %d, want 1234 (must not be reset)", b.UsedTokens)
		}
		if b.UsedPercent == nil || *b.UsedPercent != 28 {
			t.Errorf("used_percent = %v, want 28", b.UsedPercent)
		}
		if b.State != model.UsageOK {
			t.Errorf("state = %s, want ok", b.State)
		}
	}
	if !found {
		t.Fatal("no claude bucket found")
	}
}
