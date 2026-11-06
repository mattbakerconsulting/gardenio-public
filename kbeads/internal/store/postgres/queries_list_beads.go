package postgres

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
	"github.com/lib/pq"
)

// beadListWhere accumulates the WHERE clauses of a queryListBeads statement
// and the args they bind. Placeholders are numbered in the order nextArg is
// called, so buildBeadListWhere must run its steps in a fixed order: the SQL
// text (and the sqlmock expectations that pin it) depends on that order.
type beadListWhere struct {
	clauses []string
	args    []any
	argIdx  int
	// searchRelevanceExpr is a SQL CASE expression (set by addSearchFilter)
	// that scores title matches. When non-empty it is threaded into ORDER BY
	// via parseSortClause so an exact-title hit outranks a merely-recent
	// match. Empty when there is no text search, or for a CountOnly query.
	searchRelevanceExpr string
}

// nextArg reserves the next positional placeholder ($1, $2, ...).
func (w *beadListWhere) nextArg() string {
	w.argIdx++
	return fmt.Sprintf("$%d", w.argIdx)
}

// sql renders the clauses as " WHERE a AND b", or "" when there are none.
func (w *beadListWhere) sql() string {
	if len(w.clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.clauses, " AND ")
}

// addIn adds `<inOp> ($a, $b, ...)` (inOp is e.g. "status IN" or "type NOT
// IN") with one bound arg per value, or nothing when vals is empty.
func addIn[T ~string](w *beadListWhere, inOp string, vals []T) {
	if len(vals) == 0 {
		return
	}
	placeholders := make([]string, len(vals))
	for i, v := range vals {
		placeholders[i] = w.nextArg()
		w.args = append(w.args, string(v))
	}
	w.clauses = append(w.clauses, inOp+" ("+strings.Join(placeholders, ", ")+")")
}

// buildBeadListWhere turns a filter into the WHERE clauses and args shared by
// queryListBeads' data and count statements. The step order is load-bearing:
// see beadListWhere.
func buildBeadListWhere(filter model.BeadFilter) *beadListWhere {
	w := &beadListWhere{}
	w.addEnumFilters(filter)
	w.addPeopleFilters(filter)
	w.addLabelFilters(filter)
	w.addTaintFilters(filter)
	w.addTimeFilters(filter)
	w.addGraphFilters(filter)
	w.addSearchFilter(filter)
	w.addFieldFilters(filter)
	w.addFieldInFilters(filter)
	w.addQueueFilters(filter)
	w.addStateFilters(filter)
	return w
}

// addEnumFilters: status, type, kind and priority.
func (w *beadListWhere) addEnumFilters(filter model.BeadFilter) {
	addIn(w, "status IN", filter.Status)
	addIn(w, "type IN", filter.Type)
	addIn(w, "type NOT IN", filter.ExcludeTypes)
	if len(filter.ExcludeClosedTypes) > 0 {
		placeholders := make([]string, len(filter.ExcludeClosedTypes))
		for i, t := range filter.ExcludeClosedTypes {
			placeholders[i] = w.nextArg()
			w.args = append(w.args, string(t))
		}
		w.clauses = append(w.clauses, "NOT (status = 'closed' AND type IN ("+strings.Join(placeholders, ", ")+"))")
	}
	addIn(w, "beads.kind IN", filter.Kind)

	if filter.Priority != nil {
		w.clauses = append(w.clauses, "priority = "+w.nextArg())
		w.args = append(w.args, *filter.Priority)
	}

	if filter.PriorityMin != nil {
		w.clauses = append(w.clauses, "priority >= "+w.nextArg())
		w.args = append(w.args, *filter.PriorityMin)
	}

	if filter.PriorityMax != nil {
		w.clauses = append(w.clauses, "priority <= "+w.nextArg())
		w.args = append(w.args, *filter.PriorityMax)
	}
}

// addPeopleFilters: assignee, owner, creator, category and operator.
func (w *beadListWhere) addPeopleFilters(filter model.BeadFilter) {
	// Assignee: scalar (legacy) and Assignees (Phase 3b multi) merged into a
	// single ANY($::text[]) predicate so that ?assignee=foo&assignee_in=bar
	// matches rows where assignee ∈ {foo, bar}.
	if assignees := mergeNonEmpty(filter.Assignee, filter.Assignees); len(assignees) > 0 {
		p := w.nextArg()
		w.clauses = append(w.clauses, "assignee = ANY("+p+"::text[])")
		w.args = append(w.args, pq.Array(assignees))
	}

	// Unassigned: the "nobody on it yet" predicate (kd list/count --unassigned).
	// Both spellings of absent must match — the column is `assignee TEXT DEFAULT
	// ''` (migration 001) so unclaimed beads carry '' in practice, but a NULL is
	// legal and an `assignee = <empty>` alone would silently drop those rows. Held
	// deliberately as its own clause rather than folded into the ANY() above:
	// mergeNonEmpty strips the empty string, which is exactly why `?assignee=`
	// could never express this and callers fell back to full-table pull + jq
	// (ka-0000007209).
	if filter.Unassigned {
		w.clauses = append(w.clauses, "(assignee IS NULL OR assignee = '')")
	}

	if owners := mergeNonEmpty(filter.Owner, filter.Owners); len(owners) > 0 {
		p := w.nextArg()
		w.clauses = append(w.clauses, "owner = ANY("+p+"::text[])")
		w.args = append(w.args, pq.Array(owners))
	}

	if creators := mergeNonEmpty(filter.CreatedBy, filter.CreatedBys); len(creators) > 0 {
		p := w.nextArg()
		w.clauses = append(w.clauses, "created_by = ANY("+p+"::text[])")
		w.args = append(w.args, pq.Array(creators))
	}

	// Legacy --operated-by TEXT filter removed in P6 (migration 024). Callers
	// use OperatedByID for direct FK matches and OperatedByAliases for
	// "kind:subject" lookups via operator_aliases (handled below). The
	// "no human attribution" predicate is now operated_by_id = sentinel.

	// Categories: filter on materialized beads.category column. Powers the
	// inbox v1 §1.2.1 query shape and chip-strip variants.
	if len(filter.Categories) > 0 {
		p := w.nextArg()
		w.clauses = append(w.clauses, "category = ANY("+p+"::text[])")
		w.args = append(w.args, pq.Array(filter.Categories))
	}

	// OperatedByID: filter on the operators(id) FK. Single-equality predicate
	// served by idx_beads_inbox_predicate when paired with category='intake'.
	if filter.OperatedByID != nil {
		w.clauses = append(w.clauses, "operated_by_id = "+w.nextArg())
		w.args = append(w.args, *filter.OperatedByID)
	}

	// operated_by_aliases (P4 §5.3): map each "kind:subject" through
	// operator_aliases to operated_by_id, recovering legacy Slack-driven
	// beads under the canonical alias.
	//
	// The comparison is case-INSENSITIVE (LOWER on both sides). An OIDC email
	// is a case-insensitive identifier, but two ingestion paths disagreed on
	// casing: the agent fleet stamps a lowercased "oidc:<email>" while the
	// beads-viewer used to stamp the raw mixed-case X-Auth-Request-Email header
	// verbatim (e.g. oidc:Matthew.Baker@…). That forked ONE human across TWO
	// operator rows, so a "my work" filter for the lowercase form matched only
	// the lowercase-stamped beads and silently hid the rest — the "my plans
	// shows nothing" bug (ka-0000002505). Lowering both the stored alias and the
	// query value reunifies the split identity across every read surface
	// (/plans, /active, /search, /u/{slug}) without a data migration. Slack
	// subjects are ^U[A-Z0-9]+$, so no two distinct subjects differ only by
	// case — lowercasing can never over-match a different human. operator_aliases
	// is a small table, so the LOWER() expression scan in this subquery is cheap.
	if len(filter.OperatedByAliases) > 0 {
		p := w.nextArg()
		lowered := make([]string, len(filter.OperatedByAliases))
		for i, a := range filter.OperatedByAliases {
			lowered[i] = strings.ToLower(a)
		}
		w.clauses = append(w.clauses, `beads.operated_by_id IN (SELECT oa.operator_id FROM operator_aliases oa WHERE LOWER(oa.kind || ':' || oa.subject) = ANY(`+p+`::text[]))`)
		w.args = append(w.args, pq.Array(lowered))
	}

	if filter.UpdatedBefore != nil {
		w.clauses = append(w.clauses, "beads.updated_at < "+w.nextArg())
		w.args = append(w.args, *filter.UpdatedBefore)
	}
}

// addLabelFilters: label, title, project and id predicates.
func (w *beadListWhere) addLabelFilters(filter model.BeadFilter) {
	if len(filter.Labels) > 0 {
		for _, label := range filter.Labels {
			p := w.nextArg()
			w.clauses = append(w.clauses,
				fmt.Sprintf("EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = beads.id AND labels.label = %s)", p))
			w.args = append(w.args, label)
		}
	}

	if len(filter.LabelsAny) > 0 {
		p := w.nextArg()
		w.clauses = append(w.clauses,
			fmt.Sprintf("EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = beads.id AND labels.label = ANY(%s::text[]))", p))
		w.args = append(w.args, pq.Array(filter.LabelsAny))
	}

	// Titles: exact title match (role lookup by name). The unqualified column is
	// beads.title because queryListBeads aliases nothing.
	if len(filter.Titles) > 0 {
		p := w.nextArg()
		w.clauses = append(w.clauses, "beads.title = ANY("+p+"::text[])")
		w.args = append(w.args, pq.Array(filter.Titles))
	}

	// LabelsOrNoProject: one of the labels, or no project label at all (a
	// global bead). `LIKE 'project:_%%'` requires a non-empty project name, the
	// same definition as the server's hasProjectLabel.
	if len(filter.LabelsOrNoProject) > 0 {
		p := w.nextArg()
		w.clauses = append(w.clauses,
			fmt.Sprintf("(EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = beads.id AND labels.label = ANY(%s::text[])) OR NOT EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = beads.id AND labels.label LIKE 'project:_%%'))", p))
		w.args = append(w.args, pq.Array(filter.LabelsOrNoProject))
	}

	// LabelsAnyUser is the user-facing --label-any group. It gets its OWN
	// EXISTS clause so it ANDs with (does not merge into) the OIDC-scoping
	// LabelsAny above — a user any-of narrows within project scope, never
	// widens it (ka-0000003307).
	if len(filter.LabelsAnyUser) > 0 {
		p := w.nextArg()
		w.clauses = append(w.clauses,
			fmt.Sprintf("EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = beads.id AND labels.label = ANY(%s::text[]))", p))
		w.args = append(w.args, pq.Array(filter.LabelsAnyUser))
	}

	if len(filter.ExcludeLabels) > 0 {
		p := w.nextArg()
		w.clauses = append(w.clauses,
			fmt.Sprintf("NOT EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = beads.id AND labels.label = ANY(%s::text[]))", p))
		w.args = append(w.args, pq.Array(filter.ExcludeLabels))
	}

	// ExclusiveProject drops any bead carrying a `project:<other>` label — i.e.
	// one belonging to a project other than the named one. A bead with only the
	// named project's label, or with no project label at all, survives. This is
	// the server-side spelling of the client-side filterExclusiveProject pass
	// (ka-0000003411): pushing it into the WHERE clause lets LIMIT account for the
	// project narrowing, so `gb ready` / `gb news` no longer over-fetch to backfill
	// rows a post-LIMIT client filter would have dropped. The `LIKE 'project:%'`
	// prefix has no LIKE metacharacters in it, so it matches exactly the labels
	// beginning `project:`; the `<> $want` bound keeps the named project's own
	// label (and, via LIKE, every non-project label) from matching the subquery.
	if filter.ExclusiveProject != "" {
		p := w.nextArg()
		w.clauses = append(w.clauses,
			fmt.Sprintf("NOT EXISTS (SELECT 1 FROM labels WHERE labels.bead_id = beads.id AND labels.label LIKE 'project:%%' AND labels.label <> %s)", p))
		w.args = append(w.args, "project:"+filter.ExclusiveProject)
	}

	if len(filter.IDs) > 0 {
		p := w.nextArg()
		w.clauses = append(w.clauses, "beads.id = ANY("+p+"::text[])")
		w.args = append(w.args, pq.Array(filter.IDs))
	}
}

// addTaintFilters: the taint gate and the per-taint filter.
func (w *beadListWhere) addTaintFilters(filter model.BeadFilter) {
	// TaintGrantIDs (ADR docs/adr/taints.md §5.1/§15.2): a bead matches only
	// when it carries no gate-effect bead_taints row outside the caller's
	// grant set — a taint whose effects exclude "gate" (e.g. {} or
	// {restrict}) must never hide a bead, so the join filters to
	// 'gate' = ANY(t.effects). `<> ALL($N)` rather than a literal `NOT
	// taint_id IN (...)` so an empty grant set (caller holds zero grants) is
	// vacuously true for every row of the inner SELECT and correctly
	// excludes every gate-tainted bead, instead of `NOT IN ()` — which some
	// callers might expect to behave like "matches nothing" — silently
	// matching everything because SQL's `x <> ALL('{}')` and `x NOT IN ()`
	// agree, but a naive `NOT (x = ANY('{}'))` needs the same care. Nil means
	// the gate does not apply to this query (caller is exempt, or the
	// registry is empty).
	if filter.TaintGrantIDs != nil {
		p := w.nextArg()
		w.clauses = append(w.clauses,
			fmt.Sprintf("NOT EXISTS (SELECT 1 FROM bead_taints bt JOIN taints t ON t.id = bt.taint_id WHERE bt.bead_id = beads.id AND 'gate' = ANY(t.effects) AND bt.taint_id <> ALL(%s::bigint[]))", p))
		w.args = append(w.args, pq.Array(*filter.TaintGrantIDs))
	}

	// TaintID: the positive "carries this taint" filter behind the taints
	// registry's per-taint bead count (docs/adr/taints.md §9.1). Composes
	// via AND with TaintGrantIDs above — a caller must still pass the gate
	// on top of matching this taint.
	if filter.TaintID != nil {
		p := w.nextArg()
		w.clauses = append(w.clauses,
			fmt.Sprintf("EXISTS (SELECT 1 FROM bead_taints bt WHERE bt.bead_id = beads.id AND bt.taint_id = %s)", p))
		w.args = append(w.args, *filter.TaintID)
	}
}

// addTimeFilters: updated, created and closed windows.
func (w *beadListWhere) addTimeFilters(filter model.BeadFilter) {
	if filter.UpdatedAfter != nil {
		w.clauses = append(w.clauses, "beads.updated_at > "+w.nextArg())
		w.args = append(w.args, *filter.UpdatedAfter)
	}

	if filter.CreatedAfter != nil {
		w.clauses = append(w.clauses, "beads.created_at >= "+w.nextArg())
		w.args = append(w.args, *filter.CreatedAfter)
	}
	if filter.CreatedBefore != nil {
		w.clauses = append(w.clauses, "beads.created_at <= "+w.nextArg())
		w.args = append(w.args, *filter.CreatedBefore)
	}

	// closed_at is nullable: `closed_at > $n` is NULL-safe false, so
	// never-closed beads are excluded by either bound — correct for
	// "closed in window" queries.
	if filter.ClosedAfter != nil {
		w.clauses = append(w.clauses, "beads.closed_at > "+w.nextArg())
		w.args = append(w.args, *filter.ClosedAfter)
	}
	if filter.ClosedBefore != nil {
		w.clauses = append(w.clauses, "beads.closed_at < "+w.nextArg())
		w.args = append(w.args, *filter.ClosedBefore)
	}
}

// addGraphFilters: parent, subtree and mention edges.
func (w *beadListWhere) addGraphFilters(filter model.BeadFilter) {
	if filter.ParentID != "" {
		p := w.nextArg()
		w.clauses = append(w.clauses,
			fmt.Sprintf("EXISTS (SELECT 1 FROM deps WHERE deps.bead_id = beads.id AND deps.depends_on_id = %s AND deps.type IN (%s))", p, parentChildTypeInList()))
		w.args = append(w.args, filter.ParentID)
	}

	// ParentSubtreeID: transitive descendants via parent-child dep edges. The
	// recursive CTE walks down from <root>, following deps.depends_on_id ←
	// deps.bead_id (children point at parent), and excludes the root itself.
	//
	// Cycle safety (ka-0000006899): the UNION already dedupes, so a
	// parent-child cycle can never make the walk diverge. But a cycle that
	// passes through the root (e.g. an epic listing one of its own
	// descendants as its parent) re-admits the root as a descendant of
	// itself, so a subtree-drain gardener's `bead_query > 0` dormancy
	// preflight counts the still-open root and never reaches 0. The
	// dep-add cycle guard prevents NEW such edges; the explicit
	// `id <> <root>` filter here makes the count correct for data that was
	// already corrupted before the guard landed.
	if filter.ParentSubtreeID != "" {
		p := w.nextArg()
		w.clauses = append(w.clauses,
			fmt.Sprintf(`beads.id IN (
				WITH RECURSIVE subtree(id) AS (
					SELECT bead_id FROM deps WHERE depends_on_id = %[1]s AND type IN (%[2]s)
					UNION
					SELECT d.bead_id FROM deps d JOIN subtree s ON d.depends_on_id = s.id AND d.type IN (%[2]s)
				)
				SELECT id FROM subtree WHERE id <> %[1]s
			)`, p, parentChildTypeInList()))
		w.args = append(w.args, filter.ParentSubtreeID)
	}
	// ParentSubtreeIDs: the same walk seeded from several roots. Roots are not
	// excluded, so a plan nested in another plan stays in the outer one's
	// subtree; the caller drops each root from its own closure.
	if len(filter.ParentSubtreeIDs) > 0 {
		p := w.nextArg()
		w.clauses = append(w.clauses,
			fmt.Sprintf(`beads.id IN (
				WITH RECURSIVE subtree(id) AS (
					SELECT bead_id FROM deps WHERE depends_on_id = ANY(%[1]s::text[]) AND type IN (%[2]s)
					UNION
					SELECT d.bead_id FROM deps d JOIN subtree s ON d.depends_on_id = s.id AND d.type IN (%[2]s)
				)
				SELECT id FROM subtree
			)`, p, parentChildTypeInList()))
		w.args = append(w.args, pq.Array(filter.ParentSubtreeIDs))
	}

	if filter.Mentions != "" {
		p := w.nextArg()
		w.args = append(w.args, filter.Mentions)
		// MentionsType narrows by bead_references.relationship_type:
		//   - nil              → no filter (any typed or untyped row matches)
		//   - non-nil ""       → only untyped (relationship_type IS NULL)
		//   - non-nil "<verb>" → exact match
		// Mirrors relationshipTypeClause in queries_references.go.
		typeClause := ""
		if filter.MentionsType != nil {
			if *filter.MentionsType == "" {
				typeClause = " AND bead_references.relationship_type IS NULL"
			} else {
				tp := w.nextArg()
				w.args = append(w.args, *filter.MentionsType)
				typeClause = fmt.Sprintf(" AND bead_references.relationship_type = %s", tp)
			}
		}
		w.clauses = append(w.clauses,
			fmt.Sprintf("EXISTS (SELECT 1 FROM bead_references WHERE bead_references.source_bead_id = beads.id AND bead_references.target_bead_id = %s%s)", p, typeClause))
	}
}

// addSearchFilter adds the text-search predicate.
//
// ka-0000004745 (Option 1): tokenize the query on whitespace and AND each
// term as a SEPARATE (title ILIKE %term% OR description ILIKE %term%)
// clause, so word ORDER no longer matters. The previous single-ILIKE over
// the whole joined query required the exact contiguous phrase, so
// `litellm ProxyException` matched nothing even when both words were
// present out of order (ka-0000000507). Each term is a bound parameter —
// never string-concat user input. A single-term query collapses to
// exactly one clause, so the empty/single-term behavior is preserved.
//
// ka-0000003832 (Option 2): the per-term ILIKE group is kept as the recall
// FLOOR and OR'd with a Postgres FTS predicate
// (beads.search_tsv @@ websearch_to_tsquery('english', <query>)). The ILIKE
// branch matches regardless of whether search_tsv has been backfilled yet
// (the column is filled lazily — see migration 039 + `kd admin
// backfill-search-tsv`), so recall is a strict SUPERSET of Option 1 and is
// backfill-independent. The FTS branch lets the GIN index
// (idx_beads_search_tsv) accelerate matches and, more importantly, feeds a
// graded ts_rank_cd component into the relevance score below.
func (w *beadListWhere) addSearchFilter(filter model.BeadFilter) {
	terms := strings.Fields(filter.Search)
	if len(terms) == 0 {
		return
	}
	termPlaceholders, ilikeGroup := w.addSearchTerms(terms)

	// The full query is bound once and reused for BOTH the FTS match
	// predicate (WHERE) and the FTS-ranked relevance score (ORDER BY /
	// SELECT). It is appended here in the WHERE phase so it lands before
	// any LIMIT/OFFSET placeholder. The CountOnly fast path emits a bare
	// COUNT(*) with NO ORDER BY and NO score projection, so it neither
	// needs the FTS rank nor can afford an unreferenced bind (a Postgres
	// "bind message supplies N parameters" error) — but it DOES still get
	// the FTS match predicate so the count matches the ranked result set.
	fq := w.nextArg()
	w.args = append(w.args, filter.Search)

	// FTS match predicate. websearch_to_tsquery never errors on arbitrary
	// user input (unlike to_tsquery / plainto_tsquery edge cases), so it is
	// safe to feed the raw query. NULL search_tsv (un-backfilled legacy
	// rows) yields NULL @@ … = NULL, which is treated as non-matching — the
	// ILIKE branch still covers those rows.
	ftsMatch := fmt.Sprintf("beads.search_tsv @@ websearch_to_tsquery('english', %s)", fq)
	w.clauses = append(w.clauses,
		"(("+strings.Join(ilikeGroup, " AND ")+") OR "+ftsMatch+")")

	if !filter.CountOnly {
		w.searchRelevanceExpr = searchRelevanceSQL(fq, termPlaceholders)
	}
}

// addSearchTerms binds each search term once and returns the placeholders
// and the per-term recall clauses, in term order.
func (w *beadListWhere) addSearchTerms(terms []string) (termPlaceholders, ilikeGroup []string) {
	termPlaceholders = make([]string, len(terms))
	ilikeGroup = make([]string, len(terms))
	for i, term := range terms {
		p := w.nextArg()
		termPlaceholders[i] = p
		// ka-0000008243: extend the per-term recall floor to cover COMMENT
		// bodies. The FTS vector (beads.search_tsv) and the title/description
		// ILIKE branch both omit comments entirely — comments live in their
		// own table and the search_tsv trigger only fires on title/description/
		// fields — so a reference that appears ONLY in a comment (common: an
		// agent adds evidence to an open decision as a comment rather than
		// editing fields.context) was invisible to `kd search`. This burned the
		// §5.0 pre-mutation gate's leg 2, which reads clean on a bead a decision
		// names only in a comment. The comment branch mirrors the existing
		// ILIKE floor (backfill-independent) and reuses the SAME bound,
		// separator-expanded term. Kept inside the per-term OR so the AND-across-
		// terms semantics are preserved: each term must appear in the title,
		// description, OR some comment on the bead. Recall is a strict SUPERSET
		// of the prior predicate.
		//
		// ka-0000003620 (Phase 2, design ka-0000004060): the comment branch is
		// an UNCORRELATED `beads.id = ANY(ARRAY(SELECT bead_id FROM comments
		// WHERE text ILIKE ...))` rather than a correlated `EXISTS (... WHERE
		// comments.bead_id = beads.id ...)`. This is the load-bearing rewrite
		// that lets the whole disjunction planner-collapse to a BitmapOr of
		// index scans instead of a full seq scan of the 695k-row beads table.
		// With the correlated EXISTS the planner cannot fold the comment
		// disjunct into the beads BitmapOr and seq-scans beads even when the
		// pg_trgm indexes (migration 044) exist; the uncorrelated ARRAY makes
		// the disjunct `id = ANY($array)`, which the planner serves via
		// beads_pkey as one arm of the BitmapOr alongside idx_beads_title_trgm,
		// idx_beads_description_trgm and idx_beads_search_tsv. The subquery
		// itself is served by idx_comments_text_trgm. Recall is IDENTICAL to
		// the EXISTS form — `id = ANY(ARRAY(subq))` and `EXISTS (correlated
		// subq)` select the same rows (verified: 0 rows diverge either
		// direction on a 200k-bead corpus). Measured 5,078,783-cost Seq Scan
		// (425 ms) -> 437-cost BitmapOr (16 ms) on a 3-term query. Do NOT
		// revert to correlated EXISTS without re-checking the plan — it
		// silently reintroduces the seq scan and the 500/false-page pathology
		// (ka-0000003621, ka-0000006261).
		ilikeGroup[i] = fmt.Sprintf("(title ILIKE '%%' || %s || '%%' OR description ILIKE '%%' || %s || '%%' OR beads.id = ANY(ARRAY(SELECT bead_id FROM comments WHERE comments.text ILIKE '%%' || %s || '%%')))", p, p, p)
		// Bind the SEPARATOR-EXPANDED term (break-glass -> break%glass) so
		// hyphen/underscore/slash differences between the query and the bead
		// no longer zero out recall (ka-0000002772). Still a bound param.
		w.args = append(w.args, expandSearchTerm(term))
	}
	return termPlaceholders, ilikeGroup
}

// searchRelevanceSQL is the relevance score for a text search over the
// bound full query fq and the per-term placeholders.
//
// Relevance score = Option-1 title-tier CASE (exact-title-prefix 3,
// title-substring 2, all-terms-in-title 1, else 0) PLUS a graded FTS
// component PLUS an Option-3 openness/priority/type WITHIN-TIER
// boost. The CASE integer tiers dominate so every Option-1 ordering
// is preserved exactly; the FTS ts_rank_cd refines WITHIN a tier (the
// Option-2 value-add) and rescues description-only FTS hits from tier
// 0. ts_rank_cd's normalization flag 32 (rank/(rank+1)) bounds the FTS
// component to [0,1), so the FTS component ALONE can never jump a row
// across an integer tier boundary (the separate Option-3 boost below
// can add up to 0.5 more — see its tier-math note). COALESCE pins
// un-backfilled (NULL search_tsv) rows to 0 so ranking degrades
// gracefully to Option-1 behavior during the backfill window.
//
// Option-3 boost (ka-0000004282, design ka-0000000863 §2/§4): open
// escalation/decision beads are precisely the ones an agent must
// never miss, yet a purely-lexical score lets a closed title-match
// bead outrank an open P0 escalation with a description-only hit. The
// trailing CASE adds a status/type/priority weight (max 0.5) so an
// OPEN operational bead outranks a same-lexical-tier CLOSED bead.
//
// Ephemeral down-rank (ka-0000000838): the SAME CASE opens with a
// -0.5 penalty for ephemeral type=message / type=agent gardener-
// lifecycle beads. Their Gardener-* titles carry a strong lexical
// match for gardener/plan/escalation-flavored dedup queries, so a
// closed message bead lands in the same title tier as (and buries)
// the open issue-like bead an agent is actually hunting. Making it
// the FIRST branch means ephemeral beads take the -0.5 penalty and
// never ALSO collect the openness boost below (CASE stops at the
// first match), so an open message bead nets -0.5, not +0.1 — folding
// the "ephemeral never gets the openness boost" rule into the same
// expression. The penalty is additive (recall unchanged; `-t message`
// opt-in still surfaces them) and a WITHIN-tier tiebreaker: at -0.5 it
// reliably sinks an ephemeral bead below a same-tier issue bead across
// the common ts_rank spread, while staying under one tier width so it
// cannot invert the integer title tiers wholesale.
//
// The boost is a WITHIN-tier tiebreaker, NOT a tier override. Max
// boost 0.5 + ts_rank_cd < 1.0 caps the whole FTS+boost tail below
// 1.5, so a tier-0 (description-only) match tops out below the tier-2
// floor of 2.0 — it can NEVER be lifted into a title-substring (tier
// 2) or exact-title-prefix (tier 3) match. The ephemeral -0.5 only
// SUBTRACTS, so it cannot lift a tier-0 bead over that 1.5 cap either;
// the Symptom-B guarantee is preserved by construction: a CLOSED
// title(-substring)-match bug still outranks a tier-0 open escalation
// (pinned by the Option-3 integration test). Lifting an open P0
// escalation ABOVE a closed title-match would need boost >= 1.0 + a
// tier redesign + a Symptom-B test change, filed as a follow-up
// decision rather than done silently here. (The one band the tail can
// reorder against is the marginal tier-1 "all query terms present in
// the title non-contiguously", multi-term only, when that bead's
// ts_rank is near 0 — an accepted edge, not the closed title-substring
// case the design protects.) Priority is 0-critical..4-backlog, so P0
// = priority<=0. Reads only status/type/priority (indexed since
// 001_initial.up.sql), independent of search_tsv, so it composes
// cleanly with the FTS field-indexing work (ka-0000005839). Lives only
// inside the !CountOnly branch — the count path emits no ORDER BY /
// score and so gains no unreferenced bind.
func searchRelevanceSQL(fq string, termPlaceholders []string) string {
	titleHasAllTerms := make([]string, len(termPlaceholders))
	for i, p := range termPlaceholders {
		titleHasAllTerms[i] = fmt.Sprintf("title ILIKE '%%' || %s || '%%'", p)
	}
	return fmt.Sprintf(
		"(CASE WHEN title ILIKE %[1]s || '%%' THEN 3"+
			" WHEN title ILIKE '%%' || %[1]s || '%%' THEN 2"+
			" WHEN %[2]s THEN 1"+
			" ELSE 0 END)::float8"+
			" + COALESCE(ts_rank_cd(beads.search_tsv, websearch_to_tsquery('english', %[1]s), 32), 0)"+
			" + (CASE WHEN type IN ('message','agent') THEN -0.5"+
			" WHEN status IN ('open','in_progress') AND type IN ('escalation','decision') THEN 0.5"+
			" WHEN status IN ('open','in_progress') AND priority <= 0 THEN 0.3"+
			" WHEN status IN ('open','in_progress') THEN 0.1"+
			" ELSE 0 END)",
		fq, strings.Join(titleHasAllTerms, " AND "))
}

// addFieldFilters: one predicate per fields key.
func (w *beadListWhere) addFieldFilters(filter model.BeadFilter) {
	for key, val := range filter.Fields {
		kp := w.nextArg()
		switch val {
		case "":
			// Empty value: match beads where the field is absent or empty.
			w.clauses = append(w.clauses, fmt.Sprintf("(fields->>%s IS NULL OR fields->>%s = '')", kp, kp))
			w.args = append(w.args, key)
		case "*":
			// Presence sentinel (`--field key=*`): match beads where the field
			// is present and non-empty, regardless of value.
			//
			// The leading `fields ? key` guard is required for the SAME reason
			// the default branch below carries it: the partial expression
			// indexes on hot fields keys (migration 025 agent/agent_state/
			// project/role/slack_user_id, 031 schedule_id, 034 trigger_id) all
			// carry a `WHERE fields ? '<key>'` predicate, and Postgres does not
			// reason about JSONB operator implication — it cannot derive that
			// index predicate from `fields->>key IS NOT NULL` alone, so a bare
			// presence test degrades to a full Seq Scan on the ~577k-row table
			// (bug ka-0000001686; same failure mode as the 504 in ka-0000002374).
			// The guard never changes the result set: `fields ? key` is implied
			// wherever `fields->>key != ''` holds, and a JSON null value (which
			// satisfies `fields ? key` but whose `->>` is SQL NULL) is still
			// excluded because `NULL != ''` is unknown/false — so the explicit
			// non-empty term is retained.
			w.clauses = append(w.clauses, fmt.Sprintf("(fields ? %s AND fields->>%s != '')", kp, kp))
			w.args = append(w.args, key)
		default:
			vp := w.nextArg()
			// The leading `fields ? key` presence guard is what lets the planner
			// reach the PARTIAL expression indexes on hot fields keys (migration
			// 025 agent/agent_state/project/role/slack_user_id, 031 schedule_id,
			// 034 trigger_id). Those indexes carry a `WHERE fields ? '<key>'`
			// predicate, so a bare `fields->>key = $1` cannot be proven to fall
			// within the index and degrades to a full Seq Scan — e.g. the
			// /beads/gardeners run-count enrich (gardeners_enrich.go) scanned all
			// ~35k agent beads per gardener (≈600ms each), blowing the gateway
			// timeout into a 504 (bug ka-0000002374). With the guard the same
			// lookup is a sub-millisecond Index Scan. The guard never changes the
			// result set — `fields->>key = val` already implies the key is present
			// — so for non-indexed keys it is just a cheap, harmless extra check.
			w.clauses = append(w.clauses, fmt.Sprintf("fields ? %s AND fields->>%s = %s", kp, kp, vp))
			w.args = append(w.args, key, val)
		}
	}
}

// addFieldInFilters: one IN predicate per FieldsIn key, guarded by `fields ?
// key` like addFieldFilters' equality, so a key with a partial expression
// index (055 channel_id) is read through it.
func (w *beadListWhere) addFieldInFilters(filter model.BeadFilter) {
	keys := slices.Sorted(maps.Keys(filter.FieldsIn))
	for _, key := range keys {
		vals := filter.FieldsIn[key]
		if len(vals) == 0 {
			continue
		}
		kp := w.nextArg()
		w.args = append(w.args, key)
		addIn(w, fmt.Sprintf("fields ? %s AND fields->>%s IN", kp, kp), vals)
	}
}

// addQueueFilters: fleet, blocker, orphan, short-title and heraldry.
func (w *beadListWhere) addQueueFilters(filter model.BeadFilter) {
	// Apply fleet scoping unless the caller is exclusively listing cross-fleet
	// types (project, account) — those describe system-level configuration
	// and must be visible to fleet-scoped service callers (e.g. bridges) so
	// they can resolve project metadata regardless of which fleet they
	// authenticate under. See model.AllTypesCrossFleet.
	if filter.Fleet != "" && filter.Fleet != model.FleetAll && !model.AllTypesCrossFleet(filter.Type) {
		w.clauses = append(w.clauses, "fleet = "+w.nextArg())
		w.args = append(w.args, filter.Fleet)
	}

	if filter.NoOpenDeps {
		// Only treat genuinely-blocking dep types as blockers (see blockingDepTypes).
		// Structural/relational types (parent-child, parent-of, child-of, related,
		// duplicates, supersedes, …) link beads but do NOT mean the source bead is
		// waiting on the target. Without this filter, a child task of an open epic
		// appears blocked by its own epic and never surfaces in `gb ready` /
		// `kd list --no-blockers` (ka-0000000173, ka-0000004488).
		// A dep in 'blocked' status is NOT terminal-for-progression
		// (model.Status.IsTerminalForProgression is closed||partial only), so
		// it must keep its dependent OUT of the ready queue — exactly like open
		// / in_progress / deferred. Omitting 'blocked' here made the ready-queue
		// disagree with two positive terminal checks that DO treat blocked as
		// still-blocking: queryGetBlockedByForBeads (batch_queries.go, the
		// blocked-by badge source) and the formula step dispatcher
		// (formula_step_dispatch.go stepDispatchBlockerCleared). The divergence
		// only under-spawned (a step on a blocked upstream showed as ready in
		// `gb ready` but the dispatcher would not fire), so it was safe but the
		// "kept in lockstep with the ready-queue" contract was untrue. Adding
		// 'blocked' aligns all three (ka-0000006285).
		// The status set comes from openBlockerStatusSQL so this predicate and
		// the --with-blockers enrichment CANNOT drift: --with-blockers exists
		// to explain exactly the rows this clause hides, and two hand-copied
		// literals would let one grow a status the other lacks.
		w.clauses = append(w.clauses,
			"NOT EXISTS (SELECT 1 FROM deps d JOIN beads dep ON d.depends_on_id = dep.id "+
				"WHERE d.bead_id = beads.id "+
				"AND d.type IN ("+blockingDepTypeInList()+") "+
				"AND dep.status IN ("+openBlockerStatusSQL+"))")
	}

	if filter.NoParent {
		// Orphan predicate: only beads with NO parent-child parent. The canonical
		// parent-child row is written CHILD-first (deps.bead_id=child,
		// deps.depends_on_id=parent) — see cmd/kd/dep.go and the ParentID clause
		// above, which uses the same direction. So "this bead has a parent" is
		// EXISTS(deps WHERE bead_id=beads.id AND type IN (parent-child aliases));
		// the orphan filter negates it. The legacy "parent" alias is included
		// (model.ParentChildDepTypes, via parentChildTypeInList) so children of
		// pre-canonicalization epics are NOT mis-reported as orphans — the
		// concrete false-positive in ka-0000008107.
		// Lets the triage-intake sweep ask for parent-less open work in one query
		// instead of an N+1 `kd dep list` loop (ka-0000005267).
		w.clauses = append(w.clauses,
			"NOT EXISTS (SELECT 1 FROM deps d "+
				"WHERE d.bead_id = beads.id "+
				"AND d.type IN ("+parentChildTypeInList()+"))")
	}

	if filter.ShortTitleMissing {
		// The short-title groomer's queue (plan 2026-10-14-short-titles D5).
		w.clauses = append(w.clauses, "(beads.short_title IS NULL OR beads.short_title = '')")
	}

	if filter.Heraldry != "" {
		// The run, or a value still in the shield form ("blue 🔥 . 🧭 . 📦":
		// a colour word, then the places with "." for an empty one) whose
		// places read as that run. No emoji holds a space or a dot.
		run := w.nextArg()
		w.clauses = append(w.clauses, "(beads.heraldry = "+run+
			" OR (beads.heraldry LIKE '% %' AND replace(replace(substr(beads.heraldry, strpos(beads.heraldry, ' ') + 1), ' ', ''), '.', '') = "+run+"))")
		w.args = append(w.args, filter.Heraldry)
	}
	if filter.HeraldryRegister != "" {
		// The register (migration 057): served by idx_beads_heraldry_register.
		w.clauses = append(w.clauses, "beads.heraldry IS NOT NULL AND beads.project = "+w.nextArg())
		w.args = append(w.args, filter.HeraldryRegister)
	}
}

// addStateFilters: soft-delete, scheduled, report and decision state.
func (w *beadListWhere) addStateFilters(filter model.BeadFilter) {
	// Soft-delete filter (ka-0000004651 step 8b). By default, listings exclude
	// beads where deleted_at IS NOT NULL. OnlyDeleted inverts the predicate;
	// IncludeDeleted drops it entirely so admins can see live + deleted rows.
	switch {
	case filter.OnlyDeleted:
		w.clauses = append(w.clauses, "deleted_at IS NOT NULL")
	case filter.IncludeDeleted:
		// no clause — include both live and deleted rows
	default:
		w.clauses = append(w.clauses, "deleted_at IS NULL")
	}

	// ExcludeScheduled (ka-0000006652): hide beads spawned by the cron
	// scheduler. The scheduler stamps schedule_id into the bead's JSONB
	// fields when it instantiates a task each tick.
	//
	// ka-0000007340 / ka-0000007651: the legacy schedule_id predicate alone is
	// not enough. The trigger/multi scheduler emits per-run *firing* beads that
	// are plain kind=issue tasks (NOT type=schedule, and they carry NO
	// schedule_id), so they slipped past ExcludeScheduled and flooded
	// `gb ready` (194/200 rows at peak). Each firing bead carries a distinctive
	// JSONB signature stamped by the scheduler:
	//   - multi firings (label source:multi):       fields.multi_fire_key present
	//     (titled "Multi[cron|bead_event|event|decision_resolved]: <gardener>")
	//   - schedule firings (label source:schedule): fields.schedule_fire_key present
	// They re-spawn every cron cycle / on every matched bead event and never
	// need a human claim, so they are noise in ready/active views — drop them
	// alongside schedule_id beads.
	//
	// ka-0000007651: the original fix keyed off multi_entry_kind='cron', which
	// missed the event-driven variant (multi_entry_kind='bead_event'/'event')
	// — 122 of those flooded `gb ready`. multi_fire_key is the kind-agnostic
	// firing-record marker (the same field stalereaper / feed use to identify
	// fire records), so filter on its presence to drop every firing shape
	// uniformly regardless of multi_entry_kind.
	if filter.ExcludeScheduled {
		w.clauses = append(w.clauses,
			"(fields->>'schedule_id' IS NULL OR fields->>'schedule_id' = '')",
			"(fields->>'multi_fire_key' IS NULL OR fields->>'multi_fire_key' = '')",
			"(fields->>'schedule_fire_key' IS NULL OR fields->>'schedule_fire_key' = '')")
	}

	// HasReport (R1, ka-0000007923): filter to beads whose has_report
	// projection matches the requested bool. The same EXISTS predicate that
	// powers the SELECT projection is reused here so filter and per-row
	// answer can never disagree.
	if filter.HasReport != nil {
		predicate := `EXISTS (
			SELECT 1 FROM deps d JOIN beads c ON c.id = d.bead_id
			WHERE d.depends_on_id = beads.id
			  AND d.type = 'parent-child'
			  AND c.type IN ('report', 'rich-report', 'bundle')
			  AND c.deleted_at IS NULL
		)`
		if *filter.HasReport {
			w.clauses = append(w.clauses, predicate)
		} else {
			w.clauses = append(w.clauses, "NOT "+predicate)
		}
	}

	// BlockedOnDecision (ka-0000001065): filter on the daemon-computed
	// predicate. The expression is the same scalar subquery used in the
	// SELECT projection — non-empty string means "blocked by an open decision".
	if filter.BlockedOnDecision != nil {
		if *filter.BlockedOnDecision {
			w.clauses = append(w.clauses, "("+blockedOnDecisionSelectExpr+") <> ''")
		} else {
			w.clauses = append(w.clauses, "("+blockedOnDecisionSelectExpr+") = ''")
		}
	}

	// ParkedOnDecision (ka-0000003797): text-mention sibling of the above.
	// Non-empty means an open decision names this bead in its prose.
	if filter.ParkedOnDecision != nil {
		if *filter.ParkedOnDecision {
			w.clauses = append(w.clauses, "("+parkedOnDecisionSelectExpr+") <> ''")
		} else {
			w.clauses = append(w.clauses, "("+parkedOnDecisionSelectExpr+") = ''")
		}
	}
}

// countListedBeads runs the bare COUNT(*) over a list's WHERE clause.
func countListedBeads(ctx context.Context, db executor, whereSQL string, whereArgs []any) (int, error) {
	countQuery := "SELECT COUNT(*) FROM beads" + whereSQL
	var total int
	if err := db.QueryRowContext(ctx, countQuery, whereArgs...).Scan(&total); err != nil {
		return 0, fmt.Errorf("count beads: %w", err)
	}
	return total, nil
}

// beadListProjections returns the opt-in trailing columns of a list row, in
// the order scanBeadWithTotalFull reads them.
//
// has_report and blocked_on_decision_bead_id are opt-in projections
// (model.BeadFilter.WithHasReport / WithBlockedOnDecision). When the
// caller has set the matching filter we have to evaluate the predicate
// anyway, so we surface it for free; otherwise we emit a constant
// placeholder column so scanBeadWithTotalFull's column layout stays
// stable. The default-off path matters: the blocked_on_decision
// projection is a per-row MIN(d.id) over open decisions plus a two-level
// deps walk, and at gardenio-e2e scale was the dominant /v1/beads cost
// (15.9M shared buffer hits per 50-row page) before being gated.
func beadListProjections(filter model.BeadFilter, searchRelevanceExpr string) string {
	hasReportProj := ", FALSE AS has_report"
	if filter.WithHasReport || filter.HasReport != nil {
		hasReportProj = beadHasReportSelect
	}
	blockedOnDecisionProj := ", '' AS blocked_on_decision_bead_id"
	if filter.WithBlockedOnDecision || filter.BlockedOnDecision != nil {
		blockedOnDecisionProj = beadBlockedOnDecisionSelectSuffix
	}

	// parked_on_decision_bead_id: opt-in text-mention projection
	// (ka-0000003797), same default-off discipline as blocked_on_decision —
	// the EXISTS subquery over bead_references is index-backed but not free, and
	// only the ready/news queue surfaces read the column. When neither the
	// projection nor the filter is requested we emit a constant placeholder so
	// scanBeadWithTotalFull's column layout stays stable.
	parkedOnDecisionProj := ", '' AS parked_on_decision_bead_id"
	if filter.WithParkedOnDecision || filter.ParkedOnDecision != nil {
		parkedOnDecisionProj = beadParkedOnDecisionSelectSuffix
	}

	// relevance_score projection (ka-0000003832 / Option 2; ka-0000004282 /
	// Option 3). When a text search is active, surface the same composite score
	// that drives ORDER BY (Option-1 title-tier CASE + FTS ts_rank_cd +
	// Option-3 openness/priority/type boost) as a trailing float column so
	// the CLI / JSON can expose it. When there is no search the column is a
	// constant 0 so scanBeadWithTotalFull's column layout stays stable on every
	// list path. searchRelevanceExpr already references only bound args + the
	// search_tsv column, so it is safe to reuse verbatim in the SELECT.
	relevanceProj := ", 0::float8 AS relevance_score"
	if searchRelevanceExpr != "" {
		relevanceProj = ", (" + searchRelevanceExpr + ") AS relevance_score"
	}
	return hasReportProj + blockedOnDecisionProj + parkedOnDecisionProj + relevanceProj
}

// buildBeadListDataQuery builds the page statement and its args: the WHERE
// args plus any LIMIT and OFFSET binds.
//
// Single query with COUNT(*) OVER() to get total and rows atomically.
// JOIN operators on operated_by_id so the resolved chip (display + email)
// surfaces alongside every bead row. Filter and sort columns that exist on
// both sides of the join (id, kind, created_at, updated_at) are explicitly
// qualified above and inside parseSortClause.
//
// Perf: COUNT(*) OVER() makes postgres project and window over the WHOLE
// filtered set before ORDER BY ... LIMIT, so even limit=1 costs O(matching
// rows) of wide-row work (status=open ~6.5k rows 0.23s, unfiltered ~20k
// rows 0.9s — gd-g25jihMHFs). Without it the planner can top-N / index-walk
// the page. The total is then derived cheaply: a short page already tells
// us the exact total (offset + rows); only a full page needs a narrow
// SELECT COUNT(*) over the same WHERE. Text search keeps the window: its
// relevance binds are referenced only by the data query, so the separate
// count statement could not reuse args, and search sets are small.
func buildBeadListDataQuery(filter model.BeadFilter, w *beadListWhere, whereSQL string) (string, []any) {
	totalProj := "0 AS total_count, "
	if w.searchRelevanceExpr != "" {
		totalProj = "COUNT(*) OVER() AS total_count, "
	}
	args := slices.Clone(w.args)
	dataQuery := "SELECT " + totalProj + beadListColumns(filter) + beadOperatorSelectSuffix + beadListProjections(filter, w.searchRelevanceExpr) +
		" FROM beads" + beadOperatorJoin + whereSQL + " ORDER BY " + parseSortClause(filter.Sort, w.searchRelevanceExpr)

	if filter.Limit > 0 {
		dataQuery += " LIMIT " + w.nextArg()
		args = append(args, filter.Limit)
	}
	if filter.Offset > 0 {
		dataQuery += " OFFSET " + w.nextArg()
		args = append(args, filter.Offset)
	}
	return dataQuery, args
}

// queryListedBeadRows runs the page statement and scans its rows. The int is
// the last row's total_count column (0 unless the query used the window).
func queryListedBeadRows(ctx context.Context, db executor, dataQuery string, args []any) ([]*model.Bead, int, error) {
	rows, err := db.QueryContext(ctx, dataQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list beads: %w", err)
	}
	defer rows.Close()

	var beads []*model.Bead
	var total int
	for rows.Next() {
		b, t, err := scanBeadWithTotalFull(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan beads: %w", err)
		}
		total = t
		beads = append(beads, b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("scan beads: %w", err)
	}
	return beads, total, nil
}

// resolveListTotal derives the total for a list that skipped the window
// count, from the page it got (n rows) and, only when that cannot tell, a
// narrow COUNT(*) over the same WHERE.
func resolveListTotal(ctx context.Context, db executor, filter model.BeadFilter, n int, whereSQL string, whereArgs []any) (int, error) {
	// A short page (fewer rows than Limit, or no Limit at all) is the end
	// of the set, so its total is exact without a count. An empty page past
	// offset 0 is ambiguous (offset beyond the end vs. more rows exist), so
	// it, like a full page, takes the narrow count.
	short := n > 0 && (filter.Limit <= 0 || n < filter.Limit)
	switch {
	case short:
		return filter.Offset + n, nil
	case n == 0 && filter.Offset == 0:
		return 0, nil
	case filter.NoTotal:
		return -1, nil
	default:
		return countListedBeads(ctx, db, whereSQL, whereArgs)
	}
}

// enrichListedBeads fills a page's labels and, when asked, its open blockers
// and parents.
//
// Populate labels for all returned beads via queryGetLabelsForBeads,
// which uses bead_id = ANY($1::text[]) — a single parameter regardless
// of result size. The previous inline IN ($1,$2,...,$N) form overflowed
// pq's uint16 parameter count (max 65535) once the beads table grew past
// that threshold, turning unbounded list calls into 500s (ka-0000002155).
func enrichListedBeads(ctx context.Context, db executor, filter model.BeadFilter, beads []*model.Bead) error {
	if len(beads) == 0 {
		return nil
	}
	ids := make([]string, len(beads))
	for i, b := range beads {
		ids[i] = b.ID
	}
	labelMap, err := queryGetLabelsForBeads(ctx, db, ids)
	if err != nil {
		return fmt.Errorf("list beads labels: %w", err)
	}
	for _, b := range beads {
		b.Labels = labelMap[b.ID] // nil if no labels (same as before for unlabelled beads)
	}
	if filter.WithOpenBlockers {
		if err := attachOpenBlockers(ctx, db, filter, beads, ids); err != nil {
			return err
		}
	}
	if filter.WithParent {
		if err := attachParents(ctx, db, filter, beads, ids); err != nil {
			return err
		}
	}
	return nil
}

// attachOpenBlockers sets OpenBlockers on every bead of the page.
//
// open_blockers is an opt-in enrichment (WithOpenBlockers /
// `kd list --with-blockers`, ka-0000000945): one extra deps⋈beads
// batch query keyed on THIS PAGE's ids, so its cost scales with the
// page, not the matching set. Off by default because most ListBeads
// callers never read blockers. It hard-fails rather than degrading to
// silent nil: a caller that asked "what is blocking these?" and got an
// empty answer would read it as "nothing is blocked" — the exact
// false-all-clear this feature exists to prevent.
func attachOpenBlockers(ctx context.Context, db executor, filter model.BeadFilter, beads []*model.Bead, ids []string) error {
	// Carry the caller's project scoping into the join: the blocker
	// may live outside the result set, and its title must not escape
	// a project-bound token / OIDC-restricted user.
	blockerMap, err := queryGetOpenBlockersForBeads(ctx, db, ids, blockerReadScope{
		RequireAllLabels: filter.Labels,
		RequireAnyLabels: append(slices.Clone(filter.LabelsAny), filter.LabelsOrNoProject...),
	})
	if err != nil {
		return fmt.Errorf("list beads open blockers: %w", err)
	}
	for _, b := range beads {
		// ALWAYS a non-nil pointer, even when nothing blocks the bead:
		// that is what makes "&[] = I checked, it's clear" distinct
		// from "nil = nobody checked" on the wire, so a client can
		// detect a daemon too old to honor the param instead of
		// reading its silence as an all-clear (see model.Bead).
		blockers := blockerMap[b.ID]
		if blockers == nil {
			blockers = []*model.OpenBlocker{}
		}
		b.OpenBlockers = &blockers
	}
	return nil
}

// attachParents sets ParentID and ParentTitle on the beads that have one.
//
// parent_id / parent_title is an opt-in enrichment (WithParent /
// `gb ready`, ka-0000006083): one extra batched deps⋈beads query keyed on
// THIS PAGE's ids, so its cost scales with the page, not the matching set
// — no N+1 `kd show`. Off by default; the vast majority of ListBeads
// callers never read the parent. Unlike open_blockers, an absent parent is
// a plain empty string (a genuine orphan and "not asked" both read as ""),
// so there is no nil/&[] wire distinction to preserve here.
func attachParents(ctx context.Context, db executor, filter model.BeadFilter, beads []*model.Bead, ids []string) error {
	parentMap, err := queryGetParentsForBeads(ctx, db, ids, blockerReadScope{
		RequireAllLabels: filter.Labels,
		RequireAnyLabels: append(slices.Clone(filter.LabelsAny), filter.LabelsOrNoProject...),
	})
	if err != nil {
		return fmt.Errorf("list beads parents: %w", err)
	}
	for _, b := range beads {
		if p, ok := parentMap[b.ID]; ok {
			b.ParentID = p.ID
			b.ParentTitle = p.Title
		}
	}
	return nil
}
