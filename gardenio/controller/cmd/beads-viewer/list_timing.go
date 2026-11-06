package main

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/beadsapi"
)

// beadListSeconds times the viewer's GET /v1/beads calls by the viewer
// function that made them. The daemon sees every viewer list as one caller
// (machine:gardenio-beads-viewer), so this is what names the slow ones
// (gd-XOi5cqZP3I). site is a function name, so the label set is bounded by
// the code, not by traffic; outcome is beadListOutcome's small fixed set.
var beadListSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "gardenio_beads_viewer_bead_list_seconds",
	Help:    "Wall time of the viewer's GET /v1/beads calls, retries included, by the viewer function (site) that made the call.",
	Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
}, []string{"site", "outcome"})

// beadListPkg is the import-path prefix of the client package whose frames
// beadListSite skips to reach the caller.
const beadListPkg = "gardenio/controller/internal/beadsapi."

// beadListSiteOther is the site of a call with no caller frame outside
// beadsapi (it should not happen).
const beadListSiteOther = "other"

// observeBeadList is the beadsapi.BeadListObserver the viewer installs.
func observeBeadList(_ context.Context, took time.Duration, err error) {
	beadListSeconds.WithLabelValues(beadListSite(), beadListOutcome(err)).Observe(took.Seconds())
}

// beadListOutcome names how a list call ended, so a site's errors say whether
// the daemon refused it (401, 403, 404, 429, other 4xx), failed it (5xx), or
// the caller gave up (canceled, timeout): a site whose calls fail in a few
// milliseconds was not slow, it was refused (gd-qCPRRNtm4M).
func beadListOutcome(err error) string {
	var ae *beadsapi.APIError
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, beadsapi.ErrUserBearerRequiredForRead):
		return "no_bearer"
	case errors.As(err, &ae):
		switch c := ae.StatusCode; {
		case c == http.StatusUnauthorized, c == http.StatusForbidden, c == http.StatusNotFound, c == http.StatusTooManyRequests:
			return strconv.Itoa(c)
		case c >= 500:
			return "5xx"
		case c >= 400:
			return "4xx"
		}
	}
	return "error"
}

var _ beadsapi.BeadListObserver = observeBeadList

// beadListSite is the first function on the calling goroutine's stack below
// the beadsapi client's frames, without the module path or main package: for
// example "(*Server).handleActive" or "internal/viewer.(*Resolver).Agents".
func beadListSite() string {
	var pcs [32]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	inClient := false
	for {
		f, more := frames.Next()
		if strings.HasPrefix(f.Function, beadListPkg) {
			inClient = true
		} else if inClient {
			site := strings.TrimPrefix(f.Function, "gardenio/controller/")
			// The viewer's own package is "main" in the binary and its
			// import path in a test binary.
			site = strings.TrimPrefix(site, "cmd/beads-viewer.")
			return strings.TrimPrefix(site, "main.")
		}
		if !more {
			return beadListSiteOther
		}
	}
}
