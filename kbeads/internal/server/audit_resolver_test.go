package server

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestResolveOperator_ColdPath_UpsertsOperatorAndAlias proves the cold path:
// alias miss → UpsertOperator → UpsertOperatorAlias. Subsequent calls hit the
// hot path without touching operators.
func TestResolveOperator_ColdPath_UpsertsOperatorAndAlias(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	id, ok := srv.resolveOperator(context.Background(), "oidc:matt@acme.example")
	if !ok {
		t.Fatalf("resolveOperator returned ok=false on cold path")
	}
	if id == 0 {
		t.Fatalf("resolveOperator returned id=0; want non-zero")
	}

	ms := srv.store.(*mockStore)
	if got, ok := ms.operatorByPair["oidc\x00matt@acme.example"]; !ok || got != id {
		t.Fatalf("operators table missing pair (oidc, matt@acme.example) → %d; got %d ok=%v", id, got, ok)
	}
	if got, ok := ms.operatorAliases["oidc\x00matt@acme.example"]; !ok || got != id {
		t.Fatalf("operator_aliases missing alias → operator_id; got %d ok=%v", got, ok)
	}

	// Hot-path: same input must reuse the same id without writing again.
	gotID, ok := srv.resolveOperator(context.Background(), "oidc:matt@acme.example")
	if !ok || gotID != id {
		t.Fatalf("hot-path resolveOperator returned (%d, %v); want (%d, true)", gotID, ok, id)
	}
}

// TestResolveOperator_DistinctSubjectsGetDistinctIDs guards against the
// mockStore (and prod) accidentally collapsing two unrelated operators onto
// the same id, which would break attribution in the joined view.
func TestResolveOperator_DistinctSubjectsGetDistinctIDs(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	idA, _ := srv.resolveOperator(context.Background(), "slack:U001")
	idB, _ := srv.resolveOperator(context.Background(), "slack:U002")
	if idA == 0 || idB == 0 {
		t.Fatalf("expected non-zero ids; got idA=%d idB=%d", idA, idB)
	}
	if idA == idB {
		t.Fatalf("distinct subjects collapsed onto same id %d", idA)
	}
}

// TestResolveOperator_MalformedInput_NoColon falls through to ok=false so the
// audit row commits with operated_by_id = NULL. The TEXT column still carries
// the raw value, so audit observability isn't lost — but the FK side stays
// empty rather than landing under operators(kind=”) by accident.
func TestResolveOperator_MalformedInput_NoColon(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	if _, ok := srv.resolveOperator(context.Background(), "no-colon-value"); ok {
		t.Fatalf("resolveOperator(\"no-colon-value\") returned ok=true; want false")
	}
}

// TestResolveAuditOperator_Unattributable_LeavesFKNil pins the
// post-R2 (ka-0000005772) invariant: when no identity is attributable
// (anonymous ctx, no OIDC/static auth), resolveAuditOperator returns
// ("", nil, "empty"). Live-state callers route this through
// resolveAuditOperatorRequired which then fails-closed with
// InvalidArgument; history-table callers log+skip the row.
func TestResolveAuditOperator_Unattributable_LeavesFKNil(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	value, idPtr, source := srv.resolveAuditOperator(context.Background())
	if value != "" || idPtr != nil {
		t.Fatalf("unattributable resolveAuditOperator = (%q, %v); want (\"\", nil)", value, idPtr)
	}
	if source != operatedBySourceEmpty {
		t.Fatalf("unattributable source = %q; want %q", source, operatedBySourceEmpty)
	}
}

// TestResolveAuditOperator_OIDC_BothColumnsPopulated is the dual-write happy
// path: the OIDC ladder yields a TEXT value, the resolver upserts operators +
// operator_aliases, and resolveAuditOperator returns the matching FK pointer.
func TestResolveAuditOperator_OIDC_BothColumnsPopulated(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "matt@acme.example"})
	value, idPtr, source := srv.resolveAuditOperator(ctx)
	if value != "oidc:matt@acme.example" {
		t.Fatalf("value = %q; want oidc:matt@acme.example", value)
	}
	if idPtr == nil || *idPtr == 0 {
		t.Fatalf("idPtr = %v; want non-nil non-zero", idPtr)
	}
	if source != operatedBySourceOIDC {
		t.Fatalf("source = %q; want %q", source, operatedBySourceOIDC)
	}
}

// TestResolveAuditOperator_AnonymousAutonomous_LeavesFKNil exercises the
// "autonomous writes" acceptance bullet from the bead: an anonymous /
// fleet-token / system caller produces (\"\", nil) — the FK stays NULL.
func TestResolveAuditOperator_AnonymousAutonomous_LeavesFKNil(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	cases := []struct {
		name string
		ctx  context.Context
	}{
		{"anonymous", context.Background()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, idPtr, _ := srv.resolveAuditOperator(tc.ctx)
			if value != "" || idPtr != nil {
				t.Fatalf("autonomous write = (%q, %v); want (\"\", nil)", value, idPtr)
			}
		})
	}
}

// TestResolveAuditOperator_AgentBeadFallback_BothColumnsPopulated covers the
// inheritance path: a static-token agent without an X-Operated-By header
// inherits from its agent bead's operated_by field, and the FK side resolves
// alongside the TEXT side.
func TestResolveAuditOperator_AgentBeadFallback_BothColumnsPopulated(t *testing.T) {
	bead := &model.Bead{
		ID:     "agent-x",
		Fields: []byte(`{"operated_by":"slack:U0OPERATOR2"}`),
	}
	srv := newTestServerWithAgentBead(bead)

	ctx := withTestAgent(context.Background(), "agent-x")
	value, idPtr, source := srv.resolveAuditOperator(ctx)
	if value != "slack:U0OPERATOR2" {
		t.Fatalf("value = %q; want slack:U0OPERATOR2", value)
	}
	if idPtr == nil || *idPtr == 0 {
		t.Fatalf("idPtr = %v; want non-nil non-zero", idPtr)
	}
	if source != operatedBySourceFallback {
		t.Fatalf("source = %q; want %q", source, operatedBySourceFallback)
	}
}

// TestResolveAuditOperator_Fleet_HonorsHeader pins the bridge-attribution fix
// (ka-0000001777): a fleet-token call (slack-bridge / jira-bridge / gitlab-
// bridge) that sets X-Operated-By to a human-shape value resolves to the
// human, NOT to the fleet sentinel. Without the fix the bridge stamped
// fleet:slack-bridge on every thread-bead create and Matt's mention attribution
// disappeared. sanitizeOperatedByHeader already restricts the header to
// human shapes (slack:U… / oidc:…@…), so allowing fleet tokens to self-
// attribute stays inside the same trust boundary as the static-primary case.
func TestResolveAuditOperator_Fleet_HonorsHeader(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")

	value, idPtr, source := srv.resolveAuditOperator(ctx)
	if value != "slack:U0OPERATOR1" {
		t.Fatalf("value = %q; want slack:U0OPERATOR1 (header must win over fleet sentinel)", value)
	}
	if idPtr == nil || *idPtr == 0 {
		t.Fatalf("idPtr = %v; want non-nil non-zero (FK side must dual-write)", idPtr)
	}
	if source != operatedBySourceHeader {
		t.Fatalf("source = %q; want %q", source, operatedBySourceHeader)
	}
}

// TestResolveAuditOperator_Fleet_NoHeader_FallsBackToSentinel pins the
// service-driven path: when a fleet token writes without an X-Operated-By
// header (e.g. controller reconciler, scheduler dual-write), the resolver
// still returns the fleet sentinel so audit columns stay NOT NULL.
func TestResolveAuditOperator_Fleet_NoHeader_FallsBackToSentinel(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})

	value, _, source := srv.resolveAuditOperator(ctx)
	if value != "fleet:slack-bridge" {
		t.Fatalf("value = %q; want fleet:slack-bridge", value)
	}
	if source != operatedBySourceFleet {
		t.Fatalf("source = %q; want %q", source, operatedBySourceFleet)
	}
}

// TestResolveAuditOperator_Edge_HonorsHeader pins the gardenio-edge-auth path
// (cli.gardenio PAT shim / AVA-JWT proxy): EdgeAuthMiddleware verifies the HMAC
// envelope, sets AuthIdentity{Type:"edge"} and stamps "oidc:<email>" via
// applyOperatedByHeader. The resolver must honor that header (trust = HMAC,
// not raw client) so writes through the shim resolve instead of refusing with
// resolver_source=unknown. Regression guard for ka-0000004118.
func TestResolveAuditOperator_Edge_HonorsHeader(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "edge", Subject: "matthew.baker@acme.example"})
	ctx = WithOperatedByHeader(ctx, "oidc:matthew.baker@acme.example")

	value, idPtr, source := srv.resolveAuditOperator(ctx)
	if value != "oidc:matthew.baker@acme.example" {
		t.Fatalf("value = %q; want oidc:matthew.baker@acme.example (edge header must resolve)", value)
	}
	if idPtr == nil || *idPtr == 0 {
		t.Fatalf("idPtr = %v; want non-nil non-zero (FK side must dual-write)", idPtr)
	}
	if source != operatedBySourceHeader {
		t.Fatalf("source = %q; want %q", source, operatedBySourceHeader)
	}
}

// TestResolveAuditOperator_Edge_NoHeader_Empty is defensive: a trusted edge
// identity always carries a stamped header in production, but if the header is
// somehow absent the resolver must not invent an operator — it returns empty
// so resolveAuditOperatorRequired fails closed rather than mis-attributing.
func TestResolveAuditOperator_Edge_NoHeader_Empty(t *testing.T) {
	srv := newTestServerWithAgentBead(nil)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "edge", Subject: "matthew.baker@acme.example"})

	value, _, _ := srv.resolveAuditOperator(ctx)
	if value != "" {
		t.Fatalf("value = %q; want empty (no header → no attribution)", value)
	}
}

// TestResolveAuditOperatorRequired_LegacySentinel_HeaderPath simulates the
// FSM-353 jira-bridge spawn path (ka-0000005841): the controller injected
// BEADS_OPERATED_BY=system:legacy-pre-attribution into the agent pod,
// which kd CLI forwards as the X-Operated-By header. applyOperatedByHeader
// must route the sentinel through WithSystemOperator instead of rejecting
// it, and resolveAuditOperatorRequired must return ok=true with a non-zero
// FK pointing at the seeded sentinel operators row.
func TestResolveAuditOperatorRequired_LegacySentinel_HeaderPath(t *testing.T) {
	// Pin the R6 cut-over OFF so this test deterministically exercises the
	// PRE-cutover binding path regardless of wall-clock. Without this the test
	// silently flips red on/after legacySentinelCutoverDate (2026-09-01), when
	// time.Now() activates the gate and the sentinel is refused instead of
	// bound. The post-cutover refusal is covered by the _CutoverActive_ tests.
	t.Setenv("BEADS_LEGACY_SENTINEL_CUTOVER", "0")
	srv := newTestServerWithAgentBead(nil)

	// Simulate the auth interceptor: install identity, then route the
	// sentinel header through applyOperatedByHeader.
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "none"})
	ctx = applyOperatedByHeader(ctx, operatorLegacyPreAttribution, "none", "test")

	value, id, _, ok := srv.resolveAuditOperatorRequired(ctx)
	if !ok {
		t.Fatalf("resolveAuditOperatorRequired returned ok=false; want ok=true (sentinel must FK-resolve)")
	}
	if value != operatorLegacyPreAttribution {
		t.Fatalf("value = %q; want %q", value, operatorLegacyPreAttribution)
	}
	if id == 0 {
		t.Fatalf("id = 0; want non-zero (seeded sentinel row must exist after cold-path upsert)")
	}
}

// TestResolveAuditOperatorRequired_LegacySentinel_AgentBeadPath simulates
// the inheritance variant: no X-Operated-By header on the request, but
// the agent bead carries operated_by=system:legacy-pre-attribution from
// its spawn metadata. The chain walker must return the sentinel and the
// resolver must FK-bind it. (Covers the path where BEADS_OPERATED_BY was
// unset but the agent bead inherited the sentinel.)
func TestResolveAuditOperatorRequired_LegacySentinel_AgentBeadPath(t *testing.T) {
	// Pin the R6 cut-over OFF — see the _HeaderPath sibling above. This test
	// exercises the pre-cutover sentinel FK-binding via the agent-bead walk;
	// the post-cutover refusal on the same path is _CutoverActive_AgentBeadPath.
	t.Setenv("BEADS_LEGACY_SENTINEL_CUTOVER", "0")
	bead := &model.Bead{
		ID:     "agent-x",
		Fields: []byte(`{"operated_by":"system:legacy-pre-attribution"}`),
	}
	srv := newTestServerWithAgentBead(bead)

	ctx := withTestAgent(context.Background(), "agent-x")
	value, id, source, ok := srv.resolveAuditOperatorRequired(ctx)
	if !ok {
		t.Fatalf("resolveAuditOperatorRequired returned ok=false; want ok=true (sentinel must FK-resolve)")
	}
	if value != operatorLegacyPreAttribution {
		t.Fatalf("value = %q; want %q", value, operatorLegacyPreAttribution)
	}
	if id == 0 {
		t.Fatalf("id = 0; want non-zero")
	}
	if source != operatedBySourceFallback {
		t.Fatalf("source = %q; want %q (sentinel came from agent-bead walk)", source, operatedBySourceFallback)
	}
}

// TestSplitOperator pins the parser invariants: empty input → empty parts;
// no-colon input → empty kind; "<kind>:<subject>" → split parts.
func TestSplitOperator(t *testing.T) {
	cases := []struct {
		in          string
		wantKind    string
		wantSubject string
	}{
		{"", "", ""},
		{"no-colon", "", "no-colon"},
		{"oidc:matt@acme.example", "oidc", "matt@acme.example"},
		{"slack:U0OPERATOR1", "slack", "U0OPERATOR1"},
		{"system:scheduler", "system", "scheduler"},
		// Subject containing a colon should keep everything past the first.
		{"oidc:scope:value", "oidc", "scope:value"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotKind, gotSubject := splitOperator(tc.in)
			if gotKind != tc.wantKind || gotSubject != tc.wantSubject {
				t.Errorf("splitOperator(%q) = (%q, %q); want (%q, %q)",
					tc.in, gotKind, gotSubject, tc.wantKind, tc.wantSubject)
			}
		})
	}
}
