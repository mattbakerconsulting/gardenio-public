package server

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

// short_title over gRPC (plan gd-YZATAIOhfW, task gd-WSb4yxXFI6). These tests
// wire the REAL client.GRPCClient to the REAL BeadsServer over a loopback
// socket, so a proto field the client forgets to set or the server forgets to
// read fails here instead of passing two independently-mocked halves. The
// store is the only mock. The HTTP shape is pinned in beads_short_title_test.go.

// newGRPCRoundtripClient serves a BeadsServer on 127.0.0.1 behind an
// interceptor that attaches an OIDC identity (an anonymous create fails
// closed) and returns a real GRPCClient dialled at it.
func newGRPCRoundtripClient(t *testing.T) (*client.GRPCClient, *mockStore) {
	t.Helper()
	srv, ms, _ := newTestServer()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		return h(WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: defaultTestSubject}), req)
	}))
	beadsv1.RegisterBeadsServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	c, err := client.NewGRPCClient(lis.Addr().String(), "")
	if err != nil {
		t.Fatalf("NewGRPCClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ms
}

func TestGRPC_ShortTitle_CreateGetListRoundTrip(t *testing.T) {
	c, _ := newGRPCRoundtripClient(t)
	ctx := context.Background()

	named, err := c.CreateBead(ctx, &client.CreateBeadRequest{
		Title:      "Forward duration_ms through the gb hook relay so the trace panel sees it",
		ShortTitle: "  forward  duration_ms via\tgb hook relay ",
		Type:       "task",
		Labels:     []string{"project:gardenio"},
	})
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	const want = "forward duration_ms via gb hook relay"
	if named.ShortTitle != want {
		t.Fatalf("create short_title = %q, want %q (normalized)", named.ShortTitle, want)
	}

	got, err := c.GetBead(ctx, named.ID)
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if got.ShortTitle != want {
		t.Fatalf("GetBead short_title = %q, want %q", got.ShortTitle, want)
	}

	// 40 multibyte runes is the limit, not 40 bytes.
	limit, err := c.CreateBead(ctx, &client.CreateBeadRequest{Title: "At the limit", ShortTitle: shortTitle40, Type: "task", Labels: []string{"project:gardenio"}})
	if err != nil {
		t.Fatalf("CreateBead at 40 runes: %v", err)
	}
	if limit.ShortTitle != shortTitle40 {
		t.Fatalf("create short_title = %q, want %q", limit.ShortTitle, shortTitle40)
	}

	unnamed, err := c.CreateBead(ctx, &client.CreateBeadRequest{Title: "No short title yet", Type: "task", Labels: []string{"project:gardenio"}})
	if err != nil {
		t.Fatalf("CreateBead (unnamed): %v", err)
	}

	list, err := c.ListBeads(ctx, &client.ListBeadsRequest{ShortTitleMissing: true})
	if err != nil {
		t.Fatalf("ListBeads short_title_missing: %v", err)
	}
	if len(list.Beads) != 1 || list.Beads[0].ID != unnamed.ID {
		ids := make([]string, 0, len(list.Beads))
		for _, b := range list.Beads {
			ids = append(ids, b.ID)
		}
		t.Fatalf("short_title_missing returned %v, want [%s]: a dropped filter returns the wider set", ids, unnamed.ID)
	}

	all, err := c.ListBeads(ctx, &client.ListBeadsRequest{})
	if err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if len(all.Beads) != 3 {
		t.Fatalf("unfiltered list returned %d beads, want 3", len(all.Beads))
	}
	for _, b := range all.Beads {
		if b.ID == named.ID && b.ShortTitle != want {
			t.Fatalf("ListBeads short_title = %q, want %q", b.ShortTitle, want)
		}
	}
}

func TestGRPC_ShortTitle_UpdateSetFillAndClear(t *testing.T) {
	c, ms := newGRPCRoundtripClient(t)
	ctx := context.Background()
	ms.beads["gd-authored"] = &model.Bead{ID: "gd-authored", Kind: model.KindIssue, Type: model.TypeTask, Title: "Authored", ShortTitle: "authored name", Status: model.StatusOpen}
	ms.beads["gd-empty"] = &model.Bead{ID: "gd-empty", Kind: model.KindIssue, Type: model.TypeTask, Title: "Empty", Status: model.StatusOpen}

	groom := "groomer name"
	fill := &client.UpdateBeadRequest{ShortTitle: &groom, OnlyIfShortTitleEmpty: true}

	// only_if_short_title_empty never overwrites an authored value...
	b, err := c.UpdateBead(ctx, "gd-authored", fill)
	if err != nil {
		t.Fatalf("UpdateBead fill (authored): %v", err)
	}
	if b.ShortTitle != "authored name" {
		t.Fatalf("short_title = %q after a fill, want the authored value kept: only_if_short_title_empty was dropped on the wire", b.ShortTitle)
	}
	// ...and fills an unset one.
	b, err = c.UpdateBead(ctx, "gd-empty", fill)
	if err != nil {
		t.Fatalf("UpdateBead fill (empty): %v", err)
	}
	if b.ShortTitle != groom {
		t.Fatalf("short_title = %q after a fill, want %q", b.ShortTitle, groom)
	}

	// Without the guard an update replaces; an unrelated update leaves it alone.
	renamed := "renamed"
	if b, err = c.UpdateBead(ctx, "gd-authored", &client.UpdateBeadRequest{ShortTitle: &renamed}); err != nil {
		t.Fatalf("UpdateBead replace: %v", err)
	}
	if b.ShortTitle != renamed {
		t.Fatalf("short_title = %q after a replace, want %q", b.ShortTitle, renamed)
	}
	title := "Authored, retitled"
	if b, err = c.UpdateBead(ctx, "gd-authored", &client.UpdateBeadRequest{Title: &title}); err != nil {
		t.Fatalf("UpdateBead title: %v", err)
	}
	if b.ShortTitle != renamed {
		t.Fatalf("short_title = %q after an unrelated update, want it unchanged", b.ShortTitle)
	}

	// An explicit "" clears: optional string distinguishes unset from empty.
	empty := ""
	if b, err = c.UpdateBead(ctx, "gd-authored", &client.UpdateBeadRequest{ShortTitle: &empty}); err != nil {
		t.Fatalf("UpdateBead clear: %v", err)
	}
	if b.ShortTitle != "" {
		t.Fatalf("short_title = %q after clearing, want empty", b.ShortTitle)
	}
}

func TestGRPC_ShortTitle_TooLongIsInvalidArgument(t *testing.T) {
	c, ms := newGRPCRoundtripClient(t)
	ctx := context.Background()
	tooLong := shortTitle40 + "x" // 41 runes

	_, err := c.CreateBead(ctx, &client.CreateBeadRequest{Title: "Too long", ShortTitle: tooLong, Type: "task", Labels: []string{"project:gardenio"}})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("CreateBead with 41 runes: code = %v (err %v), want InvalidArgument", got, err)
	}
	if n := len(ms.beads); n != 0 {
		t.Fatalf("rejected create wrote %d bead(s), want 0", n)
	}

	ms.beads["gd-st"] = &model.Bead{ID: "gd-st", Kind: model.KindIssue, Type: model.TypeTask, Title: "Bead", Status: model.StatusOpen}
	_, err = c.UpdateBead(ctx, "gd-st", &client.UpdateBeadRequest{ShortTitle: &tooLong})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("UpdateBead with 41 runes: code = %v (err %v), want InvalidArgument", got, err)
	}

	multiline := "two\nlines"
	_, err = c.UpdateBead(ctx, "gd-st", &client.UpdateBeadRequest{ShortTitle: &multiline})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("UpdateBead multi-line: code = %v (err %v), want InvalidArgument", got, err)
	}
	if st := ms.beads["gd-st"].ShortTitle; st != "" {
		t.Fatalf("stored short_title = %q after rejected updates, want empty", st)
	}
}
