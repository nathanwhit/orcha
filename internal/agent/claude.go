package agent

import (
	"encoding/json"
	"strings"

	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// ClaudeConfig configures the interactive Claude provider.
type ClaudeConfig struct {
	// Binary is the claude executable (default "claude").
	Binary string
	// Model optionally pins a model (e.g. "claude-opus-4-8").
	Model string
	// ExtraArgs are appended to the argv (e.g. a permission mode).
	ExtraArgs []string
	// ExecutorFor overrides target executor selection (tests).
	ExecutorFor func(spec Spec) exec.Executor
}

// NewClaude builds a real, interactive Claude provider. The session is one
// durable process driven over stream-json: the opening prompt and every
// steering message are user messages written to stdin, and the agent's output
// is parsed from stream-json events on stdout. This is the interactive mode the
// spec prefers — steering means "send a message to the running session", not
// cancel-and-restart.
func NewClaude(cfg ClaudeConfig) *ProcessProvider {
	bin := cfg.Binary
	if bin == "" {
		bin = "claude"
	}
	build := func(spec Spec) exec.Command {
		args := []string{
			"--print",                       // headless transport...
			"--input-format", "stream-json", // ...but stream-json input keeps the
			"--output-format", "stream-json", //    process alive and multi-turn.
			"--verbose",
			"--replay-user-messages",
		}
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
		args = append(args, claudeControlArgs(spec)...)
		args = append(args, cfg.ExtraArgs...)
		dir := workDirFor(spec)
		return exec.Command{Name: bin, Args: args, Dir: dir}
	}
	return NewProcessProvider(ProcessConfig{
		Kind:        model.AgentClaude,
		Interactive: true,
		Build:       build,
		NewParser:   func() LineParser { return &claudeParser{} },
		EncodeInput: encodeClaudeInput,
		ExecutorFor: cfg.ExecutorFor,
	})
}

// claudeControlArgs renders the per-session control flags shared by the headless
// and tmux Claude launchers: MCP servers (the manager tool surface),
// pre-approved tools, and permission mode. Tools appear to the agent as
// mcp__<server>__<tool>.
func claudeControlArgs(spec Spec) []string {
	var args []string
	if len(spec.MCP) > 0 {
		servers := map[string]any{}
		for name, url := range spec.MCP {
			servers[name] = map[string]any{"type": "http", "url": url}
		}
		cfgJSON, _ := json.Marshal(map[string]any{"mcpServers": servers})
		args = append(args, "--mcp-config", string(cfgJSON))
	}
	if spec.PermissionMode != "" {
		args = append(args, "--permission-mode", spec.PermissionMode)
	}
	if len(spec.AllowedTools) > 0 {
		// One =-attached token. --allowedTools is variadic: as a bare flag it
		// slurps every following non-flag argument — including a positional
		// prompt, which silently became "tool rules" instead of the prompt.
		args = append(args, "--allowedTools="+strings.Join(spec.AllowedTools, ","))
	}
	// Don't stamp orcha's commits and PRs with a "Co-Authored-By: Claude" byline:
	// the work is the team's, not the model's, and the attribution is noise on the
	// PR. Injected as an inline settings override so it applies to every session.
	args = append(args, "--settings", `{"includeCoAuthoredBy":false}`)
	return args
}

// NewTmuxClaude runs Claude's interactive TUI inside an attachable tmux session,
// wired with the same per-session MCP/tool flags as the headless launcher — so
// an attachable manager can also drive the orchestrator. The opening prompt is
// passed as a positional argument (`claude "prompt"`), which the TUI queues and
// submits itself once it is up — it survives the folder trust dialog, which a
// fresh checkout always triggers and the provider auto-accepts.
func NewTmuxClaude(cfg ClaudeConfig) *TmuxProvider {
	bin := cfg.Binary
	if bin == "" {
		bin = "claude"
	}
	base := func(spec Spec, lead ...string) []string {
		args := append([]string{bin}, lead...)
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
		args = append(args, claudeControlArgs(spec)...)
		return append(args, cfg.ExtraArgs...)
	}
	return NewTmux(TmuxConfig{
		Kind:        model.AgentClaude,
		ExecutorFor: cfg.ExecutorFor,
		Command: func(spec Spec) []string {
			args := base(spec)
			if spec.Prompt != "" {
				args = append(args, spec.Prompt)
			}
			return args
		},
		// If the tmux session died, --continue reopens the most recent
		// conversation in the session's checkout instead of starting cold (the
		// prompt is already part of that conversation).
		ResumeCommand: func(spec Spec) []string {
			return base(spec, "--continue")
		},
		// The trust dialog's phrasing varies across claude versions; the accept
		// option label is the stable marker.
		AcceptDialog: func(screen string) bool {
			return strings.Contains(screen, "Yes, I trust this folder") ||
				strings.Contains(screen, "Do you trust the files in this folder?")
		},
	})
}

// encodeClaudeInput frames text as a stream-json user message line.
func encodeClaudeInput(text string) string {
	msg := claudeUserMessage{Type: "user"}
	msg.Message.Role = "user"
	msg.Message.Content = []claudeContentBlock{{Type: "text", Text: text}}
	b, err := json.Marshal(msg)
	if err != nil {
		return ""
	}
	return string(b) + "\n"
}

type claudeUserMessage struct {
	Type    string `json:"type"`
	Message struct {
		Role    string               `json:"role"`
		Content []claudeContentBlock `json:"content"`
	} `json:"message"`
}

type claudeContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result fields
	Content   json.RawMessage `json:"content,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
}

// claudeParser maps Claude stream-json output lines into orchestrator events.
// It is tolerant: anything it does not recognize is surfaced as raw stdout so
// nothing is silently dropped.
type claudeParser struct{}

func (p *claudeParser) Parse(line string) []Event {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var env struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Message struct {
			Content []claudeContentBlock `json:"content"`
		} `json:"message"`
		Result string `json:"result"`
		Usage  struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		// Not JSON we understand — keep it as raw stdout rather than dropping it.
		return []Event{{Kind: EventStdout, Source: model.MsgStdout, Content: line}}
	}

	switch env.Type {
	case "system":
		return []Event{{Kind: EventStatus, Source: model.MsgSystem, Activity: "claude: " + env.Subtype}}
	case "assistant":
		var out []Event
		for _, c := range env.Message.Content {
			switch c.Type {
			case "text":
				if c.Text != "" {
					out = append(out, Event{Kind: EventText, Source: model.MsgAgent, Content: c.Text})
				}
			case "tool_use":
				out = append(out, Event{
					Kind:     EventToolCall,
					Source:   model.MsgTool,
					Content:  c.Name + " " + string(c.Input),
					Activity: "tool: " + c.Name,
					Metadata: model.JSONMap{"tool": c.Name, "input": json.RawMessage(c.Input)},
				})
			}
		}
		return out
	case "user":
		var out []Event
		for _, c := range env.Message.Content {
			if c.Type == "tool_result" {
				out = append(out, Event{Kind: EventToolResult, Source: model.MsgTool, Content: string(c.Content)})
			}
		}
		return out
	case "result":
		var out []Event
		if env.Usage.InputTokens+env.Usage.OutputTokens > 0 {
			out = append(out, Event{Kind: EventUsage, Source: model.MsgSystem,
				UsedTokens: env.Usage.InputTokens + env.Usage.OutputTokens})
		}
		// A result marks the end of a turn (not the session). Record it as the
		// latest summary via a status event.
		summary := env.Result
		if summary == "" {
			summary = "turn complete"
		}
		out = append(out, Event{Kind: EventStatus, Source: model.MsgAgent, Content: summary, Activity: "idle (awaiting input)"})
		return out
	default:
		return nil
	}
}

func (p *claudeParser) Done(error) []Event { return nil }
