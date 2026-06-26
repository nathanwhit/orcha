// Package agent defines the runtime contract every provider implements and
// ships an in-process fake used by tests and local development.
package agent

import (
	"context"

	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// Spec describes a session to start.
type Spec struct {
	SessionID string
	Role      model.SessionRole
	Mode      model.SessionMode
	Goal      string
	Prompt    string
	// CompactContext is the summary-only context handed to the agent (never raw
	// transcripts), per the context-management rules.
	CompactContext string
	Workspace      *model.Workspace
	Target         *model.Target
	Metadata       model.JSONMap
	// Env carries extra KEY=VALUE entries for the session process. Providers
	// append these to the target's inherited environment.
	Env []string
	// MCP maps an MCP server name to its URL (e.g. {"orcha": "http://.../mcp/<id>"}).
	// Providers that support MCP (Claude) expose these tools to the session.
	MCP map[string]string
	// AllowedTools are auto-approved tool names/patterns (e.g. "mcp__orcha").
	AllowedTools []string
	// PermissionMode overrides the agent's permission mode (e.g. "acceptEdits").
	PermissionMode string
	// OneShot marks a session that completes when its turn ends (a coding worker
	// or PR/CI follow-up), as opposed to a long-lived conversational session (a
	// manager) that only ends on an explicit signal. An interactive TUI never
	// exits on its own, so tmux providers use this to know they may treat a
	// finished turn — the completion sentinel, or a long-quiescent pane — as the
	// session being done.
	OneShot bool
}

// TurnDoneSentinel is the exact line a one-shot tmux agent is asked to print as
// the very last thing it outputs, to mark its turn complete. An interactive TUI
// (claude/codex run like a human runs them) never exits, so this printed marker
// is the protocol-level completion signal a tmux provider watches for in the
// pane — not an arbitrary timer.
const TurnDoneSentinel = "===ORCHA-SESSION-COMPLETE==="

// EventKind classifies a runtime event emitted by a provider.
type EventKind string

const (
	EventText       EventKind = "text"
	EventToolCall   EventKind = "tool_call"
	EventToolResult EventKind = "tool_result"
	EventStatus     EventKind = "status"
	// EventProgress is a live progress signal scraped from an interactive TUI
	// pane: a newly-settled line of the agent's output (Content) and/or the
	// current activity (Activity). It both feeds the transcript/live view and
	// counts as forward progress, but is best-effort and may be dropped under
	// backpressure — never use it for control decisions.
	EventProgress EventKind = "progress"
	EventStdout   EventKind = "stdout"
	EventStderr   EventKind = "stderr"
	EventUsage    EventKind = "usage"
	EventError    EventKind = "error"
	// EventDone signals the provider finished; Success indicates the outcome.
	EventDone EventKind = "done"
)

// Event is a single item read from a running session.
type Event struct {
	Kind       EventKind
	Source     model.MessageSource
	Content    string
	Activity   string // optional one-line current-activity update
	UsedTokens int64  // for EventUsage
	Success    bool   // for EventDone
	Metadata   model.JSONMap
}

// Handle is an opaque reference to a running session process.
type Handle interface {
	// ID returns the provider-side identifier (may differ from session id).
	ID() string
	// Interactive reports whether send_input is supported live.
	Interactive() bool
}

// Provider is the runtime contract. Interactive providers are preferred; for
// non-interactive providers steering is implemented by the orchestrator as
// cancel + resume with compact context.
type Provider interface {
	Kind() model.AgentKind
	// StartSession launches a session and returns a handle plus an event stream.
	StartSession(ctx context.Context, spec Spec) (Handle, <-chan Event, error)
	// SendInput steers a running interactive session.
	SendInput(h Handle, text string) error
	// CancelSession terminates a session, killing its process group.
	CancelSession(h Handle) error
	// ResumeSession re-attaches/recreates a session from compact context.
	ResumeSession(ctx context.Context, sessionID string, spec Spec) (Handle, <-chan Event, error)
}

// Screen is a snapshot of a session's terminal: the visible pane content,
// carrying ANSI color/attribute escapes, plus the pane dimensions so the UI can
// size its emulator to match and render the screen 1:1.
type Screen struct {
	Content string `json:"screen"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

// Snapshotter is an optional provider capability: return the current visible
// screen for a session (e.g. a tmux capture-pane), so the UI can render the live
// terminal panel.
type Snapshotter interface {
	Snapshot(h Handle) (Screen, error)
}

// Attacher is an optional provider capability: open a live, interactive pty
// attached to a running session, so the UI can drive it like a real terminal
// (keystrokes, signals, full-screen TUIs) rather than just mirror it.
type Attacher interface {
	AttachPTY(h Handle, cols, rows uint16) (exec.PTYProcess, error)
}

// workDirFor returns the directory a session's process runs in: its workspace
// checkout when one exists, else a per-session scratch dir under the target's
// work root. An agent must never default to the orchestrator's own cwd — a
// stray coding worker there edits (and commits to) the operator's live repo.
func workDirFor(spec Spec) string {
	if spec.Workspace != nil && spec.Workspace.Path != "" {
		return spec.Workspace.Path
	}
	if spec.Target != nil && spec.Target.WorkRoot != "" {
		return spec.Target.WorkRoot + "/scratch-" + sanitizeID(spec.SessionID)
	}
	return ""
}

// ensureDir creates dir on the target (mkdir -p semantics, best effort).
func ensureDir(ctx context.Context, ex exec.Executor, dir string) {
	if dir == "" {
		return
	}
	_, _ = exec.RunCapture(ctx, ex, exec.Command{Name: "mkdir", Args: []string{"-p", dir}})
}
