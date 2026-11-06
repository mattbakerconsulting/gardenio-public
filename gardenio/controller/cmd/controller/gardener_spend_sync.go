package main

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planspend"
)

// gardenerSpendField is the gardener trigger/schedule bead field that holds the
// maintained per-gardener spend roll-up: the summed authoritative total_cost_usd
// over the gardener's own iteration runs PLUS the crew (and crew-of-crew) those
// runs transitively spawned. gardenerSpendSyncedAtField stamps the RFC3339 time
// of the last successful roll-up so the viewer can age-out a stale figure.
const (
	gardenerSpendField         = planspend.GardenerFieldUSD
	gardenerSpendSyncedAtField = planspend.GardenerFieldSyncedAt
)

// gardenerSpendSyncInterval is how often the roll-up is recomputed. The
// authoritative per-session cost is only written at session close (see
// sessionSpendReconciler), so a gardener's true spend changes at most once per
// crew/iteration close; a 10-minute cadence keeps the surfaced figure fresh
// without hammering the daemon with the per-gardener BFS every few seconds.
const gardenerSpendSyncInterval = 10 * time.Minute

// gardenerSpendHeartbeat is how old an unchanged figure on an open gardener may
// get before a pass re-stamps it anyway. It sits more than one interval below
// beads-viewer's gardenerSpendStaleAfter (45m), so /gardeners never marks a
// kept figure stale.
const gardenerSpendHeartbeat = 30 * time.Minute

// gardenerSpendListLimit bounds each gardener list call. The gardener population
// is operator-bounded (~dozens today); a gardener's iteration/crew fan-out is
// capped by gardenerSpendMaxBeads.
const gardenerSpendListLimit = 500

// gardenerSpendMaxBeads is a hard backstop on how many descendant agent beads a
// single gardener's roll-up will walk. A pathological gardener (hundreds of runs,
// each spawning wide crew trees) cannot make one sync iteration unbounded; when
// hit, the walk stops and the partial sum is written (the surfaced figure is a
// lower bound, which is the safe direction for "is this gardener wasteful").
const gardenerSpendMaxBeads = 5000

// gardenerSpendMaxDepth caps the crew BFS depth (iteration runs are depth 0,
// their direct crew depth 1, crew-of-crew depth 2, ...). Crew trees are shallow
// in practice; this guards against a spawned_by cycle (an agent name reused
// such that a later agent appears to spawn an earlier one) walking forever.
const gardenerSpendMaxDepth = 8

// gardenerSpendFieldTriggerID / gardenerSpendFieldScheduleID are the two field
// keys under which an iteration carries its gardener's id, mirroring the
// beads-viewer gardenerIterations dual-key union (bridge path stamps trigger_id,
// legacy scheduler path stamps schedule_id).
const (
	gardenerSpendFieldTriggerID  = "trigger_id"
	gardenerSpendFieldScheduleID = "schedule_id"
)

// gardenerSpendSync periodically rolls up each gardener's total agent spend —
// the gardener's iteration runs plus the crew they transitively spawned — onto
// the gardener trigger/schedule bead's gardener_spend_usd field, so /gardeners can
// surface and sort wasteful gardeners (operator request ka-0000003220).
//
// Why a controller-side loop (not derived at render time): the roll-up is a
// transitive BFS over potentially hundreds of agent beads per gardener. Doing it
// per page-load would make /gardeners slow and bursty; doing it on a slow cadence
// in the controller — the same process that already owns spendSync and the
// session-close reconciler — keeps the figure a cheap field read for the viewer.
//
// The summed input (each agent bead's total_cost_usd) is itself authoritative:
// it is the value sessionSpendReconciler writes at session close from real
// LiteLLM tag spend. This loop only AGGREGATES already-authoritative per-agent
// figures; it never queries LiteLLM itself.
type gardenerSpendSync struct {
	daemon *beadsapi.Client
	logger *slog.Logger
}

func newGardenerSpendSync(daemon *beadsapi.Client, logger *slog.Logger) *gardenerSpendSync {
	return &gardenerSpendSync{
		daemon: daemon,
		logger: logger.With("component", "gardener-spend-sync"),
	}
}

// RunLoop rolls up gardener spend every gardenerSpendSyncInterval until ctx is
// cancelled, with one immediate pass on start.
func (c *gardenerSpendSync) RunLoop(ctx context.Context) {
	c.syncAll(ctx)

	ticker := time.NewTicker(gardenerSpendSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.syncAll(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// syncAll recomputes and writes the spend roll-up for every gardener bead.
func (c *gardenerSpendSync) syncAll(ctx context.Context) {
	gardeners, err := c.listGardeners(ctx)
	if err != nil {
		c.logger.Warn("gardener spend sync: failed to list gardeners", "error", err)
		return
	}

	idx, err := c.loadAgentSpendIndex(ctx)
	if err != nil {
		c.logger.Warn("gardener spend sync: failed to list agents", "error", err)
		return
	}

	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339)
	for _, cap := range gardeners {
		spend, walked, capped := idx.rollupGardenerSpend(cap.ID)
		// gardener_spend_usd is a STRING-typed schema field (type:trigger /
		// type:schedule in kbeads config.go); the daemon rejects a raw float64
		// with HTTP 400 "gardener_spend_usd: must be a string", which silently
		// wedged every roll-up write (ka-0000006142). Format the figure as a
		// fixed-precision decimal string — the same shape the viewer's reader
		// (applyGardenerSpend → parseCostField → strconv.ParseFloat) expects.
		figure := strconv.FormatFloat(spend, 'f', 4, 64)
		if !gardenerSpendNeedsWrite(cap, figure, now) {
			continue
		}
		fields := map[string]any{
			gardenerSpendField:         figure,
			gardenerSpendSyncedAtField: stamp,
		}
		if err := c.daemon.UpdateBeadFieldsTyped(ctx, cap.ID, fields); err != nil {
			c.logger.Warn("gardener spend sync: update failed", "gardener", cap.ID, "title", cap.Title, "error", err)
			continue
		}
		c.logger.Debug("gardener spend rolled up", "gardener", cap.ID, "title", cap.Title,
			"spend_usd", spend, "agents_walked", walked, "capped", capped)
	}
}

// gardenerSpendNeedsWrite decides whether a fresh roll-up figure is written over
// what the gardener bead carries. Every write emits an "updated" event that wakes
// each plan gardener whose subtree holds the bead (gd-7smOFGjlD3), so an
// unchanged figure is re-written only as a heartbeat on an open gardener; on a
// closed gardener it is final.
func gardenerSpendNeedsWrite(b *beadsapi.BeadDetail, figure string, now time.Time) bool {
	if b.Fields[gardenerSpendField] != figure {
		return true
	}
	if b.Status == "closed" {
		return false
	}
	synced, err := time.Parse(time.RFC3339, strings.TrimSpace(b.Fields[gardenerSpendSyncedAtField]))
	return err != nil || now.Sub(synced) >= gardenerSpendHeartbeat
}

// listGardeners returns every gardener bead: a role:gardener-LABELLED or
// role=gardener-FIELDED type=trigger (modern) or type=schedule (legacy) bead.
// Mirrors loadGardenerRows' dual-type, dual-key union in beads-viewer so the
// loop maintains the field on exactly the beads the viewer renders. Deduped by
// id across all four (type × match) queries.
func (c *gardenerSpendSync) listGardeners(ctx context.Context) ([]*beadsapi.BeadDetail, error) {
	seen := map[string]bool{}
	var out []*beadsapi.BeadDetail
	collect := func(q beadsapi.ListBeadsQuery) error {
		res, err := c.daemon.ListBeadsFiltered(ctx, q)
		if err != nil {
			return err
		}
		if res == nil {
			return nil
		}
		for _, b := range res.Beads {
			if b == nil || seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			out = append(out, b)
		}
		return nil
	}
	// Both readers (syncAll and xcheck P5) use only the id, title, status and
	// the rollup fields; a gardener trigger's fields.gardener run log is most of
	// its row (gd-6PXLuR3juD).
	for _, typ := range []string{"trigger", "schedule"} {
		if err := collect(beadsapi.ListBeadsQuery{
			Types:  []string{typ},
			Labels: []string{"role:gardener"},
			Limit:  gardenerSpendListLimit,
		}.OnlyFields(gardenerSpendField, gardenerSpendSyncedAtField)); err != nil {
			return nil, err
		}
		if err := collect(beadsapi.ListBeadsQuery{
			Types:  []string{typ},
			Fields: map[string]string{"role": "gardener"},
			Limit:  gardenerSpendListLimit,
		}.OnlyFields(gardenerSpendField, gardenerSpendSyncedAtField)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// gardenerSpendWalkFields are the only agent-bead fields the roll-up reads:
// the cost, the name the next BFS round follows, and the three edges it is
// indexed on. The agent snapshot asks for just these instead of the ~8 KB row
// whose bulk is fields.prompt (gd-6PXLuR3juD).
var gardenerSpendWalkFields = []string{
	"total_cost_usd", "agent", "spawned_by",
	gardenerSpendFieldTriggerID, gardenerSpendFieldScheduleID,
}

// agentSpendIndex is one snapshot of every agent bead, indexed by the edges
// the gardener roll-up walks: owner (trigger_id or schedule_id value) and
// spawned_by. One snapshot serves every gardener of a pass, so a pass costs a
// few list pages instead of one list per agent name in every tree
// (gd-jd0BGkgmV5).
type agentSpendIndex struct {
	byOwner   map[string][]*beadsapi.BeadDetail
	bySpawner map[string][]*beadsapi.BeadDetail
}

// loadAgentSpendIndex pages through every agent bead (any status) once and
// indexes it. Field values match exactly, as the daemon's ?field= filter did.
func (c *gardenerSpendSync) loadAgentSpendIndex(ctx context.Context) (*agentSpendIndex, error) {
	res, err := c.daemon.ListAllBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"},
	}.OnlyFields(gardenerSpendWalkFields...))
	if err != nil {
		return nil, err
	}
	idx := &agentSpendIndex{
		byOwner:   map[string][]*beadsapi.BeadDetail{},
		bySpawner: map[string][]*beadsapi.BeadDetail{},
	}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		for _, key := range []string{gardenerSpendFieldTriggerID, gardenerSpendFieldScheduleID} {
			if v := b.Fields[key]; v != "" {
				idx.byOwner[key+"="+v] = append(idx.byOwner[key+"="+v], b)
			}
		}
		if v := b.Fields["spawned_by"]; v != "" {
			idx.bySpawner[v] = append(idx.bySpawner[v], b)
		}
	}
	return idx, nil
}

// rollupGardenerSpend computes one gardener's total spend: the summed
// total_cost_usd over its iteration agent beads (trigger_id/schedule_id ==
// gardenerID) plus a BFS over the crew those iterations spawned (and crew of
// crew), following the spawned_by → agent-name edge.
//
// Returns the summed spend, the number of agent beads walked, and whether the
// walk hit the gardenerSpendMaxBeads backstop (in which case the sum is a lower
// bound — the safe direction for surfacing wastefulness).
func (idx *agentSpendIndex) rollupGardenerSpend(gardenerID string) (spend float64, walked int, capped bool) {
	// visitedBeads dedupes agent beads across the whole walk (an agent could be
	// reachable by more than one edge); visitedNames dedupes the spawner-name
	// frontier so we never re-walk the same name. nextNames is the set of agent
	// names whose crew we still need to fetch.
	visitedBeads := map[string]bool{}
	visitedNames := map[string]bool{}
	nextNames := map[string]bool{}

	// Depth 0: the gardener's own iteration runs, matched under either ownership
	// key. accumulateGardenerSpend sums their cost, records their bead ids as
	// visited, and seeds nextNames with their agent names so depth 1 can find their crew.
	for _, key := range []string{gardenerSpendFieldTriggerID, gardenerSpendFieldScheduleID} {
		for _, b := range idx.byOwner[key+"="+gardenerID] {
			if accumulateGardenerSpend(b, visitedBeads, visitedNames, nextNames, &spend, &walked) && walked >= gardenerSpendMaxBeads {
				return spend, walked, true
			}
		}
	}

	// Depth 1..N: walk the spawned_by frontier. Each round takes the crew
	// spawned by the names discovered in the previous round, sums their cost,
	// and queues THEIR names for the next round, until the frontier empties or a
	// backstop trips.
	for depth := 1; depth <= gardenerSpendMaxDepth && len(nextNames) > 0; depth++ {
		frontier := nextNames
		nextNames = map[string]bool{}
		for name := range frontier {
			if visitedNames[name] {
				continue
			}
			visitedNames[name] = true
			for _, b := range idx.bySpawner[name] {
				if accumulateGardenerSpend(b, visitedBeads, visitedNames, nextNames, &spend, &walked) && walked >= gardenerSpendMaxBeads {
					return spend, walked, true
				}
			}
		}
	}
	return spend, walked, false
}

// accumulateGardenerSpend folds one agent bead into the running roll-up: it
// dedupes by bead id, adds the bead's total_cost_usd to spend, increments walked, and — when the
// bead carries an agent name not already seen — queues that name onto nextNames
// so the next BFS round fetches the crew it spawned. Returns true when the bead
// was newly counted (so the caller can check the bead backstop).
func accumulateGardenerSpend(b *beadsapi.BeadDetail, visitedBeads, visitedNames, nextNames map[string]bool, spend *float64, walked *int) bool {
	if b == nil || b.ID == "" || visitedBeads[b.ID] {
		return false
	}
	visitedBeads[b.ID] = true
	*walked++
	*spend += parseGardenerSpendCost(b.Fields["total_cost_usd"])
	if name := strings.TrimSpace(b.Fields["agent"]); name != "" && !visitedNames[name] {
		nextNames[name] = true
	}
	return true
}

// parseGardenerSpendCost parses a USD cost field, returning 0 on empty/invalid or
// negative. Mirrors the viewer's parseCostField so the roll-up sums the same
// figure the agent cards display.
func parseGardenerSpendCost(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}
