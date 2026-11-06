package main

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/cmd/beads-viewer/viewer"
)

// View telemetry — Customer-1 user-engagement instrumentation, Phase 1
// item §2/§7.3 of `gardenio/docs/research/customer-1-telemetry-plan.md`
// (epic ka-0000007629, task ka-0000006752).
//
// This promotes the three-surface `homepage.*` slog events in
// homepage_telemetry.go to a whole-app Prometheus counter. The slog
// approach instrumented exactly three page views (user_home / inbox /
// search) keyed on `viewer_email`; this middleware covers EVERY navigable
// page route and labels on the opaque `operator_id` FK instead — email is
// resolved to `operator_id` server-side via the shared viewer.Resolver and
// never appears in a label (plan §1.4 / §4 privacy boundary).
//
// Two metrics are emitted:
//
//   - gardenio_operator_activity_total{operator_id, project, activity_type,
//     surface} — the backbone engagement counter. This middleware only ever
//     emits the {activity_type=view, surface=web} slice. NULL/unprovisioned/
//     anonymous viewers roll up to the reserved operator_id="0" bucket
//     (plan §3.2) rather than being dropped, so DAU/WAU/MAU denominators stay
//     honest. The counter carries NO endpoint/email/slug label — engagement
//     is per-human, not per-URL.
//
//   - gardenio_user_request_latency_seconds{surface, endpoint, status_code} —
//     surface-health histogram. Labelled by the BOUNDED route TEMPLATE
//     (`net/http.ServeMux` pattern, e.g. "GET /u/{slug}"), never the raw URL,
//     so a path like /u/matthew.baker can never leak a slug into a label and
//     cardinality stays bounded by the finite registered-route set. There is
//     deliberately NO operator_id label here (per-endpoint, not per-human).
//
// Privacy invariant (asserted by view_telemetry_test.go): no label value on
// either metric is ever an email, slug, slack id, or raw path.

const (
	// activityTypeView / surfaceWeb are the fixed label values this
	// middleware stamps on the backbone counter. The counter is shared with
	// the kbeads-mutation and slack-spawn emitters (plan §2), which stamp
	// their own activity_type/surface — these two constants are the
	// beads-viewer slice.
	activityTypeView = "view"
	surfaceWeb       = "web"

	// operatorIDUnattributed is the reserved sentinel bucket for requests we
	// could not attribute to a real human operator: anonymous requests (no
	// OIDC email in context), unprovisioned viewers (no operator alias yet),
	// and the sentinel/transport-error paths. Mirrors the operator_id="0"
	// fallback the session-push and mutation emitters use (plan §3.2) so the
	// uniqueness query has one canonical "unknown" bucket instead of a
	// dropped sample.
	operatorIDUnattributed = "0"

	// endpointOther is the bounded fallback for the latency histogram's
	// endpoint label when ServeMux matched no registered pattern (a 404
	// inside the app mux). Collapsing every unmatched path to a single value
	// keeps the label set finite — the whole point of labelling on the route
	// TEMPLATE rather than the raw URL.
	endpointOther = "other"
)

// viewTelemetry holds the two registered metric vectors plus the
// dependencies the middleware needs to resolve an operator and tag the
// project. Constructed once in main and shared across all requests.
type viewTelemetry struct {
	activityTotal  *prometheus.CounterVec
	requestLatency *prometheus.HistogramVec

	// resolver maps the OIDC subject in the request context to an
	// operators(id) FK, server-side and cached. Nil-safe: a nil resolver
	// makes every request resolve to operatorIDUnattributed, which is the
	// correct behaviour for a viewer started without the resolver wired
	// (the counter still tracks total web views under the "0" bucket).
	resolver *viewer.Resolver

	// project is the deployment's project label (plan §2: a bounded config
	// value, not per-request). Empty collapses to "unknown" so the label is
	// never an empty string.
	project string
}

// newViewTelemetry builds and registers the view-engagement counter and the
// request-latency histogram against reg. It returns an error if registration
// fails (duplicate metric, etc.) so main can fail fast.
func newViewTelemetry(reg prometheus.Registerer, resolver *viewer.Resolver, project string) (*viewTelemetry, error) {
	if project == "" {
		project = "unknown"
	}
	vt := &viewTelemetry{
		activityTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gardenio_operator_activity_total",
			Help: "Operator engagement events, labelled by operator_id (opaque FK, never email/slug), project, activity_type, and surface. The beads-viewer emits the {activity_type=view,surface=web} slice on every page navigation; unattributed/anonymous viewers roll up to operator_id=\"0\".",
		}, []string{"operator_id", "project", "activity_type", "surface"}),
		requestLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gardenio_user_request_latency_seconds",
			Help: "User-facing request latency by surface, bounded route-template endpoint (never a raw URL), and status_code. No operator_id label (per-endpoint, not per-human).",
			// 100ms…30s per plan §5 (gardenio_user_request_latency_seconds
			// row). Page renders sit in the sub-second range; the long tail
			// catches slow daemon round-trips and SSE-adjacent stalls.
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"surface", "endpoint", "status_code"}),
		resolver: resolver,
		project:  project,
	}
	if err := reg.Register(vt.activityTotal); err != nil {
		return nil, err
	}
	if err := reg.Register(vt.requestLatency); err != nil {
		return nil, err
	}
	return vt, nil
}

// resolveOperatorIDLabel resolves the request's OIDC viewer to its
// operators(id) FK rendered as a string label, falling back to the
// operatorIDUnattributed ("0") bucket for anonymous, unprovisioned, sentinel,
// or transport-error cases. Never returns an email or slug — the whole point
// of the server-side resolve (plan §1.4).
func (vt *viewTelemetry) resolveOperatorIDLabel(ctx context.Context) string {
	if vt == nil || vt.resolver == nil {
		return operatorIDUnattributed
	}
	id, found, err := vt.resolver.OperatorID(ctx)
	if err != nil || !found || id == 0 {
		return operatorIDUnattributed
	}
	return strconv.FormatInt(id, 10)
}

// isPageView reports whether a request should count as a human "page view"
// for the engagement counter. We count top-level GET navigations and skip
// the request shapes that would inflate the engagement signal without
// representing a distinct human visit:
//
//   - non-GET (writes/actions are mutations, instrumented on the kbeads
//     surface, not as web "views");
//   - htmx partial fetches (HX-Request: true) — inline fragment refreshes
//     of a page already counted on its initial load;
//   - Server-Sent Events streams (Accept: text/event-stream) — one long-
//     lived connection per page, not a navigation;
//   - unmatched routes (r.Pattern == "") — 404s are not page views.
//
// This mirrors the original homepage_telemetry.go intent (it fired only on
// full-page GET renders) while generalising from three hard-coded routes to
// every navigable route. The listed acceptance routes (/u/{slug}, /inbox,
// /search, /agents, /active, /decisions, /advice, /costs, /schedules,
// bead-show) are all full-page GETs and so are all covered.
func isPageView(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if r.Pattern == "" {
		return false
	}
	if r.Header.Get("HX-Request") == "true" {
		return false
	}
	if accept := r.Header.Get("Accept"); accept == "text/event-stream" {
		return false
	}
	return true
}

// endpointLabel returns the bounded route-template label for the latency
// histogram. ServeMux populates r.Pattern (e.g. "GET /u/{slug}") on the
// request once it has matched a registered route; that template is finite
// and free of any path-derived PII. An empty pattern (no route matched)
// collapses to endpointOther so the label set can never grow with the URL
// space.
func endpointLabel(r *http.Request) string {
	if r.Pattern == "" {
		return endpointOther
	}
	return r.Pattern
}

// Middleware wraps the application handler with view-engagement and
// request-latency instrumentation. It MUST be installed INSIDE the auth
// chain (after the OIDC/AVA middleware has stamped the viewer email into the
// request context) and OUTSIDE the application mux, so that:
//
//   - the operator resolve sees the authenticated email, and
//   - r.Pattern is populated by the mux before this middleware reads it on
//     the way back out (verified: ServeMux sets Pattern on the same request
//     value the outer middleware holds).
//
// Nil-safe: a nil *viewTelemetry returns next unchanged, so a viewer built
// without metrics wiring (some tests) behaves exactly as before.
func (vt *viewTelemetry) Middleware(next http.Handler) http.Handler {
	if vt == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		// r.Pattern is now set by the inner ServeMux (if a route matched).
		vt.requestLatency.WithLabelValues(
			surfaceWeb,
			endpointLabel(r),
			strconv.Itoa(rec.statusCode()),
		).Observe(time.Since(start).Seconds())

		if isPageView(r) {
			vt.activityTotal.WithLabelValues(
				vt.resolveOperatorIDLabel(r.Context()),
				vt.project,
				activityTypeView,
				surfaceWeb,
			).Inc()
		}
	})
}

// statusRecorder is a minimal http.ResponseWriter wrapper that captures the
// response status code for the latency histogram's status_code label.
//
// It explicitly re-exposes http.Flusher because several viewer routes
// (agent_events.go, agents_events.go) type-assert the writer to
// http.Flusher to stream SSE; embedding the interface does not promote that
// assertion to the wrapper type, so we delegate it by hand. Unwrap() lets
// http.ResponseController find any further interfaces on the underlying
// writer for forward compatibility.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		// An implicit 200: a handler that writes a body without an explicit
		// WriteHeader. Record it so the status_code label is never "0".
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// statusCode returns the captured status, defaulting to 200 for handlers
// that returned without writing a header or body (net/http sends 200 in
// that case too).
func (s *statusRecorder) statusCode() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

// Flush delegates to the underlying writer's Flusher when present, so SSE
// routes keep streaming through the wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates to the underlying writer's Hijacker when present so routes
// that upgrade the connection keep working through the wrapper. The /grafana
// reverse proxy (grafana_proxy.go) needs this: Grafana Live streams dashboard
// updates over a WebSocket, and httputil.ReverseProxy type-asserts the writer
// to http.Hijacker to perform the upgrade — embedding http.ResponseWriter does
// not promote that assertion to *statusRecorder, so we delegate by hand (same
// reason Flush is re-exposed above). Returns http.ErrNotSupported when the
// underlying writer is not a Hijacker so the caller gets a clean error rather
// than a panic.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := s.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
