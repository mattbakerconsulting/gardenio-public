package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

// mockBeadsClient implements client.BeadsClient for testing CLI commands.
type mockBeadsClient struct {
	mu sync.Mutex

	// Call tracking.
	CreateBeadCalls []*client.CreateBeadRequest
	UpdateBeadCalls []struct {
		ID  string
		Req *client.UpdateBeadRequest
	}
	EditSubFieldCalls []struct {
		ID    string
		Field string
		Req   *client.EditSubFieldRequest
	}
	GetBeadCalls       []string
	BatchGetCalls      [][]string
	BatchGetErr        error
	AddDepCalls        []*client.AddDependencyRequest
	AddDepForceParent  []bool // per AddDependency call: was X-Force-Parent-Edge set on the ctx?
	RemoveDepCalls     []struct{ BeadID, DependsOnID, Type string }
	CloseBeadCalls     []struct{ ID, ClosedBy string }
	DeleteBeadCalls    []string
	RestoreBeadCalls   []string
	AddCommentCalls    []struct{ BeadID, Author, Text string }
	DeleteCommentCalls []struct {
		BeadID    string
		CommentID int64
	}
	DeleteCommentResult bool
	DeleteCommentErr    error
	TrimCommentsCalls   []struct {
		BeadID   string
		KeepLast int
		Author   string
	}
	TrimCommentsResult []int64
	TrimCommentsErr    error
	GetRevDepCalls     []string
	AddLabelCalls      []struct{ BeadID, Label string }
	RemoveLabelCalls   []struct{ BeadID, Label string }
	MentionedByCalls   []string
	MentionsCalls      []string
	// Last-seen relationshipType filter. T6 — kd refs --type=<verb> surface.
	// nil = no filter; non-nil "" = NULL only; non-nil verb = exact match.
	MentionedByRelType *string
	MentionsRelType    *string
	ListBeadsCalls     []*client.ListBeadsRequest
	FindSimilarCalls   []struct {
		BeadID   string
		Limit    int
		MinScore float64
	}
	SemanticSearchCalls []*client.SemanticSearchRequest

	// LookupOperatorByAliasCalls tracks every (kind, subject) lookup. When
	// LookupOperatorByAliasFn is set it computes the response; otherwise the
	// mock consults OperatorAliases (keyed "kind:subject") and reports
	// Found=false for a miss.
	LookupOperatorByAliasCalls []struct{ Kind, Subject string }
	LookupOperatorByAliasFn    func(kind, subject string) (*client.LookupOperatorByAliasResponse, error)
	OperatorAliases            map[string]int64 // "kind:subject" -> operator id
	LookupOperatorByAliasErr   error

	// Configurable return values.
	Beads           map[string]*model.Bead // keyed by ID
	ListBeadsResult []*model.Bead          // if set, ListBeads returns these
	// ListBeadsPages, when non-nil, drives multi-page ListBeads responses:
	// the i-th call returns ListBeadsPages[i]. Falls through to
	// ListBeadsResult once exhausted (or after the final empty page).
	ListBeadsPages [][]*model.Bead
	// ListBeadsTotalOverride, when > 0, is returned as Total for the
	// ListBeadsResult path instead of len(ListBeadsResult). It lets a test
	// simulate a --limit-capped page (a small page out of a larger total) so
	// the --json-envelope has_more/total signal can be exercised.
	ListBeadsTotalOverride int
	Deps                   map[string][]*model.Dependency
	RevDeps                map[string][]*model.Dependency
	// Labels, when populated for a bead, lets RemoveLabel model the true
	// 0-match vs 1-match paths (ka-0000000424). A bead absent from the map
	// falls through to assume-success, matching the daemon's posture for
	// callers that don't model label state.
	Labels      map[string][]string
	MentionedBy map[string][]*model.BeadReferenceSummary
	Mentions    map[string][]*model.BeadReferenceSummary
	CreateIDs   []string // sequential IDs for CreateBead calls
	createIndex int

	// AddDepFn, when set, computes the returned dependency from the request
	// endpoints — used to simulate the server's parent-child auto-canonicalize
	// flip (ka-0000006880), where the STORED orientation differs from the
	// requested one. When nil, AddDependency echoes the request verbatim.
	AddDepFn func(reqBeadID, reqDependsOnID, depType string) *model.Dependency

	// Error injection.
	CreateErr      error
	GetErr         error
	UpdateErr      error
	AddDepErr      error
	CloseErr       error
	DeleteErr      error
	RestoreErr     error
	CommentErr     error
	GetCommentsErr error
	// CommentsByBead, when non-nil, drives GetComments responses keyed by bead ID.
	CommentsByBead  map[string][]*model.Comment
	RevDepErr       error
	MentionedByErr  error
	MentionsErr     error
	ListErr         error
	FindSimilarErr  error
	FindSimilarResp *client.FindSimilarResponse

	SemanticSearchErr  error
	SemanticSearchResp *client.SemanticSearchResponse

	UpsertEmbeddingCalls    []*client.UpsertEmbeddingRequest
	UpsertEmbeddingErr      error
	GetEmbeddingHashesCalls [][]string
	GetEmbeddingHashesErr   error
	EmbeddingHashes         map[string]string

	MigrateEmbeddingsCalls []*client.MigrateEmbeddingsRequest
	MigrateEmbeddingsErr   error
	ReindexEmbeddingsCalls int
	ReindexEmbeddingsErr   error

	ReextractReferencesCalls []*client.ReextractReferencesRequest
	ReextractReferencesErr   error
	// ReextractReferencesPages, if non-nil, is consulted by the mock to
	// drive a multi-page backfill. Each entry is the response for the
	// matching call (1-indexed). When exhausted, the mock returns Done=true.
	ReextractReferencesPages []*client.ReextractReferencesResponse
	// ReextractReferencesFn, if non-nil, takes full control of each call —
	// used to exercise the CLI's per-page auto-halving retry, which needs a
	// response that varies with the request's BatchSize rather than a fixed
	// scripted sequence. Takes precedence over Err/Pages.
	ReextractReferencesFn func(req *client.ReextractReferencesRequest) (*client.ReextractReferencesResponse, error)

	BackfillSearchTSVCalls []*client.BackfillSearchTSVRequest
	BackfillSearchTSVErr   error
	// BackfillSearchTSVPages, if non-nil, drives a multi-page search_tsv
	// backfill: each entry is the response for the matching call (1-indexed).
	// When exhausted, the mock returns Done=true.
	BackfillSearchTSVPages []*client.BackfillSearchTSVResponse

	BackfillCategoryCalls []*client.BackfillCategoryRequest
	// SetStewardDelegationCalls records each land's delegation write, as
	// "land=op,op".
	SetStewardDelegationCalls []string
	BackfillCategoryErr       error
	// BackfillCategoryPages, if non-nil, drives a multi-page category backfill:
	// each entry is the response for the matching call (1-indexed). When
	// exhausted, the mock returns Done=true.
	BackfillCategoryPages []*client.BackfillCategoryResponse

	// BackfillProjectLabelsPages answers the Nth BackfillProjectLabels call;
	// past the end the backfill is done.
	BackfillProjectLabelsCalls []*client.BackfillProjectLabelsRequest
	BackfillProjectLabelsPages []*client.BackfillProjectLabelsResponse

	BackfillOperatorsCalls []*client.BackfillOperatorsRequest
	BackfillOperatorsErr   error
	BackfillOperatorsResp  *client.BackfillOperatorsResponse

	BackfillOperatedByLegacyCalls []*client.BackfillOperatedByLegacyRequest
	BackfillOperatedByLegacyErr   error
	BackfillOperatedByLegacyResp  *client.BackfillOperatedByLegacyResponse

	BackfillSlackUserIDCalls []*client.BackfillSlackUserIDRequest
	BackfillSlackUserIDErr   error
	BackfillSlackUserIDResp  *client.BackfillSlackUserIDResponse

	BackfillOperatorSlugsCalls []*client.BackfillOperatorSlugsRequest
	BackfillOperatorSlugsErr   error
	BackfillOperatorSlugsResp  *client.BackfillOperatorSlugsResponse

	ForgetOperatorCalls []forgetOperatorMockCall
	ForgetOperatorErr   error
	ForgetOperatorResp  *client.ForgetOperatorResponse

	BackfillOwnerCalls []*client.BackfillOwnerRequest
	BackfillOwnerErr   error
	BackfillOwnerResp  *client.BackfillOwnerResponse

	BackfillEmptyOwnerCalls []*client.BackfillEmptyOwnerRequest
	BackfillEmptyOwnerErr   error
	BackfillEmptyOwnerResp  *client.BackfillEmptyOwnerResponse

	BackfillOperatedByAgentsCalls []*client.BackfillOperatedByAgentsRequest
	BackfillOperatedByAgentsErr   error
	BackfillOperatedByAgentsResp  *client.BackfillOperatedByAgentsResponse

	BackfillParentChildDepsCalls []*client.BackfillParentChildDepsRequest
	BackfillParentChildDepsErr   error
	BackfillParentChildDepsResp  *client.BackfillParentChildDepsResponse

	RepairReversedParentChildDepsCalls []*client.RepairReversedParentChildDepsRequest
	RepairReversedParentChildDepsErr   error
	RepairReversedParentChildDepsResp  *client.RepairReversedParentChildDepsResponse

	BackfillTriggeredByDepsCalls []*client.BackfillTriggeredByDepsRequest
	BackfillTriggeredByDepsErr   error
	BackfillTriggeredByDepsResp  *client.BackfillTriggeredByDepsResponse

	AuditBlocksCyclesCalls int
	AuditBlocksCyclesErr   error
	AuditBlocksCyclesResp  *client.AuditBlocksCyclesResponse

	CloseTriggerAuditBeadsCalls []*client.CloseTriggerAuditBeadsRequest
	CloseTriggerAuditBeadsErr   error
	CloseTriggerAuditBeadsResp  *client.CloseTriggerAuditBeadsResponse

	// Sync-batch — the mock simulates what the server-side endpoint does by
	// consulting ListBeadsResult (optionally filtered by offset/limit) and the
	// EmbeddingHashes map to compute embedded/skipped counts. Individual
	// per-bead fake vectors are recorded on UpsertEmbeddingCalls so existing
	// assertions keep working.
	GetMutationsResult []*model.MutationRow
	GetMutationsErr    error

	UndoMutationCalls    []int64
	UndoMutationResponse *client.UndoMutationResponse
	UndoMutationErr      error

	// ListGlobalEventsFn drives `kd events backfill` replay tests. Each call
	// receives the page request (Cursor advances per page); return one page
	// plus HasMore/NextCursor to simulate cursor pagination.
	ListGlobalEventsFn func(*client.ListGlobalEventsRequest) (*client.ListGlobalEventsResponse, error)

	SyncBatchCalls []*client.SyncBatchEmbeddingsRequest
	SyncBatchErr   error
	// SyncBatchModel is echoed on the response so tests can assert on it.
	SyncBatchModel string

	// Per-call error injection (keyed by call argument).
	DeleteErrs  map[string]error
	CloseErrs   map[string]error
	CommentErrs map[string]error // keyed by bead ID; overrides CommentErr for that id

	// ListConfigs — drives --type filter validation tests (ka-0000006244).
	// ListConfigsByNamespace returns the configs for a queried namespace
	// (e.g. "type" → custom type:* keys); ListConfigsErr forces a transport
	// error so tests can assert validation degrades to the builtin floor.
	ListConfigsByNamespace map[string][]*model.Config
	ListConfigsErr         error
	ListConfigsCalls       []string

	// GetConfig — drives `kd fields <type>` tests. ConfigsByKey returns the
	// config for a queried key (e.g. "type:trigger" → the schema); if a key is
	// absent and GetConfigErr is set, that error is returned so tests can
	// simulate the daemon's 404-for-unknown-type path.
	ConfigsByKey   map[string]*model.Config
	GetConfigErr   error
	GetConfigCalls []string

	// ListBeadsCtxs is index-aligned with ListBeadsCalls: the context each call
	// arrived with, so a test can assert the caller bounded it with a DEADLINE.
	// That is the only way to prove a best-effort probe is bounded — an unbounded
	// maybePrintFieldKindNudge could hang the CLI after stdout already printed.
	ListBeadsCtxs []context.Context

	// Escalations (ka-0000007156). EscalationsByID / EscalationList back the
	// read verbs (GetEscalation / ListEscalations); the *Calls slices and
	// *Err fields let a test assert the resolve verbs (approve/reject/result)
	// wired the right request and propagate transport errors (e.g. a 403 for
	// an agent token hitting the operator-only endpoints).
	EscalationsByID        map[string]*client.EscalationDetail
	EscalationList         []client.EscalationDetail
	GetEscalationErr       error
	ListEscalationErr      error
	GetEscalationCalls     []string
	ListEscalationCalls    []client.ListEscalationsOptions
	ApproveEscalationCalls []struct {
		ID  string
		Req client.ApproveEscalationRequest
	}
	RejectEscalationCalls []struct {
		ID  string
		Req client.RejectEscalationRequest
	}
	ClaimEscalationCalls  []string
	HandUpEscalationCalls []struct {
		ID  string
		Req client.HandUpEscalationRequest
	}
	CaptureResultCalls []struct {
		ID  string
		Req client.CaptureEscalationResultRequest
	}
	// Releases (release.go). Releases backs ListReleases and GetRelease (by id
	// or tag); the *Calls slices record the write verbs' requests.
	Releases           []client.Release
	ListReleasesCalls  []struct{ Project, Status string }
	CreateReleaseCalls []client.CreateReleaseRequest
	// CreateReleaseErr fails CreateRelease alone (a 409 for a second open
	// release), leaving the reads working.
	CreateReleaseErr   error
	DeployReleaseCalls []struct {
		Ref, Project string
		Req          client.DeployReleaseRequest
	}
	ReleaseErr   error
	RepairResult *client.RepairDeploymentsResult
	RepairCalls  []struct {
		Project string
		DryRun  bool
	}

	ApproveEscalationErr error
	RejectEscalationErr  error
	CaptureResultErr     error

	ApproveScrubRequestCalls []struct {
		ID  string
		Req client.ApproveScrubRequestRequest
	}
	RejectScrubRequestCalls []struct {
		ID  string
		Req client.RejectScrubRequestRequest
	}
	ApproveScrubRequestErr      error
	ApproveScrubRequestArtifact *model.ScrubAuditArtifact
	RejectScrubRequestErr       error

	// Taints (docs/adr/taints.md §7).
	GrantTaintCalls []struct {
		Name string
		Req  *client.GrantTaintRequest
	}
	GrantTaintResult   *model.TaintGrant
	GrantTaintErr      error
	RevokeTaintCalls   []struct{ Name, Principal string }
	RevokeTaintResult  bool
	RevokeTaintErr     error
	ApplyTaintCalls    []struct{ BeadID, TaintName string }
	ApplyTaintResult   *model.BeadTaint
	ApplyTaintErr      error
	GetBeadTaintsCalls []string
	BeadTaints         map[string][]*model.BeadTaint
	GetBeadTaintsErr   error

	RequestTaintGrantCalls []struct {
		Name string
		Req  *client.RequestTaintGrantRequest
	}
	RequestTaintGrantResult *client.EscalationCreated
	RequestTaintGrantErr    error

	// Taints (task gd-sygX3b8TAg). TaintsByName / TaintList back the read
	// verbs; CreateTaintFn, when set, computes CreateTaint's response
	// (default: echoes the request back as a *client.TaintView).
	CreateTaintCalls []*client.CreateTaintRequest
	CreateTaintFn    func(*client.CreateTaintRequest) (*client.TaintView, error)
	CreateTaintErr   error
	TaintsByName     map[string]*client.TaintView
	TaintList        []client.TaintView
	GetTaintErr      error
	ListTaintErr     error
	GetTaintCalls    []string
	ListTaintCalls   int

	// Scrub requests. CreateScrubRequestFn, when set, computes the response
	// (default: echoes the request back as a *client.ScrubRequestView with no
	// resolved_bead_ids/detaint_mode — tests that need those set the func).
	CreateScrubRequestCalls []*client.CreateScrubRequestRequest
	CreateScrubRequestFn    func(*client.CreateScrubRequestRequest) (*client.ScrubRequestView, error)
	CreateScrubRequestErr   error
}

func newMockClient() *mockBeadsClient {
	return &mockBeadsClient{
		Beads:       make(map[string]*model.Bead),
		Deps:        make(map[string][]*model.Dependency),
		RevDeps:     make(map[string][]*model.Dependency),
		Labels:      make(map[string][]string),
		MentionedBy: make(map[string][]*model.BeadReferenceSummary),
		Mentions:    make(map[string][]*model.BeadReferenceSummary),
		DeleteErrs:  make(map[string]error),
		CloseErrs:   make(map[string]error),
		CommentErrs: make(map[string]error),
		BeadTaints:  make(map[string][]*model.BeadTaint),
	}
}

func (m *mockBeadsClient) CreateBead(_ context.Context, req *client.CreateBeadRequest) (*model.Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CreateBeadCalls = append(m.CreateBeadCalls, req)
	if m.CreateErr != nil {
		return nil, m.CreateErr
	}
	id := fmt.Sprintf("gd-new-%d", m.createIndex)
	if m.createIndex < len(m.CreateIDs) {
		id = m.CreateIDs[m.createIndex]
	}
	m.createIndex++
	return &model.Bead{
		ID:          id,
		Title:       req.Title,
		Type:        model.BeadType(req.Type),
		Description: req.Description,
		Priority:    req.Priority,
		Status:      model.StatusOpen,
		Labels:      req.Labels,
		Assignee:    req.Assignee,
		Fields:      req.Fields,
	}, nil
}

func (m *mockBeadsClient) GetBead(_ context.Context, id string) (*model.Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetBeadCalls = append(m.GetBeadCalls, id)
	if m.GetErr != nil {
		return nil, m.GetErr
	}
	if b, ok := m.Beads[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("bead %s not found", id)
}

func (m *mockBeadsClient) ListBeads(ctx context.Context, req *client.ListBeadsRequest) (*client.ListBeadsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ListBeadsCalls = append(m.ListBeadsCalls, req)
	// Record each call's context alongside the request so a test can assert a
	// caller attached a DEADLINE — the only way to prove a best-effort probe is
	// bounded (see maybePrintFieldKindNudge, whose unbounded form could hang the
	// CLI after stdout had already printed).
	m.ListBeadsCtxs = append(m.ListBeadsCtxs, ctx)
	if m.ListErr != nil {
		return nil, m.ListErr
	}
	if len(m.ListBeadsPages) > 0 {
		idx := len(m.ListBeadsCalls) - 1
		if idx < len(m.ListBeadsPages) {
			page := m.ListBeadsPages[idx]
			return &client.ListBeadsResponse{Beads: page, Total: len(page)}, nil
		}
		return &client.ListBeadsResponse{}, nil
	}
	if m.ListBeadsResult != nil {
		total := len(m.ListBeadsResult)
		if m.ListBeadsTotalOverride > 0 {
			total = m.ListBeadsTotalOverride
		}
		return &client.ListBeadsResponse{Beads: m.ListBeadsResult, Total: total}, nil
	}
	return &client.ListBeadsResponse{}, nil
}

func (m *mockBeadsClient) UpdateBead(_ context.Context, id string, req *client.UpdateBeadRequest) (*model.Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.UpdateBeadCalls = append(m.UpdateBeadCalls, struct {
		ID  string
		Req *client.UpdateBeadRequest
	}{id, req})
	if m.UpdateErr != nil {
		return nil, m.UpdateErr
	}
	if b, ok := m.Beads[id]; ok {
		return b, nil
	}
	return &model.Bead{ID: id}, nil
}

func (m *mockBeadsClient) EditBeadSubField(_ context.Context, id, field string, req *client.EditSubFieldRequest) (*model.Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.EditSubFieldCalls = append(m.EditSubFieldCalls, struct {
		ID    string
		Field string
		Req   *client.EditSubFieldRequest
	}{id, field, req})
	if m.UpdateErr != nil {
		return nil, m.UpdateErr
	}
	if b, ok := m.Beads[id]; ok {
		return b, nil
	}
	return &model.Bead{ID: id}, nil
}

func (m *mockBeadsClient) CloseBead(_ context.Context, id, closedBy string) (*model.Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CloseBeadCalls = append(m.CloseBeadCalls, struct{ ID, ClosedBy string }{id, closedBy})
	if err, ok := m.CloseErrs[id]; ok {
		return nil, err
	}
	if m.CloseErr != nil {
		return nil, m.CloseErr
	}
	return &model.Bead{ID: id, Status: model.StatusClosed}, nil
}

func (m *mockBeadsClient) DeleteBead(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeleteBeadCalls = append(m.DeleteBeadCalls, id)
	if err, ok := m.DeleteErrs[id]; ok {
		return err
	}
	return m.DeleteErr
}

func (m *mockBeadsClient) RestoreBead(_ context.Context, id string) (*model.Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RestoreBeadCalls = append(m.RestoreBeadCalls, id)
	if m.RestoreErr != nil {
		return nil, m.RestoreErr
	}
	return &model.Bead{ID: id, Status: model.StatusOpen}, nil
}

func (m *mockBeadsClient) AddDependency(ctx context.Context, req *client.AddDependencyRequest) (*model.Dependency, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.AddDepCalls = append(m.AddDepCalls, req)
	// Some callers pass a nil context; ForceParentEdgeFromContext derefs ctx, so
	// guard it — a nil ctx never carries the force flag.
	m.AddDepForceParent = append(m.AddDepForceParent, ctx != nil && client.ForceParentEdgeFromContext(ctx))
	if m.AddDepErr != nil {
		return nil, m.AddDepErr
	}
	if m.AddDepFn != nil {
		return m.AddDepFn(req.BeadID, req.DependsOnID, req.Type), nil
	}
	return &model.Dependency{
		BeadID:      req.BeadID,
		DependsOnID: req.DependsOnID,
		Type:        model.DependencyType(req.Type),
	}, nil
}

// RemoveDependency mirrors the real store's rows-affected semantics: it deletes
// the matching edge from m.Deps and reports whether one was actually there, so
// tests exercise the true 0-match vs 1-match paths instead of a canned success.
// A test that never populates m.Deps still sees removed=true, matching the
// daemon's assume-success posture for callers that don't model the graph.
func (m *mockBeadsClient) RemoveDependency(_ context.Context, beadID, dependsOnID, depType string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RemoveDepCalls = append(m.RemoveDepCalls, struct{ BeadID, DependsOnID, Type string }{beadID, dependsOnID, depType})
	deps := m.Deps[beadID]
	for i, d := range deps {
		if d.DependsOnID == dependsOnID && string(d.Type) == depType {
			m.Deps[beadID] = append(deps[:i:i], deps[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (m *mockBeadsClient) GetDependencies(_ context.Context, beadID string) ([]*model.Dependency, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Deps[beadID], nil
}

func (m *mockBeadsClient) GetReverseDependencies(_ context.Context, beadID string) ([]*model.Dependency, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetRevDepCalls = append(m.GetRevDepCalls, beadID)
	if m.RevDepErr != nil {
		return nil, m.RevDepErr
	}
	return m.RevDeps[beadID], nil
}

func (m *mockBeadsClient) ListBeadsMentioning(_ context.Context, beadID string, limit, offset int, relationshipType *string) (*client.ListReferencesResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MentionedByCalls = append(m.MentionedByCalls, beadID)
	m.MentionedByRelType = relationshipType
	if m.MentionedByErr != nil {
		return nil, m.MentionedByErr
	}
	return paginateRefs(filterRefsByType(m.MentionedBy[beadID], relationshipType), limit, offset), nil
}

func (m *mockBeadsClient) ListBeadReferencesFrom(_ context.Context, beadID string, limit, offset int, relationshipType *string) (*client.ListReferencesResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MentionsCalls = append(m.MentionsCalls, beadID)
	m.MentionsRelType = relationshipType
	if m.MentionsErr != nil {
		return nil, m.MentionsErr
	}
	return paginateRefs(filterRefsByType(m.Mentions[beadID], relationshipType), limit, offset), nil
}

// filterRefsByType applies the same three-state filter as the postgres store:
// nil → no filter; non-nil "" → only NULL relationship_type; non-nil "<v>" →
// exact match. Lets CLI tests assert that the filter actually narrows the
// page.
func filterRefsByType(in []*model.BeadReferenceSummary, filter *string) []*model.BeadReferenceSummary {
	if filter == nil {
		return in
	}
	out := make([]*model.BeadReferenceSummary, 0, len(in))
	for _, r := range in {
		if r == nil {
			continue
		}
		if *filter == "" {
			if r.RelationshipType == nil {
				out = append(out, r)
			}
			continue
		}
		if r.RelationshipType != nil && *r.RelationshipType == *filter {
			out = append(out, r)
		}
	}
	return out
}

func paginateRefs(all []*model.BeadReferenceSummary, limit, offset int) *client.ListReferencesResponse {
	total := len(all)
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	end := offset + limit
	if offset > total {
		offset = total
	}
	if end > total {
		end = total
	}
	page := all[offset:end]
	if page == nil {
		page = []*model.BeadReferenceSummary{}
	}
	return &client.ListReferencesResponse{
		References: page,
		Total:      total,
		Limit:      limit,
		Offset:     offset,
	}
}

func (m *mockBeadsClient) AddLabel(_ context.Context, beadID, label string) (*model.Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.AddLabelCalls = append(m.AddLabelCalls, struct{ BeadID, Label string }{beadID, label})
	return &model.Bead{}, nil
}

// RemoveLabel mirrors the real store's rows-affected semantics: it deletes the
// matching label from m.Labels and reports whether one was actually there, so
// tests exercise the true 0-match vs 1-match paths instead of a canned success.
// A test that never populates m.Labels still sees removed=true, matching the
// daemon's assume-success posture for callers that don't model label state.
func (m *mockBeadsClient) RemoveLabel(_ context.Context, beadID, label string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RemoveLabelCalls = append(m.RemoveLabelCalls, struct{ BeadID, Label string }{beadID, label})
	labels, tracked := m.Labels[beadID]
	if !tracked {
		return true, nil
	}
	for i, l := range labels {
		if l == label {
			m.Labels[beadID] = append(labels[:i:i], labels[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (m *mockBeadsClient) GetLabels(context.Context, string) ([]string, error) {
	return nil, nil
}

func (m *mockBeadsClient) AddComment(_ context.Context, beadID, author, text string) (*model.Comment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.AddCommentCalls = append(m.AddCommentCalls, struct{ BeadID, Author, Text string }{beadID, author, text})
	if err, ok := m.CommentErrs[beadID]; ok {
		return nil, err
	}
	if m.CommentErr != nil {
		return nil, m.CommentErr
	}
	return &model.Comment{BeadID: beadID, Author: author, Text: text}, nil
}

func (m *mockBeadsClient) GetComments(_ context.Context, beadID string) ([]*model.Comment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.GetCommentsErr != nil {
		return nil, m.GetCommentsErr
	}
	if m.CommentsByBead != nil {
		return m.CommentsByBead[beadID], nil
	}
	return nil, nil
}

func (m *mockBeadsClient) DeleteComment(_ context.Context, beadID string, commentID int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeleteCommentCalls = append(m.DeleteCommentCalls, struct {
		BeadID    string
		CommentID int64
	}{beadID, commentID})
	if m.DeleteCommentErr != nil {
		return false, m.DeleteCommentErr
	}
	return m.DeleteCommentResult, nil
}

func (m *mockBeadsClient) TrimComments(_ context.Context, beadID string, keepLast int, authorFilter string) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TrimCommentsCalls = append(m.TrimCommentsCalls, struct {
		BeadID   string
		KeepLast int
		Author   string
	}{beadID, keepLast, authorFilter})
	if m.TrimCommentsErr != nil {
		return nil, m.TrimCommentsErr
	}
	return m.TrimCommentsResult, nil
}

func (m *mockBeadsClient) GetEvents(context.Context, string) ([]*model.Event, error) {
	return nil, nil
}

func (m *mockBeadsClient) ListGlobalEvents(_ context.Context, req *client.ListGlobalEventsRequest) (*client.ListGlobalEventsResponse, error) {
	if m.ListGlobalEventsFn != nil {
		return m.ListGlobalEventsFn(req)
	}
	return &client.ListGlobalEventsResponse{}, nil
}

func (m *mockBeadsClient) GetMutations(_ context.Context, _ string) ([]*model.MutationRow, error) {
	if m.GetMutationsErr != nil {
		return nil, m.GetMutationsErr
	}
	return m.GetMutationsResult, nil
}

func (m *mockBeadsClient) UndoMutation(_ context.Context, mutationID int64) (*client.UndoMutationResponse, error) {
	m.UndoMutationCalls = append(m.UndoMutationCalls, mutationID)
	if m.UndoMutationErr != nil {
		return nil, m.UndoMutationErr
	}
	return m.UndoMutationResponse, nil
}

// --- Taints (docs/adr/taints.md §7) ---

func (m *mockBeadsClient) GrantTaint(_ context.Context, name string, req *client.GrantTaintRequest) (*model.TaintGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GrantTaintCalls = append(m.GrantTaintCalls, struct {
		Name string
		Req  *client.GrantTaintRequest
	}{name, req})
	if m.GrantTaintErr != nil {
		return nil, m.GrantTaintErr
	}
	return m.GrantTaintResult, nil
}

func (m *mockBeadsClient) RevokeTaint(_ context.Context, name, principal string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RevokeTaintCalls = append(m.RevokeTaintCalls, struct{ Name, Principal string }{name, principal})
	if m.RevokeTaintErr != nil {
		return false, m.RevokeTaintErr
	}
	return m.RevokeTaintResult, nil
}

func (m *mockBeadsClient) ApplyTaint(_ context.Context, beadID, taintName string) (*model.BeadTaint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ApplyTaintCalls = append(m.ApplyTaintCalls, struct{ BeadID, TaintName string }{beadID, taintName})
	if m.ApplyTaintErr != nil {
		return nil, m.ApplyTaintErr
	}
	return m.ApplyTaintResult, nil
}

func (m *mockBeadsClient) GetBeadTaints(_ context.Context, beadID string) ([]*model.BeadTaint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetBeadTaintsCalls = append(m.GetBeadTaintsCalls, beadID)
	if m.GetBeadTaintsErr != nil {
		return nil, m.GetBeadTaintsErr
	}
	return m.BeadTaints[beadID], nil
}

func (m *mockBeadsClient) RequestTaintGrant(_ context.Context, name string, req *client.RequestTaintGrantRequest) (*client.EscalationCreated, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RequestTaintGrantCalls = append(m.RequestTaintGrantCalls, struct {
		Name string
		Req  *client.RequestTaintGrantRequest
	}{name, req})
	if m.RequestTaintGrantErr != nil {
		return nil, m.RequestTaintGrantErr
	}
	return m.RequestTaintGrantResult, nil
}

func (m *mockBeadsClient) SetConfig(_ context.Context, key string, value json.RawMessage) (*model.Config, error) {
	return &model.Config{Key: key, Value: value}, nil
}

func (m *mockBeadsClient) GetConfig(_ context.Context, key string) (*model.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetConfigCalls = append(m.GetConfigCalls, key)
	if c, ok := m.ConfigsByKey[key]; ok {
		return c, nil
	}
	if m.GetConfigErr != nil {
		return nil, m.GetConfigErr
	}
	return &model.Config{Key: key}, nil
}

func (m *mockBeadsClient) ListConfigs(_ context.Context, namespace string) ([]*model.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ListConfigsCalls = append(m.ListConfigsCalls, namespace)
	if m.ListConfigsErr != nil {
		return nil, m.ListConfigsErr
	}
	return m.ListConfigsByNamespace[namespace], nil
}

func (m *mockBeadsClient) DeleteConfig(context.Context, string) error { return nil }

func (m *mockBeadsClient) EmitHook(context.Context, *client.EmitHookRequest) (*client.EmitHookResponse, error) {
	return &client.EmitHookResponse{}, nil
}

func (m *mockBeadsClient) ListGates(context.Context, string) ([]model.GateRow, error) {
	return nil, nil
}

func (m *mockBeadsClient) SatisfyGate(context.Context, string, string) error { return nil }
func (m *mockBeadsClient) ClearGate(context.Context, string, string) error   { return nil }

func (m *mockBeadsClient) GetAgentRoster(context.Context, int) (*client.AgentRosterResponse, error) {
	return &client.AgentRosterResponse{}, nil
}

func (m *mockBeadsClient) LookupOperatorByAlias(_ context.Context, kind, subject string) (*client.LookupOperatorByAliasResponse, error) {
	m.mu.Lock()
	m.LookupOperatorByAliasCalls = append(m.LookupOperatorByAliasCalls, struct{ Kind, Subject string }{kind, subject})
	fn := m.LookupOperatorByAliasFn
	err := m.LookupOperatorByAliasErr
	aliases := m.OperatorAliases
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if fn != nil {
		return fn(kind, subject)
	}
	if id, ok := aliases[kind+":"+subject]; ok {
		return &client.LookupOperatorByAliasResponse{OperatorID: id, OperatorKind: kind, Found: true}, nil
	}
	return &client.LookupOperatorByAliasResponse{Found: false}, nil
}

func (m *mockBeadsClient) BatchGetBeads(_ context.Context, ids []string) ([]client.BatchGetResult, error) {
	m.mu.Lock()
	m.BatchGetCalls = append(m.BatchGetCalls, append([]string(nil), ids...))
	batchErr := m.BatchGetErr
	m.mu.Unlock()
	if batchErr != nil {
		return nil, batchErr
	}
	results := make([]client.BatchGetResult, len(ids))
	for i, id := range ids {
		results[i].ID = id
		if b, ok := m.Beads[id]; ok {
			results[i].Bead = b
		} else {
			results[i].Error = "not found"
		}
	}
	return results, nil
}

func (m *mockBeadsClient) TransferBead(_ context.Context, id, toFleet string) (*model.Bead, error) {
	if b, ok := m.Beads[id]; ok {
		b.Fleet = toFleet
		return b, nil
	}
	return &model.Bead{ID: id, Fleet: toFleet}, nil
}

func (m *mockBeadsClient) FindSimilar(_ context.Context, beadID string, limit int, minScore float64) (*client.FindSimilarResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.FindSimilarCalls = append(m.FindSimilarCalls, struct {
		BeadID   string
		Limit    int
		MinScore float64
	}{beadID, limit, minScore})
	if m.FindSimilarErr != nil {
		return nil, m.FindSimilarErr
	}
	if m.FindSimilarResp != nil {
		return m.FindSimilarResp, nil
	}
	return &client.FindSimilarResponse{SourceID: beadID}, nil
}

func (m *mockBeadsClient) SemanticSearch(_ context.Context, req *client.SemanticSearchRequest) (*client.SemanticSearchResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SemanticSearchCalls = append(m.SemanticSearchCalls, req)
	if m.SemanticSearchErr != nil {
		return nil, m.SemanticSearchErr
	}
	if m.SemanticSearchResp != nil {
		return m.SemanticSearchResp, nil
	}
	return &client.SemanticSearchResponse{}, nil
}

func (m *mockBeadsClient) UpsertEmbedding(_ context.Context, req *client.UpsertEmbeddingRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.UpsertEmbeddingCalls = append(m.UpsertEmbeddingCalls, req)
	return m.UpsertEmbeddingErr
}

func (m *mockBeadsClient) GetEmbeddingHashes(_ context.Context, beadIDs []string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetEmbeddingHashesCalls = append(m.GetEmbeddingHashesCalls, beadIDs)
	if m.GetEmbeddingHashesErr != nil {
		return nil, m.GetEmbeddingHashesErr
	}
	if m.EmbeddingHashes == nil {
		return map[string]string{}, nil
	}
	out := make(map[string]string)
	for _, id := range beadIDs {
		if h, ok := m.EmbeddingHashes[id]; ok {
			out[id] = h
		}
	}
	return out, nil
}

func (m *mockBeadsClient) MigrateEmbeddings(_ context.Context, req *client.MigrateEmbeddingsRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MigrateEmbeddingsCalls = append(m.MigrateEmbeddingsCalls, req)
	if m.MigrateEmbeddingsErr != nil {
		return m.MigrateEmbeddingsErr
	}
	// Simulate the destructive truncate — existing hashes are gone.
	m.EmbeddingHashes = map[string]string{}
	return nil
}

func (m *mockBeadsClient) ReindexEmbeddings(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ReindexEmbeddingsCalls++
	return m.ReindexEmbeddingsErr
}

func (m *mockBeadsClient) ReextractReferences(_ context.Context, req *client.ReextractReferencesRequest) (*client.ReextractReferencesResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ReextractReferencesCalls = append(m.ReextractReferencesCalls, req)
	if m.ReextractReferencesFn != nil {
		return m.ReextractReferencesFn(req)
	}
	if m.ReextractReferencesErr != nil {
		return nil, m.ReextractReferencesErr
	}
	idx := len(m.ReextractReferencesCalls) - 1
	if idx < len(m.ReextractReferencesPages) {
		return m.ReextractReferencesPages[idx], nil
	}
	return &client.ReextractReferencesResponse{Done: true}, nil
}

func (m *mockBeadsClient) GetEscalation(_ context.Context, id string) (*client.EscalationDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetEscalationCalls = append(m.GetEscalationCalls, id)
	if m.GetEscalationErr != nil {
		return nil, m.GetEscalationErr
	}
	if d, ok := m.EscalationsByID[id]; ok {
		return d, nil
	}
	return nil, &client.APIError{StatusCode: 404, Message: "escalation not found"}
}

func (m *mockBeadsClient) ListEscalations(_ context.Context, opts client.ListEscalationsOptions) ([]client.EscalationDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ListEscalationCalls = append(m.ListEscalationCalls, opts)
	if m.ListEscalationErr != nil {
		return nil, m.ListEscalationErr
	}
	return m.EscalationList, nil
}

func (m *mockBeadsClient) ApproveEscalation(_ context.Context, id string, req client.ApproveEscalationRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ApproveEscalationCalls = append(m.ApproveEscalationCalls, struct {
		ID  string
		Req client.ApproveEscalationRequest
	}{id, req})
	return m.ApproveEscalationErr
}

func (m *mockBeadsClient) RejectEscalation(_ context.Context, id string, req client.RejectEscalationRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RejectEscalationCalls = append(m.RejectEscalationCalls, struct {
		ID  string
		Req client.RejectEscalationRequest
	}{id, req})
	return m.RejectEscalationErr
}

func (m *mockBeadsClient) ClaimEscalation(_ context.Context, id string) (*model.Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ClaimEscalationCalls = append(m.ClaimEscalationCalls, id)
	return &model.Bead{ID: id, Type: model.TypeEscalation, Status: model.StatusOpen}, nil
}

func (m *mockBeadsClient) HandUpEscalation(_ context.Context, id string, req client.HandUpEscalationRequest) (*model.Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.HandUpEscalationCalls = append(m.HandUpEscalationCalls, struct {
		ID  string
		Req client.HandUpEscalationRequest
	}{id, req})
	return &model.Bead{ID: id, Type: model.TypeEscalation, Status: model.StatusOpen}, nil
}

func (m *mockBeadsClient) CaptureEscalationResult(_ context.Context, id string, req client.CaptureEscalationResultRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CaptureResultCalls = append(m.CaptureResultCalls, struct {
		ID  string
		Req client.CaptureEscalationResultRequest
	}{id, req})
	return m.CaptureResultErr
}

func (m *mockBeadsClient) ApproveScrubRequest(_ context.Context, id string, req client.ApproveScrubRequestRequest) (*model.ScrubAuditArtifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ApproveScrubRequestCalls = append(m.ApproveScrubRequestCalls, struct {
		ID  string
		Req client.ApproveScrubRequestRequest
	}{id, req})
	return m.ApproveScrubRequestArtifact, m.ApproveScrubRequestErr
}

func (m *mockBeadsClient) RejectScrubRequest(_ context.Context, id string, req client.RejectScrubRequestRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RejectScrubRequestCalls = append(m.RejectScrubRequestCalls, struct {
		ID  string
		Req client.RejectScrubRequestRequest
	}{id, req})
	return m.RejectScrubRequestErr
}

func (m *mockBeadsClient) CreateTaint(_ context.Context, req *client.CreateTaintRequest) (*client.TaintView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CreateTaintCalls = append(m.CreateTaintCalls, req)
	if m.CreateTaintErr != nil {
		return nil, m.CreateTaintErr
	}
	if m.CreateTaintFn != nil {
		return m.CreateTaintFn(req)
	}
	return &client.TaintView{
		Name:        req.Name,
		Description: req.Description,
		OwnerTeam:   req.OwnerTeam,
		ScrubPolicy: req.ScrubPolicy,
	}, nil
}

func (m *mockBeadsClient) ListTaints(context.Context) (*client.ListTaintsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ListTaintCalls++
	if m.ListTaintErr != nil {
		return nil, m.ListTaintErr
	}
	return &client.ListTaintsResponse{Taints: m.TaintList}, nil
}

func (m *mockBeadsClient) GetTaint(_ context.Context, name string) (*client.TaintView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetTaintCalls = append(m.GetTaintCalls, name)
	if m.GetTaintErr != nil {
		return nil, m.GetTaintErr
	}
	if t, ok := m.TaintsByName[name]; ok {
		return t, nil
	}
	return nil, &client.APIError{StatusCode: 404, Message: "taint not found"}
}

func (m *mockBeadsClient) CreateScrubRequest(_ context.Context, req *client.CreateScrubRequestRequest) (*client.ScrubRequestView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CreateScrubRequestCalls = append(m.CreateScrubRequestCalls, req)
	if m.CreateScrubRequestErr != nil {
		return nil, m.CreateScrubRequestErr
	}
	if m.CreateScrubRequestFn != nil {
		return m.CreateScrubRequestFn(req)
	}
	return &client.ScrubRequestView{
		Bead: &model.Bead{
			ID:   "kd-scrub-mock",
			Type: model.TypeScrubRequest,
		},
	}, nil
}

func (m *mockBeadsClient) Health(context.Context) (string, error) { return "ok", nil }
func (m *mockBeadsClient) Close() error                           { return nil }

// withMockClient swaps beadsClient and actor for the duration of a test,
// restoring the originals on cleanup.
//
// It also stubs checkMRLiveState to mrLiveUnknown so no test reaches the real
// GitLab API. Without this the close guard's live re-check (close_mr_live.go)
// would fire against gitlab.com whenever GITLAB_TOKEN/GLAB_TOKEN happens to be
// set — as it is in agent pods and CI — making these tests slow and
// network-dependent. mrLiveUnknown is the neutral verdict: it preserves the
// projected state, which is exactly what every pre-existing test asserts.
// Tests that exercise the race itself override it via withMRLiveState.
func withMockClient(t *testing.T, mc *mockBeadsClient) {
	t.Helper()
	origClient := beadsClient
	origActor := actor
	origJSON := jsonOutput
	origLive := checkMRLiveState
	t.Cleanup(func() {
		beadsClient = origClient
		actor = origActor
		jsonOutput = origJSON
		checkMRLiveState = origLive
	})
	beadsClient = mc
	actor = "test-actor"
	jsonOutput = false
	checkMRLiveState = func(context.Context, string) mrLiveState { return mrLiveUnknown }
}

// withMRLiveState overrides the authoritative GitLab check for one test,
// recording the urls it was asked about so a test can assert the check ran only
// on the paths that were about to refuse. Call AFTER withMockClient, whose
// cleanup restores the original.
func withMRLiveState(t *testing.T, state mrLiveState, calls *[]string) {
	t.Helper()
	checkMRLiveState = func(_ context.Context, mrURL string) mrLiveState {
		if calls != nil {
			*calls = append(*calls, mrURL)
		}
		return state
	}
}

func (m *mockBeadsClient) CreateRelease(_ context.Context, req client.CreateReleaseRequest) (*client.Release, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CreateReleaseCalls = append(m.CreateReleaseCalls, req)
	if m.ReleaseErr != nil {
		return nil, m.ReleaseErr
	}
	if m.CreateReleaseErr != nil {
		return nil, m.CreateReleaseErr
	}
	r := client.Release{ID: "gd-rel-new", Project: req.Project, Tag: req.Tag, SHA: req.SHA, Status: "cut", Deployments: []client.ReleaseDeployment{}}
	m.Releases = append(m.Releases, r)
	return &r, nil
}

func (m *mockBeadsClient) GetRelease(_ context.Context, ref, project string) (*client.Release, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ReleaseErr != nil {
		return nil, m.ReleaseErr
	}
	for i := range m.Releases {
		r := m.Releases[i]
		if r.ID == ref || (project != "" && r.Project == project && r.Tag == ref) {
			return &r, nil
		}
	}
	return nil, &client.APIError{StatusCode: 404, Message: "release not found"}
}

func (m *mockBeadsClient) RepairReleaseDeployments(_ context.Context, project string, dryRun bool) (*client.RepairDeploymentsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RepairCalls = append(m.RepairCalls, struct {
		Project string
		DryRun  bool
	}{project, dryRun})
	if m.ReleaseErr != nil {
		return nil, m.ReleaseErr
	}
	if m.RepairResult != nil {
		return m.RepairResult, nil
	}
	return &client.RepairDeploymentsResult{Project: project, DryRun: dryRun}, nil
}

func (m *mockBeadsClient) ListReleases(_ context.Context, project, status string, _ int) ([]client.Release, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ListReleasesCalls = append(m.ListReleasesCalls, struct{ Project, Status string }{project, status})
	if m.ReleaseErr != nil {
		return nil, m.ReleaseErr
	}
	return m.Releases, nil
}

func (m *mockBeadsClient) DeployRelease(_ context.Context, ref, project string, req client.DeployReleaseRequest) (*client.Release, error) {
	m.mu.Lock()
	m.DeployReleaseCalls = append(m.DeployReleaseCalls, struct {
		Ref, Project string
		Req          client.DeployReleaseRequest
	}{ref, project, req})
	err := m.ReleaseErr
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return m.GetRelease(context.Background(), ref, project)
}
