package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
)

// Regression tests for ka-0000003827: the per-bead sub-resource GET handlers
// (comments, events, mutations, labels, dependencies, operator-history,
// followers, mentions) must enforce the SAME read-side project scope that
// handleGetBead enforces. Before this fix they handed the caller-supplied
// {id} straight to the store, so an OIDC user (or project-bound fleet token)
// could read the contents of a bead in a project they are not authorized for
// even though GET /v1/beads/{id} on the same id returns 403.
//
// These mirror the write-side denial tests in http_relations_authz_test.go,
// which had no read-side counterpart.

// readAuthzMux registers the per-bead sub-resource GET routes on the server
// under test without AuthMiddleware, so tests inject the OIDC/token context
// directly (isolating the authorizeSubresourceRead gate from the middleware).
func readAuthzMux(s *BeadsServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/beads/{id}/comments", s.handleGetComments)
	mux.HandleFunc("GET /v1/beads/{id}/events", s.handleGetEvents)
	mux.HandleFunc("GET /v1/beads/{id}/mutations", s.handleListMutations)
	mux.HandleFunc("GET /v1/beads/{id}/labels", s.handleGetLabels)
	mux.HandleFunc("GET /v1/beads/{id}/dependencies", s.handleGetDependencies)
	mux.HandleFunc("GET /v1/beads/{id}/dependents", s.handleGetReverseDependencies)
	mux.HandleFunc("GET /v1/beads/{id}/operator-history", s.handleGetOperatorHistory)
	mux.HandleFunc("GET /v1/beads/{id}/followers", s.handleListFollowers)
	mux.HandleFunc("GET /v1/beads/{id}/mentioned-by", s.handleListMentionedBy)
	mux.HandleFunc("GET /v1/beads/{id}/mentions", s.handleListMentions)
	return mux
}

// readAuthzFixture wires a server with one project-scoped target bead
// (labeled project:<targetProject>) plus a user bead granting the caller
// access to userProjects. Populate the target's sub-resources so a leak would
// return a non-empty body.
func readAuthzFixture(t *testing.T, targetProject, userEmail, userProjects string) (*mockStore, http.Handler, string) {
	t.Helper()
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := readAuthzMux(s)

	targetID := "gd-rtarget1"
	ms.beads[targetID] = &model.Bead{
		ID:     targetID,
		Title:  "target",
		Type:   "task",
		Kind:   "issue",
		Status: model.StatusOpen,
	}
	// mockStore.GetBead attaches labels from ms.labels[id].
	ms.labels[targetID] = []string{"project:" + targetProject}
	// Seed sub-resources so a bypass would return visible data.
	ms.comments[targetID] = []*model.Comment{{BeadID: targetID, Author: "someone", Text: "secret prose"}}
	ms.events = append(ms.events, &model.Event{BeadID: targetID, Topic: "beads.updated"})
	makeUserBead(ms, "gd-user-read", userEmail, userProjects, "crew")
	return ms, h, targetID
}

// readSubresourcePaths enumerates every gated GET sub-resource path for a
// target bead id.
func readSubresourcePaths(id string) []string {
	return []string{
		"/v1/beads/" + id + "/comments",
		"/v1/beads/" + id + "/events",
		"/v1/beads/" + id + "/mutations",
		"/v1/beads/" + id + "/labels",
		"/v1/beads/" + id + "/dependencies",
		"/v1/beads/" + id + "/dependents",
		"/v1/beads/" + id + "/operator-history",
		"/v1/beads/" + id + "/followers",
		"/v1/beads/" + id + "/mentioned-by",
		"/v1/beads/" + id + "/mentions",
	}
}

func TestSubresourceReads_DenyCrossProjectOIDC(t *testing.T) {
	_, h, id := readAuthzFixture(t, "gardenio", "mallory@example.com", "otherproject")

	for _, path := range readSubresourcePaths(id) {
		req := oidcCtxReq("GET", path, nil, "mallory@example.com", []string{RoleOperator})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404 (as missing) for cross-project OIDC read, got %d; body: %s",
				path, rec.Code, rec.Body.String())
		}
	}
}

func TestSubresourceReads_AllowSameProjectOIDC(t *testing.T) {
	_, h, id := readAuthzFixture(t, "gardenio", "alice@example.com", "gardenio")

	for _, path := range readSubresourcePaths(id) {
		req := oidcCtxReq("GET", path, nil, "alice@example.com", []string{RoleOperator})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200 for same-project OIDC read, got %d; body: %s",
				path, rec.Code, rec.Body.String())
		}
	}
}

func TestSubresourceReads_AdminBypass(t *testing.T) {
	_, h, id := readAuthzFixture(t, "gardenio", "admin@example.com", "otherproject")

	for _, path := range readSubresourcePaths(id) {
		req := oidcCtxReq("GET", path, nil, "admin@example.com", []string{RoleAdmin})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200 for admin read, got %d; body: %s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// A project-bound fleet token must not read the sub-resources of a bead in a
// different project. The token scope is carried via TokenProjectFromContext.
func TestSubresourceReads_DenyCrossProjectToken(t *testing.T) {
	_, h, id := readAuthzFixture(t, "gardenio", "", "")

	for _, path := range readSubresourcePaths(id) {
		req := httptest.NewRequest("GET", path, nil)
		// Project-bound fleet token scoped to a DIFFERENT project.
		req = req.WithContext(WithTokenProject(req.Context(), "otherproject"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404 (as missing) for cross-project token read, got %d; body: %s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// A project-bound fleet token scoped to the SAME project reads normally.
func TestSubresourceReads_AllowSameProjectToken(t *testing.T) {
	_, h, id := readAuthzFixture(t, "gardenio", "", "")

	for _, path := range readSubresourcePaths(id) {
		req := httptest.NewRequest("GET", path, nil)
		req = req.WithContext(WithTokenProject(req.Context(), "gardenio"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200 for same-project token read, got %d; body: %s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// An unrestricted static token (no project scope, non-OIDC) reads everything.
func TestSubresourceReads_StaticTokenUnaffected(t *testing.T) {
	_, h, id := readAuthzFixture(t, "gardenio", "", "")

	for _, path := range readSubresourcePaths(id) {
		req := httptest.NewRequest("GET", path, nil)
		req = req.WithContext(withAuthMethod(req.Context(), authMethodStatic))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200 for static token read, got %d; body: %s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// A missing parent bead answers 404 to a read-scoped caller, exactly as an
// out-of-scope one does, so the two cannot be told apart (gd-7APTAIhdC5). An
// unscoped caller (static token) still passes THROUGH to the empty 200 it has
// always had (ka-0000003827).
func TestSubresourceReads_MissingBead(t *testing.T) {
	_, h, _ := readAuthzFixture(t, "gardenio", "alice@example.com", "gardenio")

	for _, path := range readSubresourcePaths("gd-missing") {
		req := oidcCtxReq("GET", path, nil, "alice@example.com", []string{RoleOperator})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404 for a scoped caller's missing bead, got %d; body: %s",
				path, rec.Code, rec.Body.String())
		}

		req = httptest.NewRequest("GET", path, nil)
		req = req.WithContext(withAuthMethod(req.Context(), authMethodStatic))
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200 pass-through for an unscoped caller's missing bead, got %d; body: %s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// gRPC twins: the four per-bead sub-resource RPCs plus the two reference RPCs
// must answer a cross-project OIDC caller as for a missing bead (NotFound)
// and allow a same-project caller.
func TestGRPCSubresourceReads_DenyCrossProjectOIDC(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	id := "gd-e04xqvpiyn"
	ms.beads[id] = &model.Bead{ID: id, Title: "t", Type: "task", Kind: "issue", Status: model.StatusOpen}
	ms.labels[id] = []string{"project:gardenio"}
	makeUserBead(ms, "gd-user-grpc", "mallory@example.com", "otherproject", "crew")

	ctx := oidcCtx("mallory@example.com", []string{RoleOperator})

	_, err := srv.GetComments(ctx, &beadsv1.GetCommentsRequest{BeadId: id})
	requireCode(t, err, codes.NotFound)
	_, err = srv.GetEvents(ctx, &beadsv1.GetEventsRequest{BeadId: id})
	requireCode(t, err, codes.NotFound)
	_, err = srv.GetDependencies(ctx, &beadsv1.GetDependenciesRequest{BeadId: id})
	requireCode(t, err, codes.NotFound)
	_, err = srv.GetLabels(ctx, &beadsv1.GetLabelsRequest{BeadId: id})
	requireCode(t, err, codes.NotFound)
	_, err = srv.ListMentionedBy(ctx, &beadsv1.ListMentionedByRequest{BeadId: id})
	requireCode(t, err, codes.NotFound)
	_, err = srv.ListMentions(ctx, &beadsv1.ListMentionsRequest{BeadId: id})
	requireCode(t, err, codes.NotFound)
}

func TestGRPCSubresourceReads_AllowSameProjectOIDC(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	id := "gd-grpctarget2"
	ms.beads[id] = &model.Bead{ID: id, Title: "t", Type: "task", Kind: "issue", Status: model.StatusOpen}
	ms.labels[id] = []string{"project:gardenio"}
	makeUserBead(ms, "gd-user-grpc2", "alice@example.com", "gardenio", "crew")

	ctx := oidcCtx("alice@example.com", []string{RoleOperator})

	if _, err := srv.GetComments(ctx, &beadsv1.GetCommentsRequest{BeadId: id}); err != nil {
		t.Errorf("GetComments same-project: unexpected error %v", err)
	}
	if _, err := srv.GetLabels(ctx, &beadsv1.GetLabelsRequest{BeadId: id}); err != nil {
		t.Errorf("GetLabels same-project: unexpected error %v", err)
	}
}
