package beadsapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestApproveEscalation_UsesUserBearerOnWire verifies that when a per-request
// user bearer is attached, ApproveEscalation forces it onto the wire —
// overriding the static read token — so the daemon authenticates the call AS
// THE CALLING HUMAN directly, mirroring TestCreateTaint_UsesUserBearerOnWire.
// This is the path beads-viewer's web UI takes (operatorMiddleware always
// attaches the caller's OIDC bearer when present) and closes the
// shared-static-token-forgery risk described in gd-J8Fet2YH6y.
func TestApproveEscalation_UsesUserBearerOnWire(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")

	ctx := WithUserBearer(context.Background(), "approver-jwt-xyz")
	if err := c.ApproveEscalation(ctx, "gd-esc", ApproveEscalationRequest{ApprovedBy: "oidc:a@b.example", ApprovalMethod: "web_ui"}); err != nil {
		t.Fatalf("ApproveEscalation: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/escalations/gd-esc/approve" {
		t.Fatalf("expected POST /v1/escalations/gd-esc/approve, got %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer approver-jwt-xyz" {
		t.Fatalf("ApproveEscalation must authenticate as the user; Authorization = %q, want %q", gotAuth, "Bearer approver-jwt-xyz")
	}
}

// TestRejectEscalation_UsesUserBearerOnWire mirrors
// TestApproveEscalation_UsesUserBearerOnWire for the reject path.
func TestRejectEscalation_UsesUserBearerOnWire(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")

	ctx := WithUserBearer(context.Background(), "rejector-jwt-xyz")
	if err := c.RejectEscalation(ctx, "gd-esc", RejectEscalationRequest{RejectedBy: "oidc:a@b.example", Reason: "no"}); err != nil {
		t.Fatalf("RejectEscalation: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/escalations/gd-esc/reject" {
		t.Fatalf("expected POST /v1/escalations/gd-esc/reject, got %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer rejector-jwt-xyz" {
		t.Fatalf("RejectEscalation must authenticate as the user; Authorization = %q, want %q", gotAuth, "Bearer rejector-jwt-xyz")
	}
}

// TestApproveEscalation_FallsBackToStaticTokenWithoutBearer verifies
// ApproveEscalation still succeeds via the ordinary static-token +
// X-Operated-By relay when ctx carries no user bearer — the `gb escalation
// approve` CLI (headless agents, no human OIDC token) and the Slack bridge's
// escalation web API (resolves the approver from the Slack-linked user bead,
// not a browser session) both depend on this fallback continuing to work.
func TestApproveEscalation_FallsBackToStaticTokenWithoutBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")

	if err := c.ApproveEscalation(context.Background(), "gd-esc", ApproveEscalationRequest{ApprovedBy: "cli-actor"}); err != nil {
		t.Fatalf("ApproveEscalation: %v", err)
	}
	if gotAuth != "Bearer read-only-token" {
		t.Fatalf("expected the static token as fallback; Authorization = %q, want %q", gotAuth, "Bearer read-only-token")
	}
}

// TestRejectEscalation_FallsBackToStaticTokenWithoutBearer mirrors
// TestApproveEscalation_FallsBackToStaticTokenWithoutBearer for reject.
func TestRejectEscalation_FallsBackToStaticTokenWithoutBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")

	if err := c.RejectEscalation(context.Background(), "gd-esc", RejectEscalationRequest{RejectedBy: "cli-actor"}); err != nil {
		t.Fatalf("RejectEscalation: %v", err)
	}
	if gotAuth != "Bearer read-only-token" {
		t.Fatalf("expected the static token as fallback; Authorization = %q, want %q", gotAuth, "Bearer read-only-token")
	}
}

func TestListEscalationsHeldBy_Query(t *testing.T) {
	var gotPath string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query()
		_, _ = w.Write([]byte(`{"escalations":[{"escalation":{"held_by":"operator"},"issue":{"id":"gd-e1","type":"escalation","status":"open","fields":{"held_by":"operator"}}}]}`))
	}))
	defer srv.Close()
	c := newTestClientWithToken(t, srv, "tok")
	got, err := c.ListEscalationsHeldBy(context.Background(), []string{"operator", "coordinator:laptop"}, []string{"open"}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/escalations" || gotQuery.Get("held_by") != "operator,coordinator:laptop" || gotQuery.Get("status") != "open" || gotQuery.Get("limit") != "20" {
		t.Fatalf("request = %s ?%s", gotPath, gotQuery.Encode())
	}
	if len(got) != 1 || got[0].ID != "gd-e1" || got[0].Fields["held_by"] != "operator" {
		t.Fatalf("decoded = %+v", got)
	}
	if _, err := c.ListEscalationsHeldBy(context.Background(), nil, nil, 0); err == nil {
		t.Fatal("empty heldBy must be an error")
	}
}

func TestListBeadsFiltered_HeldByDelegates(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"escalations":[]}`))
	}))
	defer srv.Close()
	c := newTestClientWithToken(t, srv, "tok")
	if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{Types: []string{"escalation"}, HeldBy: []string{"@me"}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/escalations" {
		t.Fatalf("HeldBy query went to %s, want /v1/escalations", gotPath)
	}
	if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{Types: []string{"task"}, HeldBy: []string{"operator"}}); err == nil {
		t.Fatal("HeldBy on a non-escalation query must be an error")
	}
}

func TestClaimAndHandUpEscalation_Wire(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		_, _ = w.Write([]byte(`{"id":"gd-e1","type":"escalation","status":"open","fields":{"held_by":"operator","claimed_by":"gd-a"}}`))
	}))
	defer srv.Close()
	c := newTestClientWithToken(t, srv, "tok")
	b, err := c.ClaimEscalation(context.Background(), "gd-e1")
	if err != nil || gotPath != "/v1/escalations/gd-e1/claim" || b.Fields["claimed_by"] != "gd-a" {
		t.Fatalf("claim: %v %s %+v", err, gotPath, b)
	}
	if _, err := c.HandUpEscalation(context.Background(), "gd-e1", HandUpEscalationRequest{Reason: "needs owner"}); err != nil || gotPath != "/v1/escalations/gd-e1/hand-up" || !strings.Contains(gotBody, `"reason":"needs owner"`) {
		t.Fatalf("hand-up: %v %s %s", err, gotPath, gotBody)
	}
	if strings.Contains(gotBody, "operator_class") {
		t.Fatalf("hand-up without a class sent one: %s", gotBody)
	}
	if _, err := c.HandUpEscalation(context.Background(), "gd-e1", HandUpEscalationRequest{Reason: "case 2", OperatorClass: "2"}); err != nil || !strings.Contains(gotBody, `"operator_class":"2"`) {
		t.Fatalf("operator-class hand-up: %v %s", err, gotBody)
	}
}

func TestCanApproveEscalation_UsesUserBearerAndDecodes(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotPath = r.Header.Get("Authorization"), r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"allowed":false,"method":"","as":"","reason":"caller is not in the approver lane that holds this escalation (coordinator:merge-desk)"}`))
	}))
	defer srv.Close()
	c := newTestClientWithToken(t, srv, "static-tok")

	ctx := WithUserBearer(context.Background(), "user-jwt")
	got, err := c.CanApproveEscalation(ctx, "gd-e1")
	if err != nil {
		t.Fatalf("CanApproveEscalation: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/escalations/gd-e1/can-approve" || gotAuth != "Bearer user-jwt" {
		t.Fatalf("wire = %s %s auth=%q, want POST /v1/escalations/gd-e1/can-approve with the user bearer", gotMethod, gotPath, gotAuth)
	}
	if got.Allowed || got.Reason == "" {
		t.Fatalf("decoded = %+v", got)
	}
}
