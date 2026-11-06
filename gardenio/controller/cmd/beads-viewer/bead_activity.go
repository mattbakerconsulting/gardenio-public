package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// activityFeedLimit caps the number of rows the activity feed renders.
// Matches §7 of the research doc — newest 50 rows is enough for the LV-1
// "what just happened" pane without paging.
const activityFeedLimit = 50

// activityFetchLimit is the upper bound passed to the daemon's
// /v1/beads/{id}/events?limit=N. Higher than activityFeedLimit so the
// reverse-sort in buildActivityRows has a small safety margin if the daemon
// ever returns rows in a slightly different order; small enough to keep
// per-render memory in the tens-of-KB even for trigger beads with multi-MB
// total histories (bug ka-0000002256).
const activityFetchLimit = 100

// commentBodyTruncate is the soft cap on inline comment body length per §5
// of the research doc. Bodies longer than this collapse into a <details>
// element so the feed stays scannable.
const commentBodyTruncate = 300

// activityRow is the per-event view model rendered by bead_activity.html.
// One row per daemon event row, already enriched with the glyph, actor
// fallback, and per-topic body so the template can render uniformly.
type activityRow struct {
	EventID   int64
	Topic     string
	Glyph     string
	Actor     string
	CreatedAt time.Time
	// Body is the short, human-readable summary line — already
	// HTML-escaped by template execution. For comments, BodyShort holds
	// the truncated head and BodyFull holds the full text; the template
	// renders BodyFull via <details> when BodyTruncated is true.
	Body          string
	BodyShort     string
	BodyFull      string
	BodyTruncated bool
	// Operator is the secondary "operated by …" attribution — the human
	// operator on whose behalf an agent acted (ev.OperatedBy) — rendered as
	// a muted qualifier, NOT as the subject of the verb. Empty when there is
	// no distinct operator to attribute (bug ka-0000005471).
	Operator string
}

// handleBeadActivity returns the per-bead activity-feed fragment.
//
// Calls GetBead first so a 403/404 from read-authz short-circuits before we
// hit /v1/beads/{id}/events (read-parity per §4.3 of the research doc).
// Falls back to an empty list on a non-fatal events error so the partial
// always renders something for htmx polling.
func (s *Server) handleBeadActivity(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		if beadMissing(err) {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for activity", "id", id, "error", err)
		http.Error(w, "Failed to get bead", http.StatusInternalServerError)
		return
	}
	events, err := s.daemon.GetRecentEvents(r.Context(), id, activityFetchLimit)
	if err != nil {
		s.logger.Warn("getting events", "id", id, "error", err)
		events = nil
	}
	rows := buildActivityRows(events)
	s.renderPartial(w, r, "templates/partials/bead_activity.html", "bead_activity", map[string]any{
		"Bead":     bead,
		"Activity": rows,
	})
}

// buildActivityRows clamps to the newest activityFeedLimit events and turns
// each event into an activityRow with glyph + body precomputed. Daemon
// returns events oldest-first; we reverse client-side per §7 of the research
// doc so the rail shows newest-first without leaning on a sorted index.
func buildActivityRows(events []beadsapi.Event) []activityRow {
	if len(events) == 0 {
		return nil
	}
	sorted := make([]beadsapi.Event, len(events))
	copy(sorted, events)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})
	if len(sorted) > activityFeedLimit {
		sorted = sorted[:activityFeedLimit]
	}
	rows := make([]activityRow, 0, len(sorted))
	for _, ev := range sorted {
		rows = append(rows, renderActivityRow(ev))
	}
	return rows
}

// renderActivityRow turns a single event into the view-model row, including
// glyph mapping, actor fallback chain, and per-topic body construction.
func renderActivityRow(ev beadsapi.Event) activityRow {
	row := activityRow{
		EventID:   ev.ID,
		Topic:     ev.Topic,
		Glyph:     glyphForTopic(ev.Topic),
		CreatedAt: ev.CreatedAt,
	}
	row.Actor = activityActor(ev)
	row.Operator = activityOperator(ev, row.Actor)
	body, short, full, truncated := activityBody(ev)
	row.Body = body
	row.BodyShort = short
	row.BodyFull = full
	row.BodyTruncated = truncated
	return row
}

// glyphForTopic returns the per-event glyph from §5 of the research doc.
// Unknown topics fall back to a neutral bullet so future daemon-side
// additions don't break the rail.
func glyphForTopic(topic string) string {
	switch topic {
	case "beads.bead.created":
		return "✦"
	case "beads.bead.updated":
		return "✎"
	case "beads.bead.closed":
		return "✓"
	case "beads.bead.deleted":
		return "✗"
	case "beads.bead.transferred":
		return "→"
	case "beads.dependency.added":
		return "⊕"
	case "beads.dependency.removed":
		return "⊖"
	case "beads.label.added":
		return "🏷+"
	case "beads.label.removed":
		return "🏷−"
	case "beads.comment.added":
		return "💬"
	default:
		return "•"
	}
}

// activityActor returns the identity that actually PERFORMED the event — the
// subject of the verb ("<actor> closed bead"). The acting identity leads;
// operated_by is deliberately NOT consulted here (bug ka-0000005471): it names
// the human operator on whose behalf an agent acted, so leading with it made an
// operator read as if they closed/updated a bead the agent's own stop-gate
// closed. operated_by is surfaced separately as a muted "operated by …"
// qualifier via activityOperator.
//
// Resolution order for the acting identity:
//
//	payload event-actor (closed_by / …) → actor → payload.bead.assignee
//	→ payload.bead.created_by → operated_by (last resort) → "unknown"
//
// operated_by remains the final fallback so a row is never actor-less when the
// daemon only recorded the operator; it is otherwise never the verb's subject.
// Comments override with payload.comment.author when present.
func activityActor(ev beadsapi.Event) string {
	if ev.Topic == "beads.comment.added" {
		if a := commentAuthor(ev.Payload); a != "" {
			return a
		}
	}
	if a := payloadEventActor(ev); a != "" {
		return a
	}
	if ev.Actor != "" {
		return ev.Actor
	}
	if a := payloadBeadField(ev.Payload, "assignee"); a != "" {
		return a
	}
	if a := payloadBeadField(ev.Payload, "created_by"); a != "" {
		return a
	}
	if ev.OperatedBy != "" {
		return ev.OperatedBy
	}
	return "unknown"
}

// activityOperator returns the secondary "operated by …" attribution: the human
// operator (ev.OperatedBy) on whose behalf an agent acted. Returned only when it
// adds information — non-empty AND distinct from the acting identity (actor) —
// so a row where the operator IS the actor (e.g. a human's own edit) does not
// render a redundant "(operated by <self>)" qualifier. Empty otherwise (bug
// ka-0000005471).
func activityOperator(ev beadsapi.Event, actor string) string {
	if ev.OperatedBy == "" || ev.OperatedBy == actor {
		return ""
	}
	return ev.OperatedBy
}

// payloadEventActor returns the per-topic acting identity carried in the event
// payload, distinct from the operator. Today only close events name their actor
// (payload.closed_by); other topics carry it on ev.Actor. Empty when absent.
func payloadEventActor(ev beadsapi.Event) string {
	if ev.Topic == "beads.bead.closed" {
		return payloadStringField(ev.Payload, "closed_by")
	}
	return ""
}

// activityBody renders the per-topic summary line. For most topics, the
// body is a single string; the comment topic returns a short head plus the
// full body so the template can collapse long comments into <details>.
func activityBody(ev beadsapi.Event) (body, short, full string, truncated bool) {
	switch ev.Topic {
	case "beads.bead.created":
		return "created bead", "", "", false
	case "beads.bead.updated":
		return updatedBody(ev.Payload), "", "", false
	case "beads.bead.closed":
		// The closer leads the row as the acting identity (activityActor);
		// no closer parenthetical here (bug ka-0000005471).
		return "closed bead", "", "", false
	case "beads.bead.deleted":
		return "deleted bead", "", "", false
	case "beads.bead.transferred":
		return transferredBody(ev.Payload), "", "", false
	case "beads.dependency.added":
		return depBody(ev.Payload, "added"), "", "", false
	case "beads.dependency.removed":
		return depBody(ev.Payload, "removed"), "", "", false
	case "beads.label.added":
		return labelBody(ev.Payload, "added"), "", "", false
	case "beads.label.removed":
		return labelBody(ev.Payload, "removed"), "", "", false
	case "beads.comment.added":
		full := commentText(ev.Payload)
		if len(full) <= commentBodyTruncate {
			return full, "", "", false
		}
		return "", truncate(full, commentBodyTruncate), full, true
	default:
		return ev.Topic, "", "", false
	}
}

// truncate clips s at the last word boundary at or before max so the
// truncated head doesn't end mid-token. Caller is responsible for
// appending an ellipsis in the rendered output.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndexAny(s[:max], " \t\n")
	if cut <= 0 {
		cut = max
	}
	return strings.TrimRight(s[:cut], " \t\n")
}

// payloadBeadField returns a top-level string field from payload.bead,
// e.g. "assignee" or "created_by". Empty string when missing.
func payloadBeadField(payload json.RawMessage, field string) string {
	if len(payload) == 0 {
		return ""
	}
	var wrapper struct {
		Bead map[string]any `json:"bead"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil || wrapper.Bead == nil {
		return ""
	}
	if v, ok := wrapper.Bead[field].(string); ok {
		return v
	}
	return ""
}

// payloadStringField returns a top-level string field from the event payload
// (e.g. "closed_by"), distinct from payloadBeadField which reads from the
// nested payload.bead object. Empty string when missing or non-string.
func payloadStringField(payload json.RawMessage, field string) string {
	if len(payload) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	if v, ok := m[field].(string); ok {
		return v
	}
	return ""
}

// commentAuthor extracts payload.comment.author for the comment-added
// topic; falls back to empty string when the shape is unexpected.
func commentAuthor(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var wrapper struct {
		Comment struct {
			Author string `json:"author"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		return ""
	}
	return wrapper.Comment.Author
}

// commentText extracts payload.comment.text. Empty string when missing.
func commentText(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var wrapper struct {
		Comment struct {
			Text string `json:"text"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		return ""
	}
	return wrapper.Comment.Text
}

// updatedBody summarises a bead.updated event by listing the fields that
// changed, with new scalar values rendered inline where reasonable. Long or
// non-scalar values (descriptions, JSON blobs, arrays) collapse to just the
// field name so the rail stays scannable while still showing what changed.
func updatedBody(payload json.RawMessage) string {
	if len(payload) == 0 {
		return "updated bead"
	}
	var wrapper struct {
		Changes map[string]any `json:"changes"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil || len(wrapper.Changes) == 0 {
		return "updated bead"
	}
	keys := make([]string, 0, len(wrapper.Changes))
	for k := range wrapper.Changes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, formatChangeEntry(k, wrapper.Changes[k]))
	}
	return "updated " + strings.Join(parts, ", ")
}

// changeValueMaxLen caps each rendered new-value so a single change can't blow
// out the row. Names like "fields", "preferences", or "description" usually
// hold blobs we want to elide.
const changeValueMaxLen = 60

// formatChangeEntry renders one (field, new-value) pair for the activity feed.
// Scalars (string/number/bool) render as `field:value`; nil renders as
// `field:∅`; non-scalars (maps, arrays) collapse to bare `field` so we don't
// dump JSON into the rail. Strings longer than changeValueMaxLen truncate
// with an ellipsis.
func formatChangeEntry(field string, value any) string {
	switch v := value.(type) {
	case nil:
		return field + ":∅"
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return field + ":∅"
		}
		s = strings.ReplaceAll(s, "\n", " ")
		if len(s) > changeValueMaxLen {
			s = truncate(s, changeValueMaxLen) + "…"
		}
		return field + ":" + s
	case bool:
		if v {
			return field + ":true"
		}
		return field + ":false"
	case float64:
		// json.Unmarshal into any decodes all numbers as float64. Render
		// whole numbers without a trailing .0 so e.g. priority:1 stays
		// compact.
		if v == float64(int64(v)) {
			return field + ":" + strings.TrimRight(strings.TrimRight(formatChangeNumber(v), "0"), ".")
		}
		return field + ":" + formatChangeNumber(v)
	default:
		// Maps, slices, or anything else exotic — just name the field so
		// the rail doesn't render JSON.
		return field
	}
}

// formatChangeNumber renders a JSON-decoded number compactly: 1 → "1", 1.5 → "1.5".
func formatChangeNumber(v float64) string {
	return fmt.Sprintf("%g", v)
}

// transferredBody renders the from→to fleet transition.
func transferredBody(payload json.RawMessage) string {
	if len(payload) == 0 {
		return "transferred bead"
	}
	var wrapper struct {
		From string `json:"from_fleet"`
		To   string `json:"to_fleet"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		return "transferred bead"
	}
	if wrapper.From == "" && wrapper.To == "" {
		return "transferred bead"
	}
	return "transferred " + wrapper.From + " → " + wrapper.To
}

// depBody renders the depends_on target and dep type for dependency events.
func depBody(payload json.RawMessage, verb string) string {
	if len(payload) == 0 {
		return verb + " dependency"
	}
	var wrapper struct {
		Dependency struct {
			DependsOnID string `json:"depends_on_id"`
			Type        string `json:"type"`
		} `json:"dependency"`
		// dependency.removed payloads are flat (no wrapper).
		DependsOnID string `json:"depends_on_id"`
		Type        string `json:"type"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		return verb + " dependency"
	}
	target := wrapper.Dependency.DependsOnID
	if target == "" {
		target = wrapper.DependsOnID
	}
	depType := wrapper.Dependency.Type
	if depType == "" {
		depType = wrapper.Type
	}
	if target == "" {
		return verb + " dependency"
	}
	if depType == "" {
		return verb + " " + target
	}
	return verb + " " + depType + " → " + target
}

// labelBody renders the label name for label.added / label.removed.
func labelBody(payload json.RawMessage, verb string) string {
	if len(payload) == 0 {
		return verb + " label"
	}
	var wrapper struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil || wrapper.Label == "" {
		return verb + " label"
	}
	return verb + " " + wrapper.Label
}

// Compile-time assertion that beadAPI carries GetEvents and GetRecentEvents —
// keeps the activity handler honest if the interface is split or renamed.
var _ = func(ctx context.Context, api beadAPI) {
	_, _ = api.GetEvents(ctx, "")
	_, _ = api.GetRecentEvents(ctx, "", 0)
}
