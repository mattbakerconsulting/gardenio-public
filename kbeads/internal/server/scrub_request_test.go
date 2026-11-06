package server

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// baseScrubRequest returns the minimal valid create body for a
// type=scrub-request bead scoped to an explicit bead id list.
func baseScrubRequest(taint string) map[string]any {
	return map[string]any{
		"title":  "scrub tainted export",
		"type":   "scrub-request",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"taint": taint,
			"kind":  "detaint",
			"scope": map[string]any{"bead_ids": []string{"gd-a"}},
		},
	}
}

// grantScrubTaint seeds a taint and grants it to the given operator subject
// (an OIDC email), so applyScrubRequestGuard's grant check passes. It also
// marks "gd-a" — the default scope.bead_ids entry baseScrubRequest uses — as
// carrying taintID, so the taint-membership filter on the explicit bead_ids
// scope form (filterScrubRequestBeadIDsToTaint) does not itself reject the
// bead every base-fixture test creates.
func grantScrubTaint(ms *mockStore, taintID int64, taintName, subject string, operatorID int64) {
	seedTaint(ms, taintID, taintName, "gd-owner-team")
	if ms.operatorAliases == nil {
		ms.operatorAliases = map[string]int64{}
	}
	ms.operatorAliases["oidc\x00"+subject] = operatorID
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{
		TaintID: taintID, PrincipalKind: model.PrincipalOperator, Principal: strconv.FormatInt(operatorID, 10), GrantedBy: 1,
	}); err != nil {
		panic(err)
	}
	seedBeadTaints(ms, "gd-a", taintID)
}

// TestScrubRequestTypeRegistered verifies a create by a grant-holder succeeds
// and resolved_bead_ids is computed from scope.
func TestScrubRequestTypeRegistered(t *testing.T) {
	_, ms, h := newTestServer()
	grantScrubTaint(ms, 1, "acme-production", defaultTestSubject, 5)

	rec := doJSON(t, h, "POST", "/v1/beads", baseScrubRequest("acme-production"))
	requireStatus(t, rec, 201)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	if resp["type"] != "scrub-request" {
		t.Fatalf("expected type=scrub-request, got %v", resp["type"])
	}
	fields, _ := resp["fields"].(map[string]any)
	ids, _ := fields["resolved_bead_ids"].([]any)
	if len(ids) != 1 || ids[0] != "gd-a" {
		t.Errorf("expected resolved_bead_ids=[gd-a], got %v", fields["resolved_bead_ids"])
	}
	if fields["requested_by"] != defaultTestSubject {
		t.Errorf("expected requested_by=%q, got %v", defaultTestSubject, fields["requested_by"])
	}
}

// TestScrubRequestTypeConfig verifies the builtin type config is served with
// kind=data.
func TestScrubRequestTypeConfig(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/configs/type:scrub-request", nil)
	requireStatus(t, rec, 200)

	var cfg struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	decodeJSON(t, rec, &cfg)
	var tc map[string]any
	if err := json.Unmarshal(cfg.Value, &tc); err != nil {
		t.Fatalf("decode type config value: %v", err)
	}
	if tc["kind"] != "data" {
		t.Fatalf("expected kind=data, got %v", tc["kind"])
	}
}

// TestScrubRequestRequiresTaintKindScope verifies the three required author
// fields are enforced by the type schema.
func TestScrubRequestRequiresTaintKindScope(t *testing.T) {
	for _, drop := range []string{"taint", "kind", "scope"} {
		t.Run("missing "+drop, func(t *testing.T) {
			_, _, h := newTestServer()
			body := baseScrubRequest("acme-production")
			delete(body["fields"].(map[string]any), drop)
			rec := doJSON(t, h, "POST", "/v1/beads", body)
			requireStatus(t, rec, 400)
		})
	}
}

// TestScrubRequestRejectsForbiddenFieldsAtCreate is the core fail-closed
// guard (acceptance criterion 1): a requester must not be able to arrive
// already-approved or already-executed.
func TestScrubRequestRejectsForbiddenFieldsAtCreate(t *testing.T) {
	forbidden := []struct {
		field string
		value any
	}{
		{"approval_status", "approved"},
		{"approved_by", "gd-user-evil"},
		{"approved_at", "2026-07-21T00:00:00Z"},
		{"approval_mode", "auto"},
		{"approval_reason", "looks fine"},
		{"executed_at", "2026-07-21T00:00:00Z"},
		{"result", map[string]any{"ok": true}},
		{"artifact_sha", "deadbeef"},
	}
	for _, f := range forbidden {
		t.Run(f.field, func(t *testing.T) {
			_, _, h := newTestServer()
			body := baseScrubRequest("acme-production")
			body["fields"].(map[string]any)[f.field] = f.value
			rec := doJSON(t, h, "POST", "/v1/beads", body)
			requireStatus(t, rec, 400)
		})
	}
}

// TestScrubRequestRejectsDetaintModeAndModeAsUnknownField is acceptance
// criterion 6: neither detaint_mode nor mode is part of this bead type's
// schema, for either kind — the fork-vs-strip choice lives only on the
// taint's own scrub_policy, never on the request.
func TestScrubRequestRejectsDetaintModeAndModeAsUnknownField(t *testing.T) {
	for _, kind := range []string{"detaint", "purge"} {
		for _, field := range []string{"detaint_mode", "mode"} {
			t.Run(kind+"/"+field, func(t *testing.T) {
				_, _, h := newTestServer()
				body := baseScrubRequest("acme-production")
				body["fields"].(map[string]any)["kind"] = kind
				body["fields"].(map[string]any)[field] = "strip"
				rec := doJSON(t, h, "POST", "/v1/beads", body)
				requireStatus(t, rec, 400)
			})
		}
	}
}

// TestScrubRequestStampsRequestedBy verifies requested_by is server-stamped
// from the authenticated identity and cannot be forged by the client
// (acceptance criterion 2).
func TestScrubRequestStampsRequestedBy(t *testing.T) {
	_, ms, h := newTestServerAuth()
	grantScrubTaint(ms, 1, "acme-production", "alice@example.com", 7)

	body := baseScrubRequest("acme-production")
	body["fields"].(map[string]any)["requested_by"] = "gd-someone-else"
	rec := doJSONAs(t, h, "POST", "/v1/beads", body, "alice@example.com")
	requireStatus(t, rec, 201)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	fields, _ := resp["fields"].(map[string]any)
	if fields["requested_by"] != "alice@example.com" {
		t.Errorf("requested_by must be server-stamped, got %v", fields["requested_by"])
	}
}

// TestScrubRequestStampsRequestedBy_EmptyActorDeletesClientValue is PR #66
// send-back finding 3: applyScrubRequestGuard stamped requested_by only when
// actorFromContext was non-empty, silently leaving a client-supplied value
// in place otherwise — so acceptance criterion 2 ("requested_by is always
// server-derived") did not hold unconditionally. This drives the guard
// directly with a context that resolves an operator (and therefore a taint
// grant) via the relayed X-Operated-By header alone, WITHOUT an OIDC
// AuthIdentity attached — actorFromContext(ctx) reads AuthIdentity.Subject
// specifically, so this is the "no authenticated identity, but the request
// still resolves to a legitimate operator" case the fix's comment
// describes (auth disabled / a relaying caller with no identity of its
// own). The forged requested_by must not survive. Since P7 Task 3 that
// unauthenticated relay resolves no grant, so the guard now refuses before it
// reaches the stamp.
func TestScrubRequestStampsRequestedBy_EmptyActorDeletesClientValue(t *testing.T) {
	s, ms, _ := newTestServer()
	grantScrubTaint(ms, 1, "acme-production", "alice@example.com", 7)

	ctx := WithOperatedByHeader(context.Background(), "oidc:alice@example.com")
	fields, _ := json.Marshal(map[string]any{
		"taint":        "acme-production",
		"kind":         "detaint",
		"scope":        map[string]any{"bead_ids": []string{"gd-a"}},
		"requested_by": "gd-someone-else",
	})
	bead := &model.Bead{Type: model.TypeScrubRequest, Fields: fields}

	// P7 Task 3: a relay with no authenticated bridge identity behind it is
	// attribution only, so it resolves no grant and the guard refuses the
	// create outright; the forged requested_by cannot land either way.
	err := s.applyScrubRequestGuard(ctx, bead)
	if err == nil || !strings.Contains(err.Error(), "holds no grant") {
		t.Fatalf("applyScrubRequestGuard = %v, want a no-grant refusal for an unauthenticated relay", err)
	}
}

// TestScrubRequestRequiresGrant is acceptance criterion 3: the requester must
// hold a grant for the named taint, or create is rejected.
func TestScrubRequestRequiresGrant(t *testing.T) {
	t.Run("unknown taint", func(t *testing.T) {
		_, _, h := newTestServer()
		rec := doJSON(t, h, "POST", "/v1/beads", baseScrubRequest("no-such-taint"))
		requireStatus(t, rec, 400)
	})

	t.Run("taint exists but no grant", func(t *testing.T) {
		_, ms, h := newTestServer()
		seedTaint(ms, 1, "acme-production", "gd-owner-team")
		rec := doJSON(t, h, "POST", "/v1/beads", baseScrubRequest("acme-production"))
		requireStatus(t, rec, 400)
	})

	t.Run("grant for a different taint does not count", func(t *testing.T) {
		_, ms, h := newTestServer()
		seedTaint(ms, 1, "acme-production", "gd-owner-team")
		grantScrubTaint(ms, 2, "other-taint", defaultTestSubject, 5)
		rec := doJSON(t, h, "POST", "/v1/beads", baseScrubRequest("acme-production"))
		requireStatus(t, rec, 400)
	})
}

// TestScrubRequestPurgeRejectsUnreadableBead is acceptance criterion 4: a
// purge-kind request whose scope includes a bead the requester cannot read
// is rejected at create — "you cannot destroy what you cannot see". The
// unreadable bead here ALSO carries the named taint (acme-production, id 1)
// so it clears the bead_ids taint-membership filter (finding 2) and the test
// exercises the readability check specifically; its second taint
// (other-taint, id 2 — ungranted) is what makes it unreadable. A
// detaint-kind request naming the same scope is unaffected, since lifting a
// marker does not require read access to the bead's content.
func TestScrubRequestPurgeRejectsUnreadableBead(t *testing.T) {
	_, ms, h := newTestServerAuth()
	grantScrubTaint(ms, 1, "acme-production", "alice@example.com", 7)
	seedTaint(ms, 2, "other-taint", "gd-owner-team")
	ms.beads["gd-secret"] = &model.Bead{ID: "gd-secret", Kind: model.KindIssue, Type: model.TypeTask, Title: "secret", Status: model.StatusOpen}
	seedBeadTaints(ms, "gd-secret", 1, 2)

	body := baseScrubRequest("acme-production")
	body["fields"].(map[string]any)["kind"] = "purge"
	body["fields"].(map[string]any)["scope"] = map[string]any{"bead_ids": []string{"gd-secret"}}
	rec := doJSONAs(t, h, "POST", "/v1/beads", body, "alice@example.com")
	requireStatus(t, rec, 400)

	body["fields"].(map[string]any)["kind"] = "detaint"
	rec = doJSONAs(t, h, "POST", "/v1/beads", body, "alice@example.com")
	requireStatus(t, rec, 201)
}

// TestScrubRequestScopeBeadIDs_RejectsUntaintedBead is PR #66 send-back
// finding 2: the explicit scope.bead_ids form was deduped and frozen into
// resolved_bead_ids as-is, with no taint-membership check at all — unlike
// the older_than_days/labels_any predicate form, which is correctly filtered
// through GetTaintsForBeads. A purge request could therefore freeze a blast
// radius containing beads that never carried the named taint, and
// p2-scrub-execute is documented as trusting resolved_bead_ids without
// re-checking. bead_ids naming one tainted and one untainted bead must be
// rejected outright, naming the untainted offender.
func TestScrubRequestScopeBeadIDs_RejectsUntaintedBead(t *testing.T) {
	_, ms, h := newTestServer()
	grantScrubTaint(ms, 1, "acme-production", defaultTestSubject, 5)
	seedBeadTaints(ms, "gd-tainted", 1)
	// "gd-untainted" carries no taint at all.

	body := baseScrubRequest("acme-production")
	body["fields"].(map[string]any)["scope"] = map[string]any{"bead_ids": []string{"gd-tainted", "gd-untainted"}}
	rec := doJSON(t, h, "POST", "/v1/beads", body)
	requireStatus(t, rec, 400)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "gd-untainted") {
		t.Errorf("error = %q, want it to name the untainted bead gd-untainted", errMsg)
	}
	if strings.Contains(errMsg, "gd-tainted,") || strings.HasSuffix(errMsg, "gd-tainted") {
		t.Errorf("error = %q, must not also name the tainted bead gd-tainted", errMsg)
	}
}

// TestScrubRequestResolvedBeadIDsFrozen is acceptance criterion 5:
// resolved_bead_ids does not change if the underlying scope predicate would
// later match different beads.
func TestScrubRequestResolvedBeadIDsFrozen(t *testing.T) {
	_, ms, h := newTestServer()
	grantScrubTaint(ms, 1, "acme-production", defaultTestSubject, 5)

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	ms.beads["gd-old-a"] = &model.Bead{ID: "gd-old-a", Kind: model.KindIssue, Type: model.TypeTask, Title: "old a", Status: model.StatusOpen, CreatedAt: old}
	ms.labels["gd-old-a"] = []string{"project:gardenio"}
	seedBeadTaints(ms, "gd-old-a", 1)

	body := baseScrubRequest("acme-production")
	body["fields"].(map[string]any)["kind"] = "detaint"
	body["fields"].(map[string]any)["scope"] = map[string]any{
		"older_than_days": 30,
		"labels_any":      []string{"project:gardenio"},
	}
	rec := doJSON(t, h, "POST", "/v1/beads", body)
	requireStatus(t, rec, 201)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	id, _ := resp["id"].(string)
	fields, _ := resp["fields"].(map[string]any)
	ids, _ := fields["resolved_bead_ids"].([]any)
	if len(ids) != 1 || ids[0] != "gd-old-a" {
		t.Fatalf("expected resolved_bead_ids=[gd-old-a], got %v", fields["resolved_bead_ids"])
	}

	// A second bead now also matches the same predicate...
	ms.beads["gd-old-b"] = &model.Bead{ID: "gd-old-b", Kind: model.KindIssue, Type: model.TypeTask, Title: "old b", Status: model.StatusOpen, CreatedAt: old}
	ms.labels["gd-old-b"] = []string{"project:gardenio"}
	seedBeadTaints(ms, "gd-old-b", 1)

	// ...but the already-created request's resolved_bead_ids must be
	// unchanged: it was frozen once, at create.
	rec = doJSON(t, h, "GET", "/v1/beads/"+id, nil)
	requireStatus(t, rec, 200)
	decodeJSON(t, rec, &resp)
	fields, _ = resp["fields"].(map[string]any)
	ids, _ = fields["resolved_bead_ids"].([]any)
	if len(ids) != 1 || ids[0] != "gd-old-a" {
		t.Errorf("resolved_bead_ids must stay frozen, got %v", fields["resolved_bead_ids"])
	}
}

// TestScrubRequestForbiddenFieldsCoverLifecycle is a unit test over the
// guard's forbidden-set so the list can't silently drift.
func TestScrubRequestForbiddenFieldsCoverLifecycle(t *testing.T) {
	for _, want := range []string{
		"approval_status", "approved_by", "approved_at", "approval_mode",
		"approval_reason", "executed_at", "result", "artifact_sha",
		// attestation joined this set with gd-eRKKim4OJc: only
		// handleAttestScrubRequest (scrub_attestation.go) may write it, via
		// the same mergeBeadFields bypass approve/reject use.
		"attestation",
	} {
		if _, ok := scrubRequestCreateForbiddenFields[want]; !ok {
			t.Errorf("forbidden-at-create set missing lifecycle field %q", want)
		}
	}
	// Author fields, and detaint_mode/mode (which must not even be a known
	// field, let alone forbidden-but-declared), must not be in this set.
	for _, ok := range []string{"taint", "kind", "scope", "requested_by", "resolved_bead_ids", "detaint_mode", "mode"} {
		if _, bad := scrubRequestCreateForbiddenFields[ok]; bad {
			t.Errorf("field %q must not be create-forbidden", ok)
		}
	}
}

// TestScrubRequestUpdate_ImmutableFieldsRejected is PR #66 send-back finding
// 1: type:scrub-request is project_scoped, so the requester owns their own
// request bead and passes the ordinary ownership gate on PATCH exactly as on
// GET. Without an update-time guard, "born inert, frozen at create" is
// worthless — the requester could PATCH themselves straight to approved, or
// widen resolved_bead_ids, after the create-time grant/readability checks
// already ran. Both must be rejected outright, for the SAME caller that
// created the bead (project_scoped ownership would otherwise let exactly
// this caller through).
func TestScrubRequestUpdate_ImmutableFieldsRejected(t *testing.T) {
	_, ms, h := newTestServer()
	grantScrubTaint(ms, 1, "acme-production", defaultTestSubject, 5)

	rec := doJSON(t, h, "POST", "/v1/beads", baseScrubRequest("acme-production"))
	requireStatus(t, rec, 201)
	var created map[string]any
	decodeJSON(t, rec, &created)
	id, _ := created["id"].(string)

	t.Run("approval_status", func(t *testing.T) {
		rec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{
			"fields": map[string]any{"approval_status": "approved"},
		})
		requireStatus(t, rec, 400)
	})

	t.Run("resolved_bead_ids", func(t *testing.T) {
		rec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{
			"fields": map[string]any{"resolved_bead_ids": []string{"gd-other"}},
		})
		requireStatus(t, rec, 400)
	})

	// attestation (gd-eRKKim4OJc): plain PATCH must reject it exactly like
	// the approval/execution lifecycle fields — only handleAttestScrubRequest
	// (via mergeBeadFields) may write it. A self-authored/forged attestation
	// would defeat the drift check entirely.
	t.Run("attestation", func(t *testing.T) {
		rec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{
			"fields": map[string]any{"attestation": map[string]any{"agent_bead_id": "gd-forger"}},
		})
		requireStatus(t, rec, 400)
	})

	// The bead itself must be untouched by either rejected PATCH.
	getRec := doJSON(t, h, "GET", "/v1/beads/"+id, nil)
	requireStatus(t, getRec, 200)
	var resp map[string]any
	decodeJSON(t, getRec, &resp)
	fields, _ := resp["fields"].(map[string]any)
	if _, ok := fields["approval_status"]; ok {
		t.Errorf("approval_status = %v, want absent after rejected PATCH", fields["approval_status"])
	}
	ids, _ := fields["resolved_bead_ids"].([]any)
	if len(ids) != 1 || ids[0] != "gd-a" {
		t.Errorf("resolved_bead_ids = %v, want unchanged [gd-a] after rejected PATCH", fields["resolved_bead_ids"])
	}
}

// TestScrubRequestUpdate_UnrelatedFieldPatchAllowed verifies the update-path
// guard only blocks patches that actually touch an immutable or
// create-forbidden Fields-JSON key: a PATCH touching the bead's own title
// (outside the guarded Fields blob entirely) must still succeed. Every field
// this bead type declares is now either immutable or create-forbidden
// (attestation joined the latter with gd-eRKKim4OJc), so title is the
// remaining example of something the guard must not overreach and block.
func TestScrubRequestUpdate_UnrelatedFieldPatchAllowed(t *testing.T) {
	_, ms, h := newTestServer()
	grantScrubTaint(ms, 1, "acme-production", defaultTestSubject, 5)

	rec := doJSON(t, h, "POST", "/v1/beads", baseScrubRequest("acme-production"))
	requireStatus(t, rec, 201)
	var created map[string]any
	decodeJSON(t, rec, &created)
	id, _ := created["id"].(string)

	rec = doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{
		"title": "scrub tainted export (renamed)",
	})
	requireStatus(t, rec, 200)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	if resp["title"] != "scrub tainted export (renamed)" {
		t.Errorf("title = %v, want %q applied from the PATCH", resp["title"], "scrub tainted export (renamed)")
	}
	fields, _ := resp["fields"].(map[string]any)
	ids, _ := fields["resolved_bead_ids"].([]any)
	if len(ids) != 1 || ids[0] != "gd-a" {
		t.Errorf("resolved_bead_ids = %v, want unchanged [gd-a] after an unrelated-field PATCH", fields["resolved_bead_ids"])
	}
}

// TestBuiltinScrubRequestFieldsDeclared guards the writer-vs-schema drift
// class: forbidden lifecycle fields must still be DECLARED (so a later
// privileged writer's PATCH doesn't 400 "unknown field"), while
// detaint_mode/mode must be absent entirely, and creation must not require
// an RBAC min_role (it is grant-gated, not role-gated) nor be escalatable
// (a create denial here must not be escalation-replayable — see config.go).
func TestBuiltinScrubRequestFieldsDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:scrub-request"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:scrub-request\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("unmarshal config value: %v", err)
	}
	have := make(map[string]struct{}, len(tc.Fields))
	for _, f := range tc.Fields {
		have[f.Name] = struct{}{}
	}
	wantFields := []string{
		"taint", "kind", "scope", "requested_by", "resolved_bead_ids", "attestation",
		"approval_status", "approved_by", "approved_at", "approval_mode", "approval_reason",
		"executed_at", "result", "artifact_sha",
	}
	for _, want := range wantFields {
		if _, ok := have[want]; !ok {
			t.Errorf("type:scrub-request schema missing field %q", want)
		}
	}
	for _, absent := range []string{"detaint_mode", "mode"} {
		if _, ok := have[absent]; ok {
			t.Errorf("type:scrub-request schema must NOT declare %q — the fork/strip choice lives on the taint's scrub_policy, not the request", absent)
		}
	}
	if tc.AllowAdditionalFields {
		t.Error("type:scrub-request must not allow_additional_fields — that would silently accept detaint_mode/mode")
	}
	byName := make(map[string]model.FieldDef, len(tc.Fields))
	for _, f := range tc.Fields {
		byName[f.Name] = f
	}
	for _, req := range []string{"taint", "kind", "scope"} {
		if !byName[req].Required {
			t.Errorf("field %q must be required", req)
		}
	}
	if tc.WritePolicy != nil && tc.WritePolicy.MinRole != "" {
		t.Errorf("scrub-request create must not gate on min_role (grant-gated instead), got min_role=%q", tc.WritePolicy.MinRole)
	}
	if tc.WritePolicy != nil && tc.WritePolicy.Escalatable {
		t.Error("scrub-request create must not be escalatable — a grant/read denial must not be escalation-replayable")
	}
}
