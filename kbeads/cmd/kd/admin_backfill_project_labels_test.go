package main

import (
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminBackfillProjectLabels_DryRunPrintsCounts(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	mc.BackfillProjectLabelsPages = []*client.BackfillProjectLabelsResponse{
		{Missing: map[string]int{"session": 4, "agent": 2}, Done: true},
	}
	cmd := adminBackfillProjectLabelsCmd
	_ = cmd.Flags().Set("apply", "false")

	out := captureStdout(t, func() {
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if len(mc.BackfillProjectLabelsCalls) != 1 || !mc.BackfillProjectLabelsCalls[0].DryRun {
		t.Fatalf("calls = %+v, want one dry run", mc.BackfillProjectLabelsCalls)
	}
	if !strings.Contains(out, "agent=2 session=4") {
		t.Errorf("output %q, want the sorted counts", out)
	}
}

func TestAdminBackfillProjectLabels_ApplyPagesUntilDone(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	mc.BackfillProjectLabelsPages = []*client.BackfillProjectLabelsResponse{
		{Labelled: map[string]int{"agent": 2}},
		{Labelled: map[string]int{"agent": 1, "message": 1}, Done: true},
	}
	cmd := adminBackfillProjectLabelsCmd
	_ = cmd.Flags().Set("apply", "true")
	t.Cleanup(func() { _ = cmd.Flags().Set("apply", "false") })

	out := captureStdout(t, func() {
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if got := len(mc.BackfillProjectLabelsCalls); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
	if mc.BackfillProjectLabelsCalls[0].DryRun {
		t.Error("--apply sent a dry run")
	}
	if !strings.Contains(out, "labelled agent=3 message=1") {
		t.Errorf("output %q, want the totals", out)
	}
}
