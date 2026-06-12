package orch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/nathanwhit/orcha/internal/model"
)

const (
	// provisionalTitleMax bounds the placeholder title derived from the prompt.
	provisionalTitleMax = 60
	// generatedTitleMax bounds the LLM-generated title after sanitization.
	generatedTitleMax = 80
	// titleGenTimeout caps the one-shot title-generation call.
	titleGenTimeout = 30 * time.Second
)

// provisionalTitle derives an immediate, human-readable placeholder title from
// the prompt: the first non-empty line, trimmed and truncated to
// provisionalTitleMax runes with an ellipsis. It never blocks on the LLM, so an
// objective can be created the instant a request arrives.
func provisionalTitle(prompt string) string {
	line := ""
	for _, l := range strings.Split(prompt, "\n") {
		if s := strings.TrimSpace(l); s != "" {
			line = s
			break
		}
	}
	if line == "" {
		return "Untitled objective"
	}
	return truncateRunes(line, provisionalTitleMax)
}

// truncateRunes shortens s to at most max runes, appending an ellipsis when it
// had to cut. It counts runes (not bytes) so multibyte text isn't split.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}

// sanitizeTitle cleans an LLM-produced title into a single tidy line: it keeps
// the first non-empty line (models sometimes add preamble or trailing notes),
// strips surrounding quotes and whitespace, drops trailing sentence
// punctuation, and caps the length. It returns "" when nothing usable remains,
// signalling the caller to keep the provisional title.
func sanitizeTitle(raw string) string {
	s := raw
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			s = t
			break
		}
	}
	s = strings.TrimSpace(s)
	s = trimQuotes(s)
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ".!?,;:")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return truncateRunes(s, generatedTitleMax)
}

// trimQuotes removes a single pair of matching surrounding quotes (straight or
// smart) from s.
func trimQuotes(s string) string {
	if len(s) >= 2 {
		switch s[0] {
		case '"', '\'', '`':
			if s[len(s)-1] == s[0] {
				return s[1 : len(s)-1]
			}
		}
	}
	for _, q := range []struct{ open, close string }{
		{"“", "”"}, {"‘", "’"},
	} {
		if strings.HasPrefix(s, q.open) && strings.HasSuffix(s, q.close) {
			return strings.TrimSuffix(strings.TrimPrefix(s, q.open), q.close)
		}
	}
	return s
}

// titlePrompt wraps the user's prompt in a strict instruction that asks for a
// bare title and nothing else.
func titlePrompt(prompt string) string {
	return "Write a concise title (3 to 8 words) summarizing the following task. " +
		"Output ONLY the title with no quotes, no surrounding punctuation, no preamble, " +
		"and no trailing period.\n\nTask:\n" + prompt
}

// generateTitleCLI runs a one-shot LLM call to produce a title for prompt using
// the given agent's CLI, returning the raw (unsanitized) model output. The
// orchestrator host is assumed to have the agent binary on PATH; the call is
// bounded by ctx (see titleGenTimeout).
func generateTitleCLI(ctx context.Context, kind model.AgentKind, prompt string) (string, error) {
	instr := titlePrompt(prompt)
	var cmd *exec.Cmd
	switch kind {
	case model.AgentCodex:
		cmd = exec.CommandContext(ctx, "codex", "exec", "--json", "--skip-git-repo-check", instr)
	default: // claude (and any unknown kind) — the simplest reliable text-out path
		cmd = exec.CommandContext(ctx, "claude", "-p", instr, "--output-format", "text")
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if kind == model.AgentCodex {
		return lastCodexMessage(stdout.String()), nil
	}
	return stdout.String(), nil
}

// lastCodexMessage extracts the final agent_message text from `codex exec
// --json` JSONL output, which interleaves reasoning/tool events with the
// answer.
func lastCodexMessage(out string) string {
	last := ""
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if (ev.Type == "item.completed" || ev.Type == "item.updated") &&
			ev.Item.Type == "agent_message" && ev.Item.Text != "" {
			last = ev.Item.Text
		}
	}
	return last
}

// generateTitleAsync generates a concise title for an objective that was
// created with a provisional title, then persists it so the polling dashboard
// refreshes. Any failure (error, timeout, or empty output) is logged and leaves
// the provisional title in place — it never affects objective creation.
func (o *Orchestrator) generateTitleAsync(objectiveID string, kind model.AgentKind, prompt string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), titleGenTimeout)
		defer cancel()
		raw, err := generateTitleCLI(ctx, kind, prompt)
		if err != nil {
			log.Printf("orch: title generation failed for objective %s: %v", objectiveID, err)
			return
		}
		title := sanitizeTitle(raw)
		if title == "" {
			log.Printf("orch: title generation produced no usable title for objective %s; keeping provisional", objectiveID)
			return
		}
		if err := o.st.UpdateObjectiveTitle(objectiveID, title); err != nil {
			log.Printf("orch: failed to persist generated title for objective %s: %v", objectiveID, err)
			return
		}
		o.audit(objectiveID, "", "objective_title_generated", "generated title: "+title, nil)
	}()
}
