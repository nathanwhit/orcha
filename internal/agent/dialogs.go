package agent

import "strings"

// DismissStartupDialog returns the tmux key names that dismiss a known blocking
// startup dialog currently shown on screen, and ok=true when one is present.
//
// Two kinds of prompt block a freshly launched provider TUI before it accepts
// input, and both must be cleared the same way whether the launcher is a live
// agent session or the usage-monitor probe — so the matching lives here once:
//
//   - Folder trust. Claude shows "Is this a project you trust?" with a
//     "Yes, I trust this folder" option; codex shows "Do you trust the contents
//     of this directory?" with "Yes, continue". The safe choice is the
//     highlighted default, so a single Enter accepts it.
//   - Codex's "Update available" nudge. Its DEFAULT highlighted option is
//     "Update now", which shells out to `npm install -g @openai/codex`. A bare
//     Enter would run the update mid-launch; one Down moves the selection to
//     "Skip", then Enter dismisses it without updating.
//
// Wording drifts across CLI versions, so each case keys off the most stable
// substring available (an option label rather than prose).
func DismissStartupDialog(screen string) (keys []string, ok bool) {
	switch {
	case strings.Contains(screen, "Update now") && strings.Contains(screen, "Skip until next version"):
		// Codex update nudge: step off "Update now" before confirming.
		return []string{"Down", "Enter"}, true
	case strings.Contains(screen, "Yes, I trust this folder"),
		strings.Contains(screen, "Do you trust the files in this folder?"),
		strings.Contains(screen, "Yes, continue"),
		strings.Contains(screen, "Do you trust the contents of this directory?"):
		return []string{"Enter"}, true
	}
	return nil, false
}
