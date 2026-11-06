package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// oidcLinkRequest builds a request with an OIDC identity in the context so
// handleLinkOperatorAlias takes the authenticated path.
func oidcLinkRequest(method, path, email string, body []byte) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := WithAuthIdentity(req.Context(), AuthIdentity{Type: "oidc", Subject: email})
	return req.WithContext(ctx)
}

// oidcLinkRequestRoles is oidcLinkRequest with the auth method and roles the
// RBAC middleware would have stamped.
func oidcLinkRequestRoles(email string, roles []string, body []byte) *http.Request {
	req := oidcLinkRequest(http.MethodPost, "/v1/operators/me/aliases", email, body)
	ctx := withAuthMethod(req.Context(), authMethodOIDC)
	return req.WithContext(WithRoles(ctx, roles))
}

// seedLinkUserBead stores a type:user bead with the given fields whose identity
// fields were written by actor (a create row, as for a bead born with them).
func seedLinkUserBead(ms *mockStore, id, actor string, fields map[string]any) {
	raw, _ := json.Marshal(fields)
	ms.beads[id] = &model.Bead{ID: id, Type: "user", Title: id, Status: model.StatusOpen, Fields: raw}
	_ = ms.RecordMutation(context.Background(), &model.MutationRow{
		BeadID: id, Actor: actor, Field: "*", NewValue: "<created>", MutationType: model.MutationTypeCreate,
	})
}

func TestHandleLinkOperatorAlias_FreshLinkCreatesAlias(t *testing.T) {
	ms := newMockStore()
	seedLinkUserBead(ms, "gd-u-alice", "admin@example.com", map[string]any{"email": "alice@example.com", "slack_id": "U1"})
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(LinkOperatorAliasRequest{Kind: "slack", Subject: "U1", Source: "identity-link"})
	req := oidcLinkRequest(http.MethodPost, "/v1/operators/me/aliases", "alice@example.com", body)
	rec := httptest.NewRecorder()

	srv.handleLinkOperatorAlias(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp LinkOperatorAliasResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OperatorID == 0 {
		t.Errorf("OperatorID = 0, want non-zero")
	}
	if resp.Merged || resp.AlreadyLinked {
		t.Errorf("fresh link: Merged=%v AlreadyLinked=%v, want both false", resp.Merged, resp.AlreadyLinked)
	}
	if got := ms.operatorAliases["slack\x00U1"]; got != resp.OperatorID {
		t.Errorf("slack alias = %d, want %d", got, resp.OperatorID)
	}
	if got := ms.operatorAliases["oidc\x00alice@example.com"]; got != resp.OperatorID {
		t.Errorf("oidc alias = %d, want %d", got, resp.OperatorID)
	}
}

func TestHandleLinkOperatorAlias_IdempotentReturnsAlreadyLinked(t *testing.T) {
	ms := newMockStore()
	seedLinkUserBead(ms, "gd-u-alice", "admin@example.com", map[string]any{"email": "alice@example.com", "slack_id": "U1"})
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(LinkOperatorAliasRequest{Kind: "slack", Subject: "U1", Source: "identity-link"})

	// First call: fresh link.
	req1 := oidcLinkRequest(http.MethodPost, "/v1/operators/me/aliases", "alice@example.com", body)
	rec1 := httptest.NewRecorder()
	srv.handleLinkOperatorAlias(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d", rec1.Code)
	}
	var first LinkOperatorAliasResponse
	_ = json.NewDecoder(rec1.Body).Decode(&first)

	// Second call: same caller + alias. Must return AlreadyLinked=true with
	// the same operator id and no merge.
	req2 := oidcLinkRequest(http.MethodPost, "/v1/operators/me/aliases", "alice@example.com", body)
	rec2 := httptest.NewRecorder()
	srv.handleLinkOperatorAlias(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second call: expected 200, got %d", rec2.Code)
	}
	var second LinkOperatorAliasResponse
	if err := json.NewDecoder(rec2.Body).Decode(&second); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !second.AlreadyLinked {
		t.Errorf("AlreadyLinked = false, want true")
	}
	if second.OperatorID != first.OperatorID {
		t.Errorf("OperatorID = %d, want %d (same as first call)", second.OperatorID, first.OperatorID)
	}
	if second.Merged {
		t.Errorf("Merged = true, want false (no merge on idempotent replay)")
	}
}

func linkBody(kind, subject string) []byte {
	b, _ := json.Marshal(LinkOperatorAliasRequest{Kind: kind, Subject: subject, Source: "identity-link"})
	return b
}

// An OIDC non-admin may not claim an alias that sits on someone else's user
// bead, nor one that is on no bead at all.
func TestHandleLinkOperatorAlias_NonAdminClaimingSomeoneElsesAlias403(t *testing.T) {
	ms := newMockStore()
	seedLinkUserBead(ms, "gd-u-owner", "admin@example.com", map[string]any{"email": "owner@example.com", "slack_id": "UOWNER"})
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	for _, subject := range []string{"UOWNER", "UNOBODY"} {
		rec := httptest.NewRecorder()
		srv.handleLinkOperatorAlias(rec, oidcLinkRequestRoles("attacker@example.com", []string{RoleOperator}, linkBody("slack", subject)))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("slack:%s: expected 403, got %d; body: %s", subject, rec.Code, rec.Body.String())
		}
	}
	if len(ms.operatorAliases) != 0 || len(ms.operatorMerges) != 0 {
		t.Errorf("refused link wrote state: aliases=%v merges=%v", ms.operatorAliases, ms.operatorMerges)
	}
}

// A bead whose identity values were written by an agent/static token proves
// nothing, even when the email matches the caller.
func TestHandleLinkOperatorAlias_MachineWrittenUserBeadIsNotProof(t *testing.T) {
	ms := newMockStore()
	seedLinkUserBead(ms, "gd-u-evil", "gardenio-some-agent-abcd", map[string]any{"email": "attacker@example.com", "slack_id": "UOWNER"})
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	rec := httptest.NewRecorder()
	srv.handleLinkOperatorAlias(rec, oidcLinkRequestRoles("attacker@example.com", []string{RoleOperator}, linkBody("slack", "UOWNER")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// A later non-identity write by a machine token (a status_message edit) must
// not strip the admin's authorship of the identity fields.
func TestHandleLinkOperatorAlias_MachineEditOfOtherFieldKeepsProof(t *testing.T) {
	ms := newMockStore()
	seedLinkUserBead(ms, "gd-u-alice", "admin@example.com", map[string]any{"email": "alice@example.com", "slack_id": "U1"})
	_ = ms.RecordMutation(context.Background(), &model.MutationRow{
		BeadID: "gd-u-alice", Actor: "static", Field: "fields", MutationType: model.MutationTypeUpdate,
		OldValue: `{"email":"alice@example.com","slack_id":"U1"}`,
		// New value as the daemon records it: a json.RawMessage under %v.
		NewValue: fmtValue(json.RawMessage(`{"email":"alice@example.com","slack_id":"U1","status_message":"hi"}`)),
	})
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	rec := httptest.NewRecorder()
	srv.handleLinkOperatorAlias(rec, oidcLinkRequestRoles("alice@example.com", []string{RoleOperator}, linkBody("slack", "U1")))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// A machine write that CHANGES an identity field after the admin's create
// takes the proof away.
func TestHandleLinkOperatorAlias_MachineRewriteOfIdentityFieldBreaksProof(t *testing.T) {
	ms := newMockStore()
	seedLinkUserBead(ms, "gd-u-alice", "admin@example.com", map[string]any{"email": "alice@example.com"})
	ms.beads["gd-u-alice"].Fields = json.RawMessage(`{"email":"alice@example.com","slack_id":"UOWNER"}`)
	_ = ms.RecordMutation(context.Background(), &model.MutationRow{
		BeadID: "gd-u-alice", Actor: "static", Field: "fields", MutationType: model.MutationTypeUpdate,
		OldValue: `{"email":"alice@example.com"}`,
		NewValue: fmtValue(json.RawMessage(`{"email":"alice@example.com","slack_id":"UOWNER"}`)),
	})
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	rec := httptest.NewRecorder()
	srv.handleLinkOperatorAlias(rec, oidcLinkRequestRoles("alice@example.com", []string{RoleOperator}, linkBody("slack", "UOWNER")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// A backed self-link whose alias already belongs to another operator is a
// 409 and never a merge — for a non-admin and for an admin alike.
func TestHandleLinkOperatorAlias_CollisionIs409AndNeverMerges(t *testing.T) {
	for name, roles := range map[string][]string{"operator": {RoleOperator}, "admin": {RoleAdmin}} {
		t.Run(name, func(t *testing.T) {
			ms := newMockStore()
			ctx := context.Background()
			seedLinkUserBead(ms, "gd-u-alice", "admin@example.com", map[string]any{"email": "alice@example.com", "slack_id": "U1"})
			slackID, _ := ms.UpsertOperator(ctx, "slack", "U1")
			if err := ms.UpsertOperatorAlias(ctx, slackID, "slack", "U1", "resolver"); err != nil {
				t.Fatalf("seed slack alias: %v", err)
			}
			srv := NewBeadsServer(ms, &events.NoopPublisher{})

			rec := httptest.NewRecorder()
			srv.handleLinkOperatorAlias(rec, oidcLinkRequestRoles("alice@example.com", roles, linkBody("slack", "U1")))
			if rec.Code != http.StatusConflict {
				t.Fatalf("expected 409, got %d; body: %s", rec.Code, rec.Body.String())
			}
			if len(ms.operatorMerges) != 0 {
				t.Errorf("operators merged on the self-link path: %v", ms.operatorMerges)
			}
			if got := ms.operatorAliases["slack\x00U1"]; got != slackID {
				t.Errorf("slack alias moved to %d, want it left on %d", got, slackID)
			}
		})
	}
}

// The admin path needs no user bead.
func TestHandleLinkOperatorAlias_AdminNeedsNoUserBead(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	rec := httptest.NewRecorder()
	srv.handleLinkOperatorAlias(rec, oidcLinkRequestRoles("admin@example.com", []string{RoleAdmin}, linkBody("slack", "U9")))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if ms.operatorAliases["slack\x00U9"] == 0 {
		t.Errorf("admin link did not write the slack alias")
	}
}

func TestHandleLinkOperatorAlias_RequiresOIDC(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(LinkOperatorAliasRequest{Kind: "slack", Subject: "U1", Source: "identity-link"})
	// No OIDC identity attached — anonymous.
	req := httptest.NewRequest(http.MethodPost, "/v1/operators/me/aliases", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleLinkOperatorAlias(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleLinkOperatorAlias_RejectsOIDCKind(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(LinkOperatorAliasRequest{Kind: "oidc", Subject: "bob@example.com", Source: "identity-link"})
	req := oidcLinkRequest(http.MethodPost, "/v1/operators/me/aliases", "alice@example.com", body)
	rec := httptest.NewRecorder()
	srv.handleLinkOperatorAlias(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for kind=oidc, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleLinkOperatorAlias_RejectsMissingFields(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	body, _ := json.Marshal(LinkOperatorAliasRequest{Kind: "slack", Subject: "", Source: "identity-link"})
	req := oidcLinkRequest(http.MethodPost, "/v1/operators/me/aliases", "alice@example.com", body)
	rec := httptest.NewRecorder()
	srv.handleLinkOperatorAlias(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing subject, got %d", rec.Code)
	}
}
