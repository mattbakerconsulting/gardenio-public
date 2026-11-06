package model

import "time"

// DependencyType categorizes the relationship between two beads.
// Well-known constants are provided below, but dependency types are extensible.
type DependencyType string

const (
	DepBlocks      DependencyType = "blocks"
	DepParentChild DependencyType = "parent-child"
	DepRelated     DependencyType = "related"
	DepDuplicates  DependencyType = "duplicates"
	DepSupersedes  DependencyType = "supersedes"
	DepRates       DependencyType = "rates"
	DepTriggeredBy DependencyType = "triggered-by"
	// DepChainsTo is the gardener-chaining / succession relationship: a
	// predecessor gardener hands the baton to an heir gardener on
	// goal-satisfied. The forward surface verb is `chains_to` (predecessor
	// → heir) and the inverse is `succeeds` (heir → predecessor, provenance
	// stamped at heir-creation time per the succession plan §2a). It is a
	// distinct type from DepTriggeredBy on purpose: triggered-by is
	// event/trigger provenance, whereas chains-to is the gardener-succession
	// edge a workflow is built from. See gardenio gardener-chaining design §3.1.
	DepChainsTo DependencyType = "chains-to"
)

// IsValid reports whether the dependency type is a non-empty string of at most 50 characters.
// Dependency types are extensible, so any non-empty value within the length limit is accepted.
func (d DependencyType) IsValid() bool {
	return len(d) > 0 && len(d) <= 50
}

// DepParentChildLegacy is the legacy alias for the parent-child hierarchy edge.
// A large body of edges in the live graph are typed literally "parent" (created
// before the type was canonicalized to DepParentChild, by early agents and the
// `static` importer). Both encode the SAME child-first orientation
// (deps.bead_id=child, deps.depends_on_id=parent), so for READ filters they are
// interchangeable. See ParentChildDepTypes and bug ka-0000008107.
const DepParentChildLegacy DependencyType = "parent"

// ParentChildDepTypes is the canonical set of deps.type values that all encode
// the parent-child hierarchy edge: the current canonical DepParentChild plus
// the legacy DepParentChildLegacy alias. It is the single source of truth for
// every READ surface that resolves "is X a parent-of / child-of Y" — the
// postgres --no-parent / --parent / subtree filters and `kd children --type`.
//
// Matching this set on reads is backward-compatible and needs NO data
// migration; the write path still emits only the canonical DepParentChild.
// Filed for ka-0000008107 (triage-intake --no-parent / children mismatch).
var ParentChildDepTypes = []string{string(DepParentChild), string(DepParentChildLegacy)}

// IsParentChildDepType reports whether t is any of the parent-child hierarchy
// aliases in ParentChildDepTypes. Read filters use this so a caller asking for
// "parent-child" also matches legacy "parent" edges (and vice-versa).
func IsParentChildDepType(t string) bool {
	for _, pc := range ParentChildDepTypes {
		if t == pc {
			return true
		}
	}
	return false
}

// DependencyEdge is a minimal (bead_id, depends_on_id) dependency edge — the
// dependent (BeadID, the child/blocked side) and its dependency (DependsOnID,
// the parent/blocker side) — for graph-shape analysis that does not need the
// full Dependency row. Used by the blocks-cycle audit (ka-0000007984).
type DependencyEdge struct {
	BeadID      string `json:"bead_id"`
	DependsOnID string `json:"depends_on_id"`
}

// Dependency represents a directional relationship between two beads.
type Dependency struct {
	BeadID      string         `json:"bead_id"`
	DependsOnID string         `json:"depends_on_id"`
	Type        DependencyType `json:"type"`
	CreatedAt   time.Time      `json:"created_at"`
	CreatedBy   string         `json:"created_by,omitempty"`
	Metadata    string         `json:"metadata,omitempty"`
	OperatedBy  string         `json:"operated_by,omitempty"`
	// OperatedByID is the operators(id) FK populated by the resolver during
	// the dual-write window (ADR §4.4). nil when the resolver did not match.
	OperatedByID *int64 `json:"operated_by_id,omitempty"`
	// BeadType is the type of the DEPENDENT bead (deps.bead_id) — the child on
	// a reverse (parent → children) walk. It is populated ONLY by the batch
	// reverse-dependency query (GetReverseDependenciesForBeads) via a cheap
	// beads-PK join, so a caller enumerating a parent's children can filter by
	// child type BEFORE hydrating each child's full record. Empty on every
	// other path (forward deps, single-bead deps), which callers must treat as
	// "unknown" and fall back to hydration — that keeps the field a pure,
	// backward-compatible optimization hint. Added for the /plans index
	// step-data walk, which discards ~68% of a plan's children (sessions,
	// messages, agents) but paid to hydrate them all (ka-0000003113).
	BeadType BeadType `json:"bead_type,omitempty"`
}

// OpenBlocker is one OPEN blocking edge, denormalized with summary fields of
// the bead doing the blocking — the answer to "why is this bead not ready?"
// without a follow-up lookup per edge.
//
// It is an ANSWER type, not an edge record: only genuinely-blocking edge types
// (blockingDepTypes) pointing at a non-terminal bead appear, so a caller never
// has to re-derive gate logic client-side. That re-derivation is the exact bug
// this type exists to kill — the audited triage agent hand-rolled
// IN("open","in_progress","deferred") and silently MISSED a blocker sitting in
// status "blocked" (ka-0000000945). The server already knows the right set;
// this ships it.
//
// Populated by the ListBeads --with-blockers enrichment; never stored.
type OpenBlocker struct {
	// ID / Status / Type / Title describe the BLOCKING bead.
	ID     string   `json:"id"`
	Status Status   `json:"status"`
	Type   BeadType `json:"type"`
	Title  string   `json:"title,omitempty"`
	// EdgeType is the dependency edge that ties the blocked bead to this
	// blocker (blocks / blocked-by / depends-on) — it describes the EDGE, not
	// the blocker, hence the distinct name from Type above.
	EdgeType DependencyType `json:"edge_type,omitempty"`
	// OutOfScope marks a blocker that lies OUTSIDE the caller's project label
	// scope. The blocker is still reported — it genuinely gates the dependent,
	// exactly as `--no-blockers` counts it — but its free-text Title is
	// redacted (left empty) so a project-bound token cannot read a
	// cross-project bead's title through the enrichment. Without this, a
	// cross-project blocker vanished from OpenBlockers entirely, so a gated
	// bead reported an EMPTY open_blockers[] and read as dispatchable —
	// directly contradicting `--no-blockers`, which had hidden that same row
	// (ka-0000007462). id/status/type/edge_type are safe to expose (they carry
	// no free text) and are what make the row visibly gated.
	OutOfScope bool `json:"out_of_scope,omitempty"`
}
