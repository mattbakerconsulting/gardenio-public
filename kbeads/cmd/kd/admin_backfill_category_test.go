package main

import (
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminBackfillCategory_SinglePage(t *testing.T) {
	mc := newMockClient()
	mc.BackfillCategoryPages = []*client.BackfillCategoryResponse{
		{BeadsScanned: 3, LastID: "gd-c", Done: true},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := adminBackfillCategoryCmd.RunE(adminBackfillCategoryCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.BackfillCategoryCalls); got != 1 {
		t.Fatalf("expected 1 call, got %d", got)
	}
	if want := 500; mc.BackfillCategoryCalls[0].BatchSize != want {
		t.Errorf("BatchSize = %d, want %d", mc.BackfillCategoryCalls[0].BatchSize, want)
	}
	for _, want := range []string{"page 1", "restamped=3", "beads_restamped=3"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestAdminBackfillCategory_PaginatesUntilDone(t *testing.T) {
	mc := newMockClient()
	mc.BackfillCategoryPages = []*client.BackfillCategoryResponse{
		{BeadsScanned: 2, LastID: "gd-b", Done: false},
		{BeadsScanned: 2, LastID: "gd-d", Done: false},
		{BeadsScanned: 1, LastID: "gd-e", Done: true},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := adminBackfillCategoryCmd.RunE(adminBackfillCategoryCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.BackfillCategoryCalls); got != 3 {
		t.Fatalf("expected 3 calls, got %d", got)
	}
	if mc.BackfillCategoryCalls[0].AfterID != "" {
		t.Errorf("first call AfterID = %q, want empty", mc.BackfillCategoryCalls[0].AfterID)
	}
	if mc.BackfillCategoryCalls[1].AfterID != "gd-b" {
		t.Errorf("second call AfterID = %q, want %q", mc.BackfillCategoryCalls[1].AfterID, "gd-b")
	}
	if mc.BackfillCategoryCalls[2].AfterID != "gd-d" {
		t.Errorf("third call AfterID = %q, want %q", mc.BackfillCategoryCalls[2].AfterID, "gd-d")
	}
	for _, want := range []string{"page 1", "page 2", "page 3", "beads_restamped=5"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestAdminBackfillCategory_AbortsOnNonFinalPageWithNoCursor(t *testing.T) {
	mc := newMockClient()
	mc.BackfillCategoryPages = []*client.BackfillCategoryResponse{
		{BeadsScanned: 0, LastID: "", Done: false},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	captureStdout(t, func() {
		err := adminBackfillCategoryCmd.RunE(adminBackfillCategoryCmd, nil)
		if err == nil {
			t.Fatal("expected error on non-final page with no cursor, got nil")
		}
		if !strings.Contains(err.Error(), "no cursor") {
			t.Errorf("expected error to mention 'no cursor', got: %v", err)
		}
	})
}
