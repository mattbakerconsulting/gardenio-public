package gardenhub

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// The isolation cases of gd-16UJtQh3Ua that farm_cut_test.go and farm_cut_stream_test.go (gd-6UG1OGlqYg,
// gd-UKO0ZpXeha) left open: the cost blob of a viewer in an operator land, the fleet figures a scoped blob must never
// carry, and the gzip a scoped viewer is served.

// A gardenio member's cost blob is gardenio's row alone, with its own budget and spend (an operator land keeps its
// dollars), and nothing of zeta's: no row, no purse, no credits. Together the two lands' cuts carry exactly the rows of
// the admin blob, each once.
func TestFarmCutCostOperatorLand(t *testing.T) {
	src := multiLandWorld()
	f := newTestFarm(src)
	f.SetSpendSource(flatSpend{"gardenio": 4.25, "zeta": 9.75})
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	var admin FarmCost
	if err := json.Unmarshal(farmDoc(t, f)["cost"], &admin); err != nil || len(admin.Projects) != 2 {
		t.Fatalf("admin cost = %+v (%v), want a row for each land", admin, err)
	}

	_, doc, raw, _ := cutDoc(t, f, "gardenio")
	var cost FarmCost
	if err := json.Unmarshal(doc["cost"], &cost); err != nil {
		t.Fatal(err)
	}
	if len(cost.Projects) != 1 || cost.Projects[0].Project != "gardenio" {
		t.Fatalf("gardenio cost rows = %+v, want gardenio's only", cost.Projects)
	}
	row := cost.Projects[0]
	if row.Purse != nil || !row.HasBudget || row.BudgetUSD == nil || *row.BudgetUSD != 100 {
		t.Errorf("gardenio row = %+v, want its own budget and no purse", row)
	}
	for _, a := range admin.Projects {
		if a.Project == "gardenio" {
			got, _ := json.Marshal(row)
			want, _ := json.Marshal(a)
			if !bytes.Equal(got, want) {
				t.Errorf("gardenio's row in its cut differs from the admin row:\n%s\n%s", got, want)
			}
		}
	}
	for _, leak := range []string{`"zeta"`, "9.75", "lands-zeta", `"purse"`} {
		if bytes.Contains(doc["cost"], []byte(leak)) {
			t.Errorf("the gardenio cost blob carries %s: %s", leak, doc["cost"])
		}
	}
	if bytes.Contains(raw, []byte("9.75")) {
		t.Error("the gardenio cut carries zeta's spend")
	}
	// The purse sums the rows, never gardeners[].cost (gd-0VeG81uW3w): gardenio's gardeners keep their own figures on
	// its operator land, and the sum is still the row's spend alone.
	s, _, _, _ := cutDoc(t, f, "gardenio")
	if w := findGardener(s, "gd-gWork1"); w == nil || w.Cost == nil || usdString(w.Cost.SpentUSD) != "$3.20" {
		t.Errorf("gardenio cut gd-gWork1 = %+v, want its $3.20 on an operator land", w)
	}
	if cost.TodayUSD == nil || *cost.TodayUSD != 4.25 {
		t.Errorf("gardenio purse today_usd = %v, want the row's 4.25 with no gardener cost added", cost.TodayUSD)
	}

	zs, zdoc, _, _ := cutDoc(t, f, "zeta")
	if bytes.Contains(zdoc["cost"], []byte("4.25")) || bytes.Contains(zdoc["cost"], []byte(`"gardenio"`)) {
		t.Errorf("the zeta cost blob carries gardenio's row: %s", zdoc["cost"])
	}
	for _, g := range zs.Gardeners {
		if g.Cost != nil {
			t.Errorf("zeta cut gardener %s carries cost %+v on a tenant land", g.ID, g.Cost)
		}
	}
}

// The scoped cost blob is rebuilt from the rows it admits (FarmCost.ForProjects), so a fleet figure on the admin blob
// (the fleet $ today, 7d and $ remaining from account budgets, gd-594kYy1IYo) cannot ride along: beside projects a
// scoped blob carries only the set's own purse (scope "project", today and week summed over its rows in dollars),
// whatever the admin blob grows. A tenant land's row adds nothing to the sums, so a tenant-only set has no figures.
func TestFarmCutCostDropsFleetFigures(t *testing.T) {
	fleet := []byte(`{"scope":"fleet","has":true,"today_usd":1234.5,"week_usd":5678.25,"remaining_usd":4321.75,"budget_usd":9999,"has_budget":true,` +
		`"projects":[{"project":"gardenio","has":true,"has_budget":true,"budget_usd":100,"period":"month","today_usd":1,"week_usd":2,"spent_usd":3,"remaining_usd":97,"level":"ok"},` +
		`{"project":"zeta","has":true,"has_budget":false,"budget_usd":null,"today_usd":5,"week_usd":6,"spent_usd":7,"remaining_usd":null,"level":"warn",` +
		`"purse":{"granted_usd":50,"spent_usd":7,"remaining_usd":43,"llm_multiplier":1.5,"granted_credits":5000}}]}`)
	for _, tc := range []struct {
		set  []string
		keys []string
	}{
		{[]string{"gardenio"}, []string{"has", "projects", "scope", "today_usd", "week_usd"}},
		{[]string{"zeta"}, []string{"projects", "scope"}},
		{[]string{"gardenio", "zeta"}, []string{"has", "projects", "scope", "today_usd", "week_usd"}},
		{nil, []string{"projects"}},
	} {
		set := tc.set
		out := cutCost(fleet, allowIn(set))
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(out, &keys); err != nil {
			t.Fatalf("%v: cut blob is not JSON: %v", set, err)
		}
		if got := slices.Sorted(maps.Keys(keys)); !slices.Equal(got, tc.keys) {
			t.Errorf("%v: cut blob keys = %v, want %v: %s", set, got, tc.keys, out)
		}
		for _, leak := range []string{"1234.5", "5678.25", "4321.75", "9999", `"fleet"`} {
			if bytes.Contains(out, []byte(leak)) {
				t.Errorf("%v: cut blob carries the fleet figure %s: %s", set, leak, out)
			}
		}
		var c FarmCost
		_ = json.Unmarshal(out, &c)
		if len(c.Projects) != len(set) {
			t.Errorf("%v: %d rows, want one per land in the set", set, len(c.Projects))
		}
		if c.TodayUSD != nil && (*c.TodayUSD != 1 || c.WeekUSD == nil || *c.WeekUSD != 2) {
			t.Errorf("%v: purse = today %v week %v, want gardenio's own $1 and $2", set, *c.TodayUSD, c.WeekUSD)
		}
		for _, row := range c.Projects {
			if row.Project == "zeta" && (row.TodayUSD != nil || row.SpentUSD != nil || row.Level != "" || row.Purse == nil || row.Purse.GrantedUSD != nil || row.Purse.RemainingUSD != nil) {
				t.Errorf("%v: zeta row = %+v, want credits only", set, row)
			}
		}
	}
}

// flatSpend is a costs rollup that reports the same spend for every window.
type flatSpend map[string]float64

func (s flatSpend) ProjectSpendUSD(context.Context, time.Duration) (map[string]float64, error) {
	return s, nil
}

// Served gzipped, each scoped viewer gets its own cut's gzip: built once per set and version (the same bytes on a
// second request), never another set's, never the admin's, and it inflates to that viewer's document.
func TestFarmCutGzipPerViewer(t *testing.T) {
	f := newCutFarm(t, multiLandWorld())
	f.wantArchivesLocked([]string{"gardenio", "zeta"}, f.now(), true)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Farm: f, Cut: func(r *http.Request) FarmCut {
		if p := r.URL.Query().Get("p"); p != "" {
			return FarmCut{Scoped: true, Projects: strings.Split(p, ",")}
		}
		return FarmCut{}
	}}
	get := func(set string) []byte {
		t.Helper()
		req := httptest.NewRequest("GET", "/garden/farm?p="+set, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.farmPage(rec, req)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("%q: %d, Content-Encoding %q", set, rec.Code, rec.Header().Get("Content-Encoding"))
		}
		return rec.Body.Bytes()
	}
	inflate := func(gz []byte) []byte {
		t.Helper()
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(zr)
		return b
	}

	zgz, ggz, agz := get("zeta"), get("gardenio"), get("")
	if bytes.Equal(zgz, ggz) || bytes.Equal(zgz, agz) || bytes.Equal(ggz, agz) {
		t.Fatal("two audiences were served the same gzip")
	}
	if !bytes.Equal(get("zeta"), zgz) || !bytes.Equal(get("gardenio"), ggz) {
		t.Error("a second request for the same set was served different gzip")
	}
	_, zBody, zCached, _ := f.CutSnapshot(FarmCut{Scoped: true, Projects: []string{"zeta"}})
	_, gBody, _, _ := f.CutSnapshot(FarmCut{Scoped: true, Projects: []string{"gardenio"}})
	if &zCached[0] != &f.cuts["zeta"].doc.gz[0] {
		t.Error("the zeta gzip is not the one memoised on zeta's cut stream")
	}
	z, g := inflate(zgz), inflate(ggz)
	if !bytes.Equal(z, zBody) || !bytes.Equal(g, gBody) {
		t.Error("a served gzip does not inflate to its viewer's cut document")
	}
	noLeak(t, "zeta's gzip", z, gardenioOnly)
	noLeak(t, "gardenio's gzip", g, zetaOnly)
	_, admin, _ := f.Snapshot()
	if !bytes.Equal(inflate(agz), admin) {
		t.Error("the unscoped gzip is not the admin document")
	}
}
