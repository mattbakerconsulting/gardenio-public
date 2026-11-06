package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/gardenermode"
)

// healthSnapshotTimeout bounds the per-request fetch of /v1/health/snapshot.
// The snapshot itself is built from cached store reads (10 s server-side
// cache), so a tight client timeout keeps a single slow daemon from holding
// up the page. 5 s is generous for the cached path and short enough that a
// truly stuck daemon falls into the per-tile error state quickly.
const healthSnapshotTimeout = 5 * time.Second

// healthStaleThreshold is the ceiling on snapshot age before tiles render
// a "stale" pill. Tiles refresh every 30 s and the kbeads server-side
// cache is 10 s, so 60 s indicates the snapshot pipeline is actually
// behind — not just routine cache jitter.
const healthStaleThreshold = 60 * time.Second

// snapshotStaleAge returns the snapshot's age relative to now, and true
// when that age is older than healthStaleThreshold. Returns false for nil
// snapshots so callers don't double-flag the error state.
func snapshotStaleAge(snap *kbeadsHealthSnapshot, now time.Time) (time.Duration, bool) {
	if snap == nil || snap.Now.IsZero() {
		return 0, false
	}
	age := max(now.Sub(snap.Now), 0)
	return age, age > healthStaleThreshold
}

// healthOpenBeadsPriorityScanLimit caps the priority-sorted scan used to
// derive P0/P1 counts and the oldest-open age for the open-beads tile. With
// Sort=priority the daemon returns lowest-priority (P0) rows first so a
// few hundred rows is plenty to capture every P0/P1 even on a busy queue;
// the oldest-open lookup walks the same page and degrades gracefully when
// the queue exceeds the cap.
const healthOpenBeadsPriorityScanLimit = 200

// healthBridgeSourceLookback bounds the per-source bead query window. The
// bridges tile looks at the most recently created bead carrying each
// `source:<bridge>` label as a proxy for `last_event_at`; we only need the
// newest row, so Limit=1 is sufficient.
const healthBridgeSourceLookback = 1

// healthBridgeWarnAfter / healthBridgeStaleAfter classify per-bridge state.
// Below `warn`, the bridge is rendering as "ok"; between warn and stale it
// shows a "slow" pill; beyond stale it surfaces as "stale" so an operator
// can tell at a glance which bridge has actually stopped feeding events.
const (
	healthBridgeWarnAfter  = 30 * time.Minute
	healthBridgeStaleAfter = 6 * time.Hour
)

// healthInProgressScanLimit caps the in-progress scan used to count stale
// in-progress beads. A few hundred is plenty for a fleet of this size; the
// daemon truncates beyond Limit, so the Stale counter degrades to "at most
// this many" once the queue exceeds the cap — which is the right failure
// mode for a glance tile.
const healthInProgressScanLimit = 500

// healthStaleInProgressAge is the threshold beyond which an in_progress bead
// is flagged "stale" on the tile. Mirrors the operator rule-of-thumb in
// gardener advice (ka-0000007726): no live agent + >1h since update = up for
// grabs. We pick 2h here to leave a buffer for genuinely long-running tasks.
const healthStaleInProgressAge = 2 * time.Hour

// healthCostsAccountLimit caps the account scan for the costs tile. Matches
// the /costs page cap (costsAccountLimit) so the tile and page agree on the
// truncation point.
const healthCostsAccountLimit = 50

// healthTileSnapshot is the shape of one {data, spec, meta} envelope we
// receive from kbeads /v1/health/snapshot. Mirrors `healthTile` in
// kbeads/internal/server/http_health_snapshot.go.
type healthTileSnapshot struct {
	Data json.RawMessage `json:"data"`
	Spec map[string]any  `json:"spec"`
	Meta map[string]any  `json:"meta"`
}

// kbeadsHealthSnapshot is the wire shape of GET /v1/health/snapshot. We
// re-declare it here (rather than importing from kbeads) so the viewer is
// not coupled to internal kbeads types — schema_version drives compatibility
// instead.
type kbeadsHealthSnapshot struct {
	SchemaVersion int                `json:"schema_version"`
	Now           time.Time          `json:"now"`
	Fleet         healthTileSnapshot `json:"fleet"`
	Decisions     healthTileSnapshot `json:"decisions"`
	Events        healthTileSnapshot `json:"events"`
	Incidents     healthTileSnapshot `json:"incidents"`
	Kbeads        healthTileSnapshot `json:"kbeads"`
}

// fleetSnapshotData mirrors `fleetData` in the kbeads handler.
type fleetSnapshotData struct {
	AgentsAlive int `json:"agents_alive"`
	AgentsStuck int `json:"agents_stuck"`
	AgentsTotal int `json:"agents_total"`
}

// decisionPreviewSnapshot is one row in the decisions tile's `next_3` list.
type decisionPreviewSnapshot struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	AgeSeconds float64 `json:"age_seconds"`
}

// decisionsSnapshotData mirrors `decisionsData` in the kbeads handler.
type decisionsSnapshotData struct {
	Pending int                       `json:"pending"`
	Next3   []decisionPreviewSnapshot `json:"next_3"`
}

// eventHistogramBucket is one bar in the events histogram.
type eventHistogramBucket struct {
	Time  time.Time `json:"t"`
	Count int64     `json:"count"`
}

// eventsSnapshotData mirrors `eventsData` in the kbeads handler. The 24h
// total lives in the tile's Meta as `total_24h_count` per the source.
type eventsSnapshotData struct {
	WindowSeconds int                    `json:"window_seconds"`
	Total         int64                  `json:"total"`
	Histogram     []eventHistogramBucket `json:"histogram"`
}

// incidentPreviewSnapshot is one row in the incidents tile lists.
type incidentPreviewSnapshot struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

// incidentsSnapshotData mirrors `incidentsData` in the kbeads handler.
type incidentsSnapshotData struct {
	RecentReleaseBugs  []incidentPreviewSnapshot `json:"recent_release_bugs"`
	RecentAgentCrashes []incidentPreviewSnapshot `json:"recent_agent_crashes"`
	WindowSeconds      int                       `json:"window_seconds"`
}

// kbeadsSnapshotData mirrors `kbeadsData` in the kbeads handler.
type kbeadsSnapshotData struct {
	OK           bool      `json:"ok"`
	LastHealthAt time.Time `json:"last_health_at"`
	Error        string    `json:"error,omitempty"`
}

// healthFleetTileView is the template view model for tile 2 (agent fleet).
type healthFleetTileView struct {
	Alive    int
	Stuck    int
	Total    int
	Idle     int
	Error    string
	Stale    bool
	StaleAge string
	AsOf     string
}

// healthDecisionRowView is one row in the decisions tile's "next 3" list.
type healthDecisionRowView struct {
	ID    string
	Title string
	Age   string
}

// healthDecisionsTileView is the template view model for tile 6.
type healthDecisionsTileView struct {
	Pending  int
	Next     []healthDecisionRowView
	Error    string
	Stale    bool
	StaleAge string
	AsOf     string
}

// healthEventsTileView is the template view model for tile 3.
type healthEventsTileView struct {
	Total5m       int64
	Total24h      int64
	Histogram     []eventHistogramBucket
	WindowSeconds int
	Error         string
	Stale         bool
	StaleAge      string
	AsOf          string
}

// healthIncidentRowView is one row in the incidents tile.
type healthIncidentRowView struct {
	ID    string
	Title string
	Ago   string
}

// healthIncidentsTileView is the template view model for tile 4. ReleaseBugs
// and AgentCrashes are the recent-24h lists from the snapshot. The four count
// fields (ReleaseBugs24h, AgentCrashes24h, OpenReleaseBugs, OpenAgentCrashes)
// render as explicit stat pills at the top of the tile so the surface carries
// numbers even when both 24h lists are empty — that's the audit's
// "guaranteed minimum payload" requirement.
type healthIncidentsTileView struct {
	ReleaseBugs      []healthIncidentRowView
	AgentCrashes     []healthIncidentRowView
	WindowSeconds    int
	ReleaseBugs24h   int
	AgentCrashes24h  int
	OpenReleaseBugs  int
	OpenAgentCrashes int
	Error            string
	Stale            bool
	StaleAge         string
	AsOf             string
}

// healthOpenBeadsTileView is the template view model for tile 1.
type healthOpenBeadsTileView struct {
	OpenTotal        int
	ReadyTotal       int
	InProgressTotal  int
	StaleInProgress  int
	StaleThresholdHr int
	// P0Count / P1Count are the priority-0/-1 counts derived from a
	// priority-sorted scan of the open issue queue. Both default to 0 when
	// the daemon query fails so the tile keeps rendering numbers rather
	// than going blank.
	P0Count int
	P1Count int
	// OldestOpenAge is the human-readable age of the oldest open issue
	// across the priority-sorted scan (e.g. "3d", "11h"). Empty when no
	// open issues are present or the daemon query failed.
	OldestOpenAge string
	// OldestOpenID is the bead id for the oldest open issue so the tile
	// can link directly to it.
	OldestOpenID string
	Error        string
	AsOf         string
}

// healthCostsTileView is the template view model for the costs tile (8th).
// Sourced from active account beads — same data the /costs page renders,
// summarised here as a glance.
type healthCostsTileView struct {
	DailyTotalUSD float64
	// HourlyRateUSD is the rolling-window fleet rate (ka-0000002806); only
	// meaningful when HourlyRateAvailable is true.
	HourlyRateUSD       float64
	HourlyRateAvailable bool
	// HourlyRate1hUSD is the 1-hour moving-average fleet rate (ka-0000001327),
	// the load-reactive companion to the 24h HourlyRateUSD; only meaningful when
	// HourlyRate1hAvailable is true.
	HourlyRate1hUSD       float64
	HourlyRate1hAvailable bool
	WeeklyTotalUSD        float64
	ActiveCount           int
	ThrottledCount        int
	TotalCount            int
	Truncated             bool
	Error                 string
	AsOf                  string
}

// healthBuildTileView is the template view model for the build/release tile
// (9th). Surfaces the running beads-viewer build alongside pointers to the
// release-owner schedule list and the recent release-bug surface. Distinct
// from the incidents tile: incidents lists individual bug rows, this tile
// pins the build identity itself. ReleaseDate / ReleaseAge are populated by
// parsing the YYYY.DDD.N calver convention so the tile shows when the build
// shipped even when no incidents are recorded.
type healthBuildTileView struct {
	Version              string
	ReleaseDate          string
	ReleaseAge           string
	RecentReleaseBugs24h int
	AsOf                 string
	// ReleaseHref is the Release page of the running version (release_page.go);
	// empty for a dev build or a version with no release record.
	ReleaseHref string
}

// healthBridgesTileView is the template view model for tile 5. The kbeads
// snapshot doesn't yet expose per-bridge `last_event_at` directly, but
// every bridge marks the beads it ingests with a `source:<bridge>` label.
// We use the freshest such bead per source as a proxy for liveness so the
// tile carries real data instead of every row showing "unknown".
type healthBridgesTileView struct {
	Sources []healthBridgeRowView
	Note    string
	AsOf    string
}

// healthBridgeStateOK / healthBridgeStateWarn / healthBridgeStateStale /
// healthBridgeStateUnknown classify each bridge row for the template's
// pill colour. We avoid a free-form string so the template renders a
// stable set of CSS classes.
const (
	healthBridgeStateUnknown = "unknown"
	healthBridgeStateOK      = "ok"
	healthBridgeStateWarn    = "slow"
	healthBridgeStateStale   = "stale"
	// healthBridgeStateDark is the loud state for a webhook door whose
	// dark-hook deadman verdict is tripped (gardenio_{jira,gitlab}_webhook_dark
	// == 1): no authenticated delivery within the window while upstream is
	// still active. It overrides the age-based states because a dark hook can
	// look merely "slow"/"stale"/"unknown" by last_event_at alone — the exact
	// silent-failure mode GAP-D closes. See webhook_dark.go.
	healthBridgeStateDark = "dark"
)

// healthBridgeRowView is one row in the bridges tile. State is one of
// healthBridgeStateOK / healthBridgeStateWarn / healthBridgeStateStale /
// healthBridgeStateUnknown — chosen so the template renders a stable
// pill class regardless of the underlying age.
type healthBridgeRowView struct {
	Name         string
	LastEventAgo string
	LastEventID  string
	State        string
	// OK preserves the prior boolean for tests/templates that just want
	// a fast "any signal at all" check. True when State == ok.
	OK bool
	// Dark is true when the bridge's dark-hook deadman verdict is tripped
	// (gardenio_{jira,gitlab}_webhook_dark == 1). It drives the loud "dark"
	// pill and is what distinguishes a silently-failed hook from a merely
	// quiet one. Only ever set for the jira/gitlab webhook rows (GAP-D).
	Dark bool
	// DarkChecked is true when a deadman verdict was successfully read and
	// the hook is live (gauge 0) — i.e. the monitor is running and the door
	// is confirmed healthy. Distinct from "no verdict available" (both
	// false), so the template can render a positive "monitored" affordance
	// only when the check actually ran.
	DarkChecked bool
}

// healthGardenersTileView is the template view model for tile 7. Populated
// from schedule beads (type=schedule + type=trigger label=trigger:schedule)
// plus event-mode gardeners (type=trigger label=trigger:multi that are
// gardeners, gd-fL4PM5z8kc), grouped by `role` field. Cycle-success history
// requires snapshot work and isn't surfaced yet — the tile shows the
// enabled/total split per role, and lists the event-mode gardeners with their
// watchdog cadence, last run and liveness.
type healthGardenersTileView struct {
	Roles         []healthGardenerRowView
	EnabledTotal  int
	DisabledTotal int
	// EventTotal counts the open event-mode gardeners in the totals above.
	EventTotal int
	// EventStale counts event gardeners in the stale state (enabled, not run
	// within the watchdog window, ready work waiting).
	EventStale int
	// Event lists the open event-mode gardeners, stale first, capped at
	// healthEventGardenersShown; EventMore is how many were cut.
	Event     []healthEventGardenerRowView
	EventMore int
	Error     string
	Note      string
	AsOf      string
}

// healthGardenerRowView is one row in the gardeners tile. EnabledCount is
// the number of enabled schedules for the role; TotalCount is enabled +
// disabled, of which EventCount are event-mode (trigger:multi) gardeners.
// Rows are sorted by EnabledCount desc, then Role asc.
type healthGardenerRowView struct {
	Role         string
	EnabledCount int
	TotalCount   int
	EventCount   int
}

// healthEventGardenerRowView is one event-mode gardener in the tile's event
// list. Cadence is the effective cadence: the fallback-watchdog cron, or
// "events only" when the gardener has no watchdog entry.
type healthEventGardenerRowView struct {
	ID         string
	Title      string
	Role       string
	Enabled    bool
	Cadence    string
	Timezone   string
	LastRun    string // "3h ago", or "never"
	LastRunRFC string
	// State is the liveness verdict (eventGardenerState*): ok, idle, stale,
	// unchecked or paused. Only stale counts against the gardener.
	State  string
	Detail string
}

// healthGardenersTileLimit caps the schedule fetches feeding the gardeners
// tile. Matches triggersListLimit so the tile and /triggers agree on
// the truncation point.
const healthGardenersTileLimit = 500

// fetchKbeadsHealthSnapshot fetches the kbeads health snapshot. The caller
// owns the timeout via ctx; we wrap any non-2xx into an error so the per-tile
// error state can render the fallback message.
func (s *Server) fetchKbeadsHealthSnapshot(ctx context.Context) (*kbeadsHealthSnapshot, error) {
	if s.beadsHTTPAddr == "" {
		return nil, fmt.Errorf("beads HTTP address not configured")
	}
	url := strings.TrimRight(s.beadsHTTPAddr, "/") + "/v1/health/snapshot"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	tok, err := s.daemonBearer(ctx)
	if err != nil {
		return nil, fmt.Errorf("daemon credential: %w", err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("snapshot status %d", resp.StatusCode)
	}
	var out kbeadsHealthSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	return &out, nil
}

// handleHealth renders the /health page with all 7 tiles populated from a
// single snapshot fetch. The page additionally emits per-tile hx-get refresh
// targets so the browser polls each tile independently every 30 s.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthSnapshotTimeout)
	defer cancel()

	// Snapshot fetch and the open-beads-tile counts are independent —
	// kick them off in parallel so /health wall clock is the slower of
	// the two rather than their sum. The remaining tile views are pure
	// snapshot decoders so they cost nothing.
	var (
		snap      *kbeadsHealthSnapshot
		snapErr   error
		openBeads healthOpenBeadsTileView
		fetchWg   sync.WaitGroup
	)
	fetchWg.Add(2)
	go func() {
		defer fetchWg.Done()
		snap, snapErr = s.fetchKbeadsHealthSnapshot(ctx)
	}()
	go func() {
		defer fetchWg.Done()
		openBeads = s.buildOpenBeadsTileView(r.Context(), time.Now())
	}()
	fetchWg.Wait()

	now := time.Now()
	build := buildBuildTileView(snap, now)
	build.ReleaseHref = s.runningReleaseHref(ctx)
	data := map[string]any{
		"OpenBeads":       openBeads,
		"Fleet":           buildFleetTileView(snap, snapErr, now),
		"Events":          buildEventsTileView(snap, snapErr, now),
		"Incidents":       s.buildIncidentsTileView(r.Context(), snap, snapErr, now),
		"Bridges":         s.buildBridgesTileView(r.Context(), snap, now),
		"Decisions":       buildDecisionsTileView(snap, snapErr, now),
		"Gardeners":       s.buildGardenersTileView(r.Context(), now),
		"Costs":           s.buildCostsTileView(r.Context(), now),
		"Build":           build,
		"Escalations":     s.buildEscalationsTileView(r.Context(), now),
		"OperatorSignals": buildOperatorSignalsTileView(r.Context(), s.acceptanceMetricsAPI(), now),
	}
	s.render(w, r, "health.html", data)
}

// handleHealthTileOpenBeads renders the tile 1 fragment for hx-get refresh.
func (s *Server) handleHealthTileOpenBeads(w http.ResponseWriter, r *http.Request) {
	view := s.buildOpenBeadsTileView(r.Context(), time.Now())
	s.renderPartial(w, r, "templates/partials/health_tile_open_beads.html", "health_tile_open_beads", map[string]any{
		"OpenBeads": view,
	})
}

// handleHealthTileFleet renders the tile 2 fragment for hx-get refresh.
func (s *Server) handleHealthTileFleet(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthSnapshotTimeout)
	defer cancel()
	snap, err := s.fetchKbeadsHealthSnapshot(ctx)
	view := buildFleetTileView(snap, err, time.Now())
	s.renderPartial(w, r, "templates/partials/health_tile_fleet.html", "health_tile_fleet", map[string]any{
		"Fleet": view,
	})
}

// handleHealthTileEvents renders the tile 3 fragment for hx-get refresh.
func (s *Server) handleHealthTileEvents(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthSnapshotTimeout)
	defer cancel()
	snap, err := s.fetchKbeadsHealthSnapshot(ctx)
	view := buildEventsTileView(snap, err, time.Now())
	s.renderPartial(w, r, "templates/partials/health_tile_events.html", "health_tile_events", map[string]any{
		"Events": view,
	})
}

// handleHealthTileIncidents renders the tile 4 fragment for hx-get refresh.
func (s *Server) handleHealthTileIncidents(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthSnapshotTimeout)
	defer cancel()
	snap, err := s.fetchKbeadsHealthSnapshot(ctx)
	view := s.buildIncidentsTileView(r.Context(), snap, err, time.Now())
	s.renderPartial(w, r, "templates/partials/health_tile_incidents.html", "health_tile_incidents", map[string]any{
		"Incidents": view,
	})
}

// handleHealthTileBridges renders the tile 5 fragment for hx-get refresh.
func (s *Server) handleHealthTileBridges(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthSnapshotTimeout)
	defer cancel()
	snap, _ := s.fetchKbeadsHealthSnapshot(ctx)
	view := s.buildBridgesTileView(r.Context(), snap, time.Now())
	s.renderPartial(w, r, "templates/partials/health_tile_bridges.html", "health_tile_bridges", map[string]any{
		"Bridges": view,
	})
}

// handleHealthTileDecisions renders the tile 6 fragment for hx-get refresh.
func (s *Server) handleHealthTileDecisions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthSnapshotTimeout)
	defer cancel()
	snap, err := s.fetchKbeadsHealthSnapshot(ctx)
	view := buildDecisionsTileView(snap, err, time.Now())
	s.renderPartial(w, r, "templates/partials/health_tile_decisions.html", "health_tile_decisions", map[string]any{
		"Decisions": view,
	})
}

// handleHealthTileGardeners renders the tile 7 fragment for hx-get refresh.
func (s *Server) handleHealthTileGardeners(w http.ResponseWriter, r *http.Request) {
	view := s.buildGardenersTileView(r.Context(), time.Now())
	s.renderPartial(w, r, "templates/partials/health_tile_gardeners.html", "health_tile_gardeners", map[string]any{
		"Gardeners": view,
	})
}

// handleHealthTileCosts renders the costs tile fragment for hx-get refresh.
func (s *Server) handleHealthTileCosts(w http.ResponseWriter, r *http.Request) {
	view := s.buildCostsTileView(r.Context(), time.Now())
	s.renderPartial(w, r, "templates/partials/health_tile_costs.html", "health_tile_costs", map[string]any{
		"Costs": view,
	})
}

// handleHealthTileBuild renders the build/release tile fragment for hx-get refresh.
func (s *Server) handleHealthTileBuild(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthSnapshotTimeout)
	defer cancel()
	snap, _ := s.fetchKbeadsHealthSnapshot(ctx)
	view := buildBuildTileView(snap, time.Now())
	view.ReleaseHref = s.runningReleaseHref(ctx)
	s.renderPartial(w, r, "templates/partials/health_tile_build.html", "health_tile_build", map[string]any{
		"Build":   view,
		"Version": version,
	})
}

// buildOpenBeadsTileView counts open beads (status=open) and the ready queue
// (status=open AND no open deps AND not scheduled). Both queries use the
// daemon's CountOnly fast path: SELECT COUNT(*) without the COUNT(*) OVER()
// + ORDER BY pattern that would otherwise force postgres to materialize and
// sort the entire matching set. The two counts are independent, so we run
// them concurrently — wall clock drops from sum-of-two to slowest-of-two.
func (s *Server) buildOpenBeadsTileView(ctx context.Context, now time.Time) healthOpenBeadsTileView {
	v := healthOpenBeadsTileView{
		AsOf:             now.UTC().Format(time.RFC3339),
		StaleThresholdHr: int(healthStaleInProgressAge / time.Hour),
	}

	// OpenTotal and ReadyTotal are pure counts (only .Total is read), so they
	// use the daemon's CountOnly fast path — SELECT COUNT(*) without the
	// COUNT(*) OVER() + ORDER BY pattern that would otherwise force postgres
	// to materialize and sort the entire matching set. The two counts are
	// independent, so we run them concurrently — wall clock drops from
	// sum-of-two to slowest-of-two.
	//
	// Filter Kinds:["issue"] so the tile counts actual work — not audit-event
	// ledger beads (ka-0000004100, ka-0000006341), session config beads, or
	// other non-issue kinds. Matches the default `kd list` semantics.
	var (
		openRes, readyRes *beadsapi.ListBeadsResult
		openErr, readyErr error
		wg                sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		openRes, openErr = s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Kinds:     []string{"issue"},
			Statuses:  []string{"open"},
			CountOnly: true,
		})
	}()
	go func() {
		defer wg.Done()
		// "Ready queue" = open beads with no open dependencies and not
		// scheduled. The daemon doesn't expose AssigneeIsNull, so this is
		// the closest proxy for "claimable now" — same notion as `gb ready`.
		readyRes, readyErr = s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Kinds:            []string{"issue"},
			Statuses:         []string{"open"},
			NoOpenDeps:       true,
			ExcludeScheduled: true,
			CountOnly:        true,
		})
	}()
	wg.Wait()

	if openErr != nil {
		v.Error = "open-beads count unavailable"
		return v
	}
	if openRes != nil {
		v.OpenTotal = openRes.Total
	}
	if readyErr == nil && readyRes != nil {
		v.ReadyTotal = readyRes.Total
	}
	// In-progress + stale-in-progress (operator advice ka-0000007726). We
	// fetch up to healthInProgressScanLimit rows so we can date-stamp the
	// stale portion; Total is the authoritative count even when rows are
	// truncated. Failure leaves both at zero rather than failing the tile.
	ipRes, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:    []string{"issue"},
		Statuses: []string{"in_progress"},
		Limit:    healthInProgressScanLimit,
	})
	if err == nil && ipRes != nil {
		v.InProgressTotal = ipRes.Total
		cutoff := now.Add(-healthStaleInProgressAge)
		for _, b := range ipRes.Beads {
			if b == nil || b.UpdatedAt.IsZero() {
				continue
			}
			if b.UpdatedAt.Before(cutoff) {
				v.StaleInProgress++
			}
		}
	}
	// Priority-sorted scan: count P0/P1 and the oldest open id. With
	// Sort=priority the daemon returns lowest-priority numerically first,
	// so the head of the page is exactly the P0/P1 strip we want to
	// surface; the same page is walked once more to find max age.
	prRes, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:    []string{"issue"},
		Statuses: []string{"open"},
		Sort:     "priority",
		Limit:    healthOpenBeadsPriorityScanLimit,
	})
	if err == nil && prRes != nil {
		var oldest *beadsapi.BeadDetail
		for _, b := range prRes.Beads {
			if b == nil {
				continue
			}
			switch b.Priority {
			case 0:
				v.P0Count++
			case 1:
				v.P1Count++
			}
			if b.CreatedAt.IsZero() {
				continue
			}
			if oldest == nil || b.CreatedAt.Before(oldest.CreatedAt) {
				oldest = b
			}
		}
		if oldest != nil {
			v.OldestOpenAge = fmtTimeAgo(oldest.CreatedAt, now)
			v.OldestOpenID = oldest.ID
		}
	}
	return v
}

// buildCostsTileView summarises active account beads for the costs tile.
// Reuses buildCostsSummary so the tile and /costs agree exactly on the
// totals. Failure surfaces a single "unavailable" line.
func (s *Server) buildCostsTileView(ctx context.Context, now time.Time) healthCostsTileView {
	v := healthCostsTileView{AsOf: now.UTC().Format(time.RFC3339)}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open", "in_progress"},
		Sort:     "title",
		Limit:    healthCostsAccountLimit,
	})
	if err != nil {
		v.Error = "costs snapshot unavailable"
		return v
	}
	accounts := make([]costAccountView, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		accounts = append(accounts, buildCostAccountView(b, now))
	}
	// Dedup shared-litellm_key cost-duplicates before summing, exactly as
	// handleCosts does, so this tile's DailyTotal/Weekly agree with the /costs
	// headline instead of double-counting a shared key (ka-0000003446).
	markCostDuplicates(accounts)
	sum := buildCostsSummary(accounts, now)
	// Same rolling-window rate the /costs headline shows (ka-0000002806) so the
	// tile and the page agree; falls back to 0 / unavailable when vmsingle has
	// no sample.
	s.applyFleetHourlyRate(ctx, &sum)
	v.DailyTotalUSD = sum.GrandTotalUSD
	v.HourlyRateUSD = sum.HourlyRateUSD
	v.HourlyRateAvailable = sum.HourlyRateAvailable
	v.HourlyRate1hUSD = sum.HourlyRate1hUSD
	v.HourlyRate1hAvailable = sum.HourlyRate1hAvailable
	v.WeeklyTotalUSD = sum.WeeklyTotalUSD
	v.ActiveCount = sum.ActiveCount
	v.ThrottledCount = sum.ThrottledCount
	v.TotalCount = sum.TotalCount
	v.Truncated = res.Total > len(accounts)
	return v
}

// buildBuildTileView renders the build/release tile. The 24h release-bug
// count is read from the incidents snapshot — the same source as the
// incidents tile — so a flagged release surfaces both as a row in incidents
// and as a counter here. Snapshot failures collapse to a clean "n/a". The
// release-date/age fields are derived from the YYYY.DDD.N calver convention
// when present so the tile carries shipped-date signal even when the snapshot
// is unavailable (audit 20260514-inbox recommendation).
func buildBuildTileView(snap *kbeadsHealthSnapshot, now time.Time) healthBuildTileView {
	v := healthBuildTileView{
		AsOf:    now.UTC().Format(time.RFC3339),
		Version: version,
	}
	if date, ok := parseCalverDate(version); ok {
		v.ReleaseDate = date.Format("2006-01-02")
		v.ReleaseAge = fmtTimeAgo(date, now)
	}
	if snap == nil {
		return v
	}
	var d incidentsSnapshotData
	if err := json.Unmarshal(snap.Incidents.Data, &d); err != nil {
		return v
	}
	v.RecentReleaseBugs24h = len(d.RecentReleaseBugs)
	return v
}

// parseCalverDate parses a YYYY.DDD.N calver string (e.g. "2026.135.1") into
// the date YYYY-MM-DD that day-of-year DDD points at in UTC. Returns ok=false
// for "dev", empty strings, or anything that doesn't match the convention —
// the build tile then suppresses the release-date row rather than rendering
// "n/a".
func parseCalverDate(v string) (time.Time, bool) {
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	year, err := strconv.Atoi(parts[0])
	if err != nil || year < 2000 || year > 9999 {
		return time.Time{}, false
	}
	doy, err := strconv.Atoi(parts[1])
	if err != nil || doy < 1 || doy > 366 {
		return time.Time{}, false
	}
	return time.Date(year, 1, doy, 0, 0, 0, 0, time.UTC), true
}

// buildFleetTileView decodes the fleet tile. Snapshot or decode errors
// surface in the view's Error field.
func buildFleetTileView(snap *kbeadsHealthSnapshot, snapErr error, now time.Time) healthFleetTileView {
	v := healthFleetTileView{AsOf: now.UTC().Format(time.RFC3339)}
	if snapErr != nil {
		v.Error = "fleet snapshot unavailable"
		return v
	}
	var d fleetSnapshotData
	if err := json.Unmarshal(snap.Fleet.Data, &d); err != nil {
		v.Error = "fleet snapshot decode failed"
		return v
	}
	v.Alive = d.AgentsAlive
	v.Stuck = d.AgentsStuck
	v.Total = d.AgentsTotal
	// Idle is everything in the roster that is neither alive nor stuck —
	// in v1 this maps to roster eviction (the kbeads snapshot does not yet
	// expose an idle/parked count separately, see research §6 fleet row).
	if d.AgentsTotal > d.AgentsAlive+d.AgentsStuck {
		v.Idle = d.AgentsTotal - d.AgentsAlive - d.AgentsStuck
	}
	if age, stale := snapshotStaleAge(snap, now); stale {
		v.Stale = true
		v.StaleAge = fmtAgeSeconds(age.Seconds())
	}
	return v
}

// buildEventsTileView decodes the events tile. The 24h total comes from the
// tile meta `total_24h_count` (a JSON number rendered as float64). The 5m
// histogram is what the snapshot stores in `data.histogram`.
func buildEventsTileView(snap *kbeadsHealthSnapshot, snapErr error, now time.Time) healthEventsTileView {
	v := healthEventsTileView{AsOf: now.UTC().Format(time.RFC3339)}
	if snapErr != nil {
		v.Error = "events snapshot unavailable"
		return v
	}
	var d eventsSnapshotData
	if err := json.Unmarshal(snap.Events.Data, &d); err != nil {
		v.Error = "events snapshot decode failed"
		return v
	}
	v.WindowSeconds = d.WindowSeconds
	v.Histogram = d.Histogram
	for _, b := range d.Histogram {
		v.Total5m += b.Count
	}
	if raw, ok := snap.Events.Meta["total_24h_count"]; ok {
		switch x := raw.(type) {
		case float64:
			v.Total24h = int64(x)
		case int64:
			v.Total24h = x
		case int:
			v.Total24h = int64(x)
		}
	}
	if age, stale := snapshotStaleAge(snap, now); stale {
		v.Stale = true
		v.StaleAge = fmtAgeSeconds(age.Seconds())
	}
	return v
}

// healthIncidentCountLimit caps the daemon list calls that back the open-incident
// counters. We only need Total, so a small Limit keeps the round-trips cheap.
const healthIncidentCountLimit = 1

// buildIncidentsTileView decodes the incidents tile and renders relative
// timestamps for each row. The 24h lists come from the snapshot; the open-total
// counters come from daemon list queries scoped to label=release-bug /
// label=agent-crash. The opens give the tile a non-zero payload even when no
// new incidents landed in the last 24h (audit 20260514-inbox recommendation:
// "Make every tile carry a guaranteed minimum payload"). Daemon errors on the
// opens are non-fatal — the snapshot list and 24h counters still render.
func (s *Server) buildIncidentsTileView(ctx context.Context, snap *kbeadsHealthSnapshot, snapErr error, now time.Time) healthIncidentsTileView {
	v := healthIncidentsTileView{AsOf: now.UTC().Format(time.RFC3339)}
	if snapErr != nil {
		v.Error = "incidents snapshot unavailable"
		return v
	}
	var d incidentsSnapshotData
	if err := json.Unmarshal(snap.Incidents.Data, &d); err != nil {
		v.Error = "incidents snapshot decode failed"
		return v
	}
	v.WindowSeconds = d.WindowSeconds
	for _, p := range d.RecentReleaseBugs {
		v.ReleaseBugs = append(v.ReleaseBugs, healthIncidentRowView{
			ID:    p.ID,
			Title: p.Title,
			Ago:   fmtTimeAgo(p.CreatedAt, now),
		})
	}
	for _, p := range d.RecentAgentCrashes {
		v.AgentCrashes = append(v.AgentCrashes, healthIncidentRowView{
			ID:    p.ID,
			Title: p.Title,
			Ago:   fmtTimeAgo(p.CreatedAt, now),
		})
	}
	v.ReleaseBugs24h = len(v.ReleaseBugs)
	v.AgentCrashes24h = len(v.AgentCrashes)
	if s != nil && s.daemon != nil {
		if res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Kinds:    []string{"issue"},
			Statuses: []string{"open"},
			Labels:   []string{"release-bug"},
			Limit:    healthIncidentCountLimit,
		}); err == nil && res != nil {
			v.OpenReleaseBugs = res.Total
		}
		if res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Kinds:    []string{"issue"},
			Statuses: []string{"open"},
			Labels:   []string{"agent-crash"},
			Limit:    healthIncidentCountLimit,
		}); err == nil && res != nil {
			v.OpenAgentCrashes = res.Total
		}
	}
	if age, stale := snapshotStaleAge(snap, now); stale {
		v.Stale = true
		v.StaleAge = fmtAgeSeconds(age.Seconds())
	}
	return v
}

// buildDecisionsTileView decodes the decisions tile. The snapshot only
// exposes pending + next_3 today; the "recent resolutions (5m)" sub-view
// requested in the task description requires snapshot work that hasn't
// landed yet — until then, the tile renders the "next 3" list and a note.
func buildDecisionsTileView(snap *kbeadsHealthSnapshot, snapErr error, now time.Time) healthDecisionsTileView {
	v := healthDecisionsTileView{AsOf: now.UTC().Format(time.RFC3339)}
	if snapErr != nil {
		v.Error = "decisions snapshot unavailable"
		return v
	}
	var d decisionsSnapshotData
	if err := json.Unmarshal(snap.Decisions.Data, &d); err != nil {
		v.Error = "decisions snapshot decode failed"
		return v
	}
	v.Pending = d.Pending
	for _, p := range d.Next3 {
		v.Next = append(v.Next, healthDecisionRowView{
			ID:    p.ID,
			Title: p.Title,
			Age:   fmtAgeSeconds(p.AgeSeconds),
		})
	}
	if age, stale := snapshotStaleAge(snap, now); stale {
		v.Stale = true
		v.StaleAge = fmtAgeSeconds(age.Seconds())
	}
	return v
}

// healthBridgeSources is the canonical bridge → source-label mapping the
// bridges tile renders. The controller row has no `source:` label of its
// own; we proxy its liveness from the kbeads snapshot (snap.Now is the
// daemon's own clock, which the controller reads on every refresh) rather
// than querying for a controller-specific bead.
var healthBridgeSources = []struct {
	name  string
	label string
}{
	{name: "slack-bridge", label: "source:slack"},
	{name: "jira-bridge", label: "source:jira"},
	{name: "gitlab-bridge", label: "source:gitlab"},
}

// classifyBridgeAge maps a last_event_at age to the bridge row state +
// pill text used in the template. We treat a missing timestamp as
// "unknown" rather than "stale" so an empty source isn't reported as a
// failure mode.
func classifyBridgeAge(age time.Duration) string {
	switch {
	case age < healthBridgeWarnAfter:
		return healthBridgeStateOK
	case age < healthBridgeStaleAfter:
		return healthBridgeStateWarn
	default:
		return healthBridgeStateStale
	}
}

// buildBridgesTileView queries each bridge's source-labelled beads for the
// most recent CreatedAt as a proxy for `last_event_at`, and classifies the
// row as ok / slow / stale by age. The controller row is driven by the
// snapshot freshness — if snap is non-nil and not stale, the controller is
// reading the daemon, which is the most direct liveness signal we have
// without bespoke snapshot work. Daemon errors on a per-row query collapse
// to "unknown" for that row rather than failing the tile.
func (s *Server) buildBridgesTileView(ctx context.Context, snap *kbeadsHealthSnapshot, now time.Time) healthBridgesTileView {
	v := healthBridgesTileView{AsOf: now.UTC().Format(time.RFC3339)}
	for _, src := range healthBridgeSources {
		row := healthBridgeRowView{Name: src.name, State: healthBridgeStateUnknown}
		if s != nil && s.daemon != nil {
			res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
				Labels: []string{src.label},
				Sort:   "-created_at",
				Limit:  healthBridgeSourceLookback,
			})
			if err == nil && res != nil && len(res.Beads) > 0 {
				if b := res.Beads[0]; b != nil && !b.CreatedAt.IsZero() {
					age := max(now.Sub(b.CreatedAt), 0)
					row.LastEventAgo = fmtTimeAgo(b.CreatedAt, now)
					row.LastEventID = b.ID
					row.State = classifyBridgeAge(age)
					row.OK = row.State == healthBridgeStateOK
				}
			}
			// GAP-D: the dark-hook deadman verdict overrides age-based state
			// for the webhook doors. A tripped gauge means "silently dark"
			// even when the last-event age reads only slow/stale/unknown; a
			// present live verdict (gauge 0) never downgrades a row (age
			// stands). Sources with no deadman gauge return Unknown and are
			// left entirely on the age classification. See webhook_dark.go.
			switch s.webhookDarkForSource(ctx, src.label) {
			case webhookDarkDark:
				row.State = healthBridgeStateDark
				row.OK = false
				row.Dark = true
			case webhookDarkLive:
				row.DarkChecked = true
			}
		}
		v.Sources = append(v.Sources, row)
	}
	// Controller row — driven by snapshot freshness.
	ctrl := healthBridgeRowView{Name: "controller", State: healthBridgeStateUnknown}
	if snap != nil && !snap.Now.IsZero() {
		_, stale := snapshotStaleAge(snap, now)
		ctrl.LastEventAgo = fmtTimeAgo(snap.Now, now)
		if stale {
			ctrl.State = healthBridgeStateWarn
		} else {
			ctrl.State = healthBridgeStateOK
			ctrl.OK = true
		}
	}
	v.Sources = append(v.Sources, ctrl)
	return v
}

// buildGardenersTileView aggregates schedule beads (legacy type=schedule and
// type=trigger label=trigger:schedule) and event-mode gardeners (type=trigger
// label=trigger:multi passing isGardenerBead) by their `role` field. Each row
// shows the enabled/total count for that role; rows sort by enabled count desc.
// Event-mode gardeners are also listed one per row with their watchdog cadence,
// last run and liveness (eventGardenerHealth). Cycle pass/fail history would
// require snapshot work and isn't surfaced yet — the Note field still calls
// that out.
func (s *Server) buildGardenersTileView(ctx context.Context, now time.Time) healthGardenersTileView {
	v := healthGardenersTileView{AsOf: now.UTC().Format(time.RFC3339)}
	schedRes, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"schedule"},
		Limit: healthGardenersTileLimit,
	})
	if err != nil {
		v.Error = "schedule list unavailable"
		return v
	}
	// One trigger query for both shapes: trigger:schedule (timed) and
	// trigger:multi (event mode). Each result is re-checked against its label
	// below, so the any-of filter can never widen what the tile counts.
	trigRes, _ := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:     []string{"trigger"},
		LabelsAny: []string{"trigger:schedule", "trigger:multi"},
		Limit:     healthGardenersTileLimit,
	})

	byRole := make(map[string]*healthGardenerRowView)
	var events []*beadsapi.BeadDetail
	addBead := func(b *beadsapi.BeadDetail, triggerKind, event bool) {
		if b == nil {
			return
		}
		f := b.Fields
		// Closed beads aren't live schedules; skip them from the tile so
		// "enabled" reflects what's actually firing right now.
		if statusOpenSortKey(b.Status) != 0 {
			return
		}
		role := strings.TrimSpace(f["role"])
		if role == "" {
			role = "—"
		}
		row, ok := byRole[role]
		if !ok {
			row = &healthGardenerRowView{Role: role}
			byRole[role] = row
		}
		row.TotalCount++
		if event {
			row.EventCount++
			v.EventTotal++
			events = append(events, b)
		}
		enabled := parseBool(f["enabled"])
		if triggerKind {
			// Trigger schedules default-enabled unless `enabled: "false"`,
			// matching the trigger-source loader.
			enabled = f["enabled"] != "false"
		}
		if enabled {
			row.EnabledCount++
			v.EnabledTotal++
		} else {
			v.DisabledTotal++
		}
	}
	if schedRes != nil {
		for _, b := range schedRes.Beads {
			addBead(b, false, false)
		}
	}
	if trigRes != nil {
		for _, b := range trigRes.Beads {
			switch {
			case b == nil || b.Type != "trigger":
			case hasLabel(b, "trigger:schedule"):
				addBead(b, true, false)
			case hasLabel(b, "trigger:multi") && isGardenerBead(b):
				addBead(b, true, true)
			}
		}
	}

	v.Roles = gardenerRoleRows(byRole)

	v.Event, v.EventStale = s.eventGardenerRows(ctx, events, now)
	if len(v.Event) > healthEventGardenersShown {
		v.EventMore = len(v.Event) - healthEventGardenersShown
		v.Event = v.Event[:healthEventGardenersShown]
	}

	if v.EnabledTotal+v.DisabledTotal == 0 {
		v.Note = "no triggers registered"
	} else {
		v.Note = "pass/fail history requires snapshot work"
	}
	return v
}

// gardenerRoleRows is the gardeners tile's role rows, by enabled count, then
// total, then role, capped to 6 rows so the tile keeps glance density without
// overflowing the grid cell when the deployment grows a long role tail.
func gardenerRoleRows(byRole map[string]*healthGardenerRowView) []healthGardenerRowView {
	rows := make([]healthGardenerRowView, 0, len(byRole))
	for _, r := range byRole {
		rows = append(rows, *r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].EnabledCount != rows[j].EnabledCount {
			return rows[i].EnabledCount > rows[j].EnabledCount
		}
		if rows[i].TotalCount != rows[j].TotalCount {
			return rows[i].TotalCount > rows[j].TotalCount
		}
		return rows[i].Role < rows[j].Role
	})
	const gardenersTileRoleLimit = 6
	if len(rows) > gardenersTileRoleLimit {
		rows = rows[:gardenersTileRoleLimit]
	}
	return rows
}

// Event-mode gardener liveness states on the health tile (gd-fL4PM5z8kc).
const (
	eventGardenerStateOK        = "ok"        // ran within the watchdog window
	eventGardenerStateIdle      = "idle"      // has not run lately, but no ready work waits
	eventGardenerStateStale     = "stale"     // has not run lately and ready work waits
	eventGardenerStateUnchecked = "unchecked" // has not run lately; ready work could not be checked
	eventGardenerStatePaused    = "paused"    // enabled=false
)

// healthEventGardenersShown caps the event-gardener list on the tile.
const healthEventGardenersShown = 8

// healthEventReadyProbeLimit caps the daemon probes (in-flight run, ready work) one tile render
// issues. A probe is only sent for an enabled event gardener that has not run
// within its window, so in steady state this is the handful of quiet ones; the
// cap bounds the worst case (every gardener quiet) for a tile that refreshes
// every 30s. Gardeners past the cap read "unchecked", never "stale".
const healthEventReadyProbeLimit = 25

// eventGardenerNoWatchdogWindow is the recency window for an event gardener
// with no fallback-watchdog cron entry (a plan gardener bound in event mode
// carries none by default, ka-0000008237). It fires only on subtree activity,
// so there is no cadence to be late against; a day without a run while ready
// work waits is the signal that no event is coming to wake it.
const eventGardenerNoWatchdogWindow = 24 * time.Hour

// eventGardenerWindow is how long an enabled event gardener may go without a
// run before the tile asks whether ready work is waiting. It is the watchdog
// cron's interval (never a 30-minute timed cadence: an event gardener has no
// cron field) plus the cooldown and the same tolerance the gardener page's
// overdue check uses (gardenerOverdueTolerance).
func eventGardenerWindow(watchdogCron, tz string, cooldownMinutes int, now time.Time) time.Duration {
	cadence := eventGardenerNoWatchdogWindow
	if strings.TrimSpace(watchdogCron) != "" {
		if d, ok := cronInterval(watchdogCron, tz, now); ok {
			cadence = d
		}
	}
	return cadence + time.Duration(cooldownMinutes)*time.Minute + gardenerOverdueTolerance
}

// eventGardenerReadyScope returns the plan epic whose subtree an event
// gardener works, or "" when it has none (the gardener-epic:<id> label, else
// the legacy assign_bead field).
func eventGardenerReadyScope(b *beadsapi.BeadDetail) string {
	if epic := gardenermode.EpicOf(b.Labels); epic != "" {
		return epic
	}
	return strings.TrimSpace(b.Fields["assign_bead"])
}

// healthEventProbeTimeout bounds all the daemon probes (in-flight run, ready
// work) one tile refresh sends for its quiet event gardeners.
const healthEventProbeTimeout = 3 * time.Second

// healthEventProbeTTL is how long a probe answer is reused. The tile refreshes
// every 30s per open tab; a 60s TTL makes N tabs cost one probe per gardener
// per minute instead of 2N.
const healthEventProbeTTL = 60 * time.Second

// eventGardenerReadyTypes are the step types counted as ready work: the plan
// step types (steward digest stepTypes plus chore). Epics and molecules are
// containers, so an open phase epic under a plan is not work waiting on the
// gardener.
var eventGardenerReadyTypes = []string{"task", "bug", "feature", "chore"}

// eventGardenerProbeCache memoizes the tile's probe answers for
// healthEventProbeTTL: "ready:<epic>" → ready-step count, "live:<agent>" → 1
// when that agent bead is open/in_progress. Errors are never cached. nil-safe.
type eventGardenerProbeCache struct {
	mu      sync.Mutex
	entries map[string]eventGardenerProbeEntry
}

type eventGardenerProbeEntry struct {
	n       int
	expires time.Time
}

func newEventGardenerProbeCache() *eventGardenerProbeCache {
	return &eventGardenerProbeCache{entries: make(map[string]eventGardenerProbeEntry)}
}

func (c *eventGardenerProbeCache) get(key string, now time.Time) (int, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || now.After(e.expires) {
		return 0, false
	}
	return e.n, true
}

func (c *eventGardenerProbeCache) set(key string, n int, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Drop expired entries on write so the map stays bounded by the live set.
	for k, e := range c.entries {
		if now.After(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = eventGardenerProbeEntry{n: n, expires: now.Add(healthEventProbeTTL)}
}

// eventGardenerLastAgentID returns the gardener's most recent iteration agent:
// the engine's top-level last_agent_id stamp, else the gardener blob's.
func eventGardenerLastAgentID(b *beadsapi.BeadDetail) string {
	if id := strings.TrimSpace(b.Fields["last_agent_id"]); id != "" {
		return id
	}
	if def := bridge.ParseTriggerDef(b, "schedule"); def.Gardener != nil {
		return strings.TrimSpace(def.Gardener.LastAgentID)
	}
	return ""
}

// quietEventGardenerState judges an enabled event gardener that has not run
// within its window. last_run is stamped at spawn, so a long run that is still
// going looks quiet; it reads ok when its last agent bead is open or
// in_progress. Otherwise it is stale when ready steps wait in its plan subtree,
// idle when none do, and unchecked when that cannot be told (no subtree, probe
// cap, probe error or timeout). probes counts daemon calls, not cache hits.
func (s *Server) quietEventGardenerState(ctx context.Context, b *beadsapi.BeadDetail, window time.Duration, now time.Time, probes *int) (state, detail string) {
	if agentID := eventGardenerLastAgentID(b); agentID != "" {
		key := "live:" + agentID
		live, hit := s.eventProbeCache.get(key, now)
		if !hit && *probes < healthEventReadyProbeLimit {
			*probes++
			if a, err := s.daemon.GetBead(ctx, agentID); err == nil && a != nil {
				live = 0
				if a.Status == "open" || a.Status == "in_progress" {
					live = 1
				}
				s.eventProbeCache.set(key, live, now)
			}
		}
		if live == 1 {
			return eventGardenerStateOK, "run in flight (" + agentID + ")"
		}
	}

	epic := eventGardenerReadyScope(b)
	if epic == "" {
		return eventGardenerStateUnchecked, "no plan subtree to check for ready work"
	}
	key := "ready:" + epic
	ready, hit := s.eventProbeCache.get(key, now)
	if !hit {
		if *probes >= healthEventReadyProbeLimit {
			return eventGardenerStateUnchecked, "ready-work check skipped (probe cap)"
		}
		*probes++
		// Ready work = open plan steps with no open dependency, not
		// scheduled, in the gardener's plan subtree. Container types (epic,
		// molecule) are left out by the type list.
		res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Parent:           epic,
			Types:            eventGardenerReadyTypes,
			Statuses:         []string{"open"},
			NoOpenDeps:       true,
			ExcludeScheduled: true,
			CountOnly:        true,
			Limit:            1,
		})
		if err != nil || res == nil {
			return eventGardenerStateUnchecked, "ready-work check failed"
		}
		ready = res.Total
		s.eventProbeCache.set(key, ready, now)
	}
	if ready > 0 {
		return eventGardenerStateStale, fmt.Sprintf("%d ready in %s, no run in %s", ready, epic, shortDuration(window.Round(time.Minute)))
	}
	return eventGardenerStateIdle, "no ready work"
}

// eventGardenerRows builds the tile's event-gardener list and counts the stale
// ones. The liveness rule: an event gardener is healthy when it is enabled and
// either ran recently (within eventGardenerWindow of its last run, or of its
// creation if it has never run), or has a run still in flight, or has no ready
// work in its plan subtree. It is
// never stale merely for lacking a 30-minute cron. Rows sort stale first, then
// unchecked, then the healthy ones, then paused, each by id.
func (s *Server) eventGardenerRows(ctx context.Context, beads []*beadsapi.BeadDetail, now time.Time) ([]healthEventGardenerRowView, int) {
	out := make([]healthEventGardenerRowView, 0, len(beads))
	stale, probes := 0, 0
	// One deadline for every probe this refresh sends, so a slow daemon costs
	// the tile at most healthEventProbeTimeout; a probe that runs out of time
	// reads "unchecked", never "stale".
	probeCtx, cancel := context.WithTimeout(ctx, healthEventProbeTimeout)
	defer cancel()
	for _, b := range beads {
		f := b.Fields
		cron, tz := cronFromTriggers(f["triggers"])
		if tz == "" {
			tz = strings.TrimSpace(f["timezone"])
		}
		row := healthEventGardenerRowView{
			ID:       b.ID,
			Title:    b.Title,
			Role:     strings.TrimSpace(f["role"]),
			Enabled:  f["enabled"] != "false",
			Cadence:  "events only",
			Timezone: tz,
			LastRun:  "never",
		}
		if cron != "" {
			row.Cadence = "watchdog " + cron
		}
		lastRun := parseRFC3339OrZero(strings.TrimSpace(f["last_run"]))
		if !lastRun.IsZero() {
			row.LastRun = fmtTimeAgo(lastRun, now) + " ago"
			row.LastRunRFC = lastRun.UTC().Format(time.RFC3339)
		}
		window := eventGardenerWindow(cron, tz, gardenerCooldownMinutes(f), now)
		ref := lastRun
		if ref.IsZero() {
			ref = b.CreatedAt
		}
		switch {
		case !row.Enabled:
			row.State = eventGardenerStatePaused
		case !ref.IsZero() && now.Sub(ref) <= window:
			row.State = eventGardenerStateOK
		default:
			row.State, row.Detail = s.quietEventGardenerState(probeCtx, b, window, now, &probes)
			if row.State == eventGardenerStateStale {
				stale++
			}
		}
		out = append(out, row)
	}
	rank := map[string]int{
		eventGardenerStateStale:     0,
		eventGardenerStateUnchecked: 1,
		eventGardenerStateOK:        2,
		eventGardenerStateIdle:      2,
		eventGardenerStatePaused:    3,
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].State] != rank[out[j].State] {
			return rank[out[i].State] < rank[out[j].State]
		}
		return out[i].ID < out[j].ID
	})
	return out, stale
}

// fmtAgeSeconds formats a duration in seconds (as float64) into a compact
// label like "12s", "5m", "2h", "3d". Mirrors fmtTimeAgo's vocabulary.
func fmtAgeSeconds(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	d := time.Duration(sec * float64(time.Second))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
