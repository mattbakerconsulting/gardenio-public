package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
)

// computeWhoami() unit tests — the small auth-context resolver that the gRPC
// + HTTP + Connect handlers all share.

func TestComputeWhoami_Anonymous(t *testing.T) {
	op, src := computeWhoami(context.Background())
	if op != "" {
		t.Errorf("operator_id = %q, want empty", op)
	}
	if src != "anonymous" {
		t.Errorf("source = %q, want anonymous", src)
	}
}

func TestComputeWhoami_OIDCIsAuth(t *testing.T) {
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "matt@acme.example"})
	op, src := computeWhoami(ctx)
	if op != "matt@acme.example" {
		t.Errorf("operator_id = %q", op)
	}
	if src != "auth" {
		t.Errorf("source = %q, want auth", src)
	}
}

func TestComputeWhoami_FleetIsToken(t *testing.T) {
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "gardenio"})
	op, src := computeWhoami(ctx)
	if op != "gardenio" || src != "token" {
		t.Errorf("got (%q,%q), want (gardenio,token)", op, src)
	}
}

func TestComputeWhoami_UnknownTypeIsAnonymous(t *testing.T) {
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "anonymous"})
	op, src := computeWhoami(ctx)
	if op != "" || src != "anonymous" {
		t.Errorf("got (%q,%q), want (empty,anonymous)", op, src)
	}
}

func TestWhoamiGRPC_AnonymousByDefault(t *testing.T) {
	srv, _, _ := newTestServer()
	resp, err := srv.Whoami(context.Background(), &beadsv1.WhoamiRequest{})
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if resp.GetOperatorId() != "" {
		t.Errorf("operator_id = %q, want empty", resp.GetOperatorId())
	}
	if resp.GetSource() != "anonymous" {
		t.Errorf("source = %q, want anonymous", resp.GetSource())
	}
}

func TestWhoamiGRPC_OIDCIdentitySurfaces(t *testing.T) {
	srv, _, _ := newTestServer()
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "matt@acme.example"})
	resp, err := srv.Whoami(ctx, &beadsv1.WhoamiRequest{})
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if resp.GetOperatorId() != "matt@acme.example" {
		t.Errorf("operator_id = %q", resp.GetOperatorId())
	}
	if resp.GetSource() != "auth" {
		t.Errorf("source = %q, want auth", resp.GetSource())
	}
}

func TestHandleWhoami_HTTP_ReturnsAnonymous(t *testing.T) {
	_, _, h := newTestServerNoAuth()
	req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var got whoamiJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if got.OperatorID != "" {
		t.Errorf("operator_id = %q, want empty", got.OperatorID)
	}
	if got.Source != "anonymous" {
		t.Errorf("source = %q, want anonymous", got.Source)
	}
}

func TestHandleWhoami_HTTP_WithBearerAuth(t *testing.T) {
	_, _, h := newTestServerAuth()
	rr := doJSONAs(t, h, http.MethodGet, "/v1/whoami", nil, "matt@acme.example")
	requireStatus(t, rr, http.StatusOK)
	var got whoamiJSON
	decodeJSON(t, rr, &got)
	if got.OperatorID != "matt@acme.example" {
		t.Errorf("operator_id = %q", got.OperatorID)
	}
	if got.Source != "auth" {
		t.Errorf("source = %q, want auth", got.Source)
	}
}
