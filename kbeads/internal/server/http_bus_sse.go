package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"gardenio/kbeads/internal/eventbus"
	"github.com/nats-io/nats.go"
)

// busSSEEvent is the JSON payload sent in each SSE data field.
type busSSEEvent struct {
	Stream  string          `json:"stream"`  // Short stream name (e.g., "hooks")
	Type    string          `json:"type"`    // Event type extracted from subject
	Subject string          `json:"subject"` // Full NATS subject
	Seq     uint64          `json:"seq"`     // JetStream sequence number
	TS      string          `json:"ts"`      // ISO 8601 timestamp
	Payload json.RawMessage `json:"payload"` // Raw event JSON
}

// handleBusEvents handles GET /v1/bus/events (JetStream SSE endpoint).
//
// Query parameters:
//   - stream: comma-separated stream names (hooks, decisions, agents, mail,
//     mutations, config, gate, inbox, jack) or "all" (default: all)
//   - filter: event type filter (e.g., "Stop", "MutationCreate")
//   - since: ISO 8601 timestamp — replay events from this time (default: new only).
//     A replay is lossless: it applies backpressure rather than dropping. A
//     live tail drops events a slow client cannot keep up with and reports
//     each drop as a ":dropped N" SSE comment.
//   - subject_contains: only emit events whose subject contains this string
func (s *BeadsServer) handleBusEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	if s.bus == nil || !s.bus.JetStreamEnabled() {
		writeError(w, http.StatusServiceUnavailable, "JetStream not configured (set BEADS_NATS_URL)")
		return
	}

	js := s.bus.JetStream()

	// Parse stream selection.
	streams := eventbus.StreamNames // default: all
	if q := r.URL.Query().Get("stream"); q != "" && q != "all" {
		streams = nil
		for _, name := range strings.Split(q, ",") {
			name = strings.TrimSpace(name)
			if name != "" && eventbus.SubjectPrefixForStream(name) != "" {
				streams = append(streams, name)
			}
		}
		if len(streams) == 0 {
			writeError(w, http.StatusBadRequest, "no valid stream names provided")
			return
		}
	}

	filter := r.URL.Query().Get("filter")
	subjectContains := r.URL.Query().Get("subject_contains")

	// Parse optional "since" for historical replay. A replay delivers the
	// backlog far faster than the handler can write SSE, so it must apply
	// backpressure instead of dropping (gd-P7jMtvwJCk): an ordered consumer
	// (no acks, server-side flow control) lets the subscription callback
	// block on the events channel without redelivery or client-side
	// slow-consumer loss.
	subOpts := []nats.SubOpt{nats.DeliverNew()}
	replay := false
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err == nil {
			subOpts = []nats.SubOpt{nats.StartTime(t), nats.OrderedConsumer()}
			replay = true
		}
	}

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	// Create ephemeral push subscriptions for each requested stream. A replay
	// blocks until the handler takes the message (or the client goes away);
	// a live tail drops on a full channel so a slow client cannot stall the
	// consumer, and the handler reports each drop as a ":dropped N" comment.
	events := make(chan *nats.Msg, 128)
	var dropped atomic.Uint64
	var subs []*nats.Subscription
	for _, name := range streams {
		prefix := eventbus.SubjectPrefixForStream(name)
		sub, err := js.Subscribe(prefix+">", func(msg *nats.Msg) {
			if replay {
				select {
				case events <- msg:
				case <-ctx.Done():
				}
				return
			}
			select {
			case events <- msg:
			default:
				dropped.Add(1)
			}
		}, subOpts...)
		if err != nil {
			slog.Warn("bus SSE: failed to subscribe to stream", "stream", name, "err", err)
			continue
		}
		subs = append(subs, sub)
	}

	if len(subs) == 0 {
		writeError(w, http.StatusServiceUnavailable, "failed to subscribe to any streams")
		return
	}

	// Clean up subscriptions on disconnect.
	defer func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	}()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	var seq atomic.Uint64

	// reportDrops tells the client how many live events were dropped since
	// the last report, so a lossy tail is visible rather than silent.
	reportDrops := func() {
		if n := dropped.Swap(0); n > 0 {
			fmt.Fprintf(w, ":dropped %d\n\n", n)
			flusher.Flush()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-events:
			reportDrops()

			streamName := eventbus.StreamForSubject(msg.Subject)
			eventType := eventbus.EventTypeFromSubject(msg.Subject)

			// Apply event type filter.
			if filter != "" && eventType != filter {
				continue
			}

			// Apply subject filter (e.g., filter to a specific agent).
			if subjectContains != "" && !strings.Contains(msg.Subject, subjectContains) {
				continue
			}

			meta, _ := msg.Metadata()
			var jsSeq uint64
			var ts string
			if meta != nil {
				jsSeq = meta.Sequence.Stream
				ts = meta.Timestamp.UTC().Format(time.RFC3339)
			} else {
				ts = time.Now().UTC().Format(time.RFC3339)
			}

			evt := busSSEEvent{
				Stream:  streamName,
				Type:    eventType,
				Subject: msg.Subject,
				Seq:     jsSeq,
				TS:      ts,
				Payload: msg.Data,
			}

			data, err := json.Marshal(evt)
			if err != nil {
				continue
			}

			id := seq.Add(1)
			fmt.Fprintf(w, "id:%d\n", id)
			fmt.Fprintf(w, "event:%s\n", streamName)
			fmt.Fprintf(w, "data:%s\n\n", data)
			flusher.Flush()

		case <-keepalive.C:
			reportDrops()
			fmt.Fprintf(w, ":keepalive\n\n")
			flusher.Flush()
		}
	}
}
