package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminBackfillOperatedByLegacy_EmitsJSONManifest(t *testing.T) {
	mc := newMockClient()
	mc.BackfillOperatedByLegacyResp = &client.BackfillOperatedByLegacyResponse{
		DryRun:               true,
		Target:               "slack:U0OPERATOR1",
		TargetOperatorID:     42,
		BeforeOpenCandidates: 236,
		AfterOpenCandidates:  44,
		CategoryA:            120,
		CategoryB:            55,
		CategoryC:            17,
		CategoryD:            44,
		RebindCount: map[string]int64{
			"A:99": 120,
			"B:7":  55,
		},
		Iterations: 1,
		DurationMs: 800,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillOperatedByLegacyCmd.Flags().Set("target", "slack:U0OPERATOR1"); err != nil {
		t.Fatalf("set target: %v", err)
	}
	if err := adminBackfillOperatedByLegacyCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}
	defer func() {
		_ = adminBackfillOperatedByLegacyCmd.Flags().Set("dry-run", "false")
	}()

	out := captureStdout(t, func() {
		if err := adminBackfillOperatedByLegacyCmd.RunE(adminBackfillOperatedByLegacyCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	var manifest map[string]any
	if err := json.Unmarshal([]byte(out), &manifest); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got := manifest["target"]; got != "slack:U0OPERATOR1" {
		t.Errorf("target = %v, want slack:U0OPERATOR1", got)
	}
	if got := manifest["dry_run"]; got != true {
		t.Errorf("dry_run = %v, want true", got)
	}
	if got := manifest["before_open_candidates"]; got != float64(236) {
		t.Errorf("before = %v, want 236", got)
	}
	if got := manifest["after_open_candidates"]; got != float64(44) {
		t.Errorf("after = %v, want 44", got)
	}
	if len(mc.BackfillOperatedByLegacyCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mc.BackfillOperatedByLegacyCalls))
	}
	call := mc.BackfillOperatedByLegacyCalls[0]
	if call.Target != "slack:U0OPERATOR1" {
		t.Errorf("call.Target = %q", call.Target)
	}
	if !call.DryRun {
		t.Errorf("call.DryRun = false, want true")
	}
	if call.BatchSize != 200 {
		t.Errorf("call.BatchSize = %d, want 200", call.BatchSize)
	}
	if call.MaxIterations != 5 {
		t.Errorf("call.MaxIterations = %d, want 5", call.MaxIterations)
	}
}

func TestAdminBackfillOperatedByLegacy_ErrorPropagates(t *testing.T) {
	mc := newMockClient()
	mc.BackfillOperatedByLegacyErr = errors.New("boom")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillOperatedByLegacyCmd.Flags().Set("target", "slack:U0OPERATOR1"); err != nil {
		t.Fatalf("set target: %v", err)
	}

	captureStdout(t, func() {
		err := adminBackfillOperatedByLegacyCmd.RunE(adminBackfillOperatedByLegacyCmd, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("expected error to contain 'boom', got: %v", err)
		}
	})
}
