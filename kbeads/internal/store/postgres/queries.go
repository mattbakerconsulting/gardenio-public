package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
	"github.com/lib/pq"
)

// beadColumns is the column list used for plain (un-joined) SELECT and
// UPDATE...RETURNING statements on the beads table. Read paths that need the
// canonical operator display/email (resolved server-side from the operators
// table on operated_by_id) use beadColumnsBeads + beadOperatorSelectSuffix +
// beadOperatorJoin instead, paired with scanBeadFull.
//
// operated_by_id (the operators(id) FK) is projected on every read path so
// model.Bead.OperatedByID round-trips to API clients — the controller's
// session-spend reconciler labels gardenio_operator_cost_usd_total with it,
// and before this projection existed every series carried the "0" sentinel
// because the JSON always read null (ka-0000001989).
const beadColumns = `id, slug, short_title, heraldry, kind, type, title, description, notes,
	status, priority, assignee, owner, created_at, created_by, updated_at,
	closed_at, closed_by, due_at, defer_until, fields, fleet, deleted_at, category, operated_by_id`

// beadColumnsBeads is beadColumns with each column qualified with the `beads.`
// table name. Used in JOINed SELECTs against the operators table where the
// columns id / kind / created_at / updated_at / deleted_at would otherwise be
// ambiguous. We prefix every column (not just the ambiguous ones) for clarity
// and to keep the SELECT projection trivially diff-able against beadColumns.
const beadColumnsBeads = `beads.id, beads.slug, beads.short_title, beads.heraldry, beads.kind, beads.type, beads.title, beads.description, beads.notes,
	beads.status, beads.priority, beads.assignee, beads.owner, beads.created_at, beads.created_by, beads.updated_at,
	beads.closed_at, beads.closed_by, beads.due_at, beads.defer_until, beads.fields, beads.fleet, beads.deleted_at, beads.category, beads.operated_by_id`

// beadListColumns is beadColumnsBeads for the list path, with the free-text
// columns the caller opted out of (BeadFilter.OmitDescription / OmitNotes)
// replaced by a NULL of the same name, and the BeadFilter.OmitFields keys
// stripped from fields, so scanBeadWithTotalFull's column
// layout is unchanged and the body is never read off disk or sent
// (gd-zXdZq4Traf).
func beadListColumns(filter model.BeadFilter) string {
	cols := beadColumnsBeads
	if filter.OmitDescription {
		cols = strings.Replace(cols, "beads.description,", "NULL::text AS description,", 1)
	}
	if filter.OmitNotes {
		cols = strings.Replace(cols, "beads.notes,", "NULL::text AS notes,", 1)
	}
	if expr := fieldsOnlyExpr(filter.FieldsOnly); expr != "" {
		cols = strings.Replace(cols, "beads.fields,", expr+" AS fields,", 1)
	} else if expr := omitFieldsExpr(filter.OmitFields); expr != "" {
		cols = strings.Replace(cols, "beads.fields,", expr+" AS fields,", 1)
	}
	return cols
}

// fieldsOnlyExpr returns a subquery that rebuilds beads.fields from only the
// FieldsOnly keys, or "" when there is no valid key. Like omitFieldsExpr the
// keys are inlined literals; one failing model.ValidFieldKey is skipped. The
// CASE keeps a NULL or non-object fields value from making jsonb_each raise:
// such a row reads as {}, never a 500.
func fieldsOnlyExpr(keys []string) string {
	var quoted []string
	for _, k := range keys {
		if model.ValidFieldKey(k) {
			quoted = append(quoted, "'"+k+"'")
		}
	}
	if len(quoted) == 0 {
		return ""
	}
	return "(SELECT COALESCE(jsonb_object_agg(kv.k, kv.v), '{}'::jsonb)" +
		" FROM jsonb_each(CASE WHEN jsonb_typeof(beads.fields) = 'object' THEN beads.fields ELSE '{}'::jsonb END) AS kv(k, v)" +
		" WHERE kv.k IN (" + strings.Join(quoted, ", ") + "))"
}

// omitFieldsExpr returns `(beads.fields #- '{a}' #- '{b,c}')` for the
// OmitFields dotted paths, or "" when there is nothing to omit. The paths are
// inlined as literals rather than bound, so beadListColumns stays a pure
// string and the WHERE args the count query reuses are untouched; a path
// that fails model.ValidOmitFieldPath (the HTTP layer already 400s it) is
// skipped, so nothing but [A-Za-z0-9_-] segments ever reaches the SQL.
func omitFieldsExpr(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		if !model.ValidOmitFieldPath(p) {
			continue
		}
		b.WriteString(" #- '{")
		b.WriteString(strings.ReplaceAll(p, ".", ","))
		b.WriteString("}'")
	}
	if b.Len() == 0 {
		return ""
	}
	return "(beads.fields" + b.String() + ")"
}

// beadOperatorJoin is the LEFT JOIN against operators keyed on
// beads.operated_by_id. LEFT (not INNER) so beads with no resolved operator
// (autonomous writes, legacy rows, alias-upsert failures) still surface with
// empty display/email rather than disappearing from results.
const beadOperatorJoin = ` LEFT JOIN operators op ON op.id = beads.operated_by_id `

// beadOperatorSelectSuffix is the trailing three-column extension that,
// appended to beadColumnsBeads in a SELECT projection, surfaces the bead's
// resolved operator identity:
//   - operated_by:         canonical "kind:subject" alias (e.g.
//     "oidc:matt@acme.example", "slack:U0OPERATOR1") — the typed identity
//     downstream sanitizers (ka-0000002601) accept.
//   - operated_by_display: human-readable display name.
//   - operated_by_email:   email if any.
//
// Reads resolve operators.kind || ':' || operators.subject via LEFT JOIN
// (migration 024 §"Reads resolve…"). Pre-fix this alias was never projected,
// so model.Bead.OperatedBy came back empty from every read path, breaking
// downstream code (trigger engine fallback, ka-0000006816) that needs the
// canonical alias of the bead's operator. COALESCE pins all three columns
// to NOT NULL so the scanners read into plain string destinations.
const beadOperatorSelectSuffix = `, COALESCE(op.kind || ':' || op.subject, '') AS operated_by, COALESCE(op.display, '') AS operated_by_display, COALESCE(op.email, '') AS operated_by_email`

// beadLabelsSelectSuffix folds a bead's labels into the single-bead read as
// one trailing text[] column, so GetBead and the UPDATE...RETURNING paths
// (close, restore, transfer) no longer pay a separate `SELECT label FROM
// labels WHERE bead_id = $1` round trip per call — the labels N+1 behind
// ~28M such SELECTs a week (gd-cP9tODid8j). It must be appended to a
// projection whose FROM / UPDATE target is the beads table itself (it
// correlates on beads.id) and paired with scanWithLabels. array_agg over zero
// rows is NULL, which pq.StringArray scans as nil — the same nil-for-no-labels
// queryGetLabels returns. ORDER BY label is the order the (bead_id, label)
// primary-key scan behind queryGetLabels already produced.
const beadLabelsSelectSuffix = `, (SELECT array_agg(l.label ORDER BY l.label) FROM labels l WHERE l.bead_id = beads.id) AS labels`

// beadHasReportSelect is the trailing has_report projection appended to
// list-path SELECTs. True when the bead has a direct (depth-1) parent-child
// child whose type ∈ {report, rich-report, bundle} and whose deleted_at IS
// NULL. Paired with scanBeadWithTotalFull (which scans an extra trailing
// bool). Used by ListBeads to surface report-attachment state to the
// sequencer↔report linking view (R1, ka-0000007923).
const beadHasReportSelect = `, EXISTS (
	SELECT 1 FROM deps d JOIN beads c ON c.id = d.bead_id
	WHERE d.depends_on_id = beads.id
	  AND d.type = 'parent-child'
	  AND c.type IN ('report', 'rich-report', 'bundle')
	  AND c.deleted_at IS NULL
) AS has_report`

// blockedOnDecisionScopeUnion is the correlated scope set for the
// blocked_on_decision predicate (ka-0000001065 / R2): this bead (beads.id)
// plus its parent-child descendants at depth 1 and depth 2. Inlined twice
// in blockedOnDecisionSelectExpr — once for the parent-child decision
// pathway and once for the agent-yielded pathway — so it stays a single
// canonical definition rather than two divergent copies. See
// gardenio/docs/research/sequencer-report-decision-visibility.md (R2 row).
const blockedOnDecisionScopeUnion = `(
		SELECT beads.id
		UNION
		SELECT d1.bead_id FROM deps d1 WHERE d1.depends_on_id = beads.id AND d1.type = 'parent-child'
		UNION
		SELECT d2.bead_id FROM deps d2
			JOIN deps d1 ON d2.depends_on_id = d1.bead_id
				AND d1.type = 'parent-child'
				AND d2.type = 'parent-child'
			WHERE d1.depends_on_id = beads.id
	)`

// blockedOnDecisionSelectExpr is the per-row SELECT projection that
// resolves to the ka-id of the open decision blocking this bead, or ” when
// none. Predicate is OR of:
//   - an open type=decision bead has a parent-child dep edge to this bead
//     or to a descendant in scope (depth ≤ 2);
//   - a type=agent bead under this bead (depth ≤ 2) has fields.decision_bead_id
//     pointing at d.id, where d is the open decision.
//
// Stable tie-break: MIN(d.id) returns the lexicographically-first ka-id when
// multiple decisions match. The COALESCE wrapper pins the result to ” when
// no row matches so the trailing column reads into a plain string destination.
//
// Edge-case coverage (verified by TestQueryListBeadsBlockedOnDecisionEdgeCases):
//   - "closed decision clears the badge" — the d.status='open' filter excludes
//     closed decisions; once the operator closes the decision, the predicate
//     flips false on the next ListBeads call.
//   - "agent crashed before clearing" — the agent path joins ag.fields->>'decision_bead_id'
//     against d.id (an OPEN decision). A stale field pointing at a closed
//     decision does not flag because no such d row exists.
//   - "orphaned agent" — the dpa.depends_on_id IN scope clause requires the
//     agent be parented under this bead's subtree. An agent with no parent
//     edge matches no scope row, so the OR branch is empty rather than crashing.
//   - "unrelated decision does not flag" — the dp.depends_on_id IN scope clause
//     requires the decision's parent edge to land in this bead's subtree.
//
// See ka-0000001065 / R2 spec.
const blockedOnDecisionSelectExpr = `COALESCE(
	(SELECT MIN(d.id) FROM beads d
	 WHERE d.type = 'decision' AND d.status = 'open' AND d.deleted_at IS NULL
	   AND (
	     EXISTS (
	       SELECT 1 FROM deps dp
	       WHERE dp.bead_id = d.id AND dp.type = 'parent-child'
	         AND dp.depends_on_id IN ` + blockedOnDecisionScopeUnion + `
	     )
	     OR EXISTS (
	       SELECT 1 FROM beads ag
	       JOIN deps dpa ON dpa.bead_id = ag.id AND dpa.type = 'parent-child'
	       WHERE ag.type = 'agent'
	         AND ag.deleted_at IS NULL
	         AND (ag.fields->>'decision_bead_id') = d.id
	         AND dpa.depends_on_id IN ` + blockedOnDecisionScopeUnion + `
	     )
	   )
	),
	''
)`

// beadBlockedOnDecisionSelectSuffix is the trailing single-column extension
// that, appended after beadOperatorSelectSuffix in a ListBeads projection,
// surfaces the daemon-computed blocked_on_decision_bead_id. The Bead-side
// boolean is derived in scanBeadWithTotalFull from the column being non-empty.
// See ka-0000001065 / R2.
const beadBlockedOnDecisionSelectSuffix = `, ` + blockedOnDecisionSelectExpr + ` AS blocked_on_decision_bead_id`

// parkedOnDecisionSelectExpr is the per-row SELECT projection that resolves to
// the ka-id of an open decision that PARKS this bead by naming it in prose, or
// ” when none. It is the TEXT-MENTION sibling of blockedOnDecisionSelectExpr
// (ka-0000003797): where R2 catches an open decision reachable by a
// parent-child/agent EDGE, this catches an open decision whose title,
// description, notes, or a comment contains a bare/typed ka-id mention of this
// bead — the exact signal `kd list --type=decision --status=open --mentions
// <id>` reports, materialized in the bead_references table.
//
// This is the gap that made the 2026-08-20 23:43 preemption near-inevitable
// (bug ka-0000007469): the 8 escalated rows were named only in the decision's
// PROSE. They carried no blocking dep edge — a `--requesting-task` decision
// writes a `blocks` edge that already keeps its target OUT of `gb ready` via
// NoOpenDeps, and a parent-child decision is caught by R2 — so they rendered as
// ordinary claimable rows and an agent executed one leg of a question the
// operator already owned.
//
// Predicate: an open type=decision bead d has a bead_references row whose
// source is d and whose target is this bead. Stable tie-break MIN(d.id) returns
// the lexicographically-first ka-id when several open decisions mention the
// bead. COALESCE pins the result to ” so the trailing column scans into a
// plain string. Index-backed by idx_bead_references_target (migration 010) for
// the EXISTS probe and idx_beads_open_decisions_id (migration 033) for the
// outer decision scan — no new index (ka-0000003797).
//
// Edge cases (verified by TestQueryListBeadsParkedOnDecisionEdgeCases):
//   - "closed decision clears the badge" — d.status='open' excludes closed
//     decisions, so the predicate flips false the next ListBeads call once the
//     operator resolves and closes the decision.
//   - "self-mention does not flag" — a decision that mentions ITSELF cannot
//     park a work row: the target is the decision's own id, not a ready bead,
//     and a decision is never in the ready set.
//   - "unrelated mention does not flag" — the bead_references target must be
//     THIS bead's id; a decision mentioning some other bead matches no row.
const parkedOnDecisionSelectExpr = `COALESCE(
	(SELECT MIN(d.id) FROM beads d
	 WHERE d.type = 'decision' AND d.status = 'open' AND d.deleted_at IS NULL
	   AND EXISTS (
	     SELECT 1 FROM bead_references br
	     WHERE br.source_bead_id = d.id AND br.target_bead_id = beads.id
	   )
	),
	''
)`

// beadParkedOnDecisionSelectSuffix is the trailing single-column extension that,
// appended after beadBlockedOnDecisionSelectSuffix in a ListBeads projection,
// surfaces the daemon-computed parked_on_decision_bead_id. The Bead-side boolean
// is derived in scanBeadWithTotalFull from the column being non-empty.
// See ka-0000003797.
const beadParkedOnDecisionSelectSuffix = `, ` + parkedOnDecisionSelectExpr + ` AS parked_on_decision_bead_id`

// blockingDepTypes is the allow-list of dep types that mean the source bead
// is genuinely waiting on the target bead. Structural / relational types
// (parent-child, parent-of, child-of, related, duplicates, supersedes,
// assigned, session_of, promoted, …) link beads but do NOT block readiness.
//
// Used by FilterNoOpenDeps (gb ready / kd list --no-blockers): a bead's only
// open dep being parent-child to an open epic must NOT keep it out of the
// ready queue — it's a hierarchy edge, not a wait edge. Regression guard for
// ka-0000000173 and ka-0000004488.
//
// To extend: add a new blocking dep type here; blockingDepTypeInList renders
// it into every consuming query. The test in postgres_test.go pins this
// allow-list.
var blockingDepTypes = []string{"blocks", "blocked-by", "depends-on"}

// blockingDepTypeInList renders blockingDepTypes as a SQL IN-list literal,
// e.g. "'blocks', 'blocked-by', 'depends-on'". Mirrors parentChildTypeInList
// below, and exists for the same reason: every query that asks "is this bead
// waiting on something?" must render the SAME set. The values are
// compile-time constants (no user input), so inlining them as quoted literals
// is safe and keeps the predicates parameter-free.
//
// Consumers: the NoOpenDeps ready-queue predicate (gb ready / kd list
// --no-blockers) and the --with-blockers enrichment that explains exactly the
// rows it hides.
func blockingDepTypeInList() string {
	quoted := make([]string, len(blockingDepTypes))
	for i, t := range blockingDepTypes {
		quoted[i] = "'" + t + "'"
	}
	return strings.Join(quoted, ", ")
}

// parentChildTypeInList renders model.ParentChildDepTypes as a SQL IN-list
// literal, e.g. "'parent-child', 'parent'". That set is the single source of
// truth for the parent-child hierarchy edge and its legacy "parent" alias — a
// large body of live edges are typed literally "parent" (pre-canonicalization,
// child-first, same orientation as "parent-child"; verified 2026-07-14 for
// ka-0000008107). Every parent-child READ filter (NoParent, ParentID,
// ParentSubtreeID) matches on this list so children of legacy epics are neither
// mis-reported as orphans (--no-parent) nor dropped from subtree walks
// (--parent / children). This is backward-compatible and needs NO data
// migration; the write path still emits only the canonical "parent-child".
//
// The values are compile-time constants (no user input), so inlining them as
// quoted literals — matching the blockingDepTypes builder above — is safe and
// keeps the parent-child read filters parameter-free. Precedent:
// queries_bundle.go's queryHasBundleChild treats the same aliases as one set.
func parentChildTypeInList() string {
	quoted := make([]string, len(model.ParentChildDepTypes))
	for i, t := range model.ParentChildDepTypes {
		quoted[i] = "'" + t + "'"
	}
	return strings.Join(quoted, ", ")
}

// mergeNonEmpty deduplicates a scalar + slice into a single non-empty slice
// of distinct strings. Used by the Phase 3b filter merge for assignee/owner/
// created_by predicates so callers can mix the legacy scalar field with the
// new repeated *_in variants in a single query.
func mergeNonEmpty(scalar string, more []string) []string {
	seen := make(map[string]struct{}, len(more)+1)
	out := make([]string, 0, len(more)+1)
	add := func(v string) {
		if v == "" {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	add(scalar)
	for _, v := range more {
		add(v)
	}
	return out
}

// executor is the interface satisfied by both *sql.DB and *sql.Tx.
type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func queryCreateBead(ctx context.Context, db executor, b *model.Bead) error {
	fleet := b.Fleet
	if fleet == "" {
		fleet = "default"
	}
	// deleted_at defaults to NULL; not set on insert. Soft-delete (ka-0000004651
	// step 8b) updates it via queryDeleteBead, restore clears it via queryRestoreBead.
	_, err := db.ExecContext(ctx, `
		INSERT INTO beads (
			id, slug, kind, type, title, description, notes,
			status, priority, assignee, owner, created_at, created_by, updated_at,
			closed_at, closed_by, due_at, defer_until, fields, fleet, operated_by_id,
			created_by_operator_id, short_title, heraldry
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $11, $12, $13, $14,
			$15, $16, $17, $18, $19, $20, $21,
			$22, $23, $24
		)`,
		b.ID,
		nullString(b.Slug),
		string(b.Kind),
		string(b.Type),
		b.Title,
		b.Description,
		b.Notes,
		string(b.Status),
		b.Priority,
		b.Assignee,
		b.Owner,
		b.CreatedAt,
		b.CreatedBy,
		b.UpdatedAt,
		nullTimePtr(b.ClosedAt),
		b.ClosedBy,
		nullTimePtr(b.DueAt),
		nullTimePtr(b.DeferUntil),
		jsonbBytes(b.Fields),
		fleet,
		nullInt64Ptr(b.OperatedByID),
		nullInt64Ptr(b.CreatedByOperatorID),
		nullString(b.ShortTitle),
		nullString(b.Heraldry),
	)
	return err
}

func queryGetBead(ctx context.Context, db executor, id string) (*model.Bead, error) {
	// Soft-delete filter (ka-0000004651 step 8b / ka-0000007077): GetBead must
	// hide soft-deleted rows so `kd show <id>` and downstream callers (mol show,
	// dependency lookups, decisions, escalations) treat a deleted bead as
	// not-found. Restore goes through queryRestoreBead, which targets deleted
	// rows directly and bypasses this filter.
	row := db.QueryRowContext(ctx,
		`SELECT `+beadColumnsBeads+beadOperatorSelectSuffix+beadLabelsSelectSuffix+
			` FROM beads`+beadOperatorJoin+
			`WHERE beads.id = $1 AND beads.deleted_at IS NULL`, id)
	b, err := scanWithLabels(row, scanBeadFull)
	if err != nil {
		return nil, err
	}

	// Fetch dependencies.
	deps, err := queryGetDependencies(ctx, db, id)
	if err != nil {
		return nil, err
	}
	b.Dependencies = deps

	// Fetch comments.
	comments, err := queryGetComments(ctx, db, id)
	if err != nil {
		return nil, err
	}
	b.Comments = comments

	return b, nil
}

// queryGetBeadIncludingDeleted is queryGetBead without the `deleted_at IS
// NULL` filter — it returns a soft-deleted row instead of hiding it
// (model.Bead.DeletedAt tells the caller which). Scrub execution
// (gd-9rjfQsmQeD) needs this: purge and detaint/strip cascade a
// soft-deleted bead in place exactly like a live one (gd-T5P13cSoBw), so
// validating an attested clean_content.fields payload against
// queryGetBead's soft-delete-hiding view would skip the exact beads that
// still get written to. Returns sql.ErrNoRows only when no row exists for
// this id at all, live or deleted.
func queryGetBeadIncludingDeleted(ctx context.Context, db executor, id string) (*model.Bead, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+beadColumnsBeads+beadOperatorSelectSuffix+beadLabelsSelectSuffix+
			` FROM beads`+beadOperatorJoin+
			`WHERE beads.id = $1`, id)
	b, err := scanWithLabels(row, scanBeadFull)
	if err != nil {
		return nil, err
	}

	deps, err := queryGetDependencies(ctx, db, id)
	if err != nil {
		return nil, err
	}
	b.Dependencies = deps

	comments, err := queryGetComments(ctx, db, id)
	if err != nil {
		return nil, err
	}
	b.Comments = comments

	return b, nil
}

// queryFindAgentByName resolves a type=agent bead by its fields.agent value,
// optionally restricted to the supplied status set. Returns the
// most-recently-updated match (ORDER BY updated_at DESC, id DESC LIMIT 1) and
// sql.ErrNoRows when no row qualifies.
//
// The `fields ? 'agent'` predicate is required so the planner can use the
// partial expression index idx_beads_fields_agent (migration 025); without
// it the index is not eligible and the query falls back to a Seq Scan. See
// kbeads/internal/store/postgres/migration_025_integration_test.go and the
// regression guard TestQueryFindAgentByName_UsesPartialIndex.
//
// Soft-deleted rows are excluded so callers see the same view as queryGetBead.
// Bead labels/dependencies/comments are NOT hydrated because the only
// consumer (controller agent-lookup hot path) reads fields + identity only.
func queryFindAgentByName(ctx context.Context, db executor, name string, statuses []model.Status) (*model.Bead, error) {
	var (
		whereClauses = []string{
			"beads.type = 'agent'",
			"beads.deleted_at IS NULL",
			"beads.fields ? 'agent'",
			"beads.fields->>'agent' = $1",
		}
		args = []any{name}
	)

	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, s := range statuses {
			placeholders[i] = fmt.Sprintf("$%d", len(args)+1)
			args = append(args, string(s))
		}
		whereClauses = append(whereClauses, "beads.status IN ("+strings.Join(placeholders, ", ")+")")
	}

	q := `SELECT ` + beadColumnsBeads + beadOperatorSelectSuffix +
		` FROM beads` + beadOperatorJoin +
		`WHERE ` + strings.Join(whereClauses, " AND ") +
		` ORDER BY beads.updated_at DESC, beads.id DESC LIMIT 1`

	row := db.QueryRowContext(ctx, q, args...)
	return scanBeadFull(row)
}

// queryFindAgentsByNames is the batched form of queryFindAgentByName: it
// resolves a set of agent names in a single round trip and returns one
// most-recently-updated bead per matching name. Names with no matching
// bead are silently omitted from the result. The `fields ? 'agent'`
// predicate is kept so the planner remains eligible for the partial
// expression index idx_beads_fields_agent (migration 025); the equality
// becomes `= ANY($1)` so a single index scan covers the whole batch.
//
// Powers POST /v1/agents-by-name:batch — used by the beads-viewer list
// pages to prewarm the per-request agentResolver and collapse N per-row
// daemon round-trips into one (ka-0000000899).
func queryFindAgentsByNames(ctx context.Context, db executor, names []string, statuses []model.Status) ([]*model.Bead, error) {
	if len(names) == 0 {
		return nil, nil
	}

	var (
		whereClauses = []string{
			"beads.type = 'agent'",
			"beads.deleted_at IS NULL",
			"beads.fields ? 'agent'",
			"beads.fields->>'agent' = ANY($1)",
		}
		args = []any{pq.Array(names)}
	)

	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, s := range statuses {
			placeholders[i] = fmt.Sprintf("$%d", len(args)+1)
			args = append(args, string(s))
		}
		whereClauses = append(whereClauses, "beads.status IN ("+strings.Join(placeholders, ", ")+")")
	}

	// DISTINCT ON (fields->>'agent') returns one row per agent name; the
	// matching ORDER BY clause pins the choice to the most-recently-updated
	// bead, mirroring queryFindAgentByName's LIMIT 1 semantics.
	q := `SELECT DISTINCT ON (beads.fields->>'agent') ` +
		beadColumnsBeads + beadOperatorSelectSuffix +
		` FROM beads` + beadOperatorJoin +
		`WHERE ` + strings.Join(whereClauses, " AND ") +
		` ORDER BY beads.fields->>'agent', beads.updated_at DESC, beads.id DESC`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.Bead
	for rows.Next() {
		b, err := scanBeadFull(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// queryFindProjectByName resolves a type=project bead by name, matching
// either the bare title or the legacy "Project: <name>" title (see
// projectNameFromTitle in internal/server) — mirroring the title-prefix
// handling in gardenio/controller/internal/beadsapi.ListProjectBeads, so a
// project registered before that prefix was dropped resolves identically
// here. A duplicate name fails closed for the Lands kill switch: a bead with a
// land_kind wins, then a frozen one (land_frozen), then the lowest bead id (the
// deterministic-winner rule ListProjectBeads uses), so a second bead with
// neither cannot make a frozen land read as unfrozen (the controller's
// purse.readLands ranks the same way). Returns sql.ErrNoRows when no project
// bead matches. Backs per-project intake-taint propagation (ADR
// docs/adr/taints.md §10.3) and the frozen-land create gate.
func queryFindProjectByName(ctx context.Context, db executor, name string) (*model.Bead, error) {
	q := `SELECT ` + beadColumnsBeads + beadOperatorSelectSuffix +
		` FROM beads` + beadOperatorJoin +
		` WHERE beads.type = 'project' AND beads.deleted_at IS NULL
		    AND (beads.title = $1 OR beads.title = 'Project: ' || $1)
		  ORDER BY COALESCE(beads.fields->>'land_kind', '') = '' ASC,
		           COALESCE(beads.fields->>'land_frozen', '') = 'true' DESC,
		           beads.id ASC LIMIT 1`
	row := db.QueryRowContext(ctx, q, name)
	return scanBeadFull(row)
}

// expandSearchTerm rewrites a single whitespace-delimited search term so that
// the separator characters '-', '_' and '/' each match any run of characters
// (a space, a different separator, or nothing) in the target text. This lets a
// hyphenated query term like "break-glass" match beads that spell the compound
// "break glass", "breakglass" or "break_glass" — the multi-term AND used to
// zero out recall whenever the query and the bead hyphenated a compound
// differently (ka-0000002772).
//
// The result is still bound as a query PARAMETER (never string-concatenated
// into SQL); this only reshapes the VALUE that feeds the `'%' || $n || '%'`
// ILIKE pattern. Because the value lands inside an ILIKE pattern, any ILIKE
// metacharacter the user typed literally is neutralized so it matches literally
// instead of acting as a wildcard: '%' -> '\%' and the escape char '\' -> '\\'
// (Postgres LIKE/ILIKE use backslash as the default escape, so no ESCAPE clause
// is needed). Only '%' and '\' are ever prefixed with a backslash, so this
// never emits a dangling escape sequence.
//
// '_' is BOTH a separator and the ILIKE single-char wildcard. We resolve the
// ambiguity in favor of "separator", turning it into the '%' multi-char
// wildcard rather than escaping it to a literal '\_'. That is strictly more
// permissive, so recall stays a superset: a literal "a_b" query still matches
// an "a_b" bead (the '%' spans the underscore) while additionally matching
// "a-b", "a b" and "ab". Handling '_' as a separator here — instead of escaping
// then re-substituting — sidesteps the ordering hazard the two-step form has.
func expandSearchTerm(term string) string {
	var b strings.Builder
	b.Grow(len(term) + 4)
	for _, r := range term {
		switch r {
		case '-', '_', '/':
			b.WriteByte('%')
		case '%', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func queryListBeads(ctx context.Context, db executor, filter model.BeadFilter) ([]*model.Bead, int, error) {
	w := buildBeadListWhere(filter)
	whereSQL := w.sql()

	// CountOnly fast path: skip the full SELECT + ORDER BY + LIMIT shape
	// entirely and return just the count. The COUNT(*) OVER() pattern below
	// forces postgres to materialize and sort the entire matching set even
	// when LIMIT 1 is set, which on broad filters (e.g. status='open' on
	// gardenio-e2e: ~240k matching rows) costs seconds. Callers that only
	// need the count (beads-viewer's queued-count tile, etc.) take this
	// branch and avoid the sort entirely. Limit / Offset / Sort are ignored.
	if filter.CountOnly {
		total, err := countListedBeads(ctx, db, whereSQL, w.args)
		if err != nil {
			return nil, 0, err
		}
		return nil, total, nil
	}

	// A text search counts with COUNT(*) OVER() in the data query itself; any
	// other list derives its total afterwards (see buildBeadListDataQuery).
	windowCount := w.searchRelevanceExpr != ""
	dataQuery, dataArgs := buildBeadListDataQuery(filter, w, whereSQL)
	beads, total, err := queryListedBeadRows(ctx, db, dataQuery, dataArgs)
	if err != nil {
		return nil, 0, err
	}
	if !windowCount {
		if total, err = resolveListTotal(ctx, db, filter, len(beads), whereSQL, w.args); err != nil {
			return nil, 0, err
		}
	}
	if err := enrichListedBeads(ctx, db, filter, beads); err != nil {
		return nil, 0, err
	}
	return beads, total, nil
}

func queryUpdateBead(ctx context.Context, db executor, b *model.Bead) error {
	return db.QueryRowContext(ctx, `
		UPDATE beads SET
			slug = $2,
			kind = $3,
			type = $4,
			title = $5,
			description = $6,
			notes = $7,
			status = $8,
			priority = $9,
			assignee = $10,
			owner = $11,
			updated_at = NOW(),
			closed_at = $12,
			closed_by = $13,
			due_at = $14,
			defer_until = $15,
			fields = $16,
			short_title = $17,
			heraldry = $18,
			project = `+projectFromLabelsSQL+`
		WHERE id = $1
		RETURNING updated_at`,
		b.ID,
		nullString(b.Slug),
		string(b.Kind),
		string(b.Type),
		b.Title,
		b.Description,
		b.Notes,
		string(b.Status),
		b.Priority,
		b.Assignee,
		b.Owner,
		nullTimePtr(b.ClosedAt),
		b.ClosedBy,
		nullTimePtr(b.DueAt),
		nullTimePtr(b.DeferUntil),
		jsonbBytes(b.Fields),
		nullString(b.ShortTitle),
		nullString(b.Heraldry),
	).Scan(&b.UpdatedAt)
}

// queryLockBeadAssignee reads a bead's assignee with FOR UPDATE. Inside a
// transaction the row stays locked until commit, so a second taker of the
// same lock bead waits here and then reads the first taker's assignee.
func queryLockBeadAssignee(ctx context.Context, db executor, id string) (string, error) {
	var assignee string
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(assignee, '') FROM beads WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, id,
	).Scan(&assignee)
	return assignee, err
}

// queryFillShortTitle sets short_title only while the stored value is blank.
// The emptiness check and the write are one statement: under READ COMMITTED a
// concurrent writer that set a title first makes Postgres re-evaluate the WHERE
// on the new row version, so this matches no row and returns false.
func queryFillShortTitle(ctx context.Context, db executor, b *model.Bead) (bool, error) {
	err := db.QueryRowContext(ctx, `
		UPDATE beads SET
			short_title = $2,
			updated_at = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
		  AND btrim(COALESCE(short_title, '')) = ''
		RETURNING updated_at`,
		b.ID,
		nullString(b.ShortTitle),
	).Scan(&b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// queryIncrementBeadFields atomically adds numeric deltas to JSONB fields.
// The SQL builds a patch object from the deltas, adds each value to the
// existing field (defaulting to 0 if absent), and writes the result in a
// single UPDATE — no read-modify-write race.
// queryAllocateAgentHandleSeq atomically allocates the next per-gardener sequence
// number for stable, monotonic agent_handle naming (ka-0000002870). The single
// INSERT ... ON CONFLICT DO UPDATE ... RETURNING statement is race-free: two
// concurrent creates for the same gardener each execute this atomic upsert and
// receive distinct, strictly increasing values (Postgres serializes the row
// update on the primary-key conflict). The counter lives in its own table, so
// pruning type=agent beads never rewinds it — the "never resets after pruning"
// invariant the design memo requires (epic ka-0000006566 §4).
func queryAllocateAgentHandleSeq(ctx context.Context, db executor, gardenerID string) (int64, error) {
	var seq int64
	// gardener_id is the table's single key column (migration 051 collapsed the
	// captain_id / gardener_id pair, gd-3AHtqh5XXB).
	err := db.QueryRowContext(ctx, `
		INSERT INTO agent_handle_counters (gardener_id, next_seq)
		VALUES ($1, 1)
		ON CONFLICT (gardener_id)
		DO UPDATE SET next_seq = agent_handle_counters.next_seq + 1
		RETURNING next_seq`, gardenerID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("allocate agent_handle seq for %q: %w", gardenerID, err)
	}
	return seq, nil
}

func queryIncrementBeadFields(ctx context.Context, db executor, id string, deltas map[string]float64) (*model.Bead, error) {
	if len(deltas) == 0 {
		return queryGetBead(ctx, db, id)
	}

	// Validate every key up front. Keys flow into a jsonb path literal and a
	// ->> operator argument in the SQL below; anything outside the identifier
	// grammar could break out of the quoting and inject arbitrary SQL (AUDIT
	// 2026-04-18 §C1). Reject the whole batch rather than silently skipping.
	for key := range deltas {
		if !store.ValidFieldKey(key) {
			return nil, fmt.Errorf("%w: %q", store.ErrInvalidFieldKey, key)
		}
	}

	// Build SET clauses: fields = jsonb_set(fields, '{key}', to_jsonb(COALESCE((fields->>'key')::numeric, 0) + $N))
	// Each delta gets one nested jsonb_set call.
	expr := "COALESCE(fields, '{}'::jsonb)"
	args := []any{id}
	for key, delta := range deltas {
		args = append(args, delta)
		paramIdx := len(args)
		expr = fmt.Sprintf(
			"jsonb_set(%s, '{%s}', to_jsonb(COALESCE((%s->>'%s')::numeric, 0) + $%d))",
			expr, key, "COALESCE(fields, '{}'::jsonb)", key, paramIdx,
		)
	}

	query := fmt.Sprintf(`
		UPDATE beads SET
			fields = %s,
			updated_at = NOW()
		WHERE id = $1
		RETURNING %s`, expr, beadColumns)

	row := db.QueryRowContext(ctx, query, args...)
	return scanBead(row)
}

// queryMergeBeadFields atomically merges the top-level keys of extra into a
// bead's fields JSONB via Postgres' jsonb `||` operator (right-hand operand —
// extra — wins on key conflicts), in a single UPDATE. No read-modify-write
// window: unlike a Get-then-UpdateBead sequence, a concurrent merge from
// another caller can never be silently overwritten by a stale snapshot. See
// the MergeBeadFields doc comment on store.Store for why this replaced that
// pattern (gd-P6uCMzQqU9).
func queryMergeBeadFields(ctx context.Context, db executor, id string, extra map[string]any) (*model.Bead, error) {
	if len(extra) == 0 {
		return queryGetBead(ctx, db, id)
	}

	patch, err := json.Marshal(extra)
	if err != nil {
		return nil, fmt.Errorf("marshal fields patch: %w", err)
	}

	row := db.QueryRowContext(ctx, `
		UPDATE beads SET
			fields = COALESCE(fields, '{}'::jsonb) || $2::jsonb,
			updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING `+beadColumns,
		id, patch,
	)
	return scanBead(row)
}

func queryCloseBead(ctx context.Context, db executor, id string, closedBy string) (*model.Bead, error) {
	row := db.QueryRowContext(ctx, `
		UPDATE beads
		SET status = 'closed', closed_at = NOW(), closed_by = $2, updated_at = NOW()
		WHERE id = $1
		RETURNING `+beadColumns+beadLabelsSelectSuffix,
		id, closedBy,
	)
	b, err := scanWithLabels(row, scanBead)
	if err != nil {
		return nil, err
	}

	// Fetch relational data.
	deps, err := queryGetDependencies(ctx, db, id)
	if err != nil {
		return nil, err
	}
	b.Dependencies = deps

	comments, err := queryGetComments(ctx, db, id)
	if err != nil {
		return nil, err
	}
	b.Comments = comments

	return b, nil
}

// queryDeleteBead performs a SOFT delete: stamps deleted_at = NOW() so the
// bead can be restored within the retention window. Hard-delete is a separate
// future operation (purge-expired sweep). Idempotent on already-deleted rows
// in the sense that re-stamping is a no-op SQL-side; we still report
// sql.ErrNoRows when the id does not exist.
func queryDeleteBead(ctx context.Context, db executor, id string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE beads SET deleted_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		// Disambiguate: was the row missing entirely, or already soft-deleted?
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM beads WHERE id = $1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return sql.ErrNoRows
		}
		// Already soft-deleted: treat as success (idempotent).
	}
	return nil
}

// queryRestoreBead clears deleted_at, returning the restored bead. Returns
// sql.ErrNoRows when the id does not exist or the bead is not currently
// soft-deleted (callers should check the latter explicitly if they want a
// distinct error).
func queryRestoreBead(ctx context.Context, db executor, id string) (*model.Bead, error) {
	row := db.QueryRowContext(ctx, `
		UPDATE beads SET deleted_at = NULL, updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NOT NULL
		RETURNING `+beadColumns+beadLabelsSelectSuffix,
		id,
	)
	b, err := scanWithLabels(row, scanBead)
	if err != nil {
		return nil, err
	}
	deps, err := queryGetDependencies(ctx, db, id)
	if err != nil {
		return nil, err
	}
	b.Dependencies = deps
	comments, err := queryGetComments(ctx, db, id)
	if err != nil {
		return nil, err
	}
	b.Comments = comments
	return b, nil
}

func queryTransferBead(ctx context.Context, db executor, id, toFleet string) (*model.Bead, string, error) {
	// Capture old fleet before updating.
	var fromFleet string
	if err := db.QueryRowContext(ctx, `SELECT fleet FROM beads WHERE id = $1`, id).Scan(&fromFleet); err != nil {
		return nil, "", err
	}

	row := db.QueryRowContext(ctx, `
		UPDATE beads SET fleet = $2, updated_at = NOW()
		WHERE id = $1
		RETURNING `+beadColumns+beadLabelsSelectSuffix,
		id, toFleet,
	)
	b, err := scanWithLabels(row, scanBead)
	if err != nil {
		return nil, "", err
	}

	// Update related rows to match the new fleet.
	if _, err := db.ExecContext(ctx, `UPDATE deps SET fleet = $2 WHERE bead_id = $1`, id, toFleet); err != nil {
		return nil, "", fmt.Errorf("updating deps fleet: %w", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE labels SET fleet = $2 WHERE bead_id = $1`, id, toFleet); err != nil {
		return nil, "", fmt.Errorf("updating labels fleet: %w", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE comments SET fleet = $2 WHERE bead_id = $1`, id, toFleet); err != nil {
		return nil, "", fmt.Errorf("updating comments fleet: %w", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE events SET fleet = $2 WHERE bead_id = $1`, id, toFleet); err != nil {
		return nil, "", fmt.Errorf("updating events fleet: %w", err)
	}

	// Fetch relational data.
	deps, err := queryGetDependencies(ctx, db, id)
	if err != nil {
		return nil, "", err
	}
	b.Dependencies = deps

	comments, err := queryGetComments(ctx, db, id)
	if err != nil {
		return nil, "", err
	}
	b.Comments = comments

	return b, fromFleet, nil
}

func queryAddDependency(ctx context.Context, db executor, dep *model.Dependency) error {
	// ON CONFLICT CLAIMS a parser-created row for the manual actor (Proposal B,
	// ka-0000006581): when a human/API `kd dep add` targets an edge that already
	// exists as a typed-mention parser row (created_by=typedMentionDepCreator),
	// re-stamp ownership to the manual actor so the reconcile-on-reextract sweep
	// (reconcileTypedMentionDeps) never deletes an edge a human also asserted.
	// The WHERE guard scopes the update to parser rows ONLY — a row already
	// owned by a human makes the predicate false, so the statement is a no-op
	// for it, preserving the prior DO NOTHING behavior for every case except the
	// exact dual-assertion hazard. The parser path (bulkInsertTypedDeps) keeps
	// DO NOTHING, so the parser never overwrites a human claim. operated_by_id
	// is COALESCEd so a nil incoming operator can never violate the NOT NULL
	// column (migration 022) — it keeps the existing value in that case.
	_, err := db.ExecContext(ctx, `
		INSERT INTO deps (bead_id, depends_on_id, type, created_at, created_by, metadata, fleet, operated_by_id)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE((SELECT fleet FROM beads WHERE id = $1), 'default'), $7)
		ON CONFLICT (bead_id, depends_on_id, type) DO UPDATE
			SET created_by = EXCLUDED.created_by,
			    operated_by_id = COALESCE(EXCLUDED.operated_by_id, deps.operated_by_id)
			WHERE deps.created_by = $8`,
		dep.BeadID,
		dep.DependsOnID,
		string(dep.Type),
		dep.CreatedAt,
		dep.CreatedBy,
		dep.Metadata,
		nullInt64Ptr(dep.OperatedByID),
		typedMentionDepCreator,
	)
	return err
}

// queryRemoveDependency deletes one dep edge and reports whether a row actually
// matched. A 0-match is NOT an error — idempotent callers (`kd reparent`,
// `kd admin backfill-mr-edges`) depend on removing an already-absent edge
// succeeding — so the count is returned for the caller to interpret rather than
// turned into an error here (ka-0000001966). (bead_id, depends_on_id, type) is
// the deps PRIMARY KEY (migration 001), so removed is only ever 0 or 1.
func queryRemoveDependency(ctx context.Context, db executor, beadID, dependsOnID string, depType model.DependencyType) (int64, error) {
	res, err := db.ExecContext(ctx, `
		DELETE FROM deps
		WHERE bead_id = $1 AND depends_on_id = $2 AND type = $3`,
		beadID, dependsOnID, string(depType),
	)
	if err != nil {
		return 0, err
	}
	// RowsAffected is advertised as optional by database/sql. lib/pq does
	// implement it, but a driver that doesn't would report 0 for a delete that
	// really happened — which would surface as a bogus "no such dependency".
	// Treat an unsupported count as "assume it matched": the pre-existing
	// behavior, and the safe direction for a signal that only ever adds a
	// warning on top of a delete that already succeeded.
	n, err := res.RowsAffected()
	if err != nil {
		return 1, nil
	}
	return n, nil
}

func queryGetDependencies(ctx context.Context, db executor, beadID string) ([]*model.Dependency, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT bead_id, depends_on_id, type, created_at, created_by, metadata
		FROM deps
		WHERE bead_id = $1`,
		beadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDependencies(rows)
}

func queryGetReverseDependencies(ctx context.Context, db executor, beadID string) ([]*model.Dependency, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT bead_id, depends_on_id, type, created_at, created_by, metadata
		FROM deps
		WHERE depends_on_id = $1`,
		beadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDependencies(rows)
}

// queryGetDependencyEdgesByType returns every dep row of the given type as
// (bead_id, depends_on_id) edges across the whole graph, excluding rows whose
// endpoints resolve to a soft-deleted (or missing) bead. Used by the
// blocks-cycle audit (ka-0000007984) to reconstruct the single-type edge set
// for SCC detection.
func queryGetDependencyEdgesByType(ctx context.Context, db executor, depType model.DependencyType) ([]model.DependencyEdge, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT d.bead_id, d.depends_on_id
		FROM deps d
		JOIN beads src ON src.id = d.bead_id AND src.deleted_at IS NULL
		JOIN beads dst ON dst.id = d.depends_on_id AND dst.deleted_at IS NULL
		WHERE d.type = $1`,
		string(depType),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []model.DependencyEdge
	for rows.Next() {
		var e model.DependencyEdge
		if err := rows.Scan(&e.BeadID, &e.DependsOnID); err != nil {
			return nil, fmt.Errorf("scan dependency edge: %w", err)
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

func queryAddLabel(ctx context.Context, db executor, beadID, label string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO labels (bead_id, label, fleet)
		VALUES ($1, $2, COALESCE((SELECT fleet FROM beads WHERE id = $1), 'default'))
		ON CONFLICT DO NOTHING`,
		beadID, label,
	)
	if err != nil || !isProjectLabel(label) {
		return err
	}
	return querySyncBeadProject(ctx, db, beadID)
}

// projectFromLabelsSQL is beads.project's value, computed from the row's
// labels (migration 057): the project when the bead has exactly one project:
// label, NULL otherwise. It reads the correlated beads row, so it is only
// valid inside an UPDATE beads.
const projectFromLabelsSQL = `(SELECT CASE WHEN count(*) = 1 THEN substr(min(labels.label), 9) END
		FROM labels WHERE labels.bead_id = beads.id AND labels.label LIKE 'project:_%')`

// isProjectLabel reports whether label is a project:<x> label, the only kind
// whose add or remove moves beads.project.
func isProjectLabel(label string) bool {
	return strings.HasPrefix(label, "project:")
}

// querySyncBeadProject rewrites beads.project from the bead's labels after a
// project: label was added or removed. A bead with arms that moves into a
// project where those arms are taken fails here with the register index's
// 23505 (idx_beads_heraldry_register). The row is left alone when the value
// is unchanged, so the beads triggers do not fire for nothing.
func querySyncBeadProject(ctx context.Context, db executor, beadID string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE beads SET project = `+projectFromLabelsSQL+`
		WHERE id = $1 AND project IS DISTINCT FROM `+projectFromLabelsSQL,
		beadID,
	)
	return err
}

// queryRemoveLabel deletes one label and reports whether a row actually
// matched. A 0-match is NOT an error — idempotent callers (`kd update
// --remove-label`, the controller's label reconcilers) depend on removing an
// already-absent label succeeding — so the count is returned for the caller to
// interpret rather than turned into an error here (ka-0000000424).
// (bead_id, label) is the labels PRIMARY KEY (migration 001), so removed is
// only ever 0 or 1.
func queryRemoveLabel(ctx context.Context, db executor, beadID, label string) (int64, error) {
	res, err := db.ExecContext(ctx, `
		DELETE FROM labels
		WHERE bead_id = $1 AND label = $2`,
		beadID, label,
	)
	if err != nil {
		return 0, err
	}
	// RowsAffected is advertised as optional by database/sql. lib/pq does
	// implement it, but a driver that doesn't would report 0 for a delete that
	// really happened — which would surface as a bogus "no such label". Treat
	// an unsupported count as "assume it matched": the pre-existing behavior,
	// and the safe direction for a signal that only ever adds a warning on top
	// of a delete that already succeeded.
	n, err := res.RowsAffected()
	if err != nil {
		n = 1
	}
	if n > 0 && isProjectLabel(label) {
		if err := querySyncBeadProject(ctx, db, beadID); err != nil {
			return 0, err
		}
	}
	return n, nil
}

func queryGetLabels(ctx context.Context, db executor, beadID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT label FROM labels WHERE bead_id = $1`,
		beadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

// queryGetTaintsForBead returns the ids of taints applied to beadID whose
// effects include "gate" (docs/adr/taints.md §15.2, gd-9YQiNJVW6u item 5) —
// the read gate, write gate, and list predicate consult only these. Order is
// unspecified; callers treat the result as a set. Use
// queryGetAllTaintsForBead for propagation, which is effect-agnostic.
func queryGetTaintsForBead(ctx context.Context, db executor, beadID string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT bt.taint_id FROM bead_taints bt
		JOIN taints t ON t.id = bt.taint_id
		WHERE bt.bead_id = $1 AND 'gate' = ANY(t.effects)`,
		beadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaintIDs(rows)
}

// queryGetAllTaintsForBead returns every taint id applied to beadID via
// bead_taints, regardless of effects. Taint propagation (docs/adr/taints.md
// §4.2) is effect-agnostic — an inherited taint carries whatever effects it
// has, even {} or {restrict} — so propagation call sites use this instead of
// the gate-filtered queryGetTaintsForBead.
func queryGetAllTaintsForBead(ctx context.Context, db executor, beadID string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT taint_id FROM bead_taints WHERE bead_id = $1`,
		beadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaintIDs(rows)
}

// queryBeadType returns the type column of the live (non-soft-deleted) bead
// id and whether a row was found. Used by querySetTaint to validate
// owner_team without paying for queryGetBead's full label/dep/comment join.
func queryBeadType(ctx context.Context, db executor, id string) (model.BeadType, bool, error) {
	var t model.BeadType
	err := db.QueryRowContext(ctx, `SELECT type FROM beads WHERE id = $1 AND deleted_at IS NULL`, id).Scan(&t)
	switch {
	case err == nil:
		return t, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	default:
		return "", false, err
	}
}

// querySetTaint creates or updates (by name) a row in the taints registry,
// per ADR docs/adr/taints.md §3/§6.2/§10.1. This is the only write path into
// the taints table, so both validations the ADR requires happen here rather
// than at a higher layer: scrub_policy's shape (§3/§6.2) and owner_team
// resolving to a live type:team bead. Both failures come back as a
// *model.ValidationError, which the HTTP layer maps to 400.
func querySetTaint(ctx context.Context, db executor, t *model.Taint) error {
	var ve model.ValidationError

	if t.Name == "" {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "name", Message: "is required"})
	}

	if t.OwnerTeam == "" {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "owner_team", Message: "is required"})
	} else {
		ownerType, found, err := queryBeadType(ctx, db, t.OwnerTeam)
		if err != nil {
			return fmt.Errorf("resolve owner_team: %w", err)
		}
		if !found || ownerType != model.TypeTeam {
			ve.Errors = append(ve.Errors, model.FieldError{
				Field:   "owner_team",
				Message: fmt.Sprintf("%q does not resolve to a live type:team bead", t.OwnerTeam),
			})
		}
	}

	// Effects default to {gate} when omitted (docs/adr/taints.md §15.2),
	// matching the column default and today's only behavior.
	effects := t.Effects
	if effects == nil {
		effects = model.DefaultEffects()
	}
	if err := model.ValidateEffects(effects); err != nil {
		var inner *model.ValidationError
		if errors.As(err, &inner) {
			ve.Errors = append(ve.Errors, inner.Errors...)
		} else {
			return err
		}
	}

	// A taint created with no scrub_policy gets the §10.1 default; one
	// supplied gets validated against §3/§6.2/§15.4 and stored byte-for-byte
	// so GetTaint/ListTaints round-trip it unchanged.
	policy := t.ScrubPolicy
	if len(policy) == 0 || string(policy) == "null" {
		encoded, err := json.Marshal(model.DefaultScrubPolicy(t.OwnerTeam))
		if err != nil {
			return fmt.Errorf("encode default scrub_policy: %w", err)
		}
		policy = encoded
	} else {
		var parsed model.ScrubPolicy
		if err := json.Unmarshal(policy, &parsed); err != nil {
			ve.Errors = append(ve.Errors, model.FieldError{Field: "scrub_policy", Message: "must be valid JSON: " + err.Error()})
		} else if err := model.ValidateScrubPolicy(&parsed, effects); err != nil {
			var inner *model.ValidationError
			if errors.As(err, &inner) {
				ve.Errors = append(ve.Errors, inner.Errors...)
			} else {
				return err
			}
		}
	}

	if ve.HasErrors() {
		return &ve
	}

	err := db.QueryRowContext(ctx, `
		INSERT INTO taints (name, description, owner_team, scrub_policy, effects)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (name) DO UPDATE
			SET description = $2, owner_team = $3, scrub_policy = $4, effects = $5, updated_at = NOW()
		RETURNING id, created_at, updated_at`,
		t.Name, t.Description, t.OwnerTeam, []byte(policy), pq.Array(effects),
	).Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return err
	}
	t.ScrubPolicy = policy
	t.Effects = effects
	return nil
}

// queryGetTaint returns the taint named name, or sql.ErrNoRows if none
// exists.
func queryGetTaint(ctx context.Context, db executor, name string) (*model.Taint, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, name, description, owner_team, scrub_policy, effects, created_at, updated_at, retired_at
		FROM taints WHERE name = $1`, name)
	return scanTaint(row)
}

// queryListTaints returns every taint in the registry, ordered by name.
func queryListTaints(ctx context.Context, db executor) ([]*model.Taint, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, description, owner_team, scrub_policy, effects, created_at, updated_at, retired_at
		FROM taints ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaints(rows)
}

func queryAddComment(ctx context.Context, db executor, c *model.Comment) error {
	return db.QueryRowContext(ctx, `
		INSERT INTO comments (bead_id, author, text, fleet, operated_by_id, impersonated_by)
		VALUES ($1, $2, $3, COALESCE((SELECT fleet FROM beads WHERE id = $1), 'default'), $4, $5)
		RETURNING id, created_at`,
		c.BeadID, c.Author, c.Text, nullInt64Ptr(c.OperatedByID), c.ImpersonatedBy,
	).Scan(&c.ID, &c.CreatedAt)
}

// queryDeleteComment removes a single comment row keyed by (bead_id, id) and
// returns the deleted row so callers can preserve author/text in the audit
// event payload. Returns (nil, nil) when no row matched — DeleteComment is
// idempotent at the API layer. The author column is NULLABLE post-resolver
// (ka-0000001391); we COALESCE to empty string for the returned model.
func queryDeleteComment(ctx context.Context, db executor, beadID string, commentID int64) (*model.Comment, error) {
	row := db.QueryRowContext(ctx, `
		DELETE FROM comments
		WHERE id = $1 AND bead_id = $2
		RETURNING id, bead_id, COALESCE(author, ''), text, created_at`,
		commentID, beadID,
	)
	var c model.Comment
	if err := row.Scan(&c.ID, &c.BeadID, &c.Author, &c.Text, &c.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// queryTrimComments deletes every comment on beadID except the most recent
// keepLast (ordered by created_at DESC, id DESC for ties). When authorFilter
// is non-empty the keep-window AND the deletion are both restricted to that
// author so trimming the scheduler's history leaves human / agent commentary
// untouched. Returns the deleted rows in ascending creation order — the
// server layer fans out one audit event per row.
func queryTrimComments(ctx context.Context, db executor, beadID string, keepLast int, authorFilter string) ([]*model.Comment, error) {
	if keepLast < 0 {
		return nil, fmt.Errorf("keepLast must be >= 0, got %d", keepLast)
	}
	// The keep window is computed in a CTE over the same author-filtered
	// scope as the DELETE. NULLIF on the filter lets a single $3 parameter
	// drive both "filter to author X" and "no filter" branches:
	//   ($3::text IS NULL OR author = $3)  →  author predicate
	// Postgres treats NULLIF('', '') as NULL so the empty-string client
	// arg falls through to the no-filter branch.
	rows, err := db.QueryContext(ctx, `
		WITH keep AS (
			SELECT id
			  FROM comments
			 WHERE bead_id = $1
			   AND (NULLIF($3, '') IS NULL OR author = $3)
			 ORDER BY created_at DESC, id DESC
			 LIMIT $2
		)
		DELETE FROM comments
		 WHERE bead_id = $1
		   AND (NULLIF($3, '') IS NULL OR author = $3)
		   AND id NOT IN (SELECT id FROM keep)
		RETURNING id, bead_id, COALESCE(author, ''), text, created_at`,
		beadID, keepLast, authorFilter,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Comment
	for rows.Next() {
		var c model.Comment
		if err := rows.Scan(&c.ID, &c.BeadID, &c.Author, &c.Text, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func queryGetComments(ctx context.Context, db executor, beadID string) ([]*model.Comment, error) {
	// LEFT JOIN operators on comments.operated_by_id so per-comment chips
	// resolve to canonical "kind:subject" + display + email. Empty when the
	// resolver did not match an operator (history-only NULLABLE rows).
	rows, err := db.QueryContext(ctx, `
		SELECT comments.id, comments.bead_id, comments.author, comments.text, comments.created_at,
			COALESCE(op.kind || ':' || op.subject, '') AS operated_by,
			COALESCE(op.display, '') AS operated_by_display,
			COALESCE(op.email,   '') AS operated_by_email,
			comments.impersonated_by
		FROM comments
		LEFT JOIN operators op ON op.id = comments.operated_by_id
		WHERE comments.bead_id = $1
		ORDER BY comments.created_at ASC`,
		beadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommentsFull(rows)
}

func queryRecordEvent(ctx context.Context, db executor, e *model.Event) error {
	return db.QueryRowContext(ctx, `
		INSERT INTO events (topic, bead_id, actor, payload, fleet, operated_by_id)
		VALUES ($1, $2, $3, $4, COALESCE((SELECT fleet FROM beads WHERE id = $2), 'default'), $5)
		RETURNING id, created_at`,
		e.Topic, e.BeadID, e.Actor, []byte(e.Payload), nullInt64Ptr(e.OperatedByID),
	).Scan(&e.ID, &e.CreatedAt)
}

// queryEventHistogram returns time-bucketed counts of rows in the events
// table for [since, until]. The bucket argument must be a date_trunc field
// name from a fixed allowlist (see allowedHistogramBuckets); callers at the
// HTTP layer normalize "5m"/"15m" / "1d" to one of these values.
func queryEventHistogram(ctx context.Context, db executor, since, until time.Time, bucket string, scope model.EventReadScope) ([]model.EventHistogramBucket, error) {
	if !allowedHistogramBuckets[bucket] {
		return nil, fmt.Errorf("invalid histogram bucket %q", bucket)
	}
	// Scope the aggregation at the SQL level — post-filtering bucket counts is
	// impossible, so an out-of-scope caller's counts must be excluded before the
	// GROUP BY (ka-0000006863). events carry no labels, so gate via the bead's
	// labels; the placeholders follow the fixed bucket/since/until args.
	args := []any{bucket, since, until}
	scopeSQL := eventScopeSQL(scope, &args)
	rows, err := db.QueryContext(ctx, `
		SELECT date_trunc($1, created_at) AS bucket, count(*)
		FROM events
		WHERE created_at >= $2 AND created_at <= $3`+scopeSQL+`
		GROUP BY bucket
		ORDER BY bucket`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.EventHistogramBucket
	for rows.Next() {
		var b model.EventHistogramBucket
		if err := rows.Scan(&b.T, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// allowedHistogramBuckets is the fixed allowlist of date_trunc field names
// the histogram query may be parameterised with. Even though we pass bucket
// as a parameter rather than concatenating it into SQL, validating against
// this set is a defense-in-depth check that also rejects invalid Postgres
// fields (e.g. "century") that the API never wants to expose.
var allowedHistogramBuckets = map[string]bool{
	"minute": true,
	"hour":   true,
	"day":    true,
}

func queryGetEvents(ctx context.Context, db executor, beadID string) ([]*model.Event, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT events.id, events.topic, events.bead_id, events.actor, events.payload, events.created_at,
			COALESCE(op.kind || ':' || op.subject, '') AS operated_by
		FROM events
		LEFT JOIN operators op ON op.id = events.operated_by_id
		WHERE events.bead_id = $1
		ORDER BY events.created_at ASC`,
		beadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// queryGetRecentEvents returns the most-recent `limit` events for a bead,
// ordered chronologically ASC for backward-compatible payload shape. Selects
// newest rows by events.id DESC (events.id is monotonic), then re-sorts.
// Backs the ?limit=N query parameter on GET /v1/beads/{id}/events; introduced
// to keep beads-viewer from OOMKilling itself on trigger beads whose event
// history can exceed 30 MB (bug ka-0000002256).
func queryGetRecentEvents(ctx context.Context, db executor, beadID string, limit int) ([]*model.Event, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, topic, bead_id, actor, payload, created_at, operated_by FROM (
			SELECT events.id, events.topic, events.bead_id, events.actor, events.payload, events.created_at,
				COALESCE(op.kind || ':' || op.subject, '') AS operated_by
			FROM events
			LEFT JOIN operators op ON op.id = events.operated_by_id
			WHERE events.bead_id = $1
			ORDER BY events.id DESC
			LIMIT $2
		) AS recent
		ORDER BY created_at ASC`,
		beadID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// queryGetEventsByTopic is queryGetRecentEvents narrowed to one topic: the
// newest `limit` events on beadID with events.topic = topic, re-sorted ASC.
// limit <= 0 binds a NULL LIMIT, which Postgres treats as LIMIT ALL. Backs
// ?topic= on GET /v1/beads/{id}/events (gd-cP9tODid8j): the trigger-engine's
// per-candidate label.added / label.removed scans used to pull 200 events of
// every topic (bead.updated rows carry whole bead bodies) to find one label
// event.
func queryGetEventsByTopic(ctx context.Context, db executor, beadID, topic string, limit int) ([]*model.Event, error) {
	var lim any
	if limit > 0 {
		lim = limit
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, topic, bead_id, actor, payload, created_at, operated_by FROM (
			SELECT events.id, events.topic, events.bead_id, events.actor, events.payload, events.created_at,
				COALESCE(op.kind || ':' || op.subject, '') AS operated_by
			FROM events
			LEFT JOIN operators op ON op.id = events.operated_by_id
			WHERE events.bead_id = $1 AND events.topic = $2
			ORDER BY events.id DESC
			LIMIT $3
		) AS recent
		ORDER BY created_at ASC`,
		beadID, topic, lim,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// queryLastEventAtByOperatorAliases returns the most recent events.created_at
// for each requested `kind:subject` alias. Resolves the alias through
// operator_aliases → operators(id) → events.operated_by_id. Aliases with no
// events (or unknown to operator_aliases) are omitted from the result map.
func queryLastEventAtByOperatorAliases(ctx context.Context, db executor, aliases []string) (map[string]time.Time, error) {
	out := make(map[string]time.Time, len(aliases))
	if len(aliases) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT (oa.kind || ':' || oa.subject) AS alias, MAX(e.created_at) AS last_at
		FROM operator_aliases oa
		JOIN events e ON e.operated_by_id = oa.operator_id
		WHERE (oa.kind || ':' || oa.subject) = ANY($1::text[])
		GROUP BY oa.kind, oa.subject`,
		pq.Array(aliases),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var alias string
		var t time.Time
		if err := rows.Scan(&alias, &t); err != nil {
			return nil, err
		}
		out[alias] = t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// queryGetEventsByTimeRange returns events in [since, until] oldest-first,
// paged forward with a keyset cursor (0 starts at the oldest).
//
// Keyset pagination is only correct when the cursor comparison uses the SAME
// key as the ORDER BY. The sort key is the composite (created_at, id): events
// pairs a BIGSERIAL id (assigned at INSERT execution, strictly monotonic) with
// a created_at that DEFAULTs to NOW() (transaction-start time), and under
// concurrent event writes those two orderings can disagree — a row that
// committed with a later NOW() can carry a lower id. The cursor is transported
// as a bare event id (the wire/consumer contract), so we reconstruct the full
// (created_at, id) boundary by looking up the cursor row's created_at and
// compare the composite tuple: (created_at, id) > (cursor_created_at,
// cursor_id). The earlier `events.id > $3` predicate compared only id against a
// created_at-ordered scan, which silently DROPPED rows (sorts after the page's
// last row but has id <= cursor, so never returned on any page) and DUPLICATED
// others (id > cursor but sorts before the page boundary) whenever id and
// created_at order disagreed. Ordering stays (created_at, id) so the
// idx_events_created_at_id index still satisfies the range + order with no sort
// (migration 035); the cursor row's created_at is a single pkey lookup.
func queryGetEventsByTimeRange(ctx context.Context, db executor, since, until time.Time, cursor int64, limit int, scope model.EventReadScope) ([]*model.Event, error) {
	args := []any{since, until, cursor, limit}
	scopeSQL := eventScopeSQL(scope, &args)
	rows, err := db.QueryContext(ctx, `
		SELECT events.id, events.topic, events.bead_id, events.actor, events.payload, events.created_at,
			COALESCE(op.kind || ':' || op.subject, '') AS operated_by
		FROM events
		LEFT JOIN operators op ON op.id = events.operated_by_id
		WHERE events.created_at >= $1 AND events.created_at <= $2
		  AND ($3 = 0 OR (events.created_at, events.id) >
		        ((SELECT c.created_at FROM events c WHERE c.id = $3), $3))`+scopeSQL+`
		ORDER BY events.created_at ASC, events.id ASC
		LIMIT $4`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// eventScopeSQL appends the caller's project-visibility predicates (an
// EventReadScope) to the argument slice and returns the SQL fragment that
// gates events by their bead's labels. Events carry no labels of their own, so
// each predicate is an EXISTS sub-query against the labels table keyed on
// events.bead_id — the events-feed analogue of the label EXISTS predicates
// queryListBeads builds, and mirroring blockerReadScope's scoping in
// batch_queries.go. An unrestricted scope contributes nothing (ka-0000006863).
//
// RequireAllLabels ANDs each label in (project-bound fleet token); every label
// must be present on the bead. RequireAnyLabels ORs its set in via ANY(...)
// (OIDC allowed_projects); at least one must be present. Placeholders are
// appended after the caller's fixed args so the two compose safely.
func eventScopeSQL(scope model.EventReadScope, args *[]any) string {
	scopeSQL := ""
	for _, label := range scope.RequireAllLabels {
		*args = append(*args, label)
		scopeSQL += fmt.Sprintf(
			" AND EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = events.bead_id AND labels.label = $%d)", len(*args))
	}
	if len(scope.RequireAnyLabels) > 0 {
		*args = append(*args, pq.Array(scope.RequireAnyLabels))
		scopeSQL += fmt.Sprintf(
			" AND EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = events.bead_id AND labels.label = ANY($%d::text[]))", len(*args))
	}
	// TaintGrantIDs (ADR docs/adr/taints.md §4.3/§5.1/§15.2): events carry no
	// taint columns, so gate them by joining through bead_taints on
	// events.bead_id the same way the two label predicates above join
	// through labels, filtered to gate-effect taints only — a taint whose
	// effects exclude "gate" must never hide an event. `<> ALL($N)` mirrors
	// queryListBeads's TaintGrantIDs clause — an empty grant set correctly
	// excludes every event of a gate-tainted bead. Nil (the field's zero
	// value) means the gate does not apply.
	if scope.TaintGrantIDs != nil {
		*args = append(*args, pq.Array(*scope.TaintGrantIDs))
		scopeSQL += fmt.Sprintf(
			" AND NOT EXISTS (SELECT 1 FROM bead_taints bt JOIN taints t ON t.id = bt.taint_id WHERE bt.bead_id = events.bead_id AND 'gate' = ANY(t.effects) AND bt.taint_id <> ALL($%d::bigint[]))", len(*args))
	}
	// ExcludeBeadTypes: drop events whose bead has a listed type. A bead the
	// events row no longer joins to (deleted) is kept, as before.
	if len(scope.ExcludeBeadTypes) > 0 {
		*args = append(*args, pq.Array(scope.ExcludeBeadTypes))
		scopeSQL += fmt.Sprintf(
			" AND NOT EXISTS (SELECT 1 FROM beads xb WHERE xb.id = events.bead_id AND xb.type = ANY($%d::text[]))", len(*args))
	}
	return scopeSQL
}

// queryGetEventsByTimeRangeDesc is the newest-first companion to
// queryGetEventsByTimeRange. It pages BACKWARD from the newest event in
// [since, until]: cursor is the lowest event id already consumed, so only
// events with id < cursor are returned (cursor=0 starts at the newest). This
// lets a caller that only wants the most-recent N events bound its work to
// exactly that many rows instead of walking the whole window oldest-first and
// truncating at a page cap before it ever reaches recent events (the /feed
// first-paint seed, bug ka-0000004135).
//
// Like the ASC companion, the cursor comparison uses the same composite
// (created_at, id) key the ORDER BY sorts on — reconstructed from the bare
// cursor id via a pkey lookup — not a bare `events.id < $3`, which silently
// drops/duplicates events across page boundaries when id and created_at order
// disagree (see queryGetEventsByTimeRange). The ORDER BY stays (created_at DESC,
// id DESC) so Postgres walks idx_events_created_at_id backward with no sort.
func queryGetEventsByTimeRangeDesc(ctx context.Context, db executor, since, until time.Time, cursor int64, limit int, scope model.EventReadScope) ([]*model.Event, error) {
	args := []any{since, until, cursor, limit}
	scopeSQL := eventScopeSQL(scope, &args)
	rows, err := db.QueryContext(ctx, `
		SELECT events.id, events.topic, events.bead_id, events.actor, events.payload, events.created_at,
			COALESCE(op.kind || ':' || op.subject, '') AS operated_by
		FROM events
		LEFT JOIN operators op ON op.id = events.operated_by_id
		WHERE events.created_at >= $1 AND events.created_at <= $2
		  AND ($3 = 0 OR (events.created_at, events.id) <
		        ((SELECT c.created_at FROM events c WHERE c.id = $3), $3))`+scopeSQL+`
		ORDER BY events.created_at DESC, events.id DESC
		LIMIT $4`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func querySetConfig(ctx context.Context, db executor, c *model.Config) error {
	return db.QueryRowContext(ctx, `
		INSERT INTO configs (key, value)
		VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW()
		RETURNING created_at, updated_at`,
		c.Key, []byte(c.Value),
	).Scan(&c.CreatedAt, &c.UpdatedAt)
}

func queryGetConfig(ctx context.Context, db executor, key string) (*model.Config, error) {
	row := db.QueryRowContext(ctx, `
		SELECT key, value, created_at, updated_at
		FROM configs WHERE key = $1`, key)
	return scanConfig(row)
}

func queryListConfigs(ctx context.Context, db executor, namespace string) ([]*model.Config, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT key, value, created_at, updated_at
		FROM configs WHERE key LIKE $1 || ':%'
		ORDER BY key`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConfigs(rows)
}

func queryListAllConfigs(ctx context.Context, db executor) ([]*model.Config, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT key, value, created_at, updated_at
		FROM configs ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConfigs(rows)
}

func queryDeleteConfig(ctx context.Context, db executor, key string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM configs WHERE key = $1`, key)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func queryGetStats(ctx context.Context, db executor) (*model.GraphStats, error) {
	stats := &model.GraphStats{}
	err := db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN status = 'open' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'in_progress' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'blocked' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'closed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'deferred' THEN 1 ELSE 0 END), 0)
		FROM beads`).Scan(
		&stats.TotalOpen,
		&stats.TotalInProgress,
		&stats.TotalBlocked,
		&stats.TotalClosed,
		&stats.TotalDeferred,
	)
	if err != nil {
		return nil, fmt.Errorf("stats: %w", err)
	}
	return stats, nil
}

// parseSortClause builds the ORDER BY body for queryListBeads.
//
// relevanceExpr is the search-relevance CASE expression assembled by the
// filter.Search block (empty when there is no text search). When non-empty it
// is prepended as the PRIMARY sort key (highest title-match score first) and a
// `beads.created_at DESC` tiebreak is always appended last, so within equal
// relevance the most-recent bead still wins. An exact-title-substring match
// (CASE score ≥ 2) therefore outranks a recency-only match (score 0) even for a
// high-frequency single term — symptom B of ka-0000000507. Any explicit user
// `sort` columns slot between relevance and the created_at tiebreak.
//
// Every path ends with the unique `beads.id` term so LIMIT/OFFSET pagination is
// deterministic: the allowed sort columns are all low-cardinality (even
// created_at is not unique), and without a unique final key Postgres gives no
// stable order on tied rows, so a tied bead could appear on two pages while
// another is skipped (ka-0000002466).
func parseSortClause(sort, relevanceExpr string) string {
	allowed := map[string]bool{
		"priority": true, "created_at": true, "updated_at": true,
		"title": true, "status": true, "type": true,
	}
	parts := strings.Split(sort, ",")
	clauses := make([]string, 0, len(parts))
	sawCreatedAt := false
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		desc := strings.HasPrefix(p, "-")
		col := strings.TrimPrefix(p, "-")
		if !allowed[col] {
			continue
		}
		// Qualify with beads. so the clause is unambiguous when queryListBeads
		// LEFT JOINs operators on operated_by_id (operators has its own
		// created_at / updated_at columns).
		qualified := "beads." + col
		if col == "created_at" {
			sawCreatedAt = true
		}
		if desc {
			clauses = append(clauses, qualified+" DESC")
		} else {
			clauses = append(clauses, qualified+" ASC")
		}
	}

	// beads.id is appended as the final ORDER BY term on EVERY path. The allowed
	// sort columns (priority/status/type/title/created_at/updated_at) are all
	// low-cardinality, and even created_at is not unique, so ties are common.
	// Postgres gives no stable order on tied ORDER BY keys and re-plans per call,
	// so under LIMIT/OFFSET pagination a tied bead can land at a different
	// absolute position on different pages — appearing twice while another is
	// skipped. beads.id is unique, so it makes the total order deterministic and
	// pagination stable across pages (ka-0000002466). Same rationale as the b.id
	// tiebreak in queries_embedding.go.
	if relevanceExpr == "" {
		// No text search: user sort columns (or the created_at default), then the
		// unique id tiebreak.
		if len(clauses) == 0 {
			return "beads.created_at DESC, beads.id"
		}
		return strings.Join(clauses, ", ") + ", beads.id"
	}

	// Text search active: relevance is the primary key, explicit sort columns
	// (if any) refine within equal relevance, created_at DESC refines within
	// equal relevance (skipped only if the caller already sorts on created_at,
	// to avoid a redundant duplicate key), and beads.id is the final unique
	// deterministic tiebreak.
	ordered := make([]string, 0, len(clauses)+3)
	ordered = append(ordered, relevanceExpr+" DESC")
	ordered = append(ordered, clauses...)
	if !sawCreatedAt {
		ordered = append(ordered, "beads.created_at DESC")
	}
	ordered = append(ordered, "beads.id")
	return strings.Join(ordered, ", ")
}
