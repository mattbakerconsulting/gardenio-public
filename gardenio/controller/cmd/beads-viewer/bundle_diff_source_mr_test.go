package main

// Bundle diff source-MR linkage tests (C5 — ka-0000006690).
//
// A bundle that contains diff cards from an MR should show the producing MR
// as a pill at the top of each diff card. Bundles without an originating MR
// must render identically to the pre-C5 surface.

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

const minimalUnifiedDiff = `diff --git a/x b/x
--- a/x
+++ b/x
@@ -1,1 +1,1 @@
-old
+new
`

// TestRenderBundleDiff_SourceMRRow_AppearsWhenSet asserts that the diff
// renderer emits a Source pill above the file list when ctx.SourceMR is set
// (and that the pill carries the MR URL, IID, and state).
func TestRenderBundleDiff_SourceMRRow_AppearsWhenSet(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	ctx.SourceMR = &beadsapi.MergeRequestBeadFields{
		MRURL:   "https://gitlab.example/g/p/-/merge_requests/42",
		MRIID:   "42",
		MRState: "merged",
	}
	card := bundleDocCard{Format: "diff", Content: minimalUnifiedDiff, Language: "go"}
	got, err := renderBundleDiff(card, ctx)
	if err != nil {
		t.Fatalf("renderBundleDiff: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, `class="bundle-diff-source"`) {
		t.Errorf("want bundle-diff-source row, got %q", s)
	}
	if !strings.Contains(s, `href="https://gitlab.example/g/p/-/merge_requests/42"`) {
		t.Errorf("want MR URL in pill href, got %q", s)
	}
	if !strings.Contains(s, "!42") {
		t.Errorf("want !iid in pill label, got %q", s)
	}
	if !strings.Contains(s, "merged") {
		t.Errorf("want mr_state in pill label, got %q", s)
	}
	// The source row must precede the first file header so a reader sees the
	// linkage above the diff body.
	srcIdx := strings.Index(s, `class="bundle-diff-source"`)
	fileIdx := strings.Index(s, `class="bundle-diff-file"`)
	if srcIdx < 0 || fileIdx < 0 || srcIdx > fileIdx {
		t.Errorf("source row must appear above file list (srcIdx=%d fileIdx=%d)", srcIdx, fileIdx)
	}
}

// TestRenderBundleDiff_SourceMRRow_OmittedWhenNil asserts the renderer omits
// the source row entirely when ctx.SourceMR is unset — bundles without an
// originating MR must render identically to before C5.
func TestRenderBundleDiff_SourceMRRow_OmittedWhenNil(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	card := bundleDocCard{Format: "diff", Content: minimalUnifiedDiff, Language: "go"}
	got, err := renderBundleDiff(card, ctx)
	if err != nil {
		t.Fatalf("renderBundleDiff: %v", err)
	}
	s := string(got)
	if strings.Contains(s, "bundle-diff-source") {
		t.Errorf("want no source row when SourceMR is nil, got %q", s)
	}
}

// TestRenderBundleDiff_SourceMRRow_OmittedWhenEmptyURL asserts the renderer
// degrades gracefully when SourceMR is non-nil but mr_url is missing. We'd
// rather skip the row than emit a dead anchor.
func TestRenderBundleDiff_SourceMRRow_OmittedWhenEmptyURL(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	ctx.SourceMR = &beadsapi.MergeRequestBeadFields{MRIID: "7", MRState: "opened"}
	card := bundleDocCard{Format: "diff", Content: minimalUnifiedDiff, Language: "go"}
	got, err := renderBundleDiff(card, ctx)
	if err != nil {
		t.Fatalf("renderBundleDiff: %v", err)
	}
	if strings.Contains(string(got), "bundle-diff-source") {
		t.Errorf("want no source row when mr_url is empty, got %q", got)
	}
}

// TestRenderBundleDiff_SourceMRRow_EscapesURL asserts the renderer
// HTML-escapes the MR URL — author-supplied state must not leak into the
// surrounding HTML even if a bead field is corrupted.
func TestRenderBundleDiff_SourceMRRow_EscapesURL(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	ctx.SourceMR = &beadsapi.MergeRequestBeadFields{
		MRURL:   `https://x/"><script>alert(1)</script>`,
		MRIID:   "1",
		MRState: "merged",
	}
	card := bundleDocCard{Format: "diff", Content: minimalUnifiedDiff}
	got, err := renderBundleDiff(card, ctx)
	if err != nil {
		t.Fatalf("renderBundleDiff: %v", err)
	}
	s := string(got)
	if strings.Contains(s, "<script>alert(1)</script>") {
		t.Errorf("unescaped script payload leaked into output: %q", s)
	}
}

// TestBuildBundleView_StampsSourceMRFromReverseProducesEdge asserts that
// buildBundleView reverse-walks `produces` deps on the bundle, finds the
// originating MR bead, and populates ctx.SourceMR with the MR's fields.
func TestBuildBundleView_StampsSourceMRFromReverseProducesEdge(t *testing.T) {
	bundle := &beadsapi.BeadDetail{
		ID:    "gd-g0xoawomrs",
		Type:  "bundle",
		Title: "MR-produced diff bundle",
	}
	reverseDeps := []beadsapi.Dependency{
		{BeadID: "gd-doc00000001", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
		// MR bead produces the bundle — the linkage C5 wires up.
		{BeadID: "gd-mr0000000001", DependsOnID: "gd-g0xoawomrs", Type: "produces"},
	}
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-doc00000001": {
			ID:   "gd-doc00000001",
			Type: "doc",
			Fields: map[string]string{
				"format":   "diff",
				"position": "1",
				"headline": "Diff",
				"content":  minimalUnifiedDiff,
			},
		},
		"gd-mr0000000001": {
			ID:   "gd-mr0000000001",
			Type: "merge-request",
			Fields: map[string]string{
				"mr_url":   "https://gitlab.example/g/p/-/merge_requests/9",
				"mr_iid":   "9",
				"mr_state": "merged",
				"mr_title": "Refactor auth",
			},
		},
	}}

	rctx := newTestBundleRenderCtx()
	v := buildBundleView(context.Background(), bundle, nil, reverseDeps, fake, rctx)
	if v == nil {
		t.Fatal("buildBundleView returned nil")
	}
	if rctx.SourceMR == nil {
		t.Fatalf("ctx.SourceMR should be populated from reverse produces edge, got nil")
	}
	if rctx.SourceMR.MRURL != "https://gitlab.example/g/p/-/merge_requests/9" {
		t.Errorf("SourceMR.MRURL=%q, want MR url", rctx.SourceMR.MRURL)
	}
	if rctx.SourceMR.MRIID != "9" {
		t.Errorf("SourceMR.MRIID=%q, want 9", rctx.SourceMR.MRIID)
	}
	if rctx.SourceMR.MRState != "merged" {
		t.Errorf("SourceMR.MRState=%q, want merged", rctx.SourceMR.MRState)
	}
	// The rendered diff card must carry the source pill now that SourceMR
	// is set; tying both ends together prevents future renderer regressions
	// from silently dropping the linkage.
	if len(v.Docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(v.Docs))
	}
	if !strings.Contains(string(v.Docs[0].RenderedHTML), "bundle-diff-source") {
		t.Errorf("rendered diff card missing source pill: %q", v.Docs[0].RenderedHTML)
	}
}

// TestBuildBundleView_NoSourceMRWhenNoProducesEdge asserts that bundles
// without a `produces` reverse edge to a merge-request bead leave
// ctx.SourceMR nil — they must render identically to pre-C5.
func TestBuildBundleView_NoSourceMRWhenNoProducesEdge(t *testing.T) {
	bundle := &beadsapi.BeadDetail{ID: "gd-7mv421ntul", Type: "bundle"}
	reverseDeps := []beadsapi.Dependency{
		{BeadID: "gd-doc00000001", DependsOnID: "gd-7mv421ntul", Type: "parent-child"},
	}
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-doc00000001": {
			ID:     "gd-doc00000001",
			Type:   "doc",
			Fields: map[string]string{"format": "diff", "position": "1", "content": minimalUnifiedDiff},
		},
	}}

	rctx := newTestBundleRenderCtx()
	v := buildBundleView(context.Background(), bundle, nil, reverseDeps, fake, rctx)
	if v == nil {
		t.Fatal("buildBundleView returned nil")
	}
	if rctx.SourceMR != nil {
		t.Errorf("SourceMR should remain nil with no produces edge, got %+v", rctx.SourceMR)
	}
	if strings.Contains(string(v.Docs[0].RenderedHTML), "bundle-diff-source") {
		t.Errorf("diff card must not carry source pill when SourceMR is nil: %q", v.Docs[0].RenderedHTML)
	}
}

// TestBuildBundleView_IgnoresNonMRProducesTarget asserts that a `produces`
// reverse edge from a non-merge-request bead does not populate SourceMR —
// the linkage is MR-specific.
func TestBuildBundleView_IgnoresNonMRProducesTarget(t *testing.T) {
	bundle := &beadsapi.BeadDetail{ID: "gd-0hwiram2qr", Type: "bundle"}
	reverseDeps := []beadsapi.Dependency{
		{BeadID: "gd-task00000001", DependsOnID: "gd-0hwiram2qr", Type: "produces"},
	}
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-task00000001": {ID: "gd-task00000001", Type: "task"},
	}}
	rctx := newTestBundleRenderCtx()
	buildBundleView(context.Background(), bundle, nil, reverseDeps, fake, rctx)
	if rctx.SourceMR != nil {
		t.Errorf("SourceMR should stay nil when produces target is not a merge-request, got %+v", rctx.SourceMR)
	}
}
