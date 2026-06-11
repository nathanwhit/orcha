// Command orcha runs the agent team orchestrator: a SQLite-backed coordinator
// with an HTTP API and a dense dashboard. By default it registers in-process
// fake agent providers and a fake forge so the whole flow is runnable locally
// without external services.
package main

import (
	"context"
	"flag"
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
	"github.com/nathanwhit/orcha/internal/workspace"
)

func main() {
	var (
		dbPath     = flag.String("db", "orcha.db", "path to SQLite database")
		addr       = flag.String("addr", ":8080", "HTTP listen address")
		fakeAgents = flag.Bool("fake-agents", false, "use in-process fake agents instead of the real claude/codex CLIs")
		tmuxAgents = flag.Bool("tmux", false, "run agents as interactive TUIs inside attachable tmux sessions (tmux attach -t orcha-<id>)")
		claudeBin  = flag.String("claude-bin", "claude", "path to the claude CLI")
		codexBin   = flag.String("codex-bin", "codex", "path to the codex CLI")
		realForge  = flag.Bool("real-forge", false, "use the real git+gh forge (needs real workspace checkouts) instead of the in-memory fake")
		maxConc    = flag.Int("max-concurrent", 8, "max simultaneously active sessions across all targets")
		schedEvery = flag.Duration("schedule-interval", 2*time.Second, "scheduler idle tick interval")
		mcpBase    = flag.String("mcp-base-url", "http://127.0.0.1:8080", "base URL where the manager MCP tool surface is reachable by agent CLIs")
	)
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	o := orch.New(st, orch.Config{
		Guards:            orch.DefaultGuards(),
		ProviderFallback:  []model.AgentKind{model.AgentClaude, model.AgentCodex},
		ManagerMCPBaseURL: *mcpBase,
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
		o.RegisterProvider(agent.NewTmuxAgent(model.AgentCodex, *codexBin))
		log.Println("using tmux interactive TUIs (attach: tmux attach -t orcha-<sessionID>)")
	default:
		// Real CLIs, headless. Claude runs as a persistent interactive stream-json
		// session; Codex runs `codex exec` and is steered via resume.
		o.RegisterProvider(agent.NewClaude(agent.ClaudeConfig{Binary: *claudeBin}))
		o.RegisterProvider(agent.NewCodex(agent.CodexConfig{Binary: *codexBin}))
		log.Println("using real claude + codex CLIs (headless)")
	}
	if *realForge {
		// Real git push + gh PR operations, paired with real workspace
		// preparation so sessions run in fresh git checkouts branched off the
		// latest upstream.
		o.SetForge(forge.NewGit())
		o.SetWorkspacePreparer(workspace.New())
		log.Println("using real git+gh forge with real workspace checkouts")
	} else {
		o.SetForge(forge.NewFake())
		log.Println("using fake forge")
	}

	// Ensure a local target exists so sessions can be scheduled out of the box.
	ensureLocalTarget(st)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The scheduler driver makes the system self-driving: creating an objective
	// starts its manager, which spawns workers that the scheduler then runs.
	sched := orch.NewScheduler(o, *schedEvery, *maxConc)
	o.SetNotify(sched.Wake)
	go sched.Run(ctx)

	srv := api.New(o)
	mux := http.NewServeMux()
	mux.Handle("/api/", srv.Handler())
	// Manager tool surface (MCP). Manager sessions' Claude connects to
	// /mcp/<sessionID> to drive the orchestrator.
	mux.Handle("/mcp/", http.StripPrefix("/mcp", o.ManagerMCPHandler()))
	mux.HandleFunc("/", dashboard)

	httpSrv := &http.Server{Addr: *addr, Handler: mux}

	go func() {
		log.Printf("orcha listening on %s (db=%s)", *addr, *dbPath)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	_ = httpSrv.Shutdown(context.Background())
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

const dashboardHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>orcha</title>
<style>
 body{font:13px/1.4 ui-monospace,monospace;margin:0;background:#0d1117;color:#c9d1d9}
 header{padding:8px 12px;background:#161b22;border-bottom:1px solid #30363d}
 .wrap{display:flex}
 main{flex:1;padding:12px}
 aside{width:260px;padding:12px;border-left:1px solid #30363d}
 table{width:100%;border-collapse:collapse}
 td,th{text-align:left;padding:4px 8px;border-bottom:1px solid #21262d}
 .badge{padding:1px 6px;border-radius:8px;background:#30363d}
 .needs{background:#9e6a03}
 h2{font-size:12px;text-transform:uppercase;color:#8b949e}
</style></head>
<style>
 #sessions tr{cursor:pointer}
 #sessions tr.sel{background:#1f6feb33}
 #term{background:#000;color:#d0d0d0;padding:8px;border:1px solid #30363d;border-radius:4px;
   white-space:pre;overflow:auto;max-height:60vh;min-height:120px;font-size:12px}
 .attach{color:#3fb950;font-size:11px;margin:4px 0}
 code{background:#21262d;padding:1px 4px;border-radius:3px}
</style>
<body>
<header><strong>orcha</strong> — agent team orchestrator</header>
<div class="wrap">
 <main>
  <h2>Objectives</h2>
  <table id="objs"><thead><tr><th>status</th><th>title</th><th>repo</th>
   <th>sessions</th><th>PRs</th><th>needs</th><th>activity</th></tr></thead><tbody></tbody></table>
  <h2>Sessions</h2>
  <table id="sessions"><thead><tr><th>status</th><th>role</th><th>agent</th><th>mode</th><th>title</th><th>activity</th></tr></thead><tbody></tbody></table>
  <h2>Live terminal <span id="selname" style="color:#8b949e"></span></h2>
  <div id="attach" class="attach"></div>
  <div id="term">select a running session to view its live tmux panel…</div>
 </main>
 <aside>
  <h2>Targets</h2><div id="targets"></div>
  <h2>Needs user</h2><div id="questions"></div>
  <h2>Usage</h2><div id="usage"></div>
 </aside>
</div>
<script>
async function j(u){return (await fetch(u)).json()}
let sel=null;
function pick(id,title){sel=id;document.getElementById('selname').textContent=title?('— '+title):'';drawTerm();
 document.querySelectorAll('#sessions tr').forEach(r=>r.classList.toggle('sel',r.dataset.id===id));}
async function refresh(){
 const objs=await j('/api/objectives');
 document.querySelector('#objs tbody').innerHTML=objs.map(o=>
  '<tr><td><span class="badge">'+o.status+'</span></td><td>'+o.title+'</td><td>'+(o.repo||'')+
  '</td><td>'+o.active_sessions+'</td><td>'+o.pr_count+'</td><td>'+
  (o.needs_user?'<span class="badge needs">user</span>':'')+'</td><td>'+(o.latest_activity||'')+'</td></tr>').join('');
 const ss=await j('/api/sessions');
 document.querySelector('#sessions tbody').innerHTML=ss.map(s=>
  '<tr data-id="'+s.id+'" onclick="pick(\''+s.id+'\',\''+(s.title||s.role)+'\')"><td><span class="badge">'+s.status+
  '</span></td><td>'+s.role+'</td><td>'+s.agent+'</td><td>'+s.mode+'</td><td>'+(s.title||'')+'</td><td>'+(s.current_activity||'')+'</td></tr>').join('');
 if(sel)document.querySelectorAll('#sessions tr').forEach(r=>r.classList.toggle('sel',r.dataset.id===sel));
 const t=await j('/api/targets');
 document.getElementById('targets').innerHTML=t.map(x=>x.name+' ['+x.status+'] '+x.available_sessions+'/'+x.capacity_sessions).join('<br>');
 const q=await j('/api/questions');
 document.getElementById('questions').innerHTML=q.length?q.map(x=>'• '+x.question).join('<br>'):'<i>none</i>';
 const u=await j('/api/usage');
 document.getElementById('usage').innerHTML=u.length?u.map(x=>x.provider+': '+x.state).join('<br>'):'<i>n/a</i>';
}
async function drawTerm(){
 if(!sel)return;
 const r=await fetch('/api/sessions/'+sel+'/screen');
 const term=document.getElementById('term'),att=document.getElementById('attach');
 if(r.status===204){term.textContent='(no live terminal — session not running or not a tmux session)';att.textContent='';return;}
 const d=await r.json();
 term.textContent=d.screen||'(empty)';
 att.innerHTML=d.attach?('attach live: <code>'+d.attach+'</code>'):'';
}
refresh();setInterval(refresh,2000);
setInterval(drawTerm,1000);
</script>
</body></html>`

func dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}
