// Package mailproto is the label grammar that makes a type=mail bead an
// agent's inbox message (spec §3, docs/agent-mail-protocol.md). The daemon
// publishes beads.bead.created for every mail; the labels say who sent it,
// whether it interrupts, and whether it is a control message rather than
// text to read.
package mailproto

import (
	"fmt"
	"slices"
	"strings"
)

const (
	// Topic is the only SSE topic an agent subscribes to for its mail.
	Topic                     = "beads.bead.created"
	LabelFromPrefix           = "from:"
	LabelDeliveryInterrupt    = "delivery:interrupt"
	LabelControlPrefix        = "control:"
	ControlKill               = "kill"
	ControlEscalationApproved = "escalation-approved"
	// ControlPermission carries a Slack Allow/Deny verdict for a Claude Code
	// tool-approval prompt. The subscriber POSTs FieldPermissionRequestID and
	// FieldPermissionBehavior to the loopback gardenio-channel /permission
	// endpoint, which answers the prompt. The daemon lets only operators,
	// bridges and the controller send it, never a runner (kbeads
	// mail_control.go), so an agent cannot approve its own prompts.
	ControlPermission = "permission"
	// FieldPermissionRequestID and FieldPermissionBehavior ("allow" or
	// "deny") are the control:permission mail's fields.
	FieldPermissionRequestID = "request_id"
	FieldPermissionBehavior  = "behavior"
	// FieldControlActed is stamped (RFC 3339) on the close of control mail
	// the subscriber has acted on. The daemon refuses the recipient's own
	// token any other close, reopen or delete of control mail. The session
	// holds that same token and can stamp it too, so the subscriber honours
	// a kill whether its mail is open or closed (mailsub.drainClosedKills).
	// Keep in step with kbeads/internal/server/mail_control.go.
	FieldControlActed = "control_acted"
	// FieldDeliveredAt is stamped (RFC 3339) on mail the subscriber handed to
	// coop. Mail stays open until the agent reads it, so this is how the
	// bridge tells mail a live pod took from mail no pod is there to take
	// (bridge.watchMailDelivery).
	FieldDeliveredAt = "delivered_at"
)

// ControlVerbs is the closed set of control: verbs this milestone understands.
var ControlVerbs = []string{ControlKill, ControlEscalationApproved, ControlPermission}

// FromLabel returns the from:<sender> label.
func FromLabel(sender string) string { return LabelFromPrefix + sender }

// ControlLabel returns the control:<verb> label.
func ControlLabel(verb string) string { return LabelControlPrefix + verb }

// Sender returns the value of the first from: label, or "unknown".
func Sender(labels []string) string {
	for _, l := range labels {
		if from, ok := strings.CutPrefix(l, LabelFromPrefix); ok {
			return from
		}
	}
	return "unknown"
}

// ControlVerb returns the verb of the first control: label. ok is false when
// the mail is ordinary text.
func ControlVerb(labels []string) (string, bool) {
	for _, l := range labels {
		if verb, ok := strings.CutPrefix(l, LabelControlPrefix); ok {
			return verb, true
		}
	}
	return "", false
}

// ValidateLabels rejects a control: label whose verb is not in ControlVerbs.
func ValidateLabels(labels []string) error {
	for _, l := range labels {
		verb, ok := strings.CutPrefix(l, LabelControlPrefix)
		if !ok {
			continue
		}
		if !slices.Contains(ControlVerbs, verb) {
			return fmt.Errorf("unknown control verb %q (known: %s)", verb, strings.Join(ControlVerbs, ", "))
		}
	}
	return nil
}
