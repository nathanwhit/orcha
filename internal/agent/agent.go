// Package agent defines the runtime contract every provider implements and
// ships an in-process fake used by tests and local development.
package agent

import (
	"context"

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
	// MCP maps an MCP server name to its URL (e.g. {"orcha": "http://.../mcp/<id>"}).
	// Providers that support MCP (Claude) expose these tools to the session.
	MCP map[string]string
	// AllowedTools are auto-approved tool names/patterns (e.g. "mcp__orcha").
	AllowedTools []string
	// PermissionMode overrides the agent's permission mode (e.g. "acceptEdits").
	PermissionMode string
}

// EventKind classifies a runtime event emitted by a provider.
type EventKind string

const (
	EventText       EventKind = "text"
	EventToolCall   EventKind = "tool_call"
	EventToolResult EventKind = "tool_result"
	EventStatus     EventKind = "status"
	EventStdout     EventKind = "stdout"
	EventStderr     EventKind = "stderr"
	EventUsage      EventKind = "usage"
	EventError      EventKind = "error"
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
