package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"gardenio/kbeads/internal/events"
)

const machineStaticToken = "static-secret"

// machineHarness is a real go-oidc verifier against a fake Keycloak (signed
// JWT fixtures), plus a FleetConfig with an optional machine-client registry.
type machineHarness struct {
	kc       *fakeKeycloak
	verifier TokenVerifier
	fc       *FleetConfig
}

func newMachineHarness(t *testing.T, registry []string, allowedDomains ...string) *machineHarness {
	t.Helper()
	kc := newFakeKeycloak(t)
	v, err := NewOIDCVerifierWithDiscovery(context.Background(), testPublicIssuer, kc.discoveryURL(), testAudience, allowedDomains...)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	fc := NewFleetConfig("default", nil)
	if registry != nil {
		fc.SetMachineClients(registry)
	}
	return &machineHarness{kc: kc, verifier: v, fc: fc}
}

// saToken mints a Keycloak-shaped service-account token: azp=<client>,
// preferred_username=service-account-<client>, no email, roles in both claims.
func (h *machineHarness) saToken(t *testing.T, client string, roles ...string) string {
	t.Helper()
	return h.kc.mint(t, testPublicIssuer, testAudience, map[string]any{
		"azp":                client,
		"preferred_username": "service-account-" + client,
		"realm_roles":        roles,
		"realm_access":       map[string]any{"roles": roles},
	})
}

// humanToken mints an ordinary human token through the given client.
func (h *machineHarness) humanToken(t *testing.T, client, email string, roles ...string) string {
	t.Helper()
	return h.kc.mint(t, testPublicIssuer, testAudience, map[string]any{
		"azp":                client,
		"preferred_username": email,
		"email":              email,
		"email_verified":     true,
		"realm_roles":        roles,
	})
}

// seen is what a downstream handler observes.
type machineSeen struct {
	called   bool
	method   authMethod
	identity AuthIdentity
	roles    []string
	mroles   []string
	fleet    string
	ctx      context.Context
}

func (h *machineHarness) serve(t *testing.T, tok string, hdr map[string]string) (*httptest.ResponseRecorder, *machineSeen) {
	t.Helper()
	seen := &machineSeen{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		seen.called = true
		seen.ctx = ctx
		seen.method = authMethodFromContext(ctx)
		seen.identity = AuthIdentityFromContext(ctx)
		seen.roles = RolesFromContext(ctx)
		seen.mroles = machineRolesFromContext(ctx)
		seen.fleet = FleetFromContext(ctx)
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	AuthMiddleware(withTestToken(h.fc, machineStaticToken), h.verifier, next).ServeHTTP(rec, req)
	return rec, seen
}

// "Identical when off": with no registry a service-account token takes today's
// OIDC path.
func TestMachineIdentity_RegistryEmpty_ServiceAccountTokenIsOIDC(t *testing.T) {
	h := newMachineHarness(t, nil)
	rec, seen := h.serve(t, h.saToken(t, "gardenio-controller", "gardenio-machine-accounts"), nil)
	if rec.Code != http.StatusNoContent || !seen.called {
		t.Fatalf("status=%d called=%v body=%s", rec.Code, seen.called, rec.Body.String())
	}
	if seen.method != authMethodOIDC || seen.identity.Type != "oidc" {
		t.Fatalf("method=%q type=%q; want oidc/oidc", seen.method, seen.identity.Type)
	}
	if seen.mroles != nil {
		t.Fatalf("machine roles must be absent when the registry is empty, got %v", seen.mroles)
	}
}

func TestMachineIdentity_RegisteredServiceAccount(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"})
	// The token carries gardenio-admin too: it must NOT become admin.
	tok := h.saToken(t, "gardenio-controller", "gardenio-machine-accounts", RoleAdmin)
	rec, seen := h.serve(t, tok, nil)
	if rec.Code != http.StatusNoContent || !seen.called {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seen.method != authMethodMachine {
		t.Fatalf("method=%q want machine", seen.method)
	}
	if got, want := seen.identity, (AuthIdentity{Type: "machine", Subject: "machine:gardenio-controller", ClientID: "gardenio-controller"}); got != want {
		t.Fatalf("identity=%+v want %+v", got, want)
	}
	if len(seen.roles) != 0 {
		t.Fatalf("RolesFromContext must stay empty for a machine, got %v", seen.roles)
	}
	if !containsStr(seen.mroles, "gardenio-machine-accounts") || !containsStr(seen.mroles, RoleAdmin) {
		t.Fatalf("machine roles = %v; want the token's roles", seen.mroles)
	}
	if seen.fleet != "default" {
		t.Fatalf("fleet=%q want default", seen.fleet)
	}
}

func TestMachineIdentity_RegisteredClientHumanToken_401(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"})
	rec, seen := h.serve(t, h.humanToken(t, "gardenio-controller", "alice@example.com", RoleAdmin), nil)
	if rec.Code != http.StatusUnauthorized || seen.called {
		t.Fatalf("status=%d called=%v; want 401", rec.Code, seen.called)
	}
	if !strings.Contains(rec.Body.String(), "machine client token is not a service-account token") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

// Fail closed (PR #290 review D1): once azp verifies as a registered machine
// client, a claim that cannot be read must 401, never fall through to the human
// OIDC path where X-Auth-Request-Email would let it act as any email.
func TestMachineIdentity_RegisteredClientMalformedClaims_401(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"})
	victim := map[string]string{"X-Auth-Request-Email": "victim@example.com"}
	cases := map[string]map[string]any{
		// realm mapper with multivalued off: scalar instead of a list.
		"scalar realm_roles": {
			"azp":                "gardenio-controller",
			"preferred_username": "service-account-gardenio-controller",
			"realm_roles":        "gardenio-machine-accounts",
		},
		"scalar realm_access.roles": {
			"azp":                "gardenio-controller",
			"preferred_username": "service-account-gardenio-controller",
			"realm_access":       map[string]any{"roles": "gardenio-admin"},
		},
		"non-string preferred_username": {
			"azp":                "gardenio-controller",
			"preferred_username": 42,
			"realm_roles":        []string{"gardenio-machine-accounts"},
		},
	}
	for name, claims := range cases {
		t.Run(name, func(t *testing.T) {
			tok := h.kc.mint(t, testPublicIssuer, testAudience, claims)
			rec, seen := h.serve(t, tok, victim)
			if rec.Code != http.StatusUnauthorized || seen.called {
				t.Fatalf("status=%d called=%v method=%q subject=%q; want 401",
					rec.Code, seen.called, seen.method, seen.identity.Subject)
			}
		})
	}

	// Positive control: the same shape with well-formed claims is a machine.
	rec, seen := h.serve(t, h.saToken(t, "gardenio-controller", "gardenio-machine-accounts"), victim)
	if rec.Code != http.StatusNoContent || seen.method != authMethodMachine ||
		seen.identity.Subject != "machine:gardenio-controller" {
		t.Fatalf("control: status=%d method=%q subject=%q", rec.Code, seen.method, seen.identity.Subject)
	}
}

// An unregistered client with unreadable claims is not a machine: it keeps
// taking today's OIDC path (never classified as machine).
func TestMachineIdentity_UnregisteredClientMalformedClaims_NotMachine(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"})
	tok := h.kc.mint(t, testPublicIssuer, testAudience, map[string]any{
		"azp":                "some-other-client",
		"preferred_username": 42,
	})
	_, seen := h.serve(t, tok, nil)
	if seen.method == authMethodMachine {
		t.Fatalf("method=%q; an unregistered client must never classify as machine", seen.method)
	}
}

func TestMachineIdentity_UnregisteredServiceAccount_401(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"})
	rec, seen := h.serve(t, h.saToken(t, "gardenio-beads-api", RoleAdmin), nil)
	if rec.Code != http.StatusUnauthorized || seen.called {
		t.Fatalf("status=%d called=%v; want 401", rec.Code, seen.called)
	}
	if !strings.Contains(rec.Body.String(), "unregistered machine client") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

// Positive control: a human token is untouched while the registry is non-empty.
func TestMachineIdentity_HumanTokenWithRegistry_StillOIDC(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"})
	rec, seen := h.serve(t, h.humanToken(t, "gardenio-traefik-forwardauth", "alice@example.com", RoleOperator), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seen.method != authMethodOIDC || seen.identity.Subject != "alice@example.com" {
		t.Fatalf("method=%q identity=%+v", seen.method, seen.identity)
	}
	if !containsStr(seen.roles, RoleOperator) {
		t.Fatalf("roles=%v; want operator intact", seen.roles)
	}
}

// BEADS_OIDC_ALLOWED_EMAIL_DOMAINS must not refuse an email-less machine token,
// and the human domain check (oidcVerifier.Verify) is unchanged.
func TestMachineIdentity_AllowedDomains(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"}, "example.com")
	rec, seen := h.serve(t, h.saToken(t, "gardenio-controller", "gardenio-machine-accounts"), nil)
	if rec.Code != http.StatusNoContent || seen.method != authMethodMachine {
		t.Fatalf("machine token refused under allowedDomains: status=%d method=%q body=%s", rec.Code, seen.method, rec.Body.String())
	}
	bad := h.humanToken(t, "gardenio-traefik-forwardauth", "mallory@evil.test", RoleAdmin)
	if err := h.verifier.Verify(context.Background(), bad); err == nil {
		t.Fatal("human token with a disallowed email domain must still fail Verify")
	}
	good := h.humanToken(t, "gardenio-traefik-forwardauth", "alice@example.com", RoleAdmin)
	if err := h.verifier.Verify(context.Background(), good); err != nil {
		t.Fatalf("human token with an allowed domain: %v", err)
	}
}

// A machine principal's attribution is server-derived: no header can
// re-attribute it.
func TestMachineIdentity_HeadersIgnoredForAttribution(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"})
	srv := NewBeadsServer(newMockStore(), &events.NoopPublisher{})
	tok := h.saToken(t, "gardenio-controller")
	for _, hdr := range []map[string]string{
		{"X-Operated-By": "oidc:someone@example.com"},
		{"X-Actor": "someone"},
		{"X-System-Operator": "scheduler"},
		{"X-Owner-Override": "oidc:someone@example.com"},
		{"X-User-Bead-Id": "gd-user-1"},
		{"X-Fleet": "other"},
	} {
		rec, seen := h.serve(t, tok, hdr)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%v: status=%d", hdr, rec.Code)
		}
		got, src := srv.resolveOperatedByPure(seen.ctx)
		if got != "machine:gardenio-controller" || src != operatedBySourceMachine {
			t.Errorf("%v: operated_by=(%q,%q); want (machine:gardenio-controller,%q)", hdr, got, src, operatedBySourceMachine)
		}
		if seen.fleet != "default" {
			t.Errorf("%v: fleet=%q; X-Fleet must be ignored", hdr, seen.fleet)
		}
	}
}

// End to end through the real mux: a header-borne operator cannot override the
// machine subject on a stored bead.
func TestMachineIdentity_CreateBead_OperatedByIsMachineSubject(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	handler := srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))

	body, _ := json.Marshal(map[string]any{"title": "machine write", "type": "task"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.saToken(t, "gardenio-controller"))
	req.Header.Set("X-Operated-By", "oidc:someone@example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OperatedBy string `json:"operated_by"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.OperatedBy != "machine:gardenio-controller" {
		t.Fatalf("operated_by=%q; body=%s", got.OperatedBy, rec.Body.String())
	}
}

// Under BEADS_AUTHORING_HUMAN_GATE=enforce a machine principal's autonomous
// create is refused like a header-less fleet create; an update is unaffected.
func TestMachineIdentity_AuthoringHumanGate_RefusesAutonomousCreate(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "enforce")
	h := newMachineHarness(t, []string{"gardenio-controller"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	handler := srv.NewHTTPHandler(h.verifier, withTestToken(h.fc, machineStaticToken))
	tok := h.saToken(t, "gardenio-controller")

	do := func(method, path string, payload any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(payload)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// Positive control: a non-autonomous create is fine under enforce.
	if rec := do(http.MethodPost, "/v1/beads", map[string]any{"title": "plain", "type": "task"}); rec.Code != http.StatusCreated {
		t.Fatalf("non-autonomous create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := do(http.MethodPost, "/v1/beads", map[string]any{"title": "sched", "type": "schedule"})
	if rec.Code == http.StatusCreated {
		t.Fatalf("autonomous create by a machine principal must be refused under enforce; body=%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "requires a human operator") {
		t.Fatalf("status=%d body=%s; want the authoring-gate refusal", rec.Code, rec.Body.String())
	}
	// Updates are unaffected by the gate.
	var created struct {
		ID string `json:"id"`
	}
	rec = do(http.MethodPost, "/v1/beads", map[string]any{"title": "to update", "type": "task"})
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if rec := do(http.MethodPatch, "/v1/beads/"+created.ID, map[string]any{"title": "renamed"}); rec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func machineGRPC(t *testing.T, h *machineHarness, tok string) (called bool, err error) {
	t.Helper()
	handler := func(ctx context.Context, _ any) (any, error) {
		called = true
		return "ok", nil
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
	_, err = AuthInterceptor(withTestToken(h.fc, machineStaticToken), h.verifier)(ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, handler)
	return called, err
}

func TestMachineIdentity_GRPC(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-controller"})

	called, err := machineGRPC(t, h, h.saToken(t, "gardenio-controller", "gardenio-machine-accounts"))
	requireCode(t, err, codes.PermissionDenied)
	if called {
		t.Fatal("handler must not run for a machine principal over gRPC")
	}

	called, err = machineGRPC(t, h, h.humanToken(t, "gardenio-traefik-forwardauth", "alice@example.com", RoleOperator))
	if err != nil || !called {
		t.Fatalf("human token over gRPC: called=%v err=%v", called, err)
	}

	_, err = machineGRPC(t, h, h.saToken(t, "gardenio-beads-api", RoleAdmin))
	requireCode(t, err, codes.Unauthenticated)

	// Registry empty: today's behavior, the service-account token is an OIDC caller.
	off := newMachineHarness(t, nil)
	called, err = machineGRPC(t, off, off.saToken(t, "gardenio-controller"))
	if err != nil || !called {
		t.Fatalf("registry empty: called=%v err=%v", called, err)
	}
}
