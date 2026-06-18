package orch

import (
	"testing"

	"github.com/nathanwhit/orcha/internal/model"
)

// TestResolveRepo_InheritsForkForSameRepoWorker is the regression for a
// validator that overrode only base_branch (to build on another worker's
// fork-hosted branch) and redundantly repeated the objective's repo. The repeat
// must NOT drop the objective's push_repo (the fork) — otherwise the checkout
// looks for the base on origin and fails.
func TestResolveRepo_InheritsForkForSameRepoWorker(t *testing.T) {
	o, st := newTestOrch(t)
	obj := &model.Objective{
		Title: "o", Prompt: "p", Status: model.ObjectiveActive,
		Metadata: model.JSONMap{
			"repo":        "denoland/deno",
			"push_repo":   "nathanwhitbot/deno",
			"base_branch": "main",
		},
	}
	if err := st.CreateObjective(obj); err != nil {
		t.Fatalf("objective: %v", err)
	}

	// Validator: same repo, base overridden to the impl's fork-hosted branch,
	// no push_repo of its own.
	sess := &model.Session{
		ObjectiveID: obj.ID,
		Metadata: model.JSONMap{
			"repo":        "denoland/deno",
			"base_branch": "orcha/impl-9cfc5009",
		},
	}
	repo, pushRepo, _, base := o.resolveRepo(sess)
	if repo != "denoland/deno" {
		t.Errorf("repo = %q, want denoland/deno", repo)
	}
	if pushRepo != "nathanwhitbot/deno" {
		t.Errorf("pushRepo = %q, want the objective's fork nathanwhitbot/deno", pushRepo)
	}
	if base != "orcha/impl-9cfc5009" {
		t.Errorf("base = %q, want the session's override", base)
	}
}

// TestResolveRepo_DoesNotApplyForkToDifferentRepo: when a worker checks out a
// DIFFERENT repo than the objective's, the objective's fork must not bleed in.
func TestResolveRepo_DoesNotApplyForkToDifferentRepo(t *testing.T) {
	o, st := newTestOrch(t)
	obj := &model.Objective{
		Title: "o", Prompt: "p", Status: model.ObjectiveActive,
		Metadata: model.JSONMap{"repo": "denoland/deno", "push_repo": "nathanwhitbot/deno"},
	}
	if err := st.CreateObjective(obj); err != nil {
		t.Fatalf("objective: %v", err)
	}
	sess := &model.Session{
		ObjectiveID: obj.ID,
		Metadata:    model.JSONMap{"repo": "someone/other"},
	}
	_, pushRepo, _, _ := o.resolveRepo(sess)
	if pushRepo != "" {
		t.Errorf("pushRepo = %q, want empty (objective fork must not apply to a different repo)", pushRepo)
	}
}
