// Command orcha runs the agent team orchestrator: a SQLite-backed coordinator
// with an HTTP API and a dense dashboard. By default it registers in-process
// fake agent providers and a fake forge so the whole flow is runnable locally
// without external services.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/api"
	"github.com/nathanwhit/orcha/internal/forge"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/notify"
	"github.com/nathanwhit/orcha/internal/orch"
	"github.com/nathanwhit/orcha/internal/store"
	"github.com/nathanwhit/orcha/internal/version"
	"github.com/nathanwhit/orcha/internal/webui"
	"github.com/nathanwhit/orcha/internal/workspace"
)

// Version is the orcha build version, surfaced by the -version flag.
const Version = version.Version

func main() {
	var (
		dbPath       = flag.String("db", "orcha.db", "path to SQLite database")
		addr         = flag.String("addr", ":8080", "HTTP listen address")
		fakeAgents   = flag.Bool("fake-agents", false, "use in-process fake agents instead of the real claude/codex CLIs")
		tmuxAgents   = flag.Bool("tmux", false, "run agents as interactive TUIs inside attachable tmux sessions (tmux attach -t orcha-<id>)")
		claudeBin    = flag.String("claude-bin", "claude", "path to the claude CLI")
		codexBin     = flag.String("codex-bin", "codex", "path to the codex CLI")
		realForge    = flag.Bool("real-forge", false, "use the real git+gh forge (needs real workspace checkouts) instead of the in-memory fake")
		maxConc      = flag.Int("max-concurrent", 32, "max concurrent worker sessions across all targets (managers are exempt; per-target capacity still applies)")
		schedEvery   = flag.Duration("schedule-interval", 2*time.Second, "scheduler idle tick interval")
		mcpBase      = flag.String("mcp-base-url", "http://127.0.0.1:8080", "base URL where the manager MCP tool surface is reachable by agent CLIs")
		showVersion  = flag.Bool("version", false, "print version and exit")
		workerPerm   = flag.String("agent-permissions", "bypassPermissions", "permission/sandbox mode for all agents: bypassPermissions (no prompts/sandbox — safe in a VM) or acceptEdits (edits only, prompts for shell)")
		prMonitor    = flag.Duration("pr-monitor", 0, "poll open PRs for new comments/checks/merges this often and spawn follow-ups / notify the manager (0 = auto: on at 60s with -real-forge, off otherwise)")
		issueBot     = flag.String("issue-bot-login", "", "GitHub login orcha runs as; @-mentioning or assigning it on an issue in a registered project creates an objective (needs -real-forge and -issue-allow)")
		issueAllow   = flag.String("issue-allow", "", "comma-separated GitHub logins permitted to summon work via an issue @-mention/assignment (empty disables the issue trigger)")
		idleBgWork   = flag.Duration("idle-bg-work-timeout", 4*time.Hour, "tmux mode: max time a one-shot worker may sit on a static pane that still shows live background shells (a build it yielded to await) before it is reaped; must exceed the longest build+wait")
		usageMonitor = flag.Duration("usage-monitor", 0, "scrape each provider's real subscription usage (claude /usage, codex /status) via a tmux pty this often, so provider selection load-balances on actual remaining usage (0 = auto: on at 5m unless -fake-agents, off with fake)")
		notifyURL    = flag.String("notify-url", "", "POST high-signal events (worker needs input, work done, PR opened/updated, failures) here as JSON for push notifications; matches ntfy's JSON publish format so an ntfy server URL works directly (empty disables)")
		notifyTopic  = flag.String("notify-topic", "", "ntfy topic to publish to (carried in the JSON body; set for ntfy.sh, ignored by endpoints that don't use topics)")
		exeAuth      = flag.Bool("exe-auth", false, "gate the dashboard and API behind exe.dev's authenticating proxy (trusts the X-ExeDev-Email header it injects; only safe when the VM is reachable solely via that proxy). Leave off for local dev.")
		allowedMail  = flag.String("allowed-emails", "", "comma-separated exe.dev emails permitted to use the dashboard when -exe-auth is set (empty = any authenticated exe.dev user)")
	)
	flag.Parse()

	// With a real forge, watching GitHub is the default: without it, merges and
	// review comments are never observed, so a merged PR is silent and its
	// objective never wraps up. An explicit -pr-monitor still wins.
	if *prMonitor == 0 && *realForge {
		*prMonitor = 60 * time.Second
	}

	// Usage monitoring needs a real provider CLI to scrape; it makes no sense
	// with fake agents. Default it on (every 5m) otherwise so provider selection
	// load-balances on actual remaining usage out of the box.
	if *usageMonitor == 0 && !*fakeAgents {
		*usageMonitor = 5 * time.Minute
	}

	if *showVersion {
		fmt.Println(Version)
		return
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	o := orch.New(st, orch.Config{
		Guards:               orch.DefaultGuards(),
		ProviderFallback:     []model.AgentKind{model.AgentClaude, model.AgentCodex},
		ManagerMCPBaseURL:    *mcpBase,
		WorkerPermissionMode: *workerPerm,
		IssueTriggers: orch.IssueTriggerConfig{
			BotLogin:      *issueBot,
			AllowedLogins: splitCSV(*issueAllow),
		},
	})
	switch {
	case *fakeAgents:
		// Offline: scriptable in-process agents, no external CLIs needed.
		o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
		o.RegisterProvider(agent.NewFake(model.AgentCodex, false, nil))
		log.Println("using fake agents")
	case *tmuxAgents:
		// Each session is an interactive TUI inside a real, attachable tmux
		// session. Watch or take over any session with `tmux attach -t orcha-<id>`
		// (the attach command is recorded on each session).
		o.RegisterProvider(agent.NewTmuxClaude(agent.ClaudeConfig{Binary: *claudeBin, CompletionGate: o.CompletionAllowed, MaxIdleWithBgWork: *idleBgWork}))
		o.RegisterProvider(agent.NewTmuxCodex(agent.CodexConfig{Binary: *codexBin, CompletionGate: o.CompletionAllowed, MaxIdleWithBgWork: *idleBgWork}))
		o.SetUsageBin(model.AgentClaude, *claudeBin)
		o.SetUsageBin(model.AgentCodex, *codexBin)
		log.Println("using tmux interactive TUIs (attach: tmux attach -t orcha-<sessionID>)")
	default:
		// Real CLIs, headless. Claude runs as a persistent interactive stream-json
		// session; Codex runs `codex exec` and is steered via resume.
		o.RegisterProvider(agent.NewClaude(agent.ClaudeConfig{Binary: *claudeBin}))
		o.RegisterProvider(agent.NewCodex(agent.CodexConfig{Binary: *codexBin}))
		o.SetUsageBin(model.AgentClaude, *claudeBin)
		o.SetUsageBin(model.AgentCodex, *codexBin)
		log.Println("using real claude + codex CLIs (headless)")
	}
	// Workspace preparation is always real: any coding worker whose objective
	// names a repo gets a fresh isolated checkout. Without it, agents would run
	// in the orchestrator's own cwd and edit the operator's live repo.
	o.SetWorkspacePreparer(workspace.New())
	if *realForge {
		// Real git push + gh PR operations on top of the real checkouts.
		o.SetForge(forge.NewGit())
		log.Println("using real git+gh forge")
	} else {
		o.SetForge(forge.NewFake())
		log.Println("using fake forge (no real PR operations)")
	}

	// Push notifications for high-signal events (worker needs input, work done,
	// PR opened/updated, failures). Best-effort fire-and-forget; off when unset.
	if n := notify.New(*notifyURL, *notifyTopic); n != nil {
		o.SetNotifier(n)
		log.Printf("notifications on (POST to %s)", *notifyURL)
	}

	// Ensure a local target exists so sessions can be scheduled out of the box.
	ensureLocalTarget(st)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The scheduler driver makes the system self-driving: creating an objective
	// starts its manager, which spawns workers that the scheduler then runs.
	sched := orch.NewScheduler(o, *schedEvery, *maxConc)
	o.SetNotify(sched.Wake)
	// Requeue sessions a previous process left mid-flight, so restarting orcha
	// resumes in-progress objectives instead of stranding them.
	if n := o.RecoverInterrupted(); n > 0 {
		log.Printf("recovered %d interrupted session(s) from a previous run", n)
	}
	go sched.Run(ctx)

	// Host monitor: poll open PRs for new comments/checks (spawning follow-ups)
	// and registered projects' issues for @-mention/assignment triggers. Both run
	// on the same cadence. The issue trigger can be on without PR polling, so the
	// interval falls back to 60s when only it is enabled.
	monitorEvery := *prMonitor
	if monitorEvery == 0 && o.IssueTriggersEnabled() {
		monitorEvery = 60 * time.Second
	}
	if monitorEvery > 0 {
		go func() {
			t := time.NewTicker(monitorEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if *prMonitor > 0 {
						o.SyncOpenPRs(ctx)
					}
					o.SyncIssueTriggers(ctx)
				}
			}
		}()
		if *prMonitor > 0 {
			log.Printf("PR monitor on (every %s)", monitorEvery)
		}
		if o.IssueTriggersEnabled() {
			log.Printf("issue trigger on (every %s, bot=@%s, %d allowed login(s))",
				monitorEvery, *issueBot, len(splitCSV(*issueAllow)))
		}
	}

	// Usage monitor: scrape each provider's real subscription usage (claude
	// /usage, codex /status) on its own cadence and record the weekly percentage
	// that defaultAgent()/SelectProvider balance on. Slow-moving, so it ticks
	// independently of the PR/issue monitor. Runs an immediate first pass so the
	// scheduler isn't balancing blind for the first interval.
	if *usageMonitor > 0 {
		go func() {
			o.SyncUsage(ctx)
			t := time.NewTicker(*usageMonitor)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					o.SyncUsage(ctx)
				}
			}
		}()
		log.Printf("usage monitor on (every %s)", *usageMonitor)
	}

	srv := api.New(o)
	mux := http.NewServeMux()
	// Restart support: exits with code 42 after a graceful shutdown, which the
	// scripts/dev.sh supervisor takes as "rebuild from source and relaunch".
	// Without a supervisor this just stops the server. Restarting is safe for
	// in-flight work: shutdown leaves sessions recoverable and live tmux
	// sessions are re-adopted on the next boot.
	restart := make(chan struct{}, 1)
	restartHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"restarting"}` + "\n"))
		select {
		case restart <- struct{}{}:
		default:
		}
	})

	// guard gates the operator-facing surfaces (dashboard, API, pty) behind
	// exe.dev identity when -exe-auth is set; it is the identity wrapper otherwise.
	// The MCP surfaces below are deliberately left ungated: agent CLIs reach them
	// over the localhost reverse tunnel and never carry exe headers.
	guard := func(h http.Handler) http.Handler { return h }
	if *exeAuth {
		guard = api.ExeAuth(splitCSV(*allowedMail))
		log.Printf("exe.dev auth on (allowlist: %d email(s))", len(splitCSV(*allowedMail)))
	}

	mux.Handle("POST /api/restart", guard(restartHandler))
	mux.Handle("/api/", guard(srv.Handler()))
	// Manager tool surface (MCP). Manager sessions' Claude connects to
	// /mcp/<sessionID> to drive the orchestrator.
	mux.Handle("/mcp/", http.StripPrefix("/mcp", o.ManagerMCPHandler()))
	// Worker tool surface (MCP): the smaller report_result/create_note/ask_user
	// subset. Coding workers connect to /wmcp/<sessionID> to hand their result back.
	mux.Handle("/wmcp/", http.StripPrefix("/wmcp", o.WorkerMCPHandler()))
	// Follow-up tool surface (MCP): the PR-response tools (update_pr/comment_pr/…)
	// plus report_result, but not the manager's spawn/publish/mark-done tools.
	mux.Handle("/fmcp/", http.StripPrefix("/fmcp", o.FollowupMCPHandler()))
	// Reviewer tool surface (MCP): submit_review (+create_note/ask_user) for the
	// adversarial review gate. Reviewers connect to /rmcp/<sessionID>.
	mux.Handle("/rmcp/", http.StripPrefix("/rmcp", o.ReviewMCPHandler()))
	// Operator tool surface (MCP): drive orcha from the top — create/list/inspect
	// objectives, answer questions, steer a manager. Not session-bound; an outside
	// agent (e.g. reached over an SSH tunnel) connects to /omcp/<anything>.
	mux.Handle("/omcp/", http.StripPrefix("/omcp", o.OperatorMCPHandler()))
	// The dashboard SPA (built from ui/, embedded at compile time).
	mux.Handle("/", guard(webui.Handler()))

	httpSrv := &http.Server{Addr: *addr, Handler: mux}

	go func() {
		log.Printf("orcha listening on %s (db=%s)", *addr, *dbPath)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	restarting := false
	select {
	case <-ctx.Done():
	case <-restart:
		restarting = true
		stop() // unwind runs the same way a signal would
	}
	log.Println("shutting down")
	_ = httpSrv.Shutdown(context.Background())
	o.CloseTunnels() // ssh -N children don't exit on their own
	if restarting {
		os.Exit(42) // restart sentinel for scripts/dev.sh
	}
}

// splitCSV parses a comma-separated flag into trimmed, non-empty tokens.
func splitCSV(s string) []string {
	var out []string
	for _, tok := range strings.Split(s, ",") {
		if t := strings.TrimSpace(tok); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func ensureLocalTarget(st *store.Store) {
	targets, _ := st.ListTargets()
	for _, t := range targets {
		if t.Kind == model.TargetLocal {
			return
		}
	}
	_ = st.CreateTarget(&model.Target{
		Name:             "local",
		Kind:             model.TargetLocal,
		Status:           model.TargetOnline,
		WorkRoot:         "/tmp/orcha/work",
		CapacitySessions: 4,
		CPUSummary:       "local",
	})
}
