// Server-side stamp for the escalation bead's requesting_agent field
// (gd-8eu41KlMr2). Escalation replay attributes taint propagation to
// fields.requesting_agent (WithTaintAttributionAgent in replayEscalation, PR
// #47 / ADR docs/adr/taints.md §4.2 row 7) so a replayed create/update
// inherits the ORIGINAL requester's taints, not the approver's.
// requesting_agent is server-stamped only on the authz-denial escalation
// path (write_authz.go:handleEscalation, identity.Subject). On the ordinary
// path a crew files the escalation bead itself (gb escalation create -> POST
// /v1/beads type=escalation) and requesting_agent arrives in the client body
// — `gb escalation create` populates it from the local BOAT_AGENT_BEAD_ID /
// KD_AGENT_ID env vars, both freely client-controlled. Left unchecked, a
// tainted agent could name an untainted agent as requesting_agent; once a
// human approves, the replay would attribute to the clean agent and carry no
// taints — the taint boundary bypassed at write time even though the filer
// is still recorded (auditable) as created_by.
package server

import (
	"context"

	"gardenio/kbeads/internal/model"
)

// escalationRequestingAgentVerifiedField marks a requesting_agent that the
// server derived from the caller's per-agent credential. Only the create guard
// writes it, and it is immutable after create. The continuation gate
// (escalationResolutionEvidence) resumes a privileged role only for an
// escalation that carries it, so a value a static-token caller merely typed
// (requesting_agent is still routing data the bridge and replay read) can never
// authorize a respawn.
const escalationRequestingAgentVerifiedField = "requesting_agent_verified"

// escalationRequestingAgentProjectField records the project scope of the
// per-agent token that filed the escalation. The create guard stamps it next to
// requesting_agent_verified, from the credential, never from the body, and it
// is immutable after create. The spawn replay's task pre-claim runs in this
// scope (spawnRequesterContext): the requester's agent bead is config-kind, so
// its own fields.project is editable by the agent and cannot stand in for it
// (gd-G5PmhWXsF9).
const escalationRequestingAgentProjectField = "requesting_agent_project"

// applyEscalationRequestingAgentGuard server-stamps fields.requesting_agent on
// a type=escalation bead being created, always as the agent BEAD ID. That is
// the one key every consumer shares: the Slack bridge looks the requester up
// with GetBead(requesting_agent) (nudge/respawn), the replay attributes taints
// by agent bead id (WithTaintAttributionAgent), and the continuation gate
// compares it to the prior agent bead's ID. (It used to be the agent NAME for
// a per-agent token, which the bridge could not resolve and the gate matched
// against a different key than the bridge used.)
//
//   - A caller on its own per-agent token has the field stamped from the bead
//     id in its fleet name, overriding the body, and
//     requesting_agent_verified=true records that the stamp is credential-derived.
//     A project-bound token also gets requesting_agent_project, its project.
//   - Anyone else keeps the requesting_agent they supplied (routing and display)
//     and never gets the marker.
//
// The markers are cleared first if the body carried them. Mutates bead.Fields
// in place.
func (s *BeadsServer) applyEscalationRequestingAgentGuard(ctx context.Context, bead *model.Bead) {
	bead.Fields = deleteFieldKey(bead.Fields, escalationRequestingAgentVerifiedField)
	bead.Fields = deleteFieldKey(bead.Fields, escalationRequestingAgentProjectField)
	if beadID, ok := agentBeadIDFromContext(ctx); ok {
		bead.Fields = setField(bead.Fields, "requesting_agent", beadID)
		bead.Fields = setField(bead.Fields, escalationRequestingAgentVerifiedField, "true")
		if project := TokenProjectFromContext(ctx); project != "" {
			bead.Fields = setField(bead.Fields, escalationRequestingAgentProjectField, project)
		}
		return
	}
}
