package server

import (
	"context"
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// Tests for gd-DbxpPLBcsi: the taint read gate (ADR docs/adr/taints.md §5.1)
// on every per-bead sub-resource read and the decision/escalation by-id
// reads. A caller with no grant for a gate taint gets exactly what it gets
// for a missing bead, and acquires nothing; the control holds the grant.

// newGatedSubresourceServer seeds kd-secret, carrying gate taint 1, with a
// row under every sub-resource the mock serves per bead.
func newGatedSubresourceServer(t *testing.T, beadType model.BeadType) (*BeadsServer, *mockStore) {
	t.Helper()
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["kd-secret"] = &model.Bead{ID: "kd-secret", Kind: model.KindIssue, Type: beadType, Title: "secret title"}
	seedBeadTaints(ms, "kd-secret", 1)
	ms.comments["kd-secret"] = []*model.Comment{{ID: 1, BeadID: "kd-secret", Text: "secret comment"}}
	ms.labels["kd-secret"] = []string{"secret-label"}
	ms.deps["kd-secret"] = []*model.Dependency{{BeadID: "kd-secret", DependsOnID: "kd-other", Type: model.DepBlocks}}
	ms.deps["kd-child"] = []*model.Dependency{{BeadID: "kd-child", DependsOnID: "kd-secret", Type: model.DepBlocks}}
	ms.events = append(ms.events, &model.Event{ID: 1, BeadID: "kd-secret", Topic: "bead.created"})
	if err := ms.RecordMutation(context.Background(), &model.MutationRow{
		BeadID: "kd-secret", Field: "operated_by", OldValue: "", NewValue: "oidc:a@example.com", MutationType: model.MutationTypeCreate,
	}); err != nil {
		t.Fatalf("seed mutation: %v", err)
	}
	return s, ms
}

// grantAgentTaint grants agent taintID and drops the grant set an earlier
// denied read cached.
func grantAgentTaint(t *testing.T, s *BeadsServer, ms *mockStore, agent string, taintID int64) {
	t.Helper()
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: taintID, PrincipalKind: model.PrincipalAgent, Principal: agent, GrantedBy: 1}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	s.invalidateAgentGrantSet(agent)
}

func TestTaintReadGate_SubresourceReads(t *testing.T) {
	routes := []struct {
		name    string
		path    string
		handler func(*BeadsServer) http.HandlerFunc
		seeded  bool // newGatedSubresourceServer seeds rows the granted caller sees
	}{
		{"comments", "comments", func(s *BeadsServer) http.HandlerFunc { return s.handleGetComments }, true},
		{"labels", "labels", func(s *BeadsServer) http.HandlerFunc { return s.handleGetLabels }, true},
		{"dependencies", "dependencies", func(s *BeadsServer) http.HandlerFunc { return s.handleGetDependencies }, true},
		{"dependents", "dependents", func(s *BeadsServer) http.HandlerFunc { return s.handleGetReverseDependencies }, true},
		{"events", "events", func(s *BeadsServer) http.HandlerFunc { return s.handleGetEvents }, true},
		{"mutations", "mutations", func(s *BeadsServer) http.HandlerFunc { return s.handleListMutations }, true},
		{"operator-history", "operator-history", func(s *BeadsServer) http.HandlerFunc { return s.handleGetOperatorHistory }, true},
		{"followers", "followers", func(s *BeadsServer) http.HandlerFunc { return s.handleListFollowers }, false},
		{"taints", "taints", func(s *BeadsServer) http.HandlerFunc { return s.handleGetBeadTaints }, false},
		{"mentions", "mentions", func(s *BeadsServer) http.HandlerFunc { return s.handleListMentions }, false},
		{"mentioned-by", "mentioned-by", func(s *BeadsServer) http.HandlerFunc { return s.handleListMentionedBy }, false},
	}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			s, ms := newGatedSubresourceServer(t, model.TypeTask)
			serve := func(id string) (int, string) {
				rec := serveAsAgent(t, rt.handler(s), "/v1/beads/"+id+"/"+rt.path, "gd-agent1", map[string]string{"id": id})
				return rec.Code, rec.Body.String()
			}

			missingCode, missingBody := serve("kd-nope")
			deniedCode, deniedBody := serve("kd-secret")
			if deniedCode != missingCode || deniedBody != missingBody {
				t.Errorf("denied = %d %s; want the missing-bead answer %d %s", deniedCode, deniedBody, missingCode, missingBody)
			}
			if rows := ms.beadTaints["gd-agent1"]; len(rows) != 0 {
				t.Errorf("agent acquired %v on a denied read", rows)
			}

			grantAgentTaint(t, s, ms, "gd-agent1", 1)
			code, body := serve("kd-secret")
			if code != http.StatusOK || (rt.seeded && body == missingBody) {
				t.Errorf("granted caller = %d %s; want 200 with the seeded rows", code, body)
			}
		})
	}
}

func TestTaintReadGate_SubresourceReadsGRPC(t *testing.T) {
	calls := []struct {
		name string
		call func(*BeadsServer, context.Context, string) (int, error)
	}{
		{"GetComments", func(s *BeadsServer, ctx context.Context, id string) (int, error) {
			resp, err := s.GetComments(ctx, &beadsv1.GetCommentsRequest{BeadId: id})
			return len(resp.GetComments()), err
		}},
		{"GetLabels", func(s *BeadsServer, ctx context.Context, id string) (int, error) {
			resp, err := s.GetLabels(ctx, &beadsv1.GetLabelsRequest{BeadId: id})
			return len(resp.GetLabels()), err
		}},
		{"GetDependencies", func(s *BeadsServer, ctx context.Context, id string) (int, error) {
			resp, err := s.GetDependencies(ctx, &beadsv1.GetDependenciesRequest{BeadId: id})
			return len(resp.GetDependencies()), err
		}},
		{"GetEvents", func(s *BeadsServer, ctx context.Context, id string) (int, error) {
			resp, err := s.GetEvents(ctx, &beadsv1.GetEventsRequest{BeadId: id})
			return len(resp.GetEvents()), err
		}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			s, ms := newGatedSubresourceServer(t, model.TypeTask)
			ctx := staticAgentCtx("gd-agent1")
			if n, err := c.call(s, ctx, "kd-secret"); err != nil || n != 0 {
				t.Errorf("denied = %d rows, err %v; want 0 rows, nil (a missing bead's answer)", n, err)
			}
			if rows := ms.beadTaints["gd-agent1"]; len(rows) != 0 {
				t.Errorf("agent acquired %v on a denied read", rows)
			}
			grantAgentTaint(t, s, ms, "gd-agent1", 1)
			if n, err := c.call(s, ctx, "kd-secret"); err != nil || n == 0 {
				t.Errorf("granted = %d rows, err %v; want the seeded rows", n, err)
			}
		})
	}
}

// A read-scoped caller gets NotFound for a denied parent, as for a hidden one.
func TestTaintReadGate_SubresourceReadGRPC_ScopedCallerNotFound(t *testing.T) {
	s, _ := newGatedSubresourceServer(t, model.TypeTask)
	ctx := WithTokenProject(staticAgentCtx("gd-agent1"), "gardenio")
	ms := s.store.(*mockStore)
	ms.beads["kd-secret"].Labels = []string{"project:gardenio"}
	_, err := s.GetComments(ctx, &beadsv1.GetCommentsRequest{BeadId: "kd-secret"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetComments err = %v; want NotFound", err)
	}
}

func TestTaintReadGate_DecisionAndEscalationByID(t *testing.T) {
	cases := []struct {
		name     string
		beadType model.BeadType
		handler  func(*BeadsServer) http.HandlerFunc
	}{
		{"decision", model.TypeDecision, func(s *BeadsServer) http.HandlerFunc { return s.handleGetDecision }},
		{"escalation", model.TypeEscalation, func(s *BeadsServer) http.HandlerFunc { return s.handleGetEscalation }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, ms := newGatedSubresourceServer(t, c.beadType)
			rec := serveAsAgent(t, c.handler(s), "/v1/"+c.name+"s/kd-secret", "gd-agent1", map[string]string{"id": "kd-secret"})
			requireStatus(t, rec, http.StatusNotFound)
			if rows := ms.beadTaints["gd-agent1"]; len(rows) != 0 {
				t.Errorf("agent acquired %v on a denied read", rows)
			}

			grantAgentTaint(t, s, ms, "gd-agent1", 1)
			rec = serveAsAgent(t, c.handler(s), "/v1/"+c.name+"s/kd-secret", "gd-agent1", map[string]string{"id": "kd-secret"})
			requireStatus(t, rec, http.StatusOK)
		})
	}
}

// Mention rows carry the other bead's title from a join: a mentions target
// the caller may not read is blanked to a dangling reference, and a
// mentioned-by source it may not read is dropped.
func TestTaintReadGate_MentionRows(t *testing.T) {
	s, ms := newGatedSubresourceServer(t, model.TypeTask)
	ms.beads["kd-open"] = &model.Bead{ID: "kd-open", Kind: model.KindIssue, Type: model.TypeTask, Title: "open"}
	rows := func() []*model.BeadReferenceSummary {
		return []*model.BeadReferenceSummary{
			{ID: "kd-secret", Title: "secret title", Type: "task", Status: "open", SourceField: "description"},
			{ID: "kd-clean", Title: "clean title", Type: "task", Status: "open", SourceField: "description"},
		}
	}
	ms.mentions, ms.mentionsTotal = rows(), 2
	ms.mentionedBy, ms.mentionedByTotal = rows(), 2

	var out struct {
		References []*model.BeadReferenceSummary `json:"references"`
		Total      int                           `json:"total"`
	}
	rec := serveAsAgent(t, s.handleListMentions, "/v1/beads/kd-open/mentions", "gd-agent1", map[string]string{"id": "kd-open"})
	requireStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &out)
	if len(out.References) != 2 || out.References[0].ID != "kd-secret" || out.References[0].Title != "" || out.References[0].Type != "" || out.References[1].Title != "clean title" {
		t.Errorf("mentions = %+v; want kd-secret blanked, kd-clean intact", out.References)
	}

	rec = serveAsAgent(t, s.handleListMentionedBy, "/v1/beads/kd-open/mentioned-by", "gd-agent1", map[string]string{"id": "kd-open"})
	requireStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &out)
	if len(out.References) != 1 || out.References[0].ID != "kd-clean" || out.Total != 1 {
		t.Errorf("mentioned-by = %+v total %d; want only kd-clean, total 1", out.References, out.Total)
	}
	if rows := ms.beadTaints["gd-agent1"]; len(rows) != 0 {
		t.Errorf("agent acquired %v from a gated mention row", rows)
	}

	resp, err := s.ListMentionedBy(staticAgentCtx("gd-agent1"), &beadsv1.ListMentionedByRequest{BeadId: "kd-open"})
	if err != nil || len(resp.GetReferences()) != 1 || resp.GetTotal() != 1 {
		t.Errorf("gRPC mentioned-by = %+v, err %v; want only kd-clean", resp.GetReferences(), err)
	}
	ms.mentions = rows()
	mresp, err := s.ListMentions(staticAgentCtx("gd-agent1"), &beadsv1.ListMentionsRequest{BeadId: "kd-open"})
	if err != nil || len(mresp.GetReferences()) != 2 || mresp.GetReferences()[0].GetTitle() != "" {
		t.Errorf("gRPC mentions = %+v, err %v; want kd-secret blanked", mresp.GetReferences(), err)
	}

	grantAgentTaint(t, s, ms, "gd-agent1", 1)
	ms.mentions = rows()
	rec = serveAsAgent(t, s.handleListMentions, "/v1/beads/kd-open/mentions", "gd-agent1", map[string]string{"id": "kd-open"})
	decodeJSON(t, rec, &out)
	if len(out.References) != 2 || out.References[0].Title != "secret title" {
		t.Errorf("granted mentions = %+v; want kd-secret's title", out.References)
	}
}
