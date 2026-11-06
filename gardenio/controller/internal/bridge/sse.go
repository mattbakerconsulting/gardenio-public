// Package bridge provides SSE event stream support for the slack-bridge.
//
// The SSEStream connects to kbeads' Server-Sent Events endpoint and delivers
// parsed bead lifecycle events to registered handlers. It replaces the previous
// NATS subscription-based approach with a direct HTTP/SSE connection.
package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// SSEStream connects to the kbeads SSE endpoint and dispatches bead lifecycle
// events to registered topic handlers. It auto-reconnects with exponential
// backoff and uses Last-Event-ID for reconnection replay.
type SSEStream struct {
	baseURL    string
	topics     []string
	token      string               // optional bearer token for kbeads auth
	tokenSrc   beadsapi.TokenSource // optional; wins over token, fetched per connection
	logger     *slog.Logger
	httpClient *http.Client // reused across reconnections (long-lived, no timeout)

	handlers map[string][]SSEHandler // topic -> handlers
	mu       sync.RWMutex
	lastID   string        // last event ID for reconnection; protected by mu
	dedup    *Dedup        // optional event deduplicator
	state    *StateManager // optional state for persisting last event ID

	// Rate-limit state persistence warnings (fires on every SSE event when /data is unwritable).
	persistWarnOnce sync.Once
	persistWarnLast time.Time
}

// SSEHandler is a callback for SSE events on a specific topic.
type SSEHandler func(ctx context.Context, data []byte)

// SSEStreamConfig holds configuration for the SSE event stream.
type SSEStreamConfig struct {
	// BeadsHTTPAddr is the kbeads HTTP address (e.g., "http://localhost:8080").
	BeadsHTTPAddr string
	// Topics is the list of topic patterns to subscribe to.
	Topics []string
	// Token is the optional bearer token for kbeads authentication.
	Token string
	// TokenSource, when non-nil, supplies the bearer for each (re)connect and
	// wins over Token — the component's own machine principal
	// (beadsapi.MachineTokenSourceFromEnv), so the stream does not need the
	// shared static token. A fetch failure fails that connect attempt (the
	// reconnect loop backs off and retries); the static Token is never
	// substituted.
	TokenSource beadsapi.TokenSource
	// Logger for diagnostic output.
	Logger *slog.Logger
	// Dedup is an optional event deduplicator for preventing duplicate notifications.
	Dedup *Dedup
	// State is an optional state manager for persisting the last SSE event ID.
	State *StateManager
}

// NewSSEStream creates a new SSE event stream for the slack-bridge.
func NewSSEStream(cfg SSEStreamConfig) *SSEStream {
	s := &SSEStream{
		baseURL:    strings.TrimRight(cfg.BeadsHTTPAddr, "/"),
		topics:     cfg.Topics,
		token:      cfg.Token,
		tokenSrc:   cfg.TokenSource,
		logger:     cfg.Logger,
		httpClient: &http.Client{Timeout: 0}, // no timeout for long-lived SSE
		handlers:   make(map[string][]SSEHandler),
		dedup:      cfg.Dedup,
		state:      cfg.State,
	}
	// Restore last event ID from persisted state.
	if cfg.State != nil {
		if id := cfg.State.GetLastEventID(); id != "" {
			s.lastID = id
			s.logger.Info("restored SSE last event ID from state", "last_id", id)
		}
	}
	return s
}

// LastID returns the last received SSE event ID, safe for concurrent use.
func (s *SSEStream) LastID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastID
}

// setLastID sets the last received SSE event ID.
func (s *SSEStream) setLastID(id string) {
	s.mu.Lock()
	s.lastID = id
	s.mu.Unlock()
}

// On registers a handler for a specific SSE topic (e.g., "beads.bead.created").
// Safe for concurrent use with dispatch.
func (s *SSEStream) On(topic string, handler SSEHandler) {
	s.mu.Lock()
	s.handlers[topic] = append(s.handlers[topic], handler)
	s.mu.Unlock()
}

// Start connects to the SSE endpoint and streams events to registered handlers.
// Blocks until ctx is canceled. Reconnects with exponential backoff on errors.
func (s *SSEStream) Start(ctx context.Context) error {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := s.stream(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.logger.Warn("SSE connection lost, reconnecting",
			"error", err, "backoff", backoff, "last_id", s.LastID())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// stream opens a single SSE connection and reads events until error or context cancellation.
func (s *SSEStream) stream(ctx context.Context) error {
	url := s.baseURL + "/v1/events/stream"
	if len(s.topics) > 0 {
		url += "?topics=" + strings.Join(s.topics, ",")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if s.tokenSrc != nil {
		tok, err := s.tokenSrc.Token(ctx)
		if err != nil {
			return fmt.Errorf("SSE machine credential: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	} else if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	lastID := s.LastID()
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("SSE connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("SSE endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	s.logger.Info("SSE stream connected",
		"url", url, "last_id", lastID)

	return s.readEvents(ctx, resp.Body)
}

// readEvents reads and dispatches SSE events from the response body.
//
// Use bufio.Reader.ReadString rather than bufio.Scanner: bead events can
// serialize past any fixed token cap (long gardener prompts, audit dumps, big
// wrap-ups), and Scanner returns "token too long" without advancing past the
// offending event — the bridge then reconnects with the same Last-Event-ID
// and replays the oversized event forever, dropping every subsequent
// bead.created/closed/updated event (including new decisions).
// ReadString grows its buffer as needed, so there is no fixed line-length
// ceiling. See gardenio/controller/internal/subscriber/sse.go for the
// matching fix on the other SSE consumer.
func (s *SSEStream) readEvents(ctx context.Context, body io.Reader) error {
	reader := bufio.NewReaderSize(body, 64*1024)

	var currentID, currentEvent string
	var currentData strings.Builder

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("SSE stream ended (EOF)")
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("SSE read: %w", err)
		}

		// Strip trailing \r\n or \n (SSE allows either CRLF or LF).
		line = strings.TrimRight(line, "\r\n")

		// Empty line = end of event.
		if line == "" {
			// Advance Last-Event-ID at the dispatch boundary *before* running
			// handlers, per the WHATWG SSE algorithm ("set the last event ID
			// string ... then dispatch the event"). dispatch blocks on its
			// handlers (wg.Wait), so setting lastID afterwards left it stale
			// for the entire handler window — observable as a race when a
			// handler reads LastID() (e.g. the oversized-data regression test).
			if currentID != "" {
				s.setLastID(currentID)
			}
			if currentData.Len() > 0 && currentEvent != "" {
				s.dispatch(ctx, currentID, currentEvent, currentData.String())
			}
			currentID = ""
			currentEvent = ""
			currentData.Reset()
			continue
		}

		// Comment lines (keepalive).
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Parse SSE field:value.
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")

		switch field {
		case "id":
			currentID = value
		case "event":
			currentEvent = value
		case "data":
			if currentData.Len() > 0 {
				currentData.WriteByte('\n')
			}
			currentData.WriteString(value)
		}
	}
}

// dispatch calls all registered handlers for the given topic.
// If dedup is configured, events are deduplicated by bead ID + topic.
func (s *SSEStream) dispatch(ctx context.Context, id, topic, data string) {
	s.mu.RLock()
	handlers := s.handlers[topic]
	s.mu.RUnlock()
	if len(handlers) == 0 {
		return
	}

	// Deduplicate created/closed events by bead ID. Updated events are
	// exempt — the same bead can be updated many times with different state
	// (e.g., agent_state transitions from spawning→working→done) and each
	// change must be processed.
	if s.dedup != nil && topic != "beads.bead.updated" {
		bead := ParseBeadEvent([]byte(data))
		if bead != nil {
			key := topic + ":" + bead.ID
			if s.dedup.Seen(key) {
				s.logger.Debug("dedup: skipping duplicate event",
					"topic", topic, "bead", bead.ID, "sse_id", id)
				return
			}
		}
	}

	// Dispatch handlers concurrently so a slow handler (e.g. a GitLab API
	// call timing out) cannot block the SSE read loop and stall all event
	// processing. Each handler gets a 60s timeout — long enough for normal
	// operations but prevents indefinite stalls.
	var wg sync.WaitGroup
	for _, h := range handlers {
		wg.Add(1)
		go func(fn SSEHandler) {
			defer wg.Done()
			// Recover boundary: Go does not confine a goroutine panic, so a
			// panic in any handler's call tree would otherwise terminate the
			// whole single-replica bridge (and, because Last-Event-ID is
			// replayed on restart, re-crash it on every reconnect). Mirrors the
			// Socket-Mode dispatchHandler boundary (ka-0000007939).
			defer recoverHandlerGoroutine(s.logger, topic)
			hctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			fn(hctx, []byte(data))
		}(h)
	}
	wg.Wait()

	// Persist last event ID after successful dispatch.
	if s.state != nil && id != "" {
		if err := s.state.SetLastEventID(id); err != nil {
			// Rate-limit this warning — fires on every SSE event when state dir is unwritable.
			s.persistWarnOnce.Do(func() {
				s.logger.Warn("failed to persist last event ID (further failures suppressed for 5m)",
					"id", id, "error", err)
				s.persistWarnLast = time.Now()
			})
			if time.Since(s.persistWarnLast) > 5*time.Minute {
				s.persistWarnLast = time.Now()
				s.logger.Warn("still failing to persist last event ID", "id", id, "error", err)
			}
		}
	}
}

// sseBeadWrapper is the kbeads SSE payload for bead lifecycle events.
// BeadCreated: {"bead": {...}}
// BeadUpdated: {"bead": {...}, "changes": {"field": newValue, ...}, "actor": "..."?}
// BeadClosed:  {"bead": {...}, "closed_by": "...", "decision_answer": {...}?}
type sseBeadWrapper struct {
	Bead           json.RawMessage `json:"bead"`
	ClosedBy       string          `json:"closed_by,omitempty"`
	Changes        map[string]any  `json:"changes,omitempty"`
	Actor          string          `json:"actor,omitempty"`
	DecisionAnswer *DecisionAnswer `json:"decision_answer,omitempty"`
}

// sseBeadData mirrors the kbeads model.Bead fields used by the bridge.
type sseBeadData struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Status    string          `json:"status"`
	Assignee  string          `json:"assignee"`
	CreatedBy string          `json:"created_by"`
	Labels    []string        `json:"labels"`
	Priority  int             `json:"priority"`
	Fields    json.RawMessage `json:"fields"`
	Heraldry  string          `json:"heraldry,omitempty"`
	// OperatedBy is the bead's top-level operated_by (the human the daemon
	// attributes it to), distinct from any operated_by in Fields.
	OperatedBy string `json:"operated_by,omitempty"`
}

// ParseBeadEvent extracts a bridge BeadEvent from a kbeads SSE event payload.
// Returns nil if the payload is malformed or missing a bead.
func ParseBeadEvent(data []byte) *BeadEvent {
	var wrapper sseBeadWrapper
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil
	}
	if len(wrapper.Bead) == 0 {
		return nil
	}

	var bead sseBeadData
	if err := json.Unmarshal(wrapper.Bead, &bead); err != nil {
		return nil
	}

	// Parse fields from json.RawMessage to map[string]string.
	fields := beadsapi.ParseFieldsJSON(bead.Fields)

	// Mirror DecisionAnswer entries into Fields so the generic trigger:bead
	// condition DSL (which keys off Fields[field]) matches without callers
	// having to special-case decision_answer. The wrapper carries the
	// authoritative payload; Fields just gets convenient string mirrors.
	// See ka-0000005470 (GAP-4).
	if wrapper.DecisionAnswer != nil {
		if fields == nil {
			fields = make(map[string]string)
		}
		if wrapper.DecisionAnswer.Chosen != "" {
			fields["chosen"] = wrapper.DecisionAnswer.Chosen
		}
		if wrapper.DecisionAnswer.ResponseText != "" {
			fields["response_text"] = wrapper.DecisionAnswer.ResponseText
		}
		if wrapper.DecisionAnswer.RespondedBy != "" {
			fields["responded_by"] = wrapper.DecisionAnswer.RespondedBy
		}
		if wrapper.DecisionAnswer.RespondedAt != "" {
			fields["responded_at"] = wrapper.DecisionAnswer.RespondedAt
		}
		if wrapper.DecisionAnswer.Cancelled {
			fields["canceled"] = "true"
		}
	}

	return &BeadEvent{
		ID:             bead.ID,
		Type:           bead.Type,
		Title:          bead.Title,
		Status:         bead.Status,
		Assignee:       bead.Assignee,
		CreatedBy:      bead.CreatedBy,
		Labels:         bead.Labels,
		Fields:         fields,
		Priority:       bead.Priority,
		ClosedBy:       wrapper.ClosedBy,
		Changes:        wrapper.Changes,
		Actor:          wrapper.Actor,
		DecisionAnswer: wrapper.DecisionAnswer,
		Heraldry:       bead.Heraldry,
		OperatedBy:     bead.OperatedBy,
	}
}
