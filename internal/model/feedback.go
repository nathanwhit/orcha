package model

import "time"

// FeedbackKind classifies a PR feedback event detected by the monitor.
type FeedbackKind string

const (
	FeedbackIssueComment  FeedbackKind = "issue_comment"
	FeedbackReviewComment FeedbackKind = "review_comment"
	FeedbackChangesReq    FeedbackKind = "changes_requested"
	FeedbackCheckFailure  FeedbackKind = "check_failure"
	FeedbackConflict      FeedbackKind = "merge_conflict"
	FeedbackMerged        FeedbackKind = "merged"
	FeedbackClosed        FeedbackKind = "closed"
)

// PRFeedback is an actionable (or not) event observed on a pull request. The
// monitor records these; actionable, unhandled items spawn follow-up sessions.
type PRFeedback struct {
	ID         string       `json:"id"`
	PRID       string       `json:"pr_id"`
	Kind       FeedbackKind `json:"kind"`
	ExternalID string       `json:"external_id,omitempty"`
	Body       string       `json:"body,omitempty"`
	Actionable bool         `json:"actionable"`
	Handled    bool         `json:"handled"`
	SessionID  string       `json:"session_id,omitempty"`
	CreatedAt  time.Time    `json:"created_at"`
	Metadata   JSONMap      `json:"metadata,omitempty"`
}
