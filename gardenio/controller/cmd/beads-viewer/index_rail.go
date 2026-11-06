package main

import (
	"context"
	"html/template"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// squawkPreviewMaxLen caps each per-agent event-stream row rendered in the Home
// "Live agents" rail. ~80 chars keeps the row to a single line at typical
// rail width while still conveying "what just happened" (ka-0000003997).
const squawkPreviewMaxLen = 80

// squawkLookbackLimit bounds the single batched squawk fetch backing the rail
// preview. Squawks are closed type=message beads sorted newest-first; pulling
// a few hundred is enough to cover every currently-alive agent's latest lines
// without a per-agent round-trip, and the in-memory join keeps only the first
// (newest) few squawks seen per source_agent.
const squawkLookbackLimit = 300

// eventStreamRowsPerAgent caps how many recent events (squawks + comments,
// merged and de-duplicated) each Live-agents card renders. Three keeps the
// card to a compact stacked list — "what just happened", not a full timeline —
// without blowing out card height in the two-up grid (ka-0000003393).
const eventStreamRowsPerAgent = 3

// commentEventsLookbackWindow bounds the single durable cross-bead events fetch
// that surfaces recent agent COMMENTS for the stream. A few hours is enough to
// cover every currently-alive agent's latest commentary; the join keeps only
// events authored by a live agent, so a wider window just means more rows
// scanned in-memory, never more round-trips.
const commentEventsLookbackWindow = 6 * time.Hour

// commentEventsLimit caps the durable-events page pulled for the comment join.
// It is the server-side max (1000) so ONE request covers the busiest window —
// the join stays O(1) daemon round-trips regardless of fleet size
// (ka-0000003393). If the daemon ever truncates below the full window the
// stream simply shows fewer comment rows; squawks are unaffected.
const commentEventsLimit = 1000

// squawkMirrorFooter matches the trailer `gb squawk` appends when it mirrors a
// squawk onto the agent's claimed bead as a comment (see cmd/gb/squawk.go
// autoCommentClaimedBead: "_squawk gd-… from <actor>_"). Those mirror comments
// carry the SAME text as the squawk bead already surfaced by the squawk join,
// so the event-stream merge drops them to avoid a duplicate row per squawk.
var squawkMirrorFooter = regexp.MustCompile(`(?m)^_squawk (?:gd|ka)-\S+ from .+_\s*$`)

// indexHotBead is one row in the /index "Hot ready" panel — top open beads
// with no open dependencies, sorted by priority. Surfaces the ready queue
// on the landing page so the right-hand viewport carries signal instead of
// whitespace (ka-0000006596).
type indexHotBead struct {
	ID       string
	Title    string
	Type     string
	Priority int
	Project  string
	Ago      string
}

// indexLiveAgent is one row in the /index "Live agents" panel — in-progress
// agent beads with their task and project. AgentState (working/spawning/
// yielding) and OperatedBy are surfaced compactly so the Home rail carries the
// same who-is-doing-what signal as /agents without leaving the landing page
// (ka-0000007459).
//
// The second slice of ka-0000007459 enriches each row with the audit-ranked
// missing signals (ka-0000008016): Role (who is this — gardener/crew/triage),
// TaskTitle (what is it doing, beyond a bare id), Uptime + Stale (the
// "is-this-agent-wedged" signal the audit flagged as highest-value), and
// compact MR + pending-decision indicators. Every field derives from the
// agent bead already fetched by computeIndexLiveAgents — no extra round-trips.
//
// The third slice (ka-0000003997) adds the "what just happened" signal —
// Squawk (the agent's latest squawk text, truncated) — and renders OperatedBy
// as a compact initials chip (OperatedByChip) instead of a raw provider
// handle. Both are computed with NO per-agent N+1: squawks are pulled in a
// single batched list call (type=message, label=squawk) and joined in-memory
// by source_agent, and the chip is rendered from the agent bead already in
// hand.
//
// The fourth slice (ka-0000003393) upgrades that single latest-squawk preview
// into a compact per-agent RECENT EVENT STREAM (Events): the newest few real
// agent messages — squawks AND bead comments — stacked newest-first with a
// relative time per row. Both feeds stay O(1) daemon round-trips regardless of
// fleet size: squawks from the one batched type=message/label=squawk list, and
// comments from ONE durable cross-bead ListGlobalEvents page joined in-memory
// by comment author. The single-line Squawk preview it superseded — and its
// unreachable {{else if .Squawk}} template fallback — were removed in
// ka-0000000215.
type indexLiveAgent struct {
	ID         string
	Title      string
	Role       string
	Project    string
	TaskID     string
	TaskTitle  string
	AgentState string
	OperatedBy string
	// OperatedByChip is the compact initials-chip rendering of OperatedBy
	// (reusing operator_chip.go / operator_initials.go). When non-empty it
	// supersedes the raw "@handle" span in the template.
	OperatedByChip template.HTML
	Uptime         string
	Ago            string
	// Events is the compact recent-event stream for this agent — the newest
	// few squawks + comments, merged newest-first and de-duplicated, each row
	// carrying a relative time and a truncated one-line body (ka-0000003393).
	// Nil when the agent has produced nothing in the lookback windows.
	Events []agentEvent
	// Stale flags an "alive" agent whose last bead activity is older than
	// agentStaleThreshold — a likely-wedged pod the green state pill alone
	// would hide. Mirrors the /agents page's Stale semantics.
	Stale bool
	// MRURL is set when the agent is claiming a merge request, so the row can
	// render a compact [MR] badge linking out to GitLab.
	MRURL string
	// DecisionID is the open decision bead this agent is blocked on (from
	// fields.decision_bead_id), driving a [DEC] badge that links to it.
	DecisionID string
	// PodPhase is the Kubernetes pod phase (Running/Pending/…) reported on the
	// agent bead by the status reporter. Surfaced as a compact chip on the Home
	// "Active agents" primary section so an operator can spot a pod that is
	// alive-in-state but not actually Running (ka-0000007459). Empty when the
	// controller has not yet reported a phase.
	PodPhase string
}

// agentEvent is one row in a Live-agents card's recent-event stream — a single
// real message from the agent, either a squawk or a bead comment. Kind drives a
// small glyph/class in the template; Text is the one-line truncated body; Ago is
// the relative time; At is the raw timestamp used only to merge-sort the two
// feeds newest-first before rendering (ka-0000003393).
type agentEvent struct {
	Kind string // "squawk" | "comment"
	Text string
	Ago  string
	At   time.Time
}

// stampedSquawk is one dated event (squawk or comment) collected per agent for
// the recent-event stream — despite the name it backs both feeds.
// Text is the raw squawk body (not yet previewed); At is its created_at, used to
// merge squawks with comments in newest-first order.
type stampedSquawk struct {
	Text string
	At   time.Time
}

// indexOpenDecision is one row in the /index "Open decisions" panel — open
// decision beads with their bead id and project label.
type indexOpenDecision struct {
	ID      string
	Title   string
	Project string
	Ago     string
}

// indexHotEpic is one row in the /index "Hot epics" panel — open epic beads
// sorted by most-recently-updated. Fills the void beneath "Live agents" with
// signal about which large workstreams are currently active (ka-0000006022).
type indexHotEpic struct {
	ID       string
	Title    string
	Priority int
	Project  string
	Ago      string
}

// indexRail bundles the right-rail collections so handleIndex can plumb a
// single struct into the template.
type indexRail struct {
	Hot       []indexHotBead
	Agents    []indexLiveAgent
	Decisions []indexOpenDecision
	Epics     []indexHotEpic
	MRStatus  indexMRStatus
}

// computeIndexRail fetches the right-rail panels in parallel. Each sub-call
// is best-effort — a daemon error empties just that panel and is logged,
// the rest of the page still renders.
//
// viewerEmail is plumbed through to the Live agents panel solely for the
// is-viewer-self skin on the operated-by chip.
func (s *Server) computeIndexRail(ctx context.Context, viewerEmail string) indexRail {
	const panelLimit = 8
	// Active agents are now the Home page's PRIMARY content, not a side rail
	// (ka-0000007459). The cap is raised so a busy fleet fills the two-up grid
	// well past the ≥12-above-the-fold target at 1440x900 without truncating a
	// real fleet. The single ListBeadsFiltered + one batched squawk join keep
	// this O(1) round-trips regardless of the cap.
	const agentsLimit = 60

	var (
		wg    sync.WaitGroup
		rail  indexRail
		hot   []indexHotBead
		ags   []indexLiveAgent
		decs  []indexOpenDecision
		epics []indexHotEpic
		mrs   indexMRStatus
	)
	wg.Add(5)
	go func() {
		defer wg.Done()
		hot = s.computeIndexHot(ctx, panelLimit)
	}()
	go func() {
		defer wg.Done()
		ags = s.computeIndexLiveAgents(ctx, agentsLimit, viewerEmail)
	}()
	go func() {
		defer wg.Done()
		decs = s.computeIndexOpenDecisions(ctx, panelLimit)
	}()
	go func() {
		defer wg.Done()
		epics = s.computeIndexHotEpics(ctx, panelLimit)
	}()
	go func() {
		defer wg.Done()
		mrs = s.computeIndexMRStatus(ctx)
	}()
	wg.Wait()
	rail.Hot = hot
	rail.Agents = ags
	rail.Decisions = decs
	rail.Epics = epics
	rail.MRStatus = mrs
	return rail
}

// computeIndexHotEpics returns up to limit open epic beads sorted by most-
// recently-updated. Best-effort: a daemon failure returns nil so the rest
// of the rail still renders.
func (s *Server) computeIndexHotEpics(ctx context.Context, limit int) []indexHotEpic {
	if limit <= 0 {
		limit = 8
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:            []string{"epic"},
		Statuses:         []string{"open"},
		Sort:             "-updated_at",
		ExcludeScheduled: true,
		Limit:            limit,
	})
	if err != nil {
		s.logger.Warn("listing hot epics for index", "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	out := make([]indexHotEpic, 0, limit)
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		out = append(out, indexHotEpic{
			ID:       b.ID,
			Title:    b.Title,
			Priority: b.Priority,
			Project:  projectFromLabels(b.Labels),
			Ago:      formatAgo(b.UpdatedAt),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// computeIndexHot returns up to limit highest-priority open beads with no
// open dependencies (the ready queue). Restricted to issue-style actionable
// types (task/bug/epic/chore/feature/rich-report) — meta types like session,
// trigger, agent, and decision are not "work you can start" and would
// otherwise dominate the panel.
//
// Sort is "priority,-updated_at": within a priority tier, the daemon's
// SQL ORDER BY priority ASC without a tiebreaker fell back to insertion
// order, which surfaced incredibly old beads (ka-0000008099). The
// secondary -updated_at clause bubbles freshly-touched beads to the top
// of each priority bucket.
func (s *Server) computeIndexHot(ctx context.Context, limit int) []indexHotBead {
	if limit <= 0 {
		limit = 8
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:            issueMacroTypes,
		Statuses:         []string{"open"},
		Sort:             "priority,-updated_at",
		NoOpenDeps:       true,
		ExcludeScheduled: true,
		Limit:            limit,
	})
	if err != nil {
		s.logger.Warn("listing hot ready beads for index", "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	out := make([]indexHotBead, 0, limit)
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		out = append(out, indexHotBead{
			ID:       b.ID,
			Title:    b.Title,
			Type:     b.Type,
			Priority: b.Priority,
			Project:  projectFromLabels(b.Labels),
			Ago:      formatAgo(b.UpdatedAt),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// computeIndexLiveAgents returns up to limit *alive* agent beads sorted by
// most-recently-updated. "Alive" matches /agents semantics: status open or
// in_progress AND agent_state in {working, spawning, yielding}. The previous
// implementation queried status=in_progress only, which missed the common
// status=open + agent_state=working case and produced "No live agents." while
// /agents reported six alive on the same page load (ka-0000000513).
//
// Best-effort: a daemon failure returns nil.
//
// viewerEmail is the authenticated viewer's email (empty when anonymous); it
// only drives the is-viewer-self skin on the operated-by chip and is otherwise
// inert. The per-agent event stream is joined from exactly TWO batched calls
// (recentSquawksByAgent + recentCommentsByAgent) — never a per-agent N+1.
func (s *Server) computeIndexLiveAgents(ctx context.Context, limit int, viewerEmail string) []indexLiveAgent {
	if limit <= 0 {
		limit = 8
	}
	// Over-fetch so client-side agent_state filtering doesn't starve the panel
	// when idle/stuck agents dominate the active list.
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Sort:     "-updated_at",
		Limit:    limit * 5,
	})
	if err != nil {
		s.logger.Warn("listing live agents for index", "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	now := time.Now()
	// First pass: select the alive agents (capped at limit). We hold the bead
	// pointer so the second pass can render the operated-by chip and join the
	// squawk preview by agent name — no extra per-agent round-trips.
	picked := make([]*beadsapi.BeadDetail, 0, limit)
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		state := ""
		if b.Fields != nil {
			state = b.Fields["agent_state"]
		}
		if !isAgentStateIn(state, agentStateAlive) {
			continue
		}
		picked = append(picked, b)
		if len(picked) >= limit {
			break
		}
	}
	if len(picked) == 0 {
		return nil
	}

	// Two batched, O(1)-round-trip fetches feed the per-agent event stream,
	// each joined in-memory by agent name — never a per-agent N+1:
	//   - squawks: the one type=message/label=squawk list, newest N per agent.
	//   - comments: ONE durable cross-bead ListGlobalEvents page, filtered to
	//     comment.added and keyed by comment author.
	// Best-effort: a nil/empty map just means rows render with fewer events.
	squawks := s.recentSquawksByAgent(ctx)
	comments := s.recentCommentsByAgent(ctx)

	out := make([]indexLiveAgent, 0, len(picked))
	for _, b := range picked {
		taskID := ""
		taskTitle := ""
		role := ""
		mrURL := ""
		decisionID := ""
		agentName := ""
		podPhase := ""
		if b.Fields != nil {
			taskID = b.Fields["task_id"]
			taskTitle = b.Fields["task_title"]
			role = b.Fields["role"]
			mrURL = b.Fields["mr_url"]
			decisionID = b.Fields["decision_bead_id"]
			agentName = strings.TrimSpace(b.Fields["agent"])
			podPhase = strings.TrimSpace(b.Fields["pod_phase"])
		}
		row := indexLiveAgent{
			ID:             b.ID,
			Title:          b.Title,
			Role:           role,
			Project:        projectFromLabels(b.Labels),
			TaskID:         taskID,
			TaskTitle:      taskTitle,
			AgentState:     b.Fields["agent_state"],
			OperatedBy:     indexAgentOperator(b),
			OperatedByChip: indexAgentOperatorChip(b, s.basePath, viewerEmail),
			Uptime:         fmtTimeAgo(b.CreatedAt, now),
			Ago:            formatAgo(b.UpdatedAt),
			Stale:          isAgentStale(b, now),
			MRURL:          mrURL,
			DecisionID:     decisionID,
			PodPhase:       podPhase,
		}
		if agentName != "" {
			row.Events = buildAgentEventStream(squawks[agentName], comments[agentName], now)
		}
		out = append(out, row)
	}

	// The Home card headlines each row by its TASK title, not the bare ka-id
	// (ka-0000004222). Agent beads almost never carry fields.task_title, so
	// batch-resolve the real title for every distinct TaskID still missing one
	// in ONE BatchGetBeads round-trip (sharded internally at
	// BatchGetBeadsMaxIDs) — never a per-agent N+1. Unresolved ids keep the
	// id as a graceful fallback in the template.
	resolveLiveAgentTaskTitles(ctx, s, out)
	return out
}

// resolveLiveAgentTaskTitles hydrates row.TaskTitle for any live-agent row that
// has a TaskID but no title yet, using a single batched bead fetch. Best-effort:
// a daemon error or a missing task simply leaves TaskTitle empty, so the
// template falls back to rendering the bare TaskID.
func resolveLiveAgentTaskTitles(ctx context.Context, s *Server, rows []indexLiveAgent) {
	// Collect distinct TaskIDs that still need a title. A set dedupes the
	// common case of several agents claiming the same epic/task.
	need := make(map[string]struct{})
	for i := range rows {
		if rows[i].TaskID != "" && rows[i].TaskTitle == "" {
			need[rows[i].TaskID] = struct{}{}
		}
	}
	if len(need) == 0 {
		return
	}
	ids := make([]string, 0, len(need))
	for id := range need {
		ids = append(ids, id)
	}
	tasksByID, err := s.daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		s.logger.Warn("batch get live-agent task titles for index", "error", err)
		return
	}
	for i := range rows {
		if rows[i].TaskID == "" || rows[i].TaskTitle != "" {
			continue
		}
		if t, ok := tasksByID[rows[i].TaskID]; ok && t != nil && t.Title != "" {
			rows[i].TaskTitle = t.Title
		}
	}
}

// recentSquawksByAgent returns a map from agent name (squawk fields.source_agent)
// to that agent's most-recent squawks (newest-first, capped at
// eventStreamRowsPerAgent, each stamped with its created_at). It issues ONE
// batched list call for recent squawk beads (type=message, label=squawk,
// newest-first) — never a per-agent fetch — so the Home rail's event stream
// stays O(1) round-trips regardless of fleet size (ka-0000003997,
// ka-0000003393). Best-effort: a daemon error returns a nil map and the rail
// renders without squawk rows.
func (s *Server) recentSquawksByAgent(ctx context.Context) map[string][]stampedSquawk {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"message"},
		Labels:   []string{"squawk"},
		Statuses: []string{"open", "in_progress", "closed"},
		Sort:     "-created_at",
		Limit:    squawkLookbackLimit,
	})
	if err != nil {
		s.logger.Warn("listing squawks for index live-agents rail", "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	out := make(map[string][]stampedSquawk, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil || b.Fields == nil {
			continue
		}
		agent := strings.TrimSpace(b.Fields["source_agent"])
		if agent == "" {
			continue
		}
		// Beads arrive newest-first; keep only the newest few per agent so a
		// chatty agent can't crowd out others' rows in the shared scan.
		if len(out[agent]) >= eventStreamRowsPerAgent {
			continue
		}
		text := strings.TrimSpace(b.Fields["text"])
		if text == "" {
			text = strings.TrimSpace(b.Title)
		}
		if text == "" {
			continue
		}
		out[agent] = append(out[agent], stampedSquawk{Text: text, At: b.CreatedAt})
	}
	return out
}

// recentCommentsByAgent returns a map from agent name (comment author) to that
// agent's most-recent bead comments (newest-first, capped at
// eventStreamRowsPerAgent, each stamped with its created_at). It surfaces
// comments from ONE durable cross-bead ListGlobalEvents page — the cheapest
// O(1) source (never a per-agent GetRecentEvents N+1) — filtered to the
// comment.added topic and keyed by comment author (ka-0000003393).
//
// Squawk-mirror comments (the copy `gb squawk` writes onto the agent's claimed
// bead, tagged with a `_squawk gd-… from …_` footer) are dropped: their text
// duplicates the squawk bead already surfaced by recentSquawksByAgent, so
// keeping them would render the same line twice per squawk. Best-effort: a
// daemon error returns a nil map and the rail renders without comment rows.
func (s *Server) recentCommentsByAgent(ctx context.Context) map[string][]stampedSquawk {
	now := time.Now()
	resp, err := s.daemon.ListGlobalEvents(ctx, beadsapi.ListGlobalEventsRequest{
		Since: now.Add(-commentEventsLookbackWindow),
		Until: now,
		Limit: commentEventsLimit,
		Desc:  true,
	})
	if err != nil {
		s.logger.Warn("listing comment events for index live-agents rail", "error", err)
		return nil
	}
	if resp == nil {
		return nil
	}
	out := make(map[string][]stampedSquawk)
	for _, ev := range resp.Events {
		if ev.Topic != "beads.comment.added" {
			continue
		}
		author := strings.TrimSpace(commentAuthor(ev.Payload))
		if author == "" {
			continue
		}
		if len(out[author]) >= eventStreamRowsPerAgent {
			continue
		}
		text := strings.TrimSpace(commentText(ev.Payload))
		// Drop machine bookkeeping / mirror copies — the operator asked for
		// ACTUAL messages from the agent (squawks + comments), not automation
		// exhaust. Two exclusions: the squawk-mirror copy (its body is the
		// squawk already shown), and comments that lead with an HTML-comment
		// marker (e.g. "<!-- gardener-delivery-cooldown -->"), which are
		// cooldown/dedup sentinels a gardener writes to itself.
		if text == "" || strings.HasPrefix(text, "<!--") || squawkMirrorFooter.MatchString(text) {
			continue
		}
		out[author] = append(out[author], stampedSquawk{Text: text, At: ev.CreatedAt})
	}
	return out
}

// buildAgentEventStream merges an agent's recent squawks and comments into a
// single newest-first event list, capped at eventStreamRowsPerAgent. Each row
// is previewed to a single truncated line and stamped with a relative time.
// Squawks and comments arrive already newest-first per source; the merge
// re-sorts by timestamp so an interleaved comment/squawk history reads in true
// chronological order (ka-0000003393).
//
// Rows whose previewed text repeats one already kept are dropped: a squawk and
// its mirror-comment that slipped the footer filter, or a bot re-posting the
// same automated line (e.g. an aging nudge), otherwise render as two identical
// rows that read like a bug. De-dup is on the RENDERED preview so two long
// messages sharing only a truncated head still both survive only if their heads
// differ.
func buildAgentEventStream(squawks, comments []stampedSquawk, now time.Time) []agentEvent {
	if len(squawks) == 0 && len(comments) == 0 {
		return nil
	}
	events := make([]agentEvent, 0, len(squawks)+len(comments))
	for _, sq := range squawks {
		preview := squawkPreview(sq.Text)
		if preview == "" {
			continue
		}
		events = append(events, agentEvent{Kind: "squawk", Text: preview, Ago: fmtTimeAgo(sq.At, now), At: sq.At})
	}
	for _, c := range comments {
		preview := squawkPreview(c.Text)
		if preview == "" {
			continue
		}
		events = append(events, agentEvent{Kind: "comment", Text: preview, Ago: fmtTimeAgo(c.At, now), At: c.At})
	}
	// Newest-first; a stable sort keeps same-timestamp ties in insertion order
	// (squawks before comments) rather than shuffling them per render.
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.After(events[j].At) })
	deduped := events[:0]
	seen := make(map[string]struct{}, len(events))
	for _, ev := range events {
		if _, dup := seen[ev.Text]; dup {
			continue
		}
		seen[ev.Text] = struct{}{}
		deduped = append(deduped, ev)
		if len(deduped) >= eventStreamRowsPerAgent {
			break
		}
	}
	return deduped
}

// squawkPreview collapses a squawk's text to a single line and truncates it to
// squawkPreviewMaxLen runes at a word boundary, appending an ellipsis when
// clipped. Rune-aware so a multi-byte glyph (em-dash, emoji) is never split.
func squawkPreview(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= squawkPreviewMaxLen {
		return text
	}
	head := string(runes[:squawkPreviewMaxLen])
	if cut := strings.LastIndexAny(head, " "); cut > 0 {
		head = head[:cut]
	}
	return strings.TrimRight(head, " ") + "…"
}

// indexAgentOperatorChip renders the agent's operated_by as a compact initials
// chip (reusing operator_chip.go / operator_initials.go), so the Home rail
// shows a glanceable monogram (e.g. "MB") rather than a raw "slack:U…" handle
// (ka-0000003997). It mirrors indexAgentOperator's source-precedence
// (top-level operated_by → fields["operated_by"]) and prefers the
// daemon-resolved display name. Returns "" when no operator is attributable.
func indexAgentOperatorChip(b *beadsapi.BeadDetail, basePath, viewerEmail string) template.HTML {
	if b == nil {
		return ""
	}
	rawID := strings.TrimSpace(b.OperatedBy)
	if rawID == "" && b.Fields != nil {
		rawID = strings.TrimSpace(b.Fields["operated_by"])
	}
	display := strings.TrimSpace(b.OperatedByDisplay)
	if rawID == "" && display == "" {
		return ""
	}
	// resolveOperatorChip treats an empty operated_by as scheduler/agent/empty;
	// when only a resolved display is present (no raw id), fall back to it so
	// the chip still attributes a human operator.
	if rawID == "" {
		rawID = display
	}
	chip := resolveOperatorChip(rawID, display, b.OperatedByEmail, b.OperatedBySlug, b.CreatedBy, basePath, viewerEmail)
	return renderOperatorChipCompact(chip)
}

// indexAgentOperator returns a short, human-readable operator label for an
// agent bead's Live agents row. It prefers the daemon-resolved display name,
// then the top-level operated_by, then fields["operated_by"], and strips a
// leading provider prefix (e.g. "slack:U…" → "U…") so the rail shows a compact
// handle rather than a raw id. Returns "" when no operator is attributable.
func indexAgentOperator(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	cand := b.OperatedByDisplay
	if cand == "" {
		cand = b.OperatedBy
	}
	if cand == "" && b.Fields != nil {
		cand = b.Fields["operated_by"]
	}
	if i := strings.Index(cand, ":"); i >= 0 && i < len(cand)-1 {
		cand = cand[i+1:]
	}
	return cand
}

// computeIndexOpenDecisions returns up to limit open decision beads sorted
// by most-recently-updated. Best-effort: a daemon failure returns nil.
func (s *Server) computeIndexOpenDecisions(ctx context.Context, limit int) []indexOpenDecision {
	if limit <= 0 {
		limit = 8
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Sort:     "-updated_at",
		Limit:    limit,
	})
	if err != nil {
		s.logger.Warn("listing open decisions for index", "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	out := make([]indexOpenDecision, 0, limit)
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		out = append(out, indexOpenDecision{
			ID:      b.ID,
			Title:   b.Title,
			Project: projectFromLabels(b.Labels),
			Ago:     formatAgo(b.UpdatedAt),
		})
	}
	return out
}
