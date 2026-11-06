package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// xcheckVMQuery runs a single instant PromQL/MetricsQL query against vmsingle and
// returns the scalar result. It is the controller-side counterpart of the
// beads-viewer's queryFleetRate (cmd/beads-viewer/costs_rate.go): most cross-check
// pairs (P1, P2, P3, P6, P9-A) derive their two sides from metrics the fleet
// already publishes, so the honest "second data source" is simply reading those
// series back out of vmsingle rather than re-hitting LiteLLM (which would double
// the API load AND would not exercise the emit-and-scrape plumbing the pairs are
// meant to audit).
//
// The query is expected to reduce to a single scalar (wrap multi-series
// expressions in sum(...) / a scalar aggregation). The bool return is:
//   - false when no vmsingle upstream is wired (local dev / tests), the upstream
//     errors / returns non-200 / an unparseable body, or the result vector is
//     empty (the metric does not exist yet in this environment) — the sweep then
//     leaves that pair absent rather than publishing a fabricated agreement;
//   - true with the parsed value otherwise, including a real 0.
type xcheckVMClient struct {
	upstream string // vmsingle base URL, e.g. http://gardenio-vmsingle:8428 ("" disables)
	http     *http.Client
}

// vmInstantQueryPath is vmsingle's instant-query endpoint (same as the
// beads-viewer's vmsingleInstantQueryPath; duplicated here so the controller
// package has no dependency on the beads-viewer package).
const vmInstantQueryPath = "/api/v1/query"

// query runs one instant query and returns (value, ok). See the type doc for the
// availability semantics behind ok=false.
func (c *xcheckVMClient) query(ctx context.Context, promql string) (float64, bool) {
	if c == nil || strings.TrimSpace(c.upstream) == "" {
		return 0, false
	}
	client := c.http
	if client == nil {
		client = http.DefaultClient
	}

	u := strings.TrimRight(c.upstream, "/") + vmInstantQueryPath + "?query=" + url.QueryEscape(promql)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, false
	}
	return parseVMInstantScalar(body)
}

// vmInstantResponse is the subset of vmsingle's /api/v1/query JSON we read: a
// "vector"/"scalar" result whose sample value is [<ts>, "<stringified float>"].
type vmInstantResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value [2]json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// parseVMInstantScalar extracts the scalar value from a vmsingle instant
// response. Split out from the HTTP round-trip so JSON-shape handling is
// unit-testable without a server. Returns (0,false) for any shape that does not
// carry exactly one finite numeric sample (mirrors parseFleetHourlyRate).
func parseVMInstantScalar(body []byte) (float64, bool) {
	var parsed vmInstantResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, false
	}
	if parsed.Status != "success" || len(parsed.Data.Result) == 0 {
		return 0, false
	}
	raw := parsed.Data.Result[0].Value[1]
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(str), 64)
	if err != nil {
		return 0, false
	}
	// vmsingle can emit NaN/±Inf as "NaN"/"+Inf"; ParseFloat accepts those but
	// they are not a meaningful value for a conservation sum, so reject.
	if v != v || v > 1e300 || v < -1e300 {
		return 0, false
	}
	return v, true
}

// vmAvailable reports whether a vmsingle upstream is configured at all — used by
// the sweep to skip (and log once) the PromQL-backed pairs wholesale when there
// is no metrics upstream, distinct from a per-pair "metric absent" miss.
func (c *xcheckVMClient) vmAvailable() bool {
	return c != nil && strings.TrimSpace(c.upstream) != ""
}
