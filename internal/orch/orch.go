// Package orch is the orchestrator: it turns the persisted model into running
// work. It owns scheduling, locks, loop guards, the PR workflow, and the
// manager tool surface. It never holds a DB write transaction across a model,
// GitHub, shell, or SSH call — those happen between short store operations.
package orch

import (
	"errors"
	"sync"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/forge"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/store"
	"github.com/nathanwhit/orcha/internal/workspace"
)

// GuardConfig holds the loop-guard thresholds. When any trips, the offending
// session/objective is paused and the user/manager is asked for direction
// instead of spending more model calls.
type GuardConfig struct {
	MaxSameErrorRetries    int // identical error N times -> pause
	MaxNoProgressTurns     int // N turns with no new progress -> pause
	MaxModelCallsPerObjHr  int // per-objective model calls per hour
	MaxModelCallsPerSessHr int // per-session model calls per hour
	MaxUsagePercent        float64
}

// DefaultGuards returns conservative defaults.
func DefaultGuards() GuardConfig {
	return GuardConfig{
		MaxSameErrorRetries:    3,
		MaxNoProgressTurns:     5,
		MaxModelCallsPerObjHr:  300,
		MaxModelCallsPerSessHr: 120,
		MaxUsagePercent:        0.95,
	}
}

// Config configures an Orchestrator.
type Config struct {
	Guards GuardConfig
	// ProviderFallback is the ordered preference of providers when the primary
	// is exhausted. Empty means "ask the user".
	ProviderFallback []model.AgentKind
	// ManagerMCPBaseURL is the base URL where the manager MCP tool surface is
	// served (e.g. "http://127.0.0.1:8080"). When set, manager sessions are
	// launched with the orcha tools wired in at <base>/mcp/<sessionID>. Empty
	// disables manager tool-calling.
	ManagerMCPBaseURL string
	// WorkerPermissionMode is the agent permission mode for coding worker
	// sessions running in isolated checkouts (default "acceptEdits" — edits only;
	// "bypassPermissions" also lets them build/test/commit). Managers always run
	// with "default".
	WorkerPermissionMode string
	// MCPTunnelPort is the loopback port on SSH targets where the orchestrator's
	// HTTP port is exposed via a managed reverse tunnel, so remote agents can
	// reach their MCP tools (default 18080).
	MCPTunnelPort int
}

// Orchestrator coordinates sessions across targets and providers.
type Orchestrator struct {
	st        *store.Store
	cfg       Config
	providers map[model.AgentKind]agent.Provider
	forge     forge.Forge
	preparer  *workspace.Preparer
	notify    func() // optional scheduler wake hook

	mu     sync.Mutex
	guards map[string]*guardState // keyed by session id
	runs   map[string]*run        // active runs keyed by session id

	tunnelMu sync.Mutex
	tunnels  map[string]*mcpTunnel // reverse MCP tunnels keyed by target id

	adoptMu sync.Mutex // serializes PR adoption so concurrent scans can't double-record a PR

	pokeMu   sync.Mutex           // guards lastPoke
	lastPoke map[string]time.Time // per-objective last supervisor re-poke time (cooldown)

	gcMu sync.Mutex // held during a workspace-reclaim pass so passes don't overlap
}

// SetNotify installs a hook called whenever schedulable state changes (a
// session is created, freed capacity, or reached a terminal state). The
// scheduler uses this to react promptly instead of only on its tick.
func (o *Orchestrator) SetNotify(fn func()) { o.notify = fn }

func (o *Orchestrator) notifyChange() {
	if o.notify != nil {
		o.notify()
	}
}

// SetForge installs the code-host/VCS backend used by the PR workflow.
func (o *Orchestrator) SetForge(f forge.Forge) { o.forge = f }

// SetWorkspacePreparer installs the real git checkout backend. When set,
// isolated and PR-branch workspaces are materialized as fresh git checkouts on
// the session's target. When nil (e.g. in unit tests), workspace rows are
// recorded without a real checkout.
func (o *Orchestrator) SetWorkspacePreparer(p *workspace.Preparer) { o.preparer = p }

// New builds an Orchestrator.
func New(st *store.Store, cfg Config) *Orchestrator {
	if cfg.Guards == (GuardConfig{}) {
		cfg.Guards = DefaultGuards()
	}
	if cfg.WorkerPermissionMode == "" {
		cfg.WorkerPermissionMode = "bypassPermissions"
	}
	if cfg.MCPTunnelPort == 0 {
		cfg.MCPTunnelPort = 18080
	}
	return &Orchestrator{
		st:        st,
		cfg:       cfg,
		providers: map[model.AgentKind]agent.Provider{},
		guards:    map[string]*guardState{},
		runs:      map[string]*run{},
		tunnels:   map[string]*mcpTunnel{},
		lastPoke:  map[string]time.Time{},
	}
}

// Store exposes the underlying store.
func (o *Orchestrator) Store() *store.Store { return o.st }

// RegisterProvider makes an agent provider available for scheduling.
func (o *Orchestrator) RegisterProvider(p agent.Provider) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.providers[p.Kind()] = p
}

func (o *Orchestrator) provider(kind model.AgentKind) (agent.Provider, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.providers[kind]
	return p, ok
}

// emit records a transcript message for a session. This is the only path raw
// stdout/stderr take — into the transcript, never the artifact wall.
func (o *Orchestrator) emit(sessionID string, source model.MessageSource, kind model.MessageKind, content string, meta model.JSONMap) error {
	return o.st.AppendMessage(&model.Message{
		SessionID: sessionID,
		Source:    source,
		Kind:      kind,
		Content:   content,
		Metadata:  meta,
	})
}

// audit appends an audit/history event.
func (o *Orchestrator) audit(objectiveID, sessionID, typ, summary string, data model.JSONMap) {
	_, _ = o.st.AppendEvent(model.Event{
		ObjectiveID: objectiveID,
		SessionID:   sessionID,
		Type:        typ,
		Summary:     summary,
		Data:        data,
	})
}

// ErrNoProvider is returned when no usable provider is available.
var ErrNoProvider = errors.New("orch: no usable provider available")

// workspaceLockKey / prBranchLockKey / managerLockKey build canonical lock keys.
func workspaceLockKey(workspaceID string) string { return "workspace:" + workspaceID }
func prBranchLockKey(prID string) string         { return "pr_branch:" + prID }
func managerLockKey(objectiveID string) string   { return "objective_manager:" + objectiveID }
