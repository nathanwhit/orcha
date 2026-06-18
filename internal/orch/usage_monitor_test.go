package orch

import (
	"testing"
	"time"

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
	if got.usedPercent != 28 {
		t.Fatalf("got %v%% used, want 28", got.usedPercent)
	}
	// The weekly block's own "Resets …" line — not the session reset two rows up.
	if got.resetAt.IsZero() {
		t.Fatal("expected the weekly reset time to be parsed")
	}
	if got.resetAt.Month() != time.June || got.resetAt.Day() != 21 {
		t.Fatalf("reset = %s, want Jun 21", got.resetAt)
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
	if got.usedPercent != 2 {
		t.Fatalf("got %v%% used, want 2", got.usedPercent)
	}
	if got.resetAt.IsZero() || got.resetAt.Month() != time.June || got.resetAt.Day() != 24 {
		t.Fatalf("reset = %s, want Jun 24", got.resetAt)
	}
}

func TestParseCodexUsage_NoPanel(t *testing.T) {
	if _, ok := parseCodexUsage("OpenAI Codex\nContext 0% used"); ok {
		t.Fatal("expected no parse when the status panel is absent")
	}
}

func TestParseResetTime(t *testing.T) {
	// A fixed "now" so year inference is deterministic.
	now := time.Date(2026, time.June, 18, 12, 0, 0, 0, time.UTC)

	cReset, ok := parseClaudeReset("  Resets Jun 21 at 6:59pm (America/Los_Angeles)", now)
	if !ok {
		t.Fatal("claude reset did not parse")
	}
	la, _ := time.LoadLocation("America/Los_Angeles")
	want := time.Date(2026, time.June, 21, 18, 59, 0, 0, la)
	if !cReset.Equal(want) {
		t.Errorf("claude reset = %s, want %s", cReset, want)
	}

	xReset, ok := parseCodexReset("Weekly limit: 98% left (resets 14:18 on 24 Jun)", now)
	if !ok {
		t.Fatal("codex reset did not parse")
	}
	if xReset.Hour() != 14 || xReset.Minute() != 18 || xReset.Day() != 24 || xReset.Month() != time.June {
		t.Errorf("codex reset = %s, want 14:18 on Jun 24", xReset)
	}

	// Claude also renders the comma form on the hour ("Jun 22, 2am (UTC)").
	cComma, ok := parseClaudeReset("  Resets Jun 22, 2am (UTC)", now)
	if !ok {
		t.Fatal("claude comma-form reset did not parse")
	}
	wantComma := time.Date(2026, time.June, 22, 2, 0, 0, 0, time.UTC)
	if !cComma.Equal(wantComma) {
		t.Errorf("claude comma reset = %s, want %s", cComma, wantComma)
	}

	// Year rollover: a date already past this year resolves to next year.
	roll, ok := parseClaudeReset("Resets Jan 2 at 9am", now)
	if !ok || roll.Year() != 2027 {
		t.Errorf("rollover reset = %v (ok=%v), want year 2027", roll, ok)
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
	reset := time.Date(2026, time.June, 21, 19, 0, 0, 0, time.UTC)
	if err := st.SetUsageWindow(string(model.AgentClaude), "", 28, reset, model.UsageOK); err != nil {
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
		if !b.WindowEnd.Equal(reset) {
			t.Errorf("window_end = %s, want %s (the reset time)", b.WindowEnd, reset)
		}
		if b.State != model.UsageOK {
			t.Errorf("state = %s, want ok", b.State)
		}
	}
	if !found {
		t.Fatal("no claude bucket found")
	}
}
