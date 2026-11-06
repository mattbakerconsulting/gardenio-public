package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/heraldry"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

type mockEmbedding struct {
	vec      []float32
	model    string
	textHash string
}

type mockStore struct {
	// beadsMu guards every read AND write of the beads map below, and of the
	// labels map, iteration included. UserProvisioner drives CreateBead/GetBead/ListBeads
	// concurrently through a real httptest server (gd-9AxLqW9mtK: "fatal
	// error: concurrent map iteration and map write"), so every exported
	// mockStore method touching m.beads must take this lock for the duration
	// of its access. The unexported parent/blocker helpers below (e.g.
	// getParentsForBeads, parentClosedEpic, hasParentChildEdge,
	// inParentSubtree, getOpenBlockersForBeadsLocked) read m.beads WITHOUT
	// locking — they are only ever called from inside an exported method that
	// already holds beadsMu, and sync.Mutex is not reentrant: locking again
	// from a nested call would deadlock. m.labels shares this lock rather than
	// taking its own because most bead methods read both maps together; the
	// window-view flush timer reaches GetLabelsForBeads off the test goroutine
	// (gd-AHNBxcUXAB: "concurrent map read and map write").
	beadsMu sync.Mutex
	beads   map[string]*model.Bead
	configs map[string]*model.Config

	// getBeadIncludingDeletedErr, when set, is returned by
	// GetBeadIncludingDeleted for every id — injects a transient
	// store-layer error (distinct from sql.ErrNoRows) for
	// fail-closed-on-error test coverage (gd-9rjfQsmQeD).
	getBeadIncludingDeletedErr error
	events                     []*model.Event
	deps                       map[string][]*model.Dependency
	labels                     map[string][]string
	// taints backs GetTaintsForBead, keyed by bead id.
	taints map[string][]int64
	// taintRegistry backs SetTaint/GetTaint/ListTaints, keyed by taint name.
	taintRegistry map[string]*model.Taint
	// taintGrantsMu guards taintGrants: TestHandleEventStream_TaintGateRefreshedMidStream
	// (sse_test.go) revokes a grant on the main test goroutine while an SSE
	// connection's own goroutine concurrently re-resolves the taint gate on
	// its refresh ticker, reading this same map.
	taintGrantsMu sync.Mutex
	// taintGrants backs GrantTaint/RevokeTaint/GetTaintGrant, keyed by
	// "<taintID>\x00<principalKind>\x00<principal>".
	taintGrants map[string]*model.TaintGrant
	// beadTaints backs ApplyTaint/GetBeadTaints, keyed by bead id.
	beadTaints map[string][]*model.BeadTaint
	// taintAudit backs RecordTaintAudit, append-only.
	taintAudit       []*model.TaintAudit
	taintAuditNextID int64
	// scrubTaintCalls records every ScrubTaint request, for assertions on
	// what executeScrubRequest built. scrubTaintErr/scrubTaintResult, when
	// non-nil, override the canned per-bead success ScrubTaint returns.
	scrubTaintCalls  []*model.ScrubTaintRequest
	scrubTaintErr    error
	scrubTaintResult *model.ScrubTaintResult
	// beadTaintQueryCalls counts GetTaintsForBead/GetTaintsForBeads/GetBeadTaints
	// invocations, so tests can assert the zero-config hot path (ADR
	// docs/adr/taints.md §10.1) never issues a bead_taints query.
	beadTaintQueryCalls int
	// applyTaintCalls counts ApplyTaint invocations (successful or idempotent
	// no-op), so tests can assert inherit:read (gd-mUiYE0jx1d) performs no
	// bead_taints write on a taintContextOf cache hit.
	applyTaintCalls int
	// findProjectByNameCalls counts FindProjectByName invocations, so tests
	// can assert resolveProjectIntakeTaints' taintRegistryNonEmpty
	// short-circuit (ADR docs/adr/taints.md §10.1) skips the project lookup
	// entirely on an install with no taints created yet.
	findProjectByNameCalls int
	comments               map[string][]*model.Comment
	commentNextID          int64
	// handleSeqs backs AllocateAgentHandleSeq: a per-gardener monotonic counter.
	handleSeqs map[string]int64

	// embeddings, when populated, back GetEmbedding/UpsertEmbedding.
	embeddings map[string]mockEmbedding
	// uiTelemetry holds rows recorded via RecordUITelemetry, oldest-first.
	uiTelemetry []*model.UITelemetryRow
	// slowToolCalls holds rows recorded via InsertSlowToolCall, oldest-first.
	slowToolCalls []*model.SlowToolCallRow
	// pushDevices holds rows recorded via UpsertPushDevice, keyed by token.
	pushDevices map[string]*model.PushDevice
	// semanticResults, when non-nil, is returned by SemanticSearch.
	semanticResults []*model.SemanticResult
	// embeddingStats, when non-nil, is returned by GetEmbeddingStats.
	embeddingStats *model.EmbeddingStats
	// embeddingStatsErr, when non-nil, is returned by GetEmbeddingStats.
	embeddingStatsErr error
	// lastSemanticFilter records the BeadFilter passed to SemanticSearch.
	lastSemanticFilter model.BeadFilter
	// lastSemanticVec records the query vector passed to SemanticSearch.
	lastSemanticVec []float32
	// lastSemanticLimit records the limit passed to SemanticSearch.
	lastSemanticLimit int

	// operatorDirStats, when non-nil, is returned verbatim by
	// OperatorDirectoryStats; operatorDirStatsErr forces an error.
	operatorDirStats    *store.OperatorDirectoryStats
	operatorDirStatsErr error

	// operatorDirStatsArgs records the (recentPerOperator, recentCeiling,
	// recentWindowHours) OperatorDirectoryStats was last called with, so tests
	// can assert the handler clamped caller-supplied params before the store.
	operatorDirStatsArgs [3]int

	// addLabelErr, when non-nil, is returned by AddLabel (for testing rollback).
	addLabelErr error
	// updateBeadErr, when non-nil, is returned by UpdateBead (for testing the
	// transitive thread_id copy half-link path — ka-0000007300).
	updateBeadErr error
	// updateBeadErrOnce, when true, clears updateBeadErr after it is returned
	// once, so only the NEXT UpdateBead call fails — a later UpdateBead in the
	// same handler (e.g. a status-transition write after a fields-merge write)
	// succeeds. Without this, a handler with two UpdateBead calls can return
	// the same error from either one, and a test asserting only on the final
	// status code can't tell which write actually failed (gd-0TG0s16gcS).
	updateBeadErrOnce bool
	// findAgentByNameErr, when non-nil, is returned by FindAgentByName. Lets a
	// test drive a store failure through the HTTP handler + logging middleware
	// (e.g. a pq 28P01 DB-auth error → 503 + metric increment, ka-0000006554).
	findAgentByNameErr error
	// lockBeadAssigneeHook, when set, runs at the top of LockBeadAssignee:
	// a test writes a rival lock take there to stand in for the run that
	// committed between updateBead's read and its write.
	lockBeadAssigneeHook func(id string)
	// pingErr, when non-nil, is returned by Ping (for testing health check).
	pingErr error
	// embeddingErr, when non-nil, is returned by GetEmbedding.
	embeddingErr error
	// semanticErr, when non-nil, is returned by SemanticSearch.
	semanticErr error

	// histogramResults, when non-nil, is returned verbatim by EventHistogram.
	histogramResults []model.EventHistogramBucket
	// histogramErr, when non-nil, is returned by EventHistogram.
	histogramErr error
	// lastHistogramSince/Until/Bucket record the last EventHistogram args.
	lastHistogramSince  time.Time
	lastHistogramUntil  time.Time
	lastHistogramBucket string

	// revokedTokens holds the persisted revoked-token rows for tests.
	revokedTokens map[string]*model.RevokedToken

	// acceptanceMetrics, when non-nil, is returned verbatim by
	// AcceptanceMetrics. Tests pin this to assert the HTTP layer pipes
	// the store output through unchanged.
	acceptanceMetrics *store.AcceptanceMetrics

	// refreshTokens holds persisted refresh_tokens rows for tests, keyed by
	// SHA-256 hash. Mirrors the postgres refresh_tokens table.
	refreshTokens map[string]*model.RefreshToken
	// runnerTokens holds persisted runner_tokens rows (local-agents Task 7),
	// keyed by token hash. Guarded by runnerTokensMu.
	runnerTokensMu sync.Mutex
	runnerTokens   map[string]*model.RunnerToken

	// Migrate / reindex tracking for `kd embed migrate` tests.
	migrateErr     error
	reindexErr     error
	lastMigrate    *mockMigrateCall
	indexDropped   bool
	indexRecreated bool

	// staleEmbeddingIDs marks beads as stale-by-updated_at: the
	// ListBeadsNeedingEmbedding mock returns them even when an embedding
	// row exists, simulating the postgres SQL filter for the
	// b.updated_at > e.updated_at branch.
	staleEmbeddingIDs map[string]bool
	// refreshedEmbeddingCalls records each call to RefreshEmbeddingTimestamps
	// (one inner slice per call). Lets tests assert that the sync handler
	// bumped the timestamps for skipped beads, breaking the false-staleness
	// loop ka-0000000728.
	refreshedEmbeddingCalls [][]string

	// mutations is the in-memory mutation_history table for tests.
	mutations []*model.MutationRow

	// operators / operatorAliases are the in-memory operators + operator_aliases
	// tables (migration 015). UpsertOperator/LookupOperatorAlias/UpsertOperatorAlias
	// drive the resolver hot-/cold-path under test.
	//
	// opMu guards the operator/alias maps so the UserProvisioner concurrent-
	// same-user test (and any other concurrent-resolver call site) is safe
	// against Go's runtime concurrent-map-write panic (ka-0000007442).
	// advisoryLocksMu guards advisoryLocks, the mock's stand-in for Postgres
	// transaction-scoped advisory locks (TryAdvisoryLock).
	advisoryLocksMu sync.Mutex
	advisoryLocks   map[int64]bool

	opMu                 sync.Mutex
	operators            map[int64]struct{ Kind, Subject string }
	operatorByPair       map[string]int64  // "<kind>\x00<subject>" -> id
	operatorAliases      map[string]int64  // alias key -> operator_id
	operatorAliasSources map[string]string // alias key -> source recorded at first insert
	operatorIdentities   map[int64]struct{ Display, Email string }
	operatorMerges       map[int64]int64  // oldID -> newID, recorded by MergeOperators
	mergedOperatorRows   int64            // value returned by MergeOperators (test-controllable)
	operatorSlugs        map[string]int64 // slug -> operator_id (per-user homepage M1, ka-0000000022)
	operatorForgotten    map[int64]bool   // operator_id -> right-to-be-forgotten soft-delete (ka-0000004824)
	nextOperatorID       int64

	// followers backs the bead_followers join table (migration 023,
	// ka-0000004114) for tests.
	followers map[mockFollowerKey]*model.BeadFollower

	// reextractFn lets handler tests fake the per-page response.
	reextractFn    func(afterID string, batchSize int) (*store.ReextractReferencesStats, error)
	reextractCalls []mockReextractCall

	// backfillSearchTSVFn lets handler tests fake the per-page response.
	backfillSearchTSVFn    func(afterID string, batchSize int, reindex bool) (*store.BackfillSearchTSVStats, error)
	backfillSearchTSVCalls []mockBackfillSearchTSVCall

	// backfillCategoryFn lets handler tests fake the per-page response.
	backfillCategoryFn    func(afterID string, batchSize int) (*store.BackfillCategoryStats, error)
	backfillCategoryCalls []mockBackfillCategoryCall

	// backfillOperatorsFn lets handler tests fake the response.
	backfillOperatorsFn    func(store.BackfillOperatorsOptions) (*store.BackfillOperatorsStats, error)
	backfillOperatorsCalls []store.BackfillOperatorsOptions

	backfillOperatedByLegacyFn    func(store.BackfillOperatedByLegacyOptions) (*store.BackfillOperatedByLegacyStats, error)
	backfillOperatedByLegacyCalls []store.BackfillOperatedByLegacyOptions

	backfillSlackUserIDFn    func(store.BackfillSlackUserIDOptions) (*store.BackfillSlackUserIDStats, error)
	backfillSlackUserIDCalls []store.BackfillSlackUserIDOptions

	backfillOperatorSlugsFn    func(store.BackfillOperatorSlugsOptions) (*store.BackfillOperatorSlugsStats, error)
	backfillOperatorSlugsCalls []store.BackfillOperatorSlugsOptions

	forgetOperatorFn    func(int64, store.ForgetOperatorOptions) (*store.ForgetOperatorResult, error)
	forgetOperatorCalls []forgetOperatorCall

	backfillOwnerFn    func(store.BackfillOwnerOptions) (*store.BackfillOwnerStats, error)
	backfillOwnerCalls []store.BackfillOwnerOptions

	backfillEmptyOwnerFn    func(store.BackfillEmptyOwnerOptions) (*store.BackfillEmptyOwnerStats, error)
	backfillEmptyOwnerCalls []store.BackfillEmptyOwnerOptions

	backfillOperatedByAgentsFn    func(store.BackfillOperatedByAgentsOptions) (*store.BackfillOperatedByAgentsStats, error)
	backfillOperatedByAgentsCalls []store.BackfillOperatedByAgentsOptions

	backfillParentChildDepsFn    func(store.BackfillParentChildDepsOptions) (*store.BackfillParentChildDepsStats, error)
	backfillParentChildDepsCalls []store.BackfillParentChildDepsOptions

	repairReversedParentChildDepsFn    func(store.RepairReversedParentChildDepsOptions) (*store.RepairReversedParentChildDepsStats, error)
	repairReversedParentChildDepsCalls []store.RepairReversedParentChildDepsOptions

	backfillTriggeredByDepsFn    func(store.BackfillTriggeredByDepsOptions) (*store.BackfillTriggeredByDepsStats, error)
	backfillTriggeredByDepsCalls []store.BackfillTriggeredByDepsOptions

	closeTriggerAuditFn    func(store.CloseTriggerAuditBeadsOptions) (*store.CloseTriggerAuditBeadsStats, error)
	closeTriggerAuditCalls []store.CloseTriggerAuditBeadsOptions

	// references is the in-memory bead_references table for tests
	// (ka-0000002463 / docs/design-bead-references.md § 5b).
	references []*model.BeadReference

	// References API stubs for ListBeadsMentioning / ListBeadReferencesFrom.
	mentionedBy            []*model.BeadReferenceSummary
	mentionedByTotal       int
	mentionedByErr         error
	lastMentionedByID      string
	lastMentionedByLimit   int
	lastMentionedByOffset  int
	lastMentionedByRelType *string

	mentions            []*model.BeadReferenceSummary
	mentionsTotal       int
	mentionsErr         error
	lastMentionsID      string
	lastMentionsLimit   int
	lastMentionsOffset  int
	lastMentionsRelType *string

	// hasBundleChild lets tests pin the HasBundleChild result per parent ID.
	// When a parent is absent from the map, HasBundleChild scans references
	// + beads to mirror the postgres semantics.
	hasBundleChild    map[string]bool
	hasBundleChildErr error

	// mentionsBySource models bead_references for the kd search --mentions
	// filter: source bead id -> list of target ids. A bead is considered to
	// "mention" a target if the target appears in the slice keyed by the
	// source bead id. Populated by tests directly.
	mentionsBySource map[string][]string
	// listMu guards the two ListBeads call-capture fields below. ListBeads is
	// exercised concurrently by the UserProvisioner same-user test (which races
	// provisioning against listing), so an unguarded slice append here would be
	// a genuine data race, not just a benign last-writer-wins on a scalar.
	listMu sync.Mutex
	// lastListFilter records the BeadFilter passed to ListBeads. Useful for
	// tests that want to assert the HTTP handler wired a query parameter
	// through to the store, independent of mockStore filter semantics.
	lastListFilter model.BeadFilter
	// listFilters records EVERY BeadFilter passed to ListBeads, in call order.
	// Lets tests assert on the REQUEST SHAPE of a paged walk (advice
	// ka-0000003555: assert on the requests, not the result) — e.g. that the
	// auto-close closed-descendant enumeration advanced its Offset across pages
	// rather than fetching a single capped window (ka-0000008187).
	listFilters []model.BeadFilter

	// operatorLimits records the limit ListOperatorsWithSlugs was called with,
	// so the /v1/operators clamp regression test (ka-0000004753) can assert on
	// the limit the STORE actually receives — the value that bounds the SQL
	// LIMIT $1, not the handler's local variable (advice ka-0000003555).
	operatorLimits []int
}

// mockReextractCall captures one ReextractReferencesPage invocation.
type mockReextractCall struct {
	AfterID   string
	BatchSize int
	Type      string
}

// mockBackfillSearchTSVCall captures one BackfillSearchTSVPage invocation.
type mockBackfillSearchTSVCall struct {
	AfterID   string
	BatchSize int
	Reindex   bool
}

// mockBackfillCategoryCall captures one BackfillCategoryPage invocation.
type mockBackfillCategoryCall struct {
	AfterID   string
	BatchSize int
}

// mockMigrateCall records arguments passed to MigrateEmbeddingsSchema.
type mockMigrateCall struct {
	Model string
	Dims  int
	Actor string
	Note  string
}

func newMockStore() *mockStore {
	return &mockStore{
		beads:    make(map[string]*model.Bead),
		configs:  make(map[string]*model.Config),
		deps:     make(map[string][]*model.Dependency),
		labels:   make(map[string][]string),
		comments: make(map[string][]*model.Comment),
	}
}

func (m *mockStore) CreateBead(_ context.Context, bead *model.Bead) error {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	m.beads[bead.ID] = bead
	return nil
}

func (m *mockStore) GetBead(_ context.Context, id string) (*model.Bead, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, nil
	}
	// Hide soft-deleted beads (ka-0000007077) — matches queryGetBead.
	if b.DeletedAt != nil {
		return nil, sql.ErrNoRows
	}
	// Clone and attach labels so callers see the latest label state.
	clone := *b
	clone.Labels = m.labels[id]
	return &clone, nil
}

// GetBeadIncludingDeleted is GetBead but does not hide a soft-deleted bead
// — matches queryGetBeadIncludingDeleted.
func (m *mockStore) GetBeadIncludingDeleted(_ context.Context, id string) (*model.Bead, error) {
	if m.getBeadIncludingDeletedErr != nil {
		return nil, m.getBeadIncludingDeletedErr
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, nil
	}
	clone := *b
	clone.Labels = m.labels[id]
	return &clone, nil
}

// FindAgentByName scans the in-memory beads for type=agent rows whose
// fields.agent equals name and (optionally) whose status is in statuses,
// returning the most-recently-updated match.
func (m *mockStore) FindAgentByName(_ context.Context, name string, statuses []model.Status) (*model.Bead, error) {
	if m.findAgentByNameErr != nil {
		return nil, m.findAgentByNameErr
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	allowed := map[model.Status]bool{}
	for _, s := range statuses {
		allowed[s] = true
	}
	var best *model.Bead
	for _, b := range m.beads {
		if b.Type != "agent" || b.DeletedAt != nil {
			continue
		}
		if len(allowed) > 0 && !allowed[b.Status] {
			continue
		}
		var fields map[string]any
		if len(b.Fields) > 0 {
			_ = json.Unmarshal(b.Fields, &fields)
		}
		v, _ := fields["agent"].(string)
		if v != name {
			continue
		}
		if best == nil || b.UpdatedAt.After(best.UpdatedAt) {
			best = b
		}
	}
	if best == nil {
		return nil, sql.ErrNoRows
	}
	clone := *best
	clone.Labels = m.labels[best.ID]
	return &clone, nil
}

// FindAgentsByNames returns one most-recently-updated bead per matching
// name in a single call. Mirrors FindAgentByName's filtering semantics.
func (m *mockStore) FindAgentsByNames(ctx context.Context, names []string, statuses []model.Status) ([]*model.Bead, error) {
	out := make([]*model.Bead, 0, len(names))
	for _, name := range names {
		b, err := m.FindAgentByName(ctx, name, statuses)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// FindProjectByName mirrors queryFindProjectByName: matches either the bare
// title or the legacy "Project: <name>" title, breaking ties on a land_kind,
// then land_frozen, then lowest id.
func (m *mockStore) FindProjectByName(_ context.Context, name string) (*model.Bead, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	m.findProjectByNameCalls++
	var best *model.Bead
	for _, b := range m.beads {
		if b.Type != "project" || b.DeletedAt != nil {
			continue
		}
		if projectNameFromTitle(b.Title) != name {
			continue
		}
		if best == nil || projectRankLess(b, best) {
			best = b
		}
	}
	if best == nil {
		return nil, sql.ErrNoRows
	}
	clone := *best
	clone.Labels = m.labels[best.ID]
	return &clone, nil
}

// projectRankLess is queryFindProjectByName's ORDER BY: a land_kind first,
// then land_frozen=true, then the lower id.
func projectRankLess(a, b *model.Bead) bool {
	rank := func(x *model.Bead) (bool, bool) {
		var f struct {
			Kind   any `json:"land_kind"`
			Frozen any `json:"land_frozen"`
		}
		_ = json.Unmarshal(x.Fields, &f)
		kind := f.Kind != nil && fmt.Sprint(f.Kind) != ""
		return kind, fmt.Sprint(f.Frozen) == "true"
	}
	ak, af := rank(a)
	bk, bf := rank(b)
	if ak != bk {
		return ak
	}
	if af != bf {
		return af
	}
	return a.ID < b.ID
}

func (m *mockStore) ListBeads(_ context.Context, filter model.BeadFilter) ([]*model.Bead, int, error) {
	m.listMu.Lock()
	m.lastListFilter = filter
	m.listFilters = append(m.listFilters, filter)
	m.listMu.Unlock()
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	idSet := make(map[string]bool, len(filter.IDs))
	for _, id := range filter.IDs {
		idSet[id] = true
	}
	var result []*model.Bead
outer:
	for _, b := range m.beads {
		if filter.Mentions != "" {
			targets := m.mentionsBySource[b.ID]
			found := false
			for _, t := range targets {
				if t == filter.Mentions {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if len(filter.Status) > 0 {
			found := false
			for _, s := range filter.Status {
				if b.Status == s {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if len(filter.Type) > 0 {
			found := false
			for _, t := range filter.Type {
				if b.Type == t {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if slices.Contains(filter.ExcludeTypes, b.Type) {
			continue
		}
		if len(filter.Kind) > 0 {
			found := false
			for _, k := range filter.Kind {
				if b.Kind == k {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if filter.Heraldry != "" {
			// The store matches the run, or a shield-form value whose
			// places read as it (queries.go).
			if arms, err := heraldry.Read(b.Heraldry); err != nil || heraldry.Run(arms) != filter.Heraldry {
				continue
			}
		}
		if filter.HeraldryRegister != "" {
			// beads.project (migration 057): the one project label, or none.
			projects := projectLabelValues(b.Labels)
			if b.Heraldry == "" || len(projects) != 1 || projects[0] != filter.HeraldryRegister {
				continue
			}
		}
		if filter.Priority != nil && b.Priority != *filter.Priority {
			continue
		}
		if filter.Assignee != "" && b.Assignee != filter.Assignee {
			continue
		}
		if len(filter.Labels) > 0 {
			beadLabels := m.labels[b.ID]
			for _, want := range filter.Labels {
				found := false
				for _, have := range beadLabels {
					if have == want {
						found = true
						break
					}
				}
				if !found {
					continue outer
				}
			}
		}
		for _, drop := range filter.ExcludeLabels {
			if slices.Contains(m.labels[b.ID], drop) {
				continue outer
			}
		}
		if len(filter.Titles) > 0 && !slices.Contains(filter.Titles, b.Title) {
			continue
		}
		if filter.ShortTitleMissing && strings.TrimSpace(b.ShortTitle) != "" {
			continue
		}
		// LabelsOrNoProject: an in-scope label OR no project label (mirrors the
		// postgres predicate; hasProjectLabel is the shared definition).
		if len(filter.LabelsOrNoProject) > 0 {
			beadLabels := m.labels[b.ID]
			ok := !hasProjectLabel(beadLabels)
			for _, want := range filter.LabelsOrNoProject {
				for _, have := range beadLabels {
					if have == want {
						ok = true
					}
				}
			}
			if !ok {
				continue
			}
		}
		if len(filter.LabelsAny) > 0 {
			beadLabels := m.labels[b.ID]
			anyMatch := false
			for _, want := range filter.LabelsAny {
				for _, have := range beadLabels {
					if have == want {
						anyMatch = true
						break
					}
				}
				if anyMatch {
					break
				}
			}
			if !anyMatch {
				continue
			}
		}
		// LabelsAnyUser is a SEPARATE any-of group that ANDs with LabelsAny
		// (ka-0000003307): the bead must independently match at least one of
		// these too. Mirrors the store's distinct EXISTS clause.
		if len(filter.LabelsAnyUser) > 0 {
			beadLabels := m.labels[b.ID]
			anyMatch := false
			for _, want := range filter.LabelsAnyUser {
				for _, have := range beadLabels {
					if have == want {
						anyMatch = true
						break
					}
				}
				if anyMatch {
					break
				}
			}
			if !anyMatch {
				continue
			}
		}
		if filter.Fleet != "" && filter.Fleet != model.FleetAll {
			beadFleet := b.Fleet
			if beadFleet == "" {
				beadFleet = "default"
			}
			if beadFleet != filter.Fleet {
				continue
			}
		}
		if filter.Search != "" {
			if !strings.Contains(strings.ToLower(b.Title), strings.ToLower(filter.Search)) &&
				!strings.Contains(strings.ToLower(b.Description), strings.ToLower(filter.Search)) {
				continue
			}
		}
		if len(filter.Fields) > 0 {
			if b.Fields == nil {
				continue
			}
			var fm map[string]any
			if json.Unmarshal(b.Fields, &fm) != nil {
				continue
			}
			match := true
			for k, v := range filter.Fields {
				got, ok := fm[k]
				if !ok {
					match = false
					break
				}
				if fmt.Sprint(got) != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		if filter.CreatedAfter != nil && b.CreatedAt.Before(*filter.CreatedAfter) {
			continue
		}
		if filter.CreatedBefore != nil && b.CreatedAt.After(*filter.CreatedBefore) {
			continue
		}
		if filter.UpdatedAfter != nil && !b.UpdatedAt.After(*filter.UpdatedAfter) {
			continue
		}
		if filter.UpdatedBefore != nil && !b.UpdatedAt.Before(*filter.UpdatedBefore) {
			continue
		}
		// closed_at is nullable — never-closed beads match neither bound,
		// mirroring the NULL-safe SQL comparisons in the postgres store.
		if filter.ClosedAfter != nil && (b.ClosedAt == nil || !b.ClosedAt.After(*filter.ClosedAfter)) {
			continue
		}
		if filter.ClosedBefore != nil && (b.ClosedAt == nil || !b.ClosedAt.Before(*filter.ClosedBefore)) {
			continue
		}
		if filter.CreatedBy != "" && b.CreatedBy != filter.CreatedBy {
			continue
		}
		if len(filter.ExcludeTypes) > 0 {
			excluded := false
			for _, t := range filter.ExcludeTypes {
				if b.Type == t {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
		}
		if b.Status == model.StatusClosed && slices.Contains(filter.ExcludeClosedTypes, b.Type) {
			continue
		}
		if len(filter.IDs) > 0 && !idSet[b.ID] {
			continue
		}
		// ParentID: direct parent-child children of the given bead. Matches the
		// postgres EXISTS(deps.bead_id=beads.id AND deps.depends_on_id=parent)
		// clause, including the legacy "parent" alias.
		if filter.ParentID != "" {
			if !m.hasParentChildEdge(b.ID, filter.ParentID) {
				continue
			}
		}
		// ParentSubtreeID: transitive parent-child descendants of the given
		// bead (excluding the root), mirroring the recursive CTE in
		// internal/store/postgres/queries.go. Cycle-safe via a visited set.
		if filter.ParentSubtreeID != "" {
			if b.ID == filter.ParentSubtreeID || !m.inParentSubtree(b.ID, filter.ParentSubtreeID) {
				continue
			}
		}
		if len(filter.ParentSubtreeIDs) > 0 {
			if !slices.ContainsFunc(filter.ParentSubtreeIDs, func(root string) bool { return m.inParentSubtree(b.ID, root) }) {
				continue
			}
		}
		// TaintGrantIDs (ADR docs/adr/taints.md §5.1): mirrors the postgres
		// NOT EXISTS(bead_taints bt WHERE bt.taint_id <> ALL(grants)) predicate
		// — a bead is excluded when it carries any taint id absent from the
		// caller's grant slice. A nil filter means the gate does not apply
		// (exempt caller or empty registry); a non-nil-but-empty slice denies
		// every bead carrying any taint at all.
		if filter.TaintGrantIDs != nil {
			grants := *filter.TaintGrantIDs
			granted := make(map[int64]bool, len(grants))
			for _, g := range grants {
				granted[g] = true
			}
			excluded := false
			for _, tid := range m.gateTaintIDs(m.taints[b.ID]) {
				if !granted[tid] {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
		}
		// TaintID: the positive "carries this taint" filter, mirroring the
		// postgres EXISTS(bead_taints bt WHERE bt.taint_id = $N) predicate.
		if filter.TaintID != nil {
			found := false
			for _, tid := range m.taints[b.ID] {
				if tid == *filter.TaintID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		result = append(result, b)
	}
	// Faithful paging (ka-0000008187): the postgres store applies ORDER BY +
	// LIMIT/OFFSET, so a caller that pages (or caps with Limit) sees a WINDOW,
	// not the whole match set. The prior mock ignored Limit/Offset/Sort and
	// always returned every match, which hid silent-truncation bugs — a
	// single-shot `Limit: 500` fetch looked identical to a correct paged walk.
	// Mirror postgres: total is the full match count; the returned slice is the
	// requested page.
	total := len(result)
	sortMockBeads(result, filter.Sort)
	if filter.Offset > 0 {
		if filter.Offset >= len(result) {
			result = nil
		} else {
			result = result[filter.Offset:]
		}
	}
	if filter.Limit > 0 && len(result) > filter.Limit {
		result = result[:filter.Limit]
	}
	// WithOpenBlockers enrichment, mirroring queryListBeads: a second batched
	// lookup keyed on THIS PAGE's ids, applied after paging. Beads are cloned
	// first — the mock hands out pointers into m.beads, and stamping a derived
	// field onto the shared fixture would leak into later assertions.
	if filter.WithOpenBlockers && len(result) > 0 {
		ids := make([]string, len(result))
		for i, b := range result {
			ids[i] = b.ID
		}
		blockerMap := m.getOpenBlockersForBeadsLocked(ids)
		enriched := make([]*model.Bead, len(result))
		for i, b := range result {
			clone := *b
			// Non-nil even when empty, mirroring queryListBeads: present-but-
			// empty is the "checked, nothing blocks it" signal.
			blockers := blockerMap[b.ID]
			if blockers == nil {
				blockers = []*model.OpenBlocker{}
			}
			clone.OpenBlockers = &blockers
			enriched[i] = &clone
		}
		result = enriched
	}
	// WithParent enrichment (ka-0000006083), mirroring queryListBeads: a second
	// batched lookup keyed on THIS PAGE's ids, applied after paging. Cloned so
	// the derived field never leaks into the shared fixture.
	if filter.WithParent && len(result) > 0 {
		ids := make([]string, len(result))
		for i, b := range result {
			ids[i] = b.ID
		}
		parentMap := m.getParentsForBeads(ids)
		enriched := make([]*model.Bead, len(result))
		for i, b := range result {
			clone := *b
			if p, ok := parentMap[b.ID]; ok {
				clone.ParentID = p.ID
				clone.ParentTitle = p.Title
			}
			enriched[i] = &clone
		}
		result = enriched
	}
	return result, total, nil
}

// getParentsForBeads models the postgres queryGetParentsForBeads batched lookup:
// for each requested child, resolve its immediate parent-child parent id (and
// title). Keep in agreement with TestQueryListBeads_WithParent /
// TestQueryGetParentsForBeads* in internal/store/postgres.
//
// Unlocked: reads m.beads without beadsMu. Only called from ListBeads, which
// already holds the lock.
func (m *mockStore) getParentsForBeads(beadIDs []string) map[string]beadParentMock {
	idSet := make(map[string]bool, len(beadIDs))
	for _, id := range beadIDs {
		idSet[id] = true
	}
	parentEdge := map[model.DependencyType]bool{}
	for _, t := range model.ParentChildDepTypes {
		parentEdge[model.DependencyType(t)] = true
	}
	result := make(map[string]beadParentMock)
	for id, deps := range m.deps {
		if !idSet[id] {
			continue
		}
		var best *model.Dependency
		for i := range deps {
			d := deps[i]
			if !parentEdge[d.Type] {
				continue
			}
			// Mirror DISTINCT ON + the meaningful ORDER BY in the real query
			// (ka-0000001227): prefer an OPEN parent, then an EPIC, then the
			// most recent edge, then the lowest parent id.
			if best == nil || m.parentEdgeLess(d, best) {
				best = d
			}
		}
		if best != nil {
			var title string
			if p, ok := m.beads[best.DependsOnID]; ok {
				title = p.Title
			}
			result[id] = beadParentMock{ID: best.DependsOnID, Title: title}
		}
	}
	return result
}

// parentEdgeLess reports whether parent-child edge a is a BETTER "primary"
// parent than b, mirroring the ORDER BY tie-break in queryGetParentsForBeads
// (ka-0000001227): open-over-closed, then epic-over-non-epic, then most-recent
// edge, then lowest parent id. A parent not present in m.beads is treated as
// out-of-scope/soft-deleted (COALESCE(..., true) in SQL), so it loses to a
// visible open epic.
func (m *mockStore) parentEdgeLess(a, b *model.Dependency) bool {
	aClosed, aEpic := m.parentClosedEpic(a.DependsOnID)
	bClosed, bEpic := m.parentClosedEpic(b.DependsOnID)
	if aClosed != bClosed {
		return !aClosed // open (false) sorts first
	}
	if aEpic != bEpic {
		return aEpic // epic sorts first
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt) // most recent first
	}
	return a.DependsOnID < b.DependsOnID
}

// parentClosedEpic reports (parentIsClosedOrInvisible, parentIsEpic) for the
// given parent id, matching the COALESCE defaults in the SQL tie-break: a
// parent missing from m.beads counts as closed (deprioritised) and non-epic.
//
// Unlocked: reached only via getParentsForBeads <- ListBeads, which already
// holds beadsMu.
func (m *mockStore) parentClosedEpic(parentID string) (closed, epic bool) {
	p, ok := m.beads[parentID]
	if !ok {
		return true, false
	}
	return p.Status == model.StatusClosed, p.Type == model.TypeEpic
}

// beadParentMock mirrors the postgres beadParent scan tuple.
type beadParentMock struct {
	ID    string
	Title string
}

// sortMockBeads orders beads to mirror the postgres queryListBeads ORDER BY:
// the default (empty sort) is `created_at DESC`; an explicit `created_at`
// sorts ascending. A stable `id` tiebreak makes the order deterministic even
// when created_at ties (seedBead leaves CreatedAt zero), so paged tests are
// reproducible. Only the columns the auto-close paged walk relies on are
// modeled — this is a test double, not the full parseSortClause grammar.
func sortMockBeads(beads []*model.Bead, sortSpec string) {
	desc := true // postgres default: created_at DESC
	switch strings.TrimSpace(sortSpec) {
	case "", "-created_at":
		desc = true
	case "created_at":
		desc = false
	}
	sort.SliceStable(beads, func(i, j int) bool {
		ti, tj := beads[i].CreatedAt, beads[j].CreatedAt
		if !ti.Equal(tj) {
			if desc {
				return ti.After(tj)
			}
			return ti.Before(tj)
		}
		if desc {
			return beads[i].ID > beads[j].ID
		}
		return beads[i].ID < beads[j].ID
	})
}

// hasParentChildEdge reports whether childID has a direct parent-child (or
// legacy "parent") edge to parentID (childID is the bead_id / child side).
func (m *mockStore) hasParentChildEdge(childID, parentID string) bool {
	for _, d := range m.deps[childID] {
		if d.DependsOnID == parentID && model.IsParentChildDepType(string(d.Type)) {
			return true
		}
	}
	return false
}

// inParentSubtree reports whether childID is a transitive parent-child
// descendant of rootID. Walks UP from childID toward rootID with a visited set
// so a corrupt cyclic graph terminates.
func (m *mockStore) inParentSubtree(childID, rootID string) bool {
	visited := map[string]bool{}
	frontier := []string{childID}
	for len(frontier) > 0 {
		node := frontier[0]
		frontier = frontier[1:]
		if visited[node] {
			continue
		}
		visited[node] = true
		for _, d := range m.deps[node] {
			if !model.IsParentChildDepType(string(d.Type)) {
				continue
			}
			if d.DependsOnID == rootID {
				return true
			}
			if !visited[d.DependsOnID] {
				frontier = append(frontier, d.DependsOnID)
			}
		}
	}
	return false
}

func (m *mockStore) UpdateBead(_ context.Context, bead *model.Bead) error {
	if m.updateBeadErr != nil {
		err := m.updateBeadErr
		if m.updateBeadErrOnce {
			m.updateBeadErr = nil
		}
		return err
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	m.beads[bead.ID] = bead
	return nil
}

// FillShortTitle mirrors queryFillShortTitle: write short_title only while
// the stored value is blank, under the same lock as the check.
func (m *mockStore) FillShortTitle(_ context.Context, bead *model.Bead) (bool, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[bead.ID]
	if !ok || b.DeletedAt != nil || strings.TrimSpace(b.ShortTitle) != "" {
		return false, nil
	}
	clone := *b
	clone.ShortTitle = bead.ShortTitle
	clone.UpdatedAt = time.Now().UTC()
	bead.UpdatedAt = clone.UpdatedAt
	m.beads[bead.ID] = &clone
	return true, nil
}

// LockBeadAssignee mirrors queryLockBeadAssignee. The mock has no row locks:
// RunInTransaction runs fn unserialized, so tests drive the compare-and-set
// through lockBeadAssigneeHook instead of real concurrency.
func (m *mockStore) LockBeadAssignee(_ context.Context, id string) (string, error) {
	if m.lockBeadAssigneeHook != nil {
		m.lockBeadAssigneeHook(id)
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok || b.DeletedAt != nil {
		return "", sql.ErrNoRows
	}
	return b.Assignee, nil
}

// MergeBeadFields mirrors queryMergeBeadFields' semantics (shallow top-level
// merge into fields, right-hand `extra` wins) as a single locked critical
// section, so tests exercising a concurrent-merge race (gd-P6uCMzQqU9) see
// the same no-lost-update guarantee the real atomic UPDATE gives. It honors
// updateBeadErr/updateBeadErrOnce exactly like UpdateBead so existing tests
// that inject a fields-write failure through those fields keep working now
// that the handler-level mergeBeadFields helper calls this instead of
// Get+UpdateBead.
func (m *mockStore) MergeBeadFields(_ context.Context, id string, extra map[string]any) (*model.Bead, error) {
	if m.updateBeadErr != nil {
		err := m.updateBeadErr
		if m.updateBeadErrOnce {
			m.updateBeadErr = nil
		}
		return nil, err
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, nil
	}

	existing := make(map[string]any)
	if len(b.Fields) > 0 {
		_ = json.Unmarshal(b.Fields, &existing)
	}
	for k, v := range extra {
		existing[k] = v
	}
	merged, err := json.Marshal(existing)
	if err != nil {
		return nil, err
	}
	b.Fields = merged
	b.UpdatedAt = time.Now().UTC()

	clone := *b
	clone.Labels = m.labels[id]
	return &clone, nil
}

func (m *mockStore) IncrementBeadFields(_ context.Context, id string, deltas map[string]float64) (*model.Bead, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, nil
	}
	existing := make(map[string]any)
	if len(b.Fields) > 0 {
		_ = json.Unmarshal(b.Fields, &existing)
	}
	for k, d := range deltas {
		cur := 0.0
		if v, ok := existing[k]; ok {
			switch vt := v.(type) {
			case float64:
				cur = vt
			case string:
				cur, _ = strconv.ParseFloat(vt, 64)
			}
		}
		existing[k] = cur + d
	}
	merged, _ := json.Marshal(existing)
	b.Fields = merged
	b.UpdatedAt = time.Now().UTC()
	clone := *b // a copy, as the postgres RETURNING row is
	return &clone, nil
}

// mockArrayField decodes the current string-array value of a bead field,
// defaulting to an empty slice when absent/blank/unparseable — mirroring the
// COALESCE(..., '[]') default the postgres primitives use.
func mockArrayField(b *model.Bead, field string) []string {
	existing := make(map[string]any)
	if len(b.Fields) > 0 {
		_ = json.Unmarshal(b.Fields, &existing)
	}
	raw, ok := existing[field]
	if !ok {
		return []string{}
	}
	arr, ok := raw.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func mockSetArrayField(b *model.Bead, field string, val []string) {
	existing := make(map[string]any)
	if len(b.Fields) > 0 {
		_ = json.Unmarshal(b.Fields, &existing)
	}
	existing[field] = val
	merged, _ := json.Marshal(existing)
	b.Fields = merged
	b.UpdatedAt = time.Now().UTC()
}

func (m *mockStore) AllocateAgentHandleSeq(_ context.Context, gardenerID string) (int64, error) {
	if m.handleSeqs == nil {
		m.handleSeqs = make(map[string]int64)
	}
	m.handleSeqs[gardenerID]++
	return m.handleSeqs[gardenerID], nil
}

func (m *mockStore) AppendBeadArrayField(_ context.Context, id, field, element string) (*model.Bead, bool, error) {
	if !store.ValidFieldKey(field) {
		return nil, false, store.ErrInvalidFieldKey
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, false, sql.ErrNoRows
	}
	cur := mockArrayField(b, field)
	for _, e := range cur {
		if e == element {
			return b, false, nil // idempotent no-op
		}
	}
	mockSetArrayField(b, field, append(cur, element))
	return b, true, nil
}

func (m *mockStore) InsertBeadArrayField(_ context.Context, id, field, element string, index int) (*model.Bead, bool, error) {
	if !store.ValidFieldKey(field) {
		return nil, false, store.ErrInvalidFieldKey
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, false, sql.ErrNoRows
	}
	cur := mockArrayField(b, field)
	if slices.Contains(cur, element) {
		return b, false, nil // append-if-absent: already present → no-op
	}
	if index < 0 {
		index = 0
	}
	if index > len(cur) {
		index = len(cur)
	}
	mockSetArrayField(b, field, slices.Insert(cur, index, element))
	return b, true, nil
}

func (m *mockStore) CASBeadArrayField(_ context.Context, id, field string, expected, next []string) (*model.Bead, error) {
	if !store.ValidFieldKey(field) {
		return nil, store.ErrInvalidFieldKey
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if !slices.Equal(mockArrayField(b, field), expected) {
		return nil, store.ErrCASConflict
	}
	if next == nil {
		next = []string{}
	}
	mockSetArrayField(b, field, next)
	return b, nil
}

// mockFieldMap decodes the current top-level fields object as raw-JSON values so
// the sub-field mocks can mutate one key and re-marshal the rest untouched.
func mockFieldMap(b *model.Bead) map[string]json.RawMessage {
	fields := map[string]json.RawMessage{}
	if len(b.Fields) > 0 {
		_ = json.Unmarshal(b.Fields, &fields)
	}
	return fields
}

func mockWriteFields(b *model.Bead, fields map[string]json.RawMessage) {
	merged, _ := json.Marshal(fields)
	b.Fields = merged
	b.UpdatedAt = time.Now().UTC()
}

func (m *mockStore) MergeBeadMapField(_ context.Context, id, field, op, key, value string) (*model.Bead, bool, error) {
	if !store.ValidFieldKey(field) {
		return nil, false, store.ErrInvalidFieldKey
	}
	if key == "" {
		return nil, false, store.ErrInvalidSubFieldEdit
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, false, sql.ErrNoRows
	}
	fields := mockFieldMap(b)
	sub := map[string]string{}
	if raw, ok := fields[field]; ok && len(raw) > 0 {
		// Tolerate the native-object and JSON-string storage shapes, like the
		// real decodeStringMapField.
		if err := json.Unmarshal(raw, &sub); err != nil {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, false, store.ErrInvalidSubFieldEdit
			}
			if s != "" {
				if err := json.Unmarshal([]byte(s), &sub); err != nil {
					return nil, false, store.ErrInvalidSubFieldEdit
				}
			}
		}
	}
	switch op {
	case "set":
		if cur, ok := sub[key]; ok && cur == value {
			return b, false, nil
		}
		sub[key] = value
	case "unset-key":
		if _, ok := sub[key]; !ok {
			return b, false, nil
		}
		delete(sub, key)
	default:
		return nil, false, store.ErrInvalidSubFieldEdit
	}
	encoded, _ := json.Marshal(sub)
	fields[field] = encoded
	mockWriteFields(b, fields)
	return b, true, nil
}

func (m *mockStore) EditBeadCSVField(_ context.Context, id, field, op, element string) (*model.Bead, bool, error) {
	if !store.ValidFieldKey(field) {
		return nil, false, store.ErrInvalidFieldKey
	}
	element = strings.TrimSpace(element)
	if element == "" || strings.Contains(element, ",") {
		return nil, false, store.ErrInvalidSubFieldEdit
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, false, sql.ErrNoRows
	}
	fields := mockFieldMap(b)
	var parts []string
	if raw, ok := fields[field]; ok && len(raw) > 0 {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, false, store.ErrInvalidSubFieldEdit
		}
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
	}
	var next []string
	switch op {
	case "append":
		if slices.Contains(parts, element) {
			return b, false, nil
		}
		next = append(parts, element)
	case "remove":
		next = make([]string, 0, len(parts))
		for _, p := range parts {
			if p != element {
				next = append(next, p)
			}
		}
		if len(next) == len(parts) {
			return b, false, nil
		}
	default:
		return nil, false, store.ErrInvalidSubFieldEdit
	}
	encoded, _ := json.Marshal(strings.Join(next, ","))
	fields[field] = encoded
	mockWriteFields(b, fields)
	return b, true, nil
}

func (m *mockStore) CloseBead(_ context.Context, id string, closedBy string) (*model.Bead, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, nil
	}
	now := time.Now().UTC()
	b.Status = model.StatusClosed
	b.ClosedAt = &now
	b.ClosedBy = closedBy
	b.UpdatedAt = now
	b.Labels = m.labels[id]
	return b, nil
}

func (m *mockStore) DeleteBead(_ context.Context, id string) error {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return sql.ErrNoRows
	}
	now := time.Now().UTC()
	b.DeletedAt = &now
	b.UpdatedAt = now
	return nil
}

func (m *mockStore) RestoreBead(_ context.Context, id string) (*model.Bead, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if b.DeletedAt == nil {
		return nil, sql.ErrNoRows
	}
	b.DeletedAt = nil
	b.UpdatedAt = time.Now().UTC()
	b.Labels = m.labels[id]
	return b, nil
}

func (m *mockStore) PurgeBeads(_ context.Context, req store.PurgeRequest) ([]store.PurgedBead, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	ids := []string{}
	for id, b := range m.beads {
		if req.Label != "" && !slices.Contains(m.labels[id], req.Label) && !slices.Contains(b.Labels, req.Label) {
			continue
		}
		if req.Label == "" && !slices.Contains(req.IDs, id) {
			continue
		}
		if slices.Contains(req.KeepTypes, string(b.Type)) || slices.Contains(req.KeepIDs, id) {
			continue
		}
		if req.KeepShared && req.Label != "" && slices.ContainsFunc(m.labels[id], func(l string) bool {
			return strings.HasPrefix(l, "project:") && l != req.Label
		}) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]store.PurgedBead, 0, len(ids))
	for _, id := range ids {
		out = append(out, store.PurgedBead{ID: id, Type: string(m.beads[id].Type), CreatedAt: m.beads[id].CreatedAt})
	}
	if !req.DryRun {
		for _, id := range ids {
			delete(m.beads, id)
			delete(m.labels, id)
		}
	}
	return out, nil
}

func (m *mockStore) TransferBead(_ context.Context, id string, toFleet string) (*model.Bead, string, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok {
		return nil, "", sql.ErrNoRows
	}
	fromFleet := b.Fleet
	b.Fleet = toFleet
	b.UpdatedAt = time.Now().UTC()
	b.Labels = m.labels[id]
	return b, fromFleet, nil
}

func (m *mockStore) GetStats(_ context.Context) (*model.GraphStats, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	stats := &model.GraphStats{}
	for _, b := range m.beads {
		switch b.Status {
		case model.StatusOpen:
			stats.TotalOpen++
		case model.StatusInProgress:
			stats.TotalInProgress++
		case model.StatusBlocked:
			stats.TotalBlocked++
		case model.StatusClosed:
			stats.TotalClosed++
		case model.StatusDeferred:
			stats.TotalDeferred++
		}
	}
	return stats, nil
}

func (m *mockStore) GetDependenciesForBeads(_ context.Context, beadIDs []string) (map[string][]*model.Dependency, error) {
	result := make(map[string][]*model.Dependency)
	idSet := make(map[string]bool, len(beadIDs))
	for _, id := range beadIDs {
		idSet[id] = true
	}
	for id, deps := range m.deps {
		if idSet[id] {
			result[id] = deps
		}
	}
	return result, nil
}

func (m *mockStore) GetReverseDependenciesForBeads(_ context.Context, beadIDs []string) (map[string][]*model.Dependency, error) {
	idSet := make(map[string]bool, len(beadIDs))
	for _, id := range beadIDs {
		idSet[id] = true
	}
	result := make(map[string][]*model.Dependency)
	for _, deps := range m.deps {
		for _, d := range deps {
			if idSet[d.DependsOnID] {
				result[d.DependsOnID] = append(result[d.DependsOnID], d)
			}
		}
	}
	return result, nil
}

// GetChildrenStatusHistogram returns, per parent, the histogram of
// parent-child children's statuses. Mirrors the postgres semantics: parents
// with no parent-child children are omitted, soft-deleted children excluded.
// The mock applies the parent-child filter directly on the in-memory deps
// map so tests can seed both blocks-edges and parent-child edges without
// the latter polluting the histogram.
func (m *mockStore) GetChildrenStatusHistogram(_ context.Context, parentIDs []string) (map[string]map[string]int, error) {
	if len(parentIDs) == 0 {
		return make(map[string]map[string]int), nil
	}
	want := make(map[string]bool, len(parentIDs))
	for _, id := range parentIDs {
		want[id] = true
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	result := make(map[string]map[string]int)
	for _, deps := range m.deps {
		for _, d := range deps {
			if d.Type != model.DepParentChild {
				continue
			}
			if !want[d.DependsOnID] {
				continue
			}
			child := m.beads[d.BeadID]
			if child == nil {
				continue
			}
			bucket, ok := result[d.DependsOnID]
			if !ok {
				bucket = make(map[string]int)
				result[d.DependsOnID] = bucket
			}
			bucket[string(child.Status)]++
		}
	}
	return result, nil
}

func (m *mockStore) GetDependencyCounts(_ context.Context, beadIDs []string) (map[string]*model.DependencyCounts, error) {
	result := make(map[string]*model.DependencyCounts)
	for _, id := range beadIDs {
		result[id] = &model.DependencyCounts{
			DependencyCount: len(m.deps[id]),
		}
	}
	for _, deps := range m.deps {
		for _, d := range deps {
			if dc, ok := result[d.DependsOnID]; ok {
				dc.DependentCount++
			}
		}
	}
	return result, nil
}

func (m *mockStore) FleetTypeStatusCounts(_ context.Context) (map[store.FleetTypeStatusKey]int, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	result := make(map[store.FleetTypeStatusKey]int)
	for _, b := range m.beads {
		if b.DeletedAt != nil {
			continue
		}
		key := store.FleetTypeStatusKey{Type: string(b.Type), Status: string(b.Status)}
		result[key]++
	}
	return result, nil
}

func (m *mockStore) FleetActiveAgentsByProject(_ context.Context) (map[string]int, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	out := make(map[string]int)
	for _, b := range m.beads {
		if b.DeletedAt != nil || b.Type != "agent" {
			continue
		}
		if b.Status == model.StatusOpen || b.Status == model.StatusInProgress {
			out[projectFromLabels(m.labels[b.ID])]++
		}
	}
	return out, nil
}

func (m *mockStore) FleetAgentLifetimeRollup(_ context.Context) ([]store.FleetAgentProjectRollup, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	rollup := make(map[string]*store.FleetAgentProjectRollup)
	for _, b := range m.beads {
		if b.DeletedAt != nil {
			continue
		}
		if b.Type != "agent" {
			continue
		}
		project := projectFromLabels(m.labels[b.ID])
		row, ok := rollup[project]
		if !ok {
			row = &store.FleetAgentProjectRollup{Project: project}
			rollup[project] = row
		}
		row.TotalAgents++
		var fields map[string]any
		if len(b.Fields) > 0 {
			_ = json.Unmarshal(b.Fields, &fields)
		}
		if cost, ok := parseFloat(fields, "total_cost_usd"); ok {
			row.TotalCostUSD += cost
		}
		if sessions, ok := parseFloat(fields, "session_count"); ok {
			row.SessionCount += sessions
		}
	}
	out := make([]store.FleetAgentProjectRollup, 0, len(rollup))
	for _, r := range rollup {
		out = append(out, *r)
	}
	return out, nil
}

func (m *mockStore) FleetQueueDepthByProject(_ context.Context) (map[store.FleetQueueDepthKey]int, error) {
	queueTypes := make(map[string]bool, len(store.FleetQueueTypes))
	for _, t := range store.FleetQueueTypes {
		queueTypes[t] = true
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	result := make(map[store.FleetQueueDepthKey]int)
	for _, b := range m.beads {
		if b.DeletedAt != nil {
			continue
		}
		if b.Status != model.StatusOpen || !queueTypes[string(b.Type)] {
			continue
		}
		key := store.FleetQueueDepthKey{Project: projectFromLabels(m.labels[b.ID]), Type: string(b.Type)}
		result[key]++
	}
	return result, nil
}

func (m *mockStore) FleetQueueOldestAgeByProject(_ context.Context) (map[store.FleetQueueDepthKey]float64, error) {
	queueTypes := make(map[string]bool, len(store.FleetQueueTypes))
	for _, t := range store.FleetQueueTypes {
		queueTypes[t] = true
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	// Track the oldest (minimum) CreatedAt per bucket, then convert to an age.
	oldest := make(map[store.FleetQueueDepthKey]time.Time)
	for _, b := range m.beads {
		if b.DeletedAt != nil {
			continue
		}
		if b.Status != model.StatusOpen || !queueTypes[string(b.Type)] {
			continue
		}
		key := store.FleetQueueDepthKey{Project: projectFromLabels(m.labels[b.ID]), Type: string(b.Type)}
		if prev, ok := oldest[key]; !ok || b.CreatedAt.Before(prev) {
			oldest[key] = b.CreatedAt
		}
	}
	result := make(map[store.FleetQueueDepthKey]float64, len(oldest))
	for key, created := range oldest {
		result[key] = time.Since(created).Seconds()
	}
	return result, nil
}

// BackfillProjectLabelsPage mirrors queryBackfillProjectLabelsPage.
func (m *mockStore) BackfillProjectLabelsPage(ctx context.Context, projectTypes []string, batchSize int) (*store.BackfillProjectLabelsStats, error) {
	type candidate struct{ id, beadType, label string }
	var page []candidate
	m.beadsMu.Lock()
	for _, b := range m.beads {
		if b.DeletedAt != nil || hasProjectLabel(m.labels[b.ID]) {
			continue
		}
		switch {
		case slices.Contains(projectTypes, string(b.Type)):
			if p := strings.TrimSpace(projectFromFields(b.Fields)); p != "" {
				page = append(page, candidate{b.ID, string(b.Type), "project:" + p})
			}
		case b.Type == "message":
			head, _, _ := exactFieldString(b.Fields, "thread_id")
			if ps := knownProjectLabels(m.labels[strings.TrimSpace(head)]); head != "" && len(ps) == 1 {
				page = append(page, candidate{b.ID, "message", "project:" + ps[0]})
			}
		}
	}
	m.beadsMu.Unlock()
	if len(page) > batchSize {
		page = page[:batchSize]
	}
	stats := &store.BackfillProjectLabelsStats{Labelled: map[string]int{}, Done: len(page) < batchSize}
	for _, c := range page {
		if err := m.AddLabel(ctx, c.id, c.label); err != nil {
			return nil, err
		}
		stats.Labelled[c.beadType]++
	}
	return stats, nil
}

// ProjectLabelMissingCounts mirrors queryProjectLabelMissingCounts.
func (m *mockStore) ProjectLabelMissingCounts(_ context.Context, projectTypes []string) (map[string]int, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	result := make(map[string]int)
	for _, b := range m.beads {
		if b.DeletedAt != nil || hasProjectLabel(m.labels[b.ID]) {
			continue
		}
		switch {
		case slices.Contains(projectTypes, string(b.Type)):
			if strings.TrimSpace(projectFromFields(b.Fields)) != "" {
				result[string(b.Type)]++
			}
		case b.Type == "message":
			head, _, _ := exactFieldString(b.Fields, "thread_id")
			if head != "" && len(knownProjectLabels(m.labels[strings.TrimSpace(head)])) == 1 {
				result["message"]++
			}
		}
	}
	return result, nil
}

func (m *mockStore) AcceptanceMetrics(_ context.Context) (*store.AcceptanceMetrics, error) {
	if m.acceptanceMetrics != nil {
		return m.acceptanceMetrics, nil
	}
	return &store.AcceptanceMetrics{}, nil
}

func (m *mockStore) GetLabelsForBeads(_ context.Context, beadIDs []string) (map[string][]string, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	result := make(map[string][]string)
	for _, id := range beadIDs {
		if labels, ok := m.labels[id]; ok {
			result[id] = labels
		}
	}
	return result, nil
}

func (m *mockStore) GetBlockedByForBeads(_ context.Context, beadIDs []string) (map[string][]string, error) {
	idSet := make(map[string]bool, len(beadIDs))
	for _, id := range beadIDs {
		idSet[id] = true
	}
	result := make(map[string][]string)
	for id, deps := range m.deps {
		if !idSet[id] {
			continue
		}
		for _, d := range deps {
			if d.Type == model.DepBlocks {
				result[id] = append(result[id], d.DependsOnID)
			}
		}
	}
	return result, nil
}

// GetOpenBlockersForBeads mirrors the postgres query's semantics: only
// genuinely-blocking edge types pointing at a bead in a non-terminal status,
// denormalized with that bead's status/type/title.
//
// This is a HAND-WRITTEN reimplementation, not the production query, so the
// handler tests it backs prove the HTTP plumbing (param -> filter -> response
// shape), NOT the SQL. The blocking rules themselves are pinned against the
// real query in internal/store/postgres (TestQueryListBeads_WithOpenBlockers,
// TestQueryGetOpenBlockersForBeads*). Keep the two in agreement.
func (m *mockStore) GetOpenBlockersForBeads(_ context.Context, beadIDs []string) (map[string][]*model.OpenBlocker, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	return m.getOpenBlockersForBeadsLocked(beadIDs), nil
}

// getOpenBlockersForBeadsLocked is the unlocked body of GetOpenBlockersForBeads.
// ListBeads (WithOpenBlockers enrichment) calls this directly instead of the
// exported method because it already holds beadsMu — calling the exported,
// locking method from there would deadlock.
func (m *mockStore) getOpenBlockersForBeadsLocked(beadIDs []string) map[string][]*model.OpenBlocker {
	idSet := make(map[string]bool, len(beadIDs))
	for _, id := range beadIDs {
		idSet[id] = true
	}
	blockingEdge := map[model.DependencyType]bool{
		model.DepBlocks: true, "blocked-by": true, "depends-on": true,
	}
	openStatus := map[model.Status]bool{
		model.StatusOpen: true, model.StatusInProgress: true,
		model.StatusBlocked: true, model.StatusDeferred: true,
	}
	result := make(map[string][]*model.OpenBlocker)
	for id, deps := range m.deps {
		if !idSet[id] {
			continue
		}
		for _, d := range deps {
			if !blockingEdge[d.Type] {
				continue
			}
			blocker, ok := m.beads[d.DependsOnID]
			if !ok || !openStatus[blocker.Status] {
				continue
			}
			result[id] = append(result[id], &model.OpenBlocker{
				ID:       blocker.ID,
				Status:   blocker.Status,
				Type:     blocker.Type,
				Title:    blocker.Title,
				EdgeType: d.Type,
			})
		}
	}
	return result
}

func (m *mockStore) GetBeadsByIDs(_ context.Context, ids []string) ([]*model.Bead, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	var result []*model.Bead
	for _, id := range ids {
		if b, ok := m.beads[id]; ok {
			clone := *b
			clone.Labels = m.labels[id]
			result = append(result, &clone)
		}
	}
	return result, nil
}

func (m *mockStore) GetTextReferencesForBeads(_ context.Context, beadIDs []string) ([]*model.BeadReference, error) {
	if len(m.references) == 0 {
		return nil, nil
	}
	idSet := make(map[string]bool, len(beadIDs))
	for _, id := range beadIDs {
		idSet[id] = true
	}
	var result []*model.BeadReference
	for _, ref := range m.references {
		if idSet[ref.SourceBeadID] && idSet[ref.TargetBeadID] {
			result = append(result, ref)
		}
	}
	return result, nil
}

func (m *mockStore) AddDependency(_ context.Context, dep *model.Dependency) error {
	m.deps[dep.BeadID] = append(m.deps[dep.BeadID], dep)
	return nil
}

func (m *mockStore) RemoveDependency(_ context.Context, beadID, dependsOnID string, depType model.DependencyType) (bool, error) {
	deps := m.deps[beadID]
	for i, d := range deps {
		if d.DependsOnID == dependsOnID && d.Type == depType {
			m.deps[beadID] = append(deps[:i:i], deps[i+1:]...)
			return true, nil
		}
	}
	// Report the 0-match truthfully so handler tests can pin that a no-op
	// remove emits no event and no audit row (ka-0000001966).
	return false, nil
}

func (m *mockStore) GetDependencies(_ context.Context, beadID string) ([]*model.Dependency, error) {
	return m.deps[beadID], nil
}

func (m *mockStore) GetReverseDependencies(_ context.Context, beadID string) ([]*model.Dependency, error) {
	var result []*model.Dependency
	for _, deps := range m.deps {
		for _, d := range deps {
			if d.DependsOnID == beadID {
				result = append(result, d)
			}
		}
	}
	return result, nil
}

func (m *mockStore) GetDependencyEdgesByType(_ context.Context, depType model.DependencyType) ([]model.DependencyEdge, error) {
	var edges []model.DependencyEdge
	for _, deps := range m.deps {
		for _, d := range deps {
			if d.Type == depType {
				edges = append(edges, model.DependencyEdge{BeadID: d.BeadID, DependsOnID: d.DependsOnID})
			}
		}
	}
	return edges, nil
}

func (m *mockStore) ListBeadsMentioning(_ context.Context, beadID string, limit, offset int, relationshipType *string) ([]*model.BeadReferenceSummary, int, error) {
	m.lastMentionedByID = beadID
	m.lastMentionedByLimit = limit
	m.lastMentionedByOffset = offset
	m.lastMentionedByRelType = relationshipType
	if m.mentionedByErr != nil {
		return nil, 0, m.mentionedByErr
	}
	return m.mentionedBy, m.mentionedByTotal, nil
}

// HasBundleChild scans in-memory deps + beads to mirror the postgres semantics:
// at least one parent-child reference whose source bead has type=bundle.
func (m *mockStore) HasBundleChild(_ context.Context, parentID string) (bool, error) {
	if m.hasBundleChildErr != nil {
		return false, m.hasBundleChildErr
	}
	if v, ok := m.hasBundleChild[parentID]; ok {
		return v, nil
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	for _, ref := range m.references {
		if ref.TargetBeadID != parentID {
			continue
		}
		if ref.RelationshipType == nil {
			continue
		}
		switch *ref.RelationshipType {
		case "parent", "child-of", string(model.DepParentChild):
		default:
			continue
		}
		src := m.beads[ref.SourceBeadID]
		if src != nil && string(src.Type) == "bundle" && src.DeletedAt == nil {
			return true, nil
		}
	}
	return false, nil
}

func (m *mockStore) ListBeadReferencesFrom(_ context.Context, beadID string, limit, offset int, relationshipType *string) ([]*model.BeadReferenceSummary, int, error) {
	m.lastMentionsID = beadID
	m.lastMentionsLimit = limit
	m.lastMentionsOffset = offset
	m.lastMentionsRelType = relationshipType
	if m.mentionsErr != nil {
		return nil, 0, m.mentionsErr
	}
	return m.mentions, m.mentionsTotal, nil
}

func (m *mockStore) AddLabel(_ context.Context, beadID string, label string) error {
	if m.addLabelErr != nil {
		return m.addLabelErr
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	// Skip duplicates (mirrors ON CONFLICT DO NOTHING).
	for _, l := range m.labels[beadID] {
		if l == label {
			return nil
		}
	}
	m.labels[beadID] = append(m.labels[beadID], label)
	return nil
}

func (m *mockStore) RemoveLabel(_ context.Context, beadID string, label string) (bool, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	labels := m.labels[beadID]
	for i, l := range labels {
		if l == label {
			m.labels[beadID] = append(labels[:i:i], labels[i+1:]...)
			return true, nil
		}
	}
	// Report the 0-match truthfully so handler tests can pin that a no-op
	// remove emits no event (ka-0000000424).
	return false, nil
}

func (m *mockStore) GetLabels(_ context.Context, beadID string) ([]string, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	return m.labels[beadID], nil
}

// taintHasGateEffect reports whether taint id's effects include "gate"
// (docs/adr/taints.md §15.2). A taint absent from the registry, or whose
// Effects is nil (never set by the seeding test), defaults to gate=true —
// matching the real column's NOT NULL DEFAULT '{gate}' so every taint seeded
// before this task (which never sets Effects) keeps gating exactly as it did
// before gd-9YQiNJVW6u. A taint whose Effects is explicitly set to a
// non-nil, possibly-empty slice is judged on that slice alone — a
// {}-effect taint (trace-only dye, §15.2) must not gate.
func (m *mockStore) taintHasGateEffect(id int64) bool {
	for _, t := range m.taintRegistry {
		if t.ID == id {
			if t.Effects == nil {
				return true
			}
			return model.HasEffect(t.Effects, model.EffectGate)
		}
	}
	return true
}

// gateTaintIDs filters ids to those whose effects include "gate" — the read
// gate, write gate, and list predicate consult only gate-effect taints
// (docs/adr/taints.md §15.2, gd-9YQiNJVW6u item 5).
func (m *mockStore) gateTaintIDs(ids []int64) []int64 {
	var out []int64
	for _, id := range ids {
		if m.taintHasGateEffect(id) {
			out = append(out, id)
		}
	}
	return out
}

func (m *mockStore) GetTaintsForBead(_ context.Context, beadID string) ([]int64, error) {
	m.beadTaintQueryCalls++
	return m.gateTaintIDs(m.taintIDsForBeadLocked(beadID)), nil
}

func (m *mockStore) GetTaintsForBeads(_ context.Context, beadIDs []string) (map[string][]int64, error) {
	m.beadTaintQueryCalls++
	out := make(map[string][]int64)
	for _, id := range beadIDs {
		if ids := m.gateTaintIDs(m.taintIDsForBeadLocked(id)); len(ids) > 0 {
			out[id] = ids
		}
	}
	return out, nil
}

// GetAllTaintsForBead returns every taint id on beadID regardless of
// effects — propagation (docs/adr/taints.md §4.2) is effect-agnostic, unlike
// GetTaintsForBead above which gates.
func (m *mockStore) GetAllTaintsForBead(_ context.Context, beadID string) ([]int64, error) {
	m.beadTaintQueryCalls++
	return m.taintIDsForBeadLocked(beadID), nil
}

func (m *mockStore) GetAllTaintsForBeads(_ context.Context, beadIDs []string) (map[string][]int64, error) {
	m.beadTaintQueryCalls++
	out := make(map[string][]int64)
	for _, id := range beadIDs {
		if ids := m.taintIDsForBeadLocked(id); len(ids) > 0 {
			out[id] = ids
		}
	}
	return out, nil
}

// taintIDsForBeadLocked unions m.taints (pre-seeded fixture state) with the
// ids recorded in m.beadTaints (rows written via ApplyTaint during the test,
// e.g. by taint propagation on create). The real store backs both
// GetTaintsForBead and ApplyTaint with the SAME bead_taints table, so a taint
// applied earlier in a test — such as a molecule inheriting inherit:create
// from a tainted pouring agent — must be visible to a later GetTaintsForBead
// call, exactly as it would be against Postgres (docs/adr/taints.md §4.2 row
// 6: molecule taints must propagate to steps created afterward in the same
// request).
func (m *mockStore) taintIDsForBeadLocked(beadID string) []int64 {
	seen := make(map[int64]bool)
	var out []int64
	for _, id := range m.taints[beadID] {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, bt := range m.beadTaints[beadID] {
		if !seen[bt.TaintID] {
			seen[bt.TaintID] = true
			out = append(out, bt.TaintID)
		}
	}
	return out
}

func (m *mockStore) GetGrantedTaintIDs(_ context.Context, principalKind string, principals []string) ([]int64, error) {
	principalSet := make(map[string]bool, len(principals))
	for _, p := range principals {
		principalSet[p] = true
	}
	now := time.Now()
	seen := make(map[int64]bool)
	m.taintGrantsMu.Lock()
	defer m.taintGrantsMu.Unlock()
	for _, g := range m.taintGrants {
		if g.PrincipalKind != principalKind || !principalSet[g.Principal] {
			continue
		}
		if g.ExpiresAt != nil && !g.ExpiresAt.After(now) {
			continue
		}
		seen[g.TaintID] = true
	}
	if len(seen) == 0 {
		return nil, nil
	}
	out := make([]int64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out, nil
}

func (m *mockStore) GetTaintIDsOwnedByTeams(_ context.Context, teams []string) ([]int64, error) {
	teamSet := make(map[string]bool, len(teams))
	for _, t := range teams {
		teamSet[t] = true
	}
	var out []int64
	for _, t := range m.taintRegistry {
		if teamSet[t.OwnerTeam] {
			out = append(out, t.ID)
		}
	}
	return out, nil
}

func (m *mockStore) HasAnyTaints(_ context.Context) (bool, error) {
	return len(m.taintRegistry) > 0, nil
}

// SetTaint mirrors the scrub_policy validation branch of the real store's
// querySetTaint (internal/store/postgres/queries.go) — a mock that silently
// accepted an invalid policy would make handleUpdateTaintPolicy's
// errors.As(err, &ve) rejection path untestable at the HTTP layer. It does
// NOT replicate querySetTaint's owner_team-resolves-to-a-team-bead check;
// tests that need an unvalidated seed (e.g. no scrub_policy at all) should
// use putTaint instead, which writes directly into the registry.
func (m *mockStore) SetTaint(_ context.Context, t *model.Taint) error {
	if m.taintRegistry == nil {
		m.taintRegistry = make(map[string]*model.Taint)
	}
	if len(t.ScrubPolicy) > 0 && string(t.ScrubPolicy) != "null" {
		var parsed model.ScrubPolicy
		if err := json.Unmarshal(t.ScrubPolicy, &parsed); err != nil {
			return &model.ValidationError{Errors: []model.FieldError{
				{Field: "scrub_policy", Message: "must be valid JSON: " + err.Error()},
			}}
		}
		effects := t.Effects
		if effects == nil {
			effects = model.DefaultEffects()
		}
		if err := model.ValidateScrubPolicy(&parsed, effects); err != nil {
			return err
		}
	}
	m.taintRegistry[t.Name] = t
	return nil
}

func (m *mockStore) GetTaint(_ context.Context, name string) (*model.Taint, error) {
	t, ok := m.taintRegistry[name]
	if !ok {
		return nil, sql.ErrNoRows
	}
	// Return a copy, not the registry's own pointer: the real store's
	// GetTaint scans a fresh row each call, so a caller that mutates a field
	// on the result (e.g. handleUpdateTaintPolicy setting ScrubPolicy before
	// calling SetTaint) and then has SetTaint fail must not have already
	// clobbered the persisted value.
	cp := *t
	return &cp, nil
}

func (m *mockStore) ListTaints(_ context.Context) ([]*model.Taint, error) {
	var result []*model.Taint
	for _, t := range m.taintRegistry {
		result = append(result, t)
	}
	return result, nil
}

func taintGrantKey(taintID int64, principalKind, principal string) string {
	return fmt.Sprintf("%d\x00%s\x00%s", taintID, principalKind, principal)
}

func (m *mockStore) GrantTaint(_ context.Context, g *model.TaintGrant) error {
	var ve model.ValidationError
	if !model.IsValidPrincipalKind(g.PrincipalKind) {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "principal_kind", Message: "unknown kind"})
	}
	if g.Principal == "" {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "principal", Message: "is required"})
	}
	if g.GrantedBy == 0 {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "granted_by", Message: "is required"})
	}
	if ve.HasErrors() {
		return &ve
	}
	m.taintGrantsMu.Lock()
	defer m.taintGrantsMu.Unlock()
	if m.taintGrants == nil {
		m.taintGrants = make(map[string]*model.TaintGrant)
	}
	g.GrantedAt = time.Now().UTC()
	stored := *g
	m.taintGrants[taintGrantKey(g.TaintID, g.PrincipalKind, g.Principal)] = &stored
	return nil
}

func (m *mockStore) RevokeTaint(_ context.Context, taintID int64, principalKind, principal string) (bool, error) {
	m.taintGrantsMu.Lock()
	defer m.taintGrantsMu.Unlock()
	key := taintGrantKey(taintID, principalKind, principal)
	if _, ok := m.taintGrants[key]; !ok {
		return false, nil
	}
	delete(m.taintGrants, key)
	return true, nil
}

func (m *mockStore) GetTaintGrant(_ context.Context, taintID int64, principalKind, principal string) (*model.TaintGrant, error) {
	m.taintGrantsMu.Lock()
	defer m.taintGrantsMu.Unlock()
	g, ok := m.taintGrants[taintGrantKey(taintID, principalKind, principal)]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if g.ExpiresAt != nil && !g.ExpiresAt.After(time.Now()) {
		return nil, sql.ErrNoRows
	}
	return g, nil
}

func (m *mockStore) ApplyTaint(_ context.Context, bt *model.BeadTaint) (bool, error) {
	m.applyTaintCalls++
	var ve model.ValidationError
	if bt.BeadID == "" {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "bead_id", Message: "is required"})
	}
	if bt.Source == "" {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "source", Message: "is required"})
	}
	if bt.AppliedBy == "" {
		ve.Errors = append(ve.Errors, model.FieldError{Field: "applied_by", Message: "is required"})
	}
	if ve.HasErrors() {
		return false, &ve
	}
	if m.beadTaints == nil {
		m.beadTaints = make(map[string][]*model.BeadTaint)
	}
	for _, existing := range m.beadTaints[bt.BeadID] {
		if existing.TaintID == bt.TaintID {
			return false, nil // idempotent no-op, preserves original provenance
		}
	}
	stored := *bt
	stored.AppliedAt = time.Now().UTC()
	for _, t := range m.taintRegistry {
		if t.ID == bt.TaintID {
			stored.TaintName = t.Name
			break
		}
	}
	m.beadTaints[bt.BeadID] = append(m.beadTaints[bt.BeadID], &stored)
	return true, nil
}

func (m *mockStore) GetBeadTaints(_ context.Context, beadID string) ([]*model.BeadTaint, error) {
	m.beadTaintQueryCalls++
	return m.beadTaints[beadID], nil
}

// GetGateBeadTaints is GetBeadTaints filtered to gate-effect taints only —
// backs the write gate (docs/adr/taints.md §15.2).
func (m *mockStore) GetGateBeadTaints(_ context.Context, beadID string) ([]*model.BeadTaint, error) {
	m.beadTaintQueryCalls++
	var out []*model.BeadTaint
	for _, bt := range m.beadTaints[beadID] {
		if m.taintHasGateEffect(bt.TaintID) {
			out = append(out, bt)
		}
	}
	return out, nil
}

func (m *mockStore) RecordTaintAudit(_ context.Context, a *model.TaintAudit) error {
	m.taintAuditNextID++
	a.ID = m.taintAuditNextID
	a.CreatedAt = time.Now().UTC()
	stored := *a
	m.taintAudit = append(m.taintAudit, &stored)
	return nil
}

// ScrubTaint is a stand-in for the real cascade (internal/store/postgres/
// scrub_taint.go, tested independently against go-sqlmock): it records every
// call so http_scrub_requests_test.go can assert executeScrubRequest built
// the right model.ScrubTaintRequest, and returns scrubTaintErr/
// scrubTaintResult when a test sets them, else a canned per-bead success.
func (m *mockStore) ScrubTaint(_ context.Context, req *model.ScrubTaintRequest) (*model.ScrubTaintResult, error) {
	m.scrubTaintCalls = append(m.scrubTaintCalls, req)
	if m.scrubTaintErr != nil {
		return nil, m.scrubTaintErr
	}
	if m.scrubTaintResult != nil {
		return m.scrubTaintResult, nil
	}
	beads := make([]model.ScrubTaintBeadResult, 0, len(req.BeadIDs))
	for _, id := range req.BeadIDs {
		beads = append(beads, model.ScrubTaintBeadResult{BeadID: id, ContentSHA: "sha-" + id})
	}
	return &model.ScrubTaintResult{Beads: beads}, nil
}

func (m *mockStore) ListTaintGrants(_ context.Context, taintID int64) ([]*model.TaintGrant, error) {
	m.taintGrantsMu.Lock()
	defer m.taintGrantsMu.Unlock()
	var out []*model.TaintGrant
	for _, g := range m.taintGrants {
		if g.TaintID == taintID {
			out = append(out, g)
		}
	}
	return out, nil
}

func (m *mockStore) ListTaintAudit(_ context.Context, taintID int64, filter model.TaintAuditFilter) ([]*model.TaintAudit, int, error) {
	var matched []*model.TaintAudit
	for _, a := range m.taintAudit {
		if a.TaintID != taintID {
			continue
		}
		if filter.Action != "" && a.Action != filter.Action {
			continue
		}
		if filter.Actor != "" && a.Actor != filter.Actor {
			continue
		}
		if filter.CreatedAfter != nil && a.CreatedAt.Before(*filter.CreatedAfter) {
			continue
		}
		matched = append(matched, a)
	}
	total := len(matched)
	if filter.Offset > 0 {
		if filter.Offset >= len(matched) {
			matched = nil
		} else {
			matched = matched[filter.Offset:]
		}
	}
	if filter.Limit > 0 && filter.Limit < len(matched) {
		matched = matched[:filter.Limit]
	}
	return matched, total, nil
}

func (m *mockStore) GetBeadTaintsForTaint(_ context.Context, taintID int64, beadIDs []string) (map[string]*model.BeadTaint, error) {
	wanted := make(map[string]bool, len(beadIDs))
	for _, id := range beadIDs {
		wanted[id] = true
	}
	out := make(map[string]*model.BeadTaint)
	for beadID, taints := range m.beadTaints {
		if !wanted[beadID] {
			continue
		}
		for _, bt := range taints {
			if bt.TaintID == taintID {
				out[beadID] = bt
				break
			}
		}
	}
	return out, nil
}

func (m *mockStore) AddComment(_ context.Context, comment *model.Comment) error {
	m.commentNextID++
	comment.ID = m.commentNextID
	m.comments[comment.BeadID] = append(m.comments[comment.BeadID], comment)
	return nil
}

func (m *mockStore) GetComments(_ context.Context, beadID string) ([]*model.Comment, error) {
	return m.comments[beadID], nil
}

func (m *mockStore) DeleteComment(_ context.Context, beadID string, commentID int64) (*model.Comment, error) {
	cs := m.comments[beadID]
	for i, c := range cs {
		if c.ID == commentID {
			m.comments[beadID] = append(cs[:i], cs[i+1:]...)
			return c, nil
		}
	}
	return nil, nil
}

func (m *mockStore) TrimComments(_ context.Context, beadID string, keepLast int, authorFilter string) ([]*model.Comment, error) {
	cs := m.comments[beadID]
	// Partition into "in scope" (subject to filter) and "out of scope".
	type indexed struct {
		idx int
		c   *model.Comment
	}
	var inScope []indexed
	for i, c := range cs {
		if authorFilter == "" || c.Author == authorFilter {
			inScope = append(inScope, indexed{i, c})
		}
	}
	// Sort inScope by created_at DESC, id DESC; keep first keepLast.
	sort.SliceStable(inScope, func(i, j int) bool {
		a, b := inScope[i].c, inScope[j].c
		if a.CreatedAt.Equal(b.CreatedAt) {
			return a.ID > b.ID
		}
		return a.CreatedAt.After(b.CreatedAt)
	})
	keep := map[int]bool{}
	for i := 0; i < len(inScope) && i < keepLast; i++ {
		keep[inScope[i].idx] = true
	}
	var deleted []*model.Comment
	var remaining []*model.Comment
	for i, c := range cs {
		inScopeHere := authorFilter == "" || c.Author == authorFilter
		if inScopeHere && !keep[i] {
			deleted = append(deleted, c)
		} else {
			remaining = append(remaining, c)
		}
	}
	sort.SliceStable(deleted, func(i, j int) bool {
		if deleted[i].CreatedAt.Equal(deleted[j].CreatedAt) {
			return deleted[i].ID < deleted[j].ID
		}
		return deleted[i].CreatedAt.Before(deleted[j].CreatedAt)
	})
	m.comments[beadID] = remaining
	return deleted, nil
}

func (m *mockStore) ReextractReferencesPage(_ context.Context, afterID string, batchSize int, beadType string) (*store.ReextractReferencesStats, error) {
	m.reextractCalls = append(m.reextractCalls, mockReextractCall{AfterID: afterID, BatchSize: batchSize, Type: beadType})
	if m.reextractFn != nil {
		return m.reextractFn(afterID, batchSize)
	}
	return &store.ReextractReferencesStats{Done: true}, nil
}

func (m *mockStore) BackfillSearchTSVPage(_ context.Context, afterID string, batchSize int, reindex bool) (*store.BackfillSearchTSVStats, error) {
	m.backfillSearchTSVCalls = append(m.backfillSearchTSVCalls, mockBackfillSearchTSVCall{AfterID: afterID, BatchSize: batchSize, Reindex: reindex})
	if m.backfillSearchTSVFn != nil {
		return m.backfillSearchTSVFn(afterID, batchSize, reindex)
	}
	return &store.BackfillSearchTSVStats{Done: true}, nil
}

func (m *mockStore) BackfillCategoryPage(_ context.Context, afterID string, batchSize int) (*store.BackfillCategoryStats, error) {
	m.backfillCategoryCalls = append(m.backfillCategoryCalls, mockBackfillCategoryCall{AfterID: afterID, BatchSize: batchSize})
	if m.backfillCategoryFn != nil {
		return m.backfillCategoryFn(afterID, batchSize)
	}
	return &store.BackfillCategoryStats{Done: true}, nil
}

func (m *mockStore) BackfillOperators(_ context.Context, opts store.BackfillOperatorsOptions) (*store.BackfillOperatorsStats, error) {
	m.backfillOperatorsCalls = append(m.backfillOperatorsCalls, opts)
	if m.backfillOperatorsFn != nil {
		return m.backfillOperatorsFn(opts)
	}
	return &store.BackfillOperatorsStats{DryRun: opts.DryRun}, nil
}

func (m *mockStore) BackfillOperatedByLegacy(_ context.Context, opts store.BackfillOperatedByLegacyOptions) (*store.BackfillOperatedByLegacyStats, error) {
	m.backfillOperatedByLegacyCalls = append(m.backfillOperatedByLegacyCalls, opts)
	if m.backfillOperatedByLegacyFn != nil {
		return m.backfillOperatedByLegacyFn(opts)
	}
	return &store.BackfillOperatedByLegacyStats{DryRun: opts.DryRun, Target: opts.Target}, nil
}

func (m *mockStore) BackfillSlackUserID(_ context.Context, opts store.BackfillSlackUserIDOptions) (*store.BackfillSlackUserIDStats, error) {
	m.backfillSlackUserIDCalls = append(m.backfillSlackUserIDCalls, opts)
	if m.backfillSlackUserIDFn != nil {
		return m.backfillSlackUserIDFn(opts)
	}
	return &store.BackfillSlackUserIDStats{DryRun: opts.DryRun}, nil
}

func (m *mockStore) BackfillOperatorSlugs(_ context.Context, opts store.BackfillOperatorSlugsOptions) (*store.BackfillOperatorSlugsStats, error) {
	m.backfillOperatorSlugsCalls = append(m.backfillOperatorSlugsCalls, opts)
	if m.backfillOperatorSlugsFn != nil {
		return m.backfillOperatorSlugsFn(opts)
	}
	return &store.BackfillOperatorSlugsStats{DryRun: opts.DryRun}, nil
}

// forgetOperatorCall records one ForgetOperator invocation on the mock.
type forgetOperatorCall struct {
	id   int64
	opts store.ForgetOperatorOptions
}

func (m *mockStore) ForgetOperator(_ context.Context, id int64, opts store.ForgetOperatorOptions) (*store.ForgetOperatorResult, error) {
	m.forgetOperatorCalls = append(m.forgetOperatorCalls, forgetOperatorCall{id: id, opts: opts})
	if m.forgetOperatorFn != nil {
		return m.forgetOperatorFn(id, opts)
	}
	return &store.ForgetOperatorResult{ID: id}, nil
}

func (m *mockStore) BackfillOwner(_ context.Context, opts store.BackfillOwnerOptions) (*store.BackfillOwnerStats, error) {
	m.backfillOwnerCalls = append(m.backfillOwnerCalls, opts)
	if m.backfillOwnerFn != nil {
		return m.backfillOwnerFn(opts)
	}
	return &store.BackfillOwnerStats{DryRun: opts.DryRun, Target: opts.Target}, nil
}

func (m *mockStore) BackfillEmptyOwner(_ context.Context, opts store.BackfillEmptyOwnerOptions) (*store.BackfillEmptyOwnerStats, error) {
	m.backfillEmptyOwnerCalls = append(m.backfillEmptyOwnerCalls, opts)
	if m.backfillEmptyOwnerFn != nil {
		return m.backfillEmptyOwnerFn(opts)
	}
	return &store.BackfillEmptyOwnerStats{DryRun: opts.DryRun}, nil
}

func (m *mockStore) BackfillOperatedByAgents(_ context.Context, opts store.BackfillOperatedByAgentsOptions) (*store.BackfillOperatedByAgentsStats, error) {
	m.backfillOperatedByAgentsCalls = append(m.backfillOperatedByAgentsCalls, opts)
	if m.backfillOperatedByAgentsFn != nil {
		return m.backfillOperatedByAgentsFn(opts)
	}
	return &store.BackfillOperatedByAgentsStats{DryRun: opts.DryRun}, nil
}

func (m *mockStore) BackfillParentChildDeps(_ context.Context, opts store.BackfillParentChildDepsOptions) (*store.BackfillParentChildDepsStats, error) {
	m.backfillParentChildDepsCalls = append(m.backfillParentChildDepsCalls, opts)
	if m.backfillParentChildDepsFn != nil {
		return m.backfillParentChildDepsFn(opts)
	}
	return &store.BackfillParentChildDepsStats{DryRun: opts.DryRun}, nil
}

func (m *mockStore) RepairReversedParentChildDeps(_ context.Context, opts store.RepairReversedParentChildDepsOptions) (*store.RepairReversedParentChildDepsStats, error) {
	m.repairReversedParentChildDepsCalls = append(m.repairReversedParentChildDepsCalls, opts)
	if m.repairReversedParentChildDepsFn != nil {
		return m.repairReversedParentChildDepsFn(opts)
	}
	return &store.RepairReversedParentChildDepsStats{DryRun: opts.DryRun}, nil
}

func (m *mockStore) BackfillTriggeredByDeps(_ context.Context, opts store.BackfillTriggeredByDepsOptions) (*store.BackfillTriggeredByDepsStats, error) {
	m.backfillTriggeredByDepsCalls = append(m.backfillTriggeredByDepsCalls, opts)
	if m.backfillTriggeredByDepsFn != nil {
		return m.backfillTriggeredByDepsFn(opts)
	}
	return &store.BackfillTriggeredByDepsStats{DryRun: opts.DryRun}, nil
}

func (m *mockStore) CloseTriggerAuditBeads(_ context.Context, opts store.CloseTriggerAuditBeadsOptions) (*store.CloseTriggerAuditBeadsStats, error) {
	m.closeTriggerAuditCalls = append(m.closeTriggerAuditCalls, opts)
	if m.closeTriggerAuditFn != nil {
		return m.closeTriggerAuditFn(opts)
	}
	return &store.CloseTriggerAuditBeadsStats{DryRun: opts.DryRun}, nil
}

func (m *mockStore) RecordEvent(_ context.Context, event *model.Event) error {
	event.ID = int64(len(m.events) + 1)
	m.events = append(m.events, event)
	return nil
}

func (m *mockStore) GetEvents(_ context.Context, beadID string) ([]*model.Event, error) {
	var result []*model.Event
	for _, e := range m.events {
		if e.BeadID == beadID {
			result = append(result, e)
		}
	}
	return result, nil
}

func (m *mockStore) GetRecentEvents(_ context.Context, beadID string, limit int) ([]*model.Event, error) {
	var result []*model.Event
	for _, e := range m.events {
		if e.BeadID == beadID {
			result = append(result, e)
		}
	}
	if limit > 0 && len(result) > limit {
		result = result[len(result)-limit:]
	}
	return result, nil
}

func (m *mockStore) GetEventsByTopic(_ context.Context, beadID, topic string, limit int) ([]*model.Event, error) {
	var result []*model.Event
	for _, e := range m.events {
		if e.BeadID == beadID && e.Topic == topic {
			result = append(result, e)
		}
	}
	if limit > 0 && len(result) > limit {
		result = result[len(result)-limit:]
	}
	return result, nil
}

// eventMatchesScope mirrors the store's SQL-level EventReadScope join: it
// resolves the event's bead labels from m.beads and applies RequireAllLabels
// (every label present) then RequireAnyLabels (at least one present), then
// the taint gate (TaintGrantIDs: every taint on the event's bead must be in
// the caller's grant set — mirrors eventScopeSQL's bead_taints NOT EXISTS
// join, ADR docs/adr/taints.md §4.3/§5.1). An event whose bead is unknown to
// the mock carries no labels and so fails any non-empty scope — matching the
// store, where a bead with no matching project label is excluded
// (ka-0000006863).
func (m *mockStore) eventMatchesScope(e *model.Event, scope model.EventReadScope) bool {
	if scope.IsUnrestricted() {
		return true
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	if b := m.beads[e.BeadID]; b != nil {
		for _, t := range scope.ExcludeBeadTypes {
			if string(b.Type) == t {
				return false
			}
		}
	}
	if scope.TaintGrantIDs != nil && !taintIDsSubsetOfGrants(m.gateTaintIDs(m.taints[e.BeadID]), *scope.TaintGrantIDs) {
		return false
	}
	labels := m.labels[e.BeadID]
	has := func(want string) bool {
		for _, l := range labels {
			if l == want {
				return true
			}
		}
		return false
	}
	for _, want := range scope.RequireAllLabels {
		if !has(want) {
			return false
		}
	}
	if len(scope.RequireAnyLabels) > 0 {
		any := false
		for _, want := range scope.RequireAnyLabels {
			if has(want) {
				any = true
				break
			}
		}
		if !any {
			return false
		}
	}
	return true
}

func (m *mockStore) EventHistogram(_ context.Context, since, until time.Time, bucket string, scope model.EventReadScope) ([]model.EventHistogramBucket, error) {
	if m.histogramErr != nil {
		return nil, m.histogramErr
	}
	m.lastHistogramSince = since
	m.lastHistogramUntil = until
	m.lastHistogramBucket = bucket
	if m.histogramResults != nil {
		return m.histogramResults, nil
	}
	// Default: bucket events whose CreatedAt falls in [since, until] by the
	// requested truncation. Keeps the harness self-sufficient so tests that
	// don't set histogramResults still get a deterministic answer.
	counts := make(map[time.Time]int64)
	for _, e := range m.events {
		if e.CreatedAt.Before(since) || e.CreatedAt.After(until) {
			continue
		}
		if !m.eventMatchesScope(e, scope) {
			continue
		}
		t := truncateForBucket(e.CreatedAt, bucket)
		counts[t]++
	}
	out := make([]model.EventHistogramBucket, 0, len(counts))
	for t, c := range counts {
		out = append(out, model.EventHistogramBucket{T: t, Count: c})
	}
	// Stable order by time.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].T.After(out[j].T); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out, nil
}

// truncateForBucket emulates Postgres date_trunc for the allowlisted buckets.
func truncateForBucket(t time.Time, bucket string) time.Time {
	switch bucket {
	case "minute":
		return t.UTC().Truncate(time.Minute)
	case "hour":
		return t.UTC().Truncate(time.Hour)
	case "day":
		y, m, d := t.UTC().Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	return t.UTC()
}

func (m *mockStore) LastEventAtByOperatorAliases(_ context.Context, aliases []string) (map[string]time.Time, error) {
	out := make(map[string]time.Time, len(aliases))
	for _, alias := range aliases {
		// alias format is "kind:subject"
		var kind, subject string
		for i := 0; i < len(alias); i++ {
			if alias[i] == ':' {
				kind = alias[:i]
				subject = alias[i+1:]
				break
			}
		}
		if kind == "" {
			continue
		}
		opID, ok := m.operatorAliases[kind+"\x00"+subject]
		if !ok {
			continue
		}
		var best time.Time
		var have bool
		for _, e := range m.events {
			if e.OperatedByID == nil || *e.OperatedByID != opID {
				continue
			}
			if !have || e.CreatedAt.After(best) {
				best = e.CreatedAt
				have = true
			}
		}
		if have {
			out[alias] = best
		}
	}
	return out, nil
}

func (m *mockStore) GetEventsByTimeRange(_ context.Context, since, until time.Time, cursor int64, limit int, scope model.EventReadScope) ([]*model.Event, error) {
	var result []*model.Event
	for _, e := range m.events {
		if e.CreatedAt.Before(since) || e.CreatedAt.After(until) {
			continue
		}
		if cursor > 0 && e.ID <= cursor {
			continue
		}
		if !m.eventMatchesScope(e, scope) {
			continue
		}
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].ID < result[j].ID
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (m *mockStore) GetEventsByTimeRangeDesc(_ context.Context, since, until time.Time, cursor int64, limit int, scope model.EventReadScope) ([]*model.Event, error) {
	var result []*model.Event
	for _, e := range m.events {
		if e.CreatedAt.Before(since) || e.CreatedAt.After(until) {
			continue
		}
		if cursor > 0 && e.ID >= cursor {
			continue
		}
		if !m.eventMatchesScope(e, scope) {
			continue
		}
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		return result[i].ID > result[j].ID
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (m *mockStore) SetConfig(_ context.Context, config *model.Config) error {
	m.configs[config.Key] = config
	return nil
}

func (m *mockStore) GetConfig(_ context.Context, key string) (*model.Config, error) {
	c, ok := m.configs[key]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return c, nil
}

func (m *mockStore) ListConfigs(_ context.Context, namespace string) ([]*model.Config, error) {
	prefix := namespace + ":"
	var result []*model.Config
	for k, c := range m.configs {
		if strings.HasPrefix(k, prefix) {
			result = append(result, c)
		}
	}
	return result, nil
}

func (m *mockStore) ListAllConfigs(_ context.Context) ([]*model.Config, error) {
	var result []*model.Config
	for _, c := range m.configs {
		result = append(result, c)
	}
	return result, nil
}

func (m *mockStore) DeleteConfig(_ context.Context, key string) error {
	if _, ok := m.configs[key]; !ok {
		return sql.ErrNoRows
	}
	delete(m.configs, key)
	return nil
}

func (m *mockStore) RunInTransaction(_ context.Context, fn func(tx store.Store) error) error {
	return fn(m)
}

func (m *mockStore) Ping(_ context.Context) error {
	return m.pingErr
}

func (m *mockStore) Close() error {
	return nil
}

func (m *mockStore) InsertRevokedToken(_ context.Context, rt *model.RevokedToken) error {
	if m.revokedTokens == nil {
		m.revokedTokens = make(map[string]*model.RevokedToken)
	}
	if _, ok := m.revokedTokens[rt.TokenHash]; ok {
		return nil
	}
	cp := *rt
	if cp.RevokedAt.IsZero() {
		cp.RevokedAt = time.Now()
	}
	m.revokedTokens[rt.TokenHash] = &cp
	return nil
}

func (m *mockStore) ListRevokedTokenHashes(_ context.Context) ([]string, error) {
	hashes := make([]string, 0, len(m.revokedTokens))
	for h := range m.revokedTokens {
		hashes = append(hashes, h)
	}
	return hashes, nil
}

func (m *mockStore) InsertRefreshToken(_ context.Context, rt *model.RefreshToken) error {
	if m.refreshTokens == nil {
		m.refreshTokens = make(map[string]*model.RefreshToken)
	}
	cp := *rt
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	if len(cp.ScopesJSON) == 0 {
		cp.ScopesJSON = []byte("{}")
	}
	m.refreshTokens[cp.TokenHash] = &cp
	return nil
}

func (m *mockStore) GetRefreshToken(_ context.Context, hash string) (*model.RefreshToken, error) {
	rt, ok := m.refreshTokens[hash]
	if !ok {
		return nil, sql.ErrNoRows
	}
	cp := *rt
	return &cp, nil
}

func (m *mockStore) InsertRunnerToken(_ context.Context, rt *model.RunnerToken) error {
	m.runnerTokensMu.Lock()
	defer m.runnerTokensMu.Unlock()
	if m.runnerTokens == nil {
		m.runnerTokens = make(map[string]*model.RunnerToken)
	}
	cp := *rt
	m.runnerTokens[cp.TokenHash] = &cp
	return nil
}

func (m *mockStore) GetRunnerTokenByHash(_ context.Context, hash string) (*model.RunnerToken, error) {
	m.runnerTokensMu.Lock()
	defer m.runnerTokensMu.Unlock()
	rt, ok := m.runnerTokens[hash]
	if !ok {
		return nil, nil
	}
	cp := *rt
	return &cp, nil
}

func (m *mockStore) RevokeRunnerTokens(_ context.Context, runnerID string, at time.Time) (int, error) {
	m.runnerTokensMu.Lock()
	defer m.runnerTokensMu.Unlock()
	n := 0
	for _, rt := range m.runnerTokens {
		if rt.RunnerID == runnerID && rt.RevokedAt == nil {
			t := at
			rt.RevokedAt = &t
			n++
		}
	}
	return n, nil
}

func (m *mockStore) ListActiveRefreshTokenHashesByFleet(_ context.Context, fleet string) ([]string, error) {
	var hashes []string
	for h, rt := range m.refreshTokens {
		if rt.Fleet == fleet && rt.RevokedAt == nil {
			hashes = append(hashes, h)
		}
	}
	return hashes, nil
}

func (m *mockStore) RevokeRefreshTokenChain(_ context.Context, hash string, revokedAt time.Time) (int, error) {
	if revokedAt.IsZero() {
		revokedAt = time.Now()
	}
	revoked := 0
	// Walk the parent_refresh_hash chain in BFS order.
	frontier := []string{hash}
	seen := map[string]bool{hash: true}
	for len(frontier) > 0 {
		cur := frontier[0]
		frontier = frontier[1:]
		rt, ok := m.refreshTokens[cur]
		if !ok {
			continue
		}
		if rt.RevokedAt == nil && cur == hash {
			t := revokedAt
			rt.RevokedAt = &t
			revoked++
		} else if rt.RevokedAt == nil {
			t := revokedAt
			rt.RevokedAt = &t
			revoked++
		}
		// Find children whose parent_refresh_hash == cur.
		for h, child := range m.refreshTokens {
			if !seen[h] && child.ParentRefreshHash == cur {
				seen[h] = true
				frontier = append(frontier, h)
			}
		}
	}
	return revoked, nil
}

func (m *mockStore) RecordMutation(_ context.Context, mut *model.MutationRow) error {
	cp := *mut
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	cp.ID = int64(len(m.mutations) + 1)
	m.mutations = append(m.mutations, &cp)
	return nil
}

func (m *mockStore) ListMutations(_ context.Context, beadID string) ([]*model.MutationRow, error) {
	out := make([]*model.MutationRow, 0)
	for _, mut := range m.mutations {
		if mut.BeadID == beadID {
			cp := *mut
			out = append(out, &cp)
		}
	}
	return out, nil
}

// GetOperatorHistory satisfies the Store interface by projecting the
// in-memory mutations slice through the same shape as the
// bead_operator_history VIEW (mutation_history rows where
// field='operated_by'). Returned newest first, matching the postgres impl.
func (m *mockStore) GetOperatorHistory(_ context.Context, beadID string) ([]*model.OperatorHistoryRow, error) {
	out := make([]*model.OperatorHistoryRow, 0)
	for _, mut := range m.mutations {
		if mut.BeadID != beadID || mut.Field != "operated_by" {
			continue
		}
		kind := "dibs-flip"
		switch {
		case mut.MutationType == model.MutationTypeCreate:
			kind = "create"
		case mut.NewValue == mut.OldValue:
			kind = "refresh"
		case strings.HasPrefix(mut.NewValue, "system:") || strings.HasPrefix(mut.NewValue, "fleet:"):
			kind = "sentinel-attempt"
		case mut.NewValue == "":
			kind = "unknown-attempt"
		}
		out = append(out, &model.OperatorHistoryRow{
			ID:            mut.ID,
			BeadID:        mut.BeadID,
			OperatorID:    mut.NewValue,
			BecameOwnerAt: mut.CreatedAt,
			PrevOperator:  mut.OldValue,
			Actor:         mut.Actor,
			SourceKind:    kind,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BecameOwnerAt.Equal(out[j].BecameOwnerAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].BecameOwnerAt.After(out[j].BecameOwnerAt)
	})
	return out, nil
}

func (m *mockStore) GetMutation(_ context.Context, id int64) (*model.MutationRow, error) {
	for _, mut := range m.mutations {
		if mut.ID == id {
			cp := *mut
			return &cp, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (m *mockStore) GetBeadOperator(_ context.Context, beadID string) (string, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	for _, b := range m.beads {
		if b.ID == beadID {
			return b.OperatedBy, nil
		}
	}
	return "", nil
}

func (m *mockStore) UpdateBeadOperator(_ context.Context, beadID string, operatorID int64) error {
	op, ok := m.operators[operatorID]
	if !ok {
		return nil
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	canonical := op.Kind + ":" + op.Subject
	for _, b := range m.beads {
		if b.ID == beadID {
			b.OperatedBy = canonical
			return nil
		}
	}
	return nil
}

// ClaimEscalationReplay mirrors the real guarded-UPDATE semantics: it claims
// only when replay_in_progress is not already "true" on the bead, atomically
// under beadsMu (this mock's stand-in for the single-statement UPDATE's
// atomicity in Postgres).
func (m *mockStore) ClaimEscalationReplay(_ context.Context, beadID string) (bool, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[beadID]
	if !ok {
		return false, nil
	}
	fields := mockFieldMap(b)
	if raw, ok := fields["replay_in_progress"]; ok {
		var v bool
		if err := json.Unmarshal(raw, &v); err == nil && v {
			return false, nil
		}
	}
	encoded, _ := json.Marshal(true)
	fields["replay_in_progress"] = encoded
	mockWriteFields(b, fields)
	return true, nil
}

func (m *mockStore) ReleaseEscalationReplayClaim(_ context.Context, beadID string) error {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[beadID]
	if !ok {
		return nil
	}
	fields := mockFieldMap(b)
	encoded, _ := json.Marshal(false)
	fields["replay_in_progress"] = encoded
	mockWriteFields(b, fields)
	return nil
}

// ClaimEscalationExecution mirrors the real guarded UPDATE: it records
// dispatch only when execution_dispatch is unset or empty, under beadsMu.
func (m *mockStore) ClaimEscalationExecution(_ context.Context, beadID, dispatch string) (bool, string, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[beadID]
	if !ok {
		return false, "", nil
	}
	fields := mockFieldMap(b)
	if raw, ok := fields["execution_dispatch"]; ok {
		var v string
		if err := json.Unmarshal(raw, &v); err == nil && v != "" {
			return false, v, nil
		}
	}
	encoded, _ := json.Marshal(dispatch)
	fields["execution_dispatch"] = encoded
	mockWriteFields(b, fields)
	return true, dispatch, nil
}

// ClaimScrubExecution mirrors the real guarded-UPDATE semantics: it claims
// only when scrub_execution_claimed is not already "true" on the bead,
// atomically under beadsMu (this mock's stand-in for the single-statement
// UPDATE's atomicity in Postgres). See ClaimEscalationReplay above, which
// this mirrors exactly.
func (m *mockStore) ClaimScrubExecution(_ context.Context, beadID string) (bool, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[beadID]
	if !ok {
		return false, nil
	}
	fields := mockFieldMap(b)
	if raw, ok := fields["scrub_execution_claimed"]; ok {
		var v bool
		if err := json.Unmarshal(raw, &v); err == nil && v {
			return false, nil
		}
	}
	encoded, _ := json.Marshal(true)
	fields["scrub_execution_claimed"] = encoded
	mockWriteFields(b, fields)
	return true, nil
}

func (m *mockStore) ReleaseScrubExecutionClaim(_ context.Context, beadID string) error {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[beadID]
	if !ok {
		return nil
	}
	fields := mockFieldMap(b)
	encoded, _ := json.Marshal(false)
	fields["scrub_execution_claimed"] = encoded
	mockWriteFields(b, fields)
	return nil
}

// CASBeadStatus mirrors the real guarded-UPDATE semantics (status/updated_at
// only, never fields) atomically under beadsMu — this mock's stand-in for
// the single-statement UPDATE's atomicity in Postgres. Deliberately does NOT
// touch b.Fields, so a test racing this against MergeBeadFields/
// ClaimScrubExecution on the same bead sees the same no-clobber guarantee
// the real store gives (gd-P6uCMzQqU9).
func (m *mockStore) CASBeadStatus(_ context.Context, id string, from, to model.Status) (bool, error) {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	if !ok || b.Status != from {
		return false, nil
	}
	b.Status = to
	b.UpdatedAt = time.Now().UTC()
	return true, nil
}

// TryAdvisoryLock's mock counterpart uses an in-process map as a
// non-blocking mutex per key, matching pg_try_advisory_xact_lock's
// try-and-fail-fast (never block) contract closely enough for the
// decision-aging / audit-retention sweep tests, which run single-threaded
// against one mockStore and don't exercise real cross-replica contention.
func (m *mockStore) TryAdvisoryLock(ctx context.Context, key int64, fn func(ctx context.Context) error) (bool, error) {
	m.advisoryLocksMu.Lock()
	if m.advisoryLocks == nil {
		m.advisoryLocks = map[int64]bool{}
	}
	if m.advisoryLocks[key] {
		m.advisoryLocksMu.Unlock()
		return false, nil
	}
	m.advisoryLocks[key] = true
	m.advisoryLocksMu.Unlock()
	defer func() {
		m.advisoryLocksMu.Lock()
		delete(m.advisoryLocks, key)
		m.advisoryLocksMu.Unlock()
	}()

	if err := fn(ctx); err != nil {
		return true, err
	}
	return true, nil
}

func (m *mockStore) UpsertOperator(_ context.Context, kind, subject string) (int64, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	pair := kind + "\x00" + subject
	if id, ok := m.operatorByPair[pair]; ok {
		return id, nil
	}
	if m.operators == nil {
		m.operators = map[int64]struct{ Kind, Subject string }{}
		m.operatorByPair = map[string]int64{}
	}
	m.nextOperatorID++
	id := m.nextOperatorID
	m.operators[id] = struct{ Kind, Subject string }{kind, subject}
	m.operatorByPair[pair] = id
	return id, nil
}

func (m *mockStore) LookupOperatorAlias(_ context.Context, kind, subject string) (int64, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.operatorAliases == nil {
		return 0, sql.ErrNoRows
	}
	id, ok := m.operatorAliases[kind+"\x00"+subject]
	if !ok {
		return 0, sql.ErrNoRows
	}
	return id, nil
}

func (m *mockStore) LookupOperatorByAlias(_ context.Context, kind, subject string) (int64, string, error) {
	if m.operatorAliases == nil {
		return 0, "", sql.ErrNoRows
	}
	id, ok := m.operatorAliases[kind+"\x00"+subject]
	if !ok {
		return 0, "", sql.ErrNoRows
	}
	if op, ok := m.operators[id]; ok {
		return id, op.Kind, nil
	}
	return id, "", nil
}

func (m *mockStore) UpsertOperatorAlias(_ context.Context, operatorID int64, kind, subject, source string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.operatorAliases == nil {
		m.operatorAliases = map[string]int64{}
	}
	if m.operatorAliasSources == nil {
		m.operatorAliasSources = map[string]string{}
	}
	key := kind + "\x00" + subject
	if _, exists := m.operatorAliases[key]; !exists {
		// Mirrors the postgres upsert: ON CONFLICT updates operator_id only,
		// so the source recorded at first insert sticks.
		m.operatorAliasSources[key] = source
	}
	m.operatorAliases[key] = operatorID
	return nil
}

func (m *mockStore) PruneOperatorAliases(_ context.Context, source string, keep []store.OperatorAliasKey) ([]store.OperatorAliasKey, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[k.Kind+"\x00"+k.Subject] = true
	}
	var deleted []store.OperatorAliasKey
	for key, src := range m.operatorAliasSources {
		if src != source || keepSet[key] {
			continue
		}
		k, subj, _ := splitOperatorAliasKey(key)
		deleted = append(deleted, store.OperatorAliasKey{Kind: k, Subject: subj})
		delete(m.operatorAliases, key)
		delete(m.operatorAliasSources, key)
	}
	return deleted, nil
}

func (m *mockStore) ListOperatorAliasesByKind(_ context.Context, kind string) ([]store.OperatorAliasIdentity, error) {
	var out []store.OperatorAliasIdentity
	for key, id := range m.operatorAliases {
		k, subj, ok := splitOperatorAliasKey(key)
		if !ok || k != kind {
			continue
		}
		display, email := m.operatorIdentities[id].Display, m.operatorIdentities[id].Email
		out = append(out, store.OperatorAliasIdentity{
			OperatorID: id,
			Subject:    subj,
			Display:    display,
			Email:      email,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OperatorID < out[j].OperatorID })
	return out, nil
}

func (m *mockStore) UpdateOperatorIdentity(_ context.Context, operatorID int64, display, email string) (bool, error) {
	if _, ok := m.operators[operatorID]; !ok {
		return false, nil
	}
	if m.operatorIdentities == nil {
		m.operatorIdentities = map[int64]struct{ Display, Email string }{}
	}
	m.operatorIdentities[operatorID] = struct{ Display, Email string }{display, email}
	return true, nil
}

func (m *mockStore) MergeOperators(_ context.Context, oldID, newID int64) (int64, error) {
	if oldID == newID || oldID <= 0 || newID <= 0 {
		return 0, nil
	}
	moved := m.mergedOperatorRows
	if m.operatorMerges == nil {
		m.operatorMerges = map[int64]int64{}
	}
	m.operatorMerges[oldID] = newID
	// Absorb the loser's slug (ka-0000004514): the postgres merge NULLs the
	// loser's operators.slug so it drops out of the /people directory feed
	// (ListOperatorsWithSlugs filters slug IS NOT NULL) while repointing its
	// operator_slug_aliases rows so /u/{loser-slug} still resolves. The mock
	// conflates operators.slug and the slug-alias table into one operatorSlugs
	// map, so it models the directory-visible effect that this task fixes: the
	// loser's slug leaves the feed. Per-slug /u retirement durability is
	// asserted at the postgres layer (TestCovOpsTxMergeOperators).
	for slug, id := range m.operatorSlugs {
		if id == oldID {
			delete(m.operatorSlugs, slug)
		}
	}
	return moved, nil
}

func (m *mockStore) GetOperatorBySlug(_ context.Context, slug string) (int64, error) {
	id, ok := m.operatorSlugs[slug]
	if !ok {
		return 0, sql.ErrNoRows
	}
	return id, nil
}

func (m *mockStore) GetOperatorSlug(_ context.Context, operatorID int64) (string, error) {
	for slug, id := range m.operatorSlugs {
		if id == operatorID {
			return slug, nil
		}
	}
	return "", sql.ErrNoRows
}

func (m *mockStore) ListOperatorsWithSlugs(_ context.Context, limit int) ([]store.OperatorDirectoryEntry, error) {
	m.operatorLimits = append(m.operatorLimits, limit)
	if limit <= 0 {
		limit = 500
	}
	out := make([]store.OperatorDirectoryEntry, 0, len(m.operatorSlugs))
	for slug, id := range m.operatorSlugs {
		op, ok := m.operators[id]
		if !ok {
			continue
		}
		ident := m.operatorIdentities[id]
		out = append(out, store.OperatorDirectoryEntry{
			ID:      id,
			Kind:    op.Kind,
			Subject: op.Subject,
			Display: ident.Display,
			Email:   ident.Email,
			Slug:    slug,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *mockStore) OperatorDirectoryStats(_ context.Context, recentPerOperator, recentCeiling, recentWindowHours int) (*store.OperatorDirectoryStats, error) {
	m.operatorDirStatsArgs = [3]int{recentPerOperator, recentCeiling, recentWindowHours}
	if m.operatorDirStatsErr != nil {
		return nil, m.operatorDirStatsErr
	}
	if m.operatorDirStats != nil {
		return m.operatorDirStats, nil
	}
	return &store.OperatorDirectoryStats{}, nil
}

func (m *mockStore) ResolveOperators(_ context.Context, ids []int64) (map[int64]store.ResolvedOperator, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	out := make(map[int64]store.ResolvedOperator, len(ids))
	for _, id := range ids {
		if _, ok := m.operators[id]; !ok {
			continue // unknown id absent from result — caller applies fallback
		}
		ident := m.operatorIdentities[id]
		r := store.ResolvedOperator{ID: id, Display: ident.Display, Email: ident.Email}
		for slug, sid := range m.operatorSlugs {
			if sid == id {
				r.Slug = slug
				break
			}
		}
		if m.operatorForgotten[id] {
			r.Forgotten = true
			r.Display = ""
			r.Email = store.ForgottenOperatorEmail
		}
		out[id] = r
	}
	return out, nil
}

func splitOperatorAliasKey(key string) (string, string, bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}

func (m *mockStore) RecordUITelemetry(_ context.Context, row *model.UITelemetryRow) error {
	if m.uiTelemetry == nil {
		m.uiTelemetry = make([]*model.UITelemetryRow, 0)
	}
	row.ID = int64(len(m.uiTelemetry) + 1)
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	m.uiTelemetry = append(m.uiTelemetry, row)
	return nil
}

func (m *mockStore) ListUITelemetry(_ context.Context, filter model.UITelemetryFilter) ([]*model.UITelemetryRow, error) {
	out := make([]*model.UITelemetryRow, 0, len(m.uiTelemetry))
	for i := len(m.uiTelemetry) - 1; i >= 0; i-- {
		r := m.uiTelemetry[i]
		if filter.Event != "" && r.Event != filter.Event {
			continue
		}
		if !filter.Since.IsZero() && r.CreatedAt.Before(filter.Since) {
			continue
		}
		out = append(out, r)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

func (m *mockStore) InsertSlowToolCall(_ context.Context, row *model.SlowToolCallRow) error {
	row.ID = int64(len(m.slowToolCalls) + 1)
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	m.slowToolCalls = append(m.slowToolCalls, row)
	return nil
}

func (m *mockStore) DeleteSlowToolCallsBefore(_ context.Context, cutoff time.Time, limit int) (int64, error) {
	kept := m.slowToolCalls[:0:0]
	var deleted int64
	for _, r := range m.slowToolCalls {
		if r.TS.Before(cutoff) && (limit <= 0 || deleted < int64(limit)) {
			deleted++
			continue
		}
		kept = append(kept, r)
	}
	m.slowToolCalls = kept
	return deleted, nil
}

// mockBeadTypeIn reports whether bead id exists with a type in types.
func (m *mockStore) mockBeadTypeIn(id string, types []string) bool {
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	b, ok := m.beads[id]
	return ok && slices.Contains(types, string(b.Type))
}

// DeleteBeadUpdatedEventsBefore mirrors the postgres query. An event with a
// zero CreatedAt (RecordEvent leaves it unset) is never expired.
func (m *mockStore) DeleteBeadUpdatedEventsBefore(_ context.Context, types []string, cutoff time.Time, limit int) (int64, error) {
	kept := m.events[:0:0]
	var deleted int64
	for _, e := range m.events {
		if e.Topic == events.TopicBeadUpdated && !e.CreatedAt.IsZero() && e.CreatedAt.Before(cutoff) &&
			m.mockBeadTypeIn(e.BeadID, types) && (limit <= 0 || deleted < int64(limit)) {
			deleted++
			continue
		}
		kept = append(kept, e)
	}
	m.events = kept
	return deleted, nil
}

func (m *mockStore) DeleteFieldsMutationsBefore(_ context.Context, types []string, cutoff time.Time, limit int) (int64, error) {
	kept := m.mutations[:0:0]
	var deleted int64
	for _, mut := range m.mutations {
		if mut.Field == "fields" && mut.CreatedAt.Before(cutoff) &&
			m.mockBeadTypeIn(mut.BeadID, types) && (limit <= 0 || deleted < int64(limit)) {
			deleted++
			continue
		}
		kept = append(kept, mut)
	}
	m.mutations = kept
	return deleted, nil
}

func (m *mockStore) UpsertPushDevice(_ context.Context, d *model.PushDevice) error {
	if m.pushDevices == nil {
		m.pushDevices = map[string]*model.PushDevice{}
	}
	now := time.Now().UTC()
	d.CreatedAt, d.LastSeenAt = now, now
	if prev, ok := m.pushDevices[d.Token]; ok {
		d.CreatedAt = prev.CreatedAt
	}
	cp := *d
	m.pushDevices[d.Token] = &cp
	return nil
}

func (m *mockStore) ListPushDevices(_ context.Context, filter model.PushDeviceFilter) ([]*model.PushDevice, error) {
	want := map[string]bool{}
	for _, e := range filter.UserEmails {
		want[e] = true
	}
	out := []*model.PushDevice{}
	for _, d := range m.pushDevices {
		if len(want) > 0 && !want[d.UserEmail] {
			continue
		}
		if filter.Platform != "" && d.Platform != filter.Platform {
			continue
		}
		cp := *d
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UserEmail != out[j].UserEmail {
			return out[i].UserEmail < out[j].UserEmail
		}
		return out[i].Token < out[j].Token
	})
	return out, nil
}

func (m *mockStore) DeletePushDevice(_ context.Context, token, userEmail string) (bool, error) {
	d, ok := m.pushDevices[token]
	if !ok || (userEmail != "" && d.UserEmail != userEmail) {
		return false, nil
	}
	delete(m.pushDevices, token)
	return true, nil
}

func (m *mockStore) ListSlowToolCalls(_ context.Context, filter model.SlowToolCallFilter) ([]*model.SlowToolCallRow, error) {
	out := make([]*model.SlowToolCallRow, 0, len(m.slowToolCalls))
	for _, r := range m.slowToolCalls {
		if !filter.Since.IsZero() && r.TS.Before(filter.Since) {
			continue
		}
		if !filter.Until.IsZero() && r.TS.After(filter.Until) {
			continue
		}
		if filter.CmdClass != "" && r.CmdClass != filter.CmdClass {
			continue
		}
		if filter.AgentBeadID != "" && r.AgentBeadID != filter.AgentBeadID {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DurationMs > out[j].DurationMs })
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// Bead followers (migration 023, ka-0000004114). Mock keeps an in-memory
// map keyed by (beadID, operatorID) so tests can exercise the join-table
// shape without standing up Postgres.
type mockFollowerKey struct {
	beadID     string
	operatorID int64
}

func (m *mockStore) AddFollower(_ context.Context, beadID string, operatorID int64, forwardedBy *int64, note string) error {
	if m.followers == nil {
		m.followers = map[mockFollowerKey]*model.BeadFollower{}
	}
	row := &model.BeadFollower{
		BeadID:      beadID,
		OperatorID:  operatorID,
		ForwardedBy: forwardedBy,
		Note:        note,
		CreatedAt:   time.Now().UTC(),
	}
	m.followers[mockFollowerKey{beadID, operatorID}] = row
	return nil
}

func (m *mockStore) RemoveFollower(_ context.Context, beadID string, operatorID int64) (bool, error) {
	if m.followers == nil {
		return false, nil
	}
	key := mockFollowerKey{beadID, operatorID}
	if _, ok := m.followers[key]; !ok {
		return false, nil
	}
	delete(m.followers, key)
	return true, nil
}

func (m *mockStore) ListFollowers(_ context.Context, beadID string) ([]*model.BeadFollower, error) {
	var out []*model.BeadFollower
	for k, v := range m.followers {
		if k.beadID == beadID {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].OperatorID < out[j].OperatorID
	})
	return out, nil
}

func (m *mockStore) ListFollowedBeadIDs(_ context.Context, operatorID int64, limit int) ([]string, error) {
	type pair struct {
		id string
		t  time.Time
	}
	var rows []pair
	for k, v := range m.followers {
		if k.operatorID == operatorID {
			rows = append(rows, pair{k.beadID, v.CreatedAt})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].t.Equal(rows[j].t) {
			return rows[i].t.After(rows[j].t)
		}
		return rows[i].id < rows[j].id
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.id
	}
	return out, nil
}

func (m *mockStore) UpsertGate(_ context.Context, _, _ string) error { return nil }

func (m *mockStore) MarkGateSatisfied(_ context.Context, _, _ string) error { return nil }

func (m *mockStore) ClearGate(_ context.Context, _, _ string) error { return nil }

func (m *mockStore) IsGateSatisfied(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func (m *mockStore) ListGates(_ context.Context, _ string) ([]model.GateRow, error) {
	return nil, nil
}

func (m *mockStore) UpsertEmbedding(_ context.Context, beadID string, embedding []float32, modelName, textHash string) error {
	if m.embeddings == nil {
		m.embeddings = make(map[string]mockEmbedding)
	}
	m.embeddings[beadID] = mockEmbedding{vec: embedding, model: modelName, textHash: textHash}
	return nil
}

func (m *mockStore) GetEmbeddingHashes(_ context.Context, beadIDs []string) (map[string]string, error) {
	out := make(map[string]string)
	for _, id := range beadIDs {
		if e, ok := m.embeddings[id]; ok {
			out[id] = e.textHash
		}
	}
	return out, nil
}

// ListBeadsNeedingEmbedding returns the subset of mock beads that have no
// embedding row yet, paginated by limit/offset. Mirrors the postgres query
// just enough for handler tests to drive the --only-missing path; stale
// (updated_at-based) detection is not modeled in the mock.
func (m *mockStore) ListBeadsNeedingEmbedding(_ context.Context, fleet string, limit, offset int) ([]*model.Bead, int, error) {
	if limit <= 0 {
		limit = 50
	}
	m.beadsMu.Lock()
	defer m.beadsMu.Unlock()
	var pending []*model.Bead
	for _, b := range m.beads {
		if b == nil {
			continue
		}
		if fleet != "" && fleet != model.FleetAll && b.Fleet != fleet {
			continue
		}
		if _, ok := m.embeddings[b.ID]; ok {
			// Already-embedded beads are normally excluded — but staleEmbeddingIDs
			// simulates the SQL filter's b.updated_at > e.updated_at branch where
			// a bead is flagged as stale even though its embedding exists.
			if !m.staleEmbeddingIDs[b.ID] {
				continue
			}
		}
		pending = append(pending, b)
	}
	total := len(pending)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return pending[offset:end], total, nil
}

func (m *mockStore) RefreshEmbeddingTimestamps(_ context.Context, beadIDs []string) (int64, error) {
	// Record the call so tests can assert which IDs were refreshed.
	idsCopy := append([]string(nil), beadIDs...)
	m.refreshedEmbeddingCalls = append(m.refreshedEmbeddingCalls, idsCopy)
	var n int64
	for _, id := range beadIDs {
		if _, ok := m.embeddings[id]; ok {
			n++
			// Refreshing clears the staleness flag — mirrors the postgres
			// behavior where bead_embeddings.updated_at = NOW() makes
			// b.updated_at > e.updated_at false for the bumped row.
			delete(m.staleEmbeddingIDs, id)
		}
	}
	return n, nil
}

func (m *mockStore) GetEmbedding(_ context.Context, beadID string) ([]float32, string, error) {
	if m.embeddingErr != nil {
		return nil, "", m.embeddingErr
	}
	e, ok := m.embeddings[beadID]
	if !ok {
		return nil, "", sql.ErrNoRows
	}
	return e.vec, e.model, nil
}

func (m *mockStore) SemanticSearch(_ context.Context, queryVec []float32, filter model.BeadFilter, limit int) ([]*model.SemanticResult, error) {
	m.lastSemanticVec = queryVec
	m.lastSemanticFilter = filter
	m.lastSemanticLimit = limit
	if m.semanticErr != nil {
		return nil, m.semanticErr
	}
	if m.semanticResults != nil {
		results := m.semanticResults
		// TaintGrantIDs (ADR docs/adr/taints.md §4.3/§5.1): mirrors
		// querySemanticSearch's post-index NOT EXISTS(bead_taints ...)
		// predicate — the vector index itself cannot express the join, so
		// this filters the same "raw hit" set ListBeads's mock filters
		// above, letting a test prove a tainted bead's embedding is a real
		// index hit (query with a nil/exempt filter) that is nonetheless
		// absent from what an ungranted caller is served.
		if filter.TaintGrantIDs != nil {
			grants := *filter.TaintGrantIDs
			granted := make(map[int64]bool, len(grants))
			for _, g := range grants {
				granted[g] = true
			}
			filtered := make([]*model.SemanticResult, 0, len(results))
			for _, res := range results {
				excluded := false
				for _, tid := range m.gateTaintIDs(m.taints[res.Bead.ID]) {
					if !granted[tid] {
						excluded = true
						break
					}
				}
				if !excluded {
					filtered = append(filtered, res)
				}
			}
			results = filtered
		}
		if limit > 0 && len(results) > limit {
			results = results[:limit]
		}
		return results, nil
	}
	return nil, nil
}

func (m *mockStore) DeleteEmbedding(_ context.Context, _ string) error {
	return nil
}

func (m *mockStore) GetEmbeddingStats(_ context.Context) (*model.EmbeddingStats, error) {
	if m.embeddingStatsErr != nil {
		return nil, m.embeddingStatsErr
	}
	if m.embeddingStats != nil {
		return m.embeddingStats, nil
	}
	return &model.EmbeddingStats{}, nil
}

func (m *mockStore) MigrateEmbeddingsSchema(_ context.Context, modelName string, dims int, actor, note string) error {
	if m.migrateErr != nil {
		return m.migrateErr
	}
	m.embeddings = nil
	m.lastMigrate = &mockMigrateCall{Model: modelName, Dims: dims, Actor: actor, Note: note}
	m.indexDropped = true
	return nil
}

func (m *mockStore) RecreateEmbeddingIndex(_ context.Context) error {
	if m.reindexErr != nil {
		return m.reindexErr
	}
	m.indexDropped = false
	m.indexRecreated = true
	return nil
}

// newTestServer returns a fresh server, its mock store, and an HTTP handler.
// The handler is wired with a test identity verifier (bearerSubjectVerifier)
// AND wrapped with middleware that injects a default Authorization header
// when none is set, so the post-R2 fail-closed operator requirement
// (ka-0000005772) does not break the hundreds of pre-existing HTTP tests
// that never bothered with auth. Tests that need to exercise the anonymous
// path explicitly should call into srv.<Method>(ctx, ...) directly with
// context.Background() — that bypasses this wrapper.
func newTestServer() (*BeadsServer, *mockStore, http.Handler) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	// Disable the user provisioner so the default-bearer middleware doesn't
	// auto-create a user bead on every test request — that would pollute
	// the bead/graph/list assertions that pre-date R2.
	s.userProvisioner = nil
	s.SetForwardAuthClients([]string{testBrowserClient})
	inner := s.NewHTTPHandler(&bearerSubjectVerifier{})
	return s, ms, defaultBearerMiddleware(inner)
}

// newTestServerNoAuth returns the same wiring as newTestServer but WITHOUT
// the default-bearer middleware, so requests with no Authorization header
// arrive at the handler as truly anonymous. Used by the small number of
// tests that exercise the anonymous / forged-body-ignored path.
func newTestServerNoAuth() (*BeadsServer, *mockStore, http.Handler) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	return s, ms, s.NewHTTPHandler(nil)
}

// defaultBearerMiddleware injects "Authorization: Bearer test@example.com"
// when the request has neither an Authorization header nor an
// X-Auth-Request-Access-Token header set. Tests that exercise fleet tokens,
// X-Operated-By overrides, or anonymous behavior set their own headers and
// are unaffected.
const defaultTestSubject = "test@example.com"

func defaultBearerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && r.Header.Get("X-Auth-Request-Access-Token") == "" {
			r.Header.Set("Authorization", "Bearer "+defaultTestSubject)
		}
		next.ServeHTTP(w, r)
	})
}

// doJSON performs an HTTP request with an optional JSON body and returns the recorder.
func doJSON(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// newTestServerAuth returns a fresh server, its mock store, and an HTTP
// handler wired with a test-only identity verifier so that tests can
// authenticate as an arbitrary subject using `Authorization: Bearer <subject>`
// (via doJSONAs). This is the path the server uses to derive actor from
// context under AUDIT-2026-04-18 §C3.
func newTestServerAuth() (*BeadsServer, *mockStore, http.Handler) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.SetForwardAuthClients([]string{testBrowserClient})
	return s, ms, s.NewHTTPHandler(&bearerSubjectVerifier{})
}

// doJSONAs is like doJSON but adds an `Authorization: Bearer <subject>`
// header; paired with newTestServerAuth (which wires a bearerSubjectVerifier),
// this causes the server to treat `subject` as the authenticated OIDC
// identity (actor) and ignore any client-supplied actor fields in the body
// (AUDIT §C3).
func doJSONAs(t *testing.T, handler http.Handler, method, path string, body any, subject string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+subject)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// bearerSubjectVerifier is a test-only IdentityVerifier + RoleExtractor that
// accepts any non-empty bearer token and treats it verbatim as the
// authenticated subject (email). It grants admin role so RBAC checks pass.
type bearerSubjectVerifier struct{}

func (bearerSubjectVerifier) Verify(_ context.Context, _ string) error { return nil }

func (bearerSubjectVerifier) VerifyAndIdentify(_ context.Context, raw string) (string, error) {
	return raw, nil
}

func (bearerSubjectVerifier) ExtractRoles(_ context.Context, _ string) ([]string, error) {
	return []string{RoleAdmin}, nil
}

// ExtractClientID reports the browser SSO client, so these callers pass the
// escalation-approve azp allowlist (see testBrowserClient).
func (bearerSubjectVerifier) ExtractClientID(_ context.Context, _ string) (string, error) {
	return testBrowserClient, nil
}

// requireStatus asserts the recorder has the expected HTTP status code.
func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("expected status %d, got %d; body: %s", code, rec.Code, rec.Body.String())
	}
}

// decodeJSON decodes the recorder's response body into v.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
}

func TestHandleHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		method    string
		path      string
		body      any
		code      int
		wantError string
	}{
		{"CreateBead/MissingTitle", "POST", "/v1/beads", map[string]any{"type": "task"}, 400, "title is required"},
		{"GetBead/NotFound", "GET", "/v1/beads/nonexistent", nil, 404, "bead not found"},
		{"DeleteBead/NotFound", "DELETE", "/v1/beads/nonexistent", nil, 404, ""},
		{"GetConfig/NotFound", "GET", "/v1/configs/view:nonexistent", nil, 404, ""},
		{"DeleteConfig/NotFound", "DELETE", "/v1/configs/view:nonexistent", nil, 404, ""},
		{"AddComment/MissingText", "POST", "/v1/beads/gd-x/comments", map[string]any{"author": "alice"}, 400, ""},
		{"AddDependency/MissingDependsOnID", "POST", "/v1/beads/gd-a/dependencies", map[string]any{"type": "blocks"}, 400, ""},
		{"AddLabel/MissingLabel", "POST", "/v1/beads/gd-a/labels", map[string]any{}, 400, ""},
		{"RemoveDependency/MissingDependsOnID", "DELETE", "/v1/beads/gd-a/dependencies?type=blocks", nil, 400, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, h := newTestServer()
			rec := doJSON(t, h, tc.method, tc.path, tc.body)
			requireStatus(t, rec, tc.code)
			if tc.wantError != "" {
				var body map[string]string
				decodeJSON(t, rec, &body)
				if body["error"] != tc.wantError {
					t.Fatalf("expected error=%q, got %q", tc.wantError, body["error"])
				}
			}
		})
	}
}

func TestHandleHealth(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/health", nil)
	requireStatus(t, rec, 200)
	var body map[string]string
	decodeJSON(t, rec, &body)
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok, got %q", body["status"])
	}
}

func TestHandleHealth_DBDown(t *testing.T) {
	ms := newMockStore()
	ms.pingErr = errors.New("connection refused")
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler(nil)

	rec := doJSON(t, h, "GET", "/v1/health", nil)
	requireStatus(t, rec, 503)
	var body map[string]string
	decodeJSON(t, rec, &body)
	if body["status"] != "error" {
		t.Fatalf("expected status=error, got %q", body["status"])
	}
	if _, ok := body["error"]; ok {
		t.Fatalf("expected no error field in health response, got %q", body["error"])
	}
}

func TestHandleHealthLive(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/health/live", nil)
	requireStatus(t, rec, 200)
	var body map[string]string
	decodeJSON(t, rec, &body)
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok, got %q", body["status"])
	}
}

// TestHandleHealthLive_DBDown is the load-bearing assertion for ka-0000001764:
// the cheap liveness endpoint must stay 200 even when the DB is unreachable, so
// a slow/failing DB ping never pulls the single-replica beads pod out of its
// Service EndpointSlice (readiness) or restarts it (liveness). Contrast with
// TestHandleHealth_DBDown, where the DB-pinging /v1/health correctly 503s.
func TestHandleHealthLive_DBDown(t *testing.T) {
	ms := newMockStore()
	ms.pingErr = errors.New("connection refused")
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler(nil)

	rec := doJSON(t, h, "GET", "/v1/health/live", nil)
	requireStatus(t, rec, 200)
	var body map[string]string
	decodeJSON(t, rec, &body)
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok even with DB down, got %q", body["status"])
	}
}

// TestHandleHealthLive_AuthExempt verifies the liveness probe reaches the
// handler without a Bearer token even when the daemon is token-protected — the
// kubelet never carries credentials. Mirrors the /v1/health exemption.
func TestHandleHealthLive_AuthExempt(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler(nil, withTestToken(nil, "secret-token"))

	rec := doJSON(t, h, "GET", "/v1/health/live", nil)
	requireStatus(t, rec, 200)
	var body map[string]string
	decodeJSON(t, rec, &body)
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok without auth token, got %q", body["status"])
	}
}

func TestHandleCreateBead(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{"title": "Test bead", "type": "task"})
	requireStatus(t, rec, 201)
	var bead model.Bead
	decodeJSON(t, rec, &bead)
	if bead.ID == "" {
		t.Fatal("expected bead to have an ID")
	}
	if bead.Title != "Test bead" || bead.Status != model.StatusOpen || bead.Kind != model.KindIssue {
		t.Fatalf("got title=%q status=%q kind=%q", bead.Title, bead.Status, bead.Kind)
	}
}

func TestHandleListBeads(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-abc123"] = &model.Bead{ID: "gd-abc123", Title: "Bead one", Status: model.StatusOpen}
	ms.beads["gd-def456"] = &model.Bead{ID: "gd-def456", Title: "Bead two", Status: model.StatusOpen}

	rec := doJSON(t, h, "GET", "/v1/beads", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 2 || len(result.Beads) != 2 {
		t.Fatalf("expected 2 beads, got total=%d len=%d", result.Total, len(result.Beads))
	}
}

func TestHandleListBeads_WithFilters(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-f1"] = &model.Bead{ID: "gd-f1", Title: "Open test bead", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen, Assignee: "alice"}
	ms.beads["gd-f2"] = &model.Bead{ID: "gd-f2", Title: "Closed bead", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusClosed, Assignee: "alice"}

	rec := doJSON(t, h, "GET", "/v1/beads?status=open&type=task&kind=issue&limit=10&offset=0&sort=-priority&assignee=alice&search=test", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected total=1 (only open matching), got %d", result.Total)
	}
}

// TestHandleListBeads_LabelsAnyVsLabels is the ka-0000003307 regression guard:
// ?labels_any=a,b,c is the OR (union) — a bead matches with at least one label —
// while ?labels=a,b,c is the AND (intersection) — a bead must carry all. The
// canonical failure the bug describes is a release safety gate written as
// `-l release-stuck,preflight-broken,release-pipeline-broken` that returns 0
// unconditionally because no single bead carries all three kinds.
// TestHandleListBeads_WithOpenBlockers is the end-to-end proof of the fix in
// ka-0000000945: GET /v1/beads?with_open_blockers=true answers "why is each of
// these beads not ready?" in ONE call, with each blocker's status/type/title
// inline — the classification that previously cost one kd show per bead.
func TestHandleListBeads_WithOpenBlockers(t *testing.T) {
	ctx := context.Background()
	_, ms, h := newTestServer()

	// One blocked bug, gated by an open decision AND a task in status
	// 'blocked' — the status the audited agent's hand-rolled filter missed.
	ms.beads["gd-bug"] = &model.Bead{ID: "gd-bug", Title: "bug", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-dec"] = &model.Bead{ID: "gd-dec", Title: "operator must choose", Kind: model.KindData, Type: "decision", Status: model.StatusOpen}
	ms.beads["gd-upstream"] = &model.Bead{ID: "gd-upstream", Title: "upstream", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusBlocked}
	// A CLOSED blocker must NOT appear — it no longer gates anything.
	ms.beads["gd-done"] = &model.Bead{ID: "gd-done", Title: "done", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusClosed}
	// A parent-child edge must NOT appear — hierarchy is not a wait edge.
	ms.beads["gd-epic"] = &model.Bead{ID: "gd-epic", Title: "epic", Kind: model.KindIssue, Type: model.TypeEpic, Status: model.StatusOpen}
	for _, d := range []*model.Dependency{
		{BeadID: "gd-bug", DependsOnID: "gd-dec", Type: model.DepBlocks},
		{BeadID: "gd-bug", DependsOnID: "gd-upstream", Type: model.DepBlocks},
		{BeadID: "gd-bug", DependsOnID: "gd-done", Type: model.DepBlocks},
		{BeadID: "gd-bug", DependsOnID: "gd-epic", Type: model.DepParentChild},
	} {
		if err := ms.AddDependency(ctx, d); err != nil {
			t.Fatalf("seeding dep: %v", err)
		}
	}

	var result struct {
		Beads []model.Bead `json:"beads"`
	}
	rec := doJSON(t, h, "GET", "/v1/beads?with_open_blockers=true", nil)
	requireStatus(t, rec, 200)
	decodeJSON(t, rec, &result)

	var bug *model.Bead
	for i := range result.Beads {
		if result.Beads[i].ID == "gd-bug" {
			bug = &result.Beads[i]
		}
	}
	if bug == nil {
		t.Fatal("gd-bug missing from response")
	}
	// Present-but-empty vs absent is the version-skew signal, so assert the
	// key was populated at all before reading it.
	if bug.OpenBlockers == nil {
		t.Fatal("open_blockers must be present when with_open_blockers=true")
	}
	got := map[string]model.Status{}
	for _, b := range *bug.OpenBlockers {
		got[b.ID] = b.Status
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 open blockers (open decision + blocked task), got %d: %+v", len(got), got)
	}
	if got["gd-dec"] != model.StatusOpen {
		t.Errorf("open decision blocker missing/wrong: %+v", got)
	}
	// The regression that motivated the whole bug.
	if got["gd-upstream"] != model.StatusBlocked {
		t.Errorf("a blocker in status 'blocked' must still count as blocking: %+v", got)
	}
	if _, ok := got["gd-done"]; ok {
		t.Error("a CLOSED blocker must not be reported as an open blocker")
	}
	if _, ok := got["gd-epic"]; ok {
		t.Error("a parent-child edge is hierarchy, not a wait edge — must not be reported as a blocker")
	}
	// The blocker's TYPE must ride along: distinguishing "gated on an operator
	// decision" from "gated on real work" is the classification this enables.
	for _, b := range *bug.OpenBlockers {
		if b.ID == "gd-dec" && b.Type != "decision" {
			t.Errorf("blocker type not denormalized: %+v", b)
		}
		if b.ID == "gd-dec" && b.Title != "operator must choose" {
			t.Errorf("blocker title not denormalized: %+v", b)
		}
	}
}

// TestHandleListBeads_WithOpenBlockersEmptyIsPresent pins the three-state wire
// contract: an UNBLOCKED bead must come back with open_blockers present-but-
// empty ([]), never absent. Absent is reserved for "the daemon didn't answer",
// which is how a client detects version skew instead of misreading silence as
// an all-clear (ka-0000000945).
func TestHandleListBeads_WithOpenBlockersEmptyIsPresent(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-free"] = &model.Bead{ID: "gd-free", Title: "unblocked", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	rec := doJSON(t, h, "GET", "/v1/beads?with_open_blockers=true", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
	}
	decodeJSON(t, rec, &result)

	if len(result.Beads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(result.Beads))
	}
	if result.Beads[0].OpenBlockers == nil {
		t.Fatal("an unblocked bead must report open_blockers as present-and-empty, not absent — absent means 'not answered'")
	}
	if len(*result.Beads[0].OpenBlockers) != 0 {
		t.Errorf("expected no blockers, got %+v", *result.Beads[0].OpenBlockers)
	}
}

// TestHandleListBeads_WithOpenBlockersOptIn pins that the enrichment is
// strictly opt-in: a default /v1/beads call must carry no open_blockers, so
// every existing caller's payload and query cost are unchanged.
func TestHandleListBeads_WithOpenBlockersOptIn(t *testing.T) {
	ctx := context.Background()
	_, ms, h := newTestServer()
	ms.beads["gd-bug"] = &model.Bead{ID: "gd-bug", Title: "bug", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-dec"] = &model.Bead{ID: "gd-dec", Title: "d", Kind: model.KindData, Type: "decision", Status: model.StatusOpen}
	if err := ms.AddDependency(ctx, &model.Dependency{BeadID: "gd-bug", DependsOnID: "gd-dec", Type: model.DepBlocks}); err != nil {
		t.Fatalf("seeding dep: %v", err)
	}

	rec := doJSON(t, h, "GET", "/v1/beads", nil)
	requireStatus(t, rec, 200)
	if ms.lastListFilter.WithOpenBlockers {
		t.Error("WithOpenBlockers must default to false — the extra join is opt-in")
	}
	// And the JSON key must be absent entirely (omitempty), not null/[].
	if strings.Contains(rec.Body.String(), "open_blockers") {
		t.Errorf("default list response must not carry an open_blockers key: %s", rec.Body.String())
	}
}

// TestHandleListBeads_WithParent is the end-to-end proof of ka-0000006083:
// GET /v1/beads?with_parent=true carries each row's immediate parent-epic id
// (and title) inline, so a dispatcher can tell a leaf task under an epic from a
// floating one — and group by epic — without an N+1 kd show per row.
func TestHandleListBeads_WithParent(t *testing.T) {
	ctx := context.Background()
	_, ms, h := newTestServer()

	ms.beads["gd-epic"] = &model.Bead{ID: "gd-epic", Title: "Delivery Epic", Kind: model.KindIssue, Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-leaf"] = &model.Bead{ID: "gd-leaf", Title: "leaf task", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-orphan"] = &model.Bead{ID: "gd-orphan", Title: "floating task", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	// Canonical child-first parent-child edge: bead_id=child, depends_on_id=parent.
	if err := ms.AddDependency(ctx, &model.Dependency{BeadID: "gd-leaf", DependsOnID: "gd-epic", Type: model.DepParentChild}); err != nil {
		t.Fatalf("seeding dep: %v", err)
	}

	rec := doJSON(t, h, "GET", "/v1/beads?with_parent=true", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
	}
	decodeJSON(t, rec, &result)

	byID := map[string]model.Bead{}
	for _, b := range result.Beads {
		byID[b.ID] = b
	}
	if got := byID["gd-leaf"]; got.ParentID != "gd-epic" || got.ParentTitle != "Delivery Epic" {
		t.Errorf("leaf must carry its epic: id=%q title=%q", got.ParentID, got.ParentTitle)
	}
	if got := byID["gd-orphan"]; got.ParentID != "" {
		t.Errorf("orphan must have empty parent_id, got %q", got.ParentID)
	}
	// The epic itself has no parent, so it too reads empty.
	if got := byID["gd-epic"]; got.ParentID != "" {
		t.Errorf("top-level epic must have empty parent_id, got %q", got.ParentID)
	}
}

// TestHandleListBeads_WithParentMultiParent is the end-to-end proof of
// ka-0000001227: when a bead has SEVERAL parent-child edges, the enrichment must
// surface a MEANINGFUL primary parent — an OPEN EPIC — not the closed task whose
// id merely sorts earlier. Mirrors the live repro (ka-0000008173): a closed audit
// task ("gd-a-closed") and an open backlog epic ("gd-z-epic") both parent the
// child; the closed one sorts first by id but must NOT win.
func TestHandleListBeads_WithParentMultiParent(t *testing.T) {
	ctx := context.Background()
	_, ms, h := newTestServer()

	ms.beads["gd-a-closed"] = &model.Bead{ID: "gd-a-closed", Title: "Closed audit task", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusClosed}
	ms.beads["gd-z-epic"] = &model.Bead{ID: "gd-z-epic", Title: "Live backlog epic", Kind: model.KindIssue, Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-child"] = &model.Bead{ID: "gd-child", Title: "live work", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	// Both edges written in the same instant, closed-task parent added first (and
	// with the lexicographically-lower id) — exactly the shape the old id-sort
	// tie-break got wrong.
	ts := time.Date(2026, 8, 4, 23, 35, 32, 0, time.UTC)
	if err := ms.AddDependency(ctx, &model.Dependency{BeadID: "gd-child", DependsOnID: "gd-a-closed", Type: model.DepParentChild, CreatedAt: ts}); err != nil {
		t.Fatalf("seeding closed-parent dep: %v", err)
	}
	if err := ms.AddDependency(ctx, &model.Dependency{BeadID: "gd-child", DependsOnID: "gd-z-epic", Type: model.DepParentChild, CreatedAt: ts}); err != nil {
		t.Fatalf("seeding epic-parent dep: %v", err)
	}

	rec := doJSON(t, h, "GET", "/v1/beads?with_parent=true", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
	}
	decodeJSON(t, rec, &result)

	var child model.Bead
	for _, b := range result.Beads {
		if b.ID == "gd-child" {
			child = b
		}
	}
	if child.ParentID != "gd-z-epic" || child.ParentTitle != "Live backlog epic" {
		t.Errorf("multi-parent child must surface the OPEN EPIC, not the closed task: id=%q title=%q", child.ParentID, child.ParentTitle)
	}
}

// TestHandleListBeads_WithParentOptIn pins that the parent enrichment is
// strictly opt-in: a default /v1/beads call carries no parent_id key, so every
// existing caller's payload and query cost are unchanged.
func TestHandleListBeads_WithParentOptIn(t *testing.T) {
	ctx := context.Background()
	_, ms, h := newTestServer()
	ms.beads["gd-epic"] = &model.Bead{ID: "gd-epic", Title: "epic", Kind: model.KindIssue, Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-leaf"] = &model.Bead{ID: "gd-leaf", Title: "leaf", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	if err := ms.AddDependency(ctx, &model.Dependency{BeadID: "gd-leaf", DependsOnID: "gd-epic", Type: model.DepParentChild}); err != nil {
		t.Fatalf("seeding dep: %v", err)
	}

	rec := doJSON(t, h, "GET", "/v1/beads", nil)
	requireStatus(t, rec, 200)
	if ms.lastListFilter.WithParent {
		t.Error("WithParent must default to false — the extra parent join is opt-in")
	}
	if strings.Contains(rec.Body.String(), "parent_id") {
		t.Errorf("default list response must not carry a parent_id key: %s", rec.Body.String())
	}
}

func TestHandleListBeads_LabelsAnyVsLabels(t *testing.T) {
	ctx := context.Background()
	_, ms, h := newTestServer()
	// Three beads, each carrying exactly ONE distinct escalation-kind label.
	ms.beads["gd-stuck"] = &model.Bead{ID: "gd-stuck", Title: "stuck", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-preflight"] = &model.Bead{ID: "gd-preflight", Title: "preflight", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-pipeline"] = &model.Bead{ID: "gd-pipeline", Title: "pipeline", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	_ = ms.AddLabel(ctx, "gd-stuck", "release-stuck")
	_ = ms.AddLabel(ctx, "gd-preflight", "preflight-broken")
	_ = ms.AddLabel(ctx, "gd-pipeline", "release-pipeline-broken")

	decode := func(rec *httptest.ResponseRecorder) int {
		requireStatus(t, rec, 200)
		var result struct {
			Beads []model.Bead `json:"beads"`
			Total int          `json:"total"`
		}
		decodeJSON(t, rec, &result)
		return result.Total
	}

	// AND path (the footgun): no single bead carries all three → 0.
	andTotal := decode(doJSON(t, h, "GET", "/v1/beads?labels=release-stuck,preflight-broken,release-pipeline-broken", nil))
	if andTotal != 0 {
		t.Fatalf("?labels= (AND) over three distinct single-kind labels should match 0, got %d", andTotal)
	}

	// OR path (the fix): the union of all three → 3.
	orTotal := decode(doJSON(t, h, "GET", "/v1/beads?labels_any=release-stuck,preflight-broken,release-pipeline-broken", nil))
	if orTotal != 3 {
		t.Fatalf("?labels_any= (OR) over three distinct single-kind labels should match the union (3), got %d", orTotal)
	}

	// The user any-of must land on LabelsAnyUser, never the OIDC-scoping
	// LabelsAny (ka-0000003307) — so it can only narrow, never widen, scope.
	if len(ms.lastListFilter.LabelsAnyUser) != 3 {
		t.Fatalf("labels_any must populate LabelsAnyUser (3), got %v", ms.lastListFilter.LabelsAnyUser)
	}
	if len(ms.lastListFilter.LabelsAny) != 0 {
		t.Fatalf("labels_any must NOT touch the OIDC-scoping LabelsAny, got %v", ms.lastListFilter.LabelsAny)
	}
}

// TestHandleListBeads_AssigneeInFilter verifies ?assignee_in=csv parses into
// the model.BeadFilter.Assignees multi-valued predicate (design §4.2,
// ka-0000007555) — the daemon OR-merges it with the scalar assignee into a
// single `assignee = ANY(...)`. The beads-viewer bidirectional inbox issues
// type=mail&assignee_in=<recipient-handle-set> to list mail addressed to any
// of one operator's handles in one query.
func TestHandleListBeads_AssigneeInFilter(t *testing.T) {
	_, ms, h := newTestServer()

	rec := doJSON(t, h, "GET", "/v1/beads?type=mail&assignee_in=agent-alice,agent-bob", nil)
	requireStatus(t, rec, 200)

	got := ms.lastListFilter.Assignees
	if len(got) != 2 || got[0] != "agent-alice" || got[1] != "agent-bob" {
		t.Fatalf("Assignees = %v, want [agent-alice agent-bob]", got)
	}
}

// TestHandleListBeads_UnassignedFilter verifies ?unassigned=true parses into
// model.BeadFilter.Unassigned, which the store renders as the NULL-safe
// `assignee IS NULL OR assignee = <empty>` predicate (ka-0000007209).
//
// It also pins the two ways the filter must stay OFF, both of which mattered
// before the param existed:
//   - a bare /v1/beads leaves it false (so nothing changes for existing callers);
//   - ?assignee= (the empty scalar callers reached for first) does NOT imply it —
//     that spelling is genuinely indistinguishable from "no assignee filter",
//     which is exactly why an explicit param was required.
func TestHandleListBeads_UnassignedFilter(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{"unassigned=true sets the filter", "/v1/beads?unassigned=true", true},
		{"bare list leaves it off", "/v1/beads", false},
		{"empty assignee does NOT imply it", "/v1/beads?assignee=", false},
		{"unassigned=false leaves it off", "/v1/beads?unassigned=false", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()

			rec := doJSON(t, h, "GET", tc.query, nil)
			requireStatus(t, rec, 200)

			if got := ms.lastListFilter.Unassigned; got != tc.want {
				t.Fatalf("Unassigned = %v, want %v (query %s)", got, tc.want, tc.query)
			}
		})
	}
}

func TestHandleListBeads_UpdatedAfterFilter(t *testing.T) {
	_, ms, h := newTestServer()
	cutoff := time.Now().UTC().Truncate(time.Second)
	ms.beads["gd-new"] = &model.Bead{ID: "gd-new", Title: "newer", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen, UpdatedAt: cutoff.Add(time.Minute)}
	ms.beads["gd-old"] = &model.Bead{ID: "gd-old", Title: "older", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen, UpdatedAt: cutoff.Add(-time.Minute)}

	rec := doJSON(t, h, "GET", "/v1/beads?updated_after="+cutoff.Format(time.RFC3339), nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected total=1 (only newer), got %d", result.Total)
	}
	if result.Beads[0].ID != "gd-new" {
		t.Fatalf("expected gd-new, got %s", result.Beads[0].ID)
	}
}

func TestHandleListBeads_UpdatedAfterIgnoresUnparseable(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-a"] = &model.Bead{ID: "gd-a", Title: "a", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	rec := doJSON(t, h, "GET", "/v1/beads?updated_after=not-a-timestamp", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected unparseable updated_after to be silently ignored, got total=%d", result.Total)
	}
}

func TestHandleListBeads_UpdatedBeforeFilter(t *testing.T) {
	_, ms, h := newTestServer()
	cutoff := time.Now().UTC().Truncate(time.Second)
	ms.beads["gd-new"] = &model.Bead{ID: "gd-new", Title: "newer", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen, UpdatedAt: cutoff.Add(time.Minute)}
	ms.beads["gd-old"] = &model.Bead{ID: "gd-old", Title: "older", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen, UpdatedAt: cutoff.Add(-time.Minute)}

	rec := doJSON(t, h, "GET", "/v1/beads?updated_before="+cutoff.Format(time.RFC3339), nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected total=1 (only older), got %d", result.Total)
	}
	if result.Beads[0].ID != "gd-old" {
		t.Fatalf("expected gd-old, got %s", result.Beads[0].ID)
	}
}

func TestHandleListBeads_UpdatedBeforeIgnoresUnparseable(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-a"] = &model.Bead{ID: "gd-a", Title: "a", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	rec := doJSON(t, h, "GET", "/v1/beads?updated_before=not-a-timestamp", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected unparseable updated_before to be silently ignored, got total=%d", result.Total)
	}
}

// TestHandleListBeads_CreatedAfterFilter pins that the created_after query
// param is actually parsed into filter.CreatedAfter. The filter field and the
// store WHERE clause predate the HTTP parsing — until ka-0000008195 the param
// was silently ignored, returning the full unwindowed set.
func TestHandleListBeads_CreatedAfterFilter(t *testing.T) {
	_, ms, h := newTestServer()
	cutoff := time.Now().UTC().Truncate(time.Second)
	ms.beads["gd-new"] = &model.Bead{ID: "gd-new", Title: "newer", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen, CreatedAt: cutoff.Add(time.Minute)}
	ms.beads["gd-old"] = &model.Bead{ID: "gd-old", Title: "older", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen, CreatedAt: cutoff.Add(-time.Minute)}

	rec := doJSON(t, h, "GET", "/v1/beads?created_after="+cutoff.Format(time.RFC3339), nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected total=1 (only newer), got %d", result.Total)
	}
	if result.Beads[0].ID != "gd-new" {
		t.Fatalf("expected gd-new, got %s", result.Beads[0].ID)
	}
}

func TestHandleListBeads_CreatedAfterIgnoresUnparseable(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-a"] = &model.Bead{ID: "gd-a", Title: "a", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	rec := doJSON(t, h, "GET", "/v1/beads?created_after=not-a-timestamp", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected unparseable created_after to be silently ignored, got total=%d", result.Total)
	}
}

func TestHandleListBeads_CreatedBeforeFilter(t *testing.T) {
	_, ms, h := newTestServer()
	cutoff := time.Now().UTC().Truncate(time.Second)
	ms.beads["gd-new"] = &model.Bead{ID: "gd-new", Title: "newer", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen, CreatedAt: cutoff.Add(time.Minute)}
	ms.beads["gd-old"] = &model.Bead{ID: "gd-old", Title: "older", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen, CreatedAt: cutoff.Add(-time.Minute)}

	rec := doJSON(t, h, "GET", "/v1/beads?created_before="+cutoff.Format(time.RFC3339), nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected total=1 (only older), got %d", result.Total)
	}
	if result.Beads[0].ID != "gd-old" {
		t.Fatalf("expected gd-old, got %s", result.Beads[0].ID)
	}
}

func TestHandleListBeads_CreatedBeforeIgnoresUnparseable(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-a"] = &model.Bead{ID: "gd-a", Title: "a", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	rec := doJSON(t, h, "GET", "/v1/beads?created_before=not-a-timestamp", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected unparseable created_before to be silently ignored, got total=%d", result.Total)
	}
}

// TestHandleListBeads_ClosedAfterFilter pins the closed_after window on
// closed_at. The never-closed bead (nil ClosedAt) must match neither side of
// the window — the NULL-safe comparison the postgres store relies on.
func TestHandleListBeads_ClosedAfterFilter(t *testing.T) {
	_, ms, h := newTestServer()
	cutoff := time.Now().UTC().Truncate(time.Second)
	recent := cutoff.Add(time.Minute)
	stale := cutoff.Add(-time.Minute)
	ms.beads["gd-recent"] = &model.Bead{ID: "gd-recent", Title: "recently closed", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusClosed, ClosedAt: &recent}
	ms.beads["gd-stale"] = &model.Bead{ID: "gd-stale", Title: "closed long ago", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusClosed, ClosedAt: &stale}
	ms.beads["gd-open"] = &model.Bead{ID: "gd-open", Title: "never closed", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	rec := doJSON(t, h, "GET", "/v1/beads?closed_after="+cutoff.Format(time.RFC3339), nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected total=1 (only recently closed), got %d", result.Total)
	}
	if result.Beads[0].ID != "gd-recent" {
		t.Fatalf("expected gd-recent, got %s", result.Beads[0].ID)
	}
}

func TestHandleListBeads_ClosedAfterIgnoresUnparseable(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-a"] = &model.Bead{ID: "gd-a", Title: "a", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	rec := doJSON(t, h, "GET", "/v1/beads?closed_after=not-a-timestamp", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected unparseable closed_after to be silently ignored, got total=%d", result.Total)
	}
}

func TestHandleListBeads_ClosedBeforeFilter(t *testing.T) {
	_, ms, h := newTestServer()
	cutoff := time.Now().UTC().Truncate(time.Second)
	recent := cutoff.Add(time.Minute)
	stale := cutoff.Add(-time.Minute)
	ms.beads["gd-recent"] = &model.Bead{ID: "gd-recent", Title: "recently closed", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusClosed, ClosedAt: &recent}
	ms.beads["gd-stale"] = &model.Bead{ID: "gd-stale", Title: "closed long ago", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusClosed, ClosedAt: &stale}
	ms.beads["gd-open"] = &model.Bead{ID: "gd-open", Title: "never closed", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	rec := doJSON(t, h, "GET", "/v1/beads?closed_before="+cutoff.Format(time.RFC3339), nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected total=1 (only stale-closed; never-closed must not match), got %d", result.Total)
	}
	if result.Beads[0].ID != "gd-stale" {
		t.Fatalf("expected gd-stale, got %s", result.Beads[0].ID)
	}
}

func TestHandleListBeads_ClosedBeforeIgnoresUnparseable(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-a"] = &model.Bead{ID: "gd-a", Title: "a", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	rec := doJSON(t, h, "GET", "/v1/beads?closed_before=not-a-timestamp", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected unparseable closed_before to be silently ignored, got total=%d", result.Total)
	}
}

func TestHandleListBeads_MentionsFilter(t *testing.T) {
	_, ms, h := newTestServer()
	target := "ka-0000004973"
	ms.beads["gd-src1"] = &model.Bead{ID: "gd-src1", Title: "Source one", Status: model.StatusOpen, Type: model.TypeTask, Kind: model.KindIssue}
	ms.beads["gd-src2"] = &model.Bead{ID: "gd-src2", Title: "Source two", Status: model.StatusClosed, Type: model.TypeTask, Kind: model.KindIssue}
	ms.beads["gd-other"] = &model.Bead{ID: "gd-other", Title: "Unrelated", Status: model.StatusOpen, Type: model.TypeTask, Kind: model.KindIssue}
	ms.mentionsBySource = map[string][]string{
		"gd-src1": {target},
		"gd-src2": {target},
		// gd-other does not mention the target.
	}

	rec := doJSON(t, h, "GET", "/v1/beads?mentions="+target, nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 2 {
		t.Fatalf("expected total=2 (only beads mentioning %s), got %d", target, result.Total)
	}
	for _, b := range result.Beads {
		if b.ID == "gd-other" {
			t.Fatalf("gd-other should not be in mentions=%s result", target)
		}
	}
	if ms.lastListFilter.Mentions != target {
		t.Fatalf("expected store filter Mentions=%q, got %q", target, ms.lastListFilter.Mentions)
	}
}

func TestHandleListBeads_MentionsComposesWithStatus(t *testing.T) {
	_, ms, h := newTestServer()
	target := "ka-0000004973"
	ms.beads["gd-src-open"] = &model.Bead{ID: "gd-src-open", Title: "open src", Status: model.StatusOpen, Type: model.TypeTask, Kind: model.KindIssue}
	ms.beads["gd-src-closed"] = &model.Bead{ID: "gd-src-closed", Title: "closed src", Status: model.StatusClosed, Type: model.TypeTask, Kind: model.KindIssue}
	ms.mentionsBySource = map[string][]string{
		"gd-src-open":   {target},
		"gd-src-closed": {target},
	}

	rec := doJSON(t, h, "GET", "/v1/beads?mentions="+target+"&status=open", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 1 {
		t.Fatalf("expected total=1 (open AND mentioning), got %d", result.Total)
	}
	if len(result.Beads) != 1 || result.Beads[0].ID != "gd-src-open" {
		t.Fatalf("expected only gd-src-open, got %+v", result.Beads)
	}
}

// ka-0000001544: ?mentions_type=<verb> must propagate into BeadFilter.MentionsType
// with three-state semantics matching `kd refs --type`:
//   - omitted        → nil  (no relationship-type narrowing)
//   - mentions_type= → ""   (untyped only, NULL relationship_type)
//   - mentions_type=blocks → "blocks"
//
// Composing with ?mentions= is the only meaningful use; bare ?mentions_type=
// without ?mentions= is allowed by the parser but the store-level filter is
// gated on Mentions being set (see queries.go), so the param is effectively
// inert without it.
func TestHandleListBeads_MentionsTypeQueryParam(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want *string
	}{
		{"omitted", "/v1/beads?mentions=gd-Target001", nil},
		{"empty value", "/v1/beads?mentions=gd-Target001&mentions_type=", strPtr("")},
		{"verb", "/v1/beads?mentions=gd-Target001&mentions_type=blocks", strPtr("blocks")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			rec := doJSON(t, h, "GET", tc.url, nil)
			requireStatus(t, rec, 200)
			got := ms.lastListFilter.MentionsType
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("MentionsType = %q, want nil", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("MentionsType = nil, want %q", *tc.want)
			case tc.want != nil && got != nil && *tc.want != *got:
				t.Fatalf("MentionsType = %q, want %q", *got, *tc.want)
			}
		})
	}
}

// ka-0000007923 (R1): ?has_report=true|false must propagate into
// BeadFilter.HasReport with three-state semantics:
//   - omitted        → nil  (no has_report filter)
//   - has_report=true  → *true
//   - has_report=false → *false
func TestHandleListBeads_HasReportQueryParam(t *testing.T) {
	tru, fal := true, false
	cases := []struct {
		name string
		url  string
		want *bool
	}{
		{"omitted", "/v1/beads", nil},
		{"true", "/v1/beads?has_report=true", &tru},
		{"false", "/v1/beads?has_report=false", &fal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			rec := doJSON(t, h, "GET", tc.url, nil)
			requireStatus(t, rec, 200)
			got := ms.lastListFilter.HasReport
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("HasReport = %v, want nil", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("HasReport = nil, want %v", *tc.want)
			case tc.want != nil && got != nil && *tc.want != *got:
				t.Fatalf("HasReport = %v, want %v", *got, *tc.want)
			}
		})
	}
}

// ka-0000006652: ?exclude_scheduled=true must propagate through the HTTP
// query parser into the BeadFilter so the store can suppress cron-spawned
// beads. Default omission must keep ExcludeScheduled=false (backward compat).
func TestHandleListBeads_ExcludeScheduledQueryParam(t *testing.T) {
	t.Run("default omitted", func(t *testing.T) {
		_, ms, h := newTestServer()
		ms.beads["gd-x1"] = &model.Bead{ID: "gd-x1", Title: "x", Status: model.StatusOpen, Type: model.TypeTask, Kind: model.KindIssue}
		rec := doJSON(t, h, "GET", "/v1/beads", nil)
		requireStatus(t, rec, 200)
		if ms.lastListFilter.ExcludeScheduled {
			t.Fatal("expected ExcludeScheduled=false when query param omitted")
		}
	})
	t.Run("set true", func(t *testing.T) {
		_, ms, h := newTestServer()
		ms.beads["gd-x1"] = &model.Bead{ID: "gd-x1", Title: "x", Status: model.StatusOpen, Type: model.TypeTask, Kind: model.KindIssue}
		rec := doJSON(t, h, "GET", "/v1/beads?exclude_scheduled=true", nil)
		requireStatus(t, rec, 200)
		if !ms.lastListFilter.ExcludeScheduled {
			t.Fatal("expected ExcludeScheduled=true when query param set")
		}
	})
}

// §T2 (gardenio/plans/epic-mention-looping-gardener.md): ?parent=<bead-id> must
// propagate through the HTTP query parser into BeadFilter.ParentSubtreeID so
// scheduler bead_query conditions can count beads in an epic's subtree.
func TestHandleListBeads_ParentSubtreeQueryParam(t *testing.T) {
	t.Run("default omitted", func(t *testing.T) {
		_, ms, h := newTestServer()
		ms.beads["gd-x1"] = &model.Bead{ID: "gd-x1", Title: "x", Status: model.StatusOpen, Type: model.TypeTask, Kind: model.KindIssue}
		rec := doJSON(t, h, "GET", "/v1/beads", nil)
		requireStatus(t, rec, 200)
		if ms.lastListFilter.ParentSubtreeID != "" {
			t.Fatalf("expected ParentSubtreeID empty by default, got %q", ms.lastListFilter.ParentSubtreeID)
		}
	})
	t.Run("set to bead-id", func(t *testing.T) {
		_, ms, h := newTestServer()
		ms.beads["gd-x1"] = &model.Bead{ID: "gd-x1", Title: "x", Status: model.StatusOpen, Type: model.TypeTask, Kind: model.KindIssue}
		rec := doJSON(t, h, "GET", "/v1/beads?parent=gd-Epic1", nil)
		requireStatus(t, rec, 200)
		if ms.lastListFilter.ParentSubtreeID != "gd-Epic1" {
			t.Fatalf("expected ParentSubtreeID=gd-Epic1, got %q", ms.lastListFilter.ParentSubtreeID)
		}
	})
}

func TestHandleListBeads_MentionsEmptyResultWhenNoReferences(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-noref"] = &model.Bead{ID: "gd-noref", Title: "no refs", Status: model.StatusOpen, Type: model.TypeTask, Kind: model.KindIssue}
	// no references map populated

	rec := doJSON(t, h, "GET", "/v1/beads?mentions=ka-0000000015", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 0 {
		t.Fatalf("expected total=0, got %d", result.Total)
	}
}

func TestHandleCreateBead_FleetFromContext(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler(nil)

	body, _ := json.Marshal(map[string]any{"title": "Fleet bead", "type": "task"})
	req := httptest.NewRequest("POST", "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Inject fleet + fleet identity into context (simulating auth middleware).
	ctx := WithFleet(req.Context(), "qa-fleet")
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: "qa-fleet"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, 201)
	var bead model.Bead
	decodeJSON(t, rec, &bead)
	if bead.Fleet != "qa-fleet" {
		t.Fatalf("expected fleet=qa-fleet in response, got %q", bead.Fleet)
	}
	// Verify stored bead.
	stored := ms.beads[bead.ID]
	if stored.Fleet != "qa-fleet" {
		t.Fatalf("expected stored fleet=qa-fleet, got %q", stored.Fleet)
	}
}

// TestHandleListBeads_NoFleetFilter_ReadsAcrossFleets is the regression guard
// for epic ka-0000000871 / ka-0000003285 — the old per-token read-isolation
// gate (where reads defaulted to filter.Fleet = FleetFromContext(ctx)) has
// been removed. Reads now span every fleet regardless of the caller's
// resolved fleet; `bead.fleet` is audit-attribution only. The two P0/P1 bugs
// that motivated this change were ka-0000001804 (FSM-351 controller-pod-drop)
// and ka-0000003707 (Coverage Phase 2c ratchet blocker).
func TestHandleListBeads_NoFleetFilter_ReadsAcrossFleets(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler(nil)

	ms.beads["gd-qa1"] = &model.Bead{ID: "gd-qa1", Title: "QA bead", Status: model.StatusOpen, Fleet: "qa-fleet"}
	ms.beads["gd-prod1"] = &model.Bead{ID: "gd-prod1", Title: "Prod bead", Status: model.StatusOpen, Fleet: "production"}

	req := httptest.NewRequest("GET", "/v1/beads", nil)
	// Caller's resolved fleet is qa-fleet — but the read filter no longer
	// scopes to it. Both beads must come back.
	req = req.WithContext(WithFleet(req.Context(), "qa-fleet"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if len(result.Beads) != 2 {
		t.Fatalf("expected 2 beads (cross-fleet), got %d: %+v", len(result.Beads), result.Beads)
	}
	gotFleets := map[string]bool{}
	for _, b := range result.Beads {
		gotFleets[b.Fleet] = true
	}
	if !gotFleets["qa-fleet"] || !gotFleets["production"] {
		t.Fatalf("expected both qa-fleet and production beads, got fleets=%v", gotFleets)
	}
}

func TestHandleListBeads_FleetQueryParamOverride(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler(nil)

	ms.beads["gd-prod1"] = &model.Bead{ID: "gd-prod1", Title: "Prod bead", Status: model.StatusOpen, Fleet: "production"}

	// Auth context says "qa-fleet" but query param says "production".
	req := httptest.NewRequest("GET", "/v1/beads?fleet=production", nil)
	req = req.WithContext(WithFleet(req.Context(), "qa-fleet"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	// Query param "production" should override context "qa-fleet".
	if result.Total != 1 {
		t.Fatalf("expected 1 bead (production fleet), got %d", result.Total)
	}
}

func TestHandleListBeads_FleetWildcard(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler(nil)

	ms.beads["gd-qa1"] = &model.Bead{ID: "gd-qa1", Title: "QA bead", Status: model.StatusOpen, Fleet: "qa-fleet"}
	ms.beads["gd-prod1"] = &model.Bead{ID: "gd-prod1", Title: "Prod bead", Status: model.StatusOpen, Fleet: "production"}

	// fleet=* should return beads from all fleets.
	req := httptest.NewRequest("GET", "/v1/beads?fleet=*", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, 200)
	var result struct {
		Beads []model.Bead `json:"beads"`
		Total int          `json:"total"`
	}
	decodeJSON(t, rec, &result)
	if result.Total != 2 {
		t.Fatalf("expected 2 beads (all fleets), got %d", result.Total)
	}
}

func TestHandleTransferBead(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-t1"] = &model.Bead{ID: "gd-t1", Title: "Transfer me", Status: model.StatusOpen, Fleet: "alpha"}

	rec := doJSON(t, h, "POST", "/v1/beads/gd-t1/transfer", map[string]string{"to_fleet": "beta"})
	requireStatus(t, rec, 200)
	var bead model.Bead
	decodeJSON(t, rec, &bead)
	if bead.Fleet != "beta" {
		t.Fatalf("expected fleet=beta, got %q", bead.Fleet)
	}
}

func TestHandleTransferBead_NotFound(t *testing.T) {
	_, _, h := newTestServer()

	rec := doJSON(t, h, "POST", "/v1/beads/gd-nope/transfer", map[string]string{"to_fleet": "beta"})
	requireStatus(t, rec, 404)
}

func TestHandleTransferBead_MissingFleet(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-t1"] = &model.Bead{ID: "gd-t1", Title: "Transfer me", Status: model.StatusOpen, Fleet: "alpha"}

	rec := doJSON(t, h, "POST", "/v1/beads/gd-t1/transfer", map[string]string{})
	requireStatus(t, rec, 400)
}

func TestHandleGetBead(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-test1"] = &model.Bead{ID: "gd-test1", Title: "Test bead", Status: model.StatusOpen}

	rec := doJSON(t, h, "GET", "/v1/beads/gd-test1", nil)
	requireStatus(t, rec, 200)
	var bead model.Bead
	decodeJSON(t, rec, &bead)
	if bead.ID != "gd-test1" || bead.Title != "Test bead" {
		t.Fatalf("got id=%q title=%q", bead.ID, bead.Title)
	}
}

func TestHandleGetBead_HasBundleFalse(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-nb"] = &model.Bead{ID: "gd-nb", Title: "No bundle", Status: model.StatusOpen}

	rec := doJSON(t, h, "GET", "/v1/beads/gd-nb", nil)
	requireStatus(t, rec, 200)
	var resp struct {
		ID        string `json:"id"`
		HasBundle bool   `json:"has_bundle"`
	}
	decodeJSON(t, rec, &resp)
	if resp.ID != "gd-nb" {
		t.Fatalf("got id=%q", resp.ID)
	}
	if resp.HasBundle {
		t.Errorf("expected has_bundle=false, got true")
	}
}

func TestHandleGetBead_HasBundleTrue(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-parent"] = &model.Bead{ID: "gd-parent", Title: "Parent task", Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-bundle"] = &model.Bead{ID: "gd-bundle", Title: "Bundle child", Type: "bundle", Status: model.StatusOpen}
	parentChild := string(model.DepParentChild)
	ms.references = append(ms.references, &model.BeadReference{
		SourceBeadID:     "gd-bundle",
		TargetBeadID:     "gd-parent",
		RelationshipType: &parentChild,
	})

	rec := doJSON(t, h, "GET", "/v1/beads/gd-parent", nil)
	requireStatus(t, rec, 200)
	var resp struct {
		ID        string `json:"id"`
		HasBundle bool   `json:"has_bundle"`
	}
	decodeJSON(t, rec, &resp)
	if !resp.HasBundle {
		t.Errorf("expected has_bundle=true, got false")
	}
}

func TestHandleSetConfig(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "PUT", "/v1/configs/view:inbox", map[string]any{
		"value": map[string]any{"filter": map[string]any{"status": []string{"open"}}},
	})
	requireStatus(t, rec, 200)
	var config model.Config
	decodeJSON(t, rec, &config)
	if config.Key != "view:inbox" {
		t.Fatalf("expected key=%q, got %q", "view:inbox", config.Key)
	}
}

func TestHandleGetConfig(t *testing.T) {
	_, ms, h := newTestServer()
	ms.configs["view:inbox"] = &model.Config{Key: "view:inbox", Value: json.RawMessage(`{"filter":{"status":["open"]}}`)}

	rec := doJSON(t, h, "GET", "/v1/configs/view:inbox", nil)
	requireStatus(t, rec, 200)
	var config model.Config
	decodeJSON(t, rec, &config)
	if config.Key != "view:inbox" {
		t.Fatalf("expected key=%q, got %q", "view:inbox", config.Key)
	}
}

func TestHandleListConfigs(t *testing.T) {
	_, ms, h := newTestServer()
	ms.configs["view:inbox"] = &model.Config{Key: "view:inbox", Value: json.RawMessage(`{}`)}
	ms.configs["view:my-tasks"] = &model.Config{Key: "view:my-tasks", Value: json.RawMessage(`{}`)}
	ms.configs["type:decision"] = &model.Config{Key: "type:decision", Value: json.RawMessage(`{}`)}

	rec := doJSON(t, h, "GET", "/v1/configs?namespace=view", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Configs []model.Config `json:"configs"`
	}
	decodeJSON(t, rec, &result)
	// 2 stored + 3 builtins (view:ready, view:users, view:audit:recent)
	if len(result.Configs) != 5 {
		t.Fatalf("expected 5 configs, got %d", len(result.Configs))
	}
}

func TestHandleDeleteConfig(t *testing.T) {
	_, ms, h := newTestServer()
	ms.configs["view:inbox"] = &model.Config{Key: "view:inbox", Value: json.RawMessage(`{}`)}

	rec := doJSON(t, h, "DELETE", "/v1/configs/view:inbox", nil)
	requireStatus(t, rec, 204)
	if _, ok := ms.configs["view:inbox"]; ok {
		t.Fatal("expected config to be deleted")
	}
}

func TestHandleGetConfig_BuiltinFallback(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/configs/view:ready", nil)
	requireStatus(t, rec, 200)
	var config model.Config
	decodeJSON(t, rec, &config)
	if config.Key != "view:ready" {
		t.Fatalf("expected key=%q, got %q", "view:ready", config.Key)
	}
}

func TestHandleGetConfig_StoreOverridesBuiltin(t *testing.T) {
	_, ms, h := newTestServer()
	ms.configs["view:ready"] = &model.Config{Key: "view:ready", Value: json.RawMessage(`{"limit":99}`)}

	rec := doJSON(t, h, "GET", "/v1/configs/view:ready", nil)
	requireStatus(t, rec, 200)
	var config model.Config
	decodeJSON(t, rec, &config)
	if string(config.Value) != `{"limit":99}` {
		t.Fatalf("expected store override, got %s", config.Value)
	}
}

func TestHandleListConfigs_IncludesBuiltins(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/configs?namespace=view", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Configs []model.Config `json:"configs"`
	}
	decodeJSON(t, rec, &result)
	if len(result.Configs) != 3 {
		t.Fatalf("expected 3 builtin configs (view:ready, view:users, view:audit:recent), got %d", len(result.Configs))
	}
}

func TestHandleListConfigs_BuiltinNotDuplicated(t *testing.T) {
	_, ms, h := newTestServer()
	ms.configs["view:ready"] = &model.Config{Key: "view:ready", Value: json.RawMessage(`{"limit":99}`)}

	rec := doJSON(t, h, "GET", "/v1/configs?namespace=view", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Configs []model.Config `json:"configs"`
	}
	decodeJSON(t, rec, &result)
	// 1 stored view:ready (overrides builtin) + 2 builtins (view:users, view:audit:recent)
	if len(result.Configs) != 3 {
		t.Fatalf("expected 3 configs (no duplicate), got %d", len(result.Configs))
	}
	// Find the view:ready config and verify it's the store override.
	var found bool
	for _, c := range result.Configs {
		if c.Key == "view:ready" {
			if string(c.Value) != `{"limit":99}` {
				t.Fatalf("expected store override value for view:ready, got %s", c.Value)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("view:ready not found in config list")
	}
}

func TestHandleGetDependencies(t *testing.T) {
	_, ms, h := newTestServer()
	ms.deps["gd-dep1"] = []*model.Dependency{
		{BeadID: "gd-dep1", DependsOnID: "gd-dep2", Type: "blocks"},
		{BeadID: "gd-dep1", DependsOnID: "gd-dep3", Type: "related"},
	}
	rec := doJSON(t, h, "GET", "/v1/beads/gd-dep1/dependencies", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Dependencies []model.Dependency `json:"dependencies"`
	}
	decodeJSON(t, rec, &result)
	if len(result.Dependencies) != 2 {
		t.Fatalf("expected 2 dependencies, got %d", len(result.Dependencies))
	}
}

func TestHandleGetLabels(t *testing.T) {
	_, ms, h := newTestServer()
	ms.labels["gd-lbl1"] = []string{"urgent", "frontend"}

	rec := doJSON(t, h, "GET", "/v1/beads/gd-lbl1/labels", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Labels []string `json:"labels"`
	}
	decodeJSON(t, rec, &result)
	if len(result.Labels) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(result.Labels))
	}
}

func TestHandleGetComments(t *testing.T) {
	_, ms, h := newTestServer()
	ms.comments["gd-cmt1"] = []*model.Comment{
		{ID: 1, BeadID: "gd-cmt1", Author: "alice", Text: "First"},
		{ID: 2, BeadID: "gd-cmt1", Author: "bob", Text: "Second"},
	}
	rec := doJSON(t, h, "GET", "/v1/beads/gd-cmt1/comments", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Comments []model.Comment `json:"comments"`
	}
	decodeJSON(t, rec, &result)
	if len(result.Comments) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(result.Comments))
	}
}

func TestHandleGetEvents(t *testing.T) {
	_, ms, h := newTestServer()
	ms.events = []*model.Event{
		{ID: 1, Topic: "beads.bead.created", BeadID: "gd-abc123", Actor: "alice", Payload: json.RawMessage(`{}`)},
		{ID: 2, Topic: "beads.label.added", BeadID: "gd-abc123", Payload: json.RawMessage(`{}`)},
		{ID: 3, Topic: "beads.bead.created", BeadID: "gd-other", Payload: json.RawMessage(`{}`)},
	}
	rec := doJSON(t, h, "GET", "/v1/beads/gd-abc123/events", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Events []model.Event `json:"events"`
	}
	decodeJSON(t, rec, &result)
	if len(result.Events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(result.Events))
	}
}

func TestHandleGetEvents_Limit(t *testing.T) {
	_, ms, h := newTestServer()
	ms.events = []*model.Event{
		{ID: 1, Topic: "beads.bead.created", BeadID: "gd-trig", Payload: json.RawMessage(`{}`)},
		{ID: 2, Topic: "beads.label.added", BeadID: "gd-trig", Payload: json.RawMessage(`{}`)},
		{ID: 3, Topic: "beads.label.removed", BeadID: "gd-trig", Payload: json.RawMessage(`{}`)},
		{ID: 4, Topic: "beads.comment.added", BeadID: "gd-trig", Payload: json.RawMessage(`{}`)},
	}

	// limit=2 → newest 2 (ids 3,4).
	rec := doJSON(t, h, "GET", "/v1/beads/gd-trig/events?limit=2", nil)
	requireStatus(t, rec, 200)
	var result struct {
		Events []model.Event `json:"events"`
	}
	decodeJSON(t, rec, &result)
	if len(result.Events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(result.Events))
	}
	if result.Events[0].ID != 3 || result.Events[1].ID != 4 {
		t.Fatalf("expected events 3,4 (chronological), got %d,%d", result.Events[0].ID, result.Events[1].ID)
	}

	// limit > available → returns all available.
	rec = doJSON(t, h, "GET", "/v1/beads/gd-trig/events?limit=100", nil)
	requireStatus(t, rec, 200)
	decodeJSON(t, rec, &result)
	if len(result.Events) != 4 {
		t.Fatalf("expected 4 events with high limit, got %d", len(result.Events))
	}

	// empty limit → unlimited (legacy path).
	rec = doJSON(t, h, "GET", "/v1/beads/gd-trig/events", nil)
	requireStatus(t, rec, 200)
	decodeJSON(t, rec, &result)
	if len(result.Events) != 4 {
		t.Fatalf("expected 4 events with no limit, got %d", len(result.Events))
	}

	// invalid limit → 400.
	for _, bad := range []string{"0", "-1", "10001", "abc"} {
		rec = doJSON(t, h, "GET", "/v1/beads/gd-trig/events?limit="+bad, nil)
		if rec.Code != 400 {
			t.Fatalf("limit=%q: expected 400, got %d", bad, rec.Code)
		}
	}
}

func TestHandleEmptyLists(t *testing.T) {
	_, _, h := newTestServer()
	for _, tc := range []struct {
		name string
		path string
	}{
		{"Dependencies", "/v1/beads/gd-nope/dependencies"},
		{"Labels", "/v1/beads/gd-nope/labels"},
		{"Comments", "/v1/beads/gd-nope/comments"},
		{"Events", "/v1/beads/gd-nonexistent/events"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, h, "GET", tc.path, nil)
			requireStatus(t, rec, 200)
		})
	}
}

func TestHandleAddComment_Response(t *testing.T) {
	_, ms, h := newTestServerAuth()
	ms.beads["gd-cmt2"] = &model.Bead{ID: "gd-cmt2", Title: "Bead for comment", Status: model.StatusOpen}

	rec := doJSONAs(t, h, "POST", "/v1/beads/gd-cmt2/comments", map[string]any{"author": "evil-forged", "text": "A new comment"}, "alice")
	requireStatus(t, rec, 201)
	var comment model.Comment
	decodeJSON(t, rec, &comment)
	if comment.Text != "A new comment" || comment.Author != "alice" || comment.BeadID != "gd-cmt2" {
		t.Fatalf("got text=%q author=%q bead_id=%q", comment.Text, comment.Author, comment.BeadID)
	}
}

func TestHandleDeleteComment_Existing(t *testing.T) {
	_, ms, h := newTestServerAuth()
	ms.beads["gd-d1"] = &model.Bead{ID: "gd-d1", Title: "x", Status: model.StatusOpen}
	ms.comments["gd-d1"] = []*model.Comment{{ID: 1, BeadID: "gd-d1", Author: "alice", Text: "hi"}}

	rec := doJSONAs(t, h, "DELETE", "/v1/beads/gd-d1/comments/1", nil, "alice")
	requireStatus(t, rec, 200)
	var resp struct {
		Deleted bool `json:"deleted"`
	}
	decodeJSON(t, rec, &resp)
	if !resp.Deleted {
		t.Fatalf("expected deleted=true, got %+v", resp)
	}
	if len(ms.comments["gd-d1"]) != 0 {
		t.Fatalf("expected store empty, got %d", len(ms.comments["gd-d1"]))
	}
}

func TestHandleDeleteComment_Missing_ReturnsDeletedFalse(t *testing.T) {
	_, ms, h := newTestServerAuth()
	ms.beads["gd-d2"] = &model.Bead{ID: "gd-d2", Title: "x", Status: model.StatusOpen}

	rec := doJSONAs(t, h, "DELETE", "/v1/beads/gd-d2/comments/999", nil, "alice")
	requireStatus(t, rec, 200)
	var resp struct {
		Deleted bool `json:"deleted"`
	}
	decodeJSON(t, rec, &resp)
	if resp.Deleted {
		t.Fatalf("expected deleted=false for missing row, got %+v", resp)
	}
}

func TestHandleDeleteComment_BadCommentID(t *testing.T) {
	_, ms, h := newTestServerAuth()
	ms.beads["gd-d3"] = &model.Bead{ID: "gd-d3", Title: "x", Status: model.StatusOpen}
	rec := doJSONAs(t, h, "DELETE", "/v1/beads/gd-d3/comments/notanint", nil, "alice")
	requireStatus(t, rec, 400)
}

func TestHandleTrimComments_AuthorFilter(t *testing.T) {
	_, ms, h := newTestServerAuth()
	ms.beads["gd-t1"] = &model.Bead{ID: "gd-t1", Title: "x", Status: model.StatusOpen}
	now := time.Now().UTC()
	ms.comments["gd-t1"] = []*model.Comment{
		{ID: 1, BeadID: "gd-t1", Author: "scheduler", Text: "s1", CreatedAt: now.Add(-5 * time.Hour)},
		{ID: 2, BeadID: "gd-t1", Author: "scheduler", Text: "s2", CreatedAt: now.Add(-4 * time.Hour)},
		{ID: 3, BeadID: "gd-t1", Author: "scheduler", Text: "s3", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: 4, BeadID: "gd-t1", Author: "alice", Text: "human", CreatedAt: now.Add(-2 * time.Hour)},
	}

	rec := doJSONAs(t, h, "POST", "/v1/beads/gd-t1/comments/trim",
		map[string]any{"keep_last": 1, "author": "scheduler"}, "alice")
	requireStatus(t, rec, 200)
	var resp struct {
		DeletedIDs []int64 `json:"deleted_ids"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.DeletedIDs) != 2 {
		t.Fatalf("expected 2 deleted, got %v", resp.DeletedIDs)
	}
	// Human comment must remain.
	remaining := ms.comments["gd-t1"]
	var hasHuman bool
	for _, c := range remaining {
		if c.Author == "alice" {
			hasHuman = true
		}
	}
	if !hasHuman {
		t.Fatalf("author filter did not protect human comment: %+v", remaining)
	}
}

func TestHandleTrimComments_NegativeKeepLastRejected(t *testing.T) {
	_, ms, h := newTestServerAuth()
	ms.beads["gd-t2"] = &model.Bead{ID: "gd-t2", Title: "x", Status: model.StatusOpen}
	rec := doJSONAs(t, h, "POST", "/v1/beads/gd-t2/comments/trim",
		map[string]any{"keep_last": -1}, "alice")
	requireStatus(t, rec, 400)
}

// TestHandleIncrementBeadFields_RejectsSQLInjectionKey is the HTTP-level
// regression test for AUDIT-2026-04-18 §C1. Any request with a field name
// that isn't a plain identifier must return 400 before touching the store.
func TestHandleIncrementBeadFields_RejectsSQLInjectionKey(t *testing.T) {
	for _, badKey := range []string{
		`x')); DROP TABLE beads; --`,
		`has space`,
		`has-dash`,
		`has.dot`,
		`1leading_digit`,
		``,
	} {
		t.Run(badKey, func(t *testing.T) {
			_, ms, h := newTestServer()
			ms.beads["gd-inc1"] = &model.Bead{ID: "gd-inc1", Title: "x", Status: model.StatusOpen}
			rec := doJSON(t, h, "POST", "/v1/beads/gd-inc1/increment", map[string]float64{badKey: 1})
			requireStatus(t, rec, http.StatusBadRequest)
		})
	}
}

// TestHandleIncrementBeadFields_AcceptsValidKey confirms the validator does
// not over-reject legitimate field names — a valid identifier must succeed
// and update the bead through the store path.
func TestHandleIncrementBeadFields_AcceptsValidKey(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-inc2"] = &model.Bead{ID: "gd-inc2", Title: "x", Status: model.StatusOpen}

	rec := doJSON(t, h, "POST", "/v1/beads/gd-inc2/increment", map[string]float64{"retry_count": 3})
	requireStatus(t, rec, http.StatusOK)
	var bead model.Bead
	decodeJSON(t, rec, &bead)
	if bead.ID != "gd-inc2" {
		t.Fatalf("expected bead id gd-inc2, got %q", bead.ID)
	}
	var fields map[string]float64
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("failed to parse fields: %v", err)
	}
	if fields["retry_count"] != 3 {
		t.Fatalf("expected retry_count=3, got %v", fields["retry_count"])
	}
}

// TestIsStreamingPath verifies the SSE allowlist matches only the actual
// long-lived endpoints — not snapshot endpoints that happen to contain
// "events" in the path. Regression guard for ka-0000002324: the previous
// substring matcher exempted /v1/events and /v1/beads/{id}/events from the
// 30s request timeout and rate limiter, allowing slow/abusive clients to pin response buffers in memory.
func TestIsStreamingPath(t *testing.T) {
	streaming := []string{
		"/v1/events/stream",
		"/v1/bus/events",
		"/v1/windows/self/live",
	}
	for _, p := range streaming {
		if !isStreamingPath(p) {
			t.Errorf("isStreamingPath(%q) = false, want true", p)
		}
	}

	notStreaming := []string{
		"/v1/events",
		"/v1/beads/gd-abc123/events",
		"/v1/beads",
		"/v1/health",
		"/v1/embeddings/sync-batch",
		"/v1/events/stream/extra", // sub-paths must not match
		"/v1/windows/self/view",   // the window's snapshot keeps the timeout
	}
	for _, p := range notStreaming {
		if isStreamingPath(p) {
			t.Errorf("isStreamingPath(%q) = true, want false (non-streaming endpoint must respect 30s timeout + rate limiter)", p)
		}
	}
}

// TestRequestTimeoutMiddlewareLongRunningBypass verifies that long-running
// admin paths receive a request context with no deadline, while regular
// non-streaming paths receive a deadline ≤30s. Regression guard for
// ka-0000001168.
func TestRequestTimeoutMiddlewareLongRunningBypass(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		wantHasLine bool // true if a context deadline must be set
	}{
		{"long-running-backfill-operators", "/v1/admin/backfill-operators", false},
		{"streaming-events-stream", "/v1/events/stream", false},
		{"regular-beads-list", "/v1/beads", true},
		{"regular-admin-revoke-user", "/v1/admin/revoke-user", true},
		{"regular-admin-reextract-references", "/v1/admin/reextract-references", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hasDeadline bool
			h := requestTimeoutMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, hasDeadline = r.Context().Deadline()
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if hasDeadline != tc.wantHasLine {
				t.Errorf("path %q: hasDeadline=%v, want %v", tc.path, hasDeadline, tc.wantHasLine)
			}
		})
	}
}

// TestIsLongRunningPath verifies the long-running admin allowlist matches
// only the inline whole-table backfill endpoints. Regression guard for
// ka-0000001168: prod-sized backfills exceeded the 30s request timeout and
// were cancelled mid-UPDATE; only the explicit allowlist must bypass the
// timeout — every other path (including other admin endpoints that finish
// quickly or are paginated client-side) must keep the 30s cap.
func TestIsLongRunningPath(t *testing.T) {
	longRunning := []string{
		"/v1/admin/backfill-operators",
	}
	for _, p := range longRunning {
		if !isLongRunningPath(p) {
			t.Errorf("isLongRunningPath(%q) = false, want true", p)
		}
	}

	notLongRunning := []string{
		"/v1/admin/revoke-user",
		"/v1/admin/revoke-token",
		"/v1/admin/reextract-references", // paginated client-side; per-page is bounded
		"/v1/admin/operators/slack-aliases",
		"/v1/admin/backfill-operators/extra", // sub-paths must not match
		"/v1/admin/backfill",                 // prefixes must not match
		"/v1/beads",
		"/v1/health",
		"/v1/events/stream",
	}
	for _, p := range notLongRunning {
		if isLongRunningPath(p) {
			t.Errorf("isLongRunningPath(%q) = true, want false (non-allowlisted endpoint must respect 30s timeout)", p)
		}
	}
}

// TestHandleListBeads_Omit covers the gd-zXdZq4Traf projection param: omit=
// maps onto the filter's Omit* flags, is off by default, and rejects an
// unknown column with a 400 instead of silently omitting nothing.
func TestHandleListBeads_Omit(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-a"] = &model.Bead{ID: "gd-a", Title: "a", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	requireStatus(t, doJSON(t, h, "GET", "/v1/beads", nil), 200)
	if ms.lastListFilter.OmitDescription || ms.lastListFilter.OmitNotes {
		t.Error("omit flags must default to false")
	}

	requireStatus(t, doJSON(t, h, "GET", "/v1/beads?omit=description,notes", nil), 200)
	if !ms.lastListFilter.OmitDescription || !ms.lastListFilter.OmitNotes {
		t.Errorf("omit=description,notes not mapped: %+v", ms.lastListFilter)
	}

	requireStatus(t, doJSON(t, h, "GET", "/v1/beads?omit=notes", nil), 200)
	if ms.lastListFilter.OmitDescription || !ms.lastListFilter.OmitNotes {
		t.Errorf("omit=notes must set only OmitNotes: %+v", ms.lastListFilter)
	}

	requireStatus(t, doJSON(t, h, "GET", "/v1/beads?omit=fields", nil), 400)
}

// TestHandleListBeads_OmitFields covers omit_fields= (gd-zXdZq4Traf): dotted
// paths reach the filter, default is empty, and a path that is not plain
// [A-Za-z0-9_-] segments is a 400 because it is inlined into the SELECT.
func TestHandleListBeads_OmitFields(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-a"] = &model.Bead{ID: "gd-a", Title: "a", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	requireStatus(t, doJSON(t, h, "GET", "/v1/beads", nil), 200)
	if len(ms.lastListFilter.OmitFields) != 0 {
		t.Errorf("OmitFields must default to empty, got %v", ms.lastListFilter.OmitFields)
	}

	requireStatus(t, doJSON(t, h, "GET", "/v1/beads?omit_fields=prompt,gardener.run_log", nil), 200)
	if got := ms.lastListFilter.OmitFields; len(got) != 2 || got[0] != "prompt" || got[1] != "gardener.run_log" {
		t.Errorf("omit_fields not mapped: %v", got)
	}

	for _, bad := range []string{"a%27b", "a..b", ".a", "a.b.c.d.e"} {
		requireStatus(t, doJSON(t, h, "GET", "/v1/beads?omit_fields="+bad, nil), 400)
	}
}

// TestHandleListBeads_FieldIn: field_in= reaches the filter (gd-OYKXtzN9kr).
func TestHandleListBeads_FieldIn(t *testing.T) {
	_, ms, h := newTestServer()
	requireStatus(t, doJSON(t, h, "GET", "/v1/beads?field_in=channel_id=c1,c2", nil), 200)
	if got := ms.lastListFilter.FieldsIn["channel_id"]; !slices.Equal(got, []string{"c1", "c2"}) {
		t.Errorf("field_in not mapped: %v", ms.lastListFilter.FieldsIn)
	}
}

// TestHandleListBeads_FieldsOnly covers fields_only= (gd-6PXLuR3juD): top-level
// keys reach the filter, default is empty, and a dotted or non-[A-Za-z0-9_-]
// key is a 400 because it is inlined into the SELECT.
func TestHandleListBeads_FieldsOnly(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-a"] = &model.Bead{ID: "gd-a", Title: "a", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	requireStatus(t, doJSON(t, h, "GET", "/v1/beads", nil), 200)
	if len(ms.lastListFilter.FieldsOnly) != 0 {
		t.Errorf("FieldsOnly must default to empty, got %v", ms.lastListFilter.FieldsOnly)
	}

	requireStatus(t, doJSON(t, h, "GET", "/v1/beads?fields_only=total_cost_usd,%20agent", nil), 200)
	if got := ms.lastListFilter.FieldsOnly; len(got) != 2 || got[0] != "total_cost_usd" || got[1] != "agent" {
		t.Errorf("fields_only not mapped: %v", got)
	}

	for _, bad := range []string{"a%27b", "a.b", "a%3Bb"} {
		requireStatus(t, doJSON(t, h, "GET", "/v1/beads?fields_only="+bad, nil), 400)
	}
}
