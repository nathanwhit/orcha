package model

import "testing"

func TestSessionTransitions_Valid(t *testing.T) {
	valid := [][2]SessionStatus{
		{SessionQueued, SessionStarting},
		{SessionStarting, SessionRunning},
		{SessionRunning, SessionWaitingUser},
		{SessionWaitingUser, SessionRunning},
		{SessionRunning, SessionSucceeded},
		{SessionRunning, SessionFailed},
		{SessionRunning, SessionCanceled},
		{SessionWaitingCapacity, SessionRunning},
	}
	for _, tc := range valid {
		if err := ValidateSessionTransition(tc[0], tc[1]); err != nil {
			t.Errorf("expected %s->%s valid, got %v", tc[0], tc[1], err)
		}
	}
}

func TestSessionTransitions_TerminalIsFinal(t *testing.T) {
	terminals := []SessionStatus{SessionSucceeded, SessionFailed, SessionCanceled}
	nexts := []SessionStatus{SessionRunning, SessionSucceeded, SessionStarting, SessionWaitingUser}
	for _, term := range terminals {
		if !term.IsTerminal() {
			t.Fatalf("%s should be terminal", term)
		}
		for _, n := range nexts {
			if n == term {
				continue
			}
			if err := ValidateSessionTransition(term, n); err == nil {
				t.Errorf("expected %s->%s to be rejected (terminal)", term, n)
			}
		}
	}
}

// A late completion (succeeded) arriving after cancellation must be rejected so
// canceled work is never resurrected.
func TestSessionTransitions_LateCompletionRejected(t *testing.T) {
	if err := ValidateSessionTransition(SessionCanceled, SessionSucceeded); err == nil {
		t.Fatal("canceled->succeeded must be rejected")
	}
	if err := ValidateSessionTransition(SessionCanceled, SessionCanceled); err != nil {
		t.Fatalf("idempotent canceled->canceled should be allowed: %v", err)
	}
}

func TestTargetCanSchedule(t *testing.T) {
	if !TargetOnline.CanSchedule() {
		t.Error("online should schedule")
	}
	for _, s := range []TargetStatus{TargetDraining, TargetOffline, TargetDisabled} {
		if s.CanSchedule() {
			t.Errorf("%s should not schedule", s)
		}
	}
}

func TestEvaluatePush(t *testing.T) {
	if d := EvaluatePush(PRMerged); d.Allowed || d.NeedsManagerDecision {
		t.Errorf("merged must not push and needs no decision: %+v", d)
	}
	if d := EvaluatePush(PRClosed); d.Allowed || !d.NeedsManagerDecision {
		t.Errorf("closed must not push and needs a manager decision: %+v", d)
	}
	if d := EvaluatePush(PROpen); !d.Allowed {
		t.Errorf("open must allow push: %+v", d)
	}
	if d := EvaluatePush(PRDraft); !d.Allowed {
		t.Errorf("draft must allow push: %+v", d)
	}
}
