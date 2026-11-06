// Package perftracesrelay implements the PULL/relay transport that copies perf
// spans into the in-chart gardenio VictoriaTraces (epic ka-0000000598, task
// ka-0000004875). VictoriaTraces ingests OTLP only but its query API speaks
// Jaeger JSON, and the OTEL Collector has no receiver that pulls from a Jaeger
// query API — so per the merged design doc fallback
// (gardenio/docs/obs-perf-to-gardenio-victoria-traces.md §3) this is a tiny
// poller: read the perf VT Jaeger query API, convert Jaeger JSON to OTLP, and
// POST it to the in-chart VT OTLP/HTTP ingest path.
package perftracesrelay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// jaegerServicesResponse is the payload of GET /select/jaeger/api/services.
type jaegerServicesResponse struct {
	Data []string `json:"data"`
}

// jaegerTracesResponse is the payload of GET /select/jaeger/api/traces.
type jaegerTracesResponse struct {
	Data []jaegerTrace `json:"data"`
}

type jaegerTrace struct {
	TraceID   string                   `json:"traceID"`
	Spans     []jaegerSpan             `json:"spans"`
	Processes map[string]jaegerProcess `json:"processes"`
}

type jaegerSpan struct {
	TraceID       string           `json:"traceID"`
	SpanID        string           `json:"spanID"`
	OperationName string           `json:"operationName"`
	References    []jaegerRef      `json:"references"`
	StartTime     int64            `json:"startTime"` // microseconds since epoch
	Duration      int64            `json:"duration"`  // microseconds
	Tags          []jaegerKeyValue `json:"tags"`
	ProcessID     string           `json:"processID"`
}

type jaegerRef struct {
	RefType string `json:"refType"`
	TraceID string `json:"traceID"`
	SpanID  string `json:"spanID"`
}

type jaegerProcess struct {
	ServiceName string           `json:"serviceName"`
	Tags        []jaegerKeyValue `json:"tags"`
}

type jaegerKeyValue struct {
	Key   string          `json:"key"`
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// jaegerClient reads the perf VictoriaTraces Jaeger query API over basic auth.
type jaegerClient struct {
	baseURL  string
	username string
	password string
	http     *http.Client
}

func newJaegerClient(baseURL, username, password string, httpClient *http.Client) *jaegerClient {
	return &jaegerClient{
		baseURL:  baseURL,
		username: username,
		password: password,
		http:     httpClient,
	}
}

func (c *jaegerClient) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, truncate(string(body), 256))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

// Services lists the trace-emitting services known to the source VT.
func (c *jaegerClient) Services(ctx context.Context) ([]string, error) {
	var out jaegerServicesResponse
	if err := c.get(ctx, "/select/jaeger/api/services", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// Traces fetches traces for a service within [start, end] (inclusive), capped
// at limit traces.
func (c *jaegerClient) Traces(ctx context.Context, service string, start, end time.Time, limit int) ([]jaegerTrace, error) {
	q := url.Values{}
	q.Set("service", service)
	q.Set("start", strconv.FormatInt(start.UnixMicro(), 10))
	q.Set("end", strconv.FormatInt(end.UnixMicro(), 10))
	q.Set("limit", strconv.Itoa(limit))
	var out jaegerTracesResponse
	if err := c.get(ctx, "/select/jaeger/api/traces", q, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
