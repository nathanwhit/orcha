package orch

import (
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/notify"
)

// SetNotifier installs the outbound push-notification sink. When set, the
// curated high-signal events in notifySpecs are forwarded as push notifications
// in addition to being recorded in the events log. Optional; a nil notifier
// (the default, or an unconfigured -notify-url) disables notifications.
func (o *Orchestrator) SetNotifier(n *notify.Notifier) { o.notifier = n }

// notifySpec describes how one event type renders as a user-facing push.
type notifySpec struct {
	title string   // human-readable category shown as the notification title
	tags  []string // ntfy emoji shortcodes; decoration only, ignorable by other receivers
}

// notifySpecs is the allowlist of event types worth interrupting the user for.
// It maps the four moments worth a push — a worker is blocked on input, work
// finished, a PR moved, or something failed — onto the orchestrator's internal
// event types. Anything not listed here stays in the events log only. High-churn
// events (usage_synced, usage_probe_failed, and the steer_* / mcp_tunnel_*
// retries) are deliberately excluded so a stuck subsystem can't turn into a push
// storm — a single flapping reverse tunnel was observed dropping/reopening ~1×/s
// (~100k events/day), which as a per-event push would bury every real one.
var notifySpecs = map[string]notifySpec{
	// Worker needs input.
	"ask_user":           {title: "Needs your input", tags: []string{"speech_balloon"}},
	"provider_exhausted": {title: "Providers exhausted", tags: []string{"warning"}},

	// Objective / session done.
	"objective_done":   {title: "Objective done", tags: []string{"white_check_mark"}},
	"manager_notified": {title: "Worker finished", tags: []string{"checkered_flag"}},

	// PR opened / updated.
	"pr_published": {title: "PR opened", tags: []string{"rocket"}},
	"pr_updated":   {title: "PR updated", tags: []string{"arrows_counterclockwise"}},

	// Failures / errors.
	"guard_pause":              {title: "Session paused", tags: []string{"warning"}},
	"workspace_prepare_failed": {title: "Workspace setup failed", tags: []string{"x"}},
	"target_unreachable":       {title: "Target unreachable", tags: []string{"x"}},
	"target_bootstrap_failed":  {title: "Target setup failed", tags: []string{"x"}},
	// Note: mcp_tunnel_died is intentionally NOT here — it flaps (see above).
}

// notifyEvent forwards an event to the notifier when one is configured and the
// event type is on the allowlist. It is called for every audit event; the map
// lookup and nil check keep the no-notifier path free. The store is not touched
// here (audit() is called from hot/locked paths), so the message is just the
// event summary — the dashboard has the rest of the detail.
func (o *Orchestrator) notifyEvent(typ, summary string, _ model.JSONMap) {
	if o.notifier == nil {
		return
	}
	spec, ok := notifySpecs[typ]
	if !ok {
		return
	}
	msg := summary
	if msg == "" {
		msg = typ
	}
	o.notifier.Send(notify.Notification{
		Title:   spec.title,
		Message: msg,
		Tags:    spec.tags,
	})
}
