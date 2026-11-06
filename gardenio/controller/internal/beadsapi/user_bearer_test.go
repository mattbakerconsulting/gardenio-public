package beadsapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClientWithToken builds a client pointed at srv with a static read
// token, mirroring the production viewer (daemon read token + per-request user
// bearer for writes).
func newTestClientWithToken(t *testing.T, srv *httptest.Server, staticToken string) *Client {
	t.Helper()
	c, err := New(Config{HTTPAddr: srv.URL, Token: staticToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestWriteBearer_FailsClosedWithoutUserBearer is the core security guarantee
// for ka-0000006379: with write-bearer enforcement enabled, a mutating daemon
// call that carries NO per-request user bearer must fail closed — the client
// returns ErrUserBearerRequired and the request never reaches the daemon. This
// is what makes human attribution structural: the viewer's client never sends
// a bearer-less write as the shared "gardenio" identity (a read-only daemon
// token is only a partial second line, see ErrUserBearerRequired).
func TestWriteBearer_FailsClosedWithoutUserBearer(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")
	c.SetRequireUserBearerForWrites(true)

	// CreateBead is a POST mutation. No WithUserBearer on the context.
	_, err := c.CreateBead(context.Background(), CreateBeadRequest{Title: "x", Type: "task"})
	if !errors.Is(err, ErrUserBearerRequired) {
		t.Fatalf("expected ErrUserBearerRequired, got %v", err)
	}
	if hit {
		t.Fatal("daemon was contacted — write must fail closed BEFORE the request is sent")
	}
}

// TestWriteBearer_UsesUserBearerOnWire verifies that when a user bearer IS
// attached, the write authenticates AS THE USER: the user bearer overrides the
// static read token on the Authorization header so the daemon's OIDC RBAC +
// attribution path runs.
func TestWriteBearer_UsesUserBearerOnWire(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"gd-new"}`))
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")
	c.SetRequireUserBearerForWrites(true)

	ctx := WithUserBearer(context.Background(), "user-jwt-abc")
	id, err := c.CreateBead(ctx, CreateBeadRequest{Title: "x", Type: "task"})
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if id != "gd-new" {
		t.Fatalf("expected id gd-new, got %q", id)
	}
	if gotAuth != "Bearer user-jwt-abc" {
		t.Fatalf("write must authenticate as the user; Authorization = %q, want %q", gotAuth, "Bearer user-jwt-abc")
	}
}

// TestWriteBearer_ReadsUseStaticTokenAndNeedNoBearer verifies the read path is
// unaffected: a GET still uses the static read token and does NOT require a
// user bearer even when enforcement is on.
func TestWriteBearer_ReadsUseStaticTokenAndNeedNoBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"gd-1","title":"t","type":"task"}`))
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")
	c.SetRequireUserBearerForWrites(true)

	// GetBead is a GET read — no user bearer attached.
	if _, err := c.GetBead(context.Background(), "gd-1"); err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if gotAuth != "Bearer read-only-token" {
		t.Fatalf("read must use the static token; Authorization = %q, want %q", gotAuth, "Bearer read-only-token")
	}
}

// TestWriteBearer_DisabledByDefault verifies the no-flag path is unchanged: a
// write with no user bearer still goes out on the static token (today's
// behavior), so the change is inert until a deployment opts in.
func TestWriteBearer_DisabledByDefault(t *testing.T) {
	var gotAuth string
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"gd-new"}`))
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")
	// Enforcement NOT enabled.

	if _, err := c.CreateBead(context.Background(), CreateBeadRequest{Title: "x", Type: "task"}); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if !hit {
		t.Fatal("write should have been sent when enforcement is disabled")
	}
	if gotAuth != "Bearer read-only-token" {
		t.Fatalf("disabled path must use the static token; Authorization = %q", gotAuth)
	}
}

// TestCreateTaint_FailsClosedWithoutUserBearer verifies CreateTaint refuses
// to send the request at all when ctx carries no per-request user bearer —
// regardless of SetRequireUserBearerForWrites, since taint creation always
// authenticates as the human, never via the static token + X-Operated-By
// relay (kbeads' callerMayCreateTaint requires direct OIDC).
func TestCreateTaint_FailsClosedWithoutUserBearer(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")
	// Enforcement left at its default (off) — must still fail closed.

	_, err := c.CreateTaint(context.Background(), CreateTaintRequest{Name: "pii", OwnerTeam: "data-platform"})
	if !errors.Is(err, ErrTaintCreateRequiresUserBearer) {
		t.Fatalf("expected ErrTaintCreateRequiresUserBearer, got %v", err)
	}
	if hit {
		t.Fatal("daemon was contacted — CreateTaint must fail closed BEFORE the request is sent")
	}
}

// TestCreateTaint_UsesUserBearerOnWire verifies CreateTaint forces the
// per-request user bearer onto the wire, overriding the static read token,
// even with SetRequireUserBearerForWrites left at its default off — proving
// doJSONAsUser bypasses applyUserBearerWriteGate entirely rather than
// depending on the global flag.
func TestCreateTaint_UsesUserBearerOnWire(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1,"name":"pii","owner_team":"data-platform","created_at":"2026-09-23T00:00:00Z"}`))
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")
	// Enforcement NOT enabled — CreateTaint must force the bearer regardless.

	ctx := WithUserBearer(context.Background(), "admin-jwt-xyz")
	got, err := c.CreateTaint(ctx, CreateTaintRequest{Name: "pii", OwnerTeam: "data-platform", Preset: "p1-taint-crud-api"})
	if err != nil {
		t.Fatalf("CreateTaint: %v", err)
	}
	if got.ID != 1 || got.Name != "pii" || got.OwnerTeam != "data-platform" {
		t.Fatalf("unexpected result: %+v", got)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/taints" {
		t.Fatalf("expected POST /v1/taints, got %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer admin-jwt-xyz" {
		t.Fatalf("CreateTaint must authenticate as the user; Authorization = %q, want %q", gotAuth, "Bearer admin-jwt-xyz")
	}
	if !strings.Contains(string(gotBody), `"name":"pii"`) {
		t.Fatalf("expected request body to carry the taint fields, got %s", gotBody)
	}
}

// TestWriteBearer_ReadOnlyPostExempt verifies that a known read-only POST
// endpoint (semantic search) is NOT gated as a write even with enforcement on
// and no user bearer attached.
func TestWriteBearer_ReadOnlyPostExempt(t *testing.T) {
	if !isReadOnlyPath("/v1/search/semantic") {
		t.Fatal("semantic search should be classified read-only")
	}
	if isReadOnlyPath("/v1/beads") {
		t.Fatal("/v1/beads must NOT be classified read-only")
	}
	// Gate must be a no-op for the read-only POST with no bearer.
	c := &Client{requireUserBearerForWrites: true}
	if _, err := c.applyUserBearerWriteGate(context.Background(), http.MethodPost, "/v1/search/semantic"); err != nil {
		t.Fatalf("read-only POST should be exempt, got %v", err)
	}
	// And it must fail closed for a genuine write POST with no bearer.
	if _, err := c.applyUserBearerWriteGate(context.Background(), http.MethodPost, "/v1/beads"); !errors.Is(err, ErrUserBearerRequired) {
		t.Fatalf("write POST should fail closed, got %v", err)
	}
}

// TestUpdateTaintPolicy_FailsClosedWithoutUserBearer mirrors
// TestCreateTaint_FailsClosedWithoutUserBearer for the Policy tab's save
// action (operator review 2026-09-23 on PR #149, Fix #1): the daemon's
// callerMayManageTaintPolicy requires direct OIDC and never honors the
// static-token + X-Operated-By relay, so the client must refuse to send the
// request at all without a per-request user bearer.
func TestUpdateTaintPolicy_FailsClosedWithoutUserBearer(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")

	_, err := c.UpdateTaintPolicy(context.Background(), "pii", []byte(`{"default":"human"}`))
	if !errors.Is(err, ErrTaintPolicyUpdateRequiresUserBearer) {
		t.Fatalf("expected ErrTaintPolicyUpdateRequiresUserBearer, got %v", err)
	}
	if hit {
		t.Fatal("daemon was contacted — UpdateTaintPolicy must fail closed BEFORE the request is sent")
	}
}

// TestUpdateTaintPolicy_UsesUserBearerOnWire mirrors
// TestCreateTaint_UsesUserBearerOnWire: the per-request user bearer overrides
// the static read token on the wire, regardless of
// SetRequireUserBearerForWrites.
func TestUpdateTaintPolicy_UsesUserBearerOnWire(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":1,"name":"pii","owner_team":"data-platform","updated_at":"2026-09-23T00:00:00Z"}`))
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")

	ctx := WithUserBearer(context.Background(), "owner-jwt-xyz")
	_, err := c.UpdateTaintPolicy(ctx, "pii", []byte(`{"default":"human"}`))
	if err != nil {
		t.Fatalf("UpdateTaintPolicy: %v", err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/v1/taints/pii" {
		t.Fatalf("expected PATCH /v1/taints/pii, got %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer owner-jwt-xyz" {
		t.Fatalf("UpdateTaintPolicy must authenticate as the user; Authorization = %q, want %q", gotAuth, "Bearer owner-jwt-xyz")
	}
}

// TestListTaintScrubRequests_ForwardsUserBearerWhenPresent pins Fix #3
// (operator review 2026-09-23 on PR #149): the daemon's scrubApprover check
// requires the CALLER'S OWN auth to resolve as OIDC to compute real
// CanApprove/CanReject values, so this call must forward the per-request
// user bearer on the wire when one is attached, instead of always relaying
// via the static token.
func TestListTaintScrubRequests_ForwardsUserBearerWhenPresent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"scrub_requests":[],"total":0}`))
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")

	ctx := WithUserBearer(context.Background(), "alice-jwt-abc")
	if _, _, err := c.ListTaintScrubRequests(ctx, "pii", 0, 0); err != nil {
		t.Fatalf("ListTaintScrubRequests: %v", err)
	}
	if gotAuth != "Bearer alice-jwt-abc" {
		t.Fatalf("expected the caller's own bearer on the wire; Authorization = %q, want %q", gotAuth, "Bearer alice-jwt-abc")
	}
}

// TestListTaintScrubRequests_FallsBackToStaticTokenWithoutBearer verifies
// the list still succeeds via the ordinary relay when ctx carries no user
// bearer — a caller with no forwardable identity still sees the Scrubs tab,
// just with every row's CanApprove/CanReject correctly false rather than the
// whole request failing.
func TestListTaintScrubRequests_FallsBackToStaticTokenWithoutBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"scrub_requests":[],"total":0}`))
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "read-only-token")

	if _, _, err := c.ListTaintScrubRequests(context.Background(), "pii", 0, 0); err != nil {
		t.Fatalf("ListTaintScrubRequests: %v", err)
	}
	if gotAuth != "Bearer read-only-token" {
		t.Fatalf("expected the static token as fallback; Authorization = %q, want %q", gotAuth, "Bearer read-only-token")
	}
}

// WithUserBearerOnly puts the person's bearer on the wire for reads and writes
// on a client with no bearer modes on (the slack-bridge), and an empty one
// fails closed instead of going out on the client's own token (gd-Fhizmbk9Ee).
func TestUserBearerOnly_ForcesBearerAndFailsClosed(t *testing.T) {
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gd-x"}`))
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "fleet-token")
	ctx := WithUserBearerOnly(context.Background(), "user-jwt")
	if err := c.ResolveDecision(ctx, "gd-d", ResolveDecisionRequest{SelectedOption: "a"}); err != nil {
		t.Fatalf("ResolveDecision: %v", err)
	}
	if _, err := c.GetDecision(ctx, "gd-d"); err != nil {
		t.Fatalf("GetDecision: %v", err)
	}
	for i, a := range gotAuth {
		if a != "Bearer user-jwt" {
			t.Fatalf("call %d Authorization = %q, want the user's bearer", i, a)
		}
	}

	gotAuth = nil
	err := c.CancelDecision(WithUserBearerOnly(context.Background(), ""), "gd-d", "r", "")
	if !errors.Is(err, ErrUserBearerRequired) {
		t.Fatalf("empty bearer: err = %v, want ErrUserBearerRequired", err)
	}
	if len(gotAuth) != 0 {
		t.Fatalf("empty bearer reached the daemon with %q", gotAuth)
	}
}

// TestWithoutUserBearer_UsesClientCredential: under prefer-user-bearer (the
// slack-bridge), a write on a ctx whose bearer was masked goes out on the
// client's own token, as the bridge's wish-head writes must (gd-3HAfTiByJK).
func TestWithoutUserBearer_UsesClientCredential(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gd-new"}`))
	}))
	defer srv.Close()

	c := newTestClientWithToken(t, srv, "bridge-token")
	c.SetPreferUserBearerForWrites(true)

	ctx := WithUserBearer(context.Background(), "user-jwt-abc")
	if _, err := c.CreateBead(WithoutUserBearer(ctx), CreateBeadRequest{Title: "x", Type: "task"}); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if gotAuth != "Bearer bridge-token" {
		t.Fatalf("masked write Authorization = %q, want the client's own token", gotAuth)
	}
	if _, err := c.CreateBead(ctx, CreateBeadRequest{Title: "x", Type: "task"}); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if gotAuth != "Bearer user-jwt-abc" {
		t.Fatalf("unmasked write Authorization = %q, want the user's bearer", gotAuth)
	}
}
