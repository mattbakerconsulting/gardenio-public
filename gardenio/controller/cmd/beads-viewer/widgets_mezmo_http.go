package main

// HTTP implementation of MezmoClient backed by the Mezmo MCP HTTP server.
// Bead: ka-0000002519.
//
// This is the production path the beads-viewer uses to materialise live data
// for widget-instance beads with a `mezmo_source` field. The wire shape mimics
// the streamable-HTTP MCP server the agent pods talk to today
// (mcp__mezmo__get_log_histogram / mcp__mezmo__group_logs_by_field /
// mcp__mezmo__deduplicate_logs_relative_time). Authentication is bearer-token
// against a viewer-scoped service key.
//
// Credential strategy is undecided as of this commit (gb decision ka-0000007074);
// when no token is configured, NewHTTPMezmoClient returns nil and the server
// falls through to the no-op client. Once the operator picks a strategy, main
// can construct a real client and pass it to Server.SetMezmoClient.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// httpMezmoClient is the production implementation. It speaks the MCP HTTP
// streaming protocol — POST a tools/call request, read the JSON response.
// Errors and non-2xx responses bubble up as ErrMezmoUnavailable-style
// errors so the apply pipeline degrades to baked fields cleanly.
type httpMezmoClient struct {
	baseURL string
	token   string
	hc      *http.Client
}

// NewHTTPMezmoClient builds a client. baseURL must point at the MCP HTTP
// server (e.g. "https://mezmo-mcp.internal/v1"). token is the bearer service
// key. Returns nil when either is empty so callers can fall through to the
// no-op client without branching on configuration.
func NewHTTPMezmoClient(baseURL, token string) MezmoClient {
	baseURL = strings.TrimSpace(baseURL)
	token = strings.TrimSpace(token)
	if baseURL == "" || token == "" {
		return nil
	}
	return &httpMezmoClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		hc:      &http.Client{Timeout: 10 * time.Second},
	}
}

// callMCP issues a tools/call request and decodes the JSON content body.
// MCP responses come back as a JSON envelope with a `content` array of typed
// blocks; we look for the first block of type "json" and decode its `json`
// field into out. Anything else returns ErrMezmoUnavailable.
func (c *httpMezmoClient) callMCP(ctx context.Context, tool string, params map[string]any, out any) error {
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      tool,
			"arguments": params,
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/mcp", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("mezmo MCP %s: status %d", tool, resp.StatusCode)
	}
	var env struct {
		Result struct {
			Content []struct {
				Type string          `json:"type"`
				JSON json.RawMessage `json:"json"`
				Text string          `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return err
	}
	if env.Error != nil {
		return fmt.Errorf("mezmo MCP %s: %s", tool, env.Error.Message)
	}
	for _, block := range env.Result.Content {
		switch block.Type {
		case "json":
			if len(block.JSON) == 0 {
				continue
			}
			return json.Unmarshal(block.JSON, out)
		case "text":
			if block.Text == "" {
				continue
			}
			return json.Unmarshal([]byte(block.Text), out)
		}
	}
	return ErrMezmoUnavailable
}

func (c *httpMezmoClient) Histogram(ctx context.Context, query, window string) ([]HistogramPoint, error) {
	args := map[string]any{
		"query": query,
		"since": "last " + window,
	}
	var out struct {
		Buckets []struct {
			Time  any     `json:"time"`
			Count float64 `json:"count"`
			Value float64 `json:"value"`
		} `json:"buckets"`
	}
	if err := c.callMCP(ctx, "get_log_histogram", args, &out); err != nil {
		return nil, err
	}
	pts := make([]HistogramPoint, 0, len(out.Buckets))
	for _, b := range out.Buckets {
		pts = append(pts, HistogramPoint{
			Time:  parseHistogramTime(b.Time),
			Value: pickValue(b.Count, b.Value),
		})
	}
	return pts, nil
}

func (c *httpMezmoClient) GroupBy(ctx context.Context, query, field, window string) ([]GroupBucket, error) {
	args := map[string]any{
		"query":       query,
		"field":       field,
		"aggregation": "count",
		"since":       "last " + window,
	}
	var out struct {
		Buckets []struct {
			Key   string `json:"key"`
			Count int64  `json:"count"`
		} `json:"buckets"`
	}
	if err := c.callMCP(ctx, "group_logs_by_field", args, &out); err != nil {
		return nil, err
	}
	rows := make([]GroupBucket, 0, len(out.Buckets))
	for _, b := range out.Buckets {
		rows = append(rows, GroupBucket{Key: b.Key, Count: b.Count})
	}
	return rows, nil
}

func (c *httpMezmoClient) Dedup(ctx context.Context, query, window string, limit int) ([]LogEntry, error) {
	args := map[string]any{
		"query": query,
		"since": "last " + window,
	}
	var out struct {
		Entries []struct {
			Timestamp string `json:"timestamp"`
			App       string `json:"app"`
			Host      string `json:"host"`
			Level     string `json:"level"`
			Line      string `json:"line"`
			Message   string `json:"message"`
		} `json:"entries"`
	}
	if err := c.callMCP(ctx, "deduplicate_logs_relative_time", args, &out); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > len(out.Entries) {
		limit = len(out.Entries)
	}
	rows := make([]LogEntry, 0, limit)
	for i, e := range out.Entries {
		if i >= limit {
			break
		}
		t, _ := time.Parse(time.RFC3339, e.Timestamp)
		actor := e.App
		if actor == "" {
			actor = e.Host
		}
		text := e.Line
		if text == "" {
			text = e.Message
		}
		rows = append(rows, LogEntry{
			When:  t,
			Actor: actor,
			Kind:  strings.ToLower(e.Level),
			Text:  text,
		})
	}
	return rows, nil
}

// parseHistogramTime accepts unix-seconds (number) or RFC3339 (string).
// Anything else returns 0 so the splice path drops the bucket.
func parseHistogramTime(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case string:
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed.Unix()
		}
	}
	return 0
}

// pickValue returns count when set; otherwise value. Histogram responses
// vary by metric kind — the apply path doesn't care which is populated.
func pickValue(count, value float64) float64 {
	if count != 0 {
		return count
	}
	return value
}
