package server

import (
	"context"
	"encoding/json"
	"reflect"

	"gardenio/kbeads/internal/model"
)

// The Slack bridge's stamps on a message it did not write (gd-2IEp8qx7bq).
//
// The bridge writes three bookkeeping fields on message beads other
// principals created:
//
//   - handled_at: inbound_reply.go dispatchCommonsAs, once it has handed a
//     person's Commons reply or DM message to its agent. The sweep
//     (sweepCommonsInbound) skips a message that carries it.
//   - slack_channel / slack_ts: the ref of the Slack copy, stamped by the
//     conversation Router after it projects a message (router.go route,
//     upsertCardAt) and by CommonsMirror.stampRef.
//
// The message is a person's (created_by their email) or an agent's (created_by
// the agent), and the bridge writes on its fleet token (fleet "slack-bridge"),
// so authorizeOwnership refused every one of them: "ownership denied: update on
// bead ... you are slack-bridge". commons_origin.go assumed the bridge could
// make these writes, and its tests made them on the static token, which has
// the blanket ownership bypass the bridge's own token does not. On live the
// stamps never landed: a Commons reply was re-dispatched to its agent about
// every hour, a Slack reply under a mirrored copy could not find its Commons
// root, and ~30 agents' Commons posts had no Slack ref.
//
// So the bridge may stamp those fields on a message it does not own, scoped
// four ways:
//
//   - identity: the bridge fleet token only (callerIsSlackBridgeFleet), never
//     an agent's token, another bridge, a land principal or a person;
//   - type: message only;
//   - fields: exactly bridgeMessageStampFields, and no column (title, status,
//     labels, assignee...);
//   - set once: each field it changes must be empty before and non-empty
//     after. Every one of these stamps is written once, when the bead has
//     none, so the bridge cannot move a Slack ref, clear one, or re-date
//     handled_at on a bead it does not own. The person's-words gate
//     (commonsMessageEditDenial) holds the same line for slack_channel and
//     slack_ts on a person's Commons message, and runs first.
//
// The bridge's Keycloak machine principal is not admitted: it carries only the
// config-seed and DM-delivery roles (machine_grants.go), and message writes
// stay on the fleet credential.

// bridgeMessageStampFields are the message fields the Slack bridge stamps on a
// message it did not create. KEEP IN SYNC with the bridge: commonsHandledAt
// (inbound_reply.go) and the Router's "<surface>_channel"/"<surface>_ts" stamp
// for the slack surface (beadSlackChannel/beadSlackTS). Like every allowlist
// on this path it is all-or-nothing: one unlisted key sends the whole patch
// to authorizeOwnership.
var bridgeMessageStampFields = []string{
	"handled_at",
	"slack_channel",
	"slack_ts",
}

// callerIsSlackBridgeFleet reports whether the caller is the Slack bridge on
// its fleet token. The fleet name comes from the operator-registered token
// (the bridge-tokens Secret or the rotator), never from a header, and
// validateUntrustedMint reserves the "-bridge" suffix so only the token-mint
// capability can name it. A per-agent token is never the bridge.
func callerIsSlackBridgeFleet(ctx context.Context) bool {
	if AuthIdentityFromContext(ctx).Type != "fleet" {
		return false
	}
	fleet := FleetFromContext(ctx)
	return fleet == slackBridgeFleet && !isAgentFleet(fleet)
}

// authorizeBridgeMessageStamp reports whether this update is the Slack
// bridge setting its bookkeeping stamps on a message, and so may skip the
// ownership gate.
func authorizeBridgeMessageStamp(ctx context.Context, bead *model.Bead, in *updateBeadInput) bool {
	if bead == nil || bead.Type != typeMessage || !callerIsSlackBridgeFleet(ctx) {
		return false
	}
	if !patchOnlyChangesFields(bridgeMessageStampFields, in, bead.Fields) {
		return false
	}
	return patchOnlySetsEmptyFields(in.Fields, bead.Fields)
}

// patchOnlySetsEmptyFields reports whether every key the patch changes was
// empty (absent, null or "") before and is a non-empty string after. Keys
// echoed back unchanged do not count, as in patchOnlyChangesFields.
func patchOnlySetsEmptyFields(patchRaw, existingRaw json.RawMessage) bool {
	var patch map[string]any
	if err := json.Unmarshal(patchRaw, &patch); err != nil {
		return false
	}
	var existing map[string]any
	if len(existingRaw) > 0 {
		if err := json.Unmarshal(existingRaw, &existing); err != nil {
			return false
		}
	}
	for k, pv := range patch {
		ev, had := existing[k]
		if had && reflect.DeepEqual(ev, pv) {
			continue
		}
		if s, ok := ev.(string); had && ev != nil && (!ok || s != "") {
			return false
		}
		if s, ok := pv.(string); !ok || s == "" {
			return false
		}
	}
	return true
}
