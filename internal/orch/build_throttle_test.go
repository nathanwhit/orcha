package orch

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/model"
)

func TestWithBuildThrottleInjectsWorkerEnv(t *testing.T) {
	o, _ := newTestOrch(t)
	o.cfg.MaxRustBuildsPerTarget = 2
	o.cfg.CargoBuildJobs = 6
	o.cfg.BuildLeaseTimeout = time.Minute
	o.cfg.BuildLeaseStaleAfter = 2 * time.Minute

	root := t.TempDir()
	tgt := &model.Target{Name: "local", Kind: model.TargetLocal, Status: model.TargetOnline, WorkRoot: root}
	sess := &model.Session{ID: "sess1", Role: model.RoleImplementer}
	spec := o.withBuildThrottle(context.Background(), sess, tgt, agent.Spec{})

	env := envMap(spec.Env)
	if !strings.HasPrefix(env["PATH"], filepath.Join(root, ".orcha", "bin")+":") {
		t.Fatalf("PATH %q does not start with shim dir", env["PATH"])
	}
	if env["ORCHA_BUILD_MAX_SLOTS"] != "2" {
		t.Fatalf("max slots env=%q, want 2", env["ORCHA_BUILD_MAX_SLOTS"])
	}
	if env["ORCHA_CARGO_BUILD_JOBS"] != "6" {
		t.Fatalf("cargo jobs env=%q, want 6", env["ORCHA_CARGO_BUILD_JOBS"])
	}
	if _, err := os.Stat(filepath.Join(root, ".orcha", "bin", "cargo")); err != nil {
		t.Fatalf("shim not installed: %v", err)
	}
}

func TestWithBuildThrottleSkipsManagersAndDisabledConfig(t *testing.T) {
	o, _ := newTestOrch(t)
	root := t.TempDir()
	tgt := &model.Target{Name: "local", Kind: model.TargetLocal, Status: model.TargetOnline, WorkRoot: root}

	o.cfg.MaxRustBuildsPerTarget = 1
	manager := &model.Session{ID: "mgr", Role: model.RoleManager}
	if spec := o.withBuildThrottle(context.Background(), manager, tgt, agent.Spec{}); len(spec.Env) != 0 {
		t.Fatalf("manager got build throttle env: %v", spec.Env)
	}

	o.cfg.MaxRustBuildsPerTarget = 0
	worker := &model.Session{ID: "worker", Role: model.RoleImplementer}
	if spec := o.withBuildThrottle(context.Background(), worker, tgt, agent.Spec{}); len(spec.Env) != 0 {
		t.Fatalf("disabled throttle still injected env: %v", spec.Env)
	}
}

func TestBuildThrottleShimRemovesStaleSlot(t *testing.T) {
	root := t.TempDir()
	realCargo := writeFakeCargo(t, root)
	shim := writeShim(t, root)
	lockRoot := filepath.Join(root, "locks")
	stale := filepath.Join(lockRoot, "rust.0.lock")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pid"), []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hb := filepath.Join(stale, ".heartbeat")
	if err := os.WriteFile(hb, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(hb, old, old); err != nil {
		t.Fatal(err)
	}

	out, err := runShim(t, shim, realCargo, lockRoot, "build")
	if err != nil {
		t.Fatalf("shim: %v\n%s", err, out)
	}
	if !strings.Contains(out, "real-cargo build") {
		t.Fatalf("real cargo did not run, output:\n%s", out)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale slot still exists or stat failed: %v", err)
	}
}

func TestBuildThrottleShimTimeoutFailsOpen(t *testing.T) {
	root := t.TempDir()
	realCargo := writeFakeCargo(t, root)
	shim := writeShim(t, root)
	lockRoot := filepath.Join(root, "locks")
	active := filepath.Join(lockRoot, "rust.0.lock")
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, ".heartbeat"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out, err := runShim(t, shim, realCargo, lockRoot, "build")
	if err != nil {
		t.Fatalf("shim: %v\n%s", err, out)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("fail-open took too long: %s", time.Since(start))
	}
	if !strings.Contains(out, "waited 1s for a build slot") {
		t.Fatalf("missing timeout warning, output:\n%s", out)
	}
	if !strings.Contains(out, "real-cargo build") {
		t.Fatalf("real cargo did not run after timeout, output:\n%s", out)
	}
}

func writeFakeCargo(t *testing.T, root string) string {
	t.Helper()
	p := filepath.Join(root, "real-cargo")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho real-cargo \"$@\"\necho jobs=${CARGO_BUILD_JOBS:-}\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeShim(t *testing.T, root string) string {
	t.Helper()
	p := filepath.Join(root, "cargo")
	if err := os.WriteFile(p, []byte(buildThrottleShim), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func runShim(t *testing.T, shim, realCargo, lockRoot string, args ...string) (string, error) {
	t.Helper()
	cmd := osexec.Command(shim, args...)
	cmd.Env = append(os.Environ(),
		"ORCHA_REAL_CARGO="+realCargo,
		"ORCHA_ORIGINAL_PATH="+os.Getenv("PATH"),
		"ORCHA_BUILD_LOCK_ROOT="+lockRoot,
		"ORCHA_BUILD_LOCK_KEY=rust",
		"ORCHA_BUILD_MAX_SLOTS=1",
		"ORCHA_BUILD_WAIT_TIMEOUT_SECS=1",
		"ORCHA_BUILD_STALE_SECS=1800",
		"ORCHA_CARGO_BUILD_JOBS=7",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			m[k] = v
		}
	}
	return m
}
