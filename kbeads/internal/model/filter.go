package model

import (
	"fmt"
	"strings"
	"time"
)

// FleetAll is a sentinel value meaning "all fleets" (cross-fleet query).
const FleetAll = "*"

// BeadFilter holds criteria for querying beads.
type BeadFilter struct {
	Status       []Status   `json:"status,omitempty"`
	Type         []BeadType `json:"type,omitempty"`
	ExcludeTypes []BeadType `json:"exclude_types,omitempty"`
	// ExcludeClosedTypes drops the closed beads of these types and keeps the
	// open ones. The land window reads its plans' open decisions and
	// escalations without their closed history this way (gd-FC7tIACC5f).
	// Never set from a request.
	ExcludeClosedTypes []BeadType `json:"-"`
	Kind               []Kind     `json:"kind,omitempty"`
	Priority           *int       `json:"priority,omitempty"`
	PriorityMin        *int       `json:"priority_min,omitempty"`
	PriorityMax        *int       `json:"priority_max,omitempty"`
	Assignee           string     `json:"assignee,omitempty"`
	// Phase 3b (ka-0000002249): multi-valued operator-tab predicates. The
	// query builder OR-merges each *_in slice with its scalar counterpart
	// when both are set so callers can mix-and-match.
	Assignees []string `json:"assignees,omitempty"`
	// Unassigned, when true, matches only beads with NO assignee — the
	// `assignee IS NULL OR assignee = <empty>` predicate. It is the single most
	// common work-seeking clause ("open, unblocked, nobody on it yet") and had
	// no server-side expression at all: `?assignee=` is indistinguishable from
	// "no assignee filter" (the empty scalar is dropped by mergeNonEmpty), and
	// the `-f key=` field-absent syntax does not apply because assignee is a
	// first-class column, not a JSONB custom field. So the query degraded to a
	// full-table pull + hand-rolled jq, whose one wrong field name silently
	// returns a FALSE-ZERO "no work" — the same footgun --exclude-label was
	// added to kill (ka-0000007103, class bug ka-0000007209).
	//
	// Mutually exclusive with Assignee/Assignees by construction: asking for
	// "assigned to X" AND "assigned to nobody" can never match, so the CLI
	// rejects the combination rather than silently returning 0.
	Unassigned bool     `json:"unassigned,omitempty"`
	Owner      string   `json:"owner,omitempty"`
	Owners     []string `json:"owners,omitempty"`
	CreatedBy  string   `json:"created_by,omitempty"`
	CreatedBys []string `json:"created_bys,omitempty"`
	// OperatedByAliases matches the canonical `kind:subject` alias resolved
	// through operator_aliases — e.g. `oidc:matt@acme.example` returns rows
	// whose operated_by_id resolves (via the alias index) to that human's
	// canonical operator, including legacy Slack-driven beads that were
	// recorded as `slack:U123ABC` and later coalesced under the same operator
	// via post-hoc alias attach. Multiple aliases OR-merge through
	// ANY($::text[]). Joins through:
	//
	//   beads JOIN operators o ON o.id = beads.operated_by_id
	//         JOIN operator_aliases oa ON oa.operator_id = o.id
	//
	// Empty list = no filter. ADR §5.3, P4 acceptance test. Replaces the
	// legacy OperatedBy / OperatedBys / OperatedByIsNull TEXT predicates
	// removed in P6 (migration 024).
	OperatedByAliases []string   `json:"operated_by_aliases,omitempty"`
	UpdatedBefore     *time.Time `json:"updated_before,omitempty"`
	Labels            []string   `json:"labels,omitempty"`
	// LabelsAny is the INTERNAL any-of label filter: a bead matches when it
	// carries at least one of these labels (one EXISTS … label = ANY()). The
	// server uses it for OIDC read-side project scoping — every request from
	// an OIDC user is narrowed to their allowed projects by appending the
	// project labels here. It is NOT bound to any user-facing flag; binding a
	// user any-of into this slice would OR the user's labels with the OIDC
	// project labels and silently widen project isolation. User any-of goes
	// through LabelsAnyUser instead.
	LabelsAny []string `json:"labels_any,omitempty"`
	// Titles matches beads whose title is EXACTLY one of these (case-sensitive,
	// no wildcards, no full-text), the lookup type:role beads are keyed by (the
	// controller and the authorization gates resolve a role by its title). It is
	// the exact counterpart to Search and replaces fetching a whole type to
	// match titles client-side. Composes via AND, so it only narrows. Empty = no
	// filter.
	Titles []string `json:"titles,omitempty"`
	// LabelsOrNoProject is the INTERNAL "in scope, or global" label filter: a
	// bead matches when it carries at least one of these labels OR carries no
	// `project:<name>` label at all. The server sets it INSTEAD of the strict
	// Labels project predicate for a per-agent token listing only advice, so
	// label-less (Global / role-only) advice stays readable by a
	// project-scoped agent while a bead labelled for another project stays
	// hidden. It is NOT bound to any user-facing flag, and composes via AND
	// with every other predicate, so it can only ever narrow. Empty = no filter.
	LabelsOrNoProject []string `json:"labels_or_no_project,omitempty"`
	// LabelsAnyUser is the USER-facing any-of label filter (kd list
	// --label-any), surfaced over proto/HTTP as labels_any. It emits its own
	// EXISTS … label = ANY() clause that ANDs with LabelsAny, Labels, and the
	// other predicates — so a user any-of narrows *within* OIDC project scope
	// and can never widen it (ka-0000003307). A bead matches when it carries
	// at least one of these labels.
	LabelsAnyUser []string `json:"labels_any_user,omitempty"`
	// ExcludeLabels drops beads carrying ANY of these labels (label NOT IN).
	// Used by the gardener-delivery trigger's cron preflight to skip MRs
	// marked with an opt-out label (e.g. gardener-delivery-skip) so the
	// gardener stays dormant when the only open MRs are ones an operator
	// flagged as not-its-lane. Composes with Labels/LabelsAny via AND.
	ExcludeLabels []string `json:"exclude_labels,omitempty"`
	// TaintGrantIDs, when non-nil, gates results to beads carrying NO taint
	// outside this set (NOT EXISTS a bead_taints row whose taint_id is absent
	// from it), per ADR docs/adr/taints.md §5.1: `taintsOf(bead) ⊆
	// grantsOf(caller)`. Unlike Labels/LabelsAny — which are no-ops at their
	// zero value (nil or empty slice both mean "no filter") — TaintGrantIDs
	// distinguishes nil (gate not applied: caller is exempt, or the taints
	// registry is empty — the ADR §10.1 zero-config short-circuit) from a
	// non-nil-but-empty slice (gate applied, caller holds zero grants: every
	// tainted bead is excluded). Composes via AND with every other predicate;
	// it only ever narrows.
	TaintGrantIDs *[]int64 `json:"taint_grant_ids,omitempty"`
	// TaintID, when non-nil, restricts results to beads carrying this taint
	// (an EXISTS bead_taints row for the id) — the positive "carries taint
	// X" counterpart to TaintGrantIDs' negative gate predicate. Used by the
	// taints registry page (docs/adr/taints.md §9.1) to count a taint's
	// beads under the caller's own read scope. Composes via AND with every
	// other predicate, including TaintGrantIDs — a caller still needs the
	// gate to pass on top of this positive filter.
	TaintID *int64 `json:"taint_id,omitempty"`
	// ExclusiveProject narrows results to beads that belong EXCLUSIVELY to the
	// named project: a bead matches only when it carries NO `project:<other>`
	// label. A bead with just `project:gardenio` matches (ExclusiveProject
	// "gardenio"); a bead with both `project:gardenio` and `project:monorepo` is
	// dropped; a bead with no project label at all still matches (it belongs to
	// no OTHER project). This is the SQL predicate behind `gb ready` / `gb news`
	// project scoping — it replaces the client-side filterExclusiveProject pass
	// (and its over-fetch multiplier) so LIMIT accounts for the narrowing and the
	// no-underfill guarantee is exact rather than a 4x bound (ka-0000003411).
	//
	// It composes via AND with the token/OIDC project scoping (LabelsAny /
	// Labels): those decide which projects the caller MAY see, this one drops the
	// multi-project residue those clauses still let through (a dual-project row
	// carries an allowed project label, so it passes them). Empty = no filter.
	ExclusiveProject string            `json:"exclusive_project,omitempty"`
	Search           string            `json:"search,omitempty"`       // full-text search on title/description
	Fields           map[string]string `json:"fields,omitempty"`       // custom field key=value filters (JSONB)
	NoOpenDeps       bool              `json:"no_open_deps,omitempty"` // only beads with no open/in_progress/deferred dependencies
	NoParent         bool              `json:"no_parent,omitempty"`    // only beads with no parent-child parent (orphans)
	// FieldsIn keeps beads whose fields->>key is one of the listed values, per
	// key (?field_in=key=a,b,c). It reads many channels' newest messages in one
	// list rather than one list each (gd-OYKXtzN9kr). Empty = no filter.
	FieldsIn map[string][]string `json:"fields_in,omitempty"`
	// ShortTitleMissing restricts results to beads whose short_title is unset
	// (NULL or ''): the short-title groomer's work queue (plan
	// 2026-10-14-short-titles D5).
	ShortTitleMissing bool     `json:"short_title_missing,omitempty"`
	IDs               []string `json:"ids,omitempty"` // fetch specific bead IDs
	// Heraldry restricts results to beads bearing exactly these arms, as
	// the run (heraldry spec §3.1, the heraldry= list filter); a bead still
	// stored in the shield form matches by its run.
	Heraldry string `json:"heraldry,omitempty"`
	// HeraldryRegister restricts results to the beads bearing arms in this
	// project's register: beads.project (migration 057) equal to it and arms
	// set. The daemon's register check reads it; it is not on the wire.
	HeraldryRegister string `json:"-"`
	// Mentions, when set, restricts results to beads whose text references
	// (parsed ka-id mentions, stored in bead_references) include the named
	// target bead id. Joins through bead_references.target_bead_id. Composes
	// with all other structured filters via AND. See design § 5c at
	// kbeads/docs/design-bead-references.md.
	Mentions string `json:"mentions,omitempty"`
	// Categories filters on the materialized beads.category column populated by
	// the BEFORE INSERT/UPDATE trigger (migration 020). Used by the Inbox v1
	// query shape (`category='intake' AND operated_by_id=:viewer_operator_id`)
	// and chip-strip variants (intermediate / action / config). Multiple values
	// OR-merge via ANY($::text[]). Empty list = no filter. See gardenio/plans/
	// inbox-v1-option-b-sequencing.md §1.2.1.
	Categories []string `json:"categories,omitempty"`
	// OperatedByID filters on the operators(id) FK column. nil = no filter.
	// *0 = no match (clamped at the SQL layer). Used by the Inbox v1 §1.2.1
	// query shape.
	OperatedByID *int64 `json:"operated_by_id,omitempty"`
	// MentionsType narrows the Mentions filter by bead_references.relationship_type
	// (typed-mention verb such as "parent", "blocks", "related"). Only applied
	// when Mentions is non-empty. nil = no filter; non-nil "" = untyped only
	// (relationship_type IS NULL); non-nil "<verb>" = exact match. Mirrors the
	// three-state shape used by ListBeadsMentioning / ListBeadReferencesFrom
	// (see relationshipTypeClause in internal/store/postgres/queries_references.go).
	MentionsType  *string    `json:"mentions_type,omitempty"`
	UpdatedAfter  *time.Time `json:"updated_after,omitempty"`
	CreatedAfter  *time.Time `json:"created_after,omitempty"`
	CreatedBefore *time.Time `json:"created_before,omitempty"`
	// ClosedAfter / ClosedBefore window on beads.closed_at. closed_at is
	// nullable; the SQL comparisons are NULL-safe false, so never-closed beads
	// never match either bound — "closed in the last 24h" cannot leak open rows.
	ClosedAfter  *time.Time `json:"closed_after,omitempty"`
	ClosedBefore *time.Time `json:"closed_before,omitempty"`
	ParentID     string     `json:"parent_id,omitempty"`
	// ParentSubtreeID restricts results to the transitive subtree of a parent
	// bead via parent-child dependency edges (descendants at any depth, not
	// the parent itself). Used by `bead_query` conditions with a `parent`
	// field — e.g. "count open tasks under this epic" for gardener-loop
	// exit/loop_end evaluation. See gardenio/plans/epic-mention-looping-gardener.md §T2.
	ParentSubtreeID string `json:"parent_subtree_id,omitempty"`
	// ParentSubtreeIDs is ParentSubtreeID over several roots at once: the
	// union of their subtrees, in one query. A root is in the result only
	// when it descends from another root (or from itself, through a cycle).
	// The land window reads every plan's closure this way (gd-rXMNcFrzDp).
	// Never set from a request.
	ParentSubtreeIDs []string `json:"-"`
	Fleet            string   `json:"fleet,omitempty"` // scope results to a specific fleet
	Sort             string   `json:"sort,omitempty"`  // e.g. "-priority", "created_at"; prefix "-" = descending
	Limit            int      `json:"limit,omitempty"`
	Offset           int      `json:"offset,omitempty"`
	// IncludeDeleted, when true, returns soft-deleted beads (deleted_at IS NOT NULL)
	// alongside live ones. Default behavior excludes them from listings. Admin-only
	// at the API layer; see http.go include_deleted query param.
	IncludeDeleted bool `json:"include_deleted,omitempty"`
	// OnlyDeleted, when true, returns ONLY soft-deleted beads. Implies
	// IncludeDeleted at the SQL layer. Used for "deleted bead" admin views.
	OnlyDeleted bool `json:"only_deleted,omitempty"`
	// ExcludeScheduled, when true, hides beads carrying a schedule_id field
	// (auto-spawned by the cron scheduler each tick). Used by ready/active
	// views to suppress noise from beads that re-spawn every cron cycle and
	// never need a human claim. See ka-0000006652.
	ExcludeScheduled bool `json:"exclude_scheduled,omitempty"`
	// HasReport, when non-nil, filters to beads whose has_report projection
	// matches the bool. has_report is true when the bead has a direct
	// (depth-1) parent-child child whose type ∈ {report, rich-report, bundle}
	// and whose deleted_at IS NULL. Used by the sequencer↔report linking
	// view (ka-0000007923 / R1) so the sequencer board can surface which
	// scope beads have an attached report artifact. See
	// gardenio/docs/research/sequencer-report-decision-visibility.md §R1.
	//
	// A non-nil filter implicitly enables the has_report SELECT projection
	// (the predicate must be evaluated either way). Callers that want the
	// projection without filtering should set WithHasReport=true.
	HasReport *bool `json:"has_report,omitempty"`
	// WithHasReport, when true, includes the has_report column in the result
	// projection without applying it as a filter. Defaults to false because
	// the EXISTS subquery is non-trivial (joins deps to beads on the new
	// idx_deps_depends_on_id_type index — fast, but never free) and the vast
	// majority of ListBeads callers (slack-bridge config-bead lookups, the
	// scheduler roster, gb ready, kd list without --has-report) never read
	// the column. UI surfaces that need the badge (beads-viewer sequencer
	// board, kd list --has-report) opt in explicitly.
	WithHasReport bool `json:"with_has_report,omitempty"`
	// BlockedOnDecision filters on the daemon-computed blocked_on_decision
	// predicate (ka-0000001065 / R2 in
	// gardenio/docs/research/sequencer-report-decision-visibility.md). Predicate is
	// the OR of:
	//   - an open type=decision bead has a parent-child dep edge to this bead
	//     or a descendant (depth ≤ 2);
	//   - a type=agent bead under this bead (depth ≤ 2) has fields.decision_bead_id
	//     pointing at an OPEN decision (a closed-decision reference does not flag).
	// nil = no filter. *true = only blocked rows. *false = only unblocked rows.
	//
	// A non-nil filter implicitly enables the blocked_on_decision SELECT
	// projection. Callers that want the projection without filtering should
	// set WithBlockedOnDecision=true.
	BlockedOnDecision *bool `json:"blocked_on_decision,omitempty"`
	// ParkedOnDecision filters on the daemon-computed parked_on_decision
	// predicate — the TEXT-MENTION sibling of BlockedOnDecision (ka-0000003797).
	// True iff an open type=decision bead names this bead's id in its prose
	// (title/description/notes/comment), via a bead_references row. Same
	// three-state pointer semantics as BlockedOnDecision: nil = no filter,
	// *true = only parked rows, *false = only un-parked rows. A non-nil filter
	// implicitly enables the parked_on_decision SELECT projection; callers that
	// want the projection without filtering set WithParkedOnDecision=true.
	ParkedOnDecision *bool `json:"parked_on_decision,omitempty"`
	// CountOnly, when true, switches ListBeads into a count-only query path:
	// the daemon runs `SELECT COUNT(*) FROM beads WHERE ...` against the
	// resolved filter and returns ([], total, nil) — no sort, no projections,
	// no row bodies. Lets a caller that needs only the count avoid the
	// COUNT(*) OVER() + ORDER BY pattern that forces postgres to materialize
	// and sort the entire matching set even when LIMIT 1 is specified.
	// Limit / Offset / Sort are ignored when this is set.
	CountOnly bool `json:"count_only,omitempty"`
	// NoTotal, when true, skips the narrow SELECT COUNT(*) that a full page
	// (or an empty page past offset 0) otherwise pays for its total
	// (gd-g25jihMHFs): that count scans the whole matching set on every
	// refresh. A short page still reports its exact total (offset + rows);
	// otherwise total is -1, meaning "not counted, there may be more". For
	// callers that page or poll but never show a total. Text search is
	// unaffected: its total comes from the window it computes anyway.
	NoTotal bool `json:"no_total,omitempty"`
	// WithOpenBlockers, when true, populates Bead.OpenBlockers on every
	// returned row: each bead's OPEN blocking edges denormalized with the
	// blocker's status/type/title (ka-0000000945). Answers "why is each of
	// these beads not ready?" for a whole page in one call, replacing the
	// per-bead `kd show` fan-out a backlog-triage caller pays today (~44
	// round-trips to classify 23 bugs in the audited session).
	//
	// Off by default: it costs one extra deps⋈beads batch query per page, and
	// the vast majority of ListBeads callers (bridges, scheduler roster, gb
	// ready) never read blockers. Unlike the has_report / blocked_on_decision
	// projections this is NOT a SELECT column — it is a second batched query
	// keyed on the returned page's ids, so its cost scales with the PAGE, not
	// with the matching set.
	//
	// There is no matching filter field: "only blocked rows" is already
	// expressible as NoOpenDeps=false-side (`kd list` without --no-blockers),
	// and the enrichment is orthogonal to which rows match.
	WithOpenBlockers bool `json:"with_open_blockers,omitempty"`

	// WithParent, when true, populates Bead.ParentID (and Bead.ParentTitle when
	// the parent is visible under the caller's scope) on every returned row: the
	// immediate parent-child PARENT of each bead (ka-0000006083). Lets a
	// dispatcher reasoning over `gb ready --json` distinguish a leaf task under
	// an epic from a floating one — and group by epic — without an N+1 `kd show`
	// per row.
	//
	// Off by default and, like WithOpenBlockers, NOT a SELECT column: it is a
	// second batched deps⋈beads query keyed on the returned page's ids, so its
	// cost scales with the PAGE, not the matching set. The default callers
	// (bridges, scheduler roster, plain `kd list`) never read the parent and so
	// never pay for it.
	//
	// There is no matching filter field — orphan-vs-parented filtering is already
	// expressible via NoParent / ParentID / ParentSubtreeID; this enrichment only
	// surfaces the parent id per row, it does not select rows.
	WithParent bool `json:"with_parent,omitempty"`

	// OmitDescription / OmitNotes, when true, leave the bead's description /
	// notes out of the list projection: the SELECT reads a NULL in their place,
	// so the long free-text bodies cross neither the Postgres connection nor
	// the wire, and the rows come back with those fields empty (gd-zXdZq4Traf).
	// For poll loops that re-list a roster every cycle and read only ids,
	// status, labels and fields — measured at 48 GB/day of GET /v1/beads for
	// the trigger-engine alone. Off by default, like the with_* projections: a
	// caller that sets them must never write an omitted body back.
	OmitDescription bool `json:"omit_description,omitempty"`
	OmitNotes       bool `json:"omit_notes,omitempty"`

	// OmitFields drops the named keys from each row's custom fields in the
	// list projection (gd-zXdZq4Traf). Each entry is a dotted path into the
	// JSONB object — "prompt" drops a top-level key, "gardener.run_log" drops
	// one nested inside an object-valued key — applied in SQL with `#-`, so
	// the bytes never leave Postgres. Measured 2026-10-23: fields.prompt is
	// ~70% of an agent bead and fields.gardener.run_log / progress_state ~50%
	// of a gardener trigger, re-listed every 30-60 s by the controller and
	// trigger-engine. Segments are restricted to [A-Za-z0-9_-] (see
	// ValidOmitFieldPath) because the path is inlined as a SQL literal. Empty
	// by default; like OmitDescription, a caller must never write back a bead
	// it listed with keys omitted.
	OmitFields []string `json:"omit_fields,omitempty"`

	// FieldsOnly, when non-empty, keeps ONLY the named top-level keys of each
	// row's custom fields in the list projection and drops the rest in SQL
	// (gd-6PXLuR3juD). The inverse of OmitFields, for history walks that sum
	// one or two fields over thousands of beads every few minutes — the
	// controller's gardener-spend walk, the xcheck scans and the archetype
	// walk read 1-3 keys of a ~8 KB agent row. A requested key the bead does
	// not carry is simply absent. Takes precedence over OmitFields (whatever
	// OmitFields would strip is already gone). Keys must pass ValidFieldKey
	// because they are inlined as SQL literals. Empty by default; like
	// OmitFields, a caller must never write back a bead it listed this way.
	FieldsOnly []string `json:"fields_only,omitempty"`

	// WithBlockedOnDecision, when true, includes the blocked_on_decision_bead_id
	// column in the result projection without applying it as a filter.
	// Defaults to false because the projection is a per-row MIN(d.id)
	// correlated subquery over open decisions plus a two-level deps walk;
	// on the gardenio-e2e dataset this was the dominant cost on /v1/beads
	// (15.9M shared buffer hits per 50-row page, 10–30 s p99, slack-bridge
	// readiness probe failing) before it became opt-in.
	WithBlockedOnDecision bool `json:"with_blocked_on_decision,omitempty"`

	// WithParkedOnDecision, when true, includes the parked_on_decision_bead_id
	// column in the result projection without applying it as a filter
	// (ka-0000003797). Defaults to false with the same discipline as
	// WithBlockedOnDecision: the projection is a per-row MIN(d.id) correlated
	// subquery over open decisions with an EXISTS probe into bead_references, so
	// only the ready/news queue surfaces that render the parked indicator opt in.
	WithParkedOnDecision bool `json:"with_parked_on_decision,omitempty"`
}

// ValidFieldKey reports whether k is a top-level fields key FieldsOnly
// accepts: one non-empty segment of [A-Za-z0-9_-].
func ValidFieldKey(k string) bool {
	return !strings.Contains(k, ".") && ValidOmitFieldPath(k)
}

// ParseFieldsIn reads ?field_in= values into BeadFilter.FieldsIn: each is
// key=a,b,c, and a repeated key adds to its values. Values never hold a comma.
// A missing key or no value is an error. None given is nil.
func ParseFieldsIn(raw []string) (map[string][]string, error) {
	var out map[string][]string
	for _, f := range raw {
		k, v, _ := strings.Cut(f, "=")
		var vals []string
		for _, val := range strings.Split(v, ",") {
			if val = strings.TrimSpace(val); val != "" {
				vals = append(vals, val)
			}
		}
		if k == "" || len(vals) == 0 {
			return nil, fmt.Errorf("field_in: want key=value[,value...], got %q", f)
		}
		if out == nil {
			out = map[string][]string{}
		}
		out[k] = append(out[k], vals...)
	}
	return out, nil
}

// maxOmitFieldDepth bounds an OmitFields path; nothing real is nested deeper.
const maxOmitFieldDepth = 4

// ValidOmitFieldPath reports whether p is a dotted fields path OmitFields
// accepts: 1..maxOmitFieldDepth non-empty segments of [A-Za-z0-9_-].
func ValidOmitFieldPath(p string) bool {
	segs := strings.Split(p, ".")
	if len(segs) > maxOmitFieldDepth {
		return false
	}
	for _, s := range segs {
		if s == "" {
			return false
		}
		for _, r := range s {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return false
			}
		}
	}
	return true
}
