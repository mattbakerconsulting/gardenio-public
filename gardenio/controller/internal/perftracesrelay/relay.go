package perftracesrelay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	scopeName        = "gardenio-perf-traces-relay"
	maxResponseBytes = 64 << 20 // 64 MiB cap on a single Jaeger query response
	otlpInsertPath   = "/insert/opentelemetry/v1/traces"
)

// Config holds the runtime parameters of the relay poller.
type Config struct {
	// SourceURL is the perf VictoriaTraces base URL (no trailing slash), e.g.
	// https://perf.acme.example/victoria-traces.
	SourceURL string
	// SourceUsername / SourcePassword authenticate to the source VT. Read from
	// the mounted perf-traces ExternalSecret.
	SourceUsername string
	SourcePassword string
	// DestURL is the in-chart VT base URL, e.g.
	// http://gardenio-victoria-traces:10428.
	DestURL string
	// Services restricts the poll to these source services. Empty means
	// discover via the Jaeger services API each cycle.
	Services []string
	// Interval between poll cycles.
	Interval time.Duration
	// LookbackOverlap is subtracted from the previous cycle's end when computing
	// the next window start, so spans landing slightly late in the source are not
	// missed at window boundaries.
	LookbackOverlap time.Duration
	// MaxTracesPerService caps the traces pulled per service per cycle.
	MaxTracesPerService int
	// InitialLookback is the window pulled on the very first cycle.
	InitialLookback time.Duration
	// HTTPTimeout bounds each source/dest HTTP call.
	HTTPTimeout time.Duration
}

// withDefaults returns a copy of c with zero-valued fields filled in.
func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.LookbackOverlap <= 0 {
		c.LookbackOverlap = 1 * time.Minute
	}
	if c.MaxTracesPerService <= 0 {
		c.MaxTracesPerService = 200
	}
	if c.InitialLookback <= 0 {
		c.InitialLookback = 15 * time.Minute
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = 30 * time.Second
	}
	return c
}

// Relay polls the perf VT Jaeger query API and replays spans into the in-chart
// VT over OTLP/HTTP.
type Relay struct {
	cfg    Config
	logger *slog.Logger
	source *jaegerClient
	dest   *otlpExporter
}

// New constructs a Relay from cfg.
func New(cfg Config, logger *slog.Logger) *Relay {
	cfg = cfg.withDefaults()
	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	return &Relay{
		cfg:    cfg,
		logger: logger,
		source: newJaegerClient(cfg.SourceURL, cfg.SourceUsername, cfg.SourcePassword, httpClient),
		dest:   newOTLPExporter(cfg.DestURL, httpClient),
	}
}

// Run polls until ctx is cancelled. The first cycle fires immediately.
func (r *Relay) Run(ctx context.Context) error {
	r.logger.Info("perf-traces relay starting",
		"source", r.cfg.SourceURL,
		"dest", r.cfg.DestURL,
		"interval", r.cfg.Interval.String(),
		"services", r.cfg.Services,
	)

	windowEnd := time.Now()
	windowStart := windowEnd.Add(-r.cfg.InitialLookback)

	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	for {
		spans, err := r.runOnce(ctx, windowStart, windowEnd)
		if err != nil {
			r.logger.Warn("perf-traces relay cycle failed", "error", err)
		} else {
			r.logger.Info("perf-traces relay cycle complete",
				"spans", spans,
				"window_start", windowStart.Format(time.RFC3339),
				"window_end", windowEnd.Format(time.RFC3339),
			)
			windowStart = windowEnd.Add(-r.cfg.LookbackOverlap)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			windowEnd = time.Now()
		}
	}
}

// runOnce executes a single poll/convert/push cycle and returns the number of
// spans forwarded.
func (r *Relay) runOnce(ctx context.Context, start, end time.Time) (int, error) {
	services := r.cfg.Services
	if len(services) == 0 {
		discovered, err := r.source.Services(ctx)
		if err != nil {
			return 0, fmt.Errorf("list services: %w", err)
		}
		services = discovered
	}

	total := 0
	for _, service := range services {
		traces, err := r.source.Traces(ctx, service, start, end, r.cfg.MaxTracesPerService)
		if err != nil {
			r.logger.Warn("fetch traces failed", "service", service, "error", err)
			continue
		}
		resourceSpans := jaegerTracesToOTLP(traces)
		spanCount := countSpans(resourceSpans)
		if spanCount == 0 {
			continue
		}
		if err := r.dest.Export(ctx, resourceSpans); err != nil {
			r.logger.Warn("export spans failed", "service", service, "spans", spanCount, "error", err)
			continue
		}
		total += spanCount
	}
	return total, nil
}

func countSpans(rs []*tracepb.ResourceSpans) int {
	n := 0
	for _, r := range rs {
		for _, ss := range r.ScopeSpans {
			n += len(ss.Spans)
		}
	}
	return n
}

// otlpExporter POSTs OTLP/HTTP protobuf trace payloads to a VictoriaTraces
// OTLP insert endpoint.
type otlpExporter struct {
	endpoint string
	http     *http.Client
}

func newOTLPExporter(destBaseURL string, httpClient *http.Client) *otlpExporter {
	return &otlpExporter{endpoint: destBaseURL + otlpInsertPath, http: httpClient}
}

func (e *otlpExporter) Export(ctx context.Context, resourceSpans []*tracepb.ResourceSpans) error {
	req := &collectortracepb.ExportTraceServiceRequest{ResourceSpans: resourceSpans}
	body, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal OTLP request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := e.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("OTLP export: status %d: %s", resp.StatusCode, truncate(string(respBody), 256))
	}
	return nil
}
