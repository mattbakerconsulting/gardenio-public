package gardenhub

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func usdString(p *float64) string {
	if p == nil {
		return "null"
	}
	return usd(*p)
}

// TestGardenerCost: the agent's own figure is agentSpendUSD (total, plus the
// live estimate only while alive), the live part floored to a step, a 0 or
// missing figure is not known, and the words say "at least" while a session
// may still be open.
func TestGardenerCost(t *testing.T) {
	cases := []struct {
		name        string
		fields      map[string]string
		bucket      string
		run         *float64
		nilCost     bool
		spent, live string
		atLeast     bool
		gardener    string
		text        string
	}{
		{name: "alive, live estimate stuck at 0 (gd-UIlh0DR2VG): total only, at least",
			fields: map[string]string{"total_cost_usd": "2.804", "live_cost_usd": "0"}, bucket: GardenerAlive,
			spent: "$2.80", live: "null", atLeast: true, gardener: "null",
			text: "Spent at least $2.80; this session is counted in full when it ends."},
		{name: "alive, live counted in whole steps",
			fields: map[string]string{"total_cost_usd": "2.80", "live_cost_usd": "1.37"}, bucket: GardenerAlive,
			spent: "$3.80", live: "$1.00", atLeast: true, gardener: "null",
			text: "Spent at least $3.80; this session is counted in full when it ends."},
		{name: "alive, live under one step is not counted",
			fields: map[string]string{"live_cost_usd": "0.49"}, bucket: GardenerAlive,
			spent: "null", live: "null", atLeast: true, gardener: "null",
			text: "Nothing counted yet; this session is counted when it ends."},
		{name: "alive, nothing known: no fake $0",
			fields: map[string]string{}, bucket: GardenerAlive,
			spent: "null", live: "null", atLeast: true, gardener: "null",
			text: "Nothing counted yet; this session is counted when it ends."},
		{name: "stuck: the live estimate is not added (parity with agentSpendUSD), still at least",
			fields: map[string]string{"total_cost_usd": "4.10", "live_cost_usd": "3"}, bucket: GardenerStuck,
			spent: "$4.10", live: "null", atLeast: true, gardener: "null",
			text: "Spent at least $4.10; this session is counted in full when it ends."},
		{name: "dormant: the reconciled total is final",
			fields: map[string]string{"total_cost_usd": "4.10", "live_cost_usd": "3"}, bucket: GardenerDormant,
			spent: "$4.10", live: "null", gardener: "null", text: "Spent $4.10."},
		{name: "dormant with nothing known has no cost",
			fields: map[string]string{"total_cost_usd": "0", "live_cost_usd": "nope"}, bucket: GardenerDormant, nilCost: true},
		{name: "a gardener's run carries its trigger's rollup",
			fields: map[string]string{"total_cost_usd": "0.67"}, bucket: GardenerAlive, run: f64(40.79),
			spent: "$0.67", live: "null", atLeast: true, gardener: "$40.79",
			text: "Spent at least $0.67; this session is counted in full when it ends. Its gardener has spent at least $40.79 over all its runs and their crew."},
		{name: "a dormant gardener's run with no figure of its own",
			fields: map[string]string{}, bucket: GardenerDormant, run: f64(10.61),
			spent: "null", live: "null", gardener: "$10.61",
			text: "Its gardener has spent $10.61 over all its runs and their crew."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := gardenerCost(c.fields, c.bucket, c.run)
			if c.nilCost {
				if got != nil {
					t.Fatalf("cost = %+v, want null", got)
				}
				return
			}
			if got == nil {
				t.Fatal("cost is null")
			}
			if usdString(got.SpentUSD) != c.spent || usdString(got.LiveUSD) != c.live || got.AtLeast != c.atLeast ||
				usdString(got.GardenerUSD) != c.gardener || got.Text != c.text {
				t.Errorf("cost = spent %s live %s at_least %v gardener %s text %q\nwant  spent %s live %s at_least %v gardener %s text %q",
					usdString(got.SpentUSD), usdString(got.LiveUSD), got.AtLeast, usdString(got.GardenerUSD), got.Text,
					c.spent, c.live, c.atLeast, c.gardener, c.text)
			}
		})
	}
}

// TestFarmGardenerCost: on the farm, a gardener carries its cost and its
// project; a gardener's run carries its trigger's rollup, read once and
// cached until the next full read; a live estimate moving within a step does
// not re-send the gardener; and the key is always present.
func TestFarmGardenerCost(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	trig := fb("gd-trigG", "trigger", "open", "", 1)
	trig.Fields["gardener_spend_usd"] = "40.7901"
	run := agentBead("gd-gRun", "g-run", "plan-gardener", "working", "", 10)
	run.Fields["trigger_id"] = "gd-trigG"
	run.Fields["total_cost_usd"] = "0.6665"
	crew := agentBead("gd-gCrew", "g-crew", "crew", "working", "", 20)
	crew.Labels = []string{"project:zeta"}
	crew.Fields["total_cost_usd"], crew.Fields["live_cost_usd"] = "2.80", "1.10"
	fresh := agentBead("gd-gFresh", "g-fresh", "crew", "spawning", "", 30)
	src.add(trig, run, crew, fresh)
	f := newTestFarm(src)
	clock := time.Unix(baseUnix+10_000, 0)
	f.now = func() time.Time { return clock }
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	g := findGardener(s, "gd-gRun")
	if g == nil || g.Cost == nil || usdString(g.Cost.SpentUSD) != "$0.67" || usdString(g.Cost.GardenerUSD) != "$40.79" || !g.Cost.AtLeast {
		t.Fatalf("g-run = %+v cost %+v; want its own $0.67 and its gardener's $40.79", g, g.Cost)
	}
	c := findGardener(s, "gd-gCrew")
	if c == nil || c.Project != "zeta" || c.Cost == nil || usdString(c.Cost.SpentUSD) != "$3.80" || usdString(c.Cost.LiveUSD) != "$1.00" ||
		c.Cost.GardenerUSD != nil {
		t.Fatalf("g-crew = %+v cost %+v; want project zeta, $3.80 with $1.00 live, no gardener", c, c.Cost)
	}
	if fr := findGardener(s, "gd-gFresh"); fr == nil || fr.Cost == nil || fr.Cost.SpentUSD != nil {
		t.Fatalf("g-fresh = %+v; want a cost with spent_usd null, never $0", fr)
	}
	_, raw, _ := f.Snapshot()
	var doc struct {
		Gardeners []map[string]json.RawMessage `json:"gardeners"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, g := range doc.Gardeners {
		if _, ok := g["cost"]; !ok {
			t.Fatalf("gardener %s has no cost key", g["id"])
		}
	}

	// The live estimate moves within a step, and the trigger's rollup moves:
	// nothing is re-sent, and the cached rollup is not re-read.
	src.mu.Lock()
	gets := src.getCalls
	src.mu.Unlock()
	v := f.Stats().Version
	src.mutate("gd-gCrew", func(b *beadsapi.BeadDetail) { b.Fields["live_cost_usd"] = "1.45" })
	src.mutate("gd-trigG", func(b *beadsapi.BeadDetail) { b.Fields["gardener_spend_usd"] = "42.5" })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	again := src.getCalls - gets
	src.mu.Unlock()
	if again != 0 || f.Stats().Version != v {
		t.Fatalf("within a step and before the next full read: %d batch reads, version %d -> %d; want none", again, v, f.Stats().Version)
	}

	// The next full read picks up the rollup; the next step re-sends the crew.
	clock = clock.Add(farmRunSpendEvery)
	src.mutate("gd-gCrew", func(b *beadsapi.BeadDetail) { b.Fields["live_cost_usd"] = "1.52" })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	s = decodeSnap(t, f)
	if g := findGardener(s, "gd-gRun"); g == nil || g.Cost == nil || usdString(g.Cost.GardenerUSD) != "$42.50" {
		t.Errorf("g-run after the full read = %+v; want its gardener's $42.50", g)
	}
	if c := findGardener(s, "gd-gCrew"); c == nil || c.Cost == nil || usdString(c.Cost.LiveUSD) != "$1.50" {
		t.Errorf("g-crew after a step = %+v; want $1.50 live", c)
	}

	// The run leaves: its trigger is dropped from the cache.
	src.mutate("gd-gRun", func(b *beadsapi.BeadDetail) { b.Status = "closed" })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.runSpend.usd) != 0 {
		t.Errorf("runSpend = %v after the run left; want empty", f.runSpend.usd)
	}
}
