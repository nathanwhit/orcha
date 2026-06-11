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

// NewCodex builds a real Codex provider backed by `codex exec --json`. Codex
// exec is one-shot, so this provider is non-interactive: the orchestrator steers
// it via the spec's cancel-and-resume protocol rather than live stdin. The
// prompt is delivered on stdin (which is closed to signal EOF) and Codex's JSONL
// events are parsed from stdout.
func NewCodex(cfg CodexConfig) *ProcessProvider {
	bin := cfg.Binary
	if bin == "" {
		bin = "codex"
	}
	build := func(spec Spec) exec.Command {
		args := []string{"exec", "--json", "--skip-git-repo-check"}
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
		if spec.Workspace != nil && spec.Workspace.Path != "" {
			args = append(args, "--cd", spec.Workspace.Path)
		}
		args = append(args, cfg.ExtraArgs...)
		return exec.Command{Name: bin, Args: args}
	}
	return NewProcessProvider(ProcessConfig{
		Kind:        model.AgentCodex,
		Interactive: false,
		Build:       build,
		NewParser:   func() LineParser { return &codexParser{} },
		ExecutorFor: cfg.ExecutorFor,
	})
}

// codexParser maps Codex exec JSONL events into orchestrator events. The exec
// event schema varies across Codex versions, so the parser extracts the common
// fields it recognizes (agent messages, reasoning, token counts) and preserves
// anything else as raw stdout rather than dropping it.
type codexParser struct{}

func (codexParser) Parse(line string) []Event {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var ev struct {
		Type string `json:"type"`
		Msg  struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Text    string `json:"text"`
		} `json:"msg"`
		// Newer item-based shape.
		Item struct {
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

	kind := ev.Msg.Type
	text := firstNonEmpty(ev.Msg.Message, ev.Msg.Text, ev.Item.Text)
	switch {
	case strings.Contains(kind, "agent_message") || ev.Item.Type == "assistant_message":
		if text != "" {
			return []Event{{Kind: EventText, Source: model.MsgAgent, Content: text}}
		}
	case strings.Contains(kind, "reasoning"):
		if text != "" {
			return []Event{{Kind: EventStatus, Source: model.MsgAgent, Activity: "thinking"}}
		}
	case strings.Contains(kind, "exec_command") || strings.Contains(kind, "tool"):
		return []Event{{Kind: EventToolCall, Source: model.MsgTool, Content: text, Activity: "tool"}}
	}
	if ev.Usage.InputTokens+ev.Usage.OutputTokens > 0 {
		return []Event{{Kind: EventUsage, Source: model.MsgSystem, UsedTokens: ev.Usage.InputTokens + ev.Usage.OutputTokens}}
	}
	// Unrecognized but valid JSON: keep it visible in the transcript.
	return []Event{{Kind: EventStdout, Source: model.MsgStdout, Content: line}}
}

func (codexParser) Done(error) []Event { return nil }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
