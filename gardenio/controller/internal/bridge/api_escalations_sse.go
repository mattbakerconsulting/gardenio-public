package bridge

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// EscalationSSEProxy streams escalation lifecycle events to HTTP clients via SSE.
// It connects to the kbeads event stream and filters for escalation-type beads.
type EscalationSSEProxy struct {
	beadsHTTPAddr  string
	beadsToken     string
	allowedOrigins []string
	logger         *slog.Logger
}

// NewEscalationSSEProxy creates an SSE proxy for escalation events.
// allowedOrigins restricts which origins may connect via CORS. When empty,
// no CORS headers are sent (denying cross-origin access).
func NewEscalationSSEProxy(beadsHTTPAddr, beadsToken string, allowedOrigins []string, logger *slog.Logger) *EscalationSSEProxy {
	return &EscalationSSEProxy{
		beadsHTTPAddr:  beadsHTTPAddr,
		beadsToken:     beadsToken,
		allowedOrigins: allowedOrigins,
		logger:         logger,
	}
}

// escalationSSEEvent is the JSON payload sent to web clients.
type escalationSSEEvent struct {
	Type string    `json:"type"` // "created", "updated", "closed"
	Bead BeadEvent `json:"bead"`
}

// ServeHTTP handles GET /api/escalations/events as an SSE stream.
func (p *EscalationSSEProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	setCORSOrigin(w, r, p.allowedOrigins)
	flusher.Flush()

	ctx := r.Context()

	client, err := beadsapi.New(beadsapi.Config{
		HTTPAddr: p.beadsHTTPAddr,
		Token:    p.beadsToken,
	})
	if err != nil {
		p.logger.Error("failed to create beads client for escalation SSE proxy", "error", err)
		return
	}
	defer client.Close()

	events, err := client.EventStream(ctx, "beads.bead.created,beads.bead.closed,beads.bead.updated")
	if err != nil {
		p.logger.Error("failed to connect to SSE event stream for escalations", "error", err)
		return
	}

	p.logger.Debug("escalation SSE proxy connected")

	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			p.handleEvent(w, flusher, evt)
		}
	}
}

// handleEvent filters for escalation-type events and writes them to the SSE client.
func (p *EscalationSSEProxy) handleEvent(w http.ResponseWriter, flusher http.Flusher, evt beadsapi.SSEEvent) {
	bead := ParseBeadEvent(evt.Data)
	if bead == nil || bead.Type != "escalation" {
		return
	}

	eventType := ""
	switch {
	case strings.Contains(evt.Event, "created"):
		eventType = "created"
	case strings.Contains(evt.Event, "closed"):
		eventType = "closed"
	case strings.Contains(evt.Event, "updated"):
		eventType = "updated"
	default:
		return
	}

	payload := escalationSSEEvent{
		Type: eventType,
		Bead: *bead,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	fmt.Fprintf(w, "event: escalation\ndata: %s\n\n", data)
	flusher.Flush()
}
