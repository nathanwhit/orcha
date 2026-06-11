package orch

import (
	"context"
	"strings"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

// Check is one diagnostic on a target.
type Check struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Required bool   `json:"required"`
	Detail   string `json:"detail,omitempty"`
}

// DoctorReport summarizes a target's readiness to run sessions.
type DoctorReport struct {
	TargetID string   `json:"target_id"`
	Target   string   `json:"target"`
	OK       bool     `json:"ok"`      // all required checks passed
	Missing  []string `json:"missing"` // names of failed required checks
	Checks   []Check  `json:"checks"`
}

// DoctorTarget runs readiness diagnostics on a target: connectivity, tmux, git,
// at least one agent CLI, a writable work root, and (informational) gh. All
// checks run in a single shell invocation over the target's executor, so it is
// one round trip even for SSH. It does not verify agent *auth* (that needs a
// real call); missing auth surfaces when a session first runs.
func (o *Orchestrator) DoctorTarget(ctx context.Context, targetID string) (*DoctorReport, error) {
	t, err := o.st.GetTarget(targetID)
	if err != nil {
		return nil, err
	}
	ex := agent.NewExecutor(t)
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	root := t.WorkRoot
	if root == "" {
		root = "/tmp/orcha/work"
	}
	q := shSingleQuote(root)
	// One script, one line per probe: "<name> ok [detail]" or "<name> MISSING".
	script := strings.Join([]string{
		`echo "connectivity ok"`,
		`if command -v tmux >/dev/null 2>&1; then echo "tmux ok $(tmux -V 2>/dev/null)"; else echo "tmux MISSING"; fi`,
		`if command -v git >/dev/null 2>&1; then echo "git ok $(git --version 2>/dev/null)"; else echo "git MISSING"; fi`,
		`if command -v claude >/dev/null 2>&1; then echo "claude ok"; else echo "claude MISSING"; fi`,
		`if command -v codex >/dev/null 2>&1; then echo "codex ok"; else echo "codex MISSING"; fi`,
		`if command -v gh >/dev/null 2>&1; then echo "gh ok"; else echo "gh MISSING"; fi`,
		`if ( mkdir -p ` + q + ` && touch ` + q + `/.orcha-doctor && rm -f ` + q + `/.orcha-doctor ) >/dev/null 2>&1; then echo "workroot ok"; else echo "workroot MISSING"; fi`,
	}, "; ")

	out, runErr := exec.RunCapture(dctx, ex, exec.Command{Name: "sh", Args: []string{"-c", script}})
	results := parseProbes(out)
	// If the executor itself failed (e.g. SSH unreachable), connectivity is false.
	if runErr != nil {
		results["connectivity"] = probe{ok: false, detail: strings.TrimSpace(out)}
	}

	probe := func(name string, required bool) Check {
		r := results[name]
		return Check{Name: name, OK: r.ok, Required: required, Detail: r.detail}
	}
	checks := []Check{
		probe("connectivity", true),
		probe("tmux", true),
		probe("git", true),
		probe("workroot", true),
		probe("claude", false),
		probe("codex", false),
		probe("gh", false),
	}
	// Synthesize an "agent CLI present" required check (at least one).
	agentOK := results["claude"].ok || results["codex"].ok
	agentDetail := "claude and/or codex must be installed"
	if agentOK {
		agentDetail = "found: " + strings.TrimSpace(strings.Join(presentAgents(results), ", "))
	}
	checks = append(checks, Check{Name: "agent_cli", OK: agentOK, Required: true, Detail: agentDetail})

	rep := &DoctorReport{TargetID: t.ID, Target: t.Name, OK: true}
	for _, c := range checks {
		if c.Required && !c.OK {
			rep.OK = false
			rep.Missing = append(rep.Missing, c.Name)
		}
	}
	rep.Checks = checks
	o.audit("", "", "target_doctor", t.Name+": ok="+boolStr(rep.OK), model.JSONMap{
		"target_id": t.ID, "missing": anySlice(rep.Missing),
	})
	return rep, nil
}

type probe struct {
	ok     bool
	detail string
}

func parseProbes(out string) map[string]probe {
	m := map[string]probe{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		name := fields[0]
		rest := ""
		if len(fields) > 1 {
			rest = strings.TrimSpace(fields[1])
		}
		ok := strings.HasPrefix(rest, "ok")
		detail := strings.TrimSpace(strings.TrimPrefix(rest, "ok"))
		if !ok && strings.Contains(rest, "MISSING") {
			detail = "not found"
		}
		m[name] = probe{ok: ok, detail: detail}
	}
	return m
}

func presentAgents(m map[string]probe) []string {
	var out []string
	for _, a := range []string{"claude", "codex"} {
		if m[a].ok {
			out = append(out, a)
		}
	}
	return out
}

// shSingleQuote single-quotes a string for a POSIX shell.
func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
