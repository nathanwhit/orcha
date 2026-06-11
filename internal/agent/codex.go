package agent

import (
	"encoding/json"
	"strings"

	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// CodexConfig configures the Codex provider.
type CodexConfig struct {
	Binary      string
	Model       string
	ExtraArgs   []string
	ExecutorFor func(spec Spec) exec.Executor
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
		dir := ""
		if spec.Workspace != nil {
			dir = spec.Workspace.Path
		}
		threadID, _ := spec.Metadata["provider_session_id"].(string)
		return exec.Command{Name: bin, Args: codexArgs(cfg.Model, cfg.ExtraArgs, threadID), Dir: dir}
	}
	return NewProcessProvider(ProcessConfig{
		Kind:        model.AgentCodex,
		Interactive: false,
		Build:       build,
		NewParser:   func() LineParser { return &codexParser{} },
		ExecutorFor: cfg.ExecutorFor,
	})
}

// codexArgs builds the `codex` argv. With a threadID it resumes that thread
// (preserving context), reading the new turn from stdin; otherwise it starts a
// fresh one-shot exec reading instructions from stdin.
func codexArgs(modelName string, extra []string, threadID string) []string {
	var args []string
	if threadID != "" {
		args = []string{"exec", "resume", "--json", "--skip-git-repo-check"}
	} else {
		args = []string{"exec", "--json", "--skip-git-repo-check"}
	}
	if modelName != "" {
		args = append(args, "--model", modelName)
	}
	args = append(args, extra...)
	if threadID != "" {
		args = append(args, threadID, "-") // SESSION_ID then PROMPT ("-" = stdin)
	}
	return args
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
