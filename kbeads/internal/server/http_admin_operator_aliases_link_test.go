package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
)

func TestHandleAdminLinkOperatorAliases_FreshOIDCPlusSlackCreatesRows(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(AdminLinkOperatorAliasesRequest{
		Aliases: []OperatorAliasRef{
			{Kind: "oidc", Subject: "alice@example.com"},
			{Kind: "slack", Subject: "U1"},
		},
		Source: "user-bead-bridge",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/operators/link-aliases", bytes.NewReader(body)).WithContext(withAuthMethod(context.Background(), authMethodNone))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleAdminLinkOperatorAliases(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp AdminLinkOperatorAliasesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OperatorID == 0 {
		t.Errorf("OperatorID = 0, want non-zero")
	}
	if resp.AliasesInserted != 2 {
		t.Errorf("AliasesInserted = %d, want 2", resp.AliasesInserted)
	}
	if resp.AlreadyLinked {
		t.Errorf("AlreadyLinked = true, want false for fresh link")
	}
	if got := ms.operatorAliases["oidc\x00alice@example.com"]; got != resp.OperatorID {
		t.Errorf("oidc alias = %d, want %d", got, resp.OperatorID)
	}
	if got := ms.operatorAliases["slack\x00U1"]; got != resp.OperatorID {
		t.Errorf("slack alias = %d, want %d", got, resp.OperatorID)
	}
}

func TestHandleAdminLinkOperatorAliases_IdempotentReplay(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	body, _ := json.Marshal(AdminLinkOperatorAliasesRequest{
		Aliases: []OperatorAliasRef{
			{Kind: "oidc", Subject: "alice@example.com"},
			{Kind: "slack", Subject: "U1"},
		},
	})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/operators/link-aliases", bytes.NewReader(body)).WithContext(withAuthMethod(context.Background(), authMethodNone))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleAdminLinkOperatorAliases(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: expected 200, got %d; body: %s", i, rec.Code, rec.Body.String())
		}
		if i == 1 {
			var resp AdminLinkOperatorAliasesResponse
			_ = json.NewDecoder(rec.Body).Decode(&resp)
			if !resp.AlreadyLinked {
				t.Errorf("second pass: AlreadyLinked = false, want true")
			}
			if resp.AliasesInserted != 0 || resp.AliasesUpdated != 0 {
				t.Errorf("second pass writes: inserted=%d updated=%d, want 0/0", resp.AliasesInserted, resp.AliasesUpdated)
			}
			if len(resp.MergedFromIDs) != 0 {
				t.Errorf("second pass MergedFromIDs = %v, want empty", resp.MergedFromIDs)
			}
		}
	}
}

func TestHandleAdminLinkOperatorAliases_MergesLegacySlackUnderOIDC(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	// Seed legacy slack-only operator with one alias (bridge created it before
	// the user ever logged into beads-viewer).
	slackID, _ := ms.UpsertOperator(ctx, "slack", "U1")
	if err := ms.UpsertOperatorAlias(ctx, slackID, "slack", "U1", "resolver"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Seed the OIDC operator separately so the endpoint sees two distinct
	// rows and picks the OIDC one as survivor.
	oidcID, _ := ms.UpsertOperator(ctx, "oidc", "alice@example.com")
	if err := ms.UpsertOperatorAlias(ctx, oidcID, "oidc", "alice@example.com", "auto-provision"); err != nil {
		t.Fatalf("seed oidc: %v", err)
	}
	ms.mergedOperatorRows = 7

	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	body, _ := json.Marshal(AdminLinkOperatorAliasesRequest{
		Aliases: []OperatorAliasRef{
			{Kind: "oidc", Subject: "alice@example.com"},
			{Kind: "slack", Subject: "U1"},
		},
		Source: "user-bead-bridge",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/operators/link-aliases", bytes.NewReader(body)).WithContext(withAuthMethod(context.Background(), authMethodNone))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleAdminLinkOperatorAliases(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp AdminLinkOperatorAliasesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OperatorID != oidcID {
		t.Errorf("OperatorID = %d, want oidcID=%d (survivor must be the OIDC row)", resp.OperatorID, oidcID)
	}
	if len(resp.MergedFromIDs) != 1 || resp.MergedFromIDs[0] != slackID {
		t.Errorf("MergedFromIDs = %v, want [%d]", resp.MergedFromIDs, slackID)
	}
	if resp.RowsRepointed != 7 {
		t.Errorf("RowsRepointed = %d, want 7", resp.RowsRepointed)
	}
	if got := ms.operatorAliases["slack\x00U1"]; got != oidcID {
		t.Errorf("slack alias should now point at OIDC operator %d, got %d", oidcID, got)
	}
	if got := ms.operatorMerges[slackID]; got != oidcID {
		t.Errorf("operatorMerges[%d] = %d, want %d", slackID, got, oidcID)
	}
}

func TestHandleAdminLinkOperatorAliases_AddsMissingSlackToExistingOIDC(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	oidcID, _ := ms.UpsertOperator(ctx, "oidc", "alice@example.com")
	if err := ms.UpsertOperatorAlias(ctx, oidcID, "oidc", "alice@example.com", "auto-provision"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	body, _ := json.Marshal(AdminLinkOperatorAliasesRequest{
		Aliases: []OperatorAliasRef{
			{Kind: "oidc", Subject: "alice@example.com"},
			{Kind: "slack", Subject: "U1"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/operators/link-aliases", bytes.NewReader(body)).WithContext(withAuthMethod(context.Background(), authMethodNone))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleAdminLinkOperatorAliases(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp AdminLinkOperatorAliasesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OperatorID != oidcID {
		t.Errorf("OperatorID = %d, want %d", resp.OperatorID, oidcID)
	}
	if resp.AliasesInserted != 1 {
		t.Errorf("AliasesInserted = %d, want 1", resp.AliasesInserted)
	}
	if len(resp.MergedFromIDs) != 0 {
		t.Errorf("MergedFromIDs = %v, want empty (no displaced operator)", resp.MergedFromIDs)
	}
	if got := ms.operatorAliases["slack\x00U1"]; got != oidcID {
		t.Errorf("slack alias = %d, want %d", got, oidcID)
	}
}

// TestHandleAdminLinkOperatorAliases_CrossOrgMergeCollapsesDirectoryRow is the
// end-to-end regression pin for ka-0000004514: one human with two org emails
// (sam.rivers@acme.example + srivers@contractor-a.example) rendered as TWO
// /people rows because each carried its own slug and the viewer folds rows by
// email key — two different emails never fold. A cross-org operator merge must
// collapse the directory to ONE row: the loser's slug is absorbed by the
// merge so ListOperatorsWithSlugs (the /v1/operators feed) stops emitting it,
// while the survivor keeps its rich row.
func TestHandleAdminLinkOperatorAliases_CrossOrgMergeCollapsesDirectoryRow(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()

	// Survivor: the rich acme.example identity, slugged.
	survivorID, _ := ms.UpsertOperator(ctx, "oidc", "sam.rivers@acme.example")
	if err := ms.UpsertOperatorAlias(ctx, survivorID, "oidc", "sam.rivers@acme.example", "auto-provision"); err != nil {
		t.Fatalf("seed survivor alias: %v", err)
	}
	seedOperator(ms, survivorID, "oidc", "sam.rivers@acme.example", "Frank Rivers", "sam.rivers@acme.example", "sam.rivers", false)

	// Loser: the second org email, its own slugged (near-empty) row.
	loserID, _ := ms.UpsertOperator(ctx, "oidc", "srivers@contractor-a.example")
	if err := ms.UpsertOperatorAlias(ctx, loserID, "oidc", "srivers@contractor-a.example", "auto-provision"); err != nil {
		t.Fatalf("seed loser alias: %v", err)
	}
	seedOperator(ms, loserID, "oidc", "srivers@contractor-a.example", "", "srivers@contractor-a.example", "srivers", false)

	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	// Precondition: both rows are on the directory feed (the two-row bug).
	if got := listOperatorSlugs(t, srv); len(got) != 2 {
		t.Fatalf("precondition: directory feed len = %d, want 2 (the bug); got %v", len(got), got)
	}

	// Merge the two OIDC identities. The survivor is chosen by the handler's
	// oidc-first rule; with both aliases oidc the first-found (survivor) wins,
	// so pass it first.
	body, _ := json.Marshal(AdminLinkOperatorAliasesRequest{
		Aliases: []OperatorAliasRef{
			{Kind: "oidc", Subject: "sam.rivers@acme.example"},
			{Kind: "oidc", Subject: "srivers@contractor-a.example"},
		},
		Source: "operator-alias-merge",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/operators/link-aliases", bytes.NewReader(body)).WithContext(withAuthMethod(context.Background(), authMethodNone))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleAdminLinkOperatorAliases(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp AdminLinkOperatorAliasesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OperatorID != survivorID {
		t.Errorf("survivor = %d, want %d", resp.OperatorID, survivorID)
	}
	if len(resp.MergedFromIDs) != 1 || resp.MergedFromIDs[0] != loserID {
		t.Errorf("MergedFromIDs = %v, want [%d]", resp.MergedFromIDs, loserID)
	}

	// Postcondition: the directory feed now has ONE row — the survivor. The
	// loser's slug was absorbed by the merge so it no longer renders.
	gotSlugs := listOperatorSlugs(t, srv)
	if len(gotSlugs) != 1 || gotSlugs[0] != "sam.rivers" {
		t.Fatalf("directory feed after merge = %v, want [sam.rivers] (one row)", gotSlugs)
	}
	// The crux alias now resolves to the survivor so future writes/logins as
	// the merged-away identity route to the one row (durability).
	if got := ms.operatorAliases["oidc\x00srivers@contractor-a.example"]; got != survivorID {
		t.Errorf("crux alias = %d, want survivor %d after merge", got, survivorID)
	}
}

// listOperatorSlugs GETs /v1/operators and returns the rendered slugs.
func listOperatorSlugs(t *testing.T, srv *BeadsServer) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/operators?limit=500", nil)
	rec := httptest.NewRecorder()
	srv.handleListOperators(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list operators: status %d; body %s", rec.Code, rec.Body.String())
	}
	var resp ListOperatorsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	out := make([]string, 0, len(resp.Operators))
	for _, op := range resp.Operators {
		out = append(out, op.Slug)
	}
	return out
}

func TestHandleAdminLinkOperatorAliases_RejectsFewerThanTwoAliases(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	body, _ := json.Marshal(AdminLinkOperatorAliasesRequest{
		Aliases: []OperatorAliasRef{{Kind: "oidc", Subject: "alice@example.com"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/operators/link-aliases", bytes.NewReader(body)).WithContext(withAuthMethod(context.Background(), authMethodNone))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleAdminLinkOperatorAliases(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAdminLinkOperatorAliases_RejectsEmptyKindOrSubject(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	body, _ := json.Marshal(AdminLinkOperatorAliasesRequest{
		Aliases: []OperatorAliasRef{
			{Kind: "oidc", Subject: ""},
			{Kind: "slack", Subject: "U1"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/operators/link-aliases", bytes.NewReader(body)).WithContext(withAuthMethod(context.Background(), authMethodNone))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleAdminLinkOperatorAliases(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
