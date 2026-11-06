package server

import (
	"context"
	"strings"

	"gardenio/kbeads/internal/model"
)

// A laptop agent's delivered_at stamp on its own mail (gd-GTMwKmHM7o).
//
// mailsub stamps delivered_at on mail once coop takes it (stampDelivered), and
// the bridge's watchMailDelivery reads the stamp to tell mail a live agent has
// but has not read from mail no agent received. A pod stamps on its per-agent
// token, whose subject is the agent name, which is the mail's assignee, so
// ownership admits it.
//
// `gb agent start --local` has no per-agent token. It stamps on the runner
// token (subject: the runner's owner) or the operator's own sign-in, neither of
// which is the mail's creator, assignee or owner, so ownership refused every
// stamp and the bridge posted "has not picked up the message yet" after every
// mention to a laptop agent that had in fact taken it.
//
// So the principal a laptop agent runs as may stamp its mail, scoped four ways:
//
//   - type: mail only;
//   - fields: delivered_at only, and no column (title, status, assignee...);
//   - set once: delivered_at must be empty before (patchOnlySetsEmptyFields),
//     so nothing can re-date or clear a stamp it does not own;
//   - binding: the mail's assignee names an active type=agent bead with
//     execution_target=laptop that belongs to the caller. A runner caller must
//     be the runner its runtime_id names (the same test runnerStreamDenial
//     uses for the event stream); any other caller must be the agent bead's
//     creator or its operator (callerIsBeadOperator). A pod agent's mail is
//     never stamped this way: its own token already can.

// mailDeliveryStampFields is the one field mailsub stamps. KEEP IN SYNC with
// mailproto.FieldDeliveredAt (TestMailDeliveredAt_MatchesMailprotoConstant).
var mailDeliveryStampFields = []string{"delivered_at"}

// executionTargetLaptop is the agent bead execution_target `gb agent start
// --local` writes (podmanager.ExecutionTargetLaptop in the controller).
const executionTargetLaptop = "laptop"

// authorizeLaptopMailDeliveryStamp reports whether this update is the
// principal a laptop agent runs as stamping delivered_at on that agent's mail,
// and so may skip the ownership gate. Every lookup failure is a refusal.
func (s *BeadsServer) authorizeLaptopMailDeliveryStamp(ctx context.Context, bead *model.Bead, in *updateBeadInput) bool {
	if bead == nil || bead.Type != "mail" || s == nil || s.store == nil {
		return false
	}
	name := bead.Assignee
	if name == "" || strings.Contains(name, "@") {
		return false
	}
	if !patchOnlyChangesFields(mailDeliveryStampFields, in, bead.Fields) || !patchOnlySetsEmptyFields(in.Fields, bead.Fields) {
		return false
	}
	agent, err := s.store.FindAgentByName(ctx, name, activeAgentStatuses)
	if err != nil || agent == nil {
		return false
	}
	if target, _, err := exactFieldString(agent.Fields, "execution_target"); err != nil || target != executionTargetLaptop {
		return false
	}
	if authMethodFromContext(ctx) == authMethodRunner {
		rid, _, err := exactFieldString(agent.Fields, runtimeIDField)
		return err == nil && rid != "" && rid == RunnerIDFromContext(ctx)
	}
	id := AuthIdentityFromContext(ctx)
	if id.Subject == "" {
		return false
	}
	return agent.CreatedBy == id.Subject || (agent.OperatedBy != "" && s.callerIsBeadOperator(ctx, id, agent))
}
