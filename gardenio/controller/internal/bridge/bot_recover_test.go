package bridge

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// readHandlerPanicCounter returns the current value of
// gardenio_bridge_handler_panic_total for the given handler label.
func readHandlerPanicCounter(t *testing.T, handler string) float64 {
	t.Helper()
	c, err := handlerPanicTotal.GetMetricWithLabelValues(handler)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := c.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// TestDispatchHandlerRecoversPanic proves a panic inside a dispatched handler
// is recovered (does not crash the process / re-panic) AND is recorded on the
// handlerPanicTotal counter. This is the core of the HIGH finding ka-0000007939:
// the single-replica bridge must not die when any handler path panics.
func TestDispatchHandlerRecoversPanic(t *testing.T) {
	b := &Bot{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	before := readHandlerPanicCounter(t, "test_panic")

	// Must not propagate the panic to the caller (the real caller is a bare
	// goroutine, where an un-recovered panic terminates the whole process).
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("dispatchHandler leaked a panic: %v", r)
			}
		}()
		b.dispatchHandler("test_panic", func() {
			panic("boom")
		})
	}()

	after := readHandlerPanicCounter(t, "test_panic")
	if after != before+1 {
		t.Fatalf("handlerPanicTotal[test_panic] = %v, want %v (before %v)", after, before+1, before)
	}
}

// TestDispatchHandlerRunsBodyAndNoCounterOnSuccess proves the happy path runs
// the body and does NOT touch the panic counter.
func TestDispatchHandlerRunsBodyAndNoCounterOnSuccess(t *testing.T) {
	b := &Bot{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	before := readHandlerPanicCounter(t, "test_ok")

	ran := false
	b.dispatchHandler("test_ok", func() { ran = true })
	if !ran {
		t.Fatal("dispatchHandler did not run the body")
	}

	after := readHandlerPanicCounter(t, "test_ok")
	if after != before {
		t.Fatalf("handlerPanicTotal[test_ok] moved on success: %v -> %v", before, after)
	}
}

// TestDispatchHandlerConcurrentPanicsAllRecovered stresses the boundary under
// concurrency: N goroutines each panic through dispatchHandler and every one
// must be recovered and counted, mirroring a burst of poison Slack events.
func TestDispatchHandlerConcurrentPanicsAllRecovered(t *testing.T) {
	logBuf := &syncBuffer{}
	b := &Bot{logger: slog.New(slog.NewTextHandler(logBuf, nil))}

	const n = 50
	before := readHandlerPanicCounter(t, "test_concurrent")

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// No recover here: if dispatchHandler leaks, the test process dies,
			// which is exactly the production failure we are guarding against.
			b.dispatchHandler("test_concurrent", func() {
				panic("concurrent boom")
			})
		}()
	}
	wg.Wait()

	after := readHandlerPanicCounter(t, "test_concurrent")
	if after != before+n {
		t.Fatalf("handlerPanicTotal[test_concurrent] = %v, want %v", after, before+n)
	}
	if !strings.Contains(logBuf.String(), "recovered panic in bridge handler goroutine") {
		t.Fatal("expected a recovered-panic log line")
	}
}

// syncBuffer is a tiny concurrency-safe io.Writer for capturing log output
// from parallel goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
