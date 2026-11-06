package grafanagrade

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The fleet scan: run the empty-branch liveness detector over every
// chart-shipped dashboard and fail on any DEFECT that is not in the recorded
// baseline.
//
// Why a baseline at all: the detector was written after the fleet, and 26 boards
// carried pre-existing empty-branch defects that each needed their own measured
// fix (a liveness control has to be chosen per panel, from the same exporter,
// and verified against live VictoriaMetrics — exactly what ka-0000002930 took
// three passes to get right on ONE panel). The baseline made that inventory
// explicit while it drained. It is now EMPTY (ka-0000006178), so the ratchet is
// a clean gate: any defect at all reds CI. Keep the file and keep it empty.
//
// A baseline entry that no longer fires is REPORTED, not failed: dashboards are
// fixed concurrently by other agents and by in-flight MRs (!8081 fixes
// cost-overview "Deferred Agents"), and a stale-waiver hard-fail would red CI
// on somebody else's fix. Prune them when you next touch this file.
//
// Where it runs: the chart JSON lives in a sibling tree reached by relative
// path, so the scan SKIPS cleanly when it is absent. CI checks out the whole
// repo, so gardenio/helm/gardenio/dashboards/*.json is present and the
// skip must never trigger in CI — TestEmptyBranchScan_DashboardTreePresent is
// the anti-vacuity guard that says so out loud.

const chartDashboardsDir = "../../../helm/gardenio/dashboards"

const baselinePath = "testdata/empty_branch_baseline.json"

// baselineEntry is one recorded, pre-existing defect.
type baselineEntry struct {
	Dashboard string `json:"dashboard"`
	Panel     string `json:"panel"`
	Class     Class  `json:"class"`
}

func (b baselineEntry) key() string {
	return b.Dashboard + "\x00" + b.Panel + "\x00" + string(b.Class)
}

func loadDashboards(t *testing.T) map[string]*Dashboard {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(chartDashboardsDir, "*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", chartDashboardsDir, err)
	}
	if len(paths) == 0 {
		t.Skipf("chart dashboard tree absent (%s) — sibling tree not staged", chartDashboardsDir)
	}
	out := map[string]*Dashboard{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		d, err := ParseDashboard(raw)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out[filepath.Base(p)] = d
	}
	return out
}

func scanFleet(t *testing.T) []Finding {
	t.Helper()
	boards := loadDashboards(t)
	names := make([]string, 0, len(boards))
	for n := range boards {
		names = append(names, n)
	}
	sort.Strings(names)
	var all []Finding
	for _, n := range names {
		all = append(all, CheckDashboard(n, boards[n])...)
	}
	return all
}

func loadBaseline(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read %s: %v", baselinePath, err)
	}
	var entries []baselineEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("parse %s: %v", baselinePath, err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		out[e.key()] = true
	}
	return out
}

// TestEmptyBranchScan_NoNewDefects is the ratchet.
//
// Regenerate the baseline after a deliberate, reviewed change with:
//
//	GRAFANAGRADE_UPDATE_BASELINE=1 go test ./internal/grafanagrade/ -run TestEmptyBranchScan_NoNewDefects
func TestEmptyBranchScan_NoNewDefects(t *testing.T) {
	findings := scanFleet(t)
	defects := Defects(findings)

	if os.Getenv("GRAFANAGRADE_UPDATE_BASELINE") == "1" {
		entries := make([]baselineEntry, 0, len(defects))
		for _, f := range defects {
			entries = append(entries, baselineEntry{Dashboard: f.Dashboard, Panel: f.Panel, Class: f.Class})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].key() < entries[j].key() })
		raw, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			t.Fatalf("marshal baseline: %v", err)
		}
		if err := os.WriteFile(baselinePath, append(raw, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", baselinePath, err)
		}
		t.Logf("rewrote %s with %d entries", baselinePath, len(entries))
		return
	}

	baseline := loadBaseline(t)
	seen := map[string]bool{}
	for _, f := range defects {
		seen[f.Key()] = true
		if baseline[f.Key()] {
			continue
		}
		t.Errorf("new empty-branch defect (not in %s):\n  %s\n  expr:    %s\n  noValue: %s",
			baselinePath, f, f.Expr, f.NoValue)
	}
	for key := range baseline {
		if !seen[key] {
			parts := strings.Split(key, "\x00")
			t.Logf("baseline entry no longer fires — prune it: %s / %q [%s]", parts[0], parts[1], parts[2])
		}
	}

	advisories := 0
	for _, f := range findings {
		if f.Severity == SeverityAdvisory {
			advisories++
		}
	}
	t.Logf("fleet scan: %d defects (all baselined), %d blind-empty advisories across %d findings",
		len(defects), advisories, len(findings))
}

// TestEmptyBranchScan_DashboardTreePresent is the anti-vacuity guard for the
// scan's skip branch: CI checks out the dashboards, so a skip there means the
// checkout drifted and the ratchet went
// silently green (the failure mode of ka-0000001628). Locally, where a partial
// checkout is legitimate, it only reports.
func TestEmptyBranchScan_DashboardTreePresent(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(chartDashboardsDir, "*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) > 0 {
		return
	}
	msg := chartDashboardsDir + "/*.json is absent, so TestEmptyBranchScan_NoNewDefects skipped " +
		"and the fleet ratchet is VACUOUS. Make sure the CI checkout includes " +
		"gardenio/helm/gardenio/dashboards/."
	if os.Getenv("CI") != "" {
		t.Fatal(msg)
	}
	t.Skip(msg)
}

// TestEmptyBranchScan_BaselineIsWellFormed keeps the baseline honest: every
// entry must name a class the detector can actually emit, and no duplicates.
func TestEmptyBranchScan_BaselineIsWellFormed(t *testing.T) {
	raw, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read %s: %v", baselinePath, err)
	}
	var entries []baselineEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("parse %s: %v", baselinePath, err)
	}
	valid := map[Class]bool{
		ClassUngatedZero:        true,
		ClassStaleWindow:        true,
		ClassUnsupportedNoValue: true,
		ClassNoValueClaimsZero:  true,
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !valid[e.Class] {
			t.Errorf("baseline entry %s / %q has class %q, which is not a defect class", e.Dashboard, e.Panel, e.Class)
		}
		if seen[e.key()] {
			t.Errorf("duplicate baseline entry: %s / %q [%s]", e.Dashboard, e.Panel, e.Class)
		}
		seen[e.key()] = true
	}
	if len(entries) == 0 {
		t.Log("baseline is empty — the fleet is clean; keep the file so the ratchet stays wired")
	}
}
