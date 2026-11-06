package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

const (
	defaultEventsLimit = 100
	maxEventsLimit     = 1000
	defaultEventsSince = 24 * time.Hour
)

// handleListGlobalEvents handles GET /v1/events. It returns all events across
// all beads within the [since, until] time range, in chronological order
// (created_at ASC, id ASC) by default, with cursor-based pagination. Pass
// order=desc to page newest-first instead (created_at DESC, id DESC) — callers
// that only want the most-recent N events use this to bound their scan to
// exactly N rows rather than walking the window oldest-first (bug
// ka-0000004135). The cursor is transported as a bare event id, but the store
// compares the full (created_at, id) key it sorts on (reconstructed from that
// id) so pagination never drops or duplicates an event when id and created_at
// order disagree under concurrent inserts (see queryGetEventsByTimeRange).
//
// Query parameters:
//   - since:  RFC3339 timestamp (lower bound, inclusive). Default: 24h ago.
//   - until:  RFC3339 timestamp (upper bound, inclusive). Default: now.
//   - limit:  page size. Default 100, max 1000.
//   - cursor: integer event id; only events with id > cursor (ASC) or
//     id < cursor (DESC) are returned. 0 starts at the oldest (ASC) / newest (DESC).
//   - order:  "asc" (default) or "desc".
//   - exclude_types: comma-separated bead types; events whose bead has one of
//     these types are omitted (and do not count against limit). Lets a caller
//     skip bookkeeping beads (trigger, agent, ...) whose MutationUpdate events
//     carry the full fields blob.
//
// next_cursor is the id of the LAST event in the page in either order — the id
// to pass back as the next request's cursor — so the same paging contract holds
// for both directions.
//
// Response:
//
//	{ "events": [...], "has_more": bool, "next_cursor": "<id-or-empty>" }
func (s *BeadsServer) handleListGlobalEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	until := time.Now()
	since := until.Add(-defaultEventsSince)

	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since: must be an RFC3339 timestamp")
			return
		}
		since = t
	}
	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "until: must be an RFC3339 timestamp")
			return
		}
		until = t
	}
	if since.After(until) {
		writeError(w, http.StatusBadRequest, "since must be <= until")
		return
	}

	limit := defaultEventsLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit: must be a positive integer")
			return
		}
		if n > maxEventsLimit {
			n = maxEventsLimit
		}
		limit = n
	}

	var cursor int64
	if v := q.Get("cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "cursor: must be a non-negative integer")
			return
		}
		cursor = n
	}

	desc := false
	if v := q.Get("order"); v != "" {
		switch v {
		case "asc":
			desc = false
		case "desc":
			desc = true
		default:
			writeError(w, http.StatusBadRequest, "order: must be 'asc' or 'desc'")
			return
		}
	}

	// Cross-project read-authz gate (ka-0000006863): event payloads embed the
	// full bead for create/update/close topics, so a project-scoped caller must
	// only see events for beads in its authorized projects. Resolve the caller's
	// scope once and let the store JOIN through the bead's labels.
	scope := s.eventReadScope(r.Context())
	if v := q.Get("exclude_types"); v != "" {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				scope.ExcludeBeadTypes = append(scope.ExcludeBeadTypes, t)
			}
		}
	}

	// Fetch limit+1 to detect has_more without a separate count query.
	var evts []*model.Event
	var err error
	if desc {
		evts, err = s.store.GetEventsByTimeRangeDesc(r.Context(), since, until, cursor, limit+1, scope)
	} else {
		evts, err = s.store.GetEventsByTimeRange(r.Context(), since, until, cursor, limit+1, scope)
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleListGlobalEvents", err, "failed to list events")
		return
	}

	hasMore := len(evts) > limit
	if hasMore {
		evts = evts[:limit]
	}

	nextCursor := ""
	if hasMore && len(evts) > 0 {
		nextCursor = strconv.FormatInt(evts[len(evts)-1].ID, 10)
	}

	if evts == nil {
		evts = []*model.Event{}
	}
	// A create/update/close payload embeds the whole bead: inherit:read for
	// each event's owning bead.
	if err := s.propagateReadTaintBeads(r.Context(), eventBeadIDs(evts)); err != nil {
		writeServerError(r.Context(), w, "handleListGlobalEvents", err, "failed to propagate read taint")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"events":      evts,
		"has_more":    hasMore,
		"next_cursor": nextCursor,
	})
}
