package grafanagrade

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// The fleet unit ratchet: run the missing-unit detector over every
// chart-shipped dashboard and fail on any unit-less viz panel that is not in
// the recorded baseline.
//
// Why a baseline at all, when the residue it opened with was zero: the point of
// the file is the SHAPE of the drain, not its current contents. The 91/338
// figure this bug started from (ka-0000002317) was a detector artifact — see
// unit.go for the four legitimate places a unit lives that the audit scorer did
// not read — and the honest residue behind it was 16 panels, all fixed in the
// same change. Keep the file and keep it empty: a future board that ships a
// raw number with no scale reds CI here, and an agent who genuinely needs to
// defer one has somewhere to record it rather than weakening the detector.
//
// A baseline entry that no longer fires is REPORTED, not failed, for the same
// reason as the empty-branch scan: boards are fixed concurrently and a
// stale-waiver hard-fail would red CI on somebody else's fix.
//
// Regenerate after a deliberate, reviewed change with:
//
//	GRAFANAGRADE_UPDATE_UNIT_BASELINE=1 go test ./internal/grafanagrade/ -run TestUnitScan_NoNewMissingUnits
const unitBaselinePath = "testdata/missing_unit_baseline.json"

// unitBaselineEntry is one recorded, deferred unit-less panel.
type unitBaselineEntry struct {
	Dashboard string `json:"dashboard"`
	Panel     string `json:"panel"`
}

func (b unitBaselineEntry) key() string { return b.Dashboard + "\x00" + b.Panel }

func scanFleetUnits(t *testing.T) []UnitFinding {
	t.Helper()
	boards := loadDashboards(t)
	names := make([]string, 0, len(boards))
	for n := range boards {
		names = append(names, n)
	}
	sort.Strings(names)
	var all []UnitFinding
	for _, n := range names {
		all = append(all, CheckUnits(n, boards[n])...)
	}
	return all
}

func TestUnitScan_NoNewMissingUnits(t *testing.T) {
	findings := scanFleetUnits(t)

	if os.Getenv("GRAFANAGRADE_UPDATE_UNIT_BASELINE") == "1" {
		entries := make([]unitBaselineEntry, 0, len(findings))
		for _, f := range findings {
			entries = append(entries, unitBaselineEntry{Dashboard: f.Dashboard, Panel: f.Panel})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].key() < entries[j].key() })
		raw, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			t.Fatalf("marshal baseline: %v", err)
		}
		if err := os.WriteFile(unitBaselinePath, append(raw, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", unitBaselinePath, err)
		}
		t.Logf("rewrote %s with %d entries", unitBaselinePath, len(entries))
		return
	}

	raw, err := os.ReadFile(unitBaselinePath)
	if err != nil {
		t.Fatalf("read %s: %v", unitBaselinePath, err)
	}
	var entries []unitBaselineEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("parse %s: %v", unitBaselinePath, err)
	}
	baseline := map[string]bool{}
	for _, e := range entries {
		baseline[e.key()] = true
	}

	seen := map[string]bool{}
	for _, f := range findings {
		seen[f.Key()] = true
		if baseline[f.Key()] {
			continue
		}
		t.Errorf("unit-less viz panel (not in %s):\n  %s", unitBaselinePath, f)
	}
	for key := range baseline {
		if !seen[key] {
			parts := strings.Split(key, "\x00")
			t.Logf("baseline entry no longer fires — prune it: %s / %q", parts[0], parts[1])
		}
	}
	t.Logf("fleet unit scan: %d unit-less viz panel(s), %d baselined", len(findings), len(entries))
}

// TestUnitScan_FleetTable is reporting, not a gate: it prints where every viz
// panel gets its unit from, so the exemptions stay visible instead of being an
// invisible reason a count went down.
//
//	go test -v ./internal/grafanagrade/ -run TestUnitScan_FleetTable
func TestUnitScan_FleetTable(t *testing.T) {
	boards := loadDashboards(t)
	names := make([]string, 0, len(boards))
	for n := range boards {
		names = append(names, n)
	}
	sort.Strings(names)

	bySource := map[string]int{}
	viz := 0
	for _, n := range names {
		var walk func([]Panel)
		walk = func(ps []Panel) {
			for i := range ps {
				p := &ps[i]
				if strings.EqualFold(p.Type, "row") {
					walk(p.Panels)
					continue
				}
				if isViz(p) && !unitlessPanelKinds[strings.ToLower(p.Type)] {
					viz++
					src := UnitedBy(p)
					if src == "" {
						src = "NONE (flagged)"
					}
					bySource[src]++
				}
				walk(p.Panels)
			}
		}
		walk(boards[n].Panels)
	}
	sources := make([]string, 0, len(bySource))
	for s := range bySource {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	t.Logf("%d unit-bearing viz panels across %d boards", viz, len(names))
	for _, s := range sources {
		t.Logf("  %4d  %s", bySource[s], s)
	}
}
