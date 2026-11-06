package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminBackfillEmptyOwner_EmitsJSONManifest(t *testing.T) {
	mc := newMockClient()
	mc.BackfillEmptyOwnerResp = &client.BackfillEmptyOwnerResponse{
		DryRun:        true,
		BeforeEmpty:   18743,
		AfterEmpty:    0,
		Phase1Filled:  16000,
		Phase2Filled:  2214,
		Phase3Filled:  529,
		Phase1Batches: 0,
		Phase2Batches: 0,
		Phase3Batches: 0,
		DurationMs:    9800,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillEmptyOwnerCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}
	defer func() { _ = adminBackfillEmptyOwnerCmd.Flags().Set("dry-run", "false") }()

	out := captureStdout(t, func() {
		if err := adminBackfillEmptyOwnerCmd.RunE(adminBackfillEmptyOwnerCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	var manifest map[string]any
	if err := json.Unmarshal([]byte(out), &manifest); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got := manifest["dry_run"]; got != true {
		t.Errorf("dry_run = %v, want true", got)
	}
	if got := manifest["before_empty"]; got != float64(18743) {
		t.Errorf("before_empty = %v, want 18743", got)
	}
	if got := manifest["phase1_filled"]; got != float64(16000) {
		t.Errorf("phase1_filled = %v, want 16000", got)
	}
	if got := manifest["phase3_filled"]; got != float64(529) {
		t.Errorf("phase3_filled = %v, want 529", got)
	}

	if len(mc.BackfillEmptyOwnerCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mc.BackfillEmptyOwnerCalls))
	}
	call := mc.BackfillEmptyOwnerCalls[0]
	if !call.DryRun {
		t.Errorf("call.DryRun = false, want true")
	}
	if call.BatchSize != 5000 {
		t.Errorf("call.BatchSize = %d, want 5000 (default)", call.BatchSize)
	}
	if call.UnownedSentinel != "system:unowned" {
		t.Errorf("call.UnownedSentinel = %q, want system:unowned", call.UnownedSentinel)
	}
	if call.LegacyOperatedBySentinel != "system:legacy-pre-attribution" {
		t.Errorf("call.LegacyOperatedBySentinel = %q, want system:legacy-pre-attribution", call.LegacyOperatedBySentinel)
	}
}

func TestAdminBackfillEmptyOwner_ErrorPropagates(t *testing.T) {
	mc := newMockClient()
	mc.BackfillEmptyOwnerErr = errors.New("boom")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	captureStdout(t, func() {
		err := adminBackfillEmptyOwnerCmd.RunE(adminBackfillEmptyOwnerCmd, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("expected error to contain 'boom', got: %v", err)
		}
	})
}
