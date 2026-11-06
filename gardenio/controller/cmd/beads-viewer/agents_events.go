package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// agentsIndexEventsTopic is the NATS topic glob the /agents index subscribes
// to. We watch every bead lifecycle event and filter to bead.Type == "agent"
// (and an optional project scope) in-memory.
const agentsIndexEventsTopic = "beads.bead.>"

// handleAgentsEventsStatus is a cheap GET probe the SSE clients call before
// opening EventSource. It exists so Firefox does not flag the EventSource as
// "Blocked" when an AVA-fronted unauthenticated request gets a 303 to a
// cross-origin OAuth provider. Browser EventSource cannot follow cross-origin
// redirects, so it errors with no signal; a plain fetch with redirect:'manual'
// against this endpoint surfaces the same auth verdict without ever triggering
// the SSE-blocked indicator. When the request reaches this handler at all, the
// upstream auth gate has accepted it — so we just return 200.
func (s *Server) handleAgentsEventsStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok"}`)
}

// handleAgentsEvents serves a Server-Sent Events stream that fans out
// fleet-wide agent-bead lifecycle events to the /agents index. Optional
// ?project=<name> narrows the stream to one project; optional
// ?schedule_id=<gardener trigger bead ID> narrows it to the iterations one
// gardener spawned (the gardener page subscribes with this filter — design doc
// ka-0000003219 §8, v2 §5).
//
// Gardener-scope dual-key match (design doc v2 §5 / ka-0000007051): the gardener
// id is stamped on its iterations under EITHER trigger_id (modern bridge path)
// or schedule_id (legacy scheduler path). The ?schedule_id=<capID> filter
// therefore matches iterations carrying that id under *either* field — mirroring
// gardenerIterations' union query — so live rows surface for trigger-path
// gardeners too, not just legacy ones.
//
// Row shape depends on scope: the fleet-wide /agents index gets a <tr> table
// row keyed by data-bead-id / id="agent-row-<beadID>"; a gardener-scoped stream
// gets an <li> life-thread card (matching gardener_iteration_card) with an
// "in progress" badge and no uptime/MR-final on live rows. Either way the row
// is keyed by data-bead-id so the client can swap an existing row in place or
// insert a fresh one.
func (s *Server) handleAgentsEvents(w http.ResponseWriter, r *http.Request) {
	r, cancelStream := sseRequestUntilTokenExpiry(r)
	defer cancelStream()
	projectFilter := strings.TrimSpace(r.URL.Query().Get("project"))
	scheduleIDFilter := strings.TrimSpace(r.URL.Query().Get("schedule_id"))

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	events, err := s.daemon.EventStream(r.Context(), agentsIndexEventsTopic)
	if err != nil {
		s.logger.Error("opening event stream for /agents index", "error", err)
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
			s.writeAgentsIndexEvent(ctx, w, flusher, evt, projectFilter, scheduleIDFilter)
		}
	}
}

// writeAgentsIndexEvent parses a single SSE event from the daemon, filters
// out anything that isn't a `type=agent` bead (and anything outside the
// optional project scope or gardener scope), and writes one rendered row to
// the client. The row is a <tr> table row for the fleet-wide /agents index
// and an <li> life-thread card when scheduleIDFilter scopes the stream to one
// gardener (so the gardener page receives card markup it can swap into its
// <ul class="gardener-thread">).
func (s *Server) writeAgentsIndexEvent(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, evt beadsapi.SSEEvent, projectFilter, scheduleIDFilter string) {
	var wrapper agentEventSSE
	if err := json.Unmarshal(evt.Data, &wrapper); err != nil || wrapper.Bead == nil {
		return
	}
	bead := wrapper.Bead
	if bead.Type != "agent" {
		return
	}
	fields := parseAgentEventFields(bead.Fields)
	project := agentEventProject(bead, fields)
	if projectFilter != "" && project != projectFilter {
		return
	}
	// Gardener scope: the gardener page subscribes with
	// ?schedule_id=<gardener trigger bead ID> so only the iterations that
	// gardener spawned stream in. The id is stamped under EITHER trigger_id
	// (modern bridge path) or schedule_id (legacy scheduler path), so we match
	// on both — mirroring gardenerIterations' dual-key union (ka-0000007051).
	// Manual agents carry neither key, so the empty-string filter correctly
	// keeps them out of a gardener thread.
	gardenerScoped := scheduleIDFilter != ""
	if gardenerScoped && fields[fieldScheduleID] != scheduleIDFilter && fields[fieldTriggerID] != scheduleIDFilter {
		return
	}

	kind := agentEventKind(evt.Event)
	if kind == "" {
		return
	}

	var row string
	if gardenerScoped {
		// Gardener page wants an <li> life-thread card, not a <tr>: live rows
		// carry an "in progress" badge and suppress uptime/MR-final, exactly
		// like the eager-render path (design doc v2 §5).
		row = s.renderGardenerStreamCard(ctx, bead, fields, kind)
	} else {
		row = s.renderAgentsIndexRow(ctx, kind, bead, fields, project, projectFilter, s.now())
	}
	if row == "" {
		return
	}
	// SSE event payload must be single-line — collapse newlines so the client
	// receives one well-formed `data:` line per event.
	row = strings.ReplaceAll(row, "\r", "")
	row = strings.ReplaceAll(row, "\n", " ")
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, row)
	flusher.Flush()
}

// gardenerStreamCardPartial is the partial file + named template the gardener
// life-thread renders for each iteration. Reused here so a streamed live row
// is byte-identical to the eager-render card (no second markup to drift).
const (
	gardenerStreamCardFile = "templates/partials/gardener_iteration_card.html"
	gardenerStreamCardName = "gardener_iteration_card"
)

// renderGardenerStreamCard renders one gardener-scoped SSE event as an <li>
// life-thread card, matching the gardener_iteration_card the eager page emits.
// Live (non-closed) iterations carry the "in progress" ribbon and suppress the
// uptime/MR-final chips (those only exist post-close); the IsLive flag on the
// view-model drives that in the shared template, so we get identical markup.
//
// We reuse the production view-model builder (agentRowFromBeadCtx + the
// gardenerIterationView overlay) so the streamed card cannot drift from the
// eager render. Run numbering/ordering is intentionally NOT reconstructed here:
// a single SSE event has no access to the full chain, so the ribbon shows the
// in-progress/closed state and the run's own snapshot budget, while the client
// swaps the card in place by data-bead-id (the eager render already numbered it).
func (s *Server) renderGardenerStreamCard(ctx context.Context, eb *agentEventBead, fields map[string]string, kind string) string {
	bead := beadDetailFromEvent(eb, fields)
	// Resolve the operator's /u/<slug> homepage link on the streamed node so the
	// gardener-card operator chip deep-links identically to the eager render
	// rather than reconciling on the next reload (ka-0000007344). Nil-safe,
	// per-server cached, single-bead — a no-op unless OperatedByID is set.
	s.populateOperatedBySlug(ctx, bead)
	// A closed event flips the card to its terminal (historical) state even if
	// the bead status payload lags behind the lifecycle topic.
	if kind == "closed" && bead.Status != "closed" {
		bead.Status = "closed"
	}
	now := s.now()

	row := s.agentRowFromBeadCtx(ctx, bead, agentEventProject(eb, fields), now)
	iter := gardenerIterationView{
		agentRowView: row,
		RunsSpawned:  parseScheduleInt(fields, fieldScheduleRunsSpawned),
		MaxRuns:      parseScheduleInt(fields, fieldScheduleMaxRuns),
		PriorAgentID: fields[fieldSchedulePriorAgentID],
		TerminalGoal: fields[fieldScheduleTerminalGoal],
		IsLive:       bead.Status != "closed",
	}
	// RunNumber is unknown from a single event (no chain in scope); the client
	// swaps the already-numbered eager card in place. Label without a number so
	// a fresh live row reads "in progress" rather than a misleading "Iteration 0".
	iter.RunLabel = gardenerStreamRunLabel(iter.MaxRuns, iter.IsLive)

	tmpl, ok := s.partials[gardenerStreamCardFile]
	if !ok {
		s.logger.Error("gardener stream card: partial not found", "file", gardenerStreamCardFile)
		return ""
	}
	bound, err := s.bindAgentLink(tmpl, nil)
	if err != nil {
		s.logger.Error("gardener stream card: bind template", "error", err)
		return ""
	}
	var buf bytes.Buffer
	data := map[string]any{"Iter": iter, "BasePath": s.basePath}
	if err := bound.ExecuteTemplate(&buf, gardenerStreamCardName, data); err != nil {
		s.logger.Error("gardener stream card: render", "id", eb.ID, "error", err)
		return ""
	}
	return buf.String()
}

// gardenerStreamRunLabel builds the ribbon for a streamed card. Unlike runLabel
// it omits the run number (a single SSE event carries no chain position): a
// live row reads "In progress", a closed row reads "Completed" / "Completed of
// N". The client swaps this card over the eager-numbered card in place, so the
// authoritative "Iteration N" survives once the page reloads or the next eager
// render runs.
func gardenerStreamRunLabel(maxRuns int, isLive bool) string {
	if isLive {
		return "In progress"
	}
	if maxRuns > 0 {
		return fmt.Sprintf("Completed of %d", maxRuns)
	}
	return "Completed"
}

// agentEventProject returns the bead's project (from project:<name> label
// first, then the project field), falling back to "untagged" to match
// agentProjectName in the /agents page render.
func agentEventProject(b *agentEventBead, fields map[string]string) string {
	if p := projectFromLabels(b.Labels); p != "" {
		return p
	}
	if p := strings.TrimSpace(fields["project"]); p != "" {
		return p
	}
	return "untagged"
}

// parseAgentEventFields decodes the bead.fields JSON payload into a flat
// string→string map. Returns an empty map on malformed input so callers can
// safely treat missing fields as empty.
//
// Delegates to the canonical beadsapi.ParseFieldsJSON — the SAME decoder the
// eager GetBead path uses — so a NUMERIC field value survives the SSE round-trip
// as its string form instead of being silently dropped. A naive
// json.Unmarshal(raw, &map[string]string) returns an error the moment a value is
// a JSON number, and the partial decode leaves every numeric-valued key blank:
// live_turns and total_request_count are schema type=integer (written as native
// ints by reportLiveUsage / the session-close reconciler), so on the pre-fix
// path an agent that had taken a turn streamed a blank turns cell until the next
// eager reload reconciled it — the SSE field-drop class this task guards against
// (ka-0000000324; sibling to the ka-0000003512 / ka-0000000482 top-level-field
// drops). ParseFieldsJSON re-marshals numbers/arrays/objects to their JSON
// string form, matching what the row builder's field consumers
// (buildAgentUsageView, parseTokenField, …) already expect.
func parseAgentEventFields(raw json.RawMessage) map[string]string {
	return beadsapi.ParseFieldsJSON(raw)
}

// agentsRowPartial is the partial file + the two named templates the
// fleet-wide /agents index streams. Reused so a streamed row is byte-identical
// to the eager page render (no second markup to drift) — the same congruence
// contract renderGardenerStreamCard already honours for the gardener page.
const (
	agentsRowPartialFile = "templates/partials/agents_row.html"
	agentsCompactRowName = "agents_compact_row"
	agentsRecentRowName  = "agents_recent_row"
)

// renderAgentsIndexRow emits the HTML <tr> for one fleet-wide agent event by
// running the SSE bead through the SAME view-model builder + shared template the
// static /agents page uses. Before ka-0000007652 this hand-rolled an 8-column
// row (empty Links, no Owner/Usage, "—" uptime, "just now" active) that the
// client swapped in place under the static 11-column <thead> — so the live view
// visibly changed shape vs the static load. Now both paths converge on
// agentRowFromBeadCtx + agents_compact_row / agents_recent_row.
//
// A live (created/updated) event renders the compact row; a closed event
// renders the RECENT row, because the client moves a closed node into the
// 6-column recent table (agents_stream.js onClosed) — emitting the compact
// shape there is what column-shifted and overflowed the recent table.
//
// The daemon publishes the full model.Bead on every lifecycle event, and
// beadDetailFromEvent now carries created_at/updated_at/closed_at/description/
// owner/operated_by* through to the row builder — so Up (created_at),
// Active/Stale (updated_at), the description snippet, Owner, and the operator
// chip render with real values on the streamed node rather than blanking until
// the next eager reload (ka-0000000482). OperatedBySlug (a viewer-side lookup,
// not on the wire) is resolved here via populateOperatedBySlug before render, so
// the operator chip streams with its /u/<slug> deep-link too (ka-0000007344).
func (s *Server) renderAgentsIndexRow(ctx context.Context, kind string, eb *agentEventBead, fields map[string]string, project, projectFilter string, now time.Time) string {
	bead := beadDetailFromEvent(eb, fields)
	// Resolve the operator's /u/<slug> homepage link on the streamed node so the
	// operator chip deep-links identically to the eager render rather than
	// reconciling only on the next reload (ka-0000007344). Nil-safe, per-server
	// cached, single-bead — a no-op unless OperatedByID is set.
	s.populateOperatedBySlug(ctx, bead)
	// A closed event flips the row to its terminal state even if the bead
	// status payload lags behind the lifecycle topic (mirrors the gardener card).
	if kind == "closed" && bead.Status != "closed" {
		bead.Status = "closed"
	}

	row := s.agentRowFromBeadCtx(ctx, bead, project, now)

	tmpl, ok := s.partials[agentsRowPartialFile]
	if !ok {
		s.logger.Error("agents index row: partial not found", "file", agentsRowPartialFile)
		return ""
	}
	bound, err := s.bindAgentLink(tmpl, nil)
	if err != nil {
		s.logger.Error("agents index row: bind template", "error", err)
		return ""
	}

	// Closed rows live in the recent table (6 cols); live rows in the active
	// table (10/11 cols). Pick the matching named template so the swapped node
	// always fits the table the client drops it into.
	name := agentsCompactRowName
	if kind == "closed" {
		name = agentsRecentRowName
	}
	data := map[string]any{
		"Row":           row,
		"BasePath":      s.basePath,
		"ProjectFilter": projectFilter,
		"Kind":          kind,
	}
	var buf bytes.Buffer
	if err := bound.ExecuteTemplate(&buf, name, data); err != nil {
		s.logger.Error("agents index row: render", "id", eb.ID, "error", err)
		return ""
	}
	return buf.String()
}
