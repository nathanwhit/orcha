package orch

import (
	"context"
	"fmt"
	"strings"

	"github.com/nathanwhit/orcha/internal/forge"
	"github.com/nathanwhit/orcha/internal/model"
)

// SyncIssueTriggers turns GitHub issues into objectives. For every registered
// project it scans (a) recent issue comments for an @-mention of the bot, and
// (b) issues assigned to the bot — and for each, if the requesting login is
// allowlisted and the trigger has not already been acted on, it creates an
// objective and acknowledges the issue. A monitor loop calls this on the same
// cadence as the PR monitor. It is a no-op unless the feature is configured.
//
// The orchestrator's forge runs `gh` on the orchestrator host, where it must be
// authenticated as the bot account: it reads issues and posts the ack comment as
// the bot. Authorization is the allowlist, not gh auth — anyone could open an
// issue, so only an @-mention from (or an assignment by) an allowlisted login
// summons work.
func (o *Orchestrator) SyncIssueTriggers(ctx context.Context) {
	if !o.IssueTriggersEnabled() {
		return
	}
	bot := o.cfg.IssueTriggers.BotLogin
	projects, err := o.st.ListProjects()
	if err != nil {
		return
	}
	for _, p := range projects {
		o.syncRepoMentions(ctx, p, bot)
		o.syncRepoAssignments(ctx, p, bot)
	}
}

// IssueTriggersEnabled reports whether the issue-trigger monitor is configured:
// a real forge, a bot login, and a non-empty allowlist. Empty allowlist is
// fail-closed — the feature does nothing until someone is explicitly permitted.
func (o *Orchestrator) IssueTriggersEnabled() bool {
	return o.forge != nil &&
		o.cfg.IssueTriggers.BotLogin != "" &&
		len(o.cfg.IssueTriggers.AllowedLogins) > 0
}

// syncRepoMentions handles the @-mention trigger for one repo.
func (o *Orchestrator) syncRepoMentions(ctx context.Context, p *model.Project, bot string) {
	comments, err := o.forge.ListRecentIssueComments(ctx, p.Repo)
	if err != nil {
		return
	}
	for _, c := range comments {
		if c.IsPR {
			continue // PR conversation — handled by the PR feedback path
		}
		if strings.Contains(c.Body, orchaBotMarker) {
			continue // our own ack comment
		}
		if !mentionsLogin(c.Body, bot) || !o.issueLoginAllowed(c.Author) {
			continue
		}
		iss, err := o.forge.GetIssue(ctx, p.Repo, c.IssueNumber)
		if err != nil {
			continue // transient; retry next poll (no claim recorded yet)
		}
		o.triggerIssueObjective(ctx, p, iss, "comment:"+c.ExternalID, c.Author, "an @-mention")
	}
}

// syncRepoAssignments handles the assignment trigger for one repo.
func (o *Orchestrator) syncRepoAssignments(ctx context.Context, p *model.Project, bot string) {
	issues, err := o.forge.ListAssignedIssues(ctx, p.Repo, bot)
	if err != nil {
		return
	}
	for _, iss := range issues {
		actor, eventID, err := o.forge.LatestAssignment(ctx, p.Repo, iss.Number, bot)
		if err != nil || actor == "" {
			continue // can't attribute the assignment yet; retry next poll
		}
		if !o.issueLoginAllowed(actor) {
			continue
		}
		// Key dedup on the assignment event so re-assigning the bot (a new event)
		// re-fires; falling back to a per-issue key when no stable id is exposed.
		ext := "assigned"
		if eventID != "" {
			ext = "assigned:" + eventID
		}
		o.triggerIssueObjective(ctx, p, iss, ext, actor, "an assignment")
	}
}

// triggerIssueObjective claims the trigger (deduped), creates an objective for
// the issue, and acknowledges it. The claim is recorded before the objective is
// created so a concurrent poll can't double-spawn; if creation fails the claim is
// rolled back so a later poll retries.
func (o *Orchestrator) triggerIssueObjective(ctx context.Context, p *model.Project, iss forge.Issue, externalID, requester, via string) {
	inserted, err := o.st.RecordIssueTask(p.Repo, iss.Number, externalID)
	if err != nil || !inserted {
		return // already handled (or store error) — don't act twice
	}
	obj, _, err := o.CreateObjective(NewObjectiveSpec{
		Title:      fmt.Sprintf("Issue #%d: %s", iss.Number, iss.Title),
		Prompt:     issueObjectivePrompt(p.Repo, iss, requester, via),
		Repo:       p.Repo,
		PushRepo:   p.PushRepo,
		CloneURL:   p.CloneURL,
		BaseBranch: p.BaseBranch,
	})
	if err != nil {
		_ = o.st.DeleteIssueTask(p.Repo, iss.Number, externalID)
		return
	}
	_ = o.st.SetIssueTaskObjective(p.Repo, iss.Number, externalID, obj.ID)
	o.audit(obj.ID, "", "issue_triggered",
		fmt.Sprintf("created objective from issue #%d via %s by @%s", iss.Number, via, requester),
		model.JSONMap{"repo": p.Repo, "issue": iss.Number, "requester": requester})
	_ = o.forge.CommentIssue(ctx, p.Repo, iss.Number, issueAckBody(iss.Number))
}

// mentionsLogin reports whether body @-mentions login (case-insensitive), as a
// whole token — "@bot" matches but "@botfoo" or "email@bot.com" do not.
func mentionsLogin(body, login string) bool {
	needle := "@" + strings.ToLower(strings.TrimPrefix(login, "@"))
	hay := strings.ToLower(body)
	for {
		i := strings.Index(hay, needle)
		if i < 0 {
			return false
		}
		// Char before must be a boundary (not part of a longer word/email).
		if i == 0 || isMentionBoundary(hay[i-1]) {
			end := i + len(needle)
			if end >= len(hay) || isMentionBoundary(hay[end]) {
				return true
			}
		}
		hay = hay[i+len(needle):]
	}
}

func isMentionBoundary(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return false
	case b == '-', b == '_', b == '/':
		// '-' is legal in GitHub logins; '/' guards against "@bot" inside a path;
		// '_' is conservative. Treat all as non-boundary so a trailing one fails.
		return false
	default:
		return true
	}
}

// issueLoginAllowed reports whether login is on the allowlist (case-insensitive,
// tolerant of a leading @).
func (o *Orchestrator) issueLoginAllowed(login string) bool {
	want := strings.ToLower(strings.TrimPrefix(login, "@"))
	if want == "" {
		return false
	}
	for _, l := range o.cfg.IssueTriggers.AllowedLogins {
		if strings.ToLower(strings.TrimPrefix(l, "@")) == want {
			return true
		}
	}
	return false
}

// issueObjectivePrompt frames an issue as a manager prompt.
func issueObjectivePrompt(repo string, iss forge.Issue, requester, via string) string {
	body := strings.TrimSpace(iss.Body)
	if body == "" {
		body = "(the issue has no description)"
	}
	return fmt.Sprintf(
		"Work on GitHub issue #%d in %s, requested by @%s via %s on the issue.\n\n"+
			"Title: %s\n\n%s\n\nIssue URL: %s\n\n"+
			"When the work is complete, open a pull request that references and closes #%d.",
		iss.Number, repo, requester, via, iss.Title, body, iss.URL, iss.Number)
}

// issueAckBody is the comment orcha posts to acknowledge a triggered task. The
// marker keeps the mention monitor from re-triggering on orcha's own comment.
func issueAckBody(number int) string {
	return fmt.Sprintf(
		"🤖 On it — I've picked up issue #%d and started working on it. "+
			"I'll open a pull request that references this issue when it's ready.\n\n%s",
		number, orchaBotMarker)
}
