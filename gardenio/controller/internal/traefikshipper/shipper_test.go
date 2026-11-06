package traefikshipper

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSink records every batch it is asked to insert.
type fakeSink struct {
	mu       sync.Mutex
	batches  [][]byte
	failNext bool
}

func (f *fakeSink) Insert(_ context.Context, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return errors.New("sink boom")
	}
	f.batches = append(f.batches, append([]byte(nil), b...))
	return nil
}

func (f *fakeSink) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sb strings.Builder
	for _, b := range f.batches {
		sb.Write(b)
	}
	return sb.String()
}

// fakeStreamer yields the configured payload once, then blocks on ctx so the
// stream "stays open" like a real follow stream until cancelled.
type fakeStreamer struct {
	payload string
	calls   int
	mu      sync.Mutex
}

func (s *fakeStreamer) Stream(ctx context.Context, _ int64) (io.ReadCloser, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, s.payload)
		// Keep the stream open until ctx is cancelled (mimics follow).
		<-ctx.Done()
		_ = pw.CloseWithError(io.EOF)
	}()
	return pr, nil
}

func TestAcceptLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		keep bool
	}{
		{"access 200", `{"DownstreamStatus":200,"RequestMethod":"GET","RequestPath":"/"}`, true},
		{"access 404 non-2xx kept", `{"DownstreamStatus":404,"RequestMethod":"GET","request_X-Forwarded-For":"203.0.113.7"}`, true},
		{"access 401 non-2xx kept", `{"DownstreamStatus":401,"RequestMethod":"POST"}`, true},
		{"access 500 non-2xx kept", `{"DownstreamStatus":500,"RequestMethod":"GET"}`, true},
		// REAL prod shape: a Traefik v3 JSON access record ALSO carries
		// level/msg/time — it MUST be kept (regression guard: an earlier version
		// wrongly dropped any JSON with a "level" field, which is every real
		// record). Captured live from the prod traefik pod, 2026-08-08.
		{"real access record with level field kept", `{"ClientHost":"10.50.49.75","DownstreamStatus":200,"RequestMethod":"GET","RequestPath":"/beads/agents/events-status","StartUTC":"2026-08-08T21:41:58.6Z","entryPointName":"websecure","level":"info","msg":"","time":"2026-08-08T21:41:58Z","request_X-Real-Ip":"10.50.49.75"}`, true},
		{"real access 404 with level field kept", `{"DownstreamStatus":404,"RequestMethod":"GET","RequestPath":"/probe","level":"info","msg":"","time":"2026-08-08T21:41:58Z"}`, true},
		{"runtime json log (no access fields) dropped", `{"level":"error","msg":"tls config failed","time":"2026-08-08T20:00:00Z"}`, false},
		{"zerolog console line dropped", `2026-08-08T20:09:07Z ERR error="secret does not exist"`, false},
		{"empty dropped", ``, false},
		{"whitespace dropped", `   `, false},
		{"non-access json dropped", `{"foo":"bar"}`, false},
		{"malformed json dropped", `{"DownstreamStatus":200,`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, keep := acceptLine([]byte(c.in))
			if keep != c.keep {
				t.Fatalf("acceptLine(%q) keep=%v, want %v", c.in, keep, c.keep)
			}
		})
	}
}

// Non-2xx must NEVER be sampled out: a batch of mixed status codes must ship
// every non-2xx record. This is the load-bearing sampling invariant from
// ka-0000007287#c3.
func TestNeverSampleNon2xx(t *testing.T) {
	payload := strings.Join([]string{
		`{"DownstreamStatus":200,"RequestMethod":"GET","RequestPath":"/ok"}`,
		`{"DownstreamStatus":404,"RequestMethod":"GET","RequestPath":"/probe"}`,
		`{"level":"info","msg":"runtime chatter"}`,
		`{"DownstreamStatus":401,"RequestMethod":"POST","RequestPath":"/login"}`,
		`{"DownstreamStatus":500,"RequestMethod":"GET","RequestPath":"/boom"}`,
	}, "\n") + "\n"

	sink := &fakeSink{}
	st := &fakeStreamer{payload: payload}
	sh := New(Config{FlushInterval: 20 * time.Millisecond, ReconnectBackoff: time.Hour}, st, sink, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = sh.Run(ctx)

	got := sink.all()
	for _, want := range []string{`"/ok"`, `"/probe"`, `"/login"`, `"/boom"`} {
		if !strings.Contains(got, want) {
			t.Errorf("shipped batch missing %s (non-2xx must never be sampled out): got %q", want, got)
		}
	}
	if strings.Contains(got, "runtime chatter") {
		t.Errorf("runtime log leaked into access stream: %q", got)
	}
}

// XFF must survive the transport end-to-end (the whole geo story depends on it).
func TestPreservesXForwardedFor(t *testing.T) {
	payload := `{"DownstreamStatus":403,"RequestMethod":"GET","request_X-Forwarded-For":"198.51.100.42","request_User-Agent":"sqlmap/1.7"}` + "\n"
	sink := &fakeSink{}
	st := &fakeStreamer{payload: payload}
	sh := New(Config{FlushInterval: 20 * time.Millisecond, ReconnectBackoff: time.Hour}, st, sink, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = sh.Run(ctx)

	got := sink.all()
	if !strings.Contains(got, `"198.51.100.42"`) {
		t.Fatalf("X-Forwarded-For did not survive transport: %q", got)
	}
	if !strings.Contains(got, `sqlmap/1.7`) {
		t.Fatalf("User-Agent did not survive transport: %q", got)
	}
}

func TestFlushOnMaxBatchLines(t *testing.T) {
	var lines []string
	for i := 0; i < 5; i++ {
		lines = append(lines, `{"DownstreamStatus":200,"RequestMethod":"GET","RequestPath":"/p`+string(rune('0'+i))+`"}`)
	}
	payload := strings.Join(lines, "\n") + "\n"
	sink := &fakeSink{}
	st := &fakeStreamer{payload: payload}
	// MaxBatchLines=2 => flushes should happen mid-stream (not only at the end);
	// the short FlushInterval sweeps the trailing partial (5th) line.
	sh := New(Config{MaxBatchLines: 2, FlushInterval: 15 * time.Millisecond, ReconnectBackoff: time.Hour}, st, sink, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = sh.Run(ctx)

	sink.mu.Lock()
	n := len(sink.batches)
	sink.mu.Unlock()
	if n < 2 {
		t.Fatalf("expected multiple flushes with MaxBatchLines=2, got %d batches", n)
	}
	if c := strings.Count(sink.all(), "DownstreamStatus"); c != 5 {
		t.Fatalf("expected all 5 records shipped, got %d", c)
	}
}

// A sink failure must not lose the batch silently: streamOnce returns the error
// (so Run reconnects) rather than dropping records.
func TestSinkFailureSurfacesError(t *testing.T) {
	payload := `{"DownstreamStatus":200,"RequestMethod":"GET"}` + "\n"
	sink := &fakeSink{failNext: true}
	st := &fakeStreamer{payload: payload}
	sh := &Shipper{
		cfg:    Config{FlushInterval: 10 * time.Millisecond, ReconnectBackoff: time.Hour, MaxLineBytes: defaultMaxLine},
		stream: st,
		sink:   sink,
		log:    testLogger(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := sh.streamOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "insert to VictoriaLogs") {
		t.Fatalf("expected an insert error to surface, got %v", err)
	}
}

func TestRunReconnects(t *testing.T) {
	// A streamer that EOFs immediately every call, so streamOnce returns and Run
	// loops back to reconnect. With a tiny backoff we should see >1 open.
	st := &eofStreamer{}
	sh := New(Config{ReconnectBackoff: 5 * time.Millisecond}, st, &fakeSink{}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_ = sh.Run(ctx)
	if st.calls() < 2 {
		t.Fatalf("expected Run to reconnect (>=2 stream opens), got %d", st.calls())
	}
}

// eofStreamer returns a stream that EOFs immediately every call.
type eofStreamer struct {
	mu sync.Mutex
	n  int
}

func (e *eofStreamer) Stream(_ context.Context, _ int64) (io.ReadCloser, error) {
	e.mu.Lock()
	e.n++
	e.mu.Unlock()
	return io.NopCloser(strings.NewReader("")), nil
}
func (e *eofStreamer) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n
}
