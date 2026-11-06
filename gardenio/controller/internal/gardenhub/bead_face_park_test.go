package gardenhub

import (
	"context"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/plangardener"
)

// stubDormancy replaces the live probe for one test, counting its calls. The verdict itself is bridge's
// (gardener_dormancy_probe_readonly_test.go); these pin what the face does with it.
func stubDormancy(t *testing.T, verdict func(*beadsapi.BeadDetail) *bridge.ReconcileDormancy) *int {
	t.Helper()
	calls, prev := 0, gardenerDormancyProbe
	gardenerDormancyProbe = func(_ context.Context, _ bridge.DormancyReader, _ *slog.Logger, b *beadsapi.BeadDetail) *bridge.ReconcileDormancy {
		calls++
		return verdict(b)
	}
	t.Cleanup(func() { gardenerDormancyProbe = prev })
	return &calls
}

// staleLoop is the face fixture's gardener loop with the dormancy stamp a park leaves behind: dormant on gd-FaceEscal02,
// which has since closed (gd-yK7bWh9s3W).
func staleLoop(f *fakeBeads, enabled string) {
	b := f.beads["gd-FaceLoop001"]
	b.Fields["enabled"] = enabled
	b.Fields["gardener"] = `{"name":"gardenio-plan-gardener","dormant_on_escalation":true,"dormant_blocker":"gd-FaceEscal02"}`
}

// The face says the live park, titled and linked, never the stamp: a stamped loop the probe finds parked elsewhere names
// the live blocker, and one the probe finds awake (the wedge cleared, the flag not yet unset) is running.
func TestGardenerFaceLiveDormancy(t *testing.T) {
	f := faceFixture()
	staleLoop(f, "true")
	parked := true
	calls := stubDormancy(t, func(b *beadsapi.BeadDetail) *bridge.ReconcileDormancy {
		if b.ID != "gd-FaceLoop001" {
			t.Errorf("probed %s, want only the gardener loop", b.ID)
		}
		if !parked {
			return nil
		}
		return &bridge.ReconcileDormancy{Parked: true, Kind: "operator_wedge", LiveBlocker: "gd-FaceEscal01", StampedBlocker: "gd-FaceEscal02", StampedBlockerStale: true, Evaluated: true}
	})
	doc, err := readBead(context.Background(), f, "gd-FaceLoop001")
	if err != nil {
		t.Fatal(err)
	}
	g := doc.Face.Gardener
	if g.Dormant == nil || g.Dormant.ID != "gd-FaceEscal01" || g.Dormant.Title != "Merge PR #57" || g.DormantKind != "operator_wedge" {
		t.Errorf("parked face: dormant %+v kind %q, want the live gd-FaceEscal01, titled", g.Dormant, g.DormantKind)
	}
	parked = false
	if doc, _ = readBead(context.Background(), f, "gd-FaceLoop001"); doc.Face.Gardener.Dormant != nil || doc.Face.Gardener.DormantKind != "" {
		t.Errorf("awake loop with a stale stamp: dormant %+v, want none", doc.Face.Gardener.Dormant)
	}
	if _, err := readBead(context.Background(), f, "gd-FaceWork01"); err != nil || *calls != 2 {
		t.Errorf("probe calls = %d (%v), want 2: a task is never probed", *calls, err)
	}
}

// Only a running loop on a reader that can list is probed; a proven park without a blocker, or a probe that panics, shows
// no park and still serves the panel.
func TestGardenerFaceParkGuards(t *testing.T) {
	for _, c := range []struct {
		name, enabled, status string
		reader                func(*fakeBeads) BeadReader
		verdict               *bridge.ReconcileDormancy
		panics                bool
		wantProbe             bool
	}{
		{"paused", "false", "open", nil, &bridge.ReconcileDormancy{Parked: true, LiveBlocker: "gd-FaceEscal01"}, false, false},
		{"retired", "true", "closed", nil, &bridge.ReconcileDormancy{Parked: true, LiveBlocker: "gd-FaceEscal01"}, false, false},
		{"reader cannot list", "true", "open", func(f *fakeBeads) BeadReader { return readOnlyBeads{f} }, &bridge.ReconcileDormancy{Parked: true, LiveBlocker: "gd-FaceEscal01"}, false, false},
		{"parked without a blocker", "true", "open", nil, &bridge.ReconcileDormancy{Parked: true, Kind: "operator_wedge"}, false, true},
		{"probe panics", "true", "open", nil, nil, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := faceFixture()
			staleLoop(f, c.enabled)
			f.beads["gd-FaceLoop001"].Status = c.status
			calls := stubDormancy(t, func(*beadsapi.BeadDetail) *bridge.ReconcileDormancy {
				if c.panics {
					panic("probe")
				}
				return c.verdict
			})
			var src BeadReader = f
			if c.reader != nil {
				src = c.reader(f)
			}
			doc, err := readBead(context.Background(), src, "gd-FaceLoop001")
			if err != nil {
				t.Fatal(err)
			}
			if g := doc.Face.Gardener; g.Dormant != nil || g.DormantKind != "" {
				t.Errorf("dormant %+v kind %q, want none", g.Dormant, g.DormantKind)
			}
			if got := *calls > 0; got != c.wantProbe {
				t.Errorf("probed = %v, want %v", got, c.wantProbe)
			}
		})
	}
}

// readOnlyBeads is the bead view's reader without ListBeadsFiltered.
type readOnlyBeads struct{ f *fakeBeads }

func (r readOnlyBeads) GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	return r.f.GetBead(ctx, id)
}
func (r readOnlyBeads) GetComments(ctx context.Context, id string) ([]beadsapi.Comment, error) {
	return r.f.GetComments(ctx, id)
}
func (r readOnlyBeads) GetDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error) {
	return r.f.GetDependencies(ctx, id)
}
func (r readOnlyBeads) GetReverseDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error) {
	return r.f.GetReverseDependencies(ctx, id)
}

// bindLoop pins the face fixture's gardener loop to plan gd-FacePlan01 as gb plan gardener does, beside a retired
// gardener of the same plan and a running loop bound to another plan, neither of which is the plan's.
func bindLoop(f *fakeBeads, enabled string) {
	loop := f.beads["gd-FaceLoop001"]
	loop.Fields["enabled"] = enabled
	loop.Labels = append(loop.Labels, plangardener.GardenerEpicLabel("gd-FacePlan01"), plangardener.MarkerLabel)
	f.beads["gd-FaceLoop002"] = &beadsapi.BeadDetail{ID: "gd-FaceLoop002", Type: "trigger", Status: "closed", Title: "retired plan gardener",
		Labels: []string{"role:gardener", plangardener.GardenerEpicLabel("gd-FacePlan01"), plangardener.MarkerLabel},
		Fields: map[string]string{"gardener": `{"name":"old"}`}, UpdatedAt: loop.UpdatedAt.Add(time.Hour)}
	f.beads["gd-FaceLoop003"] = &beadsapi.BeadDetail{ID: "gd-FaceLoop003", Type: "trigger", Status: "open", Title: "another plan's gardener",
		Labels: []string{"role:gardener", plangardener.GardenerEpicLabel("gd-FaceEpic01")}, Fields: map[string]string{"gardener": `{"name":"other"}`},
		UpdatedAt: loop.UpdatedAt.Add(2 * time.Hour)}
}

// The plan face names its bound gardener loop, found by the one gardener-epic list, and shows that loop's live park,
// titled: the probe runs on the gardener, never on the plan (gd-7Ucln47lFS).
func TestPlanFaceGardenerPark(t *testing.T) {
	f := faceFixture()
	bindLoop(f, "true")
	calls := stubDormancy(t, func(b *beadsapi.BeadDetail) *bridge.ReconcileDormancy {
		if b.ID != "gd-FaceLoop001" {
			t.Errorf("probed %s, want the plan's running gardener", b.ID)
		}
		return &bridge.ReconcileDormancy{Parked: true, Kind: "operator_wedge", LiveBlocker: "gd-FaceEscal01", Evaluated: true}
	})
	doc, err := readBead(context.Background(), f, "gd-FacePlan01")
	if err != nil {
		t.Fatal(err)
	}
	p := doc.Face.Plan
	if p.Gardener == nil || p.Gardener.ID != "gd-FaceLoop001" || p.Gardener.Title != "plan gardener for Lands" || p.GardenerState != "running" {
		t.Errorf("plan gardener %+v state %q, want the open, marked gd-FaceLoop001 running", p.Gardener, p.GardenerState)
	}
	if p.Dormant == nil || p.Dormant.ID != "gd-FaceEscal01" || p.Dormant.Title != "Merge PR #57" || p.DormantKind != "operator_wedge" {
		t.Errorf("plan park: dormant %+v kind %q, want the live gd-FaceEscal01, titled", p.Dormant, p.DormantKind)
	}
	if q := f.listQ; !reflect.DeepEqual(q.Labels, []string{"gardener-epic:gd-FacePlan01"}) || !reflect.DeepEqual(q.Types, []string{"trigger", "schedule"}) || q.Limit != planGardenerMax {
		t.Errorf("gardener list %+v, want one label-filtered, capped list", q)
	}
	if *calls != 1 {
		t.Errorf("probe calls = %d, want 1", *calls)
	}
}

// A paused gardener is named but not probed; a plan with none bound, a failed list, or a reader that cannot list names
// none; and an epic that is no plan never lists.
func TestPlanFaceGardenerGuards(t *testing.T) {
	for _, c := range []struct {
		name, bind, id string
		fail           bool
		cannotList     bool
		wantGardener   string
		wantState      string
	}{
		{name: "paused", bind: "false", id: "gd-FacePlan01", wantGardener: "gd-FaceLoop001", wantState: "paused"},
		{name: "none bound", id: "gd-FacePlan01"},
		{name: "list fails", bind: "true", id: "gd-FacePlan01", fail: true},
		{name: "reader cannot list", bind: "true", id: "gd-FacePlan01", cannotList: true},
		{name: "an epic, no plan", bind: "true", id: "gd-FaceEpic01"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := faceFixture()
			if c.bind != "" {
				bindLoop(f, c.bind)
			}
			f.failList = c.fail
			calls := stubDormancy(t, func(*beadsapi.BeadDetail) *bridge.ReconcileDormancy {
				return &bridge.ReconcileDormancy{Parked: true, LiveBlocker: "gd-FaceEscal01"}
			})
			var src BeadReader = f
			if c.cannotList {
				src = readOnlyBeads{f}
			}
			doc, err := readBead(context.Background(), src, c.id)
			if err != nil {
				t.Fatal(err)
			}
			if *calls != 0 {
				t.Errorf("probe calls = %d, want none", *calls)
			}
			if c.id == "gd-FaceEpic01" {
				if doc.Face.Plan != nil || f.listQ.Labels != nil {
					t.Errorf("epic face %+v, list %+v: want no plan face and no list", doc.Face, f.listQ)
				}
				return
			}
			p, got := doc.Face.Plan, ""
			if p.Gardener != nil {
				got = p.Gardener.ID
			}
			if got != c.wantGardener || p.GardenerState != c.wantState || p.Dormant != nil || p.DormantKind != "" {
				t.Errorf("gardener %q state %q dormant %+v, want %q %q and no park", got, p.GardenerState, p.Dormant, c.wantGardener, c.wantState)
			}
		})
	}
}
