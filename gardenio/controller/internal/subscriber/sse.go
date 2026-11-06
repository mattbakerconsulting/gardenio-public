// Package subscriber watches for agent lifecycle events and emits them on a
// channel. The SSEWatcher connects to the kbeads SSE endpoint instead of NATS
// JetStream.
package subscriber

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
	"gardenio/controller/internal/podmanager"
)

// SSEConfig holds configuration for the SSE watcher.
type SSEConfig struct {
	// BeadsHTTPAddr is the kbeads HTTP base URL (e.g., "http://localhost:8080").
	BeadsHTTPAddr string

	// Token is an optional Bearer token for authenticating with the daemon.
	Token string

	// TokenSource, when non-nil, supplies the bearer for each (re)connect and
	// wins over Token. It is the controller's machine principal
	// (beadsapi.MachineTokenSourceFromEnv): the stream then never needs the
	// shared static token, and a fetch failure fails the connect (retried with
	// backoff) instead of falling back to it.
	TokenSource beadsapi.TokenSource

	// Topics is the optional comma-separated topic filter for the SSE endpoint.
	// Empty means all events.
	Topics string

	// Namespace is the default K8s namespace for pod metadata.
	Namespace string

	// CoopImage is the default container image for agent pods.
	CoopImage string

	// BeadsGRPCAddr is the beads daemon gRPC address (host:port) for agent pod env vars.
	BeadsGRPCAddr string
}

// SSEWatcher subscribes to the kbeads SSE event stream and translates bead
// events into lifecycle Events. It tracks Last-Event-ID for reconnection and
// auto-reconnects with exponential backoff.
type SSEWatcher struct {
	cfg        SSEConfig
	events     chan Event
	logger     *slog.Logger
	httpClient *http.Client // reused across reconnections (long-lived, no timeout)

	mu          sync.Mutex
	lastEventID string // tracks the most recent SSE event ID for reconnection
	// stoppedEmitted dedups AgentStop per agent bead so a duplicate Freeze can
	// never fire (bug ka-0000000194). agent_state lives INSIDE the fields JSON
	// blob, not as a top-level bead column, so the daemon's `changes` map only
	// ever carries a "fields" key on an agent_state change — never a literal
	// "agent_state" key (kbeads beads.go patch-merges fields wholesale). That is
	// why the obvious `Changes["agent_state"]` gate (symmetric with the
	// in_progress→Changes["status"] gate) cannot work and would suppress ALL
	// freezes. Instead we emit AgentStop only on the TRANSITION into "stopping":
	// the first `updated` with agent_state=="stopping" for a bead emits; every
	// subsequent `updated` while still stopping is a no-op. The marker is cleared
	// when the bead spawns/re-spawns or closes so a later genuine stop re-arms.
	stoppedEmitted map[string]bool
}

// NewSSEWatcher creates a watcher backed by the kbeads SSE event stream.
func NewSSEWatcher(cfg SSEConfig, logger *slog.Logger) *SSEWatcher {
	return &SSEWatcher{
		cfg:            cfg,
		events:         make(chan Event, 64),
		logger:         logger,
		httpClient:     &http.Client{Timeout: 0}, // no timeout for long-lived SSE
		stoppedEmitted: map[string]bool{},
	}
}

// Start begins watching the SSE stream. Blocks until ctx is canceled.
// Reconnects with exponential backoff on errors.
func (w *SSEWatcher) Start(ctx context.Context) error {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			close(w.events)
			return fmt.Errorf("watcher stopped: %w", ctx.Err())
		default:
		}

		err := w.stream(ctx)
		if err != nil {
			if ctx.Err() != nil {
				close(w.events)
				return fmt.Errorf("watcher stopped: %w", ctx.Err())
			}
			w.logger.Warn("SSE stream error, reconnecting",
				"error", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				close(w.events)
				return fmt.Errorf("watcher stopped: %w", ctx.Err())
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		} else {
			backoff = time.Second
		}
	}
}

// Events returns a read-only channel of lifecycle events.
func (w *SSEWatcher) Events() <-chan Event {
	return w.events
}

// stream connects to the SSE endpoint and reads events until disconnection.
func (w *SSEWatcher) stream(ctx context.Context) error {
	url := strings.TrimRight(w.cfg.BeadsHTTPAddr, "/") + "/v1/events/stream"
	if w.cfg.Topics != "" {
		url += "?topics=" + w.cfg.Topics
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	switch {
	case w.cfg.TokenSource != nil:
		tok, err := w.cfg.TokenSource.Token(ctx)
		if err != nil {
			return fmt.Errorf("SSE bearer: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	case w.cfg.Token != "":
		req.Header.Set("Authorization", "Bearer "+w.cfg.Token)
	}

	// Send Last-Event-ID for reconnection replay.
	w.mu.Lock()
	lastID := w.lastEventID
	w.mu.Unlock()
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}

	w.logger.Info("connecting to SSE event stream",
		"url", url, "last_event_id", lastID)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("SSE connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SSE endpoint returned status %d", resp.StatusCode)
	}

	w.logger.Info("SSE stream connected")

	// Use bufio.Reader.ReadString rather than bufio.Scanner: bead events
	// can serialize past any fixed token cap (long gardener prompts,
	// audit dumps, big wrap-ups), and Scanner returns "token too long"
	// without advancing past the offending event. ReadString grows its
	// buffer as needed, so there is no fixed line-length ceiling.
	reader := bufio.NewReaderSize(resp.Body, 64*1024)

	var (
		eventID   string
		eventType string
		eventData string
	)

	for {
		// Check for context cancellation between lines.
		if ctx.Err() != nil {
			return nil
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			// Process any final partial line (last bytes before close
			// without a trailing newline) only if it's a complete event,
			// which by SSE framing requires the trailing blank line —
			// so a partial line at EOF is discarded.
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("SSE stream closed by server")
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("SSE stream read: %w", err)
		}

		// Strip trailing \r\n or \n (SSE allows either CRLF or LF).
		line = strings.TrimRight(line, "\r\n")

		// Empty line = end of event.
		if line == "" {
			// Advance Last-Event-ID at the dispatch boundary *before*
			// emitting, per the WHATWG SSE algorithm ("set the last event ID
			// string ... then dispatch the event"). The channel send inside
			// processSSEEvent then publishes the updated ID with a
			// happens-before edge to any receiver, so a consumer that just
			// took an event can never observe the previous LastEventID().
			// Mirrors the bridge SSEStream ordering (internal/bridge/sse.go).
			if eventID != "" {
				w.mu.Lock()
				w.lastEventID = eventID
				w.mu.Unlock()
			}
			if eventData != "" && eventType != "" {
				w.processSSEEvent(eventID, eventType, eventData)
			}
			eventID = ""
			eventType = ""
			eventData = ""
			continue
		}

		// Comment lines (keepalive).
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Parse SSE fields.
		if strings.HasPrefix(line, "id:") {
			eventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		} else if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			eventData = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

// sseBeadPayload matches the kbeads event JSON for bead lifecycle events.
// BeadCreated: {"bead": {...}}
// BeadUpdated: {"bead": {...}, "changes": {...}}
// BeadClosed:  {"bead": {...}, "closed_by": "..."}
// BeadDeleted: {"bead_id": "..."}
type sseBeadPayload struct {
	Bead     *sseBead       `json:"bead,omitempty"`
	Changes  map[string]any `json:"changes,omitempty"`
	ClosedBy string         `json:"closed_by,omitempty"`
	BeadID   string         `json:"bead_id,omitempty"` // for delete events
}

// sseBead mirrors the kbeads model.Bead fields we need for event mapping.
type sseBead struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Type       string          `json:"type"`
	Status     string          `json:"status"`
	Labels     []string        `json:"labels"`
	Fields     json.RawMessage `json:"fields"` // raw JSON object
	AgentState string          `json:"agent_state"`
	Assignee   string          `json:"assignee"`
	CreatedBy  string          `json:"created_by"`
}

// processSSEEvent parses an SSE event and emits a lifecycle Event if relevant.
func (w *SSEWatcher) processSSEEvent(id, topic, data string) {
	// Only care about bead lifecycle events.
	if !strings.HasPrefix(topic, "beads.bead.") {
		return
	}

	var payload sseBeadPayload
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		w.logger.Debug("skipping malformed SSE event",
			"id", id, "topic", topic, "error", err)
		return
	}

	// Delete events have no bead — just bead_id.
	action := subjectAction(topic)
	if action == "deleted" {
		w.logger.Debug("SSE delete event", "id", id, "bead_id", payload.BeadID)
		// We cannot map delete events without full bead info (no type/fields).
		// The old NATS watcher had the same issue — it relied on the full bead payload.
		// For deletes we still need to check if it was an agent bead.
		// Since we only get bead_id, we emit a kill event and let the handler
		// deal with it by bead ID alone.
		return
	}

	if payload.Bead == nil {
		w.logger.Debug("skipping SSE event with no bead payload",
			"id", id, "topic", topic)
		return
	}

	// Config bead updates trigger rolling restarts for affected projects.
	if payload.Bead.Type == "config" && action == "updated" {
		project := projectFromLabels(payload.Bead.Labels)
		if project == "" {
			w.logger.Debug("skipping config update with no project label",
				"id", id, "bead_id", payload.Bead.ID, "title", payload.Bead.Title)
			return
		}
		event := Event{
			Type:    ConfigUpdated,
			Project: project,
			// The config bead's role: label scopes which agent role the config
			// governs. When set, the handler restarts only pods of that role;
			// when empty the config governs every role in the project (ka-0000002960).
			Role:   roleFromLabels(payload.Bead.Labels),
			BeadID: payload.Bead.ID,
			Metadata: map[string]string{
				"title": payload.Bead.Title,
			},
		}
		select {
		case w.events <- event:
		default:
			w.logger.Warn("event channel full, dropping config event",
				"bead", payload.Bead.ID, "project", project)
		}
		return
	}

	// Account bead updates trigger credential push to coopmux and auth failover
	// when throttle_status changes.
	if payload.Bead.Type == "account" && action == "updated" {
		fields := beadsapi.ParseFieldsJSON(payload.Bead.Fields)
		event := Event{
			Type:   AccountUpdated,
			BeadID: payload.Bead.ID,
			Metadata: map[string]string{
				"title":           payload.Bead.Title,
				"credential":      fields["credential"],
				"provider":        fields["provider"],
				"model":           fields["model"],
				"throttle_status": fields["throttle_status"],
				"base_url":        fields["base_url"],
				"proxy_url":       fields["proxy_url"],
			},
		}
		w.logger.Info("emitting account_updated event from SSE",
			"bead", payload.Bead.ID, "account", payload.Bead.Title, "sse_id", id)
		select {
		case w.events <- event:
		default:
			w.logger.Warn("event channel full, dropping account event",
				"bead", payload.Bead.ID, "account", payload.Bead.Title)
		}
		return
	}

	// Session bead close — metrics-only (gd-5MTDWjwNjm). Session beads close
	// far more often than agent beads (once per coop restart-loop iteration),
	// so this is intentionally NOT folded into the generic agent-bead path
	// below: it carries only exit_code/project/role, no pod operation.
	if payload.Bead.Type == "session" && action == "closed" {
		fields := beadsapi.ParseFieldsJSON(payload.Bead.Fields)
		event := Event{
			Type:    SessionDone,
			Project: fields["project"],
			Role:    fields["role"],
			BeadID:  payload.Bead.ID,
			Metadata: map[string]string{
				"exit_code": fields["exit_code"],
			},
		}
		select {
		case w.events <- event:
		default:
			w.logger.Warn("event channel full, dropping session_done event",
				"bead", payload.Bead.ID)
		}
		return
	}

	if payload.Bead.Type != "agent" {
		return
	}

	// Convert to the internal beadData format the mapBeadEvent expects.
	fields := beadsapi.ParseFieldsJSON(payload.Bead.Fields)
	bd := beadData{
		ID:         payload.Bead.ID,
		Title:      payload.Bead.Title,
		Type:       payload.Bead.Type,
		Status:     payload.Bead.Status,
		Labels:     payload.Bead.Labels,
		Fields:     fields,
		AgentState: fields["agent_state"],
		Assignee:   payload.Bead.Assignee,
		CreatedBy:  payload.Bead.CreatedBy,
	}

	// If agent_state comes from the bead status rather than fields, use it directly.
	if payload.Bead.AgentState != "" {
		bd.AgentState = payload.Bead.AgentState
	}

	ep := beadEventPayload{
		Bead:    bd,
		Changes: payload.Changes,
	}

	event, ok := w.mapBeadEvent(action, ep)
	if !ok {
		return
	}

	w.logger.Info("emitting lifecycle event from SSE",
		"type", event.Type, "project", event.Project,
		"role", event.Role, "agent", event.AgentName,
		"bead", event.BeadID, "sse_id", id)

	select {
	case w.events <- event:
	default:
		w.logger.Warn("event channel full, dropping event",
			"type", event.Type, "bead", event.BeadID)
	}
}

// mapBeadEvent maps a daemon bead event to a subscriber Event.
// Same logic as NATSWatcher.mapBeadEvent.
func (w *SSEWatcher) mapBeadEvent(action string, payload beadEventPayload) (Event, bool) {
	// A local agent (execution_target=laptop) runs on its owner's laptop and
	// gb owns its lifecycle: the controller spawns, stops, tears down and
	// fails over nothing for it.
	if podmanager.IsLaptopTarget(payload.Bead.Fields["execution_target"]) {
		return Event{}, false
	}
	switch action {
	case "created":
		// A fresh agent bead re-arms any prior stop marker for this id (re-used
		// nanoid is impossible, but keep the map from leaking stale ids).
		w.armStop(payload.Bead.ID)
		return w.buildEvent(AgentSpawn, payload.Bead)
	case "updated":
		// Emit AgentStop only on the TRANSITION into "stopping". agent_state is
		// field-blob-resident, so we cannot gate on payload.Changes (it carries
		// only a "fields" key); instead we dedup per-bead in stoppedEmitted. The
		// freeze/teardown path itself writes bead fields while agent_state is
		// still "stopping", which re-emits `updated` events — without this guard
		// each one re-fired AgentStop → a second Freeze that overwrote the good
		// snapshot with a degenerate (empty agent overlay-opts) one, making the
		// bundle un-thawable (bug ka-0000000194).
		if payload.Bead.AgentState == "stopping" {
			if w.markStop(payload.Bead.ID) {
				return w.buildEvent(AgentStop, payload.Bead)
			}
			// Duplicate stopping update — already emitted AgentStop for this bead.
			w.logger.Debug("suppressing duplicate AgentStop for already-stopping agent",
				"bead", payload.Bead.ID, "agent", payload.Bead.Fields["agent"])
			return w.buildEvent(AgentUpdate, payload.Bead)
		}
		// Check if agent_state changed to budget_exceeded or rate_limited.
		// Both indicate the current account's credentials are exhausted and
		// should trigger auth failover to the fallback account (if configured).
		if payload.Bead.AgentState == "budget_exceeded" || payload.Bead.AgentState == "rate_limited" {
			return w.buildEvent(BudgetExceeded, payload.Bead)
		}
		// Check if status changed to in_progress (re-spawn). A re-spawn re-arms
		// the stop marker so a later genuine stop of the new session emits again.
		if payload.Bead.Status == "in_progress" {
			if _, ok := payload.Changes["status"]; ok {
				w.armStop(payload.Bead.ID)
				return w.buildEvent(AgentSpawn, payload.Bead)
			}
		}
		return w.buildEvent(AgentUpdate, payload.Bead)
	case "closed":
		w.armStop(payload.Bead.ID)
		return w.buildEvent(AgentDone, payload.Bead)
	case "deleted":
		w.armStop(payload.Bead.ID)
		return w.buildEvent(AgentKill, payload.Bead)
	default:
		return Event{}, false
	}
}

// markStop records that AgentStop has been emitted for beadID and reports
// whether THIS call is the one that should emit (true on the first transition
// into stopping, false for every duplicate while still stopping). Thread-safe.
func (w *SSEWatcher) markStop(beadID string) bool {
	if beadID == "" {
		// No id to dedup on — fall back to old behaviour (always emit). A
		// missing id is not expected for agent beads.
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stoppedEmitted[beadID] {
		return false
	}
	w.stoppedEmitted[beadID] = true
	return true
}

// armStop clears any stop marker for beadID so a subsequent transition into
// stopping re-emits AgentStop (called on spawn / re-spawn / close / delete).
// Thread-safe.
func (w *SSEWatcher) armStop(beadID string) {
	if beadID == "" {
		return
	}
	w.mu.Lock()
	delete(w.stoppedEmitted, beadID)
	w.mu.Unlock()
}

// buildEvent constructs a lifecycle Event from a bead payload.
// Same logic as NATSWatcher.buildEvent.
func (w *SSEWatcher) buildEvent(eventType EventType, bead beadData) (Event, bool) {
	project := bead.Fields["project"]
	mode := bead.Fields["mode"]
	role := bead.Fields["role"]
	name := bead.Fields["agent"]
	if mode == "" {
		mode = "crew"
	}

	if name == "" {
		w.logger.Debug("skipping event with incomplete agent info",
			"action", eventType, "bead_id", bead.ID, "title", bead.Title)
		return Event{}, false
	}
	if role == "" {
		role = "crew"
	}

	// Start with bead fields as metadata base, then overlay controller config.
	// This passes through custom fields like mock_scenario, image overrides, etc.
	meta := make(map[string]string, len(bead.Fields)+3)
	for k, v := range bead.Fields {
		meta[k] = v
	}
	// Do NOT auto-inject meta["namespace"] = w.cfg.Namespace here. Downstream
	// consumers (namespaceFromEvent) already fall back to cfg.Namespace when no
	// explicit pin is present, so the injection was redundant for the default
	// case — but it poisoned the gVisor freeze/teardown path: routeGVisorNamespace
	// (podspec.go) is guarded by explicitNamespace=="" and treats any non-empty
	// namespace as an operator pin, so an auto-injected default silently disabled
	// runsc routing on the events-path AgentStop trigger. Leaving namespace unset
	// (unless the bead itself carries an explicit `namespace` field) lets a runsc
	// agent route to cfg.GVisorNamespace on freeze, matching the reconciler path
	// (BuildSpecFromBeadInfo). Bug ka-0000002448.
	if w.cfg.CoopImage != "" && meta["image"] == "" {
		meta["image"] = w.cfg.CoopImage
	}
	if w.cfg.BeadsGRPCAddr != "" {
		meta["beads_grpc_addr"] = w.cfg.BeadsGRPCAddr
	}

	// Dark discriminator (ka-0000005242): stamp a metadata marker when the agent
	// bead belongs to the monorepo-dark universe, so the pod-spec builder can
	// fence the privileged Jira write token (JIRA_API_TOKEN) out of dark pods —
	// defense-in-depth over the bead-shape convention, which R7 proved is not a
	// structural guarantee. Derived from the bead's labels/project, the same
	// signal the dark-only MR-cap + token-budget controls key on.
	if beadsapi.IsDark(bead.Labels, project) {
		meta[beadsapi.MetaDarkPod] = "true"
	}

	return Event{
		Type:      eventType,
		Project:   project,
		Mode:      mode,
		Role:      role,
		AgentName: name,
		BeadID:    bead.ID,
		Metadata:  meta,
	}, true
}

// LastEventID returns the most recently seen SSE event ID. Thread-safe.
func (w *SSEWatcher) LastEventID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastEventID
}

// SetLastEventID sets the last event ID for reconnection. Thread-safe.
// Useful for restoring state after a process restart.
func (w *SSEWatcher) SetLastEventID(id string) {
	w.mu.Lock()
	w.lastEventID = id
	w.mu.Unlock()
}
