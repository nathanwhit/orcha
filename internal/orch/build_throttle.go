package orch

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/exec"
	"github.com/nathanwhit/orcha/internal/model"
)

const buildThrottleShim = `#!/bin/sh

real_cargo="$ORCHA_REAL_CARGO"
if [ -z "$real_cargo" ]; then
  real_cargo="$(PATH="$ORCHA_ORIGINAL_PATH" command -v cargo 2>/dev/null || true)"
fi

run_unthrottled() {
  if [ -n "$real_cargo" ]; then
    exec "$real_cargo" "$@"
  fi
  echo "orcha cargo throttle: real cargo not found in original PATH; running cargo without throttle" >&2
  exec env PATH="$ORCHA_ORIGINAL_PATH" cargo "$@"
}

if [ "${ORCHA_BUILD_THROTTLE:-1}" = "0" ]; then
  run_unthrottled "$@"
fi

subcmd=""
for arg in "$@"; do
  case "$arg" in
    +*) continue ;;
    -*) continue ;;
    *) subcmd="$arg"; break ;;
  esac
done

case "$subcmd" in
  build|check|test|clippy|run|bench|doc|rustc|install) ;;
  *) run_unthrottled "$@" ;;
esac

if [ -n "$ORCHA_CARGO_BUILD_JOBS" ] && [ -z "$CARGO_BUILD_JOBS" ]; then
  export CARGO_BUILD_JOBS="$ORCHA_CARGO_BUILD_JOBS"
fi

if [ -z "$real_cargo" ]; then
  echo "orcha cargo throttle: real cargo not found in original PATH; running cargo without throttle" >&2
  exec env PATH="$ORCHA_ORIGINAL_PATH" cargo "$@"
fi

lock_root="${ORCHA_BUILD_LOCK_ROOT:-/tmp/orcha-build-locks}"
lock_key="${ORCHA_BUILD_LOCK_KEY:-rust}"
max_slots="${ORCHA_BUILD_MAX_SLOTS:-1}"
wait_timeout="${ORCHA_BUILD_WAIT_TIMEOUT_SECS:-1200}"
stale_after="${ORCHA_BUILD_STALE_SECS:-1800}"

case "$max_slots" in ''|*[!0-9]*) max_slots=1 ;; esac
case "$wait_timeout" in ''|*[!0-9]*) wait_timeout=1200 ;; esac
case "$stale_after" in ''|*[!0-9]*) stale_after=1800 ;; esac
[ "$max_slots" -lt 1 ] && exec "$real_cargo" "$@"

now() { date +%s; }
mtime() { stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null || echo 0; }

cleanup_if_dead_or_stale() {
  d="$1"
  [ -d "$d" ] || return 0
  pid="$(cat "$d/pid" 2>/dev/null || true)"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    hb="$d/.heartbeat"
    [ -e "$hb" ] || hb="$d"
    age=$(( $(now) - $(mtime "$hb") ))
    if [ "$age" -lt "$stale_after" ]; then
      return 1
    fi
    echo "orcha cargo throttle: removing stale active slot $d (pid $pid, heartbeat age ${age}s)" >&2
  fi
  rm -rf "$d" 2>/dev/null || return 1
  return 0
}

acquire_slot() {
  mkdir -p "$lock_root" 2>/dev/null || return 2
  started="$(now)"
  while :; do
    i=0
    while [ "$i" -lt "$max_slots" ]; do
      d="$lock_root/$lock_key.$i.lock"
      if mkdir "$d" 2>/dev/null; then
        echo "$$" > "$d/pid" 2>/dev/null || true
        now > "$d/started_at" 2>/dev/null || true
        printf '%s\n' "$PWD" > "$d/cwd" 2>/dev/null || true
        touch "$d/.heartbeat" "$d" 2>/dev/null || true
        LOCK_DIR="$d"
        return 0
      fi
      cleanup_if_dead_or_stale "$d" >/dev/null 2>&1 || true
      i=$((i + 1))
    done
    elapsed=$(( $(now) - started ))
    if [ "$elapsed" -ge "$wait_timeout" ]; then
      return 1
    fi
    sleep 2
  done
}

LOCK_DIR=""
if acquire_slot; then
  (
    n=0
    while :; do
      if [ "$n" -eq 0 ]; then
        [ -n "$LOCK_DIR" ] && touch "$LOCK_DIR/.heartbeat" "$LOCK_DIR" 2>/dev/null || exit 0
      fi
      n=$(( (n + 1) % 30 ))
      sleep 1
    done
  ) &
  hb_pid="$!"
  cleanup() {
    kill "$hb_pid" 2>/dev/null || true
    wait "$hb_pid" 2>/dev/null || true
    [ -n "$LOCK_DIR" ] && rm -rf "$LOCK_DIR" 2>/dev/null || true
  }
  trap cleanup EXIT INT TERM HUP
else
  rc="$?"
  if [ "$rc" = "1" ]; then
    echo "orcha cargo throttle: waited ${wait_timeout}s for a build slot; running cargo without throttle" >&2
  else
    echo "orcha cargo throttle: lock root $lock_root is unavailable; running cargo without throttle" >&2
  fi
fi

if [ -n "$LOCK_DIR" ]; then
  "$real_cargo" "$@"
  rc="$?"
  exit "$rc"
fi
exec "$real_cargo" "$@"
`

// withBuildThrottle injects a target-local cargo shim for coding workers. It is
// deliberately fail-open: any installation/probe error leaves the session env
// untouched, and the shim itself runs cargo unthrottled on timeout or lock-path
// trouble.
func (o *Orchestrator) withBuildThrottle(ctx context.Context, sess *model.Session, tgt *model.Target, spec agent.Spec) agent.Spec {
	if !o.sessionUsesBuildThrottle(sess) || tgt == nil || tgt.WorkRoot == "" || o.cfg.MaxRustBuildsPerTarget <= 0 {
		return spec
	}
	env, err := o.buildThrottleEnv(ctx, tgt)
	if err != nil {
		o.audit(sess.ObjectiveID, sess.ID, "build_throttle_unavailable", err.Error(), nil)
		return spec
	}
	spec.Env = append(spec.Env, env...)
	return spec
}

func (o *Orchestrator) sessionUsesBuildThrottle(sess *model.Session) bool {
	if sess == nil || sess.Role == model.RoleManager {
		return false
	}
	return isCodingWorker(sess.Role)
}

func (o *Orchestrator) buildThrottleEnv(ctx context.Context, tgt *model.Target) ([]string, error) {
	ex := agent.NewExecutor(tgt)
	path, realCargo, err := targetCargoPath(ctx, ex)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("target PATH probe returned empty PATH")
	}

	root := strings.TrimRight(tgt.WorkRoot, "/") + "/.orcha"
	binDir := root + "/bin"
	lockRoot := root + "/build-locks"
	if _, err := exec.RunCapture(ctx, ex, exec.Command{Name: "mkdir", Args: []string{"-p", binDir, lockRoot}}); err != nil {
		return nil, err
	}
	if err := writeWorkspaceFile(ctx, ex, tgt.WorkRoot, ".orcha/bin/cargo", buildThrottleShim); err != nil {
		return nil, err
	}
	if _, err := exec.RunCapture(ctx, ex, exec.Command{Name: "chmod", Args: []string{"0755", ".orcha/bin/cargo"}, Dir: tgt.WorkRoot}); err != nil {
		return nil, err
	}

	env := []string{
		"PATH=" + binDir + ":" + path,
		"ORCHA_ORIGINAL_PATH=" + path,
		"ORCHA_REAL_CARGO=" + realCargo,
		"ORCHA_BUILD_LOCK_ROOT=" + lockRoot,
		"ORCHA_BUILD_LOCK_KEY=rust",
		"ORCHA_BUILD_MAX_SLOTS=" + strconv.Itoa(o.cfg.MaxRustBuildsPerTarget),
		"ORCHA_BUILD_WAIT_TIMEOUT_SECS=" + strconv.Itoa(seconds(o.cfg.BuildLeaseTimeout)),
		"ORCHA_BUILD_STALE_SECS=" + strconv.Itoa(seconds(o.cfg.BuildLeaseStaleAfter)),
	}
	if o.cfg.CargoBuildJobs > 0 {
		env = append(env, "ORCHA_CARGO_BUILD_JOBS="+strconv.Itoa(o.cfg.CargoBuildJobs))
	}
	return env, nil
}

func targetCargoPath(ctx context.Context, ex exec.Executor) (path, realCargo string, err error) {
	out, err := exec.Capture(ctx, ex, exec.Command{
		Name: "sh",
		Args: []string{"-lc", `printf 'path=%s\ncargo=%s\n' "$PATH" "$(command -v cargo 2>/dev/null || true)"`},
	})
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "path="):
			path = strings.TrimPrefix(line, "path=")
		case strings.HasPrefix(line, "cargo="):
			realCargo = strings.TrimPrefix(line, "cargo=")
		}
	}
	return path, realCargo, nil
}

func seconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	s := int(d.Seconds())
	if s < 1 {
		return 1
	}
	return s
}
