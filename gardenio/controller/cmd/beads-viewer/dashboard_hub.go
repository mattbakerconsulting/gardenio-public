package main

// dashboard_hub.go — the Observability Hub enumeration backend (epic
// ka-0000005362, W2 task ka-0000000773; design doc ka-0000007351 §1+§2).
//
// /charts used to embed a single hardcoded dashboard (uid
// gardenio-charts-candidate, a fixed 5-panel slice — see charts.go history).
// The hub instead enumerates every `type=dashboard` bead at request time:
// each becomes a card with a live hero solo-panel embed, its panel count, and
// deep links — so `gb grafana put` of a new dashboard bead makes it appear with
// no code change (design G3), provided the bead carries the enumeration
// metadata (a charset-valid dashboard_uid and, for the panel count, a
// grafana:dashboard-model doc child).
//
// Grouping: cards are grouped by `project:` label into sections (design §3,
// decision ka-0000005880 resolved R1/relabel). The 3 monorepo perf boards now
// carry project:monorepo, so they section out naturally with NO hub-specific
// special-casing — the grouping fact lives entirely in the bead labels.
//
// Reuse, don't rebuild: each card is built from buildDashboardView /
// fetchDashboardModel / extractDashboardPanels (dashboard_view.go), and the
// hero embed is the shared grafanaembed.HTML helper (the same sandboxed,
// scroll-free solo-panel iframe the bead view and the `:::grafana` directive
// emit). The single embedding path stays the only one.
//
// Security invariant (inherited from dashboard_view.go, kept here): a bead's
// dashboard_uid MUST pass dashboardUIDRe (^[A-Za-z0-9._-]{1,64}$) before it can
// reach any iframe src. The enumeration predicate IS that gate — a bead with a
// missing/invalid uid is skipped entirely, so grafanaembed.HTML is never called
// with an unvalidated uid. The hero panel id is a digits-only const, the base
// is the operator-set Grafana env, and grafanaembed.HTML HTML-escapes every
// attribute value. Nothing author-supplied reaches the DOM unescaped.
//
// Cost + cache: building the hub fans buildDashboardView across N dashboards,
// each doing a reverse-deps + model-doc fetch. A short-TTL process-wide cache
// (dashboardHubCache) collapses a render burst — many tabs/refreshes/viewers
// within the window — into ONE enumeration pass. The TTL is short enough that a
// newly-put dashboard shows up promptly.

import (
	"context"
	"html/template"
	"sort"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/grafanaembed"
)

// dashboardHubHeroPanelID is the Grafana panelId rendered as each card's live
// hero embed (design §3: "one live hero solo-panel embed, panelId=1"). Digits
// only — re-validated by grafanaembed.HTML before it reaches the iframe src.
const dashboardHubHeroPanelID = "1"

// dashboardHubHeroHeight is the hero solo-panel iframe height (px). A card hero
// is a glance, not the full board — ~150px per design §3 (the full multi-panel
// board is one click away via the Open↗ deep link).
const dashboardHubHeroHeight = "150"

// dashboardHubListLimit bounds the enumeration query. The fleet has ~8
// dashboards today (design §1); the generous cap leaves headroom while keeping
// a single bounded daemon call.
const dashboardHubListLimit = 256

// dashboardHubActiveStatuses restricts the hub to non-terminal dashboard beads.
// Without this filter the daemon returns closed dashboards too, so a dashboard
// that has been deprecated / scoped off the hub by closing its bead would keep
// rendering an embed (often a Grafana "Dashboard not found" or an empty panel
// for series this cluster does not scrape). Filtering to the active set makes
// closing a dashboard bead the intended "remove it from /charts" lever
// (ka-0000004760). It is the shared activeEnumStatuses (active_statuses.go) that
// every live-enumeration viewer surface now uses, so the /charts fix and its
// siblings stay in lock-step. Mirrors the active set the grafana-bridge store uses.
var dashboardHubActiveStatuses = activeEnumStatuses

// dashboardHubCacheTTL bounds how stale an enumerated hub can be. Short enough
// that a freshly `gb grafana put` dashboard appears within the window, long
// enough that a render burst (tabs, refreshes, concurrent viewers) folds into
// ONE enumeration pass instead of N fan-outs of buildDashboardView (each of
// which does a reverse-deps + model-doc fetch per dashboard).
const dashboardHubCacheTTL = 30 * time.Second

// dashboardHubCard is one enumerated dashboard rendered as a card on the hub.
// All embed/URL fields are built from the operator-set Grafana base + a
// charset-validated uid + const numeric panel id — nothing author-supplied is
// emitted unescaped (grafanaembed.HTML escapes every attribute value).
type dashboardHubCard struct {
	BeadID     string
	Title      string
	UID        string
	Project    string
	PanelCount int // viz panels (rows excluded), from the model doc
	ModelDocID string
	// Panels is the dashboard's full panel list (id/title/type, row headers
	// included) parsed from the JSON model — exactly what extractDashboardPanels
	// yields for the bead view. It backs the per-card panel-list fallback (design
	// §3 "the JSON-model panel-list fallback everywhere, incl. when
	// GRAFANA_BASE_URL=''"): rendered on every card, auto-expanded when there is
	// no live hero embed so the card still conveys the board's shape with no
	// Grafana. Nil/empty → the card shows ModelErr instead.
	Panels []dashboardModelPanel
	// HeroEmbed is the live hero solo-panel iframe (panelId=1), or
	// grafanaembed.HTML's quiet link-card fallback when Grafana is unconfigured.
	HeroEmbed template.HTML
	// OpenURL is the full-board /d/<uid> deep link, or "" when grafanaBaseURL
	// is unset (degraded: no dead link).
	OpenURL template.URL
	// ModelErr is a one-line reason the panel count is unavailable (the
	// model-doc fetch/parse degraded). Empty on success.
	ModelErr string
}

// dashboardHubSection groups the cards that share one `project:` label. Project
// is the bare project name ("gardenio", "monorepo", …) or "" for dashboards with
// no project label (rendered as a catch-all by the page).
type dashboardHubSection struct {
	Project    string
	Dashboards []dashboardHubCard
}

// dashboardHubData is the full enumerated hub: grouped sections plus summary
// counts for the landing fold (design §3 "N dashboards · ~M panels · K
// sections").
type dashboardHubData struct {
	Sections        []dashboardHubSection
	TotalDashboards int
	TotalPanels     int
	SectionCount    int
}

// dashboardHubCache memoizes the enumerated hub for a short TTL. Process-local
// and goroutine-safe — concurrent /charts handlers fetch through the same mutex
// so a thundering herd collapses into one enumeration. nil-safe: a nil cache
// reports every lookup as a miss so a bare Server (hub-less unit tests) falls
// straight through to a live build.
type dashboardHubCache struct {
	mu        sync.Mutex
	data      dashboardHubData
	fetchedAt time.Time
}

func newDashboardHubCache() *dashboardHubCache { return &dashboardHubCache{} }

// get returns the cached hub when it is within ttl of fetchedAt. The boolean
// is true on a fresh hit.
func (c *dashboardHubCache) get(now time.Time, ttl time.Duration) (dashboardHubData, bool) {
	if c == nil {
		return dashboardHubData{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetchedAt.IsZero() || now.Sub(c.fetchedAt) > ttl {
		return dashboardHubData{}, false
	}
	return c.data, true
}

// set replaces the cached hub and stamps the build time.
func (c *dashboardHubCache) set(data dashboardHubData, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = data
	c.fetchedAt = now
}

// dashboardHub returns the enumerated Observability Hub, fronted by a short-TTL
// cache so a render burst shares one enumeration pass. On a miss it builds the
// hub via buildDashboardHub and caches a non-empty result; an empty build is
// returned uncached so the very next load retries (mirrors feedSeedCache, so an
// upstream blip never pins an empty hub for the whole TTL).
func (s *Server) dashboardHub(ctx context.Context) dashboardHubData {
	if isScopedCtx(ctx) {
		// Shared across principals; see listProjectBeadsCached.
		return s.buildDashboardHub(ctx)
	}
	now := s.now()
	if data, ok := s.dashboardHubCache.get(now, dashboardHubCacheTTL); ok {
		return data
	}
	data := s.buildDashboardHub(ctx)
	if data.TotalDashboards > 0 {
		s.dashboardHubCache.set(data, now)
	}
	return data
}

// buildDashboardHub enumerates every type=dashboard bead and assembles the
// grouped hub. Best-effort throughout: a daemon list error yields an empty hub
// (the page still renders 200, header link suppressed), and a per-dashboard
// model-doc failure degrades that one card's panel count without dropping the
// card.
//
// The enumeration predicate doubles as the security gate: a dashboard bead is
// included only when its dashboard_uid passes dashboardUIDRe. Beads with a
// missing/invalid uid (e.g. the not-yet-backfilled VictoriaTraces board, design
// Flag B → W4) are skipped, so grafanaembed.HTML is never handed an unvalidated
// uid.
func (s *Server) buildDashboardHub(ctx context.Context) dashboardHubData {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"dashboard"},
		Statuses: dashboardHubActiveStatuses,
		Limit:    dashboardHubListLimit,
	})
	if err != nil {
		s.logger.Warn("dashboard hub: list type=dashboard failed", "err", err)
		return dashboardHubData{}
	}
	if res == nil {
		return dashboardHubData{}
	}

	base := s.grafanaBaseURL
	byProject := map[string][]dashboardHubCard{}
	var totalDashboards, totalPanels int

	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		// buildDashboardView resolves uid + model-doc child + panel list and
		// charset-validates the uid before building any embed URL. Reuse it
		// verbatim — do not rebuild the embed plumbing.
		v := buildDashboardView(ctx, b, s.daemon, base)
		if v == nil {
			continue
		}
		// Security gate / enumeration predicate: only a charset-valid uid may
		// reach an iframe src. A missing/invalid uid → skip (W4 backfill).
		if v.UID == "" || !dashboardUIDRe.MatchString(v.UID) {
			continue
		}

		// Hero embed: the shared sandboxed solo-panel iframe (panel 1). When
		// base is empty grafanaembed.HTML emits its quiet link-card fallback
		// rather than an iframe pointed nowhere. uid is validated above; panel
		// id is a digits-only const; nothing author-supplied is unescaped.
		embed := grafanaembed.HTML(base, v.UID, grafanaembed.Opts{
			PanelID: dashboardHubHeroPanelID,
			Height:  dashboardHubHeroHeight,
		})

		project := projectFromLabels(b.Labels)
		panelCount := countVizPanels(v.Panels)
		totalPanels += panelCount
		totalDashboards++

		byProject[project] = append(byProject[project], dashboardHubCard{
			BeadID:     b.ID,
			Title:      v.Title,
			UID:        v.UID,
			Project:    project,
			PanelCount: panelCount,
			ModelDocID: v.ModelDocID,
			// Carry the full panel list onto the card so the page can render the
			// JSON-model panel-list fallback per card (design §3) — the same list
			// extractDashboardPanels parsed for the count above, not a second
			// fetch.
			Panels: v.Panels,
			// embed is built from operator-set base + validated uid + const
			// panel id, and HTML-escaped inside grafanaembed.HTML; nothing here
			// is author-supplied.
			HeroEmbed: template.HTML(embed), //nolint:gosec // see above — no untrusted input.
			OpenURL:   v.OpenURL,
			ModelErr:  v.ModelErr,
		})
	}

	return dashboardHubData{
		Sections:        groupDashboardSections(byProject),
		TotalDashboards: totalDashboards,
		TotalPanels:     totalPanels,
		SectionCount:    len(byProject),
	}
}

// countVizPanels counts the visualization panels in a model's panel list,
// excluding row-group headers — matching the inventory's "viz panels" column
// (design §1). extractDashboardPanels flattens row children one level and
// emits the row itself with Type=="row"; we drop those so the count is the
// number of actual panels, not the number of list rows.
func countVizPanels(panels []dashboardModelPanel) int {
	n := 0
	for _, p := range panels {
		if p.Type == "row" {
			continue
		}
		n++
	}
	return n
}

// groupDashboardSections turns the per-project card map into deterministically
// ordered sections: sections sorted by project name ascending with the
// unlabeled ("") catch-all last, and dashboards within each section sorted by
// title (then bead id) ascending. Stable ordering keeps the rendered page —
// and the short-TTL cache that serves it — byte-identical across renders.
func groupDashboardSections(byProject map[string][]dashboardHubCard) []dashboardHubSection {
	sections := make([]dashboardHubSection, 0, len(byProject))
	for project, cards := range byProject {
		sort.Slice(cards, func(i, j int) bool {
			if cards[i].Title != cards[j].Title {
				return cards[i].Title < cards[j].Title
			}
			return cards[i].BeadID < cards[j].BeadID
		})
		sections = append(sections, dashboardHubSection{Project: project, Dashboards: cards})
	}
	sort.Slice(sections, func(i, j int) bool {
		// Empty (unlabeled) project sorts last as the catch-all.
		ei, ej := sections[i].Project == "", sections[j].Project == ""
		if ei != ej {
			return !ei
		}
		return sections[i].Project < sections[j].Project
	})
	return sections
}
