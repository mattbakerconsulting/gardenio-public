package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// X-check trust dashboard (C5 ka-0000006644, plan ka-0000003747): the Grafana
// face of the C2 cross-check sweep. The contract enforced here:
//   - the board is registered in the controller dashboard ConfigMap and embeds
//     as PARSEABLE JSON under its stable uid (the CostXCheckDrift `dashboard`
//     annotation deep-links that uid — a rename breaks the Slack 📊 Graph link),
//   - the agreement-over-time panel (the "graph correctness improvement"
//     trend) charts gardenio_xcheck_agreement per pair,
//   - the side A/B overlay panel repeats per pair over gardenio_xcheck_value,
//     so a low score is attributable to the derivation that moved,
//   - the alert-view panel mirrors the `for: 6h` semantics via max_over_time.
//     (The P7 estimate-vs-settled ratio panel was removed with the pair —
//     ka-0000007296; its side A had no producer since coop went cost-free.)

type xcheckTrustDashboard struct {
	UID    string `json:"uid"`
	Panels []struct {
		Title   string `json:"title"`
		Type    string `json:"type"`
		Repeat  string `json:"repeat"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
	Templating struct {
		List []struct {
			Name  string `json:"name"`
			Query string `json:"query"`
		} `json:"list"`
	} `json:"templating"`
}

func parseXCheckTrustDashboard(t *testing.T) xcheckTrustDashboard {
	t.Helper()
	data := dashboardConfigMapData(t)
	const key = "gardenio-xcheck-trust.json"
	raw, ok := data[key]
	if !ok {
		t.Fatalf("dashboard ConfigMap is missing key %q; keys present: %v", key, keysOf(data))
	}
	var d xcheckTrustDashboard
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("embedded %s is not valid JSON (nindent likely broke it): %v", key, err)
	}
	if d.UID != "gardenio-xcheck-trust" {
		t.Errorf("xcheck-trust board must carry its stable uid (the CostXCheckDrift dashboard annotation links it); got %q", d.UID)
	}
	return d
}

func TestXCheckTrustDashboard_AgreementTrendPanel(t *testing.T) {
	t.Parallel()
	d := parseXCheckTrustDashboard(t)
	var found bool
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			if p.Type == "timeseries" &&
				strings.Contains(tg.Expr, `gardenio_xcheck_agreement{env=~"$env",pair=~"$pair"}`) &&
				!strings.Contains(tg.Expr, "max_over_time") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("board must have a timeseries charting raw gardenio_xcheck_agreement per pair (the correctness trend)")
	}
}

func TestXCheckTrustDashboard_SideABOverlayRepeatsPerPair(t *testing.T) {
	t.Parallel()
	d := parseXCheckTrustDashboard(t)
	var found bool
	for _, p := range d.Panels {
		if p.Repeat != "pair" {
			continue
		}
		for _, tg := range p.Targets {
			if strings.Contains(tg.Expr, `gardenio_xcheck_value{env=~"$env",pair="$pair"}`) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("board must have a per-pair repeated panel overlaying both gardenio_xcheck_value sides")
	}
	// The repeat variable must exist and be driven by the live series.
	var pairVar bool
	for _, v := range d.Templating.List {
		if v.Name == "pair" && strings.Contains(v.Query, "gardenio_xcheck_agreement") {
			pairVar = true
		}
	}
	if !pairVar {
		t.Errorf("board must define a $pair variable from label_values(gardenio_xcheck_agreement, pair)")
	}
}

func TestXCheckTrustDashboard_AlertView(t *testing.T) {
	t.Parallel()
	d := parseXCheckTrustDashboard(t)
	var alertView bool
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			// Alert view: max_over_time over the alert's 6h sustain window —
			// below the floor here means CostXCheckDrift is firing.
			if strings.Contains(tg.Expr, "max_over_time(gardenio_xcheck_agreement") &&
				strings.Contains(tg.Expr, "[6h]") {
				alertView = true
			}
		}
	}
	if !alertView {
		t.Errorf("board must have the trailing-6h max_over_time alert-view panel mirroring CostXCheckDrift's `for` semantics")
	}
	// The P7 estimate-vs-settled ratio panel was removed with the pair
	// (ka-0000007296) — the board must no longer reference it.
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			if strings.Contains(tg.Expr, `pair="estimate-vs-settled"`) {
				t.Errorf("board must not reference the retired estimate-vs-settled pair, found: %s", tg.Expr)
			}
		}
	}
}
