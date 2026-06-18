package agent

import (
	"reflect"
	"testing"
)

func TestDismissStartupDialog(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		keys   []string
		ok     bool
	}{
		{
			name: "claude folder trust",
			screen: "Quick safety check: Is this a project you trust?\n" +
				"❯ 1. Yes, I trust this folder\n  2. No, exit",
			keys: []string{"Enter"}, ok: true,
		},
		{
			name:   "codex folder trust",
			screen: "Do you trust the contents of this directory?\n❯ 1. Yes, continue",
			keys:   []string{"Enter"}, ok: true,
		},
		{
			// The default option runs `npm install`; we must step off it first.
			name: "codex update nudge",
			screen: "✨ Update available! 0.139.0 -> 0.140.0\n" +
				"› 1. Update now (runs `npm install -g @openai/codex`)\n" +
				"  2. Skip\n  3. Skip until next version\n  Press enter to continue",
			keys: []string{"Down", "Enter"}, ok: true,
		},
		{
			// The persistent post-dismissal notice must NOT re-trigger: it lacks
			// the option labels, so it is not a blocking prompt.
			name:   "codex update notice (non-blocking)",
			screen: "✨ Update available! 0.139.0 -> 0.140.0\nRun npm install -g @openai/codex to update.",
			keys:   nil, ok: false,
		},
		{
			name:   "ordinary tui",
			screen: "❯ \n  bypass permissions on · 1 shell · ? for shortcuts",
			keys:   nil, ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys, ok := DismissStartupDialog(tc.screen)
			if ok != tc.ok || !reflect.DeepEqual(keys, tc.keys) {
				t.Fatalf("got (%v, %v), want (%v, %v)", keys, ok, tc.keys, tc.ok)
			}
		})
	}
}
