package main

import (
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

// resetReextractFlags restores the shared package-level command's flags to
// their declared defaults so a test that mutates them can't leak into siblings.
func resetReextractFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = adminReextractReferencesCmd.Flags().Set("batch-size", "100")
		_ = adminReextractReferencesCmd.Flags().Set("after", "")
		_ = adminReextractReferencesCmd.Flags().Set("type", "")
	})
}

func TestAdminReextractReferences_SinglePage(t *testing.T) {
	mc := newMockClient()
	mc.ReextractReferencesPages = []*client.ReextractReferencesResponse{
		{BeadsScanned: 3, CommentsScanned: 2, ReferencesWritten: 5, LastID: "gd-c", Done: true, TotalReferences: 5},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := adminReextractReferencesCmd.RunE(adminReextractReferencesCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.ReextractReferencesCalls); got != 1 {
		t.Fatalf("expected 1 call, got %d", got)
	}
	if want := defaultReextractBatchSize; mc.ReextractReferencesCalls[0].BatchSize != want {
		t.Errorf("BatchSize = %d, want %d (safe default under the server 30s deadline)", mc.ReextractReferencesCalls[0].BatchSize, want)
	}
	for _, want := range []string{"page 1", "beads=3", "comments=2", "refs=5", "total_references=5"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestAdminReextractReferences_PaginatesUntilDone(t *testing.T) {
	mc := newMockClient()
	mc.ReextractReferencesPages = []*client.ReextractReferencesResponse{
		{BeadsScanned: 2, CommentsScanned: 1, ReferencesWritten: 3, LastID: "gd-b", Done: false},
		{BeadsScanned: 2, CommentsScanned: 0, ReferencesWritten: 1, LastID: "gd-d", Done: false},
		{BeadsScanned: 1, CommentsScanned: 1, ReferencesWritten: 2, LastID: "gd-e", Done: true, TotalReferences: 6},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := adminReextractReferencesCmd.RunE(adminReextractReferencesCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.ReextractReferencesCalls); got != 3 {
		t.Fatalf("expected 3 calls, got %d", got)
	}
	if mc.ReextractReferencesCalls[0].AfterID != "" {
		t.Errorf("first call AfterID = %q, want empty", mc.ReextractReferencesCalls[0].AfterID)
	}
	if mc.ReextractReferencesCalls[1].AfterID != "gd-b" {
		t.Errorf("second call AfterID = %q, want %q", mc.ReextractReferencesCalls[1].AfterID, "gd-b")
	}
	if mc.ReextractReferencesCalls[2].AfterID != "gd-d" {
		t.Errorf("third call AfterID = %q, want %q", mc.ReextractReferencesCalls[2].AfterID, "gd-d")
	}
	for _, want := range []string{"page 1", "page 2", "page 3", "beads_scanned=5", "references_written=6", "total_references=6"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

// TestAdminReextractReferences_TypeFlagForwarded asserts `--type decision` is
// carried into every page request so the server narrows the walk to decisions
// — the targeted pass that makes the parked_on_decision badge sound in seconds
// rather than after the full ~677k-bead backfill (ka-0000005898).
func TestAdminReextractReferences_TypeFlagForwarded(t *testing.T) {
	mc := newMockClient()
	mc.ReextractReferencesPages = []*client.ReextractReferencesResponse{
		{BeadsScanned: 2, ReferencesWritten: 4, LastID: "gd-b", Done: false},
		{BeadsScanned: 1, ReferencesWritten: 2, LastID: "gd-c", Done: true, TotalReferences: 6},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminReextractReferencesCmd.Flags().Set("type", "decision"); err != nil {
		t.Fatalf("set --type: %v", err)
	}
	// Reset the flag after the run so it doesn't leak into sibling tests that
	// share the package-level command instance.
	defer func() { _ = adminReextractReferencesCmd.Flags().Set("type", "") }()

	captureStdout(t, func() {
		if err := adminReextractReferencesCmd.RunE(adminReextractReferencesCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.ReextractReferencesCalls); got != 2 {
		t.Fatalf("expected 2 calls, got %d", got)
	}
	for i, call := range mc.ReextractReferencesCalls {
		if call.Type != "decision" {
			t.Errorf("call %d Type = %q, want %q", i, call.Type, "decision")
		}
	}
}

// TestAdminReextractReferences_AutoHalvesOnTimeout asserts that a page which
// overruns the server's ~30s deadline (surfaced as an HTTP 500) is retried at
// half the batch size against the SAME cursor, rather than aborting the whole
// walk and forcing the operator to hand-copy the cursor (ka-0000001700).
func TestAdminReextractReferences_AutoHalvesOnTimeout(t *testing.T) {
	resetReextractFlags(t)
	mc := newMockClient()
	// Fail every page whose batch-size is > 25; succeed (and finish) once the
	// walk shrinks to 25 or below.
	mc.ReextractReferencesFn = func(req *client.ReextractReferencesRequest) (*client.ReextractReferencesResponse, error) {
		if req.BatchSize > 25 {
			return nil, &client.APIError{StatusCode: 500, Message: "reextract page exceeded the request time budget"}
		}
		return &client.ReextractReferencesResponse{
			BeadsScanned: 25, ReferencesWritten: 10, LastID: "gd-z", Done: true, TotalReferences: 10,
		}, nil
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := adminReextractReferencesCmd.RunE(adminReextractReferencesCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	// Default 100 -> 50 -> 25 (success): 2 failed attempts + 1 success = 3 calls.
	if got := len(mc.ReextractReferencesCalls); got != 3 {
		t.Fatalf("expected 3 calls (100->50->25), got %d", got)
	}
	wantBatches := []int{100, 50, 25}
	for i, want := range wantBatches {
		if mc.ReextractReferencesCalls[i].BatchSize != want {
			t.Errorf("call %d BatchSize = %d, want %d", i, mc.ReextractReferencesCalls[i].BatchSize, want)
		}
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("expected a timeout/retry notice in output, got:\n%s", out)
	}
}

// TestAdminReextractReferences_TimeoutAtFloorAborts asserts the walk does not
// shrink forever: once the batch reaches the floor and still times out, it
// aborts and surfaces the actionable cursor + batch-size in the error.
func TestAdminReextractReferences_TimeoutAtFloorAborts(t *testing.T) {
	resetReextractFlags(t)
	mc := newMockClient()
	mc.ReextractReferencesErr = &client.APIError{StatusCode: 500, Message: "reextract page exceeded the request time budget"}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	captureStdout(t, func() {
		err := adminReextractReferencesCmd.RunE(adminReextractReferencesCmd, nil)
		if err == nil {
			t.Fatal("expected error when the floor batch still times out, got nil")
		}
		if !strings.Contains(err.Error(), "batch-size=25") {
			t.Errorf("expected error to name the floor batch-size, got: %v", err)
		}
	})

	// 100 -> 50 -> 25 (all fail): the last attempt at the floor is not retried.
	if got := len(mc.ReextractReferencesCalls); got != 3 {
		t.Fatalf("expected 3 calls before aborting at floor, got %d", got)
	}
}

func TestAdminReextractReferences_AbortsOnEmptyNonFinalPage(t *testing.T) {
	mc := newMockClient()
	mc.ReextractReferencesPages = []*client.ReextractReferencesResponse{
		{BeadsScanned: 0, Done: false},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	captureStdout(t, func() {
		err := adminReextractReferencesCmd.RunE(adminReextractReferencesCmd, nil)
		if err == nil {
			t.Fatal("expected error on empty non-final page, got nil")
		}
		if !strings.Contains(err.Error(), "empty page") {
			t.Errorf("expected error to mention 'empty page', got: %v", err)
		}
	})
}
