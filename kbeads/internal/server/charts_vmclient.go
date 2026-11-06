package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// charts_vmclient.go — the in-house chart backend's VictoriaMetrics client.
//
// This is the first vertical slice of the chart backend whose architecture is
// laid out in gardenio/docs/research/timeseries-chart-backend-arch.md (R5).
// That doc's §3 recommends the chart backend live inside kbeads as a set of
// /v1/charts/* endpoints rather than a new microservice; this file is the
// VictoriaMetrics access layer (§3 "vmclient.go") those endpoints share.
//
// We deliberately keep this tiny. The full Vega-Lite catalog/spec backend is
// future work — the only chart the customer-1 telemetry plan needs first is
// the DAU/WAU/MAU uniqueness panel (charts_uniqueness.go), which runs a single
// PromQL instant query. A thin instant-query client is all that requires; we
// do NOT pull in github.com/prometheus/prometheus (it drags the entire k8s
// client tree into the lean kbeads daemon) when a 60-line HTTP client over
// VictoriaMetrics' /api/v1/query endpoint answers the question faithfully.

// VMClient issues PromQL instant queries against a VictoriaMetrics
// (vmselect/vmsingle) HTTP endpoint. Only the instant-query shape the
// uniqueness panel needs is implemented; range queries land when a charting
// surface needs a time series.
type VMClient struct {
	baseURL string
	token   string
	hc      *http.Client
}

// NewVMClient builds a client against baseURL (e.g.
// "http://gardenio-vmsingle.gardenio.svc:8428"). token is an optional bearer
// for deployments that put vmselect behind ForwardAuth; pass "" when the
// endpoint is reachable in-cluster without auth. Returns nil when baseURL is
// empty so callers can fall through to a "charts disabled" path without
// branching on configuration — mirrors NewHTTPMezmoClient's nil-on-unconfigured
// idiom on the beads-viewer side.
func NewVMClient(baseURL, token string) *VMClient {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil
	}
	return &VMClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   strings.TrimSpace(token),
		// 5s is generous: the uniqueness query benchmarks at single-digit
		// milliseconds p99 (gardenio/docs/research/dau-wau-mau-benchmark.md);
		// the budget exists only to bound a wedged vmselect.
		hc: &http.Client{Timeout: 5 * time.Second},
	}
}

// vmInstantResponse is the subset of the VictoriaMetrics /api/v1/query
// envelope we consume. The uniqueness panel asks for a scalar-shaped distinct
// count (single sample); the engagement panels ask for a labeled instant
// vector (one sample per operator_id / surface / model). Both shapes share this
// envelope — metric carries the per-series label set, value the [ts,"n"] pair.
// The parser tolerates an empty result (no series in the window) without
// erroring.
type vmInstantResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			// Value is [ <unix_ts float>, "<stringified number>" ].
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
	// Populated on status:"error".
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// VMSample is one labeled instant-vector sample: the series' label set plus its
// scalar value at the evaluation instant. The engagement query layer reads the
// chosen group label (e.g. "operator_id") off Metric and Value as the count /
// sum / duration for that group.
type VMSample struct {
	Metric map[string]string
	Value  float64
}

// doInstant POSTs one PromQL instant query at the given evaluation time and
// returns the decoded envelope. Shared by QueryScalar and QueryVector so the
// auth header, content type, timeout, and status/error handling live in one
// place.
func (c *VMClient) doInstant(ctx context.Context, promql string, at time.Time) (*vmInstantResponse, error) {
	form := url.Values{}
	form.Set("query", promql)
	form.Set("time", strconv.FormatInt(at.Unix(), 10))

	endpoint := c.baseURL + "/api/v1/query"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build vm request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vm query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vm query: HTTP %d", resp.StatusCode)
	}

	var out vmInstantResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode vm response: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("vm query failed: %s: %s", out.ErrorType, out.Error)
	}
	return &out, nil
}

// QueryScalar runs a PromQL instant query at the given evaluation time and
// returns the single scalar value it produces. A query that yields an empty
// instant vector (e.g. count() over no matching series) returns (0, true, nil)
// — "no data" is a valid zero, not an error. ok is false only when the value
// could not be determined; err is non-nil on transport / decode / PromQL
// errors.
func (c *VMClient) QueryScalar(ctx context.Context, promql string, at time.Time) (value float64, ok bool, err error) {
	out, err := c.doInstant(ctx, promql, at)
	if err != nil {
		return 0, false, err
	}
	if len(out.Data.Result) == 0 {
		// Empty vector — count over no series. Honest zero.
		return 0, true, nil
	}
	return parseVMSampleValue(out.Data.Result[0].Value)
}

// QueryVector runs a PromQL instant query at the given evaluation time and
// returns every labeled sample it produces — the shape behind the engagement
// panels, where a `sum by (operator_id)(...)` / `... by (surface)` query yields
// one sample per group. An empty result is returned as an empty (non-nil)
// slice, not an error: "no operator was active" is a valid zero-row answer. A
// sample whose value element is malformed fails the whole call so a partial /
// corrupt page never silently under-reports.
func (c *VMClient) QueryVector(ctx context.Context, promql string, at time.Time) ([]VMSample, error) {
	out, err := c.doInstant(ctx, promql, at)
	if err != nil {
		return nil, err
	}
	samples := make([]VMSample, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		v, ok, err := parseVMSampleValue(r.Value)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		samples = append(samples, VMSample{Metric: r.Metric, Value: v})
	}
	return samples, nil
}

// parseVMSampleValue extracts the numeric value from a VictoriaMetrics
// instant-vector sample's [ts, "value"] pair. The value element is a JSON
// string ("4"), not a JSON number, per the Prometheus HTTP API contract.
func parseVMSampleValue(value []json.RawMessage) (float64, bool, error) {
	if len(value) != 2 {
		return 0, false, fmt.Errorf("unexpected vm sample shape: %d elements", len(value))
	}
	var s string
	if err := json.Unmarshal(value[1], &s); err != nil {
		return 0, false, fmt.Errorf("decode vm sample value: %w", err)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse vm sample value %q: %w", s, err)
	}
	return f, true, nil
}

// --- Range queries (the chart catalog's time-series access layer) -----------
//
// The instant-query layer above answers the engagement/uniqueness panels,
// which need a single sample per series. The chart catalog (E5,
// gardenio/plans/timeseries-platform.md §2.E5) renders time series — a line of
// spend-per-hour, a rate of beads-events over a window — and so needs the
// /api/v1/query_range endpoint that returns a *matrix*: one series per label
// set, each with an aligned vector of [t, value] points. QueryRange below is
// the range twin of doInstant/QueryVector; it shares the same auth header,
// timeout, and honest-zero-on-empty conventions so the catalog handlers can
// treat "no data" (E3/E4 metrics not yet emitting) as a valid empty result
// rather than an error.

// VMPoint is one sample in a range-query series: the evaluation timestamp and
// the value at that step. Time is the bucket's left edge (VictoriaMetrics
// aligns range steps to the step grid), in UTC.
type VMPoint struct {
	T     time.Time
	Value float64
}

// VMSeries is one labeled time series returned by a range query: the series'
// label set plus its step-aligned points in ascending time order. The catalog
// reshapes this into long-format data rows {t, <series-key>, <value>} for the
// ChartRender contract.
type VMSeries struct {
	Metric map[string]string
	Points []VMPoint
}

// vmRangeResponse is the subset of the VictoriaMetrics /api/v1/query_range
// envelope we consume. resultType is "matrix"; each result carries the series'
// label set in metric and its samples in values, where each value is the same
// [ <unix_ts float>, "<stringified number>" ] pair shape the instant envelope
// uses. The parser tolerates an empty result (no series in the window) without
// erroring.
type vmRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string   `json:"metric"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
	// Populated on status:"error".
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// QueryRange runs a PromQL range query over [start, end] at the given step and
// returns every labeled series it produces. An empty result is returned as an
// empty (non-nil) slice, not an error: a chart whose source metric is not yet
// emitting (E3 spend, E4 beads-events) is a valid empty time series, not a
// failure. step must be positive; start must not be after end. A series whose
// sample value is malformed fails the whole call so a partial/corrupt chart
// never silently under-reports.
func (c *VMClient) QueryRange(ctx context.Context, promql string, start, end time.Time, step time.Duration) ([]VMSeries, error) {
	if step <= 0 {
		return nil, fmt.Errorf("vm range query: step must be positive, got %s", step)
	}
	if start.After(end) {
		return nil, fmt.Errorf("vm range query: start %s is after end %s", start, end)
	}

	form := url.Values{}
	form.Set("query", promql)
	form.Set("start", strconv.FormatInt(start.Unix(), 10))
	form.Set("end", strconv.FormatInt(end.Unix(), 10))
	form.Set("step", strconv.FormatInt(int64(step.Seconds()), 10))

	endpoint := c.baseURL + "/api/v1/query_range"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build vm range request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vm range query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vm range query: HTTP %d", resp.StatusCode)
	}

	var out vmRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode vm range response: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("vm range query failed: %s: %s", out.ErrorType, out.Error)
	}

	series := make([]VMSeries, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		points := make([]VMPoint, 0, len(r.Values))
		for _, raw := range r.Values {
			t, v, ok, err := parseVMRangePoint(raw)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			points = append(points, VMPoint{T: t, Value: v})
		}
		series = append(series, VMSeries{Metric: r.Metric, Points: points})
	}
	return series, nil
}

// parseVMRangePoint extracts (timestamp, value) from one VictoriaMetrics
// range-sample [ <unix_ts float>, "<value>" ] pair. The timestamp element is a
// JSON number (seconds since epoch, possibly fractional); the value element is
// a JSON string, per the Prometheus HTTP API contract. ok is false (no error)
// when the value is the literal "NaN" gap marker VictoriaMetrics emits for
// empty buckets, so a gappy series yields a shorter point list rather than a
// run of fake zeros.
func parseVMRangePoint(value []json.RawMessage) (time.Time, float64, bool, error) {
	if len(value) != 2 {
		return time.Time{}, 0, false, fmt.Errorf("unexpected vm range sample shape: %d elements", len(value))
	}
	var ts float64
	if err := json.Unmarshal(value[0], &ts); err != nil {
		return time.Time{}, 0, false, fmt.Errorf("decode vm range sample timestamp: %w", err)
	}
	var s string
	if err := json.Unmarshal(value[1], &s); err != nil {
		return time.Time{}, 0, false, fmt.Errorf("decode vm range sample value: %w", err)
	}
	if s == "NaN" {
		// Gap bucket — VictoriaMetrics fills absent steps with NaN. Drop it so
		// the series carries only real samples.
		return time.Time{}, 0, false, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("parse vm range sample value %q: %w", s, err)
	}
	sec := int64(ts)
	nsec := int64((ts - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC(), f, true, nil
}
