package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"gardenio/kbeads/internal/events"
)

func TestHandleListSlackOperatorAliases_ReturnsAliasesJoinedWithIdentity(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()

	id1, _ := ms.UpsertOperator(ctx, "slack", "U1")
	id2, _ := ms.UpsertOperator(ctx, "slack", "U2")
	idOIDC, _ := ms.UpsertOperator(ctx, "oidc", "alice@example.com")
	if err := ms.UpsertOperatorAlias(ctx, id1, "slack", "U1", "resolver"); err != nil {
		t.Fatalf("seed alias U1: %v", err)
	}
	if err := ms.UpsertOperatorAlias(ctx, id2, "slack", "U2", "resolver"); err != nil {
		t.Fatalf("seed alias U2: %v", err)
	}
	if err := ms.UpsertOperatorAlias(ctx, idOIDC, "oidc", "alice@example.com", "resolver"); err != nil {
		t.Fatalf("seed alias oidc: %v", err)
	}
	if _, err := ms.UpdateOperatorIdentity(ctx, id1, "Alice", "alice@example.com"); err != nil {
		t.Fatalf("seed identity 1: %v", err)
	}

	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/operators/slack-aliases", nil)
	rec := httptest.NewRecorder()
	srv.handleListSlackOperatorAliases(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp ListSlackOperatorAliasesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := len(resp.Aliases); got != 2 {
		t.Fatalf("expected 2 slack aliases (oidc filtered out), got %d: %+v", got, resp.Aliases)
	}
	byID := map[int64]SlackOperatorAlias{}
	for _, a := range resp.Aliases {
		byID[a.OperatorID] = a
	}
	if a := byID[id1]; a.Subject != "U1" || a.Display != "Alice" || a.Email != "alice@example.com" {
		t.Errorf("alias 1 = %+v, want U1/Alice/alice@example.com", a)
	}
	if a := byID[id2]; a.Subject != "U2" || a.Display != "" || a.Email != "" {
		t.Errorf("alias 2 = %+v, want U2 with empty identity", a)
	}
}

func TestHandleUpdateOperatorIdentity_UpdatesRow(t *testing.T) {
	ms := newMockStore()
	ctx := context.Background()
	id, _ := ms.UpsertOperator(ctx, "slack", "U1")

	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	body, _ := json.Marshal(UpdateOperatorIdentityRequest{Display: "Alice", Email: "alice@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/operators/1/identity", bytes.NewReader(body)).WithContext(adminOIDCCtx())
	req.SetPathValue("id", "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleUpdateOperatorIdentity(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp UpdateOperatorIdentityResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Updated {
		t.Errorf("Updated = false, want true")
	}
	if got := ms.operatorIdentities[id]; got.Display != "Alice" || got.Email != "alice@example.com" {
		t.Errorf("operatorIdentities[%d] = %+v, want Alice/alice@example.com", id, got)
	}
}

func TestHandleUpdateOperatorIdentity_MissingOperatorReturnsUpdatedFalse(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	body, _ := json.Marshal(UpdateOperatorIdentityRequest{Display: "Ghost", Email: "g@x"})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/operators/999/identity", bytes.NewReader(body)).WithContext(adminOIDCCtx())
	req.SetPathValue("id", "999")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleUpdateOperatorIdentity(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp UpdateOperatorIdentityResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Updated {
		t.Errorf("Updated = true for missing operator, want false")
	}
}

func TestHandleUpdateOperatorIdentity_BadIDReturns400(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	body, _ := json.Marshal(UpdateOperatorIdentityRequest{Display: "x", Email: "y"})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/operators/abc/identity", bytes.NewReader(body))
	req.SetPathValue("id", "abc")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleUpdateOperatorIdentity(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

const identityPath = "/v1/admin/operators/1/identity"

// postIdentity sends body to handleUpdateOperatorIdentity for operator id as ctx.
func postIdentity(t *testing.T, ctx context.Context, srv *BeadsServer, id int64, body UpdateOperatorIdentityRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, identityPath, bytes.NewReader(b)).WithContext(ctx)
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleUpdateOperatorIdentity(rec, req)
	return rec
}

// BYPASS REPRODUCTION (gd-iRFGU6xSK7): the static token every agent pod holds
// renamed operator #N and put a real human's email on it. A machine caller may
// not choose display or email at all.
func TestHandleUpdateOperatorIdentity_MachineExplicitValues_403(t *testing.T) {
	for name, ctx := range map[string]context.Context{"static": machineUserCtx(), "fleet": fleetUserCtx()} {
		for _, body := range []UpdateOperatorIdentityRequest{
			{Display: "Owner", Email: "owner@example.com"},
			{Display: "Owner"},
			{Email: "owner@example.com"},
		} {
			t.Run(name, func(t *testing.T) {
				ms := newMockStore()
				srv := NewBeadsServer(ms, &events.NoopPublisher{})
				id, _ := ms.UpsertOperator(context.Background(), "oidc", "attacker@example.com")
				rec := postIdentity(t, ctx, srv, id, body)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("code = %d, want 403; body %s", rec.Code, rec.Body.String())
				}
				if got, ok := ms.operatorIdentities[id]; ok {
					t.Errorf("operator %d identity written despite 403: %+v", id, got)
				}
			})
		}
	}
}

// An OIDC gardenio-admin may still set explicit values.
func TestHandleUpdateOperatorIdentity_AdminExplicitValues(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	id, _ := ms.UpsertOperator(context.Background(), "slack", "U1")
	rec := postIdentity(t, adminOIDCCtx(), srv, id, UpdateOperatorIdentityRequest{Display: "Alice", Email: "alice@example.com"})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body %s", rec.Code, rec.Body.String())
	}
	if got := ms.operatorIdentities[id]; got.Display != "Alice" || got.Email != "alice@example.com" {
		t.Errorf("identity = %+v, want Alice/alice@example.com", got)
	}
}

// seedAliasedOperator makes one operator with the given aliases.
func seedAliasedOperator(t *testing.T, ms *mockStore, aliases ...OperatorAliasRef) int64 {
	t.Helper()
	ctx := context.Background()
	id, _ := ms.UpsertOperator(ctx, aliases[0].Kind, aliases[0].Subject)
	for _, a := range aliases {
		if err := ms.UpsertOperatorAlias(ctx, id, a.Kind, a.Subject, "resolver"); err != nil {
			t.Fatalf("seed alias: %v", err)
		}
	}
	return id
}

// The machine path reads display and email from the user bead whose aliases
// resolve to the operator, whatever the caller would have liked them to be.
func TestHandleUpdateOperatorIdentity_MachineFromUserBead(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	owner := seedAliasedOperator(t, ms, OperatorAliasRef{Kind: "slack", Subject: "U1"})
	other := seedAliasedOperator(t, ms, OperatorAliasRef{Kind: "slack", Subject: "U2"})
	seedIdentityUserBead(t, ms, "gd-alice", `{"email":"Alice@Example.com","slack_id":"U1","display_name":"Alice A"}`)
	seedIdentityUserBead(t, ms, "gd-bob", `{"email":"bob@example.com","slack_id":"U2"}`)

	for i := 0; i < 2; i++ { // replay is a no-op
		rec := postIdentity(t, machineUserCtx(), srv, owner, UpdateOperatorIdentityRequest{FromUserBead: true})
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: code = %d; body %s", i, rec.Code, rec.Body.String())
		}
		var resp UpdateOperatorIdentityResponse
		_ = json.NewDecoder(rec.Body).Decode(&resp)
		if !resp.Updated || resp.Display != "Alice A" || resp.Email != "alice@example.com" {
			t.Errorf("call %d: resp = %+v", i, resp)
		}
	}
	if got := ms.operatorIdentities[owner]; got.Display != "Alice A" || got.Email != "alice@example.com" {
		t.Errorf("owner identity = %+v, want Alice A/alice@example.com", got)
	}
	// No display_name: the bead title stands in.
	if rec := postIdentity(t, fleetUserCtx(), srv, other, UpdateOperatorIdentityRequest{FromUserBead: true}); rec.Code != http.StatusOK {
		t.Fatalf("bob: code = %d; body %s", rec.Code, rec.Body.String())
	}
	if got := ms.operatorIdentities[other]; got.Display != "owner" || got.Email != "bob@example.com" {
		t.Errorf("bob identity = %+v, want title owner/bob@example.com", got)
	}
}

// from_user_bead with explicit values is a 400, for admins too, so a machine
// caller cannot smuggle values alongside the flag.
func TestHandleUpdateOperatorIdentity_FromUserBeadWithValues_400(t *testing.T) {
	for name, ctx := range map[string]context.Context{"static": machineUserCtx(), "admin": adminOIDCCtx()} {
		t.Run(name, func(t *testing.T) {
			ms := newMockStore()
			srv := NewBeadsServer(ms, &events.NoopPublisher{})
			id := seedAliasedOperator(t, ms, OperatorAliasRef{Kind: "slack", Subject: "U1"})
			seedIdentityUserBead(t, ms, "gd-alice", `{"email":"alice@example.com","slack_id":"U1"}`)
			rec := postIdentity(t, ctx, srv, id, UpdateOperatorIdentityRequest{FromUserBead: true, Display: "Owner"})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
			if _, ok := ms.operatorIdentities[id]; ok {
				t.Errorf("identity written despite 400")
			}
		})
	}
}

// No user bead (or one without an email) resolves to the operator: nothing to
// derive, nothing written.
func TestHandleUpdateOperatorIdentity_FromUserBeadNoBead(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	id := seedAliasedOperator(t, ms, OperatorAliasRef{Kind: "slack", Subject: "U1"})
	seedIdentityUserBead(t, ms, "gd-noemail", `{"slack_id":"U1","display_name":"Mallory"}`)
	rec := postIdentity(t, machineUserCtx(), srv, id, UpdateOperatorIdentityRequest{FromUserBead: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; body %s", rec.Code, rec.Body.String())
	}
	var resp UpdateOperatorIdentityResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if !resp.NoUserBead || resp.Updated {
		t.Errorf("resp = %+v, want no_user_bead", resp)
	}
	if _, ok := ms.operatorIdentities[id]; ok {
		t.Errorf("identity written with no user bead")
	}
}

// Two user beads resolve to one operator: refuse rather than pick one.
func TestHandleUpdateOperatorIdentity_FromUserBeadAmbiguous_409(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	id := seedAliasedOperator(t, ms,
		OperatorAliasRef{Kind: "slack", Subject: "U1"},
		OperatorAliasRef{Kind: "oidc", Subject: "alice@example.com"})
	seedIdentityUserBead(t, ms, "gd-a", `{"email":"alice@example.com"}`)
	seedIdentityUserBead(t, ms, "gd-b", `{"email":"someone@example.com","slack_id":"U1"}`)
	rec := postIdentity(t, machineUserCtx(), srv, id, UpdateOperatorIdentityRequest{FromUserBead: true})
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if _, ok := ms.operatorIdentities[id]; ok {
		t.Errorf("identity written despite 409")
	}
}
