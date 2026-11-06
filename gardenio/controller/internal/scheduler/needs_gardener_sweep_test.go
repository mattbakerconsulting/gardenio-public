package scheduler

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

func TestSelectGardenerless(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)     // older than the 15m grace period
	fresh := now.Add(-1 * time.Minute) // inside the grace period
	cfg := NeedsGardenerSweepConfig{}  // defaults: needs-gardener, 15m grace

	cases := []struct {
		name  string
		agent GardenerlessAgent
		want  bool
	}{
		{
			name:  "gardenerless old agent is a leak",
			agent: GardenerlessAgent{ID: "gd-1", HasRealGardener: false, CreatedAt: old},
			want:  true,
		},
		{
			name:  "agent owned by a real gardener is not surfaced",
			agent: GardenerlessAgent{ID: "gd-2", HasRealGardener: true, CreatedAt: old},
			want:  false,
		},
		{
			name:  "fresh gardenerless agent is inside the grace period",
			agent: GardenerlessAgent{ID: "gd-3", HasRealGardener: false, CreatedAt: fresh},
			want:  false,
		},
		{
			name:  "already-labelled agent is skipped (idempotent)",
			agent: GardenerlessAgent{ID: "gd-4", HasRealGardener: false, CreatedAt: old, Labels: []string{"needs-gardener"}},
			want:  false,
		},
		{
			name:  "already-labelled is case-insensitive",
			agent: GardenerlessAgent{ID: "gd-5", HasRealGardener: false, CreatedAt: old, Labels: []string{"Needs-Gardener"}},
			want:  false,
		},
		{
			name:  "zero CreatedAt bypasses the grace guard",
			agent: GardenerlessAgent{ID: "gd-6", HasRealGardener: false},
			want:  true,
		},
		{
			name:  "other labels don't prevent surfacing",
			agent: GardenerlessAgent{ID: "gd-7", HasRealGardener: false, CreatedAt: old, Labels: []string{"project:gardenio"}},
			want:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectGardenerless([]GardenerlessAgent{tc.agent}, cfg, now)
			gotWant := len(got) == 1
			if gotWant != tc.want {
				t.Fatalf("SelectGardenerless(%+v) surfaced=%v, want %v", tc.agent, gotWant, tc.want)
			}
		})
	}
}

func TestSelectGardenerlessNegativeMinAgeDisablesGrace(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-1 * time.Second)
	cfg := NeedsGardenerSweepConfig{MinAge: -1}
	got := SelectGardenerless(
		[]GardenerlessAgent{{ID: "gd-1", HasRealGardener: false, CreatedAt: fresh}},
		cfg, now)
	if len(got) != 1 {
		t.Fatalf("negative MinAge should surface a fresh leak immediately, got %d", len(got))
	}
}

func TestSelectGardenerlessCustomLabel(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	cfg := NeedsGardenerSweepConfig{Label: "unowned"}
	// Carrying the DEFAULT label doesn't count as already-labelled under a custom label.
	agents := []GardenerlessAgent{
		{ID: "gd-1", HasRealGardener: false, CreatedAt: old, Labels: []string{"needs-gardener"}},
		{ID: "gd-2", HasRealGardener: false, CreatedAt: old, Labels: []string{"unowned"}},
	}
	got := SelectGardenerless(agents, cfg, now)
	if len(got) != 1 || got[0].ID != "gd-1" {
		t.Fatalf("custom label selection wrong: %+v", got)
	}
}

// fakeNeedsGardenerStore is an in-memory NeedsGardenerStore for the sweeper tests.
type fakeNeedsGardenerStore struct {
	agents  []GardenerlessAgent
	listErr error
	addErr  map[string]error // per-bead AddLabel error
	labeled map[string]string
}

func (f *fakeNeedsGardenerStore) ListActiveAgents(context.Context) ([]GardenerlessAgent, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.agents, nil
}

func (f *fakeNeedsGardenerStore) AddLabel(_ context.Context, beadID, label string) error {
	if err := f.addErr[beadID]; err != nil {
		return err
	}
	if f.labeled == nil {
		f.labeled = map[string]string{}
	}
	f.labeled[beadID] = label
	return nil
}

func TestNeedsGardenerSweeperSweep(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := &fakeNeedsGardenerStore{
		agents: []GardenerlessAgent{
			{ID: "gd-leak", HasRealGardener: false, CreatedAt: old},
			{ID: "gd-owned", HasRealGardener: true, CreatedAt: old},
			{ID: "gd-labelled", HasRealGardener: false, CreatedAt: old, Labels: []string{"needs-gardener"}},
		},
	}
	sweeper := NewNeedsGardenerSweeper(store, quietLogger(), NeedsGardenerSweepConfig{}, time.Minute)
	surfaced := sweeper.Sweep(context.Background(), now)
	if surfaced != 1 {
		t.Fatalf("surfaced=%d, want 1", surfaced)
	}
	if store.labeled["gd-leak"] != "needs-gardener" {
		t.Fatalf("expected gd-leak labelled needs-gardener, got %v", store.labeled)
	}
	if _, ok := store.labeled["gd-owned"]; ok {
		t.Fatal("owned agent must not be labelled")
	}
}

func TestNeedsGardenerSweeperListErrorYieldsZero(t *testing.T) {
	store := &fakeNeedsGardenerStore{listErr: errors.New("boom")}
	sweeper := NewNeedsGardenerSweeper(store, quietLogger(), NeedsGardenerSweepConfig{}, time.Minute)
	if n := sweeper.Sweep(context.Background(), time.Now()); n != 0 {
		t.Fatalf("list error should yield 0, got %d", n)
	}
}

func TestNeedsGardenerSweeperPerAgentErrorSkips(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := &fakeNeedsGardenerStore{
		agents: []GardenerlessAgent{
			{ID: "gd-a", HasRealGardener: false, CreatedAt: old},
			{ID: "gd-b", HasRealGardener: false, CreatedAt: old},
		},
		addErr: map[string]error{"gd-a": errors.New("nope")},
	}
	sweeper := NewNeedsGardenerSweeper(store, quietLogger(), NeedsGardenerSweepConfig{}, time.Minute)
	surfaced := sweeper.Sweep(context.Background(), now)
	if surfaced != 1 {
		t.Fatalf("one bad agent must not stall the rest: surfaced=%d, want 1", surfaced)
	}
	if store.labeled["gd-b"] != "needs-gardener" {
		t.Fatalf("gd-b should still be labelled: %v", store.labeled)
	}
}

func TestNeedsGardenerSweeperTickThrottles(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	store := &fakeNeedsGardenerStore{
		agents: []GardenerlessAgent{{ID: "gd-leak", HasRealGardener: false, CreatedAt: old}},
	}
	sweeper := NewNeedsGardenerSweeper(store, quietLogger(), NeedsGardenerSweepConfig{}, 10*time.Minute)
	sweeper.Tick(context.Background(), now)
	// A second tick inside the interval must be throttled (no re-label attempt).
	store.labeled = nil
	sweeper.Tick(context.Background(), now.Add(1*time.Minute))
	if len(store.labeled) != 0 {
		t.Fatalf("tick inside interval should be throttled, got %v", store.labeled)
	}
	// After the interval elapses, it runs again.
	sweeper.Tick(context.Background(), now.Add(11*time.Minute))
	if store.labeled["gd-leak"] != "needs-gardener" {
		t.Fatalf("tick after interval should sweep, got %v", store.labeled)
	}
}

func TestSelectGardenerlessStableForMultiple(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	old := now.Add(-1 * time.Hour)
	agents := []GardenerlessAgent{
		{ID: "gd-3", HasRealGardener: false, CreatedAt: old},
		{ID: "gd-1", HasRealGardener: true, CreatedAt: old},
		{ID: "gd-2", HasRealGardener: false, CreatedAt: old},
	}
	got := SelectGardenerless(agents, NeedsGardenerSweepConfig{}, now)
	ids := make([]string, len(got))
	for i, a := range got {
		ids[i] = a.ID
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "gd-2" || ids[1] != "gd-3" {
		t.Fatalf("expected [gd-2 gd-3], got %v", ids)
	}
}
