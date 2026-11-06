package main

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Retiring viewer pages into /garden (Lands workbench-glass spec §2.6, W-D6,
// WB-16). A page retires in two steps, each a runtime tunable on the
// beads-viewer config bead (env fallback, compiled default), so neither needs
// a deploy:
//
//  1. the banner: the page says "Open it in the garden", linking to the panel
//     that replaces it (default on);
//  2. the redirect: after a week of the banner, the GET page answers 302 to
//     that panel instead (default off). The template is never deleted (W-D7):
//     ?old=1 and the old viewer keep the page. The page's POST routes never
//     retire: they are the Workbench's write API.
//
// Either step applies only where the viewer reaches /garden: it is wired, and
// the principal is not a scoped one (for whom /garden* is still admin-only,
// route_classes.go, until per-viewer /garden lands, gd-MnQlEmbUq3).
//
// Each retirement has its own pair of flags, so one can flip, or flip back,
// without the others.
//
// ?old=1 on a retired page keeps the page for that request, as ?bead=1 keeps
// a plan's bead page: it is how the old viewer (old_viewer.go, step 5.13)
// still reaches the LATER parts of a retired page, a plan's critique,
// assistant and gardener on /plans/{id}, the plans bulk edits and master
// switch on /plans.
//
// R1 and R2 (step 5.5, the Queues panel): /decisions, /decisions/{id},
// /escalations, /inbox and /inbox/{id}.
const (
	gardenQueuesBannerKey   = "viewer.garden_queues_banner"
	gardenQueuesBannerEnv   = "GARDENIO_VIEWER_GARDEN_QUEUES_BANNER"
	gardenQueuesRedirectKey = "viewer.garden_queues_redirect"
	gardenQueuesRedirectEnv = "GARDENIO_VIEWER_GARDEN_QUEUES_REDIRECT"
)

// R3 (step 5.7): the bead page, GET /{id}, to the bead panel. An agent's page
// is not retired here: it carries the agent's terminal until the Glass
// (phase 6, ?glass=) replaces it.
const (
	gardenBeadBannerKey   = "viewer.garden_bead_banner"
	gardenBeadBannerEnv   = "GARDENIO_VIEWER_GARDEN_BEAD_BANNER"
	gardenBeadRedirectKey = "viewer.garden_bead_redirect"
	gardenBeadRedirectEnv = "GARDENIO_VIEWER_GARDEN_BEAD_REDIRECT"
)

// R4 (step 5.7): /search to the search panel (?q=). The viewer's Cmd-K
// palette (/cmdk/suggest) is a fragment of the viewer's pages, not a page, and
// goes with them.
const (
	gardenSearchBannerKey   = "viewer.garden_search_banner"
	gardenSearchBannerEnv   = "GARDENIO_VIEWER_GARDEN_SEARCH_BANNER"
	gardenSearchRedirectKey = "viewer.garden_search_redirect"
	gardenSearchRedirectEnv = "GARDENIO_VIEWER_GARDEN_SEARCH_REDIRECT"
)

// R6 (step 5.10): the bundle page, GET /bundle/{id}, to the bundle panel (?bundle=).
const (
	gardenBundleBannerKey   = "viewer.garden_bundle_banner"
	gardenBundleBannerEnv   = "GARDENIO_VIEWER_GARDEN_BUNDLE_BANNER"
	gardenBundleRedirectKey = "viewer.garden_bundle_redirect"
	gardenBundleRedirectEnv = "GARDENIO_VIEWER_GARDEN_BUNDLE_REDIRECT"
)

// R7 (step 5.9): /plans/{id} to the plan cockpit (?plan=), and /plans to the
// garden, whose plots and plans drawer list every plan.
const (
	gardenPlansBannerKey   = "viewer.garden_plans_banner"
	gardenPlansBannerEnv   = "GARDENIO_VIEWER_GARDEN_PLANS_BANNER"
	gardenPlansRedirectKey = "viewer.garden_plans_redirect"
	gardenPlansRedirectEnv = "GARDENIO_VIEWER_GARDEN_PLANS_REDIRECT"
)

// R5 (step 5.11): /active and /feed to the project panel's activity tab
// (?project=<name>&tab=activity), what is growing and what moved last in the
// project; with no project, to the garden, whose World is the ambient feed.
const (
	gardenActivityBannerKey   = "viewer.garden_activity_banner"
	gardenActivityBannerEnv   = "GARDENIO_VIEWER_GARDEN_ACTIVITY_BANNER"
	gardenActivityRedirectKey = "viewer.garden_activity_redirect"
	gardenActivityRedirectEnv = "GARDENIO_VIEWER_GARDEN_ACTIVITY_REDIRECT"
)

// R8 (step 5.11): the project hub, /p/{project}, to the project panel
// (?project=). Its Work list (?view=list, or any of the list's filters) is not
// retired: the project's bulk edits are LATER (spec §2.3), so it stays a
// viewer page.
const (
	gardenProjectBannerKey   = "viewer.garden_project_banner"
	gardenProjectBannerEnv   = "GARDENIO_VIEWER_GARDEN_PROJECT_BANNER"
	gardenProjectRedirectKey = "viewer.garden_project_redirect"
	gardenProjectRedirectEnv = "GARDENIO_VIEWER_GARDEN_PROJECT_REDIRECT"
)

// R9 (step 5.12): the gardener page, /gardener/{id}, to the gardener panel (?gardener=); the /gardeners index to the
// garden, whose project panels list each project's gardeners; the create-gardener sheet to its project's Gardeners tab,
// where a gardener is created; and the quick-spawn sheet to the bead panel's "Put an agent on this". The gardener page's
// fragments (/gardener/{id}/runs, /invocations, /beads, /edit, /successor) and /quick-spawn-inline are fragments of the
// pages, loaded by htmx or asked for JSON, never navigated to, and go with them.
const (
	gardenGardenerBannerKey   = "viewer.garden_gardener_banner"
	gardenGardenerBannerEnv   = "GARDENIO_VIEWER_GARDEN_GARDENER_BANNER"
	gardenGardenerRedirectKey = "viewer.garden_gardener_redirect"
	gardenGardenerRedirectEnv = "GARDENIO_VIEWER_GARDEN_GARDENER_REDIRECT"
)

// R10 (step 5.12): /settings to the settings panel (?settings=me), opened from the garden's Menu. Its htmx fragments
// (/settings/edit, /settings/view) go with it. The homepage layout it also edits (/u/{slug}) stays on the page until
// the people directory earns a row (spec §2.3).
const (
	gardenSettingsBannerKey   = "viewer.garden_settings_banner"
	gardenSettingsBannerEnv   = "GARDENIO_VIEWER_GARDEN_SETTINGS_BANNER"
	gardenSettingsRedirectKey = "viewer.garden_settings_redirect"
	gardenSettingsRedirectEnv = "GARDENIO_VIEWER_GARDEN_SETTINGS_REDIRECT"
)

// G-R1 (Lands step 6.6, gd-mWooPMTNwA): an agent's bead page, GET /{id} of a
// type=agent bead, carries its terminal (agent_terminal.js, a browser-direct
// WebSocket to coopmux) and its write toolbar; it retires to the Glass
// (?glass=<agent bead>), where watch, nudge, respond and attach are. The
// toolbar's POSTs (/agents/{id}/input, keys, nudge, respond, compact,
// control/claim) stay, as every retired page's do.
const (
	gardenGlassBannerKey   = "viewer.garden_glass_banner"
	gardenGlassBannerEnv   = "GARDENIO_VIEWER_GARDEN_GLASS_BANNER"
	gardenGlassRedirectKey = "viewer.garden_glass_redirect"
	gardenGlassRedirectEnv = "GARDENIO_VIEWER_GARDEN_GLASS_REDIRECT"
)

// G-R4 (Lands step 6.8, gd-STJOoSnUAW): the fleet list, GET /agents, to the
// Glass's Wall (?glass=wall, GL-5), the land's live sessions; with ?project=,
// to that project panel's Agents tab (WB-10). Its fragments (/agents/events,
// /agents/events-status) and the per-agent routes under /agents/{id} go with
// it, never navigated to.
const (
	gardenAgentsBannerKey   = "viewer.garden_agents_banner"
	gardenAgentsBannerEnv   = "GARDENIO_VIEWER_GARDEN_AGENTS_BANNER"
	gardenAgentsRedirectKey = "viewer.garden_agents_redirect"
	gardenAgentsRedirectEnv = "GARDENIO_VIEWER_GARDEN_AGENTS_REDIRECT"
)

// gardenRetirement is one retirement's two flags.
type gardenRetirement struct{ bannerKey, bannerEnv, redirectKey, redirectEnv string }

var (
	retireQueues = gardenRetirement{gardenQueuesBannerKey, gardenQueuesBannerEnv, gardenQueuesRedirectKey, gardenQueuesRedirectEnv}
	retireBead   = gardenRetirement{gardenBeadBannerKey, gardenBeadBannerEnv, gardenBeadRedirectKey, gardenBeadRedirectEnv}
	retireSearch = gardenRetirement{gardenSearchBannerKey, gardenSearchBannerEnv, gardenSearchRedirectKey, gardenSearchRedirectEnv}
	retireBundle = gardenRetirement{gardenBundleBannerKey, gardenBundleBannerEnv, gardenBundleRedirectKey, gardenBundleRedirectEnv}
	retirePlans  = gardenRetirement{gardenPlansBannerKey, gardenPlansBannerEnv, gardenPlansRedirectKey, gardenPlansRedirectEnv}
	// R5 and R8 (step 5.11)
	retireActivity = gardenRetirement{gardenActivityBannerKey, gardenActivityBannerEnv, gardenActivityRedirectKey, gardenActivityRedirectEnv}
	retireProject  = gardenRetirement{gardenProjectBannerKey, gardenProjectBannerEnv, gardenProjectRedirectKey, gardenProjectRedirectEnv}
	// R9 and R10 (step 5.12)
	retireGardener = gardenRetirement{gardenGardenerBannerKey, gardenGardenerBannerEnv, gardenGardenerRedirectKey, gardenGardenerRedirectEnv}
	retireSettings = gardenRetirement{gardenSettingsBannerKey, gardenSettingsBannerEnv, gardenSettingsRedirectKey, gardenSettingsRedirectEnv}
	// G-R1 (step 6.6)
	retireGlass = gardenRetirement{gardenGlassBannerKey, gardenGlassBannerEnv, gardenGlassRedirectKey, gardenGlassRedirectEnv}
	// G-R4 (step 6.8)
	retireAgents = gardenRetirement{gardenAgentsBannerKey, gardenAgentsBannerEnv, gardenAgentsRedirectKey, gardenAgentsRedirectEnv}
)

// gardenPanelKinds are the Workbench's panel parameters (web/workbench.js
// KINDS; settings, the viewer's own, since step 5.12). A retired page's own query is carried over to /garden except these,
// so a stray ?project= cannot open another panel than the one it moved to.
var gardenPanelKinds = []string{"bead", "plan", "project", "queue", "q", "bundle", "gardener", "glass", "costs", "creds", "settings", "pic"}

// oldViewerParam is the escape that keeps a retired page (?old=1).
const oldViewerParam = "old"

// gardenPanelURL is base/garden?<query>&<kind>=<id>: the page's own query,
// less any panel parameter, then the panel it moved to (none for an empty
// id: the garden itself). ?old=1 stays behind: the garden has no old page.
func gardenPanelURL(base string, query url.Values, kind, id string) string {
	keep := url.Values{}
	for k, vs := range query {
		if k != oldViewerParam && !slices.Contains(gardenPanelKinds, k) {
			keep[k] = vs
		}
	}
	q := keep.Encode()
	if id != "" {
		if q != "" {
			q += "&"
		}
		q += url.QueryEscape(kind) + "=" + url.QueryEscape(id)
	}
	if q == "" {
		return base + "/garden"
	}
	return base + "/garden?" + q
}

// gardenQueueTarget names the panel each R1/R2 page moved to: a list to its
// Queues tab, one decision or inbox item to its bead.
func gardenQueueTarget(tab string) func(*Server, *http.Request) string {
	return func(s *Server, r *http.Request) string {
		if id := strings.TrimSpace(r.PathValue("id")); id != "" {
			return gardenPanelURL(s.basePath, r.URL.Query(), "bead", id)
		}
		return gardenPanelURL(s.basePath, r.URL.Query(), "queue", tab)
	}
}

// gardenBeadTarget is the bead panel for the bead page's {id} (R3).
func gardenBeadTarget(s *Server, r *http.Request) string {
	return gardenPanelURL(s.basePath, r.URL.Query(), "bead", strings.TrimSpace(r.PathValue("id")))
}

// gardenSearchTarget is the search panel for /search?q= (R4); with no query,
// the garden, where Cmd-K and '/' search.
func gardenSearchTarget(s *Server, r *http.Request) string {
	return gardenPanelURL(s.basePath, r.URL.Query(), "q", strings.Join(strings.Fields(r.URL.Query().Get("q")), " "))
}

// gardenBundleTarget is the bundle panel for /bundle/{id} (R6).
func gardenBundleTarget(s *Server, r *http.Request) string {
	return gardenPanelURL(s.basePath, r.URL.Query(), "bundle", strings.TrimSpace(r.PathValue("id")))
}

// gardenPlanTarget is the plan cockpit for /plans/{id} (R7).
func gardenPlanTarget(s *Server, r *http.Request) string {
	return gardenPanelURL(s.basePath, r.URL.Query(), "plan", strings.TrimSpace(r.PathValue("id")))
}

// gardenPlansTarget is the garden for the /plans index (R7): its plots and its
// plans drawer list every plan. The index's own filters (project, status,
// operated_by, view, …) have no meaning there and are not carried.
func gardenPlansTarget(s *Server, r *http.Request) string {
	return gardenPanelURL(s.basePath, nil, "", "")
}

// gardenActivityTarget is the project panel's activity tab for /active and
// /feed with ?project= (R5); with none, the garden. The pages' own filters
// (operated_by, scope, view, user, include_scheduled) have no meaning there
// and are not carried.
func gardenActivityTarget(s *Server, r *http.Request) string {
	p := strings.TrimSpace(r.URL.Query().Get("project"))
	if p == "" {
		return gardenPanelURL(s.basePath, nil, "", "")
	}
	return gardenPanelURL(s.basePath, url.Values{"tab": {"activity"}}, "project", p)
}

// gardenProjectTarget is the project panel for the project hub, /p/{project}
// (R8), the page's own query kept; "" for its Work list, which stays a page.
func gardenProjectTarget(s *Server, r *http.Request) string {
	if !projectHubRequested(r.URL.Query()) {
		return ""
	}
	q := r.URL.Query()
	q.Del("view") // ?view=overview names the hub, which the panel is
	return gardenPanelURL(s.basePath, q, "project", strings.TrimSpace(r.PathValue("project")))
}

// gardenGardenerTarget is the gardener panel for /gardener/{id} (R9), the page's own query kept.
func gardenGardenerTarget(s *Server, r *http.Request) string {
	return gardenPanelURL(s.basePath, r.URL.Query(), "gardener", strings.TrimSpace(r.PathValue("id")))
}

// gardenGardenersTarget is the garden for the /gardeners index (R9): each project panel's Gardeners tab lists its
// gardeners. The index's own filters (filter, sort) have no meaning there and are not carried.
func gardenGardenersTarget(s *Server, r *http.Request) string {
	return gardenPanelURL(s.basePath, nil, "", "")
}

// gardenCreateGardenerTarget is the Gardeners tab of /create-gardener's ?project= (R9), where a gardener is created;
// with none, the garden.
func gardenCreateGardenerTarget(s *Server, r *http.Request) string {
	p := strings.TrimSpace(r.URL.Query().Get("project"))
	if p == "" {
		return gardenPanelURL(s.basePath, nil, "", "")
	}
	return gardenPanelURL(s.basePath, url.Values{"tab": {"gardeners"}}, "project", p)
}

// gardenQuickSpawnTarget is the bead panel of /quick-spawn's ?task= (R9), whose "Put an agent on this" starts an
// agent on it; else its ?project= panel; else the garden.
func gardenQuickSpawnTarget(s *Server, r *http.Request) string {
	q := r.URL.Query()
	if t := strings.TrimSpace(q.Get("task")); t != "" {
		return gardenPanelURL(s.basePath, nil, "bead", t)
	}
	if p := strings.TrimSpace(q.Get("project")); p != "" {
		return gardenPanelURL(s.basePath, nil, "project", p)
	}
	return gardenPanelURL(s.basePath, nil, "", "")
}

// gardenGlassTarget is the Glass for an agent's bead page, /{id} (G-R1), the page's own query kept.
func gardenGlassTarget(s *Server, r *http.Request) string {
	return gardenPanelURL(s.basePath, r.URL.Query(), "glass", strings.TrimSpace(r.PathValue("id")))
}

// gardenAgentsTarget is the Wall for the /agents fleet list (G-R4); with
// ?project=, that project panel's Agents tab. The page's own filters (role,
// state, group) have no meaning there and are not carried.
func gardenAgentsTarget(s *Server, r *http.Request) string {
	if p := strings.TrimSpace(r.URL.Query().Get("project")); p != "" {
		return gardenPanelURL(s.basePath, url.Values{"tab": {"agents"}}, "project", p)
	}
	return gardenPanelURL(s.basePath, nil, "glass", "wall")
}

// gardenSettingsTarget is the settings panel for /settings (R10): the viewer's own, ?settings=me.
func gardenSettingsTarget(s *Server, r *http.Request) string {
	return gardenPanelURL(s.basePath, nil, "settings", "me")
}

// reachesGarden reports whether this request's viewer can open /garden.
func (s *Server) reachesGarden(r *http.Request) bool {
	return s.garden != nil && !isScopedCtx(r.Context())
}

// gardenBanner is the "Open it in the garden" link for a retiring page, or ""
// when its banner is off, the viewer cannot reach /garden, or the target says
// this request's page is not retiring ("").
func (s *Server) gardenBanner(r *http.Request, step gardenRetirement, target func(*Server, *http.Request) string) string {
	if !s.reachesGarden(r) || !s.tunables.GetBool(step.bannerKey, step.bannerEnv, true) {
		return ""
	}
	return target(s, r)
}

// gardenQueuesBanner is gardenBanner for a retiring queue page (R1, R2).
func (s *Server) gardenQueuesBanner(r *http.Request, target func(*Server, *http.Request) string) string {
	return s.gardenBanner(r, retireQueues, target)
}

// gardenRedirects reports whether this request's retiring page answers 302 to
// the garden: the step's redirect flag is on and the viewer reaches /garden.
// An htmx swap (HX-Request) keeps the page: it wants a fragment of it, never
// the whole garden; so does ?old=1, the old viewer's way back to the page.
func (s *Server) gardenRedirects(r *http.Request, step gardenRetirement) bool {
	return r.Header.Get("HX-Request") == "" && r.URL.Query().Get(oldViewerParam) != "1" && s.reachesGarden(r) &&
		s.tunables.GetBool(step.redirectKey, step.redirectEnv, false)
}

// retireToGarden wraps a retiring GET page: once the step's redirect flag is
// on, a viewer who reaches /garden gets a 302 to the panel (302, not 301: a
// flag can flip back); everyone else keeps the page, as does a request whose
// target is "" (a view of the page that is not retiring).
func (s *Server) retireToGarden(step gardenRetirement, target func(*Server, *http.Request) string, page http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.gardenRedirects(r, step) {
			if to := target(s, r); to != "" {
				http.Redirect(w, r, to, http.StatusFound)
				return
			}
		}
		page(w, r)
	}
}

// retiredToGarden is retireToGarden for a retiring queue page (R1, R2).
func (s *Server) retiredToGarden(target func(*Server, *http.Request) string, page http.HandlerFunc) http.HandlerFunc {
	return s.retireToGarden(retireQueues, target, page)
}
