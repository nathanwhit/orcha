package orch

import (
	"github.com/nathanwhit/orcha/internal/model"
)

// NewObjectiveSpec describes a new objective.
type NewObjectiveSpec struct {
	Title  string
	Prompt string
	Agent  model.AgentKind // manager agent provider
}

// CreateObjective creates an objective and its manager session. Every objective
// starts with a manager session, per the spec.
func (o *Orchestrator) CreateObjective(spec NewObjectiveSpec) (*model.Objective, *model.Session, error) {
	obj := &model.Objective{
		Title:  spec.Title,
		Prompt: spec.Prompt,
		Status: model.ObjectiveActive,
	}
	if err := o.st.CreateObjective(obj); err != nil {
		return nil, nil, err
	}
	agentKind := spec.Agent
	if agentKind == "" {
		agentKind = o.defaultAgent()
	}
	mgr, err := o.CreateSession(SpawnSpec{
		ObjectiveID: obj.ID,
		Role:        model.RoleManager,
		Agent:       agentKind,
		Mode:        model.ModeInteractive,
		Title:       "Manager",
		Goal:        spec.Prompt,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := o.st.SetObjectiveManager(obj.ID, mgr.ID); err != nil {
		return nil, nil, err
	}
	obj.ManagerSessionID = mgr.ID
	o.audit(obj.ID, mgr.ID, "objective_created", "created objective: "+spec.Title, nil)
	return obj, mgr, nil
}

// CancelObjective cancels an objective and all of its non-terminal sessions.
func (o *Orchestrator) CancelObjective(objectiveID, summary string) error {
	sessions, err := o.st.ListSessionsByObjective(objectiveID)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if !s.Status.IsTerminal() {
			_ = o.Cancel(s.ID, true)
		}
	}
	return o.st.UpdateObjectiveStatus(objectiveID, model.ObjectiveCanceled, summary)
}
