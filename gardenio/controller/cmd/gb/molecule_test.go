package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func sampleMoleculeUsage() *beadsapi.MoleculeUsage {
	return &beadsapi.MoleculeUsage{
		MoleculeID: "gd-Abc123",
		SessionUsage: beadsapi.SessionUsage{
			InputTokens:  1_000_000,
			OutputTokens: 200_000,
			TotalCostUSD: 21.70,
		},
		WorkflowSpend: beadsapi.WorkflowSpend{
			AgentsSpawned:   47,
			InputTokens:     3_100_000,
			OutputTokens:    400_000,
			CacheReadTokens: 12_300_000,
			TotalCostUSD:    62.40,
		},
		TotalCostUSD:     84.10,
		BeadCount:        6,
		SessionCount:     11,
		WorkflowRunCount: 1,
		Partial:          false,
		Steps: []beadsapi.MoleculeStepUsage{
			{
				BeadID:       "gd-s1",
				Title:        "scan",
				SessionUsage: beadsapi.SessionUsage{TotalCostUSD: 3.20, InputTokens: 100_000},
				TotalCostUSD: 3.20,
			},
			{
				BeadID:       "gd-s2",
				Title:        "fan-out",
				SessionUsage: beadsapi.SessionUsage{TotalCostUSD: 0.80, InputTokens: 50_000},
				WorkflowSpend: &beadsapi.WorkflowSpend{
					AgentsSpawned: 47,
					TotalCostUSD:  62.40,
					Final:         true,
				},
				TotalCostUSD: 63.20,
			},
		},
	}
}

// TestPrintMoleculeCost_Text asserts the human-readable rendering surfaces the
// split (host-session vs workflow_spend), the summed total, and the per-step
// workflow detail.
func TestPrintMoleculeCost_Text(t *testing.T) {
	u := sampleMoleculeUsage()
	out := string(captureStdout(t, func() error {
		printMoleculeCost(u)
		return nil
	}))

	for _, want := range []string{
		"molecule gd-Abc123",
		"$84.10",                            // summed total
		"host sessions $21.70",              // host-session pool
		"workflow_spend $62.40",             // workflow pool
		"workflow-runs: 1",                  // run count
		"scan",                              // step title
		"gd-s2",                             // workflow step id
		"workflow_spend $62.40 (47 agents)", // per-step detail
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q\n---\n%s", want, out)
		}
	}

	// A final (complete) molecule must NOT show the partial banner.
	if strings.Contains(out, "partial") {
		t.Errorf("final molecule should not surface partial banner, got:\n%s", out)
	}
}

// TestPrintMoleculeCost_Partial asserts the partial banner appears when the
// rollup flagged a non-final descendant, and the per-step "non-final" note.
func TestPrintMoleculeCost_Partial(t *testing.T) {
	u := sampleMoleculeUsage()
	u.Partial = true
	u.Steps[1].WorkflowSpend.Final = false

	out := string(captureStdout(t, func() error {
		printMoleculeCost(u)
		return nil
	}))

	if !strings.Contains(out, "partial") {
		t.Errorf("expected partial banner, got:\n%s", out)
	}
	if !strings.Contains(out, "non-final") {
		t.Errorf("expected per-step non-final note, got:\n%s", out)
	}
}

// TestMoleculeCost_JSONShape asserts --json emits the full MoleculeUsage with
// the corrected sum semantics and round-trips back to the same struct.
func TestMoleculeCost_JSONShape(t *testing.T) {
	u := sampleMoleculeUsage()
	out := captureStdout(t, func() error {
		printJSON(u)
		return nil
	})

	var got beadsapi.MoleculeUsage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal json output: %v\n%s", err, out)
	}

	if got.MoleculeID != "gd-Abc123" {
		t.Errorf("molecule_id = %q, want gd-Abc123", got.MoleculeID)
	}
	if got.TotalCostUSD != 84.10 {
		t.Errorf("total_cost_usd = %v, want 84.10", got.TotalCostUSD)
	}
	// total == host-session + workflow_spend (the §3.2 sum, not prefer-one).
	wantSum := got.SessionUsage.TotalCostUSD + got.WorkflowSpend.TotalCostUSD
	if got.TotalCostUSD != wantSum {
		t.Errorf("total %v != session %v + workflow %v",
			got.TotalCostUSD, got.SessionUsage.TotalCostUSD, got.WorkflowSpend.TotalCostUSD)
	}
	if got.WorkflowSpend.AgentsSpawned != 47 {
		t.Errorf("workflow_spend.agents_spawned = %d, want 47", got.WorkflowSpend.AgentsSpawned)
	}
	if len(got.Steps) != 2 {
		t.Fatalf("steps len = %d, want 2", len(got.Steps))
	}
	if got.Steps[1].WorkflowSpend == nil {
		t.Error("step[1] should carry a workflow_spend object")
	}

	// Snapshot the raw JSON keys so a struct-tag rename is caught.
	raw := string(out)
	for _, key := range []string{
		`"molecule_id"`, `"session_usage"`, `"workflow_spend"`,
		`"total_cost_usd"`, `"partial"`, `"steps"`, `"workflow_run_count"`,
	} {
		if !strings.Contains(raw, key) {
			t.Errorf("json missing key %s\n%s", key, raw)
		}
	}
}

func TestFmtTokens(t *testing.T) {
	cases := map[int64]string{
		0:          "0",
		999:        "999",
		1_000:      "1.0K",
		4_100_000:  "4.1M",
		600_000:    "600.0K",
		12_300_000: "12.3M",
	}
	for n, want := range cases {
		if got := fmtTokens(n); got != want {
			t.Errorf("fmtTokens(%d) = %q, want %q", n, got, want)
		}
	}
}
