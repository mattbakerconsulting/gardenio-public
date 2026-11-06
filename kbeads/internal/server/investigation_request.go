// Create-time guard for the low-privilege investigation-request bead
// (acme-scif v1.1 Phase B; plan-of-record ka-0000002842 §1.2/§7).
//
// The invariant the whole approval mechanism rests on is: "creating an
// investigation request does nothing until a human approves it." A fresh
// request must therefore be provably INERT and PENDING at birth. This guard
// enforces that at the createBead choke point so every creation path (gRPC,
// HTTP, formula pour, escalation replay) is covered:
//
//   - Approval, launch, and egress fields cannot be set at create time. A
//     requester who could stamp approved_by / run_id / beads_doc_id at create
//     would skip the human gate entirely — the exact fail-open this design
//     exists to prevent. Any such field present on create is a hard reject.
//   - approval_status may only be "pending" at create. An explicit
//     "approved"/"denied" is rejected loudly (not silently normalized), then
//     the field is set to "pending" so a request is never born in any other
//     state.
//   - requested_by is SERVER-STAMPED from the authenticated identity, never
//     honored from client input, so a request cannot forge who asked for it.
//
// WHO may later flip approval_status to "approved" (tenant-scoped approver
// groups, four-eyes approver != requester) is Phase D's contract (D3,
// ka-0000005982), enforced by the portal — NOT here. B1 only guarantees the
// request is born un-approved and inert.
//
// The complement of this file is scif_approval_authz.go: whether the PORTAL is
// allowed to write the approval fields at all (it is neither created_by nor the
// approver, so ownership denied every Gate-1 stamp until ka-0000001628). Note
// that requested_by is excluded there for the same reason it is server-stamped
// here — it is the four-eyes input and must not be writable by the actor that
// records the decision.
package server

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// investigationRequestCreateForbiddenFields are the lifecycle fields a
// requester must NOT be able to set at create time. Every one of them is
// written LATER by a privileged actor (the approval portal, the launcher, the
// egress worker); allowing a requester to set any at create would let a
// self-authored request arrive already-approved, already-launched, or with a
// forged result — bypassing the human gate. Kept sorted for a stable error.
var investigationRequestCreateForbiddenFields = map[string]struct{}{
	// Approval (portal, Phase D).
	"approved_by":       {},
	"approved_by_email": {},
	"approved_at":       {},
	"approval_method":   {},
	"denied_by":         {},
	"denied_at":         {},
	"denial_reason":     {},
	// Launch (launcher, Phase B2).
	"run_id":      {},
	"run_status":  {},
	"launched_at": {},
	"launched_by": {},
	// Egress (worker, Phase C1).
	"confidence":       {},
	"beads_doc_id":     {},
	"redacted_summary": {},
	// Release gate (Gate-2 signal, ka-0000002770). Stamped only once a finished
	// run is awaiting release / has been released — never at create.
	"release_status": {},
}

// applyInvestigationRequestGuard enforces the born-inert invariant for a
// type=investigation-request bead being created. It rejects any approval /
// launch / egress field, normalizes approval_status to "pending", and
// server-stamps requested_by from the authenticated identity (overriding any
// client-supplied value). It mutates bead.Fields in place on success.
//
// Returns inputError when the caller tried to set a forbidden lifecycle field;
// nil otherwise.
func (s *BeadsServer) applyInvestigationRequestGuard(ctx context.Context, bead *model.Bead) error {
	if bad := forbiddenInvestigationRequestFields(bead.Fields); len(bad) > 0 {
		return inputError(
			"investigation-request cannot be created with approval/launch/result fields set (" +
				strings.Join(bad, ", ") +
				"): a request is born un-approved and inert — a human must approve it through the portal before anything runs")
	}

	// approval_status may only ever be "pending" at create. An explicit
	// "approved"/"denied" is a bypass attempt and is rejected loudly rather
	// than silently normalized — a silent fix could mask a broken caller that
	// believes it approved a request when it did not.
	if st, ok := investigationRequestApprovalStatus(bead.Fields); ok && st != "" && st != "pending" {
		return inputError(
			"investigation-request cannot be created with approval_status=" + st +
				": a request is born pending — a human must approve it through the portal before anything runs")
	}

	// Normalize approval_status to pending and stamp requested_by from the
	// authenticated identity. Both writes are unconditional: pending is the
	// only legal birth state, and requested_by must reflect who actually
	// authenticated, never client input.
	bead.Fields = setField(bead.Fields, "approval_status", "pending")
	if actor := actorFromContext(ctx); actor != "" {
		bead.Fields = setField(bead.Fields, "requested_by", actor)
	}
	return nil
}

// investigationRequestApprovalStatus returns the string value of the
// approval_status field and whether it was present as a string. A null or
// non-string value returns ok=false.
func investigationRequestApprovalStatus(fields json.RawMessage) (string, bool) {
	if len(fields) == 0 {
		return "", false
	}
	var f struct {
		ApprovalStatus *string `json:"approval_status"`
	}
	if err := json.Unmarshal(fields, &f); err != nil || f.ApprovalStatus == nil {
		return "", false
	}
	return *f.ApprovalStatus, true
}

// forbiddenInvestigationRequestFields returns the sorted set of
// create-forbidden lifecycle field keys present (with a non-null value) in the
// given fields JSON. A key set to JSON null is treated as absent — a merge
// patch that clears a key is not an attempt to set it.
func forbiddenInvestigationRequestFields(fields json.RawMessage) []string {
	if len(fields) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(fields, &m); err != nil {
		// Malformed JSON is surfaced by ValidateTypeFields; not our concern.
		return nil
	}
	var bad []string
	for k, v := range m {
		if _, forbidden := investigationRequestCreateForbiddenFields[k]; !forbidden {
			continue
		}
		if isJSONNull(v) {
			continue
		}
		bad = append(bad, k)
	}
	sort.Strings(bad)
	return bad
}

// isJSONNull reports whether a raw JSON value is the literal null (ignoring
// surrounding whitespace).
func isJSONNull(v json.RawMessage) bool {
	return strings.TrimSpace(string(v)) == "null"
}
