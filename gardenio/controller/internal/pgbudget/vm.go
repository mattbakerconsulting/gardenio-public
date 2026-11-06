package pgbudget

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Sample is one series of an instant query's vector result.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Querier runs one instant PromQL query evaluated at at.
type Querier interface {
	Query(ctx context.Context, promql string, at time.Time) ([]Sample, error)
}

// VMClient is a Querier over vmsingle's /api/v1/query.
type VMClient struct {
	BaseURL string // e.g. http://gardenio-vmsingle:8428
	HTTP    *http.Client
}

// Query implements Querier. A non-finite sample (NaN from a 0/0) is dropped.
func (c *VMClient) Query(ctx context.Context, promql string, at time.Time) ([]Sample, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return nil, fmt.Errorf("pgbudget: no vmsingle URL")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	v := url.Values{"query": {promql}, "time": {strconv.FormatInt(at.Unix(), 10)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/api/v1/query?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pgbudget: query vmsingle: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("pgbudget: read vmsingle response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pgbudget: vmsingle %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)])))
	}
	return parseVector(body)
}

func parseVector(body []byte) ([]Sample, error) {
	var r struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("pgbudget: vmsingle response: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("pgbudget: vmsingle: %s", r.Error)
	}
	if r.Data.ResultType != "vector" {
		return nil, fmt.Errorf("pgbudget: vmsingle result type %q, want vector", r.Data.ResultType)
	}
	out := make([]Sample, 0, len(r.Data.Result))
	for _, s := range r.Data.Result {
		str, _ := s.Value[1].(string)
		f, err := strconv.ParseFloat(str, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		out = append(out, Sample{Labels: s.Metric, Value: f})
	}
	return out, nil
}
