package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// plans_gardener_park_test.go — the live "Gardener parked" line on the /plans/{id}
// plan-agent card (gd-f2fc4jcRn3). The dormancy verdict itself is
// bridge.ReconcileDormancyProbe's, tested in internal/bridge; these tests stub it
// and pin what the cockpit does with it.

// parkMux serves the cockpit with the dormancy probe replaced by probe. calls
// counts how often the cockpit asked.
func parkMux(t *testing.T, f *fakeDaemon, probe func(*beadsapi.BeadDetail) *bridge.ReconcileDormancy) (http.Handler, *int) {
	t.Helper()
	calls := 0
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetNow(func() time.Time { return planGardenerTestNow })
	srv.gardenerDormancyProbe = func(_ context.Context, g *beadsapi.BeadDetail) *bridge.ReconcileDormancy {
		calls++
		return probe(g)
	}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return edgeAuth(mux), &calls
}

func getCockpit(t *testing.T, mux http.Handler) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /plans/gd-plan1: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestPlanCockpit_ParkedGardenerShowsLiveBlocker: a running gardener the probe
// proves parked renders the parked line naming the LIVE blocker (type, id link,
// title), and calls out a stamped dormant_blocker that names a different bead.
func TestPlanCockpit_ParkedGardenerShowsLiveBlocker(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	f.beads["gd-esc1"] = &beadsapi.BeadDetail{ID: "gd-esc1", Type: "escalation", Status: "open", Title: "Mint the alerts webhook"}
	mux, calls := parkMux(t, f, func(g *beadsapi.BeadDetail) *bridge.ReconcileDormancy {
		if g == nil || g.ID != "gd-cap1" {
			t.Errorf("probe got gardener %v, want gd-cap1", g)
		}
		return &bridge.ReconcileDormancy{Parked: true, Kind: "operator_wedge", LiveBlocker: "gd-esc1",
			StampedBlocker: "gd-old1", StampedBlockerStale: true, Evaluated: true}
	})

	body := getCockpit(t, mux)
	if *calls == 0 {
		t.Fatal("the cockpit never probed the running gardener's dormancy")
	}
	for _, want := range []string{
		`class="meta plan-gardener-parked"`,
		`data-park-kind="operator_wedge"`,
		"Gardener parked.",
		"Every open step waits on",
		`escalation <a href="/gd-esc1">gd-esc1</a>`,
		"Mint the alerts webhook",
		"Its stamped blocker gd-old1 is out of date.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("parked cockpit missing %q", want)
		}
	}
}

// TestPlanCockpit_MRMergedPark words the declared block-until-MR-merged park.
func TestPlanCockpit_MRMergedPark(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	mux, _ := parkMux(t, f, func(*beadsapi.BeadDetail) *bridge.ReconcileDormancy {
		return &bridge.ReconcileDormancy{Parked: true, Kind: "mr_merged", LiveBlocker: "gd-task9", Evaluated: true}
	})

	body := getCockpit(t, mux)
	for _, want := range []string{
		`data-park-kind="mr_merged"`,
		"It fires nothing until the merge request tracked by",
		`<a href="/gd-task9">gd-task9</a> merges.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mr_merged cockpit missing %q", want)
		}
	}
	if strings.Contains(body, "is out of date") {
		t.Error("no stale-stamp note expected without a stale stamp")
	}
}

// TestPlanCockpit_NoParkLine covers every case where nothing may render: the
// probe says not parked (even with a stamped dormant blocker — the stamp is never
// trusted), the probe could not run, and a paused gardener (not probed at all:
// "paused" already says why it fires nothing).
func TestPlanCockpit_NoParkLine(t *testing.T) {
	cases := []struct {
		name      string
		enabled   string
		verdict   *bridge.ReconcileDormancy
		wantProbe bool
	}{
		{"live frontier, stale stamp", "true", &bridge.ReconcileDormancy{Parked: false, DormantDeclared: true, StampedBlocker: "gd-old1", ActionableChildren: 3, Evaluated: true}, true},
		{"probe returns nil", "true", nil, true},
		{"parked without a blocker id", "true", &bridge.ReconcileDormancy{Parked: true, Kind: "operator_wedge", Evaluated: true}, true},
		{"paused gardener", "false", &bridge.ReconcileDormancy{Parked: true, Kind: "operator_wedge", LiveBlocker: "gd-esc1", Evaluated: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			seedPlanEpic(f, "gd-plan1")
			seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", tc.enabled, "open", "")
			mux, calls := parkMux(t, f, func(*beadsapi.BeadDetail) *bridge.ReconcileDormancy { return tc.verdict })

			body := getCockpit(t, mux)
			if strings.Contains(body, "plan-gardener-parked\"") || strings.Contains(body, "Gardener parked.") {
				t.Error("parked line rendered, want none")
			}
			if got := *calls > 0; got != tc.wantProbe {
				t.Errorf("probed = %v, want %v", got, tc.wantProbe)
			}
		})
	}
}
