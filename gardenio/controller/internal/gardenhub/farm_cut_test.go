package gardenhub

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

var updateCutGolden = flag.Bool("update-farm-cut-golden", false, "rewrite testdata/farm_admin_golden.json from multiLandWorld")

// zb is fb in project zeta.
func zb(id, typ, status, parent string, upd int64) *beadsapi.BeadDetail {
	b := fb(id, typ, status, parent, upd)
	b.Labels = []string{"project:zeta"}
	return b
}

// multiLandWorld is the fixture farm with a second land: zeta, a tenant land with a building plan, a ready seed, a
// finished plan, meadow beads, closed letters and an orphaned PR. Plan A (gardenio) carries a zeta step and a step blocked
// by a zeta bead; plan Z carries a gardenio step and a step blocked by a gardenio bead. One meadow bead and one PR carry
// no project at all. Both lands have a project bead, so the cost blob has a row each.
func multiLandWorld() *fakeFarmSrc {
	f := fixtureWorld()

	gp := projectBead("gd-projG", "gardenio", map[string]string{"budget_usd": "100", "budget_period": "month"})
	zp := projectBead("gd-projZ", "zeta", map[string]string{"land_kind": "tenant", "account": "lands-zeta"})
	f.add(gp, zp)

	pz := planBead("gd-planZ", "building", "in_progress", "gd-Z1")
	pz.Labels = []string{"project:zeta", "plan"}
	pz.Title = "Zeta storefront"
	z1 := zb("gd-Z1", "epic", "in_progress", "gd-planZ", 40)
	z1.Title = "Checkout"
	z1a := zb("gd-Z1a", "task", "in_progress", "gd-Z1", 880)
	z1a.Title = "Card form"
	z1b := zb("gd-Z1b", "task", "open", "gd-Z1", 870)
	z1b.Title = "Receipts after the IdP"
	z1c := fb("gd-Z1c", "task", "open", "gd-Z1", 860) // a gardenio step inside a zeta plan
	z1c.Title = "Operator-only: rotate the zeta signing key"
	z1d := zb("gd-Z1d", "decision", "open", "gd-Z1", 850)
	z1d.Title = "Which payment provider?"
	z1e := fb("gd-Z1e", "escalation", "open", "gd-Z1", 840) // a gardenio letter inside a zeta plan
	z1e.Title = "Operator: mint the zeta webhook secret"
	z1x := zb("gd-Z1x", "task", "closed", "gd-Z1", 30)
	z1x.Title = "Scaffold the shop"
	z1x.Fields["mr_url"], z1x.Fields["mr_iid"], z1x.Fields["mr_state"] = "https://github.com/o/r/pull/77", "#77", "merged"
	f.add(pz, z1, z1a, z1b, z1c, z1d, z1e, z1x)
	f.deps["gd-Z1b"] = []beadsapi.Dependency{{BeadID: "gd-Z1b", DependsOnID: "gd-A1a", Type: "blocks"}}
	f.deps["gd-Z1a"] = []beadsapi.Dependency{{BeadID: "gd-Z1a", DependsOnID: "gd-Z1d", Type: "blocks"}}

	// Plan A carries a zeta step, and one of its steps waits on a zeta bead.
	a2z := zb("gd-A2z", "task", "open", "gd-A2", 640)
	a2z.Title = "Zeta: move the shop onto the new IdP"
	f.add(a2z)
	f.deps["gd-A2c"] = []beadsapi.Dependency{{BeadID: "gd-A2c", DependsOnID: "gd-Z1a", Type: "blocks"}}

	sz := planBead("gd-seedZ", "ready", "open")
	sz.Labels = []string{"project:zeta", "plan"}
	sz.Title = "Zeta loyalty points"
	dz := planBead("gd-doneZ", "complete", "closed")
	dz.Labels = []string{"project:zeta", "plan"}
	dz.Title = "Zeta launch"
	f.add(sz, dz)

	mz := zb("gd-MZ", "task", "open", "", 120)
	mz.Title = "Zeta: the logo is blurry on phones"
	mzp := zb("gd-MZp", "bug", "open", "gd-notaplan", 110) // a zeta bead under a gardenio epic that is not a plan
	mzp.Title = "Zeta: price rounding"
	mn := fb("gd-MN", "chore", "open", "", 130)
	mn.Labels, mn.Title = nil, "Nobody's: clean the old buckets"
	f.add(mz, mzp, mn)

	for i := range 5 {
		f.add(zb("gd-zcd"+string(rune('a'+i)), "decision", "closed", "", 1))
	}
	for i := range 3 {
		f.add(zb("gd-zcw"+string(rune('a'+i)), "task", "closed", "", 1))
	}
	zgone := zb("gd-Zgone", "task", "closed", "", 2)
	zgone.Fields["mr_url"] = "https://github.com/o/r/pull/91"
	f.add(zgone,
		prBead("gd-pr91", "https://github.com/o/r/pull/91", "open", 300),
		prBead("gd-pr92", "https://github.com/o/r/pull/92", "open", 310))

	zg := agentBead("gd-gZeta", "g-zeta", "crew", "working", "gd-Z1a", 8800)
	zg.Labels = []string{"project:zeta"}
	f.add(zg)
	return f
}

func newCutFarm(t *testing.T, src FarmSource) *Farm {
	t.Helper()
	f := newTestFarm(src)
	f.SetSpendSource(&fakeSpend{byWin: map[time.Duration]map[string]float64{}})
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestFarmCutAdminGolden pins the unrestricted document byte for byte (compact, exactly as served): the per-viewer cut
// must leave the admin path's bytes and ETag as they were before it (gd-6UG1OGlqYg). The golden was written by the code
// before the cut existed; regenerate it with -update-farm-cut-golden only for an intended change to the document.
func TestFarmCutAdminGolden(t *testing.T) {
	f := newCutFarm(t, multiLandWorld())
	_, raw, _ := f.Snapshot()
	const path = "testdata/farm_admin_golden.json"
	if *updateCutGolden {
		if err := os.WriteFile(path, append(append([]byte{}, raw...), '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden (run with -update-farm-cut-golden): %v", err)
	}
	want = want[:len(want)-1]
	if string(want) != string(raw) {
		t.Fatalf("admin farm document drifted from %s", path)
	}

	// Served: the same bytes, and the same ETag shape, for every unrestricted way in.
	for name, h := range map[string]*Handler{
		"no cut wired": {Farm: f},
		"unscoped cut": {Farm: f, Cut: func(*http.Request) FarmCut { return FarmCut{} }},
	} {
		rec := httptest.NewRecorder()
		h.farmPage(rec, httptest.NewRequest("GET", "/garden/farm", nil))
		if rec.Body.String() != string(want) {
			t.Errorf("%s: served body differs from the golden", name)
		}
		if got, wantTag := rec.Header().Get("ETag"), "W/\"f1001-a1\""; got != wantTag {
			t.Errorf("%s: ETag = %s, want %s", name, got, wantTag)
		}
	}
}

// cutDoc is the scoped document for projects, decoded, with its raw bytes and ETag.
func cutDoc(t *testing.T, f *Farm, projects ...string) (snap, map[string]json.RawMessage, []byte, string) {
	t.Helper()
	etag, body, gz, _ := f.CutSnapshot(FarmCut{Scoped: true, Projects: projects})
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	unz, _ := io.ReadAll(zr)
	if !bytes.Equal(unz, body) {
		t.Fatal("the cut's gzip is not its body")
	}
	var s snap
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("cut document is not valid JSON: %v\n%s", err, body)
	}
	_ = json.Unmarshal(body, &doc)
	return s, doc, body, etag
}

func ids[T any](xs []T, id func(T) string) []string {
	out := []string{}
	for _, x := range xs {
		out = append(out, id(x))
	}
	return out
}

// A zeta member sees zeta's plan, seed, meadow, gardener and cost row, and nothing of gardenio: no plan, step, blocker,
// parent, gardener, PR or cost row. The word appears once, as the name of the project gd-Z1b waits on (blocked_by_foreign,
// gd-U2pbNv3lLC). The piles and closed plans are zeta's own, once its archives are
// read; until then they are the empty blobs a client already draws before the first slow read.
func TestFarmCutScopedViewer(t *testing.T) {
	src := multiLandWorld()
	f := newCutFarm(t, src)
	_, admin, _ := f.Snapshot()
	adminDoc := decodeSnap(t, f)

	_, doc, _, _ := cutDoc(t, f, "zeta")
	if string(doc["piles"]) != "{}" || string(doc["closed_plans"]) != `{"total":0,"shown":[]}` {
		t.Errorf("before zeta's archives are read: piles %s closed_plans %s, want the empty blobs", doc["piles"], doc["closed_plans"])
	}
	select {
	case <-f.dirty:
	default:
		t.Error("a cut wanting an unread archive did not ask for a refresh")
	}
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, doc, raw, _ := cutDoc(t, f, "zeta")

	if n := bytes.Count(raw, []byte("gardenio")); n != 1 || !bytes.Contains(raw, []byte(`"blocked_by_foreign":["gardenio"]`)) {
		t.Errorf("the zeta cut names gardenio %d times, want once, as gd-Z1b's foreign blocker", n)
	}
	for _, leak := range []string{"gd-planA", "gd-A1a", "gd-A2", "gd-Z1c", "gd-Z1e", "gd-seed1", "gd-M1", "gd-MN", "gd-notaplan", "gd-done1", "gd-pr92", "gd-gVisit", "gd-gAway", "gd-gWork1", "gd-wish"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("the zeta cut carries %q", leak)
		}
	}

	if got := ids(s.Plans, func(p FarmPlan) string { return p.ID }); strings.Join(got, ",") != "gd-planZ" {
		t.Fatalf("plans = %v, want [gd-planZ]", got)
	}
	pz := s.Plans[0]
	z1 := pz.Phases[0]
	if got := ids(z1.Tasks, func(t FarmTask) string { return t.ID }); strings.Contains(strings.Join(got, ","), "gd-Z1c") || len(got) != 3 {
		t.Errorf("phase Z1 tasks = %v, want gd-Z1a, gd-Z1b and gd-Z1d (the gardenio step and letter dropped)", got)
	}
	if b := findTask(z1.Tasks, "gd-Z1b"); b == nil || len(b.BlockedBy) != 0 || b.BlockedByProject != nil || strings.Join(b.BlockedByForeign, ",") != "gardenio" {
		t.Errorf("gd-Z1b = %+v, want its gardenio blocker's id gone and the project named", b)
	}
	if a := findTask(z1.Tasks, "gd-Z1a"); a == nil || strings.Join(a.BlockedBy, ",") != "gd-Z1d" {
		t.Errorf("gd-Z1a = %+v, want its zeta blocker kept", a)
	}
	n := z1.Counts.InProgress + z1.Counts.Review + z1.Counts.Blocked + z1.Counts.Open + z1.Counts.Deferred
	if n != len(z1.Tasks) || z1.More != 0 {
		t.Errorf("phase Z1 counts %+v more %d, want them recounted over the %d steps kept", z1.Counts, z1.More, len(z1.Tasks))
	}
	if pz.Counts.Open != z1.Counts.Open+pz.Unphased.Counts.Open {
		t.Errorf("plan counts %+v are not the sum of its phases", pz.Counts)
	}
	if pz.OpenDecisions != 1 || pz.OpenEscalations != 0 {
		t.Errorf("letters = %d decisions, %d escalations; want 1, 0", pz.OpenDecisions, pz.OpenEscalations)
	}
	for _, w := range pz.Waiting {
		if w.ID == "gd-Z1e" {
			t.Error("the gardenio escalation is still waiting on the zeta plan")
		}
	}
	if az := findPlan(adminDoc, "gd-planZ"); az == nil || az.OpenEscalations != 1 || len(az.Phases[0].Tasks) != 5 {
		t.Errorf("the admin plan Z should still carry all five steps and the escalation: %+v", az)
	}

	if got := ids(s.Seeds, func(x FarmSeed) string { return x.ID }); strings.Join(got, ",") != "gd-seedZ" {
		t.Errorf("seeds = %v, want [gd-seedZ]", got)
	}
	if got := ids(s.Meadow.Shown, func(x FarmTask) string { return x.ID }); s.Meadow.Total != 2 || strings.Join(got, ",") != "gd-MZ,gd-MZp" {
		t.Errorf("meadow = %d %v, want gd-MZ and gd-MZp", s.Meadow.Total, got)
	}
	if got := ids(s.Gardeners, func(x FarmGardener) string { return x.ID }); strings.Join(got, ",") != "gd-gZeta" {
		t.Errorf("gardeners = %v, want only gd-gZeta (gd-gVisit stands on a gardenio plot, gd-gAway on a gardenio bead)", got)
	}
	if string(doc["wishes"]) != emptyWishes {
		t.Errorf("wishes = %s, want the empty blob: zeta has no wish and gardenio's are cut away", doc["wishes"])
	}
	zrows := 0
	for _, b := range src.beads {
		if b.Status != "closed" && in(farmWorkTypes, b.Type) && projectOf(b) == "zeta" {
			zrows++
		}
	}
	if s.Meta.Rows != zrows || s.Meta.RowCap != farmRowCap || s.Meta.Schema != FarmSchema {
		t.Errorf("meta = %+v, want rows %d (zeta's open rows), the cap and the schema", s.Meta, zrows)
	}

	want := FarmPiles{ClosedDecisions: 5, ClosedWork: 5, ClosedPlans: 1, OrphanMRTotal: 1}
	if p := s.Piles; p.ClosedDecisions != want.ClosedDecisions || p.ClosedEscalations != 0 || p.ClosedWork != want.ClosedWork || p.ClosedPlans != 1 || p.OrphanMRTotal != 1 || len(p.OrphanMRs) != 1 || p.OrphanMRs[0].URL != "https://github.com/o/r/pull/91" {
		t.Errorf("piles = %+v, want zeta's: %+v and PR 91", p, want)
	}
	if s.ClosedPlans.Total != 1 || len(s.ClosedPlans.Shown) != 1 || s.ClosedPlans.Shown[0].ID != "gd-doneZ" {
		t.Errorf("closed plans = %+v, want gd-doneZ", s.ClosedPlans)
	}

	var cost FarmCost
	if err := json.Unmarshal(doc["cost"], &cost); err != nil {
		t.Fatal(err)
	}
	if len(cost.Projects) != 1 || cost.Projects[0].Project != "zeta" {
		t.Fatalf("cost rows = %+v, want zeta's only", cost.Projects)
	}
	row := cost.Projects[0]
	if row.Purse == nil || row.Purse.GrantedUSD != nil || row.Purse.SpentUSD != nil || row.Purse.RemainingUSD != nil || row.Purse.LLMMultiplier != 0 {
		t.Errorf("zeta purse = %+v, want credits only", row.Purse)
	}
	if row.TodayUSD != nil || row.WeekUSD != nil || row.SpentUSD != nil || row.RemainingUSD != nil || row.BudgetUSD != nil {
		t.Errorf("zeta row = %+v, want no at-cost dollars on a tenant land", row)
	}

	// The archives and the cut leave the admin document alone.
	if _, after, _ := f.Snapshot(); !bytes.Equal(after, admin) {
		t.Error("the admin document moved after a scoped read")
	}
}

// TestFarmCutGardenerCost: a scoped viewer sees a gardener's cost only on an operator land, and there without its
// gardener's rollup (crew the cut cannot place in the set); a tenant land gets no at-cost dollar figure on a gardener,
// as on its cost row, and with no cost blob no land can be called an operator's, so no gardener carries a cost
// (gd-0VeG81uW3w).
func TestFarmCutGardenerCost(t *testing.T) {
	src := multiLandWorld()
	src.mutate("gd-gZeta", func(b *beadsapi.BeadDetail) { b.Fields["total_cost_usd"] = "5.25" })
	f := newCutFarm(t, src)

	admin := decodeSnap(t, f)
	if z := findGardener(admin, "gd-gZeta"); z == nil || z.Cost == nil || usdString(z.Cost.SpentUSD) != "$5.25" {
		t.Fatalf("admin gd-gZeta = %+v, want its $5.25", z)
	}
	if fm := findGardener(admin, "gd-gForeman"); fm == nil || fm.Cost == nil || usdString(fm.Cost.GardenerUSD) != "$40.79" {
		t.Fatalf("admin gd-gForeman = %+v, want its gardener's $40.79", fm)
	}

	s, _, raw, _ := cutDoc(t, f, "zeta")
	z := findGardener(s, "gd-gZeta")
	if z == nil || z.Cost != nil {
		t.Fatalf("zeta cut gd-gZeta = %+v, want it shown with no cost on a tenant land", z)
	}
	if bytes.Contains(raw, []byte("5.25")) {
		t.Error("the zeta cut carries the tenant gardener's at-cost dollars")
	}

	s, _, raw, _ = cutDoc(t, f, "gardenio")
	fm := findGardener(s, "gd-gForeman")
	if fm == nil || fm.Cost == nil || usdString(fm.Cost.SpentUSD) != "$0.67" || fm.Cost.GardenerUSD != nil {
		t.Fatalf("gardenio cut gd-gForeman = %+v, want its own $0.67 and no gardener rollup", fm)
	}
	if fm.Cost.Text != "Spent at least $0.67; this session is counted in full when it ends." {
		t.Errorf("gardenio cut gd-gForeman text = %q, want its own figure only", fm.Cost.Text)
	}
	if bytes.Contains(raw, []byte("40.79")) {
		t.Error("the gardenio cut carries a gardener's rollup")
	}
	if w := findGardener(s, "gd-gWork1"); w == nil || w.Cost == nil || usdString(w.Cost.SpentUSD) != "$3.20" {
		t.Errorf("gardenio cut gd-gWork1 = %+v, want its $3.20 on an operator land", w)
	}

	nb := newTestFarm(multiLandWorld())
	if err := nb.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, _, _, _ = cutDoc(t, nb, "gardenio")
	if fm := findGardener(s, "gd-gForeman"); fm == nil || fm.Cost != nil {
		t.Errorf("no cost blob: gardenio cut gd-gForeman = %+v, want no cost", fm)
	}
}

// TestFarmCutPlanCost: a scoped viewer sees a plan's or a seed's crew spend only on an operator land (a cost row with
// no purse), and only when the cut hides none of its steps, since the figure sums the crew of every step. A tenant land
// gets no at-cost dollars on a plan, a seed or a closed plan, as on its cost row; a closed plan carries no cost in any
// scoped document (the cut cannot see its steps); with no cost blob no land is an operator's (gd-kcy4IFNzv5).
func TestFarmCutPlanCost(t *testing.T) {
	src := multiLandWorld()
	src.mutate("gd-planZ", func(b *beadsapi.BeadDetail) { setPlanSpend(b, "7.7700", baseUnix, "2", "1") })
	src.mutate("gd-seedZ", func(b *beadsapi.BeadDetail) { setPlanSpend(b, "1.1100", baseUnix, "1", "0") })
	src.mutate("gd-doneZ", func(b *beadsapi.BeadDetail) { setPlanSpend(b, "3.3300", baseUnix, "1", "0") })
	src.mutate("gd-seed1", func(b *beadsapi.BeadDetail) { setPlanSpend(b, "2.2200", baseUnix, "1", "0") })
	f := newCutFarm(t, src)
	admin := decodeSnap(t, f)
	for id, want := range map[string]float64{"gd-planA": 12.35, "gd-planZ": 7.77} {
		if p := findPlan(admin, id); p == nil || !p.Cost.Has || p.Cost.USD == nil || *p.Cost.USD != want {
			t.Fatalf("admin %s = %+v, want its $%.2f", id, p, want)
		}
	}
	if sd := findSeed(admin, "gd-seedZ"); sd == nil || !sd.Cost.Has {
		t.Fatalf("admin gd-seedZ = %+v, want its $1.11", sd)
	}
	// Read both lands' archives, so the closed plans are in the cuts below.
	cutDoc(t, f, "zeta", "gardenio")
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	unknown := FarmPlanCost{}

	s, _, raw, _ := cutDoc(t, f, "zeta")
	if pz := findPlan(s, "gd-planZ"); pz == nil || pz.Cost != unknown {
		t.Errorf("zeta cut gd-planZ = %+v, want it shown with the unknown cost on a tenant land", pz)
	}
	if sz := findSeed(s, "gd-seedZ"); sz == nil || sz.Cost.Has || sz.Cost.USD != nil {
		t.Errorf("zeta cut gd-seedZ = %+v, want no cost on a tenant land", sz)
	}
	if len(s.ClosedPlans.Shown) != 1 || s.ClosedPlans.Shown[0].ID != "gd-doneZ" || s.ClosedPlans.Shown[0].Cost.Has {
		t.Errorf("zeta cut closed plans = %+v, want gd-doneZ with no cost", s.ClosedPlans.Shown)
	}
	for _, leak := range []string{"7.77", "1.11", "3.33", `"crew":2`} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("the zeta cut carries a tenant plan's spend %s", leak)
		}
	}

	s, _, raw, _ = cutDoc(t, f, "gardenio")
	if pa := findPlan(s, "gd-planA"); pa == nil || pa.Cost != unknown {
		t.Errorf("gardenio cut gd-planA = %+v, want the unknown cost: the cut hides its zeta step gd-A2z", pa)
	}
	if bytes.Contains(raw, []byte("12.35")) {
		t.Error("the gardenio cut carries plan A's spend, which sums a hidden zeta step's crew")
	}
	if sd := findSeed(s, "gd-seed1"); sd == nil || !sd.Cost.Has || sd.Cost.USD == nil || *sd.Cost.USD != 2.22 {
		t.Errorf("gardenio cut gd-seed1 = %+v, want its $2.22 on an operator land", sd)
	}
	for _, c := range s.ClosedPlans.Shown {
		if c.Cost.Has {
			t.Errorf("gardenio cut closed plan %s = %+v, want no cost", c.ID, c.Cost)
		}
	}

	// Both lands: plan A hides nothing and keeps its figure; plan Z is still a tenant's.
	s, _, _, _ = cutDoc(t, f, "zeta", "gardenio")
	if pa := findPlan(s, "gd-planA"); pa == nil || !pa.Cost.Has || pa.Cost.USD == nil || *pa.Cost.USD != 12.35 || pa.Cost.Crew != 3 {
		t.Errorf("both-lands cut gd-planA = %+v, want its $12.35 and 3 crew", pa)
	}
	if pz := findPlan(s, "gd-planZ"); pz == nil || pz.Cost != unknown {
		t.Errorf("both-lands cut gd-planZ = %+v, want the unknown cost on a tenant land", pz)
	}

	nb := newTestFarm(src)
	if err := nb.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, _, _, _ = cutDoc(t, nb, "gardenio")
	if sd := findSeed(s, "gd-seed1"); sd == nil || sd.Cost.Has {
		t.Errorf("no cost blob: gardenio cut gd-seed1 = %+v, want no cost", sd)
	}
}

// Inside a plan the viewer can see, a step labelled for a land they cannot is dropped, its phase recounted, and a
// blocker in that land is cut from the step it blocks, leaving only the land's name.
func TestFarmCutForeignStepInVisiblePlan(t *testing.T) {
	f := newCutFarm(t, multiLandWorld())
	adminA := findPlan(decodeSnap(t, f), "gd-planA")
	s, _, raw, _ := cutDoc(t, f, "gardenio")
	a := findPlan(s, "gd-planA")
	if a == nil || findPlan(s, "gd-planZ") != nil {
		t.Fatalf("a gardenio viewer's plans: %v", ids(s.Plans, func(p FarmPlan) string { return p.ID }))
	}
	if bytes.Contains(raw, []byte("gd-A2z")) || bytes.Contains(raw, []byte("gd-Z1")) || bytes.Contains(raw, []byte("Zeta")) {
		t.Error("the gardenio cut carries a zeta step or blocker")
	}
	var adminA2, a2 FarmPhase
	for i := range a.Phases {
		if a.Phases[i].ID == "gd-A2" {
			a2, adminA2 = a.Phases[i], adminA.Phases[i]
		}
	}
	if a2.Counts.Open != adminA2.Counts.Open-1 || a.Counts.Open != adminA.Counts.Open-1 {
		t.Errorf("A2 open %d (admin %d), plan open %d (admin %d): want one fewer for the zeta step", a2.Counts.Open, adminA2.Counts.Open, a.Counts.Open, adminA.Counts.Open)
	}
	if c := findTask(a2.Tasks, "gd-A2c"); c == nil || len(c.BlockedBy) != 0 || strings.Join(c.BlockedByForeign, ",") != "zeta" {
		t.Errorf("gd-A2c = %+v, want its zeta blocker cut to the name zeta", c)
	}
	if b := findTask(a2.Tasks, "gd-A2b"); b == nil || strings.Join(b.BlockedBy, ",") != "gd-A2a" {
		t.Errorf("gd-A2b = %+v, want its gardenio blocker kept", b)
	}
	// Seeing both lands is the admin plan again, byte for byte.
	_, _, both, _ := cutDoc(t, f, "zeta", "gardenio")
	var d struct{ Plans []json.RawMessage }
	_ = json.Unmarshal(both, &d)
	for _, p := range d.Plans {
		if bytes.Contains(p, []byte(`"id":"gd-planA"`)) && !bytes.Equal(p, f.plans["gd-planA"].raw) {
			t.Error("a cut that admits every land plan A names should send its admin entry unchanged")
		}
	}
}

// A scoped principal with no projects (no project groups, an applicant) gets an empty but valid farm.
func TestFarmCutEmptySet(t *testing.T) {
	f := newCutFarm(t, multiLandWorld())
	s, doc, raw, _ := cutDoc(t, f)
	if len(s.Plans)+len(s.Seeds)+len(s.Gardeners)+s.Meadow.Total+s.Meta.Rows+s.ClosedPlans.Total != 0 {
		t.Errorf("empty cut is not empty: %s", raw)
	}
	if bytes.Contains(raw, []byte("gd-")) {
		t.Errorf("empty cut names a bead: %s", raw)
	}
	if string(doc["piles"]) == "{}" || string(doc["cost"]) != `{"projects":[]}` {
		t.Errorf("piles %s cost %s: want zero counts (nothing to read) and no cost rows", doc["piles"], doc["cost"])
	}
	for _, k := range []string{"plans", "seeds", "gardeners"} {
		if string(doc[k]) != "[]" {
			t.Errorf("%s = %s, want []", k, doc[k])
		}
	}
}

// A scoped viewer's purse is FarmCost.ForProjects: scope project, today and week summed over the set's rows in dollars,
// never the fleet's figures or its account budgets. A tenant land's row is credits only and adds no dollars; a down
// rollup leaves the sums unknown with the stale marker, never $0.
func TestFarmCutCostPurse(t *testing.T) {
	src := multiLandWorld()
	src.add(projectBead("gd-projE", "eta", map[string]string{}))
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	todayWin, monthWin := costWindows(now)
	sp := &fakeSpend{byWin: map[time.Duration]map[string]float64{
		todayWin:           {"gardenio": 2, "eta": 1.25, "zeta": 5, "unbeaded": 10},
		7 * 24 * time.Hour: {"gardenio": 20, "eta": 4, "zeta": 50, "unbeaded": 100},
		monthWin:           {"gardenio": 30, "zeta": 60},
	}}
	f := newTestFarm(src)
	f.now = func() time.Time { return now }
	f.SetSpendSource(&fakeBudget{fakeSpend: sp, budget: FleetBudget{Accounts: 1, BudgetUSD: 500, SpentUSD: 200, RemainingUSD: 300}})
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	cutCostOf := func(projects ...string) (FarmCost, string) {
		t.Helper()
		_, doc, _, _ := cutDoc(t, f, projects...)
		var c FarmCost
		if err := json.Unmarshal(doc["cost"], &c); err != nil {
			t.Fatal(err)
		}
		return c, string(doc["cost"])
	}

	var admin FarmCost
	if err := json.Unmarshal(farmDoc(t, f)["cost"], &admin); err != nil {
		t.Fatal(err)
	}
	if admin.Scope != CostScopeFleet || !eqF(admin.TodayUSD, f64(18.25)) || !admin.HasBudget || !eqF(admin.RemainingUSD, f64(300)) {
		t.Fatalf("admin purse = %+v; want the fleet's $18.25 today and $300 of account budget left", admin)
	}

	c, raw := cutCostOf("gardenio", "eta")
	if c.Scope != CostScopeProject || !c.Has || !eqF(c.TodayUSD, f64(3.25)) || !eqF(c.WeekUSD, f64(24)) || c.AsOf != 0 {
		t.Errorf("gardenio+eta purse = %s; want project scope, $3.25 today, $24 week", raw)
	}
	if c.HasBudget || c.BudgetUSD != nil || c.SpentUSD != nil || c.RemainingUSD != nil {
		t.Errorf("gardenio+eta purse = %s; want no account budgets", raw)
	}
	if len(c.Projects) != 2 || c.Projects[1].Project != "gardenio" || !eqF(c.Projects[1].RemainingUSD, f64(70)) {
		t.Errorf("gardenio+eta rows = %+v; want eta, then gardenio with its own $70 left", c.Projects)
	}

	// A tenant land in the set adds no dollars; a tenant alone has no dollar figures at all.
	if c, raw = cutCostOf("gardenio", "zeta"); !c.Has || !eqF(c.TodayUSD, f64(2)) || !eqF(c.WeekUSD, f64(20)) || c.HasBudget {
		t.Errorf("gardenio+zeta purse = %s; want gardenio's $2 today, $20 week only", raw)
	}
	c, raw = cutCostOf("zeta")
	if c.Scope != CostScopeProject || c.Has || c.TodayUSD != nil || c.WeekUSD != nil || c.AsOf != 0 || c.HasBudget || c.BudgetUSD != nil || c.RemainingUSD != nil {
		t.Errorf("zeta purse = %s; want project scope and no at-cost dollars", raw)
	}
	if len(c.Projects) != 1 || c.Projects[0].Purse == nil {
		t.Errorf("zeta rows = %s; want its row with the credits purse", raw)
	}

	// The rollup goes down: the scoped sums are unknown, with the last good read as the marker.
	good := now.Unix()
	sp.mu.Lock()
	sp.down = true
	sp.mu.Unlock()
	now = now.Add(farmCostEvery)
	f.slow.at = time.Time{}
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, raw = cutCostOf("gardenio", "eta"); c.Scope != CostScopeProject || c.Has || c.TodayUSD != nil || c.WeekUSD != nil || c.AsOf != good {
		t.Errorf("gardenio+eta purse with the rollup down = %s; want no figures, as_of %d", raw, good)
	}
}

// The memo is keyed by the sorted set: an order or a duplicate is the same document; two sets never share an ETag,
// and a version that moved nothing in a cut keeps its ETag.
func TestFarmCutMemoAndETag(t *testing.T) {
	src := multiLandWorld()
	f := newCutFarm(t, src)
	f.wantArchivesLocked([]string{"gardenio", "zeta"}, f.now(), true)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, zb1, zeta := cutDoc(t, f, "zeta")
	_, _, _, gard := cutDoc(t, f, "gardenio")
	_, _, _, both := cutDoc(t, f, "zeta", "gardenio")
	_, _, _, both2 := cutDoc(t, f, "gardenio", "zeta", "zeta")
	if !strings.HasPrefix(zeta, `W/"s`) || zeta == gard || zeta == both || gard == both {
		t.Errorf("ETags zeta %s gardenio %s both %s: want distinct cut tags", zeta, gard, both)
	}
	if both != both2 || len(f.cuts) != 3 {
		t.Errorf("reordered set: %s vs %s, %d cut streams; want one per sorted set", both, both2, len(f.cuts))
	}
	admin, _, _ := f.snapshotTag()
	if zeta == admin {
		t.Error("a cut shares the admin ETag")
	}

	// A gardenio-only change: a new version, the zeta cut's ETag unchanged, the gardenio one moved.
	src.mutate("gd-A9", func(b *beadsapi.BeadDetail) { b.Title = "Rotate the staging signing key, again" })
	v := f.st.version
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.st.version == v {
		t.Fatal("the change did not move the version")
	}
	zs2, _, _, zeta2 := cutDoc(t, f, "zeta")
	gs2, _, _, gard2 := cutDoc(t, f, "gardenio")
	if zeta2 != zeta || gard2 == gard {
		t.Errorf("after a gardenio change: zeta %s -> %s, gardenio %s -> %s; want zeta kept, gardenio moved", zeta, zeta2, gard, gard2)
	}
	// The v of a cut is its stream's (farm_cut_stream.go): it moves only when the cut does.
	var zs1 snap
	_ = json.Unmarshal(zb1, &zs1)
	if zs2.V != zs1.V || gs2.V == zs2.V || zs2.V == f.st.version {
		t.Errorf("cut versions: zeta %d -> %d, gardenio %d, farm %d; want zeta's kept and each its own", zs1.V, zs2.V, gs2.V, f.st.version)
	}

	// Served: a scoped viewer revalidates to a 304, another set's ETag is a 200 with its own body.
	h := &Handler{Farm: f, Cut: func(r *http.Request) FarmCut {
		return FarmCut{Scoped: true, Projects: strings.Split(r.URL.Query().Get("p"), ",")}
	}}
	get := func(set, inm string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/garden/farm?p="+set, nil)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		rec := httptest.NewRecorder()
		h.farmPage(rec, req)
		return rec
	}
	if rec := get("zeta", zeta); rec.Code != http.StatusNotModified {
		t.Errorf("zeta revalidation = %d, want 304", rec.Code)
	}
	rec := get("gardenio", zeta)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != gard2 || bytes.Contains(rec.Body.Bytes(), []byte("gd-planZ")) {
		t.Errorf("gardenio viewer with zeta's ETag: %d %s", rec.Code, rec.Header().Get("ETag"))
	}
	if rec.Header().Get("Cache-Control") != "private, no-cache" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

// The archives are read only for the projects a scoped request wanted, at most every farmSlowEvery, and dropped once
// nobody has asked for farmArchiveWantTTL. With no scoped viewer the farm makes no per-project read at all.
func TestFarmCutArchivesAreLazy(t *testing.T) {
	src := multiLandWorld()
	f := newCutFarm(t, src)
	now := f.now()
	f.now = func() time.Time { return now }
	before := src.counts
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.counts != before {
		t.Errorf("a refresh with no scoped viewer made %d count reads", src.counts-before)
	}
	cutDoc(t, f, "zeta")
	before = src.counts
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := src.counts - before; got != 4 {
		t.Errorf("zeta's archive read made %d count reads, want 4 (decisions, escalations, work, wishes)", got)
	}
	before = src.counts
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.counts != before {
		t.Error("the archive was re-read inside farmSlowEvery")
	}
	now = now.Add(farmArchiveWantTTL)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.arch["zeta"]; ok || len(f.archWant) != 0 {
		t.Error("an archive nobody asked for in farmArchiveWantTTL is still kept")
	}
}

// An unscoped cut is the whole farm: Snapshot's bytes, its ETag and its gzip.
func TestFarmCutUnscopedIsTheWholeFarm(t *testing.T) {
	f := newCutFarm(t, multiLandWorld())
	etag, body, gz, _ := f.CutSnapshot(FarmCut{})
	wantTag, want, _ := f.snapshotTag()
	_, wantGz := f.SnapshotGzip()
	if etag != wantTag || !bytes.Equal(body, want) || !bytes.Equal(gz, wantGz) {
		t.Errorf("unscoped cut: %s, %d bytes; want %s, %d bytes and the same gzip", etag, len(body), wantTag, len(want))
	}
}

// wishWorld is multiLandWorld with zeta's wishes (gd-gUeBN96RRL). The open one has kin in zeta (gd-Z1a, tended, on plan
// Z; gd-A2z, on gardenio's plan A), a gardenio kin (gd-Z1c), and a closed kin each in zeta and gardenio. The closed one's
// kin merged an MR each in zeta and gardenio, and its close reason names a gardenio bead.
func wishWorld() *fakeFarmSrc {
	src := multiLandWorld()
	head := func(id, status string, upd int64) *beadsapi.BeadDetail {
		b := zb(id, "task", status, "", upd)
		b.Labels = append(b.Labels, "source:slack", "thread")
		return b
	}
	zw := head("gd-zw1", "open", 900)
	zw.Title = "Slack thread: can the shop take cards?"
	zwx := head("gd-zwx", "closed", 905)
	zwx.Title = "Slack thread: fix the zeta receipts"
	zwx.Fields["close_reason"] = "superseded by gd-A1a"
	zk := zb("gd-zk1", "task", "closed", "", 901)
	zk.Fields["mr_url"], zk.Fields["mr_iid"], zk.Fields["mr_state"] = "https://github.com/o/z/pull/5", "#5", "merged"
	gk := fb("gd-gk1", "task", "closed", "", 902)
	gk.Fields["mr_url"], gk.Fields["mr_iid"], gk.Fields["mr_state"] = "https://github.com/o/r/pull/96", "#96", "merged"
	zc := zb("gd-zc1", "task", "closed", "", 903)
	gc := fb("gd-gc1", "task", "closed", "", 904)
	src.add(zw, zwx, zk, gk, zc, gc)
	for _, id := range []string{"gd-Z1a", "gd-A2z", "gd-Z1c", "gd-zc1", "gd-gc1"} {
		src.mutate(id, func(b *beadsapi.BeadDetail) { b.Fields["thread_id"] = "gd-zw1" })
	}
	for _, id := range []string{"gd-zk1", "gd-gk1"} {
		src.mutate(id, func(b *beadsapi.BeadDetail) { b.Fields["thread_id"] = "gd-zwx" })
	}
	return src
}

// A zeta member sees zeta's wishes and, inside them, only what is zeta's: no gardenio wish, kin, plot, produce or close
// reason, and the counts recounted over what shows. closed_total is zeta's own: the closed heads among the kept rows
// until zeta's archives are read, then the daemon's per-project count, never the fleet's.
func TestFarmCutWishes(t *testing.T) {
	f := newCutFarm(t, wishWorld())
	admin := decodeSnap(t, f)
	if admin.Wishes.OpenTotal != 3 || admin.Wishes.ClosedTotal != 4 {
		t.Fatalf("admin wishes: open %d closed %d, want the fleet's 3 and 4", admin.Wishes.OpenTotal, admin.Wishes.ClosedTotal)
	}

	check := func(when string, w FarmWishes, raw []byte) {
		t.Helper()
		for _, leak := range []string{"gd-wish", "gd-Z1c", "gd-gc1", "gd-gk1", "pull/96", "gd-planA", "gd-A1a"} {
			if bytes.Contains(raw, []byte(leak)) {
				t.Errorf("%s: the zeta cut carries %q", when, leak)
			}
		}
		if w.OpenTotal != 1 || len(w.Open) != 1 || w.OpenMore != nil {
			t.Fatalf("%s: open = %d %+v, want zeta's one wish", when, w.OpenTotal, w.Open)
		}
		o := w.Open[0]
		if o.ID != "gd-zw1" || strings.Join(o.Kin, ",") != "gd-Z1a,gd-A2z" || o.KinTotal != 2 || o.KinClosed != 1 || !o.Tending {
			t.Errorf("%s: open wish = %+v, want kin gd-Z1a and gd-A2z (2), one closed kin, tended", when, o)
		}
		if len(o.Plots) != 1 || o.Plots[0] != (FarmPlot{Plan: "gd-planZ", Phase: "gd-Z1"}) {
			t.Errorf("%s: plots = %+v, want only plan Z's (gd-A2z's plot is on gardenio's plan A)", when, o.Plots)
		}
		if len(w.Closed) != 1 {
			t.Fatalf("%s: closed = %+v, want zeta's one headstone", when, w.Closed)
		}
		c := w.Closed[0]
		if c.ID != "gd-zwx" || c.Kin != 1 || c.Reason != "" || c.Outcome != WishDone {
			t.Errorf("%s: headstone = %+v, want one kin, done, no reason", when, c)
		}
		if len(c.Produce) != 1 || c.Produce[0].ID != "gd-zk1" || c.ProduceMore != 0 {
			t.Errorf("%s: headstone produce = %+v, want only zeta's MR", when, c.Produce)
		}
		if w.ClosedTotal != 1 {
			t.Errorf("%s: closed_total = %d, want zeta's 1", when, w.ClosedTotal)
		}
	}
	s, _, raw, _ := cutDoc(t, f, "zeta")
	check("before the archives", s.Wishes, raw)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.arch["zeta"].wishes != 1 {
		t.Errorf("zeta's archive counts %d closed wishes, want 1", f.arch["zeta"].wishes)
	}
	s, _, raw, _ = cutDoc(t, f, "zeta")
	check("after the archives", s.Wishes, raw)

	// The admin document still carries every kin, plot, produce item and reason.
	var zw *FarmWish
	for i := range admin.Wishes.Open {
		if admin.Wishes.Open[i].ID == "gd-zw1" {
			zw = &admin.Wishes.Open[i]
		}
	}
	if zw == nil || zw.KinTotal != 3 || zw.KinClosed != 2 || len(zw.Plots) != 2 {
		t.Errorf("admin gd-zw1 = %+v, want all three kin, both closed kin and both plots", zw)
	}

	// A viewer of both lands sees both lands' wishes, and the gardenio kin of a zeta wish.
	both, _, _, _ := cutDoc(t, f, "gardenio", "zeta")
	if both.Wishes.OpenTotal != 3 || both.Wishes.ClosedTotal != 4 {
		t.Errorf("gardenio+zeta: open %d closed %d, want 3 and 4", both.Wishes.OpenTotal, both.Wishes.ClosedTotal)
	}
}

// A step the viewer can see that waits on beads they cannot read keeps whom it waits on and nothing else (gd-U2pbNv3lLC,
// decided on gd-pnmEvqmfBW): one blocked_by_foreign entry per foreign project, sorted, "" for a blocker with no project:
// label; never the blocker's id, title, status or how many there are. A kept blocker stays an id. The admin document has
// no blocked_by_foreign at all.
func TestFarmCutNamesForeignBlockers(t *testing.T) {
	src := multiLandWorld()
	bare := fb("gd-Nobody1", "task", "open", "", 95)
	bare.Labels, bare.Title = nil, "Unlabelled: the secret rotation"
	src.add(bare)
	// gd-A2c already waits on gd-Z1a (zeta); add a second zeta blocker, an unlabelled one and a gardenio one.
	src.deps["gd-A2c"] = append(src.deps["gd-A2c"],
		beadsapi.Dependency{BeadID: "gd-A2c", DependsOnID: "gd-Z1b", Type: "blocks"},
		beadsapi.Dependency{BeadID: "gd-A2c", DependsOnID: "gd-Nobody1", Type: "blocks"},
		beadsapi.Dependency{BeadID: "gd-A2c", DependsOnID: "gd-A2a", Type: "blocks"})
	f := newCutFarm(t, src)

	_, admin, _ := f.Snapshot()
	if bytes.Contains(admin, []byte("blocked_by_foreign")) {
		t.Fatal("the admin document carries blocked_by_foreign")
	}
	ac := findTask(findPlan(decodeSnap(t, f), "gd-planA").Phases[1].Tasks, "gd-A2c")
	if ac == nil || strings.Join(ac.BlockedBy, ",") != "gd-A2a,gd-Nobody1,gd-Z1a,gd-Z1b" {
		t.Fatalf("admin gd-A2c = %+v, want all four blockers", ac)
	}

	s, _, raw, _ := cutDoc(t, f, "gardenio")
	var c *FarmTask
	for _, ph := range findPlan(s, "gd-planA").Phases {
		if x := findTask(ph.Tasks, "gd-A2c"); x != nil {
			c = x
		}
	}
	if c == nil {
		t.Fatal("gd-A2c is missing from the gardenio cut")
	}
	if strings.Join(c.BlockedBy, ",") != "gd-A2a" || c.BlockedByProject != nil {
		t.Errorf("gd-A2c blocked_by %v blocked_by_project %v, want only its gardenio blocker", c.BlockedBy, c.BlockedByProject)
	}
	if got := c.BlockedByForeign; len(got) != 2 || got[0] != "" || got[1] != "zeta" {
		t.Errorf("gd-A2c blocked_by_foreign = %q, want [\"\" zeta]: one entry per project, the unlabelled one unnamed", got)
	}
	if !bytes.Contains(raw, []byte(`"blocked_by":["gd-A2a"],"blocked_by_foreign":["","zeta"]`)) {
		t.Errorf("wire shape: gd-A2c not as expected in %s", raw)
	}
	for _, leak := range []string{"gd-Nobody1", "gd-Z1a", "gd-Z1b", "secret rotation", "Card form", "Receipts"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("the gardenio cut carries the foreign blocker's %q", leak)
		}
	}

	// A viewer of both lands gets the zeta blockers as ids again; only the unlabelled one is named.
	s, _, _, _ = cutDoc(t, f, "gardenio", "zeta")
	for _, ph := range findPlan(s, "gd-planA").Phases {
		if x := findTask(ph.Tasks, "gd-A2c"); x != nil {
			if strings.Join(x.BlockedBy, ",") != "gd-A2a,gd-Z1a,gd-Z1b" || strings.Join(x.BlockedByForeign, ",") != "" || len(x.BlockedByForeign) != 1 {
				t.Errorf("both lands: gd-A2c = %+v, want the zeta ids kept and only the unlabelled blocker named", x)
			}
		}
	}
}

// A gardener on a plot outside the viewer's lands is not in their document, and one that walks across follows its plot:
// it leaves the cut when it walks out of the viewer's region and appears when it walks in (gd-U2pbNv3lLC). The walker is a
// zeta gardener, so the gardenio viewer never sees it, wherever it stands.
func TestFarmCutGardenerWalksAcrossRegions(t *testing.T) {
	src := multiLandWorld()
	f := newCutFarm(t, src)
	has := func(projects ...string) (bool, []byte) {
		_, _, raw, _ := cutDoc(t, f, projects...)
		return bytes.Contains(raw, []byte(`"id":"gd-gWork2"`)), raw
	}
	walk := func(task string) {
		t.Helper()
		src.mutate("gd-gWork2", func(b *beadsapi.BeadDetail) { b.Fields["task_id"] = task; b.UpdatedAt = b.UpdatedAt.Add(time.Minute) })
		if err := f.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if in, _ := has("gardenio"); !in {
		t.Fatal("g-work-2 on plan A is missing from the gardenio cut")
	}
	if in, _ := has("zeta"); in {
		t.Fatal("the zeta cut carries a gardenio gardener on a gardenio plot")
	}

	walk("gd-Z1b") // onto the zeta plan
	if g := findGardener(decodeSnap(t, f), "gd-gWork2"); g == nil || g.Plot == nil || g.Plot.Plan != "gd-planZ" {
		t.Fatalf("admin: g-work-2 = %+v, want it on plan Z", g)
	}
	if in, raw := has("gardenio"); in || bytes.Contains(raw, []byte("g-work-2")) {
		t.Error("a gardener that walked onto a zeta plot is still in the gardenio cut")
	}
	if in, _ := has("zeta"); in {
		t.Error("a gardenio gardener on a zeta plot is in the zeta cut")
	}

	walk("gd-A1b") // and home again
	if in, _ := has("gardenio"); !in {
		t.Error("a gardener that walked back onto plan A did not reappear in the gardenio cut")
	}

	// The zeta gardener: in the zeta cut on its own plot, never in the gardenio one, even on a gardenio plot.
	if _, raw := has("gardenio"); bytes.Contains(raw, []byte("gd-gZeta")) || bytes.Contains(raw, []byte("g-zeta")) {
		t.Error("the gardenio cut carries the zeta gardener")
	}
}

// A scoped viewer keeps a gardener's reason as its kind in fixed words: never a handle's link or title, a wrap-up's
// blockers or a wake condition, any of which can name a bead outside the set.
func TestFarmCutReason(t *testing.T) {
	for _, tc := range []struct {
		in   *FarmReason
		want *FarmReason
	}{
		{nil, nil},
		{&FarmReason{Kind: ReasonParked, Text: "Waiting on Merge PR #700 for gd-Z1a", Href: "/garden?bead=gd-eZ"}, &FarmReason{Kind: ReasonParked, Text: "Waiting on a person"}},
		{&FarmReason{Kind: ReasonDown, Text: "Pod failed: blocked on gd-Z1a in zeta"}, &FarmReason{Kind: ReasonDown, Text: "Pod failed"}},
		{&FarmReason{Kind: ReasonDown, Text: "Pod failed"}, &FarmReason{Kind: ReasonDown, Text: "Pod failed"}},
		{&FarmReason{Kind: ReasonSleeping, Text: "Sleeping until gd-Z1a merges"}, &FarmReason{Kind: ReasonSleeping, Text: "Sleeping"}},
		{&FarmReason{Kind: ReasonStale, Text: "No progress for 3h"}, &FarmReason{Kind: ReasonStale, Text: "No progress for 3h"}},
		{&FarmReason{Kind: ReasonStarting, Text: "Starting"}, &FarmReason{Kind: ReasonStarting, Text: "Starting"}},
		{&FarmReason{Kind: "someday", Text: "gd-Z1a"}, nil},
	} {
		got := cutReason(tc.in)
		if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
			t.Errorf("cutReason(%+v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}
