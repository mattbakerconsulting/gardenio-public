// Package vlsink holds the VictoriaLogs write sink every in-cluster log
// shipper feeds: HTTPSink POSTs a newline-delimited JSON batch to VictoriaLogs'
// /insert/jsonline API. Its users are bridge-log-shipper, core-log-shipper,
// k3s-audit-shipper and traefik-log-shipper (internal/bridgeshipper,
// internal/k3sauditshipper, internal/traefikshipper). The read side is
// internal/vlogs.
package vlsink

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// Sink accepts a batch of already-enriched newline-delimited JSON bytes and
// delivers it to VictoriaLogs (or a test double).
type Sink interface {
	Insert(ctx context.Context, jsonlines []byte) error
}

// HTTPSink is the production Sink: it POSTs a JSONL batch to a VictoriaLogs
// /insert/jsonline endpoint.
type HTTPSink struct {
	// Endpoint is the full VictoriaLogs jsonline ingest URL, e.g.
	// http://gardenio-victoria-logs:9428/insert/jsonline. Stream-field and
	// message-field selection are set via query params baked into this URL by
	// the caller (the cmd), so the sink itself stays a dumb POST.
	Endpoint string
	Client   *http.Client
	// Headers are extra request headers, e.g. VictoriaLogs' AccountID/ProjectID
	// tenant selectors. Nil sends none (the default 0:0 tenant).
	Headers map[string]string
}

// Insert POSTs the batch to VictoriaLogs. A non-2xx response is an error, so
// the caller sees a rejected batch and applies its own retry policy.
func (h *HTTPSink) Insert(ctx context.Context, jsonlines []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint, bytes.NewReader(jsonlines))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	for k, v := range h.Headers {
		req.Header.Set(k, v)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("VictoriaLogs /insert returned %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	// Drain the body so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
