package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/cmd/beads-viewer/viewer"
	"gardenio/controller/internal/beadsapi"
)

type fakeAliasLookup struct {
	resp *beadsapi.LookupOperatorByAliasResponse
	err  error
}

func (f *fakeAliasLookup) LookupOperatorByAlias(_ context.Context, _, _ string) (*beadsapi.LookupOperatorByAliasResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// viewerResolverFor builds a viewer.Resolver whose email extractor always
// returns the given email (empty = anonymous), backed by the supplied alias
// response.
func viewerResolverFor(email string, resp *beadsapi.LookupOperatorByAliasResponse) *viewer.Resolver {
	return viewer.NewResolver(&fakeAliasLookup{resp: resp},
		viewer.WithEmailExtractor(func(context.Context) string { return email }),
	)
}

// newTestTelemetry registers a fresh viewTelemetry against a private
// registry so tests don't collide on the global default registry.
func newTestTelemetry(t *testing.T, resolver *viewer.Resolver, project string) (*viewTelemetry, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	vt, err := newViewTelemetry(reg, resolver, project)
	if err != nil {
		t.Fatalf("newViewTelemetry: %v", err)
	}
	return vt, reg
}

// serveThrough builds a small mux mirroring the acceptance routes, wraps it
// in the telemetry middleware, and serves one request. Returns the recorder.
func serveThrough(vt *viewTelemetry, req *http.Request) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /u/{slug}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /inbox", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /search", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /active", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /decisions", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /advice", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /costs", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /schedules", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }) // bead-show
	mux.HandleFunc("POST /b/{id}/comments", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	h := vt.Middleware(mux)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func provisionedResp(id int64) *beadsapi.LookupOperatorByAliasResponse {
	return &beadsapi.LookupOperatorByAliasResponse{
		OperatorID:   id,
		OperatorKind: "human",
		Found:        true,
	}
}

// Every acceptance route must increment the view counter with
// activity_type=view, surface=web, and the resolved operator_id.
func TestViewCounter_IncrementsOnEveryAcceptanceRoute(t *testing.T) {
	routes := []string{
		"/u/matthew.baker",
		"/inbox",
		"/search",
		"/agents",
		"/active",
		"/decisions",
		"/advice",
		"/costs",
		"/schedules",
		"/gd-ABC123", // bead-show
	}
	vt, _ := newTestTelemetry(t, viewerResolverFor("matthew.baker@acme.example", provisionedResp(7)), "gardenio")
	for _, path := range routes {
		serveThrough(vt, httptest.NewRequest(http.MethodGet, path, nil))
	}
	got := testutil.ToFloat64(vt.activityTotal.WithLabelValues("7", "gardenio", activityTypeView, surfaceWeb))
	if int(got) != len(routes) {
		t.Fatalf("view counter = %v, want %d (one per acceptance route)", got, len(routes))
	}
}

// The view counter must be labelled with the operator_id FK, never the email
// or slug. Verify the resolved id is "7" and that no email/slug substring
// appears anywhere in the exported metric text.
func TestViewCounter_LabelsOperatorIDNotEmail(t *testing.T) {
	vt, reg := newTestTelemetry(t, viewerResolverFor("matthew.baker@acme.example", provisionedResp(7)), "gardenio")
	serveThrough(vt, httptest.NewRequest(http.MethodGet, "/u/matthew.baker", nil))

	if got := testutil.ToFloat64(vt.activityTotal.WithLabelValues("7", "gardenio", activityTypeView, surfaceWeb)); int(got) != 1 {
		t.Fatalf("operator_id=7 counter = %v, want 1", got)
	}
	assertNoPII(t, reg)
}

// Anonymous (no email in context) and unprovisioned (no alias) viewers roll
// up to the operator_id="0" sentinel bucket rather than being dropped.
func TestViewCounter_UnattributedFallback(t *testing.T) {
	cases := map[string]*viewTelemetry{}
	anon, _ := newTestTelemetry(t, viewerResolverFor("", provisionedResp(7)), "gardenio")
	cases["anonymous"] = anon
	unprov, _ := newTestTelemetry(t, viewerResolverFor("nobody@acme.example", &beadsapi.LookupOperatorByAliasResponse{Found: false}), "gardenio")
	cases["unprovisioned"] = unprov
	nilres, _ := newTestTelemetry(t, nil, "gardenio")
	cases["nil-resolver"] = nilres

	for name, vt := range cases {
		t.Run(name, func(t *testing.T) {
			serveThrough(vt, httptest.NewRequest(http.MethodGet, "/inbox", nil))
			got := testutil.ToFloat64(vt.activityTotal.WithLabelValues(operatorIDUnattributed, "gardenio", activityTypeView, surfaceWeb))
			if int(got) != 1 {
				t.Fatalf("operator_id=%q counter = %v, want 1", operatorIDUnattributed, got)
			}
		})
	}
}

// A sentinel-kind alias (system/fleet) must NOT be attributed to the viewer;
// it falls back to operator_id="0".
func TestViewCounter_SentinelOperatorFallsBackToZero(t *testing.T) {
	vt, _ := newTestTelemetry(t, viewerResolverFor("svc@acme.example", &beadsapi.LookupOperatorByAliasResponse{OperatorID: 1, OperatorKind: "system", Found: true}), "gardenio")
	serveThrough(vt, httptest.NewRequest(http.MethodGet, "/inbox", nil))
	if got := testutil.ToFloat64(vt.activityTotal.WithLabelValues(operatorIDUnattributed, "gardenio", activityTypeView, surfaceWeb)); int(got) != 1 {
		t.Fatalf("sentinel should map to operator_id=0; got bucket count %v", got)
	}
}

// The latency histogram must be labelled with the bounded route TEMPLATE,
// never the raw URL. Hitting /u/matthew.baker records endpoint="GET /u/{slug}".
func TestLatencyHistogram_UsesRouteTemplateNotRawURL(t *testing.T) {
	vt, reg := newTestTelemetry(t, viewerResolverFor("matthew.baker@acme.example", provisionedResp(7)), "gardenio")
	serveThrough(vt, httptest.NewRequest(http.MethodGet, "/u/matthew.baker", nil))

	count := testutil.CollectAndCount(vt.requestLatency)
	if count == 0 {
		t.Fatal("latency histogram recorded no observations")
	}
	body := dumpRegistry(t, reg)
	if !strings.Contains(body, `endpoint="GET /u/{slug}"`) {
		t.Fatalf("expected bounded template label endpoint=\"GET /u/{slug}\" in metrics:\n%s", body)
	}
	if strings.Contains(body, "matthew.baker") {
		t.Fatalf("raw slug leaked into latency labels:\n%s", body)
	}
}

// The latency histogram records the status_code label.
func TestLatencyHistogram_RecordsStatusCode(t *testing.T) {
	vt, reg := newTestTelemetry(t, viewerResolverFor("matthew.baker@acme.example", provisionedResp(7)), "gardenio")
	serveThrough(vt, httptest.NewRequest(http.MethodGet, "/search", nil))
	body := dumpRegistry(t, reg)
	if !strings.Contains(body, `status_code="200"`) {
		t.Fatalf("expected status_code=\"200\" in latency labels:\n%s", body)
	}
}

// A POST (write/mutation) is NOT a page view: latency is still recorded for
// surface health, but the view counter must not increment.
func TestViewCounter_SkipsNonGET(t *testing.T) {
	vt, _ := newTestTelemetry(t, viewerResolverFor("matthew.baker@acme.example", provisionedResp(7)), "gardenio")
	serveThrough(vt, httptest.NewRequest(http.MethodPost, "/b/gd-X/comments", nil))

	if got := testutil.CollectAndCount(vt.activityTotal); got != 0 {
		t.Fatalf("view counter should not increment on POST; got %d series", got)
	}
	if got := testutil.CollectAndCount(vt.requestLatency); got == 0 {
		t.Fatal("latency histogram should still record POST requests")
	}
}

// htmx partial fetches and SSE streams are not counted as page views (they
// refresh / stream a page already counted on its initial navigation).
func TestViewCounter_SkipsHTMXAndSSE(t *testing.T) {
	vt, _ := newTestTelemetry(t, viewerResolverFor("matthew.baker@acme.example", provisionedResp(7)), "gardenio")

	htmx := httptest.NewRequest(http.MethodGet, "/inbox", nil)
	htmx.Header.Set("HX-Request", "true")
	serveThrough(vt, htmx)

	sse := httptest.NewRequest(http.MethodGet, "/inbox", nil)
	sse.Header.Set("Accept", "text/event-stream")
	serveThrough(vt, sse)

	if got := testutil.CollectAndCount(vt.activityTotal); got != 0 {
		t.Fatalf("htmx/SSE must not increment the view counter; got %d series", got)
	}
}

// Empty project label collapses to "unknown".
func TestProjectLabel_DefaultsToUnknown(t *testing.T) {
	vt, _ := newTestTelemetry(t, viewerResolverFor("matthew.baker@acme.example", provisionedResp(7)), "")
	serveThrough(vt, httptest.NewRequest(http.MethodGet, "/agents", nil))
	if got := testutil.ToFloat64(vt.activityTotal.WithLabelValues("7", "unknown", activityTypeView, surfaceWeb)); int(got) != 1 {
		t.Fatalf("empty project should label \"unknown\"; got %v", got)
	}
}

// Whole-registry PII guard: no email/slug/slack id may appear in any
// exported label across BOTH metrics, for several operator identities.
func TestNoPIIInAnyLabel(t *testing.T) {
	vt, reg := newTestTelemetry(t, viewerResolverFor("matthew.baker@acme.example", provisionedResp(7)), "gardenio")
	for _, p := range []string{"/u/matthew.baker", "/search", "/gd-SECRET", "/inbox"} {
		serveThrough(vt, httptest.NewRequest(http.MethodGet, p, nil))
	}
	assertNoPII(t, reg)
}

// assertNoPII fails if the exported metric text contains any of the known
// PII shapes we forbid as label values.
func assertNoPII(t *testing.T, reg *prometheus.Registry) {
	t.Helper()
	body := dumpRegistry(t, reg)
	forbidden := []string{
		"matthew.baker", // slug / email local-part
		"@acme.example",  // email domain
		"acme.example",   // email domain (bare)
		"gd-SECRET",     // raw bead id in a label (only allowed via route template {id})
		"U07",           // slack user id prefix
	}
	for _, f := range forbidden {
		if strings.Contains(body, f) {
			t.Fatalf("forbidden PII-shaped substring %q found in exported metrics:\n%s", f, body)
		}
	}
}

// dumpRegistry renders the registry to the Prometheus text exposition format.
func dumpRegistry(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sb strings.Builder
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			sb.WriteString(mf.GetName())
			sb.WriteString("{")
			for _, lp := range m.GetLabel() {
				sb.WriteString(lp.GetName())
				sb.WriteString(`="`)
				sb.WriteString(lp.GetValue())
				sb.WriteString(`" `)
			}
			sb.WriteString("}\n")
		}
	}
	return sb.String()
}
