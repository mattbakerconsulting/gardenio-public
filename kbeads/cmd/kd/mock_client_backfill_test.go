package main

import (
	"context"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/embedding"
	"gardenio/kbeads/internal/model"
)

func (m *mockBeadsClient) BackfillSearchTSV(_ context.Context, req *client.BackfillSearchTSVRequest) (*client.BackfillSearchTSVResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BackfillSearchTSVCalls = append(m.BackfillSearchTSVCalls, req)
	if m.BackfillSearchTSVErr != nil {
		return nil, m.BackfillSearchTSVErr
	}
	idx := len(m.BackfillSearchTSVCalls) - 1
	if idx < len(m.BackfillSearchTSVPages) {
		return m.BackfillSearchTSVPages[idx], nil
	}
	return &client.BackfillSearchTSVResponse{Done: true}, nil
}

func (m *mockBeadsClient) SetStewardDelegation(_ context.Context, land string, ops []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SetStewardDelegationCalls = append(m.SetStewardDelegationCalls, land+"="+strings.Join(ops, ","))
	return nil
}

func (m *mockBeadsClient) BackfillCategory(_ context.Context, req *client.BackfillCategoryRequest) (*client.BackfillCategoryResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BackfillCategoryCalls = append(m.BackfillCategoryCalls, req)
	if m.BackfillCategoryErr != nil {
		return nil, m.BackfillCategoryErr
	}
	idx := len(m.BackfillCategoryCalls) - 1
	if idx < len(m.BackfillCategoryPages) {
		return m.BackfillCategoryPages[idx], nil
	}
	return &client.BackfillCategoryResponse{Done: true}, nil
}

func (m *mockBeadsClient) BackfillProjectLabels(_ context.Context, req *client.BackfillProjectLabelsRequest) (*client.BackfillProjectLabelsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BackfillProjectLabelsCalls = append(m.BackfillProjectLabelsCalls, req)
	if idx := len(m.BackfillProjectLabelsCalls) - 1; idx < len(m.BackfillProjectLabelsPages) {
		return m.BackfillProjectLabelsPages[idx], nil
	}
	return &client.BackfillProjectLabelsResponse{Labelled: map[string]int{}, Done: true}, nil
}

func (m *mockBeadsClient) BackfillOperators(_ context.Context, req *client.BackfillOperatorsRequest) (*client.BackfillOperatorsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BackfillOperatorsCalls = append(m.BackfillOperatorsCalls, req)
	if m.BackfillOperatorsErr != nil {
		return nil, m.BackfillOperatorsErr
	}
	if m.BackfillOperatorsResp != nil {
		return m.BackfillOperatorsResp, nil
	}
	return &client.BackfillOperatorsResponse{DryRun: req.DryRun}, nil
}

func (m *mockBeadsClient) BackfillOperatedByLegacy(_ context.Context, req *client.BackfillOperatedByLegacyRequest) (*client.BackfillOperatedByLegacyResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BackfillOperatedByLegacyCalls = append(m.BackfillOperatedByLegacyCalls, req)
	if m.BackfillOperatedByLegacyErr != nil {
		return nil, m.BackfillOperatedByLegacyErr
	}
	if m.BackfillOperatedByLegacyResp != nil {
		return m.BackfillOperatedByLegacyResp, nil
	}
	return &client.BackfillOperatedByLegacyResponse{DryRun: req.DryRun, Target: req.Target}, nil
}

func (m *mockBeadsClient) BackfillSlackUserID(_ context.Context, req *client.BackfillSlackUserIDRequest) (*client.BackfillSlackUserIDResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BackfillSlackUserIDCalls = append(m.BackfillSlackUserIDCalls, req)
	if m.BackfillSlackUserIDErr != nil {
		return nil, m.BackfillSlackUserIDErr
	}
	if m.BackfillSlackUserIDResp != nil {
		return m.BackfillSlackUserIDResp, nil
	}
	return &client.BackfillSlackUserIDResponse{DryRun: req.DryRun}, nil
}

func (m *mockBeadsClient) BackfillOperatorSlugs(_ context.Context, req *client.BackfillOperatorSlugsRequest) (*client.BackfillOperatorSlugsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BackfillOperatorSlugsCalls = append(m.BackfillOperatorSlugsCalls, req)
	if m.BackfillOperatorSlugsErr != nil {
		return nil, m.BackfillOperatorSlugsErr
	}
	if m.BackfillOperatorSlugsResp != nil {
		return m.BackfillOperatorSlugsResp, nil
	}
	return &client.BackfillOperatorSlugsResponse{DryRun: req.DryRun}, nil
}

// forgetOperatorMockCall records one ForgetOperator invocation on the mock.
type forgetOperatorMockCall struct {
	id  int64
	req *client.ForgetOperatorRequest
}

func (m *mockBeadsClient) ForgetOperator(_ context.Context, id int64, req *client.ForgetOperatorRequest) (*client.ForgetOperatorResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ForgetOperatorCalls = append(m.ForgetOperatorCalls, forgetOperatorMockCall{id: id, req: req})
	if m.ForgetOperatorErr != nil {
		return nil, m.ForgetOperatorErr
	}
	if m.ForgetOperatorResp != nil {
		return m.ForgetOperatorResp, nil
	}
	return &client.ForgetOperatorResponse{OperatorID: id}, nil
}

func (m *mockBeadsClient) BackfillOwner(_ context.Context, req *client.BackfillOwnerRequest) (*client.BackfillOwnerResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BackfillOwnerCalls = append(m.BackfillOwnerCalls, req)
	if m.BackfillOwnerErr != nil {
		return nil, m.BackfillOwnerErr
	}
	if m.BackfillOwnerResp != nil {
		return m.BackfillOwnerResp, nil
	}
	return &client.BackfillOwnerResponse{DryRun: req.DryRun, Target: req.Target}, nil
}

func (m *mockBeadsClient) BackfillEmptyOwner(_ context.Context, req *client.BackfillEmptyOwnerRequest) (*client.BackfillEmptyOwnerResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BackfillEmptyOwnerCalls = append(m.BackfillEmptyOwnerCalls, req)
	if m.BackfillEmptyOwnerErr != nil {
		return nil, m.BackfillEmptyOwnerErr
	}
	if m.BackfillEmptyOwnerResp != nil {
		return m.BackfillEmptyOwnerResp, nil
	}
	return &client.BackfillEmptyOwnerResponse{DryRun: req.DryRun}, nil
}

func (m *mockBeadsClient) BackfillOperatedByAgents(_ context.Context, req *client.BackfillOperatedByAgentsRequest) (*client.BackfillOperatedByAgentsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BackfillOperatedByAgentsCalls = append(m.BackfillOperatedByAgentsCalls, req)
	if m.BackfillOperatedByAgentsErr != nil {
		return nil, m.BackfillOperatedByAgentsErr
	}
	if m.BackfillOperatedByAgentsResp != nil {
		return m.BackfillOperatedByAgentsResp, nil
	}
	return &client.BackfillOperatedByAgentsResponse{DryRun: req.DryRun}, nil
}

func (m *mockBeadsClient) BackfillParentChildDeps(_ context.Context, req *client.BackfillParentChildDepsRequest) (*client.BackfillParentChildDepsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BackfillParentChildDepsCalls = append(m.BackfillParentChildDepsCalls, req)
	if m.BackfillParentChildDepsErr != nil {
		return nil, m.BackfillParentChildDepsErr
	}
	if m.BackfillParentChildDepsResp != nil {
		return m.BackfillParentChildDepsResp, nil
	}
	return &client.BackfillParentChildDepsResponse{DryRun: req.DryRun}, nil
}

func (m *mockBeadsClient) RepairReversedParentChildDeps(_ context.Context, req *client.RepairReversedParentChildDepsRequest) (*client.RepairReversedParentChildDepsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RepairReversedParentChildDepsCalls = append(m.RepairReversedParentChildDepsCalls, req)
	if m.RepairReversedParentChildDepsErr != nil {
		return nil, m.RepairReversedParentChildDepsErr
	}
	if m.RepairReversedParentChildDepsResp != nil {
		return m.RepairReversedParentChildDepsResp, nil
	}
	return &client.RepairReversedParentChildDepsResponse{DryRun: req.DryRun}, nil
}

func (m *mockBeadsClient) BackfillTriggeredByDeps(_ context.Context, req *client.BackfillTriggeredByDepsRequest) (*client.BackfillTriggeredByDepsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BackfillTriggeredByDepsCalls = append(m.BackfillTriggeredByDepsCalls, req)
	if m.BackfillTriggeredByDepsErr != nil {
		return nil, m.BackfillTriggeredByDepsErr
	}
	if m.BackfillTriggeredByDepsResp != nil {
		return m.BackfillTriggeredByDepsResp, nil
	}
	return &client.BackfillTriggeredByDepsResponse{DryRun: req.DryRun}, nil
}

func (m *mockBeadsClient) AuditBlocksCycles(_ context.Context) (*client.AuditBlocksCyclesResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.AuditBlocksCyclesCalls++
	if m.AuditBlocksCyclesErr != nil {
		return nil, m.AuditBlocksCyclesErr
	}
	if m.AuditBlocksCyclesResp != nil {
		return m.AuditBlocksCyclesResp, nil
	}
	return &client.AuditBlocksCyclesResponse{}, nil
}

func (m *mockBeadsClient) CloseTriggerAuditBeads(_ context.Context, req *client.CloseTriggerAuditBeadsRequest) (*client.CloseTriggerAuditBeadsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CloseTriggerAuditBeadsCalls = append(m.CloseTriggerAuditBeadsCalls, req)
	if m.CloseTriggerAuditBeadsErr != nil {
		return nil, m.CloseTriggerAuditBeadsErr
	}
	if m.CloseTriggerAuditBeadsResp != nil {
		return m.CloseTriggerAuditBeadsResp, nil
	}
	return &client.CloseTriggerAuditBeadsResponse{DryRun: req.DryRun}, nil
}

// SyncBatchEmbeddings simulates the server-side embed loop. It paginates
// ListBeadsResult using req.Offset/Limit, consults EmbeddingHashes to skip
// unchanged beads (unless req.Force is set), records a synthetic
// UpsertEmbeddingRequest per embedded bead, and returns per-page counts.
// The synthetic upsert rows match the real server shape closely enough for
// the CLI tests that previously asserted on them directly.
func (m *mockBeadsClient) SyncBatchEmbeddings(_ context.Context, req *client.SyncBatchEmbeddingsRequest) (*client.SyncBatchEmbeddingsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SyncBatchCalls = append(m.SyncBatchCalls, req)
	if m.SyncBatchErr != nil {
		return nil, m.SyncBatchErr
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := max(req.Offset, 0)
	// Source list: when OnlyMissing is set, the server SQL-pre-filters to
	// beads with no embedding row. Mirror that here by excluding any bead
	// whose ID is already in EmbeddingHashes (treated as "already embedded
	// with matching hash" in this mock — see the upsert path below that
	// records an EmbeddingHashes entry on each successful embed).
	src := m.ListBeadsResult
	if req.OnlyMissing {
		filtered := make([]*model.Bead, 0, len(src))
		for _, b := range src {
			if b == nil {
				continue
			}
			text := embedding.BeadText(b)
			hash := embedding.TextHash(text)
			if h, ok := m.EmbeddingHashes[b.ID]; ok && h == hash {
				continue
			}
			filtered = append(filtered, b)
		}
		src = filtered
	}
	var page []*model.Bead
	if offset < len(src) {
		end := min(offset+limit, len(src))
		page = src[offset:end]
	}
	modelName := m.SyncBatchModel
	if modelName == "" {
		modelName = "test-model"
	}
	// Apply server-side type filter (Types/ExcludeTypes) before recording
	// hash lookups — mirrors the real handler.
	allowedTypes := mockTypeSet(req.Types)
	excludedTypes := mockTypeSet(req.ExcludeTypes)
	candidates := make([]*model.Bead, 0, len(page))
	filteredType := 0
	for _, b := range page {
		if b == nil {
			continue
		}
		if !mockTypeAllowed(string(b.Type), allowedTypes, excludedTypes) {
			filteredType++
			continue
		}
		candidates = append(candidates, b)
	}

	embedded, skipped := 0, 0
	// Lazily record that GetEmbeddingHashes would have been called server-side
	// for the hash check; tests that previously asserted GetEmbeddingHashesCalls
	// keyed this array off the CLI call, so we mirror that here when !Force.
	if !req.Force && len(candidates) > 0 {
		ids := make([]string, 0, len(candidates))
		for _, b := range candidates {
			ids = append(ids, b.ID)
		}
		m.GetEmbeddingHashesCalls = append(m.GetEmbeddingHashesCalls, ids)
	}
	for i, b := range candidates {
		text := embedding.BeadText(b)
		hash := embedding.TextHash(text)
		if !req.Force {
			if h, ok := m.EmbeddingHashes[b.ID]; ok && h == hash {
				skipped++
				continue
			}
		}
		// Synthetic vector: (i, i+0.1, i+0.2, ...). Matches the old
		// in-CLI fake embedder's shape closely enough for assertions.
		vec := make([]float32, 3)
		for j := range vec {
			vec[j] = float32(i) + float32(j)/10
		}
		m.UpsertEmbeddingCalls = append(m.UpsertEmbeddingCalls, &client.UpsertEmbeddingRequest{
			BeadID:    b.ID,
			Embedding: vec,
			Model:     modelName,
			TextHash:  hash,
		})
		if m.EmbeddingHashes == nil {
			m.EmbeddingHashes = map[string]string{}
		}
		m.EmbeddingHashes[b.ID] = hash
		embedded++
	}
	totalRet := len(m.ListBeadsResult)
	if req.OnlyMissing {
		totalRet = len(src)
	}
	return &client.SyncBatchEmbeddingsResponse{
		Total:        totalRet,
		PageCount:    len(page),
		Embedded:     embedded,
		Skipped:      skipped,
		FilteredType: filteredType,
		Model:        modelName,
	}, nil
}

func mockTypeSet(types []string) map[string]struct{} {
	if len(types) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(types))
	for _, t := range types {
		if t == "" {
			continue
		}
		out[t] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mockTypeAllowed(beadType string, allowed, excluded map[string]struct{}) bool {
	if _, blocked := excluded[beadType]; blocked {
		return false
	}
	if len(allowed) == 0 {
		return true
	}
	_, ok := allowed[beadType]
	return ok
}
