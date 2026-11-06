package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"gardenio/kbeads/internal/model"
)

// Tests for the third read/write gate ADR docs/adr/taints.md §5.1/§5.3/§9.4/
// §10.1 adds: taintsOf(bead) ⊆ grantsOf(caller). Covers every acceptance
// criterion on gd-fnqTiGs3XH: grantsOf per caller kind, by-id 404, list
// silent omission, narrowing relative to the project-scope gates, the
// zero-config hot path + cache invalidation, and the denial metric.

// seedBeadTaints records beadID as carrying the given taint ids in the
// legacy GetTaintsForBead/GetTaintsForBeads-backing map (distinct from
// ApplyTaint's beadTaints map — see mockStore's field comments).
func seedBeadTaints(ms *mockStore, beadID string, taintIDs ...int64) {
	if ms.taints == nil {
		ms.taints = make(map[string][]int64)
	}
	ms.taints[beadID] = taintIDs
}

// --- grantsOf per caller kind -------------------------------------------

// TestGrantsOf_OIDCHuman covers all three OIDC grant sources (ADR §5.1): the
// operator's own taint_grants row, a row granted to a team it belongs to,
// and implicit ownership of a taint its team owns (no explicit grant row at
// all for that third taint).
func TestGrantsOf_OIDCHuman(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "own-grant", "some-other-team")
	seedTaint(ms, 2, "team-grant", "some-other-team")
	seedTaint(ms, 3, "team-owned", "gd-team-y")
	seedTeamBead(ms, "gd-team-y", `{"team":"team-y","members":["oidc:alice@example.com"]}`)
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}

	ctx := context.Background()
	if err := ms.GrantTaint(ctx, &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "7", GrantedBy: 1}); err != nil {
		t.Fatalf("seed own grant: %v", err)
	}
	if err := ms.GrantTaint(ctx, &model.TaintGrant{TaintID: 2, PrincipalKind: model.PrincipalTeam, Principal: "gd-team-y", GrantedBy: 1}); err != nil {
		t.Fatalf("seed team grant: %v", err)
	}
	// Taint 3 has NO explicit grant row at all — only team-y's ownership.

	reqCtx := asOIDC(ctx, "alice@example.com")
	grants, err := s.grantsOf(reqCtx)
	if err != nil {
		t.Fatalf("grantsOf: %v", err)
	}
	got := map[int64]bool{}
	for _, g := range grants {
		got[g] = true
	}
	for _, want := range []int64{1, 2, 3} {
		if !got[want] {
			t.Errorf("grantsOf missing taint %d, got %v", want, grants)
		}
	}
	if len(got) != 3 {
		t.Errorf("grantsOf = %v, want exactly {1,2,3}", grants)
	}
}

// TestGrantsOf_Agent covers an agent's own taint_grants row union the
// spawn-chain-root human's full effective grant set (ADR §5.1: "an agent may
// read what the human who started it may read").
func TestGrantsOf_Agent(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "agent-own", "some-team")
	seedTaint(ms, 2, "human-own", "some-team")
	seedOwnerAgentBead(t, ms, "root-human-agent", map[string]any{"operated_by": "oidc:alice@example.com"})
	seedOwnerAgentBead(t, ms, "leaf-agent", map[string]any{"spawned_by": "root-human-agent"})
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 7}

	ctx := context.Background()
	if err := ms.GrantTaint(ctx, &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalAgent, Principal: "leaf-agent", GrantedBy: 1}); err != nil {
		t.Fatalf("seed agent grant: %v", err)
	}
	if err := ms.GrantTaint(ctx, &model.TaintGrant{TaintID: 2, PrincipalKind: model.PrincipalOperator, Principal: "7", GrantedBy: 1}); err != nil {
		t.Fatalf("seed root-human grant: %v", err)
	}

	reqCtx := staticAuthCtx("leaf-agent")
	grants, err := s.grantsOf(reqCtx)
	if err != nil {
		t.Fatalf("grantsOf: %v", err)
	}
	got := map[int64]bool{}
	for _, g := range grants {
		got[g] = true
	}
	for _, want := range []int64{1, 2} {
		if !got[want] {
			t.Errorf("grantsOf missing taint %d, got %v", want, grants)
		}
	}
	if len(got) != 2 {
		t.Errorf("grantsOf = %v, want exactly {1,2}", grants)
	}
}

// TestGrantsOf_Agent_NoSpawnChainRoot covers an orphaned agent (no resolvable
// spawn-chain root human): only its own rows count.
func TestGrantsOf_Agent_NoSpawnChainRoot(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "agent-own", "some-team")
	seedOwnerAgentBead(t, ms, "lonely-agent", map[string]any{})

	ctx := context.Background()
	if err := ms.GrantTaint(ctx, &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalAgent, Principal: "lonely-agent", GrantedBy: 1}); err != nil {
		t.Fatalf("seed agent grant: %v", err)
	}

	grants, err := s.grantsOf(staticAuthCtx("lonely-agent"))
	if err != nil {
		t.Fatalf("grantsOf: %v", err)
	}
	if len(grants) != 1 || grants[0] != 1 {
		t.Errorf("grantsOf = %v, want [1]", grants)
	}
}

// TestGrantsOf_FleetToken covers a fleet-token caller: rows granted to the
// fleet name.
func TestGrantsOf_FleetToken(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "fleet-taint", "some-team")
	seedTaint(ms, 2, "other-fleet-taint", "some-team")

	ctx := context.Background()
	if err := ms.GrantTaint(ctx, &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalFleet, Principal: "acme-fleet", GrantedBy: 1}); err != nil {
		t.Fatalf("seed fleet grant: %v", err)
	}
	if err := ms.GrantTaint(ctx, &model.TaintGrant{TaintID: 2, PrincipalKind: model.PrincipalFleet, Principal: "other-fleet", GrantedBy: 1}); err != nil {
		t.Fatalf("seed other-fleet grant: %v", err)
	}

	reqCtx := WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: "acme-fleet"})
	grants, err := s.grantsOf(reqCtx)
	if err != nil {
		t.Fatalf("grantsOf: %v", err)
	}
	if len(grants) != 1 || grants[0] != 1 {
		t.Errorf("grantsOf = %v, want [1] (acme-fleet's own grant only)", grants)
	}
}

// --- primary static token: gated with no bypass ---------------------------

// TestHandleGetBead_PrimaryToken_Returns404 proves the bare primary static
// token (Type=="static", Subject=="static") reading a tainted bead it holds
// no grant for (and relays no X-Operated-By) gets the same 404 any other
// ungranted caller gets — the migration-window escape
// (BEADS_TAINT_PRIMARY_TOKEN_GRANTS) is gone; there is no bypass left
// (gd-3KUNlqC1SQ).
func TestHandleGetBead_PrimaryToken_Returns404(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["kd-tainted"] = &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-tainted", 1)

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/kd-tainted", nil)
	req.SetPathValue("id", "kd-tainted")
	ctx := withTestPrivilegedAgent(req.Context(), "static")
	rec := httptest.NewRecorder()

	s.handleGetBead(rec, req.WithContext(ctx))

	requireStatus(t, rec, http.StatusNotFound)
}

// --- by-id read: 404, and narrowing relative to project scope ------------

// TestHandleGetBead_TaintDenied_Returns404 pins ADR §5.3: a caller lacking a
// grant for a bead's taint gets 404 — not 403 — so existence isn't leaked.
func TestHandleGetBead_TaintDenied_Returns404(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["kd-tainted"] = &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-tainted", 1)

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/kd-tainted", nil)
	req.SetPathValue("id", "kd-tainted")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleGetBead(rec, req)

	requireStatus(t, rec, http.StatusNotFound)
}

// TestHandleGetBead_TaintDenied_NarrowsBeyondProjectScope proves the taint
// gate is a THIRD, narrower check: a fleet token scoped to the bead's own
// project (so it passes the existing project-scope gate) is still denied
// when it lacks a grant for the bead's taint.
func TestHandleGetBead_TaintDenied_NarrowsBeyondProjectScope(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["kd-tainted"] = &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	ms.labels["kd-tainted"] = []string{"project:acme"}
	seedBeadTaints(ms, "kd-tainted", 1)

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/kd-tainted", nil)
	req.SetPathValue("id", "kd-tainted")
	ctx := WithAuthIdentity(req.Context(), AuthIdentity{Type: "fleet", Subject: "acme-fleet"})
	ctx = WithTokenProject(ctx, "acme") // passes the project-scope gate
	rec := httptest.NewRecorder()

	s.handleGetBead(rec, req.WithContext(ctx))

	requireStatus(t, rec, http.StatusNotFound)
}

// TestHandleGetBead_GrantedCaller_Allowed is the control: a caller holding
// the grant reads the bead normally (200), proving the gate only narrows.
func TestHandleGetBead_GrantedCaller_Allowed(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["kd-tainted"] = &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-tainted", 1)
	ms.operatorAliases = map[string]int64{"oidc\x00insider@example.com": 9}
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "9", GrantedBy: 1}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/kd-tainted", nil)
	req.SetPathValue("id", "kd-tainted")
	req = req.WithContext(asOIDC(req.Context(), "insider@example.com"))
	rec := httptest.NewRecorder()

	s.handleGetBead(rec, req)

	requireStatus(t, rec, http.StatusOK)
}

// --- list: silent omission, and narrowing relative to project scope ------

// TestHandleListBeads_TaintDenied_SilentlyOmitted pins the `kd list`
// acceptance criterion: a tainted bead the caller lacks a grant for is
// dropped from the result set with no error, no 403 — it simply isn't there.
func TestHandleListBeads_TaintDenied_SilentlyOmitted(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["kd-clean"] = &model.Bead{ID: "kd-clean", Kind: model.KindIssue, Type: model.TypeTask, Title: "clean"}
	ms.beads["kd-tainted"] = &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "tainted"}
	seedBeadTaints(ms, "kd-tainted", 1)

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleListBeads(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var out struct {
		Beads []*model.Bead `json:"beads"`
		Total int           `json:"total"`
	}
	decodeJSON(t, rec, &out)
	ids := map[string]bool{}
	for _, b := range out.Beads {
		ids[b.ID] = true
	}
	if !ids["kd-clean"] {
		t.Errorf("expected kd-clean in results, got %v", ids)
	}
	if ids["kd-tainted"] {
		t.Errorf("expected kd-tainted silently omitted, got %v", ids)
	}
}

// TestHandleListBeads_TaintDenied_NarrowsBeyondProjectScope proves the list
// predicate narrows a caller who already passes project scope: a
// project-bound fleet token sees both beads in its project, EXCEPT the one
// carrying a taint it holds no grant for.
func TestHandleListBeads_TaintDenied_NarrowsBeyondProjectScope(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["kd-clean"] = &model.Bead{ID: "kd-clean", Kind: model.KindIssue, Type: model.TypeTask, Title: "clean"}
	ms.beads["kd-tainted"] = &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "tainted"}
	ms.labels["kd-clean"] = []string{"project:acme"}
	ms.labels["kd-tainted"] = []string{"project:acme"}
	seedBeadTaints(ms, "kd-tainted", 1)

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	ctx := WithAuthIdentity(req.Context(), AuthIdentity{Type: "fleet", Subject: "acme-fleet"})
	ctx = WithTokenProject(ctx, "acme")
	rec := httptest.NewRecorder()

	s.handleListBeads(rec, req.WithContext(ctx))

	requireStatus(t, rec, http.StatusOK)
	var out struct {
		Beads []*model.Bead `json:"beads"`
	}
	decodeJSON(t, rec, &out)
	ids := map[string]bool{}
	for _, b := range out.Beads {
		ids[b.ID] = true
	}
	if !ids["kd-clean"] {
		t.Errorf("expected kd-clean (in-project, untainted) in results, got %v", ids)
	}
	if ids["kd-tainted"] {
		t.Errorf("expected kd-tainted omitted despite passing project scope, got %v", ids)
	}
}

// --- zero-config hot path + cache invalidation ----------------------------

// TestZeroConfig_NoTaintQueryOnNormalRead pins ADR §10.1: with zero rows in
// the taints registry, a normal bead read never issues a bead_taints query.
func TestZeroConfig_NoTaintQueryOnNormalRead(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	ms.beads["kd-plain"] = &model.Bead{ID: "kd-plain", Kind: model.KindIssue, Type: model.TypeTask, Title: "plain"}

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/kd-plain", nil)
	req.SetPathValue("id", "kd-plain")
	req = req.WithContext(asOIDC(req.Context(), "someone@example.com"))
	rec := httptest.NewRecorder()

	s.handleGetBead(rec, req)

	requireStatus(t, rec, http.StatusOK)
	if ms.beadTaintQueryCalls != 0 {
		t.Errorf("expected 0 bead_taints queries on the zero-config hot path, got %d", ms.beadTaintQueryCalls)
	}
}

// TestZeroConfig_CacheInvalidatedOnTaintCreation pins the second half of
// §10.1: creating the first taint invalidates the cached "empty" verdict
// within the same process, so the very next read re-queries and gates
// correctly.
func TestZeroConfig_CacheInvalidatedOnTaintCreation(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	ms.beads["kd-plain"] = &model.Bead{ID: "kd-plain", Kind: model.KindIssue, Type: model.TypeTask, Title: "plain"}

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/kd-plain", nil)
	req.SetPathValue("id", "kd-plain")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))

	rec1 := httptest.NewRecorder()
	s.handleGetBead(rec1, req)
	requireStatus(t, rec1, http.StatusOK) // registry still empty: allow-all

	// Create the first taint and apply it to the bead, then invalidate —
	// exactly what the POST /v1/taints handler must do on a successful insert.
	seedTaint(ms, 1, "secret", "owner-team")
	seedBeadTaints(ms, "kd-plain", 1)
	s.InvalidateTaintRegistryCache()

	rec2 := httptest.NewRecorder()
	s.handleGetBead(rec2, req)
	requireStatus(t, rec2, http.StatusNotFound) // now gated, and denied (no grant)
}

// --- denial metric ---------------------------------------------------------

// TestTaintDenial_IncrementsMetric_Read pins: a denied by-id read increments
// gardenio_authz_denials_total{reason="taint"}.
func TestTaintDenial_IncrementsMetric_Read(t *testing.T) {
	reg := prometheus.NewRegistry()
	authzDenialsTotal.Reset()
	if err := reg.Register(authzDenialsTotal); err != nil {
		t.Fatalf("register: %v", err)
	}

	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["kd-tainted"] = &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-tainted", 1)

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/kd-tainted", nil)
	req.SetPathValue("id", "kd-tainted")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()
	s.handleGetBead(rec, req)
	requireStatus(t, rec, http.StatusNotFound)

	if got := taintDenialCount(t, reg); got != 1 {
		t.Errorf("gardenio_authz_denials_total{reason=taint} = %v, want 1", got)
	}
}

// TestTaintDenial_IncrementsMetric_Write pins: a denied write (via
// authorizeWriteTaintGate, fed through the standard recordAuthzDenial path)
// increments the same counter.
func TestTaintDenial_IncrementsMetric_Write(t *testing.T) {
	reg := prometheus.NewRegistry()
	authzDenialsTotal.Reset()
	if err := reg.Register(authzDenialsTotal); err != nil {
		t.Fatalf("register: %v", err)
	}

	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	bead := &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	ms.beads["kd-tainted"] = bead
	if _, err := ms.ApplyTaint(context.Background(), &model.BeadTaint{
		BeadID: "kd-tainted", TaintID: 1, Source: model.TaintSourceExplicit, AppliedBy: "9",
	}); err != nil {
		t.Fatalf("apply taint: %v", err)
	}

	ctx := asOIDC(context.Background(), "outsider@example.com")
	denial := s.authorizeWriteTaintGate(ctx, "update", bead)
	if denial == nil {
		t.Fatal("expected a taint write denial, got nil")
	}
	recordAuthzDenial(ctx, denial)

	if got := taintDenialCount(t, reg); got != 1 {
		t.Errorf("gardenio_authz_denials_total{reason=taint} = %v, want 1", got)
	}
}

// taintDenialCount sums gardenio_authz_denials_total across all label
// combinations whose reason label is "taint".
func taintDenialCount(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_authz_denials_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if metricLabel(m, "reason") == "taint" {
				total += m.GetCounter().GetValue()
			}
		}
	}
	return total
}

func metricLabel(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// --- effects (docs/adr/taints.md §15.2): gate-only gating ----------------
//
// gd-9YQiNJVW6u: the read gate, write gate and list predicate must consult
// ONLY taints whose effects include "gate". A taint with effects {} (or any
// non-gate set) must not hide or block anything — this is the one
// behavioural change the task makes, and it is a no-op for every taint that
// exists today (default effects is {gate}).

// TestHandleGetBead_EmptyEffectTaint_NotGated proves a bead carrying only a
// {}-effect taint (trace-only dye, §15.2) is readable by a caller holding no
// grants at all — the read gate must not treat it as a gating taint.
func TestHandleGetBead_EmptyEffectTaint_NotGated(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	dye := seedTaint(ms, 1, "dye-only", "owner-team")
	dye.Effects = []string{}
	ms.beads["kd-dyed"] = &model.Bead{ID: "kd-dyed", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-dyed", 1)

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/kd-dyed", nil)
	req.SetPathValue("id", "kd-dyed")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleGetBead(rec, req)

	requireStatus(t, rec, http.StatusOK)
}

// TestHandleGetBead_GateEffectTaint_StillGated is the control: a bead
// carrying a {gate}-effect taint is still denied to a zero-grant caller —
// proving the {} case above is a genuine narrowing, not a broken gate.
func TestHandleGetBead_GateEffectTaint_StillGated(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	gate := seedTaint(ms, 1, "secret", "owner-team")
	gate.Effects = []string{"gate"}
	ms.beads["kd-tainted"] = &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-tainted", 1)

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/kd-tainted", nil)
	req.SetPathValue("id", "kd-tainted")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleGetBead(rec, req)

	requireStatus(t, rec, http.StatusNotFound)
}

// TestAuthorizeWriteTaintGate_EmptyEffectTaint_NotGated is the write-side
// twin: a {}-effect taint must never block a write either.
func TestAuthorizeWriteTaintGate_EmptyEffectTaint_NotGated(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	dye := seedTaint(ms, 1, "dye-only", "owner-team")
	dye.Effects = []string{}
	bead := &model.Bead{ID: "kd-dyed", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	ms.beads["kd-dyed"] = bead
	if _, err := ms.ApplyTaint(context.Background(), &model.BeadTaint{
		BeadID: "kd-dyed", TaintID: 1, Source: model.TaintSourceExplicit, AppliedBy: "9",
	}); err != nil {
		t.Fatalf("apply taint: %v", err)
	}

	ctx := asOIDC(context.Background(), "outsider@example.com")
	if denial := s.authorizeWriteTaintGate(ctx, "update", bead); denial != nil {
		t.Fatalf("expected no write denial for a {}-effect taint, got %+v", denial)
	}
}

// TestAuthorizeWriteTaintGate_GateEffectTaint_StillGated is the write-side
// control matching TestHandleGetBead_GateEffectTaint_StillGated.
func TestAuthorizeWriteTaintGate_GateEffectTaint_StillGated(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	gate := seedTaint(ms, 1, "secret", "owner-team")
	gate.Effects = []string{"gate"}
	bead := &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	ms.beads["kd-tainted"] = bead
	if _, err := ms.ApplyTaint(context.Background(), &model.BeadTaint{
		BeadID: "kd-tainted", TaintID: 1, Source: model.TaintSourceExplicit, AppliedBy: "9",
	}); err != nil {
		t.Fatalf("apply taint: %v", err)
	}

	ctx := asOIDC(context.Background(), "outsider@example.com")
	if denial := s.authorizeWriteTaintGate(ctx, "update", bead); denial == nil {
		t.Fatal("expected a write denial for a {gate}-effect taint, got nil")
	}
}

// TestHandleListBeads_EmptyEffectTaint_NotOmitted is the list-predicate
// twin: a {}-effect taint must not cause the bead to be silently dropped
// from a zero-grant caller's list results.
func TestHandleListBeads_EmptyEffectTaint_NotOmitted(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	dye := seedTaint(ms, 1, "dye-only", "owner-team")
	dye.Effects = []string{}
	ms.beads["kd-dyed"] = &model.Bead{ID: "kd-dyed", Kind: model.KindIssue, Type: model.TypeTask, Title: "dyed"}
	seedBeadTaints(ms, "kd-dyed", 1)

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleListBeads(rec, req)

	requireStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "kd-dyed") {
		t.Fatalf("expected kd-dyed in list response, got %s", rec.Body.String())
	}
}
