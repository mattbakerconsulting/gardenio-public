package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminBackfillSlackUserID_EmitsJSONManifest(t *testing.T) {
	mc := newMockClient()
	mc.BackfillSlackUserIDResp = &client.BackfillSlackUserIDResponse{
		DryRun:               true,
		BeforeMissing:        122,
		AfterMissing:         122,
		Updated:              120,
		ResolvedViaThreadID:  100,
		ResolvedViaCreatedBy: 20,
		Unresolved:           2,
		DurationMs:           800,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillSlackUserIDCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}
	defer func() {
		_ = adminBackfillSlackUserIDCmd.Flags().Set("dry-run", "false")
	}()

	out := captureStdout(t, func() {
		if err := adminBackfillSlackUserIDCmd.RunE(adminBackfillSlackUserIDCmd, nil); err != nil {
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
	if got := manifest["before_missing"]; got != float64(122) {
		t.Errorf("before_missing = %v, want 122", got)
	}
	if got := manifest["updated"]; got != float64(120) {
		t.Errorf("updated = %v, want 120", got)
	}
	if got := manifest["resolved_via_thread_id"]; got != float64(100) {
		t.Errorf("resolved_via_thread_id = %v, want 100", got)
	}
	if len(mc.BackfillSlackUserIDCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mc.BackfillSlackUserIDCalls))
	}
	call := mc.BackfillSlackUserIDCalls[0]
	if !call.DryRun {
		t.Errorf("call.DryRun = false, want true")
	}
	if call.BatchSize != 200 {
		t.Errorf("call.BatchSize = %d, want 200", call.BatchSize)
	}
}

func TestAdminBackfillSlackUserID_ErrorPropagates(t *testing.T) {
	mc := newMockClient()
	mc.BackfillSlackUserIDErr = errors.New("boom")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := adminBackfillSlackUserIDCmd.RunE(adminBackfillSlackUserIDCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error containing boom, got %v", err)
	}
}
