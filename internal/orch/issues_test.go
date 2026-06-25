package orch

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/forge"
	"github.com/nathanwhit/orcha/internal/mcp"
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

func TestIssueTrigger_CommentRoutedToActiveManager(t *testing.T) {
	o, f := issueTriggerOrch(t)
	addTarget(t, o.st, "local", model.TargetLocal, 4)
	f.SetIssue("acme/widgets", forge.Issue{
		Number: 20, Title: "Fix the deadlock", Body: "It hangs.",
		URL: "https://github.com/acme/widgets/issues/20",
	})

	// First mention from an allowlisted user spins up the objective + manager.
	first := forge.IssueComment{IssueNumber: 20, ExternalID: "c1", Author: "alice", Body: "@orcha-bot please fix this"}
	f.SetIssueComments(first)
	o.SyncIssueTriggers(context.Background())

	objs := objectivesForIssue(t, o, 20)
	if len(objs) != 1 {
		t.Fatalf("want 1 objective for issue #20, got %d", len(objs))
	}
	mgr := o.activeManagerFor(objs[0].ID)
	if mgr == nil {
		t.Fatal("expected a live manager for the triggered objective")
	}
	_, _ = o.st.UpdateSessionStatus(mgr.ID, model.SessionRunning)

	// A coworker leaves a pointer comment WHILE the issue is being worked. The
	// re-poll lists both comments; c1 is already claimed, c2 is new.
	second := forge.IssueComment{IssueNumber: 20, ExternalID: "c2", Author: "alice",
		Body: "@orcha-bot you might want to look into PR #999 first"}
	f.SetIssueComments(first, second)
	o.SyncIssueTriggers(context.Background())

	// It must NOT spawn a second objective for the same issue...
	if got := objectivesForIssue(t, o, 20); len(got) != 1 {
		t.Fatalf("a comment on an in-flight issue must not duplicate the objective, got %d", len(got))
	}
	// ...and it must NOT post another ack comment (routing is not a new trigger).
	if len(f.IssueComments) != 1 {
		t.Fatalf("routing a comment should not post another ack, got %+v", f.IssueComments)
	}
	// ...the comment is steered to the manager: its body reaches the transcript.
	msgs, _ := o.st.MessagesAfter(mgr.ID, 0, 50)
	var routed string
	for _, m := range msgs {
		if m.Source == model.MsgUser && strings.Contains(m.Content, "PR #999") {
			routed = m.Content
		}
	}
	if routed == "" {
		t.Fatal("the coworker's pointer comment was not routed to the manager")
	}
	if !strings.Contains(routed, "@alice") || !strings.Contains(routed, "#20") {
		t.Fatalf("routed message should attribute the commenter and issue, got %q", routed)
	}

	// Re-polling the same comments delivers nothing new (claimed once).
	o.SyncIssueTriggers(context.Background())
	again, _ := o.st.MessagesAfter(mgr.ID, 0, 50)
	n := 0
	for _, m := range again {
		if m.Source == model.MsgUser && strings.Contains(m.Content, "PR #999") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the same comment should route exactly once, got %d deliveries", n)
	}
}

func TestCommentIssue_PostsToIssueWithMarker(t *testing.T) {
	o, f := issueTriggerOrch(t)
	addTarget(t, o.st, "local", model.TargetLocal, 4)
	f.SetIssue("acme/widgets", forge.Issue{Number: 30, Title: "Bug", Body: "x"})
	f.SetIssueComments(forge.IssueComment{IssueNumber: 30, ExternalID: "c1", Author: "alice", Body: "@orcha-bot fix it"})
	o.SyncIssueTriggers(context.Background())

	objs := objectivesForIssue(t, o, 30)
	if len(objs) != 1 {
		t.Fatalf("want 1 objective for issue #30, got %d", len(objs))
	}
	mgr := o.activeManagerFor(objs[0].ID)
	if mgr == nil {
		t.Fatal("expected a live manager")
	}

	// One ack comment exists from the trigger; the manager now replies on the issue.
	before := len(f.IssueComments)
	ctx := mcp.WithSession(context.Background(), mgr.ID)
	if _, err := o.mcpCommentIssue(ctx, map[string]any{"body": "Thanks — looking into it now."}); err != nil {
		t.Fatalf("comment_issue: %v", err)
	}
	if len(f.IssueComments) != before+1 {
		t.Fatalf("expected one new issue comment, got %d (was %d)", len(f.IssueComments), before)
	}
	last := f.IssueComments[len(f.IssueComments)-1]
	if last.Number != 30 || !strings.Contains(last.Body, "looking into it") {
		t.Fatalf("comment not posted to issue #30: %+v", last)
	}
	// The bot marker must be appended so the mention monitor never re-ingests it.
	if !strings.Contains(last.Body, orchaBotMarker) {
		t.Fatalf("issue comment must carry the bot marker, got %q", last.Body)
	}

	// The reply resolves to the objective's issue with no number argument: the
	// manager can only comment on the issue it is working.
	other, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "manual objective", Prompt: "p"})
	om := &model.Session{ObjectiveID: other.ID, Role: model.RoleManager, Status: model.SessionRunning}
	if err := o.st.CreateSession(om); err != nil {
		t.Fatalf("create session: %v", err)
	}
	octx := mcp.WithSession(context.Background(), om.ID)
	if _, err := o.mcpCommentIssue(octx, map[string]any{"body": "hi"}); err == nil {
		t.Fatal("comment_issue on an objective not tied to an issue must error")
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
