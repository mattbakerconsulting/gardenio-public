package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// agentEventsTopics is the NATS topic glob the viewer subscribes to for the
// per-agent activity lane. We watch every bead lifecycle event and filter to
// the agent's own bead, its parent task, and their descendants in-memory.
const agentEventsTopics = "beads.bead.>"

// agentEventSSE is the wrapper shape kbeads publishes on bead lifecycle SSE
// events. Mirrors bridge.sseBeadWrapper but kept local so the viewer doesn't
// import the bridge package.
type agentEventSSE struct {
	Bead     *agentEventBead `json:"bead"`
	ClosedBy string          `json:"closed_by,omitempty"`
	Changes  map[string]any  `json:"changes,omitempty"`
}

// agentEventBead is the subset of bead fields the events lane and the
// fleet-wide /agents + gardener SSE rows render. It must carry EVERY field the
// shared row/card view-model builder (agentRowFromBeadCtx) reads — not just the
// handful the lifecycle filter keys on — because a streamed row swaps the eager
// row in place: any field the daemon publishes but this struct drops renders as
// a blank/zero cell on every live refresh until the next full page load
// reconciles it.
//
// Before ka-0000000482 this was a 9-field subset (id/type/title/status/assignee/
// created_by/labels/priority/fields), so an SSE swap silently blanked uptime
// (created_at), active-ago + staleness (updated_at), the description snippet,
// the owner cell, and the operator chip (operated_by*). The daemon already
// marshals the full model.Bead on every lifecycle event, so widening the struct
// (below) is a pure parse-more change — no daemon or wire-format work needed.
type agentEventBead struct {
	ID                string   `json:"id"`
	Type              string   `json:"type"`
	Title             string   `json:"title"`
	Status            string   `json:"status"`
	Assignee          string   `json:"assignee"`
	Owner             string   `json:"owner"`
	CreatedBy         string   `json:"created_by"`
	ClosedBy          string   `json:"closed_by"`
	Labels            []string `json:"labels"`
	Priority          int      `json:"priority"`
	Description       string   `json:"description"`
	Notes             string   `json:"notes"`
	OperatedBy        string   `json:"operated_by"`
	OperatedByDisplay string   `json:"operated_by_display"`
	OperatedByEmail   string   `json:"operated_by_email"`
	// OperatedByID is the operators(id) FK the daemon marshals on every
	// lifecycle event (model.Bead.OperatedByID). The streamed row needs it so
	// populateOperatedBySlug can resolve the /u/<slug> deep-link on the swapped
	// node — without it the resolver early-returns (nil FK) and the chip streams
	// link-less until the next eager reload (ka-0000007344).
	OperatedByID *int64          `json:"operated_by_id"`
	ClosedAt     string          `json:"closed_at"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	Fields       json.RawMessage `json:"fields"`
}

// beadDetailFromEvent projects a parsed SSE agentEventBead (plus its
// pre-decoded string→string fields map) onto the beadsapi.BeadDetail the shared
// row/card view-model builder consumes. Both the fleet-wide /agents row path
// and the gardener life-thread card path funnel through here so neither can
// drift from the other — and so both carry the full field set (created_at,
// updated_at, closed_at, description, notes, owner, operated_by*) rather than
// the pre-ka-0000000482 9-field subset that blanked those cells on every live
// refresh.
//
// OperatedBySlug remains json:"-" (a viewer-side lookup, not on the wire), but
// it is no longer left blank on the streamed node: OperatedByID rides through
// here so the render path can call populateOperatedBySlug and resolve the
// /u/<slug> deep-link on the swapped row, matching the eager render
// byte-for-byte (ka-0000007344). The daemon marshals operated_by_id on every
// lifecycle event, so carrying it is a pure parse-more change.
func beadDetailFromEvent(eb *agentEventBead, fields map[string]string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:                eb.ID,
		Type:              eb.Type,
		Title:             eb.Title,
		Status:            eb.Status,
		Assignee:          eb.Assignee,
		Owner:             eb.Owner,
		CreatedBy:         eb.CreatedBy,
		ClosedBy:          eb.ClosedBy,
		Labels:            eb.Labels,
		Priority:          eb.Priority,
		Description:       eb.Description,
		Notes:             eb.Notes,
		OperatedBy:        eb.OperatedBy,
		OperatedByDisplay: eb.OperatedByDisplay,
		OperatedByEmail:   eb.OperatedByEmail,
		OperatedByID:      eb.OperatedByID,
		ClosedAt:          eb.ClosedAt,
		CreatedAt:         eb.CreatedAt,
		UpdatedAt:         eb.UpdatedAt,
		Fields:            fields,
	}
}

// handleAgentEvents serves a Server-Sent Events stream of bead lifecycle
// events filtered to the agent's interest set: the agent bead itself, its
// parent task (Fields["task_id"]), and the reverse dependencies of both
// (children/descendants).
//
// One filter set is computed at connect time and held for the life of the
// stream. New descendants created after connect won't pass the filter — the
// client EventSource is expected to reconnect (htmx default) and the filter
// is recomputed on reconnect.
func (s *Server) handleAgentEvents(w http.ResponseWriter, r *http.Request) {
	r, cancelStream := sseRequestUntilTokenExpiry(r)
	defer cancelStream()
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Agent not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting agent bead for events stream", "id", id, "error", err)
		http.Error(w, "Failed to load agent", http.StatusInternalServerError)
		return
	}
	if bead.Type != "agent" {
		http.Error(w, "Not an agent bead", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	filter := s.buildAgentEventFilter(r, bead)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	events, err := s.daemon.EventStream(r.Context(), agentEventsTopics)
	if err != nil {
		s.logger.Error("opening event stream for agent", "id", id, "error", err)
		// SSE headers are already written — emit a comment line so the client
		// reconnect logic kicks in instead of treating an empty body as success.
		fmt.Fprintf(w, ": stream open failed\n\n")
		flusher.Flush()
		return
	}

	// Push a comment line so intermediate proxies (Traefik) flush the response
	// header chunk and the browser's EventSource fires onopen.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			s.writeAgentEvent(w, flusher, evt, filter)
		}
	}
}

// buildAgentEventFilter resolves the set of bead IDs whose lifecycle events
// should appear in the agent's activity lane: the agent itself, its parent
// task, and all reverse-dependency descendants of both.
//
// Daemon errors on reverse-dep lookups degrade silently (the lane just shows
// fewer events) — the connection is still opened.
func (s *Server) buildAgentEventFilter(r *http.Request, bead *beadsapi.BeadDetail) map[string]struct{} {
	set := map[string]struct{}{bead.ID: {}}
	taskID := strings.TrimSpace(bead.Fields["task_id"])
	if taskID != "" {
		set[taskID] = struct{}{}
	}
	for _, root := range []string{bead.ID, taskID} {
		if root == "" {
			continue
		}
		revDeps, err := s.daemon.GetReverseDependencies(r.Context(), root)
		if err != nil {
			s.logger.Debug("agent events: reverse deps lookup failed", "root", root, "error", err)
			continue
		}
		for _, d := range revDeps {
			if d.BeadID != "" {
				set[d.BeadID] = struct{}{}
			}
		}
	}
	return set
}

// writeAgentEvent parses a single SSE event from the daemon, applies the
// per-agent filter, and writes one rendered HTML row to the client when it
// passes. Malformed payloads, non-bead topics, and out-of-set events are
// silently skipped.
func (s *Server) writeAgentEvent(w http.ResponseWriter, flusher http.Flusher, evt beadsapi.SSEEvent, filter map[string]struct{}) {
	var wrapper agentEventSSE
	if err := json.Unmarshal(evt.Data, &wrapper); err != nil || wrapper.Bead == nil {
		return
	}
	bead := wrapper.Bead
	if _, ok := filter[bead.ID]; !ok {
		return
	}

	kind := agentEventKind(evt.Event)
	if kind == "" {
		return
	}

	row := renderAgentEventRow(s.basePath, kind, bead, wrapper.ClosedBy, wrapper.Changes, time.Now())
	// SSE event payload must be single-line — collapse newlines so the client
	// receives one well-formed `data:` line per event.
	row = strings.ReplaceAll(row, "\r", "")
	row = strings.ReplaceAll(row, "\n", " ")
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, row)
	flusher.Flush()
}

// agentEventKind maps a kbeads SSE topic onto one of the three lifecycle
// kinds the agent activity lane styles. Unknown topics return "" so the
// caller skips the event.
func agentEventKind(topic string) string {
	switch {
	case strings.Contains(topic, "created"):
		return "created"
	case strings.Contains(topic, "closed"):
		return "closed"
	case strings.Contains(topic, "updated"):
		return "updated"
	default:
		return ""
	}
}

// renderAgentEventRow produces the HTML for one row in the agent activity
// lane. Kept inline (rather than templated) so the SSE writer can drop the
// result straight into a `data:` line — htmx's SSE swap inserts whatever
// HTML arrives without re-parsing through Go templates.
func renderAgentEventRow(basePath, kind string, b *agentEventBead, closedBy string, changes map[string]any, now time.Time) string {
	title := b.Title
	if title == "" {
		title = b.ID
	}
	var sb strings.Builder
	sb.WriteString(`<div class="agent-event agent-event-`)
	sb.WriteString(html.EscapeString(kind))
	sb.WriteString(`" data-bead-id="`)
	sb.WriteString(html.EscapeString(b.ID))
	sb.WriteString(`" data-bead-type="`)
	sb.WriteString(html.EscapeString(b.Type))
	sb.WriteString(`">`)
	sb.WriteString(`<time class="agent-event-time" datetime="`)
	sb.WriteString(now.UTC().Format(time.RFC3339Nano))
	sb.WriteString(`">`)
	sb.WriteString(now.UTC().Format("15:04:05"))
	sb.WriteString(`</time>`)
	sb.WriteString(`<span class="agent-event-kind">`)
	sb.WriteString(html.EscapeString(kind))
	sb.WriteString(`</span>`)
	if b.Type != "" {
		sb.WriteString(`<span class="agent-event-type">`)
		sb.WriteString(html.EscapeString(b.Type))
		sb.WriteString(`</span>`)
	}
	sb.WriteString(`<a class="agent-event-link" href="`)
	sb.WriteString(html.EscapeString(basePath + "/" + b.ID))
	sb.WriteString(`"><code>`)
	sb.WriteString(html.EscapeString(b.ID))
	sb.WriteString(`</code></a>`)
	sb.WriteString(`<span class="agent-event-title">`)
	sb.WriteString(html.EscapeString(title))
	sb.WriteString(`</span>`)
	if kind == "closed" && closedBy != "" {
		sb.WriteString(`<span class="agent-event-meta">by `)
		sb.WriteString(html.EscapeString(closedBy))
		sb.WriteString(`</span>`)
	}
	if kind == "updated" && len(changes) > 0 {
		sb.WriteString(`<span class="agent-event-meta">`)
		first := true
		for k := range changes {
			if !first {
				sb.WriteString(`, `)
			}
			sb.WriteString(html.EscapeString(k))
			first = false
		}
		sb.WriteString(`</span>`)
	}
	sb.WriteString(`</div>`)
	return sb.String()
}
