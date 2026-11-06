package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// mockStore is a minimal in-memory store for sync tests.
type mockStore struct {
	beads    map[string]*model.Bead
	configs  map[string]*model.Config
	labels   map[string][]string
	deps     map[string][]*model.Dependency
	comments map[string][]*model.Comment
	taints   map[string]*model.Taint
}

func newMockStore() *mockStore {
	return &mockStore{
		beads:    make(map[string]*model.Bead),
		configs:  make(map[string]*model.Config),
		labels:   make(map[string][]string),
		deps:     make(map[string][]*model.Dependency),
		comments: make(map[string][]*model.Comment),
	}
}

func (m *mockStore) CreateBead(_ context.Context, bead *model.Bead) error {
	m.beads[bead.ID] = bead
	return nil
}

func (m *mockStore) GetBead(_ context.Context, id string) (*model.Bead, error) {
	b, ok := m.beads[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return b, nil
}

func (m *mockStore) GetBeadIncludingDeleted(_ context.Context, id string) (*model.Bead, error) {
	b, ok := m.beads[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return b, nil
}

func (m *mockStore) FindAgentByName(_ context.Context, _ string, _ []model.Status) (*model.Bead, error) {
	return nil, sql.ErrNoRows
}

func (m *mockStore) FindAgentsByNames(_ context.Context, _ []string, _ []model.Status) ([]*model.Bead, error) {
	return nil, nil
}

func (m *mockStore) FindProjectByName(_ context.Context, _ string) (*model.Bead, error) {
	return nil, sql.ErrNoRows
}

func (m *mockStore) ListBeads(_ context.Context, _ model.BeadFilter) ([]*model.Bead, int, error) {
	var result []*model.Bead
	for _, b := range m.beads {
		result = append(result, b)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result, len(result), nil
}

func (m *mockStore) UpdateBead(_ context.Context, bead *model.Bead) error {
	m.beads[bead.ID] = bead
	return nil
}

func (m *mockStore) FillShortTitle(_ context.Context, bead *model.Bead) (bool, error) {
	b, ok := m.beads[bead.ID]
	if !ok || strings.TrimSpace(b.ShortTitle) != "" {
		return false, nil
	}
	b.ShortTitle = bead.ShortTitle
	return true, nil
}

func (m *mockStore) LockBeadAssignee(_ context.Context, id string) (string, error) {
	b, ok := m.beads[id]
	if !ok {
		return "", sql.ErrNoRows
	}
	return b.Assignee, nil
}

func (m *mockStore) MergeBeadFields(_ context.Context, id string, extra map[string]any) (*model.Bead, error) {
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
	return b, nil
}

func (m *mockStore) IncrementBeadFields(_ context.Context, id string, _ map[string]float64) (*model.Bead, error) {
	b := m.beads[id]
	return b, nil
}

func (m *mockStore) AllocateAgentHandleSeq(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

func (m *mockStore) AppendBeadArrayField(_ context.Context, id, _, _ string) (*model.Bead, bool, error) {
	return m.beads[id], false, nil
}

func (m *mockStore) InsertBeadArrayField(_ context.Context, id, _, _ string, _ int) (*model.Bead, bool, error) {
	return m.beads[id], false, nil
}

func (m *mockStore) CASBeadArrayField(_ context.Context, id, _ string, _, _ []string) (*model.Bead, error) {
	return m.beads[id], nil
}

func (m *mockStore) MergeBeadMapField(_ context.Context, id, _, _, _, _ string) (*model.Bead, bool, error) {
	return m.beads[id], false, nil
}

func (m *mockStore) EditBeadCSVField(_ context.Context, id, _, _, _ string) (*model.Bead, bool, error) {
	return m.beads[id], false, nil
}

func (m *mockStore) CloseBead(_ context.Context, _ string, _ string) (*model.Bead, error) {
	return nil, nil
}

func (m *mockStore) DeleteBead(_ context.Context, id string) error {
	delete(m.beads, id)
	return nil
}

func (m *mockStore) RestoreBead(_ context.Context, _ string) (*model.Bead, error) {
	return nil, nil
}

func (m *mockStore) PurgeBeads(_ context.Context, _ store.PurgeRequest) ([]store.PurgedBead, error) {
	return nil, nil
}

func (m *mockStore) TransferBead(_ context.Context, id string, toFleet string) (*model.Bead, string, error) {
	b, ok := m.beads[id]
	if !ok {
		return nil, "", fmt.Errorf("not found")
	}
	fromFleet := b.Fleet
	b.Fleet = toFleet
	return b, fromFleet, nil
}

func (m *mockStore) GetDependenciesForBeads(_ context.Context, _ []string) (map[string][]*model.Dependency, error) {
	return make(map[string][]*model.Dependency), nil
}

func (m *mockStore) GetReverseDependenciesForBeads(_ context.Context, _ []string) (map[string][]*model.Dependency, error) {
	return make(map[string][]*model.Dependency), nil
}

func (m *mockStore) GetDependencyCounts(_ context.Context, _ []string) (map[string]*model.DependencyCounts, error) {
	return make(map[string]*model.DependencyCounts), nil
}

func (m *mockStore) GetChildrenStatusHistogram(_ context.Context, _ []string) (map[string]map[string]int, error) {
	return make(map[string]map[string]int), nil
}

func (m *mockStore) FleetTypeStatusCounts(_ context.Context) (map[store.FleetTypeStatusKey]int, error) {
	return nil, nil
}

func (m *mockStore) FleetActiveAgentsByProject(_ context.Context) (map[string]int, error) {
	return nil, nil
}

func (m *mockStore) FleetAgentLifetimeRollup(_ context.Context) ([]store.FleetAgentProjectRollup, error) {
	return nil, nil
}

func (m *mockStore) FleetQueueDepthByProject(_ context.Context) (map[store.FleetQueueDepthKey]int, error) {
	return nil, nil
}

func (m *mockStore) FleetQueueOldestAgeByProject(_ context.Context) (map[store.FleetQueueDepthKey]float64, error) {
	return nil, nil
}

func (m *mockStore) ProjectLabelMissingCounts(_ context.Context, _ []string) (map[string]int, error) {
	return nil, nil
}

func (m *mockStore) BackfillProjectLabelsPage(_ context.Context, _ []string, _ int) (*store.BackfillProjectLabelsStats, error) {
	return &store.BackfillProjectLabelsStats{Done: true}, nil
}

func (m *mockStore) AcceptanceMetrics(_ context.Context) (*store.AcceptanceMetrics, error) {
	return &store.AcceptanceMetrics{}, nil
}

func (m *mockStore) GetLabelsForBeads(_ context.Context, _ []string) (map[string][]string, error) {
	return make(map[string][]string), nil
}

func (m *mockStore) GetBlockedByForBeads(_ context.Context, _ []string) (map[string][]string, error) {
	return make(map[string][]string), nil
}

func (m *mockStore) GetBeadsByIDs(_ context.Context, ids []string) ([]*model.Bead, error) {
	var result []*model.Bead
	for _, id := range ids {
		if b, ok := m.beads[id]; ok {
			result = append(result, b)
		}
	}
	return result, nil
}

func (m *mockStore) GetTextReferencesForBeads(_ context.Context, _ []string) ([]*model.BeadReference, error) {
	return nil, nil
}

func (m *mockStore) GetStats(_ context.Context) (*model.GraphStats, error) {
	return &model.GraphStats{}, nil
}

func (m *mockStore) AddDependency(_ context.Context, dep *model.Dependency) error {
	m.deps[dep.BeadID] = append(m.deps[dep.BeadID], dep)
	return nil
}

func (m *mockStore) RemoveDependency(_ context.Context, _ string, _ string, _ model.DependencyType) (bool, error) {
	return true, nil
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

func (m *mockStore) ListBeadsMentioning(_ context.Context, _ string, _, _ int, _ *string) ([]*model.BeadReferenceSummary, int, error) {
	return []*model.BeadReferenceSummary{}, 0, nil
}

func (m *mockStore) HasBundleChild(_ context.Context, _ string) (bool, error) {
	return false, nil
}

func (m *mockStore) ListBeadReferencesFrom(_ context.Context, _ string, _, _ int, _ *string) ([]*model.BeadReferenceSummary, int, error) {
	return []*model.BeadReferenceSummary{}, 0, nil
}

func (m *mockStore) AddLabel(_ context.Context, beadID string, label string) error {
	m.labels[beadID] = append(m.labels[beadID], label)
	return nil
}

func (m *mockStore) RemoveLabel(_ context.Context, _ string, _ string) (bool, error) {
	return true, nil
}

func (m *mockStore) GetLabels(_ context.Context, beadID string) ([]string, error) {
	return m.labels[beadID], nil
}

func (m *mockStore) GetTaintsForBead(_ context.Context, _ string) ([]int64, error) {
	return nil, nil
}

func (m *mockStore) GetTaintsForBeads(_ context.Context, _ []string) (map[string][]int64, error) {
	return nil, nil
}

func (m *mockStore) GetAllTaintsForBead(_ context.Context, _ string) ([]int64, error) {
	return nil, nil
}

func (m *mockStore) GetAllTaintsForBeads(_ context.Context, _ []string) (map[string][]int64, error) {
	return nil, nil
}

func (m *mockStore) GetGrantedTaintIDs(_ context.Context, _ string, _ []string) ([]int64, error) {
	return nil, nil
}

func (m *mockStore) GetTaintIDsOwnedByTeams(_ context.Context, _ []string) ([]int64, error) {
	return nil, nil
}

func (m *mockStore) HasAnyTaints(_ context.Context) (bool, error) {
	return len(m.taints) > 0, nil
}

func (m *mockStore) SetTaint(_ context.Context, t *model.Taint) error {
	if m.taints == nil {
		m.taints = make(map[string]*model.Taint)
	}
	m.taints[t.Name] = t
	return nil
}

func (m *mockStore) GetTaint(_ context.Context, name string) (*model.Taint, error) {
	t, ok := m.taints[name]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return t, nil
}

func (m *mockStore) ListTaints(_ context.Context) ([]*model.Taint, error) {
	var result []*model.Taint
	for _, t := range m.taints {
		result = append(result, t)
	}
	return result, nil
}

func (m *mockStore) GrantTaint(_ context.Context, _ *model.TaintGrant) error {
	return nil
}

func (m *mockStore) RevokeTaint(_ context.Context, _ int64, _, _ string) (bool, error) {
	return false, nil
}

func (m *mockStore) GetTaintGrant(_ context.Context, _ int64, _, _ string) (*model.TaintGrant, error) {
	return nil, sql.ErrNoRows
}

func (m *mockStore) ApplyTaint(_ context.Context, _ *model.BeadTaint) (bool, error) {
	return false, nil
}

func (m *mockStore) GetBeadTaints(_ context.Context, _ string) ([]*model.BeadTaint, error) {
	return nil, nil
}

func (m *mockStore) GetGateBeadTaints(_ context.Context, _ string) ([]*model.BeadTaint, error) {
	return nil, nil
}

func (m *mockStore) RecordTaintAudit(_ context.Context, _ *model.TaintAudit) error {
	return nil
}

func (m *mockStore) ScrubTaint(_ context.Context, _ *model.ScrubTaintRequest) (*model.ScrubTaintResult, error) {
	return &model.ScrubTaintResult{}, nil
}

func (m *mockStore) ListTaintGrants(_ context.Context, _ int64) ([]*model.TaintGrant, error) {
	return nil, nil
}

func (m *mockStore) ListTaintAudit(_ context.Context, _ int64, _ model.TaintAuditFilter) ([]*model.TaintAudit, int, error) {
	return nil, 0, nil
}

func (m *mockStore) GetBeadTaintsForTaint(_ context.Context, _ int64, _ []string) (map[string]*model.BeadTaint, error) {
	return nil, nil
}

func (m *mockStore) AddComment(_ context.Context, comment *model.Comment) error {
	m.comments[comment.BeadID] = append(m.comments[comment.BeadID], comment)
	return nil
}

func (m *mockStore) GetComments(_ context.Context, beadID string) ([]*model.Comment, error) {
	return m.comments[beadID], nil
}

func (m *mockStore) DeleteComment(_ context.Context, beadID string, commentID int64) (*model.Comment, error) {
	in := m.comments[beadID]
	for i, c := range in {
		if c.ID == commentID {
			m.comments[beadID] = append(in[:i], in[i+1:]...)
			return c, nil
		}
	}
	return nil, nil
}

func (m *mockStore) TrimComments(_ context.Context, beadID string, keepLast int, authorFilter string) ([]*model.Comment, error) {
	return nil, nil
}

func (m *mockStore) RecordEvent(_ context.Context, _ *model.Event) error {
	return nil
}

func (m *mockStore) GetEvents(_ context.Context, _ string) ([]*model.Event, error) {
	return nil, nil
}

func (m *mockStore) GetRecentEvents(_ context.Context, _ string, _ int) ([]*model.Event, error) {
	return nil, nil
}

func (m *mockStore) GetEventsByTopic(_ context.Context, _, _ string, _ int) ([]*model.Event, error) {
	return nil, nil
}

func (m *mockStore) EventHistogram(_ context.Context, _, _ time.Time, _ string, _ model.EventReadScope) ([]model.EventHistogramBucket, error) {
	return nil, nil
}

func (m *mockStore) GetEventsByTimeRange(_ context.Context, _, _ time.Time, _ int64, _ int, _ model.EventReadScope) ([]*model.Event, error) {
	return nil, nil
}

func (m *mockStore) GetEventsByTimeRangeDesc(_ context.Context, _, _ time.Time, _ int64, _ int, _ model.EventReadScope) ([]*model.Event, error) {
	return nil, nil
}

func (m *mockStore) LastEventAtByOperatorAliases(_ context.Context, _ []string) (map[string]time.Time, error) {
	return nil, nil
}

func (m *mockStore) GetOperatorHistory(_ context.Context, _ string) ([]*model.OperatorHistoryRow, error) {
	return nil, nil
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
	sort.Slice(result, func(i, j int) bool {
		return result[i].Key < result[j].Key
	})
	return result, nil
}

func (m *mockStore) DeleteConfig(_ context.Context, key string) error {
	delete(m.configs, key)
	return nil
}

func (m *mockStore) RunInTransaction(_ context.Context, fn func(tx store.Store) error) error {
	return fn(m)
}

func (m *mockStore) Ping(_ context.Context) error {
	return nil
}

func (m *mockStore) RecordUITelemetry(_ context.Context, _ *model.UITelemetryRow) error {
	return nil
}

func (m *mockStore) ListUITelemetry(_ context.Context, _ model.UITelemetryFilter) ([]*model.UITelemetryRow, error) {
	return nil, nil
}

func (m *mockStore) UpsertPushDevice(_ context.Context, _ *model.PushDevice) error {
	return nil
}

func (m *mockStore) ListPushDevices(_ context.Context, _ model.PushDeviceFilter) ([]*model.PushDevice, error) {
	return nil, nil
}

func (m *mockStore) DeletePushDevice(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func (m *mockStore) InsertSlowToolCall(_ context.Context, _ *model.SlowToolCallRow) error {
	return nil
}

func (m *mockStore) ListSlowToolCalls(_ context.Context, _ model.SlowToolCallFilter) ([]*model.SlowToolCallRow, error) {
	return nil, nil
}

func (m *mockStore) DeleteSlowToolCallsBefore(_ context.Context, _ time.Time, _ int) (int64, error) {
	return 0, nil
}

func (m *mockStore) DeleteBeadUpdatedEventsBefore(_ context.Context, _ []string, _ time.Time, _ int) (int64, error) {
	return 0, nil
}

func (m *mockStore) DeleteFieldsMutationsBefore(_ context.Context, _ []string, _ time.Time, _ int) (int64, error) {
	return 0, nil
}

func (m *mockStore) AddFollower(_ context.Context, _ string, _ int64, _ *int64, _ string) error {
	return nil
}

func (m *mockStore) RemoveFollower(_ context.Context, _ string, _ int64) (bool, error) {
	return false, nil
}

func (m *mockStore) ListFollowers(_ context.Context, _ string) ([]*model.BeadFollower, error) {
	return nil, nil
}

func (m *mockStore) ListFollowedBeadIDs(_ context.Context, _ int64, _ int) ([]string, error) {
	return nil, nil
}

func (m *mockStore) Close() error {
	return nil
}

func (m *mockStore) InsertRevokedToken(_ context.Context, _ *model.RevokedToken) error { return nil }

func (m *mockStore) ListRevokedTokenHashes(_ context.Context) ([]string, error) { return nil, nil }

func (m *mockStore) InsertRefreshToken(_ context.Context, _ *model.RefreshToken) error { return nil }

func (m *mockStore) GetRefreshToken(_ context.Context, _ string) (*model.RefreshToken, error) {
	return nil, sql.ErrNoRows
}

func (m *mockStore) RevokeRefreshTokenChain(_ context.Context, _ string, _ time.Time) (int, error) {
	return 0, nil
}

func (m *mockStore) ListActiveRefreshTokenHashesByFleet(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockStore) InsertRunnerToken(_ context.Context, _ *model.RunnerToken) error { return nil }

func (m *mockStore) GetRunnerTokenByHash(_ context.Context, _ string) (*model.RunnerToken, error) {
	return nil, nil
}

func (m *mockStore) RevokeRunnerTokens(_ context.Context, _ string, _ time.Time) (int, error) {
	return 0, nil
}

func (m *mockStore) RecordMutation(_ context.Context, _ *model.MutationRow) error { return nil }

func (m *mockStore) ListMutations(_ context.Context, _ string) ([]*model.MutationRow, error) {
	return nil, nil
}

func (m *mockStore) ReextractReferencesPage(_ context.Context, _ string, _ int, _ string) (*store.ReextractReferencesStats, error) {
	return &store.ReextractReferencesStats{Done: true}, nil
}

func (m *mockStore) BackfillSearchTSVPage(_ context.Context, _ string, _ int, _ bool) (*store.BackfillSearchTSVStats, error) {
	return &store.BackfillSearchTSVStats{Done: true}, nil
}

func (m *mockStore) BackfillCategoryPage(_ context.Context, _ string, _ int) (*store.BackfillCategoryStats, error) {
	return &store.BackfillCategoryStats{Done: true}, nil
}

func (m *mockStore) BackfillOperators(_ context.Context, _ store.BackfillOperatorsOptions) (*store.BackfillOperatorsStats, error) {
	return &store.BackfillOperatorsStats{}, nil
}

func (m *mockStore) BackfillOperatedByLegacy(_ context.Context, _ store.BackfillOperatedByLegacyOptions) (*store.BackfillOperatedByLegacyStats, error) {
	return &store.BackfillOperatedByLegacyStats{}, nil
}

func (m *mockStore) BackfillSlackUserID(_ context.Context, _ store.BackfillSlackUserIDOptions) (*store.BackfillSlackUserIDStats, error) {
	return &store.BackfillSlackUserIDStats{}, nil
}

func (m *mockStore) BackfillOperatorSlugs(_ context.Context, _ store.BackfillOperatorSlugsOptions) (*store.BackfillOperatorSlugsStats, error) {
	return &store.BackfillOperatorSlugsStats{}, nil
}

func (m *mockStore) BackfillOwner(_ context.Context, _ store.BackfillOwnerOptions) (*store.BackfillOwnerStats, error) {
	return &store.BackfillOwnerStats{}, nil
}

func (m *mockStore) BackfillEmptyOwner(_ context.Context, _ store.BackfillEmptyOwnerOptions) (*store.BackfillEmptyOwnerStats, error) {
	return &store.BackfillEmptyOwnerStats{}, nil
}

func (m *mockStore) BackfillOperatedByAgents(_ context.Context, _ store.BackfillOperatedByAgentsOptions) (*store.BackfillOperatedByAgentsStats, error) {
	return &store.BackfillOperatedByAgentsStats{}, nil
}

func (m *mockStore) BackfillParentChildDeps(_ context.Context, _ store.BackfillParentChildDepsOptions) (*store.BackfillParentChildDepsStats, error) {
	return &store.BackfillParentChildDepsStats{}, nil
}

func (m *mockStore) RepairReversedParentChildDeps(_ context.Context, _ store.RepairReversedParentChildDepsOptions) (*store.RepairReversedParentChildDepsStats, error) {
	return &store.RepairReversedParentChildDepsStats{}, nil
}

func (m *mockStore) BackfillTriggeredByDeps(_ context.Context, _ store.BackfillTriggeredByDepsOptions) (*store.BackfillTriggeredByDepsStats, error) {
	return &store.BackfillTriggeredByDepsStats{}, nil
}

func (m *mockStore) CloseTriggerAuditBeads(_ context.Context, _ store.CloseTriggerAuditBeadsOptions) (*store.CloseTriggerAuditBeadsStats, error) {
	return &store.CloseTriggerAuditBeadsStats{}, nil
}

func (m *mockStore) GetMutation(_ context.Context, _ int64) (*model.MutationRow, error) {
	return nil, nil
}

func (m *mockStore) GetBeadOperator(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (m *mockStore) UpdateBeadOperator(_ context.Context, _ string, _ int64) error {
	return nil
}

func (m *mockStore) ClaimEscalationReplay(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func (m *mockStore) ReleaseEscalationReplayClaim(_ context.Context, _ string) error {
	return nil
}

func (m *mockStore) ClaimEscalationExecution(_ context.Context, _, dispatch string) (bool, string, error) {
	return true, dispatch, nil
}

func (m *mockStore) ClaimScrubExecution(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func (m *mockStore) ReleaseScrubExecutionClaim(_ context.Context, _ string) error {
	return nil
}

func (m *mockStore) CASBeadStatus(_ context.Context, _ string, _, _ model.Status) (bool, error) {
	return true, nil
}

func (m *mockStore) TryAdvisoryLock(ctx context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	if err := fn(ctx); err != nil {
		return true, err
	}
	return true, nil
}

func (m *mockStore) UpsertOperator(_ context.Context, _, _ string) (int64, error) {
	return 0, nil
}

func (m *mockStore) LookupOperatorAlias(_ context.Context, _, _ string) (int64, error) {
	return 0, sql.ErrNoRows
}

func (m *mockStore) LookupOperatorByAlias(_ context.Context, _, _ string) (int64, string, error) {
	return 0, "", sql.ErrNoRows
}

func (m *mockStore) GetOperatorBySlug(_ context.Context, _ string) (int64, error) {
	return 0, sql.ErrNoRows
}

func (m *mockStore) GetOperatorSlug(_ context.Context, _ int64) (string, error) {
	return "", sql.ErrNoRows
}

func (m *mockStore) UpsertOperatorAlias(_ context.Context, _ int64, _, _, _ string) error {
	return nil
}

func (m *mockStore) PruneOperatorAliases(_ context.Context, _ string, _ []store.OperatorAliasKey) ([]store.OperatorAliasKey, error) {
	return nil, nil
}

func (m *mockStore) ListOperatorAliasesByKind(_ context.Context, _ string) ([]store.OperatorAliasIdentity, error) {
	return nil, nil
}

func (m *mockStore) ListOperatorsWithSlugs(_ context.Context, _ int) ([]store.OperatorDirectoryEntry, error) {
	return nil, nil
}

func (m *mockStore) OperatorDirectoryStats(_ context.Context, _, _, _ int) (*store.OperatorDirectoryStats, error) {
	return &store.OperatorDirectoryStats{}, nil
}

func (m *mockStore) ResolveOperators(_ context.Context, _ []int64) (map[int64]store.ResolvedOperator, error) {
	return map[int64]store.ResolvedOperator{}, nil
}

func (m *mockStore) UpdateOperatorIdentity(_ context.Context, _ int64, _, _ string) (bool, error) {
	return false, nil
}

func (m *mockStore) MergeOperators(_ context.Context, _, _ int64) (int64, error) {
	return 0, nil
}

func (m *mockStore) ForgetOperator(_ context.Context, id int64, _ store.ForgetOperatorOptions) (*store.ForgetOperatorResult, error) {
	return &store.ForgetOperatorResult{ID: id}, nil
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

func (m *mockStore) UpsertEmbedding(_ context.Context, _ string, _ []float32, _, _ string) error {
	return nil
}

func (m *mockStore) GetEmbedding(_ context.Context, _ string) ([]float32, string, error) {
	return nil, "", nil
}

func (m *mockStore) GetEmbeddingHashes(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (m *mockStore) ListBeadsNeedingEmbedding(_ context.Context, _ string, _, _ int) ([]*model.Bead, int, error) {
	return nil, 0, nil
}

func (m *mockStore) RefreshEmbeddingTimestamps(_ context.Context, _ []string) (int64, error) {
	return 0, nil
}

func (m *mockStore) SemanticSearch(_ context.Context, _ []float32, _ model.BeadFilter, _ int) ([]*model.SemanticResult, error) {
	return nil, nil
}

func (m *mockStore) DeleteEmbedding(_ context.Context, _ string) error {
	return nil
}

func (m *mockStore) GetEmbeddingStats(_ context.Context) (*model.EmbeddingStats, error) {
	return &model.EmbeddingStats{}, nil
}

func (m *mockStore) MigrateEmbeddingsSchema(_ context.Context, _ string, _ int, _, _ string) error {
	return nil
}

func (m *mockStore) RecreateEmbeddingIndex(_ context.Context) error {
	return nil
}
