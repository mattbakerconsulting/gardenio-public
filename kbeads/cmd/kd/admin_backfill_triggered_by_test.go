package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminBackfillTriggeredBy_EmitsJSONManifest(t *testing.T) {
	mc := newMockClient()
	mc.BackfillTriggeredByDepsResp = &client.BackfillTriggeredByDepsResponse{
		DryRun:         true,
		CandidateBeads: 200,
		EdgesAdded:     150,
		EdgesBySignal: map[string]int64{
			"slack_thread_bead_id": 90,
			"trigger_id":           60,
		},
		SkippedExistingDep:   30,
		SkippedNoSignal:      5,
		SkippedAmbiguous:     2,
		SkippedSelfReference: 1,
		SkippedTargetMissing: 12,
		Batches:              2,
		LastID:               "gd-je6cJC7uYH",
		DurationMs:           421,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillTriggeredByCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}
	defer func() {
		_ = adminBackfillTriggeredByCmd.Flags().Set("dry-run", "false")
	}()

	out := captureStdout(t, func() {
		if err := adminBackfillTriggeredByCmd.RunE(adminBackfillTriggeredByCmd, nil); err != nil {
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
	if got := manifest["edges_added"]; got != float64(150) {
		t.Errorf("edges_added = %v, want 150", got)
	}
	bySignal, ok := manifest["edges_by_signal"].(map[string]any)
	if !ok {
		t.Fatalf("edges_by_signal is not a map: %T", manifest["edges_by_signal"])
	}
	if bySignal["slack_thread_bead_id"] != float64(90) {
		t.Errorf("slack signal count = %v, want 90", bySignal["slack_thread_bead_id"])
	}
	if got := manifest["last_id"]; got != "gd-je6cJC7uYH" {
		t.Errorf("last_id = %v", got)
	}
	if len(mc.BackfillTriggeredByDepsCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mc.BackfillTriggeredByDepsCalls))
	}
	call := mc.BackfillTriggeredByDepsCalls[0]
	if !call.DryRun {
		t.Errorf("call.DryRun = false, want true")
	}
	if call.BatchSize != 500 {
		t.Errorf("call.BatchSize = %d, want 500", call.BatchSize)
	}
	if call.SinceSeconds != int64(24*3600) {
		t.Errorf("call.SinceSeconds = %d, want %d", call.SinceSeconds, 24*3600)
	}
}

func TestAdminBackfillTriggeredBy_PropagatesError(t *testing.T) {
	mc := newMockClient()
	mc.BackfillTriggeredByDepsErr = errors.New("kaboom")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := adminBackfillTriggeredByCmd.RunE(adminBackfillTriggeredByCmd, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("error = %v, want it to contain 'kaboom'", err)
	}
}
