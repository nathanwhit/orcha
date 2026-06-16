package orch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nathanwhit/orcha/internal/forge"
)

// The operator surface must let an outside agent create a repo-less objective
// and then observe it — the "chat about tasks and see state" loop.
func TestOperatorMCP_CreateListGet(t *testing.T) {
	o, _ := newTestOrch(t)
	o.SetForge(forge.NewFake())

	out, err := o.mcpCreateObjective(context.Background(), map[string]any{
		"title":  "Investigate slow build",
		"prompt": "find out why the build is slow and report back, no code changes",
	})
	if err != nil {
		t.Fatalf("create_objective: %v", err)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("create result not JSON: %v (%s)", err, out)
	}
	id, _ := created["objective_id"].(string)
	if id == "" {
		t.Fatalf("create returned no objective_id: %s", out)
	}
	if created["manager_session_id"] == "" {
		t.Fatalf("create returned no manager_session_id: %s", out)
	}

	list, err := o.mcpListObjectives(context.Background(), nil)
	if err != nil {
		t.Fatalf("list_objectives: %v", err)
	}
	if !strings.Contains(list, id) {
		t.Fatalf("new objective %s not in list: %s", id, list)
	}

	got, err := o.mcpGetObjective(context.Background(), map[string]any{"objective_id": id})
	if err != nil {
		t.Fatalf("get_objective: %v", err)
	}
	if !strings.Contains(got, "Investigate slow build") {
		t.Fatalf("get_objective missing title: %s", got)
	}
	// The detail payload carries the structured sections an operator agent reads.
	for _, key := range []string{"sessions", "pull_requests", "questions", "artifacts"} {
		if !strings.Contains(got, key) {
			t.Fatalf("get_objective missing %q section: %s", key, got)
		}
	}
}

func TestOperatorMCP_CreateRequiresTitleAndPrompt(t *testing.T) {
	o, _ := newTestOrch(t)
	o.SetForge(forge.NewFake())

	if _, err := o.mcpCreateObjective(context.Background(), map[string]any{"title": "x"}); err == nil {
		t.Fatal("expected an error when prompt is missing")
	}
	if _, err := o.mcpCreateObjective(context.Background(), map[string]any{"prompt": "y"}); err == nil {
		t.Fatal("expected an error when title is missing")
	}
}

// Empty stores must render as JSON arrays, not null, so an agent can iterate.
func TestOperatorMCP_EmptyListsAreArrays(t *testing.T) {
	o, _ := newTestOrch(t)
	o.SetForge(forge.NewFake())

	if got, err := o.mcpListObjectives(context.Background(), nil); err != nil || got != "[]" {
		t.Fatalf("list_objectives empty = %q, %v; want \"[]\"", got, err)
	}
	if got, err := o.mcpListOpenQuestions(context.Background(), nil); err != nil || got != "[]" {
		t.Fatalf("list_open_questions empty = %q, %v; want \"[]\"", got, err)
	}
}
