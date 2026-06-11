package orch

import (
	"fmt"
	"strings"

	"github.com/nathanwhit/orcha/internal/model"
)

// compactContext builds the summary-only context handed to agents and the
// manager. It deliberately excludes raw stdout and full transcripts — only
// session summaries, PR summaries, and open questions, per the context rules.
func (o *Orchestrator) compactContext(objectiveID string) string {
	if objectiveID == "" {
		return ""
	}
	var b strings.Builder
	if obj, err := o.st.GetObjective(objectiveID); err == nil {
		fmt.Fprintf(&b, "OBJECTIVE: %s\n%s\n\n", obj.Title, obj.Prompt)
		if obj.Summary != "" {
			fmt.Fprintf(&b, "LEDGER: %s\n\n", obj.Summary)
		}
	}
	if sessions, err := o.st.ListSessionsByObjective(objectiveID); err == nil && len(sessions) > 0 {
		b.WriteString("SESSION SUMMARIES:\n")
		for _, s := range sessions {
			summary := s.LatestSummary
			if summary == "" {
				summary = s.CurrentActivity
			}
			fmt.Fprintf(&b, "- [%s/%s] %s: %s\n", s.Role, s.Status, s.Title, summary)
		}
		b.WriteString("\n")
	}
	if prs, err := o.st.ListPRsByObjective(objectiveID); err == nil && len(prs) > 0 {
		b.WriteString("PULL REQUESTS:\n")
		for _, p := range prs {
			fmt.Fprintf(&b, "- #%d [%s/%s] %s: %s\n", p.Number, p.Status, p.ChecksState, p.Title, p.Summary)
		}
		b.WriteString("\n")
	}
	if qs, err := o.st.ListQuestionsByObjective(objectiveID); err == nil {
		var open []*model.Question
		for _, q := range qs {
			if q.Status == model.QuestionOpen {
				open = append(open, q)
			}
		}
		if len(open) > 0 {
			b.WriteString("OPEN QUESTIONS:\n")
			for _, q := range open {
				fmt.Fprintf(&b, "- %s\n", q.Question)
			}
		}
	}
	return b.String()
}

// recordUsage attributes token usage to the session's provider and logs a usage
// row in the transcript.
func (o *Orchestrator) recordUsage(sessionID string, tokens int64) {
	if tokens <= 0 {
		return
	}
	sess, err := o.st.GetSession(sessionID)
	if err != nil {
		return
	}
	provider := sess.UsageProvider
	if provider == "" {
		provider = string(sess.Agent)
	}
	_ = o.st.AddUsageTokens(provider, "", tokens, "")
	_ = o.emit(sessionID, model.MsgSystem, model.KindUsage,
		fmt.Sprintf("used %d tokens", tokens), model.JSONMap{"tokens": tokens})
}

// askProviderExhausted opens a user question when no provider is usable.
func (o *Orchestrator) askProviderExhausted(sess *model.Session) {
	_ = o.st.CreateQuestion(&model.Question{
		ObjectiveID: sess.ObjectiveID,
		SessionID:   sess.ID,
		Priority:    20,
		Question:    "All candidate providers are exhausted. Which provider or account should be used?",
		Context:     "preferred=" + string(sess.Agent),
	})
	o.audit(sess.ObjectiveID, sess.ID, "provider_exhausted", "all providers exhausted; asked user", nil)
}
