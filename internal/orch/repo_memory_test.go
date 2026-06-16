package orch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/workspace"
)

func TestRepoMemoryKey(t *testing.T) {
	cases := []struct{ repo, cloneURL, want string }{
		{"owner/Repo", "", "owner/repo"},
		{"", "https://github.com/Owner/Repo.git", "owner/repo"},
		{"", "git@github.com:owner/repo.git", "owner/repo"},
		{"OWNER/REPO", "ignored", "owner/repo"},
		{"", "/tmp/local/bare.git", "/tmp/local/bare"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := repoMemoryKey(c.repo, c.cloneURL); got != c.want {
			t.Errorf("repoMemoryKey(%q,%q) = %q, want %q", c.repo, c.cloneURL, got, c.want)
		}
	}
}

func TestSafeRel(t *testing.T) {
	for _, ok := range []string{"MEMORY.md", "gotchas.md", "topics/build.md"} {
		if !safeRel(ok) {
			t.Errorf("safeRel(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "../escape.md", "a/../../b"} {
		if safeRel(bad) {
			t.Errorf("safeRel(%q) = true, want false", bad)
		}
	}
}

// seedRepoTarget stands up a real bare "remote" with one commit on main and a
// writable work root, the way TestOrch_PreparesRealWorkspace does.
func seedRepoTarget(t *testing.T, o *Orchestrator, st interface {
	CreateTarget(*model.Target) error
}) (*model.Target, string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	tgit(t, root, "init", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	tgit(t, root, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, seed, "add", ".")
	tgit(t, seed, "commit", "-m", "initial")
	tgit(t, seed, "remote", "add", "origin", bare)
	tgit(t, seed, "push", "-u", "origin", "main")

	tgt := &model.Target{Name: "local", Kind: model.TargetLocal, Status: model.TargetOnline,
		WorkRoot: filepath.Join(root, "work"), CapacitySessions: 4}
	if err := st.CreateTarget(tgt); err != nil {
		t.Fatalf("target: %v", err)
	}
	return tgt, bare
}

// newMemSession prepares a fresh isolated checkout for a new implementer on the
// repo and seeds its memory, returning the session and workspace.
func newMemSession(t *testing.T, o *Orchestrator, tgt *model.Target, bare, objID string) (*model.Session, *model.Workspace) {
	t.Helper()
	s, _ := o.CreateSession(SpawnSpec{ObjectiveID: objID, Role: model.RoleImplementer, Agent: model.AgentClaude})
	w, err := o.PrepareIsolatedWorkspace(context.Background(), s.ID, "owner/repo", bare, "main")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	s, _ = o.st.GetSession(s.ID)
	o.seedRepoMemory(context.Background(), s, w, tgt)
	return s, w
}

func memFile(wsPath, rel string) string { return filepath.Join(wsPath, repoMemoryDir, rel) }

func storedMemory(t *testing.T, o *Orchestrator, key string) map[string]string {
	t.Helper()
	rows, err := o.st.ListRepoMemoryFiles(key)
	if err != nil {
		t.Fatalf("list repo memory: %v", err)
	}
	m := map[string]string{}
	for _, f := range rows {
		m[f.Path] = f.Content
	}
	return m
}

// The full loop: seed a checkout with the index, let an agent edit it and add a
// topic file, merge both back into the store, then confirm a later checkout is
// seeded with the learned files.
func TestRepoMemory_SeedEditMergeRoundTrip(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetWorkspacePreparer(workspace.New())
	tgt, bare := seedRepoTarget(t, o, st)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p", Repo: "owner/repo"})
	sess, ws := newMemSession(t, o, tgt, bare, obj.ID)

	// A brand-new repo is seeded with the index scaffold, and .orcha/ is excluded.
	got, err := os.ReadFile(memFile(ws.Path, repoMemoryIndex))
	if err != nil || !strings.Contains(string(got), "# Project memory") {
		t.Fatalf("seeded index missing/wrong: %q err=%v", got, err)
	}
	excl, _ := os.ReadFile(filepath.Join(ws.Path, gitLocalExclude))
	if !strings.Contains(string(excl), repoMemoryRoot+"/") {
		t.Fatalf(".orcha/ not excluded from git: %q", excl)
	}
	if out := tgit(t, ws.Path, "status", "--porcelain"); out != "" {
		t.Fatalf("memory leaked into git status: %q", out)
	}

	// Agent edits the index and creates a topic file, then we merge on success.
	if err := os.WriteFile(memFile(ws.Path, repoMemoryIndex),
		append(got, []byte("\n- [gotchas](gotchas.md)\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memFile(ws.Path, "gotchas.md"), []byte("Build needs CGO_ENABLED=1.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.mergeBackRepoMemory(sess)

	stored := storedMemory(t, o, "owner/repo")
	if !strings.Contains(stored[repoMemoryIndex], "gotchas.md") {
		t.Fatalf("index edit not stored: %q", stored[repoMemoryIndex])
	}
	if !strings.Contains(stored["gotchas.md"], "CGO_ENABLED=1") {
		t.Fatalf("new topic file not stored: %q", stored["gotchas.md"])
	}

	// A later session on the same repo is seeded with both learned files.
	_, ws2 := newMemSession(t, o, tgt, bare, obj.ID)
	g2, _ := os.ReadFile(memFile(ws2.Path, "gotchas.md"))
	if !strings.Contains(string(g2), "CGO_ENABLED=1") {
		t.Fatalf("second checkout missing learned topic file: %q", g2)
	}
}

// Memory grows unbounded, so the write path must handle a file far larger than
// an env var or argv could hold (~128KB/ARG_MAX). Round-trip ~1MB to prove the
// stdin pipe has no such cliff.
func TestRepoMemory_LargeContentRoundTrips(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetWorkspacePreparer(workspace.New())
	tgt, bare := seedRepoTarget(t, o, st)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p", Repo: "owner/repo"})
	sess, ws := newMemSession(t, o, tgt, bare, obj.ID)

	var big strings.Builder
	for i := 0; i < 20000; i++ {
		big.WriteString("note line with detail about the codebase ")
		big.WriteString(strings.Repeat("x", 8))
		big.WriteByte('\n')
	}
	want := big.String()
	if len(want) < 600_000 {
		t.Fatalf("test content too small to exercise the limit: %d bytes", len(want))
	}
	if err := os.WriteFile(memFile(ws.Path, "architecture.md"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	o.mergeBackRepoMemory(sess)

	if stored := storedMemory(t, o, "owner/repo")["architecture.md"]; strings.TrimSpace(stored) != strings.TrimSpace(want) {
		t.Fatalf("large memory did not round-trip: stored %d bytes, want %d", len(stored), len(want))
	}
}

// An agent that never touches the memory leaves the store empty — the seeded
// scaffold is not mistaken for a learned update.
func TestRepoMemory_NoEditStoresNothing(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetWorkspacePreparer(workspace.New())
	tgt, bare := seedRepoTarget(t, o, st)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p", Repo: "owner/repo"})
	sess, _ := newMemSession(t, o, tgt, bare, obj.ID)
	o.mergeBackRepoMemory(sess)

	if stored := storedMemory(t, o, "owner/repo"); len(stored) != 0 {
		t.Fatalf("unedited scaffold was stored: %v", stored)
	}
}

// Two sessions that each add a DIFFERENT topic file merge without any conflict —
// the whole point of per-file granularity.
func TestRepoMemory_ConcurrentAddsNoConflict(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetWorkspacePreparer(workspace.New())
	tgt, bare := seedRepoTarget(t, o, st)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p", Repo: "owner/repo"})
	s1, w1 := newMemSession(t, o, tgt, bare, obj.ID)
	s2, w2 := newMemSession(t, o, tgt, bare, obj.ID)

	if err := os.WriteFile(memFile(w1.Path, "build.md"), []byte("From worker 1.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memFile(w2.Path, "deploy.md"), []byte("From worker 2.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.mergeBackRepoMemory(s1)
	o.mergeBackRepoMemory(s2)

	stored := storedMemory(t, o, "owner/repo")
	if !strings.Contains(stored["build.md"], "From worker 1.") || !strings.Contains(stored["deploy.md"], "From worker 2.") {
		t.Fatalf("per-file adds lost an edit: %v", stored)
	}
	for _, c := range stored {
		if strings.Contains(c, "<<<<<<<") {
			t.Fatalf("unexpected conflict markers for non-overlapping adds: %v", stored)
		}
	}
}

// Two sessions editing the SAME file fall back to a 3-way merge that keeps both
// additions rather than clobbering.
func TestRepoMemory_ConcurrentSameFileMerges(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetWorkspacePreparer(workspace.New())
	tgt, bare := seedRepoTarget(t, o, st)

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p", Repo: "owner/repo"})
	s1, w1 := newMemSession(t, o, tgt, bare, obj.ID)
	s2, w2 := newMemSession(t, o, tgt, bare, obj.ID)

	idx1, _ := os.ReadFile(memFile(w1.Path, repoMemoryIndex))
	if err := os.WriteFile(memFile(w1.Path, repoMemoryIndex), append(idx1, []byte("\nFrom worker 1.\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	idx2, _ := os.ReadFile(memFile(w2.Path, repoMemoryIndex))
	if err := os.WriteFile(memFile(w2.Path, repoMemoryIndex), append([]byte("From worker 2.\n"), idx2...), 0o644); err != nil {
		t.Fatal(err)
	}
	o.mergeBackRepoMemory(s1)
	o.mergeBackRepoMemory(s2)

	got := storedMemory(t, o, "owner/repo")[repoMemoryIndex]
	if !strings.Contains(got, "From worker 1.") || !strings.Contains(got, "From worker 2.") {
		t.Fatalf("3-way merge of same file lost an edit: %q", got)
	}
}

// An agent that deletes a seeded file removes it from the store (uncontended).
func TestRepoMemory_DeletePropagates(t *testing.T) {
	o, st := newTestOrch(t)
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetWorkspacePreparer(workspace.New())
	tgt, bare := seedRepoTarget(t, o, st)
	if err := o.st.UpsertRepoMemoryFile("owner/repo", "stale.md", "outdated note\n"); err != nil {
		t.Fatal(err)
	}

	obj, _, _ := o.CreateObjective(NewObjectiveSpec{Title: "x", Prompt: "p", Repo: "owner/repo"})
	sess, ws := newMemSession(t, o, tgt, bare, obj.ID)

	// It was seeded; the agent deletes it.
	if err := os.Remove(memFile(ws.Path, "stale.md")); err != nil {
		t.Fatalf("remove seeded file: %v", err)
	}
	o.mergeBackRepoMemory(sess)

	if _, ok := storedMemory(t, o, "owner/repo")["stale.md"]; ok {
		t.Fatal("deleted file was not removed from the store")
	}
}
