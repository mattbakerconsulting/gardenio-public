package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// responseBytesTotal sizes the read traffic per caller (cost plan
// 2026-10-21-aws-cost-cuts Task B3). caller is bounded: the configured
// machine principals plus the five auth types. route is the ServeMux pattern.
// SSE streams add their bytes when the stream ends.
var responseBytesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_beads_http_response_bytes_total",
		Help: "HTTP response body bytes by caller (machine:<name> for machine principals, else static|fleet|oidc|edge|other) and route (ServeMux pattern, or unmatched).",
	},
	[]string{"caller", "route"},
)

// requestDuration times every request per caller and route, so the read load
// on postgres can be attributed to the loop that causes it (gd-g25jihMHFs):
// rate(_count) is calls/s and rate(_sum) is daemon-seconds/s per caller. Same
// bounded labels as responseBytesTotal. SSE streams are skipped: they are held
// open for minutes and would only fill the +Inf bucket.
var requestDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "gardenio_beads_http_request_duration_seconds",
		Help:    "HTTP request duration by caller (as gardenio_beads_http_response_bytes_total) and route (ServeMux pattern, or unmatched). SSE streams are not observed.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	},
	[]string{"caller", "route"},
)

// responseCaller maps an identity to the bounded caller label: the
// "machine:<clientId>" subject for machine principals, else the auth type.
func responseCaller(id AuthIdentity) string {
	t := normalizeRequestAuthType(id.Type)
	if t == "machine" && strings.HasPrefix(id.Subject, machineSubjectPrefix) {
		return id.Subject
	}
	return t
}

type routeCaptureKey struct{}

type routeCapture struct{ pattern string }

// captureRoute wraps the ServeMux. ServeMux sets r.Pattern on the request it is
// handed, but loggingMiddleware sits above handlers that copy the request
// (WithContext), so the pattern is passed back up through the context.
func captureRoute(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if rc, ok := r.Context().Value(routeCaptureKey{}).(*routeCapture); ok {
			rc.pattern = r.Pattern
		}
	})
}

func withRouteCapture(ctx context.Context) (context.Context, *routeCapture) {
	rc := &routeCapture{}
	return context.WithValue(ctx, routeCaptureKey{}, rc), rc
}
