// Create-time guard for the low-privilege scrub-request bead (ADR
// docs/adr/taints.md §6.2, decision gd-Swd7mnGf3L).
//
// A scrub-request asks the taint system to detaint or purge a set of beads
// carrying a named taint. Like investigation-request, it must be provably
// INERT at birth — creating one does nothing until it clears the taint's
// approval policy (§6.3) and p2-scrub-execute runs it:
//
//   - Approval and execution fields cannot be set at create time. A requester
//     who could stamp approval_status/executed_at/result at create would skip
//     the approval gate entirely.
//   - requested_by is SERVER-STAMPED from the authenticated identity, never
//     honored from client input.
//   - resolved_bead_ids is computed by evaluating scope EXACTLY ONCE at
//     create and frozen — it never changes even if the underlying predicate
//     would later match a different set of beads. This is what makes a scrub
//     execution reproducible and auditable: the request names a fixed blast
//     radius the moment it is filed, not whatever happens to match when it is
//     later approved/executed.
//   - The requester must hold a grant for the named taint (ADR §5.1
//     taintsOf(bead) ⊆ grantsOf(caller), applied here to the TAINT itself
//     rather than a bead) — no grant, no request.
//   - A purge-kind request additionally requires the requester to be able to
//     READ every bead the resolved scope names — "you cannot destroy what
//     you cannot see" (§11). detaint carries no such requirement: detainting
//     does not destroy anything, it only lifts a marker.
//
// Deliberately absent: any detaint_mode / mode field on this bead type at
// all. The fork-vs-strip choice lives solely on the named taint's own
// scrub_policy.detaint_mode (set by the taint owner, never the requester) —
// see p2-scrub-execute (gd-fVAy32nWDf), which reads the mode from the taint,
// not from the request. A create body carrying either field name 400s as an
// unknown field via the generic schema validator (config.go's
// "type:scrub-request" declares no such field and does not set
// allow_additional_fields) — there is no bespoke guard code for this; it
// falls out of never having declared the field.
//
// attestation (ADR §6.4, gd-eRKKim4OJc) is declared by the schema (so the
// privileged attest endpoint's PATCH-equivalent write doesn't 400 as an
// unknown field) but is in scrubRequestCreateForbiddenFields below, same as
// the approval/execution lifecycle fields: a requester must not be able to
// forge their own attestation, and only handleAttestScrubRequest
// (scrub_attestation.go) may write it, via mergeBeadFields.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// scrubRequestScopeResolveCap bounds how many candidate beads the
// older_than_days/labels_any scope form may evaluate. resolved_bead_ids is
// frozen forever at create, so silently truncating a huge match would freeze
// an incomplete (and therefore wrong) blast radius rather than the one the
// requester described — for a destructive purge that is a correctness bug,
// not a convenience. Exceeding the cap is a hard reject asking the requester
// to narrow the scope (more labels_any, a tighter older_than_days) instead.
const scrubRequestScopeResolveCap = 2000

// scrubRequestCreateForbiddenFields are the lifecycle fields a requester must
// NOT be able to set at create time. Every one of them is written LATER by a
// privileged actor (the approval step, the attestation step, p2-scrub-execute)
// — allowing a requester to set any at create would let a self-authored
// request arrive already-approved, already-attested, or already-executed,
// bypassing the approval gate entirely. attestation joined this set with
// gd-eRKKim4OJc: it is now written only by the dedicated
// POST /v1/scrub-requests/{id}/attest endpoint (handleAttestScrubRequest,
// scrub_attestation.go), via the same mergeBeadFields privileged-writer
// bypass approve/reject use, never via plain create/PATCH — a client-supplied
// content_shas would make the drift check meaningless (see that file's doc
// comment).
var scrubRequestCreateForbiddenFields = map[string]struct{}{
	"approval_status": {},
	"approved_by":     {},
	"approved_at":     {},
	"approval_mode":   {},
	"approval_reason": {},
	"executed_at":     {},
	"result":          {},
	"artifact_sha":    {},
	"attestation":     {},
}

// scrubRequestScope is the parsed shape of the scope field (ADR §6.2): either
// an explicit bead id list, or an older_than_days/labels_any predicate. The
// two forms are mutually exclusive.
type scrubRequestScope struct {
	BeadIDs       []string `json:"bead_ids"`
	OlderThanDays *int     `json:"older_than_days"`
	LabelsAny     []string `json:"labels_any"`
}

// applyScrubRequestGuard enforces the born-inert invariant for a
// type=scrub-request bead being created: it rejects any approval/execution
// field, verifies the requester holds a grant for the named taint, resolves
// and freezes scope into resolved_bead_ids, rejects a purge whose resolved
// scope includes a bead the requester cannot read, and server-stamps
// requested_by. It mutates bead.Fields in place on success.
func (s *BeadsServer) applyScrubRequestGuard(ctx context.Context, bead *model.Bead) error {
	if bad := forbiddenScrubRequestFields(bead.Fields); len(bad) > 0 {
		return inputError(
			"scrub-request cannot be created with approval/execution fields set (" +
				strings.Join(bad, ", ") +
				"): a request is born un-approved and inert — it must clear the taint's approval policy before anything runs")
	}

	taintName, kind, rawScope, err := parseScrubRequestFields(bead.Fields)
	if err != nil {
		return err
	}

	taint, err := s.store.GetTaint(ctx, taintName)
	if errors.Is(err, sql.ErrNoRows) {
		return inputError(fmt.Sprintf("scrub-request: unknown taint %q", taintName))
	}
	if err != nil {
		return err
	}

	grants, err := s.grantsOf(ctx)
	if err != nil {
		return err
	}
	if !containsTaintID(grants, taint.ID) {
		return inputError(fmt.Sprintf("scrub-request: requester holds no grant for taint %q", taintName))
	}

	resolvedIDs, err := s.resolveScrubRequestScope(ctx, taint.ID, rawScope)
	if err != nil {
		return err
	}

	if kind == "purge" {
		for _, id := range resolvedIDs {
			readable, err := s.beadReadableByRequester(ctx, id)
			if err != nil {
				return err
			}
			if !readable {
				return inputError(fmt.Sprintf(
					"scrub-request: scope includes bead %s which the requester cannot read — you cannot destroy what you cannot see", id))
			}
		}
	}

	bead.Fields = setFieldValue(bead.Fields, "resolved_bead_ids", resolvedIDs)
	// requested_by must NEVER survive from client input (acceptance criterion
	// 2). An empty actor (auth disabled, anonymous caller) means there is no
	// authenticated identity to stamp — the key is deleted rather than left
	// holding whatever the client supplied, closing the gap where a
	// client-supplied value survived unstamped.
	actor := actorFromContext(ctx)
	if actor != "" {
		bead.Fields = setFieldValue(bead.Fields, "requested_by", actor)
	} else {
		bead.Fields = deleteFieldKey(bead.Fields, "requested_by")
	}

	s.logScrubRequestPolicyPreview(ctx, taint, kind, rawScope, actor)
	return nil
}

// parseScrubRequestFields extracts the author fields ValidateTypeFields has
// already confirmed are present (taint, kind required by schema) and decodes
// scope's raw JSON for resolveScrubRequestScope.
func parseScrubRequestFields(fields json.RawMessage) (taint, kind string, scope json.RawMessage, err error) {
	var f struct {
		Taint string          `json:"taint"`
		Kind  string          `json:"kind"`
		Scope json.RawMessage `json:"scope"`
	}
	if err := json.Unmarshal(fields, &f); err != nil {
		return "", "", nil, inputError("scrub-request: invalid fields: " + err.Error())
	}
	return f.Taint, f.Kind, f.Scope, nil
}

// resolveScrubRequestScope evaluates scope (either {"bead_ids":[...]} or
// {"older_than_days":N,"labels_any":[...]}) into a concrete, deduplicated
// bead id list. The older_than_days form is implicitly narrowed to beads
// carrying taintID — the request is scoped to a specific taint (ADR §6.2's
// worked story: "every production-tainted bead older than 30 days"), not to
// every old bead in the fleet.
func (s *BeadsServer) resolveScrubRequestScope(ctx context.Context, taintID int64, rawScope json.RawMessage) ([]string, error) {
	if len(bytes.TrimSpace(rawScope)) == 0 {
		return nil, inputError("scrub-request: scope is required")
	}
	var scope scrubRequestScope
	if err := json.Unmarshal(rawScope, &scope); err != nil {
		return nil, inputError("scrub-request: scope must be a JSON object: " + err.Error())
	}

	hasBeadIDs := len(scope.BeadIDs) > 0
	hasPredicate := scope.OlderThanDays != nil || len(scope.LabelsAny) > 0
	switch {
	case hasBeadIDs && hasPredicate:
		return nil, inputError("scrub-request: scope must be either bead_ids or older_than_days/labels_any, not both")
	case hasBeadIDs:
		return s.filterScrubRequestBeadIDsToTaint(ctx, taintID, dedupeStrings(scope.BeadIDs))
	case scope.OlderThanDays == nil:
		return nil, inputError("scrub-request: scope must set bead_ids or older_than_days")
	}

	if *scope.OlderThanDays < 0 {
		return nil, inputError("scrub-request: scope.older_than_days must be >= 0")
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -*scope.OlderThanDays)
	filter := model.BeadFilter{
		CreatedBefore: &cutoff,
		Fleet:         FleetFromContext(ctx),
		Sort:          "created_at",
		Limit:         scrubRequestScopeResolveCap + 1,
	}
	if len(scope.LabelsAny) > 0 {
		filter.LabelsAnyUser = scope.LabelsAny
	}
	candidates, _, err := s.store.ListBeads(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(candidates) > scrubRequestScopeResolveCap {
		return nil, inputError(fmt.Sprintf(
			"scrub-request: scope matches more than %d beads; narrow older_than_days or labels_any", scrubRequestScopeResolveCap))
	}

	ids := make([]string, len(candidates))
	for i, b := range candidates {
		ids[i] = b.ID
	}
	taintsByBead, err := s.store.GetTaintsForBeads(ctx, ids)
	if err != nil {
		return nil, err
	}
	resolved := []string{}
	for _, id := range ids {
		for _, t := range taintsByBead[id] {
			if t == taintID {
				resolved = append(resolved, id)
				break
			}
		}
	}
	return resolved, nil
}

// filterScrubRequestBeadIDsToTaint verifies every id in ids currently carries
// taintID, mirroring the taint-membership filter the older_than_days/labels_any
// predicate form already applies via GetTaintsForBeads above. Without this,
// the explicit bead_ids form was deduped and frozen into resolved_bead_ids
// as-is (no taint check at all) — a purge request could freeze a blast
// radius containing beads that never carried the named taint, and
// p2-scrub-execute is documented as trusting resolved_bead_ids without
// re-checking taint membership at execution time. Rejecting outright (rather
// than silently dropping the offending ids) is the more honest response for
// what may become a destructive purge: silently narrowing the scope would
// let the request succeed while quietly promising less than the requester
// described.
func (s *BeadsServer) filterScrubRequestBeadIDsToTaint(ctx context.Context, taintID int64, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return ids, nil
	}
	taintsByBead, err := s.store.GetTaintsForBeads(ctx, ids)
	if err != nil {
		return nil, err
	}
	var offending []string
	for _, id := range ids {
		tainted := false
		for _, t := range taintsByBead[id] {
			if t == taintID {
				tainted = true
				break
			}
		}
		if !tainted {
			offending = append(offending, id)
		}
	}
	if len(offending) > 0 {
		sort.Strings(offending)
		return nil, inputError(fmt.Sprintf(
			"scrub-request: scope.bead_ids includes beads that do not carry the named taint: %s",
			strings.Join(offending, ", ")))
	}
	return ids, nil
}

// beadReadableByRequester reports whether the current caller may read beadID
// under ALL of the read gates a normal fetch enforces: the project-scope
// gates (getBeadFiltered) AND the taint read gate (subresourceTaintDenied) —
// neither alone is a complete read-authorization check. A missing bead is
// "cannot read" (there is nothing to authorize a purge of).
func (s *BeadsServer) beadReadableByRequester(ctx context.Context, beadID string) (bool, error) {
	bead, err := s.getBeadFiltered(ctx, beadID)
	switch {
	case err == nil:
		// fall through to the taint gate below
	case errors.Is(err, errReadTokenDenied), errors.Is(err, errReadDenied):
		return false, nil
	case errors.Is(err, sql.ErrNoRows) || err.Error() == "bead not found":
		return false, nil
	default:
		return false, err
	}

	denied, err := s.subresourceTaintDenied(ctx, bead.ID)
	if err != nil {
		return false, err
	}
	return !denied, nil
}

// forbiddenScrubRequestFields returns the sorted set of create-forbidden
// lifecycle field keys present (with a non-null value) in the given fields
// JSON. A key set to JSON null is treated as absent.
func forbiddenScrubRequestFields(fields json.RawMessage) []string {
	if len(fields) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(fields, &m); err != nil {
		return nil
	}
	var bad []string
	for k, v := range m {
		if _, forbidden := scrubRequestCreateForbiddenFields[k]; !forbidden {
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

// setFieldValue returns the fields JSON with key set to an arbitrary JSON
// value (marshaled via encoding/json), preserving any existing keys. Unlike
// setField (string-only values), this accepts the []string resolved_bead_ids
// needs. A nil/empty/`null` input becomes a fresh object; a malformed
// (non-object) input is returned unchanged so downstream validation surfaces
// the right error.
func setFieldValue(fields json.RawMessage, key string, value any) json.RawMessage {
	m := make(map[string]any)
	trimmed := bytes.TrimSpace(fields)
	if len(trimmed) > 0 && string(trimmed) != "null" {
		if err := json.Unmarshal(trimmed, &m); err != nil || m == nil {
			return fields
		}
	}
	m[key] = value
	out, err := json.Marshal(m)
	if err != nil {
		return fields
	}
	return out
}

// deleteFieldKey returns the fields JSON with key removed, preserving any
// other existing keys. A nil/empty/`null`/malformed input is returned
// unchanged — there is nothing to delete from, and downstream validation is
// left to surface the right error for a malformed blob.
func deleteFieldKey(fields json.RawMessage, key string) json.RawMessage {
	trimmed := bytes.TrimSpace(fields)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return fields
	}
	m := make(map[string]any)
	if err := json.Unmarshal(trimmed, &m); err != nil || m == nil {
		return fields
	}
	delete(m, key)
	out, err := json.Marshal(m)
	if err != nil {
		return fields
	}
	return out
}

// dedupeStrings returns s with duplicate entries removed, preserving first-
// occurrence order.
func dedupeStrings(s []string) []string {
	seen := make(map[string]bool, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// scrubRequestImmutableFields are set once at create by applyScrubRequestGuard
// and never move again: taint/kind/scope pin down WHAT the request evaluates,
// requested_by is the server-stamped requester identity, and
// resolved_bead_ids is the frozen blast radius that makes a scrub execution
// reproducible and auditable. type:scrub-request is project_scoped, so the
// requester owns their own request bead and passes the ordinary ownership
// gate on PATCH exactly as on GET — without an update-time guard, the
// "born inert, frozen at create" guarantee applyScrubRequestGuard establishes
// is worthless: an owner could PATCH resolved_bead_ids, widen scope, or
// repoint taint/kind after the create-time grant/readability checks already
// ran (PR #66 send-back finding 1).
var scrubRequestImmutableFields = []string{
	"taint",
	"kind",
	"scope",
	"requested_by",
	"resolved_bead_ids",
}

// scrubRequestFieldChanged reports whether key differs between prior and
// current, treating "absent" and "present" as distinct (so introducing a
// previously-unset key counts as a change) but tolerant of an unchanged
// echoed value (a read-modify-write client that reads-then-writes the same
// value back).
func scrubRequestFieldChanged(prior, current map[string]any, key string) bool {
	pv, pok := prior[key]
	cv, cok := current[key]
	return pok != cok || !reflect.DeepEqual(pv, cv)
}

// checkScrubRequestUpdateGuard rejects any PATCH /v1/beads/{id} on a
// type=scrub-request bead that would change one of scrubRequestImmutableFields,
// or that would set any of scrubRequestCreateForbiddenFields at all. Both
// checks tolerate a patch that echoes back a field's current value unchanged,
// and both apply to EVERY caller — including the original requester, who
// owns their own project_scoped bead and would otherwise pass the ownership
// gate untouched.
//
// The nine create-forbidden lifecycle fields (approval_status, approved_by,
// approved_at, approval_mode, approval_reason, executed_at, result,
// artifact_sha, attestation) are rejected outright on update via this generic
// PATCH path. Two of them now have a privileged writer that bypasses this
// guard entirely (mergeBeadFields calls s.store.UpdateBead directly, never
// through checkScrubRequestUpdateGuard): the §6.3 approve path stamps
// approval_status/approved_by/approved_at/approval_mode/approval_reason, and
// handleAttestScrubRequest (scrub_attestation.go, gd-eRKKim4OJc) stamps
// attestation. p2-scrub-execute (gd-fVAy32nWDf) will need its own such
// carve-out for executed_at/result/artifact_sha — exactly the way
// authorizeScifApprovalStamp does for investigation-request
// (scif_approval_authz.go), NOT a relaxation of this blanket reject for
// ordinary callers.
//
// bead.Fields must already hold the POST-merge state (the caller's patch
// applied onto the prior stored fields, as updateBead computes it) and
// priorFields the bead's Fields blob as it was BEFORE this PATCH.
func (s *BeadsServer) checkScrubRequestUpdateGuard(bead *model.Bead, priorFields json.RawMessage) error {
	if bead.Type != model.TypeScrubRequest {
		return nil
	}
	var prior map[string]any
	if len(priorFields) > 0 {
		_ = json.Unmarshal(priorFields, &prior)
	}
	var current map[string]any
	if len(bead.Fields) > 0 {
		_ = json.Unmarshal(bead.Fields, &current)
	}

	var changedImmutable []string
	for _, k := range scrubRequestImmutableFields {
		if scrubRequestFieldChanged(prior, current, k) {
			changedImmutable = append(changedImmutable, k)
		}
	}
	if len(changedImmutable) > 0 {
		sort.Strings(changedImmutable)
		return inputError("scrub-request: " + strings.Join(changedImmutable, ", ") +
			" cannot be changed after create — resolved once at create time by applyScrubRequestGuard and frozen for the life of the request")
	}

	var changedForbidden []string
	for k := range scrubRequestCreateForbiddenFields {
		if scrubRequestFieldChanged(prior, current, k) {
			changedForbidden = append(changedForbidden, k)
		}
	}
	if len(changedForbidden) > 0 {
		sort.Strings(changedForbidden)
		return inputError("scrub-request: " + strings.Join(changedForbidden, ", ") +
			" cannot be set via update — the §6.3 approve path and p2-scrub-execute will add their own privileged relaxation for these fields, exactly as authorizeScifApprovalStamp does for investigation-request")
	}
	return nil
}

// containsTaintID reports whether id is present in ids.
func containsTaintID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
