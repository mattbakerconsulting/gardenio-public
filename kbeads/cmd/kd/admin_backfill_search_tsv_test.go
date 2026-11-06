package main

import (
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminBackfillSearchTSV_SinglePage(t *testing.T) {
	mc := newMockClient()
	mc.BackfillSearchTSVPages = []*client.BackfillSearchTSVResponse{
		{BeadsScanned: 3, LastID: "gd-c", Done: true, Remaining: 0},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := adminBackfillSearchTSVCmd.RunE(adminBackfillSearchTSVCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.BackfillSearchTSVCalls); got != 1 {
		t.Fatalf("expected 1 call, got %d", got)
	}
	if want := 500; mc.BackfillSearchTSVCalls[0].BatchSize != want {
		t.Errorf("BatchSize = %d, want %d", mc.BackfillSearchTSVCalls[0].BatchSize, want)
	}
	for _, want := range []string{"page 1", "filled=3", "beads_filled=3", "remaining_null=0"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestAdminBackfillSearchTSV_PaginatesUntilDone(t *testing.T) {
	mc := newMockClient()
	mc.BackfillSearchTSVPages = []*client.BackfillSearchTSVResponse{
		{BeadsScanned: 2, LastID: "gd-b", Done: false},
		{BeadsScanned: 2, LastID: "gd-d", Done: false},
		{BeadsScanned: 1, LastID: "gd-e", Done: true, Remaining: 0},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := adminBackfillSearchTSVCmd.RunE(adminBackfillSearchTSVCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.BackfillSearchTSVCalls); got != 3 {
		t.Fatalf("expected 3 calls, got %d", got)
	}
	if mc.BackfillSearchTSVCalls[0].AfterID != "" {
		t.Errorf("first call AfterID = %q, want empty", mc.BackfillSearchTSVCalls[0].AfterID)
	}
	if mc.BackfillSearchTSVCalls[1].AfterID != "gd-b" {
		t.Errorf("second call AfterID = %q, want %q", mc.BackfillSearchTSVCalls[1].AfterID, "gd-b")
	}
	if mc.BackfillSearchTSVCalls[2].AfterID != "gd-d" {
		t.Errorf("third call AfterID = %q, want %q", mc.BackfillSearchTSVCalls[2].AfterID, "gd-d")
	}
	for _, want := range []string{"page 1", "page 2", "page 3", "beads_filled=5", "remaining_null=0"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestAdminBackfillSearchTSV_AbortsOnNonFinalPageWithNoCursor(t *testing.T) {
	mc := newMockClient()
	mc.BackfillSearchTSVPages = []*client.BackfillSearchTSVResponse{
		{BeadsScanned: 0, LastID: "", Done: false},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	captureStdout(t, func() {
		err := adminBackfillSearchTSVCmd.RunE(adminBackfillSearchTSVCmd, nil)
		if err == nil {
			t.Fatal("expected error on non-final page with no cursor, got nil")
		}
		if !strings.Contains(err.Error(), "no cursor") {
			t.Errorf("expected error to mention 'no cursor', got: %v", err)
		}
	})
}
