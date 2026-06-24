package orch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/nathanwhit/orcha/internal/mcp"
	"github.com/nathanwhit/orcha/internal/model"
)

// Adversarial review gate.
//
// When a project opts in (Project.ReviewGate, toggled from the dashboard),
// publish_pr will not open a PR until an independent reviewer — running on a
// DIFFERENT provider than the implementer, so a second model cross-examines the
// work — approves the exact diff being shipped. PublishPR auto-spawns that
// reviewer and holds the publish; the reviewer's submit_review verdict either
// auto-opens the PR (approve) or hands the findings back to the manager
// (request_changes). The verdict is keyed to a fingerprint of the diff, so any
// later change to the implementer's work invalidates a stale approval and a
// re-publish triggers a fresh review.

const (
	reviewApprove        = "approve"
	reviewRequestChanges = "request_changes"
)

// reviewGateEnabled reports whether the project for repo has the gate on. A repo
// with no registered project (or any lookup error) is gate-off — the feature is
// strictly opt-in.
func (o *Orchestrator) reviewGateEnabled(repo string) bool {
	if repo == "" {
		return false
	}
	p, err := o.st.GetProjectByRepo(repo)
	if err != nil {
		return false
	}
	return p.ReviewGate
}

// reviewGuidanceFor returns the project's free-text reviewer guidance for repo,
// or "" when there is no project or no guidance set.
func (o *Orchestrator) reviewGuidanceFor(repo string) string {
	if repo == "" {
		return ""
	}
	p, err := o.st.GetProjectByRepo(repo)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(p.ReviewGuidance)
}

// reviewBound reports whether a reviewer session was spawned by the review gate
// (it carries the reviewed session id). Only these get the submit_review surface;
// a reviewer a manager spawns by hand keeps the ordinary worker surface
// (report_result), since it has nothing to bind a verdict to.
func reviewBound(sess *model.Session) bool {
	if sess.Metadata == nil {
		return false
	}
	id, _ := sess.Metadata["reviews_session"].(string)
	return id != ""
}

// diffFingerprint hashes a diff so a verdict can be tied to the exact change it
// reviewed. If the implementer changes anything afterward, the fingerprint moves
// and the prior verdict no longer applies.
func diffFingerprint(diff string) string {
	sum := sha256.Sum256([]byte(diff))
	return hex.EncodeToString(sum[:])
}

// otherProvider returns a registered provider different from kind — the
// cross-provider adversary. With only one provider registered it returns kind (a
// same-provider review, which the caller audits as degraded).
func (o *Orchestrator) otherProvider(kind model.AgentKind) model.AgentKind {
	o.mu.Lock()
	defer o.mu.Unlock()
	for k := range o.providers {
		if k != kind {
			return k
		}
	}
	return kind
}

// activeReviewerFor returns a non-terminal reviewer session already reviewing
// implID at fingerprint fp, or nil — the guard that stops a second publish_pr
// from spawning a duplicate reviewer for the same diff (mirrors activePRFollowup).
func (o *Orchestrator) activeReviewerFor(objectiveID, implID, fp string) *model.Session {
	sessions, err := o.st.ListSessionsByObjective(objectiveID)
	if err != nil {
		return nil
	}
	for _, s := range sessions {
		if s.Role != model.RoleReviewer || s.Status.IsTerminal() {
			continue
		}
		if ri, _ := s.Metadata["reviews_session"].(string); ri != implID {
			continue
		}
		if rf, _ := s.Metadata["review_fingerprint"].(string); rf == fp {
			return s
		}
	}
	return nil
}

// stashPendingPublish records the publish intent on the implementer session so
// the reviewer can replay it (open the PR with the manager's title/body) once it
// approves.
func (o *Orchestrator) stashPendingPublish(sessID string, spec PublishSpec) error {
	_, err := o.st.UpdateSessionRuntime(sessID, func(s *model.Session) {
		if s.Metadata == nil {
			s.Metadata = model.JSONMap{}
		}
		s.Metadata["pending_publish"] = model.JSONMap{
			"title":          spec.Title,
			"body":           spec.Body,
			"commit_message": spec.CommitMessage,
			"base_branch":    spec.BaseBranch,
		}
	})
	return err
}

// pendingPublishSpec reads back a stashed publish intent. Session metadata is
// JSON in the store, so a round-tripped value arrives as map[string]any.
func pendingPublishSpec(s *model.Session) (PublishSpec, bool) {
	raw, ok := s.Metadata["pending_publish"]
	if !ok {
		return PublishSpec{}, false
	}
	m, ok := raw.(map[string]any)
	if !ok {
		// A same-process write (before any DB round-trip) is a model.JSONMap.
		jm, ok2 := raw.(model.JSONMap)
		if !ok2 {
			return PublishSpec{}, false
		}
		m = map[string]any(jm)
	}
	return PublishSpec{
		Title:         asString(m["title"]),
		Body:          asString(m["body"]),
		CommitMessage: asString(m["commit_message"]),
		BaseBranch:    asString(m["base_branch"]),
	}, true
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// firstLine returns the first non-empty line of s, bounded, for compact error
// messages relayed to the manager.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return truncateRunes(t, 200)
		}
	}
	return truncateRunes(strings.TrimSpace(s), 200)
}

// spawnReviewer creates the adversarial reviewer for impl's change: a one-shot
// reviewer on the other provider, with its own fresh checkout (provisioned by
// ensureWorkspace, since RoleReviewer needs an isolated workspace) and the diff
// inlined into its goal. It carries the reviewed session id and the diff
// fingerprint in metadata so submit_review and the gate can correlate the verdict.
func (o *Orchestrator) spawnReviewer(ctx context.Context, impl *model.Session, repo, fp string) (*model.Session, error) {
	reviewer := o.otherProvider(impl.Agent)
	meta := model.JSONMap{
		"reviews_session":    impl.ID,
		"review_fingerprint": fp,
		// Inherit the implementer's ACTUAL checkout (its branch with the change
		// applied) instead of a clean tree with the diff pasted into the prompt. The
		// implementer is terminal by review time, so there is no concurrent writer.
		// This is the fix for three problems at once: the diff no longer bloats the
		// launch command (which blew tmux's length cap and took the fleet down), the
		// reviewer can actually build/test the real change, and any fix it commits
		// lands on the branch the PR ships from — no stranded review commits.
		"depends_on": []string{impl.ID},
	}
	// Inherit the implementer's repo source too (for forge ops / fallback).
	for _, k := range []string{"repo", "push_repo", "clone_url", "base_branch"} {
		if v, ok := impl.Metadata[k].(string); ok && v != "" {
			meta[k] = v
		}
	}
	// Project-specific guidance, if the maintainers set any, is given its own
	// clearly-labelled section so the reviewer weights it heavily.
	guidanceSection := ""
	if g := o.reviewGuidanceFor(repo); g != "" {
		guidanceSection = fmt.Sprintf("Project-specific review guidance from the maintainers — weight this heavily:\n%s\n\n", g)
	}
	// Point the reviewer at the change in its own checkout rather than inlining it.
	diffHint := "Run `git diff` against the base branch (and `git log -p`) to see exactly what changed."
	if ws, err := o.st.GetWorkspace(impl.WorkspaceID); err == nil && ws != nil && ws.BaseSHA != "" {
		diffHint = fmt.Sprintf("Run `git diff %s..HEAD` and `git log -p %s..HEAD` to see exactly what changed.", ws.BaseSHA, ws.BaseSHA)
	}
	goal := fmt.Sprintf(
		"Adversarially review the implementer's change before it ships as a PR.\n\n"+
			"The implementer was given this task:\n%s\n\n"+
			"YOUR CHECKOUT IS THE CHANGE: you are on the implementer's branch with the work "+
			"applied. %s Build and run the relevant tests for real.\n\n"+
			"%s"+
			"Find real problems — bugs, regressions, missed requirements, broken or missing "+
			"tests, security issues — do not rubber-stamp. If you find a clear, contained fix you "+
			"MAY make and commit it on this branch (it will ship with the PR); otherwise report it "+
			"as a finding. When done, call submit_review with verdict %q only if it is genuinely "+
			"ready to ship, or %q with specific, actionable findings (file:line). That tool call is "+
			"how you finish your review.",
		strings.TrimSpace(impl.Goal), diffHint, guidanceSection, reviewApprove, reviewRequestChanges)

	spec := SpawnSpec{
		Role:     model.RoleReviewer,
		Agent:    reviewer,
		Mode:     model.ModeNoninteractive,
		Title:    "Review: " + impl.Title,
		Goal:     goal,
		Metadata: meta,
	}
	var (
		sess *model.Session
		err  error
	)
	if impl.ParentSessionID != "" {
		sess, err = o.SpawnSession(impl.ParentSessionID, spec)
	} else {
		spec.ObjectiveID = impl.ObjectiveID
		sess, err = o.CreateSession(spec)
	}
	if err != nil {
		return nil, err
	}
	o.audit(impl.ObjectiveID, sess.ID, "review_spawned",
		fmt.Sprintf("adversarial review of session %s on provider %s", impl.ID, reviewer),
		model.JSONMap{"reviews_session": impl.ID, "provider": string(reviewer), "same_provider": reviewer == impl.Agent})
	return sess, nil
}

// mcpSubmitReview is the reviewer's submit_review tool: it records the verdict on
// the reviewed session (where the publish gate reads it), then routes — approve
// replays the held publish so the PR opens, request_changes hands the findings to
// the manager and leaves the publish blocked.
func (o *Orchestrator) mcpSubmitReview(ctx context.Context, args map[string]any) (string, error) {
	id := mcp.SessionFromContext(ctx)
	if id == "" {
		return "", fmt.Errorf("no session bound to request")
	}
	reviewer, err := o.st.GetSession(id)
	if err != nil {
		return "", err
	}
	verdict := strings.TrimSpace(mcp.StringArg(args, "verdict"))
	if verdict != reviewApprove && verdict != reviewRequestChanges {
		return "", fmt.Errorf("verdict must be %q or %q", reviewApprove, reviewRequestChanges)
	}
	summary := strings.TrimSpace(mcp.StringArg(args, "summary"))
	if summary == "" {
		return "", fmt.Errorf("summary is required")
	}
	findings := mcp.StringsArg(args, "findings")
	if verdict == reviewRequestChanges && len(findings) == 0 {
		// A rejection with no specifics gives the manager nothing to act on.
		return "", fmt.Errorf("request_changes needs at least one specific finding")
	}

	implID, _ := reviewer.Metadata["reviews_session"].(string)
	fp, _ := reviewer.Metadata["review_fingerprint"].(string)
	if implID == "" {
		return "", fmt.Errorf("this review session is not bound to a reviewed session")
	}
	impl, err := o.st.GetSession(implID)
	if err != nil {
		return "", err
	}
	// The reviewer shares the implementer's checkout and MAY have committed a fix
	// onto the branch. That changes the diff, which would move the fingerprint and
	// make PublishPR reject this very approval as "stale" — spawning an endless
	// re-review. So pin the verdict to the diff AS IT STANDS NOW (post-fix), which
	// is exactly what the publish will ship. Falls back to the spawn-time
	// fingerprint if the checkout can't be read.
	if implWS, werr := o.st.GetWorkspace(impl.WorkspaceID); werr == nil && implWS != nil && implWS.Path != "" {
		if d, derr := o.forgeForWorkspace(implWS).Diff(ctx, implWS.Path); derr == nil {
			fp = diffFingerprint(d)
		}
	}

	findingsText := summary
	if len(findings) > 0 {
		var b strings.Builder
		b.WriteString(summary)
		b.WriteString("\n\nFindings:\n")
		for _, f := range findings {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(f))
		}
		findingsText = strings.TrimSpace(b.String())
	}

	// Record the verdict on the reviewed session so the publish gate can read it.
	if _, err := o.st.UpdateSessionRuntime(implID, func(s *model.Session) {
		if s.Metadata == nil {
			s.Metadata = model.JSONMap{}
		}
		s.Metadata["review_verdict"] = verdict
		s.Metadata["review_fingerprint"] = fp
		s.Metadata["review_findings"] = findingsText
		s.Metadata["reviewed_by"] = reviewer.ID
	}); err != nil {
		return "", err
	}
	_ = o.st.CreateArtifact(&model.Artifact{
		ObjectiveID: reviewer.ObjectiveID,
		SessionID:   reviewer.ID,
		Kind:        model.ArtifactReport,
		Title:       fmt.Sprintf("Review (%s): %s", verdict, impl.Title),
		Summary:     findingsText,
		Visibility:  model.VisibilitySecondary,
	})
	// Make the verdict the reviewer's handoff so it shows in the UI / any
	// completion relay.
	_, _ = o.st.UpdateSessionRuntime(reviewer.ID, func(s *model.Session) {
		s.HandoffSummary = strings.ToUpper(verdict) + ": " + findingsText
	})
	o.audit(reviewer.ObjectiveID, reviewer.ID, "review_submitted", verdict,
		model.JSONMap{"reviews_session": implID})

	if verdict == reviewRequestChanges {
		o.steerManagerOf(impl, fmt.Sprintf(
			"Adversarial review REQUESTED CHANGES on session %s (%q). The PR was NOT opened.\n\n%s\n\n"+
				"Address these (steer the implementer with message_session, or spawn a fix), then call publish_pr again.",
			impl.ID, impl.Title, findingsText))
		return "changes requested; the manager has your findings and the PR is held.", nil
	}

	// Approved: replay the held publish so the PR opens now.
	spec, ok := pendingPublishSpec(impl)
	if !ok {
		o.steerManagerOf(impl, fmt.Sprintf(
			"Adversarial review APPROVED session %s (%q). Call publish_pr to ship it.", impl.ID, impl.Title))
		return "approved; the manager has been told it is clear to publish.", nil
	}
	pr, err := o.PublishPR(ctx, impl.ID, spec)
	if err != nil {
		o.steerManagerOf(impl, fmt.Sprintf(
			"Adversarial review APPROVED session %s (%q), but auto-publish failed: %v. Call publish_pr to retry.",
			impl.ID, impl.Title, err))
		return "approved, but auto-publish failed: " + err.Error(), nil
	}
	o.steerManagerOf(impl, fmt.Sprintf(
		"Adversarial review APPROVED session %s and opened PR #%d: %s", impl.ID, pr.Number, pr.URL))
	return fmt.Sprintf("approved; opened PR #%d.", pr.Number), nil
}

// steerManagerOf delivers a message to the live manager that owns impl.
func (o *Orchestrator) steerManagerOf(impl *model.Session, msg string) {
	if impl == nil || impl.ParentSessionID == "" {
		return
	}
	mgr, err := o.st.GetSession(impl.ParentSessionID)
	if err != nil || mgr.Role != model.RoleManager || mgr.Status.IsTerminal() {
		return
	}
	_ = o.Steer(context.Background(), mgr.ID, msg)
}

// notifyManagerOfReview handles a finished reviewer session. On the happy path
// submit_review already steered the manager, so this only fires when the reviewer
// ended WITHOUT recording a verdict for this run (it crashed or finished without
// submitting) — the manager needs to know the review didn't land so it can retry.
func (o *Orchestrator) notifyManagerOfReview(reviewer, mgr *model.Session, success bool) {
	implID, _ := reviewer.Metadata["reviews_session"].(string)
	impl, _ := o.st.GetSession(implID)
	if success && impl != nil {
		if rb, _ := impl.Metadata["reviewed_by"].(string); rb == reviewer.ID {
			return // submit_review already handled the notification
		}
	}
	title := ""
	if impl != nil {
		title = impl.Title
	}
	o.audit(reviewer.ObjectiveID, mgr.ID, "review_incomplete",
		"reviewer ended without a verdict", model.JSONMap{"reviewer": reviewer.ID, "reviews_session": implID})
	_ = o.Steer(context.Background(), mgr.ID, fmt.Sprintf(
		"The adversarial review of session %s (%q) ended without a verdict. Call publish_pr again to start a fresh review.",
		implID, title))
}
