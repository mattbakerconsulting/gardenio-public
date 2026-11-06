package tests

import (
	"encoding/json"
	"testing"
)

// Perf-traces dashboard Duration-unit + default-service guards (bug gd-QQD1Ju6V3J).
//
// Since ka-0000002799 the chart no longer renders a dashboard ConfigMap; the
// dashboards/perf-traces.json source file is baked into the grafana-bridge image
// and provisioned at runtime. This test reads that JSON straight from disk (the
// same bytes the bridge ships) and guards the two regressions the 2026-07-27
// Grafana GRADE caught:
//
//   - The "Recent traces" Duration column must render Jaeger's raw span
//     duration (MICROSECONDS) with a microsecond unit. When it was set to "ms"
//     Grafana treated the µs value as ms and inflated every duration exactly
//     1000x (264ms shown as 4.40 mins). The reference board
//     monorepo-distributed-traces leaves the unit unset (Jaeger datasource
//     default = µs); we pin it explicitly here so the intent is testable.
//   - Absolute thresholds are compared against the RAW field value (µs), so the
//     green/yellow/orange/red boundaries must be expressed in µs, not ms —
//     otherwise a sub-ms span trips the "red" (2.5s) threshold.
//   - The Service variable must default to a service that emits SERVER-kind
//     spans. The perf-harness span topology shifted after 2026-07-27: keycloak
//     now emits 0 span series (VM: span_duration_milliseconds_bucket has none
//     for service_name="keycloak"), while site-api / sponsor-api carry the live
//     SERVER-kind spanmetrics (site-api alone had 816 bucket series, ~25 reqps
//     peak over 6h — grounded 2026-08-19, ka-0000004122). A keycloak default
//     therefore opens the board all-"No data" over a 12k-series live surface, so
//     the guard now forbids keycloak and pins a live SERVER-span service.

// perfTracesDashboard renders the vtsingle dashboard ConfigMap and returns the
// embedded perf-traces board, parsed against a shape rich enough to assert the
// Duration field override and the Service template default.
type perfTracesDashboard struct {
	UID    string `json:"uid"`
	Panels []struct {
		ID          int    `json:"id"`
		Type        string `json:"type"`
		FieldConfig struct {
			Overrides []struct {
				Matcher struct {
					ID      string `json:"id"`
					Options string `json:"options"`
				} `json:"matcher"`
				Properties []struct {
					ID    string          `json:"id"`
					Value json.RawMessage `json:"value"`
				} `json:"properties"`
			} `json:"overrides"`
		} `json:"fieldConfig"`
	} `json:"panels"`
	Templating struct {
		List []struct {
			Name    string `json:"name"`
			Current struct {
				Text  string `json:"text"`
				Value string `json:"value"`
			} `json:"current"`
		} `json:"list"`
	} `json:"templating"`
}

func loadPerfTracesDashboard(t *testing.T) perfTracesDashboard {
	t.Helper()
	raw := readDashboardJSON(t, "perf-traces.json")
	var d perfTracesDashboard
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("dashboards/perf-traces.json is not valid JSON: %v", err)
	}
	if d.UID != "gardenio-perf-traces" {
		t.Fatalf("perf-traces board must carry its stable uid; got %q", d.UID)
	}
	return d
}

// durationOverride returns the property id->value map for the "Duration" field
// override on the Recent-traces table (panel id 2).
func durationOverride(t *testing.T, d perfTracesDashboard) map[string]json.RawMessage {
	t.Helper()
	for _, p := range d.Panels {
		if p.ID != 2 {
			continue
		}
		for _, ov := range p.FieldConfig.Overrides {
			if ov.Matcher.ID == "byName" && ov.Matcher.Options == "Duration" {
				props := map[string]json.RawMessage{}
				for _, pr := range ov.Properties {
					props[pr.ID] = pr.Value
				}
				return props
			}
		}
	}
	t.Fatal("perf-traces Recent-traces table (panel id 2) must have a byName=Duration field override")
	return nil
}

func TestPerfTracesDashboard_DurationUnitIsMicroseconds(t *testing.T) {
	t.Parallel()
	props := durationOverride(t, loadPerfTracesDashboard(t))
	unit, ok := props["unit"]
	if !ok {
		t.Fatal("Duration override must set an explicit unit")
	}
	var got string
	if err := json.Unmarshal(unit, &got); err != nil {
		t.Fatalf("Duration unit is not a string: %v", err)
	}
	// Jaeger returns span duration in microseconds; the unit must match or every
	// duration reads 1000x too large (gd-QQD1Ju6V3J).
	if got != "µs" {
		t.Errorf("Duration unit must be microseconds (\"µs\") to match Jaeger's raw µs values; got %q — a \"ms\" unit inflates every duration 1000x", got)
	}
}

func TestPerfTracesDashboard_DurationThresholdsInMicroseconds(t *testing.T) {
	t.Parallel()
	props := durationOverride(t, loadPerfTracesDashboard(t))
	raw, ok := props["thresholds"]
	if !ok {
		t.Fatal("Duration override must define color thresholds")
	}
	var thr struct {
		Steps []struct {
			Color string   `json:"color"`
			Value *float64 `json:"value"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &thr); err != nil {
		t.Fatalf("Duration thresholds not parseable: %v", err)
	}
	// Grafana compares absolute thresholds against the RAW field value (µs), so
	// the intended 250ms / 1s / 2.5s boundaries must be expressed in µs. If they
	// were left in ms, a 300µs (0.3ms) span would trip "orange".
	want := map[string]float64{
		"yellow": 250000,
		"orange": 1000000,
		"red":    2500000,
	}
	for _, s := range thr.Steps {
		exp, tracked := want[s.Color]
		if !tracked {
			continue
		}
		if s.Value == nil {
			t.Errorf("Duration threshold %q must have a numeric µs value", s.Color)
			continue
		}
		if *s.Value != exp {
			t.Errorf("Duration threshold %q must be %.0f µs (its ms intent × 1000); got %.0f", s.Color, exp, *s.Value)
		}
	}
}

func TestPerfTracesDashboard_DefaultServiceHasServerSpans(t *testing.T) {
	t.Parallel()
	d := loadPerfTracesDashboard(t)
	var svcDefault string
	for _, v := range d.Templating.List {
		if v.Name == "service" {
			svcDefault = v.Current.Value
		}
	}
	if svcDefault == "" {
		t.Fatal("perf-traces board must have a 'service' template variable with a default")
	}
	// keycloak emits 0 span series in gardenio-vmsingle (grounded 2026-08-19,
	// ka-0000004122), so a keycloak default opens the board all-"No data" over a
	// 12k-series live surface. The default must be a service the perf load
	// harness confirms emits SERVER-kind spanmetrics — site-api and sponsor-api
	// are the live SERVER-span services; $__all is also acceptable.
	if svcDefault == "keycloak" {
		t.Error("perf-traces Service default must not be keycloak (0 span series in VM, opens all-\"No data\"); use a live SERVER-span service e.g. site-api / sponsor-api, or $__all")
	}
	liveServers := map[string]bool{"site-api": true, "sponsor-api": true, "$__all": true}
	if !liveServers[svcDefault] {
		t.Errorf("perf-traces Service default %q is not a confirmed live SERVER-span service; expected one of site-api / sponsor-api / $__all (grounded against gardenio-vmsingle 2026-08-19)", svcDefault)
	}
}
