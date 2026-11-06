package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// authorizeWriteTaintGate enforces the write-side twin of the read gate ADR
// docs/adr/taints.md §9.4/§5.1 adds: a caller may not write to a bead that
// currently carries a taint it lacks a grant for, even once authorizeWriteTitled
// and authorizeOwnership have already allowed the write. It narrows; it
// never widens what those two checks already allow.
//
// Returns nil when the write is allowed: the gate is exempt (primary static
// token, or an empty taints registry), or the caller holds a grant for every
// taint the bead carries. On denial, returns a structured *AuthzDenial naming
// the missing taints and their owner teams (§9.4), routed through the same
// writeAuthzDenial / auditDenial / recordAuthzDenial pipeline as every other
// write denial — deriveAuthzDenialReason maps it to reasonTaint.
//
// A store error resolving the grant set or the bead's own taints fails
// CLOSED: the write is denied (with no taint names to show, since which
// taints are missing could not be determined), matching the read gate's
// fail-closed error handling.
func (s *BeadsServer) authorizeWriteTaintGate(ctx context.Context, action string, bead *model.Bead) *AuthzDenial {
	taintGate, err := s.resolveBeadTaintGateState(ctx)
	if err != nil {
		slog.Warn("taint write gate: failed to resolve caller grants, failing closed",
			"action", action, "bead_id", bead.ID, "error", err)
		return s.taintWriteDenial(ctx, action, bead, nil)
	}
	if taintGate.exempt {
		return nil
	}

	// Gate-effect taints only (docs/adr/taints.md §15.2): a taint whose
	// effects exclude "gate" (e.g. {} or {restrict}) must never block a
	// write.
	beadTaints, err := s.store.GetGateBeadTaints(ctx, bead.ID)
	if err != nil {
		slog.Warn("taint write gate: failed to fetch bead taints, failing closed",
			"action", action, "bead_id", bead.ID, "error", err)
		return s.taintWriteDenial(ctx, action, bead, nil)
	}
	if len(beadTaints) == 0 {
		return nil
	}

	granted := make(map[int64]bool, len(taintGate.grants))
	for _, g := range taintGate.grants {
		granted[g] = true
	}
	var missing []*model.BeadTaint
	for _, bt := range beadTaints {
		if !granted[bt.TaintID] {
			missing = append(missing, bt)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return s.taintWriteDenial(ctx, action, bead, missing)
}

// taintWriteDenial builds the structured *AuthzDenial for a taint write-gate
// rejection. Unlike every other write denial, it does NOT go through the
// generic populateAdvisoryText templating (defaultAdvisoryText/escalateHint):
// ADR docs/adr/taints.md §9.4 pins an exact advisory-text shape (naming the
// bead id, the taint(s), and the owner team(s), with a taint-specific
// --escalate hint) that the generic {action}/{type}/{reason}/{escalate_hint}
// template cannot reproduce, so buildTaintDenialAdvisoryText constructs it
// directly. Escalatable is always true here — X-Escalate on a taint denial
// files a `grant` escalation (handleEscalation) rather than replaying the
// original write. missing is nil when the gate failed closed on a store
// error — the caller cannot be shown which taints are actually missing in
// that case, only that the write was denied.
func (s *BeadsServer) taintWriteDenial(ctx context.Context, action string, bead *model.Bead, missing []*model.BeadTaint) *AuthzDenial {
	names := make([]string, 0, len(missing))
	ownerTeamSet := make(map[string]bool, len(missing))
	for _, bt := range missing {
		names = append(names, bt.TaintName)
		if t, err := s.store.GetTaint(ctx, bt.TaintName); err == nil && t != nil && t.OwnerTeam != "" {
			ownerTeamSet[t.OwnerTeam] = true
		}
	}
	ownerTeams := make([]string, 0, len(ownerTeamSet))
	for team := range ownerTeamSet {
		ownerTeams = append(ownerTeams, team)
	}

	reason := "this bead carries a taint you do not hold a grant for"
	if len(names) > 0 {
		reason = "this bead carries taint(s) [" + strings.Join(names, ", ") + "] you do not hold a grant for"
	}

	return &AuthzDenial{
		Action:          action,
		BeadType:        string(bead.Type),
		Escalatable:     true,
		Message:         "not authorized: " + reason,
		AdvisoryText:    buildTaintDenialAdvisoryText(action, bead.ID, names, ownerTeams),
		DeniedReason:    reasonTaint,
		TaintNames:      names,
		TaintOwnerTeams: ownerTeams,
	}
}

// buildTaintDenialAdvisoryText reproduces the exact ADR docs/adr/taints.md
// §9.4 advisory-text template:
//
//	Write denied: update on gd-abc carries taint "acme-production" that you do not hold.
//	Ask team acme-prod-owners for a grant, or retry with --escalate to file a grant request.
//
// names/teams may hold more than one entry when the bead carries multiple
// missing taints or those taints have distinct owner teams — the ADR only
// worked example is singular, so the plural phrasing here ("taints"/"teams",
// comma-joined) is this task's extrapolation, not a quoted spec shape. Empty
// names/teams (the store-error fail-closed path) fall back to a
// taint-agnostic phrasing that still ends in the same escalate hint.
func buildTaintDenialAdvisoryText(action, beadID string, names, teams []string) string {
	var taintClause string
	switch len(names) {
	case 0:
		taintClause = "carries a taint you do not hold"
	case 1:
		taintClause = fmt.Sprintf("carries taint %q that you do not hold", names[0])
	default:
		quoted := make([]string, len(names))
		for i, n := range names {
			quoted[i] = fmt.Sprintf("%q", n)
		}
		taintClause = fmt.Sprintf("carries taints %s that you do not hold", strings.Join(quoted, ", "))
	}

	var askClause string
	switch len(teams) {
	case 0:
		askClause = "Ask an admin for a grant"
	case 1:
		askClause = fmt.Sprintf("Ask team %s for a grant", teams[0])
	default:
		askClause = fmt.Sprintf("Ask teams %s for a grant", strings.Join(teams, ", "))
	}

	return fmt.Sprintf("Write denied: %s on %s %s.\n%s, or retry with --escalate to file a grant request.",
		action, beadID, taintClause, askClause)
}
