package metrics

// advice_catalog.go emits the advice-catalog size gauges — the controller-side
// half of the Phase-1 "measure the audit" instrumentation from the
// context-crisis plan (ka-0000003733 / ka-0000006708). Where `gb advice audit`
// is the on-demand CLI form, these gauges make corpus growth visible in
// Grafana over time, so the Part-A advice cut has a live regression floor
// instead of a one-off number in a doc.

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// adviceCatalogLabels dimension the catalog gauges by the archetype the
// matched pool is simulated for (project, role) and the targeting scope the
// matched beads carry (global / project / role / agent). Summing over scope
// gives the archetype's total matched catalog — the number the ≤18k-token
// Part-A budget applies to.
var adviceCatalogLabels = []string{"project", "role", "scope"}

// newAdviceBroadcastGauge constructs the per-project broadcast-bead gauge:
// how many open advice beads target a whole project with no role:/agent:
// narrowing, split by delivery status ("delivered" = carries the
// scope:project-broadcast opt-in, "denied" = dropped by the delivery path).
// A rising delivered count means every agent of that project pays for more
// broadcast advice; any denied count is authoring debt `gb advice lint`
// flags. Phase 3 of the context-crisis plan (ka-0000003733 / ka-0000004186).
func newAdviceBroadcastGauge() *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "advice_broadcast_beads",
		Help:      "Open bare-project broadcast advice beads per project, by delivery status (delivered = scope:project-broadcast opt-in, denied = dropped by delivery). Broadcast beads reach every agent in the project (ka-0000003733 Phase 3).",
	}, []string{"project", "status"})
}

// adviceCorpusCreationWindow is the trailing window over which gardenio_advice_
// created_30d / _closed_30d count. 30 days matches how the corpus growth was
// characterised in the R3 root-cause (~110 gross/month), so the gauge reads as
// "advice created this month" and net growth = created − closed.
const adviceCorpusCreationWindow = 30 * 24 * time.Hour

// newAdviceCorpusGauges constructs the whole-pool corpus-size + churn gauges
// that feed the R4 tripwire (ka-0000002665 / ka-0000006409). Split out of New so
// the field wiring stays readable.
func newAdviceCorpusGauges() (openTotal, supersededOpen, created30d, closed30d prometheus.Gauge) {
	openTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "advice_open_total",
		Help:      "Open, real (non-fixture, non-expired) advice beads across the whole corpus. The number the R1 500-pool cap and the R4 tripwire watch (advice-budget hardening, ka-0000002665).",
	})
	supersededOpen = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "advice_superseded_open",
		Help:      "Open advice beads that another open advice bead explicitly supersedes — hidden from loadouts but still consuming the pool cap. The compaction backlog the advice sweeper drains (R3 ka-0000006409); should trend to ~0.",
	})
	created30d = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "advice_created_30d",
		Help:      "Advice beads created in the trailing 30 days (gross monthly creation). Net corpus growth = advice_created_30d − advice_closed_30d (feeds the R4 tripwire).",
	})
	closed30d = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "advice_closed_30d",
		Help:      "Advice beads closed in the trailing 30 days (expiry + supersession compaction + manual retirement). Net corpus growth = advice_created_30d − advice_closed_30d.",
	})
	return openTotal, supersededOpen, created30d, closed30d
}

// collectAdviceCorpus publishes the whole-pool corpus-size + churn gauges from
// the same open-advice snapshot the catalog scrape already fetched, plus two
// cheap count-only queries for the trailing-30d creation/close churn. Separated
// from the per-archetype catalog audit because it measures the ENTIRE corpus,
// not a single agent's simulated loadout.
func (c *Collector) collectAdviceCorpus(ctx context.Context, openAdvice []*beadsapi.BeadDetail) {
	// Open pool = the canonical read-path definition (ListOpenAdvice): open,
	// non-expired, non-fixture, non-hippo-mirror. Matches what the cap counts.
	real := make([]*beadsapi.BeadDetail, 0, len(openAdvice))
	for _, b := range openAdvice {
		if b == nil || b.Status == "closed" || advice.IsExpired(b) || advice.IsFixture(b) || advice.IsHippoMirror(b) {
			continue
		}
		real = append(real, b)
	}
	c.adviceOpenTotal.Set(float64(len(real)))
	c.adviceSupersededOpen.Set(float64(len(advice.SupersededOpenIDs(real))))

	cutoff := time.Now().Add(-adviceCorpusCreationWindow)
	if n, err := c.countAdvice(ctx, beadsapi.ListBeadsQuery{
		Types:        []string{"advice"},
		CreatedAfter: cutoff,
		CountOnly:    true,
	}); err != nil {
		c.logger.Warn("failed to count advice created in window", "error", err)
		c.scrapeErrors.Inc()
	} else {
		c.adviceCreated30d.Set(float64(n))
	}
	if n, err := c.countAdvice(ctx, beadsapi.ListBeadsQuery{
		Types:       []string{"advice"},
		Statuses:    []string{"closed"},
		ClosedAfter: cutoff,
		CountOnly:   true,
	}); err != nil {
		c.logger.Warn("failed to count advice closed in window", "error", err)
		c.scrapeErrors.Inc()
	} else {
		c.adviceClosed30d.Set(float64(n))
	}
}

// countAdvice runs a count-only advice query and returns the Total.
func (c *Collector) countAdvice(ctx context.Context, q beadsapi.ListBeadsQuery) (int, error) {
	res, err := c.querier.ListBeadsFiltered(ctx, q)
	if err != nil {
		return 0, err
	}
	return res.Total, nil
}

// newAdviceCatalogGauges constructs the catalog gauge vectors. Split out of
// New so the field wiring stays readable.
func newAdviceCatalogGauges() (bytes, beads, renderBytes *prometheus.GaugeVec) {
	bytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "advice_catalog_bytes",
		Help:      "Matched advice-catalog description bytes per (project, role) archetype, by targeting scope. Simulated through the real delivery pipeline (MatchesSubscriptions + supersession). Divide by 4 for ~tokens; the Part-A budget is 18k tokens/archetype (ka-0000003733).",
	}, adviceCatalogLabels)
	beads = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "advice_catalog_beads",
		Help:      "Matched advice-bead count per (project, role) archetype, by targeting scope. Same simulation as gardenio_advice_catalog_bytes.",
	}, adviceCatalogLabels)
	// Not dimensioned by scope: the abbreviated byte budget applies to the
	// archetype's WHOLE non-inline matched pool, so this is the per-(project,
	// role) render demand the R4 tripwire compares to ~80% of the 60000-byte
	// budget (ka-0000002748).
	renderBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "advice_prime_render_bytes",
		Help:      "Abbreviated-render demand per (project, role) archetype: bytes the non-inline matched advice emits through the real gb prime abbreviated rendering (advice.AbbrevRenderBytes) — what accumulates against the 60000-byte abbreviated budget. The render half of the R4 tripwire (ka-0000002748); alert crossing ~80% (48000). Distinct from gardenio_advice_catalog_bytes (full-description catalog churn).",
	}, []string{"project", "role"})
	return bytes, beads, renderBytes
}

// collectAdviceCatalog audits the open advice corpus against the archetype
// grid derived from the corpus's own project/role labels and publishes the
// per-archetype matched bytes/bead-count gauges. It reuses the exact same
// audit pipeline as `gb advice audit` (advice.AuditCatalog), so the dashboard
// number and the CLI number can never diverge.
func (c *Collector) collectAdviceCatalog(ctx context.Context) {
	result, err := c.querier.ListAllBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"advice"},
		Statuses: []string{"open"},
	})
	if err != nil {
		c.logger.Warn("failed to list advice beads for catalog metrics", "error", err)
		c.scrapeErrors.Inc()
		return
	}

	// Derive the archetype grid from the real fleet (agent beads over a recent
	// window), not the advice-corpus cross-product that measured 144 phantom
	// archetypes while the highest-volume real pairs went unmeasured
	// (ka-0000002153). A fleet query failure is non-fatal: fall back to no
	// archetypes so the broadcast gauge below still publishes.
	archetypes := c.deriveFleetArchetypes(ctx)
	audits := advice.AuditCatalog(result.Beads, archetypes)

	// Reset so retired projects/roles/scopes disappear on the next scrape.
	c.adviceCatalogBytes.Reset()
	c.adviceCatalogBeads.Reset()
	c.advicePrimeRenderBytes.Reset()

	for _, a := range audits {
		for scope, st := range a.ByScope {
			c.adviceCatalogBytes.WithLabelValues(a.Project, a.Role, scope).Set(float64(st.Bytes))
			c.adviceCatalogBeads.WithLabelValues(a.Project, a.Role, scope).Set(float64(st.Beads))
		}
		// Abbreviated-render demand for the whole archetype (not per scope) —
		// the R4 tripwire watches this against ~80% of the 60000-byte budget.
		c.advicePrimeRenderBytes.WithLabelValues(a.Project, a.Role).Set(float64(a.RenderBytes))
	}

	c.collectAdviceBroadcast(result.Beads)
	c.collectAdviceCorpus(ctx, result.Beads)
}

// deriveFleetArchetypes returns the real (project, role) archetype grid for the
// current scrape WITHOUT ever running the agent-bead walk on the scrape
// goroutine. It returns the cached grid immediately; on a cache miss or a grid
// older than fleetArchetypeTTL it kicks off a background refresh (single-flight)
// and still returns the last good grid (nil on the very first, cold scrape,
// which then publishes no per-archetype series that one cycle — preferable to
// the phantom advice-corpus cross-product, ka-0000002153).
//
// Why async and not just paged: the paged walk (advice.FleetArchetypesPaged)
// already bounds peak memory to one page, so it no longer OOMs inline. But the
// 15m TTL cannot cover the FIRST scrape after every restart — that is always a
// cache miss — and doing the walk inline there is exactly where the unbounded
// fetch used to kill the process (ka-0000006669). Refreshing off the scrape
// goroutine means a cold scrape can never do heavy work on the hot path, paged
// or not: defense-in-depth against any future regrowth of that per-scrape cost.
func (c *Collector) deriveFleetArchetypes(ctx context.Context) []advice.Archetype {
	now := time.Now()

	c.fleetMu.Lock()
	defer c.fleetMu.Unlock()
	if !c.fleetArchetypesAt.IsZero() && now.Sub(c.fleetArchetypesAt) < fleetArchetypeTTL {
		return c.fleetArchetypes
	}
	// Stale or never fetched: refresh off the scrape goroutine. fleetRefreshing
	// is the single-flight guard so overlapping scrapes launch only one walk.
	if !c.fleetRefreshing {
		c.fleetRefreshing = true
		go c.refreshFleetArchetypes(ctx)
	}
	return c.fleetArchetypes // last good grid, or nil if never fetched
}

// refreshFleetArchetypes performs the bounded, PAGED agent-bead walk and updates
// the archetype cache. It is the ONLY place the walk runs — deriveFleetArchetypes
// only ever triggers it on a background goroutine — so a cold scrape never does
// the walk inline (ka-0000006669). It is self-contained, so a test may call it
// synchronously to prime the cache.
//
// advice.FleetArchetypesPaged folds one page at a time and drops it before the
// next fetch, so peak memory is one page regardless of fleet size — never the
// whole 66 MiB, 8.8k-bead window the old single-shot Limit:20000 fetch
// materialized. A walk failure keeps the last good grid rather than publishing a
// partial one that would silently drop real archetypes from the audit.
func (c *Collector) refreshFleetArchetypes(ctx context.Context) {
	defer func() {
		c.fleetMu.Lock()
		c.fleetRefreshing = false
		c.fleetMu.Unlock()
	}()

	now := time.Now()
	archetypes, err := advice.FleetArchetypesPaged(ctx, c.querier, now)
	if err != nil {
		c.logger.Warn("failed to list agent beads for archetype derivation", "error", err)
		c.scrapeErrors.Inc()
		return // keep last good grid
	}

	c.fleetMu.Lock()
	c.fleetArchetypes = archetypes
	c.fleetArchetypesAt = now
	c.fleetMu.Unlock()
}

// collectAdviceBroadcast publishes the per-project broadcast-bead counts from
// the same corpus snapshot as the catalog gauges. A bead broadcasting to
// several projects counts once per project.
func (c *Collector) collectAdviceBroadcast(beads []*beadsapi.BeadDetail) {
	c.adviceBroadcastBeads.Reset()
	for _, b := range beads {
		if b == nil || b.Status == "closed" || advice.IsExpired(b) || advice.IsFixture(b) || advice.IsHippoMirror(b) {
			continue
		}
		if !advice.IsProjectBroadcast(b.Labels) {
			continue
		}
		status := "denied"
		if advice.HasBroadcastOptIn(b.Labels) {
			status = "delivered"
		}
		for _, p := range advice.BroadcastProjects(b.Labels) {
			c.adviceBroadcastBeads.WithLabelValues(p, status).Inc()
		}
	}
}
