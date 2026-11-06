package server

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// seedScrubTaint registers a taint in the mock store's taint registry with
// the given scrub_policy, mirroring seedTeamBead's role for team beads.
func seedScrubTaint(t *testing.T, ms *mockStore, id int64, name string, policy model.ScrubPolicy) {
	t.Helper()
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal scrub policy: %v", err)
	}
	if ms.taintRegistry == nil {
		ms.taintRegistry = make(map[string]*model.Taint)
	}
	ms.taintRegistry[name] = &model.Taint{
		ID:          id,
		Name:        name,
		OwnerTeam:   "gd-teamA",
		ScrubPolicy: raw,
		Effects:     model.DefaultEffects(),
	}
}

// seedScrubRequestBead seeds a pending scrub-request bead directly into the
// mock store, bypassing applyScrubRequestGuard (create-time) the way the
// escalation tests bypass escalation create — these tests target the
// approve/reject handlers, not create.
func seedScrubRequestBead(ms *mockStore, id, taintName, kind, requestedBy string) {
	fields, _ := json.Marshal(map[string]any{
		"taint":             taintName,
		"kind":              kind,
		"requested_by":      requestedBy,
		"resolved_bead_ids": []string{"gd-target1"},
		"approval_status":   "pending",
	})
	ms.beads[id] = &model.Bead{
		ID: id, Kind: model.KindData, Type: model.TypeScrubRequest,
		Title: "scrub " + id, Status: model.StatusOpen, Fields: fields,
	}
}

func scrubRequestFields(t *testing.T, ms *mockStore, id string) map[string]any {
	t.Helper()
	bead := ms.beads[id]
	if bead == nil {
		t.Fatalf("bead %s not found in store", id)
	}
	var fields map[string]any
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("failed to decode fields: %v", err)
	}
	return fields
}

// TestApproveScrubRequest_BodyApprovedByIgnored verifies taints ADR §6.3
// correction 1 for the scrub-request approve path (acceptance criterion a):
// a body-supplied approved_by is a spoofing attempt and must be ignored in
// favor of the authenticated caller's own identity — only logged, never
// applied.
func TestApproveScrubRequest_BodyApprovedByIgnored(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{
		Default: model.ScrubModeHuman,
	})
	seedScrubRequestBead(ms, "gd-scrub1", "t1", "detaint", "someone-else@example.com")

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub1/approve", map[string]any{
		"approved_by": "gd-user-alice",
	})
	requireStatus(t, rec, 200)

	fields := scrubRequestFields(t, ms, "gd-scrub1")
	if fields["approved_by"] != defaultTestSubject {
		t.Errorf("expected approved_by=%s (server-derived identity, not the spoofed body value), got %v", defaultTestSubject, fields["approved_by"])
	}
	if fields["approval_status"] != "approved" {
		t.Errorf("expected approval_status=approved, got %v", fields["approval_status"])
	}
	bead := ms.beads["gd-scrub1"]
	// gd-qn6yEdlOmx: successful execution closes the scrub-request bead
	// (mirrors reject's CloseBead) rather than leaving it in_progress forever.
	if bead.Status != model.StatusClosed {
		t.Errorf("expected status=closed after successful execution, got %s", bead.Status)
	}
	if len(ms.taintAudit) != 1 || ms.taintAudit[0].Action != model.TaintAuditScrubApprove {
		t.Fatalf("expected one scrub-approve taint_audit row, got %+v", ms.taintAudit)
	}
}

// TestApproveScrubRequest_RequiresOIDC verifies taints ADR §6.3 correction 2
// for scrub-request approve (acceptance criterion b): a caller with no
// OIDC-derived human identity — here a fully anonymous caller, the same
// posture as a bare static/fleet token with no relayed human — is refused
// with 403, stronger than requireRole's short-circuit for non-OIDC callers.
func TestApproveScrubRequest_RequiresOIDC(t *testing.T) {
	_, ms, h := newTestServerNoAuth()

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	seedScrubRequestBead(ms, "gd-scrub-noauth", "t1", "detaint", "someone@example.com")

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-noauth/approve", map[string]any{
		"approved_by": "gd-user-alice",
	})
	requireStatus(t, rec, 403)

	bead := ms.beads["gd-scrub-noauth"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected scrub-request to remain open after refused approval, got %s", bead.Status)
	}
	fields := scrubRequestFields(t, ms, "gd-scrub-noauth")
	if fields["approval_status"] != "pending" {
		t.Errorf("expected approval_status to remain pending, got %v", fields["approval_status"])
	}
	if _, ok := fields["approved_by"]; ok {
		t.Errorf("expected no approved_by field to be recorded, got %v", fields["approved_by"])
	}
}

// TestApproveScrubRequest_RejectsRelayedOperatedBy is the regression test
// for the coordinator send-back on PR #87: scrub-request approve must NOT
// accept a static/fleet token relaying a human identity via X-Operated-By.
// Any holder of the shared crew static token controls that header's value,
// so honoring it here would let that caller name an arbitrary
// "oidc:<email>" — including a real approver's address — and pass four-eyes
// without ever having authenticated as that person. gd-ZjiSd9Kzio round 3
// brought escalation approve (TestApproveEscalation_StaticTokenWithOperatedByHuman)
// in line with this same refusal — this test predates that fix and pins the
// behavior scrub-requests always had.
func TestApproveScrubRequest_RejectsRelayedOperatedBy(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	seedScrubRequestBead(ms, "gd-scrub-relay", "t1", "detaint", "someone@example.com")

	rec := doStaticTokenWithOperator(t, h, "POST", "/v1/scrub-requests/gd-scrub-relay/approve",
		"oidc:matt@example.com", map[string]any{"approval_reason": "looks fine"})
	requireStatus(t, rec, 403)

	bead := ms.beads["gd-scrub-relay"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected scrub-request to remain open after refused relayed approval, got %s", bead.Status)
	}
	fields := scrubRequestFields(t, ms, "gd-scrub-relay")
	if fields["approval_status"] != "pending" {
		t.Errorf("expected approval_status to remain pending, got %v", fields["approval_status"])
	}
	if _, ok := fields["approved_by"]; ok {
		t.Errorf("expected no approved_by field to be recorded, got %v", fields["approved_by"])
	}
}

// TestRejectScrubRequest_RejectsRelayedOperatedBy mirrors
// TestApproveScrubRequest_RejectsRelayedOperatedBy for reject: the same
// relayed-static-token shape must not be able to close a scrub-request as
// denied either.
func TestRejectScrubRequest_RejectsRelayedOperatedBy(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	seedScrubRequestBead(ms, "gd-scrub-relay-reject", "t1", "detaint", "someone@example.com")

	rec := doStaticTokenWithOperator(t, h, "POST", "/v1/scrub-requests/gd-scrub-relay-reject/reject",
		"oidc:matt@example.com", map[string]any{"reason": "not needed"})
	requireStatus(t, rec, 403)

	bead := ms.beads["gd-scrub-relay-reject"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected scrub-request to remain open after refused relayed rejection, got %s", bead.Status)
	}
}

// TestRejectScrubRequest_RestrictedSameAsApprove verifies acceptance
// criterion d's identity half: reject has the SAME OIDC-only restriction as
// approve, unlike handleRejectEscalation (which accepts a body-supplied
// rejected_by with no identity check at all).
func TestRejectScrubRequest_RestrictedSameAsApprove(t *testing.T) {
	_, ms, h := newTestServerNoAuth()

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	seedScrubRequestBead(ms, "gd-scrub-reject-noauth", "t1", "detaint", "someone@example.com")

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-reject-noauth/reject", map[string]any{
		"reason": "not needed",
	})
	requireStatus(t, rec, 403)

	bead := ms.beads["gd-scrub-reject-noauth"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected scrub-request to remain open after refused rejection, got %s", bead.Status)
	}
	fields := scrubRequestFields(t, ms, "gd-scrub-reject-noauth")
	if fields["approval_status"] != "pending" {
		t.Errorf("expected approval_status to remain pending, got %v", fields["approval_status"])
	}
}

// TestRejectScrubRequest_RequiresReason verifies acceptance criterion d's
// reason half: `kd scrub reject` requires --reason, enforced server-side as
// a 400 on an empty/missing reason.
func TestRejectScrubRequest_RequiresReason(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	seedScrubRequestBead(ms, "gd-scrub-reject1", "t1", "detaint", "someone@example.com")

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-reject1/reject", map[string]any{})
	requireStatus(t, rec, 400)

	bead := ms.beads["gd-scrub-reject1"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected scrub-request to remain open when reason is missing, got %s", bead.Status)
	}

	rec = doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-reject1/reject", map[string]any{
		"reason": "duplicate request",
	})
	requireStatus(t, rec, 200)

	bead = ms.beads["gd-scrub-reject1"]
	if bead.Status != model.StatusClosed {
		t.Errorf("expected status=closed after reject, got %s", bead.Status)
	}
	fields := scrubRequestFields(t, ms, "gd-scrub-reject1")
	if fields["approval_status"] != "denied" {
		t.Errorf(`expected approval_status="denied" (the schema's only reject-adjacent enum value), got %v`, fields["approval_status"])
	}
	if fields["approval_reason"] != "duplicate request" {
		t.Errorf("expected approval_reason=%q, got %v", "duplicate request", fields["approval_reason"])
	}
	if len(ms.taintAudit) != 1 || ms.taintAudit[0].Action != model.TaintAuditScrubReject {
		t.Fatalf("expected one scrub-reject taint_audit row, got %+v", ms.taintAudit)
	}
}

// TestApproveScrubRequest_FourEyesBlocksSameOperator verifies taints ADR
// §6.3 correction 3 (acceptance criterion c, part 1): when the resolved mode
// is human-four-eyes, an approver that resolves — AFTER operator_aliases
// resolution — to the SAME operators.id as the request's requested_by is
// refused, even though the raw identity strings differ ("slack:U123" vs the
// OIDC approver's bare email).
func TestApproveScrubRequest_FourEyesBlocksSameOperator(t *testing.T) {
	_, ms, h := newTestServer()

	ms.operatorAliases = map[string]int64{
		"slack\x00U123":                 1,
		"oidc\x00" + defaultTestSubject: 1,
	}

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{
		Default:   model.ScrubModeHumanFourEyes,
		Approvers: []string{"team:gd-teamA"},
	})
	seedScrubRequestBead(ms, "gd-scrub-4eyes-same", "t1", "detaint", "slack:U123")

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-4eyes-same/approve", map[string]any{})
	requireStatus(t, rec, 403)

	bead := ms.beads["gd-scrub-4eyes-same"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected scrub-request to remain open after four-eyes refusal, got %s", bead.Status)
	}
	fields := scrubRequestFields(t, ms, "gd-scrub-4eyes-same")
	if fields["approval_status"] != "pending" {
		t.Errorf("expected approval_status to remain pending, got %v", fields["approval_status"])
	}
}

// TestApproveScrubRequest_FourEyesAllowsDifferentOperatorInApprovers
// verifies acceptance criterion c, part 2: a genuinely different requester
// and approver (distinct operators.id after alias resolution) is let
// through, PROVIDED the approver is also a member of the rule's approvers
// (the PINNED coordinator constraint: scrub_policy approvers entries are
// "team:<team bead id>", compared as bead ids via TeamsOf — no slug
// resolution).
func TestApproveScrubRequest_FourEyesAllowsDifferentOperatorInApprovers(t *testing.T) {
	_, ms, h := newTestServer()

	ms.operatorAliases = map[string]int64{
		"slack\x00U123":                 1,
		"oidc\x00" + defaultTestSubject: 2,
	}
	seedTeamBead(ms, "gd-teamA", `{"team":"teamA","members":[2]}`)

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{
		Default:   model.ScrubModeHumanFourEyes,
		Approvers: []string{"team:gd-teamA"},
	})
	seedScrubRequestBead(ms, "gd-scrub-4eyes-diff", "t1", "detaint", "slack:U123")

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-4eyes-diff/approve", map[string]any{})
	requireStatus(t, rec, 200)

	fields := scrubRequestFields(t, ms, "gd-scrub-4eyes-diff")
	if fields["approved_by"] != defaultTestSubject {
		t.Errorf("expected approved_by=%s, got %v", defaultTestSubject, fields["approved_by"])
	}
	if fields["approval_mode"] != string(model.ScrubModeHumanFourEyes) {
		t.Errorf("expected approval_mode=%s, got %v", model.ScrubModeHumanFourEyes, fields["approval_mode"])
	}
}

// TestApproveScrubRequest_FourEyesBlocksApproverNotInApprovers verifies the
// approvers-membership half of correction 3 independently of the
// same-operator check: a genuinely different approver who is NOT a member of
// the rule's approvers is still refused.
func TestApproveScrubRequest_FourEyesBlocksApproverNotInApprovers(t *testing.T) {
	_, ms, h := newTestServer()

	ms.operatorAliases = map[string]int64{
		"slack\x00U123":                 1,
		"oidc\x00" + defaultTestSubject: 2,
	}
	// gd-teamA has no members at all — the approver (operator 2) is not in it.
	seedTeamBead(ms, "gd-teamA", `{"team":"teamA","members":[]}`)

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{
		Default:   model.ScrubModeHumanFourEyes,
		Approvers: []string{"team:gd-teamA"},
	})
	seedScrubRequestBead(ms, "gd-scrub-4eyes-notmember", "t1", "detaint", "slack:U123")

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-4eyes-notmember/approve", map[string]any{})
	requireStatus(t, rec, 403)
}

// TestApproveScrubRequest_RuleModeOverridesDefault verifies
// resolveScrubRuleForApproval picks a matching rule's mode (auto, here)
// over the policy default
// (human-four-eyes), and that an "auto"-resolved approval does not enforce
// four-eyes.
func TestApproveScrubRequest_RuleModeOverridesDefault(t *testing.T) {
	_, ms, h := newTestServer()

	ms.operatorAliases = map[string]int64{
		"oidc\x00" + defaultTestSubject: 1,
	}

	ruleMaxBeads := 5
	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{
		Rules: []model.ScrubRule{
			{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeAuto, MaxBeads: &ruleMaxBeads},
		},
		Default:   model.ScrubModeHumanFourEyes,
		Approvers: []string{"team:gd-teamA"},
	})
	// requested_by resolves to the SAME operator as the approver — would
	// violate four-eyes if the default mode applied, but the matching rule
	// resolves to auto, which does not enforce it.
	seedScrubRequestBead(ms, "gd-scrub-rule-auto", "t1", "detaint", "oidc:"+defaultTestSubject)

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-rule-auto/approve", map[string]any{})
	requireStatus(t, rec, 200)

	fields := scrubRequestFields(t, ms, "gd-scrub-rule-auto")
	if fields["approval_mode"] != string(model.ScrubModeAuto) {
		t.Errorf("expected approval_mode=%s, got %v", model.ScrubModeAuto, fields["approval_mode"])
	}
}

// seedTaintAuditScrubExecuteRows appends n scrub-execute taint_audit rows
// for taintID directly into the mock store, simulating n real scrubs having
// already executed today (docs/adr/taints.md §11 invariant 5: caps are
// counted from durable taint_audit rows, never a client-supplied counter) —
// without having to actually run n scrubs through the handler.
func seedTaintAuditScrubExecuteRows(ms *mockStore, taintID int64, n int) {
	for i := 0; i < n; i++ {
		ms.taintAudit = append(ms.taintAudit, &model.TaintAudit{
			ID:        int64(len(ms.taintAudit) + 1),
			TaintID:   taintID,
			Action:    model.TaintAuditScrubExecute,
			Actor:     "someone@example.com",
			CreatedAt: time.Now().UTC(),
		})
	}
}

// TestApproveScrubRequest_OverMaxBeadsFallsThrough is acceptance criterion 1
// for gd-d3ZZbjU9gH (docs/adr/taints.md §6.3/§10.5): an "auto" rule whose
// request exceeds the rule's own max_beads must not auto-approve a
// truncated scope — it falls through to the next matching rule (here, a
// second "detaint" rule in human mode), never auto-approves anyway.
func TestApproveScrubRequest_OverMaxBeadsFallsThrough(t *testing.T) {
	_, ms, h := newTestServer()

	lowCap := 1
	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{
		Rules: []model.ScrubRule{
			{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeAuto, MaxBeads: &lowCap},
			{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeHuman},
		},
		Default: model.ScrubModeHumanFourEyes,
	})
	// 2 resolved beads exceeds the first rule's max_beads of 1.
	seedScrubRequestBeadWithAttestation(ms, "gd-scrub-overcap", "t1", "detaint", "someone@example.com",
		[]string{"gd-target1", "gd-target2"}, nil)

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-overcap/approve", map[string]any{})
	requireStatus(t, rec, 200)

	fields := scrubRequestFields(t, ms, "gd-scrub-overcap")
	if fields["approval_mode"] != string(model.ScrubModeHuman) {
		t.Errorf("expected approval_mode=human (fell through the over-max_beads auto rule to the next matching rule), got %v", fields["approval_mode"])
	}
}

// TestApproveScrubRequest_OverMaxPerDayFallsThrough is acceptance criterion
// 2 for gd-d3ZZbjU9gH: an "auto" rule whose taint has already hit
// max_per_day today — counted from taint_audit, seeded directly here rather
// than running N real scrubs — falls through the same way as an
// over-max_beads rule.
func TestApproveScrubRequest_OverMaxPerDayFallsThrough(t *testing.T) {
	_, ms, h := newTestServer()

	maxBeads := 10
	maxPerDay := 2
	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{
		Rules: []model.ScrubRule{
			{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeAuto, MaxBeads: &maxBeads, MaxPerDay: &maxPerDay},
		},
		Default: model.ScrubModeHuman,
	})
	// The taint has already hit its max_per_day of 2 today.
	seedTaintAuditScrubExecuteRows(ms, 1, 2)
	seedScrubRequestBead(ms, "gd-scrub-daycap", "t1", "detaint", "someone@example.com")

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-daycap/approve", map[string]any{})
	requireStatus(t, rec, 200)

	fields := scrubRequestFields(t, ms, "gd-scrub-daycap")
	if fields["approval_mode"] != string(model.ScrubModeHuman) {
		t.Errorf("expected approval_mode=human (fell through the at-max_per_day auto rule), got %v", fields["approval_mode"])
	}
}

// TestApproveScrubRequest_RequestWouldPushDailyTotalOverCapFallsThrough is
// the merge-desk-flagged boundary gap in TestApproveScrubRequest_OverMaxPerDayFallsThrough
// (PR #182 review): scrubRuleWithinCaps must fall through not only when
// today's total has ALREADY hit max_per_day, but whenever approving THIS
// request would push it over — dailyTotal + this request's own bead count >
// max_per_day. 28 already executed today + a 3-bead request under
// max_per_day=30 must fall through (28 < 30 alone looks within cap, but
// 28+3=31 overshoots); a 2-bead request in the same state must still
// auto-approve, landing exactly at the cap (28+2=30, not over).
func TestApproveScrubRequest_RequestWouldPushDailyTotalOverCapFallsThrough(t *testing.T) {
	maxBeads := 10
	maxPerDay := 30

	t.Run("3-bead request would land the day at 31, falls through", func(t *testing.T) {
		_, ms, h := newTestServer()
		seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{
			Rules: []model.ScrubRule{
				{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeAuto, MaxBeads: &maxBeads, MaxPerDay: &maxPerDay},
			},
			Default: model.ScrubModeHuman,
		})
		seedTaintAuditScrubExecuteRows(ms, 1, 28)
		seedScrubRequestBeadWithAttestation(ms, "gd-scrub-wouldoverflow", "t1", "detaint", "someone@example.com",
			[]string{"gd-target1", "gd-target2", "gd-target3"}, nil)

		rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-wouldoverflow/approve", map[string]any{})
		requireStatus(t, rec, 200)

		fields := scrubRequestFields(t, ms, "gd-scrub-wouldoverflow")
		if fields["approval_mode"] != string(model.ScrubModeHuman) {
			t.Errorf("expected approval_mode=human (28 executed + 3 requested = 31 > max_per_day 30), got %v", fields["approval_mode"])
		}
	})

	t.Run("2-bead request lands exactly at the cap, still auto-approves", func(t *testing.T) {
		_, ms, h := newTestServer()
		seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{
			Rules: []model.ScrubRule{
				{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeAuto, MaxBeads: &maxBeads, MaxPerDay: &maxPerDay},
			},
			Default: model.ScrubModeHuman,
		})
		seedTaintAuditScrubExecuteRows(ms, 1, 28)
		seedScrubRequestBeadWithAttestation(ms, "gd-scrub-exactcap", "t1", "detaint", "someone@example.com",
			[]string{"gd-target1", "gd-target2"}, nil)

		rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-exactcap/approve", map[string]any{})
		requireStatus(t, rec, 200)

		fields := scrubRequestFields(t, ms, "gd-scrub-exactcap")
		if fields["approval_mode"] != string(model.ScrubModeAuto) {
			t.Errorf("expected approval_mode=auto (28 executed + 2 requested = 30, exactly at max_per_day, not over), got %v", fields["approval_mode"])
		}
	})
}

// TestApproveScrubRequest_MaxPerDayDefaultsToTenTimesMaxBeads is acceptance
// criterion 3 for gd-d3ZZbjU9gH (docs/adr/taints.md §10.5): a rule that
// declares max_beads but omits max_per_day defaults to 10x max_beads —
// pinned against scrubPolicyDefaultMaxPerDayMultiplier directly so a future
// change to the multiplier constant is forced to update this test's
// expectations too.
func TestApproveScrubRequest_MaxPerDayDefaultsToTenTimesMaxBeads(t *testing.T) {
	maxBeads := 3
	defaultCap := maxBeads * scrubPolicyDefaultMaxPerDayMultiplier

	t.Run("just under the default cap still auto-approves", func(t *testing.T) {
		_, ms, h := newTestServer()
		seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{
			Rules: []model.ScrubRule{
				{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeAuto, MaxBeads: &maxBeads},
			},
			Default: model.ScrubModeHuman,
		})
		seedTaintAuditScrubExecuteRows(ms, 1, defaultCap-1)
		seedScrubRequestBead(ms, "gd-scrub-underdefault", "t1", "detaint", "someone@example.com")

		rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-underdefault/approve", map[string]any{})
		requireStatus(t, rec, 200)

		fields := scrubRequestFields(t, ms, "gd-scrub-underdefault")
		if fields["approval_mode"] != string(model.ScrubModeAuto) {
			t.Errorf("expected approval_mode=auto (under the default %dx max_beads cap), got %v", scrubPolicyDefaultMaxPerDayMultiplier, fields["approval_mode"])
		}
	})

	t.Run("at the default cap falls through", func(t *testing.T) {
		_, ms, h := newTestServer()
		seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{
			Rules: []model.ScrubRule{
				{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeAuto, MaxBeads: &maxBeads},
			},
			Default: model.ScrubModeHuman,
		})
		seedTaintAuditScrubExecuteRows(ms, 1, defaultCap)
		seedScrubRequestBead(ms, "gd-scrub-atdefault", "t1", "detaint", "someone@example.com")

		rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-atdefault/approve", map[string]any{})
		requireStatus(t, rec, 200)

		fields := scrubRequestFields(t, ms, "gd-scrub-atdefault")
		if fields["approval_mode"] != string(model.ScrubModeHuman) {
			t.Errorf("expected approval_mode=human (at the default %dx max_beads cap), got %v", scrubPolicyDefaultMaxPerDayMultiplier, fields["approval_mode"])
		}
	})
}

// TestApproveScrubRequest_RejectedRequestCannotLaterBeApproved covers the
// operator-review fix on PR #144: authorizeResolutionTarget has no status
// check of its own (it authorizes read scope, not resolution eligibility),
// so without an explicit guard a scrub-request closed by reject
// (approval_status=denied) could still be approved afterward — running
// ScrubTaint on a request the operator already denied. The handler must
// reject a closed target with 409 before it ever resolves policy or calls
// ScrubTaint.
func TestApproveScrubRequest_RejectedRequestCannotLaterBeApproved(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeAuto})
	seedScrubRequestBead(ms, "gd-scrub-rejected", "t1", "purge", "someone@example.com")

	rejectRec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-rejected/reject",
		map[string]any{"reason": "not needed"})
	requireStatus(t, rejectRec, 200)

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-rejected/approve", map[string]any{})
	requireStatus(t, rec, 409)

	if len(ms.scrubTaintCalls) != 0 {
		t.Errorf("scrubTaintCalls = %d, want 0 — a rejected scrub-request must never execute", len(ms.scrubTaintCalls))
	}
}

// TestApproveScrubRequest_ReopenedDeniedRequestCannotBeApproved covers the
// merge-desk fix (round 3, PR #144): the closed-status guard above only
// catches a target that is STILL closed. A status-only PATCH
// (`kd update <id> --status open`) reopens the bead without ever touching
// Fields, so approval_status stays "denied" while Status flips back to
// open — bypassing the closed check entirely. The handler must also refuse
// on the denied verdict itself, independent of the bead's current status.
func TestApproveScrubRequest_ReopenedDeniedRequestCannotBeApproved(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeAuto})
	seedScrubRequestBead(ms, "gd-scrub-reopened", "t1", "purge", "someone@example.com")

	rejectRec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-reopened/reject",
		map[string]any{"reason": "not needed"})
	requireStatus(t, rejectRec, 200)

	// Simulate a status-only reopen that never touches Fields (approval_status
	// stays "denied"), bypassing the closed-status guard.
	ms.beads["gd-scrub-reopened"].Status = model.StatusOpen

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-reopened/approve", map[string]any{})
	requireStatus(t, rec, 409)

	if len(ms.scrubTaintCalls) != 0 {
		t.Errorf("scrubTaintCalls = %d, want 0 — a reopened denied scrub-request must never execute", len(ms.scrubTaintCalls))
	}
}

// TestApproveScrubRequest_FieldsWriteFailureIsFatal is the regression test
// for gd-09omBt40fj (merge desk follow-up on PR #144): a failed write of the
// approval fields (approval_status/approved_by/approval_mode) must abort the
// request with a server error, not proceed as if approval succeeded. Before
// the fix, this error was only logged — the handler went on to flip the
// scrub-request to in_progress and record a taint_audit approve row even
// though the bead itself never persisted approval_status=approved, which
// would let a caller treat the 200 as a green light to replay ScrubTaint
// against a request that still reads pending/stale.
//
// updateBeadErrOnce is REQUIRED here, not cosmetic (gd-0TG0s16gcS): the
// handler makes two UpdateBead calls — the fields merge, then (pre-fix,
// reached anyway) the open->in_progress status flip. A plain (permanent)
// updateBeadErr fails both, so the pre-fix log-and-continue handler still
// 500s at the SECOND call and this test can't tell the two code paths
// apart — it passes whether or not the fix is present. Failing only the
// first call lets the pre-fix handler sail through to a 200, which is what
// makes the assertions below actually discriminate.
func TestApproveScrubRequest_FieldsWriteFailureIsFatal(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	seedScrubRequestBead(ms, "gd-scrub-fieldserr", "t1", "detaint", "someone-else@example.com")
	ms.updateBeadErr = errors.New("simulated fields write failure")
	ms.updateBeadErrOnce = true

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-fieldserr/approve", map[string]any{})
	requireStatus(t, rec, 500)

	bead := ms.beads["gd-scrub-fieldserr"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected scrub-request to remain open after failed fields write, got %s", bead.Status)
	}
	fields := scrubRequestFields(t, ms, "gd-scrub-fieldserr")
	if fields["approval_status"] != "pending" {
		t.Errorf("expected approval_status to remain pending, got %v", fields["approval_status"])
	}
	if len(ms.taintAudit) != 0 {
		t.Errorf("expected no taint_audit row when the approval fields write failed, got %+v", ms.taintAudit)
	}
}

// TestRejectScrubRequest_FieldsWriteFailureIsFatal mirrors
// TestApproveScrubRequest_FieldsWriteFailureIsFatal for reject: a lost write
// of approval_status=denied must not be masked by CloseBead succeeding
// anyway, which would leave a closed scrub-request bead whose fields still
// read pending/stale.
func TestRejectScrubRequest_FieldsWriteFailureIsFatal(t *testing.T) {
	_, ms, h := newTestServer()

	seedScrubTaint(t, ms, 1, "t1", model.ScrubPolicy{Default: model.ScrubModeHuman})
	seedScrubRequestBead(ms, "gd-scrub-rejecterr", "t1", "detaint", "someone-else@example.com")
	ms.updateBeadErr = errors.New("simulated fields write failure")

	rec := doJSON(t, h, "POST", "/v1/scrub-requests/gd-scrub-rejecterr/reject", map[string]any{
		"reason": "not needed",
	})
	requireStatus(t, rec, 500)

	bead := ms.beads["gd-scrub-rejecterr"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected scrub-request to remain open after failed fields write, got %s", bead.Status)
	}
	fields := scrubRequestFields(t, ms, "gd-scrub-rejecterr")
	if fields["approval_status"] != "pending" {
		t.Errorf("expected approval_status to remain pending, got %v", fields["approval_status"])
	}
	if len(ms.taintAudit) != 0 {
		t.Errorf("expected no taint_audit row when the reject fields write failed, got %+v", ms.taintAudit)
	}
}
