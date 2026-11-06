package scheduler

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler/conditions"
)

// fakePreflightDaemon implements TriggerDaemon (BeadClient + AncestorClient)
// for cron-preflight gating tests. total is the bead_query result count.
type fakePreflightDaemon struct {
	total int
	calls int
}

func (f *fakePreflightDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.calls++
	return &beadsapi.ListBeadsResult{Total: f.total}, nil
}

func (f *fakePreflightDaemon) IsAncestor(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func cronPreflightEntry(t *testing.T) TriggerEntry {
	t.Helper()
	expr, err := conditions.ParseExpression(`{"all":[{"type":"bead_query","op":">","value":"0","parent":"gd-Epic","types":["task"],"statuses":["open","in_progress","blocked"]}]}`)
	if err != nil {
		t.Fatalf("ParseExpression: %v", err)
	}
	return TriggerEntry{
		Kind:      TriggerKindCron,
		Cron:      "0 9 * * *",
		Timezone:  "UTC",
		Preflight: &expr,
	}
}

// TestEvaluateTriggers_CronPreflightGates is the regression lock for
// ka-0000007730: a cron entry with a preflight must only fire when the
// daemon-side predicate holds. Before the fix, Preflight was parsed but
// never evaluated, so the cron floor fired unconditionally.
func TestEvaluateTriggers_CronPreflightGates(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

	// (1) preflight holds (epic has open children) -> cron fires.
	t.Run("holds_fires", func(t *testing.T) {
		e := NewEntryEngine()
		d := &fakePreflightDaemon{total: 3}
		tctx := TriggerEvalContext{TriggerID: "s", Timezone: "UTC", Project: "gardenio",
			Triggers: []TriggerEntry{cronPreflightEntry(t)}}
		dec := EvaluateTriggers(context.Background(), e, d, quietLogger(), tctx, now)
		if !dec.Fire || dec.Kind != TriggerKindCron {
			t.Fatalf("expected cron fire when preflight holds, got %+v (daemon calls=%d)", dec, d.calls)
		}
	})

	// (2) preflight fails (no open children) -> suppressed.
	t.Run("fails_suppressed", func(t *testing.T) {
		e := NewEntryEngine()
		d := &fakePreflightDaemon{total: 0}
		tctx := TriggerEvalContext{TriggerID: "s", Timezone: "UTC", Project: "gardenio",
			Triggers: []TriggerEntry{cronPreflightEntry(t)}}
		if EvaluateTriggers(context.Background(), e, d, quietLogger(), tctx, now).Fire {
			t.Error("cron fire should be suppressed when preflight predicate is false")
		}
		if d.calls == 0 {
			t.Error("daemon should have been consulted for preflight")
		}
	})

	// (3) preflight present but daemon nil -> fail-closed (suppressed).
	t.Run("nil_daemon_suppressed", func(t *testing.T) {
		e := NewEntryEngine()
		tctx := TriggerEvalContext{TriggerID: "s", Timezone: "UTC",
			Triggers: []TriggerEntry{cronPreflightEntry(t)}}
		if EvaluateTriggers(context.Background(), e, nil, quietLogger(), tctx, now).Fire {
			t.Error("cron fire should be suppressed (fail-closed) when preflight present but daemon nil")
		}
	})

	// (4) no preflight -> fires unconditionally (no regression to bare cron).
	t.Run("no_preflight_fires", func(t *testing.T) {
		e := NewEntryEngine()
		tctx := TriggerEvalContext{TriggerID: "s", Timezone: "UTC",
			Triggers: []TriggerEntry{{Kind: TriggerKindCron, Cron: "0 9 * * *", Timezone: "UTC"}}}
		if !EvaluateTriggers(context.Background(), e, nil, quietLogger(), tctx, now).Fire {
			t.Error("cron entry without preflight must fire unconditionally (bare-cron behavior)")
		}
	})
}
