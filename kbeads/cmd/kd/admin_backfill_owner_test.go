package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminBackfillOwner_EmitsJSONManifest(t *testing.T) {
	mc := newMockClient()
	mc.BackfillOwnerResp = &client.BackfillOwnerResponse{
		DryRun:           true,
		Target:           "slack:U0OPERATOR1",
		BeforeCandidates: 18743,
		AfterCandidates:  214,
		CandidateBeads:   18743,
		RebindCount: map[string]int64{
			"slack:U02ABC":           12000,
			"oidc:alice@example.com": 4000,
			"system:unowned":         2529,
		},
		KeptMattCount:   214,
		UnresolvedCount: 2529,
		Batches:         4,
		DurationMs:      9800,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillOwnerCmd.Flags().Set("rebind-matt-default", "true"); err != nil {
		t.Fatalf("set rebind-matt-default: %v", err)
	}
	if err := adminBackfillOwnerCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}
	defer func() {
		_ = adminBackfillOwnerCmd.Flags().Set("rebind-matt-default", "false")
		_ = adminBackfillOwnerCmd.Flags().Set("dry-run", "false")
	}()

	out := captureStdout(t, func() {
		if err := adminBackfillOwnerCmd.RunE(adminBackfillOwnerCmd, nil); err != nil {
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
	if got := manifest["before_candidates"]; got != float64(18743) {
		t.Errorf("before_candidates = %v, want 18743", got)
	}
	rebind, ok := manifest["rebind_count"].(map[string]any)
	if !ok {
		t.Fatalf("rebind_count missing or wrong type: %T", manifest["rebind_count"])
	}
	if got := rebind["system:unowned"]; got != float64(2529) {
		t.Errorf("rebind[system:unowned] = %v, want 2529", got)
	}

	if len(mc.BackfillOwnerCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mc.BackfillOwnerCalls))
	}
	call := mc.BackfillOwnerCalls[0]
	if call.Target != "slack:U0OPERATOR1" {
		t.Errorf("call.Target = %q", call.Target)
	}
	if !call.DryRun {
		t.Errorf("call.DryRun = false, want true")
	}
	if call.BatchSize != 5000 {
		t.Errorf("call.BatchSize = %d, want 5000", call.BatchSize)
	}
	if call.UnownedSentinel != "system:unowned" {
		t.Errorf("call.UnownedSentinel = %q, want system:unowned", call.UnownedSentinel)
	}
	if call.Since.IsZero() {
		t.Errorf("call.Since should be set when --rebind-matt-default is true")
	}
}

func TestAdminBackfillOwner_ErrorPropagates(t *testing.T) {
	mc := newMockClient()
	mc.BackfillOwnerErr = errors.New("boom")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillOwnerCmd.Flags().Set("target", "slack:U0OPERATOR1"); err != nil {
		t.Fatalf("set target: %v", err)
	}

	captureStdout(t, func() {
		err := adminBackfillOwnerCmd.RunE(adminBackfillOwnerCmd, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("expected error to contain 'boom', got: %v", err)
		}
	})
}
