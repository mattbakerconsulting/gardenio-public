package beadsapi

import (
	"runtime"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// listRequestsTotal counts every GET /v1/beads list request by call site, so
// a component's list traffic can be attributed to the loop issuing it. The
// daemon's gardenio_beads_http_request_duration_seconds has only a caller
// (principal) label: the controller's 22-53/s of lists could not be traced to
// a loop from it (gd-jd0BGkgmV5).
//
// site is the first function outside this package on the stack and api the
// outermost function of this package (the method the caller invoked), both
// with the module prefix trimmed. no_total says whether the request skipped
// the daemon's full-set count.
//
// Not promauto: the controller swaps prometheus.DefaultRegisterer for its own
// registry after package init, so a component that serves it registers
// ListRequestsCollector itself.
var listRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_beadsapi_list_requests_total",
	Help: "GET /v1/beads list requests sent to the beads daemon, by calling function (site), the beadsapi method it called (api), and whether the request set no_total.",
}, []string{"site", "api", "no_total"})

// ListRequestsCollector returns the per-call-site list request counter for a
// component to register on the registry its /metrics serves.
func ListRequestsCollector() prometheus.Collector { return listRequestsTotal }

const beadsapiPkgPrefix = "gardenio/controller/internal/beadsapi."

// countListRequest records one list request when method and path are a
// GET /v1/beads list. Called from doJSON, so the stack is
// site -> api... -> doJSON -> countListRequest.
func countListRequest(method, path string) {
	if method != "GET" || (path != "/v1/beads" && !strings.HasPrefix(path, "/v1/beads?")) {
		return
	}
	site, api := listCallSite(3)
	noTotal := "false"
	if strings.Contains(path, "no_total=true") {
		noTotal = "true"
	}
	listRequestsTotal.WithLabelValues(site, api, noTotal).Inc()
}

// listCallSite walks the stack from skip and returns the first function
// outside this package (site) and the last one inside it (api). site is
// "unknown" when no frame outside the package is found within 32 frames.
func listCallSite(skip int) (site, api string) {
	var pcs [32]uintptr
	n := runtime.Callers(skip, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	site, api = "unknown", "unknown"
	for {
		f, more := frames.Next()
		if strings.HasPrefix(f.Function, beadsapiPkgPrefix) {
			api = strings.TrimPrefix(f.Function, beadsapiPkgPrefix)
		} else if f.Function != "" {
			site = trimModulePath(f.Function)
			return site, api
		}
		if !more {
			return site, api
		}
	}
}

// trimModulePath shortens a runtime function name to its path after the
// controller module ("internal/reconciler.(*R).sync"), or after the last
// slash for a function outside it.
func trimModulePath(fn string) string {
	const mod = "gardenio/controller/"
	if s, ok := strings.CutPrefix(fn, mod); ok {
		return s
	}
	if i := strings.LastIndex(fn, "/"); i >= 0 {
		return fn[i+1:]
	}
	return fn
}
