package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Tests for docs/adr/taints.md §4.2 row 3 (inherit:read), gd-mUiYE0jx1d, and
// the §15.3 trusted-subject amendment layered on top of it.

// getBeadAsAgent issues a GET for beadID through handleGetBead directly
// (bypassing the mux, like taint_read_gate_test.go's style) under a static
// agent-bead identity, and returns the recorder.
func getBeadAsAgent(t *testing.T, s *BeadsServer, beadID, agentBeadID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/beads/"+beadID, nil)
	req.SetPathValue("id", beadID)
	ctx := withTestPrivilegedAgent(req.Context(), agentBeadID)
	ctx = WithOperatedByHeader(ctx, "slack:U_TEST")
	rec := httptest.NewRecorder()
	s.handleGetBead(rec, req.WithContext(ctx))
	return rec
}

// TestPropagateReadTaint_AgentAcquiresOnFirstRead pins the base case of ADR
// §4.2 row 3: an agent reading a tainted bead for the first time acquires
// that taint on its own agent bead, source inherit:read. The taint carries
// the `gate` effect and the agent holds a grant for it — the §15.3 table's
// `gate` row: a grant clears the bead for reading, it does NOT exempt the
// reader from carrying the mark.
func TestPropagateReadTaint_AgentAcquiresOnFirstRead(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team") // Effects unset -> gates by default
	ms.beads["kd-tainted"] = &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-tainted", 1)
	if err := ms.GrantTaint(t.Context(), &model.TaintGrant{
		TaintID: 1, PrincipalKind: model.PrincipalAgent, Principal: "gd-agent1", GrantedBy: 1,
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	rec := getBeadAsAgent(t, s, "kd-tainted", "gd-agent1")
	requireStatus(t, rec, http.StatusOK)

	rows := ms.beadTaints["gd-agent1"]
	row, ok := beadTaintByID(rows, 1)
	if !ok {
		t.Fatalf("expected agent gd-agent1 to acquire taint 1 on read, got %v", rows)
	}
	if row.Source != model.TaintSourceInheritRead {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritRead)
	}
	if row.SourceBeadID != "kd-tainted" {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, "kd-tainted")
	}
}

// TestPropagateReadTaint_UntaintedBeadAcquiresNothing is the negative
// control: reading an untainted bead writes zero bead_taints rows.
func TestPropagateReadTaint_UntaintedBeadAcquiresNothing(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	ms.beads["kd-clean"] = &model.Bead{ID: "kd-clean", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}

	rec := getBeadAsAgent(t, s, "kd-clean", "gd-agent1")
	requireStatus(t, rec, http.StatusOK)

	if rows := ms.beadTaints["gd-agent1"]; len(rows) != 0 {
		t.Fatalf("expected no bead_taints rows for an untainted read, got %v", rows)
	}
}

// TestPropagateReadTaint_SecondReadWithinTTLIsCacheHit pins the cost-control
// acceptance criterion: a second read of the same tainted bead within
// identityCacheTTL performs no bead_taints WRITE — the taintContextOf
// cache entry updated by the first read already reflects the acquired
// taint, so the second read's "does the agent already carry this" check is
// a cache hit and resolveReadTaints finds nothing missing.
func TestPropagateReadTaint_SecondReadWithinTTLIsCacheHit(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["kd-tainted"] = &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-tainted", 1)
	if err := ms.GrantTaint(t.Context(), &model.TaintGrant{
		TaintID: 1, PrincipalKind: model.PrincipalAgent, Principal: "gd-agent1", GrantedBy: 1,
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	requireStatus(t, getBeadAsAgent(t, s, "kd-tainted", "gd-agent1"), http.StatusOK)
	if got := ms.applyTaintCalls; got != 1 {
		t.Fatalf("after first read: applyTaintCalls = %d; want 1", got)
	}

	requireStatus(t, getBeadAsAgent(t, s, "kd-tainted", "gd-agent1"), http.StatusOK)
	if got := ms.applyTaintCalls; got != 1 {
		t.Fatalf("after second read within TTL: applyTaintCalls = %d; want 1 (cache hit, no write)", got)
	}

	rows := ms.beadTaints["gd-agent1"]
	if len(rows) != 1 {
		t.Fatalf("expected exactly one bead_taints row for the agent, got %v", rows)
	}
}

// TestPropagateReadTaint_ThenWritePropagates is the integration test named
// in gd-mUiYE0jx1d's acceptance criteria: after acquiring a taint via read,
// the agent's subsequent write propagates it per p1-prop-write
// (resolveWriteTaints / propagateWriteTaint), chaining both hooks.
func TestPropagateReadTaint_ThenWritePropagates(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["kd-tainted"] = &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-tainted", 1)
	if err := ms.GrantTaint(t.Context(), &model.TaintGrant{
		TaintID: 1, PrincipalKind: model.PrincipalAgent, Principal: "gd-agent1", GrantedBy: 1,
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	requireStatus(t, getBeadAsAgent(t, s, "kd-tainted", "gd-agent1"), http.StatusOK)
	if _, ok := beadTaintByID(ms.beadTaints["gd-agent1"], 1); !ok {
		t.Fatalf("precondition failed: agent did not acquire taint 1 on read")
	}

	target, err := s.createBead(ctxAsTestSubject(), createBeadInput{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	title := "updated by agent that read a tainted bead"
	if _, err := s.updateBead(staticAgentCtx("gd-agent1"), target.ID, updateBeadInput{Title: &title}); err != nil {
		t.Fatalf("updateBead error: %v", err)
	}

	row, ok := beadTaintByID(ms.beadTaints[target.ID], 1)
	if !ok {
		t.Fatalf("expected write by newly-tainted agent to propagate taint 1 onto %s, got %v", target.ID, ms.beadTaints[target.ID])
	}
	if row.Source != model.TaintSourceInheritWrite {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritWrite)
	}
	if row.SourceBeadID != "gd-agent1" {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, "gd-agent1")
	}
}

// TestPropagateReadTaint_TrustedSubjectSkipsNonGateTaint pins the §15.3
// amendment's positive case: a taint whose effects exclude `gate` is NOT
// acquired on read by an agent that holds a grant for it — the Biba
// trusted-subject rule (a granted reviewer's own output must not itself be
// marked untrusted).
func TestPropagateReadTaint_TrustedSubjectSkipsNonGateTaint(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	tt := seedTaint(ms, 2, "untrusted-input", "owner-team")
	tt.Effects = []string{model.EffectRestrict}
	ms.beads["kd-restrict"] = &model.Bead{ID: "kd-restrict", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-restrict", 2)
	if err := ms.GrantTaint(t.Context(), &model.TaintGrant{
		TaintID: 2, PrincipalKind: model.PrincipalAgent, Principal: "gd-reviewer", GrantedBy: 1,
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	rec := getBeadAsAgent(t, s, "kd-restrict", "gd-reviewer")
	requireStatus(t, rec, http.StatusOK)

	if rows := ms.beadTaints["gd-reviewer"]; len(rows) != 0 {
		t.Fatalf("granted trusted subject must NOT acquire a non-gate taint on read, got %v", rows)
	}
}

// TestPropagateReadTaint_UngrantedAgentStillAcquiresNonGateTaint is the
// negative control for the §15.3 branch: WITHOUT a grant, an agent reading
// the same non-gate taint acquires it normally — the exception is keyed on
// holding a grant, not on the taint's effect set alone.
func TestPropagateReadTaint_UngrantedAgentStillAcquiresNonGateTaint(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	tt := seedTaint(ms, 3, "trace-dye", "owner-team")
	tt.Effects = []string{}
	ms.beads["kd-dye"] = &model.Bead{ID: "kd-dye", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-dye", 3)

	rec := getBeadAsAgent(t, s, "kd-dye", "gd-agent-ungranted")
	requireStatus(t, rec, http.StatusOK)

	row, ok := beadTaintByID(ms.beadTaints["gd-agent-ungranted"], 3)
	if !ok {
		t.Fatalf("expected ungranted agent to acquire the trace-only dye taint on read, got %v", ms.beadTaints["gd-agent-ungranted"])
	}
	if row.Source != model.TaintSourceInheritRead {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritRead)
	}
}

// TestPropagateReadTaint_GrantedAgentStillAcquiresGateTaint pins the §15.3
// table's `gate` row explicitly (distinct from the base acquisition test,
// which also happens to use a gate-effect taint): a granted agent reading a
// bead whose taint DOES include `gate` still acquires it — the grant clears
// the read, it does not exempt the reader from the mark. Only effects that
// EXCLUDE gate are eligible for the trusted-subject skip.
func TestPropagateReadTaint_GrantedAgentStillAcquiresGateTaint(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	tt := seedTaint(ms, 4, "gated-secret", "owner-team")
	tt.Effects = []string{model.EffectGate}
	ms.beads["kd-gated"] = &model.Bead{ID: "kd-gated", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-gated", 4)
	if err := ms.GrantTaint(t.Context(), &model.TaintGrant{
		TaintID: 4, PrincipalKind: model.PrincipalAgent, Principal: "gd-agent1", GrantedBy: 1,
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	rec := getBeadAsAgent(t, s, "kd-gated", "gd-agent1")
	requireStatus(t, rec, http.StatusOK)

	row, ok := beadTaintByID(ms.beadTaints["gd-agent1"], 4)
	if !ok {
		t.Fatalf("expected granted agent to still acquire the gate-effect taint on read, got %v", ms.beadTaints["gd-agent1"])
	}
	if row.Source != model.TaintSourceInheritRead {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritRead)
	}
}

// TestPropagateReadTaint_NonAgentCallerAcquiresNothing pins ADR §4.1: an
// OIDC human never carries taints, so reading a tainted bead as one is a
// no-op for propagation (the existing project/taint READ gates still apply
// independently; this test uses a granted human so the read itself
// succeeds, isolating the propagation no-op).
func TestPropagateReadTaint_NonAgentCallerAcquiresNothing(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["kd-tainted"] = &model.Bead{ID: "kd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "kd-tainted", 1)
	ms.operatorAliases = map[string]int64{"oidc\x00insider@example.com": 9}
	if err := ms.GrantTaint(t.Context(), &model.TaintGrant{
		TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "9", GrantedBy: 1,
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/kd-tainted", nil)
	req.SetPathValue("id", "kd-tainted")
	req = req.WithContext(asOIDC(req.Context(), "insider@example.com"))
	rec := httptest.NewRecorder()
	s.handleGetBead(rec, req)

	requireStatus(t, rec, http.StatusOK)
	if before := ms.applyTaintCalls; before != 0 {
		t.Fatalf("an OIDC human read must not call ApplyTaint at all, applyTaintCalls = %d", before)
	}
}
