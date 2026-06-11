package orch

import (
	"context"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/model"
)

// RegisterTarget creates a target (local or SSH) and verifies it: it runs the
// bootstrap command and a health check, marking the target online if reachable
// or offline otherwise. SSH targets read host/user/work_root plus ssh_port /
// identity_file / bootstrap from metadata. Registration nudges the scheduler so
// queued work can land on the new machine.
func (o *Orchestrator) RegisterTarget(ctx context.Context, t *model.Target) (*model.Target, error) {
	if t.Kind == "" {
		t.Kind = model.TargetLocal
	}
	if t.Status == "" {
		t.Status = model.TargetOnline
	}
	if t.WorkRoot == "" {
		t.WorkRoot = "/tmp/orcha/work"
	}
	if err := o.st.CreateTarget(t); err != nil {
		return nil, err
	}
	o.audit("", "", "target_registered", "registered "+string(t.Kind)+" target "+t.Name, model.JSONMap{"target_id": t.ID})
	_ = o.HealthCheckTarget(ctx, t.ID) // best-effort; sets online/offline
	o.notifyChange()
	return o.st.GetTarget(t.ID)
}

// HealthCheckTarget bootstraps and pings a target, updating its status and
// last_seen_at. A disabled target is left alone (operator intent); a draining
// target stays draining when healthy.
func (o *Orchestrator) HealthCheckTarget(ctx context.Context, id string) error {
	t, err := o.st.GetTarget(id)
	if err != nil {
		return err
	}
	if t.Status == model.TargetDisabled {
		return nil
	}
	ex := agent.NewExecutor(t)
	hctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	if err := ex.Bootstrap(hctx); err != nil {
		_ = o.st.MarkTargetSeen(id, model.TargetOffline)
		o.audit("", "", "target_bootstrap_failed", err.Error(), model.JSONMap{"target_id": id})
		return err
	}
	if err := ex.HealthCheck(hctx); err != nil {
		_ = o.st.MarkTargetSeen(id, model.TargetOffline)
		o.audit("", "", "target_unreachable", err.Error(), model.JSONMap{"target_id": id})
		return err
	}

	status := model.TargetOnline
	if t.Status == model.TargetDraining {
		status = model.TargetDraining // preserve operator drain
	}
	if err := o.st.MarkTargetSeen(id, status); err != nil {
		return err
	}
	o.notifyChange()
	return nil
}

// targetRequestFor builds a placement request from a session's preferences: a
// pinned target (by id or name) and/or required labels recorded in metadata,
// plus the repo for build-cache locality.
func (o *Orchestrator) targetRequestFor(sess *model.Session) TargetRequest {
	req := TargetRequest{}
	if repo, _ := sess.Metadata["repo"].(string); repo != "" {
		req.ProjectPath = repo
	}
	if labels := metaStrings(sess.Metadata, "target_labels"); len(labels) > 0 {
		req.RequiredLabels = labels
	}
	if pin, _ := sess.Metadata["pinned_target"].(string); pin != "" {
		req.PinnedTargetID = o.resolveTargetID(pin)
	}
	return req
}

// resolveTargetID accepts a target id or name and returns the id.
func (o *Orchestrator) resolveTargetID(idOrName string) string {
	if _, err := o.st.GetTarget(idOrName); err == nil {
		return idOrName
	}
	if targets, err := o.st.ListTargets(); err == nil {
		for _, t := range targets {
			if t.Name == idOrName {
				return t.ID
			}
		}
	}
	return idOrName // let placement fail clearly if unknown
}

func metaStrings(m model.JSONMap, key string) []string {
	switch v := m[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
