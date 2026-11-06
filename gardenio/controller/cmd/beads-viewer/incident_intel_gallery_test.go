package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIncidentIntelGallery_Renders verifies the Phase 1 primitive gallery
// loads end-to-end and exercises each of the eight building blocks. A render
// regression here means downstream widget authors lose their reference doc.
// See bead ka-0000007090.
func TestIncidentIntelGallery_Renders(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/dev/incident-intel/gallery", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /dev/incident-intel/gallery, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Each primitive should leave a fingerprint on the page so a regression
	// in any one of them is caught here, not later in widget builds.
	for _, want := range []string{
		// 1. severity + confidence pills
		`data-severity="p1"`,
		`data-confidence="high"`,
		// 2. source-kind chips — must include all eight kinds
		`data-source-kind="mezmo"`,
		`data-source-kind="victoria-metrics"`,
		`data-source-kind="victoria-traces"`,
		`data-source-kind="sentry"`,
		`data-source-kind="jira"`,
		`data-source-kind="gitlab"`,
		`data-source-kind="code"`,
		`data-source-kind="slack"`,
		// 3. letter-key bus scope
		`data-ii-scope="gallery-keys"`,
		// 4. relative-time
		`class="ii-relative-time"`,
		// 5. confirm modal demo
		`id="ii-confirm-demo"`,
		`id="ii-confirm-danger-demo"`,
		// 6. drawer demo
		`id="ii-drawer-demo"`,
		// 7. filter-chip strip
		`data-ii-chip-strip`,
		// 8. state pill (covered by data-severity above; also danger semantics)
		`ii-pill`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("gallery body missing fingerprint %q", want)
		}
	}
}

// TestIncidentIntelGallery_Phase5SectionRenders confirms the Phase 5 cards
// surface in the dev gallery — both widgets dispatched, with chroma highlight
// signals and the source-kind chip used by the RCA card. Bead: ka-0000004643.
func TestIncidentIntelGallery_Phase5SectionRenders(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/dev/incident-intel/gallery", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Phase 5 — code-highlighted widgets",
		`data-widget-type="rca-evidence-cards"`,
		`data-widget-type="source-code-analysis"`,
		// chroma fingerprints — both widgets surface highlighted code in
		// their fixtures, so a regression in the highlighter wiring shows
		// up here.
		"background-color:",
		`<span style="color:`,
		// Phase 1 source-kind chip used by the RCA card fixture.
		`data-source-kind="mezmo"`,
		// Drilldown drawer scaffolding from the first card.
		"ii-w-rca-drill",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Phase 5 gallery body missing %q", want)
		}
	}
}

// TestIncidentIntelGallery_Phase6SectionRenders confirms the diff widget
// surfaces in the dev gallery with the chroma + diff classes wired up.
// Bead: ka-0000005934.
func TestIncidentIntelGallery_Phase6SectionRenders(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/dev/incident-intel/gallery", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Phase 6 — diff widget",
		`data-widget-type="fix-available-pending-branch"`,
		"ii-w-pending-branch",
		"ii-w-diff-add",
		"ii-w-diff-del",
		`data-state="passed"`,
		"fast-track-merge",
		// chroma fingerprint — the sample diff is Go and the highlighter
		// emits inline color spans we can detect.
		`<span style="color:`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Phase 6 gallery body missing %q", want)
		}
	}
}

// TestStaticAsset_ServesIncidentIntelCSS confirms the Phase 1 CSS bundle is
// reachable via the same /assets/static/ route used by every other static
// asset. layout.html links it from the global <head>, so a missing asset
// would unstyle every page that uses incident-intel primitives.
func TestStaticAsset_ServesIncidentIntelCSS(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/incident_intel.css", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"--ii-sev-p1-bg",
		"--ii-src-mezmo",
		".ii-pill",
		".ii-source",
		".ii-drawer",
		".ii-modal",
		".ii-chip",
		".ii-key",
		".ii-relative-time",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("incident_intel.css missing %q", want)
		}
	}
}

// TestStaticAsset_ServesIncidentIntelJS confirms the Phase 1 JS bundle is
// reachable. layout.html includes it as a deferred script — a missing asset
// breaks letter-key dispatch, drawer/confirm/filter primitives at runtime.
func TestStaticAsset_ServesIncidentIntelJS(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/incident_intel.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"window.IncidentIntel",
		"registerKey",
		"confirmModal",
		"openDrawer",
		"refreshRelativeTimes",
		"bindChipStrips",
		"bindDrawerKeys",
		"sourceChip",
		"RESERVED_KEYS",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("incident_intel.js missing exported symbol %q", want)
		}
	}
}

// TestLayout_LinksIncidentIntelAssets ensures the global layout pulls in
// both the CSS and JS bundles. Without these <link>/<script> tags the
// primitives would render unstyled and untriggered on every page.
func TestLayout_LinksIncidentIntelAssets(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/assets/static/incident_intel.css") {
		t.Errorf("layout missing incident_intel.css link tag")
	}
	if !strings.Contains(body, "/assets/static/incident_intel.js") {
		t.Errorf("layout missing incident_intel.js script tag")
	}
}
