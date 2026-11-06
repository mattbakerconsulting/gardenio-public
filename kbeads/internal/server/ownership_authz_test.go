package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
)

// Ownership middleware regression tests (ka-0000001016).
// Only bead.CreatedBy, Assignee, Owner, or an admin-role caller may mutate a
// non-config-kind bead. The primary static bearer token also bypasses.
// Fleet tokens, OIDC operators, and anonymous callers must be denied.

// ownedBead returns a minimal non-config-kind bead owned by `owner`.
func ownedBead(id, owner string) *model.Bead {
	return &model.Bead{
		ID:        id,
		Title:     "owned",
		Type:      "task",
		Kind:      model.KindIssue,
		Status:    model.StatusOpen,
		CreatedBy: owner,
	}
}

func fleetCtx(subject string) context.Context {
	ctx := withAuthMethod(context.Background(), authMethodStatic)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: subject})
	return ctx
}

func TestAuthorizeOwnership_CreatorAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("alice@example.com", []string{RoleOperator})

	if d := srv.authorizeOwnership(ctx, "update", ownedBead("gd-o1", "alice@example.com")); d != nil {
		t.Fatalf("creator should be allowed, got: %v", d)
	}
}

func TestAuthorizeOwnership_AssigneeAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("bob@example.com", []string{RoleOperator})

	b := ownedBead("gd-o2", "alice@example.com")
	b.Assignee = "bob@example.com"
	if d := srv.authorizeOwnership(ctx, "close", b); d != nil {
		t.Fatalf("assignee should be allowed, got: %v", d)
	}
}

func TestAuthorizeOwnership_OwnerAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("carol@example.com", []string{RoleOperator})

	b := ownedBead("gd-o3", "alice@example.com")
	b.Owner = "carol@example.com"
	if d := srv.authorizeOwnership(ctx, "delete", b); d != nil {
		t.Fatalf("owner should be allowed, got: %v", d)
	}
}

func TestAuthorizeOwnership_CrossOwnerDenied(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("mallory@example.com", []string{RoleOperator})

	d := srv.authorizeOwnership(ctx, "close", ownedBead("gd-o4", "alice@example.com"))
	if d == nil {
		t.Fatal("non-owner should be denied")
	}
	if !d.Escalatable {
		t.Error("ownership denial should be escalatable")
	}
	if !strings.Contains(d.Message, "ownership denied") {
		t.Errorf("message should mention ownership, got: %s", d.Message)
	}
	if d.AdvisoryText == "" {
		t.Error("denial should include advisory text")
	}
}

func TestAuthorizeOwnership_AdminBypass(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("admin@example.com", []string{RoleAdmin})

	if d := srv.authorizeOwnership(ctx, "delete", ownedBead("gd-o5", "alice@example.com")); d != nil {
		t.Fatalf("admin should bypass ownership, got: %v", d)
	}
}

func TestAuthorizeOwnership_FleetTokenNotBypassed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := fleetCtx("fleet-worker")

	// Fleet identity subject differs from bead owner — must be denied.
	if d := srv.authorizeOwnership(ctx, "update", ownedBead("gd-o7", "alice@example.com")); d == nil {
		t.Fatal("fleet token should NOT bypass ownership")
	}
}

// dmBead returns a minimal type=dm bead created by `creator`, mirroring the
// shape the slack-bridge takes over for DM delivery.
func dmBead(id, creator string) *model.Bead {
	return &model.Bead{
		ID:        id,
		Title:     "dm",
		Type:      "dm",
		Kind:      model.KindData,
		Status:    model.StatusOpen,
		CreatedBy: creator,
	}
}

// TestAuthorizeOwnership_FleetDMCarveout asserts the bridge DM-delivery
// carve-out (ka-0000005087): an ordinary (non-privileged) fleet identity such
// as fleet:slack-bridge may update and close a type=dm bead created by an
// agent — so it can write delivery metadata (dm_channel/dm_ts), close
// fire-and-forget DMs, and capture replies — even though its subject differs
// from the creator.
func TestAuthorizeOwnership_FleetDMCarveout(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := fleetCtx("bridge:slack")

	if d := srv.authorizeOwnership(ctx, "update", dmBead("gd-dm1", "thread-agent")); d != nil {
		t.Fatalf("fleet should update agent-created dm bead, got: %v", d)
	}
	if d := srv.authorizeOwnership(ctx, "close", dmBead("gd-dm2", "thread-agent")); d != nil {
		t.Fatalf("fleet should close agent-created dm bead, got: %v", d)
	}
}

// TestAuthorizeOwnership_FleetDMCarveoutScoped asserts the carve-out is
// narrow: a fleet identity still cannot mutate non-dm beads it does not own.
// This guards against the carve-out accidentally widening into a general
// fleet ownership bypass.
func TestAuthorizeOwnership_FleetDMCarveoutScoped(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := fleetCtx("bridge:slack")

	if d := srv.authorizeOwnership(ctx, "update", ownedBead("gd-dm3", "alice@example.com")); d == nil {
		t.Fatal("fleet DM carve-out must NOT extend to non-dm beads")
	}
}

// privilegedFleetCtx builds a context for a PRIVILEGED service-fleet identity
// (ka-0000001946) whose subject differs from the bead owner.
func privilegedFleetCtx(subject string) context.Context {
	ctx := withAuthMethod(context.Background(), authMethodStatic)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: subject, Privileged: true})
	return ctx
}

// TestAuthorizeOwnership_PrivilegedFleetBypass asserts that a designated
// privileged service-fleet identity (e.g. the gitlab-bridge) bypasses the
// per-bead ownership gate for system-managed graph writes (ka-0000001946),
// while an ORDINARY fleet token with the same subject stays denied.
func TestAuthorizeOwnership_PrivilegedFleetBypass(t *testing.T) {
	srv, _, _ := testCtx(t)

	// Privileged fleet on a cross-owner bead → allowed.
	if d := srv.authorizeOwnership(privilegedFleetCtx("bridge:gitlab"), "update", ownedBead("gd-pf1", "alice@example.com")); d != nil {
		t.Fatalf("privileged fleet token should bypass ownership, got: %v", d)
	}

	// Same subject WITHOUT the privileged flag → denied (ordinary fleet).
	if d := srv.authorizeOwnership(fleetCtx("bridge:gitlab"), "update", ownedBead("gd-pf2", "alice@example.com")); d == nil {
		t.Fatal("ordinary fleet token (Privileged=false) must NOT bypass ownership")
	}
}

// TestAuthorizeOwnership_PrivilegedFleetAddDependency covers the concrete
// failing path from the bug: a privileged fleet performing an update-action
// graph write (AddDependency goes through loadBeadForWrite action=update) on a
// cross-owner bead must not get an ownership 403.
func TestAuthorizeOwnership_PrivilegedFleetAddDependency(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := privilegedFleetCtx("bridge:gitlab")

	if d := srv.authorizeOwnership(ctx, "update", ownedBead("gd-pf3", "carol@example.com")); d != nil {
		t.Fatalf("privileged fleet should perform system-managed dep write, got: %v", d)
	}
}

func TestAuthorizeOwnership_FleetTokenMatchingSubjectAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := fleetCtx("fleet-worker")

	// Fleet's own subject matches the bead's creator.
	if d := srv.authorizeOwnership(ctx, "update", ownedBead("gd-o7b", "fleet-worker")); d != nil {
		t.Fatalf("fleet subject matching creator should be allowed, got: %v", d)
	}
}

// TestAuthorizeOwnership_BridgeLegacyCreatedBy is the ka-0000007614 regression:
// the OUTAGE shape. The jira-bridge authenticates with the per-identity token
// subject "bridge:jira" (bridgetokenrotator.deriveSubject), but every task bead
// it ingested before per-identity subjects existed carries created_by =
// "jira-bridge" (the bare fleet name, see jira_poller.go / jira_config.go).
// Those two strings name the SAME bridge, so authorizeOwnership must let the
// bridge update the very beads it created — otherwise jira→bead status/label
// write-back is silently denied on the historical population.
func TestAuthorizeOwnership_BridgeLegacyCreatedBy(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := fleetCtx("bridge:jira")

	// The exact evidence shape: a type=task issue bead created_by "jira-bridge".
	if d := srv.authorizeOwnership(ctx, "update", ownedBead("gd-jb1", "jira-bridge")); d != nil {
		t.Fatalf("bridge:jira should update its own legacy jira-bridge bead, got: %v", d)
	}
	if d := srv.authorizeOwnership(ctx, "close", ownedBead("gd-jb2", "jira-bridge")); d != nil {
		t.Fatalf("bridge:jira should close its own legacy jira-bridge bead, got: %v", d)
	}
}

// TestAuthorizeOwnership_BridgeLegacyCreatedByScoped pins the carve-out narrow:
// the bridge-self match admits ONLY beads whose created_by is the bridge's own
// reconstructed legacy name. A different bridge, an agent, or an operator bead
// is still denied, so this can never widen into a general fleet write bypass.
func TestAuthorizeOwnership_BridgeLegacyCreatedByScoped(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := fleetCtx("bridge:jira")

	// A DIFFERENT bridge's bead — bridge:jira must not touch it.
	if d := srv.authorizeOwnership(ctx, "update", ownedBead("gd-jb3", "gitlab-bridge")); d == nil {
		t.Fatal("bridge:jira must NOT update a gitlab-bridge-created bead")
	}
	// An ordinary agent's work bead — must stay denied.
	if d := srv.authorizeOwnership(ctx, "update", ownedBead("gd-jb4", "some-agent")); d == nil {
		t.Fatal("bridge:jira must NOT update an agent-created bead")
	}
	// An operator's bead — must stay denied.
	if d := srv.authorizeOwnership(ctx, "update", ownedBead("gd-jb5", "alice@example.com")); d == nil {
		t.Fatal("bridge:jira must NOT update an operator-created bead")
	}
}

// TestAuthorizeOwnership_NonBridgeFleetNoLegacyMatch guards that the self-match
// only fires for the "bridge:<name>" subject shape — a plain fleet worker whose
// created_by happens to end in "-bridge" gets no special treatment.
func TestAuthorizeOwnership_NonBridgeFleetNoLegacyMatch(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := fleetCtx("fleet-worker")

	if d := srv.authorizeOwnership(ctx, "update", ownedBead("gd-jb6", "fleet-worker-bridge")); d == nil {
		t.Fatal("a non-bridge fleet subject must not match a *-bridge created_by")
	}
}

func TestBridgeSubjectMatchesLegacyCreatedBy(t *testing.T) {
	cases := []struct {
		subject   string
		createdBy string
		want      bool
	}{
		{"bridge:jira", "jira-bridge", true},
		{"bridge:slack", "slack-bridge", true},
		{"bridge:gitlab", "gitlab-bridge", true},
		{"bridge:jira", "gitlab-bridge", false}, // different bridge
		{"bridge:jira", "jira-bridge-extra", false},
		{"bridge:jira", "", false},
		{"bridge:", "-bridge", false},           // empty name
		{"jira-bridge", "jira-bridge", false},   // not a bridge: subject
		{"fleet-worker", "fleet-worker", false}, // ordinary fleet
		{"", "jira-bridge", false},
	}
	for _, c := range cases {
		if got := bridgeSubjectMatchesLegacyCreatedBy(c.subject, c.createdBy); got != c.want {
			t.Errorf("bridgeSubjectMatchesLegacyCreatedBy(%q, %q) = %v, want %v",
				c.subject, c.createdBy, got, c.want)
		}
	}
}

func TestAuthorizeOwnership_ConfigKindSkipped(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("mallory@example.com", []string{RoleOperator})

	b := ownedBead("gd-o8", "alice@example.com")
	b.Kind = model.KindConfig
	if d := srv.authorizeOwnership(ctx, "update", b); d != nil {
		t.Fatalf("config-kind must be skipped (governed by WritePolicy), got: %v", d)
	}
}

func TestAuthorizeOwnership_LegacyBeadSkipped(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("mallory@example.com", []string{RoleOperator})

	b := &model.Bead{
		ID:     "gd-o9",
		Title:  "legacy",
		Type:   "task",
		Kind:   model.KindIssue,
		Status: model.StatusOpen,
		// No CreatedBy, Assignee, or Owner.
	}
	if d := srv.authorizeOwnership(ctx, "update", b); d != nil {
		t.Fatalf("legacy bead with no ownership fields should be skipped, got: %v", d)
	}
}

func TestAuthorizeOwnership_AnonymousSkipped(t *testing.T) {
	srv, _, _ := testCtx(t)
	// No identity on context.
	ctx := withAuthMethod(context.Background(), authMethodOIDC)

	if d := srv.authorizeOwnership(ctx, "update", ownedBead("gd-o10", "alice@example.com")); d != nil {
		t.Fatalf("anonymous ctx should defer to outer middleware, got: %v", d)
	}
}

// --- gRPC handler coverage ---

func TestGRPCCloseBead_OwnershipDenied(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-c1"] = ownedBead("gd-c1", "alice@example.com")

	ctx := oidcCtx("mallory@example.com", []string{RoleOperator})
	_, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-c1"})
	requireCode(t, err, codes.PermissionDenied)

	// Ensure no close event was emitted.
	for _, e := range ms.events {
		if e.Topic == "beads.bead.closed" {
			t.Errorf("denied close must not emit bead.closed event")
		}
	}
}

func TestGRPCCloseBead_CreatorAllowed(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-c2"] = ownedBead("gd-c2", "alice@example.com")

	ctx := oidcCtx("alice@example.com", []string{RoleOperator})
	_, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-c2"})
	if err != nil {
		t.Fatalf("creator should be able to close own bead, got: %v", err)
	}
}

// TestGRPCCloseBead_EscalationDiverted verifies the resolve-surface guard
// (ka-0000005516) applies to the gRPC close path too: a generic CloseBead on an
// OPEN escalation is refused with FailedPrecondition (not silently plain-closed)
// and the escalation stays open.
func TestGRPCCloseBead_EscalationDiverted(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-gesc1"] = escBead("gd-gesc1", "some-other-agent")

	ctx := oidcCtx("teammate@example.com", []string{RoleOperator})
	_, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-gesc1"})
	requireCode(t, err, codes.FailedPrecondition)

	if got := ms.beads["gd-gesc1"].Status; got != model.StatusOpen {
		t.Errorf("escalation must stay open after a diverted gRPC close, got status=%s", got)
	}
	for _, e := range ms.events {
		if e.Topic == "beads.bead.closed" {
			t.Errorf("diverted close must not emit bead.closed event")
		}
	}
}

func TestGRPCDeleteBead_OwnershipDenied(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-d1"] = ownedBead("gd-d1", "alice@example.com")

	ctx := oidcCtx("mallory@example.com", []string{RoleOperator})
	_, err := srv.DeleteBead(ctx, &beadsv1.DeleteBeadRequest{Id: "gd-d1"})
	requireCode(t, err, codes.PermissionDenied)

	// Bead must still exist.
	if _, ok := ms.beads["gd-d1"]; !ok {
		t.Error("denied delete must not remove bead")
	}
}

func TestGRPCDeleteBead_AdminAllowed(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-d2"] = ownedBead("gd-d2", "alice@example.com")

	ctx := oidcCtx("admin@example.com", []string{RoleAdmin})
	_, err := srv.DeleteBead(ctx, &beadsv1.DeleteBeadRequest{Id: "gd-d2"})
	if err != nil {
		t.Fatalf("admin should be able to delete, got: %v", err)
	}
}

func TestGRPCUpdateBead_OwnershipDenied(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-u1"] = ownedBead("gd-u1", "alice@example.com")

	ctx := oidcCtx("mallory@example.com", []string{RoleOperator})
	title := "hijacked"
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-u1", Title: &title})
	requireCode(t, err, codes.PermissionDenied)
}

// --- HTTP handler coverage ---

// ownershipBareMux wires DELETE/close/hook-emit routes without AuthMiddleware
// so tests can inject OIDC identity directly.
func ownershipBareMux(s *BeadsServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/beads/{id}", s.handleDeleteBead)
	mux.HandleFunc("POST /v1/beads/{id}/close", s.handleCloseBead)
	mux.HandleFunc("POST /v1/hooks/emit", s.handleHookEmit)
	return mux
}

func TestHTTPDeleteBead_OwnershipDenied(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := ownershipBareMux(s)
	ms.beads["gd-hd1"] = ownedBead("gd-hd1", "alice@example.com")

	req := oidcCtxReq("DELETE", "/v1/beads/gd-hd1", nil,
		"mallory@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if _, ok := ms.beads["gd-hd1"]; !ok {
		t.Error("denied delete must not remove bead")
	}
}

func TestHTTPDeleteBead_OwnershipEscalation(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := ownershipBareMux(s)
	ms.beads["gd-hd2"] = ownedBead("gd-hd2", "alice@example.com")

	req := oidcCtxReq("DELETE", "/v1/beads/gd-hd2", nil,
		"mallory@example.com", []string{RoleOperator})
	req.Header.Set("X-Escalate", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 escalation, got %d; body: %s", rec.Code, rec.Body.String())
	}
	// Escalation bead was created.
	found := false
	for _, b := range ms.beads {
		if b.Type == "escalation" {
			found = true
			break
		}
	}
	if !found {
		t.Error("escalation bead must be created on X-Escalate: true")
	}
	// Target bead still exists.
	if _, ok := ms.beads["gd-hd2"]; !ok {
		t.Error("bead must remain during escalation")
	}
}

func TestHTTPCloseBead_OwnershipDenied(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := ownershipBareMux(s)
	ms.beads["gd-hc1"] = ownedBead("gd-hc1", "alice@example.com")

	req := oidcCtxReq("POST", "/v1/beads/gd-hc1/close", map[string]any{}, "mallory@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// A generic close of an OPEN escalation over the HTTP close route is DIVERTED
// (ka-0000005516): even though the shared-queue carve-out (ka-0000002032) no
// longer ownership-denies a cross-operator teammate, the generic close path
// skips operation_spec replay and wrongly releases close-on-escalation wrappers,
// so it must redirect to the dedicated approve/reject/result verbs with 409.
// The underlying ownership relaxation is still covered by
// TestAuthorizeOwnership_EscalationCrossOperatorClose/Update.
func TestHTTPCloseBead_EscalationGenericCloseDiverted(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := ownershipBareMux(s)
	ms.beads["gd-hesc1"] = escBead("gd-hesc1", "some-other-agent")

	req := oidcCtxReq("POST", "/v1/beads/gd-hesc1/close", map[string]any{}, "teammate@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 diverting a generic escalation close, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "escalation approve|reject|close") {
		t.Errorf("body should point at the resolve verbs; got: %s", rec.Body.String())
	}
	// The bead must stay OPEN — the divert must not close it.
	if got := ms.beads["gd-hesc1"].Status; got != model.StatusOpen {
		t.Errorf("escalation must remain open after a diverted close, got status=%s", got)
	}
}

func TestHTTPHookEmit_OwnershipDenied(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := ownershipBareMux(s)

	// Agent beads carry the agent identity in CreatedBy.
	ms.beads["gd-ag1"] = &model.Bead{
		ID:        "gd-ag1",
		Title:     "agent",
		Type:      "agent",
		Kind:      model.KindConfig, // agent beads are config-kind...
		Status:    model.StatusOpen,
		CreatedBy: "alice@example.com",
	}
	// ...but for this test we want to exercise the ownership gate, so flip
	// to issue-kind. (In practice config-kind is gated by WritePolicy; the
	// hook-emit path still calls authorizeWriteForBead which covers both.)
	ms.beads["gd-ag1"].Kind = model.KindIssue

	body := map[string]any{
		"agent_bead_id": "gd-ag1",
		"hook_type":     "Stop",
		"actor":         "mallory@example.com",
	}
	req := oidcCtxReq("POST", "/v1/hooks/emit", body, "mallory@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPHookEmit_MatchingCreatorAllowed(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := ownershipBareMux(s)

	ms.beads["gd-ag2"] = &model.Bead{
		ID:        "gd-ag2",
		Title:     "agent",
		Type:      "agent",
		Kind:      model.KindIssue,
		Status:    model.StatusOpen,
		CreatedBy: "alice@example.com",
	}

	body := map[string]any{
		"agent_bead_id": "gd-ag2",
		"hook_type":     "PreToolUse",
		"actor":         "alice@example.com",
	}
	req := oidcCtxReq("POST", "/v1/hooks/emit", body, "alice@example.com", []string{RoleOperator})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// --- Config-kind ownership skip (integration-ish) ---

func TestGRPCCloseBead_ConfigKindSkipsOwnership(t *testing.T) {
	srv, ms, _ := newTestServer()
	// Config-kind beads use WritePolicy; ownership must not block here.
	ms.beads["gd-cfg1"] = &model.Bead{
		ID:        "gd-cfg1",
		Title:     "task-like config",
		Type:      "task", // not admin-gated
		Kind:      model.KindConfig,
		Status:    model.StatusOpen,
		CreatedBy: "alice@example.com",
	}

	// OIDC operator, not the creator, but ownership should be bypassed for
	// config-kind. task type has no write_policy so the call should succeed.
	ctx := oidcCtx("mallory@example.com", []string{RoleOperator})
	_, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-cfg1"})
	if err != nil {
		t.Fatalf("config-kind close should skip ownership, got: %v", err)
	}
}

// Decision-type beads: an edge-auth-stamped identity that matches
// bead.OperatedBy can mutate the decision even though it is not the
// creator/assignee/owner. This is the slack-bridge close-from-Slack
// path (ka-0000007637).
func TestAuthorizeOwnership_DecisionOperatedByAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := withAuthMethod(context.Background(), authMethodEdge)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "edge", Subject: "slack:U0OPERATOR1"})

	b := &model.Bead{
		ID:         "gd-dec1",
		Title:      "decide",
		Type:       model.TypeDecision,
		Kind:       model.KindIssue,
		Status:     model.StatusOpen,
		CreatedBy:  "agent-bot",
		OperatedBy: "slack:U0OPERATOR1",
	}
	if d := srv.authorizeOwnership(ctx, "close", b); d != nil {
		t.Fatalf("edge-auth operator matching bead.OperatedBy should be allowed on a decision, got: %v", d)
	}
}

// The operator bypass now applies to ALL bead types, not just decisions
// (ka-0000001692): the originating human operator may mutate beads their
// agents filed for them. Byte-equal operated_by on a non-decision bead is
// allowed, matching the decision path.
func TestAuthorizeOwnership_OperatedByAllowedForNonDecision(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := withAuthMethod(context.Background(), authMethodEdge)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "edge", Subject: "slack:U0OPERATOR1"})

	b := ownedBead("gd-t1", "agent-bot")
	b.OperatedBy = "slack:U0OPERATOR1"
	if d := srv.authorizeOwnership(ctx, "close", b); d != nil {
		t.Fatalf("operator matching operated_by should be allowed on any type, got: %v", d)
	}
}

// The bug scenario (ka-0000001692): an OIDC caller (Subject = bare email)
// closing an agent-filed bead whose slack:U… operated_by differs literally
// from the caller. The bypass must reconcile both sides through the operators
// alias table — when oidc:<email> and slack:U… resolve to the SAME
// operator_id, the operator is allowed. Uses a task bead so it exercises the
// operator-alias path rather than the escalation shared-queue carve-out
// (ka-0000002032), which would admit the operator regardless.
func TestAuthorizeOwnership_OperatorBypassCanonicalMatch(t *testing.T) {
	srv, ms, _ := testCtx(t)
	if ms.operatorAliases == nil {
		ms.operatorAliases = map[string]int64{}
	}
	ms.operatorAliases["oidc\x00matt@acme.example"] = 42
	ms.operatorAliases["slack\x00U0OPERATOR1"] = 42 // linked: same human

	ctx := oidcCtx("matt@acme.example", []string{RoleOperator})
	b := &model.Bead{
		ID:         "gd-task1",
		Title:      "agent-filed task",
		Type:       "task",
		Kind:       model.KindIssue,
		Status:     model.StatusOpen,
		CreatedBy:  "firehose-gardener-agent",
		OperatedBy: "slack:U0OPERATOR1",
	}
	if d := srv.authorizeOwnership(ctx, "close", b); d != nil {
		t.Fatalf("operator whose linked identity matches operated_by should be allowed, got: %v", d)
	}
}

// Fail-closed: when the caller's identity is NOT linked to the bead's
// operator (the alias resolves to a different operator_id), the bypass must
// deny — no cross-operator authority.
func TestAuthorizeOwnership_OperatorBypassDeniedDifferentOperator(t *testing.T) {
	srv, ms, _ := testCtx(t)
	if ms.operatorAliases == nil {
		ms.operatorAliases = map[string]int64{}
	}
	ms.operatorAliases["oidc\x00mallory@acme.example"] = 7
	ms.operatorAliases["slack\x00U0OPERATOR1"] = 42 // different operator

	// A non-escalation agent-created bead (escalations now bypass ownership for
	// any operator via the shared-queue carve-out ka-0000002032; the operator-
	// alias fail-closed guarantee still governs every other agent-filed type).
	ctx := oidcCtx("mallory@acme.example", []string{RoleOperator})
	b := &model.Bead{
		ID:         "gd-task2",
		Title:      "agent-filed task",
		Type:       "task",
		Kind:       model.KindIssue,
		Status:     model.StatusOpen,
		CreatedBy:  "firehose-gardener-agent",
		OperatedBy: "slack:U0OPERATOR1",
	}
	if d := srv.authorizeOwnership(ctx, "close", b); d == nil {
		t.Fatal("caller linked to a different operator must be denied")
	}
}

// The production scenario: edge-auth from a GitLab PAT (the cli.gardenio.app
// shim) arrives as Type "edge" with a BARE email Subject — no scheme prefix.
// operatorAliasForIdentity must treat that as an oidc email so it resolves to
// the same operator_id as the bead's slack:U… operated_by. Regression guard
// for the gap where splitOperator(bare-email) yielded an empty kind and the
// bypass silently never fired (the second-pass fix to ka-0000001692).
func TestAuthorizeOwnership_OperatorBypassEdgeBareEmail(t *testing.T) {
	srv, ms, _ := testCtx(t)
	if ms.operatorAliases == nil {
		ms.operatorAliases = map[string]int64{}
	}
	ms.operatorAliases["oidc\x00matt@acme.example"] = 229
	ms.operatorAliases["slack\x00U0OPERATOR1"] = 229

	ctx := withAuthMethod(context.Background(), authMethodEdge)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "edge", Subject: "matt@acme.example"})
	// A task bead so the operator-alias bypass is what's under test — the
	// escalation shared-queue carve-out (ka-0000002032) admits non-OIDC edge
	// callers unconditionally and would mask this path.
	b := &model.Bead{
		ID:         "gd-task-edge",
		Title:      "agent-filed task",
		Type:       "task",
		Kind:       model.KindIssue,
		Status:     model.StatusOpen,
		CreatedBy:  "firehose-gardener-agent",
		OperatedBy: "slack:U0OPERATOR1",
	}
	if d := srv.authorizeOwnership(ctx, "close", b); d != nil {
		t.Fatalf("edge-auth bare-email operator should resolve to oidc and be allowed, got: %v", d)
	}
}

// operatorAliasForIdentity unit coverage across the subject shapes.
func TestOperatorAliasForIdentity(t *testing.T) {
	cases := []struct {
		name              string
		id                AuthIdentity
		wantKind, wantSub string
	}{
		{"oidc bare email", AuthIdentity{Type: "oidc", Subject: "a@b.com"}, "oidc", "a@b.com"},
		{"edge bare email (GitLab PAT)", AuthIdentity{Type: "edge", Subject: "a@b.com"}, "oidc", "a@b.com"},
		{"edge slack-prefixed", AuthIdentity{Type: "edge", Subject: "slack:U1"}, "slack", "U1"},
		{"edge oidc-prefixed", AuthIdentity{Type: "edge", Subject: "oidc:a@b.com"}, "oidc", "a@b.com"},
		{"fleet rejected", AuthIdentity{Type: "fleet", Subject: "gardenio"}, "", ""},
		{"static rejected", AuthIdentity{Type: "static", Subject: "static"}, "", ""},
		{"empty subject", AuthIdentity{Type: "oidc", Subject: ""}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, s := operatorAliasForIdentity(tc.id)
			if k != tc.wantKind || s != tc.wantSub {
				t.Fatalf("got (%q,%q), want (%q,%q)", k, s, tc.wantKind, tc.wantSub)
			}
		})
	}
}

// Fail-closed: when the caller's identity has no operator alias at all (never
// linked / logged in), the bypass cannot resolve them and must deny.
func TestAuthorizeOwnership_OperatorBypassDeniedUnlinked(t *testing.T) {
	srv, ms, _ := testCtx(t)
	if ms.operatorAliases == nil {
		ms.operatorAliases = map[string]int64{}
	}
	ms.operatorAliases["slack\x00U0OPERATOR1"] = 42 // bead operator known…
	// …but no oidc alias for the caller.

	// A non-escalation agent-created bead: escalations now bypass ownership for
	// any operator (shared-queue carve-out ka-0000002032), so the operator-
	// alias fail-closed path is exercised against a task instead.
	ctx := oidcCtx("matt@acme.example", []string{RoleOperator})
	b := &model.Bead{
		ID:         "gd-task3",
		Title:      "agent-filed task",
		Type:       "task",
		Kind:       model.KindIssue,
		Status:     model.StatusOpen,
		CreatedBy:  "firehose-gardener-agent",
		OperatedBy: "slack:U0OPERATOR1",
	}
	if d := srv.authorizeOwnership(ctx, "close", b); d == nil {
		t.Fatal("caller with no linked operator alias must be denied")
	}
}

// escBead returns a minimal type=escalation bead created by `creator` with a
// distinct operated_by, mirroring an escalation an agent filed for one human.
func escBead(id, creator string) *model.Bead {
	return &model.Bead{
		ID:        id,
		Title:     "escalation",
		Type:      model.TypeEscalation,
		Kind:      model.KindData,
		Status:    model.StatusOpen,
		CreatedBy: creator,
	}
}

// Escalation shared-queue carve-out (ka-0000002032): an operator who did NOT
// file the escalation may still close it — escalations are shared operator work
// so the fleet can unblock each other. Mirrors the approve/reject audience.
func TestAuthorizeOwnership_EscalationCrossOperatorClose(t *testing.T) {
	srv, _, _ := testCtx(t)
	// A different operator than the creator, with no operated_by link at all.
	ctx := oidcCtx("teammate@example.com", []string{RoleOperator})

	if d := srv.authorizeOwnership(ctx, "close", escBead("gd-esc-x1", "firehose-gardener-agent")); d != nil {
		t.Fatalf("any operator should be able to close a shared escalation, got: %v", d)
	}
}

// The carve-out also covers "modify" actions (reflag / pingback go through
// update/PATCH), so a teammate can bump another agent's stale escalation.
func TestAuthorizeOwnership_EscalationCrossOperatorUpdate(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("teammate@example.com", []string{RoleOperator})

	if d := srv.authorizeOwnership(ctx, "update", escBead("gd-esc-x2", "some-other-agent")); d != nil {
		t.Fatalf("any operator should be able to modify a shared escalation, got: %v", d)
	}
}

// The carve-out is scoped to the approve/reject audience: an OIDC *viewer*
// (below operator) must NOT get the ownership bypass on the gRPC
// close/update/delete path, which is not itself wrapped in requireRole.
func TestAuthorizeOwnership_EscalationViewerDenied(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("watcher@example.com", []string{RoleViewer})

	if d := srv.authorizeOwnership(ctx, "close", escBead("gd-esc-x3", "some-agent")); d == nil {
		t.Fatal("an OIDC viewer must NOT bypass ownership on a shared escalation")
	}
}

// A non-OIDC caller (fleet agent / static token / edge-auth CLI) passes the
// carve-out, matching how requireRole(RoleOperator) lets non-OIDC callers
// through on the dedicated endpoints. This is the path a teammate's CLI hits.
func TestAuthorizeOwnership_EscalationFleetAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := fleetCtx("teammate-agent")

	if d := srv.authorizeOwnership(ctx, "close", escBead("gd-esc-x4", "some-other-agent")); d != nil {
		t.Fatalf("non-OIDC (fleet) caller should be able to close a shared escalation, got: %v", d)
	}
}

// The carve-out is narrow: it does not widen ownership on non-escalation beads.
// An operator still cannot close an arbitrary task they don't own.
func TestAuthorizeOwnership_EscalationCarveoutScoped(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("teammate@example.com", []string{RoleOperator})

	if d := srv.authorizeOwnership(ctx, "close", ownedBead("gd-esc-x5", "alice@example.com")); d == nil {
		t.Fatal("escalation carve-out must NOT extend to non-escalation beads")
	}
}

// decBead returns a minimal type=decision bead filed by `creator` FOR the
// operator `operatedBy`, mirroring a decision an agent raised for one human.
func decBead(id, creator, operatedBy string) *model.Bead {
	return &model.Bead{
		ID:         id,
		Title:      "decide",
		Type:       model.TypeDecision,
		Kind:       model.KindIssue,
		Status:     model.StatusOpen,
		CreatedBy:  creator,
		OperatedBy: operatedBy,
	}
}

// Decision shared-queue carve-out (ka-0000000798): a decision filed FOR the
// operator must be RESOLVABLE by that operator (and, like escalations, by any
// operator on the shared queue). Before this fix `gb decision respond` closed
// the bead through the generic ownership-gated path and 403'd for the very
// human the decision was addressed to — the person who is none of
// created_by/assignee/owner. An edge-auth operator whose operated_by does NOT
// literally match is now admitted, mirroring the escalation carve-out audience:
// answering the decision is the whole point of the object.
func TestAuthorizeOwnership_DecisionCrossOperatorAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := withAuthMethod(context.Background(), authMethodEdge)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "edge", Subject: "slack:UDIFFERENT"})

	if d := srv.authorizeOwnership(ctx, "close", decBead("gd-dec2", "agent-bot", "slack:U0OPERATOR1")); d != nil {
		t.Fatalf("any operator should be able to resolve a shared decision, got: %v", d)
	}
}

// The carve-out also covers "update" — the obvious self-assign-then-close
// workaround (kd update --assignee) was ALSO blocked by the same guard before
// the fix (ka-0000000798 repro). A teammate operator can now update a shared
// decision that they did not file.
func TestAuthorizeOwnership_DecisionCrossOperatorUpdate(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("teammate@example.com", []string{RoleOperator})

	if d := srv.authorizeOwnership(ctx, "update", decBead("gd-dec2u", "some-other-agent", "slack:U0OPERATOR1")); d != nil {
		t.Fatalf("any operator should be able to update a shared decision, got: %v", d)
	}
}

// The decision carve-out is scoped to the resolve-endpoint audience exactly as
// the escalation one is: an OIDC *viewer* (below operator) must NOT get the
// ownership bypass on the gRPC close/update path, which is not itself wrapped
// in requireRole.
func TestAuthorizeOwnership_DecisionViewerDenied(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("watcher@example.com", []string{RoleViewer})

	if d := srv.authorizeOwnership(ctx, "close", decBead("gd-dec2v", "some-agent", "slack:U0OPERATOR1")); d == nil {
		t.Fatal("an OIDC viewer must NOT bypass ownership on a shared decision")
	}
}

// A plain (non-bridge) fleet token no longer passes the decision carve-out: an
// agent credential must not close a decision it did not file (gd-Du6wHclx4g).
// Bridge, machine, edge and OIDC callers still do; see decision_resolver_authz_test.go.
func TestAuthorizeOwnership_DecisionFleetAgentDenied(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := fleetCtx("teammate-agent")

	if d := srv.authorizeOwnership(ctx, "close", decBead("gd-dec2f", "some-other-agent", "slack:U0OPERATOR1")); d == nil {
		t.Fatal("a plain fleet caller must NOT close a decision it did not file")
	}
}

// Ensure the ownership denial is treated as a real AuthzDenial (not a plain
// error).
func TestAuthorizeOwnership_ReturnsAuthzDenial(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := oidcCtx("mallory@example.com", []string{RoleOperator})

	d := srv.authorizeOwnership(ctx, "update", ownedBead("gd-ad1", "alice@example.com"))
	if d == nil {
		t.Fatal("expected denial")
	}
	// Roundtrip to JSON to make sure it serializes.
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"denied_action":"update"`) {
		t.Errorf("denial JSON missing denied_action: %s", data)
	}
}
