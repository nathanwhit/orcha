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
	"syscall"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/api"
	"github.com/nathanwhit/orcha/internal/forge"
	"github.com/nathanwhit/orcha/internal/model"
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
		dbPath      = flag.String("db", "orcha.db", "path to SQLite database")
		addr        = flag.String("addr", ":8080", "HTTP listen address")
		fakeAgents  = flag.Bool("fake-agents", false, "use in-process fake agents instead of the real claude/codex CLIs")
		tmuxAgents  = flag.Bool("tmux", false, "run agents as interactive TUIs inside attachable tmux sessions (tmux attach -t orcha-<id>)")
		claudeBin   = flag.String("claude-bin", "claude", "path to the claude CLI")
		codexBin    = flag.String("codex-bin", "codex", "path to the codex CLI")
		realForge   = flag.Bool("real-forge", false, "use the real git+gh forge (needs real workspace checkouts) instead of the in-memory fake")
		maxConc     = flag.Int("max-concurrent", 8, "max simultaneously active sessions across all targets")
		schedEvery  = flag.Duration("schedule-interval", 2*time.Second, "scheduler idle tick interval")
		mcpBase     = flag.String("mcp-base-url", "http://127.0.0.1:8080", "base URL where the manager MCP tool surface is reachable by agent CLIs")
		showVersion = flag.Bool("version", false, "print version and exit")
		workerPerm  = flag.String("agent-permissions", "bypassPermissions", "permission/sandbox mode for all agents: bypassPermissions (no prompts/sandbox — safe in a VM) or acceptEdits (edits only, prompts for shell)")
		prMonitor   = flag.Duration("pr-monitor", 0, "poll open PRs for new comments/checks this often and spawn follow-ups (0 = off; needs -real-forge)")
	)
	flag.Parse()

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
		o.RegisterProvider(agent.NewTmuxClaude(agent.ClaudeConfig{Binary: *claudeBin}))
		o.RegisterProvider(agent.NewTmuxCodex(agent.CodexConfig{Binary: *codexBin}))
		log.Println("using tmux interactive TUIs (attach: tmux attach -t orcha-<sessionID>)")
	default:
		// Real CLIs, headless. Claude runs as a persistent interactive stream-json
		// session; Codex runs `codex exec` and is steered via resume.
		o.RegisterProvider(agent.NewClaude(agent.ClaudeConfig{Binary: *claudeBin}))
		o.RegisterProvider(agent.NewCodex(agent.CodexConfig{Binary: *codexBin}))
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

	// PR monitor: poll open PRs for new comments/checks and spawn follow-ups.
	if *prMonitor > 0 {
		go func() {
			t := time.NewTicker(*prMonitor)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					o.SyncOpenPRs(ctx)
				}
			}
		}()
		log.Printf("PR monitor on (every %s)", *prMonitor)
	}

	srv := api.New(o)
	mux := http.NewServeMux()
	// Restart support: exits with code 42 after a graceful shutdown, which the
	// scripts/dev.sh supervisor takes as "rebuild from source and relaunch".
	// Without a supervisor this just stops the server. Restarting is safe for
	// in-flight work: shutdown leaves sessions recoverable and live tmux
	// sessions are re-adopted on the next boot.
	restart := make(chan struct{}, 1)
	mux.HandleFunc("POST /api/restart", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"restarting"}` + "\n"))
		select {
		case restart <- struct{}{}:
		default:
		}
	})
	mux.Handle("/api/", srv.Handler())
	// Manager tool surface (MCP). Manager sessions' Claude connects to
	// /mcp/<sessionID> to drive the orchestrator.
	mux.Handle("/mcp/", http.StripPrefix("/mcp", o.ManagerMCPHandler()))
	// The dashboard SPA (built from ui/, embedded at compile time).
	mux.Handle("/", webui.Handler())

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
