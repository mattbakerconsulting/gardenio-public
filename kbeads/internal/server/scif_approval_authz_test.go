package server

import (
	"encoding/json"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
)

// Tests for the acme-scif Gate-1 approval-stamp carve-out (ka-0000001628).
//
// The prod failure: the portal ("bridge:acme-scif") is a THIRD identity — not
// the crew agent that filed the request, not the human approver — so
// authorizeOwnership denied every Gate-1 stamp with
//
//	HTTP 403: ownership denied: update on bead ka-0000005984 requires
//	created_by, assignee, owner, or admin role
//
// The POSITIVE test below reproduces the exact prod patch. Everything after it
// is a negative: the fix has to prove what it does NOT admit, because the
// previous instance of this class (ka-0000004245) shipped on a happy path.

const scifPortalSubject = "bridge:acme-scif"

// requestExistingFields is a born-inert investigation-request as
// applyInvestigationRequestGuard leaves it: pending, with requested_by
// server-stamped to the CREW AGENT that filed it — never the portal, never the
// approver. This string is the four-eyes input.
const requestExistingFields = `{"prompt":"why did claim 8812 deny?","data_scope":"{\"tenant\":\"t1\",\"db\":\"acme\",\"tables\":[\"claims\"]}","requested_by":"agent:acme-scif-crew-7","requesting_agent":"gd-agent7","approval_status":"pending"}`

// requestBead returns an investigation-request bead created by someone OTHER
// than the portal — the structural situation: every request bead is filed by a
// crew agent and stamped by the portal.
func requestBead(id string) *model.Bead {
	return &model.Bead{
		ID:        id,
		Title:     "investigation request",
		Type:      model.TypeInvestigationRequest,
		Kind:      model.KindData,
		Status:    model.StatusOpen,
		CreatedBy: "agent:acme-scif-crew-7",
		Labels:    []string{"project:acme-scif"},
		Fields:    json.RawMessage(requestExistingFields),
	}
}

// portalApprovePatch is the literal approve patch from decideRequestCore.
func portalApprovePatch() []byte {
	b, _ := json.Marshal(map[string]any{
		"approval_status":   "approved",
		"approved_by":       "matthew.baker@acme.example",
		"approved_by_email": "matthew.baker@acme.example",
		"approved_at":       "2026-08-26T21:24:46Z",
		"approval_method":   "web_ui",
		"operated_by":       "oidc:matthew.baker@acme.example",
	})
	return b
}

// TestGRPCUpdateBead_ScifPortalStampsApprovalOnForeignRequest is the
// ka-0000001628 regression, in the OUTAGE shape: the portal's own approve
// patch, on a request bead created_by a crew agent, from the portal's
// authenticated fleet subject. This 403'd in prod on the operator's first real
// Gate-1 click (2026-08-26T21:24:46Z).
func TestGRPCUpdateBead_ScifPortalStampsApprovalOnForeignRequest(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["ka-0000005984"] = requestBead("ka-0000005984")

	ctx := fleetCtx(scifPortalSubject)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "ka-0000005984",
		Fields: portalApprovePatch(),
	}); err != nil {
		t.Fatalf("portal approve stamp must be allowed, got: %v", err)
	}

	var stored map[string]any
	if err := json.Unmarshal(ms.beads["ka-0000005984"].Fields, &stored); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	if stored["approval_status"] != "approved" {
		t.Errorf("approval_status not persisted: got %v", stored["approval_status"])
	}
	if stored["approved_by"] != "matthew.baker@acme.example" {
		t.Errorf("approved_by not persisted: got %v", stored["approved_by"])
	}
	// The four-eyes input must be exactly as the server stamped it at create.
	if stored["requested_by"] != "agent:acme-scif-crew-7" {
		t.Errorf("requested_by must be untouched by an approval stamp: got %v", stored["requested_by"])
	}
}

// TestGRPCUpdateBead_ScifPortalStampsDenialOnForeignRequest covers the deny
// branch of the same stamp helper (denied_by / denied_at / denial_reason),
// which hits the identical gate.
func TestGRPCUpdateBead_ScifPortalStampsDenialOnForeignRequest(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-req-deny"] = requestBead("gd-req-deny")

	ctx := fleetCtx(scifPortalSubject)
	fields, _ := json.Marshal(map[string]any{
		"approval_status": "denied",
		"denied_by":       "matthew.baker@acme.example",
		"denied_at":       "2026-08-26T21:30:00Z",
		"denial_reason":   "scope too broad",
	})
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-req-deny",
		Fields: fields,
	}); err != nil {
		t.Fatalf("portal deny stamp must be allowed, got: %v", err)
	}
}

// TestGRPCUpdateBead_ScifPortalStampsReleaseStatusOnForeignRequest covers the
// Gate-2 reconcile sweep's release stamp (ka-0000002770): the portal stamps
// release_status onto a request bead it does not own, via the same ownership
// bypass as the Gate-1 approval stamp. Without the allowlist this is the write
// that would 403 and silently swallow the "awaiting release" signal.
func TestGRPCUpdateBead_ScifPortalStampsReleaseStatusOnForeignRequest(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-req-release"] = requestBead("gd-req-release")

	ctx := fleetCtx(scifPortalSubject)
	fields, _ := json.Marshal(map[string]any{"release_status": "pending"})
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-req-release",
		Fields: fields,
	}); err != nil {
		t.Fatalf("portal release stamp must be allowed, got: %v", err)
	}

	var stored map[string]any
	if err := json.Unmarshal(ms.beads["gd-req-release"].Fields, &stored); err != nil {
		t.Fatalf("unmarshal stored fields: %v", err)
	}
	if stored["release_status"] != "pending" {
		t.Errorf("release_status not persisted: got %v", stored["release_status"])
	}
	// The four-eyes input is still untouched by a Gate-2 stamp.
	if stored["requested_by"] != "agent:acme-scif-crew-7" {
		t.Errorf("requested_by must be untouched by a release stamp: got %v", stored["requested_by"])
	}
}

// TestGRPCUpdateBead_NonPortalCannotStampReleaseStatus proves the bypass is
// still scoped to the portal identity: an ordinary fleet caller that does not
// own the request bead is denied, exactly as before this change.
func TestGRPCUpdateBead_NonPortalCannotStampReleaseStatus(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beads["gd-req-release2"] = requestBead("gd-req-release2")

	ctx := fleetCtx("bridge:some-other-fleet")
	fields, _ := json.Marshal(map[string]any{"release_status": "pending"})
	_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     "gd-req-release2",
		Fields: fields,
	})
	if err == nil {
		t.Fatal("a non-portal fleet caller must not stamp release_status on a bead it does not own")
	}
	requireCode(t, err, codes.PermissionDenied)
}

// TestScifApprovalStamp_NeedsNoAssigneeWorkaround pins that the fix makes the
// per-bead workaround (assignee=bridge:acme-scif, applied to ka-0000005984 to
// unblock the operator) unnecessary: the SAME stamp succeeds on a bead with an
// empty assignee. If this ever regresses, reverting the workaround on a live
// request bead would re-break its approval.
func TestScifApprovalStamp_NeedsNoAssigneeWorkaround(t *testing.T) {
	b := requestBead("gd-req-noassignee")
	if b.Assignee != "" {
		t.Fatalf("fixture must have no assignee, got %q", b.Assignee)
	}
	in := &updateBeadInput{Fields: portalApprovePatch()}
	if !authorizeScifApprovalStamp(fleetCtx(scifPortalSubject), b, in) {
		t.Fatal("approval stamp must be authorized without an assignee workaround")
	}
}

// TestGRPCUpdateBead_ScifPortalCannotTouchRequestedBy is THE constraint of
// this change, and the test that fails if someone adds requested_by to
// scifApprovalStampFields.
//
// requested_by is the INPUT to four-eyes: decideRequestCore reads
// bead.Fields["requested_by"] into scif.DecisionContext.RequestedBy and the
// authorizer refuses approver == requester. A portal able to rewrite it could
// point requested_by at anyone and make the comparison pass by fiat — it would
// defeat the control the portal exists to enforce. It is also server-stamped
// at create and never honored from client input.
//
// Both shapes are proven: requested_by ALONE, and requested_by smuggled into
// an otherwise-legitimate approve patch (the realistic attack — the whole
// patch must be refused, not silently filtered).
func TestGRPCUpdateBead_ScifPortalCannotTouchRequestedBy(t *testing.T) {
	cases := map[string]map[string]any{
		"alone": {
			"requested_by": "matthew.baker@acme.example",
		},
		"smuggled into a valid approve patch": {
			"approval_status":   "approved",
			"approved_by":       "matthew.baker@acme.example",
			"approved_by_email": "matthew.baker@acme.example",
			"approved_at":       "2026-08-26T21:24:46Z",
			"approval_method":   "web_ui",
			"operated_by":       "oidc:matthew.baker@acme.example",
			"requested_by":      "matthew.baker@acme.example",
		},
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			srv, ms, _ := newTestServer()
			ms.beads["gd-req-fe"] = requestBead("gd-req-fe")
			ctx := fleetCtx(scifPortalSubject)
			fields, _ := json.Marshal(patch)
			_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
				Id:     "gd-req-fe",
				Fields: fields,
			})
			requireCode(t, err, codes.PermissionDenied)

			// And the stored value is unchanged, not partially applied.
			var stored map[string]any
			if uErr := json.Unmarshal(ms.beads["gd-req-fe"].Fields, &stored); uErr != nil {
				t.Fatalf("unmarshal stored fields: %v", uErr)
			}
			if stored["requested_by"] != "agent:acme-scif-crew-7" {
				t.Fatalf("requested_by was mutated: got %v", stored["requested_by"])
			}
		})
	}
}

// TestScifPortal_RequestedByNeverAllowlisted is the cheap, direct assertion of
// the same rule against the allowlist itself, so the intent survives even if
// someone rewrites the transport-level tests above.
func TestScifPortal_RequestedByNeverAllowlisted(t *testing.T) {
	for _, k := range scifApprovalStampFields {
		if k == "requested_by" {
			t.Fatal("requested_by must NEVER be in scifApprovalStampFields: it is the " +
				"INPUT to the four-eyes check (approver != requester). A portal that can " +
				"rewrite it defeats the control it exists to enforce. See ka-0000001628.")
		}
	}
}

// TestGRPCUpdateBead_ScifPortalCannotTouchOtherFieldClasses is the
// NON-WIDENING proof. Admitting the approval-state keys must not let the portal
// touch anything else on a request bead it does not own. Each key carries real
// authority:
//
//	prompt / context           — rewrites what the investigation asks
//	data_scope                 — repoints which PHI tables an approved run READS
//	origin_bead / requesting_agent — forges provenance
//	run_id / run_status / launched_at / launched_by — forges the launch record (B2's job)
//	confidence / beads_doc_id / redacted_summary   — forges the result (C1's job)
//	idempotency_key            — replay control
//
// Each is submitted ALONE, the strongest form: if a lone key were allowed, a
// mixed patch certainly would be.
func TestGRPCUpdateBead_ScifPortalCannotTouchOtherFieldClasses(t *testing.T) {
	for _, key := range []string{
		"prompt", "data_scope", "context", "origin_bead", "requesting_agent",
		"run_id", "run_status", "launched_at", "launched_by",
		"confidence", "beads_doc_id", "redacted_summary",
		"idempotency_key", "user_bead_id", "slack_user_id",
	} {
		t.Run(key, func(t *testing.T) {
			srv, ms, _ := newTestServer()
			ms.beads["gd-req-cfg"] = requestBead("gd-req-cfg")
			ctx := fleetCtx(scifPortalSubject)
			fields, _ := json.Marshal(map[string]any{key: "hijacked"})
			_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
				Id:     "gd-req-cfg",
				Fields: fields,
			})
			requireCode(t, err, codes.PermissionDenied)
		})
	}
}

// TestGRPCUpdateBead_ScifPortalCannotTouchColumns proves the carve-out is
// fields-only. A patch that also moves a non-fields column — title, status,
// assignee, owner, labels — is refused wholesale, so the portal cannot grant
// itself durable ownership (assignee/owner) or retitle someone's request while
// stamping a decision.
func TestGRPCUpdateBead_ScifPortalCannotTouchColumns(t *testing.T) {
	approved := "approved"
	title := "hijacked"
	closed := string(model.StatusClosed)
	portal := scifPortalSubject

	t.Run("title", func(t *testing.T) {
		srv, ms, _ := newTestServer()
		ms.beads["gd-req-col1"] = requestBead("gd-req-col1")
		fields, _ := json.Marshal(map[string]any{"approval_status": approved})
		_, err := srv.UpdateBead(fleetCtx(portal), &beadsv1.UpdateBeadRequest{
			Id: "gd-req-col1", Title: &title, Fields: fields,
		})
		requireCode(t, err, codes.PermissionDenied)
	})

	t.Run("assignee", func(t *testing.T) {
		srv, ms, _ := newTestServer()
		ms.beads["gd-req-col2"] = requestBead("gd-req-col2")
		fields, _ := json.Marshal(map[string]any{"approval_status": approved})
		_, err := srv.UpdateBead(fleetCtx(portal), &beadsv1.UpdateBeadRequest{
			Id: "gd-req-col2", Assignee: &portal, Fields: fields,
		})
		requireCode(t, err, codes.PermissionDenied)
	})

	t.Run("status", func(t *testing.T) {
		srv, ms, _ := newTestServer()
		ms.beads["gd-req-col3"] = requestBead("gd-req-col3")
		fields, _ := json.Marshal(map[string]any{"approval_status": approved})
		_, err := srv.UpdateBead(fleetCtx(portal), &beadsv1.UpdateBeadRequest{
			Id: "gd-req-col3", Status: &closed, Fields: fields,
		})
		requireCode(t, err, codes.PermissionDenied)
	})
}

// TestGRPCUpdateBead_ScifApprovalKeysAreRequestScoped proves the new keys did
// not leak to any other bead type. The portal patching approval state on a
// task / trigger / agent bead it does not own stays denied: the carve-out is
// pinned to type=investigation-request.
func TestGRPCUpdateBead_ScifApprovalKeysAreRequestScoped(t *testing.T) {
	for _, key := range scifApprovalStampFields {
		t.Run("task/"+key, func(t *testing.T) {
			srv, ms, _ := newTestServer()
			ms.beads["gd-task-scif"] = ownedBead("gd-task-scif", "someone-else")
			fields, _ := json.Marshal(map[string]any{key: "x"})
			_, err := srv.UpdateBead(fleetCtx(scifPortalSubject), &beadsv1.UpdateBeadRequest{
				Id: "gd-task-scif", Fields: fields,
			})
			requireCode(t, err, codes.PermissionDenied)
		})
		t.Run("trigger/"+key, func(t *testing.T) {
			srv, ms, _ := newTestServer()
			ms.beads["gd-trig-scif"] = triggerBead("gd-trig-scif", "someone-else")
			fields, _ := json.Marshal(map[string]any{key: "x"})
			_, err := srv.UpdateBead(fleetCtx(scifPortalSubject), &beadsv1.UpdateBeadRequest{
				Id: "gd-trig-scif", Fields: fields,
			})
			requireCode(t, err, codes.PermissionDenied)
		})
	}
}

// TestGRPCUpdateBead_ScifApprovalRejectsOtherFleets is the identity half of the
// narrowness proof. The carve-out keys on the SERVER-DERIVED authenticated
// subject, so no other fleet — including the sibling bridges and plausible
// look-alikes ("bridge:acme-scif-portal", the typo'd "bridge:fits-scif", the
// bare fleet name) — can forge the record of a human authorizing a PHI-bearing
// run.
//
// The empty subject is deliberately NOT in this table: authorizeOwnership has
// always short-circuited an identity with no Subject ("let outer middleware
// decide"), long predating this change. Asserting a denial there would be
// testing — and inviting a rewrite of — the pre-existing guard rather than this
// carve-out. callerIsScifApprovalPortal still refuses an empty subject on its
// own (it can never be a key in scifApprovalPortalSubjects), so this carve-out
// adds nothing to that path.
func TestGRPCUpdateBead_ScifApprovalRejectsOtherFleets(t *testing.T) {
	for _, subject := range []string{
		"bridge:jira", "bridge:slack", "bridge:gitlab",
		"bridge:acme-scif-portal", "bridge:fits-scif", "acme-scif",
	} {
		t.Run("subject="+subject, func(t *testing.T) {
			srv, ms, _ := newTestServer()
			ms.beads["gd-req-other"] = requestBead("gd-req-other")
			_, err := srv.UpdateBead(fleetCtx(subject), &beadsv1.UpdateBeadRequest{
				Id: "gd-req-other", Fields: portalApprovePatch(),
			})
			requireCode(t, err, codes.PermissionDenied)
		})
	}
}

// TestScifApprovalStamp_IgnoresClientSuppliedOperatedBy pins that the
// carve-out reads ONLY the server-derived auth identity. The client-supplied
// X-Operated-By header (applyOperatedByHeader → WithOperatedByHeader, driven
// by the BEADS_OPERATED_BY client env var) lands in a SEPARATE context slot;
// setting it to the portal's subject must not buy any caller the bypass.
func TestScifApprovalStamp_IgnoresClientSuppliedOperatedBy(t *testing.T) {
	ctx := WithOperatedByHeader(fleetCtx("bridge:jira"), scifPortalSubject)
	in := &updateBeadInput{Fields: portalApprovePatch()}
	if authorizeScifApprovalStamp(ctx, requestBead("gd-req-hdr"), in) {
		t.Fatal("a client-supplied X-Operated-By must never grant the portal carve-out")
	}
}

// TestScifApprovalStamp_NoOpPatchIsNotABypass — a patch that changes nothing
// (every key already at its target value, as a read-modify-write client echoes
// back) must not count as an authorized stamp. Mirrors the net-change rule the
// trigger path already relies on.
func TestScifApprovalStamp_NoOpPatchIsNotABypass(t *testing.T) {
	b := requestBead("gd-req-noop")
	in := &updateBeadInput{Fields: json.RawMessage(`{"approval_status":"pending"}`)}
	if authorizeScifApprovalStamp(fleetCtx(scifPortalSubject), b, in) {
		t.Fatal("a net-zero patch must not be treated as an approval stamp")
	}
}

// TestScifApprovalStamp_TolerantOfEchoedFields — the portal's client
// (beadsapi.Client.UpdateBeadFieldsTyped) does a read-modify-write; a future
// change that echoes untouched user-authored keys back must still stamp, since
// unchanged keys are not a net change. This is the property that made the
// trigger allowlist work in practice, restated for this path.
func TestScifApprovalStamp_TolerantOfEchoedFields(t *testing.T) {
	b := requestBead("gd-req-echo")
	patch := map[string]any{
		"prompt":            "why did claim 8812 deny?", // echoed, unchanged
		"requested_by":      "agent:acme-scif-crew-7",   // echoed, unchanged
		"approval_status":   "approved",
		"approved_by":       "matthew.baker@acme.example",
		"approved_by_email": "matthew.baker@acme.example",
		"approved_at":       "2026-08-26T21:24:46Z",
		"approval_method":   "web_ui",
		"operated_by":       "oidc:matthew.baker@acme.example",
	}
	raw, _ := json.Marshal(patch)
	if !authorizeScifApprovalStamp(fleetCtx(scifPortalSubject), b, &updateBeadInput{Fields: raw}) {
		t.Fatal("echoed-unchanged keys must not block the approval stamp")
	}
	// ...but echoing a CHANGED requested_by is still refused.
	patch["requested_by"] = "matthew.baker@acme.example"
	raw, _ = json.Marshal(patch)
	if authorizeScifApprovalStamp(fleetCtx(scifPortalSubject), b, &updateBeadInput{Fields: raw}) {
		t.Fatal("a CHANGED requested_by must refuse the stamp even among echoed keys")
	}
}

// TestScifApprovalStampFieldsAreSchemaDeclared closes the sibling drift class:
// an allowlisted key that the type:investigation-request schema does not
// declare would be ownership-allowed and then 400'd by ValidateTypeFields —
// a fix that trades a 403 for a 400.
func TestScifApprovalStampFieldsAreSchemaDeclared(t *testing.T) {
	cfg, ok := builtinConfigs["type:investigation-request"]
	if !ok {
		t.Fatal("builtin type:investigation-request config missing")
	}
	var parsed struct {
		Fields []struct{ Name string } `json:"fields"`
	}
	if err := json.Unmarshal(cfg.Value, &parsed); err != nil {
		t.Fatalf("unmarshal type:investigation-request: %v", err)
	}
	declared := map[string]bool{}
	for _, f := range parsed.Fields {
		declared[f.Name] = true
	}
	for _, k := range scifApprovalStampFields {
		if !declared[k] {
			t.Errorf("scifApprovalStampFields allows %q but type:investigation-request does not "+
				"declare it — the stamp would clear ownership and then 400 on the unknown field "+
				"(add it to config.go)", k)
		}
	}
}

// TestCallerIsScifApprovalPortal_EmptySubject pins the property the table
// above deliberately leaves to the pre-existing guard: an identity with no
// subject is never the portal.
func TestCallerIsScifApprovalPortal_EmptySubject(t *testing.T) {
	if callerIsScifApprovalPortal(fleetCtx("")) {
		t.Fatal("an empty auth subject must never be recognised as the portal")
	}
}
