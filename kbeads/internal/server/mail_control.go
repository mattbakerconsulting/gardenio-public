package server

import (
	"context"
	"os"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
)

// mailControlPrefix is the reserved mail label prefix (control:kill,
// control:escalation-approved). Keep in step with
// gardenio/controller/internal/mailproto (a separate module, so the constant
// is repeated, not imported).
const mailControlPrefix = "control:"

// mailControlPermission is the label of a Slack permission verdict
// (mailproto.ControlPermission): the recipient's subscriber hands it to the
// loopback channel server, which answers Claude Code's tool-approval prompt.
// It is the one control: label a runner may not set, even on its own agent's
// mail: the runner's credential sits on the same machine as the agent it
// runs, so letting it through would let a local agent approve its own
// prompts. Only bridges, the controller and operators send it.
const mailControlPermission = mailControlPrefix + "permission"

// defaultControlMailFleets are the fleet identities allowed to send control:
// mail when KBEADS_CONTROL_MAIL_FLEETS is unset.
const defaultControlMailFleets = "slack-bridge,gitlab-bridge,jira-bridge,github-bridge"

func parseControlMailFleets(raw string) map[string]bool {
	if strings.TrimSpace(raw) == "" {
		raw = defaultControlMailFleets
	}
	out := map[string]bool{}
	for _, f := range strings.Split(raw, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out[f] = true
		}
	}
	return out
}

func controlMailFleetsFromEnv() map[string]bool {
	return parseControlMailFleets(os.Getenv("KBEADS_CONTROL_MAIL_FLEETS"))
}

type runnerRevokeKillKey struct{}

// withRunnerRevokeKill marks ctx as handleRevokeRunner sending its control:kill
// mail once the revoke is authorized, so the create guard does not re-judge the
// revoker against the control: mail rules (gd-qoiurW3S7m). It has no header
// path; set it only there.
func withRunnerRevokeKill(ctx context.Context) context.Context {
	return context.WithValue(ctx, runnerRevokeKillKey{}, true)
}

func isRunnerRevokeKill(ctx context.Context) bool {
	v, _ := ctx.Value(runnerRevokeKillKey{}).(bool)
	return v
}

// applyMailControlGuard enforces spec §3.3: a control: label on mail may be
// set only by an operator (oidc, or edge, the operator envelope), a bridge
// fleet identity listed in KBEADS_CONTROL_MAIL_FLEETS, the controller, or the
// runner that owns the recipient (not for control:permission). A per-agent
// token is refused, so one crew cannot kill another, nor answer its own
// permission prompts.
//
// The retired shared static token (gd-ke6PC7bRto) is not on the list: no
// request carries that identity any more, and a stray one is refused.
//
// Kill mail handleRevokeRunner sends after its own authz (callerMayRevokeRunner)
// is the daemon's, not the caller's, and is not checked (withRunnerRevokeKill);
// nor is the escalation-approved mail an authorized approval sends
// (withEscalationApprovedMail).
func (s *BeadsServer) applyMailControlGuard(ctx context.Context, bead *model.Bead) error {
	if isRunnerRevokeKill(ctx) || isEscalationApprovedMail(ctx) {
		return nil
	}
	if d := s.mailControlDenial(ctx, "create", bead); d != nil {
		return d
	}
	return nil
}

// mailControlLabelChangeDenial is the guard for the single-label paths
// (gRPC AddLabel and RemoveLabel, HTTP POST /v1/beads/{id}/labels and DELETE
// /v1/beads/{id}/labels/{label}): adding or removing a control: label on an
// existing mail bead needs the same identity the create guard asks for. An
// unguarded add lets a crew send plain mail and then add control:kill to it;
// an unguarded remove lets a crew strip control:kill from mail an operator
// sent another agent before that agent's mailsub Drain honours it.
func (s *BeadsServer) mailControlLabelChangeDenial(ctx context.Context, target *model.Bead, label string) *AuthzDenial {
	if target == nil || target.Type != "mail" || !strings.HasPrefix(label, mailControlPrefix) {
		return nil
	}
	return s.mailControlDenial(ctx, "update", &model.Bead{Type: target.Type, Assignee: target.Assignee, Labels: []string{label}})
}

// mailControlUpdateDenial is the guard for updateBead on a mail bead, run on
// the bead as the update would leave it. A control: label the update adds
// is checked, and so is every control: label already on the bead when the
// update changes the assignee, since retargeting control mail sends it to a
// new recipient. A control: label the update removes is checked against the
// recipient it was aimed at (the prior assignee), since stripping it cancels
// the control action. Leaving existing control: labels in place on an update
// that keeps the recipient is not checked, so the recipient can still edit
// its own mail. An update cannot change a bead's type, so mail cannot be
// made out of a bead that already carries a control: label.
func (s *BeadsServer) mailControlUpdateDenial(ctx context.Context, bead *model.Bead, oldLabels []string, oldAssignee string) *AuthzDenial {
	if bead == nil || bead.Type != "mail" {
		return nil
	}
	retargeted := bead.Assignee != oldAssignee
	had := make(map[string]bool, len(oldLabels))
	for _, l := range oldLabels {
		had[l] = true
	}
	kept := make(map[string]bool, len(bead.Labels))
	var checked []string
	for _, l := range bead.Labels {
		kept[l] = true
		if strings.HasPrefix(l, mailControlPrefix) && (retargeted || !had[l]) {
			checked = append(checked, l)
		}
	}
	var removed []string
	for _, l := range oldLabels {
		if strings.HasPrefix(l, mailControlPrefix) && !kept[l] {
			removed = append(removed, l)
		}
	}
	if len(removed) > 0 {
		if d := s.mailControlDenial(ctx, "update", &model.Bead{Type: bead.Type, Assignee: oldAssignee, Labels: removed}); d != nil {
			return d
		}
	}
	if len(checked) == 0 {
		return nil
	}
	return s.mailControlDenial(ctx, "update", &model.Bead{Type: bead.Type, Assignee: bead.Assignee, Labels: checked})
}

// mailControlActedField is the field the recipient's mail subscriber stamps
// on the close of control: mail it has acted on (mailsub.kill). Keep in step
// with mailproto.FieldControlActed.
const mailControlActedField = "control_acted"

// mailControlCloseDenial is the guard for ending control: mail: close (gRPC
// CloseBead, HTTP POST /v1/beads/{id}/close), delete, and an updateBead that
// changes its status. mailsub's Drain reads open mail, so any of these
// cancels a control action that has not been honoured yet. Callers the create
// guard allows pass. Anyone else (in practice the recipient's own token, the
// only other caller past the ownership gate) is refused, except for a close
// whose fields stamp control_acted: mailsub closes kill mail that way once it
// has handed the kill to coop.
//
// The exemption is housekeeping, not a boundary. mailsub runs in the same
// process tree and uid as the agent's session and holds the same token, so
// no credential the daemon could tell apart is out of the session's reach,
// and the session can stamp control_acted too. A forged close is made
// pointless instead: mailsub honours kill mail whether it is open or closed
// (mailsub drainClosedKills, gd-nua84XbHla), and a Slack kill hard-closes the
// agent bead after its wait, which deletes the pod.
// fields is the close body's extra fields; nil for every other action.
func (s *BeadsServer) mailControlCloseDenial(ctx context.Context, action string, target *model.Bead, fields map[string]any) *AuthzDenial {
	if target == nil || target.Type != "mail" {
		return nil
	}
	var control []string
	for _, l := range target.Labels {
		if strings.HasPrefix(l, mailControlPrefix) {
			control = append(control, l)
		}
	}
	if len(control) == 0 {
		return nil
	}
	d := s.mailControlDenial(ctx, action, &model.Bead{Type: target.Type, Assignee: target.Assignee, Labels: control})
	if d == nil {
		return nil
	}
	if action == "close" {
		if v, ok := fields[mailControlActedField].(string); ok && strings.TrimSpace(v) != "" {
			return nil
		}
	}
	d.Message = "control: mail is closed by the recipient's mail subscriber once it acts on it; only operators, bridges, the controller, and the recipient's own runner may otherwise close, reopen or delete it"
	return d
}

func (s *BeadsServer) mailControlDenial(ctx context.Context, action string, bead *model.Bead) *AuthzDenial {
	hasControl := false
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, mailControlPrefix) {
			hasControl = true
			break
		}
	}
	if !hasControl {
		return nil
	}
	deny := &AuthzDenial{
		Action:      action,
		BeadType:    "mail",
		Message:     "control: labels are reserved for operators, bridges, the controller, and the recipient's own runner",
		Escalatable: false,
	}
	id := AuthIdentityFromContext(ctx)
	switch id.Type {
	case "oidc", "edge":
		return nil
	case "fleet":
		fleets := s.controlMailFleets
		if fleets == nil {
			fleets = controlMailFleetsFromEnv()
		}
		if fleets[FleetFromContext(ctx)] {
			return nil
		}
		return deny
	case runnerIdentityType:
		runnerID := RunnerIDFromContext(ctx)
		if runnerID == "" || bead.Assignee == "" || slices.Contains(bead.Labels, mailControlPermission) {
			return deny
		}
		agent, err := s.store.FindAgentByName(ctx, bead.Assignee, nil)
		if err != nil || agent == nil {
			return deny
		}
		// Exact key only, as runnerStreamDenial reads it.
		if rid, _, err := exactFieldString(agent.Fields, runtimeIDField); err == nil && rid != "" && rid == runnerID {
			return nil
		}
		return deny
	default:
		return deny
	}
}
