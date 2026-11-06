package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/model"
	"golang.org/x/sync/errgroup"
)

// healthSnapshotSchemaVersion is bumped when the response shape changes in a
// way that would break a cached client (added optional fields are not a
// schema bump).
//
// v2 (ka-0000003660): adds the `bridges` tile exposing per-bridge
// last_event_at, sourced from operator_aliases → events for each of the four
// canonical bridge identities (slack-bridge, jira-bridge, gitlab-bridge,
// controller). Older viewers ignore the extra top-level key.
//
// v3 (ka-0000002122): adds the `gardeners_cycles` tile rolling up the last
// healthGardenersCyclesLimit cycle outcomes (pass/fail) per gardener role,
// sourced from each schedule bead's `run_log` field. The viewer's
// gardeners tile uses this to render colored micro-bars per role.
const healthSnapshotSchemaVersion = 3

// healthGardenersCyclesLimit caps the number of cycle outcomes returned per
// gardener role. Six gives one row of micro-bars at a glance density that
// matches the rest of the /health page; the underlying run_log holds up to
// 50 per schedule (scheduler.MaxRunLogEntries) so we trim aggressively at
// the snapshot layer.
const healthGardenersCyclesLimit = 6

// healthGardenersCyclesScheduleLimit caps the number of schedule beads scanned
// for the gardeners_cycles tile. Matches the viewer's healthGardenersTileLimit
// so the tile and `/schedules` agree on the truncation point.
const healthGardenersCyclesScheduleLimit = 500

// healthBridgeAliases is the fixed list of canonical bridge identities the
// snapshot reports. The map preserves the display name ("slack-bridge")
// surfaced to operators while binding it to the operator-alias key
// ("system:slack-bridge") used to query the events table. The order of
// `healthBridgeOrder` is the order the bridges tile renders rows.
//
// The `controller` row uses the `fleet:gardenio` sentinel — controller-internal
// mutations stamp X-Operated-By with that alias per
// operated-by-transitivity-r3-dibs.md.
var healthBridgeAliases = map[string]string{
	"slack-bridge":  "system:slack-bridge",
	"jira-bridge":   "system:jira-bridge",
	"gitlab-bridge": "system:gitlab-bridge",
	"controller":    "fleet:gardenio",
}

var healthBridgeOrder = []string{
	"slack-bridge",
	"jira-bridge",
	"gitlab-bridge",
	"controller",
}

// healthSnapshotCacheTTL bounds how long a single computed snapshot is served
// to repeated callers. Matches the fleet-collector scrape TTL (ka-0000006525)
// so that the snapshot never lags fleet metrics by more than ~10s and so that
// rapid page reloads cost ~nothing.
const healthSnapshotCacheTTL = 10 * time.Second

// healthSnapshotCacheEntry is one cached payload plus the instant it was built.
type healthSnapshotCacheEntry struct {
	cachedAt time.Time
	payload  *HealthSnapshotResponse
}

// healthSnapshotCache is a scope-keyed, time-bounded cache for the snapshot
// payload. It is safe for concurrent use; callers serialise on builderMu when
// the cached entry is stale to avoid the thundering-herd that would otherwise
// fan a burst of pageviews into a burst of redundant store queries.
//
// The cache is keyed by the caller's read-scope (ka-0000008145): the decisions
// and incidents tiles now flow through scopeListFilter, so a restricted caller
// and an unrestricted operator produce DIFFERENT payloads. A single shared slot
// would let whoever populated it first serve their (possibly unscoped) view to
// everyone — re-opening the very cross-project leak this endpoint's tiles were
// hardened against. Scope keys derive only from the server-resolved token
// project + OIDC allowed_projects (see healthSnapshotScopeKey), so the keyspace
// is bounded by real project scopes, never by client-controlled input.
type healthSnapshotCache struct {
	mu        sync.RWMutex
	entries   map[string]healthSnapshotCacheEntry
	builderMu sync.Mutex
}

// healthTile is the per-tile envelope shaped to match the R5 chart-catalog
// contract (`{data, spec, meta}`). Tiles in the v1 health page are not yet
// real chart-catalog charts, but the shape makes the v1.1 migration a
// one-step swap rather than a redesign (research §11).
type healthTile struct {
	Data any            `json:"data"`
	Spec map[string]any `json:"spec"`
	Meta map[string]any `json:"meta"`
}

// HealthSnapshotResponse is the response body for GET /v1/health/snapshot.
// See gardenio/docs/research/beads-viewer-health-page.md §6+§7 and bead
// ka-0000000587 for the contract.
type HealthSnapshotResponse struct {
	SchemaVersion int       `json:"schema_version"`
	Now           time.Time `json:"now"`

	Fleet           healthTile `json:"fleet"`
	Decisions       healthTile `json:"decisions"`
	Events          healthTile `json:"events"`
	Incidents       healthTile `json:"incidents"`
	Kbeads          healthTile `json:"kbeads"`
	Bridges         healthTile `json:"bridges"`
	GardenersCycles healthTile `json:"gardeners_cycles"`
}

// gardenerCycleOutcome is one entry in a role's recent cycle history.
// Outcome is the raw `agent_state` string written by the scheduler
// (`done`, `failed`, `timeout`, `reaped`, etc.); Pass collapses that to
// the two-state pass/fail signal the viewer needs to render micro-bars.
// Pass = (Outcome == "done"). EndedAt is the agent's terminal-state
// timestamp; nil-valued entries (no ended_at recorded yet) are skipped
// upstream.
type gardenerCycleOutcome struct {
	Outcome  string    `json:"outcome"`
	Pass     bool      `json:"pass"`
	EndedAt  time.Time `json:"ended_at"`
	Schedule string    `json:"schedule,omitempty"`
}

// gardenersCycleRow is one row in the gardeners_cycles tile — a gardener role
// (`gardener-delivery`, `gardener-bugfix`, …) plus its most recent
// healthGardenersCyclesLimit cycle outcomes across every schedule bead that
// carries that role. Cycles are newest-first so the viewer can splay them
// left-to-right or stack them with the most recent on top.
type gardenersCycleRow struct {
	Role   string                 `json:"role"`
	Cycles []gardenerCycleOutcome `json:"cycles"`
}

// gardenersCyclesData is the `data` payload for the gardeners_cycles tile.
type gardenersCyclesData struct {
	Roles []gardenersCycleRow `json:"roles"`
}

// bridgeStatus is one row in the bridges tile.
type bridgeStatus struct {
	// Name is the operator-facing bridge name (slack-bridge, jira-bridge,
	// gitlab-bridge, controller).
	Name string `json:"name"`
	// Alias is the canonical operator alias (`kind:subject`) the row was
	// resolved against. Surfaced so viewers can deep-link an operator-chip
	// filter at the bridge identity.
	Alias string `json:"alias"`
	// LastEventAt is the most recent events.created_at attributed to this
	// bridge's operator alias. nil = no event has ever been attributed to
	// this bridge in the events table (the resolver may not have seen the
	// alias yet, or the bridge has been idle since the operators table was
	// introduced — both render as "unknown" upstream).
	LastEventAt *time.Time `json:"last_event_at"`
}

// bridgesData is the `data` payload for the bridges tile.
type bridgesData struct {
	Sources []bridgeStatus `json:"sources"`
}

// fleetData is the `data` payload for the fleet tile.
type fleetData struct {
	AgentsAlive int `json:"agents_alive"`
	AgentsStuck int `json:"agents_stuck"`
	AgentsTotal int `json:"agents_total"`
}

// decisionPreview is one item in the decision tile's `next_3` list.
type decisionPreview struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	AgeSeconds float64 `json:"age_seconds"`
}

// decisionsData is the `data` payload for the decisions tile.
type decisionsData struct {
	Pending int               `json:"pending"`
	Next3   []decisionPreview `json:"next_3"`
}

// eventsData is the `data` payload for the events tile.
type eventsData struct {
	WindowSeconds int                          `json:"window_seconds"`
	Total         int64                        `json:"total"`
	Histogram     []model.EventHistogramBucket `json:"histogram"`
}

// incidentPreview is one item in the incidents tile's recent lists.
type incidentPreview struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

// incidentsData is the `data` payload for the incidents tile.
type incidentsData struct {
	RecentReleaseBugs  []incidentPreview `json:"recent_release_bugs"`
	RecentAgentCrashes []incidentPreview `json:"recent_agent_crashes"`
	WindowSeconds      int               `json:"window_seconds"`
}

// kbeadsData is the `data` payload for the kbeads tile.
type kbeadsData struct {
	OK           bool      `json:"ok"`
	LastHealthAt time.Time `json:"last_health_at"`
	Error        string    `json:"error,omitempty"`
}

// handleHealthSnapshot handles GET /v1/health/snapshot.
//
// Returns a single bounded JSON payload composed of per-tile envelopes
// shaped like the R5 chart-catalog contract ({data, spec, meta}). Backs the
// beads-viewer /health page (epic ka-0000000152). A 10s in-memory cache
// guards repeated pageviews; concurrent callers on a stale entry serialise
// on a single rebuild to avoid thundering-herd queries.
func (s *BeadsServer) handleHealthSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.healthSnapshot == nil {
		s.healthSnapshot = &healthSnapshotCache{}
	}

	now := time.Now().UTC()
	scopeKey := s.healthSnapshotScopeKey(r.Context())

	if cached := s.healthSnapshot.read(scopeKey, now); cached != nil {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	s.healthSnapshot.builderMu.Lock()
	defer s.healthSnapshot.builderMu.Unlock()

	// Re-check after acquiring the builder lock: another goroutine may have
	// rebuilt this scope's entry while we were queued.
	if cached := s.healthSnapshot.read(scopeKey, now); cached != nil {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	payload, err := s.buildHealthSnapshot(r.Context(), now)
	if err != nil {
		writeServerError(r.Context(), w, "handleHealthSnapshot", err, "snapshot build failed")
		return
	}

	s.healthSnapshot.write(scopeKey, now, payload)
	writeJSON(w, http.StatusOK, payload)
}

// healthSnapshotScopeKey derives the cache key for the caller's read-scope. It
// combines the fleet-token project (ANDed scope) and the sorted OIDC
// allowed-projects (ORed scope) exactly as scopeListFilter consumes them, so
// two callers share a cache entry iff they see the identical scoped payload. An
// unrestricted caller (static token, admin, wildcard, unrestricted OIDC user)
// resolves to the fixed unscoped key. Both inputs are server-resolved, so the
// keyspace is bounded by real project scopes — never by client input.
func (s *BeadsServer) healthSnapshotScopeKey(ctx context.Context) string {
	var b strings.Builder
	b.WriteString("tok=")
	b.WriteString(TokenProjectFromContext(ctx))
	b.WriteString(";oidc=")
	if allowed := oidcAllowedProjects(ctx, s.store); allowed != nil {
		// A scoped principal with no projects must not share the unrestricted
		// caller's (empty) key, or it would be served the unrestricted payload.
		b.WriteString("[")
		sorted := append([]string(nil), allowed...)
		sort.Strings(sorted)
		b.WriteString(strings.Join(sorted, ","))
		b.WriteString("]")
	}
	return b.String()
}

// read returns the cached payload for scopeKey when it is fresh, else nil.
func (c *healthSnapshotCache) read(scopeKey string, now time.Time) *HealthSnapshotResponse {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[scopeKey]
	if !ok || entry.payload == nil {
		return nil
	}
	if now.Sub(entry.cachedAt) > healthSnapshotCacheTTL {
		return nil
	}
	return entry.payload
}

// write installs a freshly-built payload as the cache entry for scopeKey.
func (c *healthSnapshotCache) write(scopeKey string, now time.Time, payload *HealthSnapshotResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]healthSnapshotCacheEntry)
	}
	c.entries[scopeKey] = healthSnapshotCacheEntry{cachedAt: now, payload: payload}
}

// buildHealthSnapshot composes the response from the store + presence
// tracker. The tile builders are independent and run in parallel via
// errgroup — wall-clock used to be the sum of all of them (~1.5 s on
// gardenio-e2e), now it's the slowest single tile. errgroup short-circuits on
// the first store error so the cache is never populated with a partial
// payload.
func (s *BeadsServer) buildHealthSnapshot(ctx context.Context, now time.Time) (*HealthSnapshotResponse, error) {
	g, gctx := errgroup.WithContext(ctx)

	var (
		fleet           healthTile
		decisions       healthTile
		events          healthTile
		incidents       healthTile
		kbeads          healthTile
		bridges         healthTile
		gardenersCycles healthTile
	)

	g.Go(func() error {
		var err error
		fleet, err = s.buildFleetTile(gctx, now)
		return err
	})
	g.Go(func() error {
		var err error
		decisions, err = s.buildDecisionsTile(gctx, now)
		return err
	})
	g.Go(func() error {
		var err error
		events, err = s.buildEventsTile(gctx, now)
		return err
	})
	g.Go(func() error {
		var err error
		incidents, err = s.buildIncidentsTile(gctx, now)
		return err
	})
	g.Go(func() error {
		kbeads = s.buildKbeadsTile(gctx, now)
		return nil
	})
	g.Go(func() error {
		var err error
		bridges, err = s.buildBridgesTile(gctx, now)
		return err
	})
	g.Go(func() error {
		var err error
		gardenersCycles, err = s.buildGardenersCyclesTile(gctx, now)
		return err
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return &HealthSnapshotResponse{
		SchemaVersion:   healthSnapshotSchemaVersion,
		Now:             now,
		Fleet:           fleet,
		Decisions:       decisions,
		Events:          events,
		Incidents:       incidents,
		Kbeads:          kbeads,
		Bridges:         bridges,
		GardenersCycles: gardenersCycles,
	}, nil
}

// buildFleetTile counts alive vs stuck agents from the presence tracker.
// Stuck = Reaped (the reaper marked the agent dead after the dead-threshold
// expired). Alive = present and not reaped. The total comes from the store
// agent rollup so the operator-actionable comparison ("12 of 15 agents
// alive") is meaningful even when reaped agents have been evicted from the
// roster. That count reads only the active agent beads, so it is cheap per
// snapshot (gd-ipZ10t3oTo).
func (s *BeadsServer) buildFleetTile(ctx context.Context, now time.Time) (healthTile, error) {
	var alive, stuck int
	if s.Presence != nil {
		// Use a wide threshold so reaped (stuck) entries are still in the
		// roster — the reaper evicts after EvictAfter (default 30m), so a
		// stale_threshold > EvictAfter would drop them.
		for _, e := range s.Presence.Roster(0) {
			if e.Reaped {
				stuck++
			} else {
				alive++
			}
		}
	}

	total := alive + stuck
	if active, err := s.store.FleetActiveAgentsByProject(ctx); err == nil {
		t := 0
		for _, n := range active {
			t += n
		}
		// Prefer the store-side total when it's larger (covers the
		// evicted-but-still-an-agent-bead case).
		if t > total {
			total = t
		}
	}

	return healthTile{
		Data: fleetData{
			AgentsAlive: alive,
			AgentsStuck: stuck,
			AgentsTotal: total,
		},
		Spec: map[string]any{
			"kind":  "single_value",
			"unit":  "agents",
			"title": "Fleet liveness",
		},
		Meta: map[string]any{
			"as_of":  now,
			"source": "presence_tracker+fleet_rollup",
		},
	}, nil
}

// buildDecisionsTile reports the open decision queue: count + the three
// oldest pending decisions. Pending = open|in_progress (the resolution of
// a decision closes the bead — see http_decisions.go). Total comes from the
// COUNT(*) OVER() the same query already computes — no separate count call.
func (s *BeadsServer) buildDecisionsTile(ctx context.Context, now time.Time) (healthTile, error) {
	// Route through scopeListFilter so a project-bound fleet token or a
	// restricted OIDC user cannot read decision titles/IDs from projects it is
	// not authorized for (ka-0000008145). The decisions queue carries no
	// operator predicate, so unscoped it returns the oldest pending decisions
	// across EVERY project. An unrestricted caller is left untouched, preserving
	// the operator's fleet-wide dashboard. Mirrors the my-work-summary decisions
	// queue (mywork.go, the ka-0000008132 read-authz sweep).
	beads, total, err := s.store.ListBeads(ctx, s.scopeListFilter(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeDecision},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress},
		Sort:   "created_at",
		Limit:  3,
	}))
	if err != nil {
		return healthTile{}, err
	}

	previews := make([]decisionPreview, 0, len(beads))
	for _, b := range beads {
		previews = append(previews, decisionPreview{
			ID:         b.ID,
			Title:      b.Title,
			AgeSeconds: now.Sub(b.CreatedAt).Seconds(),
		})
	}

	return healthTile{
		Data: decisionsData{
			Pending: total,
			Next3:   previews,
		},
		Spec: map[string]any{
			"kind":  "list",
			"title": "Decisions waiting on operator",
		},
		Meta: map[string]any{
			"as_of":  now,
			"source": "beads(type=decision)",
		},
	}, nil
}

// buildEventsTile reports event volume across the last 5m and 24h. The 5m
// histogram is per-minute; the 24h tally is the sum over a per-hour bucket.
// The two windows answer "is the fleet doing work right now?" and "did
// anything weird happen overnight?" — both first-30s questions per the
// research doc.
func (s *BeadsServer) buildEventsTile(ctx context.Context, now time.Time) (healthTile, error) {
	// The health snapshot is an intentionally fleet-wide dashboard (its sibling
	// ListBeads tiles are likewise unscoped), so it counts events across every
	// project — pass the zero-value (unrestricted) EventReadScope to preserve
	// that behavior. The per-project gate (ka-0000006863) lives on the
	// caller-facing /v1/events[/histogram] handlers, not here.
	since5m := now.Add(-5 * time.Minute)
	hist5m, err := s.store.EventHistogram(ctx, since5m, now, "minute", model.EventReadScope{})
	if err != nil {
		return healthTile{}, err
	}
	if hist5m == nil {
		hist5m = []model.EventHistogramBucket{}
	}

	since24h := now.Add(-24 * time.Hour)
	hist24h, err := s.store.EventHistogram(ctx, since24h, now, "hour", model.EventReadScope{})
	if err != nil {
		return healthTile{}, err
	}

	var total24h int64
	for _, b := range hist24h {
		total24h += b.Count
	}

	return healthTile{
		Data: eventsData{
			WindowSeconds: 300,
			Total:         total24h,
			Histogram:     hist5m,
		},
		Spec: map[string]any{
			"kind":         "histogram",
			"x":            "t",
			"y":            "count",
			"window_label": "last 5m (per minute)",
			"title":        "Bead-event throughput",
		},
		Meta: map[string]any{
			"as_of":           now,
			"source":          "events_table",
			"total_24h_count": total24h,
		},
	}, nil
}

// buildIncidentsTile lists recent bug-type beads filed in the last 24h.
// Two flavours are surfaced separately: those flagged as release bugs
// (label `release-bug`) and recent agent crashes (label `agent-crash`).
// The label-based split is conservative: when a deployment uses different
// labels, both lists may be empty — which is itself useful signal.
func (s *BeadsServer) buildIncidentsTile(ctx context.Context, now time.Time) (healthTile, error) {
	since := now.Add(-24 * time.Hour)
	const incidentLimit = 5

	releaseBugs, err := s.listIncidents(ctx, "release-bug", since, incidentLimit)
	if err != nil {
		return healthTile{}, err
	}
	agentCrashes, err := s.listIncidents(ctx, "agent-crash", since, incidentLimit)
	if err != nil {
		return healthTile{}, err
	}

	return healthTile{
		Data: incidentsData{
			RecentReleaseBugs:  releaseBugs,
			RecentAgentCrashes: agentCrashes,
			WindowSeconds:      24 * 60 * 60,
		},
		Spec: map[string]any{
			"kind":  "list",
			"title": "Recent incidents (last 24h)",
		},
		Meta: map[string]any{
			"as_of":  now,
			"source": "beads(type=bug, label=release-bug|agent-crash)",
		},
	}, nil
}

func (s *BeadsServer) listIncidents(ctx context.Context, label string, since time.Time, limit int) ([]incidentPreview, error) {
	// Route through scopeListFilter so a restricted caller cannot read bug
	// titles/IDs from unauthorized projects (ka-0000008145). scopeListFilter
	// appends a project label to filter.Labels for a fleet token (ANDs, safe
	// alongside the incident `label`) and to filter.LabelsAny for a restricted
	// OIDC user (ORs the allowed projects); an unrestricted caller is untouched.
	beads, _, err := s.store.ListBeads(ctx, s.scopeListFilter(ctx, model.BeadFilter{
		Type:         []model.BeadType{model.TypeBug},
		Labels:       []string{label},
		CreatedAfter: &since,
		Sort:         "-created_at",
		Limit:        limit,
	}))
	if err != nil {
		return nil, err
	}
	previews := make([]incidentPreview, 0, len(beads))
	for _, b := range beads {
		previews = append(previews, incidentPreview{
			ID:        b.ID,
			Title:     b.Title,
			CreatedAt: b.CreatedAt,
		})
	}
	return previews, nil
}

// buildBridgesTile reports the last events.created_at attributed to each of
// the four canonical bridge identities. Resolves through operator_aliases so
// the lookup is keyed on the kind:subject the bridges actually stamp into
// X-Operated-By, not free-form actor strings. Bridges with no events ever
// recorded render with last_event_at = null; the viewer presents that as
// "unknown" so operators can distinguish "bridge not wired" from "bridge
// idle but recently seen". Source: bead ka-0000003660.
func (s *BeadsServer) buildBridgesTile(ctx context.Context, now time.Time) (healthTile, error) {
	aliases := make([]string, 0, len(healthBridgeOrder))
	for _, name := range healthBridgeOrder {
		aliases = append(aliases, healthBridgeAliases[name])
	}
	lastAt, err := s.store.LastEventAtByOperatorAliases(ctx, aliases)
	if err != nil {
		return healthTile{}, err
	}
	sources := make([]bridgeStatus, 0, len(healthBridgeOrder))
	for _, name := range healthBridgeOrder {
		alias := healthBridgeAliases[name]
		row := bridgeStatus{Name: name, Alias: alias}
		if t, ok := lastAt[alias]; ok {
			tt := t.UTC()
			row.LastEventAt = &tt
		}
		sources = append(sources, row)
	}
	return healthTile{
		Data: bridgesData{Sources: sources},
		Spec: map[string]any{
			"kind":  "list",
			"title": "Bridges & controller",
		},
		Meta: map[string]any{
			"as_of":  now,
			"source": "events JOIN operator_aliases (canonical bridge identities)",
		},
	}, nil
}

// scheduleRunLogEntry mirrors the shape of one entry in a schedule bead's
// `run_log` field, as written by the gardenio scheduler's trackLastAgentStatus.
// The scheduler owns the canonical type
// (gardenio/controller/internal/scheduler/runlog.go); we re-declare it here so
// kbeads does not import gardenio. Only the three fields the gardeners_cycles
// tile actually consumes are decoded — extra keys (started_at, duration_s,
// wrapup_summary, close_reason, agent) are ignored.
type scheduleRunLogEntry struct {
	EndedAt time.Time `json:"ended_at"`
	Outcome string    `json:"outcome"`
}

// scheduleFieldsForCycles is the minimal view of a schedule bead's `fields`
// JSON used by buildGardenersCyclesTile. We only need `role` and `run_log`;
// everything else (cron, project, enabled, etc.) is ignored.
type scheduleFieldsForCycles struct {
	Role   string `json:"role"`
	RunLog string `json:"run_log"`
}

// buildGardenersCyclesTile rolls up the last healthGardenersCyclesLimit cycle
// outcomes per gardener role across every open `type=schedule` bead. Source
// of truth is the `run_log` field that gardenio's scheduler writes on each
// schedule bead at terminal-state observation
// (gardenio/controller/internal/scheduler/runlog.go). Each entry's `outcome`
// is the raw agent_state (`done` / `failed` / `timeout` / `reaped` / …);
// pass = (outcome == "done").
//
// Schedules with no `role` field fall under the "—" placeholder role so the
// tile still surfaces them rather than dropping the cycle history. Closed
// schedule beads are excluded — the same convention the viewer's gardeners
// tile already uses for the enabled/total split, so the two surfaces agree
// on which schedules are "live".
//
// Failures to parse a single bead's run_log are logged-but-swallowed: a
// malformed run_log on one schedule must not poison the whole tile.
func (s *BeadsServer) buildGardenersCyclesTile(ctx context.Context, now time.Time) (healthTile, error) {
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.BeadType("schedule")},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress},
		Limit:  healthGardenersCyclesScheduleLimit,
	})
	if err != nil {
		return healthTile{}, err
	}

	byRole := make(map[string][]gardenerCycleOutcome)
	for _, b := range beads {
		if b == nil || len(b.Fields) == 0 {
			continue
		}
		var f scheduleFieldsForCycles
		if err := json.Unmarshal(b.Fields, &f); err != nil {
			continue
		}
		role := strings.TrimSpace(f.Role)
		if role == "" {
			role = "—"
		}
		raw := strings.TrimSpace(f.RunLog)
		if raw == "" {
			// No history yet; still register the role so a freshly-spawned
			// schedule renders an empty row rather than disappearing.
			if _, ok := byRole[role]; !ok {
				byRole[role] = nil
			}
			continue
		}
		var entries []scheduleRunLogEntry
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			continue
		}
		for _, e := range entries {
			if e.EndedAt.IsZero() {
				continue
			}
			byRole[role] = append(byRole[role], gardenerCycleOutcome{
				Outcome:  e.Outcome,
				Pass:     e.Outcome == "done",
				EndedAt:  e.EndedAt.UTC(),
				Schedule: b.ID,
			})
		}
	}

	rows := make([]gardenersCycleRow, 0, len(byRole))
	for role, cycles := range byRole {
		sort.SliceStable(cycles, func(i, j int) bool {
			return cycles[i].EndedAt.After(cycles[j].EndedAt)
		})
		if len(cycles) > healthGardenersCyclesLimit {
			cycles = cycles[:healthGardenersCyclesLimit]
		}
		rows = append(rows, gardenersCycleRow{Role: role, Cycles: cycles})
	}
	// Stable, role-name-sorted output so a viewer that doesn't re-sort lands
	// on a predictable order. Roles with more recent activity bubble first via
	// the secondary key on most-recent ended_at desc; ties fall back to name.
	sort.SliceStable(rows, func(i, j int) bool {
		var ti, tj time.Time
		if len(rows[i].Cycles) > 0 {
			ti = rows[i].Cycles[0].EndedAt
		}
		if len(rows[j].Cycles) > 0 {
			tj = rows[j].Cycles[0].EndedAt
		}
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return rows[i].Role < rows[j].Role
	})

	return healthTile{
		Data: gardenersCyclesData{Roles: rows},
		Spec: map[string]any{
			"kind":           "list",
			"title":          "Gardener cycles (last N pass/fail)",
			"cycles_per_row": healthGardenersCyclesLimit,
		},
		Meta: map[string]any{
			"as_of":  now,
			"source": "beads(type=schedule).fields.run_log",
		},
	}, nil
}

// buildKbeadsTile surfaces a fast self-check on the daemon. A failed Ping
// is reported as ok=false; the snapshot itself still returns 200 because
// the operator needs to *see* the failure mode in the tile.
func (s *BeadsServer) buildKbeadsTile(ctx context.Context, now time.Time) healthTile {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	data := kbeadsData{LastHealthAt: now, OK: true}
	if err := s.store.Ping(pingCtx); err != nil {
		data.OK = false
		data.Error = err.Error()
	}
	return healthTile{
		Data: data,
		Spec: map[string]any{
			"kind":  "single_value",
			"title": "kbeads daemon",
		},
		Meta: map[string]any{
			"as_of":  now,
			"source": "store.Ping",
		},
	}
}
