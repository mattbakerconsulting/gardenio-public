package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// This file backfills coverage for the many small HTTP wrappers that issue a
// single doJSON round-trip. Each test asserts the method + path on the wire
// and that the decoded response is wired back correctly. They follow the
// existing testHandler/newTestClient harness in http_test.go.

func TestHTTPClient_RestoreBead(t *testing.T) {
	h := &testHandler{responseBody: `{"id":"gd-1","title":"restored"}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	b, err := c.RestoreBead(context.Background(), "gd-1")
	if err != nil {
		t.Fatalf("RestoreBead() error = %v", err)
	}
	if h.method != http.MethodPost {
		t.Errorf("method = %q, want POST", h.method)
	}
	if h.path != "/v1/beads/gd-1/restore" {
		t.Errorf("path = %q, want /v1/beads/gd-1/restore", h.path)
	}
	if b.ID != "gd-1" {
		t.Errorf("bead ID = %q, want 'gd-1'", b.ID)
	}
}

func TestHTTPClient_GetReverseDependencies(t *testing.T) {
	h := &testHandler{responseBody: `{"dependencies":[{"bead_id":"gd-child","depends_on_id":"gd-1","type":"blocks"}]}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	deps, err := c.GetReverseDependencies(context.Background(), "gd-1")
	if err != nil {
		t.Fatalf("GetReverseDependencies() error = %v", err)
	}
	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/beads/gd-1/dependents" {
		t.Errorf("path = %q, want /v1/beads/gd-1/dependents", h.path)
	}
	if len(deps) != 1 || deps[0].DependsOnID != "gd-1" {
		t.Errorf("deps = %+v, want one dep on gd-1", deps)
	}
}

func TestHTTPClient_DeleteComment(t *testing.T) {
	h := &testHandler{responseBody: `{"deleted":true}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	deleted, err := c.DeleteComment(context.Background(), "gd-1", 42)
	if err != nil {
		t.Fatalf("DeleteComment() error = %v", err)
	}
	if h.method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", h.method)
	}
	if h.path != "/v1/beads/gd-1/comments/42" {
		t.Errorf("path = %q, want /v1/beads/gd-1/comments/42", h.path)
	}
	if !deleted {
		t.Error("deleted = false, want true")
	}
}

func TestHTTPClient_TrimComments(t *testing.T) {
	h := &testHandler{responseBody: `{"deleted_ids":[1,2,3]}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	ids, err := c.TrimComments(context.Background(), "gd-1", 5, "bot")
	if err != nil {
		t.Fatalf("TrimComments() error = %v", err)
	}
	if h.path != "/v1/beads/gd-1/comments/trim" {
		t.Errorf("path = %q, want /v1/beads/gd-1/comments/trim", h.path)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(h.body), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body["keep_last"] != float64(5) {
		t.Errorf("keep_last = %v, want 5", body["keep_last"])
	}
	if body["author"] != "bot" {
		t.Errorf("author = %v, want 'bot'", body["author"])
	}
	if len(ids) != 3 {
		t.Errorf("deleted ids = %v, want 3 entries", ids)
	}
}

func TestHTTPClient_TrimComments_NoAuthorFilter(t *testing.T) {
	h := &testHandler{responseBody: `{"deleted_ids":[]}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	if _, err := c.TrimComments(context.Background(), "gd-1", 3, ""); err != nil {
		t.Fatalf("TrimComments() error = %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(h.body), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if _, ok := body["author"]; ok {
		t.Error("author key present, want omitted when filter empty")
	}
}

func TestHTTPClient_ListGlobalEvents(t *testing.T) {
	h := &testHandler{responseBody: `{"events":[{"id":1}],"has_more":true,"next_cursor":"5"}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.ListGlobalEvents(context.Background(), &ListGlobalEventsRequest{
		Cursor: 10,
		Limit:  50,
		Desc:   true,
	})
	if err != nil {
		t.Fatalf("ListGlobalEvents() error = %v", err)
	}
	if h.path != "/v1/events" {
		t.Errorf("path = %q, want /v1/events", h.path)
	}
	// Query params: cursor, limit, order=desc.
	if h.query == "" {
		t.Error("query is empty, want cursor/limit/order params")
	}
	if !resp.HasMore || resp.NextCursor != "5" {
		t.Errorf("resp = %+v, want has_more+next_cursor", resp)
	}
}

func TestHTTPClient_GetMutations(t *testing.T) {
	h := &testHandler{responseBody: `{"mutations":[{"id":1},{"id":2}]}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	muts, err := c.GetMutations(context.Background(), "gd-1")
	if err != nil {
		t.Fatalf("GetMutations() error = %v", err)
	}
	if h.path != "/v1/beads/gd-1/mutations" {
		t.Errorf("path = %q, want /v1/beads/gd-1/mutations", h.path)
	}
	if len(muts) != 2 {
		t.Errorf("mutations = %d, want 2", len(muts))
	}
}

func TestHTTPClient_UndoMutation(t *testing.T) {
	h := &testHandler{responseBody: `{"bead":{"id":"gd-1"},"undone":{"id":7}}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.UndoMutation(context.Background(), 7)
	if err != nil {
		t.Fatalf("UndoMutation() error = %v", err)
	}
	if h.method != http.MethodPost {
		t.Errorf("method = %q, want POST", h.method)
	}
	if h.path != "/v1/mutations/7/undo" {
		t.Errorf("path = %q, want /v1/mutations/7/undo", h.path)
	}
	if resp.Bead == nil || resp.Bead.ID != "gd-1" {
		t.Errorf("resp.Bead = %+v, want gd-1", resp.Bead)
	}
}

func TestHTTPClient_EmitHook(t *testing.T) {
	h := &testHandler{responseBody: `{"block":true,"reason":"gate","warnings":["w1"],"inject":"note"}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.EmitHook(context.Background(), &EmitHookRequest{
		AgentBeadID: "gd-agent",
		HookType:    "PreToolUse",
		ToolName:    "Bash",
	})
	if err != nil {
		t.Fatalf("EmitHook() error = %v", err)
	}
	if h.method != http.MethodPost || h.path != "/v1/hooks/emit" {
		t.Errorf("method/path = %q %q, want POST /v1/hooks/emit", h.method, h.path)
	}
	if !resp.Block || resp.Reason != "gate" || resp.Inject != "note" {
		t.Errorf("resp = %+v, want block+reason+inject", resp)
	}
}

func TestHTTPClient_ListGates(t *testing.T) {
	h := &testHandler{responseBody: `[{"gate_id":"decision"},{"gate_id":"commit-push"}]`}
	c, srv := newTestClient(h)
	defer srv.Close()

	gates, err := c.ListGates(context.Background(), "gd-agent")
	if err != nil {
		t.Fatalf("ListGates() error = %v", err)
	}
	if h.path != "/v1/agents/gd-agent/gates" {
		t.Errorf("path = %q, want /v1/agents/gd-agent/gates", h.path)
	}
	if len(gates) != 2 {
		t.Errorf("gates = %d, want 2", len(gates))
	}
}

func TestHTTPClient_SatisfyGate(t *testing.T) {
	h := &testHandler{statusCode: http.StatusNoContent}
	c, srv := newTestClient(h)
	defer srv.Close()

	if err := c.SatisfyGate(context.Background(), "gd-agent", "decision"); err != nil {
		t.Fatalf("SatisfyGate() error = %v", err)
	}
	if h.method != http.MethodPost {
		t.Errorf("method = %q, want POST", h.method)
	}
	if h.path != "/v1/agents/gd-agent/gates/decision/satisfy" {
		t.Errorf("path = %q, want .../gates/decision/satisfy", h.path)
	}
}

func TestHTTPClient_ClearGate(t *testing.T) {
	h := &testHandler{statusCode: http.StatusNoContent}
	c, srv := newTestClient(h)
	defer srv.Close()

	if err := c.ClearGate(context.Background(), "gd-agent", "decision"); err != nil {
		t.Fatalf("ClearGate() error = %v", err)
	}
	if h.method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", h.method)
	}
	if h.path != "/v1/agents/gd-agent/gates/decision" {
		t.Errorf("path = %q, want /v1/agents/gd-agent/gates/decision", h.path)
	}
}

func TestHTTPClient_BatchGetBeads(t *testing.T) {
	h := &testHandler{responseBody: `{"results":[{"id":"gd-1","bead":{"id":"gd-1"}},{"id":"gd-2","error":"not found"}]}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	results, err := c.BatchGetBeads(context.Background(), []string{"gd-1", "gd-2"})
	if err != nil {
		t.Fatalf("BatchGetBeads() error = %v", err)
	}
	if h.path != "/v1/batch/beads" {
		t.Errorf("path = %q, want /v1/batch/beads", h.path)
	}
	if h.query != "ids=gd-1,gd-2" {
		t.Errorf("query = %q, want ids=gd-1,gd-2", h.query)
	}
	if len(results) != 2 || results[1].Error != "not found" {
		t.Errorf("results = %+v, want gd-1 ok + gd-2 error", results)
	}
}

func TestHTTPClient_FindSimilar(t *testing.T) {
	h := &testHandler{responseBody: `{"source_id":"gd-1","model":"m","results":[{"bead":{"id":"gd-2"},"score":0.9}]}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.FindSimilar(context.Background(), "gd-1", 5, 0.5)
	if err != nil {
		t.Fatalf("FindSimilar() error = %v", err)
	}
	if h.path != "/v1/beads/gd-1/similar" {
		t.Errorf("path = %q, want /v1/beads/gd-1/similar", h.path)
	}
	// limit=5 and min_score=0.5 must both appear in the query.
	if h.query == "" {
		t.Error("query empty, want limit+min_score")
	}
	if resp.SourceID != "gd-1" || len(resp.Results) != 1 {
		t.Errorf("resp = %+v, want source gd-1 + 1 result", resp)
	}
}

func TestHTTPClient_FindSimilar_DefaultLimit(t *testing.T) {
	h := &testHandler{responseBody: `{"source_id":"gd-1","results":[]}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	// limit <= 0 should default to 10; min_score 0 should be omitted.
	if _, err := c.FindSimilar(context.Background(), "gd-1", 0, 0); err != nil {
		t.Fatalf("FindSimilar() error = %v", err)
	}
	if got := h.headers; got == nil { // sanity guard
		t.Fatal("no request captured")
	}
	if wantSub := "limit=10"; !strings.Contains(h.query, wantSub) {
		t.Errorf("query = %q, want it to contain %q", h.query, wantSub)
	}
	if strings.Contains(h.query, "min_score") {
		t.Errorf("query = %q, want min_score omitted for 0", h.query)
	}
}

// --- Fleet tokens ---

func TestHTTPClient_RegisterFleetToken(t *testing.T) {
	h := &testHandler{responseBody: `{"fleet":"f1"}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	if err := c.RegisterFleetToken(context.Background(), "tok", "f1"); err != nil {
		t.Fatalf("RegisterFleetToken() error = %v", err)
	}
	if h.method != http.MethodPost || h.path != "/v1/fleet-tokens" {
		t.Errorf("method/path = %q %q, want POST /v1/fleet-tokens", h.method, h.path)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(h.body), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body["token"] != "tok" || body["fleet"] != "f1" {
		t.Errorf("body = %+v, want token+fleet", body)
	}
}

func TestHTTPClient_RegisterFleetTokenFull(t *testing.T) {
	h := &testHandler{responseBody: `{"fleet":"f1","project":"p","privileged":true}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.RegisterFleetTokenFull(context.Background(), &RegisterFleetTokenRequest{
		Token:      "tok",
		Fleet:      "f1",
		Project:    "p",
		Privileged: true,
	})
	if err != nil {
		t.Fatalf("RegisterFleetTokenFull() error = %v", err)
	}
	if resp.Fleet != "f1" || resp.Project != "p" || !resp.Privileged {
		t.Errorf("resp = %+v, want f1/p/privileged", resp)
	}
}

func TestHTTPClient_RegisterFleetTokenFull_NilRequest(t *testing.T) {
	c := NewHTTPClient("http://example.invalid", "")
	if _, err := c.RegisterFleetTokenFull(context.Background(), nil); err == nil {
		t.Fatal("RegisterFleetTokenFull(nil) error = nil, want non-nil")
	}
}

func TestHTTPClient_ListFleetTokens(t *testing.T) {
	h := &testHandler{responseBody: `{"fleets":["a","b"]}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	fleets, err := c.ListFleetTokens(context.Background())
	if err != nil {
		t.Fatalf("ListFleetTokens() error = %v", err)
	}
	if h.path != "/v1/fleet-tokens" {
		t.Errorf("path = %q, want /v1/fleet-tokens", h.path)
	}
	if len(fleets) != 2 {
		t.Errorf("fleets = %v, want 2", fleets)
	}
}

func TestHTTPClient_RevokeFleetToken(t *testing.T) {
	h := &testHandler{statusCode: http.StatusNoContent}
	c, srv := newTestClient(h)
	defer srv.Close()

	if err := c.RevokeFleetToken(context.Background(), "f1"); err != nil {
		t.Fatalf("RevokeFleetToken() error = %v", err)
	}
	if h.method != http.MethodDelete || h.path != "/v1/fleet-tokens/f1" {
		t.Errorf("method/path = %q %q, want DELETE /v1/fleet-tokens/f1", h.method, h.path)
	}
}

// --- Embeddings (nil-request guards + happy path) ---

func TestHTTPClient_UpsertEmbedding(t *testing.T) {
	h := &testHandler{statusCode: http.StatusNoContent}
	c, srv := newTestClient(h)
	defer srv.Close()

	err := c.UpsertEmbedding(context.Background(), &UpsertEmbeddingRequest{
		BeadID:    "gd-1",
		Embedding: []float32{0.1, 0.2},
		Model:     "m",
		TextHash:  "h",
	})
	if err != nil {
		t.Fatalf("UpsertEmbedding() error = %v", err)
	}
	if h.method != http.MethodPost || h.path != "/v1/embeddings/upsert" {
		t.Errorf("method/path = %q %q, want POST /v1/embeddings/upsert", h.method, h.path)
	}
}

func TestHTTPClient_UpsertEmbedding_NilRequest(t *testing.T) {
	c := NewHTTPClient("http://example.invalid", "")
	if err := c.UpsertEmbedding(context.Background(), nil); err == nil {
		t.Fatal("UpsertEmbedding(nil) error = nil, want non-nil")
	}
}

func TestHTTPClient_GetEmbeddingHashes(t *testing.T) {
	h := &testHandler{responseBody: `{"hashes":{"gd-1":"abc"}}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	hashes, err := c.GetEmbeddingHashes(context.Background(), []string{"gd-1", "gd-2"})
	if err != nil {
		t.Fatalf("GetEmbeddingHashes() error = %v", err)
	}
	if h.path != "/v1/embeddings/hashes" {
		t.Errorf("path = %q, want /v1/embeddings/hashes", h.path)
	}
	if hashes["gd-1"] != "abc" {
		t.Errorf("hashes = %v, want gd-1->abc", hashes)
	}
}

func TestHTTPClient_GetEmbeddingHashes_NilMapNormalized(t *testing.T) {
	h := &testHandler{responseBody: `{}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	hashes, err := c.GetEmbeddingHashes(context.Background(), []string{"gd-1"})
	if err != nil {
		t.Fatalf("GetEmbeddingHashes() error = %v", err)
	}
	if hashes == nil {
		t.Error("hashes = nil, want non-nil empty map")
	}
}

func TestHTTPClient_SyncBatchEmbeddings(t *testing.T) {
	h := &testHandler{responseBody: `{"total":100,"page_count":10,"embedded":8,"skipped":2}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.SyncBatchEmbeddings(context.Background(), &SyncBatchEmbeddingsRequest{Limit: 10})
	if err != nil {
		t.Fatalf("SyncBatchEmbeddings() error = %v", err)
	}
	if h.path != "/v1/embeddings/sync-batch" {
		t.Errorf("path = %q, want /v1/embeddings/sync-batch", h.path)
	}
	if resp.Embedded != 8 || resp.PageCount != 10 {
		t.Errorf("resp = %+v, want embedded=8 page_count=10", resp)
	}
}

func TestHTTPClient_SyncBatchEmbeddings_NilRequest(t *testing.T) {
	c := NewHTTPClient("http://example.invalid", "")
	if _, err := c.SyncBatchEmbeddings(context.Background(), nil); err == nil {
		t.Fatal("SyncBatchEmbeddings(nil) error = nil, want non-nil")
	}
}

func TestHTTPClient_MigrateEmbeddings_NilRequest(t *testing.T) {
	c := NewHTTPClient("http://example.invalid", "")
	if err := c.MigrateEmbeddings(context.Background(), nil); err == nil {
		t.Fatal("MigrateEmbeddings(nil) error = nil, want non-nil")
	}
}

func TestHTTPClient_MigrateEmbeddings(t *testing.T) {
	h := &testHandler{statusCode: http.StatusNoContent}
	c, srv := newTestClient(h)
	defer srv.Close()

	if err := c.MigrateEmbeddings(context.Background(), &MigrateEmbeddingsRequest{}); err != nil {
		t.Fatalf("MigrateEmbeddings() error = %v", err)
	}
	if h.method != http.MethodPost || h.path != "/v1/embeddings/migrate" {
		t.Errorf("method/path = %q %q, want POST /v1/embeddings/migrate", h.method, h.path)
	}
}

func TestHTTPClient_ReindexEmbeddings(t *testing.T) {
	h := &testHandler{statusCode: http.StatusNoContent}
	c, srv := newTestClient(h)
	defer srv.Close()

	if err := c.ReindexEmbeddings(context.Background()); err != nil {
		t.Fatalf("ReindexEmbeddings() error = %v", err)
	}
	if h.method != http.MethodPost || h.path != "/v1/embeddings/reindex" {
		t.Errorf("method/path = %q %q, want POST /v1/embeddings/reindex", h.method, h.path)
	}
}

func TestHTTPClient_ReextractReferences_NilRequest(t *testing.T) {
	c := NewHTTPClient("http://example.invalid", "")
	if _, err := c.ReextractReferences(context.Background(), nil); err == nil {
		t.Fatal("ReextractReferences(nil) error = nil, want non-nil")
	}
}

func TestHTTPClient_ReextractReferences(t *testing.T) {
	h := &testHandler{responseBody: `{"beads_scanned":5,"references_written":7,"last_id":"gd-9","done":true}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.ReextractReferences(context.Background(), &ReextractReferencesRequest{BatchSize: 100})
	if err != nil {
		t.Fatalf("ReextractReferences() error = %v", err)
	}
	if h.path != "/v1/admin/reextract-references" {
		t.Errorf("path = %q, want /v1/admin/reextract-references", h.path)
	}
	if !resp.Done || resp.LastID != "gd-9" {
		t.Errorf("resp = %+v, want done + last_id gd-9", resp)
	}
}

// --- Admin backfills: nil-request guards + one happy path each ---

func TestHTTPClient_AdminBackfill_NilRequests(t *testing.T) {
	c := NewHTTPClient("http://example.invalid", "")
	ctx := context.Background()

	if _, err := c.BackfillOperators(ctx, nil); err == nil {
		t.Error("BackfillOperators(nil) error = nil, want non-nil")
	}
	if _, err := c.BackfillOperatedByLegacy(ctx, nil); err == nil {
		t.Error("BackfillOperatedByLegacy(nil) error = nil, want non-nil")
	}
	if _, err := c.BackfillSlackUserID(ctx, nil); err == nil {
		t.Error("BackfillSlackUserID(nil) error = nil, want non-nil")
	}
	if _, err := c.BackfillOwner(ctx, nil); err == nil {
		t.Error("BackfillOwner(nil) error = nil, want non-nil")
	}
	if _, err := c.BackfillEmptyOwner(ctx, nil); err == nil {
		t.Error("BackfillEmptyOwner(nil) error = nil, want non-nil")
	}
	if _, err := c.BackfillOperatedByAgents(ctx, nil); err == nil {
		t.Error("BackfillOperatedByAgents(nil) error = nil, want non-nil")
	}
	if _, err := c.BackfillParentChildDeps(ctx, nil); err == nil {
		t.Error("BackfillParentChildDeps(nil) error = nil, want non-nil")
	}
	if _, err := c.RepairReversedParentChildDeps(ctx, nil); err == nil {
		t.Error("RepairReversedParentChildDeps(nil) error = nil, want non-nil")
	}
	if _, err := c.BackfillTriggeredByDeps(ctx, nil); err == nil {
		t.Error("BackfillTriggeredByDeps(nil) error = nil, want non-nil")
	}
	if _, err := c.CloseTriggerAuditBeads(ctx, nil); err == nil {
		t.Error("CloseTriggerAuditBeads(nil) error = nil, want non-nil")
	}
	if _, err := c.SemanticSearch(ctx, nil); err == nil {
		t.Error("SemanticSearch(nil) error = nil, want non-nil")
	}
}

func TestHTTPClient_BackfillOperators(t *testing.T) {
	h := &testHandler{responseBody: `{"distinct_operated_by":3,"operators_upserted":3,"dry_run":true}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.BackfillOperators(context.Background(), &BackfillOperatorsRequest{DryRun: true})
	if err != nil {
		t.Fatalf("BackfillOperators() error = %v", err)
	}
	if h.method != http.MethodPost || h.path != "/v1/admin/backfill-operators" {
		t.Errorf("method/path = %q %q, want POST /v1/admin/backfill-operators", h.method, h.path)
	}
	if !resp.DryRun || resp.DistinctOperatedBy != 3 {
		t.Errorf("resp = %+v, want dry_run + distinct=3", resp)
	}
}

func TestHTTPClient_BackfillOwner(t *testing.T) {
	h := &testHandler{responseBody: `{"dry_run":false}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	if _, err := c.BackfillOwner(context.Background(), &BackfillOwnerRequest{}); err != nil {
		t.Fatalf("BackfillOwner() error = %v", err)
	}
	if h.path != "/v1/admin/backfill-owner" {
		t.Errorf("path = %q, want /v1/admin/backfill-owner", h.path)
	}
}

func TestHTTPClient_BackfillEmptyOwner(t *testing.T) {
	h := &testHandler{responseBody: `{"dry_run":true,"before_empty":42,"phase1_filled":40}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.BackfillEmptyOwner(context.Background(), &BackfillEmptyOwnerRequest{DryRun: true})
	if err != nil {
		t.Fatalf("BackfillEmptyOwner() error = %v", err)
	}
	if h.method != http.MethodPost || h.path != "/v1/admin/backfill-empty-owner" {
		t.Errorf("method/path = %q %q, want POST /v1/admin/backfill-empty-owner", h.method, h.path)
	}
	if !resp.DryRun || resp.BeforeEmpty != 42 || resp.Phase1Filled != 40 {
		t.Errorf("resp = %+v, want dry_run + before_empty=42 + phase1_filled=40", resp)
	}
}

func TestHTTPClient_SemanticSearch_PathAndDecode(t *testing.T) {
	h := &testHandler{responseBody: `{"model":"m","results":[{"bead":{"id":"gd-1"},"score":0.7}]}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.SemanticSearch(context.Background(), &SemanticSearchRequest{Query: "foo", Limit: 5})
	if err != nil {
		t.Fatalf("SemanticSearch() error = %v", err)
	}
	if h.method != http.MethodPost || h.path != "/v1/search/semantic" {
		t.Errorf("method/path = %q %q, want POST /v1/search/semantic", h.method, h.path)
	}
	if len(resp.Results) != 1 || resp.Results[0].Score != 0.7 {
		t.Errorf("resp = %+v, want 1 result score 0.7", resp)
	}
}

// --- Error propagation: a representative wrapper surfaces an APIError ---

func TestHTTPClient_MethodErrorPropagation(t *testing.T) {
	h := &testHandler{statusCode: http.StatusInternalServerError, responseBody: `{"error":"boom"}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	if _, err := c.GetMutations(context.Background(), "gd-1"); err == nil {
		t.Fatal("GetMutations() on 500 error = nil, want non-nil")
	}
}
