package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminBackfillOperatorSlugs_EmitsJSONManifest(t *testing.T) {
	mc := newMockClient()
	mc.BackfillOperatorSlugsResp = &client.BackfillOperatorSlugsResponse{
		DryRun:           true,
		BeforeOpaque:     52,
		AfterOpaque:      52,
		Renamed:          40,
		SkippedCollision: 2,
		Machine:          10,
		DurationMs:       500,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillOperatorSlugsCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}
	defer func() {
		_ = adminBackfillOperatorSlugsCmd.Flags().Set("dry-run", "false")
	}()

	out := captureStdout(t, func() {
		if err := adminBackfillOperatorSlugsCmd.RunE(adminBackfillOperatorSlugsCmd, nil); err != nil {
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
	if got := manifest["before_opaque"]; got != float64(52) {
		t.Errorf("before_opaque = %v, want 52", got)
	}
	if got := manifest["renamed"]; got != float64(40) {
		t.Errorf("renamed = %v, want 40", got)
	}
	if got := manifest["machine"]; got != float64(10) {
		t.Errorf("machine = %v, want 10", got)
	}
	if got := manifest["skipped_collision"]; got != float64(2) {
		t.Errorf("skipped_collision = %v, want 2", got)
	}
	if len(mc.BackfillOperatorSlugsCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mc.BackfillOperatorSlugsCalls))
	}
	call := mc.BackfillOperatorSlugsCalls[0]
	if !call.DryRun {
		t.Errorf("call.DryRun = false, want true")
	}
	if call.BatchSize != 200 {
		t.Errorf("call.BatchSize = %d, want 200", call.BatchSize)
	}
}

func TestAdminBackfillOperatorSlugs_ErrorPropagates(t *testing.T) {
	mc := newMockClient()
	mc.BackfillOperatorSlugsErr = errors.New("boom")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := adminBackfillOperatorSlugsCmd.RunE(adminBackfillOperatorSlugsCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error containing boom, got %v", err)
	}
}
