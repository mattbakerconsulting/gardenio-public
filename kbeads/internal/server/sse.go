package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gardenio/kbeads/internal/model"
)

const (
	// sseRingBufferSize is the number of recent events kept in memory for
	// Last-Event-ID reconnection support.
	sseRingBufferSize = 1000

	// sseKeepaliveInterval is how often keepalive comments are sent to
	// prevent connection timeouts.
	sseKeepaliveInterval = 15 * time.Second

	// sseTaintGateRefreshIntervalDefault is how often a connected client's
	// read scope is re-resolved for the lifetime of the SSE connection: its
	// taint gate (ADR docs/adr/taints.md §5.1; grants can be revoked
	// mid-stream, ka-INalwzaYD9) and its allowed projects (a store-backed
	// permission can be revoked too: D.9 gd-Z9VVGjvzBk, scope resolved at
	// delivery, not at subscribe). Only tokenProject, an immutable token
	// claim, is fixed for the connection.
	sseTaintGateRefreshIntervalDefault = 5 * time.Second

	// sseClientBuffer is the per-subscriber cap: how many events may wait for
	// one slow client. A client that falls further behind is cut (D.9
	// gd-Z9VVGjvzBk) rather than silently losing events: it reconnects with
	// Last-Event-ID and the ring replays what it missed.
	sseClientBuffer = 64
)

// sseTaintGateRefreshInterval is a var (not used as the const above
// directly) so tests can shrink it to exercise the mid-stream refresh tick
// without sleeping for the production interval.
var sseTaintGateRefreshInterval = sseTaintGateRefreshIntervalDefault

// sseTaintFetchFailedSentinel stands in for "GetTaintsForBead failed" when
// resolveSSEEventScope threads its result through sseHub.broadcast. Taint
// ids are always positive (bigserial), so this coerces
// taintIDsSubsetOfGrants to fail for every non-exempt client — i.e. fail
// closed on a lookup error — without adding a separate error/bool channel to
// sseHub.broadcast's signature or to the buffered sseEvent.
var sseTaintFetchFailedSentinel = []int64{-1}

// sseEvent is a single event stored in the ring buffer and sent to SSE clients.
type sseEvent struct {
	ID     uint64 // monotonically increasing sequence number
	Topic  string
	Data   []byte // JSON-encoded payload
	Fleet  string // fleet extracted from payload (for filtering)
	BeadID string // owning bead, "" for the handful of bead-less topics (admin refresh, taint grant/revoke)

	// Labels and TaintIDs are the owning bead's CURRENT project labels and
	// taint ids, resolved ONCE at broadcast time (resolveSSEEventScope) and
	// reused for both live fan-out and Last-Event-ID replay — the same
	// "resolve once, check many" shape beadTaintGateState uses one layer up.
	// Both are the zero value (unrestricted / no taints) when BeadID=="".
	Labels   []string
	TaintIDs []int64

	// BeadType and Assignee are the owning bead's type and assignee, resolved
	// at broadcast time alongside Labels so the agent scope (spec §3.3,
	// matchesAgentScope) applies identically on live fan-out and replay.
	// Both are "" when BeadID=="" or the bead could not be resolved.
	BeadType string
	Assignee string
}

// sseHub fans out events from recordAndPublish to connected SSE clients.
// It maintains an in-memory ring buffer for Last-Event-ID reconnection.
type sseHub struct {
	mu      sync.RWMutex
	clients map[*sseClient]struct{}
	taps    []func(*sseEvent)
	nextID  atomic.Uint64

	// Ring buffer for replay on reconnection.
	ringMu  sync.RWMutex
	ring    [sseRingBufferSize]sseEvent
	ringPos int // next write position (wraps around)
	ringLen int // number of valid entries (up to sseRingBufferSize)
}

// sseClient represents a single connected SSE consumer.
type sseClient struct {
	topics []string       // topic glob patterns to match (empty = all)
	fleet  string         // fleet to filter by (empty = all)
	ch     chan *sseEvent // buffered channel for event delivery

	// Read-scope state (ADR docs/adr/taints.md §4.3/§5.1; ka-INalwzaYD9).
	// tokenProject is an immutable token claim, fixed at subscribe time.
	// allowedProjects and taintGate are refreshed on a ticker for the life of
	// the connection (sseTaintGateRefreshInterval), since a permission or a
	// taint grant can be revoked mid-stream, and are checked again at
	// delivery (D.9 gd-Z9VVGjvzBk), so an event queued before a revocation
	// is not written after it. They are atomic.Pointers so the refresh (the
	// connection's own handler goroutine, see handleEventStream) and
	// hub.broadcast (whichever goroutine calls recordAndPublish) can touch
	// them without a data race.
	tokenProject    string
	allowedProjects atomic.Pointer[[]string]
	taintGate       atomic.Pointer[beadTaintGateState]

	// hiddenTypes are the bead types a tenant client may not read
	// (read_authz_tenant.go), fixed at subscribe like tokenProject: whether
	// the caller is a tenant follows from its credential.
	hiddenTypes []model.BeadType

	// Agent scope (spec §3.3, local-agents plan Task 1). When agentBeadID is
	// set the client sees only events on that bead and type=mail beads
	// assigned to agentName. Snapshotted at subscribe like tokenProject.
	agentBeadID string
	agentName   string

	// cut is closed when the client overflows its buffer (sseClientBuffer);
	// handleEventStream then ends the stream so the client resumes from its
	// Last-Event-ID instead of carrying a silent gap.
	cut     chan struct{}
	cutOnce sync.Once
}

// sseClientScope is the subscriber-side read-scope state sseHub.subscribe
// installs atomically with the client, so no broadcast can observe a
// half-initialized client and wrongly allow (or transiently drop) an event.
type sseClientScope struct {
	tokenProject        string
	oidcAllowedProjects []string
	taintGate           beadTaintGateState
	hiddenTypes         []model.BeadType
	agentBeadID         string
	agentName           string
}

func newSSEHub() *sseHub {
	return &sseHub{
		clients: make(map[*sseClient]struct{}),
	}
}

// broadcast sends an event to all connected clients whose topic, fleet, and
// read-scope (agent + project + taint gate) all match.
// beadID/beadType/assignee/labels/taintIDs are the owning bead's identity
// and CURRENT scope, resolved once by the caller
// (BeadsServer.broadcastEvent, via resolveSSEEventScope) — never once per
// client — and are also stashed on the ring-buffer entry so a later
// Last-Event-ID replay enforces the identical gate (ka-INalwzaYD9).
func (h *sseHub) broadcast(topic string, payload []byte, beadID, beadType, assignee string, labels []string, taintIDs []int64) {
	id := h.nextID.Add(1)
	evt := &sseEvent{
		ID:       id,
		Topic:    topic,
		Data:     payload,
		Fleet:    extractFleetFromPayload(payload),
		BeadID:   beadID,
		Labels:   labels,
		TaintIDs: taintIDs,
		BeadType: beadType,
		Assignee: assignee,
	}

	// Store in ring buffer.
	h.ringMu.Lock()
	h.ring[h.ringPos] = *evt
	h.ringPos = (h.ringPos + 1) % sseRingBufferSize
	if h.ringLen < sseRingBufferSize {
		h.ringLen++
	}
	h.ringMu.Unlock()

	// Fan out to connected clients.
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if c.matchesTopic(topic) && c.matchesFleet(evt.Fleet) && c.matchesScope(beadID, beadType, assignee, labels, taintIDs) {
			select {
			case c.ch <- evt:
			default:
				// The client is slow: never block the publisher. Cut it so it
				// reconnects and replays from the ring, rather than carrying
				// on past a gap it cannot see.
				c.cutOnce.Do(func() { close(c.cut) })
			}
		}
	}
	for _, tap := range h.taps {
		tap(evt)
	}
}

// addTap registers fn to see every event the hub broadcasts, unfiltered, on
// the publisher's goroutine: fn must not block. The land feed (landmap, D.9)
// is the one tap; it carries no payload onward and resolves each reader's
// scope itself, at delivery. Taps are added at startup and never removed.
func (h *sseHub) addTap(fn func(*sseEvent)) {
	h.mu.Lock()
	h.taps = append(h.taps, fn)
	h.mu.Unlock()
}

// subscribe registers a new SSE client and returns it. Call unsubscribe when done.
func (h *sseHub) subscribe(topics []string, fleet string, scope sseClientScope) *sseClient {
	c := &sseClient{
		topics:       topics,
		fleet:        fleet,
		ch:           make(chan *sseEvent, sseClientBuffer),
		tokenProject: scope.tokenProject,
		hiddenTypes:  scope.hiddenTypes,
		agentBeadID:  scope.agentBeadID,
		agentName:    scope.agentName,
		cut:          make(chan struct{}),
	}
	c.setScope(scope.oidcAllowedProjects, scope.taintGate)
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

// unsubscribe removes a client from the hub.
func (h *sseHub) unsubscribe(c *sseClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// eventsSince returns buffered events with ID > lastID, in order.
// Returns nil if lastID is too old (no longer in buffer).
func (h *sseHub) eventsSince(lastID uint64) []*sseEvent {
	h.ringMu.RLock()
	defer h.ringMu.RUnlock()

	if h.ringLen == 0 {
		return nil
	}

	var result []*sseEvent

	// Walk the ring buffer from oldest to newest.
	start := h.ringPos - h.ringLen
	if start < 0 {
		start += sseRingBufferSize
	}
	for i := range h.ringLen {
		idx := (start + i) % sseRingBufferSize
		// Copy the event by value while holding the lock. Taking &h.ring[idx]
		// here would alias the ring backing array, and the pointer escapes the
		// RLock on return — a later broadcast (which holds ringMu.Lock and does
		// h.ring[idx] = *evt) would then overwrite the slot the caller is still
		// reading, a data race on the sseEvent (including its Data slice header).
		evt := h.ring[idx]
		if evt.ID > lastID {
			result = append(result, &evt)
		}
	}

	return result
}

// setScope installs the client's refreshable read scope.
func (c *sseClient) setScope(allowedProjects []string, gate beadTaintGateState) {
	c.allowedProjects.Store(&allowedProjects)
	c.taintGate.Store(&gate)
}

// matchesScope reports whether an event's owning bead — its id, type,
// assignee, CURRENT project labels and taint ids — satisfies c's read scope:
// the agent scope (matchesAgentScope) AND the project-scope gates
// (matchesProjectScope) AND the taint gate (matchesTaintGate), for a bead of a
// type the client may read (hiddenTypes). Live fan-out
// (sseHub.broadcast), Last-Event-ID replay and the delivery-time re-check
// (handleEventStream) all call this so a client can never receive live what
// it would be denied on replay, or vice versa.
func (c *sseClient) matchesScope(beadID, beadType, assignee string, labels []string, taintIDs []int64) bool {
	return c.matchesTypeScope(beadID, beadType) &&
		c.matchesAgentScope(beadID, beadType, assignee) && c.matchesProjectScope(labels) && c.matchesTaintGate(taintIDs)
}

// matchesTypeScope reports whether the event's bead is of a type c may read.
// For a client that hides types, a bead whose type did not resolve is
// refused, so a failed lookup cannot pass an account bead's event.
func (c *sseClient) matchesTypeScope(beadID, beadType string) bool {
	if len(c.hiddenTypes) == 0 {
		return true
	}
	if beadType == "" {
		return beadID == ""
	}
	return !slices.Contains(c.hiddenTypes, model.BeadType(beadType))
}

// matchesAgentScope implements the `agent` SSE scope (spec §3.3): an
// agent-scoped client receives events on its own agent bead and on mail
// addressed to it, nothing else. A client with no agent scope matches.
func (c *sseClient) matchesAgentScope(beadID, beadType, assignee string) bool {
	if c.agentBeadID == "" {
		return true
	}
	if beadID == c.agentBeadID {
		return true
	}
	return beadType == "mail" && assignee != "" && assignee == c.agentName
}

// matchesProjectScope reports whether labels satisfy c's project-scope
// gates, the SSE-connection-lifetime equivalent of beadMatchesTokenProject +
// beadMatchesOIDCProjects: a project-bound fleet token's required
// "project:<name>" label must be present (AND), and an OIDC user's allowed
// projects must include at least one of the bead's project labels (OR). An
// unscoped client (tokenProject=="" and oidcAllowedProjects==nil) always
// matches, preserving the pre-existing unrestricted behavior.
func (c *sseClient) matchesProjectScope(labels []string) bool {
	if c.tokenProject != "" {
		found := false
		target := "project:" + c.tokenProject
		for _, l := range labels {
			if l == target {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	allowed := c.allowedProjects.Load()
	if allowed == nil {
		return false // set before subscribe returns; fail closed on a bug
	}
	return beadMatchesOIDCProjects(labels, *allowed)
}

// matchesTaintGate reports whether taintIDs passes c's taint gate (ADR
// docs/adr/taints.md §5.1: taintsOf(bead) ⊆ grantsOf(caller)). c.taintGate
// is always set before subscribe() returns (see sseClientScope) and
// refreshed on a ticker thereafter (handleEventStream), so a nil Load()
// here would only mean a bug in that ordering — fail closed rather than
// risk leaking a tainted event.
func (c *sseClient) matchesTaintGate(taintIDs []int64) bool {
	gate := c.taintGate.Load()
	if gate == nil {
		return false
	}
	return gate.allows(taintIDs)
}

// matchesFleet checks whether the event's fleet matches the client's fleet filter.
// An empty client fleet matches all events.
// An empty event fleet (legacy events without fleet metadata) matches all filters.
func (c *sseClient) matchesFleet(eventFleet string) bool {
	if c.fleet == "" || eventFleet == "" {
		return true
	}
	return c.fleet == eventFleet
}

// extractFleetFromPayload extracts the "fleet" field from a JSON payload.
// For events with an embedded bead ({"bead":{"fleet":"x"}}), it checks the nested field.
// Returns empty string if no fleet found.
func extractFleetFromPayload(data []byte) string {
	var raw struct {
		Fleet string `json:"fleet"`
		Bead  *struct {
			Fleet string `json:"fleet"`
		} `json:"bead"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}
	if raw.Fleet != "" {
		return raw.Fleet
	}
	if raw.Bead != nil {
		return raw.Bead.Fleet
	}
	return ""
}

// matchesTopic checks whether the client's topic filters match the given topic.
// An empty filter list matches all topics.
// Supports simple glob patterns: "beads.bead.*" matches "beads.bead.created".
func (c *sseClient) matchesTopic(topic string) bool {
	if len(c.topics) == 0 {
		return true
	}
	for _, pattern := range c.topics {
		if matchTopicPattern(pattern, topic) {
			return true
		}
	}
	return false
}

// matchTopicPattern matches a dot-separated topic against a pattern.
// Supports "*" as a single-segment wildcard and ">" as a multi-segment
// suffix wildcard (NATS-style).
func matchTopicPattern(pattern, topic string) bool {
	if pattern == topic {
		return true
	}

	patParts := strings.Split(pattern, ".")
	topParts := strings.Split(topic, ".")

	for i, pp := range patParts {
		if pp == ">" {
			// ">" matches one or more remaining segments.
			return i < len(topParts)
		}
		if i >= len(topParts) {
			return false
		}
		if pp != "*" && pp != topParts[i] {
			return false
		}
	}

	return len(patParts) == len(topParts)
}

// handleEventStream handles GET /v1/events/stream (SSE endpoint).
func (s *BeadsServer) handleEventStream(w http.ResponseWriter, r *http.Request) {
	// Ensure response supports flushing (required for SSE).
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Parse optional topic filters from query params.
	var topics []string
	if q := r.URL.Query().Get("topics"); q != "" {
		for _, t := range strings.Split(q, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				topics = append(topics, t)
			}
		}
	}

	// Parse optional fleet filter. Defaults to no filter (subscribe across all
	// fleets). `bead.fleet` is audit-attribution only — the old per-token
	// read-isolation gate was removed under epic ka-0000000871 / ka-0000003285.
	fleet := r.URL.Query().Get("fleet")

	// A runner streams only for an agent running on it (spec §3.3).
	if code, msg := s.runnerStreamDenial(r); code != 0 {
		writeError(w, code, msg)
		return
	}

	// Agent scope (spec §3.3): `agent=<bead id>` narrows the stream to that
	// agent's own bead and its mail. Resolve the bead once, up front, and
	// refuse anything that is not an agent bead (type agent, or the kd:agent
	// label local agents carry) so a caller cannot use the scope to follow an
	// arbitrary bead's assignee.
	var agentBeadID, agentName string
	if a := strings.TrimSpace(r.URL.Query().Get("agent")); a != "" {
		b, err := s.store.GetBead(r.Context(), a)
		if err != nil || b == nil || !isAgentBead(b) {
			writeError(w, http.StatusNotFound, "agent bead not found")
			return
		}
		agentBeadID = b.ID
		agentName = b.Assignee
		if agentName == "" {
			agentName = b.Title
		}
	}

	// Resolve the subscriber's read scope before the client is registered,
	// so no broadcast can ever observe a half-scoped client (ADR
	// docs/adr/taints.md §4.3/§5.1; ka-INalwzaYD9). tokenProject is an
	// immutable token claim, fixed for the life of the connection. The taint
	// gate and the allowed projects are refreshed on a ticker below and
	// checked again as each event is written (D.9 gd-Z9VVGjvzBk), since a
	// grant or a permission can be revoked mid-stream.
	taintGate, err := s.resolveBeadTaintGateState(r.Context())
	if err != nil {
		slog.Warn("sse: failed to resolve taint grants, failing closed", "error", err)
		taintGate = beadTaintGateState{} // zero value: not exempt, no grants -> denies any tainted bead
	}
	client := s.sseHub.subscribe(topics, fleet, sseClientScope{
		tokenProject:        TokenProjectFromContext(r.Context()),
		oidcAllowedProjects: oidcAllowedProjects(r.Context(), s.store),
		taintGate:           taintGate,
		hiddenTypes:         s.tenantHiddenReadTypes(r.Context()),
		agentBeadID:         agentBeadID,
		agentName:           agentName,
	})
	defer s.sseHub.unsubscribe(client)

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// If the client sent Last-Event-ID, replay buffered events.
	if lastIDStr := r.Header.Get("Last-Event-ID"); lastIDStr != "" {
		if lastID, err := strconv.ParseUint(lastIDStr, 10, 64); err == nil {
			replayed := s.sseHub.eventsSince(lastID)
			for _, evt := range replayed {
				// matchesScope applies the same project + taint gate a live
				// delivery would (ka-INalwzaYD9): a reconnecting client
				// cannot use Last-Event-ID replay to see anything it
				// couldn't have seen live.
				if client.matchesTopic(evt.Topic) && client.matchesFleet(evt.Fleet) && client.matchesScope(evt.BeadID, evt.BeadType, evt.Assignee, evt.Labels, evt.TaintIDs) && s.sseReadTaint(r.Context(), evt) {
					writeSSEEvent(w, evt)
				}
			}
			flusher.Flush()
		}
	}

	// Stream events until client disconnects.
	ctx := r.Context()
	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	taintRefresh := time.NewTicker(sseTaintGateRefreshInterval)
	defer taintRefresh.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-client.cut:
			// Overflowed its buffer: end the stream. The client reconnects
			// with Last-Event-ID and replays from the ring.
			return
		case evt := <-client.ch:
			// Scope at delivery: the event passed the gate when it was
			// queued, but a refresh may have narrowed the scope since.
			if !client.matchesScope(evt.BeadID, evt.BeadType, evt.Assignee, evt.Labels, evt.TaintIDs) {
				continue
			}
			if !s.sseReadTaint(ctx, evt) {
				continue
			}
			writeSSEEvent(w, evt)
			flusher.Flush()
		case <-keepalive.C:
			// Send a comment line as keepalive.
			fmt.Fprintf(w, ":keepalive\n\n")
			flusher.Flush()
		case <-taintRefresh.C:
			// Re-resolve the read scope so a grant or permission revoked
			// mid-stream takes effect on this connection without requiring
			// a reconnect (ka-INalwzaYD9's fix-shape; D.9 gd-Z9VVGjvzBk). On
			// a transient taint or permission lookup error, keep the previous
			// (already-enforced) scope rather than reset it — the next tick
			// retries. The permission lookup must not fail open here as
			// oidcAllowedProjects does: nil is unrestricted, so one failed
			// lookup would widen a scoped stream to every project.
			gate, err := s.resolveBeadTaintGateState(ctx)
			if err != nil {
				slog.Warn("sse: failed to refresh taint grants, keeping previous state", "error", err)
				continue
			}
			allowed, err := resolveOIDCAllowedProjects(ctx, s.store)
			if err != nil {
				slog.Warn("sse: failed to refresh allowed projects, keeping previous state", "error", err)
				continue
			}
			client.setScope(allowed, gate)
		}
	}
}

// sseReadTaint applies inherit:read for an event about to be written to the
// stream: its payload is the owning bead's text, so an agent subscriber
// acquires that bead's taints (gd-WdnPW8TLPq). It reports whether the event
// may be written; on a store error it is dropped, since writing it would hand
// the agent the text without the mark.
func (s *BeadsServer) sseReadTaint(ctx context.Context, evt *sseEvent) bool {
	if evt.BeadID == "" {
		return true
	}
	if err := s.propagateReadTaint(ctx, evt.BeadID); err != nil {
		slog.Warn("sse: failed to propagate read taint, dropping event", "bead_id", evt.BeadID, "error", err)
		return false
	}
	return true
}

// isAgentBead reports whether b is an agent bead: type agent (pod and
// thread-spawned agents) or the kd:agent label (gb agent start --local
// registers a type=task bead with that label).
func isAgentBead(b *model.Bead) bool {
	return string(b.Type) == "agent" || slices.Contains(b.Labels, "kd:agent")
}

// writeSSEEvent writes a single SSE event to the writer.
func writeSSEEvent(w http.ResponseWriter, evt *sseEvent) {
	fmt.Fprintf(w, "id:%d\n", evt.ID)
	fmt.Fprintf(w, "event:%s\n", evt.Topic)
	fmt.Fprintf(w, "data:%s\n\n", evt.Data)
}

// broadcastEvent is called by recordAndPublish to fan out events to SSE
// clients. beadID is the event's owning bead (may be "" for a handful of
// bead-less topics — admin refresh, taint grant/revoke), used to resolve the
// per-event read scope (ADR docs/adr/taints.md §4.3/§5.1; ka-INalwzaYD9)
// before fanning out.
func (s *BeadsServer) broadcastEvent(ctx context.Context, topic, beadID string, event any) {
	if s.sseHub == nil {
		return
	}
	payload, err := json.Marshal(event)
	if err != nil {
		slog.Warn("failed to marshal event for SSE broadcast", "topic", topic, "error", err)
		return
	}
	beadType, assignee, labels, taintIDs := s.resolveSSEEventScope(ctx, beadID)
	s.sseHub.broadcast(topic, payload, beadID, beadType, assignee, labels, taintIDs)
}

// resolveSSEEventScope resolves beadID's type, assignee, CURRENT project
// labels and taint ids for the SSE hub's per-event read gate. It runs once per broadcast
// event — never once per connected client, mirroring
// resolveBeadTaintGateState's "resolve once, check many" shape one level up
// — and its result is reused for both live fan-out and later Last-Event-ID
// replay, so a reconnecting client sees the same gate a live delivery would
// have applied at broadcast time.
//
// beadID=="" covers the handful of topics with no single owning bead (an
// admin-wide refresh event, taint grant/revoke) — this fix's scope is
// per-bead events leaking across project/taint boundaries, not those admin
// topics, so they stay unrestricted (the zero value), matching their
// pre-existing behavior.
//
// The type and assignee feed only the agent scope (matchesAgentScope). They
// come from GetBeadsByIDs, a single row read (GetBead would also load the
// bead's dependencies and comments on every broadcast). A store error leaves
// them "", which fails CLOSED for agent-scoped clients: a mail whose
// assignee cannot be resolved is not streamed to anyone agent-scoped, and
// the agent's drain-on-reconnect (`gb mail inbox`, spec §3.2) picks it up.
//
// A store error resolving labels fails OPEN (nil, matching the pre-existing
// project-scope gates' backward-compatible fail-open behavior — ADR §11
// invariant #9). A store error resolving taints fails CLOSED
// (sseTaintFetchFailedSentinel), since the taint gate is a security boundary
// per the ADR's explicit asymmetry between the two. The taint lookup itself
// is skipped entirely when the taints registry is empty (ADR §10.1's
// zero-configuration short-circuit), so a normal install with no taints
// configured pays no bead_taints query on this hot path.
func (s *BeadsServer) resolveSSEEventScope(ctx context.Context, beadID string) (beadType, assignee string, labels []string, taintIDs []int64) {
	if beadID == "" {
		return "", "", nil, nil
	}

	if beads, err := s.store.GetBeadsByIDs(ctx, []string{beadID}); err != nil {
		slog.Warn("sse: failed to resolve bead for agent-scope filtering", "bead_id", beadID, "error", err)
	} else if len(beads) == 1 && beads[0] != nil {
		beadType = string(beads[0].Type)
		assignee = beads[0].Assignee
	}

	labelsByBead, err := s.store.GetLabelsForBeads(ctx, []string{beadID})
	if err != nil {
		slog.Warn("sse: failed to resolve bead labels for read-scope filtering", "bead_id", beadID, "error", err)
	} else {
		labels = labelsByBead[beadID]
	}

	nonEmpty, err := s.taintRegistryNonEmpty(ctx)
	if err != nil {
		slog.Warn("sse: failed to check taint registry, failing closed", "bead_id", beadID, "error", err)
		return beadType, assignee, labels, sseTaintFetchFailedSentinel
	}
	if !nonEmpty {
		return beadType, assignee, labels, nil
	}

	taintIDs, err = s.store.GetTaintsForBead(ctx, beadID)
	if err != nil {
		slog.Warn("sse: failed to resolve bead taints, failing closed", "bead_id", beadID, "error", err)
		return beadType, assignee, labels, sseTaintFetchFailedSentinel
	}
	return beadType, assignee, labels, taintIDs
}
