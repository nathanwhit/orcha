package model

import "fmt"

// ---------------------------------------------------------------------------
// Session state machine
// ---------------------------------------------------------------------------

// sessionTransitions maps each status to the set of statuses it may move to.
// Terminal statuses (succeeded, failed, canceled) intentionally have no
// outgoing edges: this is what guarantees a late completion can never
// resurrect canceled work.
var sessionTransitions = map[SessionStatus]map[SessionStatus]bool{
	SessionQueued: {
		SessionStarting:        true,
		SessionWaitingCapacity: true,
		SessionWaitingUser:     true, // parked: blocked on the user before ever starting
		SessionCanceled:        true,
		SessionFailed:          true,
	},
	SessionStarting: {
		SessionRunning:     true,
		SessionWaitingUser: true,
		SessionFailed:      true,
		SessionCanceled:    true,
	},
	SessionRunning: {
		SessionWaitingUser:     true,
		SessionWaitingCapacity: true,
		SessionSucceeded:       true,
		SessionFailed:          true,
		SessionCanceled:        true,
	},
	SessionWaitingUser: {
		SessionRunning:   true,
		SessionStarting:  true,
		SessionCanceled:  true,
		SessionFailed:    true,
		SessionSucceeded: true,
	},
	SessionWaitingCapacity: {
		SessionQueued:      true,
		SessionStarting:    true,
		SessionRunning:     true,
		SessionWaitingUser: true, // parked: blocked on the user while awaiting capacity
		SessionCanceled:    true,
		SessionFailed:      true,
	},
	// Terminal states: no outgoing transitions.
	SessionSucceeded: {},
	SessionFailed:    {},
	SessionCanceled:  {},
}

// IsTerminal reports whether a session status is final.
func (s SessionStatus) IsTerminal() bool {
	switch s {
	case SessionSucceeded, SessionFailed, SessionCanceled:
		return true
	}
	return false
}

// CanTransitionTo reports whether a session may move from its current status
// to next.
func (s SessionStatus) CanTransitionTo(next SessionStatus) bool {
	if s == next {
		return true // idempotent no-op
	}
	return sessionTransitions[s][next]
}

// ValidateSessionTransition returns an error describing why a transition is
// illegal, or nil if it is allowed. Callers use the error to reject late
// completions and other invalid moves.
func ValidateSessionTransition(from, to SessionStatus) error {
	if from == to {
		return nil
	}
	if from.IsTerminal() {
		return fmt.Errorf("session is terminal (%s); cannot transition to %s", from, to)
	}
	if !from.CanTransitionTo(to) {
		return fmt.Errorf("illegal session transition %s -> %s", from, to)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Objective state machine
// ---------------------------------------------------------------------------

// IsTerminal reports whether an objective status is final.
func (s ObjectiveStatus) IsTerminal() bool {
	switch s {
	case ObjectiveSucceeded, ObjectiveFailed, ObjectiveCanceled:
		return true
	}
	return false
}

var objectiveTransitions = map[ObjectiveStatus]map[ObjectiveStatus]bool{
	ObjectiveActive: {
		ObjectiveWaitingUser: true,
		ObjectiveSucceeded:   true,
		ObjectiveFailed:      true,
		ObjectiveCanceled:    true,
	},
	ObjectiveWaitingUser: {
		ObjectiveActive:    true,
		ObjectiveSucceeded: true,
		ObjectiveFailed:    true,
		ObjectiveCanceled:  true,
	},
	ObjectiveSucceeded: {},
	ObjectiveFailed:    {},
	ObjectiveCanceled:  {},
}

// ValidateObjectiveTransition returns an error if the transition is illegal.
func ValidateObjectiveTransition(from, to ObjectiveStatus) error {
	if from == to {
		return nil
	}
	if from.IsTerminal() {
		return fmt.Errorf("objective is terminal (%s); cannot transition to %s", from, to)
	}
	if !objectiveTransitions[from][to] {
		return fmt.Errorf("illegal objective transition %s -> %s", from, to)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Target scheduling
// ---------------------------------------------------------------------------

// CanSchedule reports whether new sessions may be placed on a target given its
// status. Draining targets keep existing sessions but accept no new ones.
func (t TargetStatus) CanSchedule() bool {
	return t == TargetOnline
}

// ---------------------------------------------------------------------------
// PR push safety
// ---------------------------------------------------------------------------

// PushDecision is the result of evaluating whether a follow-up may push to a
// PR branch.
type PushDecision struct {
	// Allowed is true when a push is mechanically safe.
	Allowed bool
	// NeedsManagerDecision is true when the situation requires the manager to
	// decide what to do (e.g. the PR was closed).
	NeedsManagerDecision bool
	// Reason explains the decision in operational terms.
	Reason string
}

// EvaluatePush applies the PR branch-safety rules from the spec:
//   - merged: never push
//   - closed: do not push; create a manager decision point
//   - open/draft: push allowed
func EvaluatePush(status PRStatus) PushDecision {
	switch status {
	case PRMerged:
		return PushDecision{Allowed: false, Reason: "PR is merged; pushing is forbidden"}
	case PRClosed:
		return PushDecision{
			Allowed:              false,
			NeedsManagerDecision: true,
			Reason:               "PR is closed; manager must decide whether to open a new PR",
		}
	case PROpen, PRDraft:
		return PushDecision{Allowed: true, Reason: "PR is open; follow-up push allowed"}
	default:
		return PushDecision{Allowed: false, Reason: fmt.Sprintf("unknown PR status %q", status)}
	}
}
