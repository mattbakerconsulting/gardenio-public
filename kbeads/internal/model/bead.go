package model

import (
	"encoding/json"
	"time"
)

// Kind is a two-level classification for beads.
type Kind string

const (
	KindIssue  Kind = "issue"
	KindData   Kind = "data"
	KindConfig Kind = "config"
)

// String returns the string representation of the kind.
func (k Kind) String() string {
	return string(k)
}

// IsValid checks whether the kind is a known value.
func (k Kind) IsValid() bool {
	switch k {
	case KindIssue, KindData, KindConfig:
		return true
	}
	return false
}

// BeadType categorizes the kind of bead.
// Well-known constants are provided below, but bead types are extensible;
// custom types (e.g. "message", "gate", "workflow") are valid.
type BeadType string

// Issue-kind types.
const (
	TypeEpic    BeadType = "epic"
	TypeTask    BeadType = "task"
	TypeFeature BeadType = "feature"
	TypeChore   BeadType = "chore"
	TypeBug     BeadType = "bug"
)

// Data-kind types.
const (
	TypeAdvice     BeadType = "advice"
	TypeJack       BeadType = "jack"
	TypeDecision   BeadType = "decision"
	TypeEscalation BeadType = "escalation"
	TypeReport     BeadType = "report"
	TypeFormula    BeadType = "formula"
	// TypeEvidence is one quality record about a step: a UX grade today
	// (fields.kind=ux), filed parented to the step and closed straight away.
	// It is data, not work, so no queue lists it. The Lands harvest reads it
	// (spec gardenio/docs/superpowers/specs/2026-10-22-lands-harvest-icons-design.md §2.1).
	TypeEvidence BeadType = "evidence"
	// TypeRelease is one tag of one project: the commit, the merged PRs it
	// carries and where it runs (gardenio/docs/releases-design.md §3.1). It
	// is parented to its project bead and keyed by (project, tag); its status
	// (cut, built, live, superseded) is derived by the daemon from its
	// fields, never written by a client.
	TypeRelease BeadType = "release"
	// TypeInvestigationRequest is a low-privilege request to run a
	// acme-scif investigation (acme-scif v1.1 Phase B, plan ka-0000002842
	// §1.2/§7). Any agent may create one; a fresh request is inert — it does
	// nothing until a human approves it through the Phase D portal. The
	// create-time guard (applyInvestigationRequestGuard, server package)
	// enforces that a request is born un-approved: approval / launch / egress
	// fields are rejected at create and requested_by is server-stamped from
	// the authenticated identity so it cannot be spoofed.
	TypeInvestigationRequest BeadType = "investigation-request"
	// TypeScrubRequest is a low-privilege request to scrub (detaint or purge)
	// a set of tainted beads (ADR docs/adr/taints.md §6.2, decision
	// gd-Swd7mnGf3L). Like TypeInvestigationRequest, it is born inert: the
	// create-time guard (applyScrubRequestGuard, server package) rejects any
	// approval/execution field set at create, stamps requested_by from the
	// authenticated identity, and freezes resolved_bead_ids by evaluating
	// scope exactly once. Notably absent from its schema: any detaint_mode /
	// mode field — the fork-vs-strip choice lives only on the taint's own
	// scrub_policy, set by the taint owner, never by the requester.
	TypeScrubRequest BeadType = "scrub-request"
)

// Config-kind types.
const (
	TypeFleet   BeadType = "fleet"
	TypeProject BeadType = "project"
	TypeAccount BeadType = "account"
	// TypeTeam is the principal kind ADR docs/adr/taints.md §8.1 defines:
	// membership (explicit ids or IdP groups) resolved per-request by
	// server.(*BeadsServer).TeamsOf. In the same family as TypeProject and
	// TypeAccount — strict schema, admin-only writes, cross-fleet visible.
	TypeTeam BeadType = "team"
)

// crossFleetTypes are bead types whose rows describe system-level
// configuration (projects, accounts, teams) intended to be visible to every
// fleet — not just the fleet of the requesting caller. The SQL builder
// bypasses the implicit `fleet = $current` clause when a list query targets
// only these types so that fleet-scoped service tokens (e.g. bridges) can
// still resolve project/account/team metadata.
var crossFleetTypes = map[BeadType]struct{}{
	TypeProject: {},
	TypeAccount: {},
	TypeTeam:    {},
}

// IsCrossFleetType reports whether beads of this type are shared across all
// fleets (system-level configuration). See crossFleetTypes for the set.
func IsCrossFleetType(t BeadType) bool {
	_, ok := crossFleetTypes[t]
	return ok
}

// AllTypesCrossFleet reports whether every type in the filter is a
// cross-fleet type and the filter is non-empty. Used by the SQL builder to
// decide whether to drop the fleet WHERE clause for a fleet-scoped caller.
func AllTypesCrossFleet(types []BeadType) bool {
	if len(types) == 0 {
		return false
	}
	for _, t := range types {
		if !IsCrossFleetType(t) {
			return false
		}
	}
	return true
}

// Molecule is an issue-kind type (instantiated formula, acts as an epic).
const TypeMolecule BeadType = "molecule"

// TypeRichReport is an issue-kind container that holds widget-instance children
// and renders as a dense user-facing page (Incident Intelligence, epic
// ka-0000006762). It is also the type fsm-triage creates its triage beads as,
// which is why it carries the repro-harness gate fields (ka-0000003501).
//
// NOTE (retired authoring surface): the widget/report deprecation (operator
// directive, epic ka-0000007561) retires rich-report as an AUTHORING surface in
// favor of TypeBundle. fsm-triage, the only live producer, migrates onto
// TypeBundle (ka-0000004066). Existing rich-report rows still render and the
// daemon still validates them, so this const is still referenced; do not author
// new rich-report beads. (Intentionally not a `Deprecated:` doc marker — that
// magic prefix would trip staticcheck SA1019 on the const's own live internal
// validation uses.)
const TypeRichReport BeadType = "rich-report"

// TypeBundle is an issue-kind container holding type=doc children linked via
// parent-child edges; the beads-viewer renders it as a multi-doc page. It is
// the canonical user-facing deliverable type (advice ka-0000007697) and the
// migration target for fsm-triage off TypeRichReport (ka-0000004066), so it
// carries the repro-harness gate fields (reproFields) and the same
// gate_passed→valid-URL invariant that TypeRichReport does.
const TypeBundle BeadType = "bundle"

// String returns the string representation of the bead type.
func (t BeadType) String() string {
	return string(t)
}

// IsValid reports whether the bead type is a non-empty string.
// Bead types are extensible, so any non-empty value is accepted.
func (t BeadType) IsValid() bool {
	return t != ""
}

// TypeAliases maps deprecated type names to their canonical replacements.
// "template" → "formula".
//
// Note: "bundle" was previously aliased to "molecule"; the alias was removed
// when type=bundle became a first-class type for the agent-artifact-bundles
// feature (see gardenio/plans/agent-artifact-bundles-multi-doc-viewer.md).
// Existing molecule rows are unaffected — they were stored with type="molecule"
// at write time, so the alias only ever rewrote new writes.
var TypeAliases = map[BeadType]BeadType{
	"template": TypeFormula,
}

// Status represents the current state of a bead.
type Status string

const (
	StatusOpen       Status = "open"
	StatusInProgress Status = "in_progress"
	StatusDeferred   Status = "deferred"
	StatusClosed     Status = "closed"
	StatusBlocked    Status = "blocked"
	// StatusPartial is terminal-for-progression: a step in 'partial' fires its
	// on_complete handler and unblocks downstream steps just like 'closed', but
	// is semantically distinct — some descendant work failed (durable follow-up
	// failure beads exist). Unlike 'closed' it does NOT stamp closed_at, so it
	// does not count as a clean completion. See design ka-0000007305 §2.3.
	StatusPartial Status = "partial"
)

// String returns the string representation of the status.
func (s Status) String() string {
	return string(s)
}

// IsValid checks whether the status is a known value.
func (s Status) IsValid() bool {
	switch s {
	case StatusOpen, StatusInProgress, StatusDeferred, StatusClosed, StatusBlocked, StatusPartial:
		return true
	}
	return false
}

// IsTerminalForProgression reports whether a bead in this status is "done" for
// the purpose of unblocking downstream work and firing on_complete handlers.
// Both 'closed' (clean completion) and 'partial' (completed with some descendant
// failures) satisfy blocking dependencies so dependents can proceed. See
// design ka-0000007305 §2.3.
func (s Status) IsTerminalForProgression() bool {
	return s == StatusClosed || s == StatusPartial
}

// Bead is the core work-item record.
type Bead struct {
	ID          string          `json:"id"`
	Slug        string          `json:"slug,omitempty"`
	ShortTitle  string          `json:"short_title,omitempty"`
	Heraldry    string          `json:"heraldry,omitempty"` // arms, canonical form (heraldry spec §4.1); "" = none
	Kind        Kind            `json:"kind"`
	Type        BeadType        `json:"type"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Notes       string          `json:"notes,omitempty"`
	Status      Status          `json:"status"`
	Priority    int             `json:"priority"`
	Assignee    string          `json:"assignee,omitempty"`
	Owner       string          `json:"owner,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	CreatedBy   string          `json:"created_by,omitempty"`
	UpdatedAt   time.Time       `json:"updated_at"`
	ClosedAt    *time.Time      `json:"closed_at,omitempty"`
	ClosedBy    string          `json:"closed_by,omitempty"`
	DueAt       *time.Time      `json:"due_at,omitempty"`
	DeferUntil  *time.Time      `json:"defer_until,omitempty"`
	DeletedAt   *time.Time      `json:"deleted_at,omitempty"`
	Fields      json.RawMessage `json:"fields,omitempty"`
	Fleet       string          `json:"fleet"`
	// Category is the materialized inbox-lens classification (intake /
	// intermediate / action / config / artifact) populated by the SQL trigger
	// introduced in migration 020 (derive_bead_category, rule table last
	// rewritten in migration 043). Read-only at the API layer — clients cannot
	// set it directly.
	Category string `json:"category,omitempty"`

	// OperatedBy identifies the human operator who caused this bead to be
	// created — distinct from CreatedBy (the agent that performed the write).
	// Format: "<provider>:<subject>" (e.g. "slack:U…", "oidc:matt@acme.example").
	// Server-resolved via resolveOperatedBy; clients cannot supply it
	// directly. Post-R2 (ka-0000005772) live-state writes fail-closed when
	// the resolver cannot attribute the request to an operator, so this
	// field is always non-empty on rows written through current API paths.
	// See migration 012_human_attribution.up.sql.
	OperatedBy        string `json:"operated_by,omitempty"`
	OperatedByDisplay string `json:"operated_by_display,omitempty"`
	// OperatedByEmail is the canonical email resolved from operators.email
	// via LEFT JOIN on operated_by_id. Empty when no operator row matches or
	// operators.email is blank. Read-only from clients; write side is
	// the operator-resolver pipeline (migration 015).
	OperatedByEmail string `json:"operated_by_email,omitempty"`
	// OperatedByID is the operators(id) FK populated by the resolver during
	// the dual-write window (P2 of ADR docs/adr/human-attribution-identity-v2.md).
	// nil when the resolver did not match an operator (autonomous writes,
	// flag-off, or alias upsert failure). Migration 016 will enforce NOT NULL
	// for non-history-only tables; until then both columns are written together
	// and the TEXT column remains the read-side source of truth.
	OperatedByID *int64 `json:"operated_by_id,omitempty"`

	// CreatedByOperatorID is the immortal originator FK — the operator who
	// raised the bead, distinct from OperatedByID which flips on dibs
	// handoffs. Set once at create-time and never updated thereafter.
	// Surfaces the raise-only operator on the inbox card (inbox-constellation
	// §4.3.1). nil for legacy rows with no recoverable originator.
	// See migration 018_created_by_operator_id.up.sql.
	CreatedByOperatorID *int64 `json:"created_by_operator_id,omitempty"`

	// HasReport is the per-row projection of the has_report predicate: true
	// when this bead has a direct (depth-1) parent-child child whose type is
	// one of {report, rich-report, bundle} and whose deleted_at IS NULL.
	// Populated by ListBeads SELECT projection; not stored. See R1 spec at
	// gardenio/docs/research/sequencer-report-decision-visibility.md.
	HasReport bool `json:"has_report,omitempty"`

	// ParentID is the immediate parent-child PARENT's bead id (the epic /
	// container this bead hangs under), populated ONLY when the caller opts in
	// (BeadFilter.WithParent / `gb ready`). Empty in three cases that a scalar
	// deliberately conflates because none is "a parent to work with": the bead
	// is a genuine orphan (no parent-child edge), the caller did not opt in, or
	// the daemon predates the projection. Read-only — derived per query from the
	// deps table (bead_id=child, depends_on_id=parent, type ∈ ParentChildDepTypes),
	// never stored on the bead. Lets a dispatcher reasoning over `gb ready --json`
	// tell a leaf task under an epic from a floating one — and group by epic —
	// without an N+1 `kd show` per row (ka-0000006083).
	//
	// A non-empty value is ONE OF possibly several parents: ~28% of gb-ready rows
	// carry more than one parent-child edge. It is the "primary" parent chosen by
	// a MEANINGFUL tie-break — open over closed, epic over non-epic, then most
	// recent edge (ka-0000001227) — not merely the lowest parent id. Callers that
	// need the full set must enumerate the deps table directly.
	ParentID string `json:"parent_id,omitempty"`
	// ParentTitle is the title of ParentID's bead, populated alongside ParentID
	// when WithParent is set AND the parent is visible under the caller's project
	// scope. Empty (with ParentID still set) when the parent is out of the
	// caller's label scope or soft-deleted — the edge is real, the title just
	// must not leak across a project boundary. Best-effort convenience label;
	// ParentID is the load-bearing field.
	ParentTitle string `json:"parent_title,omitempty"`

	// Relational data -- populated by queries, not stored in the beads table.
	Labels       []string      `json:"labels,omitempty"`
	Dependencies []*Dependency `json:"dependencies,omitempty"`
	Comments     []*Comment    `json:"comments,omitempty"`

	// OpenBlockers is the per-row projection of this bead's OPEN blocking
	// edges, denormalized with each blocker's status/type/title (ka-0000000945).
	// Populated by ListBeads ONLY when the caller opts in (BeadFilter.
	// WithOpenBlockers / `kd list --with-blockers`). Uses the same
	// blocking-edge and open-status sets as the NoOpenDeps ready-queue
	// predicate, so a bead with a non-empty OpenBlockers is exactly a bead
	// `--no-blockers` hides. Read-only — derived per query, never stored.
	//
	// It is a POINTER to a slice so the wire format distinguishes three states
	// that a plain slice collapses into one:
	//
	//	nil       → not asked for (key absent)   — every default caller
	//	&[]       → asked, answer is "none"      — genuinely unblocked
	//	&[...]    → asked, answer is these       — blocked, here's why
	//
	// The nil/&[] split is load-bearing, not pedantry. A daemon too old to
	// know the with_open_blockers param ignores it silently and returns rows
	// with the key ABSENT; without this distinction that is byte-identical to
	// "I checked, nothing blocks anything" — a fleet-wide false all-clear
	// handed to the exact triage callers this feature serves. `kd list
	// --with-blockers` treats an all-absent response as a version-skew error
	// rather than an empty backlog (see cmd/kd/list.go).
	OpenBlockers *[]*OpenBlocker `json:"open_blockers,omitempty"`

	// BlockedOnDecision is a daemon-computed predicate set by ListBeads (ka-0000001065 /
	// R2). True iff at least one open type=decision bead is reachable via parent-child
	// (depth ≤ 2) from this bead, OR a type=agent bead under this bead carries
	// fields.decision_bead_id pointing at an open decision. Read-only at the API
	// layer — derived from the SQL projection, never stored on beads. See
	// gardenio/docs/research/sequencer-report-decision-visibility.md (R2 row).
	BlockedOnDecision bool `json:"blocked_on_decision,omitempty"`
	// BlockedOnDecisionBeadID is the ka-id of the open decision that flagged the
	// row (the lexicographically-first id when multiple decisions match — stable
	// for snapshot tests). Empty string when BlockedOnDecision is false.
	BlockedOnDecisionBeadID string `json:"blocked_on_decision_bead_id,omitempty"`

	// ParkedOnDecision is the daemon-computed TEXT-MENTION sibling of
	// BlockedOnDecision (ka-0000003797). True iff at least one open type=decision
	// bead names this bead's id in its title/description/notes/a comment — the
	// signal `kd list --type=decision --status=open --mentions <id>` reports,
	// materialized in bead_references. Distinct from BlockedOnDecision, which is
	// edge-based (parent-child / agent decision_bead_id): a prose-only mention
	// carries no blocking edge, so such a bead stays in `gb ready` and needs this
	// flag to be seen as parked. Read-only at the API layer — derived from the
	// SQL projection, never stored.
	ParkedOnDecision bool `json:"parked_on_decision,omitempty"`
	// ParkedOnDecisionBeadID is the ka-id of the open decision that parks the row
	// (lexicographically-first id when several mention it — stable for snapshot
	// tests). Empty string when ParkedOnDecision is false.
	ParkedOnDecisionBeadID string `json:"parked_on_decision_bead_id,omitempty"`

	// RelevanceScore is the text-search relevance of this bead for the query
	// that produced it (ka-0000003832 / ka-search relevance Option 2). It is the
	// composite of the Option-1 title-match tier (0–3) and a Postgres FTS
	// ts_rank_cd component over the search_tsv column — higher means more
	// relevant. Populated by the ListBeads SELECT projection ONLY when a text
	// search is active; 0 (and omitted from JSON) on non-search listings and on
	// read paths other than ListBeads. Read-only at the API layer — derived from
	// the SQL projection, never stored on beads.
	RelevanceScore float64 `json:"relevance_score,omitempty"`
}
