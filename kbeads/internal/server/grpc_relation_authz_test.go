package server

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// The gRPC relation RPCs (AddLabel, RemoveLabel, AddComment, DeleteComment,
// TrimComments, AddDependency, RemoveDependency) run the same token-project,
// write_policy and ownership gates as their HTTP twins (gd-FiWt1xkXzb,
// gd-G0DPygReo5). Before, they ran only the scoped
// principal gate, so a static or fleet token could move a type:permission
// bead from project:gardenio to another land's project label, and whoever
// answers that land's cards could then answer gardenio's tool call.

// relationCall is one relation RPC against bead id.
type relationCall struct {
	name string
	call func(srv *BeadsServer, ctx context.Context, id string) error
}

// relationCalls are the seven relation RPCs. Every one writes to id; the
// dependency target and the moved-to label point at another land, and the
// comment delete and trim remove the bead's seeded comment.
func relationCalls() []relationCall {
	return []relationCall{
		{"AddLabel", func(srv *BeadsServer, ctx context.Context, id string) error {
			_, err := srv.AddLabel(ctx, &beadsv1.AddLabelRequest{BeadId: id, Label: "project:claimlayer"})
			return err
		}},
		{"RemoveLabel", func(srv *BeadsServer, ctx context.Context, id string) error {
			_, err := srv.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{BeadId: id, Label: "project:gardenio"})
			return err
		}},
		{"AddComment", func(srv *BeadsServer, ctx context.Context, id string) error {
			_, err := srv.AddComment(ctx, &beadsv1.AddCommentRequest{BeadId: id, Text: "allow"})
			return err
		}},
		{"DeleteComment", func(srv *BeadsServer, ctx context.Context, id string) error {
			_, err := srv.DeleteComment(ctx, &beadsv1.DeleteCommentRequest{BeadId: id, CommentId: seededCommentID})
			return err
		}},
		{"TrimComments", func(srv *BeadsServer, ctx context.Context, id string) error {
			_, err := srv.TrimComments(ctx, &beadsv1.TrimCommentsRequest{BeadId: id, KeepLast: 0})
			return err
		}},
		{"AddDependency", func(srv *BeadsServer, ctx context.Context, id string) error {
			_, err := srv.AddDependency(ctx, &beadsv1.AddDependencyRequest{BeadId: id, DependsOnId: "gd-other", Type: "related"})
			return err
		}},
		{"RemoveDependency", func(srv *BeadsServer, ctx context.Context, id string) error {
			_, err := srv.RemoveDependency(ctx, &beadsv1.RemoveDependencyRequest{BeadId: id, DependsOnId: "gd-other", Type: "related"})
			return err
		}},
	}
}

// seededCommentID is the id of the one comment seedRelationComment stores.
const seededCommentID int64 = 1

// seedRelationComment stores one existing comment on id for DeleteComment and
// TrimComments to find.
func seedRelationComment(ms *mockStore, id string) {
	ms.comments[id] = []*model.Comment{{ID: seededCommentID, BeadID: id, Author: "alice@example.com", Text: "history"}}
	ms.commentNextID = seededCommentID
}

// seedRelationFixture stores the permission bead, its dependency target, one
// existing edge for RemoveDependency to find and one comment.
func seedRelationFixture(ms *mockStore) {
	ms.beads["gd-perm1"] = permissionBead("gd-perm1")
	ms.labels["gd-perm1"] = []string{"project:gardenio"}
	seedBead(ms, "gd-other", model.TypeTask, model.StatusOpen, []string{"project:gardenio"}, nil)
	ms.deps["gd-perm1"] = []*model.Dependency{{BeadID: "gd-perm1", DependsOnID: "gd-other", Type: "related"}}
	seedRelationComment(ms, "gd-perm1")
}

// requireRelationsUntouched fails if a refused call changed the bead's labels,
// comments or edges, or emitted a mutation event.
func requireRelationsUntouched(t *testing.T, ms *mockStore, id string) {
	t.Helper()
	if got := ms.labels[id]; !slices.Equal(got, []string{"project:gardenio"}) {
		t.Errorf("labels = %v, want [project:gardenio]", got)
	}
	if cs := ms.comments[id]; len(cs) != 1 || cs[0].ID != seededCommentID {
		t.Errorf("comments = %+v, want only the seeded comment", cs)
	}
	if len(ms.deps[id]) != 1 {
		t.Errorf("deps = %+v, want only the seeded edge", ms.deps[id])
	}
	assertNoMutationEvent(t, ms)
}

// deniedRelationCtxs are deniedPermissionCtxs plus a land principal for the
// land the bead would be moved to. Land principals are refused at the gRPC
// interceptor (TestLandPrincipal_GRPCRefused); the handlers refuse them too.
func deniedRelationCtxs() map[string]context.Context {
	ctxs := deniedPermissionCtxs()
	ctxs["land principal (claimlayer)"] = landPrincipalContext(context.Background(), nil, "claimlayer")
	ctxs["land principal (gardenio)"] = landPrincipalContext(context.Background(), nil, "gardenio")
	return ctxs
}

func TestGRPCRelations_PermissionBead_Denied(t *testing.T) {
	for name, ctx := range deniedRelationCtxs() {
		for _, rc := range relationCalls() {
			t.Run(name+"/"+rc.name, func(t *testing.T) {
				srv, ms, _ := testCtx(t)
				seedRelationFixture(ms)
				// Another land's principal cannot read gd-perm1, so it answers
				// as missing (gd-7APTAIhdC5).
				want := codes.PermissionDenied
				if name == "land principal (claimlayer)" {
					want = codes.NotFound
				}
				requireCode(t, rc.call(srv, ctx, "gd-perm1"), want)
				requireRelationsUntouched(t, ms, "gd-perm1")
			})
		}
	}
}

func TestGRPCRelations_PermissionBead_AdminAllowed(t *testing.T) {
	ctx := oidcCtx("owner@example.com", []string{RoleAdmin})
	for _, rc := range relationCalls() {
		t.Run(rc.name, func(t *testing.T) {
			srv, ms, _ := testCtx(t)
			seedRelationFixture(ms)
			if err := rc.call(srv, ctx, "gd-perm1"); err != nil {
				t.Fatalf("admin %s: %v", rc.name, err)
			}
			if rc.name == "DeleteComment" || rc.name == "TrimComments" {
				if cs := ms.comments["gd-perm1"]; len(cs) != 0 {
					t.Errorf("admin %s left comments %+v", rc.name, cs)
				}
			}
		})
	}
}

// Comment-create skips the ownership gate (ka-0000005884), as on HTTP; the
// label, dependency and comment delete/trim RPCs keep it. A fleet token that
// is not the bead's creator, assignee or owner may comment on a task but not
// relabel, link it or remove its comments.
func TestGRPCRelations_OwnershipGate(t *testing.T) {
	ctx := fleetCtxWithFleet("agent-gd-other", "agent-gd-other")
	for _, rc := range relationCalls() {
		t.Run(rc.name, func(t *testing.T) {
			srv, ms, _ := testCtx(t)
			seedRelationFixture(ms)
			task := ownedBead("gd-task1", "alice@example.com")
			task.Labels = []string{"project:gardenio"}
			ms.beads["gd-task1"] = task
			ms.labels["gd-task1"] = []string{"project:gardenio"}
			ms.deps["gd-task1"] = []*model.Dependency{{BeadID: "gd-task1", DependsOnID: "gd-other", Type: "related"}}
			seedRelationComment(ms, "gd-task1")

			err := rc.call(srv, ctx, "gd-task1")
			if rc.name == "AddComment" {
				if err != nil {
					t.Fatalf("non-owner comment: %v", err)
				}
				return
			}
			requireCode(t, err, codes.PermissionDenied)
			requireRelationsUntouched(t, ms, "gd-task1")
		})
	}
}

// A project-bound token may not write relations on another project's bead,
// which answers as a missing one (gd-7APTAIhdC5).
func TestGRPCRelations_TokenProjectGate(t *testing.T) {
	ctx := WithTokenProject(staticCtx(), "claimlayer")
	for _, rc := range relationCalls() {
		t.Run(rc.name, func(t *testing.T) {
			srv, ms, _ := testCtx(t)
			seedRelationFixture(ms)
			seedBead(ms, "gd-task2", model.TypeTask, model.StatusOpen, []string{"project:gardenio"}, nil)
			ms.labels["gd-task2"] = []string{"project:gardenio"}
			ms.deps["gd-task2"] = []*model.Dependency{{BeadID: "gd-task2", DependsOnID: "gd-other", Type: "related"}}
			seedRelationComment(ms, "gd-task2")

			requireCode(t, rc.call(srv, ctx, "gd-task2"), codes.NotFound)
			requireRelationsUntouched(t, ms, "gd-task2")
		})
	}
}

func TestGRPCRelations_MissingBeadNotFound(t *testing.T) {
	for _, rc := range relationCalls() {
		t.Run(rc.name, func(t *testing.T) {
			srv, _, ctx := testCtx(t)
			requireCode(t, rc.call(srv, ctx, "gd-missing"), codes.NotFound)
		})
	}
}
