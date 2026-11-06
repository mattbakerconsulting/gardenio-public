package bridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// typedFieldsDaemon records the gardener blob gardenerPostFire writes.
type typedFieldsDaemon struct {
	*darkBudgetDaemon
	gardener string
}

func (d *typedFieldsDaemon) UpdateBeadFieldsTyped(_ context.Context, _ string, fields map[string]any) error {
	if g, ok := fields["gardener"].(string); ok {
		d.gardener = g
	}
	return nil
}

func dailyCapGardener(t *testing.T, spec GardenerSpec) TriggerDef {
	t.Helper()
	spec.RunBudget = 100
	spec.Standing = true
	spec.Stop = json.RawMessage(boundedStopBlob(t, "168h"))
	return TriggerDef{ID: "gd-steward", Project: "gardenio", Labels: []string{"role:gardener", "project:gardenio"}, Gardener: &spec}
}

func TestGardenerPreFire_DailyRunCap(t *testing.T) {
	today := gardenerRunsDay(time.Now())
	for _, tc := range []struct {
		name string
		spec GardenerSpec
		skip bool
	}{
		{"under the cap", GardenerSpec{MaxRunsPerDay: 24, RunsDay: today, RunsToday: 23}, false},
		{"at the cap", GardenerSpec{MaxRunsPerDay: 24, RunsDay: today, RunsToday: 24}, true},
		{"yesterday's count does not hold", GardenerSpec{MaxRunsPerDay: 24, RunsDay: "2000-01-01", RunsToday: 24}, false},
		{"no daily cap", GardenerSpec{RunsDay: today, RunsToday: 500}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skip, reason := gardenerPreFireCheck(context.Background(), newDarkBudgetDaemon(), nil, dailyCapGardener(t, tc.spec))
			if got := skip && reason == gardenerSkipDailyRunCap; got != tc.skip {
				t.Fatalf("daily-cap skip = %v (skip=%v reason=%q), want %v", got, skip, reason, tc.skip)
			}
			if hit := GardenerDailyRunCapHit(&tc.spec, time.Now()); hit != tc.skip {
				t.Errorf("GardenerDailyRunCapHit = %v, want %v", hit, tc.skip)
			}
		})
	}
}

func TestGardenerPostFire_CountsRunsToday(t *testing.T) {
	today := gardenerRunsDay(time.Now())
	for _, tc := range []struct {
		name      string
		spec      GardenerSpec
		wantDay   string
		wantToday int
	}{
		{"first run today", GardenerSpec{MaxRunsPerDay: 24}, today, 1},
		{"another run today", GardenerSpec{MaxRunsPerDay: 24, RunsDay: today, RunsToday: 5}, today, 6},
		{"a new day restarts the count", GardenerSpec{MaxRunsPerDay: 24, RunsDay: "2000-01-01", RunsToday: 24}, today, 1},
		{"no daily cap keeps no count", GardenerSpec{}, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &typedFieldsDaemon{darkBudgetDaemon: newDarkBudgetDaemon()}
			trig := dailyCapGardener(t, tc.spec)
			gardenerPostFire(context.Background(), d, discardLogger(), trig, "gd-agent-1", "")
			var got GardenerSpec
			if err := json.Unmarshal([]byte(d.gardener), &got); err != nil {
				t.Fatalf("decoding written blob %q: %v", d.gardener, err)
			}
			if got.RunsDay != tc.wantDay || got.RunsToday != tc.wantToday {
				t.Errorf("written runs_day=%q runs_today=%d, want %q %d", got.RunsDay, got.RunsToday, tc.wantDay, tc.wantToday)
			}
			if trig.Gardener.RunsToday != tc.wantToday {
				t.Errorf("in-memory RunsToday = %d, want %d", trig.Gardener.RunsToday, tc.wantToday)
			}
		})
	}
}
