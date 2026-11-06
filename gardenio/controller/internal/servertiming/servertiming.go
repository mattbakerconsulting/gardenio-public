// Package servertiming times the phases of one /garden request and reports them twice: as a Server-Timing header
// (RFC 8673), read in the browser's devtools, by `curl -sI` and by the Lands budget harness, and as route and phase
// latency histograms on the viewer's metrics port, so the backend half of each Lands budget shows in Grafana (plan
// gd-MEGB3dEaHu, step P0.3 gd-7jlAbyk8Fy).
//
// A package's Register wraps its routes with Wrap (or Registrar), and a handler calls Lap(w, phase) at the end of each
// phase: "snapshot", "template", "kbeads", "encode", "gzip". The header goes out with the first WriteHeader or Write,
// carrying every lap so far and "total" (the time to the header); the histograms are observed once the handler
// returns, with the time after the last lap as "write". A handler that never laps is timed as one "handler" phase.
// Lap on a writer Wrap did not make is a no-op, so a handler called bare in a test behaves as before.
//
// It changes no response but the one header. Labels are bounded: route is the registered ServeMux pattern (never the
// raw path), phase is one of the handful of names above, code is the status class.
package servertiming

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// buckets run from 1 ms to 10 s: the backend half of a Lands budget is milliseconds (an in-memory snapshot, a gzip
// served from cache) to a second (a cold kbeads read), so the coarse 100 ms floor of
// gardenio_user_request_latency_seconds cannot tell them apart.
var buckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

var (
	routeSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gardenio_garden_route_duration_seconds",
		Help:    "Server time of a /garden route (the page, the farm document, the Commons and Workbench routes; not its event streams), from the handler's start to its return, by route pattern and status class.",
		Buckets: buckets,
	}, []string{"route", "code"})
	phaseSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gardenio_garden_route_phase_seconds",
		Help:    "Server time of one phase of a /garden route (snapshot, template, kbeads, encode, gzip, write; handler when the route records none), by route pattern and phase. The Server-Timing header's split.",
		Buckets: buckets,
	}, []string{"route", "phase"})
)

// Collectors are the two histograms, for the viewer to register.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{routeSeconds, phaseSeconds}
}

// Registrar is the slice of *http.ServeMux that Wrap registers on (gardenhub.Registrar and commons.Registrar).
type Registrar interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// timed is a Registrar that wraps each route it registers.
type timed struct{ next Registrar }

// Timed returns a Registrar that registers every route on next under Wrap, except an event stream (a pattern ending
// in /events or /stream): a stream's time is its lifetime, not a latency, and it writes its headers at once.
func Timed(next Registrar) Registrar { return timed{next} }

func (t timed) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	if strings.HasSuffix(pattern, "/events") || strings.HasSuffix(pattern, "/stream") {
		t.next.HandleFunc(pattern, handler)
		return
	}
	t.next.HandleFunc(pattern, Wrap(pattern, handler))
}

// Wrap times handler under route (its ServeMux pattern).
func Wrap(route string, handler func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		tw := &writer{ResponseWriter: w, t: &timing{start: now, mark: now}}
		handler(tw, r)
		tw.t.finish(route, tw.status)
	}
}

// Lap ends the phase named name at this instant: the time since the previous lap (or the handler's start). w is the
// handler's writer; one Wrap did not make records nothing.
func Lap(w http.ResponseWriter, name string) {
	if t := of(w); t != nil {
		t.lap(name, time.Now())
	}
}

// Note adds a header-only entry: a duration that is not a phase of this request (the age of the snapshot it served),
// so it is never summed into total or observed. A no-op once the header is out.
func Note(w http.ResponseWriter, name string, d time.Duration) {
	if t := of(w); t != nil {
		t.mu.Lock()
		t.notes = append(t.notes, phase{name, d})
		t.mu.Unlock()
	}
}

// of finds the timing on w or on a writer it wraps.
func of(w http.ResponseWriter) *timing {
	for w != nil {
		if tw, ok := w.(*writer); ok {
			return tw.t
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
	return nil
}

type phase struct {
	name string
	d    time.Duration
}

type timing struct {
	start time.Time
	mu    sync.Mutex
	mark  time.Time
	laps  []phase
	notes []phase
}

func (t *timing) lap(name string, now time.Time) {
	t.mu.Lock()
	t.laps = append(t.laps, phase{name, now.Sub(t.mark)})
	t.mark = now
	t.mu.Unlock()
}

// header is the Server-Timing value at now: the laps, the notes, then total. A handler that has lapped nothing is
// given one "handler" lap first, so its time is still split from the write.
func (t *timing) header(now time.Time) string {
	t.mu.Lock()
	if len(t.laps) == 0 {
		t.laps = append(t.laps, phase{"handler", now.Sub(t.mark)})
		t.mark = now
	}
	parts := make([]string, 0, len(t.laps)+len(t.notes)+1)
	for _, p := range t.laps {
		parts = append(parts, entry(p))
	}
	for _, p := range t.notes {
		parts = append(parts, entry(p))
	}
	t.mu.Unlock()
	return strings.Join(append(parts, entry(phase{"total", now.Sub(t.start)})), ", ")
}

func entry(p phase) string {
	return fmt.Sprintf("%s;dur=%.2f", p.name, float64(p.d.Microseconds())/1000)
}

// finish laps the rest as "write" (as "handler" when nothing was lapped or written) and observes the histograms.
func (t *timing) finish(route string, status int) {
	now := time.Now()
	t.mu.Lock()
	rest := "write"
	if len(t.laps) == 0 {
		rest = "handler"
	}
	t.mu.Unlock()
	t.lap(rest, now)
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range t.laps {
		phaseSeconds.WithLabelValues(route, p.name).Observe(p.d.Seconds())
	}
	routeSeconds.WithLabelValues(route, codeClass(status)).Observe(now.Sub(t.start).Seconds())
}

// codeClass is the status class ("2xx"); a handler that wrote nothing answered 200.
func codeClass(status int) string {
	if status == 0 {
		status = http.StatusOK
	}
	return fmt.Sprintf("%dxx", status/100)
}

// writer puts the Server-Timing header on the response as it goes out and keeps its status.
type writer struct {
	http.ResponseWriter
	t      *timing
	status int
}

func (w *writer) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.Header().Add("Server-Timing", w.t.header(time.Now()))
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *writer) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps a handler's http.Flusher assertion working through the wrapper.
func (w *writer) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }
