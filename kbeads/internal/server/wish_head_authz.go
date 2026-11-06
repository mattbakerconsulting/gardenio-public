package server

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Wish-head marks (gd-3HAfTiByJK, defence in depth for gd-PTOUU1ufzq). The
// bridge reads a bead labelled thread as a wish head (isWishHeadBead), and one
// whose fields.promoted_from names a Commons chat root as that root's head
// (promotedHeadOf). A member may create and update tasks in its land, so
// without this gate it could write a head for another member's chat root
// ahead of the bridge, and that member's mention would then run under a head
// the forger wrote. Only the bridge and the viewer's composer mint heads; both
// write as service or operator identities, never as a tenant.
//
// A head is also found by its identity, not only by its marks: the Slack
// thread label slack-thread:<c>:<ts> (getOrCreateThreadBead) and the create's
// idempotency key (commons-promote:<root id> or the same Slack label,
// promoteChatRoot). The daemon returns the existing bead for a key whoever
// wrote it, so a tenant holding a head's identity first would get the bridge
// to adopt and stamp its bead. Those are reserved too.

// wishHeadLabel is the bare wish-head label. Keep in step with
// gardenio/controller/internal/bridge/thread_bead.go threadKindLabel (a
// separate module, so the constant is repeated, not imported). thread:<id>
// and thread-say are not reserved: members write them on replies.
const wishHeadLabel = "thread"

// wishHeadIdentityPrefixes are the prefixes of a head's identity, as a label
// (slack-thread:) or as an idempotency key (both). Keep in step with the
// bridge's threadBeadLabel and promoteChatRoot.
var wishHeadIdentityPrefixes = []string{"slack-thread:", "commons-promote:"}

// isWishHeadIdentity reports whether v names a wish head's identity.
func isWishHeadIdentity(v string) bool {
	for _, p := range wishHeadIdentityPrefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

// isWishHeadLabel reports whether label is reserved: the bare thread label or
// a Slack thread's identity label.
func isWishHeadLabel(label string) bool {
	return label == wishHeadLabel || strings.HasPrefix(label, "slack-thread:")
}

// wishHeadFields are the promotion links between a Commons chat root and its
// wish head (gardenio/controller/internal/commons fieldPromotedFrom,
// fieldPromotedTo).
var wishHeadFields = map[string]bool{
	"promoted_from": true,
	"promoted_to":   true,
}

// changedWishHeadFields returns the sorted reserved keys a create (existing
// == nil) or update (existing = the stored fields) would set, change or clear:
// a promotion link, or an idempotency key that is or was a head's identity.
// An unchanged value echoed back by a read-modify-write client does not count,
// so a member can still edit a task that carries one.
func changedWishHeadFields(patch, existing json.RawMessage) []string {
	if len(patch) == 0 {
		return nil
	}
	var p, e map[string]any
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil
	}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &e)
	}
	var changed []string
	for k, pv := range p {
		next, prev := channelBindingValue(pv), channelBindingValue(e[k])
		if next == prev {
			continue
		}
		if wishHeadFields[k] || (k == idempotencyKeyField && (isWishHeadIdentity(next) || isWishHeadIdentity(prev))) {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return changed
}

// changedWishHeadLabels returns, sorted and as "label <l>", the reserved
// labels (isWishHeadLabel) that going from before to after adds or removes.
func changedWishHeadLabels(before, after []string) []string {
	var changed []string
	for _, side := range [2][2][]string{{before, after}, {after, before}} {
		for _, l := range side[0] {
			if isWishHeadLabel(l) && !slices.Contains(side[1], l) {
				changed = append(changed, "label "+l)
			}
		}
	}
	sort.Strings(changed)
	return slices.Compact(changed)
}

// wishHeadDenial is the gate on creates and updates: labels are the reserved
// labels the write adds or removes (changedWishHeadLabels); patch and
// existing are as for changedWishHeadFields.
func (s *BeadsServer) wishHeadDenial(ctx context.Context, action string, beadType model.BeadType, labels []string, patch, existing json.RawMessage) *AuthzDenial {
	changed := append(slices.Clone(labels), changedWishHeadFields(patch, existing)...)
	return s.denyWishHeadMarks(ctx, action, beadType, changed)
}

// wishHeadLabelChangeDenial gates the single-label routes (gRPC AddLabel and
// RemoveLabel, HTTP POST /v1/beads/{id}/labels and DELETE
// /v1/beads/{id}/labels/{label}).
func (s *BeadsServer) wishHeadLabelChangeDenial(ctx context.Context, action string, target *model.Bead, label string) *AuthzDenial {
	if target == nil || !isWishHeadLabel(label) {
		return nil
	}
	return s.denyWishHeadMarks(ctx, action, target.Type, []string{"label " + label})
}

// singleLabelChangeDenial is the authz shared by the single-label routes
// (HTTP and gRPC add and remove): the mail-control and wish-head label gates.
func (s *BeadsServer) singleLabelChangeDenial(ctx context.Context, target *model.Bead, label string) *AuthzDenial {
	if denial := s.mailControlLabelChangeDenial(ctx, target, label); denial != nil {
		return denial
	}
	if denial := s.stewardAdviceLabelChangeDenial(ctx, target, label); denial != nil {
		return denial
	}
	return s.wishHeadLabelChangeDenial(ctx, "update", target, label)
}

// wishHeadKeyDenial gates the single-field endpoints and the extra-fields
// merge, where only the key is known: touching a promotion key or the
// idempotency key at all is a change.
func (s *BeadsServer) wishHeadKeyDenial(ctx context.Context, action string, bead *model.Bead, keys ...string) *AuthzDenial {
	if bead == nil {
		return nil
	}
	var changed []string
	for _, k := range keys {
		if wishHeadFields[k] || k == idempotencyKeyField {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return s.denyWishHeadMarks(ctx, action, bead.Type, changed)
}

// denyWishHeadMarks is the single decision point: a tenant caller (a scoped
// principal, or an agent of a tenant land, callerIsTenant) may not write a
// wish-head mark or identity. Every non-tenant caller passes (the bridge's
// fleet token, the viewer's service identity, an operator or fleet agent), as
// does a server write on no caller's behalf (isMachineWrite).
func (s *BeadsServer) denyWishHeadMarks(ctx context.Context, action string, beadType model.BeadType, changed []string) *AuthzDenial {
	if len(changed) == 0 || isMachineWrite(ctx) || !s.callerIsTenant(ctx) {
		return nil
	}
	return &AuthzDenial{
		Action:   action,
		BeadType: string(beadType),
		Message: "a land member may not " + action + " " + strings.Join(changed, ", ") +
			": a wish head's marks and identity are reserved for the bridge and fleet writers",
	}
}
