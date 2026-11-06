package main

// Lockstep guard for the C6 /costs trust strip (ka-0000007295).
//
// The strip's per-pair drift floors (costsTrustPairFloors) MUST match the C5
// CostXCheckDrift alert's per-pair thresholds
// (helm/gardenio/values.yaml vmalert.gapAlerts.xcheckDrift.pairThresholds) so
// the strip and the alert judge every pair the same way. Two hardcoded copies
// in two languages is exactly what broke this before: commit c30892e46 retuned
// the alert's pairThresholds (settled-vs-bill 0.95→0.5, bill-conservation
// 0.85, estimate-vs-settled 0) but never touched the Go constant, so the strip
// painted known-structural gaps red for four days. Prose ("change them in
// lockstep") did not enforce it — this test does. A future retune of
// values.yaml that forgets the Go map now fails CI here.

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// valuesYAMLPath is the chart values file relative to this package's dir.
const valuesYAMLPath = "../../../helm/gardenio/values.yaml"

// TestCostsTrustFloorsMatchValuesYAML fails when the Go per-pair floor map and
// values.yaml vmalert.gapAlerts.xcheckDrift.pairThresholds disagree — including
// the default threshold vs costsTrustDriftFloor.
func TestCostsTrustFloorsMatchValuesYAML(t *testing.T) {
	raw, err := os.ReadFile(valuesYAMLPath)
	if err != nil {
		t.Fatalf("read %s: %v", valuesYAMLPath, err)
	}

	var doc struct {
		VMAlert struct {
			GapAlerts struct {
				XCheckDrift struct {
					Threshold      float64            `yaml:"threshold"`
					PairThresholds map[string]float64 `yaml:"pairThresholds"`
				} `yaml:"xcheckDrift"`
			} `yaml:"gapAlerts"`
		} `yaml:"vmalert"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", valuesYAMLPath, err)
	}

	xd := doc.VMAlert.GapAlerts.XCheckDrift
	if len(xd.PairThresholds) == 0 {
		t.Fatalf("no pairThresholds parsed from %s — the YAML path "+
			"vmalert.gapAlerts.xcheckDrift.pairThresholds may have moved; "+
			"update this test AND costsTrustPairFloors together", valuesYAMLPath)
	}

	// Default threshold must equal the Go default constant.
	if xd.Threshold != costsTrustDriftFloor {
		t.Errorf("default floor drift: values.yaml threshold=%v, costsTrustDriftFloor=%v — change them in lockstep",
			xd.Threshold, costsTrustDriftFloor)
	}

	goFloors := costsTrustPairFloors()

	// Every values.yaml override must be mirrored in the Go map, same value.
	for pair, want := range xd.PairThresholds {
		got, ok := goFloors[pair]
		if !ok {
			t.Errorf("values.yaml pairThresholds has %q=%v but costsTrustPairFloors is missing it — add it to keep the strip and the alert in lockstep", pair, want)
			continue
		}
		if got != want {
			t.Errorf("floor drift for %q: values.yaml=%v, costsTrustPairFloors=%v", pair, want, got)
		}
	}

	// And no stale Go override that values.yaml no longer sets.
	for pair, got := range goFloors {
		if _, ok := xd.PairThresholds[pair]; !ok {
			t.Errorf("costsTrustPairFloors has %q=%v but values.yaml pairThresholds no longer sets it — remove the stale Go override", pair, got)
		}
	}
}
