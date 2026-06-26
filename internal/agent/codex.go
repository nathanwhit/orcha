package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// CodexConfig configures the Codex provider.
type CodexConfig struct {
	Binary      string
	Model       string
	ExtraArgs   []string
	ExecutorFor func(spec Spec) exec.Executor
	// CompletionGate vetoes idle-pane completion in tmux mode (see TmuxConfig).
	CompletionGate func(sessionID string) bool
	// MaxIdleWithBgWork overrides TmuxConfig.MaxIdleWithBgWork (tmux mode); zero
	// uses the default.
	MaxIdleWithBgWork time.Duration
}

// NewCodex builds a real Codex provider backed by `codex exec --json`.
//
// Codex has no persistent stdin-streaming mode like Claude's stream-json — its
// only stable programmatic mode is the one-shot `codex exec` (true interactive
// access is the experimental app-server protocol). So this provider is
// non-interactive: the orchestrator steers it via the spec's cancel/resume
// protocol. Crucially, resume preserves the logical session: Codex emits a
// thread id on start (surfaced as provider_session_id), and a resumed run uses
// `codex exec resume <thread-id>` so the conversation/context carries over
// rather than starting fresh.
func NewCodex(cfg CodexConfig) *ProcessProvider {
	bin := cfg.Binary
	if bin == "" {
		bin = "codex"
	}
	build := func(spec Spec) exec.Command {
		dir := workDirFor(spec)
		threadID, _ := spec.Metadata["provider_session_id"].(string)
		return exec.Command{Name: bin, Args: codexArgs(cfg.Model, cfg.ExtraArgs, threadID, spec.PermissionMode, spec.MCP), Dir: dir, Env: spec.Env}
	}
	return NewProcessProvider(ProcessConfig{
		Kind:        model.AgentCodex,
		Interactive: false,
		Build:       build,
		NewParser:   func() LineParser { return &codexParser{} },
		ExecutorFor: cfg.ExecutorFor,
	})
}

// NewTmuxCodex runs Codex's interactive TUI inside an attachable tmux session —
// the plain `codex` a human runs, in a pty, steered via send-keys — mirroring
// NewTmuxClaude. The opening prompt is a positional argument; a fresh checkout's
// folder-trust dialog is auto-accepted; a dead session resumes the most recent
// codex conversation in the checkout.
func NewTmuxCodex(cfg CodexConfig) *TmuxProvider {
	bin := cfg.Binary
	if bin == "" {
		bin = "codex"
	}
	base := func(spec Spec, sub ...string) []string {
		args := append([]string{bin}, sub...)
		args = append(args, codexSandboxArgs(spec.PermissionMode)...)
		args = append(args, codexMCPArgs(spec.MCP)...)
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
		return append(args, cfg.ExtraArgs...)
	}
	return NewTmux(TmuxConfig{
		Kind:              model.AgentCodex,
		ExecutorFor:       cfg.ExecutorFor,
		CompletionGate:    cfg.CompletionGate,
		MaxIdleWithBgWork: cfg.MaxIdleWithBgWork,
		Command: func(spec Spec) []string {
			args := base(spec)
			if spec.Prompt != "" {
				args = append(args, spec.Prompt)
			}
			return args
		},
		// If the tmux session died, resume the most recent conversation in the
		// checkout instead of starting cold (the prompt is already in it).
		ResumeCommand: func(spec Spec) []string {
			return base(spec, "resume", "--last")
		},
		// Codex shows a folder-trust dialog on a fresh checkout, like claude, and
		// an "Update available" nudge whose default option would npm-install; the
		// watchdog clears both (see DismissStartupDialog).
		DismissDialogs: true,
	})
}

// codexArgs builds the `codex` argv. With a threadID it resumes that thread
// (preserving context), reading the new turn from stdin; otherwise it starts a
// fresh one-shot exec reading instructions from stdin. MCP flags go before the
// trailing positional SESSION_ID/PROMPT so they parse as options, not prompts.
func codexArgs(modelName string, extra []string, threadID, permMode string, mcp map[string]string) []string {
	var args []string
	if threadID != "" {
		args = []string{"exec", "resume", "--json", "--skip-git-repo-check"}
	} else {
		args = []string{"exec", "--json", "--skip-git-repo-check"}
	}
	args = append(args, codexSandboxArgs(permMode)...)
	args = append(args, codexMCPArgs(mcp)...)
	if modelName != "" {
		args = append(args, "--model", modelName)
	}
	args = append(args, extra...)
	if threadID != "" {
		args = append(args, threadID, "-") // SESSION_ID then PROMPT ("-" = stdin)
	}
	return args
}

// codexMCPArgs renders the spec's MCP servers as codex `-c` config overrides,
// e.g. -c 'mcp_servers.orcha.url="http://..."'. Codex speaks the Streamable
// HTTP MCP transport natively (no experimental flag), so this is all that is
// needed to hand a codex session the orcha manager tool surface — the same
// tools a claude manager gets via --mcp-config. Tools then appear to the agent
// as mcp__<server>.<tool>. The value after `=` is parsed as TOML, so the URL is
// emitted as a quoted TOML basic string. Servers are sorted for a stable argv.
func codexMCPArgs(mcp map[string]string) []string {
	if len(mcp) == 0 {
		return nil
	}
	names := make([]string, 0, len(mcp))
	for name := range mcp {
		names = append(names, name)
	}
	sort.Strings(names)
	var args []string
	for _, name := range names {
		args = append(args, "-c", fmt.Sprintf("mcp_servers.%s.url=%q", name, mcp[name]))
	}
	return args
}

// codexSandboxArgs maps a permission mode to codex's sandbox/approval flags.
// Codex defaults to a read-only sandbox with approval prompts, so without this
// a worker cannot write and a headless run hangs on the first prompt.
//
//   - bypassPermissions -> --dangerously-bypass-approvals-and-sandbox (no
//     sandbox, no prompts; only safe when the host is itself sandboxed, e.g. a
//     VM — exactly orcha's deployment).
//   - anything else      -> --sandbox workspace-write (can write its checkout,
//     no network; the auto approval policy never blocks headless on edits).
func codexSandboxArgs(permMode string) []string {
	if permMode == "bypassPermissions" {
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	}
	return []string{"--sandbox", "workspace-write"}
}

// codexParser maps Codex exec JSONL events into orchestrator events. The shape
// is: thread.started (carries the resumable thread id), turn.started,
// item.completed (agent messages, reasoning, tool/command items), and
// turn.completed (usage). Anything unrecognized is preserved as raw stdout.
type codexParser struct{}

func (codexParser) Parse(line string) []Event {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var ev struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Item     struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return []Event{{Kind: EventStdout, Source: model.MsgStdout, Content: line}}
	}

	switch ev.Type {
	case "thread.started":
		// Surface the thread id so the orchestrator can resume this session.
		return []Event{{
			Kind:     EventStatus,
			Source:   model.MsgSystem,
			Activity: "codex thread " + ev.ThreadID,
			Metadata: model.JSONMap{"provider_session_id": ev.ThreadID},
		}}
	case "turn.started":
		return []Event{{Kind: EventStatus, Source: model.MsgAgent, Activity: "working"}}
	case "item.completed", "item.updated":
		switch ev.Item.Type {
		case "agent_message":
			if ev.Item.Text != "" {
				return []Event{{Kind: EventText, Source: model.MsgAgent, Content: ev.Item.Text}}
			}
		case "reasoning":
			return []Event{{Kind: EventStatus, Source: model.MsgAgent, Activity: "thinking"}}
		case "command_execution", "mcp_tool_call", "file_change", "web_search":
			return []Event{{Kind: EventToolCall, Source: model.MsgTool,
				Content: ev.Item.Type + ": " + ev.Item.Text, Activity: "tool: " + ev.Item.Type}}
		}
	case "turn.completed":
		if ev.Usage.InputTokens+ev.Usage.OutputTokens > 0 {
			return []Event{{Kind: EventUsage, Source: model.MsgSystem,
				UsedTokens: ev.Usage.InputTokens + ev.Usage.OutputTokens}}
		}
		return nil
	case "error", "turn.failed":
		return []Event{{Kind: EventError, Source: model.MsgSystem, Content: line}}
	}
	// Unrecognized but valid JSON: keep it visible in the transcript.
	return []Event{{Kind: EventStdout, Source: model.MsgStdout, Content: line}}
}

func (codexParser) Done(error) []Event { return nil }
