// Package client provides a transport-agnostic interface for the kbeads service
// and an HTTP/JSON implementation that talks to the kbeads REST API.
package client

import (
	"context"
	"encoding/json"
	"time"

	"gardenio/kbeads/internal/model"
)

// BeadsClient is the interface that all kbeads CLI commands use to communicate
// with the beads server. It is implemented by HTTPClient (default) and can be
// backed by any transport.
type BeadsClient interface {
	// Bead CRUD
	CreateBead(ctx context.Context, req *CreateBeadRequest) (*model.Bead, error)
	GetBead(ctx context.Context, id string) (*model.Bead, error)
	ListBeads(ctx context.Context, req *ListBeadsRequest) (*ListBeadsResponse, error)
	UpdateBead(ctx context.Context, id string, req *UpdateBeadRequest) (*model.Bead, error)
	// EditBeadSubField edits ONE key of a JSON-map field (channel_roles) or ONE
	// element of a CSV field (slack_channel) WITHOUT clobbering the field's
	// other entries — the non-clobbering counterpart to UpdateBead's whole-field
	// replace of a shared aggregate (ka-0000007767). HTTP-only (POST
	// /v1/beads/{id}/fields/{field}/edit); the gRPC transport returns an
	// unsupported error, mirroring the array-field primitives.
	EditBeadSubField(ctx context.Context, id, field string, req *EditSubFieldRequest) (*model.Bead, error)
	CloseBead(ctx context.Context, id string, closedBy string) (*model.Bead, error)
	DeleteBead(ctx context.Context, id string) error
	RestoreBead(ctx context.Context, id string) (*model.Bead, error)

	// Dependencies
	AddDependency(ctx context.Context, req *AddDependencyRequest) (*model.Dependency, error)
	// RemoveDependency deletes one dependency edge. `removed` is false only when
	// the daemon affirmatively reported that no row matched; a 0-match is not an
	// error, so idempotent callers may discard it. A daemon that predates the
	// rows-affected signal reports removed=true (assume-success), so an older
	// daemon can never make a real removal look like a no-op (ka-0000001966).
	RemoveDependency(ctx context.Context, beadID, dependsOnID, depType string) (removed bool, err error)
	GetDependencies(ctx context.Context, beadID string) ([]*model.Dependency, error)
	GetReverseDependencies(ctx context.Context, beadID string) ([]*model.Dependency, error)

	// References (ka-0000001713 IDs parsed from bead text). See
	// kbeads/docs/design-bead-references.md § 5a. relationshipType applies
	// the typed-mention filter (T6, kbeads/docs/research-typed-mention-syntax.md):
	// nil = no filter; non-nil "" = untyped only (relationship_type IS NULL);
	// non-nil "<verb>" = exact match. The HTTP client surfaces this via
	// ?type= on the wire; the gRPC client does not yet (proto change pending).
	ListBeadsMentioning(ctx context.Context, beadID string, limit, offset int, relationshipType *string) (*ListReferencesResponse, error)
	ListBeadReferencesFrom(ctx context.Context, beadID string, limit, offset int, relationshipType *string) (*ListReferencesResponse, error)

	// Labels
	AddLabel(ctx context.Context, beadID, label string) (*model.Bead, error)
	// RemoveLabel deletes one label. `removed` is false only when the daemon
	// affirmatively reported that no row matched; a 0-match is not an error, so
	// idempotent callers may discard it. A daemon that predates the
	// rows-affected signal reports removed=true (assume-success), so an older
	// daemon can never make a real removal look like a no-op (ka-0000000424).
	RemoveLabel(ctx context.Context, beadID, label string) (removed bool, err error)
	GetLabels(ctx context.Context, beadID string) ([]string, error)

	// Comments
	AddComment(ctx context.Context, beadID, author, text string) (*model.Comment, error)
	GetComments(ctx context.Context, beadID string) ([]*model.Comment, error)
	// DeleteComment removes a single comment by id. Idempotent: returns
	// (false, nil) if the row was already gone, (true, nil) on a real delete.
	DeleteComment(ctx context.Context, beadID string, commentID int64) (bool, error)
	// TrimComments deletes all but the most recent keepLast comments on a
	// bead, optionally scoped to a single author. When authorFilter is
	// non-empty, both the keep window and the delete are restricted to
	// comments by that author — comments by other authors are never
	// touched. Returns the ids of deleted rows in chronological order.
	TrimComments(ctx context.Context, beadID string, keepLast int, authorFilter string) ([]int64, error)

	// Mutation history (append-only per-field audit log).
	GetMutations(ctx context.Context, beadID string) ([]*model.MutationRow, error)
	// UndoMutation reverses one mutation_history row. Admin-only on the
	// server. Returns the bead after the reversal and the original mutation
	// row that was undone.
	UndoMutation(ctx context.Context, mutationID int64) (*UndoMutationResponse, error)

	// Fleet
	TransferBead(ctx context.Context, beadID, toFleet string) (*model.Bead, error)

	// Events
	GetEvents(ctx context.Context, beadID string) ([]*model.Event, error)
	// ListGlobalEvents returns all events across all beads in [since, until]
	// (inclusive), in chronological order (created_at ASC, id ASC), paginated
	// by event id via cursor (only events with id > cursor are returned).
	// Backs the `kd events backfill` replay. HTTP-only; the gRPC transport
	// returns an unsupported error (see GetMutations for the same pattern).
	ListGlobalEvents(ctx context.Context, req *ListGlobalEventsRequest) (*ListGlobalEventsResponse, error)

	// Config
	SetConfig(ctx context.Context, key string, value json.RawMessage) (*model.Config, error)
	GetConfig(ctx context.Context, key string) (*model.Config, error)
	ListConfigs(ctx context.Context, namespace string) ([]*model.Config, error)
	DeleteConfig(ctx context.Context, key string) error

	// Hooks
	EmitHook(ctx context.Context, req *EmitHookRequest) (*EmitHookResponse, error)

	// Gates
	ListGates(ctx context.Context, agentBeadID string) ([]model.GateRow, error)
	SatisfyGate(ctx context.Context, agentBeadID, gateID string) error
	ClearGate(ctx context.Context, agentBeadID, gateID string) error

	// Roster
	GetAgentRoster(ctx context.Context, staleThresholdSecs int) (*AgentRosterResponse, error)

	// Operators
	// LookupOperatorByAlias resolves a (kind, subject) operator_aliases pair
	// (e.g. "oidc", "alice@example.com") to an operators(id). Found is false
	// (not an error) when no alias row exists yet. HTTP-only; the gRPC
	// transport returns an unsupported error (no operators service in the
	// proto).
	LookupOperatorByAlias(ctx context.Context, kind, subject string) (*LookupOperatorByAliasResponse, error)

	// Batch
	BatchGetBeads(ctx context.Context, ids []string) ([]BatchGetResult, error)

	// Semantic search
	FindSimilar(ctx context.Context, beadID string, limit int, minScore float64) (*FindSimilarResponse, error)
	SemanticSearch(ctx context.Context, req *SemanticSearchRequest) (*SemanticSearchResponse, error)

	// Embedding maintenance (admin-only; used by `kd embed sync`).
	UpsertEmbedding(ctx context.Context, req *UpsertEmbeddingRequest) error
	GetEmbeddingHashes(ctx context.Context, beadIDs []string) (map[string]string, error)
	SyncBatchEmbeddings(ctx context.Context, req *SyncBatchEmbeddingsRequest) (*SyncBatchEmbeddingsResponse, error)

	// Destructive embedding-space migration (admin-only; used by `kd embed migrate`).
	MigrateEmbeddings(ctx context.Context, req *MigrateEmbeddingsRequest) error
	ReindexEmbeddings(ctx context.Context) error

	// References backfill (admin-only; used by `kd admin reextract-references`).
	ReextractReferences(ctx context.Context, req *ReextractReferencesRequest) (*ReextractReferencesResponse, error)

	// search_tsv FTS backfill (admin-only; used by `kd admin backfill-search-tsv`).
	BackfillSearchTSV(ctx context.Context, req *BackfillSearchTSVRequest) (*BackfillSearchTSVResponse, error)

	// SetStewardDelegation sets a tenant land's land_steward_approves through
	// POST /v1/lands/{land}/steward-delegation (the land owner or a
	// gardenio-admin; used by `kd admin backfill-land-steward-approves`).
	SetStewardDelegation(ctx context.Context, land string, ops []string) error

	// category materialization backfill (admin-only; used by `kd admin backfill-category`).
	BackfillCategory(ctx context.Context, req *BackfillCategoryRequest) (*BackfillCategoryResponse, error)
	BackfillProjectLabels(ctx context.Context, req *BackfillProjectLabelsRequest) (*BackfillProjectLabelsResponse, error)

	// operators-FK backfill (admin-only; used by `kd admin backfill-operators`).
	BackfillOperators(ctx context.Context, req *BackfillOperatorsRequest) (*BackfillOperatorsResponse, error)

	// Legacy-operated-by cleanup (admin-only; used by
	// `kd admin backfill-operated-by-legacy`). See research note
	// gardenio/docs/research/legacy-operated-by-matt-cleanup.md.
	BackfillOperatedByLegacy(ctx context.Context, req *BackfillOperatedByLegacyRequest) (*BackfillOperatedByLegacyResponse, error)
	BackfillSlackUserID(ctx context.Context, req *BackfillSlackUserIDRequest) (*BackfillSlackUserIDResponse, error)

	// op-<id> → readable slug backfill (admin-only; used by
	// `kd admin backfill-operator-slugs` — ka-0000001149).
	BackfillOperatorSlugs(ctx context.Context, req *BackfillOperatorSlugsRequest) (*BackfillOperatorSlugsResponse, error)

	// Operator right-to-be-forgotten soft-delete (admin-only; used by
	// `kd admin forget-operator` — ka-0000005150).
	ForgetOperator(ctx context.Context, id int64, req *ForgetOperatorRequest) (*ForgetOperatorResponse, error)

	// Matt-default owner rebind (admin-only; used by
	// `kd admin backfill-owner --rebind-matt-default` — ka-0000006961).
	BackfillOwner(ctx context.Context, req *BackfillOwnerRequest) (*BackfillOwnerResponse, error)

	// Three-phase empty-owner backfill (admin-only; used by
	// `kd admin backfill-empty-owner` — ka-0000007029).
	BackfillEmptyOwner(ctx context.Context, req *BackfillEmptyOwnerRequest) (*BackfillEmptyOwnerResponse, error)

	// Per-user-homepage M2 agent operated_by_id backfill (admin-only;
	// used by `kd admin backfill-operated-by-agents` — sub-item #1 of
	// ka-0000006817).
	BackfillOperatedByAgents(ctx context.Context, req *BackfillOperatedByAgentsRequest) (*BackfillOperatedByAgentsResponse, error)

	// Parent-child reconstruction over agent-orphan beads (admin-only;
	// used by `kd admin backfill-parent-child-deps` — ka-0000006775).
	BackfillParentChildDeps(ctx context.Context, req *BackfillParentChildDepsRequest) (*BackfillParentChildDepsResponse, error)
	RepairReversedParentChildDeps(ctx context.Context, req *RepairReversedParentChildDepsRequest) (*RepairReversedParentChildDepsResponse, error)

	// Blocks-cycle audit (admin-only; used by `kd admin audit-blocks-cycles`
	// — ka-0000007984). Scans the whole blocks graph for cycles.
	AuditBlocksCycles(ctx context.Context) (*AuditBlocksCyclesResponse, error)

	// Triggered-by auto-link backfill scanner (admin-only; used by
	// `kd admin backfill-triggered-by-deps` — ka-0000000004 / M4).
	BackfillTriggeredByDeps(ctx context.Context, req *BackfillTriggeredByDepsRequest) (*BackfillTriggeredByDepsResponse, error)

	// Close legacy trigger-audit beads (admin-only; used by
	// `kd admin close-trigger-audit-beads` — ka-0000006341).
	CloseTriggerAuditBeads(ctx context.Context, req *CloseTriggerAuditBeadsRequest) (*CloseTriggerAuditBeadsResponse, error)

	// Escalations (ka-0000007156). GetEscalation/ListEscalations read the
	// operator escalation queue; Approve/Reject/CaptureEscalationResult are the
	// resolve verbs that hit POST /v1/escalations/{id}/{approve,reject,result}.
	// The resolve endpoints are RoleOperator-gated server-side, so an agent
	// token gets 403 by design — a crew agent needing a work-blocking answer
	// files a `gb decision` instead. HTTP-only; the gRPC transport returns an
	// unsupported error (no escalation service in the proto).
	GetEscalation(ctx context.Context, id string) (*EscalationDetail, error)
	ListEscalations(ctx context.Context, opts ListEscalationsOptions) ([]EscalationDetail, error)
	ApproveEscalation(ctx context.Context, id string, req ApproveEscalationRequest) error
	RejectEscalation(ctx context.Context, id string, req RejectEscalationRequest) error
	ClaimEscalation(ctx context.Context, id string) (*model.Bead, error)
	HandUpEscalation(ctx context.Context, id string, req HandUpEscalationRequest) (*model.Bead, error)
	CaptureEscalationResult(ctx context.Context, id string, req CaptureEscalationResultRequest) error

	// Releases (releases.go): a project's releases and where they run.
	// HTTP-only; the gRPC transport returns an unsupported error.
	CreateRelease(ctx context.Context, req CreateReleaseRequest) (*Release, error)
	GetRelease(ctx context.Context, ref, project string) (*Release, error)
	ListReleases(ctx context.Context, project, status string, limit int) ([]Release, error)
	DeployRelease(ctx context.Context, ref, project string, req DeployReleaseRequest) (*Release, error)
	RepairReleaseDeployments(ctx context.Context, project string, dryRun bool) (*RepairDeploymentsResult, error)

	// Scrub-request approve/reject (docs/adr/taints.md §6.3, gd-DyWyWFxnzC):
	// POST /v1/scrub-requests/{id}/{approve,reject}, RoleOperator-gated and
	// OIDC-only server-side (same posture as the escalation resolve verbs
	// above). Scrub-requests otherwise read/write through the generic bead
	// CRUD surface (GetBead/ListBeads/CreateBead) — only approve/reject need
	// dedicated verbs, to keep the server-derived-approver invariant out of
	// reach of the generic PATCH path. HTTP-only; the gRPC transport returns
	// an unsupported error.
	ApproveScrubRequest(ctx context.Context, id string, req ApproveScrubRequestRequest) (*model.ScrubAuditArtifact, error)
	RejectScrubRequest(ctx context.Context, id string, req RejectScrubRequestRequest) error

	// Taints (docs/adr/taints.md §7): grant/revoke against taint_grants,
	// explicit apply/show against bead_taints. HTTP-only — taints get no
	// gRPC RPCs (ADR §13 OQ2); the gRPC transport returns an unsupported
	// error, mirroring GetMutations/ListGlobalEvents.
	GrantTaint(ctx context.Context, name string, req *GrantTaintRequest) (*model.TaintGrant, error)
	// RevokeTaint deletes one grant. `removed` is false only when the daemon
	// affirmatively reported that no row matched — the same idempotent-delete
	// signal shape as RemoveLabel/RemoveDependency.
	RevokeTaint(ctx context.Context, name, principal string) (removed bool, err error)
	ApplyTaint(ctx context.Context, beadID, taintName string) (*model.BeadTaint, error)
	GetBeadTaints(ctx context.Context, beadID string) ([]*model.BeadTaint, error)
	// RequestTaintGrant files a `grant`-type escalation addressed to the
	// taint's owner team (ADR docs/adr/taints.md §9.2/§9.4), so a developer
	// can ask for a grant without knowing who owns the taint. Returns the
	// same *EscalationCreated shape as the X-Escalate retry on a taint write
	// denial produces.
	RequestTaintGrant(ctx context.Context, name string, req *RequestTaintGrantRequest) (*EscalationCreated, error)

	// Taints (task gd-sygX3b8TAg; ADR docs/adr/taints.md §7). CreateTaint is
	// RoleAdmin-gated server-side; ListTaints/GetTaint are RoleViewer-gated
	// with owner-team/admin-only visibility into scrub_policy. HTTP-only —
	// the gRPC transport returns an unsupported error (no taint service in
	// the proto).
	CreateTaint(ctx context.Context, req *CreateTaintRequest) (*TaintView, error)
	ListTaints(ctx context.Context) (*ListTaintsResponse, error)
	GetTaint(ctx context.Context, name string) (*TaintView, error)

	// Scrub requests (docs/adr/taints.md §6.2/§7). CreateScrubRequest is the
	// grant-holder-facing create surface backing `kd scrub request`;
	// RoleOperator-gated server-side, with the grant-holder check enforced
	// inside applyScrubRequestGuard. HTTP-only — no gRPC RPC exists for this.
	CreateScrubRequest(ctx context.Context, req *CreateScrubRequestRequest) (*ScrubRequestView, error)

	// Health
	Health(ctx context.Context) (string, error)

	// Lifecycle
	Close() error
}

// EmitHookRequest holds parameters for emitting a hook event.
type EmitHookRequest struct {
	AgentBeadID     string `json:"agent_bead_id"`
	HookType        string `json:"hook_type"`
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
	// SessionID is the harness-neutral session id; Harness is "claude" or "codex".
	SessionID string `json:"session_id,omitempty"`
	Harness   string `json:"harness,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Actor     string `json:"actor,omitempty"`
	ToolName  string `json:"tool_name,omitempty"` // tool name from the agent CLI (e.g. "Bash", "Read")
}

// EmitHookResponse is the response from EmitHook.
type EmitHookResponse struct {
	Block    bool     `json:"block,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Inject   string   `json:"inject,omitempty"`
}

// LookupOperatorByAliasResponse is the response from LookupOperatorByAlias.
type LookupOperatorByAliasResponse struct {
	OperatorID   int64  `json:"operator_id"`
	OperatorKind string `json:"operator_kind"`
	Found        bool   `json:"found"`
}

// AgentRosterResponse is the response from GetAgentRoster.
type AgentRosterResponse struct {
	Actors         []RosterEntry   `json:"actors"`
	UnclaimedTasks []UnclaimedTask `json:"unclaimed_tasks"`
}

// RosterEntry represents one agent in the roster.
type RosterEntry struct {
	Actor     string  `json:"actor"`
	TaskID    string  `json:"task_id,omitempty"`
	TaskTitle string  `json:"task_title,omitempty"`
	EpicID    string  `json:"epic_id,omitempty"`
	EpicTitle string  `json:"epic_title,omitempty"`
	IdleSecs  float64 `json:"idle_secs"`
	LastEvent string  `json:"last_event,omitempty"`
	ToolName  string  `json:"tool_name,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	CWD       string  `json:"cwd,omitempty"`
	Reaped    bool    `json:"reaped,omitempty"`
}

// UnclaimedTask is an in_progress bead with no assignee.
type UnclaimedTask struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Priority int    `json:"priority"`
}

// CreateBeadRequest holds parameters for creating a bead.
type CreateBeadRequest struct {
	Title string `json:"title"`
	// ShortTitle is the <=40-char human name (model.ShortTitleHelp). Surfaced
	// as `kd create --short-title`; the server normalizes and validates it.
	ShortTitle string `json:"short_title,omitempty"`
	// Heraldry is the optional arms, in any input form (bare emoji, placed,
	// canonical, with or without "on <colour>"); the server stores the
	// canonical form. Surfaced as `kd create --heraldry`.
	Heraldry    string          `json:"heraldry,omitempty"`
	Kind        string          `json:"kind,omitempty"`
	Type        string          `json:"type"`
	Description string          `json:"description,omitempty"`
	Notes       string          `json:"notes,omitempty"`
	Priority    int             `json:"priority"`
	Assignee    string          `json:"assignee,omitempty"`
	Owner       string          `json:"owner,omitempty"`
	Labels      []string        `json:"labels,omitempty"`
	CreatedBy   string          `json:"created_by,omitempty"`
	Fields      json.RawMessage `json:"fields,omitempty"`
	DueAt       *time.Time      `json:"due_at,omitempty"`
	DeferUntil  *time.Time      `json:"defer_until,omitempty"`
	// ParentBeadID, when set, makes the server wire a parent-child dep from
	// the new bead to this bead inside the same transaction as the create,
	// and skip the post-create static-actor auto-parent step (D5,
	// ka-0000007622). Use this whenever the caller has an explicit lineage
	// target (formula apply → molecule, `/formula pour` → thread bead, etc.)
	// so step beads don't accumulate a second auto-parent edge to the
	// agent's claimed task.
	ParentBeadID string `json:"parent_bead_id,omitempty"`
	// IdempotencyKey, when set, makes the create lookup-or-return: if a
	// non-deleted bead in the same project already carries this key, the
	// server returns that existing bead instead of inserting a duplicate
	// (and does not emit a second BeadCreated event). Lets a resumed pod
	// safely re-issue a create that may have already committed before a
	// crash. Wire field: proto field 15 in beads.v1.CreateBeadRequest.
	// See ka-0000002856.
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// SawOpenHash echoes the caller's acknowledgement of the current open
	// decision/escalation list for the dedup-ack gate (epic ka-0000004599).
	// After a create is rejected with a DedupAckRequiredError, the caller
	// re-submits with this set to that error's Hash. Serialized to the server
	// input field saw_open_hash. Only meaningful for decision/escalation
	// creates and only when BEADS_DEDUP_GATE_ENABLED is on server-side.
	SawOpenHash string `json:"saw_open_hash,omitempty"`

	// Force bypasses the server-side content-shape guards for the created
	// bead: the decision ack-only / no-fork guard and the decision/escalation
	// dedup-ack gate (epic ka-0000004599). Surfaced as `kd create --force`.
	// Each bypass is logged server-side for audit. Never a security boundary.
	Force bool `json:"force,omitempty"`
}

// ListBeadsRequest holds parameters for listing beads.
type ListBeadsRequest struct {
	Status []string `json:"status,omitempty"`
	Type   []string `json:"type,omitempty"`
	// ExcludeTypes drops beads whose type is in this list (type NOT IN).
	// Surfaced as `kd list --exclude-type`; the server maps it onto
	// model.BeadFilter.ExcludeTypes (HTTP only — the proto ListBeadsRequest
	// has no exclude_type field, so the gRPC client errors rather than
	// silently dropping the filter; see grpc.go ListBeads). This is the type
	// analogue of ExcludeLabels: it lets `kd list --mentions <epic>
	// --all-types --exclude-type message` drop the type=message narration
	// noise that otherwise dominates a "find related beads" scan
	// (ka-0000007843).
	ExcludeTypes []string `json:"exclude_types,omitempty"`
	Kind         []string `json:"kind,omitempty"`
	// Labels ANDs: a bead must carry ALL of these (one EXISTS per label).
	Labels []string `json:"labels,omitempty"`
	// LabelsAny ORs: a bead matches when it carries AT LEAST ONE of these.
	// Surfaced as `kd list --label-any`; the server routes it into the
	// user-facing model.BeadFilter.LabelsAnyUser so it never widens OIDC
	// project scope (ka-0000003307).
	LabelsAny []string `json:"labels_any,omitempty"`
	// ExcludeLabels drops beads carrying ANY of these labels (label NOT IN).
	// Surfaced as `kd list --exclude-label`; the server maps it onto
	// model.BeadFilter.ExcludeLabels (HTTP only — the proto BeadFilter has no
	// exclude_labels field, so the gRPC client errors rather than silently
	// dropping the filter; see grpc.go ListBeads).
	ExcludeLabels []string `json:"exclude_labels,omitempty"`
	Assignee      string   `json:"assignee,omitempty"`
	// Unassigned restricts results to beads with NO assignee (server-side
	// `assignee IS NULL OR assignee = <empty>`). Surfaced as `kd list --unassigned`
	// / `kd count --unassigned`; maps onto model.BeadFilter.Unassigned via
	// ?unassigned=true. HTTP only — the proto ListBeadsRequest has no
	// unassigned field, so the gRPC client errors rather than silently dropping
	// it (a dropped predicate returns every bead including CLAIMED ones, the
	// widen-on-drop footgun the ExcludeLabels guard exists to kill). Cannot be
	// expressed as Assignee:"" — the empty scalar reads as "no filter"
	// (ka-0000007209).
	Unassigned bool   `json:"unassigned,omitempty"`
	CreatedBy  string `json:"created_by,omitempty"`
	Search     string `json:"search,omitempty"`
	Sort       string `json:"sort,omitempty"`
	Priority   *int   `json:"priority,omitempty"`
	NoOpenDeps bool   `json:"no_open_deps,omitempty"` // only beads with no open/in_progress/deferred dependencies
	NoParent   bool   `json:"no_parent,omitempty"`    // only beads with no parent-child parent (orphans)
	// ShortTitleMissing restricts results to beads with no short_title.
	// Surfaced as `kd list --missing-short-title`; maps onto
	// model.BeadFilter.ShortTitleMissing via ?short_title_missing=true.
	ShortTitleMissing bool `json:"short_title_missing,omitempty"`
	// Heraldry restricts results to beads bearing these arms, in any input
	// form: with a colour it matches exactly, without one it matches the
	// same emoji in the same places on any colour (?heraldry=).
	Heraldry string `json:"heraldry,omitempty"`
	Fleet    string `json:"fleet,omitempty"` // filter by fleet ("*" for all fleets, requires admin token)
	// HeraldryProject restricts results to the beads bearing arms in that
	// project: the register `kd heraldry check` reads. HTTP only
	// (?heraldry_project=).
	HeraldryProject string `json:"heraldry_project,omitempty"`
	// IncludeDeleted, when true, includes soft-deleted beads in the result.
	// OnlyDeleted, when true, restricts to soft-deleted beads only. Both
	// require admin role server-side.
	IncludeDeleted bool `json:"include_deleted,omitempty"`
	OnlyDeleted    bool `json:"only_deleted,omitempty"`
	// ExcludeScheduled, when true, hides beads with a `schedule_id` JSONB
	// field (auto-spawned by the cron scheduler each tick). See
	// ka-0000006652.
	ExcludeScheduled bool              `json:"exclude_scheduled,omitempty"`
	Limit            int               `json:"limit,omitempty"`
	Offset           int               `json:"offset,omitempty"`
	FieldFilters     map[string]string `json:"field_filters,omitempty"`
	// Mentions, when non-empty, restricts results to beads whose extracted
	// ka-id text references include this target bead id (joins through
	// bead_references). Composes with the other filters.
	Mentions string `json:"mentions,omitempty"`
	// MentionsType narrows Mentions by typed-mention relationship verb
	// (parent / blocks / related / …). Only applied when Mentions is set.
	// nil = no filter; non-nil "" = untyped (NULL) only; non-nil "<verb>" =
	// exact match. Same three-state shape used by `kd refs --type=<verb>`.
	MentionsType *string `json:"mentions_type,omitempty"`
	// OperatedByAliases resolves each `kind:subject` string through
	// operator_aliases and matches beads.operated_by_id, recovering legacy
	// Slack-driven beads under their canonical alias. ADR §5.3, P4.
	// (The legacy TEXT-keyed OperatedBy field was removed with migration
	// 024 (P6).)
	OperatedByAliases []string `json:"operated_by_aliases,omitempty"`
	// HasReport, when non-nil, filters by the per-row has_report
	// projection (R1, ka-0000007923): &true keeps only beads with a
	// deliverable report child; &false keeps only beads without one.
	// nil means no filter.
	HasReport *bool `json:"has_report,omitempty"`
	// BlockedOnDecision, when non-nil, filters by the daemon-computed
	// blocked_on_decision predicate (R2, ka-0000001065). Same three-state
	// shape as HasReport.
	BlockedOnDecision *bool `json:"blocked_on_decision,omitempty"`
	// ParkedOnDecision, when non-nil, filters by the daemon-computed
	// parked_on_decision predicate — the text-mention sibling of
	// BlockedOnDecision (ka-0000003797): an open decision names this bead in its
	// prose. Same three-state shape. HTTP transport only.
	ParkedOnDecision *bool `json:"parked_on_decision,omitempty"`
	// WithOpenBlockers, when true, asks the daemon to populate each returned
	// bead's OpenBlockers — its OPEN blocking edges denormalized with the
	// blocker's status/type/title (ka-0000000945). An ENRICHMENT, not a
	// filter: it changes what each row carries, never which rows match.
	// HTTP transport only (?with_open_blockers=true); over gRPC the client
	// errors rather than silently returning rows with no blocker data, which
	// a caller would misread as "nothing is blocked".
	WithOpenBlockers bool `json:"with_open_blockers,omitempty"`
	// CountOnly, when true, asks the daemon for just the total via the
	// count-only fast path (SELECT COUNT(*), no ORDER BY / projections / row
	// bodies) — filter.CountOnly (model.BeadFilter). The response carries the
	// count in Total and an empty Beads slice. Used by the `kd list`
	// hidden-operational nudge to size the escalation/decision count without a
	// full fetch. HTTP transport only (?count_only=true); the proto
	// ListBeadsRequest has no count_only field, so over gRPC this is a no-op —
	// safe because CountOnly changes only WHETHER row bodies are returned, not
	// WHICH rows match, so Total is identical either way (unlike a dropped
	// FILTER, which would return the wrong set).
	CountOnly bool `json:"count_only,omitempty"`
	// Parent, when non-empty, restricts results to the transitive subtree
	// under the given bead via parent-child dependency edges (all
	// descendants at any depth, NOT the parent itself). This is the REVERSE
	// child query — beads whose parent-child edge points AT <Parent> —
	// surfaced as `kd list --parent <epic>`. It maps to the same cycle-safe
	// recursive CTE (model.BeadFilter.ParentSubtreeID, HTTP ?parent=) the
	// scheduler's `bead_query {parent}` loop_end gate uses, so a child-count
	// taken here agrees with the gardener's own dormancy preflight by
	// construction. Critically it does NOT read the parent's OWN forward
	// depends_on edges (which point UP at a grandparent), so an epic that
	// carries an upward parent-child edge no longer reports 0 children
	// (ka-0000000208). Composes via AND with status/type/etc.
	Parent string `json:"parent,omitempty"`
	// Time-window filters (ka-0000008195), surfaced as `kd list
	// --created-after` / --created-before / --updated-after / --updated-before
	// / --closed-after / --closed-before. Server-side WHERE clauses on
	// created_at (inclusive >=/<=), updated_at (strict >/<), and closed_at
	// (strict >/<; NULL-safe — never-closed beads match neither closed_*
	// bound). HTTP transport only: the proto BeadFilter has no time-window
	// fields, so the gRPC client errors rather than silently dropping the
	// window (see grpc.go ListBeads — the ExcludeLabels precedent; a dropped
	// window returns the full unwindowed set, the exact footgun these flags
	// exist to kill).
	CreatedAfter  *time.Time `json:"created_after,omitempty"`
	CreatedBefore *time.Time `json:"created_before,omitempty"`
	UpdatedAfter  *time.Time `json:"updated_after,omitempty"`
	UpdatedBefore *time.Time `json:"updated_before,omitempty"`
	ClosedAfter   *time.Time `json:"closed_after,omitempty"`
	ClosedBefore  *time.Time `json:"closed_before,omitempty"`
}

// ListBeadsResponse is the response from ListBeads.
type ListBeadsResponse struct {
	Beads []*model.Bead `json:"beads"`
	Total int           `json:"total"`
}

// UpdateBeadRequest holds optional parameters for updating a bead.
// Nil pointer fields mean "don't change".
type UpdateBeadRequest struct {
	Title       *string         `json:"title,omitempty"`
	ShortTitle  *string         `json:"short_title,omitempty"`
	Heraldry    *string         `json:"heraldry,omitempty"` // any input form; "" or "none" removes the arms
	Description *string         `json:"description,omitempty"`
	Notes       *string         `json:"notes,omitempty"`
	Status      *string         `json:"status,omitempty"`
	Priority    *int            `json:"priority,omitempty"`
	Assignee    *string         `json:"assignee,omitempty"`
	Owner       *string         `json:"owner,omitempty"`
	DueAt       *time.Time      `json:"due_at,omitempty"`
	DeferUntil  *time.Time      `json:"defer_until,omitempty"`
	Fields      json.RawMessage `json:"fields,omitempty"`
	Labels      []string        `json:"labels,omitempty"`
	// OnlyIfShortTitleEmpty makes ShortTitle a fill, not a replace: the
	// server applies it only when the bead has no short_title yet. Surfaced
	// as `kd update --only-if-short-title-empty`.
	OnlyIfShortTitleEmpty bool `json:"only_if_short_title_empty,omitempty"`
}

// EditSubFieldRequest is the body of POST /v1/beads/{id}/fields/{field}/edit.
// `Op` selects the aggregate shape and the mutation: "set"/"unset-key" edit ONE
// key of a JSON-map field, "append"/"remove" edit ONE element of a CSV field.
// `Key`+`Value` apply to the map ops; `Element` applies to the CSV ops.
type EditSubFieldRequest struct {
	Op      string `json:"op"`
	Key     string `json:"key,omitempty"`
	Value   string `json:"value,omitempty"`
	Element string `json:"element,omitempty"`
}

// BatchGetResult holds the result of fetching a single bead in a batch request.
type BatchGetResult struct {
	ID    string      `json:"id"`
	Bead  *model.Bead `json:"bead,omitempty"`
	Error string      `json:"error,omitempty"`
}

// FindSimilarResponse is the response from FindSimilar.
type FindSimilarResponse struct {
	SourceID string               `json:"source_id"`
	Model    string               `json:"model,omitempty"`
	Results  []*FindSimilarResult `json:"results"`
}

// FindSimilarResult is one neighbor bead with its similarity score. The shape
// matches model.SemanticResult for JSON wire compatibility.
type FindSimilarResult struct {
	Bead     *model.Bead `json:"bead"`
	Score    float64     `json:"score"`
	Distance float64     `json:"distance"`
}

// SemanticSearchRequest is the request body for POST /v1/search/semantic.
type SemanticSearchRequest struct {
	Query    string   `json:"query"`
	Status   []string `json:"status,omitempty"`
	Type     []string `json:"type,omitempty"`
	Kind     []string `json:"kind,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	Assignee string   `json:"assignee,omitempty"`
	Fleet    string   `json:"fleet,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	MinScore float64  `json:"min_score,omitempty"`
}

// SemanticSearchResponse is the response from SemanticSearch.
type SemanticSearchResponse struct {
	Model   string                  `json:"model,omitempty"`
	Results []*SemanticSearchResult `json:"results"`
}

// SemanticSearchResult is one scored bead from a semantic search. The shape
// matches model.SemanticResult for JSON wire compatibility.
type SemanticSearchResult struct {
	Bead     *model.Bead `json:"bead"`
	Score    float64     `json:"score"`
	Distance float64     `json:"distance"`
}

// UpsertEmbeddingRequest is the body for POST /v1/embeddings/upsert.
type UpsertEmbeddingRequest struct {
	BeadID    string    `json:"bead_id"`
	Embedding []float32 `json:"embedding"`
	Model     string    `json:"model"`
	TextHash  string    `json:"text_hash"`
}

// SyncBatchEmbeddingsRequest is the body for POST /v1/embeddings/sync-batch.
// The server paginates beads with Offset/Limit, computes and upserts
// embeddings for the current page using its configured embedder, and returns
// per-page stats so the caller can drive the loop. Unset Offset starts from
// the beginning. Fleet scopes the backfill; Force skips the text_hash check.
type SyncBatchEmbeddingsRequest struct {
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
	Fleet  string `json:"fleet,omitempty"`
	Force  bool   `json:"force,omitempty"`
	// OnlyMissing tells the server to SQL-filter the page to beads that
	// have no embedding row or whose embedding is older than the bead,
	// instead of walking every bead and skipping client-side.
	OnlyMissing bool `json:"only_missing,omitempty"`
	// Types, when non-empty, restricts embedding to beads whose type is
	// in this list (operator-driven cost control — see the `kd embed sync`
	// long help for rationale). Applied server-side after listing the page.
	Types []string `json:"types,omitempty"`
	// ExcludeTypes skips beads whose type is in this list. Applied after
	// Types so it can subtract from an allowlist.
	ExcludeTypes []string `json:"exclude_types,omitempty"`
}

// SyncBatchEmbeddingsResponse describes what the server did for one batch.
// PageCount is the number of beads the server considered in this call
// (embedded + skipped + filtered_type). Callers should advance Offset by
// PageCount and stop when PageCount is 0 or less than Limit. Total is
// reported when ListBeads provides it; 0 means "unknown".
type SyncBatchEmbeddingsResponse struct {
	Total     int `json:"total"`
	PageCount int `json:"page_count"`
	Embedded  int `json:"embedded"`
	Skipped   int `json:"skipped"`
	// FilteredType is the count of beads on the page that were skipped
	// because of the Types/ExcludeTypes filter (separate bucket from
	// Skipped, which counts text_hash-unchanged skips).
	FilteredType int    `json:"filtered_type,omitempty"`
	Model        string `json:"model,omitempty"`
}

// ReextractReferencesRequest is the body for POST /v1/admin/reextract-references.
// Callers paginate the backfill by advancing AfterID with the LastID returned
// in each response, until Done=true. Type, when non-empty, narrows the walk to
// beads of that type so a targeted backfill can reindex `type=decision` FIRST
// and make the parked_on_decision badge sound quickly (ka-0000005898).
type ReextractReferencesRequest struct {
	AfterID   string `json:"after_id"`
	BatchSize int    `json:"batch_size"`
	Type      string `json:"type,omitempty"`
}

// ReextractReferencesResponse describes what the server did for one batch.
// LastID is the cursor for the next call (empty when the page was empty).
// On the final page (Done=true) TotalReferences reports the post-backfill
// cardinality of bead_references.
type ReextractReferencesResponse struct {
	BeadsScanned      int    `json:"beads_scanned"`
	CommentsScanned   int    `json:"comments_scanned"`
	ReferencesWritten int    `json:"references_written"`
	LastID            string `json:"last_id"`
	Done              bool   `json:"done"`
	TotalReferences   int64  `json:"total_references,omitempty"`
}

// BackfillSearchTSVRequest is the body for POST /v1/admin/backfill-search-tsv.
// Callers paginate the backfill by advancing AfterID with the LastID returned
// in each response, until Done=true. (ka-0000003832 / ka-search relevance
// Option 2.) Reindex=true refreshes context/options-bearing rows for migration
// 040's fields extension (ka-0000005839) instead of filling NULL legacy rows.
type BackfillSearchTSVRequest struct {
	AfterID   string `json:"after_id"`
	BatchSize int    `json:"batch_size"`
	Reindex   bool   `json:"reindex,omitempty"`
}

// BackfillSearchTSVResponse describes what the server did for one batch.
// LastID is the cursor for the next call (empty when the page was empty). On
// the final page (Done=true) Remaining reports the count of beads whose
// search_tsv is still NULL (0 on a clean finish).
type BackfillSearchTSVResponse struct {
	BeadsScanned int    `json:"beads_scanned"`
	LastID       string `json:"last_id"`
	Done         bool   `json:"done"`
	Remaining    int64  `json:"remaining,omitempty"`
}

// BackfillCategoryRequest is the body for POST /v1/admin/backfill-category.
// Callers paginate the backfill by advancing AfterID with the LastID returned
// in each response, until Done=true (ka-0000007274). It re-stamps the
// beads.category column on legacy rows whose stored category disagrees with the
// current derive_bead_category rule table (migration 043).
type BackfillCategoryRequest struct {
	AfterID   string `json:"after_id"`
	BatchSize int    `json:"batch_size"`
}

// BackfillCategoryResponse describes what the server did for one batch.
// LastID is the cursor for the next call (empty when the page was empty).
type BackfillCategoryResponse struct {
	BeadsScanned int    `json:"beads_scanned"`
	LastID       string `json:"last_id"`
	Done         bool   `json:"done"`
}

// BackfillProjectLabelsRequest is the body for
// POST /v1/admin/backfill-project-labels (gd-YdsEpnN3Qo).
type BackfillProjectLabelsRequest struct {
	BatchSize int  `json:"batch_size"`
	DryRun    bool `json:"dry_run"`
}

// BackfillProjectLabelsResponse: Labelled is what one page wrote, by bead
// type; Missing is the remaining count on a dry run.
type BackfillProjectLabelsResponse struct {
	Labelled map[string]int `json:"labelled"`
	Missing  map[string]int `json:"missing,omitempty"`
	Done     bool           `json:"done"`
}

// BackfillOperatorsRequest is the body for
// POST /v1/admin/backfill-operators. The CLI fires one POST per run.
type BackfillOperatorsRequest struct {
	DryRun    bool `json:"dry_run"`
	BatchSize int  `json:"batch_size,omitempty"`
}

// BackfillOperatorsResponse summarizes one operators-FK backfill run
// (P3 of docs/adr/human-attribution-identity-v2.md). Per-table fields
// are row counts: rows_updated for real runs, the would-be-updated count
// for dry runs. Verifier fields report rows where operated_by TEXT is
// populated but operated_by_id FK is still NULL — must be zero on
// success modulo QuarantinedOperatedBy.
type BackfillOperatorsResponse struct {
	DistinctOperatedBy    int      `json:"distinct_operated_by"`
	OperatorsUpserted     int      `json:"operators_upserted"`
	AliasesUpserted       int      `json:"aliases_upserted"`
	BeadsUpdated          int64    `json:"beads_updated"`
	CommentsUpdated       int64    `json:"comments_updated"`
	DepsUpdated           int64    `json:"deps_updated"`
	EventsUpdated         int64    `json:"events_updated"`
	MutationsUpdated      int64    `json:"mutations_updated"`
	BeadsEmptySentinel    int64    `json:"beads_empty_sentinel"`
	CommentsEmptySentinel int64    `json:"comments_empty_sentinel"`
	DepsEmptySentinel     int64    `json:"deps_empty_sentinel"`
	QuarantinedOperatedBy []string `json:"quarantined_operated_by,omitempty"`
	VerifierBeads         int64    `json:"verifier_beads"`
	VerifierComments      int64    `json:"verifier_comments"`
	VerifierDeps          int64    `json:"verifier_deps"`
	VerifierEvents        int64    `json:"verifier_events"`
	VerifierMutations     int64    `json:"verifier_mutations"`
	DryRun                bool     `json:"dry_run"`
	DurationMs            int64    `json:"duration_ms"`
}

// BackfillOperatedByLegacyRequest is the body for
// POST /v1/admin/backfill-operated-by-legacy. Drives the cleanup defined
// in gardenio/docs/research/legacy-operated-by-matt-cleanup.md §3.2.
type BackfillOperatedByLegacyRequest struct {
	Target        string   `json:"target"`
	Statuses      []string `json:"statuses,omitempty"`
	BatchSize     int      `json:"batch_size,omitempty"`
	MaxIterations int      `json:"max_iterations,omitempty"`
	DryRun        bool     `json:"dry_run,omitempty"`
}

// BackfillOperatedByLegacyResponse summarizes one run of the cleanup.
type BackfillOperatedByLegacyResponse struct {
	DryRun               bool             `json:"dry_run"`
	Target               string           `json:"target"`
	TargetOperatorID     int64            `json:"target_operator_id"`
	BeforeOpenCandidates int64            `json:"before_open_candidates"`
	AfterOpenCandidates  int64            `json:"after_open_candidates"`
	CategoryA            int64            `json:"category_a"`
	CategoryB            int64            `json:"category_b"`
	CategoryC            int64            `json:"category_c"`
	CategoryD            int64            `json:"category_d"`
	RebindCount          map[string]int64 `json:"rebind_count,omitempty"`
	Iterations           int              `json:"iterations"`
	IterationsCapped     bool             `json:"iterations_capped,omitempty"`
	DurationMs           int64            `json:"duration_ms"`
}

// BackfillSlackUserIDRequest is the body for
// POST /v1/admin/backfill-slack-user-id. Drives the thread-derived
// slack_user_id backfill (ka-0000001582).
type BackfillSlackUserIDRequest struct {
	Statuses  []string `json:"statuses,omitempty"`
	BatchSize int      `json:"batch_size,omitempty"`
	DryRun    bool     `json:"dry_run,omitempty"`
}

// BackfillSlackUserIDResponse summarizes one run of the backfill.
type BackfillSlackUserIDResponse struct {
	DryRun               bool  `json:"dry_run"`
	BeforeMissing        int64 `json:"before_missing"`
	AfterMissing         int64 `json:"after_missing"`
	Updated              int64 `json:"updated"`
	ResolvedViaThreadID  int64 `json:"resolved_via_thread_id"`
	ResolvedViaCreatedBy int64 `json:"resolved_via_created_by"`
	Unresolved           int64 `json:"unresolved"`
	DurationMs           int64 `json:"duration_ms"`
}

// BackfillOperatorSlugsRequest is the body for
// POST /v1/admin/backfill-operator-slugs. Drives the op-<id> → readable slug
// backfill (ka-0000001149).
type BackfillOperatorSlugsRequest struct {
	BatchSize int  `json:"batch_size,omitempty"`
	DryRun    bool `json:"dry_run,omitempty"`
}

// BackfillOperatorSlugsResponse summarizes one run of the backfill.
type BackfillOperatorSlugsResponse struct {
	DryRun           bool  `json:"dry_run"`
	BeforeOpaque     int64 `json:"before_opaque"`
	AfterOpaque      int64 `json:"after_opaque"`
	Renamed          int64 `json:"renamed"`
	SkippedCollision int64 `json:"skipped_collision"`
	Machine          int64 `json:"machine"`
	DurationMs       int64 `json:"duration_ms"`
}

// ForgetOperatorRequest is the body for
// POST /v1/admin/operators/{id}/forget. Drives the operator soft-delete
// primitive (ka-0000005150). All fields optional.
type ForgetOperatorRequest struct {
	Reason    string `json:"reason,omitempty"`
	Confirm   bool   `json:"confirm,omitempty"`
	Threshold int    `json:"threshold,omitempty"`
}

// ForgetOperatorResponse summarizes one forget-operator call.
type ForgetOperatorResponse struct {
	OperatorID         int64     `json:"operator_id"`
	AlreadyForgotten   bool      `json:"already_forgotten"`
	ForgottenAt        time.Time `json:"forgotten_at"`
	SlugCleared        bool      `json:"slug_cleared"`
	SlugAliasesRetired int64     `json:"slug_aliases_retired"`
	AuditRows          int64     `json:"audit_rows"`
}

// BackfillOwnerRequest is the body for POST /v1/admin/backfill-owner.
// Drives one run of the Matt-default owner rebind (ka-0000006961).
type BackfillOwnerRequest struct {
	Target          string    `json:"target"`
	Since           time.Time `json:"since"`
	BatchSize       int       `json:"batch_size,omitempty"`
	MaxBatches      int       `json:"max_batches,omitempty"`
	UnownedSentinel string    `json:"unowned_sentinel,omitempty"`
	DryRun          bool      `json:"dry_run,omitempty"`
}

// BackfillOwnerResponse summarizes one run of the Matt-default owner rebind.
type BackfillOwnerResponse struct {
	DryRun           bool             `json:"dry_run"`
	Target           string           `json:"target"`
	Since            time.Time        `json:"since,omitempty"`
	BeforeCandidates int64            `json:"before_candidates"`
	AfterCandidates  int64            `json:"after_candidates"`
	CandidateBeads   int64            `json:"candidate_beads"`
	RebindCount      map[string]int64 `json:"rebind_count,omitempty"`
	KeptMattCount    int64            `json:"kept_matt_count"`
	UnresolvedCount  int64            `json:"unresolved_count"`
	Batches          int              `json:"batches"`
	BatchesCapped    bool             `json:"batches_capped,omitempty"`
	LastID           string           `json:"last_id,omitempty"`
	DurationMs       int64            `json:"duration_ms"`
}

// BackfillEmptyOwnerRequest is the body for
// POST /v1/admin/backfill-empty-owner. Drives one run of the three-phase
// empty-owner backfill (ka-0000007029).
type BackfillEmptyOwnerRequest struct {
	BatchSize                int      `json:"batch_size,omitempty"`
	MaxBatches               int      `json:"max_batches,omitempty"`
	UnownedSentinel          string   `json:"unowned_sentinel,omitempty"`
	LegacyOperatedBySentinel string   `json:"legacy_operated_by_sentinel,omitempty"`
	Phase2Exclusions         []string `json:"phase2_exclusions,omitempty"`
	DryRun                   bool     `json:"dry_run,omitempty"`
}

// BackfillEmptyOwnerResponse summarizes one run of the three-phase
// empty-owner backfill.
type BackfillEmptyOwnerResponse struct {
	DryRun        bool  `json:"dry_run"`
	BeforeEmpty   int64 `json:"before_empty"`
	AfterEmpty    int64 `json:"after_empty"`
	Phase1Filled  int64 `json:"phase1_filled"`
	Phase2Filled  int64 `json:"phase2_filled"`
	Phase3Filled  int64 `json:"phase3_filled"`
	Phase1Batches int   `json:"phase1_batches"`
	Phase2Batches int   `json:"phase2_batches"`
	Phase3Batches int   `json:"phase3_batches"`
	BatchesCapped bool  `json:"batches_capped,omitempty"`
	DurationMs    int64 `json:"duration_ms"`
}

// BackfillOperatedByAgentsRequest is the body for
// POST /v1/admin/backfill-operated-by-agents. Drives one run of the
// per-user-homepage M2 agent operated_by_id backfill (sub-item #1 of
// ka-0000006817).
type BackfillOperatedByAgentsRequest struct {
	BatchSize       int  `json:"batch_size,omitempty"`
	MaxBatches      int  `json:"max_batches,omitempty"`
	IncludeSentinel bool `json:"include_sentinel,omitempty"`
	DryRun          bool `json:"dry_run,omitempty"`
}

// BackfillOperatedByAgentsResponse summarizes one run.
type BackfillOperatedByAgentsResponse struct {
	DryRun         bool             `json:"dry_run"`
	Processed      int64            `json:"processed"`
	Updated        int64            `json:"updated"`
	Skipped        int64            `json:"skipped"`
	RebindBySignal map[string]int64 `json:"rebind_by_signal,omitempty"`
	Batches        int              `json:"batches"`
	BatchesCapped  bool             `json:"batches_capped,omitempty"`
	LastID         string           `json:"last_id,omitempty"`
	DurationMs     int64            `json:"duration_ms"`
}

// BackfillParentChildDepsRequest is the body for
// POST /v1/admin/backfill-parent-child-deps. Drives one run of the
// agent-orphan parent-child reconstruction (ka-0000006775).
type BackfillParentChildDepsRequest struct {
	SinceSeconds  int64 `json:"since_seconds,omitempty"`
	IncludeClosed bool  `json:"include_closed,omitempty"`
	BatchSize     int   `json:"batch_size,omitempty"`
	MaxBatches    int   `json:"max_batches,omitempty"`
	DryRun        bool  `json:"dry_run,omitempty"`
}

// BackfillParentChildDepsResponse summarizes one run.
type BackfillParentChildDepsResponse struct {
	DryRun               bool             `json:"dry_run"`
	CandidateBeads       int64            `json:"candidate_beads"`
	EdgesAdded           int64            `json:"edges_added"`
	EdgesByAuthor        map[string]int64 `json:"edges_by_author,omitempty"`
	SkippedNoAgent       int64            `json:"skipped_no_agent"`
	SkippedNoTaskID      int64            `json:"skipped_no_task_id"`
	SkippedSelfParent    int64            `json:"skipped_self_parent"`
	SkippedTaskMissing   int64            `json:"skipped_task_missing"`
	SkippedExistingDep   int64            `json:"skipped_existing_dep"`
	SkippedParentMention int64            `json:"skipped_parent_mention"`
	Batches              int              `json:"batches"`
	BatchesCapped        bool             `json:"batches_capped,omitempty"`
	LastID               string           `json:"last_id,omitempty"`
	DurationMs           int64            `json:"duration_ms"`
}

// RepairReversedParentChildDepsRequest is the body for
// POST /v1/admin/repair-reversed-parent-child-deps. Drives one run of the
// reversed epic->child parent-child edge audit/repair (ka-0000006880).
type RepairReversedParentChildDepsRequest struct {
	BatchSize  int  `json:"batch_size,omitempty"`
	MaxBatches int  `json:"max_batches,omitempty"`
	DryRun     bool `json:"dry_run,omitempty"`
}

// ReversedParentChildEdgeDTO is one reversed parent-child row reported by
// the audit: EpicID is on the bead_id (child) side, ChildID on the
// depends_on_id (parent) side, ChildType is ChildID's resolved type.
type ReversedParentChildEdgeDTO struct {
	EpicID    string `json:"epic_id"`
	ChildID   string `json:"child_id"`
	ChildType string `json:"child_type"`
	CreatedBy string `json:"created_by,omitempty"`
}

// RepairReversedParentChildDepsResponse summarizes one run.
type RepairReversedParentChildDepsResponse struct {
	DryRun                 bool                         `json:"dry_run"`
	ReversedRows           int64                        `json:"reversed_rows"`
	EdgesFlipped           int64                        `json:"edges_flipped"`
	SkippedCanonicalExists int64                        `json:"skipped_canonical_exists"`
	Edges                  []ReversedParentChildEdgeDTO `json:"edges,omitempty"`
	Batches                int                          `json:"batches"`
	BatchesCapped          bool                         `json:"batches_capped,omitempty"`
	DurationMs             int64                        `json:"duration_ms"`
}

// BlocksCycleDTO is one cycle in the blocks graph reported by the audit:
// Members is the sorted set of bead IDs mutually reachable via blocks edges
// (each blocked-by the next around the ring), and Size is len(Members).
type BlocksCycleDTO struct {
	Members []string `json:"members"`
	Size    int      `json:"size"`
}

// AuditBlocksCyclesResponse mirrors server.BlocksCycleAudit over the wire —
// the result of GET /v1/admin/audit-blocks-cycles (ka-0000007984).
type AuditBlocksCyclesResponse struct {
	EdgeCount  int              `json:"edge_count"`
	NodeCount  int              `json:"node_count"`
	CycleCount int              `json:"cycle_count"`
	Cycles     []BlocksCycleDTO `json:"cycles"`
}

// BackfillTriggeredByDepsRequest is the body for
// POST /v1/admin/backfill-triggered-by-deps. Drives one run of the
// M4 auto-link backfill scanner (ka-0000000004).
type BackfillTriggeredByDepsRequest struct {
	SinceSeconds int64 `json:"since_seconds,omitempty"`
	BatchSize    int   `json:"batch_size,omitempty"`
	MaxBatches   int   `json:"max_batches,omitempty"`
	DryRun       bool  `json:"dry_run,omitempty"`
}

// BackfillTriggeredByDepsResponse summarizes one run.
type BackfillTriggeredByDepsResponse struct {
	DryRun               bool             `json:"dry_run"`
	CandidateBeads       int64            `json:"candidate_beads"`
	EdgesAdded           int64            `json:"edges_added"`
	EdgesBySignal        map[string]int64 `json:"edges_by_signal,omitempty"`
	SkippedExistingDep   int64            `json:"skipped_existing_dep"`
	SkippedNoSignal      int64            `json:"skipped_no_signal"`
	SkippedAmbiguous     int64            `json:"skipped_ambiguous"`
	SkippedSelfReference int64            `json:"skipped_self_reference"`
	SkippedTargetMissing int64            `json:"skipped_target_missing"`
	Batches              int              `json:"batches"`
	BatchesCapped        bool             `json:"batches_capped,omitempty"`
	LastID               string           `json:"last_id,omitempty"`
	DurationMs           int64            `json:"duration_ms"`
}

// CloseTriggerAuditBeadsRequest is the body for
// POST /v1/admin/close-trigger-audit-beads. Drives one run of the
// legacy trigger-audit close sweep (ka-0000006341).
type CloseTriggerAuditBeadsRequest struct {
	BatchSize  int  `json:"batch_size,omitempty"`
	MaxBatches int  `json:"max_batches,omitempty"`
	DryRun     bool `json:"dry_run,omitempty"`
}

// CloseTriggerAuditBeadsResponse summarizes one run.
type CloseTriggerAuditBeadsResponse struct {
	DryRun         bool   `json:"dry_run"`
	CandidateBeads int64  `json:"candidate_beads"`
	BeadsClosed    int64  `json:"beads_closed"`
	Batches        int    `json:"batches"`
	BatchesCapped  bool   `json:"batches_capped,omitempty"`
	LastID         string `json:"last_id,omitempty"`
	DurationMs     int64  `json:"duration_ms"`
}

// MigrateEmbeddingsRequest is the body for POST /v1/embeddings/migrate.
type MigrateEmbeddingsRequest struct {
	Model string `json:"model"`
	Dims  int    `json:"dims"`
	Actor string `json:"actor,omitempty"`
	Note  string `json:"note,omitempty"`
}

// UndoMutationResponse is the response body from POST /v1/mutations/{id}/undo.
// Bead is the state of the bead after the reversal was applied; Undone is the
// original mutation_history row that was reversed (for surfacing in the CLI).
type UndoMutationResponse struct {
	Bead   *model.Bead        `json:"bead"`
	Undone *model.MutationRow `json:"undone"`
}

// ListGlobalEventsRequest parameterizes one page of GET /v1/events. Since and
// Until bound the created_at window (inclusive); a zero Until means "now" and
// a zero Since means the server default 24h-before-Until. Cursor is the last
// event id already consumed (0 = start of window). Limit is the page size,
// capped server-side at 1000. When Desc is true the page is ordered
// newest-first (created_at DESC, id DESC) and Cursor selects events with
// id < Cursor, so a caller can page backward from the newest event.
type ListGlobalEventsRequest struct {
	Since  time.Time
	Until  time.Time
	Cursor int64
	Limit  int
	Desc   bool
}

// ListGlobalEventsResponse is one page of GET /v1/events. NextCursor is the id
// to pass as the next request's Cursor; it is only meaningful when HasMore is
// true.
type ListGlobalEventsResponse struct {
	Events     []*model.Event `json:"events"`
	HasMore    bool           `json:"has_more"`
	NextCursor string         `json:"next_cursor"`
}

// ListReferencesResponse is the wire shape returned by both
// `GET /v1/beads/{id}/mentioned-by` and `GET /v1/beads/{id}/mentions`.
// See kbeads/docs/design-bead-references.md § 5a.
type ListReferencesResponse struct {
	References []*model.BeadReferenceSummary `json:"references"`
	Total      int                           `json:"total"`
	Limit      int                           `json:"limit"`
	Offset     int                           `json:"offset"`
}

// GrantTaintRequest is the body for POST /v1/taints/{name}/grants. Principal
// is the combined "<kind>:<value>" form (docs/adr/taints.md §7); Until is
// the caller's requested expires_at, honored for team/operator/fleet
// principals and always ignored for an agent principal (ADR §5.2). Nil means
// "use the ADR §10.4 default table".
type GrantTaintRequest struct {
	Principal string     `json:"principal"`
	Until     *time.Time `json:"until,omitempty"`
}

// RequestTaintGrantRequest is the body for
// POST /v1/taints/{name}/grant-requests (ADR docs/adr/taints.md §9.2).
// Reason is optional free text recorded on the resulting escalation for the
// approving team's context.
type RequestTaintGrantRequest struct {
	Reason string `json:"reason,omitempty"`
}

// CreateScrubRequestRequest is the body for POST /v1/scrub-requests
// (docs/adr/taints.md §6.2/§7). Deliberately carries no mode/detaint_mode
// field — that choice is not requester-controlled (see scrub_request.go).
type CreateScrubRequestRequest struct {
	Taint string          `json:"taint"`
	Kind  string          `json:"kind"`
	Scope json.RawMessage `json:"scope"`
}

// ScrubRequestView is the response from CreateScrubRequest: the created
// scrub-request bead (Fields carries the frozen resolved_bead_ids) plus,
// for a kind=detaint request only, the named taint's CURRENT
// scrub_policy.detaint_mode — read-only surfacing of the taint's own field,
// never a field on the request bead itself.
type ScrubRequestView struct {
	*model.Bead
	DetaintMode string `json:"detaint_mode,omitempty"`
}

// AddDependencyRequest holds parameters for adding a dependency.
type AddDependencyRequest struct {
	BeadID      string `json:"bead_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
	CreatedBy   string `json:"created_by,omitempty"`
}
