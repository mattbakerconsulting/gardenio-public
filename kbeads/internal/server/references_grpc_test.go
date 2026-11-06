package server

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func newRefsTestServer() (*BeadsServer, *mockStore) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	return s, ms
}

func TestGRPCListMentionedBy_HappyPath(t *testing.T) {
	s, ms := newRefsTestServer()
	ms.mentionedBy = []*model.BeadReferenceSummary{
		{ID: "gd-ldkpaoah1s", Title: "alpha", Type: "task", Status: "open", SourceField: "description"},
	}
	ms.mentionedByTotal = 1

	resp, err := s.ListMentionedBy(context.Background(), &beadsv1.ListMentionedByRequest{
		BeadId: "gd-target00001", Limit: 50, Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListMentionedBy: %v", err)
	}
	if resp.GetTotal() != 1 || len(resp.GetReferences()) != 1 {
		t.Fatalf("total/len = %d/%d, want 1/1", resp.GetTotal(), len(resp.GetReferences()))
	}
	r := resp.GetReferences()[0]
	if r.GetId() != "gd-ldkpaoah1s" || r.GetSourceField() != "description" || r.GetType() != "task" {
		t.Errorf("ref = %+v", r)
	}
	if ms.lastMentionedByID != "gd-target00001" || ms.lastMentionedByLimit != 50 {
		t.Errorf("store called with id=%q limit=%d", ms.lastMentionedByID, ms.lastMentionedByLimit)
	}
}

func TestGRPCListMentionedBy_EmptyBeadID(t *testing.T) {
	s, _ := newRefsTestServer()
	_, err := s.ListMentionedBy(context.Background(), &beadsv1.ListMentionedByRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCListMentionedBy_NegativeLimit(t *testing.T) {
	s, _ := newRefsTestServer()
	_, err := s.ListMentionedBy(context.Background(), &beadsv1.ListMentionedByRequest{
		BeadId: "gd-x", Limit: -1,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCListMentionedBy_NegativeOffset(t *testing.T) {
	s, _ := newRefsTestServer()
	_, err := s.ListMentionedBy(context.Background(), &beadsv1.ListMentionedByRequest{
		BeadId: "gd-x", Offset: -1,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCListMentionedBy_StoreError(t *testing.T) {
	s, ms := newRefsTestServer()
	ms.mentionedByErr = errors.New("boom")
	_, err := s.ListMentionedBy(context.Background(), &beadsv1.ListMentionedByRequest{BeadId: "gd-x"})
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
	}
}

func TestGRPCListMentions_HappyPath(t *testing.T) {
	s, ms := newRefsTestServer()
	ms.mentions = []*model.BeadReferenceSummary{
		{ID: "gd-FNHTKRBDIQ", Title: "real", Type: "task", Status: "open", SourceField: "description"},
		{ID: "gd-ZACQZ1SGLD", Title: "", Type: "", Status: "", SourceField: "comment:42"},
	}
	ms.mentionsTotal = 2

	resp, err := s.ListMentions(context.Background(), &beadsv1.ListMentionsRequest{
		BeadId: "gd-ldkpaoah1s", Limit: 25, Offset: 5,
	})
	if err != nil {
		t.Fatalf("ListMentions: %v", err)
	}
	if resp.GetTotal() != 2 || resp.GetLimit() != 25 || resp.GetOffset() != 5 {
		t.Errorf("envelope total/limit/offset = %d/%d/%d", resp.GetTotal(), resp.GetLimit(), resp.GetOffset())
	}
	if len(resp.GetReferences()) != 2 {
		t.Fatalf("len(refs) = %d, want 2", len(resp.GetReferences()))
	}
	// Dangling target should still come through with empty Title/Type/Status.
	if resp.GetReferences()[1].GetId() != "gd-ZACQZ1SGLD" || resp.GetReferences()[1].GetTitle() != "" {
		t.Errorf("dangling ref = %+v", resp.GetReferences()[1])
	}
}

func TestGRPCListMentions_EmptyBeadID(t *testing.T) {
	s, _ := newRefsTestServer()
	_, err := s.ListMentions(context.Background(), &beadsv1.ListMentionsRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPCListMentions_StoreError(t *testing.T) {
	s, ms := newRefsTestServer()
	ms.mentionsErr = errors.New("db down")
	_, err := s.ListMentions(context.Background(), &beadsv1.ListMentionsRequest{BeadId: "gd-x"})
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
	}
}

// gd-9ephUcYMIy: ListMentionedByRequest.relationship_type must propagate to
// the store as a *string with three-state semantics matching the HTTP ?type=
// shape (see TestHandleListMentionedBy_TypeFilter_*):
//   - field absent     → store receives nil  (no filter)
//   - field present "" → store receives &""  (untyped only — IS NULL)
//   - field "<verb>"   → store receives &"<verb>" (exact match)
func TestGRPCListMentionedBy_RelationshipTypeThreeState(t *testing.T) {
	cases := []struct {
		name string
		req  *beadsv1.ListMentionedByRequest
		want *string
	}{
		{"omitted", &beadsv1.ListMentionedByRequest{BeadId: "gd-target00001"}, nil},
		{"empty value", &beadsv1.ListMentionedByRequest{BeadId: "gd-target00001", RelationshipType: strPtr("")}, strPtr("")},
		{"verb", &beadsv1.ListMentionedByRequest{BeadId: "gd-target00001", RelationshipType: strPtr("blocks")}, strPtr("blocks")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ms := newRefsTestServer()
			if _, err := s.ListMentionedBy(context.Background(), tc.req); err != nil {
				t.Fatalf("ListMentionedBy: %v", err)
			}
			got := ms.lastMentionedByRelType
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("rel = %q, want nil", *got)
			case tc.want != nil && got == nil:
				t.Errorf("rel = nil, want %q", *tc.want)
			case tc.want != nil && got != nil && *tc.want != *got:
				t.Errorf("rel = %q, want %q", *got, *tc.want)
			}
		})
	}
}

// gd-9ephUcYMIy: same three-state propagation for the forward (mentions) path.
func TestGRPCListMentions_RelationshipTypeThreeState(t *testing.T) {
	cases := []struct {
		name string
		req  *beadsv1.ListMentionsRequest
		want *string
	}{
		{"omitted", &beadsv1.ListMentionsRequest{BeadId: "gd-ldkpaoah1s"}, nil},
		{"empty value", &beadsv1.ListMentionsRequest{BeadId: "gd-ldkpaoah1s", RelationshipType: strPtr("")}, strPtr("")},
		{"verb", &beadsv1.ListMentionsRequest{BeadId: "gd-ldkpaoah1s", RelationshipType: strPtr("parent")}, strPtr("parent")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ms := newRefsTestServer()
			if _, err := s.ListMentions(context.Background(), tc.req); err != nil {
				t.Fatalf("ListMentions: %v", err)
			}
			got := ms.lastMentionsRelType
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("rel = %q, want nil", *got)
			case tc.want != nil && got == nil:
				t.Errorf("rel = nil, want %q", *tc.want)
			case tc.want != nil && got != nil && *tc.want != *got:
				t.Errorf("rel = %q, want %q", *got, *tc.want)
			}
		})
	}
}
