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
# Any other exit code stops the loop (Ctrl-C included).
set -u
cd "$(dirname "$0")/.."
mkdir -p bin
while :; do
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
