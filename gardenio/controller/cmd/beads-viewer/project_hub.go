package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// projectHubTab is one entry in the project-hub tab/section bar (design doc §2).
// Overview renders eagerly here; Work is the legacy list (?view=list, owned by
// ka-0000001620); Agents/Gardeners/Costs link out to the existing surfaces until
// the lazy in-hub sections land (ka-0000003487); Config is the admin-gated
// editor seam (ka-0000007456). Sequencer→ is the reserved dark slot that lights
// up only when SequencerEnabled (ka-0000003558 is frozen — design doc §7).
type projectHubTab struct {
	Label  string // user-visible tab label
	Href   string // destination (absolute, basePath-prefixed)
	Active bool   // currently-selected tab
	Dark   bool   // reserved/disabled tab (rendered muted, not a link)
	Boost  bool   // hx-boost the navigation (same-document swaps)
}

// buildProjectHubTabs assembles the tab bar in its canonical render order:
//
//	[Overview] [Work] [Agents] [Gardeners] [Costs] [Sequencer→] [Config]
//
// Overview and Work are same-document tabs on /p/<project> (?view=overview is
// the eager hub; ?view=list is the legacy list). Agents/Gardeners/Costs deep-link
// to the existing project-scoped surfaces — they convert to in-hub lazy sections
// under ka-0000003487 without changing this bar's shape. Config is admin-gated:
// non-admins never see the tab (the daemon enforces min_role gardenio-admin on
// the write regardless — design doc §6). Sequencer→ renders muted/disabled
// unless the frozen sequencer epic is unfrozen (SequencerEnabled).
func buildProjectHubTabs(basePath, project string, active string, isAdmin, sequencerEnabled bool) []projectHubTab {
	p := url.PathEscape(project)
	tabs := []projectHubTab{
		{Label: "Overview", Href: basePath + "/p/" + p + "?view=overview", Active: active == "overview", Boost: true},
		{Label: "Work", Href: basePath + "/p/" + p + "?view=list", Active: active == "list", Boost: true},
		{Label: "Agents", Href: basePath + "/agents?project=" + url.QueryEscape(project)},
		{Label: "Gardeners", Href: basePath + "/gardeners?project=" + url.QueryEscape(project)},
		{Label: "Costs", Href: basePath + "/costs?project=" + url.QueryEscape(project)},
		{Label: "Sequencer→", Href: basePath + "/p/" + p + "/sequencer", Dark: !sequencerEnabled, Active: active == "sequencer"},
	}
	if isAdmin {
		// P1–P3 ship read-only: the Config tab anchors the Overview's config
		// summary; the in-page editor arrives in P4 (ka-0000007456).
		tabs = append(tabs, projectHubTab{Label: "Config", Href: basePath + "/p/" + p + "?view=overview#config", Active: active == "config", Boost: true})
	}
	return tabs
}

// chipQueryFromRequest extracts the chip-strip filter dimensions (status / label
// / q / sort) plus the resolved active type from a request, so the hub's chip
// strip preserves the same filters across a type click as the legacy list. It
// mirrors the param resolution in handleProject.
func chipQueryFromRequest(r *http.Request) chipQuery {
	q := r.URL.Query()
	return chipQuery{
		Status: strings.TrimSpace(q.Get("status")),
		Label:  strings.TrimSpace(q.Get("label")),
		Search: strings.TrimSpace(q.Get("q")),
		Sort:   strings.TrimSpace(q.Get("sort")),
		Active: resolveProjectTypeFilter(q.Get("type")),
	}
}

// handleProjectHub renders the eager project-hub Overview — the default view at
// a bare /p/<project> (and the explicit ?view=overview alias): the project
// header, the tab/section bar, and the eager Overview (config summary with
// validity badge, health panel, work breakdown digest). Expensive
// dependent-object sections load lazily (ka-0000003487); the legacy bead list
// lives under ?view=list (ka-0000001620).
//
// It is a pure read → viewer-scoped (plain HandleFunc behind the ambient auth
// middleware, like handleProject and the gardener page). Best-effort throughout:
// buildProjectHub never returns nil and degrades each panel independently, so a
// daemon error hides one panel rather than 500-ing the page.
func (s *Server) handleProjectHub(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if project == "" {
		http.Error(w, "Missing project", http.StatusBadRequest)
		return
	}

	q := chipQueryFromRequest(r)
	view := s.buildProjectHub(r.Context(), r, project, q, s.now())
	tabs := buildProjectHubTabs(s.basePath, project, "overview", view.IsAdmin, view.SequencerEnabled)

	data := map[string]any{
		"Project":      project,
		"Hub":          view,
		"Tabs":         tabs,
		"LazySections": projectHubLazySections,
		"GardenHref":   s.gardenBanner(r, retireProject, gardenProjectTarget),
	}
	s.render(w, r, "project_hub.html", data)
}

// projectHubLazyShell describes one reveal-on-scroll dependent-object section in
// the hub Overview (task ka-0000003487, design doc §3.2). The template renders a
// loading shell per entry that HTMX swaps (hx-trigger="intersect once",
// hx-swap="outerHTML") with the fragment served by the matching
// /b/project/{name}/<Endpoint> handler (project_hub_sections.go). Slug must
// match the section template's Section.Slug so the swapped-in <section> keeps a
// stable DOM id (and the in-fragment retry button re-targets it correctly).
type projectHubLazyShell struct {
	Slug     string // DOM id stem, matches the fragment's Section.Slug
	Endpoint string // /b/project/{name}/<Endpoint> handler suffix
	Title    string // heading shown in the loading shell (mirrors the fragment)
	Loading  string // lowercase noun for the "Loading <noun>…" placeholder
}

// projectHubLazySections is the canonical render order of the six lazy hub
// sections (design doc §3.2 items 4–9): agents, gardeners, accounts, costs,
// activity, decisions. Each entry's Endpoint + Slug line up with a handler in
// project_hub_sections.go and the shared project_hub_section.html partial.
var projectHubLazySections = []projectHubLazyShell{
	{Slug: "project-hub-agents", Endpoint: "agents", Title: "Agents", Loading: "agents"},
	{Slug: "project-hub-gardeners", Endpoint: "gardeners", Title: "Gardeners & schedules", Loading: "gardeners"},
	{Slug: "project-hub-accounts", Endpoint: "accounts", Title: "Accounts", Loading: "accounts"},
	{Slug: "project-hub-costs", Endpoint: "costs", Title: "Costs", Loading: "costs"},
	{Slug: "project-hub-activity", Endpoint: "activity", Title: "Recent activity", Loading: "recent activity"},
	{Slug: "project-hub-decisions", Endpoint: "decisions", Title: "Decisions & escalations", Loading: "decisions"},
}

// projectHubView is the eager-render view-model for the rich project hub at
// /p/<project>. It carries everything the hub Overview paints on first byte:
// the project config (from ListProjectBeads), the right-rail health summary
// (from buildProjectSummary), the type-chip strip, and the admin/sequencer
// gates. Expensive dependent-object sections (agents, gardeners, accounts,
// costs, recent activity, decisions) are NOT here — they load lazily as HTMX
// fragments per design doc §3.2.
//
// Design doc §4 (gardenio/docs/project-first-class-hub-design.md).
type projectHubView struct {
	// Name is the project name (the URL path value), always populated even
	// when no config bead exists for it.
	Name string

	// Config is the parsed type=project bead for this project. Zero-valued
	// (ConfigBeadID == "") when no config bead is registered — the hub still
	// renders health for label-only projects.
	Config beadsapi.ProjectInfo

	// ConfigBeadID / ConfigBeadStatus mirror Config.BeadID / Config.Status,
	// hoisted to the top level so the template can deep-link to the config
	// bead and render its status chip without reaching into Config. Empty when
	// no config bead exists.
	ConfigBeadID     string
	ConfigBeadStatus string

	// Validity is the config-validity health signal (design doc §5.3). It is a
	// health report, NOT a parallel schema: the authoritative validation lives
	// in the daemon TypeConfig. buildProjectValidity populates it best-effort
	// from the config bead's raw fields; a daemon error or label-only project
	// degrades to the zero report (no findings → green/no badge). The canonical
	// type lives in project_validity.go.
	Validity projectValidityReport

	// Summary is the right-rail health facet panel (open/in-progress/blocked
	// counts, priority distribution, top assignees, attention lists). Reused
	// verbatim from buildProjectSummary. Nil when the daemon errored on the
	// summary query — the panel is hidden, the rest of the hub still renders.
	Summary *projectSummary

	// Chips is the type-filter chip strip (Issues/All/Tasks/...), with counts
	// drawn from Summary.TypeCounts when the summary loaded. Identical strip to
	// the legacy /p/<project> list so the Work tab and Overview agree.
	Chips []filterChip

	// IsAdmin gates the (P4) config-edit affordance. type=project writes
	// require gardenio-admin (lvl 3) for an OIDC caller and are not
	// project-scoped, so this mirrors viewerCanEditProjectConfig — a global
	// check, not per-project delegation (design doc §6).
	IsAdmin bool

	// SequencerEnabled gates the dark "Sequencer→" hub tab. The sequencer epic
	// (ka-0000003558) is frozen; this stays false until it is unfrozen, at
	// which point the tab lights up with zero rework (design doc §7).
	SequencerEnabled bool
}

// buildProjectHub assembles the eager project-hub Overview for /p/<project>.
//
// It is EAGER-only and reuse-only: it issues exactly the queries the legacy
// /p/<project> page already issued — one ListProjectBeads (Config) and one
// buildProjectSummary (health) — and slices them into the hub view-model. No
// dependent-object section re-queries here; those are lazy HTMX fragments
// (design doc §3.2).
//
// Best-effort degradation (design doc §3, §4): a daemon error on any single
// sub-fetch hides only that panel — it never fails the whole assembly and the
// caller never 500s. A nil/zero sub-result simply renders an empty panel. The
// only "hard" requirement is the project name, which the caller always has.
func (s *Server) buildProjectHub(ctx context.Context, r *http.Request, project string, q chipQuery, now time.Time) *projectHubView {
	view := &projectHubView{
		Name:             project,
		IsAdmin:          viewerCanEditProjectConfig(r),
		SequencerEnabled: false, // ka-0000003558 frozen (design doc §7).
	}

	// Config (best-effort). ListProjectBeads is the same call the index uses;
	// a daemon error here hides the config panel + validity badge but leaves
	// health and the work breakdown intact.
	if projects, err := s.daemon.ListProjectBeads(ctx); err != nil {
		s.logger.Warn("project hub: listing project beads", "project", project, "error", err)
	} else if info, ok := projects[project]; ok {
		view.Config = info
		view.ConfigBeadID = info.BeadID
		view.ConfigBeadStatus = info.Status
	}
	// No config bead (label-only project) is not an error: the hub still
	// renders health below. view.Config stays zero-valued.

	// Validity (best-effort). Computes the config-validity health report from
	// the config bead's RAW fields (malformed JSON, bad K8s quantities, etc.).
	// A label-only project (no config bead) or any daemon error degrades to the
	// zero report (no findings → no badge).
	view.Validity = buildProjectValidity(ctx, s.daemon, view.ConfigBeadID)

	// Health (best-effort). buildProjectSummary already swallows-and-returns
	// its own errors as (nil, err); we hide the panel on error but still build
	// the chip strip (counts fall back to label-only).
	summary, err := buildProjectSummary(ctx, s.daemon, project, now)
	if err != nil {
		s.logger.Warn("project hub: building summary", "project", project, "error", err)
		summary = nil
	}
	view.Summary = summary

	// Chip strip. Counts come from the summary when it loaded; otherwise the
	// chips render label-only (ShowCount=false), matching handleProject.
	view.Chips = buildProjectChips(s.basePath, project, q, summary)

	return view
}

// buildProjectValidity returns the config-validity health signal for a project
// (design doc §5.3). It fetches the config bead by ID and runs the canonical
// buildProjectValidityReport (project_validity.go) over its RAW fields — the
// validator deliberately works off the raw map[string]string rather than a
// parsed ProjectInfo, because ProjectInfo has already discarded every malformed
// field, which is exactly what we want to surface.
//
// Best-effort: an empty configBeadID (label-only project) or any daemon error
// degrades to the zero report (no findings → no badge), never failing the
// assembler. The account-reference check is skipped (knownAccounts nil) here:
// the eager hub doesn't load the account list, so we only surface checks that
// don't need it (JSON-shape + K8s-quantity). The full check set runs where the
// account list is already in hand (index cards, P4 config editor).
func buildProjectValidity(ctx context.Context, daemon beadAPI, configBeadID string) projectValidityReport {
	if configBeadID == "" {
		return projectValidityReport{}
	}
	bead, err := daemon.GetBead(ctx, configBeadID)
	if err != nil || bead == nil {
		return projectValidityReport{}
	}
	return buildProjectValidityReport(bead.Fields, nil)
}

// chipQuery is the subset of the request query that the chip strip preserves
// across a type-filter click (status / label / q / sort). Decoupling the chip
// builder from *http.Request keeps buildProjectHub unit-testable without
// constructing a full request.
type chipQuery struct {
	Status string
	Label  string
	Search string
	Sort   string
	// Active is the currently-selected chip value (the resolved ?type=).
	Active string
}

// buildProjectChips builds the type-filter chip strip for the hub, with counts
// drawn from the summary when it loaded. Mirrors the chip assembly in
// handleProject so the Overview and Work tab render an identical strip.
func buildProjectChips(basePath, project string, q chipQuery, summary *projectSummary) []filterChip {
	chips := make([]filterChip, len(projectTypeChips))
	for i, c := range projectTypeChips {
		count := 0
		if summary != nil {
			count = summary.TypeCounts[c.Value]
		}
		chips[i] = filterChip{
			Label:     c.Label,
			Value:     c.Value,
			Href:      buildProjectChipHrefFromQuery(basePath, project, c.Value, q),
			Active:    c.Value == q.Active,
			Count:     count,
			ShowCount: summary != nil,
		}
	}
	return chips
}

// buildProjectChipHrefFromQuery adapts a chipQuery to the url.Values the
// canonical buildProjectChipHref expects, so the hub chip strip preserves the
// same status/label/q/sort filters across a type click as the legacy list.
func buildProjectChipHrefFromQuery(basePath, project, typeValue string, q chipQuery) string {
	current := url.Values{}
	if q.Status != "" {
		current.Set("status", q.Status)
	}
	if q.Label != "" {
		current.Set("label", q.Label)
	}
	if q.Search != "" {
		current.Set("q", q.Search)
	}
	if q.Sort != "" {
		current.Set("sort", q.Sort)
	}
	return buildProjectChipHref(basePath, project, typeValue, current)
}

// viewerCanAdmin reports whether the request's viewer may see admin-only
// affordances (the P4 project config editor). It mirrors viewerCanOperate
// (gardener_ops.go) EXACTLY but on the admin minimum: type=project writes
// require gardenio-admin (lvl 3) and are NOT project-scoped, so a "project
// admin" is a global admin (design doc §6).
//
// It FAILS CLOSED: a request with no OIDC roles (dev bypass, forwardauth edge
// stamping only an email, internal caller) is NOT an admin. kbeads decides admin
// solely from verified OIDC realm roles (or edge-v2 roles) and refuses static-
// token callers, so rendering admin affordances for a roleless session shows
// levers the daemon will 403 (gd-pjKksLDLWM). The roles consulted here are the
// ones oidcauth lifted from the caller's own token — the same claim the daemon
// reads off the forwarded user bearer — so the page and the daemon agree.
// adminSessionBanner explains the hidden levers to a roleless session.
func viewerCanAdmin(r *http.Request) bool {
	return oidcauth.HasMinimumRole(oidcauth.RolesFromContext(r.Context()), oidcauth.RoleAdmin)
}

// viewerImpersonator names the admin behind an impersonated session, from the verified edge bearer's impersonator
// claim alone (attachEdgeBearerRoles), or "".
func viewerImpersonator(r *http.Request) string {
	return oidcauth.ImpersonatorFromContext(r.Context())
}

// viewerCanEditProjectConfig reports whether the project Config editor should
// render. It differs from viewerCanAdmin on purpose: a roleless session (dev
// bypass, email-only edge) writes to the daemon with the static token, and
// kbeads' machineAllowedWrites accepts update/project and comment/project for
// that caller — so the editor works for it and hiding it would remove a lever
// that succeeds. A session WITH roles forwards its own bearer, which the daemon
// judges on those roles, so it needs gardenio-admin. Taint creation and team
// membership are not allow-listed for the static token and stay on
// viewerCanAdmin.
func viewerCanEditProjectConfig(r *http.Request) bool {
	roles := oidcauth.RolesFromContext(r.Context())
	return len(roles) == 0 || oidcauth.HasMinimumRole(roles, oidcauth.RoleAdmin)
}

// adminSessionBanner returns the layout banner text for a session whose daemon
// writes will not carry a verified role (no OIDC roles on the request: dev auth
// bypass, or an edge that stamps only an email), or "" for a real OIDC session.
// Such a session reaches the daemon with the static token, which kbeads refuses
// for taint creation and team membership, so those affordances are hidden
// (viewerCanAdmin) and this says so rather than leaving the page silently
// read-only. Project config edits are NOT affected: the daemon accepts them
// from the static token (viewerCanEditProjectConfig). Under
// BEADS_VIEWER_DEV_AUTH_BYPASS it also names the bypass, so a live overlay
// cannot run it unnoticed (a startup WARN alone is not visible to users).
func adminSessionBanner(r *http.Request) string {
	if r == nil || len(oidcauth.RolesFromContext(r.Context())) > 0 {
		return ""
	}
	if devAuthBypassEnabled() {
		return "Taint creation and team membership are hidden — BEADS_VIEWER_DEV_AUTH_BYPASS is active, so this session has no OIDC roles and the daemon will refuse those writes from it. Sign in with a gardenio-admin OIDC session to use them."
	}
	return "Taint creation and team membership are hidden — this session carries no OIDC roles, so the daemon will refuse those writes from it. Sign in with a gardenio-admin OIDC session to use them."
}
