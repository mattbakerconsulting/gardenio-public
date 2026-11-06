package main

import (
	"os"
	"path/filepath"
	"testing"

	"gardenio/controller/internal/advice"
)

// ocFinding is a compact directive-density finding constructor for the CLI seam
// tests.
func ocFinding(id string, count int) advice.OverConstraintFinding {
	return advice.OverConstraintFinding{
		ID: id, Check: advice.OverConstraintDirectiveDensity, DirectiveCount: count,
	}
}

// TestApplyBaselineFiles_FirstRunEmitsBaseline verifies that a first run with no
// prior file (baseline path absent on disk) reads every finding as NEW and, with
// --emit-baseline, writes a floor at the current counts.
func TestApplyBaselineFiles_FirstRunEmitsBaseline(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "baseline.json")
	emitPath := filepath.Join(dir, "next.json")

	findings := []advice.OverConstraintFinding{ocFinding("gd-a", 17), ocFinding("gd-b", 9)}

	got, err := applyBaselineFiles(findings, basePath, emitPath)
	if err != nil {
		t.Fatalf("applyBaselineFiles: %v", err)
	}
	// A missing baseline file must not error and must classify everything NEW.
	for _, f := range got {
		if f.BaselineDelta != advice.BaselineNew {
			t.Errorf("%s: delta=%q, want new (no prior baseline)", f.ID, f.BaselineDelta)
		}
	}
	// The emitted baseline records the current counts as the floor.
	data, err := os.ReadFile(emitPath)
	if err != nil {
		t.Fatalf("read emitted baseline: %v", err)
	}
	next, err := advice.LoadOverConstraintBaseline(data)
	if err != nil {
		t.Fatalf("load emitted baseline: %v", err)
	}
	floors := map[string]int{}
	for _, e := range next.Entries {
		floors[e.ID] = e.DirectiveCount
	}
	if floors["gd-a"] != 17 || floors["gd-b"] != 9 {
		t.Errorf("emitted floors = %v, want gd-a:17 gd-b:9", floors)
	}
}

// TestApplyBaselineFiles_PlateauStaysClimbed is the end-to-end CLI-seam proof of
// the property the change exists for: a prior floor below the current count reads
// CLIMBED, and re-emitting keeps the floor (does not ratchet up), so the next run
// still reads CLIMBED.
func TestApplyBaselineFiles_PlateauStaysClimbed(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "baseline.json")
	emitPath := filepath.Join(dir, "next.json")

	// Seed a baseline with gd-hx floored at 10 (its remediated state).
	seed := advice.OverConstraintBaseline{Version: 1, Entries: []advice.OverConstraintBaselineEntry{
		{ID: "gd-hx", DirectiveCount: 10},
	}}
	blob, _ := advice.MarshalOverConstraintBaseline(seed)
	if err := os.WriteFile(basePath, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	findings := []advice.OverConstraintFinding{ocFinding("gd-hx", 17)}
	got, err := applyBaselineFiles(findings, basePath, emitPath)
	if err != nil {
		t.Fatalf("applyBaselineFiles: %v", err)
	}
	if got[0].BaselineDelta != advice.BaselineClimbed || got[0].BaselineFloor != 10 {
		t.Fatalf("delta=%q floor=%d, want climbed/10", got[0].BaselineDelta, got[0].BaselineFloor)
	}
	if !advice.HasBaselineRegression(got) {
		t.Error("a climbed finding must count as a baseline regression")
	}

	// The re-emitted baseline must keep the floor at 10, not raise it to 17.
	data, _ := os.ReadFile(emitPath)
	next, _ := advice.LoadOverConstraintBaseline(data)
	if len(next.Entries) != 1 || next.Entries[0].DirectiveCount != 10 {
		t.Errorf("re-emitted floor = %+v, want gd-hx:10 (must not ratchet up)", next.Entries)
	}
}

// TestApplyBaselineFiles_MalformedBaselineErrors verifies a corrupt baseline file
// surfaces an error rather than being silently treated as an empty floor (which
// would hide every regression).
func TestApplyBaselineFiles_MalformedBaselineErrors(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "baseline.json")
	if err := os.WriteFile(basePath, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := applyBaselineFiles([]advice.OverConstraintFinding{ocFinding("gd-a", 5)}, basePath, ""); err == nil {
		t.Error("expected error on malformed baseline file")
	}
}

// TestApplyBaselineFiles_NoBaselinePathIsPassthrough verifies that with no
// baseline path the findings are returned unchanged (no delta annotation).
func TestApplyBaselineFiles_NoBaselinePathIsPassthrough(t *testing.T) {
	findings := []advice.OverConstraintFinding{ocFinding("gd-a", 5)}
	got, err := applyBaselineFiles(findings, "", "")
	if err != nil {
		t.Fatalf("applyBaselineFiles: %v", err)
	}
	if got[0].BaselineDelta != "" {
		t.Errorf("delta=%q, want empty (no baseline path)", got[0].BaselineDelta)
	}
}

// TestBaselineDeltaLabel covers the rendered DELTA column labels.
func TestBaselineDeltaLabel(t *testing.T) {
	cases := []struct {
		f    advice.OverConstraintFinding
		want string
	}{
		{advice.OverConstraintFinding{BaselineDelta: advice.BaselineNew}, "NEW"},
		{advice.OverConstraintFinding{BaselineDelta: advice.BaselineClimbed, BaselineFloor: 10}, "CLIMBED (was 10)"},
		{advice.OverConstraintFinding{BaselineDelta: advice.BaselineSame, BaselineFloor: 9}, "same (floor 9)"},
		{advice.OverConstraintFinding{}, "-"},
	}
	for _, c := range cases {
		if got := baselineDeltaLabel(c.f); got != c.want {
			t.Errorf("baselineDeltaLabel(%+v) = %q, want %q", c.f.BaselineDelta, got, c.want)
		}
	}
}

// TestCountBaselineRegressions covers the gate/summary counter.
func TestCountBaselineRegressions(t *testing.T) {
	findings := []advice.OverConstraintFinding{
		{BaselineDelta: advice.BaselineNew},
		{BaselineDelta: advice.BaselineClimbed},
		{BaselineDelta: advice.BaselineSame},
		{}, // non-density, no delta
	}
	if got := countBaselineRegressions(findings); got != 2 {
		t.Errorf("countBaselineRegressions = %d, want 2", got)
	}
}
