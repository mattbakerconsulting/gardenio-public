package servertiming

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// samples is how many observations h's series for labels has.
func samples(t *testing.T, h *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	var m dto.Metric
	if err := h.WithLabelValues(labels...).(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

func serve(route string, handler func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	Wrap(route, handler)(rec, httptest.NewRequest("GET", "/x", nil))
	return rec
}

// The header names each lap in order, then the notes, then total; every value is milliseconds.
func TestHeaderCarriesLapsNotesAndTotal(t *testing.T) {
	route := "GET /test/laps"
	rec := serve(route, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Millisecond)
		Lap(w, "kbeads")
		Lap(w, "encode")
		Note(w, "snapshot-age", 1500*time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})
	got := rec.Header().Get("Server-Timing")
	if !regexp.MustCompile(`^kbeads;dur=\d+\.\d\d, encode;dur=\d+\.\d\d, snapshot-age;dur=1500\.00, total;dur=\d+\.\d\d$`).MatchString(got) {
		t.Fatalf("Server-Timing = %q", got)
	}
	if strings.HasPrefix(got, "kbeads;dur=0.") || strings.HasPrefix(got, "kbeads;dur=1.") {
		t.Errorf("kbeads lap %q is shorter than the 2 ms the handler slept", got)
	}
	if rec.Body.String() != "{}" || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("the response changed: %q %q", rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	for _, p := range []string{"kbeads", "encode", "write"} {
		if n := samples(t, phaseSeconds, route, p); n != 1 {
			t.Errorf("phase %s observed %d times, want 1", p, n)
		}
	}
	if n := samples(t, phaseSeconds, route, "snapshot-age"); n != 0 {
		t.Errorf("a note was observed as a phase (%d)", n)
	}
	if n := samples(t, routeSeconds, route, "2xx"); n != 1 {
		t.Errorf("route observed %d times, want 1", n)
	}
}

// A handler that laps nothing is one "handler" phase on the header and in the histogram.
func TestUnlappedHandlerIsOnePhase(t *testing.T) {
	route := "GET /test/bare"
	rec := serve(route, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	if got := rec.Header().Get("Server-Timing"); !regexp.MustCompile(`^handler;dur=[\d.]+, total;dur=[\d.]+$`).MatchString(got) {
		t.Fatalf("Server-Timing = %q", got)
	}
	if n := samples(t, phaseSeconds, route, "handler"); n != 1 {
		t.Errorf("handler phase observed %d times", n)
	}

	route = "GET /test/silent" // writes nothing at all: no header goes out, and the route still counts as 200
	serve(route, func(http.ResponseWriter, *http.Request) {})
	if n := samples(t, phaseSeconds, route, "handler"); n != 1 {
		t.Errorf("silent handler phase observed %d times", n)
	}
	if n := samples(t, routeSeconds, route, "2xx"); n != 1 {
		t.Errorf("silent route observed %d times as 2xx", n)
	}
}

// The status class is the route histogram's code; a 304 still carries the header.
func TestStatusClassAndNotModified(t *testing.T) {
	route := "GET /test/304"
	rec := serve(route, func(w http.ResponseWriter, _ *http.Request) {
		Lap(w, "snapshot")
		w.WriteHeader(http.StatusNotModified)
	})
	if rec.Code != http.StatusNotModified || !strings.HasPrefix(rec.Header().Get("Server-Timing"), "snapshot;dur=") {
		t.Fatalf("code %d, Server-Timing %q", rec.Code, rec.Header().Get("Server-Timing"))
	}
	if n := samples(t, routeSeconds, route, "3xx"); n != 1 {
		t.Errorf("3xx observed %d times", n)
	}
	route = "GET /test/err"
	serve(route, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusBadGateway) })
	if n := samples(t, routeSeconds, route, "5xx"); n != 1 {
		t.Errorf("5xx observed %d times", n)
	}
}

// Lap and Note on a writer Wrap did not make do nothing, so a handler called bare behaves as it did.
func TestBareWriterIsUntouched(t *testing.T) {
	rec := httptest.NewRecorder()
	Lap(rec, "kbeads")
	Note(rec, "snapshot-age", time.Second)
	_, _ = rec.Write([]byte("x"))
	if got := rec.Header().Get("Server-Timing"); got != "" {
		t.Fatalf("Server-Timing on a bare writer: %q", got)
	}
}

// A wrapping writer that unwraps to ours still finds the timing.
type outer struct{ http.ResponseWriter }

func (o outer) Unwrap() http.ResponseWriter { return o.ResponseWriter }

func TestLapThroughAnUnwrapper(t *testing.T) {
	rec := serve("GET /test/unwrap", func(w http.ResponseWriter, _ *http.Request) {
		o := outer{w}
		Lap(o, "kbeads")
		_, _ = o.Write([]byte("x"))
	})
	if got := rec.Header().Get("Server-Timing"); !strings.HasPrefix(got, "kbeads;dur=") {
		t.Fatalf("Server-Timing = %q", got)
	}
}

type registrar map[string]func(http.ResponseWriter, *http.Request)

func (r registrar) HandleFunc(p string, h func(http.ResponseWriter, *http.Request)) { r[p] = h }

// Timed wraps every route but an event stream, which keeps its own writer (and so its Flusher) untouched.
func TestTimedSkipsStreams(t *testing.T) {
	reg := registrar{}
	tm := Timed(reg)
	for _, p := range []string{"GET /garden", "GET /garden/events", "GET /garden/glass/{agent}/stream"} {
		tm.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			if _, ok := w.(http.Flusher); !ok {
				t.Errorf("%s: the writer lost http.Flusher", p)
			}
			w.(http.Flusher).Flush()
		})
	}
	for p, want := range map[string]bool{"GET /garden": true, "GET /garden/events": false, "GET /garden/glass/{agent}/stream": false} {
		rec := httptest.NewRecorder()
		reg[p](rec, httptest.NewRequest("GET", "/x", nil))
		if got := rec.Header().Get("Server-Timing") != ""; got != want {
			t.Errorf("%s: timed %v, want %v", p, got, want)
		}
		if !rec.Flushed {
			t.Errorf("%s: Flush did not reach the recorder", p)
		}
	}
}

// A header the handler set itself is kept beside ours.
func TestHandlerHeaderKept(t *testing.T) {
	rec := serve("GET /test/own", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server-Timing", "cache;desc=hit")
		_, _ = w.Write([]byte("x"))
	})
	if got := rec.Header().Values("Server-Timing"); len(got) != 2 || got[0] != "cache;desc=hit" {
		t.Fatalf("Server-Timing = %q", got)
	}
}
