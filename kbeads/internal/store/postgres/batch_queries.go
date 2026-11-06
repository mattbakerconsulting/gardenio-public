package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/model"
	"github.com/lib/pq"
)

// queryGetTaintsForBeads returns, keyed by bead ID, the ids of taints on each
// of beadIDs whose effects include "gate" (docs/adr/taints.md §15.2,
// gd-9YQiNJVW6u item 5) — this backs batch read-gate checks, which must
// consult only gate-effect taints. A bead absent from the result carries no
// gate-effect taints.
func queryGetTaintsForBeads(ctx context.Context, db executor, beadIDs []string) (map[string][]int64, error) {
	if len(beadIDs) == 0 {
		return make(map[string][]int64), nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT bt.bead_id, bt.taint_id FROM bead_taints bt
		JOIN taints t ON t.id = bt.taint_id
		WHERE bt.bead_id = ANY($1::text[]) AND 'gate' = ANY(t.effects)`,
		pq.Array(beadIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("batch taints: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]int64)
	for rows.Next() {
		var beadID string
		var taintID int64
		if err := rows.Scan(&beadID, &taintID); err != nil {
			return nil, fmt.Errorf("scan batch taint: %w", err)
		}
		result[beadID] = append(result[beadID], taintID)
	}
	return result, rows.Err()
}

// queryGetAllTaintsForBeads returns, keyed by bead ID, every taint id on each
// of beadIDs regardless of effects — the batch form of
// queryGetAllTaintsForBead, for inherit:read on multi-bead reads. A bead
// absent from the result carries no taints.
func queryGetAllTaintsForBeads(ctx context.Context, db executor, beadIDs []string) (map[string][]int64, error) {
	if len(beadIDs) == 0 {
		return make(map[string][]int64), nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT bead_id, taint_id FROM bead_taints
		WHERE bead_id = ANY($1::text[])`,
		pq.Array(beadIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("batch all taints: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]int64)
	for rows.Next() {
		var beadID string
		var taintID int64
		if err := rows.Scan(&beadID, &taintID); err != nil {
			return nil, fmt.Errorf("scan batch all taint: %w", err)
		}
		result[beadID] = append(result[beadID], taintID)
	}
	return result, rows.Err()
}

// queryGetDependenciesForBeads returns forward dependencies keyed by bead ID.
func queryGetDependenciesForBeads(ctx context.Context, db executor, beadIDs []string) (map[string][]*model.Dependency, error) {
	if len(beadIDs) == 0 {
		return make(map[string][]*model.Dependency), nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT bead_id, depends_on_id, type, created_at, created_by, metadata
		FROM deps
		WHERE bead_id = ANY($1::text[])`,
		pq.Array(beadIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("batch deps: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]*model.Dependency)
	for rows.Next() {
		d, err := scanDependency(rows)
		if err != nil {
			return nil, fmt.Errorf("scan batch dep: %w", err)
		}
		result[d.BeadID] = append(result[d.BeadID], d)
	}
	return result, rows.Err()
}

// queryGetReverseDependenciesForBeads returns reverse dependencies keyed by target bead ID.
func queryGetReverseDependenciesForBeads(ctx context.Context, db executor, beadIDs []string) (map[string][]*model.Dependency, error) {
	if len(beadIDs) == 0 {
		return make(map[string][]*model.Dependency), nil
	}
	// LEFT JOIN the dependent (child) bead purely to carry its type. This lets a
	// caller enumerating a parent's children skip hydrating child types it will
	// discard (the /plans step-data walk keeps only issue/phase/decision
	// children — ka-0000003113). The join is on the beads PK and matches
	// non-deleted rows only; a dangling edge (child soft-deleted or absent)
	// yields a NULL type, which scans to an empty BeadType and reads as
	// "unknown" — the caller then falls back to hydration, so a missing type is
	// never load-bearing. This is the ONLY reverse-dep read that carries the
	// child type; the single-bead and forward paths are unchanged.
	rows, err := db.QueryContext(ctx, `
		SELECT d.bead_id, d.depends_on_id, d.type, d.created_at, d.created_by, d.metadata, child.type
		FROM deps d
		LEFT JOIN beads child ON child.id = d.bead_id AND child.deleted_at IS NULL
		WHERE d.depends_on_id = ANY($1::text[])`,
		pq.Array(beadIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("batch reverse deps: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]*model.Dependency)
	for rows.Next() {
		d, err := scanReverseDependencyWithChildType(rows)
		if err != nil {
			return nil, fmt.Errorf("scan batch reverse dep: %w", err)
		}
		result[d.DependsOnID] = append(result[d.DependsOnID], d)
	}
	return result, rows.Err()
}

// scanReverseDependencyWithChildType scans a reverse-dependency row that carries
// the dependent (child) bead's type as a trailing column. Mirrors
// scanDependency but populates model.Dependency.BeadType; a NULL child type
// (dangling / soft-deleted edge) scans to "".
func scanReverseDependencyWithChildType(row scannable) (*model.Dependency, error) {
	var d model.Dependency
	var (
		createdBy sql.NullString
		metadata  sql.NullString
		childType sql.NullString
	)
	if err := row.Scan(
		&d.BeadID,
		&d.DependsOnID,
		&d.Type,
		&d.CreatedAt,
		&createdBy,
		&metadata,
		&childType,
	); err != nil {
		return nil, err
	}
	d.CreatedBy = createdBy.String
	d.Metadata = metadata.String
	d.BeadType = model.BeadType(childType.String)
	return &d, nil
}

// queryGetDependencyCounts returns forward and reverse dep counts per bead.
func queryGetDependencyCounts(ctx context.Context, db executor, beadIDs []string) (map[string]*model.DependencyCounts, error) {
	if len(beadIDs) == 0 {
		return make(map[string]*model.DependencyCounts), nil
	}

	result := make(map[string]*model.DependencyCounts, len(beadIDs))

	// Forward counts (how many things this bead depends on)
	fwdRows, err := db.QueryContext(ctx, `
		SELECT bead_id, COUNT(*) FROM deps
		WHERE bead_id = ANY($1::text[])
		GROUP BY bead_id`,
		pq.Array(beadIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("dep counts forward: %w", err)
	}
	defer fwdRows.Close()

	for fwdRows.Next() {
		var id string
		var count int
		if err := fwdRows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("scan dep count: %w", err)
		}
		result[id] = &model.DependencyCounts{DependencyCount: count}
	}
	if err := fwdRows.Err(); err != nil {
		return nil, err
	}

	// Reverse counts (how many things depend on this bead)
	revRows, err := db.QueryContext(ctx, `
		SELECT depends_on_id, COUNT(*) FROM deps
		WHERE depends_on_id = ANY($1::text[])
		GROUP BY depends_on_id`,
		pq.Array(beadIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("dep counts reverse: %w", err)
	}
	defer revRows.Close()

	for revRows.Next() {
		var id string
		var count int
		if err := revRows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("scan reverse dep count: %w", err)
		}
		if dc, ok := result[id]; ok {
			dc.DependentCount = count
		} else {
			result[id] = &model.DependencyCounts{DependentCount: count}
		}
	}
	return result, revRows.Err()
}

// queryGetChildrenStatusHistogram returns, for each parent bead ID supplied,
// a histogram of its parent-child children grouped by status. The map shape
// is parentID -> status -> count. Parents with no parent-child children are
// omitted from the result; callers should treat a missing entry as "no
// children". Soft-deleted child rows are excluded so the counts match what
// the rest of the read API returns. See the project-view sequencer plan
// (gardenio/plans/project-view-epic-sequencer.md AC-2 / OPEN-3) for the
// consumer.
func queryGetChildrenStatusHistogram(ctx context.Context, db executor, parentIDs []string) (map[string]map[string]int, error) {
	if len(parentIDs) == 0 {
		return make(map[string]map[string]int), nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT d.depends_on_id, b.status, COUNT(*)
		FROM deps d
		JOIN beads b ON d.bead_id = b.id
		WHERE d.depends_on_id = ANY($1::text[])
		  AND d.type = 'parent-child'
		  AND b.deleted_at IS NULL
		GROUP BY d.depends_on_id, b.status`,
		pq.Array(parentIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("children status histogram: %w", err)
	}
	defer rows.Close()

	result := make(map[string]map[string]int)
	for rows.Next() {
		var parentID, status string
		var count int
		if err := rows.Scan(&parentID, &status, &count); err != nil {
			return nil, fmt.Errorf("scan children status histogram: %w", err)
		}
		bucket, ok := result[parentID]
		if !ok {
			bucket = make(map[string]int)
			result[parentID] = bucket
		}
		bucket[status] += count
	}
	return result, rows.Err()
}

// queryGetLabelsForBeads returns labels keyed by bead ID.
func queryGetLabelsForBeads(ctx context.Context, db executor, beadIDs []string) (map[string][]string, error) {
	if len(beadIDs) == 0 {
		return make(map[string][]string), nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT bead_id, label FROM labels
		WHERE bead_id = ANY($1::text[])`,
		pq.Array(beadIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("batch labels: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]string)
	for rows.Next() {
		var beadID, label string
		if err := rows.Scan(&beadID, &label); err != nil {
			return nil, fmt.Errorf("scan batch label: %w", err)
		}
		result[beadID] = append(result[beadID], label)
	}
	return result, rows.Err()
}

// queryGetBlockedByForBeads returns open blocker IDs keyed by bead ID.
func queryGetBlockedByForBeads(ctx context.Context, db executor, beadIDs []string) (map[string][]string, error) {
	if len(beadIDs) == 0 {
		return make(map[string][]string), nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT d.bead_id, d.depends_on_id
		FROM deps d
		JOIN beads blocker ON d.depends_on_id = blocker.id
		WHERE d.bead_id = ANY($1::text[])
		  AND d.type = 'blocks'
		  AND blocker.status IN ('open', 'in_progress', 'blocked', 'deferred')`,
		pq.Array(beadIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("batch blocked-by: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]string)
	for rows.Next() {
		var beadID, blockerID string
		if err := rows.Scan(&beadID, &blockerID); err != nil {
			return nil, fmt.Errorf("scan blocked-by: %w", err)
		}
		result[beadID] = append(result[beadID], blockerID)
	}
	return result, rows.Err()
}

// openBlockerStatusSQL is the SQL IN-list of statuses that mean a blocker has
// NOT yet released its dependent — the same set FilterNoOpenDeps uses in
// queries.go. 'blocked' belongs here: only closed/partial are terminal for
// progression (model.Status.IsTerminalForProgression). Named so the
// --with-blockers ⟷ --no-blockers lockstep is assertable in a test rather
// than a literal duplicated across two files.
const openBlockerStatusSQL = `'open', 'in_progress', 'blocked', 'deferred'`

// blockerReadScope carries the caller's project-visibility predicates into the
// blocker join. The enrichment reaches beads outside the caller's result set
// and returns their titles, so it must honor the SAME label scoping the row
// query applied — see queryGetOpenBlockersForBeads.
//
// RequireAllLabels mirrors BeadFilter.Labels (project-bound fleet token: every
// label must be present); RequireAnyLabels mirrors BeadFilter.LabelsAny (OIDC
// read-side isolation: at least one). Both empty means an unscoped caller
// (admin / internal), which sees every blocker.
type blockerReadScope struct {
	RequireAllLabels []string
	RequireAnyLabels []string
}

// queryGetOpenBlockersForBeads returns each bead's OPEN blocker edges,
// denormalized with the blocker's status, type and title, keyed by bead ID.
//
// This is the batch, enriched form of the frontier question "why is this bead
// not ready, and what is it waiting on?". Before it, a triage caller could get
// the answer only per-bead: `kd show` returns bare edges and the CLI resolves
// each neighbor through a SECOND round-trip (cmd/kd/show_dep_expand.go), so
// classifying an N-bug backlog cost ~2N calls (ka-0000000945 measured ~44 for
// 23 bugs). One JOIN answers it for a whole page.
//
// Blocking semantics are deliberately IDENTICAL to the NoOpenDeps ready-queue
// predicate in queries.go — same blockingDepTypes allow-list (via
// blockingDepTypeInList), same open-status set (via openBlockerStatusSQL) —
// so `--with-blockers` explains exactly the beads `--no-blockers` hides, and
// neither can drift into contradicting the other. Note this is WIDER than
// queryGetBlockedByForBeads above, which matches only type='blocks'; that one
// feeds the graph badge and is left alone here.
//
// Cross-project blockers are REPORTED, not dropped (ka-0000007462). The
// NoOpenDeps predicate applies NO label scope to the blocker, so a blocker in
// another project still hides its dependent from `--no-blockers`. If this
// enrichment silently omitted that same out-of-scope blocker, a genuinely
// gated bead would report an EMPTY open_blockers[] and read as dispatchable —
// the exact contradiction between the two flags this feature exists to
// prevent. So the label scope no longer FILTERS the join; it computes a
// per-row in_scope flag. An out-of-scope blocker is still returned (id /
// status / type / edge_type — none of which carry free text), but its Title
// is REDACTED to "" and OutOfScope is set, so a project-bound token learns the
// row is gated without reading a cross-project bead's title.
//
// Deliberately NO `blocker.deleted_at IS NULL` filter, despite that reading as
// the obviously-hygienic thing to add: NoOpenDeps has no such check, so a
// soft-deleted open blocker still HIDES its dependent from `--no-blockers`.
// Filtering it here would make this query report that same bead as unblocked —
// a bead the ready queue refuses to surface, described as clear. That is the
// exact false all-clear this feature exists to eliminate, so the asymmetry
// must not be introduced on one side alone. If soft-deleted blockers should
// stop blocking, change BOTH predicates together.
//
// Ordering is deterministic (bead_id, then blocker id) so a caller diffing two
// snapshots — or a snapshot test — never sees spurious churn from Postgres
// row order.
func queryGetOpenBlockersForBeads(ctx context.Context, db executor, beadIDs []string, scope blockerReadScope) (map[string][]*model.OpenBlocker, error) {
	if len(beadIDs) == 0 {
		return make(map[string][]*model.OpenBlocker), nil
	}
	args := []any{pq.Array(beadIDs)}
	// Authz: this join reaches beads OUTSIDE the caller's result set, and it
	// returns their free-text TITLE. The list-level project predicates only
	// constrain which rows MATCH, so without accounting for them here a
	// project-bound token could read a cross-project blocker's title through
	// the enrichment. But we must NOT filter out-of-scope blockers — doing so
	// made a gated bead report an empty open_blockers[] (ka-0000007462). So
	// instead of a WHERE clause, build an in_scope boolean expression: the
	// blocker still appears, and the caller redacts its title when out of
	// scope. An unscoped caller (admin / internal — both label sets empty)
	// gets TRUE for every blocker, so nothing is ever redacted for them.
	inScopeSQL := "TRUE"
	var scopeTerms []string
	for _, label := range scope.RequireAllLabels {
		args = append(args, label)
		scopeTerms = append(scopeTerms, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = blocker.id AND labels.label = $%d)", len(args)))
	}
	if len(scope.RequireAnyLabels) > 0 {
		args = append(args, pq.Array(scope.RequireAnyLabels))
		scopeTerms = append(scopeTerms, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = blocker.id AND labels.label = ANY($%d::text[]))", len(args)))
	}
	if len(scopeTerms) > 0 {
		inScopeSQL = "(" + strings.Join(scopeTerms, " AND ") + ")"
	}
	rows, err := db.QueryContext(ctx, `
		SELECT d.bead_id, d.depends_on_id, d.type, blocker.status, blocker.type, blocker.title,
		       `+inScopeSQL+` AS in_scope
		FROM deps d
		JOIN beads blocker ON d.depends_on_id = blocker.id
		WHERE d.bead_id = ANY($1::text[])
		  AND d.type IN (`+blockingDepTypeInList()+`)
		  AND blocker.status IN (`+openBlockerStatusSQL+`)
		ORDER BY d.bead_id, d.depends_on_id, d.type`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("batch open blockers: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]*model.OpenBlocker)
	for rows.Next() {
		var beadID string
		var inScope bool
		var b model.OpenBlocker
		if err := rows.Scan(&beadID, &b.ID, &b.EdgeType, &b.Status, &b.Type, &b.Title, &inScope); err != nil {
			return nil, fmt.Errorf("scan open blocker: %w", err)
		}
		if !inScope {
			// Out-of-scope: keep the non-free-text fields (id/status/type/
			// edge_type) so the row reads as gated, but redact the title so a
			// project-bound token cannot read a cross-project bead's title.
			b.Title = ""
			b.OutOfScope = true
		}
		result[beadID] = append(result[beadID], &b)
	}
	return result, rows.Err()
}

// beadParent carries a bead's immediate parent-child parent id plus (when in
// scope) the parent's title, keyed back to the child in queryGetParentsForBeads.
type beadParent struct {
	ID    string
	Title string
}

// queryGetParentsForBeads returns each bead's immediate parent-child PARENT,
// keyed by CHILD bead id (ka-0000006083). This is the batch answer to "what epic
// does each of these beads hang under?" — one deps⋈beads query per page instead
// of an N+1 `kd show` fan-out. It mirrors queryGetOpenBlockersForBeads: same
// batched shape (bead_id = ANY($1::text[]), a single param regardless of page
// size — never an IN($1,…,$N) that overflows pq's 65535 param cap on a large
// page, per ka-0000002155), same project-scope authz on the joined-in title.
//
// The canonical parent-child row is written CHILD-first: deps.bead_id = child,
// deps.depends_on_id = parent (see model.ParentChildDepTypes / queries.go
// parentChildTypeInList). We match the full alias set ('parent-child' AND legacy
// 'parent') so children of legacy epics still resolve a parent rather than
// reading as orphans.
//
// The parent TITLE is carried via a JOIN that re-applies the caller's label
// scope, exactly like the open-blockers enrichment: the parent may live outside
// the caller's result set, and its free-text title must not leak across a
// project boundary. When the parent is out of scope (or soft-deleted) the LEFT
// JOIN yields a NULL title, so the child still learns its parent ID with an
// empty title rather than the row vanishing. A bead with more than one
// parent-child parent (routine, not an anomaly: ~28% of gb-ready rows) resolves
// to a MEANINGFUL "primary" parent, not the lexicographically-first id: the
// ORDER BY below prefers an OPEN parent, then an EPIC, then the most recent edge
// (ka-0000001227), so the scalar names the live container the bead hangs under
// rather than a dead one that merely sorts earlier.
func queryGetParentsForBeads(ctx context.Context, db executor, beadIDs []string, scope blockerReadScope) (map[string]beadParent, error) {
	if len(beadIDs) == 0 {
		return make(map[string]beadParent), nil
	}
	args := []any{pq.Array(beadIDs)}
	// Authz: the JOIN reaches the parent bead OUTSIDE the caller's result set
	// and returns its TITLE. Re-apply the same label predicates the row query
	// used so a project-bound token / OIDC-restricted user cannot read a
	// cross-project parent's title through this enrichment. The predicates go in
	// the LEFT JOIN's ON clause (NOT the WHERE): a WHERE on the right-hand table
	// would degrade the LEFT JOIN to an INNER JOIN and drop the child row
	// entirely for an out-of-scope parent. In the ON clause a non-matching
	// parent instead yields a NULL title while the child row — and its parent id
	// (d.depends_on_id, which never depends on the join) — is preserved. The id
	// alone is not sensitive and is exactly what makes leaf-vs-container /
	// group-by-epic possible; only the free-text title is scope-gated.
	joinScopeSQL := ""
	if scope.RequireAllLabels != nil {
		for _, label := range scope.RequireAllLabels {
			args = append(args, label)
			joinScopeSQL += fmt.Sprintf(
				" AND EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = parent.id AND labels.label = $%d)", len(args))
		}
	}
	if len(scope.RequireAnyLabels) > 0 {
		args = append(args, pq.Array(scope.RequireAnyLabels))
		joinScopeSQL += fmt.Sprintf(
			" AND EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = parent.id AND labels.label = ANY($%d::text[]))", len(args))
	}
	// DISTINCT ON (d.bead_id) keeps ONE row per child; the ORDER BY tie-break picks
	// WHICH parent that is when a child has several parent-child edges (ka-0000001227:
	// 47/167 gb-ready rows are multi-parent). The prior tie-break was
	// `d.depends_on_id` — lexicographically-lowest parent id — which is deterministic
	// but semantically ARBITRARY: it routinely surfaced a CLOSED task while an OPEN
	// EPIC parent on the same bead stayed hidden, making the parent column actively
	// misleading. The tie-break now prefers, in order:
	//   1. an OPEN parent over a closed one   (parent.status = 'closed' ASC → false first)
	//   2. an EPIC parent over a non-epic one (parent.type <> 'epic'  ASC → false first)
	//   3. the MOST RECENT edge              (d.created_at DESC)
	//   4. lowest parent id                  (final deterministic key, stable output)
	// COALESCE(..., true) deprioritises a parent that came back NULL (soft-deleted or
	// out of the caller's label scope) so a visible open epic always wins over one we
	// cannot see. The LEFT JOIN still keeps the child's parent id (and NULL title) for
	// those cases — the child never vanishes.
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT ON (d.bead_id) d.bead_id, d.depends_on_id, parent.title
		FROM deps d
		LEFT JOIN beads parent ON parent.id = d.depends_on_id AND parent.deleted_at IS NULL`+joinScopeSQL+`
		WHERE d.bead_id = ANY($1::text[])
		  AND d.type IN (`+parentChildTypeInList()+`)
		ORDER BY d.bead_id,
		         COALESCE(parent.status = 'closed', true),
		         COALESCE(parent.type <> 'epic', true),
		         d.created_at DESC,
		         d.depends_on_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("batch parents: %w", err)
	}
	defer rows.Close()

	result := make(map[string]beadParent)
	for rows.Next() {
		var childID, parentID string
		var parentTitle sql.NullString
		if err := rows.Scan(&childID, &parentID, &parentTitle); err != nil {
			return nil, fmt.Errorf("scan parent: %w", err)
		}
		result[childID] = beadParent{ID: parentID, Title: parentTitle.String}
	}
	return result, rows.Err()
}

// queryGetBeadsByIDs fetches beads by their IDs without relational data.
func queryGetBeadsByIDs(ctx context.Context, db executor, ids []string) ([]*model.Bead, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// Soft-delete filter (ka-0000004651 step 8b / ka-0000007077): hide
	// soft-deleted rows from batch fetch, matching queryGetBead.
	// JOIN operators on operated_by_id so the resolved chip (display + email)
	// surfaces alongside every bead row, matching queryGetBead / queryListBeads.
	rows, err := db.QueryContext(ctx,
		`SELECT `+beadColumnsBeads+beadOperatorSelectSuffix+
			` FROM beads`+beadOperatorJoin+
			`WHERE beads.id = ANY($1::text[]) AND beads.deleted_at IS NULL`,
		pq.Array(ids),
	)
	if err != nil {
		return nil, fmt.Errorf("get beads by ids: %w", err)
	}
	defer rows.Close()

	var beads []*model.Bead
	for rows.Next() {
		b, err := scanBeadFull(rows)
		if err != nil {
			return nil, fmt.Errorf("scan bead by id: %w", err)
		}
		beads = append(beads, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan bead by id: %w", err)
	}

	// Populate labels for every returned bead so batch fetch matches the
	// single-GET (queryGetBead) and list (queryListBeads) shapes — otherwise
	// b.Labels is silently nil for batch consumers. queryGetLabelsForBeads uses
	// bead_id = ANY($1::text[]) (a single param regardless of result size), so
	// it stays clear of pq's 65535-parameter ceiling (ka-0000002155). Missing
	// this made the beads-viewer chat panel lose kind badges and from:<name>
	// authorship, because it hydrates via /v1/batch/beads (ka-0000000676).
	if len(beads) > 0 {
		beadIDs := make([]string, len(beads))
		for i, b := range beads {
			beadIDs[i] = b.ID
		}
		labelMap, err := queryGetLabelsForBeads(ctx, db, beadIDs)
		if err != nil {
			return nil, fmt.Errorf("get beads by ids labels: %w", err)
		}
		for _, b := range beads {
			b.Labels = labelMap[b.ID] // nil if no labels (same as before for unlabelled beads)
		}
	}
	return beads, nil
}
