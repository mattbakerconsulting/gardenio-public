package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// runBundleReport drives `gb decision report --type=bundle` against the stub
// with GARDENIO_BUNDLE_AUTOCOLLECT unset — the bundle artifact_type must route
// through the bundle path unconditionally (ka-0000007047), unlike the env-gated
// autocollect path exercised in decision_autocollect_test.go.
func runBundleReport(t *testing.T, stub *autoCollectStub) *autoCollectStub {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	cmd := decisionReportCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("content", "ok")
	_ = cmd.Flags().Set("type", "bundle")
	_ = cmd.Flags().Set("format", "markdown")
	t.Cleanup(func() {
		_ = cmd.Flags().Set("content", "")
		_ = cmd.Flags().Set("type", "")
		_ = cmd.Flags().Set("format", "markdown")
	})
	if err := cmd.RunE(cmd, []string{"gd-decision-1"}); err != nil {
		t.Fatalf("decision report --type=bundle: %v", err)
	}
	return stub
}

// TestDecisionReport_BundleArtifact_RoutesToBundle_EnvUnset asserts that a
// decision report with artifact_type=bundle creates/finds a bundle on the
// origin task and links the report under it, WITHOUT the T4 autocollect env
// var set — the bundle type is the operator's explicit request for a multi-doc
// deliverable, so it is unconditional.
func TestDecisionReport_BundleArtifact_RoutesToBundle_EnvUnset(t *testing.T) {
	t.Setenv("GARDENIO_BUNDLE_AUTOCOLLECT", "")
	stub := stubWithOriginAndDecision()
	runBundleReport(t, stub)

	bundleCount := 0
	var bundleID string
	for _, b := range stub.createdBeads {
		if b.Type == "bundle" {
			bundleCount++
			bundleID = b.ID
		}
	}
	if bundleCount != 1 {
		t.Fatalf("artifact_type=bundle with env unset: want exactly 1 bundle created, got %d (%+v)", bundleCount, stub.createdBeads)
	}

	// bundle → origin parent-child, and report → bundle parent-child.
	wantBundleToOrigin, wantReportToBundle := false, false
	for _, d := range stub.addedDeps {
		if d.Type != "parent-child" {
			continue
		}
		if d.BeadID == bundleID && d.DependsOnID == "gd-task-orig" {
			wantBundleToOrigin = true
		}
		if strings.HasPrefix(d.BeadID, "gd-report-") && d.DependsOnID == bundleID {
			wantReportToBundle = true
		}
	}
	if !wantBundleToOrigin {
		t.Errorf("expected bundle→origin parent-child edge; got %+v", stub.addedDeps)
	}
	if !wantReportToBundle {
		t.Errorf("expected report→bundle parent-child edge; got %+v", stub.addedDeps)
	}
}

// TestArtifactTypes_IncludesBundle guards the shared allowlist: bundle must be
// a recognized artifact_type across every surface that reads
// beadsapi.ArtifactTypes.
func TestArtifactTypes_IncludesBundle(t *testing.T) {
	if !beadsapi.IsValidArtifactType("bundle") {
		t.Fatal("beadsapi.IsValidArtifactType(\"bundle\") = false, want true")
	}
	if !strings.Contains(beadsapi.ArtifactTypesList(), "bundle") {
		t.Errorf("ArtifactTypesList()=%q missing bundle", beadsapi.ArtifactTypesList())
	}
}
