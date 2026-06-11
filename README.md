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
| Agent runtime | `agent.Provider` | `agent.NewFake` | `agent.NewClaude` (interactive stream-json), `agent.NewCodex` (`codex exec`) |
| Execution location | `exec.Executor` | — | `exec.NewLocal` (process group), `exec.NewSSH` (`ssh -tt`) |
| Code host / VCS | `forge.Forge` | `forge.NewFake` | `forge.NewGit` (`git` + `gh`) |
| Workspace checkout | `workspace.Preparer` | (row only) | `workspace.New` (mirror cache + fresh-upstream clone) |

`cmd/orcha` flags: `-fake-agents` (offline agents) and `-real-forge` (git+gh
forge **and** real workspace checkouts). Live backend tests are gated behind
`ORCHA_CLAUDE_LIVE`, `ORCHA_SSH_TEST_HOST`, and `ORCHA_GH_LIVE`.

**Workspace freshness:** every prepared workspace is based on the latest
upstream. A per-target bare mirror cache gives clone speed and build/cache
locality, but the isolated checkout always re-fetches from the real origin and
branches off the freshly-fetched base — never a stale local copy. PR follow-up
workspaces check out the PR branch at its fresh head. Preparation runs through
the `exec.Executor`, so it works identically on local and SSH targets.

**Still not built:** a continuous **scheduler driver loop** that pulls queued
sessions and starts them (the selection/lock/capacity primitives exist; the loop
that calls them on a tick does not).
