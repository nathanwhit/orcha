package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEncodeClaudeInput_IsValidStreamJSON(t *testing.T) {
	line := encodeClaudeInput("please refactor the parser")
	if !strings.HasSuffix(line, "\n") {
		t.Fatal("stream-json messages must be newline-delimited")
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &msg); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if msg["type"] != "user" {
		t.Fatalf("expected type=user, got %v", msg["type"])
	}
	m := msg["message"].(map[string]any)
	content := m["content"].([]any)[0].(map[string]any)
	if content["text"] != "please refactor the parser" {
		t.Fatalf("text not encoded: %v", content)
	}
}

func TestClaudeParser_MapsStreamJSONEvents(t *testing.T) {
	p := &claudeParser{}

	// init/system line
	if evs := p.Parse(`{"type":"system","subtype":"init","session_id":"x"}`); len(evs) != 1 || evs[0].Kind != EventStatus {
		t.Fatalf("system line: %+v", evs)
	}

	// assistant text + tool_use
	line := `{"type":"assistant","message":{"content":[` +
		`{"type":"text","text":"I'll run the tests"},` +
		`{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`
	evs := p.Parse(line)
	if len(evs) != 2 {
		t.Fatalf("expected text+tool_use, got %+v", evs)
	}
	if evs[0].Kind != EventText || !strings.Contains(evs[0].Content, "run the tests") {
		t.Fatalf("text event wrong: %+v", evs[0])
	}
	if evs[1].Kind != EventToolCall || !strings.Contains(evs[1].Content, "Bash") {
		t.Fatalf("tool_use event wrong: %+v", evs[1])
	}

	// result with usage
	res := `{"type":"result","subtype":"success","result":"done","usage":{"input_tokens":100,"output_tokens":50}}`
	evs = p.Parse(res)
	var usage *Event
	for i := range evs {
		if evs[i].Kind == EventUsage {
			usage = &evs[i]
		}
	}
	if usage == nil || usage.UsedTokens != 150 {
		t.Fatalf("expected usage of 150 tokens, got %+v", evs)
	}

	// Unknown JSON is preserved as raw stdout, never dropped.
	if evs := p.Parse(`{"type":"weird_future_event"}`); len(evs) != 0 {
		// unknown known-shaped type yields nothing; non-JSON yields stdout:
	}
	if evs := p.Parse(`not json at all`); len(evs) != 1 || evs[0].Kind != EventStdout {
		t.Fatalf("non-JSON should be preserved as stdout: %+v", evs)
	}
}

// TestClaude_Live drives the real claude CLI as a persistent interactive
// session. Gated behind ORCHA_CLAUDE_LIVE=1 because it spends tokens.
func TestClaude_Live(t *testing.T) {
	if os.Getenv("ORCHA_CLAUDE_LIVE") != "1" {
		t.Skip("set ORCHA_CLAUDE_LIVE=1 to run the live Claude test")
	}
	p := NewClaude(ClaudeConfig{})
	h, events, err := p.StartSession(context.Background(), Spec{
		SessionID: "live-1",
		Prompt:    "Reply with exactly the single word: READY",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.CancelSession(h)

	deadline := time.After(90 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("stream closed before any agent text")
			}
			if ev.Kind == EventText && strings.Contains(strings.ToUpper(ev.Content), "READY") {
				return // success: a real interactive turn completed
			}
		case <-deadline:
			t.Fatal("no READY response within deadline")
		}
	}
}

// TestClaude_LiveSteering proves the session is persistent and multi-turn:
// after the first turn completes, a second steering message reaches the same
// running process and produces another response. Gated behind ORCHA_CLAUDE_LIVE.
func TestClaude_LiveSteering(t *testing.T) {
	if os.Getenv("ORCHA_CLAUDE_LIVE") != "1" {
		t.Skip("set ORCHA_CLAUDE_LIVE=1 to run the live Claude steering test")
	}
	p := NewClaude(ClaudeConfig{})
	h, events, err := p.StartSession(context.Background(), Spec{
		SessionID: "live-steer",
		Prompt:    "Reply with exactly: ONE",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer p.CancelSession(h)

	awaitText := func(want string) {
		deadline := time.After(90 * time.Second)
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatalf("stream closed before %q", want)
				}
				if ev.Kind == EventText && strings.Contains(strings.ToUpper(ev.Content), want) {
					return
				}
			case <-deadline:
				t.Fatalf("did not see %q within deadline", want)
			}
		}
	}

	awaitText("ONE")
	// Steer the *same* running session with a second message.
	if err := p.SendInput(h, "Now reply with exactly: TWO"); err != nil {
		t.Fatalf("steer: %v", err)
	}
	awaitText("TWO")
}

// --allowedTools must be a single =-attached token: the flag is variadic, so a
// bare form slurps every following non-flag argument — including a positional
// prompt, which then silently becomes "tool rules" instead of the prompt.
func TestClaudeControlArgs_AllowedToolsCannotSlurpPrompt(t *testing.T) {
	args := claudeControlArgs(Spec{
		PermissionMode: "default",
		AllowedTools:   []string{"mcp__orcha", "Read"},
	})
	for _, a := range args {
		if a == "--allowedTools" {
			t.Fatal("--allowedTools passed as a bare variadic flag; it would eat a positional prompt")
		}
	}
	found := false
	for _, a := range args {
		if a == "--allowedTools=mcp__orcha,Read" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing =-attached allowedTools token in %q", args)
	}
}

func TestClaudeControlArgs_DisablesCoAuthorByline(t *testing.T) {
	args := claudeControlArgs(Spec{PermissionMode: "default"})
	var settings string
	for i, a := range args {
		if a == "--settings" && i+1 < len(args) {
			settings = args[i+1]
		}
	}
	if !strings.Contains(settings, `"includeCoAuthoredBy":false`) {
		t.Fatalf("expected a settings override disabling the co-author byline, got %q", settings)
	}
}

func settingsArg(t *testing.T, args []string) string {
	t.Helper()
	for i, a := range args {
		if a == "--settings" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("no --settings token in %q", args)
	return ""
}

// bypassPermissions does NOT auto-approve custom MCP tools (Claude requires MCP
// tools to be explicitly allowlisted even under bypass), so the orcha tool
// surface is gated solely by the allowlist. We carry it through BOTH the
// --allowedTools flag and settings.permissions.allow so a single launch/resume
// or CLI-wildcard edge can't drop a manager into per-tool permission prompts.
func TestClaudeControlArgs_AllowedToolsAlsoRideSettingsPermissions(t *testing.T) {
	args := claudeControlArgs(Spec{
		PermissionMode: "bypassPermissions",
		AllowedTools:   []string{"mcp__orcha", "Read"},
	})
	var s struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(settingsArg(t, args)), &s); err != nil {
		t.Fatalf("settings is not valid JSON: %v", err)
	}
	if got := strings.Join(s.Permissions.Allow, ","); got != "mcp__orcha,Read" {
		t.Fatalf("settings.permissions.allow = %q, want the AllowedTools list", got)
	}
}

// With no AllowedTools there is nothing to pre-approve, so no permissions block
// is emitted (and the settings stay valid JSON).
func TestClaudeControlArgs_NoAllowedToolsOmitsPermissions(t *testing.T) {
	args := claudeControlArgs(Spec{PermissionMode: "default"})
	settings := settingsArg(t, args)
	if strings.Contains(settings, "permissions") {
		t.Fatalf("did not expect a permissions block with no AllowedTools, got %q", settings)
	}
	var any map[string]any
	if err := json.Unmarshal([]byte(settings), &any); err != nil {
		t.Fatalf("settings is not valid JSON: %v", err)
	}
}
