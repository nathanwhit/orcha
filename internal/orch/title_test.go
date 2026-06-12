package orch

import (
	"strings"
	"testing"
)

func hasSuffixRune(s string, r rune) bool {
	return strings.HasSuffix(s, string(r))
}

func TestProvisionalTitle(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{"empty", "", "Untitled objective"},
		{"whitespace only", "   \n\t\n  ", "Untitled objective"},
		{"short single line", "Ship the health endpoint", "Ship the health endpoint"},
		{"first non-empty line", "\n\n  Add retries  \nthen more detail", "Add retries"},
		{
			"truncated with ellipsis",
			"This is a fairly long objective prompt that goes well beyond the sixty character limit for sure",
			"", // checked by properties below, not an exact string
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := provisionalTitle(c.prompt)
			if c.name == "truncated with ellipsis" {
				// Verify properties rather than a brittle exact string.
				if r := []rune(got); len(r) > provisionalTitleMax {
					t.Fatalf("len = %d runes, want <= %d (%q)", len(r), provisionalTitleMax, got)
				}
				if !hasSuffixRune(got, '…') {
					t.Fatalf("want trailing ellipsis, got %q", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("provisionalTitle(%q) = %q, want %q", c.prompt, got, c.want)
			}
		})
	}
}

func TestProvisionalTitleExactBoundary(t *testing.T) {
	// Exactly provisionalTitleMax runes must pass through untouched.
	in := ""
	for len([]rune(in)) < provisionalTitleMax {
		in += "x"
	}
	if got := provisionalTitle(in); got != in {
		t.Fatalf("boundary input mutated: got %q (%d runes)", got, len([]rune(got)))
	}
}

func TestSanitizeTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Add health endpoint", "Add health endpoint"},
		{"trim whitespace", "   Add health endpoint  ", "Add health endpoint"},
		{"strip double quotes", "\"Add health endpoint\"", "Add health endpoint"},
		{"strip single quotes", "'Add health endpoint'", "Add health endpoint"},
		{"strip backticks", "`Add health endpoint`", "Add health endpoint"},
		{"strip smart quotes", "“Add health endpoint”", "Add health endpoint"},
		{"trailing period", "Add health endpoint.", "Add health endpoint"},
		{"trailing punctuation mix", "Add health endpoint?!", "Add health endpoint"},
		{"first non-empty line only", "Sure, here it is:\nAdd health endpoint\n", "Sure, here it is"},
		{"quoted then punctuation", "\"Add health endpoint.\"", "Add health endpoint"},
		{"empty", "", ""},
		{"whitespace only", "   \n  ", ""},
		{"punctuation only", "...", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeTitle(c.in); got != c.want {
				t.Fatalf("sanitizeTitle(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSanitizeTitleCapsLength(t *testing.T) {
	long := ""
	for len([]rune(long)) < generatedTitleMax+50 {
		long += "word "
	}
	got := sanitizeTitle(long)
	if r := []rune(got); len(r) > generatedTitleMax {
		t.Fatalf("sanitizeTitle did not cap length: %d runes (max %d)", len(r), generatedTitleMax)
	}
}

func TestLastCodexMessage(t *testing.T) {
	out := `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}
{"type":"item.completed","item":{"type":"agent_message","text":"First draft"}}
{"type":"item.completed","item":{"type":"agent_message","text":"Add health endpoint"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}
not json at all`
	if got := lastCodexMessage(out); got != "Add health endpoint" {
		t.Fatalf("lastCodexMessage = %q, want %q", got, "Add health endpoint")
	}
	if got := lastCodexMessage(""); got != "" {
		t.Fatalf("lastCodexMessage(empty) = %q, want empty", got)
	}
}
