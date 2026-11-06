package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planspend"
)

var planSpendNow = time.Date(2026, 10, 27, 19, 30, 0, 0, time.UTC)

// planSpendFake is an in-memory planSpendDaemon: list answers by status,
// reverse deps and beads from maps, and records every field write.
type planSpendFake struct {
	open, closed []*beadsapi.BeadDetail
	rev          map[string][]beadsapi.Dependency
	beads        map[string]*beadsapi.BeadDetail
	listErr      error
	depsErr      error
	updateErr    error
	attempts     map[string]int
	queries      []beadsapi.ListBeadsQuery
	writes       map[string]map[string]any
}

func newPlanSpendFake() *planSpendFake {
	return &planSpendFake{rev: map[string][]beadsapi.Dependency{}, beads: map[string]*beadsapi.BeadDetail{}, writes: map[string]map[string]any{}}
}

func (f *planSpendFake) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.queries = append(f.queries, q)
	if f.listErr != nil {
		return nil, f.listErr
	}
	if slices.Contains(q.Statuses, "closed") {
		return &beadsapi.ListBeadsResult{Beads: f.closed}, nil
	}
	return &beadsapi.ListBeadsResult{Beads: f.open}, nil
}

func (f *planSpendFake) BatchGetDependencies(_ context.Context, ids []string, _ string) (map[string][]beadsapi.Dependency, error) {
	if f.depsErr != nil {
		return nil, f.depsErr
	}
	out := map[string][]beadsapi.Dependency{}
	for _, id := range ids {
		out[id] = f.rev[id]
	}
	return out, nil
}

func (f *planSpendFake) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	out := map[string]*beadsapi.BeadDetail{}
	for _, id := range ids {
		if b := f.beads[id]; b != nil {
			out[id] = b
		}
	}
	return out, nil
}

func (f *planSpendFake) UpdateBeadFieldsTyped(_ context.Context, id string, fields map[string]any) error {
	if f.attempts == nil {
		f.attempts = map[string]int{}
	}
	f.attempts[id]++
	if f.updateErr != nil {
		return f.updateErr
	}
	f.writes[id] = fields
	return nil
}

// crewOn assigns an agent with the given cost to target.
func (f *planSpendFake) crewOn(target, id, state, status, total string) {
	f.beads[id] = &beadsapi.BeadDetail{ID: id, Type: "agent", Status: status, UpdatedAt: planSpendNow.Add(-time.Minute),
		Fields: map[string]string{"agent_state": state, "total_cost_usd": total}}
	f.rev[target] = append(f.rev[target], beadsapi.Dependency{BeadID: id, DependsOnID: target, Type: "assigned"})
}

func planEpic(id, status string, fields map[string]string) *beadsapi.BeadDetail {
	if fields == nil {
		fields = map[string]string{}
	}
	return &beadsapi.BeadDetail{ID: id, Type: "epic", Status: status, Labels: []string{"plan"}, Fields: fields}
}

func newTestPlanSpendSync(f *planSpendFake) *planSpendSync {
	c := newPlanSpendSync(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.now = func() time.Time { return planSpendNow }
	return c
}

func TestPlanSpendSync_WritesRollupOnPlanEpic(t *testing.T) {
	f := newPlanSpendFake()
	f.open = []*beadsapi.BeadDetail{planEpic("gd-P", "in_progress", nil)}
	f.beads["gd-s1"] = &beadsapi.BeadDetail{ID: "gd-s1", Type: "task", Status: "open"}
	f.rev["gd-P"] = []beadsapi.Dependency{{BeadID: "gd-s1", DependsOnID: "gd-P", Type: "parent-child"}}
	f.crewOn("gd-s1", "gd-a", "working", "in_progress", "1.50")
	f.crewOn("gd-s1", "gd-b", "done", "closed", "2.25")

	newTestPlanSpendSync(f).syncAll(context.Background())

	w := f.writes["gd-P"]
	if w == nil {
		t.Fatal("no write on gd-P")
	}
	want := map[string]any{
		planspend.FieldUSD:      "3.7500",
		planspend.FieldSyncedAt: "2026-10-27T19:30:00Z",
		planspend.FieldCrew:     2,
		planspend.FieldLive:     1,
	}
	for k, v := range want {
		if w[k] != v {
			t.Errorf("%s = %#v, want %#v", k, w[k], v)
		}
	}
	// The list reads only the fields the sync uses.
	for _, q := range f.queries {
		if !slices.Contains(q.FieldsOnly, "phase_order") || !slices.Contains(q.FieldsOnly, planspend.FieldSyncedAt) {
			t.Errorf("list query fields_only = %v, want phase_order and the rollup fields", q.FieldsOnly)
		}
	}
}

func TestPlanSpendSync_SkipsUnchangedSettledPlan(t *testing.T) {
	f := newPlanSpendFake()
	stamp := planSpendNow.Add(-6 * time.Hour).Format(time.RFC3339)
	f.open = []*beadsapi.BeadDetail{planEpic("gd-P", "open", map[string]string{
		planspend.FieldUSD: "2.2500", planspend.FieldSyncedAt: stamp, planspend.FieldCrew: "1", planspend.FieldLive: "0",
	})}
	f.crewOn("gd-P", "gd-b", "done", "closed", "2.25")

	newTestPlanSpendSync(f).syncAll(context.Background())
	if w, ok := f.writes["gd-P"]; ok {
		t.Errorf("settled, unchanged plan was rewritten: %v", w)
	}
}

func TestPlanSpendNeedsWrite(t *testing.T) {
	read := func(usd float64, crew, live int, age time.Duration) planspend.Reading {
		return planspend.Reading{Has: true, USD: usd, Crew: crew, Live: live, SyncedAt: planSpendNow.Add(-age)}
	}
	for name, c := range map[string]struct {
		old   planspend.Reading
		r     planspend.Rollup
		final bool
		want  bool
	}{
		"never written":             {planspend.Reading{}, planspend.Rollup{}, false, true},
		"unchanged, settled":        {read(1, 1, 0, 5*time.Hour), planspend.Rollup{USD: 1.001, Crew: 1}, false, false},
		"moved a cent":              {read(1, 1, 0, time.Minute), planspend.Rollup{USD: 1.01, Crew: 1}, false, true},
		"new crew":                  {read(1, 1, 0, time.Minute), planspend.Rollup{USD: 1, Crew: 2}, false, true},
		"crew went idle":            {read(1, 1, 1, time.Minute), planspend.Rollup{USD: 1, Crew: 1}, false, true},
		"live, stamp fresh":         {read(1, 1, 1, 10*time.Minute), planspend.Rollup{USD: 1, Crew: 1, Live: 1}, false, false},
		"live, unchanged, old":      {read(1, 1, 1, 5*time.Hour), planspend.Rollup{USD: 1, Crew: 1, Live: 1}, false, false},
		"live, moved a cent":        {read(1, 1, 1, time.Minute), planspend.Rollup{USD: 1.01, Crew: 1, Live: 1}, false, true},
		"closed plan, final rollup": {read(1, 1, 0, time.Minute), planspend.Rollup{USD: 1, Crew: 1}, true, true},
	} {
		if got := planSpendNeedsWrite(c.old, c.r, c.final); got != c.want {
			t.Errorf("%s: planSpendNeedsWrite = %v, want %v", name, got, c.want)
		}
	}
}

func TestPlanSpendSync_ClosedPlanRolledUpOnceAfterClose(t *testing.T) {
	closedAt := planSpendNow.Add(-time.Hour)
	owed := planEpic("gd-owed", "closed", map[string]string{
		planspend.FieldUSD: "1.0000", planspend.FieldSyncedAt: closedAt.Add(-time.Minute).Format(time.RFC3339),
		planspend.FieldCrew: "1", planspend.FieldLive: "0",
	})
	owed.ClosedAt = closedAt.Format(time.RFC3339Nano)
	done := planEpic("gd-done", "closed", map[string]string{
		planspend.FieldUSD: "1.0000", planspend.FieldSyncedAt: closedAt.Add(time.Minute).Format(time.RFC3339),
	})
	done.ClosedAt = closedAt.Format(time.RFC3339)
	never := planEpic("gd-never", "closed", nil)
	f := newPlanSpendFake()
	f.closed = []*beadsapi.BeadDetail{owed, done, never}
	f.crewOn("gd-owed", "gd-a", "done", "closed", "1.00")

	newTestPlanSpendSync(f).syncAll(context.Background())

	// The owed plan is written even though its figure did not change, so it
	// stops being owed; the one rolled up after close is left alone.
	if f.writes["gd-owed"] == nil {
		t.Error("closed plan owed its final rollup was not written")
	}
	if f.writes["gd-never"] == nil {
		t.Error("closed plan never rolled up was not written")
	}
	if w, ok := f.writes["gd-done"]; ok {
		t.Errorf("closed plan already rolled up after close was rewritten: %v", w)
	}
}

func TestPlanSpendSync_FailuresWriteNothing(t *testing.T) {
	f := newPlanSpendFake()
	f.open = []*beadsapi.BeadDetail{planEpic("gd-P", "open", nil)}
	f.crewOn("gd-P", "gd-a", "done", "closed", "1.00")
	f.depsErr = errors.New("daemon down")
	newTestPlanSpendSync(f).syncAll(context.Background())
	if len(f.writes) != 0 {
		t.Errorf("a failed walk wrote %v", f.writes)
	}

	f = newPlanSpendFake()
	f.listErr = errors.New("daemon down")
	newTestPlanSpendSync(f).syncAll(context.Background())
	if len(f.writes) != 0 {
		t.Errorf("a failed list wrote %v", f.writes)
	}
}

// A 403 on a plan's stamp is the daemon's authz answer, not a fault: the sync
// sends it once, then leaves that plan alone on later passes until
// planSpendRefusedBackoff has passed (gd-Hk70sS1XuS: re-sending every pass was
// ~12.6k denials/day). Another error is retried next pass as before.
func TestPlanSpendSync_403BacksOff(t *testing.T) {
	f := newPlanSpendFake()
	f.open = []*beadsapi.BeadDetail{planEpic("gd-P", "open", nil)}
	f.closed = []*beadsapi.BeadDetail{func() *beadsapi.BeadDetail {
		b := planEpic("gd-C", "closed", nil)
		b.ClosedAt = planSpendNow.Add(-time.Hour).Format(time.RFC3339)
		return b
	}()}
	f.updateErr = &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "ownership denied"}
	c := newTestPlanSpendSync(f)

	now := planSpendNow
	c.now = func() time.Time { return now }
	for range 3 {
		c.syncAll(context.Background())
		now = now.Add(planspend.SyncInterval)
	}
	if f.attempts["gd-P"] != 1 || f.attempts["gd-C"] != 1 {
		t.Fatalf("attempts after three passes = %v; want one refused write per plan", f.attempts)
	}

	// Past the backoff the plan is tried again, so a grant that has since
	// landed takes effect without a restart.
	f.updateErr = nil
	now = planSpendNow.Add(planSpendRefusedBackoff)
	c.syncAll(context.Background())
	if f.writes["gd-P"] == nil || f.writes["gd-C"] == nil {
		t.Fatalf("no write after the backoff; writes=%v", f.writes)
	}
	if len(c.refused) != 0 {
		t.Fatalf("refused = %v after a landed write; want empty", c.refused)
	}
}

func TestPlanSpendSync_OtherErrorsRetryNextPass(t *testing.T) {
	f := newPlanSpendFake()
	f.open = []*beadsapi.BeadDetail{planEpic("gd-P", "open", nil)}
	f.updateErr = &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "busy"}
	c := newTestPlanSpendSync(f)
	c.syncAll(context.Background())
	c.syncAll(context.Background())
	if f.attempts["gd-P"] != 2 {
		t.Fatalf("attempts = %d; a non-403 failure must be retried each pass", f.attempts["gd-P"])
	}
}
