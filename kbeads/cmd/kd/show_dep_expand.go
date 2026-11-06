package main

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/model"
)

// ka-0000000053 (discoverability): a dependency edge recorded ONLY the two
// endpoint IDs and the edge type, so `kd show <id> --json` told a caller THAT
// it depends on ka-Y but nothing ABOUT ka-Y. Every dependency-frontier scan
// ("are my blockers closed yet?") therefore paid one extra round-trip PER EDGE
// just to read a neighbor's status — the N+1 loop the CED inspector observed.
// Denormalizing the neighbor's status and title onto the edge answers the
// frontier question from a single `kd show` (ka-0000005454).

// depEdgeView is one dependency edge denormalized with summary fields of the
// bead it points at. The edge itself is EMBEDDED rather than copied field by
// field, so every key an existing consumer reads (bead_id, depends_on_id,
// type, created_at, created_by, metadata, operated_by…) promotes unchanged and
// automatically keeps pace with model.Dependency. The added keys are omitempty
// and share the depends_on_ prefix of the id they describe, so an edge whose
// neighbor could not be resolved encodes byte-identically to a bare
// model.Dependency — old key set ⊆ new key set, always.
type depEdgeView struct {
	*model.Dependency
	// DependsOnStatus / DependsOnTitle describe the bead named by
	// DependsOnID, NOT this edge. Empty when the neighbor lookup was
	// skipped, failed, or the caller cannot read that bead.
	DependsOnStatus model.Status `json:"depends_on_status,omitempty"`
	DependsOnTitle  string       `json:"depends_on_title,omitempty"`

	// ID / Status / Title are ADDITIVE ALIASES of the three depends_on_*
	// keys above, carrying byte-identical values (pinned by test). The
	// depends_on_ prefix names the edge's own column, but a caller reading
	// this sub-object is describing the NEIGHBOUR, so the conventional guess
	// is the unprefixed `.dependencies[] | .id/.status/.title` — the same
	// vocabulary the top-level bead object uses for exactly these three
	// concepts. That guess returned a SILENT jq null (an absent key is not an
	// error), so agents burned guess→null→inspect-raw→re-project round-trips
	// reverse-engineering the sub-object schema (ka-0000002877, observed live
	// in session ka-0000008257). The depends_on_* keys stay canonical — every
	// existing consumer is untouched; this is a pure superset.
	ID     string       `json:"id"`
	Status model.Status `json:"status,omitempty"`
	Title  string       `json:"title,omitempty"`
}

// depExpandBatchSize chunks the neighbor lookup to stay under the daemon's
// per-request /v1/batch/beads cap (batchGetMaxIDs = 500). A bead with more
// forward edges than that is pathological, but an over-cap request 400s and
// would silently cost the WHOLE bead its enrichment, so we chunk rather than
// gamble. Kept below the server cap deliberately: a client must never outrun
// an older daemon mid-rolling-deploy.
const depExpandBatchSize = 500

// expandDepEdges wraps each edge with its neighbor's status and title, using
// ONE batch round-trip for all edges (chunked) instead of one per edge — the
// whole point of the denormalization. It is the single source of the expanded
// edge slice for both the --json and the text surface, so the two cannot drift.
//
// Best-effort by contract, exactly like countParentChildChildren: a nil
// client, a transport error, or a neighbor the caller cannot read yields a
// wrapper with the summary fields left empty rather than an error. `kd show`
// must never fail — or lose its primary output — because an auxiliary join
// did. Returns nil for an empty edge list so a leaf bead costs no round-trip.
func expandDepEdges(ctx context.Context, edges []*model.Dependency) []*depEdgeView {
	if len(edges) == 0 {
		return nil
	}

	neighbors := fetchDepNeighbors(ctx, edges)

	views := make([]*depEdgeView, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		v := &depEdgeView{Dependency: e}
		if n := neighbors[e.DependsOnID]; n != nil {
			v.DependsOnStatus = n.Status
			v.DependsOnTitle = n.Title
		}
		// Mirror the three neighbour-describing keys under their unprefixed
		// alias names. Assigned from v (not n) so the alias tracks the
		// canonical field even on the un-enriched path, where n is nil and
		// both sides stay empty together.
		v.ID, v.Status, v.Title = v.DependsOnID, v.DependsOnStatus, v.DependsOnTitle
		views = append(views, v)
	}
	return views
}

// depEdgeTitleWidth caps the neighbor title on the text "Depends On:" line so
// a long epic title cannot wrap the section into unreadability.
const depEdgeTitleWidth = 60

// formatDepEdgeLine renders one text-view "Depends On:" entry. The neighbor's
// status and title are APPENDED to the original "<id> (<type>)" text rather
// than folded into it, so the id stays field 1 and "(type)" field 2 for
// anything already slicing this line with awk/cut. An un-enriched edge (join
// skipped or failed) formats byte-identically to the pre-denormalization line.
func formatDepEdgeLine(d *depEdgeView) string {
	line := fmt.Sprintf("%s (%s)", d.DependsOnID, d.Type)
	if d.DependsOnStatus != "" {
		line += fmt.Sprintf(" [%s]", d.DependsOnStatus)
	}
	if d.DependsOnTitle != "" {
		line += " " + truncateDisplay(d.DependsOnTitle, depEdgeTitleWidth)
	}
	return line
}

// fetchDepNeighbors resolves the beads the edges point at, keyed by ID. IDs
// are de-duplicated first: two edges of different types between the same pair
// (a blocks edge AND a parent-child edge) are common and must not be fetched
// twice. Returns a nil map on any failure — every caller reads it with a map
// index, which yields the zero value for a missing key, so a nil map degrades
// to "no enrichment" without a nil check.
func fetchDepNeighbors(ctx context.Context, edges []*model.Dependency) map[string]*model.Bead {
	if beadsClient == nil {
		return nil
	}

	ids := make([]string, 0, len(edges))
	seen := make(map[string]bool, len(edges))
	for _, e := range edges {
		if e == nil || e.DependsOnID == "" || seen[e.DependsOnID] {
			continue
		}
		seen[e.DependsOnID] = true
		ids = append(ids, e.DependsOnID)
	}
	if len(ids) == 0 {
		return nil
	}

	out := make(map[string]*model.Bead, len(ids))
	for start := 0; start < len(ids); start += depExpandBatchSize {
		end := min(start+depExpandBatchSize, len(ids))
		results, err := beadsClient.BatchGetBeads(ctx, ids[start:end])
		if err != nil {
			// Partial enrichment beats none: keep whatever earlier chunks
			// resolved rather than discarding the map.
			break
		}
		for _, r := range results {
			// A not-found / unreadable neighbor comes back with r.Error set
			// and a nil bead; skip it so its edge stays un-enriched.
			if r.Bead != nil {
				out[r.ID] = r.Bead
			}
		}
	}
	return out
}
