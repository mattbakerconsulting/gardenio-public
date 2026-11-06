package server

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Channel Slack binding gate (gd-ecFKkjtdvS). A channel bead's
// slack_channel_id says which Slack channel it mirrors, and mirror says which
// way. The bridge attributes a Slack channel to whichever channel bead names it
// (channel_stamp.go resolveChannelBead), and with two-way mirroring it copies
// that Slack channel's messages into the bead's land and posts the land's
// messages back as the bot. So binding is an authority grant across the land
// wall, not a channel setting: a tenant who could set slack_channel_id could
// claim a private operator channel the bot is in, read it from their land and
// post into it. Only the operator binds a channel to Slack.
//
// The uniqueness rule in channel_validation.go is not enough on its own: it
// stops two lands sharing one Slack channel, not a land claiming one first.

// channelBindingFields are the gated type:channel fields.
var channelBindingFields = map[string]bool{
	"slack_channel_id": true,
	"mirror":           true,
}

// channelBindingValue is a binding field's value for comparison: absent, null
// and "" are all "not set".
func channelBindingValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// changedChannelBindingFields returns the sorted binding keys a create
// (existing == nil) or update (existing = the stored fields) would set, change
// or clear. Clearing counts: it unbinds the operator's Slack channel from the
// land and frees it for another to claim. An unchanged value echoed back by a
// read-modify-write client does not count, so a non-admin can still rename or
// re-topic a bound channel.
func changedChannelBindingFields(patch, existing json.RawMessage) []string {
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
		if !channelBindingFields[k] || channelBindingValue(pv) == channelBindingValue(e[k]) {
			continue
		}
		changed = append(changed, k)
	}
	sort.Strings(changed)
	return changed
}

// authorizeChannelBindingFields is the field-level gate on type:channel
// creates and updates. patch is the incoming fields (create body or update
// patch); existing is the stored fields on update, nil on create.
func (s *BeadsServer) authorizeChannelBindingFields(ctx context.Context, action string, beadType model.BeadType, patch, existing json.RawMessage) *AuthzDenial {
	if beadType != typeChannel {
		return nil
	}
	return s.denyChannelBindingFields(ctx, action, changedChannelBindingFields(patch, existing))
}

// authorizeChannelBindingKeyWrite gates the single-field mutation endpoints and
// the extra-fields merge on a channel bead. Only the key is known, not the
// value, so touching a binding key at all is denied for a non-admin.
func (s *BeadsServer) authorizeChannelBindingKeyWrite(ctx context.Context, action string, bead *model.Bead, keys ...string) *AuthzDenial {
	if bead == nil || bead.Type != typeChannel {
		return nil
	}
	var changed []string
	for _, k := range keys {
		if channelBindingFields[k] {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return s.denyChannelBindingFields(ctx, action, changed)
}

// denyChannelBindingFields is the single decision point: given the binding
// fields a write would change, it returns a 403 denial unless the caller is an
// OIDC gardenio-admin (a verified v2 edge envelope counts as OIDC), an
// auth-disabled daemon, or an approved escalation replay. Everyone else is
// refused: a land principal and a land member (both hold gardenio-operator,
// never admin), a non-admin operator, a v1 edge envelope (no role,
// gd-AvSKOUyqRz), and every service token (static, fleet, machine), which an
// agent pod in any land may hold.
// The refusal names no Slack channel and no other bead, and runs before the
// uniqueness check, so a non-admin cannot use it to learn which Slack
// channels are bound.
func (s *BeadsServer) denyChannelBindingFields(ctx context.Context, action string, changed []string) *AuthzDenial {
	if len(changed) == 0 || isMachineWrite(ctx) {
		return nil
	}
	switch authMethodFromContext(ctx) {
	case authMethodNone:
		return nil
	case authMethodOIDC:
		if hasMinimumRole(RolesFromContext(ctx), RoleAdmin) {
			return nil
		}
	}
	d := &AuthzDenial{
		Action:       action,
		BeadType:     string(typeChannel),
		RequiredRole: RoleAdmin,
		Escalatable:  true,
		Message: "insufficient permissions: " + action + " on type channel setting " +
			strings.Join(changed, ", ") + " requires " + RoleAdmin +
			"; binding a channel to Slack mirrors that Slack channel into the land, so only the operator sets it",
	}
	s.populateAdvisoryText(ctx, d, "Binding a channel to a Slack channel (slack_channel_id, mirror) copies Slack traffic across the land wall; it needs "+RoleAdmin+".")
	return d
}
