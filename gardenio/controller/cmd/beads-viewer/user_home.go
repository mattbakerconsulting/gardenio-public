package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// userHomeListLimit caps per-section bead counts on /u/{slug}. M1 set 50; M2
// keeps the same ceiling so dense sections (recent MRs/closures) still fit
// without forcing pagination plumbing in v1.
const userHomeListLimit = 50

// Window constants for the time-bounded sections. Kept here so future tuning
// touches one file instead of every query call site (design §3 footnote).
const (
	userHomeRecentMRWindow      = 14 * 24 * time.Hour
	userHomeRecentMergedWindow  = 14 * 24 * time.Hour
	userHomeForwardedFetchLimit = 50
	userHomeRecentCommentLimit  = 10
)

// sectionActivitySpine is the DOM id of the single merged activity section
// (templates/partials/user_home_activity.html `id="activity"`). The summary
// stat tiles anchor here after the merged-spine rework (ka-0000002041) removed
// the per-section anchors they used to target (bug ka-0000000915). Keep this in
// sync with the template's id.
const sectionActivitySpine = "activity"

// userHomeSection slug IDs. The order here drives render order on the page.
const (
	sectionPinned            = "pinned"
	sectionActiveAgents      = "active-agents"
	sectionOpenDecisions     = "open-decisions"
	sectionRecentComments    = "recent-comments"
	sectionOpenMRs           = "open-mrs"
	sectionRecentCommits     = "recent-commits"
	sectionRecentMerged      = "recent-merged"
	sectionForwardedMentions = "forwarded-mentions"
	// sectionTheirAutomations is the M3 (ka-0000003587) "Their automations"
	// section — surfaces schedules (type=schedule, operated_by_id=$op) so
	// the operator's recurring agents show alongside their other work
	// (design doc §7.2). Suppressed for operators with zero active
	// schedules so the homepage stays compact for non-power-users.
	sectionTheirAutomations = "their-automations"
	// sectionTheirGardeners (ka-0000002818, design doc §6) lists the
	// operator's scheduled gardeners — type=trigger + role:gardener beads owned
	// by this operator. Sibling of "Their automations"; each row links to
	// /gardener/{id}. Suppressed (zero-bead section) for operators without
	// gardeners so non-power-users keep a compact homepage.
	sectionTheirGardeners = "their-gardeners"
	// sectionTheirPlans (ka-0000003002, plan epic ka-0000004110 Step 3) lists
	// the plans this operator is INVOLVED in — creator ∪ step-owner ∪
	// decision-resolver ∪ gardener-owner, via the reusable plansInvolvingOperator
	// query Step 1 built (plans_involvement.go). Sibling of "Their gardeners" /
	// "Their automations"; each row links to /plans/{id} rather than the generic
	// bead page. Zero-plan sections contribute nothing to the merged stream, so a
	// non-power-user's homepage stays compact.
	sectionTheirPlans = "their-plans"
)

// userHomeBeadRow is the row view-model rendered by each homepage section.
type userHomeBeadRow struct {
	ID        string
	Title     string
	Type      string
	Status    string
	Priority  int
	Project   string
	UpdatedAt time.Time
	// CreatedAt is the bead's creation time. The activity stream sorts and
	// shows the relative age off UpdatedAt (last activity), but also surfaces
	// created time (ka-0000007477) — for closed/message-shaped rows the
	// created time is the more meaningful "when did this happen" signal than a
	// bookkeeping close-time bump.
	CreatedAt time.Time
	Ago       string
	// Href, when non-empty, overrides the default /{ID} bead link for this
	// row. The "Their gardeners" section sets it to /gardener/{id} so gardener
	// rows open the gardener page rather than the generic bead view.
	Href string
}

// userHomeSection groups a labeled list of bead rows. Sections are assembled
// by buildUserHomeSections and flattened into the single merged activity spine
// by buildUserHomeActivity — the per-section card layout (and its lazy-load
// mechanism) was retired by the merged-spine rework (ka-0000002041).
//
// Every section populates Beads/Count directly. The three formerly-lazy
// populations (forwarded mentions, recent comments, message inbox) are now
// fetched eagerly in buildUserHomeActivity so they interleave into the same
// stream; they no longer ride on a placeholder section view-model here.
type userHomeSection struct {
	Slug    string
	Title   string
	Count   int
	Beads   []userHomeBeadRow
	Empty   string
	MoreURL string
}

// userHomeViewDashboard is the default /u/{slug} render — the 8-section
// homepage view-model. userHomeViewConstellation is the M3-preserved legacy
// dashboard (formerly /inbox) accessed via /u/me?view=constellation
// (ka-0000003587 / design doc §8).
const (
	userHomeViewDashboard     = "dashboard"
	userHomeViewConstellation = "constellation"
)

// userHomeSlugMe is the magic slug that 308s to the authenticated viewer's
// own homepage (preserving query). Lets /inbox 308 to a single canonical URL
// (/u/me?view=dashboard) without needing the viewer's resolved slug.
const userHomeSlugMe = "me"

// handleUserHome renders GET /u/{slug} — the per-user homepage from epic
// ka-0000006114. M2 ships the full 8-section layout: pinned, active agents,
// open decisions, recent comments-light, open MRs, recent commits, recent
// merged work, recent forwarded-mentions. Heavy sections lazy-load via HTMX.
//
// M3 (ka-0000003587): the magic slug `me` 308s to the viewer's own homepage,
// and `?view=constellation` dispatches to the legacy /inbox dashboard
// renderer so power users keep their original layout via the canonical URL.
func (s *Server) handleUserHome(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// /u/me — 308 to the viewer's own slug, preserving the query string so
	// /u/me?view=dashboard and /u/me?view=constellation land on the right
	// view after the redirect. Unauthenticated callers get a /login bounce.
	if slug == userHomeSlugMe {
		s.redirectMeToViewer(w, r)
		return
	}
	ctx := r.Context()
	resp, err := s.daemon.LookupOperatorBySlug(ctx, slug)
	if err != nil {
		s.logger.Error("user home: lookup operator by slug failed", "slug", slug, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if !resp.Found {
		http.NotFound(w, r)
		return
	}
	operatorID := resp.OperatorID

	// view=constellation hands off to the legacy /inbox renderer. The
	// constellation view is viewer-scoped (it loads the operator's own
	// intakes, not the page-owner's), matching the design-doc intent that
	// /u/me?view=constellation reproduces the user's previous /inbox layout.
	if strings.EqualFold(r.URL.Query().Get("view"), userHomeViewConstellation) {
		s.handleInbox(w, r)
		return
	}

	viewerEmail := oidcauth.EmailFromContext(r.Context())
	isViewerSelf := false
	if s.resolver != nil {
		vid, found, lerr := s.resolver.OperatorID(ctx)
		if lerr == nil && found && vid == operatorID {
			isViewerSelf = true
		}
	}

	prefs, err := s.lookupProfilePrefByOperator(ctx, operatorID)
	if err != nil {
		s.logger.Warn("user home: profile pref lookup failed",
			"operator_id", operatorID, "slug", slug, "error", err)
	}
	// The eager bead populations feed both the top-of-fold summary counts and
	// the merged activity stream. Summary is built from the full section set
	// (before pref hiding) so the aggregates reflect real activity even when a
	// section is hidden from the operator's own card list.
	sections := s.buildUserHomeSections(ctx, operatorID, slug, prefs)
	summary := buildUserHomeSummary(sections)

	// Merged, reverse-chronological activity spine (bug ka-0000002041). This
	// replaces the former scatter of 11 disjoint section cards with ONE
	// chronological stream interleaving decisions, messages, MRs, comments,
	// closed work, and the rest by timestamp — the former section cards
	// survive only as client-side facet chips over this one stream. Built from
	// the eager sections (pref hiding is applied so a hidden section stays out
	// of the stream too) plus the three formerly-lazy populations.
	streamSections := applyProfilePref(sections, prefs)
	activity := s.buildUserHomeActivity(ctx, operatorID, slug, isViewerSelf, streamSections)

	// status_message lives on the operator's user bead (same Fields map as
	// the identity fields edited from /settings). Missing user bead or
	// lookup failure renders the read-only block with an empty value — the
	// self-only inline editor still renders so a fresh operator can set
	// their first message without an out-of-band write.
	userBead, lerr := s.lookupUserBeadByOperator(ctx, operatorID)
	if lerr != nil {
		s.logger.Warn("user home: lookup user bead for status_message", "slug", slug, "error", lerr)
	}

	// DisplayName drives the human-facing heading + tab title. Resolve it
	// through the SAME sibling-directory + user-bead join /people uses
	// (resolveOperatorGroupName → operatorGroup.resolvedName), so the /people
	// card heading and this h1 never state two different names for one human
	// (ka-0000001547). /people got that join in MR !6238 (ka-0000006552) but
	// this page still resolved from a single user bead, so a name that lives
	// only on a sibling slack row's directory Display — op-240 → "Mitch
	// Yates" — rendered the raw slug/op-id here while /people showed the name.
	//
	// Fallbacks preserve the prior behavior for pages /people does not list: an
	// operator dropped by filterHumanOperators (agent/sentinel kinds) or a
	// directory miss yields no group, so we fall to the single-user-bead
	// resolution (display_name → name → email) and finally the slug — an
	// operator with only an op-<id> sentinel slug still renders exactly as before.
	displayName := s.resolveOperatorGroupName(ctx, operatorID)
	if displayName == "" {
		displayName = displayNameFromUserBead(userBead)
	}
	if displayName == "" {
		displayName = slug
	}

	data := map[string]any{
		"Slug":         slug,
		"DisplayName":  displayName,
		"OperatorID":   operatorID,
		"IsViewerSelf": isViewerSelf,
		"ViewerEmail":  viewerEmail,
		"Summary":      summary,
		"Activity":     activity,
		"SearchAllURL": s.basePath + "/search?operated_by_slug=" + slug,
		// InboxURL is the eager, Embedded-independent entry point to the full
		// /u/{slug}/inbox page (bug ka-0000004829). The merged-spine rework
		// (ka-0000002041) dropped the lazy /section/inbox shell that used to set
		// Embedded=true — the only place the partial's "Open inbox ›" link
		// rendered — so the full inbox page was left reachable only by typing the
		// URL. A profile-card link (mirroring "All beads ›") restores a real,
		// JS-free entry point without reintroducing a lazy section card.
		"InboxURL":      s.basePath + "/u/" + slug + "/inbox",
		"StatusMessage": statusMessageFromBead(userBead),
	}

	s.render(w, r, "user_home.html", data)

	// §9 telemetry — latency, viewer-self vs cross-user, chip-source referer.
	// Emitted after render so latency_ms reflects the full handler.
	emitTelemetry(s.logger, telemetryEventUserHomeView,
		"slug", slug,
		"operator_id", operatorID,
		"viewer_email", trimEmail(viewerEmail),
		"viewer_self", isViewerSelf,
		"pinned_count", len(prefs.PinnedBeadIDs),
		"referer_path", refererPath(r),
		"latency_ms", time.Since(start).Milliseconds(),
	)
}

// handleMe renders /me — a 308 redirect to the authenticated viewer's
// /u/{slug}. Unauthenticated callers get a redirect to /login.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	s.redirectMeToViewer(w, r)
}

// redirectMeToViewer 308s the request to the authenticated viewer's
// /u/{slug}, preserving the request's query string. Used by both /me and
// /u/me (M3 / ka-0000003587) so the canonical "redirect to my homepage"
// behaviour lives in one place.
func (s *Server) redirectMeToViewer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.resolver == nil {
		http.Error(w, "viewer identity not configured", http.StatusServiceUnavailable)
		return
	}
	operatorID, found, err := s.resolver.OperatorID(ctx)
	if err != nil {
		s.logger.Error("me redirect: resolve viewer operator failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if !found {
		// Preserve the original path on /login bounce so the post-login
		// hop returns the user to the URL they tried to load.
		next := s.basePath + r.URL.Path
		if r.URL.RawQuery != "" {
			next += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, s.basePath+"/login?next="+url.QueryEscape(next), http.StatusFound)
		return
	}
	slugResp, err := s.daemon.GetOperatorSlug(ctx, operatorID)
	if err != nil {
		s.logger.Error("me redirect: get operator slug failed", "operator_id", operatorID, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	slug := ""
	if slugResp != nil && slugResp.Found {
		slug = slugResp.Slug
	}
	if slug == "" {
		slug = slugRefForOperator(operatorID)
	}
	target := s.basePath + "/u/" + slug
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}

// slugRefForOperator returns the opaque `op-<id>` fallback used when the
// operator has no canonical slug yet. Mirrors the SQL backfill in migration 028.
func slugRefForOperator(operatorID int64) string {
	return "op-" + itoa64(operatorID)
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// buildUserHomeSections assembles the eager homepage section populations. They
// fan out in parallel via a sync.WaitGroup (design §4.1 — one slug resolution
// at request entry, parallel per-section queries from there) and feed both the
// top-of-fold summary band (buildUserHomeSummary) and the merged activity spine
// (buildUserHomeActivity).
//
// There are no lazy placeholder sections here anymore. The merged-spine rework
// (ka-0000002041) collapsed the per-section card layout — including its
// hx-get/LazyURL deferred-load mechanism — into the single #activity stream,
// and buildUserHomeActivity fetches the three formerly-lazy populations
// (forwarded mentions, recent comments, message inbox) eagerly itself so they
// interleave into that stream. The standalone /u/{slug}/section/{forwarded,
// recent-comments,inbox} routes still render those partials (they back the
// Users-CAMP audit-harness reachability probes — see server.go), but nothing on
// the homepage lazy-loads them, so no LazyURL view-model is built for them.
func (s *Server) buildUserHomeSections(ctx context.Context, operatorID int64, slug string, prefs ProfilePref) []userHomeSection {
	now := time.Now()

	// Run eager sections in parallel. Each closure writes to its own slot
	// so no synchronization is needed beyond the WaitGroup.
	var (
		pinnedSec        userHomeSection
		activeAgentsSec  userHomeSection
		openDecsSec      userHomeSection
		openMRsSec       userHomeSection
		recentCommits    userHomeSection
		recentMerged     userHomeSection
		theirAutomations userHomeSection
		theirGardeners   userHomeSection
		theirPlans       userHomeSection
	)

	var wg sync.WaitGroup
	wg.Add(9)

	go func() {
		defer wg.Done()
		pinnedSec = s.buildPinnedSection(ctx, operatorID, slug, prefs)
	}()
	go func() {
		defer wg.Done()
		activeAgentsSec = s.runUserHomeSection(ctx, sectionActiveAgents, "Active agents",
			beadsapi.ListBeadsQuery{
				OperatedByID: operatorID,
				Types:        []string{"agent"},
				Statuses:     []string{"in_progress"},
				Limit:        userHomeListLimit,
			}, slug, "No active agents.", "agent", nil)
	}()
	go func() {
		defer wg.Done()
		openDecsSec = s.runUserHomeSection(ctx, sectionOpenDecisions, "Open decisions",
			beadsapi.ListBeadsQuery{
				OperatedByID: operatorID,
				Types:        []string{"decision"},
				Statuses:     []string{"open"},
				Limit:        userHomeListLimit,
			}, slug, "No open decisions.", "decision", nil)
	}()
	go func() {
		defer wg.Done()
		openMRsSec = s.runUserHomeSection(ctx, sectionOpenMRs, "Open MRs",
			beadsapi.ListBeadsQuery{
				OperatedByID: operatorID,
				Types:        []string{"merge-request"},
				Statuses:     []string{"open", "in_progress"},
				Limit:        userHomeListLimit,
			}, slug, "No open merge requests.", "merge-request", nil)
	}()
	go func() {
		defer wg.Done()
		// ka-0000004155: this section queries CLOSED merge-request beads — i.e.
		// the operator's merged MRs in the window — so the heading and empty
		// copy both say "Merged MRs", not "commits". The old "Recent commits"
		// label was a mismatch against its own merge-request query + empty line.
		recentCommits = s.runUserHomeSection(ctx, sectionRecentCommits, "Merged MRs (14d)",
			beadsapi.ListBeadsQuery{
				OperatedByID: operatorID,
				Types:        []string{"merge-request"},
				Statuses:     []string{"closed"},
				UpdatedAfter: now.Add(-userHomeRecentMRWindow),
				Limit:        userHomeListLimit,
			}, slug, "No merged MRs in the last 14 days.", "merge-request", nil)
	}()
	go func() {
		defer wg.Done()
		// Recent closed work = recently-closed non-MR, non-agent beads (tasks,
		// bugs, features…). The daemon list endpoint has no "exclude type"
		// predicate, so we fetch closed beads updated in the window and filter
		// MR/agent rows out client-side — those surface in their own dedicated
		// sections above. Titled "Closed work" (not "merged") since these are
		// closed beads, not merges (ka-0000004155 label hygiene).
		//
		// type=message is also excluded (ka-0000007477): a closed message bead
		// is not "work the operator did" — it is a chat line that got swept
		// closed, and this section stamps updated=close-time, so those rows
		// read as freshly-active when they are really old sent messages. They
		// already surface (with their true sent/created time) under the
		// Messages facet, so dropping them here removes the duplicate + the
		// misleading close-time stamp.
		recentMerged = s.runUserHomeSection(ctx, sectionRecentMerged, "Closed work (14d)",
			beadsapi.ListBeadsQuery{
				OperatedByID: operatorID,
				Statuses:     []string{"closed"},
				UpdatedAfter: now.Add(-userHomeRecentMergedWindow),
				Limit:        userHomeListLimit,
			}, slug, "No work closed in the last 14 days.", "", excludeTypeFilter("merge-request", "agent", "message"))
	}()
	go func() {
		defer wg.Done()
		// M3 (ka-0000003587): "Their automations" — type=schedule beads owned
		// by this operator, filtered to enabled=true (active). Default-on for
		// operators with >0 active schedules per design doc §8; the empty-state
		// returns a zero-bead section so empty-section visibility logic in the
		// template can suppress the heading for non-power-users.
		theirAutomations = s.runUserHomeSection(ctx, sectionTheirAutomations, "Their automations",
			beadsapi.ListBeadsQuery{
				OperatedByID: operatorID,
				Types:        []string{"schedule"},
				Statuses:     []string{"open", "in_progress"},
				Limit:        userHomeListLimit,
			}, slug, "No active automations.", "schedule", scheduleEnabledFilter())
	}()
	go func() {
		defer wg.Done()
		// ka-0000004692 (design doc §6.2.2): "Their gardeners" — type=trigger +
		// role:gardener beads whose *resolved* owner is this operator. Unlike the
		// other sections this does NOT key on operated_by_id alone: a gardener's
		// owner is resolved in three tiers (GardenerSpec.UserBeadID →
		// SlackUserID → trigger operated_by), the same path the /gardeners index
		// and the /gardener/{id} header use. Filtering on operated_by_id would
		// only catch tier-3 gardeners; resolving per-bead and matching the slug
		// catches gardeners linked via the strong keys too. Each row links to
		// /gardener/{id} rather than the generic bead page. Zero-bead sections
		// are suppressed by the template.
		theirGardeners = s.buildTheirGardenersSection(ctx, slug)
	}()
	go func() {
		defer wg.Done()
		// Step 3 (ka-0000003002, plan epic ka-0000004110): "Their plans" — the
		// plans this operator is INVOLVED in (creator ∪ step-owner ∪
		// decision-resolver ∪ gardener-owner), via the reusable
		// plansInvolvingOperator query Step 1 built. Rows link to /plans/{id}.
		// Zero plans → empty section, suppressed from the merged stream so a
		// non-power-user homepage stays compact.
		theirPlans = s.buildTheirPlansSection(ctx, operatorID, slug)
	}()

	wg.Wait()

	// Default render/precedence order (ka-0000008222 relevance fix): lead with
	// the needs-you / live signal, then the accomplishments, then the auxiliary
	// and habitually-empty sections. This order governs the summary band and the
	// merged-spine precedence; profile prefs override it downstream via
	// applyProfilePref. The forwarded / recent-comments / message-inbox
	// populations are NOT assembled here — buildUserHomeActivity fetches them
	// eagerly and interleaves them into the merged stream directly.
	return []userHomeSection{
		openDecsSec,      // needs-you, highest urgency
		activeAgentsSec,  // live work in flight
		recentCommits,    // accomplishment: merged MRs
		recentMerged,     // accomplishment: closed work
		openMRsSec,       // open MRs
		pinnedSec,        // operator-curated, usually empty
		theirAutomations, // automations, usually empty
		theirGardeners,   // gardeners, usually empty
		theirPlans,       // plans they're involved in, usually empty
	}
}

// userHomeSummary is the top-of-fold accomplishment + live-signal band
// (ka-0000007148). It carries recency-framed aggregates computed from the
// eager section counts so a stranger landing on /u/{slug} sees, above the
// fold and without scrolling, how much this operator has shipped — instead of
// a column of empty sections. Each stat links to the section that holds the
// underlying rows.
type userHomeSummary struct {
	WindowLabel string             // e.g. "last 14 days"
	Stats       []userHomeStatTile // ordered left-to-right
	HasSignal   bool               // true when any stat is non-zero
}

// userHomeStatTile is one figure in the summary band.
type userHomeStatTile struct {
	Label string // "Merged MRs", "Closed work", …
	Count int
	// Href is the anchor the tile navigates to. Every tile points at the real
	// #activity spine anchor (the merged-spine rework, ka-0000002041, deleted
	// the per-section anchors these tiles used to target — bug ka-0000000915),
	// so the click always resolves even with JS disabled.
	Href string
	// Facet is the activity-stream facet slug the tile deep-links into. A small
	// JS hook (user_home_activity.html) reads data-facet-link and preselects the
	// matching chip so the stream is filtered to this population on click. Empty
	// ⇒ no facet preselect (scroll only).
	Facet     string
	Accent    bool   // accomplishments render emphasized; live signal muted
	Recency   bool   // true ⇒ figure is window-bounded ("14d"), else all-time/now
	EmptyHint string // optional sub-note when Count==0 (e.g. "all clear")
}

// buildUserHomeSummary derives the summary band from the assembled sections.
// It reads counts off the eager sections (open-decisions, active-agents,
// merged-MRs, closed-work, open-MRs) by slug so the band stays in sync with
// the section list without re-querying the daemon. Lazy sections (messages,
// comments, forwarded) are deliberately excluded — their counts aren't known
// at first paint, and surfacing a placeholder 0 is exactly the stale-chip
// defect we're fixing (ka-0000004155).
//
// Every tile targets the single #activity spine anchor and carries a Facet
// slug: the merged-spine rework (ka-0000002041) collapsed the per-section
// cards these tiles used to anchor into ONE stream, so the old per-section
// hrefs (#recent-commits, …) resolved to nothing and every click was a no-op
// (bug ka-0000000915). Pointing at #activity restores the scroll, and the
// Facet drives a chip-preselect so the stream is filtered to the tile's
// population. "Merged MRs" and "Open MRs" carry DISTINCT facet slugs
// (mrs-merged / mrs-open — user_home_activity.go §facet slugs) so each tile
// filters to the rows it actually counts; folding them into one coarse `mrs`
// facet made both click through to the same set, surfacing open drafts under
// "Merged MRs 0" (bug ka-0000008262).
func buildUserHomeSummary(sections []userHomeSection) userHomeSummary {
	bySlug := make(map[string]userHomeSection, len(sections))
	for _, sec := range sections {
		bySlug[sec.Slug] = sec
	}
	count := func(slug string) int { return bySlug[slug].Count }

	// spineHref anchors every tile at the one merged-activity section.
	const spineHref = "#" + sectionActivitySpine

	sum := userHomeSummary{WindowLabel: "last 14 days"}
	sum.Stats = []userHomeStatTile{
		{Label: "Merged MRs", Count: count(sectionRecentCommits), Href: spineHref, Facet: facetMRsMerged, Accent: true, Recency: true},
		{Label: "Closed work", Count: count(sectionRecentMerged), Href: spineHref, Facet: facetClosed, Accent: true, Recency: true},
		{Label: "Open MRs", Count: count(sectionOpenMRs), Href: spineHref, Facet: facetMRsOpen},
		{Label: "Needs you", Count: count(sectionOpenDecisions), Href: spineHref, Facet: facetDecisions, EmptyHint: "all clear"},
		{Label: "Active agents", Count: count(sectionActiveAgents), Href: spineHref, Facet: facetAgents},
	}
	for _, st := range sum.Stats {
		if st.Count > 0 {
			sum.HasSignal = true
			break
		}
	}
	return sum
}

// scheduleEnabledFilter accepts only schedule beads with their `enabled` field
// set to a truthy value. Stored schedules use string field values, so this
// matches both "true" and "1" case-insensitively to stay robust against any
// upstream toggle UI that flips the value to a non-`"true"` truthy string.
func scheduleEnabledFilter() func(*beadsapi.BeadDetail) bool {
	return func(b *beadsapi.BeadDetail) bool {
		if b == nil || b.Fields == nil {
			return false
		}
		switch strings.ToLower(strings.TrimSpace(b.Fields["enabled"])) {
		case "true", "1", "yes":
			return true
		}
		return false
	}
}

// excludeTypeFilter returns a row predicate that drops beads whose Type is in
// the exclusion set. Used by the "recent merged work" section to keep MR /
// agent rows in their dedicated sections instead of double-listing here.
func excludeTypeFilter(types ...string) func(*beadsapi.BeadDetail) bool {
	if len(types) == 0 {
		return nil
	}
	deny := make(map[string]struct{}, len(types))
	for _, t := range types {
		deny[t] = struct{}{}
	}
	return func(b *beadsapi.BeadDetail) bool {
		if b == nil {
			return false
		}
		_, blocked := deny[b.Type]
		return !blocked
	}
}

// runUserHomeSection issues one ListBeadsFiltered call and projects the
// result into a userHomeSection view-model. accept, when non-nil, filters
// rows the daemon returned but that don't belong in this section
// (e.g. type-exclusion). Errors are logged and the section renders empty.
func (s *Server) runUserHomeSection(
	ctx context.Context,
	slugID, title string,
	q beadsapi.ListBeadsQuery,
	opSlug, emptyMsg, moreType string,
	accept func(*beadsapi.BeadDetail) bool,
) userHomeSection {
	sec := userHomeSection{
		Slug:    slugID,
		Title:   title,
		Empty:   emptyMsg,
		MoreURL: s.userHomeMoreURL(opSlug, moreType),
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, q)
	if err != nil {
		if !errors.Is(err, errSectionSilentlyEmpty) {
			s.logger.Warn("user home: section query failed",
				"section", slugID, "operator_slug", opSlug, "error", err)
		}
		return sec
	}
	if res == nil {
		return sec
	}
	now := time.Now()
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if accept != nil && !accept(b) {
			continue
		}
		sec.Beads = append(sec.Beads, userHomeBeadRow{
			ID:        b.ID,
			Title:     b.Title,
			Type:      b.Type,
			Status:    b.Status,
			Priority:  b.Priority,
			Project:   projectFromLabels(b.Labels),
			UpdatedAt: b.UpdatedAt,
			CreatedAt: b.CreatedAt,
			Ago:       fmtTimeAgo(b.UpdatedAt, now),
		})
	}
	sec.Count = len(sec.Beads)
	return sec
}

func (s *Server) userHomeMoreURL(opSlug, sectionType string) string {
	if opSlug == "" {
		return ""
	}
	u := s.basePath + "/search?operated_by_slug=" + opSlug
	if sectionType != "" {
		u += "&type=" + sectionType
	}
	return u
}

// buildPinnedSection assembles the pinned-beads section. The prefs blob is
// resolved once in handleUserHome and threaded down so this and the section-
// ordering step share the same read. Until pin-management UI ships, the list
// is empty for real operators and the section renders with an empty-state.
func (s *Server) buildPinnedSection(ctx context.Context, operatorID int64, slug string, prefs ProfilePref) userHomeSection {
	sec := userHomeSection{
		Slug:  sectionPinned,
		Title: "Pinned",
		Empty: "No pinned beads.",
	}
	if len(prefs.PinnedBeadIDs) == 0 {
		return sec
	}
	// Cap to the same per-section limit as the others so a misconfigured
	// pref blob doesn't blow up the page.
	ids := prefs.PinnedBeadIDs
	if len(ids) > userHomeListLimit {
		ids = ids[:userHomeListLimit]
	}
	beads, err := s.daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		s.logger.Warn("user home: batch get pinned beads failed",
			"operator_id", operatorID, "slug", slug, "error", err)
		return sec
	}
	now := time.Now()
	for _, id := range ids {
		b, ok := beads[id]
		if !ok || b == nil {
			continue
		}
		sec.Beads = append(sec.Beads, userHomeBeadRow{
			ID:        b.ID,
			Title:     b.Title,
			Type:      b.Type,
			Status:    b.Status,
			Priority:  b.Priority,
			Project:   projectFromLabels(b.Labels),
			UpdatedAt: b.UpdatedAt,
			CreatedAt: b.CreatedAt,
			Ago:       fmtTimeAgo(b.UpdatedAt, now),
		})
	}
	sec.Count = len(sec.Beads)
	return sec
}

// buildTheirGardenersSection lists the gardeners whose resolved owner is the
// page-owner identified by slug (design doc §6.2.2). It fetches every gardener
// (type=trigger + role:gardener that is open/in_progress), resolves each one's
// owner through the shared three-tier path (gardenerViewFromBead →
// resolveGardenerOwner), and keeps the rows whose resolved OwnerSlug matches the
// page slug. This is the reverse of the /gardeners owner chip: there we render a
// gardener's owner; here we list a user's gardeners.
//
// Matching on the resolved OwnerSlug (not operated_by_id) is what lets gardeners
// linked via the strong keys (GardenerSpec.UserBeadID / SlackUserID) surface on
// their owner's homepage even when the trigger bead's operated_by_id is empty
// or points elsewhere. Rows sort most-recently-updated first, mirroring the
// other homepage sections.
func (s *Server) buildTheirGardenersSection(ctx context.Context, slug string) userHomeSection {
	sec := userHomeSection{
		Slug:    sectionTheirGardeners,
		Title:   "Their gardeners",
		Empty:   "No gardeners.",
		MoreURL: s.basePath + "/gardeners",
	}
	if strings.TrimSpace(slug) == "" {
		return sec
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Labels:   []string{"role:gardener"},
		Statuses: []string{"open", "in_progress"},
		Sort:     "updated_at",
		Limit:    userHomeListLimit,
	})
	if err != nil {
		s.logger.Warn("user home: their-gardeners query failed", "slug", slug, "error", err)
		return sec
	}
	if res == nil {
		return sec
	}
	now := time.Now()
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		hdr, herr := s.gardenerViewFromBead(ctx, b)
		if herr != nil {
			s.logger.Debug("user home: their-gardeners header build", "id", b.ID, "error", herr)
			continue
		}
		if hdr.OwnerSlug != slug {
			continue
		}
		sec.Beads = append(sec.Beads, userHomeBeadRow{
			ID:        b.ID,
			Title:     b.Title,
			Type:      b.Type,
			Status:    b.Status,
			Priority:  b.Priority,
			Project:   projectFromLabels(b.Labels),
			UpdatedAt: b.UpdatedAt,
			CreatedAt: b.CreatedAt,
			Ago:       fmtTimeAgo(b.UpdatedAt, now),
			Href:      s.basePath + "/gardener/" + b.ID,
		})
	}
	// Most-recently-updated first (the daemon Sort is best-effort across the
	// merged set; re-sort defensively so the section order is deterministic).
	sort.SliceStable(sec.Beads, func(i, j int) bool {
		return sec.Beads[i].UpdatedAt.After(sec.Beads[j].UpdatedAt)
	})
	sec.Count = len(sec.Beads)
	return sec
}

// buildTheirPlansSection lists the plans this operator is INVOLVED in (design
// doc §4 Step 3, task ka-0000003002): the creator ∪ step-owner ∪
// decision-resolver ∪ gardener-owner union that Step 1 packaged as the reusable
// plansInvolvingOperator query (plans_involvement.go). It mirrors the /plans
// cold-load default scope — active-status plan epics — so a plan surfaced here
// is exactly one the operator would see under "Showing your work" on /plans.
//
// The involvement query keys on the operator's canonical ALIAS (its facets lean
// on the daemon alias filter + the decision `responded_by` string), while the
// homepage carries the operator's numeric id. We bridge the two the same way the
// user-bead read path does: resolve operatorID → directory email → canonical
// oidc alias (emailForOperator + canonicalOIDC). An operator with no resolvable
// email (no directory entry) yields an empty section rather than an error — the
// homepage still renders, just without a plans facet.
//
// Rows link to /plans/{id} (not the generic bead page) so a plan opens its
// plan-agent view, matching the Their-gardeners → /gardener/{id} precedent. Newest
// plan first, mirroring the involvement query's -created_at sort. A zero-plan
// section contributes nothing to the merged activity stream, keeping the
// homepage compact for operators who drive no plans.
func (s *Server) buildTheirPlansSection(ctx context.Context, operatorID int64, slug string) userHomeSection {
	sec := userHomeSection{
		Slug:  sectionTheirPlans,
		Title: "Their plans",
		Empty: "No plans.",
	}
	alias := canonicalOIDC(s.emailForOperator(ctx, operatorID))
	if alias == "" {
		// No resolvable operator alias (operator has no directory email) — the
		// involvement query has nothing to key on. Degrade to an empty section.
		return sec
	}
	// "See all" scopes the /plans index to this operator via ?operated_by=<alias>
	// — the same param the S1 header chip / planCreatorChip use so the index
	// lands on their INVOLVED plans (not /search, which /plans does not share).
	sec.MoreURL = s.userHomePlansURL(alias)

	baseQuery := beadsapi.ListBeadsQuery{
		Types:            []string{"epic"},
		Labels:           []string{planLabel},
		Statuses:         planStatusEnum(planStatusActive),
		Sort:             "-created_at",
		ExcludeScheduled: true,
		Limit:            userHomeListLimit,
	}
	res, err := s.plansInvolvingOperator(ctx, baseQuery, alias)
	if err != nil {
		s.logger.Warn("user home: their-plans involvement query failed",
			"operator_id", operatorID, "slug", slug, "error", err)
		return sec
	}
	if res == nil {
		return sec
	}
	now := time.Now()
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		sec.Beads = append(sec.Beads, userHomeBeadRow{
			ID:        b.ID,
			Title:     b.Title,
			Type:      b.Type,
			Status:    b.Status,
			Priority:  b.Priority,
			Project:   projectFromLabels(b.Labels),
			UpdatedAt: b.UpdatedAt,
			CreatedAt: b.CreatedAt,
			Ago:       fmtTimeAgo(b.UpdatedAt, now),
			Href:      s.basePath + "/plans/" + b.ID,
		})
	}
	sec.Count = len(sec.Beads)
	return sec
}

// userHomePlansURL returns the "See all" target for the Their-plans section:
// the /plans index scoped to this operator's plans via ?operated_by=<alias> —
// the same operator-scope param the S1 header chip and planCreatorChip emit, so
// a click lands on the operator's involved plans on /plans itself (not /search,
// which /plans does not share). Empty alias → "".
func (s *Server) userHomePlansURL(alias string) string {
	if alias == "" {
		return ""
	}
	return s.basePath + "/plans?operated_by=" + url.QueryEscape(alias)
}

// lookupProfilePrefByOperator resolves an operator id to their user bead and
// extracts the profile_prefs blob. Returns the zero value (no error) when no
// user bead matches — operators without a user bead simply have no prefs yet.
func (s *Server) lookupProfilePrefByOperator(ctx context.Context, operatorID int64) (ProfilePref, error) {
	bead, err := s.lookupUserBeadByOperator(ctx, operatorID)
	if err != nil {
		return ProfilePref{}, err
	}
	if bead == nil {
		return ProfilePref{}, nil
	}
	return parseProfilePref(bead.Fields), nil
}

// handleUserHomeSectionForwarded renders the lazy-loaded forwarded-mentions
// section. Returns an HTML fragment via the user_home_section partial.
func (s *Server) handleUserHomeSectionForwarded(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	resp, err := s.daemon.LookupOperatorBySlug(ctx, slug)
	if err != nil {
		s.logger.Error("user home: lookup operator by slug (forwarded) failed", "slug", slug, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if !resp.Found {
		http.NotFound(w, r)
		return
	}
	sec := s.buildForwardedSection(ctx, resp.OperatorID, slug)
	s.renderPartial(w, r, "templates/partials/user_home_section.html", "user_home_section", map[string]any{
		"Section":  sec,
		"BasePath": s.basePath,
	})
}

// handleUserHomeSectionRecentComments renders the lazy-loaded recent-
// comments-light section. Returns an HTML fragment.
func (s *Server) handleUserHomeSectionRecentComments(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	resp, err := s.daemon.LookupOperatorBySlug(ctx, slug)
	if err != nil {
		s.logger.Error("user home: lookup operator by slug (comments) failed", "slug", slug, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if !resp.Found {
		http.NotFound(w, r)
		return
	}
	sec := s.buildRecentCommentsSection(ctx, resp.OperatorID, slug)
	s.renderPartial(w, r, "templates/partials/user_home_section.html", "user_home_section", map[string]any{
		"Section":  sec,
		"BasePath": s.basePath,
	})
}

// buildForwardedSection resolves the operator's followed beads (the daemon
// side of the inbox UNION — operators who have been @-mentioned via a forward
// land here, ka-0000007204) and projects them into the standard section row
// shape.
func (s *Server) buildForwardedSection(ctx context.Context, operatorID int64, slug string) userHomeSection {
	sec := userHomeSection{
		Slug:    sectionForwardedMentions,
		Title:   "Forwarded mentions",
		Empty:   "Nothing has been forwarded to this user.",
		MoreURL: s.basePath + "/inbox",
	}
	ids, err := s.daemon.ListFollowedBeadIDs(ctx, operatorID, userHomeForwardedFetchLimit)
	if err != nil {
		s.logger.Warn("user home: list followed beads failed",
			"operator_id", operatorID, "slug", slug, "error", err)
		return sec
	}
	if len(ids) == 0 {
		return sec
	}
	beads, err := s.daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		s.logger.Warn("user home: batch get forwarded beads failed",
			"operator_id", operatorID, "slug", slug, "error", err)
		return sec
	}
	now := time.Now()
	for _, id := range ids {
		b, ok := beads[id]
		if !ok || b == nil {
			continue
		}
		sec.Beads = append(sec.Beads, userHomeBeadRow{
			ID:        b.ID,
			Title:     b.Title,
			Type:      b.Type,
			Status:    b.Status,
			Priority:  b.Priority,
			Project:   projectFromLabels(b.Labels),
			UpdatedAt: b.UpdatedAt,
			CreatedAt: b.CreatedAt,
			Ago:       fmtTimeAgo(b.UpdatedAt, now),
		})
	}
	sec.Count = len(sec.Beads)
	return sec
}

// buildRecentCommentsSection assembles the recent comments-light section.
// "Light" because we don't (yet) have a per-operator recent-comments daemon
// endpoint — design §3 explicitly defers the full comment-index schema to
// v2. As a useful approximation we sort the operator's recently-touched
// beads by UpdatedAt DESC and surface them; rows whose UpdatedAt is the
// bead's CreatedAt (untouched-since-creation) are dropped so this section
// reflects actual *touch* activity rather than fresh creations.
//
// When the per-operator-comments endpoint lands (parent epic deferred list),
// this function can swap to it without touching the section view-model or
// the lazy-load wiring.
func (s *Server) buildRecentCommentsSection(ctx context.Context, operatorID int64, slug string) userHomeSection {
	sec := userHomeSection{
		Slug:    sectionRecentComments,
		Title:   "Recent comments",
		Empty:   "No recent comments.",
		MoreURL: s.userHomeMoreURL(slug, ""),
	}
	now := time.Now()
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		OperatedByID: operatorID,
		UpdatedAfter: now.Add(-7 * 24 * time.Hour),
		Limit:        userHomeRecentCommentLimit * 3,
	})
	if err != nil {
		s.logger.Warn("user home: recent comments query failed",
			"operator_id", operatorID, "slug", slug, "error", err)
		return sec
	}
	if res == nil {
		return sec
	}
	rows := make([]userHomeBeadRow, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		// Drop rows that haven't been touched since creation — they
		// represent "freshly raised" beads, not commentary.
		if !b.UpdatedAt.IsZero() && !b.CreatedAt.IsZero() && !b.UpdatedAt.After(b.CreatedAt) {
			continue
		}
		rows = append(rows, userHomeBeadRow{
			ID:        b.ID,
			Title:     b.Title,
			Type:      b.Type,
			Status:    b.Status,
			Priority:  b.Priority,
			Project:   projectFromLabels(b.Labels),
			UpdatedAt: b.UpdatedAt,
			CreatedAt: b.CreatedAt,
			Ago:       fmtTimeAgo(b.UpdatedAt, now),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].UpdatedAt.After(rows[j].UpdatedAt)
	})
	if len(rows) > userHomeRecentCommentLimit {
		rows = rows[:userHomeRecentCommentLimit]
	}
	sec.Beads = rows
	sec.Count = len(rows)
	return sec
}

var errSectionSilentlyEmpty = errors.New("user home section silently empty")
