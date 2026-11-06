package main

import (
	"context"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestBuildBundleView_FiltersAndOrdersDocs(t *testing.T) {
	bundle := &beadsapi.BeadDetail{
		ID:          "gd-g0xoawomrs",
		Type:        "bundle",
		Title:       "Auth refactor deliverables",
		Description: "Plan + diff-summary + checklist for the JWT cutover.",
		Fields:      map[string]string{"kind": "plan"},
	}

	reverseDeps := []beadsapi.Dependency{
		// parent-child doc, position 2 → second in TOC.
		{BeadID: "gd-doc00000002", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
		// parent-child doc, position 1 → first.
		{BeadID: "gd-doc00000001", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
		// parent-child doc, position 3 → third.
		{BeadID: "gd-doc00000003", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
		// `related` dep — must be ignored.
		{BeadID: "gd-doc00000099", DependsOnID: "gd-g0xoawomrs", Type: "related"},
		// parent-child task referencing the bundle — must be filtered out
		// after fetch (wrong child type).
		{BeadID: "gd-task00000001", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
	}

	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-doc00000001": {
			ID:   "gd-doc00000001",
			Type: "doc",
			Fields: map[string]string{
				"format":   "md",
				"position": "1",
				"headline": "Plan of record",
				"content":  "# Plan\nStep 1.\n",
			},
		},
		"gd-doc00000002": {
			ID:   "gd-doc00000002",
			Type: "doc",
			Fields: map[string]string{
				"format":   "diff",
				"position": "2",
				"headline": "Diff summary",
				"content":  "--- a/x\n+++ b/x\n",
				"language": "go",
			},
		},
		"gd-doc00000003": {
			ID:   "gd-doc00000003",
			Type: "doc",
			Fields: map[string]string{
				"format":      "link",
				"position":    "3",
				"headline":    "Pipeline run",
				"content_url": "https://example.invalid/pipeline/1",
			},
		},
		"gd-doc00000099": {
			ID:     "gd-doc00000099",
			Type:   "doc",
			Fields: map[string]string{"format": "md"},
		},
		"gd-task00000001": {
			ID:   "gd-task00000001",
			Type: "task",
		},
	}}

	v := buildBundleView(context.Background(), bundle, nil, reverseDeps, fake, nil)
	if v == nil {
		t.Fatal("buildBundleView returned nil")
	}
	if v.Title != "Auth refactor deliverables" {
		t.Errorf("Title=%q, want bundle title", v.Title)
	}
	if v.Kind != "plan" {
		t.Errorf("Kind=%q, want plan", v.Kind)
	}
	if got, want := len(v.Docs), 3; got != want {
		t.Fatalf("got %d docs, want %d (filter dropped wrong rows)", got, want)
	}
	gotIDs := []string{v.Docs[0].BeadID, v.Docs[1].BeadID, v.Docs[2].BeadID}
	wantIDs := []string{"gd-doc00000001", "gd-doc00000002", "gd-doc00000003"}
	for i := range gotIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("docs[%d]=%q, want %q (position-asc ordering)", i, gotIDs[i], wantIDs[i])
		}
	}
	if v.Docs[1].Format != "diff" || v.Docs[1].Language != "go" {
		t.Errorf("doc[1] format/language not propagated: %+v", v.Docs[1])
	}
	if v.Docs[2].ContentURL == "" {
		t.Errorf("doc[2] should have content_url propagated, got empty")
	}
}

func TestBuildBundleView_DropsMissingOrDeletedChild(t *testing.T) {
	// A parent-child edge whose target bead no longer resolves (deleted in a
	// bundle re-cut, or a transient batch miss) must NOT render a ghost
	// "(unset) / No content" card — it is dropped. Rendering empty placeholders
	// for deleted docs littered every edited bundle (ka-0000007961 follow-up).
	bundle := &beadsapi.BeadDetail{
		ID:    "gd-g0xoawomrs",
		Type:  "bundle",
		Title: "Test bundle",
	}
	reverseDeps := []beadsapi.Dependency{
		{BeadID: "gd-doc00000001", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
	}

	// fakeDaemon with no beads — the child id resolves to nothing (deleted).
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{}}

	v := buildBundleView(context.Background(), bundle, nil, reverseDeps, fake, nil)
	if v == nil {
		t.Fatal("expected a bundle view, got nil")
	}
	if len(v.Docs) != 0 {
		t.Fatalf("expected the deleted/missing child to be dropped, got %d docs: %+v", len(v.Docs), v.Docs)
	}
}

func TestBuildBundleView_KeepsLiveChildAlongsideDeletedOne(t *testing.T) {
	// A live doc must still render even when a sibling parent-child edge points
	// at a deleted bead — the drop is per-doc, not all-or-nothing.
	bundle := &beadsapi.BeadDetail{ID: "gd-7mv421ntul", Type: "bundle", Title: "Mixed"}
	reverseDeps := []beadsapi.Dependency{
		{BeadID: "gd-deleted00001", DependsOnID: "gd-7mv421ntul", Type: "parent-child"},
		{BeadID: "gd-live00000001", DependsOnID: "gd-7mv421ntul", Type: "parent-child"},
	}
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-live00000001": {
			ID:   "gd-live00000001",
			Type: "doc",
			Fields: map[string]string{
				"format": "md", "content": "# alive", "position": "0", "headline": "Live doc",
			},
		},
	}}

	v := buildBundleView(context.Background(), bundle, nil, reverseDeps, fake, nil)
	if v == nil || len(v.Docs) != 1 {
		t.Fatalf("expected exactly the live doc, got %+v", v)
	}
	if v.Docs[0].BeadID != "gd-live00000001" {
		t.Errorf("kept the wrong doc: %q", v.Docs[0].BeadID)
	}
}

func TestBuildBundleView_BackLinkFromFirstParentMention(t *testing.T) {
	parentChild := "parent-child"
	related := "related"
	mentions := []beadsapi.BeadReferenceSummary{
		{ID: "gd-related0001", Type: "task", RelationshipType: &related, Title: "related thing"},
		{ID: "gd-origin00001", Type: "task", RelationshipType: &parentChild, Title: "Origin task"},
		{ID: "gd-second00001", Type: "task", RelationshipType: &parentChild, Title: "Second parent"},
	}

	v := buildBundleView(context.Background(), &beadsapi.BeadDetail{ID: "gd-g0xoawomrs", Type: "bundle"}, mentions, nil, &fakeDaemon{}, nil)
	if v.BackLink == nil {
		t.Fatal("expected BackLink, got nil")
	}
	if v.BackLink.ID != "gd-origin00001" || v.BackLink.Title != "Origin task" {
		t.Errorf("BackLink=%+v, want first parent-child mention", v.BackLink)
	}
}

func TestBuildBundleView_BackLinkTitleFallsBackToIDWhenBlank(t *testing.T) {
	// A whitespace-only title must fall back to the ID rather than rendering
	// an empty back-link label.
	parentChild := "parent-child"
	mentions := []beadsapi.BeadReferenceSummary{
		{ID: "gd-origin00001", Type: "task", RelationshipType: &parentChild, Title: " \t\n"},
	}

	v := buildBundleView(context.Background(), &beadsapi.BeadDetail{ID: "gd-g0xoawomrs", Type: "bundle"}, mentions, nil, &fakeDaemon{}, nil)
	if v.BackLink == nil {
		t.Fatal("expected BackLink, got nil")
	}
	if v.BackLink.Title != "gd-origin00001" {
		t.Errorf("blank Title should fall back to ID: got %q", v.BackLink.Title)
	}
}

func newBundleFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-bundle-empty": {
				ID:    "gd-bundle-empty",
				Type:  "bundle",
				Title: "Empty bundle",
			},
			"gd-bundle-full": {
				ID:          "gd-bundle-full",
				Type:        "bundle",
				Title:       "Three-doc bundle",
				Description: "Bundle description text.",
				Fields:      map[string]string{"kind": "plan"},
			},
			"gd-doc-aaa": {
				ID:   "gd-doc-aaa",
				Type: "doc",
				Fields: map[string]string{
					"format":   "md",
					"position": "1",
					"headline": "First doc headline",
					"content":  "first content",
				},
			},
			"gd-doc-bbb": {
				ID:   "gd-doc-bbb",
				Type: "doc",
				Fields: map[string]string{
					"format":   "diff",
					"position": "2",
					"headline": "Second doc headline",
					"content":  "second content",
				},
			},
			"gd-doc-ccc": {
				ID:   "gd-doc-ccc",
				Type: "doc",
				Fields: map[string]string{
					"format":   "code",
					"position": "3",
					"headline": "Third doc headline",
					"content":  "third content",
				},
			},
			// A non-bundle bead so /bundle/{non-bundle-id} → 404.
			"gd-task-misc": {
				ID:    "gd-task-misc",
				Type:  "task",
				Title: "A regular task",
			},
		},
		revDeps: map[string][]beadsapi.Dependency{
			"gd-bundle-full": {
				// Out-of-order on purpose — buildBundleView sorts by position.
				{BeadID: "gd-doc-ccc", DependsOnID: "gd-bundle-full", Type: "parent-child"},
				{BeadID: "gd-doc-aaa", DependsOnID: "gd-bundle-full", Type: "parent-child"},
				{BeadID: "gd-doc-bbb", DependsOnID: "gd-bundle-full", Type: "parent-child"},
			},
		},
	}
}

func TestBundleShow_NotFound(t *testing.T) {
	mux := mkServer(t, newBundleFake())

	// gd-doesnotexist is absent from the fake's bead map, so GetBead returns
	// a 404 APIError — handler must propagate it as 404 (not 500).
	req := httptest.NewRequest("GET", "/bundle/gd-doesnotexist", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing bundle, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBundleShow_RejectsNonBundleType(t *testing.T) {
	mux := mkServer(t, newBundleFake())

	req := httptest.NewRequest("GET", "/bundle/gd-task-misc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-bundle bead, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBundleShow_EmptyRendersTOC(t *testing.T) {
	mux := mkServer(t, newBundleFake())

	req := httptest.NewRequest("GET", "/bundle/gd-bundle-empty", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty bundle, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"Empty bundle",
		"gd-bundle-empty",
		"No documents attached to this bundle yet.",
		"bundle-toc",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty-state body missing %q\n--- body ---\n%s", want, body)
		}
	}
	// Sanity: the per-doc article should NOT be present on the empty page.
	if strings.Contains(body, `id="doc-`) {
		t.Errorf("empty-state body should not render any doc article, got:\n%s", body)
	}
}

func TestBundleShow_PopulatedRendersOrderedCards(t *testing.T) {
	mux := mkServer(t, newBundleFake())

	req := httptest.NewRequest("GET", "/bundle/gd-bundle-full", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for populated bundle, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"Three-doc bundle",
		"Bundle description text.",
		`id="doc-gd-doc-aaa"`,
		`id="doc-gd-doc-bbb"`,
		`id="doc-gd-doc-ccc"`,
		"First doc headline",
		"Second doc headline",
		"Third doc headline",
		// T6 — ka-0000007526: left-rail TOC layout, deep-link anchors, view-raw link.
		`class="bundle-layout"`,
		`class="bundle-toc-rail"`,
		`href="#doc-gd-doc-aaa"`,
		`href="#doc-gd-doc-bbb"`,
		`href="#doc-gd-doc-ccc"`,
		`data-doc-id="gd-doc-aaa"`,
		`class="bundle-doc-raw"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("populated body missing %q", want)
		}
	}

	// Verify TOC + card order: First < Second < Third.
	idxFirst := strings.Index(body, "First doc headline")
	idxSecond := strings.Index(body, "Second doc headline")
	idxThird := strings.Index(body, "Third doc headline")
	if !(idxFirst >= 0 && idxFirst < idxSecond && idxSecond < idxThird) {
		t.Errorf("expected position-asc card order, got first=%d second=%d third=%d",
			idxFirst, idxSecond, idxThird)
	}
}

// Sanity-check: a registered renderer is reachable via lookup so T5 can wire
// per-format renderers (md, diff, csv, …) without further plumbing.
func TestBundleRendererRegistry_RoundTrip(t *testing.T) {
	const slug = "bundle-test-only-format"
	registerBundleRenderer(slug, func(card bundleDocCard, ctx *bundleRenderCtx) (template.HTML, error) {
		return template.HTML("<em>" + card.BeadID + "</em>"), nil
	})
	r, ok := lookupBundleRenderer(slug)
	if !ok || r == nil {
		t.Fatal("registered renderer not retrievable")
	}
	out, err := r(bundleDocCard{BeadID: "gd-doc-test"}, nil)
	if err != nil {
		t.Fatalf("renderer returned error: %v", err)
	}
	if string(out) != "<em>gd-doc-test</em>" {
		t.Errorf("renderer output=%q, want injected html", string(out))
	}
}

func TestBuildBundleSummaryView_FiltersAndOrdersChildren(t *testing.T) {
	parent := &beadsapi.BeadDetail{
		ID:    "gd-g0xoawomrs",
		Type:  "bundle",
		Title: "Active Incident: post-mortem",
	}

	reverseDeps := []beadsapi.Dependency{
		// doc child, position 2 — second.
		{BeadID: "gd-doc00000002", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
		// doc child, position 1 — first.
		{BeadID: "gd-doc00000001", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
		// `related` dep — must be ignored.
		{BeadID: "gd-doc00000003", DependsOnID: "gd-g0xoawomrs", Type: "related"},
		// parent-child task — child type filtered out post-fetch.
		{BeadID: "gd-task00000001", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
	}

	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-doc00000001": {
			ID:    "gd-doc00000001",
			Type:  "doc",
			Title: "Raw title 1",
			Fields: map[string]string{
				"format":   "md",
				"position": "1",
				"headline": "Plan overview",
			},
		},
		"gd-doc00000002": {
			ID:    "gd-doc00000002",
			Type:  "doc",
			Title: "Raw title 2",
			Fields: map[string]string{
				"format":   "diff",
				"position": "2",
			},
		},
		"gd-doc00000003": {
			ID:     "gd-doc00000003",
			Type:   "doc",
			Title:  "ignored related",
			Fields: map[string]string{"format": "md"},
		},
		"gd-task00000001": {
			ID:    "gd-task00000001",
			Type:  "task",
			Title: "ignored task",
		},
	}}

	v := buildBundleSummaryView(context.Background(), parent, reverseDeps, fake, "")
	if v == nil {
		t.Fatal("buildBundleSummaryView returned nil")
	}
	if v.CTAURL != "/bundle/gd-g0xoawomrs" {
		t.Errorf("CTAURL=%q, want /bundle/gd-g0xoawomrs", v.CTAURL)
	}
	if got, want := len(v.Docs), 2; got != want {
		t.Fatalf("got %d docs, want %d (filter dropped wrong rows)", got, want)
	}
	if v.Docs[0].BeadID != "gd-doc00000001" || v.Docs[1].BeadID != "gd-doc00000002" {
		t.Errorf("ordering wrong: %s, %s — expected position-asc",
			v.Docs[0].BeadID, v.Docs[1].BeadID)
	}
	if v.Docs[0].Title != "Plan overview" {
		t.Errorf("first doc title=%q, want headline-override 'Plan overview'", v.Docs[0].Title)
	}
	if v.Docs[1].Title != "Raw title 2" {
		t.Errorf("second doc title=%q, want fallback to bead Title", v.Docs[1].Title)
	}
	if v.Docs[0].Format != "md" || v.Docs[1].Format != "diff" {
		t.Errorf("formats not propagated: %q, %q", v.Docs[0].Format, v.Docs[1].Format)
	}
}

func TestBuildBundleSummaryView_BasePath(t *testing.T) {
	parent := &beadsapi.BeadDetail{ID: "gd-bun", Type: "bundle"}
	v := buildBundleSummaryView(context.Background(), parent, nil, &fakeDaemon{}, "/beads-viewer")
	if v == nil {
		t.Fatal("nil view")
	}
	if v.CTAURL != "/beads-viewer/bundle/gd-bun" {
		t.Errorf("CTAURL=%q, want /beads-viewer/bundle/gd-bun", v.CTAURL)
	}
}

func TestBuildBundleSummaryView_DegradesOnFetchError(t *testing.T) {
	parent := &beadsapi.BeadDetail{ID: "gd-bun", Type: "bundle"}

	reverseDeps := []beadsapi.Dependency{
		{BeadID: "gd-doc00000001", DependsOnID: "gd-bun", Type: "parent-child"},
	}
	// Fake with no matching bead → GetBead returns an APIError.
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{}}

	v := buildBundleSummaryView(context.Background(), parent, reverseDeps, fake, "")
	if v == nil {
		t.Fatal("nil view")
	}
	if got, want := len(v.Docs), 1; got != want {
		t.Fatalf("got %d docs, want %d (fetch error must not drop the row)", got, want)
	}
	if v.Docs[0].Title != "gd-doc00000001" {
		t.Errorf("placeholder title=%q, want fallback to bead ID", v.Docs[0].Title)
	}
}

func TestBeadShow_BundleType_RendersTOC(t *testing.T) {
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-bun": {
				ID:     "gd-bun",
				Title:  "Plan: agent-artifact-bundles",
				Type:   "bundle",
				Status: "open",
				Fields: map[string]string{
					"kind":     "plan",
					"headline": "Multi-doc viewer rollout",
				},
			},
			"gd-doc00000001": {
				ID:    "gd-doc00000001",
				Type:  "doc",
				Title: "Overview",
				Fields: map[string]string{
					"format":   "md",
					"position": "1",
				},
			},
			"gd-doc00000002": {
				ID:    "gd-doc00000002",
				Type:  "doc",
				Title: "Diff",
				Fields: map[string]string{
					"format":   "diff",
					"position": "2",
				},
			},
		},
		revDeps: map[string][]beadsapi.Dependency{
			"gd-bun": {
				{BeadID: "gd-doc00000002", DependsOnID: "gd-bun", Type: "parent-child"},
				{BeadID: "gd-doc00000001", DependsOnID: "gd-bun", Type: "parent-child"},
			},
		},
	}
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// Bundle beads default-redirect to the immersive doc view at /bundle/<id>.
	req := httptest.NewRequest("GET", "/gd-bun", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect for bundle bead, got %d", w.Code)
	}
	if got, want := w.Header().Get("Location"), "/bundle/gd-bun"; got != want {
		t.Errorf("redirect Location=%q, want %q", got, want)
	}

	// `?bead=1` escape returns the raw bead-metadata page with the summary
	// partial — link surfaced from the bundle.html "View bead" affordance.
	req = httptest.NewRequest("GET", "/gd-bun?bead=1", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for ?bead=1, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`class="bundle-summary"`,
		"Overview",
		"Diff",
		"gd-doc00000001",
		"gd-doc00000002",
		`href="/bundle/gd-bun"`,
		"Open as document",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered bundle missing %q", want)
		}
	}
	if strings.Contains(body, "No documents yet.") {
		t.Errorf("non-empty bundle should not show empty placeholder")
	}
	// TOC ordering: position 1 (Overview) must come before position 2 (Diff).
	overview := strings.Index(body, "gd-doc00000001")
	diff := strings.Index(body, "gd-doc00000002")
	if overview < 0 || diff < 0 || overview > diff {
		t.Errorf("TOC ordering wrong: overview at %d, diff at %d", overview, diff)
	}
}

func TestBeadShow_BundleType_EmptyRendersPlaceholder(t *testing.T) {
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-bun": {
				ID:     "gd-bun",
				Title:  "Empty bundle",
				Type:   "bundle",
				Status: "open",
				Fields: map[string]string{"kind": "plan"},
			},
		},
		// No mentionedBy → no doc children.
	}
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/gd-bun?bead=1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No documents yet.") {
		t.Errorf("empty bundle should render placeholder, got body: %s", body)
	}
	if !strings.Contains(body, `href="/bundle/gd-bun"`) {
		t.Errorf("CTA should still render even when bundle is empty")
	}
}

func TestBeadShow_BundleType_RedirectsToImmersive(t *testing.T) {
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-bun": {ID: "gd-bun", Title: "B", Type: "bundle", Status: "open"},
		},
	}
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// Default: bundle bead URL redirects to /bundle/<id>.
	req := httptest.NewRequest("GET", "/gd-bun", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if got, want := w.Header().Get("Location"), "/bundle/gd-bun"; got != want {
		t.Errorf("Location=%q, want %q", got, want)
	}

	// ?bead=1 escape skips the redirect.
	req = httptest.NewRequest("GET", "/gd-bun?bead=1", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for ?bead=1, got %d", w.Code)
	}
}

// TestBeadShow_PlanEpic_RedirectsToPlanCockpit asserts that a plan bead (an
// epic carrying the `plan` label) opened at /{id} default-redirects to the
// immersive plan cockpit at /plans/{id} (Matt, ka-0000001056), and that the
// `?bead=1` escape returns the raw bead page instead. A plain epic WITHOUT the
// plan label is unaffected — it renders as a normal bead.
func TestBeadShow_PlanEpic_RedirectsToPlanCockpit(t *testing.T) {
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-plan": {ID: "gd-plan", Title: "P", Type: "epic", Status: "open", Labels: []string{"project:gardenio", "plan"}},
			"gd-epic": {ID: "gd-epic", Title: "E", Type: "epic", Status: "open", Labels: []string{"project:gardenio"}},
		},
	}
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// Default: a plan-labeled epic redirects to /plans/<id>.
	req := httptest.NewRequest("GET", "/gd-plan", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 for plan epic, got %d", w.Code)
	}
	if got, want := w.Header().Get("Location"), "/plans/gd-plan"; got != want {
		t.Errorf("Location=%q, want %q", got, want)
	}

	// ?bead=1 escape skips the redirect and renders the raw bead page.
	req = httptest.NewRequest("GET", "/gd-plan?bead=1", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for ?bead=1, got %d", w.Code)
	}

	// A plain epic (no plan label) is NOT redirected — it renders normally.
	req = httptest.NewRequest("GET", "/gd-epic", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-plan epic, got %d", w.Code)
	}
}

// TestBuildBundleView_BatchesChildFetch asserts the N+1 refactor (gd-dfLn28ZU8O):
// a bundle with ≥3 children must drive exactly one BatchGetBeads call and zero
// per-child GetBead calls.
func TestBuildBundleView_BatchesChildFetch(t *testing.T) {
	bundle := &beadsapi.BeadDetail{ID: "gd-g0xoawomrs", Type: "bundle", Title: "T"}
	reverseDeps := []beadsapi.Dependency{
		{BeadID: "gd-doc00000001", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
		{BeadID: "gd-doc00000002", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
		{BeadID: "gd-doc00000003", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
		// Dedup: this id repeats, must NOT inflate the batch.
		{BeadID: "gd-doc00000001", DependsOnID: "gd-g0xoawomrs", Type: "parent-child"},
	}
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-doc00000001": {ID: "gd-doc00000001", Type: "doc", Fields: map[string]string{"format": "md", "position": "1"}},
		"gd-doc00000002": {ID: "gd-doc00000002", Type: "doc", Fields: map[string]string{"format": "md", "position": "2"}},
		"gd-doc00000003": {ID: "gd-doc00000003", Type: "doc", Fields: map[string]string{"format": "md", "position": "3"}},
	}}

	v := buildBundleView(context.Background(), bundle, nil, reverseDeps, fake, nil)
	if v == nil || len(v.Docs) != 3 {
		t.Fatalf("expected 3 docs, got %+v", v)
	}
	if fake.batchGetBeadsCalls != 1 {
		t.Errorf("BatchGetBeads calls=%d, want 1", fake.batchGetBeadsCalls)
	}
	if fake.batchGetBeadsLastN != 3 {
		t.Errorf("BatchGetBeads last shard size=%d, want 3 (dedup must collapse the repeat)", fake.batchGetBeadsLastN)
	}
	if fake.getBeadCalls != 0 {
		t.Errorf("GetBead calls=%d, want 0 (N+1 must be gone)", fake.getBeadCalls)
	}
}

// TestBuildBundleSummaryView_BatchesChildFetch is the summary-view twin of
// TestBuildBundleView_BatchesChildFetch.
func TestBuildBundleSummaryView_BatchesChildFetch(t *testing.T) {
	parent := &beadsapi.BeadDetail{ID: "gd-bun", Type: "bundle"}
	reverseDeps := []beadsapi.Dependency{
		{BeadID: "gd-doc00000001", DependsOnID: "gd-bun", Type: "parent-child"},
		{BeadID: "gd-doc00000002", DependsOnID: "gd-bun", Type: "parent-child"},
		{BeadID: "gd-doc00000003", DependsOnID: "gd-bun", Type: "parent-child"},
	}
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-doc00000001": {ID: "gd-doc00000001", Type: "doc", Fields: map[string]string{"position": "1"}},
		"gd-doc00000002": {ID: "gd-doc00000002", Type: "doc", Fields: map[string]string{"position": "2"}},
		"gd-doc00000003": {ID: "gd-doc00000003", Type: "doc", Fields: map[string]string{"position": "3"}},
	}}

	v := buildBundleSummaryView(context.Background(), parent, reverseDeps, fake, "")
	if v == nil || len(v.Docs) != 3 {
		t.Fatalf("expected 3 docs, got %+v", v)
	}
	if fake.batchGetBeadsCalls != 1 {
		t.Errorf("BatchGetBeads calls=%d, want 1", fake.batchGetBeadsCalls)
	}
	if fake.getBeadCalls != 0 {
		t.Errorf("GetBead calls=%d, want 0 (N+1 must be gone)", fake.getBeadCalls)
	}
}

// TestBuildBundleView_KeepsExistingBlankDoc pins the render-guard contract for
// ka-0000003524: a doc bead that EXISTS but has no content/content_url (the
// mis-author that wrote its body to `description`) must be KEPT in the view — not
// dropped like a deleted doc — so the template renders a visible, bead-naming
// placeholder instead of the reader seeing nothing. The BeadID must survive so
// the placeholder can name it.
func TestBuildBundleView_KeepsExistingBlankDoc(t *testing.T) {
	bundle := &beadsapi.BeadDetail{ID: "gd-0hwiram2qr", Type: "bundle", Title: "Blank doc bundle"}
	reverseDeps := []beadsapi.Dependency{
		{BeadID: "gd-blank0000001", DependsOnID: "gd-0hwiram2qr", Type: "parent-child"},
	}
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-blank0000001": {
			ID:   "gd-blank0000001",
			Type: "doc",
			// The mis-author: body in description, no fields.content.
			Description: "# FSM Triage Report Card\n\nlots of markdown that never renders",
			Fields:      map[string]string{"format": "md"},
		},
	}}

	v := buildBundleView(context.Background(), bundle, nil, reverseDeps, fake, nil)
	if v == nil || len(v.Docs) != 1 {
		t.Fatalf("existing blank doc must be kept (not dropped like a deleted doc), got %+v", v)
	}
	if v.Docs[0].BeadID != "gd-blank0000001" {
		t.Errorf("blank doc BeadID must survive for the placeholder to name it, got %q", v.Docs[0].BeadID)
	}
	if v.Docs[0].Content != "" || v.Docs[0].ContentURL != "" {
		t.Errorf("blank doc should have no content payload, got content=%q url=%q", v.Docs[0].Content, v.Docs[0].ContentURL)
	}
	if v.Docs[0].RenderedHTML != "" {
		t.Errorf("blank doc should not produce rendered HTML (drives the placeholder branch), got %q", v.Docs[0].RenderedHTML)
	}
}
