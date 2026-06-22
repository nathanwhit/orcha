package orch

import (
	"errors"
	"testing"
	"time"

	"github.com/nathanwhit/orcha/internal/model"
)

func TestParseLoadOutput(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		wantPC float64
		wantMB int
		wantOK bool
	}{
		{"normal", "load 3.0 cores 6 memkb 8388608", 0.5, 8192, true},
		{"idle", "load 0.12 cores 4 memkb 1048576", 0.03, 1024, true},
		{"missing mem token", "load 1.0 cores 2 memkb", 0.5, 0, true},
		{"no cores fails open", "load 1.0 cores memkb 1024", 0, 0, false},
		{"no load fails open", "load cores 4 memkb 1024", 0, 0, false},
		{"garbage fails open", "totally unrelated text", 0, 0, false},
		{"zero cores fails open", "load 1.0 cores 0 memkb 1024", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pc, mb, ok := parseLoadOutput(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if pc != c.wantPC || mb != c.wantMB {
				t.Fatalf("got perCore=%v mb=%d, want %v %d", pc, mb, c.wantPC, c.wantMB)
			}
		})
	}
}

// The load gate must skip a saturated target and place on a quieter one even
// when the saturated box has far more free capacity slots.
func TestSelectTarget_SkipsOverloadedPicksQuieter(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.MaxLoadPerCore = 1.5
	small := addTarget(t, st, "small", model.TargetLocal, 4)
	big := addTarget(t, st, "big", model.TargetSSH, 16)
	// big is hammered (2.0/core), small is idle — without the gate, big wins on
	// capacity; with it, small must be chosen.
	mustSetLoad(t, st, big.ID, 2.0)
	mustSetLoad(t, st, small.ID, 0.2)

	got, err := o.SelectTarget(TargetRequest{})
	if err != nil {
		t.Fatalf("SelectTarget: %v", err)
	}
	if got.ID != small.ID {
		t.Fatalf("placed on %s, want the quieter target %s", got.Name, small.Name)
	}
}

// When every schedulable target is over the load ceiling, placement is refused
// (ErrNoTarget) so the session parks as waiting_capacity until load drops —
// backpressure rather than piling on.
func TestSelectTarget_AllOverloadedReturnsNoTarget(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.MaxLoadPerCore = 1.5
	a := addTarget(t, st, "a", model.TargetLocal, 4)
	b := addTarget(t, st, "b", model.TargetSSH, 16)
	mustSetLoad(t, st, a.ID, 1.6)
	mustSetLoad(t, st, b.ID, 3.0)

	_, err := o.SelectTarget(TargetRequest{})
	if !errors.Is(err, ErrNoTarget) {
		t.Fatalf("err=%v, want ErrNoTarget", err)
	}
}

// With load-aware scheduling disabled (MaxLoadPerCore<=0), load is ignored and
// the most-free-capacity target wins as before — even if it is hammered.
func TestSelectTarget_GateDisabledIgnoresLoad(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.MaxLoadPerCore = 0 // disabled
	addTarget(t, st, "small", model.TargetLocal, 4)
	big := addTarget(t, st, "big", model.TargetSSH, 16)
	mustSetLoad(t, st, big.ID, 9.0) // would be gated if enabled

	got, err := o.SelectTarget(TargetRequest{})
	if err != nil {
		t.Fatalf("SelectTarget: %v", err)
	}
	if got.ID != big.ID {
		t.Fatalf("placed on %s, want highest-capacity %s when gating is off", got.Name, big.Name)
	}
}

// A stale load sample must not gate a target forever: past loadStaleAfter the
// reading is ignored and the target schedules as if no data exists.
func TestSelectTarget_StaleLoadFailsOpen(t *testing.T) {
	o, st := newTestOrch(t)
	o.cfg.MaxLoadPerCore = 1.5
	old := st.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	tgt := &model.Target{
		Name: "stale", Kind: model.TargetSSH, Status: model.TargetOnline,
		WorkRoot: "/w", CapacitySessions: 8,
		Metadata: model.JSONMap{"load_per_core": 5.0, "load_probed_at": old},
	}
	if err := st.CreateTarget(tgt); err != nil {
		t.Fatalf("create target: %v", err)
	}

	got, err := o.SelectTarget(TargetRequest{})
	if err != nil {
		t.Fatalf("stale sample should fail open, got: %v", err)
	}
	if got.ID != tgt.ID {
		t.Fatalf("placed on %s, want %s", got.Name, tgt.Name)
	}
}

// SetTargetLoad persists the sample and targetLoadPerCore reads it back fresh.
func TestSetTargetLoad_RoundTrip(t *testing.T) {
	o, st := newTestOrch(t)
	tgt := addTarget(t, st, "t", model.TargetSSH, 8)
	if err := st.SetTargetLoad(tgt.ID, 0.75, 2048, st.Now()); err != nil {
		t.Fatalf("SetTargetLoad: %v", err)
	}
	reread, err := st.GetTarget(tgt.ID)
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}
	lpc, ok := o.targetLoadPerCore(reread, st.Now())
	if !ok {
		t.Fatal("expected a fresh load sample after SetTargetLoad")
	}
	if lpc != 0.75 {
		t.Fatalf("load_per_core=%v, want 0.75", lpc)
	}
}

func mustSetLoad(t *testing.T, st interface {
	SetTargetLoad(string, float64, int, time.Time) error
	Now() time.Time
}, id string, perCore float64) {
	t.Helper()
	if err := st.SetTargetLoad(id, perCore, 4096, st.Now()); err != nil {
		t.Fatalf("SetTargetLoad: %v", err)
	}
}
