package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gardenio/kbeads/internal/model"
)

var errBackfillLegacyTargetRequired = errors.New("BackfillOperatedByLegacy: Target is required (e.g. slack:U0OPERATOR1)")

// errBackfillSlackUserIDBatchInvalid is returned when BatchSize is
// negative. Zero / unset falls back to the 200-row default.
var errBackfillSlackUserIDBatchInvalid = errors.New("BackfillSlackUserID: BatchSize must be >= 0")

// errBackfillOperatorSlugsBatchInvalid is returned when BatchSize is
// negative. Zero / unset falls back to the 200-row default.
var errBackfillOperatorSlugsBatchInvalid = errors.New("BackfillOperatorSlugs: BatchSize must be >= 0")

// errBackfillParentChildSinceInvalid is returned when --since is supplied
// as a negative duration. Zero / unset is allowed and falls back to the
// 6-month default per ka-0000006775.
var errBackfillParentChildSinceInvalid = errors.New("BackfillParentChildDeps: Since must be >= 0")

// errBackfillTriggeredBySinceInvalid is returned when --since is supplied
// as a negative duration to the M4 auto-link backfill scanner
// (ka-0000000004). Zero / unset is allowed and falls back to the 24-hour
// rolling-window default per the design doc ka-0000000827 M4/D.
var errBackfillTriggeredBySinceInvalid = errors.New("BackfillTriggeredByDeps: Since must be >= 0")

// errCloseTriggerAuditBatchSize is returned when --batch-size is supplied
// as a negative value. Zero falls back to the default per ka-0000006341.
var errCloseTriggerAuditBatchSize = errors.New("CloseTriggerAuditBeads: BatchSize must be >= 0")

// UpsertOperatorEmailError is the loud, structured error UpsertOperator
// returns when it cannot derive a non-empty email for a (kind, subject)
// pair. Post-migration 027 the operators.email column is NOT NULL with a
// CHECK (email <> ”), so the insert would fail anyway — surfacing this
// error at the resolver boundary gives callers (audit.go, user-provision,
// HTTP link handlers) a clearly-typed signal they can log + bubble up
// instead of an opaque CHECK violation from the driver.
//
// Kind is the operator kind that failed derivation ("slack", "gitlab",
// "jira", or a custom string registered by an integration); Subject is
// the raw subject string passed in. R2 of ka-0000006809 / task
// ka-0000008244.
type UpsertOperatorEmailError struct {
	Kind    string
	Subject string
}

func (e *UpsertOperatorEmailError) Error() string {
	return "UpsertOperator: cannot derive email for kind=" + e.Kind + " subject=" + e.Subject
}

// ReextractReferencesStats summarizes one page of the bead-references
// backfill. LastID is the id of the last bead processed in this page (empty
// when the page was empty); the caller advances the cursor by passing it as
// afterID on the next call. Done is true when the page is the last one
// (PageCount < batchSize); on the final page TotalReferences reports the
// post-backfill cardinality of bead_references.
type ReextractReferencesStats struct {
	BeadsScanned      int
	CommentsScanned   int
	ReferencesWritten int
	LastID            string
	Done              bool
	TotalReferences   int64
}

// BackfillSearchTSVStats summarizes one page of the search_tsv backfill
// (ka-0000003832 / ka-search relevance Option 2). The backfill fills the
// search_tsv FTS column on legacy beads that pre-date migration 039 (new and
// updated beads are filled by the migration's trigger). BeadsScanned is the
// number of NULL-search_tsv rows updated in this page; LastID is the id of the
// last row processed (empty when the page was empty), which the caller passes
// as afterID on the next call. Done is true when fewer than batchSize rows
// remained; on the final page Remaining reports the count of rows still NULL
// (0 on a clean finish) so the caller can confirm full coverage.
type BackfillSearchTSVStats struct {
	BeadsScanned int
	LastID       string
	Done         bool
	Remaining    int64
}

// BackfillCategoryStats summarizes one page of the category materialization
// backfill (ka-0000007274). The backfill re-stamps the beads.category column on
// legacy rows whose stored category disagrees with the current
// derive_bead_category (SQL) rule table — chiefly the
// deliverable/artifact rows written between migration 020 and migration 043 that
// carry a stale `intermediate`. New and updated beads are kept current by the
// migration-020 BEFORE INSERT/UPDATE trigger; this sweeps the pre-existing rows.
// BeadsScanned is the number of rows re-stamped in this page; LastID is the id of
// the last candidate row processed (empty when the page was empty), which the
// caller passes as afterID on the next call. Done is true when fewer than
// batchSize candidate rows remained.
type BackfillCategoryStats struct {
	BeadsScanned int
	LastID       string
	Done         bool
}

// BackfillProjectLabelsStats summarizes one page of the project-label backfill
// (gd-YdsEpnN3Qo): Labelled counts, by bead type, the beads that got the
// project: label the invariant requires (see ProjectLabelMissingCounts). A
// labelled bead leaves the candidate set, so pages need no cursor; Done is true
// when fewer than batchSize candidates remained.
type BackfillProjectLabelsStats struct {
	Labelled map[string]int
	Done     bool
}

// BackfillOperatorsOptions controls one run of the operators FK backfill.
// See kbeads/cmd/kd/admin_backfill_operators.go and
// docs/adr/human-attribution-identity-v2.md §4.3 / §4.5 / §4.6 for the
// algorithm. Pass 1 walks DISTINCT operated_by across the five audit
// tables and upserts operators + operator_aliases (source='migration');
// Pass 2 batched-UPDATEs operated_by_id from operator_aliases. Empty-string
// TEXT rows get the §4.5 split treatment (mutation_history+events stay NULL,
// beads+comments+deps bind to the system:legacy-pre-attribution sentinel).
type BackfillOperatorsOptions struct {
	// DryRun reports the rows the backfill would touch without writing.
	DryRun bool
	// BatchSize bounds each UPDATE's scope by primary-key range. Defaults
	// to 50000 when ≤ 0 (matches the operated_by backfill convention; the
	// resolver path keeps the new partial indexes warm during the run).
	BatchSize int
}

// BackfillOperatorsStats summarizes one run of the operators FK backfill.
// DistinctOperatedBy is the cardinality of the Pass-1 union over the five
// audit tables (excludes empty strings). OperatorsUpserted /
// AliasesUpserted count rows the migration inserted (idempotent on
// re-runs — re-running on a fully-backfilled DB reports zeros). Per-table
// row counts are split into "non-empty" (TEXT had a value, FK was NULL,
// JOIN-bound to alias) and "empty-sentinel" (TEXT=”, bound to the
// system:legacy-pre-attribution sentinel; only beads/comments/deps).
// QuarantinedOperatedBy is the set of TEXT values that did not contain a
// `:` separator (do not expect any; surfaced for manual review).
// VerifierTextPopulatedFKNull is the per-table count of rows where
// operated_by <> ” AND operated_by_id IS NULL after the run — must be
// zero on success modulo any quarantined values.
type BackfillOperatorsStats struct {
	DistinctOperatedBy    int
	OperatorsUpserted     int
	AliasesUpserted       int
	BeadsUpdated          int64
	CommentsUpdated       int64
	DepsUpdated           int64
	EventsUpdated         int64
	MutationsUpdated      int64
	BeadsEmptySentinel    int64
	CommentsEmptySentinel int64
	DepsEmptySentinel     int64
	QuarantinedOperatedBy []string
	VerifierBeads         int64
	VerifierComments      int64
	VerifierDeps          int64
	VerifierEvents        int64
	VerifierMutations     int64
	DryRun                bool
	DurationMs            int64
}

// BackfillOperatedByLegacyOptions controls one run of `kd admin
// backfill-operated-by-legacy` (ka-0000001880). Target is the
// "<kind>:<subject>" operator (typically slack:U0OPERATOR1 — Matt) whose
// candidate beads will be re-categorized and rebound. Statuses limits the
// scan (defaults to {"open"}). BatchSize bounds each Pass-1 transaction
// (defaults to 200). MaxIterations caps the Pass-2 parent-walk (defaults
// to 5). DryRun reports the would-be rebinds without writing.
type BackfillOperatedByLegacyOptions struct {
	Target        string
	Statuses      []string
	BatchSize     int
	MaxIterations int
	DryRun        bool
}

// Validate fills defaults and rejects malformed Targets.
func (o *BackfillOperatedByLegacyOptions) Validate() error {
	if o.Target == "" {
		return errBackfillLegacyTargetRequired
	}
	if len(o.Statuses) == 0 {
		o.Statuses = []string{"open"}
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 200
	}
	if o.MaxIterations <= 0 {
		o.MaxIterations = 5
	}
	return nil
}

// TargetSubject returns the `<subject>` portion of Target. Empty if Target
// has no colon.
func (o *BackfillOperatedByLegacyOptions) TargetSubject() string {
	for i := 0; i < len(o.Target); i++ {
		if o.Target[i] == ':' {
			return o.Target[i+1:]
		}
	}
	return ""
}

// BackfillOperatedByLegacyStats summarizes one run. CategoryA/B/C counts
// reflect rows actually rebound (or, in DryRun, rows that would be);
// CategoryD counts rows the algorithm left attributed to the target.
// RebindCount keys are "<A|B|C>:<new-operator-id>".
type BackfillOperatedByLegacyStats struct {
	DryRun               bool
	Target               string
	TargetOperatorID     int64
	BeforeOpenCandidates int64
	AfterOpenCandidates  int64
	CategoryA            int64
	CategoryB            int64
	CategoryC            int64
	CategoryD            int64
	RebindCount          map[string]int64
	Iterations           int
	IterationsCapped     bool
	DurationMs           int64
}

// BackfillSlackUserIDOptions controls one run of `kd admin
// backfill-slack-user-id` (ka-0000001582). It backfills the missing
// `fields.slack_user_id` on thread-derived legacy beads by resolving the
// originating Slack thread's user id from the root "Slack thread:" bead
// created by the slack-bridge. No Slack API call is required: the root
// bead already stores slack_user_id at thread-open time, and every
// thread-derived bead references its root either via `fields.thread_id`
// (a bead-id FK) or via `created_by = "thread-<sanitized-ts>"` (the
// slack_thread_ts with '.' replaced by '-').
//
// Statuses limits the scan (defaults to {"open"}). BatchSize bounds each
// per-batch transaction (defaults to 200). DryRun reports the would-be
// writes without mutating.
type BackfillSlackUserIDOptions struct {
	Statuses  []string
	BatchSize int
	DryRun    bool
}

// Validate fills defaults and rejects a negative BatchSize.
func (o *BackfillSlackUserIDOptions) Validate() error {
	if o.BatchSize < 0 {
		return errBackfillSlackUserIDBatchInvalid
	}
	if len(o.Statuses) == 0 {
		o.Statuses = []string{"open"}
	}
	if o.BatchSize == 0 {
		o.BatchSize = 200
	}
	return nil
}

// BackfillSlackUserIDStats summarizes one run of the slack_user_id
// backfill. BeforeMissing / AfterMissing bracket the count of candidate
// beads still lacking slack_user_id (in DryRun, AfterMissing == BeforeMissing
// since nothing is written). ResolvedViaThreadID / ResolvedViaCreatedBy tally
// which resolution strategy supplied each write. Unresolved counts candidates
// whose root bead could not be found or whose root lacked slack_user_id.
type BackfillSlackUserIDStats struct {
	DryRun               bool
	BeforeMissing        int64
	AfterMissing         int64
	Updated              int64
	ResolvedViaThreadID  int64
	ResolvedViaCreatedBy int64
	Unresolved           int64
	DurationMs           int64
}

// BackfillOperatorSlugsOptions controls one run of `kd admin
// backfill-operator-slugs` (ka-0000001149). It recomputes readable slugs for
// operators that are still on the opaque `op-<id>` form but now carry a real
// (non-sentinel) email — the human survivors of the operator-cluster merge
// cleanup (ka-0000006421), whose oidc/resync email lets us derive a memorable
// `matthew.baker`-style slug.
//
// The scan is intentionally kind-agnostic: it keys off the email, not the
// operator kind, so a slack/gitlab identity whose real email was harvested by
// the resync jobs still gets a readable slug. Rows whose email is still the
// synthetic sentinel (system:*, fleet:*, un-resynced slack) fail the
// readable-slug derivation and are left on op-<id> — exactly the machine /
// sentinel rows the task says to leave alone.
//
// Old slugs are preserved as retired operator_slug_aliases rows so existing
// /u/op-NNN links keep resolving to the same operator after the rename.
//
// BatchSize bounds each per-batch transaction (defaults to 200). DryRun
// reports the would-be renames without mutating.
type BackfillOperatorSlugsOptions struct {
	BatchSize int
	DryRun    bool
}

// Validate fills defaults and rejects a negative BatchSize.
func (o *BackfillOperatorSlugsOptions) Validate() error {
	if o.BatchSize < 0 {
		return errBackfillOperatorSlugsBatchInvalid
	}
	if o.BatchSize == 0 {
		o.BatchSize = 200
	}
	return nil
}

// BackfillOperatorSlugsStats summarizes one run of the op-<id> slug backfill.
// BeforeOpaque / AfterOpaque bracket the count of operators still on an
// op-<id> slug (in DryRun, AfterOpaque == BeforeOpaque since nothing is
// written). Renamed counts operators whose slug moved to a readable form.
// SkippedCollision counts candidates whose readable slug was already claimed
// by another operator (left on op-<id>). Machine counts op-<id> rows with no
// derivable readable slug (sentinel-email rows — left as-is).
type BackfillOperatorSlugsStats struct {
	DryRun           bool
	BeforeOpaque     int64
	AfterOpaque      int64
	Renamed          int64
	SkippedCollision int64
	Machine          int64
	DurationMs       int64
}

// BackfillParentChildDepsOptions controls one run of `kd admin
// backfill-parent-child-deps` (ka-0000006775). Before the server-side
// auto-parent inheritance (ka-0000007224) shipped, every agent-authored
// bead was born orphaned. This backfill reconstructs the missing
// parent-child edge by joining each candidate bead to its author's
// agent bead and reading fields.task_id.
//
// Scope is "agent-authored beads created within the last Since duration
// OR currently closed", per the bead description. Defaults: Since =
// 6 months, BatchSize = 500, IncludeClosed = true. DryRun reports the
// would-be edges grouped by author without writing.
type BackfillParentChildDepsOptions struct {
	// Since limits the scan to beads created within the last Since
	// duration. Defaults to 6 months (180 days) when ≤ 0.
	Since time.Duration
	// IncludeClosed widens the scope to include all closed beads
	// regardless of age. Defaults to true.
	IncludeClosed bool
	// BatchSize bounds each scan page; each batch is its own
	// transaction. Defaults to 500 when ≤ 0.
	BatchSize int
	// MaxBatches caps the number of pages processed in one call.
	// 0 means "no cap" (drain the queue). Useful for canary runs.
	MaxBatches int
	// DryRun reports the would-be edges without writing.
	DryRun bool
}

// Validate fills defaults and rejects malformed options.
func (o *BackfillParentChildDepsOptions) Validate() error {
	if o.Since < 0 {
		return errBackfillParentChildSinceInvalid
	}
	if o.Since == 0 {
		// 6 months ≈ 180 days. Calendars vary; calendar-accurate math is
		// not worth the complexity for a one-shot backfill.
		o.Since = 180 * 24 * time.Hour
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 500
	}
	return nil
}

// BackfillParentChildDepsStats summarizes one run. CandidateBeads is the
// total number of agent-authored beads scanned (passed all the cheap
// filters: created_by present, within window, no existing parent-child
// dep, description does not start with `parent(gd-…)`). EdgesAdded is
// the number of parent-child deps that were inserted (or, in DryRun, that
// would have been). EdgesByAuthor groups EdgesAdded by created_by (agent
// slug) so operators can spot-check which agents account for which slice
// of the backfill. Resolution failures are tallied so dry-runs surface
// the long tail: SkippedNoAgent (created_by did not match any agent
// bead), SkippedNoTaskID (agent bead has no fields.task_id),
// SkippedSelfParent (task_id == bead.id), SkippedTaskMissing (referenced
// task_id does not exist or is soft-deleted), SkippedExistingDep (parent
// edge appeared between scan and write), SkippedParentMention
// (description begins with parent(gd-…) — already intentionally parented).
type BackfillParentChildDepsStats struct {
	DryRun               bool
	CandidateBeads       int64
	EdgesAdded           int64
	EdgesByAuthor        map[string]int64
	SkippedNoAgent       int64
	SkippedNoTaskID      int64
	SkippedSelfParent    int64
	SkippedTaskMissing   int64
	SkippedExistingDep   int64
	SkippedParentMention int64
	Batches              int
	BatchesCapped        bool
	LastID               string
	DurationMs           int64
}

// errRepairReversedParentChildBatchInvalid is returned when BatchSize is
// supplied as a negative value to RepairReversedParentChildDeps. Zero /
// unset falls back to the 500-row default.
var errRepairReversedParentChildBatchInvalid = errors.New("RepairReversedParentChildDeps: BatchSize must be >= 0")

// RepairReversedParentChildDepsOptions controls one run of
// `kd admin repair-reversed-parent-child-deps` (ka-0000006880).
//
// The audit scans the deps table for parent-child rows stored in the
// REVERSED direction — bead_id resolves to a type=epic bead and
// depends_on_id resolves to a NON-epic bead — which the ParentSubtreeID
// CTE (seeded from `depends_on_id = <root>`) cannot see, silently
// breaking subtree counts and gardener preflights. The canonical
// direction is bead_id = CHILD, depends_on_id = PARENT.
//
// DryRun (the default at the CLI) reports the reversed rows without
// writing. When DryRun is false each reversed row is flipped: the row is
// rewritten so bead_id=child, depends_on_id=parent. The flip is
// idempotent — a re-run finds nothing to fix because the canonical row
// no longer matches the epic-on-bead_id predicate.
type RepairReversedParentChildDepsOptions struct {
	// BatchSize bounds each scan+repair page; each batch is its own
	// transaction. Defaults to 500 when ≤ 0.
	BatchSize int
	// MaxBatches caps the number of pages processed in one call.
	// 0 means "no cap" (drain the queue).
	MaxBatches int
	// DryRun reports the reversed rows without flipping them.
	DryRun bool
}

// Validate fills defaults and rejects malformed options.
func (o *RepairReversedParentChildDepsOptions) Validate() error {
	if o.BatchSize < 0 {
		return errRepairReversedParentChildBatchInvalid
	}
	if o.BatchSize == 0 {
		o.BatchSize = 500
	}
	return nil
}

// ReversedParentChildEdge is one reversed parent-child row found by the
// audit: EpicID is the bead currently on the bead_id (child) side, and
// ChildID is the non-epic bead currently on the depends_on_id (parent)
// side. ChildType is the resolved type of ChildID (for the report).
type ReversedParentChildEdge struct {
	EpicID    string `json:"epic_id"`
	ChildID   string `json:"child_id"`
	ChildType string `json:"child_type"`
	CreatedBy string `json:"created_by,omitempty"`
}

// RepairReversedParentChildDepsStats summarizes one run.
// ReversedRows is the number of reversed parent-child rows found.
// EdgesFlipped is the number actually rewritten (0 in DryRun, or when a
// canonical row already exists so the flip would collide and is skipped).
// SkippedCanonicalExists counts reversed rows whose canonical twin
// (bead_id=child, depends_on_id=epic) already exists — those are deleted
// rather than flipped to avoid a PK collision, and counted here.
type RepairReversedParentChildDepsStats struct {
	DryRun                 bool                      `json:"dry_run"`
	ReversedRows           int64                     `json:"reversed_rows"`
	EdgesFlipped           int64                     `json:"edges_flipped"`
	SkippedCanonicalExists int64                     `json:"skipped_canonical_exists"`
	Edges                  []ReversedParentChildEdge `json:"edges,omitempty"`
	Batches                int                       `json:"batches"`
	BatchesCapped          bool                      `json:"batches_capped,omitempty"`
	DurationMs             int64                     `json:"duration_ms"`
}

// BackfillTriggeredByDepsOptions controls one run of `kd admin
// backfill-triggered-by-deps` (ka-0000000004) — the auto-link M4
// scanner from design doc ka-0000000827.
//
// The scanner walks recently-created beads (default 24h window) that
// have no incoming triggered-by dep AND no parent-child dep, and tries
// to recover the originating bead via conservative signals lifted from
// the bead's fields:
//
//   - fields.slack_thread_bead_id — Slack-thread spawn writes the
//     thread bead ID directly here (bot_mentions.go).
//   - fields.trigger_id           — trigger-engine spawn writes the
//     trigger bead ID here (triggers.go).
//
// Both signal fields hold canonical bead IDs at write time so there is
// no ambiguity to resolve — when set and pointing at a live bead, the
// scanner writes triggered-by. When both fields are set and point at
// different beads, the scanner records an ambiguous skip (conservative
// per acceptance criteria).
//
// Append-only: never modifies existing deps. Idempotent via the
// standard ON CONFLICT … DO NOTHING dep insert path. DryRun reports
// would-be edges grouped by signal without writing.
type BackfillTriggeredByDepsOptions struct {
	// Since limits the scan to beads created within the last Since
	// duration. Defaults to 24h when ≤ 0 (per design doc ka-0000000827
	// M4/D — rolling window, not full history).
	Since time.Duration
	// BatchSize bounds each scan page; each batch is its own
	// transaction. Defaults to 500 when ≤ 0.
	BatchSize int
	// MaxBatches caps the number of pages processed in one call.
	// 0 means "no cap" (drain the queue). Useful for canary runs.
	MaxBatches int
	// DryRun reports the would-be edges without writing.
	DryRun bool
}

// Validate fills defaults and rejects malformed options.
func (o *BackfillTriggeredByDepsOptions) Validate() error {
	if o.Since < 0 {
		return errBackfillTriggeredBySinceInvalid
	}
	if o.Since == 0 {
		o.Since = 24 * time.Hour
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 500
	}
	return nil
}

// BackfillTriggeredByDepsStats summarizes one M4 backfill run.
// EdgesAdded counts triggered-by deps inserted (or would-be in DryRun).
// EdgesBySignal groups EdgesAdded by which field carried the recovery
// signal ("slack_thread_bead_id" or "trigger_id") so operators can see
// which path is driving the writes.
//
// Skip tallies — by acceptance criteria the scanner must surface why it
// did not write a dep:
//   - SkippedExistingDep    — bead already has any parent-child or
//     triggered-by dep (regression guard per task description).
//   - SkippedNoSignal       — neither signal field is set / both empty.
//   - SkippedAmbiguous      — two distinct signals disagree
//     (e.g. slack_thread_bead_id != trigger_id, both set).
//   - SkippedSelfReference  — signal points at the bead itself.
//   - SkippedTargetMissing  — signal points at a soft-deleted / missing bead.
type BackfillTriggeredByDepsStats struct {
	DryRun               bool
	CandidateBeads       int64
	EdgesAdded           int64
	EdgesBySignal        map[string]int64
	SkippedExistingDep   int64
	SkippedNoSignal      int64
	SkippedAmbiguous     int64
	SkippedSelfReference int64
	SkippedTargetMissing int64
	Batches              int
	BatchesCapped        bool
	LastID               string
	DurationMs           int64
}

// CloseTriggerAuditBeadsOptions controls one run of `kd admin
// close-trigger-audit-beads` (ka-0000006341). The forward-fix in
// ka-0000004100 made recordTriggerAudit create new audit-event beads with
// status=closed, but ~226k legacy rows are still status=open. They are
// append-only ledger records (type=event, label=auto:trigger-audit) —
// closing them removes a large chunk of phantom open beads from
// ready/news queries and the beads-viewer counts.
type CloseTriggerAuditBeadsOptions struct {
	// BatchSize bounds each scan/update page; each batch is its own
	// transaction. Defaults to 500 when ≤ 0.
	BatchSize int
	// MaxBatches caps the number of pages processed in one call.
	// 0 means "no cap" (drain the queue). Useful for canary runs.
	MaxBatches int
	// DryRun reports the would-be closes without writing.
	DryRun bool
}

// Validate fills defaults and rejects malformed options.
func (o *CloseTriggerAuditBeadsOptions) Validate() error {
	if o.BatchSize < 0 {
		return errCloseTriggerAuditBatchSize
	}
	if o.BatchSize == 0 {
		o.BatchSize = 500
	}
	return nil
}

// CloseTriggerAuditBeadsStats summarizes one run.
type CloseTriggerAuditBeadsStats struct {
	DryRun         bool
	CandidateBeads int64
	BeadsClosed    int64
	Batches        int
	BatchesCapped  bool
	LastID         string
	DurationMs     int64
}

// errBackfillOwnerTargetRequired is returned when --target is empty.
var errBackfillOwnerTargetRequired = errors.New("BackfillOwner: Target is required (e.g. slack:U0OPERATOR1)")

// errBackfillOwnerBatchSizeInvalid is returned when --batch-size is negative.
var errBackfillOwnerBatchSizeInvalid = errors.New("BackfillOwner: BatchSize must be >= 0")

// BackfillOwnerOptions controls one run of `kd admin backfill-owner
// --rebind-matt-default` (ka-0000006961 / C4 of epic ka-0000004247).
// Target is the "<kind>:<subject>" owner sentinel — typically
// "slack:U0OPERATOR1" (Matt) — that the ka-0000000269 verbatim-default
// stamped onto every spawn-path bead. Since limits the scan to beads
// created at-or-after ka-0000000269 landed; pre-existing Matt-owned beads
// from older code paths are out of scope. UnownedSentinel is the owner
// value to write when the spawn-chain walk yields no human; defaults to
// "system:unowned".
type BackfillOwnerOptions struct {
	// Target is the current owner value to rebind from (e.g.
	// "slack:U0OPERATOR1"). Required.
	Target string
	// Since limits the scan to beads created at-or-after this timestamp.
	// Zero means no lower bound; the CLI defaults this to the ka-0000000269
	// deploy date (2026-05-13 18:14:39 UTC).
	Since time.Time
	// BatchSize bounds each scan page; each batch is its own transaction.
	// Defaults to 5000 when ≤ 0.
	BatchSize int
	// MaxBatches caps the number of pages processed in one call. 0 means
	// "no cap" — drain the queue.
	MaxBatches int
	// UnownedSentinel is the owner value to write when the spawn-chain
	// walk does not yield a human identity. Defaults to "system:unowned".
	UnownedSentinel string
	// DryRun reports the would-be rebinds without writing.
	DryRun bool
}

// Validate fills defaults and rejects malformed options.
func (o *BackfillOwnerOptions) Validate() error {
	if o.Target == "" {
		return errBackfillOwnerTargetRequired
	}
	if o.BatchSize < 0 {
		return errBackfillOwnerBatchSizeInvalid
	}
	if o.BatchSize == 0 {
		o.BatchSize = 5000
	}
	if o.UnownedSentinel == "" {
		o.UnownedSentinel = "system:unowned"
	}
	return nil
}

// BackfillOwnerStats summarizes one run of the Matt-default owner rebind.
// CandidateBeads is the total count of rows scanned (had owner=Target,
// created_at >= Since, not soft-deleted). RebindCount keys are the new
// owner value (e.g. "slack:U02ABC", "oidc:alice@example.com",
// "system:unowned"); the value is the number of beads rebound to that
// owner. KeptMattCount is the number of beads whose spawn-chain root
// genuinely resolved to Target — those rows are left alone. The
// per-bucket histogram is what the dry-run produces (acceptance:
// histogram of `(current owner=Matt) → (would-rebind-to)` buckets).
type BackfillOwnerStats struct {
	DryRun           bool
	Target           string
	Since            time.Time
	BeforeCandidates int64
	AfterCandidates  int64
	CandidateBeads   int64
	RebindCount      map[string]int64
	KeptMattCount    int64
	UnresolvedCount  int64
	Batches          int
	BatchesCapped    bool
	LastID           string
	DurationMs       int64
}

// errBackfillEmptyOwnerBatchSizeInvalid is returned when --batch-size is negative.
var errBackfillEmptyOwnerBatchSizeInvalid = errors.New("BackfillEmptyOwner: BatchSize must be >= 0")

// errBackfillEmptyOwnerOperatorRequired is returned when a non-dry-run is
// invoked without a resolved OperatedByID. Migration 026's NOT-NULL CHECK on
// mutation_history.operated_by_id would otherwise 500 on the first write.
var errBackfillEmptyOwnerOperatorRequired = errors.New("BackfillEmptyOwner: OperatedByID must be > 0 for a non-dry-run")

// defaultLegacyOperatedBySentinel is the operated_by value that predates
// human attribution; a row carrying it is not a usable Owner source, so
// Phase 1 excludes it (alongside the empty string).
const defaultLegacyOperatedBySentinel = "system:legacy-pre-attribution"

// defaultUnownedSentinel is the Phase-3 residual owner value — beads with
// no usable operated_by or created_by are tagged with it so the viewer
// renders "explicitly unowned" distinctly from "default not yet applied".
const defaultUnownedSentinel = "system:unowned"

// defaultEmptyOwnerPhase2Exclusions are the created_by sentinels that are
// pure system/bridge actors, not meaningful human/agent owners. Phase 2
// skips them so they fall through to the Phase-3 system:unowned tag rather
// than being stamped as an Owner. Matches the report's exclusion list
// (gardenio/docs/research/ownership-defaults.md §Recommended backfill
// sequence: "jira-bridge", "system:scheduler", …).
var defaultEmptyOwnerPhase2Exclusions = []string{
	"jira-bridge",
	"system:scheduler",
	"static",
}

// BackfillEmptyOwnerOptions controls one run of `kd admin
// backfill-empty-owner` (ka-0000007029). See
// gardenio/docs/research/ownership-defaults.md §Recommended backfill
// sequence.
//
// This is the complement of BackfillOwner: where BackfillOwner rebinds an
// already-set (wrong) owner, this fills rows whose `owner` is still empty
// (`”` or NULL) after the ka-0000000269 server-side default landed. It
// runs three sequential phases, each a chunked `UPDATE … LIMIT BatchSize`
// loop that runs until zero rows change:
//
//	Phase 1 — owner ← the resolved operator identity
//	          (operators.kind||':'||subject via beads.operated_by_id;
//	          the legacy `operated_by` TEXT column was dropped in migration
//	          024), excluding the legacy-pre-attribution sentinel.
//	Phase 2 — owner ← created_by (legacy-sentinel rows with a usable
//	          created_by; excludes pure system/bridge created_by values).
//	Phase 3 — owner ← UnownedSentinel for whatever residual remains.
//
// Idempotent: every UPDATE filters `COALESCE(owner,”) = ”`, so a re-run
// on a fully-drained set is a no-op. Each batch is its own transaction per
// kbeads/CLAUDE.md §Migration policy (no eager full-table updates).
type BackfillEmptyOwnerOptions struct {
	// BatchSize bounds each phase's UPDATE; each batch is its own
	// transaction. Defaults to 5000 when ≤ 0.
	BatchSize int
	// MaxBatches caps the number of batch iterations PER PHASE. 0 means
	// "no cap" — drain each phase fully.
	MaxBatches int
	// UnownedSentinel is the Phase-3 residual owner value. Defaults to
	// "system:unowned".
	UnownedSentinel string
	// LegacyOperatedBySentinel is the resolved operator identity Phase 1
	// excludes (rows whose operators.kind||':'||subject equals it were
	// sentinel-bound by migration 022 and carry no real operator). Defaults
	// to "system:legacy-pre-attribution".
	LegacyOperatedBySentinel string
	// Phase2Exclusions are created_by values Phase 2 skips (pure
	// system/bridge actors). Defaults to defaultEmptyOwnerPhase2Exclusions.
	Phase2Exclusions []string
	// OperatedByID is the resolved operators(id) FK stamped on every
	// mutation_history row this run writes. Migration 026 added a NOT-NULL
	// CHECK constraint on mutation_history.operated_by_id, so a non-dry-run
	// MUST carry a non-zero value here or every phase-1 batch 500s on the
	// constraint. The server handler resolves it fail-closed via
	// resolveAuditOperatorRequired before calling. Ignored in a dry-run
	// (no rows are written).
	OperatedByID int64
	// DryRun partitions the current owner='' set into per-phase would-be
	// counts without writing.
	DryRun bool
}

// Validate fills defaults and rejects malformed options.
func (o *BackfillEmptyOwnerOptions) Validate() error {
	if o.BatchSize < 0 {
		return errBackfillEmptyOwnerBatchSizeInvalid
	}
	if o.BatchSize == 0 {
		o.BatchSize = 5000
	}
	if !o.DryRun && o.OperatedByID <= 0 {
		return errBackfillEmptyOwnerOperatorRequired
	}
	if o.UnownedSentinel == "" {
		o.UnownedSentinel = defaultUnownedSentinel
	}
	if o.LegacyOperatedBySentinel == "" {
		o.LegacyOperatedBySentinel = defaultLegacyOperatedBySentinel
	}
	if o.Phase2Exclusions == nil {
		o.Phase2Exclusions = defaultEmptyOwnerPhase2Exclusions
	}
	return nil
}

// BackfillEmptyOwnerStats summarizes one run of the three-phase empty-owner
// backfill. Phase{1,2,3}Filled are the number of rows whose owner was set
// by that phase (in a dry-run: the number that WOULD be set, computed by
// partitioning the current owner=” set so the three counts + AfterEmpty
// reconstruct BeforeEmpty). Phase{1,2,3}Batches count the UPDATE iterations
// each phase ran (always 0 in a dry-run). BatchesCapped is set when any
// phase hit MaxBatches before draining.
type BackfillEmptyOwnerStats struct {
	DryRun        bool
	BeforeEmpty   int64
	AfterEmpty    int64
	Phase1Filled  int64
	Phase2Filled  int64
	Phase3Filled  int64
	Phase1Batches int
	Phase2Batches int
	Phase3Batches int
	BatchesCapped bool
	DurationMs    int64
}

// errBackfillOperatedByAgentsBatchSizeInvalid is returned when --batch-size is negative.
var errBackfillOperatedByAgentsBatchSizeInvalid = errors.New("BackfillOperatedByAgents: BatchSize must be >= 0")

// BackfillOperatedByAgentsOptions controls one run of
// `kd admin backfill-operated-by-agents` — sub-item #1 of ka-0000006817
// (per-user homepage M2). Scans `type=agent` beads whose operated_by_id is
// still unset (NULL on legacy envs, or bound to the
// `system:legacy-pre-attribution` sentinel on envs that passed through
// migration 022) and resolves the owning human via the agent bead's own
// fields (slack_user_id / gitlab_username / jira_user_id / operated_by
// text) → operator_aliases lookup.
//
// Idempotent. Per-batch transactions per kbeads/CLAUDE.md §Migration
// policy. Skipped rows (no resolvable signal) are counted, not bound to
// a fallback — the goal here is to surface signal-bearing rows onto the
// per-user homepage; sentinel-bound rows that genuinely have no human
// stay where they are.
type BackfillOperatedByAgentsOptions struct {
	// BatchSize bounds each scan page; each batch is its own transaction.
	// Defaults to 500 when ≤ 0.
	BatchSize int
	// MaxBatches caps the number of pages processed in one call. 0 means
	// "no cap" — drain the queue.
	MaxBatches int
	// IncludeSentinel widens the scan to also include rows currently
	// bound to the `system:legacy-pre-attribution` sentinel. Defaults
	// to true — production envs land legacy rows on the sentinel via
	// migration 022, so the natural target set is `IS NULL OR =
	// sentinel`.
	IncludeSentinel bool
	// DryRun reports the would-be rebinds without writing.
	DryRun bool
}

// Validate fills defaults and rejects malformed options.
func (o *BackfillOperatedByAgentsOptions) Validate() error {
	if o.BatchSize < 0 {
		return errBackfillOperatedByAgentsBatchSizeInvalid
	}
	if o.BatchSize == 0 {
		o.BatchSize = 500
	}
	return nil
}

// BackfillOperatedByAgentsStats summarizes one run. Processed counts
// every agent bead scanned; Updated counts rows actually rebound to a
// resolved human operator; Skipped counts rows where no resolvable
// signal was found. RebindBySignal groups Updated by which field
// carried the resolution signal so operators can see which path is
// driving the writes ("slack_user_id" / "gitlab_username" /
// "jira_user_id" / "operated_by").
type BackfillOperatedByAgentsStats struct {
	DryRun         bool
	Processed      int64
	Updated        int64
	Skipped        int64
	RebindBySignal map[string]int64
	Batches        int
	BatchesCapped  bool
	LastID         string
	DurationMs     int64
}

// AcceptanceMetrics carries the C5 acceptance signals
// (ka-0000004365): the share of last-24h beads owned by Matt
// (canonicalized across his slack:/oidc: identities), the count of
// last-1h audit writes whose operator_id points at an agent-shape
// operator row, the count of last-1h audit writes that fell back to a
// fleet sentinel (ka-0000005200 — this MUST NOT be silently excluded
// from health), and the share of `type=user` beads whose slack_id /
// email pair is fully indexed in operator_aliases under a single
// operator id. These numbers feed the gardener bead-query preflight
// gate that decides whether the operated_by multi-user readiness work
// (epic ka-0000004247) is done.
type AcceptanceMetrics struct {
	OwnerShareMatt            AcceptanceOwnerShare        `json:"owner_share_matt"`
	AgentOperatorLeaks        AcceptanceOperatorLeaks     `json:"agent_operator_leaks"`
	FleetFallbackCount        AcceptanceFleetFallback     `json:"fleet_fallback_count"`
	OperatorAliasCoverage     AcceptanceAliasCoverage     `json:"operator_alias_coverage"`
	OperatorCanonicalCoverage AcceptanceCanonicalCoverage `json:"operator_canonical_coverage"`
}

// AcceptanceOwnerShare reports the fraction of beads created in the
// last 24h whose owner resolves to Matt. "Resolves to Matt" is
// identity-canonical (ka-0000005200): the owner's "<kind>:<subject>"
// string is matched against every operator_aliases row belonging to
// the operator(s) that own Matt's canonical slack:U0OPERATOR1 /
// oidc:matthew.baker@acme.example identities, so the same human counts
// whether he was stamped in slack: or oidc: form. Pre-fix this
// fraction sat above 50% because the resolver stamped Matt onto every
// bead it could not attribute; the acceptance target is "Matt's
// natural share" (<10%).
type AcceptanceOwnerShare struct {
	Total    int     `json:"total"`
	Matt     int     `json:"matt"`
	Fraction float64 `json:"fraction"`
}

// AcceptanceOperatorLeaks counts mutation_history rows in the last 1h
// whose operated_by_id resolves to an operators row that is neither a
// well-formed human (slack:U…, oidc:…@…) nor a sentinel (system:…,
// fleet:…). Steady-state value MUST be 0 per the ka-0000006809
// Invariant. Fleet-sentinel writes are counted separately in
// AcceptanceFleetFallback rather than folded in here — a fleet
// fallback is a distinct resolver regression (a lost human), not a
// malformed agent-shape operator.
type AcceptanceOperatorLeaks struct {
	Count int `json:"count"`
}

// AcceptanceFleetFallback counts mutation_history rows in the last 1h
// whose operated_by_id resolves to an operators row with kind='fleet'
// (ka-0000005200). The fleet sentinel is written when an autonomous or
// respawn path loses the human on the way to the daemon; every such
// write is a resolver regression that the spawn-propagation fixes
// (epic ka-0000000095) must drive to zero. queryAgentOperatorLeaks
// deliberately excludes fleet/system from its leak count, which left
// fleet fallbacks invisible to monitoring — this signal makes them
// visible without conflating the two failure modes. Steady-state value
// MUST be 0.
type AcceptanceFleetFallback struct {
	Count int `json:"count"`
}

// AcceptanceAliasCoverage reports the share of `type=user` beads with
// both fields.slack_id and fields.email populated whose two
// identities are linked in operator_aliases under a single operator
// id. Matched = exactly one operator row owns both aliases. The
// acceptance target is 100%.
type AcceptanceAliasCoverage struct {
	Total    int     `json:"total"`
	Matched  int     `json:"matched"`
	Fraction float64 `json:"fraction"`
}

// AcceptanceCanonicalCoverage is the daemon analogue of the C5
// alias-coverage metric (ka-0000002687): it proves the "one human is one
// operator" invariant holds at the operators-ROW level, not just the
// alias level. operator_aliases can unify slack:U… and oidc:<email> under
// one operator_id, but a DUPLICATE operators row that shares the same
// human email still splits that person's beads across two operated_by_id
// values — the exact fragmentation this task ends (Matt was 250 beads on a
// slack row + 221 on an oidc row).
//
// HumanEmails counts distinct real human emails; OperatorRows counts the
// operators rows carrying them. Sentinel addresses (@noreply.kbeads.local)
// and forgotten operators are excluded, and email is lower-cased so a case
// variant is not miscounted as a second human. When every human owns
// exactly one row HumanEmails == OperatorRows, Fragmented == 0, and
// Fraction == 1.0 (the acceptance target); Fragmented > 0 is the count of
// surplus rows a MergeOperators consolidation must still collapse.
type AcceptanceCanonicalCoverage struct {
	HumanEmails  int     `json:"human_emails"`
	OperatorRows int     `json:"operator_rows"`
	Fragmented   int     `json:"fragmented"`
	Fraction     float64 `json:"fraction"`
}

// FleetTypeStatusKey identifies a (type, status) bucket in fleet rollup output.
// OperatorAliasKey is the (kind, subject) primary key of an operator_aliases row.
type OperatorAliasKey struct {
	Kind    string
	Subject string
}

// OperatorAliasIdentity is one row of the slack-alias resync feed: an alias
// row joined with its parent operator's current display/email. The Slack
// users.info job calls slack.GetUserInfo(subject), then writes display/email
// back via UpdateOperatorIdentity when the values change.
type OperatorAliasIdentity struct {
	OperatorID int64
	Subject    string
	Display    string
	Email      string
}

// OperatorDirectoryEntry is one row of the public operator directory feed
// (M2 §4 /people, ka-0000000340). Only operators that have a slug appear in
// the directory; sentinel/system operators without a slug are excluded.
type OperatorDirectoryEntry struct {
	ID      int64
	Kind    string
	Subject string
	Display string
	Email   string
	Slug    string
}

// OperatorStat is one operator's real aggregate for the /people directory,
// computed by a GROUP BY operated_by_id over the whole beads table — NOT an
// in-memory sample (ka-0000001390). OpenBeads counts open/in_progress beads
// attributed to the operator; LastActive is the max(updated_at) across ALL
// statuses so an operator whose work has all closed still sorts by genuine
// recency.
//
// Alias-awareness: operated_by_id is the canonical operator the write-path
// resolver bound through operator_aliases, so every (kind, subject) identity
// that has been linked to a human folds into one row here. After a
// MergeOperators consolidation the displaced rows' beads are repointed to the
// survivor, so the survivor's count is the human's true total — not the
// pre-merge survivor-only slice. Identities that are NOT yet linked still
// surface as separate operator ids; the beads-viewer's render-time group fold
// (peopleIdentityKey) sums those together for display.
//
// The per-operator counts below (InProgress … GardenersActive) are computed in
// the same single GROUP BY operated_by_id pass as OpenBeads via COUNT(*)
// FILTER columns (design §4.1, ka-0000002003) — no extra query, so /people
// stays at 3 bounded queries total. All are additive; OpenBeads/LastActive are
// unchanged. The 7d windows are evaluated server-side (now() - interval '7
// days') so the counts are consistent regardless of viewer clock.
type OperatorStat struct {
	OperatorID int64
	OpenBeads  int
	// InProgress is the in_progress-only slice of OpenBeads (actively-touched
	// vs merely-open).
	InProgress int
	// Created7d / Closed7d are throughput: beads created in the last 7 days,
	// and beads closed (status='closed' AND closed_at) in the last 7 days.
	Created7d int
	Closed7d  int
	// MRsOpen / MRsMerged7d count type='merge-request' beads: open/in_progress
	// now, and closed (merged) within the last 7 days.
	MRsOpen     int
	MRsMerged7d int
	// DecisionsPending counts open type='decision' beads attributed to the
	// operator — "is someone waiting on this person?".
	DecisionsPending int
	// AgentsActive / GardenersActive count active (open/in_progress) fleet
	// footprint owned by the operator: type='agent' and type='trigger'
	// respectively.
	AgentsActive    int
	GardenersActive int
	// LastActive is the most recent updated_at across the operator's
	// HUMAN-authored beads — type='agent'/'session'/'trigger' are excluded
	// because that machine scaffolding heartbeats on its own schedule and
	// would report a person as active when they did nothing (ka-0000001991).
	// Zero when the operator has no human-authored activity at all.
	LastActive time.Time
}

// OperatorRecentBead is one entry in an operator's "recently created" chip
// list, keyed by operated_by_id — the column the write path actually
// populates. (created_by_operator_id is the semantically nicer "immortal
// originator", but only user-provisioning ever sets it, so keying on it
// returned one row fleet-wide — see the query comment in
// postgres/operators_stats.go.) Ordered most-recent-first within an operator
// by the query that produces it, and bounded to a recent window so a months-old
// bead cannot render as current work.
type OperatorRecentBead struct {
	OperatorID int64 // operated_by_id
	BeadID     string
	Title      string
	CreatedAt  time.Time
}

// OperatorDirectoryStats bundles the two per-operator aggregates the /people
// directory needs in one round trip: the GROUP BY open-count + last-active
// stats, and the bounded per-operator "recently created" chips.
type OperatorDirectoryStats struct {
	Stats  []OperatorStat
	Recent []OperatorRecentBead
}

// ResolvedOperator is the server-side identity resolution of one opaque
// operator_id metric label into a human-presentable identity. Produced by
// ResolveOperators and consumed by the chart backend's engagement / security
// dashboards (ka-0000004824). This is the SINGLE place an operator_id becomes
// a display name — metric series themselves NEVER carry email / display /
// slack_user_id (customer-1 telemetry plan §4 privacy allow-list).
//
// Right-to-be-forgotten (plan §4): when the operators row is soft-deleted
// (forgotten_at IS NOT NULL) the resolver redacts — Display is "" and Email is
// the sentinel "deleted operator" — while the opaque ID persists so historical
// series stay attributable to the (now anonymous) actor. Forgotten is true in
// that case so callers can render a redaction badge without string-matching.
type ResolvedOperator struct {
	ID        int64  `json:"operator_id"`
	Display   string `json:"display"`
	Email     string `json:"email"`
	Slug      string `json:"slug"`
	Forgotten bool   `json:"forgotten"`
}

// ForgottenOperatorEmail is the sentinel an operator's email resolves to once
// the operators row is soft-deleted for a right-to-be-forgotten request
// (plan §4). Kept here, not duplicated at call sites, so the chart backend and
// any future resolver agree on one redaction string.
const ForgottenOperatorEmail = "deleted operator"

// DefaultForgetOperatorAuditGuard is the audit-history row count above which
// ForgetOperator refuses to soft-delete an operator unless the caller passes
// Confirm=true. The soft-delete keeps the operator_id (historical metric
// series stay attributable), but redacting a principal that authored a large
// slice of the audit trail is a high-consequence action worth a deliberate
// second look — the guard turns a fat-fingered id into a loud refusal rather
// than a silent mass redaction. The four rows the 2026-06-16 raw-SQL cleanup
// forgot (op-264/297/321/356) were low-history junk identities, so a 1000-row
// default clears them while still catching a real, heavily-attributed human.
const DefaultForgetOperatorAuditGuard = 1000

// ForgetOperatorOptions parameterizes ForgetOperator.
type ForgetOperatorOptions struct {
	// Actor is the server-derived AuthIdentity subject performing the forget,
	// recorded as operators.forgotten_by. The store MUST be passed the
	// authenticated actor (never a client-supplied string) so the audit
	// attribution cannot be spoofed. Empty is allowed (auth disabled /
	// anonymous) and stored as NULL.
	Actor string
	// Reason is an optional free-text justification recorded as
	// operators.forgotten_reason (e.g. a ticket id or erasure-request ref).
	// Empty is stored as NULL.
	Reason string
	// Confirm bypasses the audit-history guard. When false and the operator
	// owns more than AuditGuardThreshold audit rows, ForgetOperator returns a
	// *OperatorForgetGuardError WITHOUT mutating; when true the forget proceeds
	// regardless of history size.
	Confirm bool
	// AuditGuardThreshold overrides DefaultForgetOperatorAuditGuard. <=0 falls
	// back to the default. A negative/zero value never disables the guard —
	// pass Confirm=true to force.
	AuditGuardThreshold int
}

// ForgetOperatorResult summarizes one ForgetOperator call.
type ForgetOperatorResult struct {
	ID int64 `json:"operator_id"`
	// AlreadyForgotten is true when the operator's forgotten_at was already
	// set on entry — the call was an idempotent no-op and ForgottenAt reflects
	// the ORIGINAL forget time (not re-stamped), SlugCleared is false, and
	// forgotten_by/reason are left untouched.
	AlreadyForgotten bool `json:"already_forgotten"`
	// ForgottenAt is the soft-delete timestamp (the freshly-stamped time on a
	// first forget, or the pre-existing time on an idempotent no-op).
	ForgottenAt time.Time `json:"forgotten_at"`
	// SlugCleared is true when this call nulled a previously non-null slug
	// (dropping the operator from the /people directory feed).
	SlugCleared bool `json:"slug_cleared"`
	// SlugAliasesRetired is the number of operator_slug_aliases rows this call
	// stamped retired_at on (previously-live aliases), so /u/{slug} still
	// resolves to the now-redacted operator instead of 404ing.
	SlugAliasesRetired int64 `json:"slug_aliases_retired"`
	// AuditRows is the total operated_by_id references this operator owned
	// across the five audit-FK tables (beads, comments, deps, events,
	// mutation_history) at forget time — the quantity the guard checks.
	AuditRows int64 `json:"audit_rows"`
}

// OperatorForgetGuardError is returned by ForgetOperator when the target
// operator owns more audit history than the threshold and Confirm was not set.
// It carries the measured row count and the threshold so the caller (the HTTP
// handler / CLI) can tell the operator exactly how much history is at stake and
// how to proceed (re-run with --confirm).
type OperatorForgetGuardError struct {
	OperatorID int64
	AuditRows  int64
	Threshold  int
}

func (e *OperatorForgetGuardError) Error() string {
	return fmt.Sprintf(
		"operator %d owns %d audit rows (threshold %d); pass Confirm/--confirm to forget anyway",
		e.OperatorID, e.AuditRows, e.Threshold)
}

// OperatorNotFoundError is returned by ForgetOperator when no operators row
// exists for the given id, so the caller can map it to a 404 rather than a
// generic 500.
type OperatorNotFoundError struct {
	OperatorID int64
}

func (e *OperatorNotFoundError) Error() string {
	return fmt.Sprintf("operator %d not found", e.OperatorID)
}

type FleetTypeStatusKey struct {
	Type   string
	Status string
}

// FleetQueueDepthKey identifies a (project, type) bucket in the queue-depth
// rollup. Project is the value parsed from the first `project:<value>` label on
// the bead, or "unknown" when no such label exists.
type FleetQueueDepthKey struct {
	Project string
	Type    string
}

// FleetQueueTypes is the closed set of operator-facing "queue" bead types whose
// open (undrained) depth is exposed as a per-project point-in-time gauge. These
// are the beads a human still has to act on: decisions to answer, escalations
// to grant, and mail to read. Fleet-wide open counts for these types are
// already available via gardenio_beads_total{type,status}; the queue-depth gauge
// adds the missing PROJECT dimension so agents can reason over their own
// project's backlog (ka-0000002235 / rescope ka-0000003130).
var FleetQueueTypes = []string{"decision", "escalation", "mail"}

// FleetAgentProjectRollup is one project's row in the agent lifetime rollup.
// Project is the value parsed from the first `project:<value>` label on a given
// agent bead, or "unknown" when no such label exists. TotalAgents counts every
// live agent bead, closed ones included. TotalCostUSD and SessionCount sum the
// `total_cost_usd` and `session_count` JSONB fields, treating non-numeric or
// missing values as zero.
type FleetAgentProjectRollup struct {
	Project      string
	TotalAgents  int
	TotalCostUSD float64
	SessionCount float64
}

// PurgeRequest selects the beads Store.PurgeBeads deletes: every bead
// carrying Label, or exactly IDs (one of the two), minus any of KeepTypes or
// KeepIDs. With KeepShared (Label only), a bead that carries a project: label
// other than Label is kept too: it is not that project's alone.
type PurgeRequest struct {
	Label      string
	IDs        []string
	KeepTypes  []string
	KeepIDs    []string
	KeepShared bool
	DryRun     bool
}

// PurgedBead is one bead Store.PurgeBeads selected.
type PurgedBead struct {
	ID        string
	Type      string
	CreatedAt time.Time
}

// Store defines the persistence interface for beads.
type Store interface {
	// Bead CRUD
	CreateBead(ctx context.Context, bead *model.Bead) error
	GetBead(ctx context.Context, id string) (*model.Bead, error)
	// GetBeadIncludingDeleted is GetBead but does not hide a soft-deleted
	// row (model.Bead.DeletedAt tells the caller which). Returns
	// sql.ErrNoRows only when no row exists for this id at all.
	GetBeadIncludingDeleted(ctx context.Context, id string) (*model.Bead, error)
	// FindAgentByName returns the most-recently-updated type=agent bead whose
	// fields.agent equals name, optionally restricted to the given statuses.
	// Returns sql.ErrNoRows when no match exists. Soft-deleted rows are
	// excluded. Backed by the partial expression index idx_beads_fields_agent
	// (migration 025). Powers GET /v1/agents-by-name/{name} so the controller
	// can resolve agent beads in O(log N) instead of pulling the full active
	// agent list and filtering client-side (ka-0000005291).
	FindAgentByName(ctx context.Context, name string, statuses []model.Status) (*model.Bead, error)
	// FindAgentsByNames is the batched form of FindAgentByName: it resolves
	// a set of agent names in a single round trip and returns one
	// most-recently-updated bead per matching name. Names with no matching
	// bead are silently omitted. Soft-deleted rows are excluded. Backed by
	// the same partial expression index idx_beads_fields_agent (migration
	// 025). Powers POST /v1/agents-by-name:batch so list pages in
	// beads-viewer can prewarm the per-request agent resolver and collapse
	// N per-row daemon round-trips into one (ka-0000000899).
	FindAgentsByNames(ctx context.Context, names []string, statuses []model.Status) ([]*model.Bead, error)
	// FindProjectByName resolves a type=project bead by name, matching either
	// the bare title or the legacy "Project: <name>" title (see
	// projectNameFromTitle in internal/server), mirroring the title-prefix
	// handling in beadsapi.ListProjectBeads. Ties (two project beads sharing a
	// name) go to a bead with a land_kind, then a frozen one (land_frozen),
	// then the lowest bead id (ListProjectBeads' deterministic-winner rule),
	// so a duplicate cannot shadow a land's kill switch. Returns sql.ErrNoRows when no project bead
	// matches. Soft-deleted rows are excluded. Backs per-project intake-taint
	// propagation (ADR docs/adr/taints.md §10.3).
	FindProjectByName(ctx context.Context, name string) (*model.Bead, error)
	ListBeads(ctx context.Context, filter model.BeadFilter) ([]*model.Bead, int, error) // returns beads, total count, error
	UpdateBead(ctx context.Context, bead *model.Bead) error
	// FillShortTitle writes bead.ShortTitle only while the stored short_title
	// is empty, in one conditional UPDATE, and returns false (no error, no
	// write) when the row already has one. No other column is written, so a
	// fill cannot revert a concurrent edit. It backs the short-title fill
	// ownership exception (gd-JOOXA7UqLh): the check that the title is empty
	// and the write are the same statement, so a title set between the
	// caller's read and this write is never overwritten. Sets
	// bead.UpdatedAt on success.
	FillShortTitle(ctx context.Context, bead *model.Bead) (bool, error)
	// LockBeadAssignee returns the bead's stored assignee and, inside a
	// transaction, row-locks the bead until the transaction ends (SELECT ...
	// FOR UPDATE). A lock-bead take compares it with the assignee it read
	// before writing, so of two runs taking the same lock only the first
	// wins (lock_bead.go, gd-SlLPKtUGWZ). Returns sql.ErrNoRows when no live
	// row has this id.
	LockBeadAssignee(ctx context.Context, id string) (string, error)
	// MergeBeadFields atomically merges the top-level keys of `extra` into a
	// bead's existing `fields` JSONB in a single `fields = COALESCE(fields,
	// '{}'::jsonb) || $patch::jsonb` UPDATE, replacing any prior read (Get) +
	// whole-object write (UpdateBead) sequence a caller might otherwise do to
	// patch a few fields. That read-then-write shape is a check-then-act race:
	// a second concurrent merge that read its snapshot before a first merge's
	// write lands, then writes back, silently clobbers whatever the first
	// merge added — including a claim flag another caller just set (gd-P6uCMzQqU9,
	// merge desk send-back on PR #169: this is exactly how a concurrent
	// scrub-request approve's fields merge could erase a just-taken
	// scrub_execution_claimed claim and let a second approve execute again).
	// A single UPDATE has no such window. Values in `extra` are marshaled to
	// JSON and merged shallow (top-level keys only, same as the old
	// read-modify-write's semantics) — a key set to a Go nil is stored as a
	// JSON null (present, not removed), matching prior behavior. Returns
	// sql.ErrNoRows if the bead does not exist.
	MergeBeadFields(ctx context.Context, id string, extra map[string]any) (*model.Bead, error)
	IncrementBeadFields(ctx context.Context, id string, deltas map[string]float64) (*model.Bead, error)
	// AllocateAgentHandleSeq atomically allocates and returns the next
	// per-gardener sequence number used to build a stable, monotonic
	// agent_handle (`<gardener>-<N>`). The counter is stored independently of
	// type=agent beads so pruning agents never rewinds it (ka-0000002870,
	// epic ka-0000006566 §4). Concurrent creates for the same gardener each get
	// a distinct, strictly increasing value.
	AllocateAgentHandleSeq(ctx context.Context, gardenerID string) (int64, error)
	// AppendBeadArrayField atomically appends `element` to the string-array
	// JSONB field `field` (e.g. phase_order / step_order) in a single UPDATE,
	// skipping the write if the element is already present (append-if-absent —
	// idempotent). The `changed` return is false on that no-op. Because two
	// concurrent appends each add a distinct element in their own statement,
	// neither can clobber the other — no read-modify-write window. Returns
	// sql.ErrNoRows if the bead does not exist, ErrInvalidFieldKey if `field`
	// is not a safe identifier. See ka-0000000988.
	AppendBeadArrayField(ctx context.Context, id, field, element string) (bead *model.Bead, changed bool, err error)
	// InsertBeadArrayField atomically inserts `element` at `index` in the
	// string-array JSONB field `field` (e.g. phase_order), shifting the tail
	// right — the POSITIONAL sibling of AppendBeadArrayField (which is always a
	// tail append). It is the "target cell" primitive: emplace a poured phase at
	// a chosen slot rather than only at the end (ka-0000001001). Like the append,
	// it is append-if-absent idempotent — if `element` is already present the
	// array is left untouched and `changed` is false (never moved to a new slot,
	// so a re-submit can't reorder an existing phase). index is clamped to
	// [0, len]; index==len (or any larger value) is exactly a tail append, so
	// InsertBeadArrayField(id, field, elem, len) == AppendBeadArrayField(id,
	// field, elem). The read-modify-write runs in ONE transaction under SELECT …
	// FOR UPDATE, so a concurrent append/insert on the same row blocks rather
	// than clobbering — same atomicity contract as MergeBeadMapField. Returns
	// sql.ErrNoRows if the bead does not exist, ErrInvalidFieldKey if `field` is
	// not a safe identifier. See ka-0000001001.
	InsertBeadArrayField(ctx context.Context, id, field, element string, index int) (bead *model.Bead, changed bool, err error)
	// CASBeadArrayField atomically sets the string-array JSONB field `field` to
	// `next`, but ONLY IF its current value equals `expected` (compare-and-swap).
	// This is the reorder primitive: a reorder is a blind full-array rewrite, so
	// under naive read-modify-write a concurrent append is silently lost. CAS
	// makes the rewrite conditional — on a stale `expected` it returns
	// ErrCASConflict and writes nothing, so the caller re-reads, re-permutes
	// against the fresh array, and retries. Returns sql.ErrNoRows if the bead
	// does not exist, ErrInvalidFieldKey if `field` is not a safe identifier.
	// See ka-0000000988.
	CASBeadArrayField(ctx context.Context, id, field string, expected, next []string) (*model.Bead, error)
	// MergeBeadMapField atomically sets or deletes ONE key of the JSON-map
	// field `field` (e.g. channel_roles) without disturbing the field's other
	// keys or the bead's other fields. This is the non-clobbering edit path for
	// a SHARED AGGREGATE map: a bare `-f field={…}` PATCH replaces the WHOLE
	// map, silently dropping keys other agents/operators set, whereas this edits
	// one key. op is "set" (fields[field][key]=value) or "unset-key" (delete the
	// key). The read-modify-write runs in one transaction under SELECT … FOR
	// UPDATE, so concurrent writers to DIFFERENT keys can never lose an update.
	// `changed` is false on an idempotent no-op (set to the current value, or
	// unset of an absent key). Returns sql.ErrNoRows if the bead does not exist,
	// ErrInvalidFieldKey if `field` is not a safe identifier, and
	// ErrInvalidSubFieldEdit for a bad op / empty key / non-map stored value.
	// See ka-0000007767.
	MergeBeadMapField(ctx context.Context, id, field, op, key, value string) (bead *model.Bead, changed bool, err error)
	// EditBeadCSVField atomically appends or removes ONE element of the CSV
	// string field `field` (e.g. slack_channel) without disturbing the field's
	// other elements or the bead's other fields — the CSV analogue of
	// MergeBeadMapField. op is "append" (add-if-absent, idempotent) or "remove"
	// (drop every occurrence). Same one-transaction FOR-UPDATE atomicity, so
	// concurrent appends of DIFFERENT elements never clobber. `changed` is false
	// on an idempotent no-op. Returns sql.ErrNoRows if the bead does not exist,
	// ErrInvalidFieldKey if `field` is not a safe identifier, and
	// ErrInvalidSubFieldEdit for a bad op / empty element / an element
	// containing a comma / a non-CSV stored value. See ka-0000007767.
	EditBeadCSVField(ctx context.Context, id, field, op, element string) (bead *model.Bead, changed bool, err error)
	CloseBead(ctx context.Context, id string, closedBy string) (*model.Bead, error)
	DeleteBead(ctx context.Context, id string) error
	// RestoreBead clears deleted_at on a soft-deleted bead and returns it.
	// Returns sql.ErrNoRows when the bead does not exist or is not soft-deleted.
	RestoreBead(ctx context.Context, id string) (*model.Bead, error)
	// PurgeBeads hard-deletes the beads req selects (soft-deleted ones too)
	// with everything keyed on them, and returns them sorted by id. With
	// req.DryRun it deletes nothing. Lands step 8.11 (http_lands_purge.go).
	PurgeBeads(ctx context.Context, req PurgeRequest) ([]PurgedBead, error)

	// Fleet
	TransferBead(ctx context.Context, id string, toFleet string) (bead *model.Bead, fromFleet string, err error)

	// Dependencies
	AddDependency(ctx context.Context, dep *model.Dependency) error
	// RemoveDependency deletes one dependency edge. `removed` is false when the
	// (beadID, dependsOnID, depType) triple matched no row. That is NOT an
	// error: idempotent callers (`kd reparent`, `kd admin backfill-mr-edges`)
	// rely on removing an already-absent edge succeeding, so a 0-match returns
	// (false, nil) and callers that don't care may discard it. Interactive
	// `kd dep remove` uses it to report a no-op instead of a false success
	// (ka-0000001966).
	RemoveDependency(ctx context.Context, beadID, dependsOnID string, depType model.DependencyType) (removed bool, err error)
	GetDependencies(ctx context.Context, beadID string) ([]*model.Dependency, error)
	GetReverseDependencies(ctx context.Context, beadID string) ([]*model.Dependency, error)
	// GetDependencyEdgesByType returns every dep row of the given type as
	// (bead_id, depends_on_id) pairs across the whole graph, restricted to
	// non-soft-deleted endpoints. Used by the blocks-cycle audit
	// (`kd admin audit-blocks-cycles`, ka-0000007984) to reconstruct the
	// full single-type edge set and run SCC detection over it. The result
	// is unbounded in principle but the blocks graph is small (thousands of
	// edges), so a single scan is fine.
	GetDependencyEdgesByType(ctx context.Context, depType model.DependencyType) ([]model.DependencyEdge, error)

	// Batch dependency/label queries (graph endpoint)
	GetDependenciesForBeads(ctx context.Context, beadIDs []string) (map[string][]*model.Dependency, error)
	GetReverseDependenciesForBeads(ctx context.Context, beadIDs []string) (map[string][]*model.Dependency, error)
	GetDependencyCounts(ctx context.Context, beadIDs []string) (map[string]*model.DependencyCounts, error)
	// GetChildrenStatusHistogram returns, for each parent bead ID, a histogram
	// of its parent-child children grouped by status (open/in_progress/closed/…).
	// Used by the project-view sequencer to color epic pills in O(1) round
	// trips per page render. Parents with no parent-child children are omitted
	// from the result map. Soft-deleted children are excluded.
	GetChildrenStatusHistogram(ctx context.Context, parentIDs []string) (map[string]map[string]int, error)
	GetLabelsForBeads(ctx context.Context, beadIDs []string) (map[string][]string, error)
	GetBlockedByForBeads(ctx context.Context, beadIDs []string) (map[string][]string, error)
	GetBeadsByIDs(ctx context.Context, ids []string) ([]*model.Bead, error)
	// GetTextReferencesForBeads returns ka-id references parsed out of bead
	// text where BOTH source and target are in the supplied set. Dangling
	// references (target not in window) are omitted by design — see
	// kbeads/docs/design-bead-references.md § 5b.
	GetTextReferencesForBeads(ctx context.Context, beadIDs []string) ([]*model.BeadReference, error)

	// GetTaintsForBead returns the ids of the taints currently on beadID
	// whose effects include "gate" (bead_taints.taint_id), per ADR
	// docs/adr/taints.md §3/§4.1/§15.2. Empty when the bead carries no
	// gate-effect taints or does not exist. This is what the read gate,
	// write gate, and list predicate consult — a taint whose effects
	// exclude "gate" (e.g. {} or {restrict}) must never hide or block a
	// bead. Use GetAllTaintsForBead for propagation, which is
	// effect-agnostic.
	GetTaintsForBead(ctx context.Context, beadID string) ([]int64, error)

	// GetTaintsForBeads is the batch form of GetTaintsForBead, keyed by bead
	// ID. A bead absent from the result map carries no gate-effect taints.
	// Used by the read-gate's third check (beadReadAuthorized) so a
	// batch/list-style caller pays one query for the whole page instead of
	// one per bead.
	GetTaintsForBeads(ctx context.Context, beadIDs []string) (map[string][]int64, error)

	// GetAllTaintsForBead returns the ids of every taint currently on
	// beadID, regardless of effects. Taint propagation (docs/adr/taints.md
	// §4.2) is effect-agnostic — an inherited taint carries whatever
	// effects it has — so propagation call sites use this instead of the
	// gate-filtered GetTaintsForBead.
	GetAllTaintsForBead(ctx context.Context, beadID string) ([]int64, error)

	// GetAllTaintsForBeads is the batch form of GetAllTaintsForBead, keyed
	// by bead ID. A bead absent from the result map carries no taints. Used
	// by inherit:read on reads that return many beads at once (lists,
	// search, batch), so a page costs one query instead of one per bead.
	GetAllTaintsForBeads(ctx context.Context, beadIDs []string) (map[string][]int64, error)

	// GetGrantedTaintIDs returns the distinct taint ids for which a
	// currently-valid (expires_at IS NULL OR expires_at > now()) taint_grants
	// row exists with principal_kind = principalKind and principal IN
	// principals, per ADR docs/adr/taints.md §5.1. Used by grantsOf to build
	// the caller's grant set for each applicable principal kind (own operator
	// id, teams, agent bead id, fleet name).
	GetGrantedTaintIDs(ctx context.Context, principalKind string, principals []string) ([]int64, error)

	// GetTaintIDsOwnedByTeams returns the ids of every taint whose owner_team
	// is in teams, per ADR docs/adr/taints.md §5.1/§10.1: "creating a taint
	// grants the owner team implicitly (it is not a row; ownership implies
	// the grant)". Not gated on retired_at — ADR §11 invariant #8 says
	// retiring blocks new applies, not existing read access.
	GetTaintIDsOwnedByTeams(ctx context.Context, teams []string) ([]int64, error)

	// HasAnyTaints reports whether the taints registry holds at least one
	// row, per ADR docs/adr/taints.md §10.1: "No taints exist -> nothing
	// changes. No table is read on the hot path; the gate short-circuits on
	// an empty registry cached per process." Callers cache this fact
	// themselves (taint_registry_cache.go) — this method always queries.
	HasAnyTaints(ctx context.Context) (bool, error)

	// SetTaint creates or updates (by name) a row in the taints registry,
	// per ADR docs/adr/taints.md §3/§6.2/§10.1. This is the only write path
	// into the taints table, so validation lives here: scrub_policy is
	// checked against the shape in §3/§6.2 (default required and never
	// "auto", every rule's mode known, an "auto" rule carries max_beads),
	// owner_team must resolve to a live type:team bead, and a taint created
	// with no scrub_policy gets the §10.1 human-four-eyes default with the
	// owner team as approver. A validation failure returns
	// *model.ValidationError. On success t is populated with its id,
	// resolved scrub_policy, and timestamps.
	SetTaint(ctx context.Context, t *model.Taint) error
	// GetTaint returns the taint named name, or sql.ErrNoRows if none exists.
	GetTaint(ctx context.Context, name string) (*model.Taint, error)
	// ListTaints returns every taint in the registry, ordered by name.
	ListTaints(ctx context.Context) ([]*model.Taint, error)

	// GrantTaint creates or refreshes (by taint_id+principal_kind+principal) a
	// taint_grants row, per ADR docs/adr/taints.md §3/§5.1/§7/§10.4. Validates
	// PrincipalKind against model.IsValidPrincipalKind and Principal is
	// required; a failure returns *model.ValidationError. g.GrantedAt is
	// populated on success.
	GrantTaint(ctx context.Context, g *model.TaintGrant) error
	// RevokeTaint deletes one taint_grants row. removed is false only when no
	// row matched (idempotent — a repeat revoke is not an error).
	RevokeTaint(ctx context.Context, taintID int64, principalKind, principal string) (removed bool, err error)
	// GetTaintGrant returns the CURRENTLY VALID grant (expires_at IS NULL OR
	// expires_at > now()) for the given taint/principal, or sql.ErrNoRows if
	// none exists or the only matching row has expired.
	GetTaintGrant(ctx context.Context, taintID int64, principalKind, principal string) (*model.TaintGrant, error)

	// ApplyTaint writes a bead_taints row, per ADR docs/adr/taints.md
	// §3/§4.2/§7. Idempotent: applying a taint already on the bead is a
	// no-op that preserves the original source/applied_by/applied_at
	// (ON CONFLICT (bead_id, taint_id) DO NOTHING). inserted is false when
	// the taint was already applied — callers use this to avoid writing a
	// duplicate taint_audit row for a no-op re-apply.
	ApplyTaint(ctx context.Context, bt *model.BeadTaint) (inserted bool, err error)
	// GetBeadTaints returns every taint applied to beadID, joined with each
	// taint's name, ordered by taint name. Unfiltered by effects — used for
	// display and propagation, where every taint on the bead is relevant
	// regardless of what it gates.
	GetBeadTaints(ctx context.Context, beadID string) ([]*model.BeadTaint, error)
	// GetGateBeadTaints is GetBeadTaints filtered to taints whose effects
	// include "gate" (docs/adr/taints.md §15.2). The write gate consults
	// this instead of GetBeadTaints so a taint whose effects exclude "gate"
	// never blocks a write.
	GetGateBeadTaints(ctx context.Context, beadID string) ([]*model.BeadTaint, error)
	// RecordTaintAudit writes a taint_audit row, per ADR docs/adr/taints.md
	// §3: a hash-only record of a taint mutation (grant, revoke, or apply).
	// a.Actor must already be server-derived; content is never stored.
	// a.ID and a.CreatedAt are populated on success.
	RecordTaintAudit(ctx context.Context, a *model.TaintAudit) error

	// ScrubTaint executes a scrub (purge or detaint) against every bead in
	// req.BeadIDs, in a single transaction per docs/adr/taints.md §6.5:
	//
	//   - purge and detaint/strip run the shared content cascade (every
	//     table in cascadeContentTables, postgres/scrub_cascade_tables.go),
	//     blank/overwrite the bead's own content, DELETE bead_embeddings,
	//     DELETE the bead_taints row, and INSERT a taint_audit row hashing
	//     the pre-scrub content.
	//   - detaint/fork never touches the original's content or bead_taints
	//     row at all: it closes the original (fields.close_reason=
	//     "scrubbed"), creates a successor bead from
	//     req.CleanContent[beadID] under a machine-origin write (no
	//     propagation runs at this layer, so the successor is never
	//     re-tainted), and adds a "supersedes" deps edge from the successor
	//     to the original.
	//   - detaint/lift and any other DetaintMode value are refused with an
	//     error — implementing "lift" is a separate task (gd-XAXEBMrKOD);
	//     this switch is written generically so adding that arm later needs
	//     no restructuring.
	//
	// Every row this call writes to mutation_history carries
	// model.SystemActorTaintScrub as its actor, which
	// internal/server/undo.go's undoMutation refuses to reverse.
	ScrubTaint(ctx context.Context, req *model.ScrubTaintRequest) (*model.ScrubTaintResult, error)

	// ListTaintGrants returns every taint_grants row for taintID, including
	// expired ones, ordered by granted_at descending — the viewer's Grants
	// tab (docs/adr/taints.md §9.1) shows expiry itself rather than hiding an
	// expired grant.
	ListTaintGrants(ctx context.Context, taintID int64) ([]*model.TaintGrant, error)

	// ListTaintAudit returns taint_audit rows for taintID matching filter,
	// newest first, for the viewer's Audit tab (docs/adr/taints.md §9.1).
	ListTaintAudit(ctx context.Context, taintID int64, filter model.TaintAuditFilter) ([]*model.TaintAudit, int, error)

	// GetBeadTaintsForTaint is the batch form of GetBeadTaints, filtered to
	// one taint and restricted to beadIDs, keyed by bead id. Used by the
	// viewer's Beads tab (docs/adr/taints.md §9.1) to annotate an
	// already-read-gated page of beads with their propagation source for
	// this taint without an N+1 query per row.
	GetBeadTaintsForTaint(ctx context.Context, taintID int64, beadIDs []string) (map[string]*model.BeadTaint, error)

	// Labels
	AddLabel(ctx context.Context, beadID string, label string) error
	// RemoveLabel deletes one label. `removed` is false when the
	// (beadID, label) pair matched no row. That is NOT an error: idempotent
	// callers (`kd update --remove-label`, the controller's label reconcilers,
	// SetLabels) rely on removing an already-absent label succeeding, so a
	// 0-match returns (false, nil) and callers that don't care may discard it.
	// Interactive `kd label remove` uses it to report a no-op instead of a
	// false success (ka-0000000424).
	RemoveLabel(ctx context.Context, beadID string, label string) (removed bool, err error)
	GetLabels(ctx context.Context, beadID string) ([]string, error)

	// Comments
	AddComment(ctx context.Context, comment *model.Comment) error
	GetComments(ctx context.Context, beadID string) ([]*model.Comment, error)
	// DeleteComment removes a single comment row. Returns the deleted
	// comment so callers can carry author/text into audit events; returns
	// (nil, nil) when no row matched (comment_id absent or on a different
	// bead) — callers translate this to an idempotent "already gone"
	// response. See migration 022 — every delete writes an audit event row
	// whose operated_by_id is required.
	DeleteComment(ctx context.Context, beadID string, commentID int64) (*model.Comment, error)
	// TrimComments removes all but the most recent keepLast comments on
	// beadID, optionally restricted to comments authored by authorFilter
	// (empty string means all authors). Returns the deleted rows in
	// ascending creation order so the server layer can fan out one audit
	// event per removed comment. keepLast must be >= 0; a negative value
	// is rejected by the caller. keepLast == 0 deletes every matching
	// comment.
	TrimComments(ctx context.Context, beadID string, keepLast int, authorFilter string) ([]*model.Comment, error)

	// References (ka-0000001713 text references). ReextractReferencesPage walks
	// one keyset-paginated page of beads (id > afterID, ORDER BY id ASC, LIMIT
	// batchSize), re-runs reference extraction over each bead's text fields and
	// every one of its comments, and replaces the corresponding rows in
	// bead_references. Idempotent. Per-bead work runs in its own transaction so
	// the page can be retried after a transient failure without leaving the
	// table half-rewritten. The final page (Done=true) reports the table's
	// total cardinality. When beadType is non-empty the page walk is narrowed
	// to beads of that type (index-backed) so a targeted backfill can reindex
	// one type FIRST — chiefly `type=decision`, the only type the
	// parked_on_decision badge queries (ka-0000005898). See
	// kbeads/docs/design-bead-references.md § 4 and the
	// `kd admin reextract-references` CLI driver.
	ReextractReferencesPage(ctx context.Context, afterID string, batchSize int, beadType string) (*ReextractReferencesStats, error)

	// BackfillSearchTSVPage fills or refreshes the search_tsv FTS column on one
	// keyset-paginated page of beads (id > afterID, ORDER BY id ASC, LIMIT
	// batchSize). The per-page UPDATE is its own transaction so the backfill
	// respects the no-eager-backfill policy (kbeads/CLAUDE.md) — never one
	// table-locking pass. See the `kd admin backfill-search-tsv` CLI driver.
	//
	// reindex selects the mode:
	//   - false (migration 039, ka-0000003832): sweep LEGACY rows whose
	//     search_tsv IS NULL. New/updated rows are kept current by the
	//     migration's BEFORE INSERT/UPDATE trigger; this fills the rows that
	//     pre-date the migration. Idempotent via the WHERE search_tsv IS NULL
	//     guard. The final page (Done=true) reports Remaining, the count still
	//     NULL (0 on a clean finish).
	//   - true (migration 040, ka-0000005839): REFRESH rows that carry a
	//     `context` or `options` field, whose 040 vector (now indexing that
	//     free text) differs from the title+description-only vector 039 stored.
	//     Those rows are already non-NULL, so the NULL-fill mode never touches
	//     them. Idempotent via a `search_tsv IS DISTINCT FROM <expr>` guard;
	//     Remaining is not meaningful and stays 0 (Done is the completion
	//     signal).
	BackfillSearchTSVPage(ctx context.Context, afterID string, batchSize int, reindex bool) (*BackfillSearchTSVStats, error)

	// BackfillCategoryPage re-stamps the beads.category materialized column on
	// one keyset-paginated page of beads (id > afterID, ORDER BY id ASC, LIMIT
	// batchSize) whose stored category disagrees with the current
	// derive_bead_category rule table. The per-page UPDATE is its own
	// transaction so the backfill respects the no-eager-backfill policy
	// (kbeads/CLAUDE.md) — never one table-locking pass. Idempotent via a
	// `category IS DISTINCT FROM derive_bead_category(id, type)` guard, so a
	// re-run (or a row the trigger already refreshed) is a true no-op. Used by
	// the `kd admin backfill-category` CLI driver after deploying migration 043
	// (ka-0000007274) to sweep legacy deliverable rows carrying a stale
	// `intermediate` category onto `artifact`.
	BackfillCategoryPage(ctx context.Context, afterID string, batchSize int) (*BackfillCategoryStats, error)

	// BackfillProjectLabelsPage adds the project: label to up to batchSize of
	// the beads ProjectLabelMissingCounts counts, each through the AddLabel
	// path (fleet column, beads.project sync), in one transaction per page.
	BackfillProjectLabelsPage(ctx context.Context, projectTypes []string, batchSize int) (*BackfillProjectLabelsStats, error)

	// BackfillOperators rebinds every audit row that carries a non-empty
	// operated_by TEXT value to its operators(id) FK, and binds empty-string
	// rows on live-state tables (beads, comments, deps) to the
	// system:legacy-pre-attribution sentinel. Idempotent: every UPDATE
	// filters operated_by_id IS NULL so re-runs on already-bound data are
	// no-ops. See ADR docs/adr/human-attribution-identity-v2.md §4.3 / §4.5
	// / §4.6 and the `kd admin backfill-operators` CLI driver. Used once at
	// rollout time, not on the steady-state write path.
	BackfillOperators(ctx context.Context, opts BackfillOperatorsOptions) (*BackfillOperatorsStats, error)

	// BackfillOperatedByLegacy rebinds beads currently bound to a legacy
	// `operated_by_id` (e.g. slack:U0OPERATOR1 / Matt) to their correct
	// operator according to the categorization in
	// gardenio/docs/research/legacy-operated-by-matt-cleanup.md §3.2:
	//   A: Slack-thread agents → resolve via fields.slack_user_id
	//   B: cron-spawned beads (created_by='static') → schedule.operated_by_id
	//   C: agent-spawned children → iterative parent-walk via deps
	//   D: genuinely the target operator → leave alone
	// Each batch (Pass 1) and each parent-walk iteration (Pass 2) runs in
	// its own transaction per kbeads/CLAUDE.md §Migration policy. Every
	// UPDATE writes one mutation_history row per affected bead.
	BackfillOperatedByLegacy(ctx context.Context, opts BackfillOperatedByLegacyOptions) (*BackfillOperatedByLegacyStats, error)

	// BackfillSlackUserID populates the missing `fields.slack_user_id` on
	// thread-derived legacy beads (ka-0000001582). It resolves the
	// originating Slack thread's user id from the root "Slack thread:" bead
	// (created by the slack-bridge, which stamps slack_user_id at thread
	// open) — first via each candidate's `fields.thread_id` bead-id FK, then
	// via the timestamp embedded in `created_by = "thread-<sanitized-ts>"`.
	// No Slack API call is required. Each batch runs in its own transaction
	// per kbeads/CLAUDE.md §Migration policy; every write records one
	// mutation_history row.
	BackfillSlackUserID(ctx context.Context, opts BackfillSlackUserIDOptions) (*BackfillSlackUserIDStats, error)

	// BackfillOperatorSlugs recomputes readable slugs for operators still on
	// the opaque `op-<id>` form that now carry a real (non-sentinel) email
	// (ka-0000001149). Run AFTER the operator-cluster merge cleanup so it
	// slugs the human survivors rather than soon-to-be-merged duplicates.
	// The old op-<id> slug is preserved as a retired operator_slug_aliases
	// row so existing /u/op-NNN links still resolve. Machine / sentinel rows
	// (system:*, fleet:*, un-resynced slack) have no derivable readable slug
	// and are left on op-<id>. Each batch runs in its own transaction per
	// kbeads/CLAUDE.md §Migration policy.
	BackfillOperatorSlugs(ctx context.Context, opts BackfillOperatorSlugsOptions) (*BackfillOperatorSlugsStats, error)

	// BackfillParentChildDeps reconstructs missing parent-child edges
	// for agent-authored beads that were orphaned by default before the
	// server-side auto-parent inheritance landed (ka-0000007224). For
	// each candidate bead, it joins to the author's agent bead and reads
	// fields.task_id; when set and no parent-child edge already exists,
	// it adds one. Each batch (BatchSize) is its own transaction so the
	// backfill is resumable — re-running on already-linked data is a
	// no-op via ON CONFLICT DO NOTHING. See ka-0000006775 and the
	// `kd admin backfill-parent-child-deps` CLI driver.
	BackfillParentChildDeps(ctx context.Context, opts BackfillParentChildDepsOptions) (*BackfillParentChildDepsStats, error)

	// RepairReversedParentChildDeps scans the deps table for parent-child
	// rows stored in the REVERSED direction — bead_id is a type=epic bead
	// and depends_on_id is a non-epic bead — and (unless DryRun) flips
	// each to the canonical direction (bead_id=child, depends_on_id=epic)
	// so the ParentSubtreeID CTE and gardener preflights see the subtree.
	// Each batch is its own transaction; the repair is idempotent. See
	// ka-0000006880 and the `kd admin repair-reversed-parent-child-deps`
	// CLI driver.
	RepairReversedParentChildDeps(ctx context.Context, opts RepairReversedParentChildDepsOptions) (*RepairReversedParentChildDepsStats, error)

	// BackfillTriggeredByDeps walks recently-created beads with no
	// existing parent-child / triggered-by dep and writes a
	// triggered-by edge when a high-confidence signal field on the
	// bead points unambiguously at a single live origin bead. Catches
	// escapes from M2/M3 (ka-0000000827 M4/D). Append-only, idempotent
	// via the standard dep INSERT ON CONFLICT path. See ka-0000000004
	// and the `kd admin backfill-triggered-by-deps` CLI driver.
	BackfillTriggeredByDeps(ctx context.Context, opts BackfillTriggeredByDepsOptions) (*BackfillTriggeredByDepsStats, error)

	// CloseTriggerAuditBeads closes legacy type=event beads labeled
	// `auto:trigger-audit` whose status is still open. These are
	// append-only ledger records — the forward-fix (ka-0000004100) now
	// creates new audit beads with status=closed, but ~226k legacy rows
	// from before the fix are still open and inflate the beads-viewer
	// open-beads count. Each batch (BatchSize) is its own transaction so
	// the sweep is resumable; re-running on already-closed data is a
	// no-op via WHERE status != 'closed'. See ka-0000006341 and the
	// `kd admin close-trigger-audit-beads` CLI driver.
	CloseTriggerAuditBeads(ctx context.Context, opts CloseTriggerAuditBeadsOptions) (*CloseTriggerAuditBeadsStats, error)

	// BackfillOwner rebinds beads whose `owner` text column equals the
	// target sentinel (typically slack:U0OPERATOR1 — Matt) and were
	// created at-or-after the ka-0000000269 deploy date. For each
	// candidate it walks the spawn chain (created_by → agent bead →
	// fields.spawned_by → … → root) to resolve the originating human;
	// when the resolved human is the target, the row is left alone, when
	// the resolved human is different the row is rebound to that human,
	// when no human resolves the row is rebound to opts.UnownedSentinel
	// (default `system:unowned`). Each batch (BatchSize) is its own
	// transaction; idempotent via the `WHERE owner = $target` filter —
	// re-runs on a fully-cleaned set are no-ops. See ka-0000006961 and
	// the `kd admin backfill-owner` CLI driver.
	BackfillOwner(ctx context.Context, opts BackfillOwnerOptions) (*BackfillOwnerStats, error)

	// BackfillEmptyOwner fills the `owner` text column on beads where it is
	// still empty (`''` or NULL) after the ka-0000000269 server-side
	// default landed. It runs three sequential chunked phases: owner ←
	// operated_by (Phase 1), owner ← created_by for pre-attribution rows
	// (Phase 2), and owner ← opts.UnownedSentinel for the residual (Phase
	// 3). Each phase is a `UPDATE … LIMIT BatchSize` loop that runs until
	// zero rows change; each batch is its own transaction. Idempotent via
	// the `COALESCE(owner,'') = ''` filter — a re-run on a drained set is a
	// no-op. Complements BackfillOwner (which rebinds an already-set
	// owner). See ka-0000007029 and the `kd admin backfill-empty-owner` CLI
	// driver.
	BackfillEmptyOwner(ctx context.Context, opts BackfillEmptyOwnerOptions) (*BackfillEmptyOwnerStats, error)

	// BackfillOperatedByAgents resolves operated_by_id on `type=agent`
	// beads where it is still unset (NULL on legacy envs, or bound to
	// the `system:legacy-pre-attribution` sentinel post-migration 022)
	// by reading the bead's own identity fields and looking them up in
	// operator_aliases. The per-user homepage (ka-0000006114) filters
	// agent beads by operated_by_id; legacy rows that never got stamped
	// at write time stay invisible on a human's page until this
	// backfill stamps them. Sub-item #1 of ka-0000006817. Each batch is
	// its own transaction per kbeads/CLAUDE.md §Migration policy; every
	// UPDATE writes one mutation_history row.
	BackfillOperatedByAgents(ctx context.Context, opts BackfillOperatedByAgentsOptions) (*BackfillOperatedByAgentsStats, error)

	// Bead references (ka-0000001713 IDs parsed from bead text).
	// See kbeads/docs/design-bead-references.md § 5a.
	//
	// ListBeadsMentioning returns the beads whose text references targetID,
	// joined to beads(id) so the caller gets a minimal summary per row.
	// Soft-deleted source beads are excluded. Pagination via limit/offset;
	// total reflects the full unbounded result size. relationshipType is the
	// typed-mention filter (T6, kbeads/docs/research-typed-mention-syntax.md):
	// nil = no filter; non-nil "" = untyped only (relationship_type IS NULL);
	// non-nil "<verb>" = exact match.
	ListBeadsMentioning(ctx context.Context, targetID string, limit, offset int, relationshipType *string) ([]*model.BeadReferenceSummary, int, error)
	// HasBundleChild reports whether parentID has at least one child of
	// type=bundle linked via a parent-child reference. Used by GET
	// /v1/beads/{id} to set BeadDetail.HasBundle so the UI can show a
	// "View bundle" affordance without a follow-up query. Backed by the
	// idx_bead_references_target_type partial index. See plan
	// gardenio/plans/agent-artifact-bundles-multi-doc-viewer.md §3.5.
	HasBundleChild(ctx context.Context, parentID string) (bool, error)
	// ListBeadReferencesFrom returns the references emitted by sourceID,
	// LEFT-joined to beads(id) so dangling targets (typos, cross-fleet IDs)
	// still appear with empty Title/Type/Status. Pagination via limit/offset;
	// total reflects the full unbounded result size. relationshipType applies
	// the same filter semantics as ListBeadsMentioning.
	ListBeadReferencesFrom(ctx context.Context, sourceID string, limit, offset int, relationshipType *string) ([]*model.BeadReferenceSummary, int, error)

	// Events
	RecordEvent(ctx context.Context, event *model.Event) error
	GetEvents(ctx context.Context, beadID string) ([]*model.Event, error)
	// GetRecentEvents returns the most-recent `limit` events for a bead, sorted
	// ASC by created_at (same shape as GetEvents). limit must be > 0. Backs the
	// ?limit=N parameter on GET /v1/beads/{id}/events — beads-viewer uses it on
	// hot paths to avoid loading multi-MB event histories on trigger beads.
	GetRecentEvents(ctx context.Context, beadID string, limit int) ([]*model.Event, error)
	// GetEventsByTopic is GetEvents / GetRecentEvents narrowed to one topic:
	// the most-recent `limit` events on beadID whose topic is `topic`, sorted
	// ASC by created_at. limit <= 0 returns every event of that topic. Backs
	// the opt-in ?topic= parameter on GET /v1/beads/{id}/events, which lets a
	// caller that only wants one signal (the trigger-engine's label.added /
	// label.removed scans) skip shipping the bead's whole recent history
	// (gd-cP9tODid8j).
	GetEventsByTopic(ctx context.Context, beadID, topic string, limit int) ([]*model.Event, error)
	// EventHistogram returns time-bucketed counts of rows in the events table
	// for [since, until], using the given Postgres date_trunc field
	// ("minute", "hour", "day"). The bucket parameter is the date_trunc
	// argument; HTTP-level aliases like "5m"/"15m" must be normalized by the
	// caller (see internal/server/http_events_histogram.go).
	//
	// scope restricts the aggregation to events whose bead is within the
	// caller's authorized projects (ka-0000006863); an unrestricted scope
	// (the zero value) counts events across every project. Because counting
	// must be scoped at the SQL level — post-filtering aggregate rows is
	// impossible — the query JOINs through the bead's labels.
	EventHistogram(ctx context.Context, since, until time.Time, bucket string, scope model.EventReadScope) ([]model.EventHistogramBucket, error)
	// GetEventsByTimeRange returns events across all beads created within
	// [since, until], in chronological order (created_at ASC, id ASC). When
	// cursor > 0, only events with id > cursor are returned. limit is the
	// maximum number of events to return; callers may pass limit+1 and
	// trim to detect has_more.
	//
	// scope restricts the result to events whose bead is within the caller's
	// authorized projects (ka-0000006863); an unrestricted scope (the zero
	// value) returns events across every project. The event payload embeds the
	// full bead for create/update/close topics, so scoping here is a
	// confidentiality gate, not merely a filter.
	GetEventsByTimeRange(ctx context.Context, since, until time.Time, cursor int64, limit int, scope model.EventReadScope) ([]*model.Event, error)

	// GetEventsByTimeRangeDesc is the reverse-chronological companion to
	// GetEventsByTimeRange: events within [since, until] ordered created_at
	// DESC, id DESC. When cursor > 0, only events with id < cursor are
	// returned (cursor=0 starts at the newest). It lets a caller that only
	// wants the most-recent N events bound its scan to exactly N rows. scope
	// applies the same cross-project read gate (ka-0000006863).
	GetEventsByTimeRangeDesc(ctx context.Context, since, until time.Time, cursor int64, limit int, scope model.EventReadScope) ([]*model.Event, error)

	// LastEventAtByOperatorAliases returns the most recent events.created_at
	// timestamp for each `kind:subject` alias in aliases. Aliases are resolved
	// through operator_aliases to events.operated_by_id; aliases that never
	// emitted an event are omitted from the result map. Backs the per-bridge
	// last_event_at signal on /v1/health/snapshot (bridges tile).
	LastEventAtByOperatorAliases(ctx context.Context, aliases []string) (map[string]time.Time, error)

	// Stats
	GetStats(ctx context.Context) (*model.GraphStats, error)

	// Fleet metrics aggregations. Used by the Prometheus fleetCollector to
	// avoid pulling tens of thousands of full bead rows on every scrape.
	// FleetTypeStatusCounts returns counts of live (non-soft-deleted) beads
	// grouped by (type, status). FleetActiveAgentsByProject counts the open
	// and in_progress agent beads per project; it reads only those rows, so
	// it is cheap enough for every refresh and the health snapshot.
	// FleetAgentLifetimeRollup returns per-project totals over every agent
	// bead ever created (count, summed total_cost_usd, summed session_count);
	// it scans all of them, so it runs on a slower cadence (gd-ipZ10t3oTo).
	// All three aggregate in the database; per-call Go heap is bounded by the
	// number of distinct (type,status) pairs and project labels — typically a
	// few KB. See kbeads/docs/oom-fleet-collector-investigation.md
	// (ka-0000006525).
	FleetTypeStatusCounts(ctx context.Context) (map[FleetTypeStatusKey]int, error)
	FleetActiveAgentsByProject(ctx context.Context) (map[string]int, error)
	FleetAgentLifetimeRollup(ctx context.Context) ([]FleetAgentProjectRollup, error)

	// FleetQueueDepthByProject returns the count of OPEN operator-facing queue
	// beads (types in FleetQueueTypes) grouped by (project, type). Project is
	// the first `project:*` label on the bead, or "unknown". Result cardinality
	// is bounded by (#projects × #queue types) — a handful of rows. Powers the
	// gardenio_beads_open_queue_depth gauge (ka-0000002235).
	FleetQueueDepthByProject(ctx context.Context) (map[FleetQueueDepthKey]int, error)

	// FleetQueueOldestAgeByProject returns the age IN SECONDS of the OLDEST
	// open operator-facing queue bead (types in FleetQueueTypes) grouped by
	// (project, type) — i.e. how long the least-recently-created undrained
	// item in each bucket has waited. It is the head-of-line-staleness twin of
	// FleetQueueDepthByProject (which reports the count): the same bucket can
	// have a small depth but a large oldest-age when one item sits un-actioned.
	// Cardinality is identical — bounded by (#projects × #queue types). Powers
	// the gardenio_beads_open_queue_oldest_age_seconds gauge (rescope
	// ka-0000003130). Buckets with no open queue bead are simply absent, so the
	// gauge is never emitted as a misleading 0 for an empty queue.
	FleetQueueOldestAgeByProject(ctx context.Context) (map[FleetQueueDepthKey]float64, error)

	// ProjectLabelMissingCounts counts live beads, by type, that break the
	// project-label invariant: one of projectTypes with fields.project and no
	// project: label, or a message with no project: label whose thread head
	// carries exactly one (gd-YdsEpnN3Qo). Powers the
	// gardenio_beads_project_label_missing gauge. Types with none are absent.
	ProjectLabelMissingCounts(ctx context.Context, projectTypes []string) (map[string]int, error)

	// AcceptanceMetrics returns the three C5 acceptance signals
	// (ka-0000004365) in one round trip. Powers GET
	// /v1/metrics/acceptance and the `gb metrics` CLI subcommands.
	AcceptanceMetrics(ctx context.Context) (*AcceptanceMetrics, error)

	// Configs
	SetConfig(ctx context.Context, config *model.Config) error
	GetConfig(ctx context.Context, key string) (*model.Config, error)
	ListConfigs(ctx context.Context, namespace string) ([]*model.Config, error)
	ListAllConfigs(ctx context.Context) ([]*model.Config, error)
	DeleteConfig(ctx context.Context, key string) error

	// Embedding operations
	UpsertEmbedding(ctx context.Context, beadID string, embedding []float32, modelName string, textHash string) error
	GetEmbedding(ctx context.Context, beadID string) (embedding []float32, modelName string, err error)
	// GetEmbeddingHashes returns a map of bead_id -> text_hash for the subset
	// of beadIDs that currently have an embedding row. Missing beads are
	// omitted from the result. Used by the embed-sync backfill to skip beads
	// whose text content hasn't changed.
	GetEmbeddingHashes(ctx context.Context, beadIDs []string) (map[string]string, error)
	// ListBeadsNeedingEmbedding paginates beads that have no embedding row OR
	// whose stored embedding is older than the bead. Returned total reflects
	// the total backfill workload, not the global bead count. Used by
	// `kd embed sync --only-missing` to skip walking the full beads table.
	ListBeadsNeedingEmbedding(ctx context.Context, fleet string, limit, offset int) ([]*model.Bead, int, error)
	// RefreshEmbeddingTimestamps bumps bead_embeddings.updated_at = NOW() for
	// the given bead IDs, marking their stored embedding as fresh relative
	// to the bead's updated_at. Used by `kd embed sync --only-missing` after
	// the server determines a bead's text_hash is unchanged: without the
	// bump, the SQL filter (b.updated_at > e.updated_at) keeps re-listing
	// these beads forever and the CLI loop never makes progress (ka-0000000728).
	// Returns the number of rows updated. Beads without an embedding row
	// are silently ignored.
	RefreshEmbeddingTimestamps(ctx context.Context, beadIDs []string) (int64, error)
	SemanticSearch(ctx context.Context, queryVec []float32, filter model.BeadFilter, limit int) ([]*model.SemanticResult, error)
	DeleteEmbedding(ctx context.Context, beadID string) error
	GetEmbeddingStats(ctx context.Context) (*model.EmbeddingStats, error)

	// MigrateEmbeddingsSchema is the destructive DDL half of `kd embed migrate`:
	// drops the ANN index, truncates bead_embeddings, alters the embedding
	// column to vector(dims), and records an audit row in
	// bead_embedding_migrations. Callers must follow up with a backfill
	// (`kd embed sync --force`) and then RecreateEmbeddingIndex.
	MigrateEmbeddingsSchema(ctx context.Context, modelName string, dims int, actor, note string) error
	// RecreateEmbeddingIndex re-creates the ANN index on bead_embeddings.
	// Called after the post-migration backfill has populated the table.
	RecreateEmbeddingIndex(ctx context.Context) error

	// Revoked tokens (persistent SHA-256-hash blocklist).
	InsertRevokedToken(ctx context.Context, rt *model.RevokedToken) error
	ListRevokedTokenHashes(ctx context.Context) ([]string, error)

	// Refresh tokens (C1.2 — Shape C). Plaintext is never stored; rows are
	// keyed by SHA-256 hash. RevokeRefreshTokenChain marks the given hash
	// and all descendants (parent_refresh_hash chain) as revoked.
	InsertRefreshToken(ctx context.Context, rt *model.RefreshToken) error
	GetRefreshToken(ctx context.Context, hash string) (*model.RefreshToken, error)
	RevokeRefreshTokenChain(ctx context.Context, hash string, revokedAt time.Time) (int, error)
	// ListActiveRefreshTokenHashesByFleet returns the hashes of every
	// unrevoked refresh token issued to the fleet (served by
	// idx_refresh_tokens_fleet), so a fleet revoke can cascade to them.
	ListActiveRefreshTokenHashesByFleet(ctx context.Context, fleet string) ([]string, error)

	// Runner tokens (local-agents spec §4.2). Plaintext "gr_..." bearers are
	// never stored; rows are keyed by SHA-256 hash. GetRunnerTokenByHash
	// returns (nil, nil) for an unknown hash. RevokeRunnerTokens stamps
	// revoked_at on every live token of one runner and returns the count.
	InsertRunnerToken(ctx context.Context, rt *model.RunnerToken) error
	GetRunnerTokenByHash(ctx context.Context, hash string) (*model.RunnerToken, error)
	RevokeRunnerTokens(ctx context.Context, runnerID string, at time.Time) (int, error)

	// Mutation history (append-only per-field audit log).
	RecordMutation(ctx context.Context, mut *model.MutationRow) error
	ListMutations(ctx context.Context, beadID string) ([]*model.MutationRow, error)
	// GetMutation returns one mutation_history row by primary key.
	// Returns sql.ErrNoRows when the id does not exist.
	GetMutation(ctx context.Context, id int64) (*model.MutationRow, error)

	// Operator-ownership history (R4 Option C, migration 014). Returns rows
	// from the bead_operator_history VIEW for one bead, newest first.
	// Powers the beads-viewer ownership tab (R5 §2 row S4); the VIEW hides
	// the field='operated_by' filter and the source-kind classification so
	// every reader targets one stable shape.
	GetOperatorHistory(ctx context.Context, beadID string) ([]*model.OperatorHistoryRow, error)

	// Dibs rule support (R3 §4 / R4 §7). GetBeadOperator returns the current
	// operator as the canonical "kind:subject" alias string (resolved via
	// JOIN to operators on beads.operated_by_id). UpdateBeadOperator flips
	// beads.operated_by_id by FK id (callers resolve through UpsertOperator
	// or the resolver before calling). Both treat a missing / soft-deleted
	// bead as "" / no-op so the dibs hot path stays best-effort.
	GetBeadOperator(ctx context.Context, beadID string) (string, error)
	UpdateBeadOperator(ctx context.Context, beadID string, operatorID int64) error

	// Escalation replay idempotency (Phase 2 2-replica spike, gd-XxrObXvc8r).
	// ClaimEscalationReplay atomically claims exclusive right to replay one
	// escalation's approved operation via a guarded UPDATE on the bead's
	// `replay_in_progress` field — the guard (only claim when the field is not
	// already "true") and the write happen in one statement, so two concurrent
	// approve requests (double-click, client retry, or two requests landing on
	// two different replicas) can never both win the claim. Returns
	// claimed=false, err=nil when the bead is missing/deleted or another
	// caller already holds the claim; the caller's normal response is to skip
	// its own replay attempt rather than treat that as an error.
	// ReleaseEscalationReplayClaim clears the claim so a later retry (e.g.
	// after a replay failure) can claim again; it is idempotent and does not
	// error when the claim is already clear or the bead is missing.
	ClaimEscalationReplay(ctx context.Context, beadID string) (claimed bool, err error)
	ReleaseEscalationReplayClaim(ctx context.Context, beadID string) error

	// ClaimEscalationExecution atomically records which executor runs an
	// approved escalation (P7 Task 12, spec D10) in fields.execution_dispatch.
	// The guard (only when execution_dispatch is unset or empty) and the write
	// are one statement. Returns claimed=false and the value already there
	// when another approve won; (false, "", nil) when the bead is missing.
	ClaimEscalationExecution(ctx context.Context, beadID, dispatch string) (claimed bool, current string, err error)

	// Scrub-request execution idempotency (gd-P6uCMzQqU9). The approve
	// handler's approval_status/executed_at fields are a check-then-act read,
	// not an atomic claim: two concurrent /approve calls on the same
	// fork-mode scrub-request can both observe executed_at=="" and both
	// proceed to run ScrubTaint, creating two successor beads. Mirrors
	// ClaimEscalationReplay exactly — a single guarded UPDATE on the bead's
	// `scrub_execution_claimed` field, so the guard and the write happen in
	// one statement and no two concurrent callers can both win. Returns
	// claimed=false, err=nil when the bead is missing/deleted or another
	// caller already holds the claim; ReleaseScrubExecutionClaim clears the
	// claim (idempotent) so a retry after an execution failure can claim
	// again.
	ClaimScrubExecution(ctx context.Context, beadID string) (claimed bool, err error)
	ReleaseScrubExecutionClaim(ctx context.Context, beadID string) error

	// CASBeadStatus atomically transitions a bead's status column FROM one
	// value TO another in a single `UPDATE beads SET status = $to WHERE id =
	// $id AND status = $from` — and touches ONLY status/updated_at, never
	// `fields`. This is the non-clobbering sibling of MergeBeadFields for the
	// status column: the pattern it replaces (GetBead, mutate bead.Status in
	// Go, UpdateBead(bead)) writes the WHOLE row back, including whatever
	// `fields` snapshot GetBead happened to return — so a concurrent
	// field-level write (e.g. ClaimScrubExecution's guarded UPDATE) that
	// lands between the GetBead and the UpdateBead is silently clobbered
	// (gd-P6uCMzQqU9, second merge desk send-back on PR #169: the approve
	// handler's open->in_progress transition did exactly this and could wipe
	// a just-taken scrub_execution_claimed claim, or resurrect a bead a
	// racing write had just closed). Returns changed=false, err=nil (not an
	// error) when the bead is missing/deleted or its status is not `from` —
	// the caller's normal response to a lost race is to skip the local
	// status-changed side effects (event publish), not treat it as a
	// failure.
	CASBeadStatus(ctx context.Context, id string, from, to model.Status) (changed bool, err error)

	// TryAdvisoryLock attempts to acquire a Postgres transaction-scoped
	// advisory lock identified by key (pg_try_advisory_xact_lock) and, only
	// if acquired, runs fn while holding it. The lock is released
	// automatically when fn returns — by the transaction commit on success or
	// the rollback on error/panic-unwind — so there is no separate unlock
	// call and no way to leak the lock past this call. acquired=false with a
	// nil error means another session already holds key; that is normal
	// contention, not a failure — callers use it to skip a periodic sweep
	// tick that a different replica is already running (Phase 2 2-replica
	// spike, gd-XxrObXvc8r: decision-aging and audit-retention sweeps use
	// distinct keys so only one replica runs a given tick of either).
	TryAdvisoryLock(ctx context.Context, key int64, fn func(ctx context.Context) error) (acquired bool, err error)

	// Operators / operator_aliases (Option B pivot, ADR
	// docs/adr/human-attribution-identity-v2.md §3.1, §5.1). These power
	// the resolver's dual-write phase: the cold path inserts a new operator
	// (or returns the existing id), and the hot path looks up the FK by
	// (kind, subject) on every audit-bearing write.
	//
	// UpsertOperator inserts (kind, subject) into operators if absent, then
	// returns the id. Idempotent — the unique (kind, subject) constraint
	// makes ON CONFLICT DO UPDATE ... RETURNING id reliable. Used only on
	// hot-path miss (first-ever sighting of an operator); thereafter the
	// alias index serves the lookup. Also writes the canonical alias row
	// transactionally so future lookups land in the hot path.
	UpsertOperator(ctx context.Context, kind, subject string) (int64, error)

	// LookupOperatorAlias returns the operator_id for a known (kind, subject)
	// alias, or sql.ErrNoRows when no alias exists. The hot path of every
	// audit write — runs against the (kind, subject) PK so it is a single
	// index probe.
	LookupOperatorAlias(ctx context.Context, kind, subject string) (int64, error)

	// LookupOperatorByAlias returns the operator_id and the parent operator's
	// canonical kind for a known (kind, subject) alias, or sql.ErrNoRows when
	// no alias exists. Used by the viewer-identity resolver
	// (kbeads inbox v1 G2 / inbox-constellation-design.md §1.4) to look up
	// the canonical operator id for an OIDC subject AND check that the
	// alias does not point at a sentinel (`kind ∈ {'system','fleet'}`)
	// operator — a property the resolver enforces per §1.5.
	LookupOperatorByAlias(ctx context.Context, kind, subject string) (operatorID int64, operatorKind string, err error)

	// UpsertOperatorAlias inserts or updates an alias row pointing at
	// operatorID. Source records who wrote the alias ("resolver", "migration",
	// "slack-resync", …). On (kind, subject) conflict the row's operator_id
	// is updated — this is the consolidation-in-place affordance the schema
	// comment in migration 015 calls out.
	UpsertOperatorAlias(ctx context.Context, operatorID int64, kind, subject, source string) error

	// ListOperatorAliasesByKind returns every (operator_id, subject) pair in
	// operator_aliases for the given kind, joined with the operators row's
	// current display/email so callers can short-circuit no-op refreshes.
	// Used by the Slack users.info resync job (ADR §7 follow-up #1).
	ListOperatorAliasesByKind(ctx context.Context, kind string) ([]OperatorAliasIdentity, error)

	// PruneOperatorAliases deletes every operator_aliases row written under
	// source whose (kind, subject) is not in keep, and returns the rows it
	// deleted. It only ever touches rows whose source column equals source, so
	// aliases written by the OIDC login path, a migration or an admin link
	// are never revoked by a reconcile of another source. This is the
	// revocation half of the user-bead alias bridge (gd-uAvleAZUvV): nothing
	// else deletes alias rows, so an identity removed from a user bead would
	// otherwise resolve to its operator forever. Does not touch operators or
	// audit-FK rows.
	PruneOperatorAliases(ctx context.Context, source string, keep []OperatorAliasKey) ([]OperatorAliasKey, error)

	// UpdateOperatorIdentity rewrites operators.display and operators.email
	// for the given operator id and bumps updated_at when either changes.
	// Idempotent — the same (display, email) write twice in a row is a no-op.
	// Returns true when a row was matched (false on missing operator id, e.g.
	// after a merge that deleted the row).
	UpdateOperatorIdentity(ctx context.Context, operatorID int64, display, email string) (bool, error)

	// MergeOperators repoints every operated_by_id reference from oldID to
	// newID across the five audit-FK tables (beads, comments, deps, events,
	// mutation_history). Used by the gb identity link-operator flow to merge
	// a legacy slack:U… operator into the caller's oidc:<email> operator
	// (ADR docs/adr/human-attribution-identity-v2.md §3.1). Returns the total
	// number of rows repointed across all tables. Caller is responsible for
	// repointing aliases (UpsertOperatorAlias) so future lookups land on the
	// surviving operator.
	MergeOperators(ctx context.Context, oldID, newID int64) (int64, error)

	// ForgetOperator soft-deletes an operator for a right-to-be-forgotten
	// request (ka-0000005150): it stamps forgotten_at (the redaction marker
	// migration 030's ResolveOperators consumes), nulls slug so the row drops
	// out of the /people directory feed, retires the operator's live
	// operator_slug_aliases rows (so /u/{slug} still resolves to the redacted
	// operator rather than 404ing), and records the acting principal + reason
	// (opts.Actor / opts.Reason) as the auditable event of the forget. The
	// opaque operator_id and every operated_by_id FK are left intact so
	// historical metric series stay attributable to the now-anonymous actor.
	//
	// This is the API/CLI primitive that replaces the raw
	// `UPDATE operators SET forgotten_at=now(), slug=NULL` the 2026-06-16
	// cleanup (ka-0000006421) had to run by hand.
	//
	// Idempotent: forgetting an already-forgotten operator is a no-op that
	// returns AlreadyForgotten=true with the ORIGINAL forgotten_at (the
	// timestamp and forgotten_by/reason are never re-stamped).
	//
	// Guard: when the operator owns more than the audit-history threshold
	// (opts.AuditGuardThreshold, default DefaultForgetOperatorAuditGuard) and
	// opts.Confirm is false, it returns a *OperatorForgetGuardError WITHOUT
	// mutating. Returns *OperatorNotFoundError when no such operator exists.
	ForgetOperator(ctx context.Context, id int64, opts ForgetOperatorOptions) (*ForgetOperatorResult, error)

	// Operator slugs (per-user homepage M1, ka-0000000022 / ka-0000006114).
	// Migration 028 adds operators.slug + operator_slug_aliases; these
	// methods power the /u/{slug} viewer route and the /me redirect.
	//
	// GetOperatorBySlug returns the operator id for a current OR retired
	// slug (resolved through operator_slug_aliases so legacy links keep
	// working after a rename). Returns sql.ErrNoRows when no alias exists.
	GetOperatorBySlug(ctx context.Context, slug string) (int64, error)

	// GetOperatorSlug returns the current canonical slug for the given
	// operator id. Returns sql.ErrNoRows when the operator has no slug.
	GetOperatorSlug(ctx context.Context, operatorID int64) (string, error)

	// ListOperatorsWithSlugs returns operators that have a canonical slug
	// (the only ones routable via /u/{slug}), ordered by slug ascending,
	// for the public /people directory (M2 §4, ka-0000000340). Limit
	// bounds the result; 0 falls back to a default of 500.
	ListOperatorsWithSlugs(ctx context.Context, limit int) ([]OperatorDirectoryEntry, error)

	// OperatorDirectoryStats returns real per-operator aggregates for the
	// /people directory (ka-0000001390): a GROUP BY operated_by_id open-count
	// + a human-authored max(updated_at) over the whole beads table, plus a
	// bounded, age-limited "recently created" chip list per operator keyed by
	// operated_by_id. This replaces the beads-viewer's old 5000-bead in-memory
	// sample, whose per-row counts and header total were sampling artifacts
	// against a 245K+ open-bead fleet.
	//
	// recentPerOperator caps how many recent chips each operator gets
	// (<=0 falls back to a default). recentCeiling is a hard ceiling on the
	// TOTAL number of chip rows returned across all operators so the chip
	// query can never fan out unbounded (<=0 falls back to a default).
	// recentWindowHours bounds how far back a chip may be created (<=0 falls
	// back to a default) so stale work cannot read as current. The open-count
	// aggregate is a true full-table GROUP BY and is NOT subject to any cap.
	//
	// LastActive reflects HUMAN-authored bead types only — type='agent',
	// 'session' and 'trigger' rows are machine scaffolding that heartbeat on
	// their own and would otherwise report a person as active when they did
	// nothing (ka-0000001991). The AgentsActive / GardenersActive counts DO
	// still see those rows: the fleet chips are about the machines, the
	// recency is about the human.
	OperatorDirectoryStats(ctx context.Context, recentPerOperator, recentCeiling, recentWindowHours int) (*OperatorDirectoryStats, error)

	// ResolveOperators turns a set of opaque operator_id metric labels into
	// human-presentable identities, SERVER-SIDE, in one round trip (ka-0000004824,
	// customer-1 telemetry plan §4). It is the single resolution point for the
	// chart backend's operator-keyed engagement / security dashboards — metric
	// series carry only operator_id, and display/email/slug are joined here from
	// the operators table, never emitted as a label.
	//
	// Right-to-be-forgotten: a soft-deleted operator (forgotten_at IS NOT NULL)
	// resolves with Forgotten=true, Email=ForgottenOperatorEmail, Display="" so
	// every dashboard redacts at once without rewriting any historical series.
	//
	// Only ids that exist in operators appear in the result map; unknown ids
	// (e.g. the unattributed sentinel "0" when no operators(id=0) row exists)
	// are simply absent so the caller can apply its own fallback label. Passing
	// an empty slice returns an empty, non-nil map.
	ResolveOperators(ctx context.Context, ids []int64) (map[int64]ResolvedOperator, error)

	// Bead followers (ka-0000004114). Many-to-many "I want this bead in your
	// inbox" join table introduced by migration 023. Forwarding a bead is
	// additive — it does not change `operated_by_id`. The inbox query unions
	// `operated_by_id = $me` with rows from this table where `operator_id = $me`.
	//
	// AddFollower idempotently inserts (beadID, operatorID) and records the
	// forwarder + optional note. Re-forwarding the same pair refreshes
	// forwarded_by + note (the latest forwarder wins) without raising a
	// conflict so the API is safe to retry.
	AddFollower(ctx context.Context, beadID string, operatorID int64, forwardedBy *int64, note string) error

	// RemoveFollower deletes the (beadID, operatorID) row. Returns true when a
	// row was matched (false = already absent — caller-visible idempotency).
	RemoveFollower(ctx context.Context, beadID string, operatorID int64) (bool, error)

	// ListFollowers returns the followers of one bead. Rows are joined with
	// the parent operators row so callers get canonical (kind, subject,
	// display, email) without a second round trip.
	ListFollowers(ctx context.Context, beadID string) ([]*model.BeadFollower, error)

	// ListFollowedBeadIDs returns the bead IDs the given operator follows
	// (the inbox UNION predicate's right side). Bounded by the caller's
	// limit; 0 means no limit. Newest-forwarded-first.
	ListFollowedBeadIDs(ctx context.Context, operatorID int64, limit int) ([]string, error)

	// UI telemetry (ka-0000006089). RecordUITelemetry inserts one row of
	// click telemetry posted by the static incident_intel_telemetry.js
	// module via POST /v1/ui/telemetry. ListUITelemetry returns rows in
	// reverse-chronological order (newest first), filtered by Event/Since
	// and capped by Limit.
	RecordUITelemetry(ctx context.Context, row *model.UITelemetryRow) error
	ListUITelemetry(ctx context.Context, filter model.UITelemetryFilter) ([]*model.UITelemetryRow, error)

	// Slow-tool-call ledger (plan agent-tool-latency-attribution.md P2, bead
	// gd-sWwPnwkZtw) — the record-plane twin of the bounded
	// gardenio_tool_latency_seconds / gardenio_bash_cmd_latency_seconds
	// histograms. InsertSlowToolCall writes one call over the configured
	// threshold with its full redacted argv; ListSlowToolCalls returns the N
	// slowest calls in a time window (duration_ms DESC), not the newest.
	InsertSlowToolCall(ctx context.Context, row *model.SlowToolCallRow) error
	ListSlowToolCalls(ctx context.Context, filter model.SlowToolCallFilter) ([]*model.SlowToolCallRow, error)
	// DeleteSlowToolCallsBefore is the retention reaper's primitive: it
	// deletes up to limit rows whose ts is older than cutoff (oldest first,
	// via idx_slow_tool_calls_ts) and returns how many were removed. Callers
	// loop until a batch comes back short; limit <= 0 uses a store default.
	DeleteSlowToolCallsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)

	// DeleteBeadUpdatedEventsBefore and DeleteFieldsMutationsBefore are the
	// history-retention sweep's primitives (gd-1F8R0SQtiQ). Each deletes up to
	// limit rows older than cutoff on beads whose type is in types:
	// beads.bead.updated events, and field='fields' mutation_history rows.
	// Every other topic and field is kept. Callers loop until a batch comes
	// back short; limit <= 0 uses a store default.
	DeleteBeadUpdatedEventsBefore(ctx context.Context, types []string, cutoff time.Time, limit int) (int64, error)
	DeleteFieldsMutationsBefore(ctx context.Context, types []string, cutoff time.Time, limit int) (int64, error)

	// Push device registry (migration 053, Lands 4.7b gd-a7U4AgwgCA). One row
	// per app install, owned by the human who registered it.
	// UpsertPushDevice inserts the row or, for a token already registered,
	// moves it to d.UserEmail and refreshes platform, bundle id and
	// last_seen_at; it fills CreatedAt/LastSeenAt from the stored row.
	// ListPushDevices returns matching rows ordered by user_email, token.
	// DeletePushDevice removes the token, only when it belongs to userEmail
	// unless userEmail is empty; it returns true when a row was removed.
	UpsertPushDevice(ctx context.Context, d *model.PushDevice) error
	ListPushDevices(ctx context.Context, filter model.PushDeviceFilter) ([]*model.PushDevice, error)
	DeletePushDevice(ctx context.Context, token, userEmail string) (bool, error)

	// Gate operations
	UpsertGate(ctx context.Context, agentBeadID, gateID string) error
	MarkGateSatisfied(ctx context.Context, agentBeadID, gateID string) error
	ClearGate(ctx context.Context, agentBeadID, gateID string) error
	IsGateSatisfied(ctx context.Context, agentBeadID, gateID string) (bool, error)
	ListGates(ctx context.Context, agentBeadID string) ([]model.GateRow, error)

	// Transaction support
	RunInTransaction(ctx context.Context, fn func(tx Store) error) error

	// Lifecycle
	Ping(ctx context.Context) error
	Close() error
}
