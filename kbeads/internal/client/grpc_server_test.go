package client

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// fakeBeadsServer is an in-process gRPC server used to exercise the GRPCClient
// transport end-to-end over a bufconn pipe. Only the RPCs the tests touch are
// overridden; everything else inherits the Unimplemented stubs.
type fakeBeadsServer struct {
	beadsv1.UnimplementedBeadsServiceServer

	// lastMD captures the incoming metadata on the most recent unary call so
	// tests can assert that SetFleet/SetActor/SetOperatedBy thread through.
	lastMD metadata.MD

	// lastListBeads captures the most recent ListBeads request as it arrived ON
	// THE WIRE, so tests can assert a filter the client is supposed to carry
	// actually reached the server. A filter the client silently forgets to
	// populate returns a WIDER set than asked for — invisible to any assertion
	// made only against the client-side struct.
	lastListBeads *beadsv1.ListBeadsRequest
}

func (s *fakeBeadsServer) capture(ctx context.Context) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		s.lastMD = md
	}
}

func (s *fakeBeadsServer) CreateBead(ctx context.Context, req *beadsv1.CreateBeadRequest) (*beadsv1.CreateBeadResponse, error) {
	s.capture(ctx)
	return &beadsv1.CreateBeadResponse{Bead: &beadsv1.Bead{
		Id:    "gd-new",
		Title: req.GetTitle(),
		Type:  req.GetType(),
	}}, nil
}

func (s *fakeBeadsServer) GetBead(ctx context.Context, req *beadsv1.GetBeadRequest) (*beadsv1.GetBeadResponse, error) {
	s.capture(ctx)
	return &beadsv1.GetBeadResponse{Bead: &beadsv1.Bead{Id: req.GetId(), Title: "got"}}, nil
}

func (s *fakeBeadsServer) ListBeads(ctx context.Context, req *beadsv1.ListBeadsRequest) (*beadsv1.ListBeadsResponse, error) {
	s.capture(ctx)
	s.lastListBeads = req
	return &beadsv1.ListBeadsResponse{
		Beads: []*beadsv1.Bead{{Id: "gd-1"}, {Id: "gd-2"}},
		Total: 2,
	}, nil
}

func (s *fakeBeadsServer) CloseBead(ctx context.Context, req *beadsv1.CloseBeadRequest) (*beadsv1.CloseBeadResponse, error) {
	s.capture(ctx)
	return &beadsv1.CloseBeadResponse{Bead: &beadsv1.Bead{Id: req.GetId(), Status: "closed"}}, nil
}

func (s *fakeBeadsServer) DeleteBead(ctx context.Context, req *beadsv1.DeleteBeadRequest) (*beadsv1.DeleteBeadResponse, error) {
	s.capture(ctx)
	return &beadsv1.DeleteBeadResponse{}, nil
}

func (s *fakeBeadsServer) AddDependency(ctx context.Context, req *beadsv1.AddDependencyRequest) (*beadsv1.AddDependencyResponse, error) {
	s.capture(ctx)
	return &beadsv1.AddDependencyResponse{Dependency: &beadsv1.Dependency{
		BeadId:      req.GetBeadId(),
		DependsOnId: req.GetDependsOnId(),
		Type:        req.GetType(),
	}}, nil
}

func (s *fakeBeadsServer) RemoveDependency(ctx context.Context, req *beadsv1.RemoveDependencyRequest) (*beadsv1.RemoveDependencyResponse, error) {
	s.capture(ctx)
	return &beadsv1.RemoveDependencyResponse{}, nil
}

func (s *fakeBeadsServer) GetDependencies(ctx context.Context, req *beadsv1.GetDependenciesRequest) (*beadsv1.GetDependenciesResponse, error) {
	s.capture(ctx)
	return &beadsv1.GetDependenciesResponse{Dependencies: []*beadsv1.Dependency{
		{BeadId: req.GetBeadId(), DependsOnId: "gd-dep", Type: "blocks"},
	}}, nil
}

func (s *fakeBeadsServer) AddLabel(ctx context.Context, req *beadsv1.AddLabelRequest) (*beadsv1.AddLabelResponse, error) {
	s.capture(ctx)
	return &beadsv1.AddLabelResponse{Bead: &beadsv1.Bead{Id: req.GetBeadId(), Labels: []string{req.GetLabel()}}}, nil
}

func (s *fakeBeadsServer) RemoveLabel(ctx context.Context, req *beadsv1.RemoveLabelRequest) (*beadsv1.RemoveLabelResponse, error) {
	s.capture(ctx)
	return &beadsv1.RemoveLabelResponse{}, nil
}

func (s *fakeBeadsServer) GetLabels(ctx context.Context, req *beadsv1.GetLabelsRequest) (*beadsv1.GetLabelsResponse, error) {
	s.capture(ctx)
	return &beadsv1.GetLabelsResponse{Labels: []string{"a", "b"}}, nil
}

func (s *fakeBeadsServer) AddComment(ctx context.Context, req *beadsv1.AddCommentRequest) (*beadsv1.AddCommentResponse, error) {
	s.capture(ctx)
	return &beadsv1.AddCommentResponse{Comment: &beadsv1.Comment{
		Id:     1,
		BeadId: req.GetBeadId(),
		Author: req.GetAuthor(),
		Text:   req.GetText(),
	}}, nil
}

func (s *fakeBeadsServer) GetComments(ctx context.Context, req *beadsv1.GetCommentsRequest) (*beadsv1.GetCommentsResponse, error) {
	s.capture(ctx)
	return &beadsv1.GetCommentsResponse{Comments: []*beadsv1.Comment{{Id: 1, Text: "hi"}}}, nil
}

func (s *fakeBeadsServer) DeleteComment(ctx context.Context, req *beadsv1.DeleteCommentRequest) (*beadsv1.DeleteCommentResponse, error) {
	s.capture(ctx)
	return &beadsv1.DeleteCommentResponse{Deleted: true}, nil
}

func (s *fakeBeadsServer) TrimComments(ctx context.Context, req *beadsv1.TrimCommentsRequest) (*beadsv1.TrimCommentsResponse, error) {
	s.capture(ctx)
	return &beadsv1.TrimCommentsResponse{DeletedIds: []int64{1, 2}}, nil
}

func (s *fakeBeadsServer) GetEvents(ctx context.Context, req *beadsv1.GetEventsRequest) (*beadsv1.GetEventsResponse, error) {
	s.capture(ctx)
	return &beadsv1.GetEventsResponse{Events: []*beadsv1.Event{{Id: 1, Topic: "created"}}}, nil
}

func (s *fakeBeadsServer) SetConfig(ctx context.Context, req *beadsv1.SetConfigRequest) (*beadsv1.SetConfigResponse, error) {
	s.capture(ctx)
	return &beadsv1.SetConfigResponse{Config: &beadsv1.Config{Key: req.GetKey(), Value: req.GetValue()}}, nil
}

func (s *fakeBeadsServer) GetConfig(ctx context.Context, req *beadsv1.GetConfigRequest) (*beadsv1.GetConfigResponse, error) {
	s.capture(ctx)
	return &beadsv1.GetConfigResponse{Config: &beadsv1.Config{Key: req.GetKey(), Value: []byte(`{"k":1}`)}}, nil
}

func (s *fakeBeadsServer) ListConfigs(ctx context.Context, req *beadsv1.ListConfigsRequest) (*beadsv1.ListConfigsResponse, error) {
	s.capture(ctx)
	return &beadsv1.ListConfigsResponse{Configs: []*beadsv1.Config{{Key: "view:a"}, {Key: "view:b"}}}, nil
}

func (s *fakeBeadsServer) DeleteConfig(ctx context.Context, req *beadsv1.DeleteConfigRequest) (*beadsv1.DeleteConfigResponse, error) {
	s.capture(ctx)
	return &beadsv1.DeleteConfigResponse{}, nil
}

func (s *fakeBeadsServer) ListMentionedBy(ctx context.Context, req *beadsv1.ListMentionedByRequest) (*beadsv1.ListMentionedByResponse, error) {
	s.capture(ctx)
	return &beadsv1.ListMentionedByResponse{
		References: []*beadsv1.BeadReferenceSummary{{Id: "gd-ref", Title: "r", Type: "task"}},
		Total:      1,
		Limit:      int32(req.GetLimit()),
		Offset:     int32(req.GetOffset()),
	}, nil
}

func (s *fakeBeadsServer) ListMentions(ctx context.Context, req *beadsv1.ListMentionsRequest) (*beadsv1.ListMentionsResponse, error) {
	s.capture(ctx)
	return &beadsv1.ListMentionsResponse{
		References: []*beadsv1.BeadReferenceSummary{{Id: "gd-out"}},
		Total:      1,
	}, nil
}

// newFakeGRPCClient spins up a bufconn-backed server + a GRPCClient dialed at
// it. The returned cleanup closes both. fleet/actor/operatedBy are applied to
// the client so metadata-threading can be asserted via srv.lastMD.
func newFakeGRPCClient(t *testing.T) (*GRPCClient, *fakeBeadsServer, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	fake := &fakeBeadsServer{}
	beadsv1.RegisterBeadsServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	c := &GRPCClient{conn: conn, client: beadsv1.NewBeadsServiceClient(conn)}

	cleanup := func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	}
	return c, fake, cleanup
}

func TestGRPCClient_CRUD(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()
	ctx := context.Background()

	b, err := c.CreateBead(ctx, &CreateBeadRequest{Title: "hi", Type: "task"})
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if b.ID != "gd-new" || b.Title != "hi" {
		t.Errorf("CreateBead = %+v, want gd-new/hi", b)
	}

	got, err := c.GetBead(ctx, "gd-5")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if got.ID != "gd-5" {
		t.Errorf("GetBead ID = %q, want gd-5", got.ID)
	}

	list, err := c.ListBeads(ctx, &ListBeadsRequest{Limit: 10})
	if err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if list.Total != 2 || len(list.Beads) != 2 {
		t.Errorf("ListBeads = %+v, want total 2", list)
	}

	cb, err := c.CloseBead(ctx, "gd-5", "bot")
	if err != nil {
		t.Fatalf("CloseBead: %v", err)
	}
	if cb.Status != "closed" {
		t.Errorf("CloseBead status = %q, want closed", cb.Status)
	}

	if err := c.DeleteBead(ctx, "gd-5"); err != nil {
		t.Fatalf("DeleteBead: %v", err)
	}
}

func TestGRPCClient_Dependencies(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()
	ctx := context.Background()

	dep, err := c.AddDependency(ctx, &AddDependencyRequest{BeadID: "gd-1", DependsOnID: "gd-2", Type: "blocks"})
	if err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	if dep.DependsOnID != "gd-2" {
		t.Errorf("AddDependency = %+v, want depends-on gd-2", dep)
	}

	// The fake leaves the optional `removed` field unset, i.e. exactly what a
	// daemon predating the rows-affected signal sends. Absent must read as
	// removed=true so a real removal is never reported as a no-op.
	removed, err := c.RemoveDependency(ctx, "gd-1", "gd-2", "blocks")
	if err != nil {
		t.Fatalf("RemoveDependency: %v", err)
	}
	if !removed {
		t.Error("removed = false for a daemon that omits the field; want true (assume-success)")
	}

	deps, err := c.GetDependencies(ctx, "gd-1")
	if err != nil {
		t.Fatalf("GetDependencies: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != "gd-dep" {
		t.Errorf("GetDependencies = %+v, want one dep on gd-dep", deps)
	}
}

func TestGRPCClient_LabelsAndComments(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()
	ctx := context.Background()

	bead, err := c.AddLabel(ctx, "gd-1", "urgent")
	if err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if len(bead.Labels) != 1 || bead.Labels[0] != "urgent" {
		t.Errorf("AddLabel labels = %v, want [urgent]", bead.Labels)
	}
	// The fake leaves the optional `removed` field unset, i.e. exactly what a
	// daemon predating the rows-affected signal sends. Absent must read as
	// removed=true so a real removal is never reported as a no-op.
	removed, err := c.RemoveLabel(ctx, "gd-1", "urgent")
	if err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if !removed {
		t.Error("removed = false for a daemon that omits the field; want true (assume-success)")
	}
	labels, err := c.GetLabels(ctx, "gd-1")
	if err != nil {
		t.Fatalf("GetLabels: %v", err)
	}
	if len(labels) != 2 {
		t.Errorf("GetLabels = %v, want 2", labels)
	}

	cm, err := c.AddComment(ctx, "gd-1", "alice", "hello")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if cm.Author != "alice" || cm.Text != "hello" {
		t.Errorf("AddComment = %+v, want alice/hello", cm)
	}
	comments, err := c.GetComments(ctx, "gd-1")
	if err != nil {
		t.Fatalf("GetComments: %v", err)
	}
	if len(comments) != 1 {
		t.Errorf("GetComments = %d, want 1", len(comments))
	}
	deleted, err := c.DeleteComment(ctx, "gd-1", 1)
	if err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	if !deleted {
		t.Error("DeleteComment = false, want true")
	}
	trimmed, err := c.TrimComments(ctx, "gd-1", 2, "")
	if err != nil {
		t.Fatalf("TrimComments: %v", err)
	}
	if len(trimmed) != 2 {
		t.Errorf("TrimComments = %v, want 2 deleted", trimmed)
	}
}

func TestGRPCClient_EventsAndConfig(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()
	ctx := context.Background()

	events, err := c.GetEvents(ctx, "gd-1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("GetEvents = %d, want 1", len(events))
	}

	cfg, err := c.SetConfig(ctx, "view:inbox", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if cfg.Key != "view:inbox" {
		t.Errorf("SetConfig key = %q, want view:inbox", cfg.Key)
	}
	gc, err := c.GetConfig(ctx, "view:inbox")
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if gc.Key != "view:inbox" {
		t.Errorf("GetConfig key = %q, want view:inbox", gc.Key)
	}
	configs, err := c.ListConfigs(ctx, "view")
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 2 {
		t.Errorf("ListConfigs = %d, want 2", len(configs))
	}
	if err := c.DeleteConfig(ctx, "view:inbox"); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}
}

func TestGRPCClient_References(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()
	ctx := context.Background()

	typed := "parent"
	in, err := c.ListBeadsMentioning(ctx, "gd-1", 10, 0, &typed)
	if err != nil {
		t.Fatalf("ListBeadsMentioning: %v", err)
	}
	if in.Total != 1 || len(in.References) != 1 || in.References[0].ID != "gd-ref" {
		t.Errorf("ListBeadsMentioning = %+v, want one ref gd-ref", in)
	}

	out, err := c.ListBeadReferencesFrom(ctx, "gd-1", 0, 0, nil)
	if err != nil {
		t.Fatalf("ListBeadReferencesFrom: %v", err)
	}
	if len(out.References) != 1 || out.References[0].ID != "gd-out" {
		t.Errorf("ListBeadReferencesFrom = %+v, want one ref gd-out", out)
	}
}

func TestGRPCClient_BatchGetBeads(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()

	results, err := c.BatchGetBeads(context.Background(), []string{"gd-1", "gd-2"})
	if err != nil {
		t.Fatalf("BatchGetBeads: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("BatchGetBeads = %d results, want 2", len(results))
	}
	for i, r := range results {
		if r.Bead == nil || r.Bead.ID != r.ID {
			t.Errorf("result[%d] = %+v, want bead with matching ID", i, r)
		}
	}
}

// TestGRPCClient_MetadataThreading verifies SetFleet/SetActor/SetOperatedBy
// surface as x-fleet/x-actor/x-operated-by metadata on outgoing calls.
func TestGRPCClient_MetadataThreading(t *testing.T) {
	c, fake, cleanup := newFakeGRPCClient(t)
	defer cleanup()

	c.SetFleet("fleet-9")
	c.SetActor("actor-9")
	c.SetOperatedBy("op@x.ai")

	if _, err := c.CreateBead(context.Background(), &CreateBeadRequest{Title: "t"}); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if got := fake.lastMD.Get("x-fleet"); len(got) != 1 || got[0] != "fleet-9" {
		t.Errorf("x-fleet = %v, want [fleet-9]", got)
	}
	if got := fake.lastMD.Get("x-actor"); len(got) != 1 || got[0] != "actor-9" {
		t.Errorf("x-actor = %v, want [actor-9]", got)
	}
	if got := fake.lastMD.Get("x-operated-by"); len(got) != 1 || got[0] != "op@x.ai" {
		t.Errorf("x-operated-by = %v, want [op@x.ai]", got)
	}
}

func TestGRPCClient_Close(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()
	if err := c.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

// TestGRPCClient_UnsupportedMethods exercises the transport stubs that
// deliberately error, asserting they fail with a non-nil error rather than
// panicking or silently succeeding.
func TestGRPCClient_UnsupportedMethods(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := c.RestoreBead(ctx, "gd-1"); err == nil {
		t.Error("RestoreBead error = nil, want non-nil")
	}
	if _, err := c.TransferBead(ctx, "gd-1", "f2"); err == nil {
		t.Error("TransferBead error = nil, want non-nil")
	}
	if _, err := c.GetReverseDependencies(ctx, "gd-1"); err == nil {
		t.Error("GetReverseDependencies error = nil, want non-nil")
	}
	if _, err := c.GetAgentRoster(ctx, 60); err == nil {
		t.Error("GetAgentRoster error = nil, want non-nil")
	}
	if _, err := c.ListGates(ctx, "gd-1"); err == nil {
		t.Error("ListGates error = nil, want non-nil")
	}
	if err := c.SatisfyGate(ctx, "gd-1", "g"); err == nil {
		t.Error("SatisfyGate error = nil, want non-nil")
	}
	if err := c.ClearGate(ctx, "gd-1", "g"); err == nil {
		t.Error("ClearGate error = nil, want non-nil")
	}
	if _, err := c.EmitHook(ctx, &EmitHookRequest{}); err == nil {
		t.Error("EmitHook error = nil, want non-nil")
	}
	if _, err := c.FindSimilar(ctx, "gd-1", 5, 0); err == nil {
		t.Error("FindSimilar error = nil, want non-nil")
	}
	if _, err := c.SemanticSearch(ctx, &SemanticSearchRequest{}); err == nil {
		t.Error("SemanticSearch error = nil, want non-nil")
	}
	if _, err := c.ListGlobalEvents(ctx, &ListGlobalEventsRequest{}); err == nil {
		t.Error("ListGlobalEvents error = nil, want non-nil")
	}
	if _, err := c.GetMutations(ctx, "gd-1"); err == nil {
		t.Error("GetMutations error = nil, want non-nil")
	}
	if _, err := c.UndoMutation(ctx, 1); err == nil {
		t.Error("UndoMutation error = nil, want non-nil")
	}
	if err := c.UpsertEmbedding(ctx, &UpsertEmbeddingRequest{}); err == nil {
		t.Error("UpsertEmbedding error = nil, want non-nil")
	}
	if _, err := c.GetEmbeddingHashes(ctx, nil); err == nil {
		t.Error("GetEmbeddingHashes error = nil, want non-nil")
	}
	if _, err := c.SyncBatchEmbeddings(ctx, &SyncBatchEmbeddingsRequest{}); err == nil {
		t.Error("SyncBatchEmbeddings error = nil, want non-nil")
	}
	if err := c.MigrateEmbeddings(ctx, &MigrateEmbeddingsRequest{}); err == nil {
		t.Error("MigrateEmbeddings error = nil, want non-nil")
	}
	if err := c.ReindexEmbeddings(ctx); err == nil {
		t.Error("ReindexEmbeddings error = nil, want non-nil")
	}
	if _, err := c.ReextractReferences(ctx, &ReextractReferencesRequest{}); err == nil {
		t.Error("ReextractReferences error = nil, want non-nil")
	}
	if _, err := c.BackfillOperators(ctx, &BackfillOperatorsRequest{}); err == nil {
		t.Error("BackfillOperators error = nil, want non-nil")
	}
	if _, err := c.BackfillOperatedByLegacy(ctx, &BackfillOperatedByLegacyRequest{}); err == nil {
		t.Error("BackfillOperatedByLegacy error = nil, want non-nil")
	}
	if _, err := c.BackfillSlackUserID(ctx, &BackfillSlackUserIDRequest{}); err == nil {
		t.Error("BackfillSlackUserID error = nil, want non-nil")
	}
	if _, err := c.BackfillOwner(ctx, &BackfillOwnerRequest{}); err == nil {
		t.Error("BackfillOwner error = nil, want non-nil")
	}
	if _, err := c.BackfillEmptyOwner(ctx, &BackfillEmptyOwnerRequest{}); err == nil {
		t.Error("BackfillEmptyOwner error = nil, want non-nil")
	}
	if _, err := c.BackfillOperatedByAgents(ctx, &BackfillOperatedByAgentsRequest{}); err == nil {
		t.Error("BackfillOperatedByAgents error = nil, want non-nil")
	}
	if _, err := c.BackfillParentChildDeps(ctx, &BackfillParentChildDepsRequest{}); err == nil {
		t.Error("BackfillParentChildDeps error = nil, want non-nil")
	}
	if _, err := c.RepairReversedParentChildDeps(ctx, &RepairReversedParentChildDepsRequest{}); err == nil {
		t.Error("RepairReversedParentChildDeps error = nil, want non-nil")
	}
	if _, err := c.BackfillTriggeredByDeps(ctx, &BackfillTriggeredByDepsRequest{}); err == nil {
		t.Error("BackfillTriggeredByDeps error = nil, want non-nil")
	}
	if _, err := c.CloseTriggerAuditBeads(ctx, &CloseTriggerAuditBeadsRequest{}); err == nil {
		t.Error("CloseTriggerAuditBeads error = nil, want non-nil")
	}
}

// TestTokenCredentials verifies the PerRPCCredentials shim emits a Bearer
// header and does not require transport security.
func TestTokenCredentials(t *testing.T) {
	tc := tokenCredentials{token: "abc"}
	md, err := tc.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if md["authorization"] != "Bearer abc" {
		t.Errorf("authorization = %q, want 'Bearer abc'", md["authorization"])
	}
	if tc.RequireTransportSecurity() {
		t.Error("RequireTransportSecurity = true, want false")
	}
}

// TestNewGRPCClient verifies the constructor builds a usable client for both
// the token and no-token paths (grpc.NewClient is lazy, so no server needed).
func TestNewGRPCClient(t *testing.T) {
	c, err := NewGRPCClient("passthrough:///x", "")
	if err != nil {
		t.Fatalf("NewGRPCClient(no token): %v", err)
	}
	_ = c.Close()

	c2, err := NewGRPCClient("passthrough:///x", "tok")
	if err != nil {
		t.Fatalf("NewGRPCClient(token): %v", err)
	}
	_ = c2.Close()
}

// TestGRPCClient_ListBeads_FieldFiltersReachTheWire pins that a `-f/--field`
// predicate is actually SENT over gRPC. Unlike --exclude-label/--exclude-type/
// --parent below (which the proto genuinely cannot express, so the client fails
// loud), field_filters HAS existed on the proto since beads.proto field 11 and
// the server honors it (internal/server/list_filter.go) — but this client built
// pbReq without it, so every `-f` predicate was silently dropped and the
// response came back UNFILTERED: `kd count -f spawned_by=* --transport=grpc`
// answered with the TOTAL bead count instead of the matching one. That is the
// widen-on-drop footgun the sibling guards exist to kill, and it was invisible
// to any test asserting only on the client-side struct — hence the assertion
// here is against the request as the SERVER received it (ka-0000004030).
func TestGRPCClient_ListBeads_FieldFiltersReachTheWire(t *testing.T) {
	c, fake, cleanup := newFakeGRPCClient(t)
	defer cleanup()

	_, err := c.ListBeads(context.Background(), &ListBeadsRequest{
		FieldFilters: map[string]string{"spawned_by": "*", "role": "gardener"},
	})
	if err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if fake.lastListBeads == nil {
		t.Fatal("server received no ListBeads request")
	}
	got := fake.lastListBeads.GetFieldFilters()
	if got["spawned_by"] != "*" {
		t.Errorf("field_filters[spawned_by] = %q on the wire, want %q — a dropped predicate returns an unfiltered (wider) set", got["spawned_by"], "*")
	}
	if got["role"] != "gardener" {
		t.Errorf("field_filters[role] = %q on the wire, want %q", got["role"], "gardener")
	}
}

// TestGRPCClient_ListBeads_ExcludeLabelsUnsupported pins that --exclude-label
// over gRPC fails LOUDLY rather than silently dropping the filter. The proto
// BeadFilter has no exclude_labels field, so a dropped negative-label filter
// would return a WIDER set than asked for — the exact false-result footgun the
// flag exists to kill (ka-0000007103). The default kd transport is HTTP, where
// the filter is honored.
func TestGRPCClient_ListBeads_ExcludeLabelsUnsupported(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()

	_, err := c.ListBeads(context.Background(), &ListBeadsRequest{
		ExcludeLabels: []string{"needs-intake"},
	})
	if err == nil {
		t.Fatal("ListBeads with ExcludeLabels over gRPC must error, got nil")
	}
	if !strings.Contains(err.Error(), "exclude-label") || !strings.Contains(err.Error(), "http") {
		t.Errorf("error %q should mention --exclude-label and the http transport", err)
	}
}

// TestGRPCClient_ListBeads_UnassignedUnsupported pins that --unassigned over
// gRPC fails LOUDLY rather than silently dropping the filter. The proto
// ListBeadsRequest has no unassigned field, and a dropped predicate here is
// especially bad: the caller asked for UNCLAIMED work and would get every bead
// in scope, claimed ones included — so a work-seeking agent could pick up a task
// another agent is already driving (ka-0000007209). HTTP (the default) honors it.
func TestGRPCClient_ListBeads_UnassignedUnsupported(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()

	_, err := c.ListBeads(context.Background(), &ListBeadsRequest{Unassigned: true})
	if err == nil {
		t.Fatal("ListBeads with Unassigned over gRPC must error, got nil")
	}
	if !strings.Contains(err.Error(), "unassigned") || !strings.Contains(err.Error(), "http") {
		t.Errorf("error %q should mention --unassigned and the http transport", err)
	}
}

// TestGRPCClient_ListBeads_WithOpenBlockersUnsupported pins that
// --with-blockers over gRPC fails LOUDLY. Silently dropping an ENRICHMENT is
// subtler than dropping a filter and worse here: the right rows come back, but
// every one carries no open_blockers, which reads as "nothing is blocked" —
// the false all-clear the flag exists to prevent (ka-0000000945).
func TestGRPCClient_ListBeads_WithOpenBlockersUnsupported(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()

	_, err := c.ListBeads(context.Background(), &ListBeadsRequest{WithOpenBlockers: true})
	if err == nil {
		t.Fatal("ListBeads with WithOpenBlockers over gRPC must error, got nil")
	}
	if !strings.Contains(err.Error(), "with-blockers") || !strings.Contains(err.Error(), "http") {
		t.Errorf("error %q should mention --with-blockers and the http transport", err)
	}
}

// TestGRPCClient_ListBeads_ExcludeTypesUnsupported pins that --exclude-type
// over gRPC fails LOUDLY rather than silently dropping the filter. The proto
// ListBeadsRequest has no exclude_type field, so a dropped negative-type
// filter would return a WIDER set than asked for — the same false-result
// footgun the ExcludeLabels guard kills (ka-0000007843). The default kd
// transport is HTTP, where the filter is honored.
func TestGRPCClient_ListBeads_ExcludeTypesUnsupported(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()

	_, err := c.ListBeads(context.Background(), &ListBeadsRequest{
		ExcludeTypes: []string{"message"},
	})
	if err == nil {
		t.Fatal("ListBeads with ExcludeTypes over gRPC must error, got nil")
	}
	if !strings.Contains(err.Error(), "exclude-type") || !strings.Contains(err.Error(), "http") {
		t.Errorf("error %q should mention --exclude-type and the http transport", err)
	}
}

// TestGRPCClient_ListBeads_ParentUnsupported pins that --parent over gRPC
// fails LOUDLY rather than silently dropping the filter. The proto
// ListBeadsRequest has no parent field, so a dropped parent filter would
// return the WHOLE project instead of the epic's children — the false
// all-clear that drove the meta-gardener false STALE-STOP (ka-0000000208).
// The default kd transport is HTTP, where the filter is honored.
func TestGRPCClient_ListBeads_ParentUnsupported(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()

	_, err := c.ListBeads(context.Background(), &ListBeadsRequest{
		Parent: "gd-Epic0000001",
	})
	if err == nil {
		t.Fatal("ListBeads with Parent over gRPC must error, got nil")
	}
	if !strings.Contains(err.Error(), "parent") || !strings.Contains(err.Error(), "http") {
		t.Errorf("error %q should mention --parent and the http transport", err)
	}
}

// TestGRPCClient_ListBeads_TimeWindowUnsupported pins that the time-window
// filters over gRPC fail LOUDLY rather than silently dropping the window. The
// proto BeadFilter has no time-window fields, so a dropped cutoff would return
// the full unwindowed set — the exact footgun the flags exist to kill
// (ka-0000008195). The default kd transport is HTTP, where they are honored.
func TestGRPCClient_ListBeads_TimeWindowUnsupported(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()

	cutoff := time.Now().Add(-24 * time.Hour)
	for _, tc := range []struct {
		name string
		req  ListBeadsRequest
	}{
		{"created_after", ListBeadsRequest{CreatedAfter: &cutoff}},
		{"created_before", ListBeadsRequest{CreatedBefore: &cutoff}},
		{"updated_after", ListBeadsRequest{UpdatedAfter: &cutoff}},
		{"updated_before", ListBeadsRequest{UpdatedBefore: &cutoff}},
		{"closed_after", ListBeadsRequest{ClosedAfter: &cutoff}},
		{"closed_before", ListBeadsRequest{ClosedBefore: &cutoff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ListBeads(context.Background(), &tc.req)
			if err == nil {
				t.Fatalf("ListBeads with %s over gRPC must error, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "time-window") || !strings.Contains(err.Error(), "http") {
				t.Errorf("error %q should mention time-window filters and the http transport", err)
			}
		})
	}
}

// TestGRPCClient_ListBeads_NoExcludeLabelsStillWorks guards that the guard only
// fires when ExcludeLabels is non-empty — an ordinary gRPC list is unaffected.
func TestGRPCClient_ListBeads_NoExcludeLabelsStillWorks(t *testing.T) {
	c, _, cleanup := newFakeGRPCClient(t)
	defer cleanup()

	resp, err := c.ListBeads(context.Background(), &ListBeadsRequest{Limit: 10})
	if err != nil {
		t.Fatalf("ListBeads without ExcludeLabels must succeed over gRPC, got: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2 (fake server)", resp.Total)
	}
}
