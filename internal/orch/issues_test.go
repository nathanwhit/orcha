package orch

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/forge"
	"github.com/nathanwhit/orcha/internal/model"
)

// issueTriggerOrch builds an orchestrator wired with a fake forge and a fake
// provider, the issue trigger enabled (bot @orcha-bot, allowlist {alice}), and a
// registered project for repo "acme/widgets".
func issueTriggerOrch(t *testing.T) (*Orchestrator, *forge.Fake) {
	t.Helper()
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	f := forge.NewFake()
	o.SetForge(f)
	o.cfg.IssueTriggers = IssueTriggerConfig{BotLogin: "orcha-bot", AllowedLogins: []string{"alice"}}
	if err := st.UpsertProject(&model.Project{Repo: "acme/widgets", BaseBranch: "main"}); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	return o, f
}

func objectivesForIssue(t *testing.T, o *Orchestrator, number int) []*model.Objective {
	t.Helper()
	objs, err := o.st.ListObjectives()
	if err != nil {
		t.Fatalf("list objectives: %v", err)
	}
	var out []*model.Objective
	for _, obj := range objs {
		if strings.Contains(obj.Title, fmt.Sprintf("Issue #%d", number)) {
			out = append(out, obj)
		}
	}
	return out
}

func TestIssueTrigger_MentionByAllowedUserCreatesObjective(t *testing.T) {
	o, f := issueTriggerOrch(t)
	f.SetIssue("acme/widgets", forge.Issue{
		Number: 5, Title: "Cache is stale", Body: "Bust it on write.", URL: "https://github.com/acme/widgets/issues/5",
	})
	f.SetIssueComments(forge.IssueComment{
		IssueNumber: 5, ExternalID: "https://github.com/acme/widgets/issues/5#issuecomment-1",
		Author: "alice", Body: "hey @orcha-bot please work on this",
	})

	o.SyncIssueTriggers(context.Background())

	objs := objectivesForIssue(t, o, 5)
	if len(objs) != 1 {
		t.Fatalf("want 1 objective for issue #5, got %d", len(objs))
	}
	if objs[0].Metadata["repo"] != "acme/widgets" {
		t.Fatalf("objective repo = %v, want acme/widgets", objs[0].Metadata["repo"])
	}
	// Acknowledged on the issue with the bot marker (so it won't re-trigger).
	if len(f.IssueComments) != 1 || f.IssueComments[0].Number != 5 ||
		!strings.Contains(f.IssueComments[0].Body, orchaBotMarker) {
		t.Fatalf("expected one marked ack comment on #5, got %+v", f.IssueComments)
	}

	// Re-poll: the same mention must not spawn a second objective.
	o.SyncIssueTriggers(context.Background())
	if objs := objectivesForIssue(t, o, 5); len(objs) != 1 {
		t.Fatalf("dedup failed: %d objectives after second poll", len(objs))
	}
}

func TestIssueTrigger_MentionByDisallowedUserIgnored(t *testing.T) {
	o, f := issueTriggerOrch(t)
	f.SetIssue("acme/widgets", forge.Issue{Number: 6, Title: "Do a thing"})
	f.SetIssueComments(forge.IssueComment{
		IssueNumber: 6, ExternalID: "c-mallory", Author: "mallory",
		Body: "@orcha-bot work on this",
	})

	o.SyncIssueTriggers(context.Background())

	if objs := objectivesForIssue(t, o, 6); len(objs) != 0 {
		t.Fatalf("mention by non-allowlisted user should be ignored, got %d objectives", len(objs))
	}
	if len(f.IssueComments) != 0 {
		t.Fatalf("should not ack a disallowed mention, got %+v", f.IssueComments)
	}
}

func TestIssueTrigger_NoMentionIgnored(t *testing.T) {
	o, f := issueTriggerOrch(t)
	f.SetIssue("acme/widgets", forge.Issue{Number: 7, Title: "Chatter"})
	f.SetIssueComments(forge.IssueComment{
		IssueNumber: 7, ExternalID: "c1", Author: "alice",
		Body: "I think the orcha-bot approach is nice but no mention here",
	})

	o.SyncIssueTriggers(context.Background())

	if objs := objectivesForIssue(t, o, 7); len(objs) != 0 {
		t.Fatalf("a comment without an @-mention should not trigger, got %d", len(objs))
	}
}

func TestIssueTrigger_PRCommentIgnored(t *testing.T) {
	o, f := issueTriggerOrch(t)
	f.SetIssue("acme/widgets", forge.Issue{Number: 8, Title: "On a PR"})
	f.SetIssueComments(forge.IssueComment{
		IssueNumber: 8, ExternalID: "c1", Author: "alice", IsPR: true,
		Body: "@orcha-bot work on this",
	})

	o.SyncIssueTriggers(context.Background())

	if objs := objectivesForIssue(t, o, 8); len(objs) != 0 {
		t.Fatalf("a PR-conversation comment must not create an issue objective, got %d", len(objs))
	}
}

func TestIssueTrigger_AssignmentByAllowedActor(t *testing.T) {
	o, f := issueTriggerOrch(t)
	f.SetAssignedIssues("acme/widgets", forge.Issue{
		Number: 9, Title: "Assigned work", Body: "Please do it.",
		Assignees: []string{"orcha-bot"},
	})
	f.SetAssignment("acme/widgets", 9, "alice", "evt-1")

	o.SyncIssueTriggers(context.Background())

	if objs := objectivesForIssue(t, o, 9); len(objs) != 1 {
		t.Fatalf("want 1 objective from assignment, got %d", len(objs))
	}
	// Dedup across re-poll: same assignment event must not re-spawn.
	o.SyncIssueTriggers(context.Background())
	if objs := objectivesForIssue(t, o, 9); len(objs) != 1 {
		t.Fatalf("assignment dedup failed: %d objectives", len(objs))
	}
}

func TestIssueTrigger_ReassignmentSkipsWhileActiveRefiresAfterTerminal(t *testing.T) {
	o, f := issueTriggerOrch(t)
	f.SetAssignedIssues("acme/widgets", forge.Issue{Number: 12, Title: "Redo", Assignees: []string{"orcha-bot"}})
	f.SetAssignment("acme/widgets", 12, "alice", "evt-1")

	o.SyncIssueTriggers(context.Background())
	objs := objectivesForIssue(t, o, 12)
	if len(objs) != 1 {
		t.Fatalf("first assignment: want 1 objective, got %d", len(objs))
	}

	// Re-assign (new event id) WHILE the first objective is still active: this must
	// NOT spawn a duplicate, even though the trigger event itself is new.
	f.SetAssignment("acme/widgets", 12, "alice", "evt-2")
	o.SyncIssueTriggers(context.Background())
	if got := objectivesForIssue(t, o, 12); len(got) != 1 {
		t.Fatalf("re-assignment while active should not duplicate: want 1 objective, got %d", len(got))
	}

	// Once the first objective is terminal, a fresh trigger event re-fires.
	if err := o.CancelObjective(objs[0].ID, "test"); err != nil {
		t.Fatalf("cancel objective: %v", err)
	}
	f.SetAssignment("acme/widgets", 12, "alice", "evt-3")
	o.SyncIssueTriggers(context.Background())
	if got := objectivesForIssue(t, o, 12); len(got) != 2 {
		t.Fatalf("re-trigger after the prior objective ended should spawn fresh: want 2, got %d", len(got))
	}
}

func TestIssueTrigger_AssignmentByDisallowedActorIgnored(t *testing.T) {
	o, f := issueTriggerOrch(t)
	f.SetAssignedIssues("acme/widgets", forge.Issue{Number: 10, Title: "Sneaky", Assignees: []string{"orcha-bot"}})
	f.SetAssignment("acme/widgets", 10, "mallory", "evt-1")

	o.SyncIssueTriggers(context.Background())

	if objs := objectivesForIssue(t, o, 10); len(objs) != 0 {
		t.Fatalf("assignment by non-allowlisted actor should be ignored, got %d", len(objs))
	}
}

func TestIssueTrigger_DisabledWithoutAllowlist(t *testing.T) {
	o, f := issueTriggerOrch(t)
	o.cfg.IssueTriggers.AllowedLogins = nil // fail-closed
	f.SetIssue("acme/widgets", forge.Issue{Number: 11, Title: "x"})
	f.SetIssueComments(forge.IssueComment{IssueNumber: 11, ExternalID: "c1", Author: "alice", Body: "@orcha-bot go"})

	if o.IssueTriggersEnabled() {
		t.Fatal("trigger should be disabled with an empty allowlist")
	}
	o.SyncIssueTriggers(context.Background())
	if objs := objectivesForIssue(t, o, 11); len(objs) != 0 {
		t.Fatalf("disabled trigger created %d objectives", len(objs))
	}
}

func TestMentionsLogin(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"@orcha-bot work on this", true},
		{"hey @Orcha-Bot please", true},      // case-insensitive
		{"text @orcha-bot, then more", true}, // trailing punctuation is a boundary
		{"no mention here", false},
		{"email me at bob@orcha-bot.com", false}, // not a whole-token mention
		{"@orcha-botanist is someone else", false},
		{"ping @orcha-bot.", true},
	}
	for _, c := range cases {
		if got := mentionsLogin(c.body, "orcha-bot"); got != c.want {
			t.Errorf("mentionsLogin(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}
