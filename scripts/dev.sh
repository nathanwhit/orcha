#!/bin/sh
# Build-and-run supervisor for orcha.
#
#   ./scripts/dev.sh -tmux -real-forge
#
# POST /api/restart (or the dashboard's restart button) makes the server exit
# with code 42 after a graceful shutdown; this loop then rebuilds from the
# current source and relaunches with the same flags. Restarting is safe for
# in-flight work: live sessions stay recoverable and tmux sessions are
# re-adopted with their context intact.
#
# Each (re)start first pulls the latest source: `jj git fetch` + rebase of the
# local stack onto the fresh trunk in a jj repo (a plain `git pull` fails on
# jj's detached HEAD), or `git pull --ff-only` otherwise. A pull/rebase failure
# is logged and the current source is used — it never blocks the restart.
#
#   ORCHA_NO_PULL=1       skip pulling entirely
#   ORCHA_PULL_REMOTE=…   git remote to pull from (default origin) — e.g. on a
#                         machine whose clone is a fork, pull upstream instead:
#                         ORCHA_PULL_REMOTE=upstream ORCHA_PULL_BRANCH=main
#   ORCHA_PULL_BRANCH=…   branch to pull (default: the current branch's own)
#
# Any other exit code stops the loop (Ctrl-C included).
set -u
cd "$(dirname "$0")/.."
mkdir -p bin

update_source() {
  [ "${ORCHA_NO_PULL:-}" = "1" ] && return 0
  if [ -d .jj ]; then
    if ! jj git fetch; then
      echo "dev.sh: jj git fetch failed; continuing with local source" >&2
      return 0
    fi
    # Keep local work stacked on the freshly fetched trunk; conflicts surface
    # in `jj st` and in the build below.
    jj rebase -d 'trunk()' || echo "dev.sh: rebase onto trunk failed; continuing" >&2
  elif [ -d .git ]; then
    # shellcheck disable=SC2086 — an empty branch must expand to no argument
    git pull --ff-only "${ORCHA_PULL_REMOTE:-origin}" ${ORCHA_PULL_BRANCH:-} ||
      echo "dev.sh: git pull failed; continuing with local source" >&2
  fi
}

# The dashboard is embedded into the binary from internal/webui/dist (build
# output, not committed) — rebuild it so the served UI matches the source. A
# box without npm still gets a working server: the binary falls back to a
# "UI not built" page and the API is unaffected.
build_ui() {
  if ! command -v npm >/dev/null 2>&1; then
    echo "dev.sh: npm not found; serving the previously built UI (if any)" >&2
    return 0
  fi
  [ -d ui/node_modules ] || (cd ui && npm install) || return 1
  (cd ui && npm run build) || return 1
}

while :; do
  update_source
  if ! build_ui; then
    echo "dev.sh: UI build failed; fix the code — retrying in 3s" >&2
    sleep 3
    continue
  fi
  if ! go build -o bin/orcha ./cmd/orcha; then
    echo "dev.sh: build failed; fix the code — retrying in 3s" >&2
    sleep 3
    continue
  fi
  ./bin/orcha "$@"
  code=$?
  if [ "$code" -ne 42 ]; then
    echo "dev.sh: orcha exited with code $code (not a restart request); stopping" >&2
    exit "$code"
  fi
  echo "dev.sh: restart requested; rebuilding..." >&2
done
