package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// newLiveFake builds a fakeDaemon seeded with one widget-instance bead per
// live-allowlist slug. Each bead's `data` field carries a minimal payload the
// renderer accepts.
func newLiveFake() *fakeDaemon {
	mk := func(id, slug, data string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID:   id,
			Type: "widget-instance",
			Fields: map[string]string{
				"widget_type": slug,
				"data":        data,
			},
		}
	}
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-live-header": mk("gd-live-header", "header-incident-summary",
				`{"title":"live header","severity":"P1","status":"open","started_at":"2026-05-06T12:00:00Z"}`),
			"gd-live-svc": mk("gd-live-svc", "service-health",
				`{"services":[{"service":"study-web","state":"critical","detail":"5xx"}]}`),
			"gd-live-act": mk("gd-live-act", "activity-log",
				`{"entries":[{"when":"2026-05-06T12:00:00Z","actor":"a","kind":"comment","text":"x"}]}`),
			"gd-live-tl": mk("gd-live-tl", "event-timeline",
				`{"events":[{"when":"2026-05-06T12:00:00Z","kind":"mezmo","title":"a","service":"study-web"},{"when":"2026-05-06T12:01:00Z","kind":"jira","title":"b","service":"sponsor-api"}]}`),
		},
	}
}

func TestLiveAllowlist_FourWidgets(t *testing.T) {
	got := liveAllowlist()
	for _, want := range []string{
		"header-incident-summary",
		"service-health",
		"activity-log",
		"event-timeline",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("liveAllowlist missing %q: %v", want, got)
		}
	}
}

func TestHandleLiveWidget_RendersFragment(t *testing.T) {
	mux := mkServer(t, newLiveFake())
	req := httptest.NewRequest("GET", "/widgets/service-health/gd-live-svc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, "study-web") || !strings.Contains(body, `data-state="critical"`) {
		t.Errorf("expected service-health fragment, got: %s", body)
	}
}

func TestHandleLiveWidget_404OnUnknownSlug(t *testing.T) {
	mux := mkServer(t, newLiveFake())
	req := httptest.NewRequest("GET", "/widgets/affected-files/gd-live-svc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-allowlisted slug, got %d", w.Code)
	}
}

func TestHandleLiveWidget_404OnUnknownBead(t *testing.T) {
	mux := mkServer(t, newLiveFake())
	req := httptest.NewRequest("GET", "/widgets/service-health/gd-does-not-exist", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	// Daemon error → live-error chip (still 200; htmx retries).
	if w.Code != http.StatusOK {
		t.Errorf("want 200 with error chip, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "ii-w-live-error") || !strings.Contains(body, "bead unavailable") {
		t.Errorf("expected error chip in body: %s", body)
	}
}

func TestHandleLiveWidget_NoDataReturnsErrorChip(t *testing.T) {
	fake := newLiveFake()
	// strip the data field to exercise the empty-data branch.
	fake.beads["gd-live-svc"].Fields["data"] = ""
	mux := mkServer(t, fake)
	req := httptest.NewRequest("GET", "/widgets/service-health/gd-live-svc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "no data on widget bead") {
		t.Errorf("expected empty-data chip, got: %s", w.Body.String())
	}
}

func TestHandleLiveWidget_ServerSideTimelineFilter(t *testing.T) {
	mux := mkServer(t, newLiveFake())
	req := httptest.NewRequest("GET", "/widgets/event-timeline/gd-live-tl?filter=source-kind:mezmo", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// 'a' (mezmo) must appear; 'b' (jira) must be filtered out by server-side filter.
	if !strings.Contains(body, ">a</span>") && !strings.Contains(body, "ii-w-event-timeline-event-title\">a") {
		t.Errorf("expected mezmo event 'a' in body: %s", body)
	}
	if strings.Contains(body, "ii-w-event-timeline-event-title\">b") {
		t.Errorf("jira event 'b' should have been filtered out; body: %s", body)
	}
}

func TestLiveAttrs_AllowlistedAndAbsent(t *testing.T) {
	got := liveAttrs("service-health", "gd-x", "")
	for _, want := range []string{
		`hx-get="/widgets/service-health/gd-x"`,
		`hx-trigger="every 5s"`,
		`hx-swap="innerHTML"`,
		"hx-vals=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("liveAttrs missing %q in: %s", want, got)
		}
	}
	if liveAttrs("affected-files", "gd-x", "") != "" {
		t.Error("non-allowlisted slug should return empty attrs")
	}
	if liveAttrs("service-health", "", "") != "" {
		t.Error("empty bead id should return empty attrs")
	}
	// basePath prefixed.
	if !strings.Contains(liveAttrs("service-health", "gd-x", "/bv"), `hx-get="/bv/widgets/service-health/gd-x"`) {
		t.Error("basePath should prefix the URL")
	}
}

func TestItoa(t *testing.T) {
	cases := map[int]string{
		0: "0", 1: "1", 5: "5", 10: "10", 99: "99", 1000: "1000", -7: "-7",
	}
	for in, want := range cases {
		if got := itoa(in); got != want {
			t.Errorf("itoa(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestIncidentIntelGallery_Phase8SectionRenders(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/dev/incident-intel/gallery", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Phase 8 — live updates + cross-filter v2",
		`id="ii-phase8-chips"`,
		`data-value="service:study-web"`,
		`data-value="source-kind:mezmo"`,
		`data-widget-type="header-incident-summary"`,
		`data-widget-type="service-health"`,
		`data-widget-type="event-timeline"`,
		`incident_intel_crossfilter.js`,
		// allowlist surfaced into the gallery copy
		"event-timeline",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Phase 8 gallery body missing %q", want)
		}
	}
}

func TestLayout_LinksCrossFilterScript(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "/assets/static/incident_intel_crossfilter.js") {
		t.Errorf("layout missing crossfilter script tag")
	}
}

func TestStaticAsset_ServesCrossFilterJS(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/incident_intel_crossfilter.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"IncidentIntel.crossFilter",
		"serialize",
		"applyTo",
		"ii:crossfilter-changed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("crossfilter.js missing %q", want)
		}
	}
}

func TestEscapeText(t *testing.T) {
	in := `<script>alert("x")&y`
	want := `&lt;script&gt;alert(&quot;x&quot;)&amp;y`
	if got := escapeText(in); got != want {
		t.Errorf("escapeText: got %q want %q", got, want)
	}
}
