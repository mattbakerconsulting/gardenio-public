package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testCtx creates a fresh BeadsServer with a mock store and an OIDC-attributed
// context (subject = defaultTestSubject). Tests that need to exercise the
// anonymous path explicitly should construct their own context.Background().
// The default OIDC identity is required after R2 (ka-0000005772) which made
// write paths fail-closed when no operator can be resolved.
func testCtx(t *testing.T) (*BeadsServer, *mockStore, context.Context) {
	t.Helper()
	return testCtxAs(t, defaultTestSubject)
}

// testCtxAs is like testCtx but attaches an OIDC AuthIdentity with the given
// subject to the returned context. Used by gRPC mutation tests that need the
// server to derive an actor from the context (AUDIT-2026-04-18 §C3).
func testCtxAs(t *testing.T, subject string) (*BeadsServer, *mockStore, context.Context) {
	t.Helper()
	srv, ms, _ := newTestServer()
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: subject})
	return srv, ms, ctx
}

// requireCode asserts that err is a gRPC error with the given status code.
func requireCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected gRPC error with code %v, got nil", code)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != code {
		t.Fatalf("expected code=%v, got %v", code, st.Code())
	}
}

// requireEvent asserts exactly n events were recorded, with the last having the given topic.
func requireEvent(t *testing.T, ms *mockStore, n int, topic string) {
	t.Helper()
	if len(ms.events) != n {
		t.Fatalf("expected %d event(s), got %d", n, len(ms.events))
	}
	if ms.events[n-1].Topic != topic {
		t.Fatalf("expected topic=%q, got %q", topic, ms.events[n-1].Topic)
	}
}

func TestGRPCErrorCodes(t *testing.T) {
	title := "x"
	for _, tc := range []struct {
		name string
		call func(*BeadsServer, context.Context) error
		code codes.Code
	}{
		// Beads
		{"CreateBead/MissingTitle", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.CreateBead(ctx, &beadsv1.CreateBeadRequest{Type: "task"})
			return err
		}, codes.InvalidArgument},
		{"GetBead/MissingID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.GetBead(ctx, &beadsv1.GetBeadRequest{})
			return err
		}, codes.InvalidArgument},
		{"GetBead/NotFound", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.GetBead(ctx, &beadsv1.GetBeadRequest{Id: "nonexistent"})
			return err
		}, codes.NotFound},
		{"UpdateBead/MissingID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Title: &title})
			return err
		}, codes.InvalidArgument},
		{"UpdateBead/NotFound", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "nonexistent", Title: &title})
			return err
		}, codes.NotFound},
		{"CloseBead/MissingID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.CloseBead(ctx, &beadsv1.CloseBeadRequest{})
			return err
		}, codes.InvalidArgument},
		{"CloseBead/NotFound", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "nonexistent"})
			return err
		}, codes.NotFound},
		{"DeleteBead/MissingID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.DeleteBead(ctx, &beadsv1.DeleteBeadRequest{})
			return err
		}, codes.InvalidArgument},
		{"DeleteBead/NotFound", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.DeleteBead(ctx, &beadsv1.DeleteBeadRequest{Id: "nonexistent"})
			return err
		}, codes.NotFound},

		// Dependencies
		{"AddDependency/MissingBeadID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.AddDependency(ctx, &beadsv1.AddDependencyRequest{DependsOnId: "gd-b", Type: "blocks"})
			return err
		}, codes.InvalidArgument},
		{"AddDependency/MissingDependsOnID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.AddDependency(ctx, &beadsv1.AddDependencyRequest{BeadId: "gd-a", Type: "blocks"})
			return err
		}, codes.InvalidArgument},
		{"RemoveDependency/MissingBeadID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.RemoveDependency(ctx, &beadsv1.RemoveDependencyRequest{DependsOnId: "gd-b"})
			return err
		}, codes.InvalidArgument},
		{"RemoveDependency/MissingDependsOnID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.RemoveDependency(ctx, &beadsv1.RemoveDependencyRequest{BeadId: "gd-a"})
			return err
		}, codes.InvalidArgument},
		{"GetDependencies/MissingBeadID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.GetDependencies(ctx, &beadsv1.GetDependenciesRequest{})
			return err
		}, codes.InvalidArgument},

		// Labels
		{"AddLabel/MissingBeadID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.AddLabel(ctx, &beadsv1.AddLabelRequest{Label: "urgent"})
			return err
		}, codes.InvalidArgument},
		{"AddLabel/MissingLabel", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.AddLabel(ctx, &beadsv1.AddLabelRequest{BeadId: "gd-a"})
			return err
		}, codes.InvalidArgument},
		{"RemoveLabel/MissingBeadID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{Label: "urgent"})
			return err
		}, codes.InvalidArgument},
		{"RemoveLabel/MissingLabel", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{BeadId: "gd-a"})
			return err
		}, codes.InvalidArgument},
		{"GetLabels/MissingBeadID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.GetLabels(ctx, &beadsv1.GetLabelsRequest{})
			return err
		}, codes.InvalidArgument},

		// Comments
		{"AddComment/MissingBeadID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.AddComment(ctx, &beadsv1.AddCommentRequest{Text: "hello"})
			return err
		}, codes.InvalidArgument},
		{"AddComment/MissingText", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.AddComment(ctx, &beadsv1.AddCommentRequest{BeadId: "gd-a"})
			return err
		}, codes.InvalidArgument},
		{"GetComments/MissingBeadID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.GetComments(ctx, &beadsv1.GetCommentsRequest{})
			return err
		}, codes.InvalidArgument},

		// Events
		{"GetEvents/MissingBeadID", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.GetEvents(ctx, &beadsv1.GetEventsRequest{})
			return err
		}, codes.InvalidArgument},

		// Configs
		{"SetConfig/MissingKey", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.SetConfig(ctx, &beadsv1.SetConfigRequest{Value: []byte(`{}`)})
			return err
		}, codes.InvalidArgument},
		{"GetConfig/MissingKey", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.GetConfig(ctx, &beadsv1.GetConfigRequest{})
			return err
		}, codes.InvalidArgument},
		{"GetConfig/NotFound", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.GetConfig(ctx, &beadsv1.GetConfigRequest{Key: "view:nonexistent"})
			return err
		}, codes.NotFound},
		{"ListConfigs/MissingNamespace", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.ListConfigs(ctx, &beadsv1.ListConfigsRequest{})
			return err
		}, codes.InvalidArgument},
		{"DeleteConfig/MissingKey", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.DeleteConfig(ctx, &beadsv1.DeleteConfigRequest{})
			return err
		}, codes.InvalidArgument},
		{"DeleteConfig/NotFound", func(s *BeadsServer, ctx context.Context) error {
			_, err := s.DeleteConfig(ctx, &beadsv1.DeleteConfigRequest{Key: "view:nonexistent"})
			return err
		}, codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, ctx := testCtx(t)
			requireCode(t, tc.call(srv, ctx), tc.code)
		})
	}
}

func TestGRPCAddDependency(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	seedBead(ms, "gd-a", model.TypeTask, model.StatusOpen, nil, nil)
	resp, err := srv.AddDependency(ctx, &beadsv1.AddDependencyRequest{
		BeadId: "gd-a", DependsOnId: "gd-b", Type: "blocks", CreatedBy: "alice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Dependency.BeadId != "gd-a" || resp.Dependency.DependsOnId != "gd-b" {
		t.Fatalf("got bead_id=%q depends_on_id=%q", resp.Dependency.BeadId, resp.Dependency.DependsOnId)
	}
	requireEvent(t, ms, 1, "beads.dependency.added")
}

func TestGRPCRemoveDependency(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	seedBead(ms, "gd-a", model.TypeTask, model.StatusOpen, nil, nil)
	ms.deps["gd-a"] = []*model.Dependency{
		{BeadID: "gd-a", DependsOnID: "gd-b", Type: model.DepBlocks},
	}
	resp, err := srv.RemoveDependency(ctx, &beadsv1.RemoveDependencyRequest{
		BeadId: "gd-a", DependsOnId: "gd-b", Type: "blocks",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Removed == nil || !resp.GetRemoved() {
		t.Errorf("removed = %v; want an explicit true for a matched edge", resp.Removed)
	}
	requireEvent(t, ms, 1, "beads.dependency.removed")
}

// TestGRPCRemoveDependency_NoMatch pins the no-op: removing an absent edge is
// NOT an error (idempotent callers depend on that), reports removed=false
// explicitly, and records neither an event nor an audit row (ka-0000001966).
func TestGRPCRemoveDependency_NoMatch(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	seedBead(ms, "gd-a", model.TypeTask, model.StatusOpen, nil, nil)
	resp, err := srv.RemoveDependency(ctx, &beadsv1.RemoveDependencyRequest{
		BeadId: "gd-a", DependsOnId: "gd-b", Type: "parent-child",
	})
	if err != nil {
		t.Fatalf("a 0-match must not error: %v", err)
	}
	if resp.Removed == nil {
		t.Fatal("removed is absent; want an explicit false so clients can tell it from an old daemon")
	}
	if resp.GetRemoved() {
		t.Error("removed = true for an edge that never existed; want false")
	}
	if len(ms.events) != 0 {
		t.Errorf("expected no event for a no-op remove, got %d", len(ms.events))
	}
}

func TestGRPCGetDependencies(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.deps["gd-a"] = []*model.Dependency{{BeadID: "gd-a", DependsOnID: "gd-b", Type: model.DepBlocks}}

	resp, err := srv.GetDependencies(ctx, &beadsv1.GetDependenciesRequest{BeadId: "gd-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Dependencies) != 1 || resp.Dependencies[0].DependsOnId != "gd-b" {
		t.Fatalf("expected 1 dependency with depends_on_id=gd-b, got %v", resp.Dependencies)
	}
}

func TestGRPCAddLabel(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.beads["gd-lbl1"] = &model.Bead{ID: "gd-lbl1", Title: "Bead", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	resp, err := srv.AddLabel(ctx, &beadsv1.AddLabelRequest{BeadId: "gd-lbl1", Label: "urgent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Id != "gd-lbl1" {
		t.Fatalf("got bead_id=%q", resp.Bead.Id)
	}
	if len(ms.labels["gd-lbl1"]) != 1 {
		t.Fatalf("expected 1 label, got %d", len(ms.labels["gd-lbl1"]))
	}
	requireEvent(t, ms, 1, "beads.label.added")
}

func TestGRPCRemoveLabel(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	seedBead(ms, "gd-a", model.TypeTask, model.StatusOpen, nil, nil)
	ms.labels["gd-a"] = []string{"urgent"}
	resp, err := srv.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{BeadId: "gd-a", Label: "urgent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Removed == nil || !resp.GetRemoved() {
		t.Errorf("removed = %v; want an explicit true for a matched label", resp.Removed)
	}
	requireEvent(t, ms, 1, "beads.label.removed")
}

// TestGRPCRemoveLabel_NoMatch pins the no-op: removing an absent label is NOT
// an error (idempotent callers depend on that), reports removed=false
// explicitly, and records no event (ka-0000000424).
func TestGRPCRemoveLabel_NoMatch(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	seedBead(ms, "gd-a", model.TypeTask, model.StatusOpen, nil, nil)
	resp, err := srv.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{BeadId: "gd-a", Label: "not-a-real-label"})
	if err != nil {
		t.Fatalf("a 0-match must not error: %v", err)
	}
	if resp.Removed == nil {
		t.Fatal("removed is absent; want an explicit false so clients can tell it from an old daemon")
	}
	if resp.GetRemoved() {
		t.Error("removed = true for a label that never existed; want false")
	}
	if len(ms.events) != 0 {
		t.Errorf("expected no event for a no-op remove, got %d", len(ms.events))
	}
}

func TestGRPCGetLabels(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.labels["gd-a"] = []string{"urgent", "frontend"}

	resp, err := srv.GetLabels(ctx, &beadsv1.GetLabelsRequest{BeadId: "gd-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Labels) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(resp.Labels))
	}
}

func TestGRPCAddComment(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "bob")
	seedBead(ms, "gd-cmt1", model.TypeTask, model.StatusOpen, nil, nil)
	// SECURITY (AUDIT §C3): client-supplied Author must be ignored.
	resp, err := srv.AddComment(ctx, &beadsv1.AddCommentRequest{BeadId: "gd-cmt1", Author: "evil-forged", Text: "Hello world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c := resp.Comment
	if c.BeadId != "gd-cmt1" || c.Text != "Hello world" || c.Author != "bob" {
		t.Fatalf("got bead_id=%q text=%q author=%q", c.BeadId, c.Text, c.Author)
	}
	requireEvent(t, ms, 1, "beads.comment.added")
}

func TestGRPCGetComments(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.comments["gd-a"] = []*model.Comment{
		{ID: 1, BeadID: "gd-a", Author: "bob", Text: "Comment 1"},
		{ID: 2, BeadID: "gd-a", Author: "alice", Text: "Comment 2"},
	}
	resp, err := srv.GetComments(ctx, &beadsv1.GetCommentsRequest{BeadId: "gd-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Comments) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(resp.Comments))
	}
}

func TestGRPCGetEvents(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.events = []*model.Event{
		{ID: 1, Topic: "beads.bead.created", BeadID: "gd-a", Payload: json.RawMessage(`{}`)},
		{ID: 2, Topic: "beads.bead.updated", BeadID: "gd-a", Payload: json.RawMessage(`{}`)},
		{ID: 3, Topic: "beads.bead.created", BeadID: "gd-b", Payload: json.RawMessage(`{}`)},
	}
	resp, err := srv.GetEvents(ctx, &beadsv1.GetEventsRequest{BeadId: "gd-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(resp.Events))
	}
}

func TestGRPCHealth(t *testing.T) {
	srv, _, ctx := testCtx(t)
	resp, err := srv.Health(ctx, &beadsv1.HealthRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("got status=%q", resp.Status)
	}
}

func TestLoggingInterceptor(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	resp, err := LoggingInterceptor(context.Background(), nil, info,
		func(ctx context.Context, req any) (any, error) { return "ok", nil })
	if err != nil || resp != "ok" {
		t.Fatalf("expected resp=%q err=nil, got resp=%v err=%v", "ok", resp, err)
	}

	_, err = LoggingInterceptor(context.Background(), nil, info,
		func(ctx context.Context, req any) (any, error) { return nil, fmt.Errorf("boom") })
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRecoveryInterceptor(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	resp, err := RecoveryInterceptor(context.Background(), nil, info,
		func(ctx context.Context, req any) (any, error) { return "ok", nil })
	if err != nil || resp != "ok" {
		t.Fatalf("expected resp=%q err=nil, got resp=%v err=%v", "ok", resp, err)
	}

	_, err = RecoveryInterceptor(context.Background(), nil, info,
		func(_ context.Context, _ any) (any, error) { panic("test panic") })
	requireCode(t, err, codes.Internal)
}
