package beadsapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SSEEvent represents a parsed Server-Sent Event.
type SSEEvent struct {
	ID    string          // SSE id: line (empty when the server sent none)
	Event string          // event type (e.g., "bead.closed")
	Data  json.RawMessage // raw JSON payload
}

// Event mirrors a row from the daemon's events table — one event per
// bead.created / bead.updated / bead.closed / dependency.* / label.* /
// comment.added on a bead. Returned by GetEvents.
type Event struct {
	ID         int64           `json:"id"`
	Topic      string          `json:"topic"`
	BeadID     string          `json:"bead_id"`
	Actor      string          `json:"actor,omitempty"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
	OperatedBy string          `json:"operated_by,omitempty"`
}

// GetEvents fetches the full event history for a bead from the daemon.
// Wraps GET /v1/beads/{id}/events; returns rows in daemon order (ascending
// id, i.e. oldest first). Callers that want the rail's newest-first order
// should reverse client-side.
//
// Prefer GetRecentEvents for activity-feed renders — trigger beads can have
// >30 MB of event history (bug ka-0000002256) and loading the full payload
// at request time can OOMKill the caller pod under modest concurrency.
func (c *Client) GetEvents(ctx context.Context, beadID string) ([]Event, error) {
	var resp struct {
		Events []Event `json:"events"`
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/events"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting events for %s: %w", beadID, err)
	}
	return resp.Events, nil
}

// GetRecentEvents fetches the most-recent `limit` events for a bead, in the
// same ascending-by-created_at order as GetEvents. limit must be in [1, 10000].
// Backed by ?limit=N on GET /v1/beads/{id}/events.
func (c *Client) GetRecentEvents(ctx context.Context, beadID string, limit int) ([]Event, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	var resp struct {
		Events []Event `json:"events"`
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/events?" + q.Encode()
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting recent events for %s: %w", beadID, err)
	}
	return resp.Events, nil
}

// GetRecentEventsByTopic is GetRecentEvents narrowed to one topic: the
// most-recent `limit` events on the bead whose topic is `topic`, ascending.
// Backed by ?topic=T&limit=N on GET /v1/beads/{id}/events (gd-cP9tODid8j). A
// caller after one signal (a label.added / label.removed scan) gets only
// those rows instead of the bead's whole recent history, whose bead.updated
// payloads carry full bead bodies.
//
// A daemon that predates ?topic= ignores it and answers like GetRecentEvents,
// so callers must still check each event's Topic themselves.
func (c *Client) GetRecentEventsByTopic(ctx context.Context, beadID, topic string, limit int) ([]Event, error) {
	q := url.Values{}
	q.Set("topic", topic)
	q.Set("limit", strconv.Itoa(limit))
	var resp struct {
		Events []Event `json:"events"`
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/events?" + q.Encode()
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting recent %s events for %s: %w", topic, beadID, err)
	}
	return resp.Events, nil
}

// ListGlobalEventsRequest parameterizes one page of GET /v1/events — the
// durable, Postgres-backed cross-bead event log. A zero Since means the
// server default (24h before Until); a zero Until means now. Cursor is the
// last event id already consumed (0 = start of window). Limit is the page
// size, capped server-side at 1000. When Desc is true the page is ordered
// newest-first (created_at DESC, id DESC) and Cursor selects events with
// id < Cursor — so a caller can page backward from the newest event and bound
// its scan to exactly the most-recent rows it paints (the /feed first-paint
// seed, bug ka-0000004135).
type ListGlobalEventsRequest struct {
	Since  time.Time
	Until  time.Time
	Cursor int64
	Limit  int
	Desc   bool
	// ExcludeTypes drops events whose bead has one of these types, server-side
	// (GET /v1/events?exclude_types=). The /feed seed uses it so its bounded
	// raw-event scan is not spent on bookkeeping beads it never displays
	// (gd-Mfr8MRXbYG).
	ExcludeTypes []string
}

// ListGlobalEventsResponse is one page of GET /v1/events. NextCursor is the id
// to pass as the next request's Cursor; it is only meaningful when HasMore is
// true. Events are ordered chronologically (created_at ASC, id ASC).
type ListGlobalEventsResponse struct {
	Events     []Event `json:"events"`
	HasMore    bool    `json:"has_more"`
	NextCursor string  `json:"next_cursor"`
}

// ListGlobalEvents fetches one page of the durable cross-bead event log via
// GET /v1/events. Unlike EventStream (live SSE, in-memory per-process ids),
// this is durable and Postgres-backed: it survives viewer/daemon restarts and
// supports reconnect backfill by time window. The activity feed uses it for
// both first-paint seeding and gap-fill on reconnect (?since=<last-event-ts>),
// which is why backfill keys off created_at rather than the SSE Last-Event-ID
// (those ids are an in-process counter, not the durable events.id).
func (c *Client) ListGlobalEvents(ctx context.Context, req ListGlobalEventsRequest) (*ListGlobalEventsResponse, error) {
	q := url.Values{}
	if !req.Since.IsZero() {
		q.Set("since", req.Since.UTC().Format(time.RFC3339))
	}
	if !req.Until.IsZero() {
		q.Set("until", req.Until.UTC().Format(time.RFC3339))
	}
	if req.Cursor > 0 {
		q.Set("cursor", strconv.FormatInt(req.Cursor, 10))
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	if req.Desc {
		q.Set("order", "desc")
	}
	if len(req.ExcludeTypes) > 0 {
		q.Set("exclude_types", strings.Join(req.ExcludeTypes, ","))
	}
	path := "/v1/events"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var resp ListGlobalEventsResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("listing global events: %w", err)
	}
	return &resp, nil
}

// EventStream opens an SSE connection to the daemon and returns a channel of
// events. The channel is closed when the context is canceled or the stream ends.
// topics is the NATS-style topic filter (e.g., "beads.bead.>", "decisions.>").
// It is EventStreamScoped with no agent scope and no replay cursor.
func (c *Client) EventStream(ctx context.Context, topics string) (<-chan SSEEvent, error) {
	return c.EventStreamScoped(ctx, topics, "", "")
}

// EventStreamScoped is EventStream with the daemon's `agent` scope (spec
// §3.3: only that agent bead's events and the mail addressed to it) and a
// Last-Event-ID cursor, so a reconnect replays what the daemon's ring buffer
// still holds. Empty agentBead or lastEventID omits that part.
func (c *Client) EventStreamScoped(ctx context.Context, topics, agentBead, lastEventID string) (<-chan SSEEvent, error) {
	q := url.Values{}
	if topics != "" {
		q.Set("topics", topics)
	}
	if agentBead != "" {
		q.Set("agent", agentBead)
	}
	path := "/v1/events/stream"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("creating SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	override := ""
	if c.systemPrincipal != nil {
		// A stream carries no human write and the daemon authenticates it once,
		// at connect: it goes out as the system principal, never the static token.
		tok, err := c.systemPrincipalToken(ctx, http.MethodGet, path)
		if err != nil {
			return nil, fmt.Errorf("setting SSE auth: %w", err)
		}
		override = tok
	}
	if _, err := c.setAuth(ctx, req, override); err != nil {
		return nil, fmt.Errorf("setting SSE auth: %w", err)
	}

	// Use the dedicated SSE client (no timeout) for long-lived streams.
	resp, err := c.sseClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to SSE stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("SSE stream returned HTTP %d", resp.StatusCode)
	}

	ch := make(chan SSEEvent, 16)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		// Allow events past the default 64KB so a full bead representation
		// in an SSE event doesn't disconnect the stream. Match the
		// gardenio/controller/internal/subscriber/sse.go ceiling.
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var eventID, eventType string
		var dataLines []string

		for scanner.Scan() {
			line := scanner.Text()

			if line == "" {
				// Empty line = end of event.
				if len(dataLines) > 0 {
					data := strings.Join(dataLines, "\n")
					select {
					case ch <- SSEEvent{ID: eventID, Event: eventType, Data: json.RawMessage(data)}:
					case <-ctx.Done():
						return
					}
				}
				eventID, eventType = "", ""
				dataLines = nil
				continue
			}

			if strings.HasPrefix(line, "id:") {
				eventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			} else if strings.HasPrefix(line, "event:") {
				eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimPrefix(line, "data:"))
			}
		}
	}()

	return ch, nil
}

// BaseURL returns the client's base URL (useful for constructing SSE URLs externally).
func (c *Client) BaseURL() string {
	return c.baseURL
}
