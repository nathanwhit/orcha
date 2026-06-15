// Package model defines the core domain types for the agent team orchestrator.
//
// The conceptual model is intentionally small: objectives own sessions,
// targets, workspaces, PRs, questions, and artifacts. Everything visible to
// the user maps onto one of these concepts.
package model

import "time"

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

// ObjectiveStatus is the lifecycle of a user-level goal.
type ObjectiveStatus string

const (
	ObjectiveActive      ObjectiveStatus = "active"
	ObjectiveWaitingUser ObjectiveStatus = "waiting_user"
	ObjectiveSucceeded   ObjectiveStatus = "succeeded"
	ObjectiveFailed      ObjectiveStatus = "failed"
	ObjectiveCanceled    ObjectiveStatus = "canceled"
)

// SessionRole describes what a session is for.
type SessionRole string

const (
	RoleManager     SessionRole = "manager"
	RoleImplementer SessionRole = "implementer"
	RoleReviewer    SessionRole = "reviewer"
	RoleValidator   SessionRole = "validator"
	RolePRFollowup  SessionRole = "pr_followup"
	RoleCIFollowup  SessionRole = "ci_followup"
	RoleResearcher  SessionRole = "researcher"
	RoleCustom      SessionRole = "custom"
)

// AgentKind identifies the backing agent provider.
type AgentKind string

const (
	AgentCodex  AgentKind = "codex"
	AgentClaude AgentKind = "claude"
	AgentOther  AgentKind = "other"
)

// SessionMode is whether the session can be steered live.
type SessionMode string

const (
	ModeInteractive    SessionMode = "interactive"
	ModeNoninteractive SessionMode = "noninteractive"
)

// SessionStatus is the lifecycle of an agent session.
type SessionStatus string

const (
	SessionQueued          SessionStatus = "queued"
	SessionStarting        SessionStatus = "starting"
	SessionRunning         SessionStatus = "running"
	SessionWaitingUser     SessionStatus = "waiting_user"
	SessionWaitingCapacity SessionStatus = "waiting_capacity"
	SessionSucceeded       SessionStatus = "succeeded"
	SessionFailed          SessionStatus = "failed"
	SessionCanceled        SessionStatus = "canceled"
)

// TargetKind is local or a remote SSH machine.
type TargetKind string

const (
	TargetLocal TargetKind = "local"
	TargetSSH   TargetKind = "ssh"
)

// TargetStatus controls scheduling onto a target.
type TargetStatus string

const (
	TargetOnline   TargetStatus = "online"
	TargetOffline  TargetStatus = "offline"
	TargetDraining TargetStatus = "draining"
	TargetDisabled TargetStatus = "disabled"
)

// WorkspaceKind describes how a workspace is used.
type WorkspaceKind string

const (
	WorkspaceIsolated WorkspaceKind = "isolated"
	WorkspaceShared   WorkspaceKind = "shared"
	WorkspacePRBranch WorkspaceKind = "pr_branch"
)

// VCS is the version control system backing a workspace.
type VCS string

const (
	VCSGit  VCS = "git"
	VCSJJ   VCS = "jj"
	VCSNone VCS = "none"
)

// WorkspaceStatus is the lifecycle of a workspace.
type WorkspaceStatus string

const (
	WorkspacePreparing WorkspaceStatus = "preparing"
	WorkspaceReady     WorkspaceStatus = "ready"
	WorkspaceDirty     WorkspaceStatus = "dirty"
	WorkspaceArchived  WorkspaceStatus = "archived"
	WorkspaceFailed    WorkspaceStatus = "failed"
)

// PRStatus is the lifecycle of a pull request.
type PRStatus string

const (
	PRDraft  PRStatus = "draft"
	PROpen   PRStatus = "open"
	PRMerged PRStatus = "merged"
	PRClosed PRStatus = "closed"
)

// ChecksState is the aggregate CI state of a PR.
type ChecksState string

const (
	ChecksUnknown ChecksState = "unknown"
	ChecksPending ChecksState = "pending"
	ChecksPassing ChecksState = "passing"
	ChecksFailing ChecksState = "failing"
)

// MessageSource is who produced a transcript row.
type MessageSource string

const (
	MsgUser   MessageSource = "user"
	MsgAgent  MessageSource = "agent"
	MsgSystem MessageSource = "system"
	MsgTool   MessageSource = "tool"
	MsgStdout MessageSource = "stdout"
	MsgStderr MessageSource = "stderr"
)

// MessageKind classifies a transcript row.
type MessageKind string

const (
	KindText       MessageKind = "text"
	KindToolCall   MessageKind = "tool_call"
	KindToolResult MessageKind = "tool_result"
	KindStatus     MessageKind = "status"
	KindError      MessageKind = "error"
	KindUsage      MessageKind = "usage"
)

// QuestionStatus is the lifecycle of a user question.
type QuestionStatus string

const (
	QuestionOpen     QuestionStatus = "open"
	QuestionAnswered QuestionStatus = "answered"
	QuestionCanceled QuestionStatus = "canceled"
)

// ArtifactKind classifies a durable output.
type ArtifactKind string

const (
	ArtifactPullRequest     ArtifactKind = "pull_request"
	ArtifactPatch           ArtifactKind = "patch"
	ArtifactBenchmarkResult ArtifactKind = "benchmark_result"
	ArtifactReport          ArtifactKind = "report"
	ArtifactFile            ArtifactKind = "file"
	ArtifactBuildOutput     ArtifactKind = "build_output"
	ArtifactLogRef          ArtifactKind = "log_ref"
	ArtifactNote            ArtifactKind = "note"
	ArtifactOther           ArtifactKind = "other"
)

// Visibility ranks how prominently an artifact should be shown.
type Visibility string

const (
	VisibilityPrimary   Visibility = "primary"
	VisibilitySecondary Visibility = "secondary"
	VisibilityDebug     Visibility = "debug"
)

// UsageState is the health of a provider's usage window.
type UsageState string

const (
	UsageOK          UsageState = "ok"
	UsageConstrained UsageState = "constrained"
	UsageExhausted   UsageState = "exhausted"
	UsageUnknown     UsageState = "unknown"
)

// LockKind enumerates the scheduling locks.
type LockKind string

const (
	LockWorkspace        LockKind = "workspace"
	LockPRBranch         LockKind = "pr_branch"
	LockObjectiveManager LockKind = "objective_manager"
	LockTargetSlot       LockKind = "target_slot"
)

// ---------------------------------------------------------------------------
// Entities
// ---------------------------------------------------------------------------

// Objective is a user-level goal owning all downstream work.
type Objective struct {
	ID               string          `json:"id"`
	Title            string          `json:"title"`
	Prompt           string          `json:"prompt"`
	Status           ObjectiveStatus `json:"status"`
	ManagerSessionID string          `json:"manager_session_id,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	CompletedAt      *time.Time      `json:"completed_at,omitempty"`
	Summary          string          `json:"summary,omitempty"`
	Metadata         JSONMap         `json:"metadata,omitempty"`
}

// Session is a durable agent process/conversation.
type Session struct {
	ID              string        `json:"id"`
	ObjectiveID     string        `json:"objective_id,omitempty"`
	ParentSessionID string        `json:"parent_session_id,omitempty"`
	Role            SessionRole   `json:"role"`
	Agent           AgentKind     `json:"agent"`
	Mode            SessionMode   `json:"mode"`
	Status          SessionStatus `json:"status"`
	Title           string        `json:"title"`
	Goal            string        `json:"goal"`
	CurrentActivity string        `json:"current_activity,omitempty"`
	LatestSummary   string        `json:"latest_summary,omitempty"`
	// HandoffSummary is the worker-authored result relayed to the manager when the
	// session finishes (set via the report_result tool). Unlike LatestSummary —
	// which is scraped from the agent's last output and can capture a transitional
	// line or noisy TUI pane — this is exactly what the worker chose to hand off
	// (findings, a diff, references). Preferred over LatestSummary wherever a
	// worker's result is relayed.
	HandoffSummary string     `json:"handoff_summary,omitempty"`
	TargetID       string     `json:"target_id,omitempty"`
	WorkspaceID    string     `json:"workspace_id,omitempty"`
	UsageProvider  string     `json:"usage_provider,omitempty"`
	UsedTokens     int64      `json:"used_tokens"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	Metadata       JSONMap    `json:"metadata,omitempty"`
}

// Target is a machine where sessions can run.
type Target struct {
	ID                string       `json:"id"`
	Name              string       `json:"name"`
	Kind              TargetKind   `json:"kind"`
	Status            TargetStatus `json:"status"`
	Host              string       `json:"host,omitempty"`
	User              string       `json:"user,omitempty"`
	WorkRoot          string       `json:"work_root"`
	Labels            []string     `json:"labels,omitempty"`
	CapacitySessions  int          `json:"capacity_sessions"`
	AvailableSessions int          `json:"available_sessions"`
	CPUSummary        string       `json:"cpu_summary,omitempty"`
	MemorySummary     string       `json:"memory_summary,omitempty"`
	DiskSummary       string       `json:"disk_summary,omitempty"`
	LastSeenAt        *time.Time   `json:"last_seen_at,omitempty"`
	Metadata          JSONMap      `json:"metadata,omitempty"`
}

// Workspace is a filesystem checkout or scratch directory.
type Workspace struct {
	ID          string          `json:"id"`
	ObjectiveID string          `json:"objective_id,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
	TargetID    string          `json:"target_id"`
	Kind        WorkspaceKind   `json:"kind"`
	ProjectPath string          `json:"project_path"`
	VCS         VCS             `json:"vcs"`
	Path        string          `json:"path"`
	BaseRef     string          `json:"base_ref,omitempty"`
	BaseSHA     string          `json:"base_sha,omitempty"`
	BranchName  string          `json:"branch_name,omitempty"`
	Status      WorkspaceStatus `json:"status"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Metadata    JSONMap         `json:"metadata,omitempty"`
}

// PullRequest is a durable artifact owned by an objective.
type PullRequest struct {
	ID                 string      `json:"id"`
	ObjectiveID        string      `json:"objective_id,omitempty"`
	CreatedBySessionID string      `json:"created_by_session_id,omitempty"`
	Repo               string      `json:"repo"`
	Number             int         `json:"number"`
	URL                string      `json:"url"`
	Branch             string      `json:"branch"`
	BaseBranch         string      `json:"base_branch"`
	HeadSHA            string      `json:"head_sha,omitempty"`
	Status             PRStatus    `json:"status"`
	ChecksState        ChecksState `json:"checks_state"`
	Title              string      `json:"title"`
	Summary            string      `json:"summary,omitempty"`
	LastSyncedAt       *time.Time  `json:"last_synced_at,omitempty"`
	CreatedAt          time.Time   `json:"created_at"`
	UpdatedAt          time.Time   `json:"updated_at"`
	Metadata           JSONMap     `json:"metadata,omitempty"`
}

// Message is a transcript row from a session.
type Message struct {
	ID        string        `json:"id"`
	SessionID string        `json:"session_id"`
	Seq       int64         `json:"seq"`
	Source    MessageSource `json:"source"`
	Kind      MessageKind   `json:"kind"`
	Content   string        `json:"content"`
	Metadata  JSONMap       `json:"metadata,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}

// Question is a first-class request for user input.
type Question struct {
	ID          string         `json:"id"`
	ObjectiveID string         `json:"objective_id,omitempty"`
	SessionID   string         `json:"session_id,omitempty"`
	Status      QuestionStatus `json:"status"`
	Priority    int            `json:"priority"`
	Question    string         `json:"question"`
	Context     string         `json:"context,omitempty"`
	Answer      string         `json:"answer,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	AnsweredAt  *time.Time     `json:"answered_at,omitempty"`
	Metadata    JSONMap        `json:"metadata,omitempty"`
}

// Artifact is a durable useful output.
type Artifact struct {
	ID          string       `json:"id"`
	ObjectiveID string       `json:"objective_id,omitempty"`
	SessionID   string       `json:"session_id,omitempty"`
	Kind        ArtifactKind `json:"kind"`
	Title       string       `json:"title"`
	Summary     string       `json:"summary,omitempty"`
	URI         string       `json:"uri,omitempty"`
	Visibility  Visibility   `json:"visibility"`
	CreatedAt   time.Time    `json:"created_at"`
	Metadata    JSONMap      `json:"metadata,omitempty"`
}

// UsageBucket tracks provider usage within a window.
type UsageBucket struct {
	ID          string     `json:"id"`
	Provider    string     `json:"provider"`
	Account     string     `json:"account"`
	WindowStart time.Time  `json:"window_start"`
	WindowEnd   time.Time  `json:"window_end"`
	UsedTokens  int64      `json:"used_tokens"`
	UsedPercent *float64   `json:"used_percent,omitempty"`
	State       UsageState `json:"state"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ObjectiveUsage is the aggregate token usage for one objective: a grand
// total plus per-session and per-provider breakdowns.
type ObjectiveUsage struct {
	ObjectiveID string                  `json:"objective_id"`
	TotalTokens int64                   `json:"total_tokens"`
	Sessions    []SessionUsageBreakdown `json:"sessions"`
	Providers   []ProviderUsageTotal    `json:"providers"`
}

// SessionUsageBreakdown is one session's contribution to an objective's usage.
type SessionUsageBreakdown struct {
	SessionID  string `json:"session_id"`
	Title      string `json:"title"`
	Role       string `json:"role"`
	Provider   string `json:"provider"`
	UsedTokens int64  `json:"used_tokens"`
}

// ProviderUsageTotal is the total tokens an objective spent against one
// provider (sessions.usage_provider, falling back to sessions.agent).
type ProviderUsageTotal struct {
	Provider   string `json:"provider"`
	UsedTokens int64  `json:"used_tokens"`
}

// Lock is a scheduling lock held by a session.
type Lock struct {
	Key             string    `json:"key"`
	Kind            LockKind  `json:"kind"`
	HolderSessionID string    `json:"holder_session_id"`
	AcquiredAt      time.Time `json:"acquired_at"`
	Reason          string    `json:"reason,omitempty"`
}

// Event is an audit/history row. It is not the only source of state.
type Event struct {
	ID          int64     `json:"id"`
	ObjectiveID string    `json:"objective_id,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	Type        string    `json:"type"`
	Summary     string    `json:"summary"`
	Data        JSONMap   `json:"data,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Project is a registered repository teams work on. Objectives pick one from
// a list instead of retyping owner/repo; repos used once are remembered.
//
// Repo is the UPSTREAM (owner/repo): checkouts base off its branches and PRs
// open against it. PushRepo, when set, is the fork branches are pushed to —
// the standard fork workflow (base off upstream, push to fork, PR against
// upstream). Empty PushRepo means pushes go to Repo itself.
type Project struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Repo       string    `json:"repo"`
	PushRepo   string    `json:"push_repo,omitempty"`
	CloneURL   string    `json:"clone_url,omitempty"`
	BaseBranch string    `json:"base_branch,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}
