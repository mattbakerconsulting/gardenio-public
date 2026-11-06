package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSemanticSearch_RejectsUnknownType locks in that the gRPC SemanticSearch
// RPC returns codes.InvalidArgument (not a silent empty result) for an unknown
// type filter — the SEARCH twin of TestListBeads_RejectsUnknownType
// (ka-0000007554). Validation fires BEFORE the embedder check, so a bad type
// fails loud even with no embedder configured.
func TestSemanticSearch_RejectsUnknownType(t *testing.T) {
	srv, _, ctx := testCtx(t)

	_, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{
		Query: "anything",
		Type:  []string{"zzznotatype"},
	})
	if err == nil {
		t.Fatal("expected error for unknown type on SemanticSearch")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v (%v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "zzznotatype") {
		t.Errorf("error should name the unknown type; got %q", err.Error())
	}
}

// TestSemanticSearch_AcceptsValidTypeAndAlias confirms a valid canonical type —
// and a deprecated-but-valid alias (template→formula) — is NOT rejected by the
// SEARCH validation, and that the alias is folded to its canonical form before
// reaching the store (so ?type=template does not silently match nothing).
func TestSemanticSearch_AcceptsValidTypeAndAlias(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{0.1}, name: "test-model", dims: 1})
	ms.semanticResults = []*model.SemanticResult{}

	// Valid canonical type.
	if _, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{
		Query: "q", Type: []string{"formula"},
	}); err != nil {
		t.Fatalf("valid type 'formula' rejected: %v", err)
	}
	if len(ms.lastSemanticFilter.Type) != 1 || ms.lastSemanticFilter.Type[0] != model.TypeFormula {
		t.Errorf("type filter: got %+v, want [formula]", ms.lastSemanticFilter.Type)
	}

	// Deprecated alias resolves to formula and must still be accepted + folded.
	if _, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{
		Query: "q", Type: []string{"template"},
	}); err != nil {
		t.Fatalf("valid alias 'template' rejected: %v", err)
	}
	if len(ms.lastSemanticFilter.Type) != 1 || ms.lastSemanticFilter.Type[0] != model.TypeFormula {
		t.Errorf("alias fold: got %+v, want [formula]", ms.lastSemanticFilter.Type)
	}
}

// TestHandleSemanticSearch_RejectsUnknownType covers the HTTP
// /v1/search/semantic path — an unknown type must 400 with a self-correcting
// message, not silently match nothing (ka-0000007554). Validation fires before
// the embedder check, so no embedder need be configured.
func TestHandleSemanticSearch_RejectsUnknownType(t *testing.T) {
	_, _, h := newTestServer() // embedder left nil

	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic",
		map[string]any{"query": "x", "type": []string{"zzznotatype"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding error body: %v", err)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "unknown bead type") || !strings.Contains(msg, "zzznotatype") {
		t.Errorf("error body should name the unknown type; got %q", msg)
	}
	if !strings.Contains(msg, "valid types:") {
		t.Errorf("error body should list valid types; got %q", msg)
	}
}

// TestHandleSemanticSearch_AcceptsValidTypeAndAlias confirms the HTTP path still
// 200s for a valid type — including the template→formula alias, which
// toBeadTypes now folds before validation — and that the folded canonical type
// reaches the store.
func TestHandleSemanticSearch_AcceptsValidTypeAndAlias(t *testing.T) {
	s, ms, h := newTestServer()
	s.SetEmbedder(&fakeEmbedder{vec: []float32{0.1}, name: "test-model", dims: 1})
	ms.semanticResults = []*model.SemanticResult{}

	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic",
		map[string]any{"query": "x", "type": []string{"template"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if len(ms.lastSemanticFilter.Type) != 1 || ms.lastSemanticFilter.Type[0] != model.TypeFormula {
		t.Errorf("alias fold: got %+v, want [formula]", ms.lastSemanticFilter.Type)
	}
}
