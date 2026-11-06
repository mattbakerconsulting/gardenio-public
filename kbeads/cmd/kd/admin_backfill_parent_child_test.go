package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminBackfillParentChild_EmitsJSONManifest(t *testing.T) {
	mc := newMockClient()
	mc.BackfillParentChildDepsResp = &client.BackfillParentChildDepsResponse{
		DryRun:         true,
		CandidateBeads: 1234,
		EdgesAdded:     310,
		EdgesByAuthor: map[string]int64{
			"sched-gardenio-gardener-delivery-f318": 120,
			"gardenio-research-attribution-rq6":     55,
		},
		SkippedNoAgent:       40,
		SkippedNoTaskID:      33,
		SkippedSelfParent:    1,
		SkippedTaskMissing:   2,
		SkippedExistingDep:   500,
		SkippedParentMention: 50,
		Batches:              3,
		LastID:               "gd-BUeO2s4sgS",
		DurationMs:           742,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillParentChildCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}
	defer func() {
		_ = adminBackfillParentChildCmd.Flags().Set("dry-run", "false")
	}()

	out := captureStdout(t, func() {
		if err := adminBackfillParentChildCmd.RunE(adminBackfillParentChildCmd, nil); err != nil {
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
	if got := manifest["edges_added"]; got != float64(310) {
		t.Errorf("edges_added = %v, want 310", got)
	}
	if got := manifest["candidate_beads"]; got != float64(1234) {
		t.Errorf("candidate_beads = %v, want 1234", got)
	}
	byAuthor, ok := manifest["edges_by_author"].(map[string]any)
	if !ok {
		t.Fatalf("edges_by_author is not a map: %T", manifest["edges_by_author"])
	}
	if byAuthor["sched-gardenio-gardener-delivery-f318"] != float64(120) {
		t.Errorf("gardener author count = %v, want 120", byAuthor["sched-gardenio-gardener-delivery-f318"])
	}
	if got := manifest["last_id"]; got != "gd-BUeO2s4sgS" {
		t.Errorf("last_id = %v", got)
	}
	if len(mc.BackfillParentChildDepsCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mc.BackfillParentChildDepsCalls))
	}
	call := mc.BackfillParentChildDepsCalls[0]
	if !call.DryRun {
		t.Errorf("call.DryRun = false, want true")
	}
	if call.BatchSize != 500 {
		t.Errorf("call.BatchSize = %d, want 500", call.BatchSize)
	}
	if !call.IncludeClosed {
		t.Errorf("call.IncludeClosed = false, want true (default)")
	}
	if call.SinceSeconds <= 0 {
		t.Errorf("call.SinceSeconds = %d, want > 0", call.SinceSeconds)
	}
}

func TestAdminBackfillParentChild_FlagOverrides(t *testing.T) {
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillParentChildCmd.Flags().Set("since", "720h"); err != nil {
		t.Fatalf("set since: %v", err)
	}
	if err := adminBackfillParentChildCmd.Flags().Set("batch-size", "50"); err != nil {
		t.Fatalf("set batch-size: %v", err)
	}
	if err := adminBackfillParentChildCmd.Flags().Set("max-batches", "2"); err != nil {
		t.Fatalf("set max-batches: %v", err)
	}
	if err := adminBackfillParentChildCmd.Flags().Set("include-closed", "false"); err != nil {
		t.Fatalf("set include-closed: %v", err)
	}
	defer func() {
		_ = adminBackfillParentChildCmd.Flags().Set("since", "4320h") // 180d
		_ = adminBackfillParentChildCmd.Flags().Set("batch-size", "500")
		_ = adminBackfillParentChildCmd.Flags().Set("max-batches", "0")
		_ = adminBackfillParentChildCmd.Flags().Set("include-closed", "true")
	}()

	captureStdout(t, func() {
		if err := adminBackfillParentChildCmd.RunE(adminBackfillParentChildCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.BackfillParentChildDepsCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mc.BackfillParentChildDepsCalls))
	}
	call := mc.BackfillParentChildDepsCalls[0]
	if call.SinceSeconds != int64(720*60*60) {
		t.Errorf("SinceSeconds = %d, want %d", call.SinceSeconds, 720*60*60)
	}
	if call.BatchSize != 50 {
		t.Errorf("BatchSize = %d, want 50", call.BatchSize)
	}
	if call.MaxBatches != 2 {
		t.Errorf("MaxBatches = %d, want 2", call.MaxBatches)
	}
	if call.IncludeClosed {
		t.Errorf("IncludeClosed = true, want false")
	}
}

func TestAdminBackfillParentChild_ErrorPropagates(t *testing.T) {
	mc := newMockClient()
	mc.BackfillParentChildDepsErr = errors.New("kaboom")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	captureStdout(t, func() {
		err := adminBackfillParentChildCmd.RunE(adminBackfillParentChildCmd, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "kaboom") {
			t.Errorf("expected error to contain 'kaboom', got: %v", err)
		}
	})
}
