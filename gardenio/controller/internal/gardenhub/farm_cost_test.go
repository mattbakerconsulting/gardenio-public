package gardenhub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// fakeSpend is a costs rollup keyed by window length.
type fakeSpend struct {
	mu      sync.Mutex
	down    bool
	byWin   map[time.Duration]map[string]float64
	windows []time.Duration
}

func (s *fakeSpend) ProjectSpendUSD(_ context.Context, w time.Duration) (map[string]float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.windows = append(s.windows, w)
	if s.down {
		return nil, fmt.Errorf("vmsingle unreachable")
	}
	return s.byWin[w], nil
}

func f64(v float64) *float64 { return &v }

func eqF(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestBudgetOf(t *testing.T) {
	for _, tc := range []struct {
		usd, period string
		want        float64
		wantPeriod  string
	}{
		{"", "", 0, ""},
		{"", "7d", 0, ""}, // a period alone is no budget
		{"0", "month", 0, ""},
		{"-5", "month", 0, ""},
		{"lots", "month", 0, ""},
		{"NaN", "month", 0, ""},
		{"250", "", 250, BudgetMonth},
		{" 99.5 ", "month", 99.5, BudgetMonth},
		{"40", "7d", 40, Budget7d},
		{"40", "year", 0, ""},
	} {
		got, period := budgetOf(map[string]string{"budget_usd": tc.usd, "budget_period": tc.period})
		if got != tc.want || period != tc.wantPeriod {
			t.Errorf("budgetOf(%q, %q) = %v, %q; want %v, %q", tc.usd, tc.period, got, period, tc.want, tc.wantPeriod)
		}
	}
}

func TestCostWindows(t *testing.T) {
	for _, tc := range []struct {
		now          time.Time
		today, month time.Duration
	}{
		// 00:00 UTC on the 1st: both windows start now, so the month resets.
		{time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), 0, 0},
		{time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC), 23*time.Hour + 59*time.Minute, 30*24*time.Hour + 23*time.Hour + 59*time.Minute},
		{time.Date(2026, 10, 19, 6, 30, 0, 0, time.UTC), 6*time.Hour + 30*time.Minute, 18*24*time.Hour + 6*time.Hour + 30*time.Minute},
		// A non-UTC clock reads the UTC day: 01:00 on the 1st at UTC+2 is
		// still the last day of the previous month in UTC.
		{time.Date(2026, 11, 1, 1, 0, 0, 0, time.FixedZone("x", 2*3600)), 23 * time.Hour, 30*24*time.Hour + 23*time.Hour},
	} {
		today, month := costWindows(tc.now)
		if today != tc.today || month != tc.month {
			t.Errorf("costWindows(%v) = %v, %v; want %v, %v", tc.now, today, month, tc.today, tc.month)
		}
	}
}

// TestBuildFarmCost covers the honest states: no budget, under budget, over
// budget (clamped to 0), a 7d budget reading the week, and the rollup down.
func TestBuildFarmCost(t *testing.T) {
	projects := []projectBudget{
		{name: "alpha"}, // no budget
		{name: "beta", budget: 100, period: BudgetMonth}, // under
		{name: "gamma", budget: 20, period: BudgetMonth}, // over
		{name: "delta", budget: 50.5, period: Budget7d},  // 7d budget
		{name: "quiet", budget: 10, period: BudgetMonth}, // no spend at all
	}
	up := projectSpend{
		ok:    true,
		today: map[string]float64{"alpha": 1.234, "beta": 2, "gamma": 3, "delta": 4},
		week:  map[string]float64{"alpha": 10, "beta": 20, "gamma": 30, "delta": 40.25},
		month: map[string]float64{"alpha": 50, "beta": 60.004, "gamma": 70, "delta": 80},
	}
	good := time.Unix(baseUnix, 0)

	got := buildFarmCost(projects, up, good, nil)
	want := []FarmCostProject{
		{Project: "alpha", Has: true, TodayUSD: f64(1.23), WeekUSD: f64(10)},
		{Project: "beta", Has: true, HasBudget: true, BudgetUSD: f64(100), Period: BudgetMonth, TodayUSD: f64(2), WeekUSD: f64(20), SpentUSD: f64(60), RemainingUSD: f64(40), Level: BudgetLevelOK},
		{Project: "gamma", Has: true, HasBudget: true, BudgetUSD: f64(20), Period: BudgetMonth, TodayUSD: f64(3), WeekUSD: f64(30), SpentUSD: f64(70), RemainingUSD: f64(0), Level: BudgetLevelOver},
		{Project: "delta", Has: true, HasBudget: true, BudgetUSD: f64(50.5), Period: Budget7d, TodayUSD: f64(4), WeekUSD: f64(40.25), SpentUSD: f64(40.25), RemainingUSD: f64(10.25), Level: BudgetLevelWarn},
		{Project: "quiet", Has: true, HasBudget: true, BudgetUSD: f64(10), Period: BudgetMonth, TodayUSD: f64(0), WeekUSD: f64(0), SpentUSD: f64(0), RemainingUSD: f64(10), Level: BudgetLevelOK},
	}
	checkCost(t, "up", got, want)

	// Rollup down: every figure is unknown, never $0, and as_of marks the
	// last good read. The budget itself is still known.
	got = buildFarmCost(projects[:2], projectSpend{}, good, nil)
	checkCost(t, "down", got, []FarmCostProject{
		{Project: "alpha", AsOf: good.Unix()},
		{Project: "beta", HasBudget: true, BudgetUSD: f64(100), Period: BudgetMonth, AsOf: good.Unix()},
	})
	// Down since the process started: no as_of to give.
	got = buildFarmCost(projects[:1], projectSpend{}, time.Time{}, nil)
	checkCost(t, "never read", got, []FarmCostProject{{Project: "alpha"}})

	raw, _ := json.Marshal(buildFarmCost(projects[1:2], projectSpend{}, good, nil))
	for _, k := range []string{`"remaining_usd":null`, `"spent_usd":null`, `"today_usd":null`, `"week_usd":null`, `"has":false`, `"has_budget":true`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("down row JSON %s lacks %s", raw, k)
		}
	}
	raw, _ = json.Marshal(buildFarmCost(projects[:1], up, good, nil))
	for _, k := range []string{`"has_budget":false`, `"budget_usd":null`, `"remaining_usd":null`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("no-budget row JSON %s lacks %s", raw, k)
		}
	}
	if strings.Contains(string(raw), `"as_of"`) || strings.Contains(string(raw), `"period"`) || strings.Contains(string(raw), `"level"`) {
		t.Errorf("fresh no-budget row JSON %s carries as_of, period or level", raw)
	}
}

func TestBudgetLevel(t *testing.T) {
	for _, tc := range []struct {
		spent, budget float64
		want          string
	}{
		{0, 0, ""}, // no budget, no level
		{5, -1, ""},
		{0, 100, BudgetLevelOK},
		{74.99, 100, BudgetLevelOK},
		{75, 100, BudgetLevelWarn},
		{99.99, 100, BudgetLevelWarn},
		{100, 100, BudgetLevelOver},
		{250, 100, BudgetLevelOver},
		{3, 4, BudgetLevelWarn}, // exactly 75% of a small budget
		{0.29, 0.39, BudgetLevelOK},
	} {
		if got := BudgetLevel(tc.spent, tc.budget); got != tc.want {
			t.Errorf("BudgetLevel(%v, %v) = %q, want %q", tc.spent, tc.budget, got, tc.want)
		}
	}
}

// TestBuildFarmCostFleet covers the purse's own figures: the fleet sums every
// project the rollup reports (one with no project bead too), a down rollup
// nulls them with the stale marker, and the account budgets are a dash until
// some account has one, and stay known while the rollup is down.
func TestBuildFarmCostFleet(t *testing.T) {
	projects := []projectBudget{{name: "alpha"}}
	up := projectSpend{
		ok:    true,
		today: map[string]float64{"alpha": 1.234, "nobead": 2.001},
		week:  map[string]float64{"alpha": 10, "nobead": 5.5},
	}
	good := time.Unix(baseUnix, 0)
	budget := &FleetBudget{Accounts: 2, BudgetUSD: 300, SpentUSD: 120.004, RemainingUSD: 180}

	got := buildFarmCost(projects, up, good, budget)
	if got.Scope != CostScopeFleet || !got.Has || !eqF(got.TodayUSD, f64(3.24)) || !eqF(got.WeekUSD, f64(15.5)) || got.AsOf != 0 {
		t.Errorf("fleet up = %+v; want fleet, has, $3.24 today, $15.50 week", got)
	}
	if !got.HasBudget || !eqF(got.BudgetUSD, f64(300)) || !eqF(got.SpentUSD, f64(120)) || !eqF(got.RemainingUSD, f64(180)) {
		t.Errorf("fleet budget = %v %v %v %v; want has, $300, $120 spent, $180 left", got.HasBudget, got.BudgetUSD, got.SpentUSD, got.RemainingUSD)
	}
	// The fleet's level buckets the summed accounts as a row's does: 40% is ok,
	// 80% is warn, and an overspend is over.
	for _, tc := range []struct {
		spent float64
		want  string
	}{{120, BudgetLevelOK}, {240, BudgetLevelWarn}, {300, BudgetLevelOver}} {
		b := &FleetBudget{Accounts: 1, BudgetUSD: 300, SpentUSD: tc.spent, RemainingUSD: max(0, 300-tc.spent)}
		if lv := buildFarmCost(projects, up, good, b).Level; lv != tc.want {
			t.Errorf("fleet level at $%v of $300 = %q, want %q", tc.spent, lv, tc.want)
		}
	}
	// One account $350 into its $100 while another has spent none of its
	// $200: $200 is left, so the level reads that, not the summed $350.
	if lv := buildFarmCost(projects, up, good, &FleetBudget{Accounts: 2, BudgetUSD: 300, SpentUSD: 350, RemainingUSD: 200}).Level; lv != BudgetLevelOK {
		t.Errorf("fleet level with $200 of $300 left = %q, want ok", lv)
	}

	// Rollup down: spend unknown, never $0, with the stale marker; the
	// budgets do not come from the rollup and stay.
	got = buildFarmCost(projects, projectSpend{}, good, budget)
	if got.Has || got.TodayUSD != nil || got.WeekUSD != nil || got.AsOf != good.Unix() || !got.HasBudget || !eqF(got.RemainingUSD, f64(180)) {
		t.Errorf("fleet down = %+v; want no spend figures, as_of %d, budget kept", got, good.Unix())
	}

	// No account has a budget, or none read yet: a dash, never $0.
	for name, b := range map[string]*FleetBudget{"none read": nil, "none set": {}} {
		raw, _ := json.Marshal(buildFarmCost(projects, up, good, b))
		for _, k := range []string{`"scope":"fleet"`, `"has":true`, `"today_usd":3.24`} {
			if !strings.Contains(string(raw), k) {
				t.Errorf("%s: fleet JSON %s lacks %s", name, raw, k)
			}
		}
		top := topKeys(t, raw)
		for _, k := range []string{"as_of", "has_budget", "budget_usd", "spent_usd", "remaining_usd", "level"} {
			if _, ok := top[k]; ok {
				t.Errorf("%s: fresh fleet JSON with no budget %s carries %s", name, raw, k)
			}
		}
	}
	// Never read: no figure at all, never $0.
	raw, _ := json.Marshal(buildFarmCost(projects, projectSpend{}, time.Time{}, nil))
	if !strings.Contains(string(raw), `"scope":"fleet"`) {
		t.Errorf("never-read fleet JSON %s lacks its scope", raw)
	}
	top := topKeys(t, raw)
	for _, k := range []string{"has", "today_usd", "week_usd", "as_of"} {
		if _, ok := top[k]; ok {
			t.Errorf("never-read fleet JSON %s carries %s", raw, k)
		}
	}
}

// TestFarmCostForProjects: a scoped viewer's cut keeps only its rows, sums
// their spend, and never carries the fleet's account budgets. A tenant land's
// row is cut to credits and adds no dollars to the sums.
func TestFarmCostForProjects(t *testing.T) {
	fleet := FarmCost{
		Scope: CostScopeFleet, Has: true, TodayUSD: f64(100), WeekUSD: f64(700),
		HasBudget: true, BudgetUSD: f64(1000), SpentUSD: f64(400), RemainingUSD: f64(600), Level: BudgetLevelOK,
		Projects: []FarmCostProject{
			{Project: "alpha", Has: true, TodayUSD: f64(1.1), WeekUSD: f64(10)},
			{Project: "beta", Has: true, HasBudget: true, BudgetUSD: f64(50), Period: BudgetMonth, TodayUSD: f64(2.2), WeekUSD: f64(20), SpentUSD: f64(30), RemainingUSD: f64(20)},
			{Project: "gamma", Has: true, TodayUSD: f64(50), WeekUSD: f64(300)},
		},
	}

	got := fleet.ForProjects(allowIn([]string{"beta", "alpha", "nosuch"}))
	if got.Scope != CostScopeProject || !got.Has || !eqF(got.TodayUSD, f64(3.3)) || !eqF(got.WeekUSD, f64(30)) {
		t.Errorf("cut = %+v; want project, has, $3.30 today, $30 week", got)
	}
	if got.HasBudget || got.BudgetUSD != nil || got.SpentUSD != nil || got.RemainingUSD != nil || got.Level != "" {
		t.Errorf("cut carries the fleet's account budgets: %+v", got)
	}
	checkCost(t, "cut", got, fleet.Projects[:2])
	if fleet.Scope != CostScopeFleet || len(fleet.Projects) != 3 {
		t.Error("ForProjects changed the fleet blob")
	}

	// A row the rollup could not read makes the sum unknown, with its marker.
	down := fleet
	down.Projects = []FarmCostProject{fleet.Projects[0], {Project: "beta", HasBudget: true, BudgetUSD: f64(50), Period: BudgetMonth, AsOf: baseUnix}}
	got = down.ForProjects(allowIn([]string{"alpha", "beta"}))
	if got.Has || got.TodayUSD != nil || got.WeekUSD != nil || got.AsOf != baseUnix {
		t.Errorf("cut with a down row = %+v; want no figures, as_of %d", got, baseUnix)
	}

	// A tenant land's row: credits only, and its dollars are not summed.
	tenant := fleet
	tenant.Projects = append(slices.Clone(fleet.Projects), FarmCostProject{
		Project: "zeta", Has: true, HasBudget: true, BudgetUSD: f64(40), Period: BudgetMonth,
		TodayUSD: f64(9), WeekUSD: f64(90), SpentUSD: f64(30), RemainingUSD: f64(10), Level: BudgetLevelOK,
		Purse: &purse.Purse{Land: "zeta", GrantedUSD: f64(100), SpentUSD: f64(30), RemainingUSD: f64(70), LLMMultiplier: 1.5},
	})
	got = tenant.ForProjects(allowIn([]string{"alpha", "zeta"}))
	if got.Scope != CostScopeProject || !got.Has || !eqF(got.TodayUSD, f64(1.1)) || !eqF(got.WeekUSD, f64(10)) || got.HasBudget {
		t.Errorf("cut with a tenant land = %+v; want alpha's $1.10 today, $10 week only", got)
	}
	if z := got.Projects[1]; z.TodayUSD != nil || z.WeekUSD != nil || z.SpentUSD != nil || z.RemainingUSD != nil || z.BudgetUSD != nil || z.HasBudget || z.Level != "" ||
		z.Purse == nil || z.Purse.GrantedUSD != nil || z.Purse.SpentUSD != nil || z.Purse.RemainingUSD != nil || z.Purse.LLMMultiplier != 0 {
		t.Errorf("tenant row = %+v purse %+v; want credits only", z, z.Purse)
	}
	if tenant.Projects[3].TodayUSD == nil || tenant.Projects[3].Purse.GrantedUSD == nil {
		t.Error("ForProjects changed the fleet's tenant row")
	}
	// Only tenant lands: no row in dollars, so no figures, and no stale marker.
	got = tenant.ForProjects(allowIn([]string{"zeta"}))
	if got.Scope != CostScopeProject || got.Has || got.TodayUSD != nil || got.WeekUSD != nil || got.AsOf != 0 || len(got.Projects) != 1 {
		t.Errorf("tenant-only cut = %+v; want project scope, no dollar figures", got)
	}

	// None of the viewer's projects has a row: the bare empty blob.
	raw, _ := json.Marshal(fleet.ForProjects(allowIn(nil)))
	if string(raw) != `{"projects":[]}` {
		t.Errorf("empty cut JSON %s, want {\"projects\":[]}", raw)
	}
}

// topKeys is a JSON object's own keys, not its rows'.
func topKeys(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func checkCost(t *testing.T, name string, got FarmCost, want []FarmCostProject) {
	t.Helper()
	if len(got.Projects) != len(want) {
		t.Fatalf("%s: %d rows, want %d", name, len(got.Projects), len(want))
	}
	for i, w := range want {
		g := got.Projects[i]
		if g.Project != w.Project || g.Has != w.Has || g.HasBudget != w.HasBudget || g.Period != w.Period || g.AsOf != w.AsOf ||
			!eqF(g.BudgetUSD, w.BudgetUSD) || !eqF(g.TodayUSD, w.TodayUSD) || !eqF(g.WeekUSD, w.WeekUSD) ||
			!eqF(g.SpentUSD, w.SpentUSD) || !eqF(g.RemainingUSD, w.RemainingUSD) || g.Level != w.Level {
			gj, _ := json.Marshal(g)
			wj, _ := json.Marshal(w)
			t.Errorf("%s row %d:\n got %s\nwant %s", name, i, gj, wj)
		}
	}
}

func projectBead(id, title string, fields map[string]string) *beadsapi.BeadDetail {
	b := fb(id, "project", "open", "", 1)
	b.Title, b.Labels, b.Fields = title, nil, fields
	return b
}

func farmDoc(t *testing.T, f *Farm) map[string]json.RawMessage {
	t.Helper()
	_, b, _ := f.Snapshot()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestFarmCostBlob drives the blob through the farm: rows per project bead,
// a patch when the rollup goes down, and no blob without a spend source.
func TestFarmCostBlob(t *testing.T) {
	src := newFakeFarm()
	src.add(
		projectBead("gd-proj1", "gardenio", map[string]string{"budget_usd": "100", "budget_period": "month"}),
		projectBead("gd-proj2", "Project: monorepo", map[string]string{}),
		projectBead("gd-proj0", "gardenio", map[string]string{"budget_usd": "5", "budget_period": "7d"}), // duplicate name, lower id wins
	)
	f := newTestFarm(src)
	if _, ok := farmDoc(t, f)["cost"]; ok {
		t.Fatal("cost blob present before any refresh")
	}

	// Without a spend source: no blob, and no cost reads.
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := farmDoc(t, f)["cost"]; ok {
		t.Fatal("cost blob present with no spend source")
	}

	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	f.now = func() time.Time { return now }
	sp := &fakeSpend{byWin: map[time.Duration]map[string]float64{
		12 * time.Hour:     {"gardenio": 1, "monorepo": 2},
		7 * 24 * time.Hour: {"gardenio": 7.5, "monorepo": 9},
	}}
	f = newTestFarm(src)
	f.now = func() time.Time { return now }
	f.SetSpendSource(sp)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	var cost FarmCost
	if err := json.Unmarshal(farmDoc(t, f)["cost"], &cost); err != nil {
		t.Fatal(err)
	}
	checkCost(t, "farm up", cost, []FarmCostProject{
		{Project: "gardenio", Has: true, HasBudget: true, BudgetUSD: f64(5), Period: Budget7d, TodayUSD: f64(1), WeekUSD: f64(7.5), SpentUSD: f64(7.5), RemainingUSD: f64(0), Level: BudgetLevelOver},
		{Project: "monorepo", Has: true, TodayUSD: f64(2), WeekUSD: f64(9)},
	})
	// No month budget, so no month query.
	if len(sp.windows) != 2 {
		t.Errorf("spend queries = %v, want today and 7d only", sp.windows)
	}

	// Inside farmCostEvery: no re-read.
	_, live, cancel := f.Subscribe(f.st.version)
	defer cancel()
	now = now.Add(farmCostEvery / 2)
	f.slow.at = time.Time{} // let the slow tier run
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sp.windows) != 2 {
		t.Errorf("cost re-read inside farmCostEvery: %v", sp.windows)
	}

	// The rollup goes down: one patch carrying the cost blob, every row
	// has=false with the last good read as the stale marker.
	sp.mu.Lock()
	sp.down = true
	sp.mu.Unlock()
	good := now.Add(-farmCostEvery / 2)
	now = now.Add(farmCostEvery)
	f.slow.at = time.Time{}
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-live:
		var p struct {
			Cost *FarmCost `json:"cost"`
		}
		if err := json.Unmarshal(m.Data, &p); err != nil || p.Cost == nil {
			t.Fatalf("patch %s carries no cost blob (err %v)", m.Data, err)
		}
		checkCost(t, "farm down", *p.Cost, []FarmCostProject{
			{Project: "gardenio", HasBudget: true, BudgetUSD: f64(5), Period: Budget7d, AsOf: good.Unix()},
			{Project: "monorepo", AsOf: good.Unix()},
		})
	case <-time.After(time.Second):
		t.Fatal("no patch when the rollup went down")
	}
}

// fakeBudget is a costs rollup that also reads the account budgets.
type fakeBudget struct {
	*fakeSpend
	budget FleetBudget
	err    error
	reads  int
}

func (b *fakeBudget) FleetBudgetUSD(context.Context) (FleetBudget, error) {
	b.reads++
	return b.budget, b.err
}

// TestFarmCostFleetBudget drives the account budgets through the farm: read
// with the spend on the cost tier, kept across a failed read, and absent (a
// dash) from a spend source that cannot read them.
func TestFarmCostFleetBudget(t *testing.T) {
	src := newFakeFarm()
	src.add(projectBead("gd-proj1", "gardenio", map[string]string{}))
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	sp := &fakeSpend{byWin: map[time.Duration]map[string]float64{
		12 * time.Hour:     {"gardenio": 1, "unbeaded": 0.5},
		7 * 24 * time.Hour: {"gardenio": 7.5},
	}}
	bs := &fakeBudget{fakeSpend: sp, budget: FleetBudget{Accounts: 1, BudgetUSD: 200, SpentUSD: 50, RemainingUSD: 150}}
	f := newTestFarm(src)
	f.now = func() time.Time { return now }
	f.SetSpendSource(bs)
	read := func() FarmCost {
		t.Helper()
		if err := f.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		var cost FarmCost
		if err := json.Unmarshal(farmDoc(t, f)["cost"], &cost); err != nil {
			t.Fatal(err)
		}
		return cost
	}

	cost := read()
	if cost.Scope != CostScopeFleet || !cost.Has || !eqF(cost.TodayUSD, f64(1.5)) || !eqF(cost.WeekUSD, f64(7.5)) {
		t.Errorf("fleet spend = %+v; want $1.50 today (the unbeaded project too), $7.50 week", cost)
	}
	if !cost.HasBudget || !eqF(cost.BudgetUSD, f64(200)) || !eqF(cost.RemainingUSD, f64(150)) || bs.reads != 1 {
		t.Errorf("fleet budget = %+v after %d reads; want $200, $150 left, one read", cost, bs.reads)
	}

	// A failed budget read keeps the last budgets read.
	bs.err = fmt.Errorf("daemon down")
	now = now.Add(farmCostEvery)
	f.slow.at = time.Time{}
	if cost = read(); !cost.HasBudget || !eqF(cost.RemainingUSD, f64(150)) || bs.reads != 2 {
		t.Errorf("after a failed read: %+v (%d reads); want the last budgets kept", cost, bs.reads)
	}

	// A spend source that is not a BudgetSource: spend, and a dash for budgets.
	f = newTestFarm(src)
	f.now = func() time.Time { return now }
	f.SetSpendSource(sp)
	if cost = read(); !cost.Has || cost.HasBudget || cost.RemainingUSD != nil {
		t.Errorf("no budget source: %+v; want spend and no budgets", cost)
	}
}

// purseFailSrc fails every credit_rate read, so the purse cannot be read. The error names another land and the
// daemon, as the purse's own wrapped errors do ("reading basil's credit ledger: ...").
type purseFailSrc struct{ *fakeFarmSrc }

const purseFailErr = `reading basil's credit ledger: Get "http://kbeads.gardenio.svc:8080/v1/beads": daemon down`

func (s purseFailSrc) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if in(q.Types, "credit_rate") {
		return nil, errors.New(purseFailErr)
	}
	return s.fakeFarmSrc.ListBeadsFiltered(ctx, q)
}

// TestFarmCostPurse: a tenant land's row carries its purse (Lands 8.2); an
// operator land's row is unchanged; a failed purse read is unknown, never full.
func TestFarmCostPurse(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	src := newFakeFarm()
	rate := fb("gd-rate1", "credit_rate", "open", "", 1)
	rate.Fields = map[string]string{"version": "1", "llm_multiplier": "1.5"}
	grant := fb("gd-grant1", "credit_grant", "closed", "", 1)
	grant.Labels, grant.Fields = []string{"project:tomato"}, map[string]string{"land": "tomato", "amount_credits": "40"}
	acct := fb("gd-acct1", "account", "open", "", 1)
	acct.Title, acct.Labels = "lands-tomato", nil
	acct.Fields = map[string]string{"total_cost_usd": "10", "spend_synced_at": now.Add(-time.Minute).Format(time.RFC3339)}
	src.add(
		projectBead("gd-proj1", "gardenio", map[string]string{}),
		projectBead("gd-proj2", "tomato", map[string]string{"land_kind": "tenant", "account": "lands-tomato"}),
		rate, grant, acct,
	)
	sp := &fakeSpend{byWin: map[time.Duration]map[string]float64{}}

	read := func(src FarmSource) FarmCost {
		t.Helper()
		f := newTestFarm(src)
		f.now = func() time.Time { return now }
		f.SetSpendSource(sp)
		if err := f.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		var cost FarmCost
		if err := json.Unmarshal(farmDoc(t, f)["cost"], &cost); err != nil {
			t.Fatal(err)
		}
		return cost
	}

	cost := read(src)
	if len(cost.Projects) != 2 {
		t.Fatalf("rows = %d, want 2", len(cost.Projects))
	}
	op, tenant := cost.Projects[0], cost.Projects[1]
	if op.Project != "gardenio" || op.Purse != nil {
		t.Errorf("operator row %+v carries a purse", op)
	}
	p := tenant.Purse
	if tenant.Project != "tomato" || p == nil {
		t.Fatalf("tenant row %+v has no purse", tenant)
	}
	if p.State != "ok" || p.GrantedCredits != 40 || *p.SpentCredits != 15 || *p.RemainingCredits != 25 || *p.SpentUSD != 10 {
		t.Errorf("purse = %+v; want ok, 40 granted, 15 spent ($10), 25 left", p)
	}

	cost = read(purseFailSrc{src})
	if p := cost.Projects[1].Purse; p == nil || p.State != "unknown" || !p.Blocked || p.RemainingCredits != nil {
		t.Errorf("failed purse read = %+v; want unknown, blocked, no figures", p)
	}
	// The batch's error stays in the log: the reason reaches tomato's tenant, so it names no other land and no daemon
	// (security review 3.5, gd-E7FphpZNMI).
	if p := cost.Projects[1].Purse; p != nil && (strings.Contains(p.Reason, "basil") || strings.Contains(p.Reason, "://") || strings.Contains(p.Reason, "daemon")) {
		t.Errorf("failed purse read reason %q carries the batch's error", p.Reason)
	}
	if cost.Projects[0].Purse != nil {
		t.Error("a failed purse read gave the operator land a purse")
	}
}

// TestFarmCostIsBounded holds every cost field to its budget on the 10x worst case (TestFarmWorstCaseIsBounded's
// world): every plan rolled up, more live gardeners than maxGardeners, each with a reconciled total, a live estimate and
// a run of one of 60 triggers (past maxRunSpendReads), and a project bead per plan under a budget, one of them a tenant
// land with its purse and steward. The cost blob is one row per project bead, so it is bounded by the project list's
// listLimit, not by the farm's size.
func TestFarmCostIsBounded(t *testing.T) {
	src := bigWorld(40, 12, 30, 25, 1000, 200)
	addHarvests(src, 40, 12, maxClosedPerPlan+16)
	now := time.Unix(baseUnix+10_000, 0)
	for p := range 40 {
		src.mutate(fmt.Sprintf("gd-P%03d", p), func(b *beadsapi.BeadDetail) { setPlanSpend(b, "1234.5678", now.Unix()-3_600, "120", "30") })
		src.add(projectBead(fmt.Sprintf("gd-proj%03d", p), fmt.Sprintf("a-project-named-for-its-plan-%03d", p),
			map[string]string{"budget_usd": "1250.5", "budget_period": "month"}))
	}
	for i := range 60 {
		trig := agentBead(fmt.Sprintf("gd-trig%02d", i), "Plan gardener", "", "", "", 1)
		trig.Type, trig.Status, trig.Fields = "trigger", "open", map[string]string{"gardener_spend_usd": "12345.6789"}
		src.add(trig)
	}
	for i := range maxGardeners + 20 {
		a := agentBead(fmt.Sprintf("gd-agent%04d", i), fmt.Sprintf("gardenio-some-crew-agent-%04d", i), "crew", "working",
			fmt.Sprintf("gd-P%03d-ph%d-t%d", i%40, i%12, i%30), int64(i))
		a.Fields["thread_id"] = "gd-thread"
		a.Fields["total_cost_usd"], a.Fields["live_cost_usd"], a.Fields["trigger_id"] = "1234.56", "123.45", fmt.Sprintf("gd-trig%02d", i%60)
		src.add(a)
	}
	// The fixture's four rows, the tenant land with its purse and steward among them, and a row with four-digit amounts.
	sp := fixtureCost(src, now)
	sp.byWin[7*24*time.Hour]["a-project-named-for-its-plan-000"] = 9999.99
	f := newTestFarm(src)
	f.SetSpendSource(sp)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc := farmDoc(t, f)
	var parts struct {
		Plans []struct {
			Cost json.RawMessage `json:"cost"`
		} `json:"plans"`
		Gardeners []struct {
			Cost json.RawMessage `json:"cost"`
		} `json:"gardeners"`
	}
	_, raw, _ := f.Snapshot()
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatal(err)
	}
	var cost FarmCost
	if err := json.Unmarshal(doc["cost"], &cost); err != nil {
		t.Fatal(err)
	}
	maxOf := func(n int, at func(int) int) (most, sum int) {
		for i := range n {
			sum += at(i)
			most = max(most, at(i))
		}
		return most, sum
	}
	planMax, planSum := maxOf(len(parts.Plans), func(i int) int { return len(parts.Plans[i].Cost) })
	gMax, gSum := maxOf(len(parts.Gardeners), func(i int) int { return len(parts.Gardeners[i].Cost) })
	// The row cap keeps only three plans of this world, so plan cost is held per plan (about 85 B with every field set).
	// A row's own fields are about 200 B with four-digit amounts; a tenant land's adds its purse and steward.
	var opMax, tenantMax, opRows, tenantRows int
	for _, r := range cost.Projects {
		b, _ := json.Marshal(r)
		if r.Purse != nil {
			tenantRows++
			tenantMax = max(tenantMax, len(b))
			continue
		}
		opRows++
		opMax = max(opMax, len(b))
	}
	t.Logf("document %d B; cost blob %d B (%d rows: operator rows up to %d B, %d tenant up to %d B); plan cost %d B over %d plans (largest %d B); gardener cost %d B over %d gardeners (largest %d B)",
		len(raw), len(doc["cost"]), len(cost.Projects), opMax, tenantRows, tenantMax, planSum, len(parts.Plans), planMax, gSum, len(parts.Gardeners), gMax)
	if len(cost.Projects) != 44 || tenantRows != 1 || len(parts.Gardeners) != maxGardeners {
		t.Fatalf("rows = %d (%d tenant), gardeners = %d; want 44 (1), %d", len(cost.Projects), tenantRows, len(parts.Gardeners), maxGardeners)
	}
	const opRowMax, tenantRowMax, blobHead, planCostMax, gardenerCostMax = 220, 900, 200, 100, 260
	if opMax > opRowMax || tenantMax > tenantRowMax {
		t.Errorf("cost rows up to %d B (operator) and %d B (tenant), budgets %d and %d", opMax, tenantMax, opRowMax, tenantRowMax)
	}
	if want := blobHead + opRows*(opRowMax+1) + tenantRows*(tenantRowMax+1); len(doc["cost"]) > want {
		t.Errorf("cost blob %d B, budget %d", len(doc["cost"]), want)
	}
	if planMax > planCostMax || gMax > gardenerCostMax {
		t.Errorf("plan cost up to %d B, gardener cost up to %d B; budgets %d and %d", planMax, gMax, planCostMax, gardenerCostMax)
	}
	// The 10x budget (TestFarmWorstCaseIsBounded) holds with every cost field.
	if len(raw) > 700_000 {
		t.Fatalf("worst-case snapshot with costs is %d bytes, budget 700000", len(raw))
	}
}

// fixtureCost gives the fixture farm its purse (gd-dLmvf6zosw): project beads for a row under a month budget (warn), a
// row over a 7d budget, a row with no budget and a tenant land with its purse and steward, and a costs rollup that also
// reads two budgeted accounts. A project with spend and no bead ("unbeaded") counts in the fleet's figures only. It
// adds beads to src, so fixtureWorld itself (and the goldens built on it) is unchanged.
func fixtureCost(src *fakeFarmSrc, now time.Time) *fakeBudget {
	rate := fb("gd-costRate", "credit_rate", "open", "", 1)
	rate.Fields = map[string]string{"version": "1", "llm_multiplier": "1.5"}
	grant := fb("gd-costGrant", "credit_grant", "closed", "", 1)
	grant.Labels, grant.Fields = []string{"project:tomato"}, map[string]string{"land": "tomato", "amount_credits": "100"}
	acct := fb("gd-costAcct", "account", "open", "", 1)
	acct.Title, acct.Labels = "lands-tomato", nil
	acct.Fields = map[string]string{"total_cost_usd": "10", "spend_synced_at": now.Add(-2 * time.Minute).UTC().Format(time.RFC3339)}
	src.add(
		projectBead("gd-projGardenio", "gardenio", map[string]string{"budget_usd": "250", "budget_period": "month"}),
		projectBead("gd-projKbeads", "kbeads", map[string]string{"budget_usd": "35", "budget_period": "7d"}),
		projectBead("gd-projZeta", "zeta", map[string]string{}),
		projectBead("gd-projTomato", "tomato", map[string]string{"land_kind": "tenant", "account": "lands-tomato"}),
		rate, grant, acct,
		stewardTrigger("gd-stwTomato", "tomato", `{"max_runs_per_day":24}`),
	)
	today, month := costWindows(now)
	sp := &fakeSpend{byWin: map[time.Duration]map[string]float64{
		today:              {"gardenio": 4.1, "kbeads": 2.05, "unbeaded": 0.6},
		7 * 24 * time.Hour: {"gardenio": 61.75, "kbeads": 38.4, "zeta": 3.3, "unbeaded": 1.2},
		month:              {"gardenio": 190.2, "kbeads": 120, "zeta": 9, "unbeaded": 2.5},
	}}
	return &fakeBudget{fakeSpend: sp, budget: FleetBudget{Accounts: 2, BudgetUSD: 600, SpentUSD: 230.5, RemainingUSD: 380}}
}

// checkFixtureGolden compares raw, indented, with the golden at path, first
// rewriting it under -update-farm-fixture.
func checkFixtureGolden(t *testing.T, raw []byte, path string) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	buf.WriteByte('\n')
	pretty := buf.Bytes()
	if *updateFixture {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pretty, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing fixture (run with -update-farm-fixture): %v", err)
	}
	if string(want) != string(pretty) {
		t.Fatalf("farm JSON drifted from %s; if intentional, regenerate with -update-farm-fixture and update docs/homestead-farm-read-model.md", path)
	}
}
