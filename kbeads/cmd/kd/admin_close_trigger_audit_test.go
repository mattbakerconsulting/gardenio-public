package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminCloseTriggerAudit_EmitsJSONManifest(t *testing.T) {
	mc := newMockClient()
	mc.CloseTriggerAuditBeadsResp = &client.CloseTriggerAuditBeadsResponse{
		DryRun:         true,
		CandidateBeads: 226_749,
		BeadsClosed:    226_749,
		Batches:        454,
		LastID:         "gd-je6cJC7uYH",
		DurationMs:     12_000,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminCloseTriggerAuditCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}
	defer func() {
		_ = adminCloseTriggerAuditCmd.Flags().Set("dry-run", "false")
	}()

	out := captureStdout(t, func() {
		if err := adminCloseTriggerAuditCmd.RunE(adminCloseTriggerAuditCmd, nil); err != nil {
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
	if got := manifest["candidate_beads"]; got != float64(226_749) {
		t.Errorf("candidate_beads = %v, want 226749", got)
	}
	if got := manifest["beads_closed"]; got != float64(226_749) {
		t.Errorf("beads_closed = %v, want 226749", got)
	}
	if got := manifest["last_id"]; got != "gd-je6cJC7uYH" {
		t.Errorf("last_id = %v", got)
	}

	if len(mc.CloseTriggerAuditBeadsCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mc.CloseTriggerAuditBeadsCalls))
	}
	call := mc.CloseTriggerAuditBeadsCalls[0]
	if !call.DryRun {
		t.Errorf("call.DryRun = false, want true")
	}
	if call.BatchSize != 500 {
		t.Errorf("call.BatchSize = %d, want 500 (default)", call.BatchSize)
	}
}

func TestAdminCloseTriggerAudit_FlagOverrides(t *testing.T) {
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminCloseTriggerAuditCmd.Flags().Set("batch-size", "50"); err != nil {
		t.Fatalf("set batch-size: %v", err)
	}
	if err := adminCloseTriggerAuditCmd.Flags().Set("max-batches", "3"); err != nil {
		t.Fatalf("set max-batches: %v", err)
	}
	defer func() {
		_ = adminCloseTriggerAuditCmd.Flags().Set("batch-size", "500")
		_ = adminCloseTriggerAuditCmd.Flags().Set("max-batches", "0")
	}()

	_ = captureStdout(t, func() {
		if err := adminCloseTriggerAuditCmd.RunE(adminCloseTriggerAuditCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.CloseTriggerAuditBeadsCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mc.CloseTriggerAuditBeadsCalls))
	}
	call := mc.CloseTriggerAuditBeadsCalls[0]
	if call.BatchSize != 50 {
		t.Errorf("call.BatchSize = %d, want 50", call.BatchSize)
	}
	if call.MaxBatches != 3 {
		t.Errorf("call.MaxBatches = %d, want 3", call.MaxBatches)
	}
}

func TestAdminCloseTriggerAudit_PropagatesError(t *testing.T) {
	mc := newMockClient()
	mc.CloseTriggerAuditBeadsErr = errors.New("boom")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := adminCloseTriggerAuditCmd.RunE(adminCloseTriggerAuditCmd, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want substring 'boom'", err)
	}
}
