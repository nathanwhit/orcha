package orch

import (
	"fmt"

	"github.com/nathanwhit/orcha/internal/model"
)

// guardState tracks loop-guard counters for a single session. It lives in
// memory; the durable consequence of a trip (a paused session + recorded
// reason + question) is persisted.
type guardState struct {
	lastError      string
	sameErrorCount int
	noProgressRuns int
	modelCalls     int
}

func (o *Orchestrator) guardFor(sessionID string) *guardState {
	o.mu.Lock()
	defer o.mu.Unlock()
	g := o.guards[sessionID]
	if g == nil {
		g = &guardState{}
		o.guards[sessionID] = g
	}
	return g
}

// GuardTrip describes why a guard fired.
type GuardTrip struct {
	Reason string
}

func (g GuardTrip) Error() string { return "orch: loop guard tripped: " + g.Reason }

// RecordProgress resets the no-progress counter. Call whenever a session makes
// observable forward progress (a new tool result, a summary change, a commit).
func (o *Orchestrator) RecordProgress(sessionID string) {
	g := o.guardFor(sessionID)
	o.mu.Lock()
	g.noProgressRuns = 0
	g.lastError = ""
	g.sameErrorCount = 0
	o.mu.Unlock()
}

// CheckError feeds an error string to the same-error guard. If the identical
// error repeats beyond the threshold it returns a GuardTrip.
func (o *Orchestrator) CheckError(sessionID, errText string) error {
	g := o.guardFor(sessionID)
	o.mu.Lock()
	defer o.mu.Unlock()
	if errText == g.lastError {
		g.sameErrorCount++
	} else {
		g.lastError = errText
		g.sameErrorCount = 1
	}
	if g.sameErrorCount >= o.cfg.Guards.MaxSameErrorRetries {
		return GuardTrip{Reason: fmt.Sprintf("same error repeated %d times: %s", g.sameErrorCount, errText)}
	}
	return nil
}

// CheckNoProgress increments the no-progress counter for a turn that produced
// nothing new. It returns a GuardTrip once the threshold is crossed.
func (o *Orchestrator) CheckNoProgress(sessionID string) error {
	g := o.guardFor(sessionID)
	o.mu.Lock()
	defer o.mu.Unlock()
	g.noProgressRuns++
	if g.noProgressRuns >= o.cfg.Guards.MaxNoProgressTurns {
		return GuardTrip{Reason: fmt.Sprintf("no progress for %d turns", g.noProgressRuns)}
	}
	return nil
}

// CountModelCall increments the per-session model-call counter and trips when
// the per-session hourly cap is exceeded.
func (o *Orchestrator) CountModelCall(sessionID string) error {
	g := o.guardFor(sessionID)
	o.mu.Lock()
	defer o.mu.Unlock()
	g.modelCalls++
	if g.modelCalls > o.cfg.Guards.MaxModelCallsPerSessHr {
		return GuardTrip{Reason: fmt.Sprintf("session exceeded %d model calls/hour", o.cfg.Guards.MaxModelCallsPerSessHr)}
	}
	return nil
}

// PauseSession pauses a session due to a guard trip: it moves the session to
// waiting_user, records a compact reason in the transcript and audit log, and
// opens a question so the user/manager can give direction. It stops spending
// model calls by design — the caller must abort its run loop after this.
func (o *Orchestrator) PauseSession(sessionID, reason string) error {
	sess, err := o.st.UpdateSessionStatus(sessionID, model.SessionWaitingUser)
	if err != nil {
		// If the session is already terminal there is nothing to pause.
		return err
	}
	_, _ = o.st.UpdateSessionRuntime(sessionID, func(s *model.Session) {
		s.CurrentActivity = "paused: " + reason
	})
	_ = o.emit(sessionID, model.MsgSystem, model.KindStatus, "guard paused session: "+reason, nil)
	o.audit(sess.ObjectiveID, sessionID, "guard_pause", reason, nil)
	_ = o.st.CreateQuestion(&model.Question{
		ObjectiveID: sess.ObjectiveID,
		SessionID:   sessionID,
		Priority:    10,
		Question:    "A session was paused by a loop guard. How should it proceed?",
		Context:     reason,
	})
	return nil
}
