package main

// Lazy dependent-object aggregator fragments for the rich project hub
// (task ka-0000004817, design doc §3.2). The eager Overview (project_hub.go)
// renders config + health + the work-breakdown digest on first paint; the
// expensive dependent-object sections load lazily as HTMX fragments revealed
// once the hub scrolls into view (mirroring the gardener-page eager-head +
// lazy-tail skeleton, gardener_page.go).
//
// Routes live under /b/ to avoid net/http.ServeMux's overlap rejection with
// /p/{project} (server.go:1172 — fragments take an unambiguous prefix):
//
//	GET /b/project/{name}/agents     — active project-scoped agents
//	GET /b/project/{name}/gardeners   — type:trigger + type:schedule gardeners
//	GET /b/project/{name}/accounts   — resolved type:account beads
//	GET /b/project/{name}/costs      — cost rollup over the project's accounts
//	GET /b/project/{name}/activity   — most-recently-updated project beads
//	GET /b/project/{name}/decisions  — blocked-on-decision beads + escalations
//
// Every fragment is BEST-EFFORT (design doc §3, §4): a daemon error renders an
// empty section with a retry affordance, never a 500 — matching the
// handleProject / gardener-page degradation rules. Each is a read → viewer-scoped
// (plain HandleFunc, gated by the ambient auth middleware, exactly like the
// gardener-page lazy sections). All dependent aggregations REUSE the existing
// builders (agentRowFromBeadCtx, gardenerRowFromBead, buildCostAccountView,
// computeIndexRecent, buildProjectSummary, ListEscalations) so the hub and its
// source pages never drift.

import (
	"context"
	"maps"
	"net/http"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// projectHubAgentsLimit caps the active-agent fetch for the hub's agents
// fragment. The hub is a digest, not the full /agents board, so a small cap
// keeps the section compact and the query cheap.
const projectHubAgentsLimit = 50

// projectHubActivityLimit caps the recent-activity fragment row count. Mirrors
// the /index recent panel's bound — enough to answer "what just happened in
// this project" without paging.
const projectHubActivityLimit = 20

// projectHubAccountsLimit caps the account-bead fetch backing the accounts and
// costs fragments. Realistic projects authorize a handful of accounts; the cap
// is a safety bound, not an expected truncation point.
const projectHubAccountsLimit = 50

// projectHubSection identifies which lazy fragment a hub shell requests, so the
// section view-model and the section template render a consistent header,
// retry URL, and empty-state copy across all six.
type projectHubSection struct {
	// Slug is the section's DOM id + the htmx swap target. Stable per section.
	Slug string
	// Title is the human-readable section heading.
	Title string
	// LazyURL is the /b/project/{name}/<section> endpoint the shell fetches on
	// reveal and the retry button re-fetches on error.
	LazyURL string
	// Project is the project name, carried so the section template can build
	// see-all links (e.g. to /agents?project= or /escalations?project=).
	Project string
	// Error is set when the underlying fetch failed; the template renders a
	// retry affordance instead of the (possibly partial) body. Empty on the
	// happy path AND on the empty path — an empty section is not an error.
	Error string
}

// hubSectionData wraps a section's typed payload alongside the shared
// projectHubSection chrome so a single template can render every fragment's
// header + retry + empty-state uniformly. The typed list lives under the
// section-specific key the template switches on.
func (s *Server) hubSectionData(sec projectHubSection, extra map[string]any) map[string]any {
	data := map[string]any{"Section": sec}
	maps.Copy(data, extra)
	return data
}

// projectHubAgentView is one row in the agents fragment. It is a thin subset of
// the full agentRowView (agents.go) — the hub digest shows identity, role,
// state, and the current task, linking out to the agent bead for detail. We
// reuse agentRowFromBeadCtx to build the full row, then project the fields the
// compact card needs.
type projectHubAgentView struct {
	ID         string
	Name       string
	Role       string
	AgentState string
	Status     string
	TaskID     string
	TaskTitle  string
	Ago        string
}

// handleProjectHubAgents renders the lazy agents fragment: active
// (open + in_progress) agent beads labelled project:<name>. Reuses the same
// query shape /agents uses (Types=[agent], active statuses) plus a
// project:<name> label, and the shared agentRowFromBeadCtx projection so the
// hub and /agents agree on identity/state. Unengaged prewarmed-pool members are
// dropped exactly as /agents drops them (isUnengagedPrewarmedAgent).
func (s *Server) handleProjectHubAgents(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	if project == "" {
		http.NotFound(w, r)
		return
	}
	sec := projectHubSection{
		Slug:    "project-hub-agents",
		Title:   "Agents",
		LazyURL: s.basePath + "/b/project/" + project + "/agents",
		Project: project,
	}

	rows, err := s.projectHubAgentRows(r.Context(), project)
	if err != nil {
		s.logger.Warn("project hub agents: listing", "project", project, "error", err)
		sec.Error = "Couldn't load agents for this project."
		s.renderPartial(w, r, "templates/partials/project_hub_section.html", "project_hub_section",
			s.hubSectionData(sec, nil))
		return
	}

	s.renderPartial(w, r, "templates/partials/project_hub_section.html", "project_hub_section",
		s.hubSectionData(sec, map[string]any{"Agents": rows}))
}

// projectHubAgentRows is the agents section's rows: the project's active
// agents, unengaged prewarmed-pool members dropped. The hub fragment and the
// garden's project panel (project_garden.go) both read it.
func (s *Server) projectHubAgentRows(ctx context.Context, project string) ([]projectHubAgentView, error) {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Labels:   []string{"project:" + project},
		Sort:     "-updated_at",
		Limit:    projectHubAgentsLimit,
		NoTotal:  true, // the roster is never counted (gd-I2zntgOsCq)
	})
	if err != nil {
		return nil, err
	}
	now := s.now()
	rows := make([]projectHubAgentView, 0)
	if res != nil {
		for _, b := range res.Beads {
			if b == nil || isUnengagedPrewarmedAgent(b) {
				continue
			}
			full := s.agentRowFromBeadCtx(ctx, b, project, now)
			rows = append(rows, projectHubAgentView{
				ID:         full.ID,
				Name:       full.Name,
				Role:       full.Role,
				AgentState: full.AgentState,
				Status:     full.Status,
				TaskID:     full.TaskID,
				TaskTitle:  full.TaskTitle,
				Ago:        full.Ago,
			})
		}
	}
	return rows, nil
}

// projectHubGardenerView is one row in the gardeners fragment. A thin subset of
// gardenerRowView (gardeners.go): identity, kind (trigger/schedule), enabled +
// status, the run tally, and a link to /gardener/{id}.
type projectHubGardenerView struct {
	ID           string
	Name         string
	Kind         string // "trigger" | "schedule"
	Status       string // "active" | "disabled" | "closed"
	Enabled      bool
	TerminalGoal string
	RunsSpawned  int
	Href         string
}

// handleProjectHubGardeners renders the lazy gardeners/schedules fragment:
// role:gardener type=trigger + type=schedule beads labelled project:<name>, with
// a paused banner when the project's schedules_enabled / triggers_enabled flag
// is false (design doc §3.2). Reuses gardenerRowFromBead so identity/status/
// counters match /gardeners, and the fixture / schedule-mirror exclusions so
// foreign or synthetic gardeners never leak onto the hub.
func (s *Server) handleProjectHubGardeners(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	if project == "" {
		http.NotFound(w, r)
		return
	}
	sec := projectHubSection{
		Slug:    "project-hub-gardeners",
		Title:   "Gardeners & schedules",
		LazyURL: s.basePath + "/b/project/" + project + "/gardeners",
		Project: project,
	}

	rows, err := s.projectHubGardenerRows(r.Context(), project)
	if err != nil {
		s.logger.Warn("project hub gardeners: listing", "project", project, "error", err)
		sec.Error = "Couldn't load gardeners for this project."
		s.renderPartial(w, r, "templates/partials/project_hub_section.html", "project_hub_section",
			s.hubSectionData(sec, nil))
		return
	}
	schedulesPaused, triggersPaused := s.projectHubPaused(r.Context(), project)

	s.renderPartial(w, r, "templates/partials/project_hub_section.html", "project_hub_section",
		s.hubSectionData(sec, map[string]any{
			"Gardeners":       rows,
			"SchedulesPaused": schedulesPaused,
			"TriggersPaused":  triggersPaused,
		}))
}

// projectHubGardenerRows is the gardeners section's rows: the project's live
// trigger and schedule gardeners, triggers first, then by name. The hub
// fragment and the garden's project panel both read it.
func (s *Server) projectHubGardenerRows(ctx context.Context, project string) ([]projectHubGardenerView, error) {
	now := s.now()
	rows := make([]projectHubGardenerView, 0)
	seen := map[string]bool{}
	var fetchErr error
	// Query both label and role-field, both trigger and schedule kinds, scoped
	// to the project label — the same union loadGardenerRows walks, narrowed to
	// one project. Dedupe by ID across the four queries.
	collect := func(q beadsapi.ListBeadsQuery) {
		if fetchErr != nil {
			return
		}
		q.NoTotal = true // the digest is never counted (gd-I2zntgOsCq)
		res, err := s.daemon.ListBeadsFiltered(ctx, q)
		if err != nil {
			fetchErr = err
			return
		}
		if res == nil {
			return
		}
		for _, b := range res.Beads {
			if b == nil || seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			// Exclude E2E fixtures + gardenio-next schedule-mirror artifacts, so
			// the hub never renders a synthetic or foreign gardener as a real one
			// (mirrors loadGardenerRows, bugs ka-0000008054 / ka-0000002811).
			if excludeFixtureGardener(b) || isScheduleMirrorGardenerBead(b) {
				continue
			}
			row := s.gardenerRowFromBead(ctx, b, now)
			// Mirror the /gardeners "Live" default (gardenersRowMatches,
			// gardenersFilterLive): a closed gardener is retired and should drop off
			// the project digest, just as closing a dashboard drops it off /charts
			// (ka-0000005651). A post-build skip on the derived status (rather than a
			// query Statuses filter) keeps DISABLED-but-open gardeners visible, exactly
			// as /gardeners does — the digest should show the live operational roster,
			// and the full "Archived" view remains reachable at /gardeners?filter=archived.
			if row.Status == "closed" {
				continue
			}
			rows = append(rows, projectHubGardenerView{
				ID:           row.ID,
				Name:         row.Name,
				Kind:         row.Kind,
				Status:       row.Status,
				Enabled:      row.Enabled,
				TerminalGoal: row.TerminalGoal,
				RunsSpawned:  row.RunsSpawned,
				Href:         row.Href,
			})
		}
	}
	for _, typ := range []string{"trigger", "schedule"} {
		collect(beadsapi.ListBeadsQuery{
			Types:  []string{typ},
			Labels: []string{"role:gardener", "project:" + project},
			Sort:   "updated_at",
			Limit:  gardenersListLimit,
		})
		collect(beadsapi.ListBeadsQuery{
			Types:  []string{typ},
			Labels: []string{"project:" + project},
			Fields: map[string]string{"role": "gardener"},
			Sort:   "updated_at",
			Limit:  gardenersListLimit,
		})
	}
	if fetchErr != nil {
		return nil, fetchErr
	}

	// Stable order: trigger-kind first, then by name — the hub is a digest, so a
	// deterministic listing beats the run-centric /gardeners sort here.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Kind != rows[j].Kind {
			return rows[i].Kind < rows[j].Kind
		}
		return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
	})

	return rows, nil
}

// projectHubPaused reports whether the project config turns its schedules or
// its triggers off, for the gardeners section's paused banner. Best-effort: a
// ListProjectBeads error reports neither.
func (s *Server) projectHubPaused(ctx context.Context, project string) (schedulesPaused, triggersPaused bool) {
	if projects, err := s.daemon.ListProjectBeads(ctx); err == nil {
		if info, ok := projects[project]; ok {
			return !info.SchedulesEnabled, !info.TriggersEnabled
		}
	}
	return false, false
}

// handleProjectHubAccounts renders the lazy accounts fragment: the type=account
// beads authorized for the project (project:<name> label, active status),
// resolved via ListAccountBeadsByProject and projected through the SAME
// buildCostAccountView the /costs page uses so provider/model/status/spend
// render identically. design doc §3.2 (item 6).
func (s *Server) handleProjectHubAccounts(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	if project == "" {
		http.NotFound(w, r)
		return
	}
	sec := projectHubSection{
		Slug:    "project-hub-accounts",
		Title:   "Accounts",
		LazyURL: s.basePath + "/b/project/" + project + "/accounts",
		Project: project,
	}

	accounts, err := s.projectAccountViews(r.Context(), project)
	if err != nil {
		s.logger.Warn("project hub accounts: listing", "project", project, "error", err)
		sec.Error = "Couldn't load accounts for this project."
		s.renderPartial(w, r, "templates/partials/project_hub_section.html", "project_hub_section",
			s.hubSectionData(sec, nil))
		return
	}

	s.renderPartial(w, r, "templates/partials/project_hub_section.html", "project_hub_section",
		s.hubSectionData(sec, map[string]any{"Accounts": accounts}))
}

// handleProjectHubCosts renders the lazy costs fragment: a spend rollup over
// the project's authorized accounts (design doc §3.2 item 7). It sums the same
// LiteLLM-sourced per-account cost fields the /costs page reads — no
// client-side estimation — into a compact daily/weekly/total summary plus the
// per-account cards.
func (s *Server) handleProjectHubCosts(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	if project == "" {
		http.NotFound(w, r)
		return
	}
	sec := projectHubSection{
		Slug:    "project-hub-costs",
		Title:   "Costs",
		LazyURL: s.basePath + "/b/project/" + project + "/costs",
		Project: project,
	}

	accounts, err := s.projectAccountViews(r.Context(), project)
	if err != nil {
		s.logger.Warn("project hub costs: listing accounts", "project", project, "error", err)
		sec.Error = "Couldn't load costs for this project."
		s.renderPartial(w, r, "templates/partials/project_hub_section.html", "project_hub_section",
			s.hubSectionData(sec, nil))
		return
	}

	rollup := buildProjectCostRollup(accounts)
	extra := map[string]any{
		"Accounts":    accounts,
		"CostRollup":  rollup,
		"HasAccounts": len(accounts) > 0,
	}

	s.renderPartial(w, r, "templates/partials/project_hub_section.html", "project_hub_section",
		s.hubSectionData(sec, extra))
}

// handleProjectHubActivity renders the lazy recent-activity fragment: the
// most-recently-updated in-progress beads in the project (design doc §3.2 item
// 8). Mirrors computeIndexRecent — same non-actionable-type filtering — but
// scoped to the project label. Reuses indexRecentItem so the row shape matches
// the /index recent panel.
func (s *Server) handleProjectHubActivity(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	if project == "" {
		http.NotFound(w, r)
		return
	}
	sec := projectHubSection{
		Slug:    "project-hub-activity",
		Title:   "Recent activity",
		LazyURL: s.basePath + "/b/project/" + project + "/activity",
		Project: project,
	}

	rows, err := s.projectHubActivityRows(r.Context(), project, []string{"in_progress"})
	if err != nil {
		s.logger.Warn("project hub activity: listing", "project", project, "error", err)
		sec.Error = "Couldn't load recent activity for this project."
		s.renderPartial(w, r, "templates/partials/project_hub_section.html", "project_hub_section",
			s.hubSectionData(sec, nil))
		return
	}

	s.renderPartial(w, r, "templates/partials/project_hub_section.html", "project_hub_section",
		s.hubSectionData(sec, map[string]any{"Activity": rows}))
}

// projectHubActivityRows is the project's most recently updated beads of the
// given statuses (every status when none), meta types dropped as /index's
// recent panel drops them. The hub's activity fragment asks for in_progress;
// the garden's project panel also asks for every status, its "recently moved"
// list (the project's slice of /feed).
func (s *Server) projectHubActivityRows(ctx context.Context, project string, statuses []string) ([]indexRecentItem, error) {
	// Over-fetch (×3) like computeIndexRecent so meta-type beads don't starve
	// the panel after filtering.
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Statuses:         statuses,
		Labels:           []string{"project:" + project},
		Sort:             "-updated_at",
		Limit:            projectHubActivityLimit * 3,
		ExcludeScheduled: true,
		// Nothing shows the total, and for the every-status "recently moved"
		// list the page is always full: its COUNT(*) re-reads every bead of the
		// project (4.3 s of 8 s for gardenio, gd-I2zntgOsCq), on every refresh
		// of the garden's project panel.
		NoTotal: true,
	})
	if err != nil {
		return nil, err
	}
	rows := make([]indexRecentItem, 0, projectHubActivityLimit)
	if res != nil {
		for _, b := range res.Beads {
			if b == nil {
				continue
			}
			if _, skip := recentActivityNonActionableTypes[b.Type]; skip {
				continue
			}
			rows = append(rows, indexRecentItem{
				ID:      b.ID,
				Title:   b.Title,
				Type:    b.Type,
				Status:  b.Status,
				Project: projectFromLabels(b.Labels),
				Ago:     formatAgo(b.UpdatedAt),
			})
			if len(rows) >= projectHubActivityLimit {
				break
			}
		}
	}
	return rows, nil
}

// handleProjectHubDecisions renders the lazy decisions/escalations fragment:
// the project's blocked-on-decision beads plus its open escalations (design doc
// §3.2 item 9). The blocked list is sourced from buildProjectSummary's
// BlockedBeads (the same one-query attention list the eager right rail uses),
// and escalations from ListEscalations filtered to the project label (mirroring
// handleEscalations' ?project= path).
func (s *Server) handleProjectHubDecisions(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PathValue("name"))
	if project == "" {
		http.NotFound(w, r)
		return
	}
	sec := projectHubSection{
		Slug:    "project-hub-decisions",
		Title:   "Decisions & escalations",
		LazyURL: s.basePath + "/b/project/" + project + "/decisions",
		Project: project,
	}

	now := s.now()

	// Blocked beads (best-effort). buildProjectSummary already swallows its own
	// error as (nil, err); a failure here only drops the blocked list — the
	// escalations list below still renders.
	var blocked []projectSummaryListItem
	summary, serr := buildProjectSummary(r.Context(), s.daemon, project, now)
	if serr != nil {
		s.logger.Warn("project hub decisions: summary", "project", project, "error", serr)
	} else if summary != nil {
		blocked = summary.BlockedBeads
	}

	// Escalations (best-effort). Filtered to the project label, mirroring
	// handleEscalations' ?project= path. A daemon error drops only this list.
	escalations := make([]projectHubEscalationView, 0)
	if eds, eerr := s.daemon.ListEscalations(r.Context(), "", escalationsListLimit); eerr != nil {
		s.logger.Warn("project hub decisions: escalations", "project", project, "error", eerr)
	} else {
		for _, ed := range eds {
			if ed.Issue == nil {
				continue
			}
			b := ed.Issue
			if projectFromLabels(b.Labels) != project {
				continue
			}
			escalations = append(escalations, projectHubEscalationView{
				ID:            b.ID,
				Title:         b.Title,
				OperationType: b.Fields["operation_type"],
				Status:        b.Status,
				Ago:           fmtTimeAgo(b.CreatedAt, now),
			})
		}
	}

	s.renderPartial(w, r, "templates/partials/project_hub_section.html", "project_hub_section",
		s.hubSectionData(sec, map[string]any{
			"Blocked":     blocked,
			"Escalations": escalations,
		}))
}

// projectHubEscalationView is one escalation row in the decisions fragment.
type projectHubEscalationView struct {
	ID            string
	Title         string
	OperationType string
	Status        string
	Ago           string
}

// projectCostRollup is the compact spend summary the costs fragment renders
// above the per-account cards. All figures are sums of the LiteLLM-sourced
// per-account cost fields (buildCostAccountView) — the viewer performs NO
// client-side cost estimation (cost read-only contract, costs.go).
type projectCostRollup struct {
	DailyUSD  float64
	WeeklyUSD float64
	TotalUSD  float64
	HasSpend  bool
	Accounts  int
}

// buildProjectCostRollup sums the per-account daily/weekly/total spend into the
// project's headline figures. HasSpend gates rendering so a project whose
// accounts have no LiteLLM spend shows "$0", never a fabricated estimate.
//
// Shared-litellm_key cost-duplicates are booked once (ka-0000003446): spend_sync
// stamps the same per-key total onto every account bead sharing a key, so a
// project authorizing two accounts on one key would otherwise double-count that
// bill. markCostDuplicates flags all but a deterministic owner; the flagged
// accounts' dollars are skipped here. The Accounts count and the per-account
// cards (which read DailyCostUSD directly) are unaffected — only the summed
// dollars dedup.
func buildProjectCostRollup(accounts []costAccountView) projectCostRollup {
	markCostDuplicates(accounts)
	r := projectCostRollup{Accounts: len(accounts)}
	for _, a := range accounts {
		if a.CostDuplicate {
			continue
		}
		r.DailyUSD += a.DailyCostUSD
		r.WeeklyUSD += a.WeeklyCostUSD
		r.TotalUSD += a.TotalCostUSD
	}
	r.HasSpend = r.TotalUSD > 0 || r.WeeklyUSD > 0 || r.DailyUSD > 0
	return r
}

// projectAccountViews resolves the project's authorized accounts and projects
// each through buildCostAccountView so the accounts + costs fragments share a
// single fetch shape and the exact /costs card view-model. It re-fetches the
// account beads by ID (ListAccountBeadsByProject returns AccountInfo, not the
// raw bead) so buildCostAccountView's full field set is available — the hub
// agents/gardeners sections already establish the per-section fetch budget, and
// realistic projects authorize only a handful of accounts.
func (s *Server) projectAccountViews(ctx context.Context, project string) ([]costAccountView, error) {
	infos, err := s.daemon.ListAccountBeadsByProject(ctx, project)
	if err != nil {
		return nil, err
	}
	now := s.now()
	views := make([]costAccountView, 0, len(infos))
	for _, info := range infos {
		// Resolve the account's bead by name to reuse buildCostAccountView. The
		// account bead's ID is not on AccountInfo, so look it up by title via a
		// scoped account-type query. A miss degrades to a name-only card rather
		// than failing the whole section.
		bead := s.findAccountBeadByName(ctx, info.Name)
		if bead == nil {
			views = append(views, costAccountView{
				Name:     info.Name,
				Provider: info.Provider,
				Model:    info.Model,
				Status:   info.Status,
			})
			continue
		}
		views = append(views, buildCostAccountView(bead, now))
	}
	sort.Slice(views, func(i, j int) bool {
		return strings.ToLower(views[i].Name) < strings.ToLower(views[j].Name)
	})
	return views, nil
}

// findAccountBeadByName returns the type=account bead whose title matches name,
// or nil when none is found / the daemon errors. Best-effort lookup used by the
// accounts + costs fragments to recover the full bead for buildCostAccountView.
func (s *Server) findAccountBeadByName(ctx context.Context, name string) *beadsapi.BeadDetail {
	if name == "" {
		return nil
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open", "in_progress"},
		Limit:    projectHubAccountsLimit,
	})
	if err != nil || res == nil {
		return nil
	}
	for _, b := range res.Beads {
		if b != nil && b.Title == name {
			return b
		}
	}
	return nil
}
