package main

import (
	"bytes"
	"context"
	"html/template"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// parseConstellationTemplates parses the constellation partial standalone so
// the tests can render the named blocks directly. We mirror the parse path
// used in newServer() — embed.FS lookup via templateFS — so the embedded
// template source is what's under test.
func parseConstellationTemplates(t *testing.T) *template.Template {
	t.Helper()
	tmpl, err := template.New("").ParseFS(templateFS, "templates/partials/constellation.html")
	if err != nil {
		t.Fatalf("parse constellation template: %v", err)
	}
	return tmpl
}

// TestConstellation_NodeTemplateHasBeadAnchor pins the JS↔template contract:
// the popover JS depends on each orbit node being wrapped in <a href="/gd-...">.
// Stripping the <a> from constellation_node should fail this test.
func TestConstellation_NodeTemplateHasBeadAnchor(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	node := constellationNode{
		ID:     "gd-foo",
		Title:  "Sample bead",
		X:      100,
		Y:      100,
		Radius: 16,
		Role:   "forward",
		Color:  "#a8d4ab",
		Stroke: "#3a8a3f",
		Glyph:  "T",
		Href:   "/gd-foo",
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation_node", node); err != nil {
		t.Fatalf("execute constellation_node: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `href="/gd-foo"`) {
		t.Errorf("constellation_node output missing href=\"/gd-foo\"; popover JS will not fire on SVG nodes.\noutput:\n%s", out)
	}
	if !strings.Contains(out, "<a ") {
		t.Errorf("constellation_node output missing <a> wrapper; popover JS depends on it.\noutput:\n%s", out)
	}
}

// TestConstellation_CenterTemplateHasBeadAnchor pins the center anchor in the
// "constellation" wrapper template — same contract as orbit nodes.
func TestConstellation_CenterTemplateHasBeadAnchor(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	view := struct {
		Constellation *constellationView
	}{
		Constellation: &constellationView{
			Width:  1000,
			Height: 500,
			Center: constellationNode{
				ID:       "gd-foo",
				Title:    "Center bead",
				X:        500,
				Y:        250,
				Radius:   42,
				HaloR:    92,
				GlyphY:   256,
				IDLabelY: 314,
				TopY:     196,
				Role:     "center",
				Color:    "#a8d4ab",
				Stroke:   "#d4a017",
				IsCenter: true,
				Glyph:    "T",
				Href:     "/gd-foo",
			},
			HasReverse:  true, // gate HasAnything
			HasAnything: true,
		},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation", view); err != nil {
		t.Fatalf("execute constellation: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `href="/gd-foo"`) {
		t.Errorf("center anchor missing href=\"/gd-foo\"; popover JS will not fire on center node.\noutput:\n%s", out)
	}
	if !strings.Contains(out, `class="constellation-center"`) {
		t.Errorf("constellation output missing center group; template structure changed.\noutput:\n%s", out)
	}
}

// --- MR diamond + state overlay (ka-0000004604) ---

// TestConstellation_MRNodeRendersAsDiamond pins the type=merge-request branch
// of the constellation_node template: MR nodes must render as an SVG <path>
// diamond (not a <circle>), so they read as distinct from task/bug/decision
// nodes in the constellation. Acceptance ka-0000004604#1.
func TestConstellation_MRNodeRendersAsDiamond(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	node := constellationNode{
		ID:        "gd-mr1",
		Type:      "merge-request",
		X:         120,
		Y:         80,
		Radius:    16,
		Role:      "forward",
		Color:     "#a8d4ab",
		Stroke:    "#3a8a3f",
		Glyph:     "↗",
		Href:      "/gd-mr1",
		IsMR:      true,
		MRState:   "opened",
		MROverlay: "#1d6bbd",
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation_node", node); err != nil {
		t.Fatalf("execute constellation_node: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "<path d=\"M0,-16.0 L 16.0,0 L 0,16.0 L -16.0,0 Z\"") {
		t.Errorf("MR node missing diamond <path>; output:\n%s", out)
	}
	if strings.Contains(out, "<circle cx=\"120.0\" cy=\"80.0\"") {
		t.Errorf("MR node should not emit a body <circle>; output:\n%s", out)
	}
	if !strings.Contains(out, `data-mr-state="opened"`) {
		t.Errorf("MR node missing data-mr-state attr; output:\n%s", out)
	}
	if !strings.Contains(out, "constellation-node-mr") {
		t.Errorf("MR node missing constellation-node-mr class; output:\n%s", out)
	}
}

// TestConstellation_MRNodeOverlayTinted verifies the corner badge fills with
// the state-tinted color (purple/blue/gray) so merged/opened/closed reads at
// a glance. Acceptance ka-0000004604#2.
func TestConstellation_MRNodeOverlayTinted(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	cases := []struct {
		state   string
		overlay string
	}{
		{"merged", "#8a5cf6"},
		{"opened", "#1d6bbd"},
		{"closed", "#777777"},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			node := constellationNode{
				ID:        "gd-mr1",
				Type:      "merge-request",
				X:         100,
				Y:         100,
				Radius:    16,
				Role:      "forward",
				Glyph:     "↗",
				Href:      "/gd-mr1",
				IsMR:      true,
				MRState:   tc.state,
				MROverlay: tc.overlay,
			}
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, "constellation_node", node); err != nil {
				t.Fatalf("execute: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, `fill="`+tc.overlay+`"`) {
				t.Errorf("state=%s: overlay badge missing fill=%q; output:\n%s", tc.state, tc.overlay, out)
			}
		})
	}
}

// TestConstellation_NonMRNodeStaysCircle defends the default render path —
// type=task and friends must remain circles, not regress to the diamond glyph.
func TestConstellation_NonMRNodeStaysCircle(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	node := constellationNode{
		ID:     "gd-task1",
		Type:   "task",
		X:      100,
		Y:      100,
		Radius: 16,
		Role:   "forward",
		Color:  "#a8d4ab",
		Stroke: "#3a8a3f",
		Glyph:  "T",
		Href:   "/gd-task1",
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation_node", node); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `<circle cx="100.0" cy="100.0"`) {
		t.Errorf("non-MR node missing <circle>; output:\n%s", out)
	}
	if strings.Contains(out, "<path d=\"M0,-") {
		t.Errorf("non-MR node should not emit a diamond <path>; output:\n%s", out)
	}
}

// TestApplyMRState_HappyPath verifies the helper flips IsMR/MRState/MROverlay
// only for type=merge-request beads and reads state via mr_state with `state`
// fallback (mirrors bead_preview / inbox-chip lookups).
func TestApplyMRState_HappyPath(t *testing.T) {
	cases := []struct {
		name        string
		beadType    string
		fields      map[string]string
		wantIsMR    bool
		wantState   string
		wantOverlay string
	}{
		{"task-untouched", "task", map[string]string{"mr_state": "merged"}, false, "", ""},
		{"mr-merged", "merge-request", map[string]string{"mr_state": "merged"}, true, "merged", "#8a5cf6"},
		{"mr-opened", "merge-request", map[string]string{"mr_state": "opened"}, true, "opened", "#1d6bbd"},
		{"mr-closed", "merge-request", map[string]string{"mr_state": "closed"}, true, "closed", "#777777"},
		{"mr-state-alias", "merge-request", map[string]string{"state": "merged"}, true, "merged", "#8a5cf6"},
		{"mr-unknown-state", "merge-request", map[string]string{}, true, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := constellationNode{Type: tc.beadType}
			applyMRState(&n, tc.beadType, tc.fields)
			if n.IsMR != tc.wantIsMR {
				t.Errorf("IsMR = %v; want %v", n.IsMR, tc.wantIsMR)
			}
			if n.MRState != tc.wantState {
				t.Errorf("MRState = %q; want %q", n.MRState, tc.wantState)
			}
			if n.MROverlay != tc.wantOverlay {
				t.Errorf("MROverlay = %q; want %q", n.MROverlay, tc.wantOverlay)
			}
		})
	}
}

// TestApplyMRState_ExternalMirror is the E-PROV FW-5 viewer GATE
// (ka-0000003146): an MR bead carrying external_mirror=true (or its upstream
// alias) flips IsExternal so the constellation renders it as an external
// (dashed-stroke) diamond — the read-only upstream-MR mirror. A plain dark MR
// (no marker) must NOT be flagged external.
func TestApplyMRState_ExternalMirror(t *testing.T) {
	cases := []struct {
		name           string
		beadType       string
		fields         map[string]string
		wantIsExternal bool
	}{
		{"dark-mr-not-external", "merge-request", map[string]string{"mr_state": "opened"}, false},
		{"external-mirror-field", "merge-request", map[string]string{"mr_state": "opened", "external_mirror": "true"}, true},
		{"upstream-alias-field", "merge-request", map[string]string{"mr_state": "merged", "upstream": "true"}, true},
		{"task-with-external-field-untouched", "task", map[string]string{"external_mirror": "true"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := constellationNode{Type: tc.beadType}
			applyMRState(&n, tc.beadType, tc.fields)
			if n.IsExternal != tc.wantIsExternal {
				t.Errorf("IsExternal = %v; want %v", n.IsExternal, tc.wantIsExternal)
			}
			// IsExternal only ever rides on an MR node.
			if n.IsExternal && !n.IsMR {
				t.Errorf("IsExternal set on a non-MR node (IsMR=%v)", n.IsMR)
			}
		})
	}
}

// TestConstellation_LegendIncludesMREntries verifies the legend explains the
// shape + overlay color for MR nodes. Acceptance ka-0000004604#3.
func TestConstellation_LegendIncludesMREntries(t *testing.T) {
	items := constellationLegendItems()
	want := map[string]bool{
		"MR (merged)": false,
		"MR (opened)": false,
		"MR (closed)": false,
	}
	for _, it := range items {
		if _, ok := want[it.Label]; ok {
			if it.Shape != "mr" {
				t.Errorf("legend %q Shape = %q; want \"mr\"", it.Label, it.Shape)
			}
			if it.Overlay == "" {
				t.Errorf("legend %q missing Overlay color", it.Label)
			}
			want[it.Label] = true
		}
	}
	for label, ok := range want {
		if !ok {
			t.Errorf("constellation legend missing entry %q", label)
		}
	}
}

// TestConstellation_RespectsBasePath verifies the templates emit the Href
// field verbatim, so a non-empty BasePath (e.g. "/v") flows through to the
// rendered <a href="/v/gd-...">. This is the path the JS popover uses to
// extract the bead ID for /b/{id}/preview.
func TestConstellation_RespectsBasePath(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	node := constellationNode{
		ID:     "gd-foo",
		X:      100,
		Y:      100,
		Radius: 16,
		Role:   "reverse",
		Color:  "#a8d4ab",
		Stroke: "#3a8a3f",
		Glyph:  "T",
		Href:   "/v/gd-foo",
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation_node", node); err != nil {
		t.Fatalf("execute constellation_node: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, `href="/v/gd-foo"`) {
		t.Errorf("constellation_node did not honor BasePath in Href; want href=\"/v/gd-foo\".\noutput:\n%s", got)
	}
}

// --- Age-based opacity (gd-OeoTbDki6b / parent epic gd-tLuqh07lS7) ---

// TestComputeAgeOpacity_NewestIsOne verifies the newest timestamp returns 1.0.
func TestComputeAgeOpacity_NewestIsOne(t *testing.T) {
	oldest := time.Date(2025, 10, 15, 0, 0, 0, 0, time.UTC)
	newest := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	got := computeAgeOpacity(newest, oldest, newest, 0.30)
	if got != 1.0 {
		t.Errorf("newest opacity = %v; want 1.0", got)
	}
}

// TestComputeAgeOpacity_OldestIsFloor verifies the oldest timestamp returns the floor.
func TestComputeAgeOpacity_OldestIsFloor(t *testing.T) {
	oldest := time.Date(2025, 10, 15, 0, 0, 0, 0, time.UTC)
	newest := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	got := computeAgeOpacity(oldest, oldest, newest, 0.30)
	if math.Abs(got-0.30) > 1e-9 {
		t.Errorf("oldest opacity = %v; want 0.30", got)
	}
}

// TestComputeAgeOpacity_MiddleBetween verifies a node halfway through the
// log-age range falls strictly between floor and 1.0.
func TestComputeAgeOpacity_MiddleBetween(t *testing.T) {
	oldest := time.Date(2025, 10, 15, 0, 0, 0, 0, time.UTC)
	newest := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	mid := newest.Add(-newest.Sub(oldest) / 2)
	got := computeAgeOpacity(mid, oldest, newest, 0.30)
	if got <= 0.30 || got >= 1.0 {
		t.Errorf("midway opacity = %v; want in (0.30, 1.0)", got)
	}
}

// TestComputeAgeOpacity_DegenerateRange verifies oldest==newest returns 1.0.
func TestComputeAgeOpacity_DegenerateRange(t *testing.T) {
	t0 := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	got := computeAgeOpacity(t0, t0, t0, 0.30)
	if got != 1.0 {
		t.Errorf("degenerate range opacity = %v; want 1.0", got)
	}
}

// TestComputeAgeOpacity_ZeroTimestamp verifies a zero ts returns 1.0.
func TestComputeAgeOpacity_ZeroTimestamp(t *testing.T) {
	oldest := time.Date(2025, 10, 15, 0, 0, 0, 0, time.UTC)
	newest := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	got := computeAgeOpacity(time.Time{}, oldest, newest, 0.30)
	if got != 1.0 {
		t.Errorf("zero-ts opacity = %v; want 1.0", got)
	}
}

// TestComputeAgeOpacity_StrictlyDecreasing verifies older nodes get lower
// opacity all the way down to the floor.
func TestComputeAgeOpacity_StrictlyDecreasing(t *testing.T) {
	oldest := time.Date(2025, 10, 15, 0, 0, 0, 0, time.UTC)
	newest := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	span := newest.Sub(oldest)
	a := computeAgeOpacity(newest.Add(-span/10), oldest, newest, 0.30)
	b := computeAgeOpacity(newest.Add(-span/2), oldest, newest, 0.30)
	c := computeAgeOpacity(newest.Add(-9*span/10), oldest, newest, 0.30)
	if !(a > b && b > c) {
		t.Errorf("expected strictly decreasing opacity newest→oldest; got a=%v b=%v c=%v", a, b, c)
	}
	if c < 0.30-1e-6 || a > 1.0+1e-6 {
		t.Errorf("opacity out of band [0.30,1.0]; a=%v c=%v", a, c)
	}
}

// TestApplyAgeOpacity_CenterStaysOne verifies the center node always renders
// at full opacity regardless of its age.
func TestApplyAgeOpacity_CenterStaysOne(t *testing.T) {
	oldTS := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newTS := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	view := &constellationView{
		Center: constellationNode{ID: "gd-center"},
		Forward: []constellationNode{
			{ID: "gd-old"},
			{ID: "gd-new"},
		},
	}
	hydrated := map[string]*beadsapi.BeadDetail{
		"gd-old": {ID: "gd-old", UpdatedAt: oldTS},
		"gd-new": {ID: "gd-new", UpdatedAt: newTS},
	}
	center := &beadsapi.BeadDetail{ID: "gd-center", UpdatedAt: oldTS}
	applyAgeOpacity(view, center, hydrated, nil)
	if view.Center.Opacity != 1.0 {
		t.Errorf("center opacity = %v; want 1.0 regardless of age", view.Center.Opacity)
	}
	if view.Forward[1].Opacity != 1.0 {
		t.Errorf("newest forward opacity = %v; want 1.0", view.Forward[1].Opacity)
	}
	if math.Abs(view.Forward[0].Opacity-ageOpacityFloor) > 1e-9 {
		t.Errorf("oldest forward opacity = %v; want floor %v", view.Forward[0].Opacity, ageOpacityFloor)
	}
}

// TestApplyAgeOpacity_EmptyDoesNotPanic verifies the helper handles a view
// with no orbit nodes (empty constellation) and one with only one node.
func TestApplyAgeOpacity_EmptyDoesNotPanic(t *testing.T) {
	view := &constellationView{Center: constellationNode{ID: "gd-only"}}
	applyAgeOpacity(view, &beadsapi.BeadDetail{ID: "gd-only"}, nil, nil)
	if view.Center.Opacity != 1.0 {
		t.Errorf("empty-constellation center opacity = %v; want 1.0", view.Center.Opacity)
	}

	view2 := &constellationView{
		Center:  constellationNode{ID: "gd-c"},
		Forward: []constellationNode{{ID: "gd-only-orbit"}},
	}
	hydrated := map[string]*beadsapi.BeadDetail{
		"gd-only-orbit": {ID: "gd-only-orbit", UpdatedAt: time.Now()},
	}
	applyAgeOpacity(view2, nil, hydrated, nil)
	if view2.Forward[0].Opacity != 1.0 {
		t.Errorf("single-orbit-node opacity = %v; want 1.0 (no spread)", view2.Forward[0].Opacity)
	}
}

// TestApplyAgeOpacity_NoTimestampsKeepOne verifies nodes with no
// hydrated/known timestamp get Opacity = 1.0 (template never emits an empty
// fill-opacity attribute).
func TestApplyAgeOpacity_NoTimestampsKeepOne(t *testing.T) {
	view := &constellationView{
		Center:  constellationNode{ID: "gd-c"},
		Forward: []constellationNode{{ID: "gd-unknown1"}, {ID: "gd-unknown2"}},
	}
	applyAgeOpacity(view, nil, map[string]*beadsapi.BeadDetail{}, nil)
	for i, n := range view.Forward {
		if n.Opacity != 1.0 {
			t.Errorf("Forward[%d].Opacity = %v; want 1.0 (no timestamp data)", i, n.Opacity)
		}
	}
}

// --- Typed/untyped-mention edge tagging (ka-0000004349) ---

// TestMergeTypedMentions_ForwardOnlyTyped verifies that mergeTypedMentionsAsDepEdges
// only folds typed mentions (RelationshipType != nil) into the forward edge list.
// Untyped mentions are appended separately by appendUntypedMentionEdges.
func TestMergeTypedMentions_ForwardOnlyTyped(t *testing.T) {
	parent := "parent-child"
	mentions := []beadsapi.BeadReferenceSummary{
		{ID: "gd-qcmoiejzjo", SourceField: "description", RelationshipType: &parent},
		{ID: "gd-17bscqb8mt", SourceField: "description"},
	}
	got := mergeTypedMentionsAsDepEdges(nil, "gd-vnudveh8ps", mentions, true)
	if len(got) != 1 {
		t.Fatalf("expected 1 typed-only synthetic edge, got %d: %+v", len(got), got)
	}
	if got[0].DependsOnID != "gd-qcmoiejzjo" || got[0].BeadID != "gd-vnudveh8ps" || got[0].Type != "parent-child" || got[0].Origin != "text" {
		t.Errorf("forward typed dep wrong shape: %+v", got[0])
	}
}

// TestMergeTypedMentions_ReverseDirection verifies reverse-direction typed
// mentions (mentionedBy: X mentions self) populate BeadID = X / DependsOnID
// = self so the taggedTargets(forward=false) path picks them up.
func TestMergeTypedMentions_ReverseDirection(t *testing.T) {
	blocks := "blocks"
	mentions := []beadsapi.BeadReferenceSummary{
		{ID: "gd-jdo6duberu", SourceField: "description", RelationshipType: &blocks},
	}
	got := mergeTypedMentionsAsDepEdges(nil, "gd-vnudveh8ps", mentions, false)
	if len(got) != 1 {
		t.Fatalf("expected 1 reverse typed edge, got %d", len(got))
	}
	if got[0].BeadID != "gd-jdo6duberu" || got[0].DependsOnID != "gd-vnudveh8ps" || got[0].Origin != "text" {
		t.Errorf("reverse typed dep wrong shape: %+v", got[0])
	}
}

// TestMergeTypedMentions_DedupesAgainstExistingDeps verifies a typed mention
// to a bead that is ALREADY a deps-table dep does not double up — the
// existing dep edge wins (it carries first-class deps-table provenance).
func TestMergeTypedMentions_DedupesAgainstExistingDeps(t *testing.T) {
	parent := "parent-child"
	existing := tagExistingDeps([]beadsapi.Dependency{
		{BeadID: "gd-vnudveh8ps", DependsOnID: "gd-c3u76usvml", Type: "parent-child"},
	})
	mentions := []beadsapi.BeadReferenceSummary{
		{ID: "gd-c3u76usvml", SourceField: "description", RelationshipType: &parent},
		{ID: "gd-berameq1lq", SourceField: "description", RelationshipType: &parent},
	}
	got := mergeTypedMentionsAsDepEdges(existing, "gd-vnudveh8ps", mentions, true)
	if len(got) != 2 {
		t.Fatalf("expected 1 existing + 1 new (dedup applied), got %d: %+v", len(got), got)
	}
	if got[0].Origin != "dep" {
		t.Errorf("expected first edge to retain dep origin, got %+v", got[0])
	}
	if got[1].DependsOnID != "gd-berameq1lq" || got[1].Origin != "text" {
		t.Errorf("expected non-duplicate text mention to remain, got %+v", got[1])
	}
}

// TestMergeTypedMentions_DropsSelfReference verifies a typed mention pointing
// at the same bead (a self-reference in prose) does not produce a self-loop.
func TestMergeTypedMentions_DropsSelfReference(t *testing.T) {
	parent := "parent-child"
	mentions := []beadsapi.BeadReferenceSummary{
		{ID: "gd-vnudveh8ps", SourceField: "description", RelationshipType: &parent},
	}
	got := mergeTypedMentionsAsDepEdges(nil, "gd-vnudveh8ps", mentions, true)
	if len(got) != 0 {
		t.Errorf("expected self-reference dropped, got %+v", got)
	}
}

// TestAppendUntypedMentionEdges_ForwardOnlyUntyped verifies untyped mentions
// (RelationshipType == nil) become Origin="mention" edges; typed mentions
// are skipped (they belong to mergeTypedMentionsAsDepEdges).
func TestAppendUntypedMentionEdges_ForwardOnlyUntyped(t *testing.T) {
	parent := "parent-child"
	mentions := []beadsapi.BeadReferenceSummary{
		{ID: "gd-qcmoiejzjo", SourceField: "description", RelationshipType: &parent},
		{ID: "gd-17bscqb8mt", SourceField: "description"},
	}
	got := appendUntypedMentionEdges(nil, "gd-vnudveh8ps", mentions, true)
	if len(got) != 1 {
		t.Fatalf("expected 1 untyped-only edge, got %d: %+v", len(got), got)
	}
	if got[0].DependsOnID != "gd-17bscqb8mt" || got[0].Origin != "mention" || got[0].Type != "" {
		t.Errorf("untyped edge wrong shape: %+v", got[0])
	}
}

// TestAppendUntypedMentionEdges_DedupesAgainstDepAndText verifies untyped
// mentions to targets already represented as a dep or typed mention are dropped.
func TestAppendUntypedMentionEdges_DedupesAgainstDepAndText(t *testing.T) {
	existing := []taggedDep{
		{BeadID: "gd-vnudveh8ps", DependsOnID: "gd-t7dgqowlqn", Type: "blocks", Origin: "dep"},
		{BeadID: "gd-vnudveh8ps", DependsOnID: "gd-i4bev3kkmv", Type: "related", Origin: "text"},
	}
	mentions := []beadsapi.BeadReferenceSummary{
		{ID: "gd-t7dgqowlqn", SourceField: "description"},
		{ID: "gd-i4bev3kkmv", SourceField: "notes"},
		{ID: "gd-asn7ngwqls", SourceField: "description"},
	}
	got := appendUntypedMentionEdges(existing, "gd-vnudveh8ps", mentions, true)
	if len(got) != 3 {
		t.Fatalf("expected 2 existing + 1 new untyped, got %d: %+v", len(got), got)
	}
	if got[2].DependsOnID != "gd-asn7ngwqls" || got[2].Origin != "mention" {
		t.Errorf("expected fresh untyped to remain, got %+v", got[2])
	}
}

// --- Edge styling per origin (ka-0000004349) ---

// TestEdgeStyle_DepSolidDirectionColor pins the dep-edge palette: forward
// uses #3b6fa0, reverse uses #5d8a3a, both solid at 0.85.
func TestEdgeStyle_DepSolidDirectionColor(t *testing.T) {
	stroke, dash, op := edgeStyle("forward", "dep")
	if stroke != "#3b6fa0" || dash != "" || op != 0.85 {
		t.Errorf("forward dep style = (%q,%q,%v); want (#3b6fa0,'',0.85)", stroke, dash, op)
	}
	stroke, dash, op = edgeStyle("reverse", "dep")
	if stroke != "#5d8a3a" || dash != "" || op != 0.85 {
		t.Errorf("reverse dep style = (%q,%q,%v); want (#5d8a3a,'',0.85)", stroke, dash, op)
	}
}

// TestEdgeStyle_TextDashed verifies typed-mention edges keep the direction
// color but render with a 6 2 dash to mark text-derived provenance.
func TestEdgeStyle_TextDashed(t *testing.T) {
	stroke, dash, op := edgeStyle("forward", "text")
	if stroke != "#3b6fa0" || dash != "6 2" || op != 0.85 {
		t.Errorf("forward text style = (%q,%q,%v); want (#3b6fa0,'6 2',0.85)", stroke, dash, op)
	}
}

// TestEdgeStyle_MentionGray verifies untyped-mention edges render gray,
// dashed 3 4, at 0.55 opacity, regardless of direction.
func TestEdgeStyle_MentionGray(t *testing.T) {
	for _, role := range []string{"forward", "reverse"} {
		stroke, dash, op := edgeStyle(role, "mention")
		if stroke != "#7a7a7a" || dash != "3 4" || op != 0.55 {
			t.Errorf("%s mention style = (%q,%q,%v); want (#7a7a7a,'3 4',0.55)", role, stroke, dash, op)
		}
	}
}

// TestEdgeTitle pins the SVG <title> tooltip strings — operators rely on
// "(text)" suffix to distinguish typed mentions from explicit deps without
// reading the dash pattern.
func TestEdgeTitle(t *testing.T) {
	cases := []struct {
		role, origin, verb, want string
	}{
		{"forward", "dep", "blocks", "blocks"},
		{"forward", "dep", "", "depends on"},
		{"reverse", "dep", "", "depended on by"},
		{"forward", "text", "blocks", "blocks (text)"},
		{"forward", "text", "", "mention (text)"},
		{"forward", "mention", "", "mentions (text)"},
	}
	for _, c := range cases {
		got := edgeTitle(c.role, c.origin, c.verb)
		if got != c.want {
			t.Errorf("edgeTitle(%q,%q,%q) = %q; want %q", c.role, c.origin, c.verb, got, c.want)
		}
	}
}

// --- Edge SVG rendering (ka-0000004349) ---

// TestConstellation_EdgeRendersDataAttributes verifies the edge template emits
// data-origin / data-relationship-type / data-source-bead / data-target-bead
// and a <title> tooltip — these are the contract E2 (clickable edges) reads.
func TestConstellation_EdgeRendersDataAttributes(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	view := constellationFixture()
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation", map[string]any{"Constellation": view}); err != nil {
		t.Fatalf("execute constellation: %v", err)
	}
	out := buf.String()
	wants := []string{
		`data-origin="dep"`,
		`data-origin="text"`,
		`data-origin="mention"`,
		`data-relationship-type="blocks"`,
		`data-source-bead="gd-vnudveh8ps"`,
		`data-target-bead="gd-t7dgqowlqn"`,
		`data-target-bead="gd-i4bev3kkmv"`,
		`data-target-bead="gd-mention0001"`,
		`<title>blocks</title>`,
		`<title>parent-child (text)</title>`,
		`<title>mentions (text)</title>`,
		`stroke-dasharray="6 2"`, // typed-text edge dash
		`stroke-dasharray="3 4"`, // untyped-mention edge dash
		`#7a7a7a`,                // untyped-mention gray
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("constellation output missing %q\noutput:\n%s", w, out)
		}
	}
}

// TestConstellation_LegendIncludesMentionEntries pins the new legend rows so
// a future legend rewrite that drops the typed/untyped lines fails this test.
func TestConstellation_LegendIncludesMentionEntries(t *testing.T) {
	items := constellationLegendItems()
	want := map[string]bool{
		"typed mention (text)": false,
		"untyped mention":      false,
	}
	for _, it := range items {
		if _, ok := want[it.Label]; ok {
			want[it.Label] = true
		}
	}
	for label, found := range want {
		if !found {
			t.Errorf("legend missing entry %q; got %+v", label, items)
		}
	}
}

// TestConstellation_FigureCarriesBeadID pins the data-bead-id attribute on
// the figure so the toggle JS can scope writes to a specific constellation.
func TestConstellation_FigureCarriesBeadID(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	view := constellationFixture()
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation", map[string]any{"Constellation": view}); err != nil {
		t.Fatalf("execute constellation: %v", err)
	}
	if !strings.Contains(buf.String(), `data-bead-id="gd-vnudveh8ps"`) {
		t.Errorf("figure missing data-bead-id; toggle JS will not scope correctly")
	}
}

// TestConstellation_HideUntypedToggleRendered pins the checkbox UI so the
// toggle remains discoverable to users who find the gray edges noisy.
func TestConstellation_HideUntypedToggleRendered(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	view := constellationFixture()
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation", map[string]any{"Constellation": view}); err != nil {
		t.Fatalf("execute constellation: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `class="constellation-hide-untyped"`) {
		t.Errorf("hide-untyped checkbox missing from figcaption\noutput:\n%s", out)
	}
}

// TestConstellation_ShowClosedAgentsToggle pins the ka-0000005724 opt-in
// checkbox: hidden by default, checked when ShowClosedAgents is true.
func TestConstellation_ShowClosedAgentsToggle(t *testing.T) {
	tmpl := parseConstellationTemplates(t)

	// Default (filter active, box unchecked).
	view := constellationFixture()
	view.ShowClosedAgents = false
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation", map[string]any{"Constellation": view}); err != nil {
		t.Fatalf("execute constellation: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `class="constellation-show-closed-agents"`) {
		t.Errorf("show-closed-agents checkbox missing from figcaption\noutput:\n%s", out)
	}
	// Default render: unchecked. Match attribute presence rather than
	// exact whitespace — html/template controls how the {{if}} block
	// emits the attribute.
	if strings.Contains(out, `constellation-show-closed-agents" checked`) {
		t.Errorf("default render should not pre-check the show-closed-agents box\noutput:\n%s", out)
	}

	// Opt-in (box checked).
	view.ShowClosedAgents = true
	buf.Reset()
	if err := tmpl.ExecuteTemplate(&buf, "constellation", map[string]any{"Constellation": view}); err != nil {
		t.Fatalf("execute constellation (opt-in): %v", err)
	}
	out = buf.String()
	if !strings.Contains(out, `class="constellation-show-closed-agents"`) {
		t.Errorf("show-closed-agents checkbox missing from figcaption (opt-in)\noutput:\n%s", out)
	}
	if !strings.Contains(out, "checked") {
		t.Errorf("opt-in render should emit checked attribute on the show-closed-agents box\noutput:\n%s", out)
	}
}

// constellationFixture builds a view containing one of each edge origin so
// the template tests can pin all three render paths in a single render.
func constellationFixture() *constellationView {
	cx, cy := 500.0, 250.0
	view := &constellationView{
		Width:  1000,
		Height: 500,
		Center: constellationNode{
			ID:       "gd-vnudveh8ps",
			Title:    "Center",
			X:        cx,
			Y:        cy,
			Radius:   42,
			HaloR:    92,
			GlyphY:   cy + 6,
			IDLabelY: cy + 64,
			TopY:     cy - 54,
			Role:     "center",
			Color:    "#a8d4ab",
			Stroke:   "#d4a017",
			IsCenter: true,
			Glyph:    "T",
			Href:     "/gd-vnudveh8ps",
			Opacity:  1.0,
		},
		HasForward:  true,
		HasReverse:  false,
		HasSimilar:  false,
		HasAnything: true,
	}
	add := func(id string, x, y float64, role string, td taggedDep) {
		node := constellationNode{
			ID: id, X: x, Y: y, Radius: 16,
			GlyphY: y + 4, IDLabelY: y + 30,
			Role: role, Color: "#a8d4ab", Stroke: "#3a8a3f",
			Glyph: "T", Href: "/" + id, Opacity: 1.0,
		}
		view.Forward = append(view.Forward, node)
		view.Edges = append(view.Edges, mentionEdge(cx, cy, x, y, role, "gd-vnudveh8ps", id, td))
	}
	add("gd-t7dgqowlqn", 380, 90, "forward", taggedDep{BeadID: "gd-vnudveh8ps", DependsOnID: "gd-t7dgqowlqn", Type: "blocks", Origin: "dep"})
	add("gd-i4bev3kkmv", 500, 90, "forward", taggedDep{BeadID: "gd-vnudveh8ps", DependsOnID: "gd-i4bev3kkmv", Type: "parent-child", Origin: "text"})
	add("gd-mention0001", 620, 90, "forward", taggedDep{BeadID: "gd-vnudveh8ps", DependsOnID: "gd-mention0001", Origin: "mention"})
	view.Legend = constellationLegendItems()
	return view
}

// TestTaggedTargets_TruncateKeepsLookupAligned verifies that when more
// targets exist than `limit`, the returned ID slice and the lookup map are
// truncated together so callers can't read a Origin for a dropped ID.
func TestTaggedTargets_TruncateKeepsLookupAligned(t *testing.T) {
	rows := []taggedDep{
		{BeadID: "self", DependsOnID: "gd-aaaa", Origin: "dep"},
		{BeadID: "self", DependsOnID: "gd-bbbb", Origin: "text"},
		{BeadID: "self", DependsOnID: "gd-cccc", Origin: "mention"},
	}
	ids, lookup := taggedTargets(rows, true, 2)
	if len(ids) != 2 {
		t.Fatalf("ids len = %d; want 2", len(ids))
	}
	for _, id := range ids {
		if _, ok := lookup[id]; !ok {
			t.Errorf("lookup missing surviving id %q", id)
		}
	}
	if len(lookup) != 2 {
		t.Errorf("lookup len = %d; want 2 (no orphan entries)", len(lookup))
	}
}

// TestConstellation_NodeTemplateEmitsFillOpacity verifies the orbit-node
// template renders fill-opacity from the Opacity field. This pins the
// template↔model contract so a future refactor that drops the attribute
// from the SVG fails the test instead of silently un-aging the constellation.
func TestConstellation_NodeTemplateEmitsFillOpacity(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	node := constellationNode{
		ID: "gd-foo", X: 100, Y: 100, Radius: 16,
		Role: "forward", Color: "#a8d4ab", Stroke: "#3a8a3f", Glyph: "T",
		Href: "/gd-foo", Opacity: 0.42,
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation_node", node); err != nil {
		t.Fatalf("execute constellation_node: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `fill-opacity="0.42"`) {
		t.Errorf("constellation_node missing fill-opacity=\"0.42\"; age fade will not render.\noutput:\n%s", out)
	}
}

// --- Tree-level slider (ka-0000000739) ---

// TestParseTreeLevelParam pins the URL→slider plumbing: empty/garbage falls
// back to constellationDefaultTreeLevel (3), valid integers clamp to
// [1, constellationMaxTreeLevel].
func TestParseTreeLevelParam(t *testing.T) {
	cases := map[string]int{
		"":      constellationDefaultTreeLevel,
		"  ":    constellationDefaultTreeLevel,
		"0":     1,
		"-3":    1,
		"abc":   constellationDefaultTreeLevel,
		"1":     1,
		"2":     2,
		"3":     3,
		"99":    constellationMaxTreeLevel,
		"  2  ": 2,
	}
	for in, want := range cases {
		if got := parseTreeLevelParam(in); got != want {
			t.Errorf("parseTreeLevelParam(%q) = %d; want %d", in, got, want)
		}
	}
}

// TestConstellationLayout pins the per-level viewBox so the SVG keeps the
// outer-ring nodes on-canvas — drift here makes deeper levels render off-screen.
func TestConstellationLayout(t *testing.T) {
	if w, h, _, _, radii := constellationLayout(1); w != 1000 || h != 500 || len(radii) != 1 {
		t.Errorf("level 1 layout drifted: w=%d h=%d radii=%v", w, h, radii)
	}
	if w, h, _, _, radii := constellationLayout(2); w != 1200 || h != 800 || len(radii) != 2 {
		t.Errorf("level 2 layout drifted: w=%d h=%d radii=%v", w, h, radii)
	}
	if w, h, _, _, radii := constellationLayout(3); w != 1400 || h != 1100 || len(radii) != 3 {
		t.Errorf("level 3 layout drifted: w=%d h=%d radii=%v", w, h, radii)
	}
	// Out-of-range clamps to max.
	if w, _, _, _, _ := constellationLayout(99); w != 1400 {
		t.Errorf("level 99 should clamp to level 3; got width=%d", w)
	}
}

// TestConstellation_TreeLevelRendersSlider verifies the figcaption renders the
// slider control with min/max/value attributes that match the active level.
func TestConstellation_TreeLevelRendersSlider(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	view := struct{ Constellation *constellationView }{
		Constellation: &constellationView{
			Width: 1000, Height: 500,
			Center:       constellationNode{ID: "gd-c", Href: "/gd-c"},
			Forward:      []constellationNode{{ID: "gd-fwd", Href: "/gd-fwd"}},
			HasForward:   true,
			HasAnything:  true,
			TreeLevel:    2,
			MaxTreeLevel: constellationMaxTreeLevel,
		},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation", view); err != nil {
		t.Fatalf("execute constellation: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `class="constellation-tree-level-input"`) {
		t.Errorf("missing tree-level slider input.\n%s", out)
	}
	if !strings.Contains(out, `value="2"`) {
		t.Errorf("slider value not rendered (expected value=\"2\").\n%s", out)
	}
	if !strings.Contains(out, `data-tree-level="2"`) {
		t.Errorf("figure missing data-tree-level=\"2\" hook for CSS.\n%s", out)
	}
}

// TestConstellation_OuterNodesRendered verifies Outer nodes flow through the
// node-template loop alongside Forward/Reverse/Similar.
func TestConstellation_OuterNodesRendered(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	view := struct{ Constellation *constellationView }{
		Constellation: &constellationView{
			Width: 1200, Height: 800,
			Center: constellationNode{ID: "gd-c", Href: "/gd-c"},
			Outer: []constellationNode{
				{ID: "gd-grandparent", X: 600, Y: 100, Radius: 14, Role: "outer-forward", Color: "#a8d4ab", Stroke: "#3a8a3f", Glyph: "T", Href: "/gd-grandparent"},
			},
			HasOuter:     true,
			HasAnything:  true,
			TreeLevel:    2,
			MaxTreeLevel: constellationMaxTreeLevel,
		},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation", view); err != nil {
		t.Fatalf("execute constellation: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `href="/gd-grandparent"`) {
		t.Errorf("outer node not rendered in SVG.\n%s", out)
	}
	if !strings.Contains(out, `1 transitive`) {
		t.Errorf("figcaption missing transitive count.\n%s", out)
	}
}

// TestConstellation_ExpandOuterBatchesDeps confirms the expandLevel inner
// loop fires exactly one BatchGetDependencies call per outer level per
// direction — not N calls per parent in the frontier. This is the
// regression guard for the perf refactor described in ka-0000008009: at
// constellation treeLevel=3 with a wide first-level frontier the per-parent
// fetch was the dominant daemon load on the bead-detail render.
//
// The fake seeds enough fan-out at level 1 that a regression to per-parent
// GetDependencies would push batchGetDepsCalls well above 4 (treeLevel=3 →
// 2 outer levels × 2 directions = 4).
func TestConstellation_ExpandOuterBatchesDeps(t *testing.T) {
	fake := newFake()
	if fake.revDeps == nil {
		fake.revDeps = map[string][]beadsapi.Dependency{}
	}
	// Center bead with three first-level forward parents and three reverse
	// children — wide enough that a regression to one fetch-per-parent would
	// be obvious in the call count.
	fake.beads["gd-center"] = &beadsapi.BeadDetail{ID: "gd-center", Type: "task", Status: "open", Title: "Center"}
	fwd := []beadsapi.Dependency{
		{BeadID: "gd-center", DependsOnID: "gd-fwd1", Type: "parent-child"},
		{BeadID: "gd-center", DependsOnID: "gd-fwd2", Type: "parent-child"},
		{BeadID: "gd-center", DependsOnID: "gd-fwd3", Type: "parent-child"},
	}
	rev := []beadsapi.Dependency{
		{BeadID: "gd-rev1", DependsOnID: "gd-center", Type: "blocks"},
		{BeadID: "gd-rev2", DependsOnID: "gd-center", Type: "blocks"},
		{BeadID: "gd-rev3", DependsOnID: "gd-center", Type: "blocks"},
	}
	// Hydrate the level-1 nodes so they actually render and become the
	// frontier for the level-2 batched fetch below.
	for i := 1; i <= 3; i++ {
		idF := "gd-fwd" + string(rune('0'+i))
		idR := "gd-rev" + string(rune('0'+i))
		fake.beads[idF] = &beadsapi.BeadDetail{ID: idF, Type: "task", Status: "open"}
		fake.beads[idR] = &beadsapi.BeadDetail{ID: idR, Type: "task", Status: "open"}
		// Each frontier parent gets its own outgoing forward dep and incoming
		// reverse dep so the batched call at level 2 actually has rows to
		// fan out (otherwise the inner loop short-circuits before exercising
		// the batch path's "found rows" branch).
		fake.deps[idF] = []beadsapi.Dependency{
			{BeadID: idF, DependsOnID: idF + "-p", Type: "parent-child"},
		}
		fake.revDeps[idR] = []beadsapi.Dependency{
			{BeadID: idR + "-c", DependsOnID: idR, Type: "blocks"},
		}
	}

	srv := NewServer(fake, slog.Default(), "", "", "", nil)
	view := srv.buildConstellation(context.Background(),
		fake.beads["gd-center"], fwd, rev,
		nil, nil, nil,
		3, /* treeLevel */
		true /* showClosedAgents */)
	if view == nil {
		t.Fatal("constellation view nil")
	}
	// treeLevel=3 means the outer-expansion loop runs for level=1 and
	// level=2 — two iterations × two directions = exactly 4 batched fetches.
	// A regression to per-parent GetDependencies would push the call count
	// far above this floor.
	if fake.batchGetDepsCalls != 4 {
		t.Errorf("expected 4 BatchGetDependencies calls (one per direction per outer level), got %d",
			fake.batchGetDepsCalls)
	}
}

// TestConstellation_SessionBeadsHiddenEvenWithShowClosed pins ka-0000000532 on
// the bead-show surface: a session bead in the deps is dropped from the orbit
// nodes even when the user has opted into show_closed_agents=true (which only
// re-admits closed AGENT beads, not session bookkeeping).
func TestConstellation_SessionBeadsHiddenEvenWithShowClosed(t *testing.T) {
	fake := newFake()
	fake.beads["gd-center"] = &beadsapi.BeadDetail{ID: "gd-center", Type: "task", Status: "open", Title: "Center"}
	fake.beads["gd-sess"] = &beadsapi.BeadDetail{ID: "gd-sess", Type: "session", Status: "open"}
	fake.beads["gd-agent"] = &beadsapi.BeadDetail{ID: "gd-agent", Type: "agent", Status: "closed"}
	fake.beads["gd-task"] = &beadsapi.BeadDetail{ID: "gd-task", Type: "task", Status: "open"}
	rev := []beadsapi.Dependency{
		{BeadID: "gd-sess", DependsOnID: "gd-center", Type: "blocks"},
		{BeadID: "gd-agent", DependsOnID: "gd-center", Type: "blocks"},
		{BeadID: "gd-task", DependsOnID: "gd-center", Type: "blocks"},
	}

	srv := NewServer(fake, slog.Default(), "", "", "", nil)
	view := srv.buildConstellation(context.Background(),
		fake.beads["gd-center"], nil, rev,
		nil, nil, nil,
		1, /* treeLevel */
		true /* showClosedAgents — closed agents allowed, sessions never */)
	if view == nil {
		t.Fatal("constellation view nil")
	}
	got := map[string]bool{}
	for _, n := range view.Reverse {
		got[n.ID] = true
	}
	if got["gd-sess"] {
		t.Errorf("session bead must be hidden even with showClosedAgents=true; reverse nodes=%v", got)
	}
	if !got["gd-agent"] {
		t.Errorf("closed agent should be visible with showClosedAgents=true; reverse nodes=%v", got)
	}
	if !got["gd-task"] {
		t.Errorf("task should always be visible; reverse nodes=%v", got)
	}
}

func containsNodeID(nodes []constellationNode, id string) bool {
	for _, n := range nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

func nodeIDs(nodes []constellationNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

// TestConstellation_EdgesPropagateThroughAgentHub is the regression guard for
// ka-0000006339: edges must propagate THROUGH an agent node. Topology under
// test (Matt's report): center task T1 is connected to agent A1 (A1 is
// assigned-to T1, so A1 is a REVERSE dep of T1), and A1 is connected to tasks
// T2 and T3 (A1 is assigned-to T2/T3, so T2/T3 are FORWARD deps of A1). The
// reverse-only walk dead-ended at A1 because A1's siblings hang off the
// opposite dep direction; the fix fetches both directions for agent-like
// hubs so T2/T3 surface on the outer ring.
func TestConstellation_EdgesPropagateThroughAgentHub(t *testing.T) {
	fake := newFake()
	if fake.revDeps == nil {
		fake.revDeps = map[string][]beadsapi.Dependency{}
	}
	fake.beads["gd-t1"] = &beadsapi.BeadDetail{ID: "gd-t1", Type: "task", Status: "open", Title: "T1"}
	fake.beads["gd-a1"] = &beadsapi.BeadDetail{ID: "gd-a1", Type: "agent", Status: "open", Title: "A1"}
	fake.beads["gd-t2"] = &beadsapi.BeadDetail{ID: "gd-t2", Type: "task", Status: "open", Title: "T2"}
	fake.beads["gd-t3"] = &beadsapi.BeadDetail{ID: "gd-t3", Type: "task", Status: "open", Title: "T3"}

	// A1 is assigned-to T1 → A1 depends-on T1 → A1 is a reverse dep of T1.
	rev := []beadsapi.Dependency{
		{BeadID: "gd-a1", DependsOnID: "gd-t1", Type: "assigned"},
	}
	// A1 is assigned-to T2 and T3 → those are A1's forward deps. The reverse
	// walk from A1 alone would never reach them; the hub passthrough must.
	fake.deps["gd-a1"] = []beadsapi.Dependency{
		{BeadID: "gd-a1", DependsOnID: "gd-t2", Type: "assigned"},
		{BeadID: "gd-a1", DependsOnID: "gd-t3", Type: "assigned"},
	}

	srv := NewServer(fake, slog.Default(), "", "", "", nil)
	view := srv.buildConstellation(context.Background(),
		fake.beads["gd-t1"], nil, rev,
		nil, nil, nil,
		2,     /* treeLevel: expand one ring past L1 */
		false, /* showClosedAgents: production default */
	)
	if view == nil {
		t.Fatal("constellation view nil")
	}
	// A1 sits on the L1 reverse ring.
	if !containsNodeID(view.Reverse, "gd-a1") {
		t.Fatalf("agent A1 missing from L1 reverse ring; got %v", nodeIDs(view.Reverse))
	}
	// T2 and T3 must surface on the outer ring, reached THROUGH A1.
	for _, want := range []string{"gd-t2", "gd-t3"} {
		if !containsNodeID(view.Outer, want) {
			t.Errorf("sibling task %s did not propagate through agent hub; outer ring = %v", want, nodeIDs(view.Outer))
		}
	}
	// And the hop-by-hop edge A1→Tn must exist so the topology reads visually.
	for _, want := range []string{"gd-t2", "gd-t3"} {
		found := false
		for _, e := range view.Edges {
			if e.SourceBead == "gd-a1" && e.TargetBead == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing hub edge gd-a1 -> %s", want)
		}
	}
}

// TestConstellation_NonAgentNodeDoesNotPropagateBothDirections guards the
// scope of the ka-0000006339 fix: the both-directions passthrough is reserved
// for agent/session hubs. A plain task on the frontier must NOT pull in its
// opposite-direction neighbors, or the de-hairball work (ka-0000000447) regresses.
func TestConstellation_NonAgentNodeDoesNotPropagateBothDirections(t *testing.T) {
	fake := newFake()
	if fake.revDeps == nil {
		fake.revDeps = map[string][]beadsapi.Dependency{}
	}
	fake.beads["gd-t1"] = &beadsapi.BeadDetail{ID: "gd-t1", Type: "task", Status: "open", Title: "T1"}
	fake.beads["gd-p1"] = &beadsapi.BeadDetail{ID: "gd-p1", Type: "task", Status: "open", Title: "P1"}
	fake.beads["gd-side"] = &beadsapi.BeadDetail{ID: "gd-side", Type: "task", Status: "open", Title: "Side"}

	// P1 is a plain-task reverse dep of T1.
	rev := []beadsapi.Dependency{
		{BeadID: "gd-p1", DependsOnID: "gd-t1", Type: "blocks"},
	}
	// P1 has an opposite-direction (forward) neighbor. For a non-agent node it
	// must stay hidden — only same-direction expansion applies.
	fake.deps["gd-p1"] = []beadsapi.Dependency{
		{BeadID: "gd-p1", DependsOnID: "gd-side", Type: "blocks"},
	}

	srv := NewServer(fake, slog.Default(), "", "", "", nil)
	view := srv.buildConstellation(context.Background(),
		fake.beads["gd-t1"], nil, rev,
		nil, nil, nil,
		2, false,
	)
	if view == nil {
		t.Fatal("constellation view nil")
	}
	if containsNodeID(view.Outer, "gd-side") {
		t.Errorf("non-agent node pulled in opposite-direction neighbor gd-side; outer = %v", nodeIDs(view.Outer))
	}
}

// TestConstellation_HydrateBeadsBatches is the regression guard for the N+1
// refactor in gd-og6ohKnpwJ: hydrateBeads must drive exactly one
// BatchGetBeads call and zero per-id GetBead calls per buildConstellation
// invocation at the L1 entry point. Before MR B this path fired one goroutine
// per id with a 6-worker semaphore — at treeLevel=2/3 with a wide frontier
// that turned into dozens of round-trips. A regression to per-id GetBead would
// push getBeadCalls above 0.
func TestConstellation_HydrateBeadsBatches(t *testing.T) {
	fake := newFake()
	fake.beads["gd-center"] = &beadsapi.BeadDetail{ID: "gd-center", Type: "task", Status: "open", Title: "Center"}
	fwd := make([]beadsapi.Dependency, 0, 4)
	rev := make([]beadsapi.Dependency, 0, 4)
	for i := 1; i <= 4; i++ {
		idF := "gd-fwd" + string(rune('0'+i))
		idR := "gd-rev" + string(rune('0'+i))
		fake.beads[idF] = &beadsapi.BeadDetail{ID: idF, Type: "task", Status: "open"}
		fake.beads[idR] = &beadsapi.BeadDetail{ID: idR, Type: "task", Status: "open"}
		fwd = append(fwd, beadsapi.Dependency{BeadID: "gd-center", DependsOnID: idF, Type: "parent-child"})
		rev = append(rev, beadsapi.Dependency{BeadID: idR, DependsOnID: "gd-center", Type: "blocks"})
	}

	srv := NewServer(fake, slog.Default(), "", "", "", nil)
	view := srv.buildConstellation(context.Background(),
		fake.beads["gd-center"], fwd, rev,
		nil, nil, nil,
		1, /* treeLevel: just the L1 hydrate path */
		true /* showClosedAgents */)
	if view == nil {
		t.Fatal("constellation view nil")
	}
	if fake.batchGetBeadsCalls != 1 {
		t.Errorf("BatchGetBeads calls=%d, want 1 (single batched fetch per hydrateBeads)", fake.batchGetBeadsCalls)
	}
	if fake.getBeadCalls != 0 {
		t.Errorf("GetBead calls=%d, want 0 (refactor must not fall back to per-id fetches)", fake.getBeadCalls)
	}
	// 4 forward + 4 reverse = 8 ids hydrated in a single shard (well under
	// BatchGetBeadsMaxIDs, so no sharding kicks in).
	if fake.batchGetBeadsLastN != 8 {
		t.Errorf("BatchGetBeads last shard size=%d, want 8 (4 forward + 4 reverse ids)", fake.batchGetBeadsLastN)
	}
}

// TestConstellation_HydrateBeadsMissingIDsAbsent verifies the 404 semantics
// are preserved by the BatchGetBeads refactor: ids the daemon omits from the
// batch response must be absent from the returned map (callers fall through
// to ID-only rendering). The fake's BatchGetBeads omits unknown ids — matching
// the daemon's per-result `error: "not found"` shape collapsed by the client.
func TestConstellation_HydrateBeadsMissingIDsAbsent(t *testing.T) {
	fake := newFake()
	fake.beads["gd-present"] = &beadsapi.BeadDetail{ID: "gd-present", Type: "task", Status: "open"}
	srv := NewServer(fake, slog.Default(), "", "", "", nil)
	out := srv.hydrateBeads(context.Background(), []string{"gd-present", "gd-missing"})
	if _, ok := out["gd-present"]; !ok {
		t.Errorf("expected gd-present in hydrated map")
	}
	if _, ok := out["gd-missing"]; ok {
		t.Errorf("gd-missing must be absent from hydrated map (preserves pre-refactor 404 semantics)")
	}
	if fake.batchGetBeadsCalls != 1 {
		t.Errorf("BatchGetBeads calls=%d, want 1", fake.batchGetBeadsCalls)
	}
}

// --- Assignee linkage (ka-0000003339) ---

// TestEdgeStyle_AssigneeGolden pins the golden palette for the synthetic
// assignee edge so a future palette change has to update this test.
func TestEdgeStyle_AssigneeGolden(t *testing.T) {
	stroke, dash, op := edgeStyle("assignee", "assignee")
	if stroke != "#b9851b" || dash != "" || op != 0.85 {
		t.Errorf("assignee style = (%q,%q,%v); want (#b9851b,'',0.85)", stroke, dash, op)
	}
}

// TestEdgeTitle_Assignee pins the tooltip string used by the SVG <title>.
func TestEdgeTitle_Assignee(t *testing.T) {
	if got := edgeTitle("assignee", "assignee", ""); got != "assigned to" {
		t.Errorf("edgeTitle assignee = %q; want %q", got, "assigned to")
	}
}

// TestAssigneeNode_Shape pins the disk shape, glyph, and href contract — the
// node must read as an "agent" disk with the golden palette and route its
// anchor through basePath so /v/ deployments link correctly.
func TestAssigneeNode_Shape(t *testing.T) {
	n := assigneeNode("matt-1", 100, 200, "/v/gd-pir8sgar3n")
	if n.ID != "matt-1" {
		t.Errorf("ID = %q; want matt-1", n.ID)
	}
	if n.Role != "assignee" {
		t.Errorf("Role = %q; want assignee", n.Role)
	}
	if n.Type != "agent" || n.Glyph != "A" {
		t.Errorf("Type/Glyph = %q/%q; want agent/A", n.Type, n.Glyph)
	}
	if n.Color != "#f1c84a" || n.Stroke != "#b9851b" {
		t.Errorf("colors = (%q,%q); want (#f1c84a,#b9851b)", n.Color, n.Stroke)
	}
	if n.Href != "/v/gd-pir8sgar3n" {
		t.Errorf("Href = %q; want /v/gd-pir8sgar3n (resolved href passed through)", n.Href)
	}
	if n.Opacity != 1.0 {
		t.Errorf("Opacity = %v; want 1.0 (assignee never age-fades)", n.Opacity)
	}
}

// TestAssigneeNodeHref pins the resolution contract for the agent-disk link
// (ka-0000002108). Before the fix the href was always basePath+"/"+assignee,
// which 404'd for raw agent names (they're not bead ids). Now:
//   - agent names resolve to their agent bead at /{gd-id};
//   - gd-XXX ids route straight through;
//   - unresolvable / human assignees fall back to a search page.
func TestAssigneeNodeHref(t *testing.T) {
	fake := newFake()
	// An agent bead that "agent-7" resolves to.
	fake.beads["gd-agentbead01"] = &beadsapi.BeadDetail{
		ID: "gd-agentbead01", Type: "agent", Status: "open",
		Fields: map[string]string{"agent": "agent-7"},
	}
	srv := NewServer(fake, slog.Default(), "/v", "", "", nil)
	ctx := context.Background()

	t.Run("agent name resolves to its bead", func(t *testing.T) {
		got := srv.assigneeNodeHref(ctx, "agent-7")
		if want := "/v/gd-agentbead01"; got != want {
			t.Errorf("href = %q; want %q", got, want)
		}
	})
	t.Run("kd id routes straight through", func(t *testing.T) {
		got := srv.assigneeNodeHref(ctx, "gd-Wj3qtMvsHA")
		if want := "/v/gd-Wj3qtMvsHA"; got != want {
			t.Errorf("href = %q; want %q", got, want)
		}
	})
	t.Run("unresolved agent name falls back to search", func(t *testing.T) {
		got := srv.assigneeNodeHref(ctx, "ghost-agent")
		if want := "/v/search?q=ghost-agent"; got != want {
			t.Errorf("href = %q; want %q (search fallback, not a 404)", got, want)
		}
	})
	t.Run("human assignee falls back to search", func(t *testing.T) {
		got := srv.assigneeNodeHref(ctx, "matt@example.com")
		if want := "/v/search?q=matt%40example.com"; got != want {
			t.Errorf("href = %q; want %q (search fallback)", got, want)
		}
	})
	t.Run("empty assignee yields empty href", func(t *testing.T) {
		if got := srv.assigneeNodeHref(ctx, ""); got != "" {
			t.Errorf("href = %q; want empty", got)
		}
	})
}

// TestAssigneePosition_BelowCenter verifies the assignee disk lands directly
// below the center at ~55% of the L1 radius — the "empty area" between the
// center and the L1 dep ring. Tightens guard against a future layout move
// that would put the assignee on top of the parents/children arcs.
func TestAssigneePosition_BelowCenter(t *testing.T) {
	x, y := assigneePosition(500, 250, 240)
	if x != 500 {
		t.Errorf("x = %v; want 500 (directly below center)", x)
	}
	wantY := 250 + 240*0.55
	if math.Abs(y-wantY) > 1e-9 {
		t.Errorf("y = %v; want %v", y, wantY)
	}
	if y >= 250+240 {
		t.Errorf("assignee y=%v should sit inside the L1 ring (cy+l1=%v)", y, 250+240)
	}
}

// TestAssigneeEdge_DataAttributes pins the edge's data-source/target-bead
// contract so any future click-handler can navigate from the SVG.
func TestAssigneeEdge_DataAttributes(t *testing.T) {
	e := assigneeEdge(0, 0, 10, 20, "gd-vnudveh8ps", "matt-1")
	if e.Origin != "assignee" || e.Role != "assignee" {
		t.Errorf("origin/role = (%q,%q); want (assignee,assignee)", e.Origin, e.Role)
	}
	if e.SourceBead != "gd-vnudveh8ps" || e.TargetBead != "matt-1" {
		t.Errorf("source/target = (%q,%q); want (gd-vnudveh8ps,matt-1)", e.SourceBead, e.TargetBead)
	}
	if e.TitleText != "assigned to" {
		t.Errorf("title = %q; want %q", e.TitleText, "assigned to")
	}
}

// TestConstellation_AssigneeRendersInTemplate verifies the assignee node
// renders inside the orbit-nodes group and the edge carries the
// data-origin="assignee" hook. Locks the new linkage in regression so a
// future rewrite of the template that drops the {{if .HasAssignee}} branch
// fails this test.
func TestConstellation_AssigneeRendersInTemplate(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	view := constellationFixture()
	cx, cy := view.Center.X, view.Center.Y
	ax, ay := assigneePosition(cx, cy, 240)
	view.Assignee = assigneeNode("matt-1", ax, ay, "/matt-1")
	view.Edges = append(view.Edges, assigneeEdge(cx, cy, ax, ay, view.Center.ID, "matt-1"))
	view.HasAssignee = true
	view.HasAnything = true
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation", map[string]any{"Constellation": view}); err != nil {
		t.Fatalf("execute constellation: %v", err)
	}
	out := buf.String()
	wants := []string{
		`href="/matt-1"`,              // assignee disk anchor
		`constellation-node-assignee`, // role-class on the node group
		`data-origin="assignee"`,      // edge origin attr
		`data-target-bead="matt-1"`,   // edge target = assignee string
		`<title>assigned to</title>`,  // edge tooltip
		`#b9851b`,                     // golden stroke (legend + edge)
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("constellation output missing %q\noutput:\n%s", w, out)
		}
	}
}

// TestConstellation_NoAssigneeOmitsNode verifies the template omits the
// synthetic assignee disk and edge when the bead has no assignee.
func TestConstellation_NoAssigneeOmitsNode(t *testing.T) {
	tmpl := parseConstellationTemplates(t)
	view := constellationFixture()
	view.HasAssignee = false
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "constellation", map[string]any{"Constellation": view}); err != nil {
		t.Fatalf("execute constellation: %v", err)
	}
	if strings.Contains(buf.String(), `constellation-node-assignee`) {
		t.Errorf("assignee node rendered without HasAssignee=true; should be hidden")
	}
}

// TestConstellation_LegendIncludesAssigneeEntry pins the legend row so an
// accidental legend rewrite doesn't drop the assignee edge from the key.
func TestConstellation_LegendIncludesAssigneeEntry(t *testing.T) {
	items := constellationLegendItems()
	for _, it := range items {
		if it.Label == "assignee" && it.Stroke == "#b9851b" {
			return
		}
	}
	t.Errorf("legend missing assignee entry with stroke #b9851b; got %+v", items)
}

// --- Closed agent/session filter (ka-0000005724) ---

// TestIsClosedAgentLike pins the predicate that decides whether a hydrated
// dep target is a "closed agent or session bead" — the long-tail noise we
// strip from the constellation by default.
// TestIsHiddenOrbitBead pins the default-hide predicate: ANY session bead is
// hidden (ka-0000000532) and closed agents are hidden (ka-0000005724), while
// open agents and every non-agent/non-session type stay visible.
func TestIsHiddenOrbitBead(t *testing.T) {
	cases := []struct {
		name string
		bd   *beadsapi.BeadDetail
		want bool
	}{
		{"nil", nil, false},
		{"closed agent", &beadsapi.BeadDetail{Type: "agent", Status: "closed"}, true},
		{"closed session", &beadsapi.BeadDetail{Type: "session", Status: "closed"}, true},
		{"open agent", &beadsapi.BeadDetail{Type: "agent", Status: "open"}, false},
		{"open session", &beadsapi.BeadDetail{Type: "session", Status: "open"}, true}, // ka-0000000532: sessions hidden regardless of status
		{"in_progress session", &beadsapi.BeadDetail{Type: "session", Status: "in_progress"}, true},
		{"closed task", &beadsapi.BeadDetail{Type: "task", Status: "closed"}, false},
		{"closed epic", &beadsapi.BeadDetail{Type: "epic", Status: "closed"}, false},
		{"empty type", &beadsapi.BeadDetail{Type: "", Status: "closed"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHiddenOrbitBead(tc.bd); got != tc.want {
				t.Errorf("isHiddenOrbitBead(%+v) = %v; want %v", tc.bd, got, tc.want)
			}
		})
	}
}

// TestIsSessionBead pins the session-only predicate used by the gardener map and
// inbox panel, where closed agents are meaningful seeds but session beads are
// still hidden (ka-0000000532).
func TestIsSessionBead(t *testing.T) {
	cases := []struct {
		name string
		bd   *beadsapi.BeadDetail
		want bool
	}{
		{"nil", nil, false},
		{"open session", &beadsapi.BeadDetail{Type: "session", Status: "open"}, true},
		{"closed session", &beadsapi.BeadDetail{Type: "session", Status: "closed"}, true},
		{"closed agent", &beadsapi.BeadDetail{Type: "agent", Status: "closed"}, false},
		{"open agent", &beadsapi.BeadDetail{Type: "agent", Status: "open"}, false},
		{"task", &beadsapi.BeadDetail{Type: "task", Status: "open"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSessionBead(tc.bd); got != tc.want {
				t.Errorf("isSessionBead(%+v) = %v; want %v", tc.bd, got, tc.want)
			}
		})
	}
}

// TestParseShowClosedAgentsParam pins the URL-param truthy set. Closed
// agent/session beads stay hidden unless the user opts in via a recognized
// truthy value.
func TestParseShowClosedAgentsParam(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"off", false},
		{"junk", false},
		{"1", true},
		{"true", true},
		{"True", true},
		{"YES", true},
		{"on", true},
		{"  1  ", true}, // whitespace tolerated
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if got := parseShowClosedAgentsParam(tc.raw); got != tc.want {
				t.Errorf("parseShowClosedAgentsParam(%q) = %v; want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestFilterClosedAgents_DropsClosedAndBackfills verifies that closed
// agent/session targets are dropped and the next non-closed candidates
// backfill the visible slots, up to displayCap.
func TestFilterHiddenOrbitBeads_DropsClosedAndBackfills(t *testing.T) {
	ids := []string{"gd-1", "gd-2", "gd-3", "gd-4", "gd-5"}
	originByID := map[string]taggedDep{
		"gd-1": {BeadID: "gd-1", Origin: "dep"},
		"gd-2": {BeadID: "gd-2", Origin: "dep"},
		"gd-3": {BeadID: "gd-3", Origin: "dep"},
		"gd-4": {BeadID: "gd-4", Origin: "dep"},
		"gd-5": {BeadID: "gd-5", Origin: "dep"},
	}
	hydrated := map[string]*beadsapi.BeadDetail{
		"gd-1": {ID: "gd-1", Type: "session", Status: "closed"}, // drop
		"gd-2": {ID: "gd-2", Type: "task", Status: "open"},      // keep
		"gd-3": {ID: "gd-3", Type: "agent", Status: "closed"},   // drop
		"gd-4": {ID: "gd-4", Type: "epic", Status: "open"},      // keep
		"gd-5": {ID: "gd-5", Type: "task", Status: "closed"},    // keep (closed task ≠ closed agent)
	}
	keptIDs, keptOrigin := filterHiddenOrbitBeads(ids, originByID, hydrated, 3)
	want := []string{"gd-2", "gd-4", "gd-5"}
	if len(keptIDs) != len(want) {
		t.Fatalf("kept ids = %v; want %v", keptIDs, want)
	}
	for i, id := range want {
		if keptIDs[i] != id {
			t.Errorf("keptIDs[%d] = %q; want %q", i, keptIDs[i], id)
		}
		if _, ok := keptOrigin[id]; !ok {
			t.Errorf("keptOrigin missing %q", id)
		}
	}
	if _, ok := keptOrigin["gd-1"]; ok {
		t.Errorf("keptOrigin should not carry dropped id gd-1")
	}
}

// TestFilterClosedAgents_RespectsDisplayCap verifies the cap truncates
// after filtering, never before — so a closed-agent in slot 0 still leaves
// displayCap visible non-closed nodes when candidates exist.
func TestFilterHiddenOrbitBeads_RespectsDisplayCap(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}
	originByID := map[string]taggedDep{
		"a": {BeadID: "a"}, "b": {BeadID: "b"}, "c": {BeadID: "c"}, "d": {BeadID: "d"},
	}
	hydrated := map[string]*beadsapi.BeadDetail{
		"a": {Type: "task", Status: "open"},
		"b": {Type: "task", Status: "open"},
		"c": {Type: "task", Status: "open"},
		"d": {Type: "task", Status: "open"},
	}
	got, _ := filterHiddenOrbitBeads(ids, originByID, hydrated, 2)
	if len(got) != 2 {
		t.Errorf("len(got) = %d; want 2 (display cap)", len(got))
	}
	if got[0] != "a" || got[1] != "b" {
		t.Errorf("got = %v; want [a b]", got)
	}
}

// TestFilterOrbitBy_PrioritizesOpenAtCap verifies that when survivors overflow
// displayCap, open/in_progress beads win render slots over closed/deferred ones
// (ka-0000002450): the bead-show orbit exists to surface active work, so a
// truncating cap must not drop live beads to draw closed ones. The closed beads
// here sort BEFORE the open ones by ID, so pure ID-order would crowd the open
// beads out entirely.
func TestFilterOrbitBy_PrioritizesOpenAtCap(t *testing.T) {
	ids := []string{"gd-aclosed-1", "gd-aclosed-2", "gd-bopen-1", "gd-bprog-2"}
	originByID := map[string]taggedDep{
		"gd-aclosed-1": {BeadID: "gd-aclosed-1", Origin: "dep"},
		"gd-aclosed-2": {BeadID: "gd-aclosed-2", Origin: "dep"},
		"gd-bopen-1":   {BeadID: "gd-bopen-1", Origin: "dep"},
		"gd-bprog-2":   {BeadID: "gd-bprog-2", Origin: "dep"},
	}
	hydrated := map[string]*beadsapi.BeadDetail{
		"gd-aclosed-1": {ID: "gd-aclosed-1", Type: "task", Status: "closed"},
		"gd-aclosed-2": {ID: "gd-aclosed-2", Type: "task", Status: "deferred"},
		"gd-bopen-1":   {ID: "gd-bopen-1", Type: "task", Status: "open"},
		"gd-bprog-2":   {ID: "gd-bprog-2", Type: "task", Status: "in_progress"},
	}
	// drop nothing — exercise the cap ordering in isolation.
	keepAll := func(*beadsapi.BeadDetail) bool { return false }
	got, gotOrigin := filterOrbitBy(ids, originByID, hydrated, 2, keepAll)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d; want 2 (display cap)", len(got))
	}
	want := []string{"gd-bopen-1", "gd-bprog-2"}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("got[%d] = %q; want %q (open/in_progress must win cap slots)", i, got[i], id)
		}
		if _, ok := gotOrigin[id]; !ok {
			t.Errorf("gotOrigin missing kept id %q", id)
		}
	}
	if _, ok := gotOrigin["gd-aclosed-1"]; ok {
		t.Errorf("gotOrigin should not carry truncated closed id gd-aclosed-1")
	}
}

// TestFilterOrbitBy_StableWithinStatusTier verifies that within a single status
// tier the incoming ID order is preserved (taggedTargets sorts by ID), so layout
// stays deterministic when no status reordering is needed.
func TestFilterOrbitBy_StableWithinStatusTier(t *testing.T) {
	ids := []string{"gd-1", "gd-2", "gd-3"}
	originByID := map[string]taggedDep{
		"gd-1": {BeadID: "gd-1"}, "gd-2": {BeadID: "gd-2"}, "gd-3": {BeadID: "gd-3"},
	}
	hydrated := map[string]*beadsapi.BeadDetail{
		"gd-1": {Type: "task", Status: "open"},
		"gd-2": {Type: "task", Status: "open"},
		"gd-3": {Type: "task", Status: "open"},
	}
	keepAll := func(*beadsapi.BeadDetail) bool { return false }
	got, _ := filterOrbitBy(ids, originByID, hydrated, 2, keepAll)
	if len(got) != 2 || got[0] != "gd-1" || got[1] != "gd-2" {
		t.Errorf("got = %v; want [gd-1 gd-2] (ID order preserved within tier)", got)
	}
}

// TestFilterClosedAgents_KeepsUnhydrated verifies that targets missing
// from the hydrated map (timeout, 404) are kept — we don't know their
// type/status, and rendering them as "?" nodes is preferable to silently
// dropping them.
func TestFilterHiddenOrbitBeads_KeepsUnhydrated(t *testing.T) {
	ids := []string{"gd-missing", "gd-known-closed"}
	originByID := map[string]taggedDep{
		"gd-missing":      {BeadID: "gd-missing"},
		"gd-known-closed": {BeadID: "gd-known-closed"},
	}
	hydrated := map[string]*beadsapi.BeadDetail{
		"gd-known-closed": {Type: "agent", Status: "closed"},
	}
	got, _ := filterHiddenOrbitBeads(ids, originByID, hydrated, 5)
	if len(got) != 1 || got[0] != "gd-missing" {
		t.Errorf("got = %v; want [gd-missing]", got)
	}
}

// TestApplyAgeOpacity_HandlesOuter verifies outer-ring nodes get age-faded
// alongside Forward/Reverse/Similar so the recency cue stays consistent
// across the whole tree.
func TestApplyAgeOpacity_HandlesOuter(t *testing.T) {
	oldTS := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	newTS := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	view := &constellationView{
		Center:  constellationNode{ID: "gd-c"},
		Forward: []constellationNode{{ID: "gd-fwd-new"}},
		Outer:   []constellationNode{{ID: "gd-outer-old"}},
	}
	hydrated := map[string]*beadsapi.BeadDetail{
		"gd-fwd-new":   {ID: "gd-fwd-new", UpdatedAt: newTS},
		"gd-outer-old": {ID: "gd-outer-old", UpdatedAt: oldTS},
	}
	applyAgeOpacity(view, &beadsapi.BeadDetail{ID: "gd-c"}, hydrated, nil)
	if math.Abs(view.Outer[0].Opacity-ageOpacityFloor) > 1e-9 {
		t.Errorf("outer (oldest) opacity = %v; want floor %v", view.Outer[0].Opacity, ageOpacityFloor)
	}
	if view.Forward[0].Opacity != 1.0 {
		t.Errorf("forward (newest) opacity = %v; want 1.0", view.Forward[0].Opacity)
	}
}

// --- Succession edge (task ka-0000007852) ---

// TestConstellation_SuccessionEdgeForAgent verifies that an agent center bead
// carrying a prior_agent_id gets a DISPLAY-ONLY predecessor node + succession
// edge, distinct from the deps graph.
func TestConstellation_SuccessionEdgeForAgent(t *testing.T) {
	fake := newFake()
	fake.beads["gd-head"] = &beadsapi.BeadDetail{
		ID: "gd-head", Type: "agent", Status: "in_progress", Title: "head",
		Fields: map[string]string{"agent": "agent-head", "prior_agent_id": "gd-prior"},
	}
	fake.beads["gd-prior"] = &beadsapi.BeadDetail{
		ID: "gd-prior", Type: "agent", Status: "closed", Title: "prior",
		Fields: map[string]string{"agent": "agent-prior"},
	}
	srv := NewServer(fake, slog.Default(), "", "", "", nil)
	view := srv.buildConstellation(context.Background(),
		fake.beads["gd-head"], nil, nil, nil, nil, nil, 1, true)
	if view == nil {
		t.Fatal("constellation view nil")
	}
	if !view.HasPredecessor {
		t.Fatal("expected HasPredecessor true for agent with prior_agent_id")
	}
	if view.Predecessor.ID != "gd-prior" {
		t.Errorf("Predecessor.ID = %q, want gd-prior", view.Predecessor.ID)
	}
	if view.Predecessor.Role != "succession" {
		t.Errorf("Predecessor.Role = %q, want succession", view.Predecessor.Role)
	}
	// The succession edge is present and out of the dep-role families.
	var found *constellationEdge
	for i := range view.Edges {
		if view.Edges[i].Origin == "succession" {
			found = &view.Edges[i]
			break
		}
	}
	if found == nil {
		t.Fatal("no succession-origin edge emitted")
	}
	if found.TargetBead != "gd-prior" || found.SourceBead != "gd-head" {
		t.Errorf("succession edge endpoints wrong: src=%q target=%q", found.SourceBead, found.TargetBead)
	}
	// It must NOT appear as a forward/reverse dependency node.
	for _, n := range append(append([]constellationNode{}, view.Forward...), view.Reverse...) {
		if n.ID == "gd-prior" {
			t.Error("predecessor leaked into the deps graph (forward/reverse) — must stay display-only")
		}
	}
}

// TestConstellation_NoSuccessionForNonAgent pins that a non-agent center never
// gets a succession node even if it carries a prior_agent_id field.
func TestConstellation_NoSuccessionForNonAgent(t *testing.T) {
	fake := newFake()
	fake.beads["gd-t"] = &beadsapi.BeadDetail{
		ID: "gd-t", Type: "task", Status: "open", Title: "task",
		Fields: map[string]string{"prior_agent_id": "gd-prior"},
	}
	srv := NewServer(fake, slog.Default(), "", "", "", nil)
	view := srv.buildConstellation(context.Background(),
		fake.beads["gd-t"], nil, nil, nil, nil, nil, 1, true)
	if view == nil {
		t.Fatal("constellation view nil")
	}
	if view.HasPredecessor {
		t.Error("non-agent center should not get a succession predecessor node")
	}
}

// TestConstellation_SuccessionFallsBackToSchedulePrior verifies the walk reads
// schedule_prior_agent_id when prior_agent_id is absent (legacy gardeners).
func TestConstellation_SuccessionFallsBackToSchedulePrior(t *testing.T) {
	fake := newFake()
	fake.beads["gd-head"] = &beadsapi.BeadDetail{
		ID: "gd-head", Type: "agent", Status: "in_progress", Title: "head",
		Fields: map[string]string{"agent": "cap-2", "schedule_prior_agent_id": "gd-prior"},
	}
	fake.beads["gd-prior"] = &beadsapi.BeadDetail{
		ID: "gd-prior", Type: "agent", Status: "closed", Title: "prior",
		Fields: map[string]string{"agent": "cap-1"},
	}
	srv := NewServer(fake, slog.Default(), "", "", "", nil)
	view := srv.buildConstellation(context.Background(),
		fake.beads["gd-head"], nil, nil, nil, nil, nil, 1, true)
	if !view.HasPredecessor || view.Predecessor.ID != "gd-prior" {
		t.Fatalf("expected schedule_prior fallback predecessor gd-prior, got HasPredecessor=%v id=%q",
			view.HasPredecessor, view.Predecessor.ID)
	}
}

// TestConstellationLegend_IncludesSuccession verifies the legend advertises the
// display-only succession edge so operators can decode it.
func TestConstellationLegend_IncludesSuccession(t *testing.T) {
	items := constellationLegendItems()
	found := false
	for _, it := range items {
		if strings.HasPrefix(it.Label, "succession") {
			found = true
			if it.Stroke != "#0f9b8e" {
				t.Errorf("succession legend stroke = %q, want #0f9b8e", it.Stroke)
			}
		}
	}
	if !found {
		t.Error("legend missing a succession entry")
	}
}
