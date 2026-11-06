package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/cmd/beads-viewer/viewer"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// inboxListLimit caps the number of intake beads pulled for the index. The
// inbox-primary view's predicate is small in the median (~24 intakes per
// operator today, design doc §1.2.1). 500 matches /active.
const inboxListLimit = 500

// inboxAgentsActiveLimit / inboxAgentsRecentLimit / inboxAgentsRecentWindow
// bound the Agents roster on /inbox. Active is open+in_progress agents
// scoped to the viewer; recent is the closed-agents tail filtered to a
// time window so the operator's "what just happened on my behalf" view
// stays bounded.
const (
	inboxAgentsActiveLimit  = 50
	inboxAgentsRecentLimit  = 20
	inboxAgentsRecentWindow = 24 * time.Hour
)

// inboxOpenEpicsLimit caps the "Your open epics" section in the right-pane.
// At realistic load the per-operator open-epic set is small (single digits to
// low double digits); 50 leaves headroom without flooding the page.
const inboxOpenEpicsLimit = 50

// inboxDescPreviewLimit is the rune budget for left-rail row descriptions.
const inboxDescPreviewLimit = 140

// inboxDecisionsLimit caps the number of pending decisions surfaced in the
// inbox right-pane Decisions section. Pending-decision counts are typically
// in the single digits; 25 keeps the right pane compact even on busy days
// while leaving room before the truncation banner kicks in.
const inboxDecisionsLimit = 25

// inboxScopes lists the chip variants exposed by the index. M3 (ka-0000003587)
// retired the disabled `team` / `on_call` / `raise_only` chips per design doc
// §8 — chip behaviours folded into person chips / `/fleet/<bridge>` indexes.
var inboxScopes = []string{"mine"}

// inboxRowView is one row in the left-rail index.
type inboxRowView struct {
	ID                string
	Title             string
	Type              string
	Priority          int
	OperatedBy        string
	OperatedByDisplay string
	DescPreview       string
	Ago               string
	Flagged           bool   // bead carries `flag:operator-blocking`
	Source            string // first `source:*` label, e.g. "slack"
	InboxWorthy       bool   // bead carries `flag:inbox-worthy`
	Pinned            bool   // bead carries `flag:inbox-pinned`
	// Forwarded marks rows surfaced via the bead_followers UNION
	// (ka-0000007204) — the right side of the inbox-query OR. Forwarded
	// rows render at the top of the rail with ForwardOrder ascending
	// (0 = most-recently-forwarded), regardless of priority/category.
	Forwarded    bool
	ForwardOrder int // -1 when not forwarded; 0..N for followed beads in DESC follow order.
	// MRState / MRURL surface the linked merge-request bead's state when the
	// row's bead is type=merge-request (own fields) or a task bead with a
	// `produces` edge to one (resolved via inboxMRResolver). Empty when no
	// linked MR — the chip suppresses itself (ka-0000007315).
	MRState string
	MRURL   string
}

// inboxOrphanRowView is the synthetic pseudo-intake row appended below real
// intakes per design §6.6.1. Count==0 collapses the row entirely.
type inboxOrphanRowView struct {
	Scope string
	Title string
	Count int
	Href  string
}

// inboxOpenEpicRowView is one row in the right-pane "Your open epics" section.
// Each row links to the bead's constellation drill (/inbox/{id}) so the
// operator can fan into the descendant subgraph in the same layout.
type inboxOpenEpicRowView struct {
	ID       string
	Title    string
	Status   string
	Priority int
	Href     string
}

// handleInbox renders /inbox — the Gmail-density unified activity stream.
//
// Query: §1.2.1 — `category='intake' AND operated_by_id = :viewer_operator_id`
// (chip=mine), unioned with `flag:inbox-worthy` and the bead_followers right
// side (ka-0000007204). Other chips override the operated_by_id predicate;
// `team` / `on_call` / `raise_only` are disabled in v1 pending operator/team
// and on-call primitives (§4.3.1, §4.3.3). Empty resolver or unprovisioned
// viewer renders Mode A blocking banner (§4.6.1).
//
// Layout (ka-0000000894): a single full-width column interleaves intakes,
// pending decisions, agents (active + recent), open epics, and forwarded
// beads in activity-timestamp DESC order with pinned overrides at the top.
// `?kind=` filters the stream client-side via a chip strip below the scope
// chips.
func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		scope = "mine"
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if !isValidInboxKind(kind) {
		kind = ""
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))

	email := oidcauth.EmailFromContext(r.Context())

	// §9 telemetry — every /inbox view contributes to the pre-M3 baseline
	// (bounce rate = inbox views vs. follow-on bead-show clicks scraped from
	// the same log stream). Must land BEFORE the M3 308 redirect lands
	// (ka-0000003587) so post-cutover ratios have something to diff against.
	// Captured via defer so early-return branches (mode_a banner, errors)
	// still emit a record.
	var teleStreamTotal int
	var teleMineEmpty bool
	defer func() {
		emitTelemetry(s.logger, telemetryEventInboxView,
			"viewer_email", trimEmail(email),
			"scope", scope,
			"kind", kind,
			"project", project,
			"stream_total", teleStreamTotal,
			"mine_empty", teleMineEmpty,
			"referer_path", refererPath(r),
		)
	}()
	data := map[string]any{
		"ViewerEmail":         email,
		"Scope":               scope,
		"Kind":                kind,
		"Project":             project,
		"Chips":               buildInboxChips(s.basePath, scope),
		"KindChips":           []inboxKindChipView{},
		"KindChipsEmpty":      []inboxKindChipView{},
		"ProjectOptions":      []inboxProjectOption{},
		"Rows":                []inboxRowView{},
		"Stream":              []inboxStreamRow{},
		"StreamTotal":         0,
		"Total":               0,
		"OrphanRow":           inboxOrphanRowView{},
		"BannerMode":          "",
		"InboxBeadID":         "",
		"InboxBeadActive":     false,
		"InboxBeadType":       "",
		"Constellation":       (*constellationView)(nil),
		"AgentsActive":        []agentRowView{},
		"AgentsRecent":        []agentRowView{},
		"PendingDecisions":    []decisionRowView{},
		"DecisionsTruncated":  false,
		"DecisionsLimit":      inboxDecisionsLimit,
		"OpenEpics":           []inboxOpenEpicRowView{},
		"AdjacentBuckets":     inboxAdjacentBucketsView{},
		"RecentMentions":      []inboxMentionRow{},
		"RecentMentionsCount": 0,
		"MineEmpty":           false,
		// ka-0000002514 (M2 §5): "moved to your homepage" pointer banner.
		"ShowInboxHomepageBanner": s.shouldRenderInboxHomepageBanner(r),
		"GardenHref":              s.gardenQueuesBanner(r, gardenQueueTarget(gardenhub.QueueInbox)),
	}

	opID, banner, err := s.resolveInboxViewerOperator(r.Context(), email)
	if err != nil {
		s.logger.Error("inbox: resolver error", "error", err)
		data["BannerMode"] = "error"
		data["BannerError"] = "Failed to resolve viewer identity."
		s.render(w, r, "inbox.html", data)
		return
	}
	data["BannerMode"] = banner

	if banner == "mode_a" {
		// No operators row → cannot scope; render the page with the banner only.
		s.render(w, r, "inbox.html", data)
		return
	}

	intake, forwardOrder, decisions, decisionTrunc, decisionBeads,
		activeAgents, activeAgentBeads, recentAgents, recentAgentBeads,
		epics, epicBeads, intakeTotal, intakeErr := s.fetchInboxStreamSources(r.Context(), opID)
	if intakeErr != nil {
		s.logger.Error("listing inbox beads", "error", intakeErr)
		data["Error"] = "Failed to load inbox."
		s.render(w, r, "inbox.html", data)
		return
	}

	data["PendingDecisions"] = decisions
	data["DecisionsTruncated"] = decisionTrunc
	data["AgentsActive"] = activeAgents
	data["AgentsRecent"] = recentAgents
	data["OpenEpics"] = epics

	// MR-state chip (ka-0000007315): one batched lookup per request for the
	// intake rail's linked merge-requests. Default-on; ?mr_chips=0 disables
	// the lookup entirely (escape hatch if a future perf regression demands
	// it without a redeploy).
	mr := newInboxMRResolver()
	if r.URL.Query().Get("mr_chips") != "0" && s.daemon != nil {
		mr.Prewarm(r.Context(), s.daemon, intake)
	}

	// Legacy intake-row projection — kept so any other consumers of the
	// inbox payload (HTMX swaps, /inbox/{id} drill) still see the rail
	// data; the densified template renders from .Stream.
	rows := make([]inboxRowView, 0, len(intake))
	for _, b := range intake {
		rows = append(rows, inboxRowFromBeadWithMR(b, mr))
	}
	applyForwardedFlags(rows, forwardOrder)
	sortInboxRows(rows)
	data["Rows"] = rows
	data["Total"] = len(rows)
	data["Truncated"] = intakeTotal > len(intake)
	data["Limit"] = inboxListLimit

	stream := buildInboxStream(
		intake, forwardOrder,
		decisions, decisionBeads,
		activeAgents, activeAgentBeads,
		recentAgents, recentAgentBeads,
		epics, epicBeads,
		mr,
		s.basePath, time.Now(),
	)
	if !isValidInboxProject(stream, project) {
		project = ""
	}
	data["Project"] = project
	data["ProjectOptions"] = buildInboxProjectOptions(stream, project)
	projectFiltered := filterStreamByProject(stream, project)
	kindChips := buildInboxKindChips(projectFiltered, s.basePath, scope, kind, project)
	data["KindChips"] = kindChips
	data["KindChipsEmpty"] = collapsedKindChips(kindChips)
	data["StreamTotal"] = len(projectFiltered)
	streamFiltered := filterStreamByKind(projectFiltered, kind)
	data["Stream"] = streamFiltered

	// ka-0000002575 / decision ka-0000003122 option `adjacent-buckets`: when
	// the rendered stream is at or below the threshold (often the post-Slack
	// quiet state), inline-render previews of pending Decisions and
	// Escalations so the operator still sees signal. Mentions remain a TODO
	// pending the data-shape research bead.
	if len(streamFiltered) <= inboxAdjacentBucketsThreshold {
		data["AdjacentBuckets"] = s.buildInboxAdjacentBuckets(
			r.Context(),
			len(streamFiltered),
			decisions,
			len(decisions),
			time.Now(),
		)
	} else {
		data["AdjacentBuckets"] = inboxAdjacentBucketsView{}
	}

	// Recent-mentions section (ka-0000005641): RENDERING-only projection of
	// already-fetched data. Shows a dedicated section when the operator's
	// scope-mine intake is empty, a small badge otherwise.
	mentions := computeInboxRecentMentions(activeAgentBeads, recentAgentBeads, decisionBeads, s.basePath, time.Now())
	data["RecentMentions"] = mentions
	data["RecentMentionsCount"] = len(mentions)
	data["MineEmpty"] = scope == "mine" && len(streamFiltered) == 0

	// Orphan pseudo-row (§6.3, §6.6.1). Count is best-effort — a daemon error
	// degrades to "no row" without breaking the index.
	data["OrphanRow"] = s.computeInboxOrphanRow(r.Context(), scope, opID)

	// Prewarm agent-link lookups for every bead that surfaces on this page,
	// so template render fires one batched daemon call instead of N
	// per-row lookups (ka-0000000899).
	combined := make([]*beadsapi.BeadDetail, 0, len(intake)+len(decisionBeads)+len(activeAgentBeads)+len(recentAgentBeads)+len(epicBeads))
	combined = append(combined, intake...)
	combined = append(combined, decisionBeads...)
	combined = append(combined, activeAgentBeads...)
	combined = append(combined, recentAgentBeads...)
	combined = append(combined, epicBeads...)
	r = s.prewarmAgentLinksFromBeads(r, combined)
	s.render(w, r, "inbox.html", data)

	// Populate the §9 telemetry view-model fields the defer'd emit reads.
	teleStreamTotal, _ = data["StreamTotal"].(int)
	teleMineEmpty, _ = data["MineEmpty"].(bool)
}

// handleInboxBead renders /inbox/{id} — the index plus the right-pane
// constellation drilled to the chosen intake. The id MUST be an intake bead;
// non-intake ids fall back to the bare /inbox view with a banner.
func (s *Server) handleInboxBead(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		http.Redirect(w, r, s.basePath+"/inbox", http.StatusSeeOther)
		return
	}

	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		scope = "mine"
	}

	email := oidcauth.EmailFromContext(r.Context())
	data := map[string]any{
		"ViewerEmail":        email,
		"GardenHref":         s.gardenQueuesBanner(r, gardenQueueTarget(gardenhub.QueueInbox)),
		"Scope":              scope,
		"Kind":               "",
		"Chips":              buildInboxChips(s.basePath, scope),
		"KindChips":          []inboxKindChipView{},
		"KindChipsEmpty":     []inboxKindChipView{},
		"Project":            "",
		"ProjectOptions":     []inboxProjectOption{},
		"Rows":               []inboxRowView{},
		"Stream":             []inboxStreamRow{},
		"StreamTotal":        0,
		"Total":              0,
		"OrphanRow":          inboxOrphanRowView{},
		"BannerMode":         "",
		"InboxBeadID":        beadID,
		"InboxBeadActive":    true,
		"InboxBeadType":      "",
		"Constellation":      (*constellationView)(nil),
		"AgentsActive":       []agentRowView{},
		"AgentsRecent":       []agentRowView{},
		"PendingDecisions":   []decisionRowView{},
		"DecisionsTruncated": false,
		"DecisionsLimit":     inboxDecisionsLimit,
		"OpenEpics":          []inboxOpenEpicRowView{},
	}

	opID, banner, err := s.resolveInboxViewerOperator(r.Context(), email)
	if err != nil {
		s.logger.Error("inbox: resolver error", "error", err)
		data["BannerMode"] = "error"
		data["BannerError"] = "Failed to resolve viewer identity."
		s.render(w, r, "inbox.html", data)
		return
	}
	data["BannerMode"] = banner

	if banner == "mode_a" {
		s.render(w, r, "inbox.html", data)
		return
	}

	decisions, truncated := s.loadInboxPendingDecisions(r.Context())
	data["PendingDecisions"] = decisions
	data["DecisionsTruncated"] = truncated

	// Same index query as /inbox so the left rail stays populated when the
	// right pane drills in.
	if beads, forwardOrder, _, err := s.listInboxBeadsForViewer(r.Context(), opID); err == nil {
		mr := newInboxMRResolver()
		if r.URL.Query().Get("mr_chips") != "0" && s.daemon != nil {
			mr.Prewarm(r.Context(), s.daemon, beads)
		}
		rows := make([]inboxRowView, 0, len(beads))
		for _, b := range beads {
			rows = append(rows, inboxRowFromBeadWithMR(b, mr))
		}
		applyForwardedFlags(rows, forwardOrder)
		sortInboxRows(rows)
		data["Rows"] = rows
		data["Total"] = len(rows)
	} else {
		s.logger.Warn("listing inbox beads (drill)", "error", err)
	}

	// Right pane: hydrate the intake + immediate dependencies and pass to the
	// inbox-flavored constellation builder. Errors degrade to "no constellation"
	// without breaking the index.
	bead, err := s.daemon.GetBead(r.Context(), beadID)
	if err != nil || bead == nil {
		data["DrillError"] = "Intake not found."
		s.render(w, r, "inbox.html", data)
		return
	}
	deps, _ := s.daemon.GetDependencies(r.Context(), beadID)
	revDeps, _ := s.daemon.GetReverseDependencies(r.Context(), beadID)
	view := s.buildInboxConstellation(r.Context(), bead, deps, revDeps)
	data["Constellation"] = view
	data["InboxBeadType"] = bead.Type

	if banner != "mode_a" {
		data["OrphanRow"] = s.computeInboxOrphanRow(r.Context(), scope, opID)
		active, recent := s.computeInboxAgents(r.Context(), opID)
		data["AgentsActive"] = active
		data["AgentsRecent"] = recent
		data["OpenEpics"] = s.computeInboxOpenEpics(r.Context(), opID)
	}

	// Prewarm agent-link lookups for the drilled bead surface (the
	// intake + its constellation neighbours) so the page makes one
	// batched daemon call instead of N (ka-0000000899).
	r = s.prewarmAgentLinksFromBeads(r, []*beadsapi.BeadDetail{bead})
	s.render(w, r, "inbox.html", data)
}

// handleInboxOrphans renders /inbox/orphans/{scope} — the orphan pseudo-intake
// drill from §6.6.2. Renders a flat constellation of action beads whose
// parent-child chain reaches no intake ancestor (§6.2).
func (s *Server) handleInboxOrphans(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	if !isValidInboxScope(scope) {
		http.NotFound(w, r)
		return
	}

	email := oidcauth.EmailFromContext(r.Context())
	data := map[string]any{
		"ViewerEmail":        email,
		"Scope":              scope,
		"Kind":               "",
		"Chips":              buildInboxChips(s.basePath, scope),
		"KindChips":          []inboxKindChipView{},
		"KindChipsEmpty":     []inboxKindChipView{},
		"Project":            "",
		"ProjectOptions":     []inboxProjectOption{},
		"Rows":               []inboxRowView{},
		"Stream":             []inboxStreamRow{},
		"StreamTotal":        0,
		"Total":              0,
		"OrphanRow":          inboxOrphanRowView{},
		"BannerMode":         "",
		"InboxBeadID":        "inbox:orphans:" + scope,
		"InboxBeadActive":    true,
		"InboxOrphansActive": true,
		"Constellation":      (*constellationView)(nil),
		"OrphanBeads":        []inboxRowView{},
		"AgentsActive":       []agentRowView{},
		"AgentsRecent":       []agentRowView{},
		"PendingDecisions":   []decisionRowView{},
		"DecisionsTruncated": false,
		"DecisionsLimit":     inboxDecisionsLimit,
		"OpenEpics":          []inboxOpenEpicRowView{},
	}

	opID, banner, err := s.resolveInboxViewerOperator(r.Context(), email)
	if err != nil {
		s.logger.Error("inbox/orphans: resolver error", "error", err)
		data["BannerMode"] = "error"
		s.render(w, r, "inbox.html", data)
		return
	}
	data["BannerMode"] = banner
	if banner == "mode_a" {
		s.render(w, r, "inbox.html", data)
		return
	}

	decisions, truncated := s.loadInboxPendingDecisions(r.Context())
	data["PendingDecisions"] = decisions
	data["DecisionsTruncated"] = truncated

	// Index query (left rail).
	if beads, forwardOrder, _, err := s.listInboxBeadsForViewer(r.Context(), opID); err == nil {
		mr := newInboxMRResolver()
		if r.URL.Query().Get("mr_chips") != "0" && s.daemon != nil {
			mr.Prewarm(r.Context(), s.daemon, beads)
		}
		rows := make([]inboxRowView, 0, len(beads))
		for _, b := range beads {
			rows = append(rows, inboxRowFromBeadWithMR(b, mr))
		}
		applyForwardedFlags(rows, forwardOrder)
		sortInboxRows(rows)
		data["Rows"] = rows
		data["Total"] = len(rows)
	}

	// Orphan set (right pane). v1 approximation: action beads (category=action)
	// scoped to the viewer with no parent-child ancestor that is category=intake.
	// Daemon does not yet expose the recursive ancestor query; the inbox falls
	// back to "list category=action for the viewer, then drop those with an
	// intake parent in deps". This is OQ-9 follow-up work; the predicate's
	// precise SQL lives in design §6.5.
	orphans := s.computeInboxOrphans(r.Context(), opID)
	data["OrphanBeads"] = orphans
	data["OrphanRow"] = inboxOrphanRowView{
		Scope: scope,
		Title: orphanRowTitle(scope),
		Count: len(orphans),
		Href:  s.basePath + "/inbox/orphans/" + scope,
	}

	active, recent := s.computeInboxAgents(r.Context(), opID)
	data["AgentsActive"] = active
	data["AgentsRecent"] = recent
	data["OpenEpics"] = s.computeInboxOpenEpics(r.Context(), opID)

	s.render(w, r, "inbox.html", data)
}

// handleInboxSubgraphJSON is the JSON sibling of /inbox/{id} for the (future)
// htmx-only swap path. v1 returns the same constellation view as JSON; subtask 7
// will own the wire format. Predicates G3 just need the route to exist.
func (s *Server) handleInboxSubgraphJSON(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		http.NotFound(w, r)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), beadID)
	if err != nil || bead == nil {
		http.NotFound(w, r)
		return
	}
	deps, _ := s.daemon.GetDependencies(r.Context(), beadID)
	revDeps, _ := s.daemon.GetReverseDependencies(r.Context(), beadID)
	view := s.buildInboxConstellation(r.Context(), bead, deps, revDeps)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(view); err != nil {
		s.logger.Error("inbox: subgraph json encode", "error", err)
	}
}

// handleOperatorInboxRedirect 308-redirects /operator-inbox{/...} → /inbox{/...}.
// 308 (Permanent Redirect) preserves the request method and signals to caches
// that the move is canonical (predicate G3.b).
func (s *Server) handleOperatorInboxRedirect(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	target := s.basePath + "/inbox"
	if rest != "" {
		target += "/" + rest
	}
	if q := r.URL.RawQuery; q != "" {
		target += "?" + q
	}
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}

// handleInboxFolded308 308-redirects /inbox → /u/me?view=dashboard.
// M3 (ka-0000003587) folds the legacy /inbox route into the per-user homepage
// per design doc §8. The legacy dashboard remains available at
// /u/me?view=constellation (handled by handleUserHome → handleInbox dispatch).
func (s *Server) handleInboxFolded308(w http.ResponseWriter, r *http.Request) {
	target := s.basePath + "/u/me?view=dashboard"
	if extra := r.URL.RawQuery; extra != "" {
		target += "&" + extra
	}
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}

// handleSetParent handles POST /b/{id}/parent — the new endpoint design §5.5.3
// names. v1 implements path 5.5.3-A: the viewer rewrites the typed-mention
// `parent(ka-X)` server-side via the existing description endpoint. The
// daemon-side endpoint (5.5.3-B) is filed as follow-up work — see plan §1.6.
func (s *Server) handleSetParent(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	parentID := strings.TrimSpace(r.PostForm.Get("parent_id"))

	// Reject obvious cycles: parent_id == bead_id.
	if parentID != "" && parentID == beadID {
		http.Error(w, "parent_id == bead_id (cycle)", http.StatusBadRequest)
		return
	}

	bead, err := s.daemon.GetBead(r.Context(), beadID)
	if err != nil || bead == nil {
		http.Error(w, "bead not found", http.StatusNotFound)
		return
	}
	if parentID != "" {
		if pb, err := s.daemon.GetBead(r.Context(), parentID); err != nil || pb == nil {
			http.Error(w, "parent not found", http.StatusNotFound)
			return
		}
	}

	newDesc, ok := rewriteParentMention(bead.Description, parentID)
	if !ok {
		http.Error(w, "could not safely rewrite parent mention; edit description manually", http.StatusConflict)
		return
	}
	if err := s.daemon.UpdateBead(r.Context(), beadID, beadsapi.UpdateBeadRequest{
		Description: &newDesc,
	}); err != nil {
		s.logger.Error("inbox: set parent", "error", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveInboxViewerOperator converts the viewer's email to a (operator_id,
// banner) pair. banner is one of:
//
//   - ""        — fully provisioned, no banner
//   - "mode_a"  — no operators row (blocking, design §4.6.1)
//   - "mode_b"  — operators row exists but a platform alias is missing (informational)
//
// When the resolver is nil (test or pre-wire), returns (0, "mode_a", nil). When
// the email is empty (anonymous), returns (0, "mode_a", nil).
func (s *Server) resolveInboxViewerOperator(ctx context.Context, email string) (int64, string, error) {
	if strings.TrimSpace(email) == "" {
		return 0, "mode_a", nil
	}
	if s.resolver == nil {
		return 0, "mode_a", nil
	}
	id, found, err := s.resolver.OperatorID(ctx)
	if err != nil {
		// Sentinel-kind alias points at a system/fleet operator. Treat as
		// mode_a so the viewer cannot scope to a sentinel id; the banner copy
		// covers the recovery path.
		if errors.Is(err, viewer.ErrSentinelOperator) {
			return 0, "mode_a", nil
		}
		return 0, "", err
	}
	if !found {
		return 0, "mode_a", nil
	}
	// Mode B detection (missing-platform alias) is OQ-9 follow-up work — it
	// requires intersecting source-label seen-set with the alias kinds on the
	// viewer's operators row. v1 ships without it.
	return id, "", nil
}

// loadInboxPendingDecisions returns the open decision beads to render in the
// inbox right-pane Decisions section. Newest-first, capped at
// inboxDecisionsLimit. Errors degrade to an empty slice + a non-nil truncated
// flag set via the second return so the section still renders the empty state.
//
// We deliberately do not scope by viewer operator: decisions don't carry
// operated_by today, and operators want to see the full pending queue.
func (s *Server) loadInboxPendingDecisions(ctx context.Context) ([]decisionRowView, bool) {
	rows, truncated, _ := s.loadInboxPendingDecisionsWithBeads(ctx)
	return rows, truncated
}

// loadInboxPendingDecisionsWithBeads is the same as loadInboxPendingDecisions
// but also returns the underlying *BeadDetail slice in row order so the
// stream builder can read activity timestamps without a re-query.
func (s *Server) loadInboxPendingDecisionsWithBeads(ctx context.Context) ([]decisionRowView, bool, []*beadsapi.BeadDetail) {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Sort:     "-created_at",
		Limit:    inboxDecisionsLimit,
		NoTotal:  true, // only truncation is shown, as "N+" (gd-I2zntgOsCq)
	})
	if err != nil {
		s.logger.Warn("inbox: listing pending decisions", "error", err)
		return nil, false, nil
	}
	evidence := s.decisionEvidenceAll(ctx, res.Beads)
	now := time.Now()
	rows := make([]decisionRowView, 0, len(res.Beads))
	beads := make([]*beadsapi.BeadDetail, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		row := decisionRowView{
			ID:           b.ID,
			Title:        b.Title,
			Project:      projectFromLabels(b.Labels),
			Prompt:       snippet(b.Fields["prompt"], 240),
			RequestedBy:  b.Fields["requested_by"],
			Options:      parseDecisionOptions(b.Fields["options"]),
			Context:      s.decisionContextOf(b),
			Evidence:     evidence[b.ID],
			Ago:          fmtTimeAgo(b.CreatedAt, now),
			UpdatedAtRFC: rfcTime(b.UpdatedAt),
		}
		if row.RequestedBy == "" {
			row.RequestedBy = b.CreatedBy
		}
		rows = append(rows, row)
		beads = append(beads, b)
	}
	return rows, res.MayHaveMore(), beads
}

// computeInboxAgents returns (active, recent) agent rows scoped to opID — the
// "what's happening on my behalf" roster surfaced on /inbox. Active is
// open+in_progress agents; recent is closed agents whose UpdatedAt is within
// inboxAgentsRecentWindow. Both are best-effort: a daemon error degrades the
// roster to empty without breaking the page.
func (s *Server) computeInboxAgents(ctx context.Context, opID int64) ([]agentRowView, []agentRowView) {
	active, _, recent, _ := s.computeInboxAgentsWithBeads(ctx, opID)
	return active, recent
}

// computeInboxAgentsWithBeads is computeInboxAgents but also returns the
// raw *BeadDetail slices in row order so the stream builder can pick up
// activity timestamps without re-querying.
func (s *Server) computeInboxAgentsWithBeads(ctx context.Context, opID int64) ([]agentRowView, []*beadsapi.BeadDetail, []agentRowView, []*beadsapi.BeadDetail) {
	if opID <= 0 {
		return nil, nil, nil, nil
	}
	now := time.Now()

	active, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:        []string{"agent"},
		Statuses:     []string{"open", "in_progress"},
		OperatedByID: opID,
		Sort:         "updated_at",
		Limit:        inboxAgentsActiveLimit,
		NoTotal:      true, // the roster is never counted (gd-I2zntgOsCq)
	})
	if err != nil {
		s.logger.Warn("inbox: listing active agents", "error", err)
		active = &beadsapi.ListBeadsResult{}
	}
	recent, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:        []string{"agent"},
		Statuses:     []string{"closed"},
		OperatedByID: opID,
		Sort:         "updated_at",
		Limit:        inboxAgentsRecentLimit,
		// Never counted, and the page is always full: the count would read
		// every agent the operator ever ran (8.9k for the owner, gd-I2zntgOsCq).
		NoTotal: true,
	})
	if err != nil {
		s.logger.Warn("inbox: listing recent agents", "error", err)
		recent = &beadsapi.ListBeadsResult{}
	}

	activeRows := make([]agentRowView, 0, len(active.Beads))
	activeBeads := make([]*beadsapi.BeadDetail, 0, len(active.Beads))
	for _, b := range active.Beads {
		if b == nil {
			continue
		}
		activeRows = append(activeRows, s.agentRowFromBeadCtx(ctx, b, agentProjectName(b), now))
		activeBeads = append(activeBeads, b)
	}

	recentRows := make([]agentRowView, 0, len(recent.Beads))
	recentBeads := make([]*beadsapi.BeadDetail, 0, len(recent.Beads))
	cutoff := now.Add(-inboxAgentsRecentWindow)
	for _, b := range recent.Beads {
		if b == nil {
			continue
		}
		if !b.UpdatedAt.IsZero() && b.UpdatedAt.Before(cutoff) {
			continue
		}
		recentRows = append(recentRows, s.agentRowFromBeadCtx(ctx, b, agentProjectName(b), now))
		recentBeads = append(recentBeads, b)
	}
	return activeRows, activeBeads, recentRows, recentBeads
}

// computeInboxOpenEpics returns the viewer's open and in-progress epics — the
// "Your open epics" right-pane section. Scoped to opID via OperatedByID,
// matching the operator-resolution chain the rest of the inbox uses. Sorted
// by Priority asc, ID asc as the stable tiebreaker. Best-effort: a daemon
// error degrades the section to empty without breaking the page.
func (s *Server) computeInboxOpenEpics(ctx context.Context, opID int64) []inboxOpenEpicRowView {
	rows, _ := s.computeInboxOpenEpicsWithBeads(ctx, opID)
	return rows
}

// computeInboxOpenEpicsWithBeads is the same as computeInboxOpenEpics but
// also returns the raw *BeadDetail slice in row order. Used by the stream
// builder to surface activity timestamps and project labels.
func (s *Server) computeInboxOpenEpicsWithBeads(ctx context.Context, opID int64) ([]inboxOpenEpicRowView, []*beadsapi.BeadDetail) {
	if opID <= 0 {
		return nil, nil
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:            []string{"epic"},
		Statuses:         []string{"open", "in_progress"},
		OperatedByID:     opID,
		ExcludeScheduled: true,
		Limit:            inboxOpenEpicsLimit,
		NoTotal:          true, // the section is never counted (gd-I2zntgOsCq)
	})
	if err != nil {
		s.logger.Warn("inbox: listing open epics", "error", err)
		return nil, nil
	}
	if res == nil {
		return nil, nil
	}
	type indexed struct {
		row  inboxOpenEpicRowView
		bead *beadsapi.BeadDetail
	}
	pairs := make([]indexed, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		pairs = append(pairs, indexed{
			row: inboxOpenEpicRowView{
				ID:       b.ID,
				Title:    b.Title,
				Status:   b.Status,
				Priority: b.Priority,
				Href:     s.basePath + "/inbox/" + b.ID,
			},
			bead: b,
		})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].row.Priority != pairs[j].row.Priority {
			return pairs[i].row.Priority < pairs[j].row.Priority
		}
		return pairs[i].row.ID < pairs[j].row.ID
	})
	rows := make([]inboxOpenEpicRowView, 0, len(pairs))
	beads := make([]*beadsapi.BeadDetail, 0, len(pairs))
	for _, p := range pairs {
		rows = append(rows, p.row)
		beads = append(beads, p.bead)
	}
	return rows, beads
}

// computeInboxOrphanRow returns the synthetic pseudo-intake row for the given
// scope. Best-effort: errors collapse the row.
func (s *Server) computeInboxOrphanRow(ctx context.Context, scope string, opID int64) inboxOrphanRowView {
	if !isValidInboxScope(scope) {
		return inboxOrphanRowView{}
	}
	orphans := s.computeInboxOrphansFor(ctx, opID)
	if len(orphans) == 0 {
		return inboxOrphanRowView{}
	}
	return inboxOrphanRowView{
		Scope: scope,
		Title: orphanRowTitle(scope),
		Count: len(orphans),
		Href:  s.basePath + "/inbox/orphans/" + scope,
	}
}

// computeInboxOrphans returns orphan action beads visible to the inbox/orphans
// page. Same backend as computeInboxOrphanRow but returns row views for
// rendering.
func (s *Server) computeInboxOrphans(ctx context.Context, opID int64) []inboxRowView {
	beads := s.computeInboxOrphansFor(ctx, opID)
	out := make([]inboxRowView, 0, len(beads))
	for _, b := range beads {
		out = append(out, inboxRowFromBead(b))
	}
	return out
}

// computeInboxOrphansFor returns the raw orphan bead list for the operator.
// v1 approximation: list category=action open beads scoped to the viewer that
// do not have a parent-child dep at all (no parent → no intake ancestor by
// definition). Beads with a parent that is itself orphaned remain hidden in
// v1; the recursive walk lands when the daemon exposes the §6.5 query.
func (s *Server) computeInboxOrphansFor(ctx context.Context, opID int64) []*beadsapi.BeadDetail {
	q := beadsapi.ListBeadsQuery{
		Categories:       []string{"action"},
		Statuses:         []string{"open", "in_progress"},
		Limit:            inboxListLimit,
		ExcludeScheduled: true,
		NoTotal:          true, // orphans are counted from the rows (gd-I2zntgOsCq)
	}
	if opID > 0 {
		q.OperatedByID = opID
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, q)
	if err != nil || res == nil {
		return nil
	}
	candidates := make([]*beadsapi.BeadDetail, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b != nil {
			candidates = append(candidates, b)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	// Cheap orphan test: bead has no parent-child dep. The full recursive
	// ancestor query is OQ-9 follow-up work. Both `parent-child` and the
	// inverse-direction synonym `child-of` count as "has a parent" so a bead
	// linked via either storage shape is not flagged as an orphan.
	//
	// Fan the per-bead GetDependencies out under a bounded semaphore — with
	// up to inboxListLimit (500) candidates, the serial loop turned the
	// orphan rail into a 500-RTT chain on every /inbox render.
	const orphanLookupConcurrency = 16
	hasParent := make([]bool, len(candidates))
	sem := make(chan struct{}, orphanLookupConcurrency)
	var wg sync.WaitGroup
	for i := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			deps, derr := s.daemon.GetDependencies(ctx, candidates[i].ID)
			if derr != nil {
				// Treat lookup error as "has parent" so we don't
				// surface the bead as an orphan on a transient daemon
				// hiccup — same conservative behavior as the serial
				// `continue` clause used to exhibit.
				hasParent[i] = true
				return
			}
			for _, d := range deps {
				if isParentChildAlias(d.Type) {
					hasParent[i] = true
					return
				}
			}
		}(i)
	}
	wg.Wait()

	out := make([]*beadsapi.BeadDetail, 0, len(candidates))
	for i, b := range candidates {
		if !hasParent[i] {
			out = append(out, b)
		}
	}
	return out
}

func buildInboxChips(basePath, activeScope string) []filterChip {
	// M3 (ka-0000003587): only the `mine` chip remains. The disabled
	// `team` / `on_call` / `raise_only` chips were retired per design doc §8 —
	// their behaviours fold into person chips / `/fleet/<bridge>` indexes.
	return []filterChip{
		{
			Label:  "mine",
			Value:  "mine",
			Href:   basePath + "/inbox",
			Active: activeScope == "mine",
		},
	}
}

func inboxRowFromBead(b *beadsapi.BeadDetail) inboxRowView {
	return inboxRowFromBeadWithMR(b, nil)
}

// inboxRowFromBeadWithMR is the resolver-aware variant. Nil resolver
// leaves MRState/MRURL empty so callers that have not prewarmed (legacy
// paths, tests) keep their existing behaviour.
func inboxRowFromBeadWithMR(b *beadsapi.BeadDetail, mr *inboxMRResolver) inboxRowView {
	row := inboxRowView{
		ID:                b.ID,
		Title:             b.Title,
		Type:              b.Type,
		Priority:          b.Priority,
		OperatedBy:        b.OperatedBy,
		OperatedByDisplay: b.OperatedByDisplay,
		DescPreview:       snippet(b.Description, inboxDescPreviewLimit),
		ForwardOrder:      -1,
	}
	for _, l := range b.Labels {
		switch {
		case l == "flag:operator-blocking":
			row.Flagged = true
		case l == labelInboxWorthy:
			row.InboxWorthy = true
		case l == labelInboxPinned:
			row.Pinned = true
		case strings.HasPrefix(l, "source:"):
			if row.Source == "" {
				row.Source = strings.TrimPrefix(l, "source:")
			}
		}
	}
	if info := mr.Lookup(b.ID); info.State != "" {
		row.MRState = info.State
		row.MRURL = info.URL
	}
	return row
}

// listInboxBeadsForViewer returns the union of:
//
//   - category=intake scoped to the operator (the original left side)
//   - label=flag:inbox-worthy scoped to the operator
//   - bead_followers where operator_id = opID (ka-0000007204 — the
//     "forwarded to me" right side of the inbox-query OR)
//
// The daemon does not support OR across category/label predicates in a
// single query, so this issues parallel queries and dedupes by id. The
// returned forwardOrder map carries each followed bead's rank (0 = most
// recently forwarded) so the row builder can flag and sort them.
//
// Errors on the inbox-worthy query are logged upstream — callers should
// fall back to a partial rail rather than failing the whole page. Errors
// on the followers query degrade the right side to empty for the same
// reason; the operator still sees their intake rail.
func (s *Server) listInboxBeadsForViewer(ctx context.Context, opID int64) ([]*beadsapi.BeadDetail, map[string]int, int, error) {
	intakeQ := beadsapi.ListBeadsQuery{
		Categories:       []string{"intake"},
		Statuses:         []string{"open", "in_progress"},
		Limit:            inboxListLimit,
		ExcludeScheduled: true,
	}
	worthyQ := beadsapi.ListBeadsQuery{
		Labels:           []string{labelInboxWorthy},
		Statuses:         []string{"open", "in_progress"},
		Limit:            inboxListLimit,
		ExcludeScheduled: true,
	}
	if opID > 0 {
		intakeQ.OperatedByID = opID
		worthyQ.OperatedByID = opID
	}
	intakeRes, err := s.daemon.ListBeadsFiltered(ctx, intakeQ)
	if err != nil {
		return nil, nil, 0, err
	}
	seen := make(map[string]struct{}, len(intakeRes.Beads))
	out := make([]*beadsapi.BeadDetail, 0, len(intakeRes.Beads))
	for _, b := range intakeRes.Beads {
		if b == nil {
			continue
		}
		seen[b.ID] = struct{}{}
		out = append(out, b)
	}
	total := intakeRes.Total
	if worthyRes, werr := s.daemon.ListBeadsFiltered(ctx, worthyQ); werr == nil && worthyRes != nil {
		for _, b := range worthyRes.Beads {
			if b == nil {
				continue
			}
			if _, dup := seen[b.ID]; dup {
				continue
			}
			seen[b.ID] = struct{}{}
			out = append(out, b)
			total++
		}
	} else if werr != nil {
		s.logger.Warn("listing inbox-worthy beads", "error", werr)
	}

	// Forwarded UNION (ka-0000007204): pull the operator's followed bead IDs
	// and hydrate any that are not already in the intake/worthy union. The
	// daemon returns IDs ordered by follow created_at DESC, so the slice
	// position is the desired sort key (0 = most recently forwarded).
	forwardOrder := make(map[string]int)
	if opID > 0 {
		ids, ferr := s.daemon.ListFollowedBeadIDs(ctx, opID, inboxListLimit)
		if ferr != nil {
			s.logger.Warn("listing followed bead ids", "error", ferr)
		} else {
			for i, id := range ids {
				if id == "" {
					continue
				}
				if _, ok := forwardOrder[id]; ok {
					continue
				}
				forwardOrder[id] = i
				if _, dup := seen[id]; dup {
					// Already hydrated by intake/worthy — flagged later.
					continue
				}
				b, gerr := s.daemon.GetBead(ctx, id)
				if gerr != nil || b == nil {
					// Followed bead no longer exists or is unreadable —
					// drop it from the forwardOrder map so the row builder
					// does not flag a row that is not in `out`.
					delete(forwardOrder, id)
					continue
				}
				seen[id] = struct{}{}
				out = append(out, b)
				total++
			}
		}
	}

	return out, forwardOrder, total, nil
}

// applyForwardedFlags walks the row slice and stamps Forwarded/ForwardOrder
// for any row whose bead ID appears in the forward-order map. Kept separate
// from inboxRowFromBead so the bead-level builder stays pure.
func applyForwardedFlags(rows []inboxRowView, forwardOrder map[string]int) {
	if len(forwardOrder) == 0 {
		return
	}
	for i := range rows {
		if order, ok := forwardOrder[rows[i].ID]; ok {
			rows[i].Forwarded = true
			rows[i].ForwardOrder = order
		}
	}
}

// sortInboxRows orders rows: pinned first, then forwarded (ka-0000007204)
// by ForwardOrder asc (0 = most-recently-forwarded), then priority asc,
// then ID asc. Forwarded ranks above priority/category so an operator's
// freshly-forwarded beads always land at the top of their inbox; pinning
// still wins (operators explicitly opt into pinning).
func sortInboxRows(rows []inboxRowView) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Pinned != rows[j].Pinned {
			return rows[i].Pinned
		}
		if rows[i].Forwarded != rows[j].Forwarded {
			return rows[i].Forwarded
		}
		if rows[i].Forwarded && rows[j].Forwarded && rows[i].ForwardOrder != rows[j].ForwardOrder {
			return rows[i].ForwardOrder < rows[j].ForwardOrder
		}
		if rows[i].Priority != rows[j].Priority {
			return rows[i].Priority < rows[j].Priority
		}
		return rows[i].ID < rows[j].ID
	})
}

func isValidInboxScope(scope string) bool {
	return slices.Contains(inboxScopes, scope)
}

func orphanRowTitle(_ string) string {
	// M3 (ka-0000003587): only `mine` remains after the chip retirement; the
	// raise_only branch is gone. Signature kept so callers don't churn.
	return "Unparented action"
}

// inboxCountForViewer returns the inbox-count chrome badge value for the given
// request. Returns 0 when the resolver is not wired, the viewer is not signed in,
// or the daemon errors. The handler is intentionally cheap — it issues a single
// limit=1 listing against the §1.2.1 predicate and reads the Total field. Callers
// (handleIndex, handleActive) inject the result as InboxCount so the layout's
// `[Inbox · N]` nav badge renders consistently across pages (predicate G3.i).
func (s *Server) inboxCountForViewer(ctx context.Context, email string) int {
	if s.resolver == nil || strings.TrimSpace(email) == "" {
		return 0
	}
	id, found, err := s.resolver.OperatorID(ctx)
	if err != nil || !found || id == 0 {
		return 0
	}
	intake, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Categories:       []string{"intake"},
		Statuses:         []string{"open", "in_progress"},
		Limit:            1,
		ExcludeScheduled: true,
		OperatedByID:     id,
	})
	if err != nil || intake == nil {
		return 0
	}
	worthy, werr := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels:           []string{labelInboxWorthy},
		Statuses:         []string{"open", "in_progress"},
		Limit:            1,
		ExcludeScheduled: true,
		OperatedByID:     id,
	})
	count := intake.Total
	if werr == nil && worthy != nil {
		count += worthy.Total
	}
	// Forwarded contribution (ka-0000007204). The daemon does not expose a
	// count-only endpoint for follower IDs yet, so the badge issues the
	// same list as the rail. The follower set is small in the median; this
	// is a deliberate over-count rather than a join.
	if ids, ferr := s.daemon.ListFollowedBeadIDs(ctx, id, inboxListLimit); ferr == nil {
		count += len(ids)
	}
	// Approximation: sum totals. Overlap (worthy beads that are also intake,
	// or followed beads that the operator already owns) is not subtracted
	// because the badge is informational and a small over-count is
	// preferable to issuing a join query.
	return count
}

// rewriteParentMention rewrites or inserts a `parent(ka-X)` typed mention in
// the bead's description. Returns the new description and true on success.
// Returns false when the description has multiple parent mentions (multi-parent
// edge case — surface as 409 to keep the operator from clobbering deliberate
// state, design §5.5.3).
//
// When parentID is empty, the function strips the existing parent mention.
// When the description has no parent mention and parentID is set, the function
// appends `parent(ka-X)` on a new line at the end.
func rewriteParentMention(desc, parentID string) (string, bool) {
	const prefix = "parent("
	count := 0
	idx := -1
	for i := 0; i < len(desc); i++ {
		if i+len(prefix) > len(desc) {
			break
		}
		if desc[i:i+len(prefix)] == prefix {
			// Walk to closing paren.
			j := i + len(prefix)
			for j < len(desc) && desc[j] != ')' {
				j++
			}
			if j >= len(desc) {
				continue
			}
			count++
			if count == 1 {
				idx = i
			}
		}
	}
	if count > 1 {
		return "", false
	}
	if count == 0 {
		if parentID == "" {
			return desc, true
		}
		sep := "\n\n"
		if desc == "" {
			sep = ""
		} else if strings.HasSuffix(desc, "\n") {
			sep = "\n"
		}
		return desc + sep + "parent(" + parentID + ")", true
	}
	// count == 1 — splice in place.
	end := idx + len(prefix)
	for end < len(desc) && desc[end] != ')' {
		end++
	}
	if end >= len(desc) {
		return "", false
	}
	end++ // include the closing paren
	if parentID == "" {
		// Strip the mention plus a trailing newline if it left a blank line.
		out := desc[:idx] + desc[end:]
		out = strings.TrimRight(out, " \t")
		return out, true
	}
	return desc[:idx] + "parent(" + parentID + ")" + desc[end:], true
}
