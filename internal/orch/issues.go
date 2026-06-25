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
		o.triggerIssueObjective(ctx, p, iss, "comment:"+c.ExternalID, c.Author, "an @-mention", c.Body)
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
		// An assignment carries no message body to relay, so note is empty: a
		// re-assignment of an already-active issue just no-ops (there's nothing to
		// route), exactly as before.
		o.triggerIssueObjective(ctx, p, iss, ext, actor, "an assignment", "")
	}
}

// triggerIssueObjective claims the trigger (deduped), creates an objective for
// the issue, and acknowledges it. The claim is recorded before the objective is
// created so a concurrent poll can't double-spawn; if creation fails the claim is
// rolled back so a later poll retries.
func (o *Orchestrator) triggerIssueObjective(ctx context.Context, p *model.Project, iss forge.Issue, externalID, requester, via, note string) {
	inserted, err := o.st.RecordIssueTask(p.Repo, iss.Number, externalID)
	if err != nil || !inserted {
		return // already handled (or store error) — don't act twice
	}
	// Don't open a second objective for an issue that already has an active one.
	// Re-assigning or re-mentioning the bot mints a NEW trigger event (so the
	// claim above is fresh), but if work is already in flight a duplicate
	// objective just contends for the same scarce manager/target slots and muddies
	// the dashboard. The claim stays recorded so this event isn't re-evaluated;
	// once the prior objective is terminal a later re-trigger starts fresh work.
	//
	// But a fresh COMMENT on an in-flight issue is mid-flight guidance ("look at
	// PR #X first", "also handle Windows") — dropping it is how a coworker's
	// pointer silently reached no one. So instead of returning empty-handed, route
	// the comment to the objective's manager. Assignments carry no note and still
	// just no-op here.
	if existing, err := o.st.ActiveObjectiveForIssue(p.Repo, iss.Number); err == nil && existing != "" {
		if strings.TrimSpace(note) != "" {
			o.routeIssueCommentToManager(ctx, p.Repo, iss, existing, requester, note)
		}
		return
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

// routeIssueCommentToManager delivers a new comment on an actively-worked issue
// to the manager driving that issue's objective, so guidance left on the issue
// after work began (a pointer to prior art, a correction, an added constraint)
// reaches the team instead of being dropped. Each comment is claimed once
// (RecordIssueTask, by the caller) before we get here, so a re-poll never
// re-delivers the same one.
//
// If a live manager is driving the objective it is steered in place. If the
// objective has parked with no live manager (e.g. its PR is up and awaiting
// review), a new human comment is an actionable event, so we revive a manager —
// mirroring notifyManagerOfMerge/notifyManagerOfFollowup — with the comment baked
// into its resume prompt so it isn't lost. Revival respects the manager respawn
// budget so a stream of comments can't drive an unbounded respawn loop.
func (o *Orchestrator) routeIssueCommentToManager(ctx context.Context, repo string, iss forge.Issue, objectiveID, requester, body string) {
	msg := fmt.Sprintf(
		"New comment from @%s on issue #%d (%q), which you are working on:\n\n%s\n\n"+
			"Take it into account. If it changes the plan, steer or re-scope your workers "+
			"(message_session) accordingly; if it asks something you can answer, reply via the "+
			"related PR (comment_pr) or just proceed.",
		requester, iss.Number, iss.Title, strings.TrimSpace(body))

	if mgr := o.activeManagerFor(objectiveID); mgr != nil {
		o.audit(objectiveID, mgr.ID, "issue_comment_routed",
			fmt.Sprintf("routed issue #%d comment from @%s to manager", iss.Number, requester),
			model.JSONMap{"repo": repo, "issue": iss.Number, "requester": requester})
		_ = o.Steer(ctx, mgr.ID, msg)
		return
	}

	// No live manager. Only revive for a genuinely ACTIVE objective: a
	// waiting_user one is parked on an open dashboard question and has no clean
	// session to resume into, so record the comment and leave it for the human.
	obj, err := o.st.GetObjective(objectiveID)
	if err != nil || obj.Status != model.ObjectiveActive {
		o.audit(objectiveID, "", "issue_comment_unrouted",
			fmt.Sprintf("issue #%d comment from @%s not routed: no active manager", iss.Number, requester),
			model.JSONMap{"repo": repo, "issue": iss.Number, "requester": requester})
		return
	}
	if o.managerRespawnExhausted(objectiveID) {
		o.audit(objectiveID, "", "issue_comment_unrouted",
			fmt.Sprintf("issue #%d comment from @%s not routed: manager respawn budget exhausted", iss.Number, requester),
			model.JSONMap{"repo": repo, "issue": iss.Number, "requester": requester})
		return
	}
	o.audit(objectiveID, "", "issue_comment_routed",
		fmt.Sprintf("routed issue #%d comment from @%s; reviving manager", iss.Number, requester),
		model.JSONMap{"repo": repo, "issue": iss.Number, "requester": requester})
	o.respawnManager(supervisorAction{
		objectiveID: objectiveID,
		prompt:      obj.Prompt + "\n\n" + msg,
		agent:       o.lastManagerAgent(objectiveID),
	})
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
