package bridge

import (
	"fmt"
	"strings"
)

// RelevanceMR is the minimal projection of a GitLab merge request that the MR
// Relevance Gardener's ticket-resolved (TV=0) close gate needs to reason about.
// It is deliberately decoupled from GitLabMR so the gate decision is a pure,
// unit-testable function with no GitLab client dependency.
type RelevanceMR struct {
	IID          int
	TargetBranch string
	// State mirrors GitLab's MR state: "opened", "closed", "merged", "locked".
	State string
}

// merged reports whether this MR reached the merged terminal state.
func (m RelevanceMR) merged() bool { return m.State == "merged" }

// IsProtectedTargetBranch reports whether branch is a protected long-lived
// branch — i.e. main (or master), or a release branch matching the `*.x`
// convention (e.g. "2.5.x", "1.x"). A merge to such a branch is the canonical
// ship target; an open MR targeting one must not be auto-closed merely because
// its ticket resolved elsewhere. See bug ka-0000002060.
func IsProtectedTargetBranch(branch string) bool {
	b := strings.TrimSpace(branch)
	if b == "" {
		return false
	}
	if b == "main" || b == "master" {
		return true
	}
	// Release branches use the `<series>.x` convention (e.g. 2.5.x, 1.x).
	if strings.HasSuffix(b, ".x") {
		return true
	}
	return false
}

// TicketResolvedGateInput is the input to the TV=0 (ticket-resolved) close
// gate. MR is the open MR the gardener is considering closing because its linked
// ticket resolved (Done / Won't-Fix). SiblingMRs are the *other* MRs linked to
// the same ticket; the slice may or may not include MR itself (it is matched
// out by IID).
type TicketResolvedGateInput struct {
	MR         RelevanceMR
	SiblingMRs []RelevanceMR
}

// TicketResolvedGateDecision is the outcome of the TV=0 close gate.
type TicketResolvedGateDecision struct {
	// MayClose is true when the gardener is permitted to fire the TV=0 close
	// gate on MR. When false, the MR must be left open (optionally flagged for
	// a human).
	MayClose bool
	// Reason is a one-line, human-readable explanation of the decision —
	// suitable for a squawk or an MR-description annotation.
	Reason string
	// BlockingSiblingIID is the IID of the merged sibling MR (on a different
	// branch) that resolved the ticket, when MayClose is false. Zero otherwise.
	BlockingSiblingIID int
}

// EvaluateTicketResolvedCloseGate decides whether the MR Relevance Gardener may
// fire its TV=0 (ticket-resolved) close gate on the given MR.
//
// The rule (bug ka-0000002060): a ticket can be resolved by a sibling MR on a
// different branch (e.g. a hotfix merged to a release branch) while an open MR
// targeting main or a release branch still needs to ship the same fix. Closing
// the protected-target MR in that case loses the unshipped fix. So:
//
//   - If MR's target branch is NOT protected (a feature/topic branch), the gate
//     fires normally — same as before.
//   - If MR's target branch IS protected (main / master / *.x release branch)
//     AND the ticket was resolved by a *different* merged MR on a *different*
//     branch, the gate is blocked: leave the MR open for a human.
//   - Otherwise (protected target but the resolution was same-branch, or there
//     is no merged sibling MR — i.e. non-sibling resolution), the gate fires.
func EvaluateTicketResolvedCloseGate(in TicketResolvedGateInput) TicketResolvedGateDecision {
	if !IsProtectedTargetBranch(in.MR.TargetBranch) {
		return TicketResolvedGateDecision{
			MayClose: true,
			Reason: fmt.Sprintf(
				"target branch %q is not protected; TV=0 close gate fires normally",
				in.MR.TargetBranch),
		}
	}

	// Protected target: look for a sibling MR (different IID) that is merged on
	// a *different* branch. Such a sibling resolved the ticket without shipping
	// this MR's fix to its protected target.
	for _, sib := range in.SiblingMRs {
		if sib.IID == in.MR.IID {
			continue
		}
		if !sib.merged() {
			continue
		}
		if sib.TargetBranch == in.MR.TargetBranch {
			// Same-branch resolution: the fix is already on this MR's target,
			// so closing is correct.
			continue
		}
		return TicketResolvedGateDecision{
			MayClose: false,
			Reason: fmt.Sprintf(
				"ticket resolved by sibling MR !%d merged to %q; MR !%d targets protected branch %q and its fix is not yet shipped there — leaving open for human review",
				sib.IID, sib.TargetBranch, in.MR.IID, in.MR.TargetBranch),
			BlockingSiblingIID: sib.IID,
		}
	}

	// Protected target, but no different-branch merged sibling: either the
	// resolution was same-branch (fix is on this target) or non-sibling
	// (Won't-Fix / resolved without a sibling MR). The gate fires.
	return TicketResolvedGateDecision{
		MayClose: true,
		Reason: fmt.Sprintf(
			"target branch %q is protected but no sibling MR resolved the ticket on a different branch; TV=0 close gate fires",
			in.MR.TargetBranch),
	}
}
