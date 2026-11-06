// acme-scif Gate-1 approval-stamp carve-out (ka-0000001628).
//
// THE STRUCTURAL PROBLEM. A acme-scif Gate-1 decision has THREE parties, by
// design:
//
//	created_by  the in-cluster crew agent that filed the investigation-request
//	approver    the human who clicks approve/deny in the portal
//	writer      the PORTAL ITSELF (auth subject "bridge:acme-scif"), which is
//	            the only thing that actually stamps the decision onto the bead
//
// Four-eyes (D3) requires approver != requester and works correctly. But the
// writer is a THIRD identity that is neither created_by nor the approver, so
// authorizeOwnership correctly refused every single Gate-1 stamp:
//
//	HTTP 403: ownership denied: update on bead ka-0000005984 requires
//	created_by, assignee, owner, or admin role
//
// observed in prod 2026-08-26T21:24:46Z on the operator's first real Gate-1
// click. Because EVERY request bead is filed by a crew agent and stamped by the
// portal, this was not a one-off: it blocked every Gate-1 approval, forever.
// The design's own separation of duties is what tripped the guard.
//
// THE FIX, and why it is shaped like this. Same class as ka-0000004245 (the
// jira bridge could not stamp trigger fire-state on triggers it did not
// create), fixed by allowlisting exactly the keys the stamp helper writes
// (serviceManagedFieldsByType, beads.go). This file is that pattern, tightened
// one notch: a Gate-1 stamp carries real authority (it is the record that a
// human authorized a PHI-bearing run), so the bypass is scoped THREE ways —
// identity AND bead type AND field set — where the trigger allowlist is scoped
// by type and field set alone.
//
// WHAT IS DELIBERATELY NOT HERE:
//
//   - `requested_by` is NOT allowlisted. It is the INPUT to the four-eyes
//     check (decideRequestCore reads bead.Fields["requested_by"] into
//     scif.DecisionContext.RequestedBy). A portal that could rewrite it could
//     make requester == approver false by fiat and defeat the control it
//     exists to enforce. It is also server-stamped at create and never honored
//     from client input (applyInvestigationRequestGuard). Pinned by
//     TestScifPortal_RequestedByNeverAllowlisted and
//     TestGRPCUpdateBead_ScifPortalCannotTouchRequestedBy.
//   - No launch or egress fields (run_id, run_status, launched_at,
//     launched_by, confidence, beads_doc_id, redacted_summary). Those are
//     written by the launcher (B2) and the egress worker (C1) — different
//     actors, out of scope; the portal never writes them (it makes exactly ONE
//     bead write, the one below).
//   - No request content (prompt, data_scope, origin_bead, ...). A writer able
//     to repoint data_scope on a bead it does not own would widen what an
//     approved run may READ — privilege escalation, not bookkeeping.
//   - Nothing keys off X-Operated-By / BEADS_OPERATED_BY. Those are
//     CLIENT-supplied (applyOperatedByHeader → WithOperatedByHeader lands them
//     in a separate context slot) and must never gate authorization. This
//     keys only on the server-derived authenticated subject.
package server

import (
	"context"

	"gardenio/kbeads/internal/model"
)

// scifApprovalPortalSubjects are the SERVER-DERIVED authenticated subjects of
// the acme-scif approval portal — the only identities that may stamp Gate-1
// approval state on a request bead they did not create.
//
// The subject is set at fleet-token registration time
// (FleetConfig.RegisterTokenFull / bridgetokenrotator.deriveSubject: fleet
// "acme-scif" → "bridge:acme-scif") and read back out of the request context
// by AuthIdentityFromContext. It is NOT client-settable: a caller cannot name
// its own subject, only present a token the operator registered.
//
// Kept as a set rather than a single constant so a second portal deployment
// (a differently-named devel fleet) is a one-line addition rather than a
// refactor — but it stays a CLOSED list on purpose. Do not replace this with
// "any fleet token": a rogue or misconfigured fleet able to stamp
// approval_status=approved would forge the audit record of a human authorizing
// a PHI-bearing run.
var scifApprovalPortalSubjects = map[string]bool{
	"bridge:acme-scif": true,
}

// scifApprovalStampFields is EXACTLY the union of field keys the portal's
// Gate-1 stamp writes — nothing more.
//
// Source of truth: server.decideRequestCore in
// gardenio/controller/cmd/scif-portal/handlers_request.go, which builds a
// `fields` map and hands it to beadsapi.Client.UpdateBeadFieldsTyped. That is
// the portal's ONLY bead write of any kind.
//
//	approve branch → approval_status, approved_by, approved_by_email,
//	                 approved_at, approval_method, operated_by
//	deny branch    → approval_status, denied_by, denied_at, denial_reason
//
// KEEP THIS IN SYNC with that function. Like the trigger allowlist, the bypass
// is ALL-OR-NOTHING: one unlisted key in the patch sends the whole write to
// authorizeOwnership, which denies it — so adding a key to the stamp map
// without adding it here silently re-breaks every Gate-1 approval. That is
// exactly the ka-0000004245 failure mode, and it is why
// TestScifApprovalAllowlistMatchesPortalStamp parses the portal's source and
// fails on divergence instead of trusting this comment.
//
// On `operated_by`: this is the FIELD key, not the beads.operated_by column
// (updateBead never promotes a field into that column — only createBead
// resolves it). It roots the human-attribution chain the portal deliberately
// records — approve → launch directive → meta.json → report bead — from the
// approver the portal itself authenticated. Allowlisting it grants the portal
// no ownership it did not already have: authorizeOwnership's operator bypass
// reads bead.OperatedBy, the column, which this patch cannot reach.
var scifApprovalStampFields = []string{
	// Both branches.
	"approval_status",
	// approve branch.
	"approved_by",
	"approved_by_email",
	"approved_at",
	"approval_method",
	"operated_by",
	// deny branch.
	"denied_by",
	"denied_at",
	"denial_reason",
}

// scifReleaseStampFields is EXACTLY the union of field keys the portal's Gate-2
// reconcile SWEEP writes on a request bead it does not own — one key,
// release_status.
//
// Source of truth: gate2Sweeper.stamp in
// gardenio/controller/cmd/scif-portal/gate2_sweep.go, which calls
// UpdateBeadFieldsTyped with map[string]any{"release_status": target}. That is
// the portal's ONLY bead write outside decideRequestCore, and
// TestScifReleaseAllowlistMatchesSweepStamp parses the sweep source and fails on
// divergence instead of trusting this comment (same drift guard as the Gate-1
// stamp; the bypass is all-or-nothing, so a drifted key silently 403s the sweep
// and the release signal never appears).
//
// SCOPE ANALYSIS — why release_status is safe to allowlist (ka-0000002770),
// scoped the same THREE ways as the Gate-1 stamp (identity + type + field set):
//
//   - It is a DERIVED MIRROR of S3 gate state (a finished run's meta.json says
//     releasable and no approval.json marker yet => "pending"; the marker's
//     decision => "released"/"denied"), written purely so a waiting gate is
//     bead-visible and the slack-bridge can alert. The sweep reconciles it from
//     S3 every tick and never trusts a client value.
//   - NOTHING in any authorization or egress path READS it. The egress worker
//     gates on the S3 marker (decision=approved AND gardenio_eligible=true), and
//     four-eyes reads requested_by — never this field. A portal able to forge
//     release_status=released therefore releases NOTHING; it is bookkeeping, not
//     authority, the same class as the trigger fire-state allowlist. Its only
//     reader is bridge/scif_gate_alert.go, which merely posts a pointer alert.
//   - It is server-stamped only (born-inert: forbidden at create by
//     investigationRequestCreateForbiddenFields) and enum-constrained to
//     {pending, released, denied} by the type schema — a forged value cannot
//     even be a free string.
//
// Kept SEPARATE from scifApprovalStampFields rather than merged so each write
// site keeps its own single-source-of-truth drift guard: the Gate-1 list is
// pinned to decideRequestCore, this list to the sweep. The ownership bypass
// authorizes the UNION (scifPortalStampFields), because both sets are
// independently-proven-safe bookkeeping on the same bead type by the same
// closed portal subject.
var scifReleaseStampFields = []string{
	"release_status",
}

// scifPortalStampFields is the full set of request-bead field keys the portal
// may stamp on a bead it does not own — the union of the Gate-1 approval stamp
// and the Gate-2 release stamp. This is what the ownership bypass authorizes.
func scifPortalStampFields() []string {
	out := make([]string, 0, len(scifApprovalStampFields)+len(scifReleaseStampFields))
	out = append(out, scifApprovalStampFields...)
	out = append(out, scifReleaseStampFields...)
	return out
}

// callerIsScifApprovalPortal reports whether the authenticated caller is the
// acme-scif approval portal's service identity.
//
// It reads ONLY AuthIdentityFromContext — the identity the server derived when
// it validated the presented token. It deliberately ignores the operated-by
// context slot (client-supplied X-Operated-By), FleetFromContext, and any bead
// field: none of those are trustworthy inputs to an authorization decision.
//
// Requiring Type "fleet" is belt-and-braces: a "bridge:*" subject can only
// come from a registered fleet token, and pinning the type means an
// OIDC/edge/static identity that somehow presented that subject string still
// falls through to the ordinary gate.
func callerIsScifApprovalPortal(ctx context.Context) bool {
	id := AuthIdentityFromContext(ctx)
	if id.Type != "fleet" || id.Subject == "" {
		return false
	}
	// A per-agent token is an agent, whatever subject it was minted with: an
	// agent can never be the approval portal (belt and braces behind the mint's
	// subject validation).
	if isAgentFleet(FleetFromContext(ctx)) {
		return false
	}
	return scifApprovalPortalSubjects[id.Subject]
}

// authorizeScifApprovalStamp reports whether this update is the acme-scif
// portal stamping GATE state onto an investigation-request, and may therefore
// skip the ownership gate. It covers both portal writes on a request bead it
// does not own: the Gate-1 approval stamp (decideRequestCore) and the Gate-2
// release stamp (the reconcile sweep) — the union scifPortalStampFields.
//
// All three conditions must hold — identity, bead type, field set:
//
//  1. the caller is the portal's authenticated service identity;
//  2. the target is a type=investigation-request bead; and
//  3. the patch changes NOTHING outside scifPortalStampFields (reusing
//     patchOnlyChangesFields, so column writes are disqualified and echoed
//     unchanged keys are tolerated exactly as on the trigger path).
//
// This relaxes ONLY ownership. authorizeWriteTitled has already run upstream, so the
// type/project write policy (investigation-request is project_scoped) and the
// token's type/op scopes still apply, and every other bead type — including
// every other bead the portal might reach — is completely untouched.
func authorizeScifApprovalStamp(ctx context.Context, bead *model.Bead, in *updateBeadInput) bool {
	if bead == nil || bead.Type != model.TypeInvestigationRequest {
		return false
	}
	if !callerIsScifApprovalPortal(ctx) {
		return false
	}
	return patchOnlyChangesFields(scifPortalStampFields(), in, bead.Fields)
}
