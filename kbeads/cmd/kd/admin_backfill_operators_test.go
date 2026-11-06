package main

import (
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminBackfillOperators_DryRun(t *testing.T) {
	mc := newMockClient()
	mc.BackfillOperatorsResp = &client.BackfillOperatorsResponse{
		DistinctOperatedBy:    7,
		OperatorsUpserted:     0, // dry-run does not write
		AliasesUpserted:       0,
		BeadsUpdated:          12,
		CommentsUpdated:       34,
		DepsUpdated:           5,
		EventsUpdated:         99,
		MutationsUpdated:      3,
		BeadsEmptySentinel:    2,
		CommentsEmptySentinel: 1,
		DepsEmptySentinel:     0,
		QuarantinedOperatedBy: []string{"weird-no-colon", ":leadingcolon"},
		DryRun:                true,
		DurationMs:            250,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillOperatorsCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}
	defer func() { _ = adminBackfillOperatorsCmd.Flags().Set("dry-run", "false") }()

	out := captureStdout(t, func() {
		if err := adminBackfillOperatorsCmd.RunE(adminBackfillOperatorsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.BackfillOperatorsCalls); got != 1 {
		t.Fatalf("expected 1 call, got %d", got)
	}
	call := mc.BackfillOperatorsCalls[0]
	if !call.DryRun {
		t.Errorf("expected DryRun=true on call")
	}
	if call.BatchSize != 50000 {
		t.Errorf("BatchSize = %d, want 50000", call.BatchSize)
	}
	for _, want := range []string{
		"dry-run",
		"would update",
		"distinct operated_by values: 7",
		"quarantined",
		"\"weird-no-colon\"",
		"\":leadingcolon\"",
		"beads:            12",
		"comments:         34",
		"deps:             5",
		"events:           99",
		"mutation_history: 3",
		"total:            153",
		"empty-string → legacy-pre-attribution sentinel",
		"verifier",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestAdminBackfillOperators_RealRunVerifierClean(t *testing.T) {
	mc := newMockClient()
	mc.BackfillOperatorsResp = &client.BackfillOperatorsResponse{
		DistinctOperatedBy: 3,
		OperatorsUpserted:  3,
		AliasesUpserted:    3,
		BeadsUpdated:       1,
		CommentsUpdated:    2,
		DepsUpdated:        0,
		EventsUpdated:      4,
		MutationsUpdated:   1,
		DryRun:             false,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillOperatorsCmd.Flags().Set("dry-run", "false"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}

	out := captureStdout(t, func() {
		if err := adminBackfillOperatorsCmd.RunE(adminBackfillOperatorsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.BackfillOperatorsCalls); got != 1 {
		t.Fatalf("expected 1 call, got %d", got)
	}
	if mc.BackfillOperatorsCalls[0].DryRun {
		t.Errorf("expected DryRun=false on call")
	}
	for _, want := range []string{
		"running operators-FK backfill",
		"operators upserted:          3",
		"operator_aliases upserted:   3",
		"rows updated",
		"total:            8",
		"verifier clean",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestAdminBackfillOperators_RealRunVerifierDirty(t *testing.T) {
	mc := newMockClient()
	mc.BackfillOperatorsResp = &client.BackfillOperatorsResponse{
		DistinctOperatedBy: 3,
		OperatorsUpserted:  3,
		AliasesUpserted:    3,
		BeadsUpdated:       1,
		VerifierBeads:      2, // straggler
		DryRun:             false,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminBackfillOperatorsCmd.Flags().Set("dry-run", "false"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}

	out := captureStdout(t, func() {
		if err := adminBackfillOperatorsCmd.RunE(adminBackfillOperatorsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if !strings.Contains(out, "verifier nonzero") {
		t.Errorf("expected 'verifier nonzero' warning, got:\n%s", out)
	}
	if !strings.Contains(out, "2 straggler rows") {
		t.Errorf("expected straggler count, got:\n%s", out)
	}
}

func TestAdminBackfillOperators_ErrorPropagates(t *testing.T) {
	mc := newMockClient()
	mc.BackfillOperatorsErr = errors.New("boom")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	captureStdout(t, func() {
		err := adminBackfillOperatorsCmd.RunE(adminBackfillOperatorsCmd, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("expected error to contain 'boom', got: %v", err)
		}
	})
}
