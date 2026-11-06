package server

import (
	"context"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
)

// The short-title fill exception to the ownership gate (gd-JOOXA7UqLh).
//
// The short-title groomer (role short-title-groomer) names beads other crews
// filed and left without a short_title, so authorizeOwnership refused it on
// its per-agent token. An all-types ownership_grants row on the role would let
// it update and close any non-decision bead and makes the role privileged
// (privileged_role_grants_test.go). Instead, any agent may fill an
// EMPTY short_title on a bead it does not own, scoped five ways:
//
//   - identity: a per-agent token only (agentBeadIDFromContext). The primary
//     static token already bypasses ownership on non-decision beads and is
//     unchanged; machine, land, bridge and OIDC callers are not admitted;
//   - project: the token's project must be set and the bead must carry that
//     project label;
//   - type: never a decision;
//   - patch: short_title only, to a non-empty value. No other column, no
//     fields, no labels, and never a close or delete (those are separate
//     actions that do not reach this path);
//   - set once: the stored short_title must be empty. updateBead re-checks it
//     in the write itself (store.FillShortTitle), so a title an owner sets
//     between the read and the write is never overwritten.
//
// The write is audited like any update, but does not flip dibs: naming a bead
// is not authoring it.

// authorizeShortTitleFill reports whether this update is an agent filling the
// empty short_title of a bead in its project, and so may pass the ownership
// gate. updateBead consults it only after authorizeOwnership denied, and must
// then write through store.FillShortTitle rather than UpdateBead.
func authorizeShortTitleFill(ctx context.Context, bead *model.Bead, in *updateBeadInput) bool {
	if bead == nil || bead.Type == model.TypeDecision {
		return false
	}
	if _, ok := agentBeadIDFromContext(ctx); !ok {
		return false
	}
	project := TokenProjectFromContext(ctx)
	if project == "" || !slices.Contains(bead.Labels, "project:"+project) {
		return false
	}
	if !in.patchOnlySetsShortTitle() {
		return false
	}
	return strings.TrimSpace(bead.ShortTitle) == ""
}

// patchOnlySetsShortTitle reports whether this patch sets short_title to a
// non-empty value and touches nothing else. only_if_short_title_empty is a
// guard on the same write, not a change, so it may ride along.
func (in *updateBeadInput) patchOnlySetsShortTitle() bool {
	return in.ShortTitle != nil && strings.TrimSpace(*in.ShortTitle) != "" &&
		in.Title == nil && in.Heraldry == nil && in.Description == nil && in.Notes == nil &&
		in.Status == nil && in.Priority == nil && in.Assignee == nil && in.Owner == nil &&
		!in.dueAtSet && !in.deferUntilSet && in.Fields == nil && !in.labelsSet
}

// shortTitleFillLostRace is the denial for a fill whose bead gained a
// short_title after updateBead read it: the authored value wins, and the
// caller, which does not own the bead, may not replace it.
func shortTitleFillLostRace(bead *model.Bead) *AuthzDenial {
	return &AuthzDenial{
		Action:   "update",
		BeadType: string(bead.Type),
		Message: "ownership denied: update on bead " + bead.ID +
			" may only fill an empty short_title, and it was set by another writer before this one landed",
	}
}
