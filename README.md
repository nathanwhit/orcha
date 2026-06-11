# orcha — Agent Team Orchestrator

A local/private orchestrator for supervising a team of AI agents working on
software projects. The core question it answers is *who is working on what, what
do they need, what changed, and where is it running* — not "here are many task
logs".

## Conceptual model (intentionally small)

```
objective ── sessions ── targets (where they run)
          ├─ workspaces (isolated / shared scratch / pr_branch)
          ├─ pull requests ── pr_feedback
          ├─ questions (needs-user)
          └─ artifacts (durable outputs, not stdout)
```

A **manager** is just a session with `role=manager`. Every objective starts with
one. Work can produce zero, one, or many PRs and can run locally or on SSH
targets.

## Layout

| Package | Responsibility |
|---|---|
| `internal/model` | Domain types + the **state machines** (session/objective transitions, target scheduling, PR push safety). |
| `internal/store` | SQLite (WAL) persistence. Normalized current-state tables; `events` is audit history. Dashboard queries return small rows; transcripts load separately by `seq` cursor. Atomic locks and target-slot claims. |
| `internal/agent` | The provider runtime contract (`start/send/cancel/resume/read_events`) + an in-process scriptable `FakeProvider`. |
| `internal/forge` | Git + code-host abstraction (push / open / refresh PR) with an in-memory `Fake`. |
| `internal/orch` | The orchestrator: scheduling, locks, loop guards, the session driver, steering, the PR workflow, the PR-feedback monitor, and the manager tool surface. |
| `internal/api` | HTTP API + incremental SSE streaming. |
| `cmd/orcha` | Wires it together with fakes and a dense dashboard; runnable with no external services. |

## Run

```sh
go run ./cmd/orcha            # serves :8080 with fake providers + fake forge
curl -X POST localhost:8080/api/objectives \
  -d '{"title":"Port subsystem to Rust","prompt":"...","agent":"claude"}'
open http://localhost:8080/   # dense dashboard
```

## Safety invariants enforced (with tests)

- **No late completion resurrects canceled work** — terminal session statuses
  have no outgoing transitions; the store validates transitions inside a
  transaction (`store.UpdateSessionStatus`, `model.ValidateSessionTransition`).
- **One writer per workspace / one updater per PR branch** — row-unique locks.
- **Draining targets accept no new sessions; capacity is bounded** — atomic
  `ClaimTargetSlot`.
- **Never push to a merged PR; a closed PR creates a manager decision** —
  `model.EvaluatePush` + `orch.UpdatePR`.
- **Force push requires an explicit, recorded reason.**
- **Loop guards** (same-error, no-progress, per-session model-call cap) pause the
  session, record a compact reason, and open a question — instead of burning
  quota.
- **Usage exhaustion** falls back to another provider or asks the user; it never
  retries an exhausted provider.
- **No long external op inside a DB transaction** — model/forge/SSH calls happen
  between short store operations.
- **Dashboard stays small** — dashboard queries select no transcript content;
  transcripts paginate via `/api/sessions/:id/messages?after=` and stream via
  `/api/sessions/:id/stream`.

## Tests

```sh
go test ./...          # unit + integration
go test -race ./...    # the session driver is concurrent
```

The suite covers the spec's required unit and integration tests, including:
objectives opening two independent PRs, PR feedback spawning a follow-up while
another worker runs, interactive vs non-interactive steering, remote log
streaming, remote cancel killing the process group, and the small-dashboard
guarantee under large transcripts.

## Backends

The orchestrator core (model, storage, scheduler primitives, locks, guards, PR
workflow, feedback monitor, manager tools, HTTP API) runs against pluggable
backends. Both fakes (for offline/tested flows) and real implementations exist:

| Concern | Interface | Fake | Real |
|---|---|---|---|
| Agent runtime | `agent.Provider` | `agent.NewFake` | `agent.NewClaude` (interactive stream-json), `agent.NewCodex` (`codex exec`, resume-based steering), `agent.NewTmuxAgent` (interactive TUI in attachable tmux) |
| Terminal multiplexing | `tmux.Controller` | — | real `tmux` over local/SSH |
| Remote machines | `exec.SSHExecutor` | — | real `ssh` targets (register, health-check, bootstrap) |
| Execution location | `exec.Executor` | — | `exec.NewLocal` (process group), `exec.NewSSH` (`ssh -tt`) |
| Code host / VCS | `forge.Forge` | `forge.NewFake` | `forge.NewGit` (`git` + `gh`) |
| Workspace checkout | `workspace.Preparer` | (row only) | `workspace.New` (mirror cache + fresh-upstream clone) |
| Manager tools | `mcp` server | (call orch directly) | `Orchestrator.ManagerMCPHandler` (MCP over HTTP) |

`cmd/orcha` flags: `-fake-agents` (offline agents) and `-real-forge` (git+gh
forge **and** real workspace checkouts). Live backend tests are gated behind
`ORCHA_CLAUDE_LIVE`, `ORCHA_SSH_TEST_HOST`, and `ORCHA_GH_LIVE`.

**Auto-prep for spawned workers:** when an objective carries a repo
(`repo`/`base_branch` on the objective, or a per-spawn override on
`spawn_session`), coding sessions the manager spawns get a fresh isolated
checkout automatically. The checkout is prepared at start time on the session's
placed target (so the clone and the agent run on the same machine), branched off
the latest upstream. This closes the loop: objective → manager spawns worker →
worker runs in a real checkout → `publish_pr`.

**Workspace freshness:** every prepared workspace is based on the latest
upstream. A per-target bare mirror cache gives clone speed and build/cache
locality, but the isolated checkout always re-fetches from the real origin and
branches off the freshly-fetched base — never a stale local copy. PR follow-up
workspaces check out the PR branch at its fresh head. Preparation runs through
the `exec.Executor`, so it works identically on local and SSH targets.

## Scheduler driver

`orch.Scheduler` makes the system self-driving: each tick it finds runnable
queued/waiting-capacity sessions and starts them, respecting declared
dependencies, a global concurrency cap, per-target capacity, workspace/PR-branch
locks, and provider usage. It reacts promptly via a wake hook
(`SetNotify`) when work is created or completes, falling back to an idle tick.
Creating an objective therefore auto-starts its manager, which spawns workers
the scheduler then runs — no manual restart. Dependencies that fail/cancel cancel
their dependents; exhausted providers park the session and ask the user instead
of respinning. Flags: `-max-concurrent`, `-schedule-interval`.

## Manager tool-calling

Every objective starts with a manager session. The manager *agent* drives the
orchestrator through tools exposed over MCP: `Orchestrator.ManagerMCPHandler`
serves a minimal Streamable-HTTP MCP server (mounted at `/mcp/<sessionID>`), and
each manager session's Claude is launched with `--mcp-config` pointing at its own
endpoint (`Config.ManagerMCPBaseURL`). Tool calls — `spawn_session`, `ask_user`,
`publish_pr`, `update_pr`, `comment_pr`, `create_note`, `mark_objective_done`,
`cancel_session` — are scoped to the calling session and execute the
corresponding orchestrator methods. Spawned workers are then auto-started by the
scheduler.

Verified live (`ORCHA_LIVE_MANAGER=1`): a real Claude manager connects over HTTP
and calls `spawn_session`, creating a worker in the orchestrator.

## Remote (SSH) machines

Register an SSH box and sessions run there — agent, tmux pane, git checkout, and
all. A target carries `host`/`user`/`work_root` (plus `ssh_port`/`identity_file`/
`bootstrap` in metadata); everything routes through `exec.SSHExecutor` (which
wraps commands in `ssh -tt`), so:

- the agent's **tmux session runs on the remote host**, attachable with
  `ssh -t user@host tmux attach -t orcha-<id>`,
- **workspace checkouts** clone on the remote machine,
- **process-group cancellation** works (kill the local ssh → remote SIGHUP).

Add one via `POST /api/targets` (or the dashboard's "add ssh machine" form):

```sh
curl -X POST localhost:8080/api/targets -H 'Content-Type: application/json' -d '{
  "name":"gpu-box","kind":"ssh","host":"1.2.3.4","user":"bot",
  "work_root":"/home/bot/work","capacity_sessions":4,
  "bootstrap":"mkdir -p /home/bot/work"
}'
```

Registration runs the bootstrap command and a health check, marking the target
`online`/`offline` (re-check with `POST /api/targets/:id/healthcheck`). Direct
work to a box by pinning: `spawn_session(..., target: "gpu-box")`, or
`drain`/`disable` to take it out of rotation. **Requirements on the remote
host:** `tmux`, `git`, the agent CLIs (`claude`/`codex`) and their auth, plus
key-based SSH (the executor uses `BatchMode=yes`). Verified locally; a live
remote test is gated behind `ORCHA_SSH_TEST_HOST`.

## Interactive tmux sessions (you can watch and take over)

Run with `-tmux` and every session becomes a real, **attachable** tmux session
running the agent's interactive TUI (or a plain shell). `agent.TmuxProvider`
(over `tmux.Controller`, which works on local and SSH targets):

- starts a detached `tmux new-session` running the program in the workspace dir,
- streams the pane via `pipe-pane` into the session transcript,
- steers by typing into the live pane with `send-keys` (truly interactive),
- cancels with `kill-session`,
- and records the attach command on the session, so a human runs
  `tmux attach -t orcha-<sessionID>` (or `ssh -t <host> tmux attach -t ...` for
  SSH targets) to watch or take over live.

Verified against real tmux: interactive shell, send-keys steering, pipe-pane
streaming, and kill-session cancellation.

A tmux **manager** session is launched with the same MCP/tool flags as the
headless one (`NewTmuxClaude`), so an attachable manager can also drive the
orchestrator via its tools. The live pane renders in the dashboard: the
`GET /api/sessions/:id/screen` endpoint returns a `capture-pane` snapshot (via
the `agent.Snapshotter` capability), which the dashboard polls into a terminal
panel alongside the `tmux attach` command.

## Interactivity: Claude vs Codex (headless mode)

The two agents differ in how steering works, and the UI reflects each session's
mode (`interactive` vs `noninteractive`):

- **Claude** has a real persistent mode (`--input-format/--output-format
  stream-json`), so a session is one long-lived process and steering is just
  another user message to its stdin — verified live including multi-turn.
- **Codex** has no stable streaming-stdin equivalent; its only stable
  programmatic mode is the one-shot `codex exec` (true interactive access is the
  *experimental* app-server protocol). So Codex is non-interactive and steered
  via the spec's cancel/resume protocol — but resume **preserves the
  conversation**: Codex emits a `thread_id` on start (captured as
  `provider_session_id`), and a resumed turn uses `codex exec resume <thread_id>`
  so context carries over rather than restarting. Verified live
  (`ORCHA_CODEX_LIVE=1`): a resumed thread recalls earlier context.
