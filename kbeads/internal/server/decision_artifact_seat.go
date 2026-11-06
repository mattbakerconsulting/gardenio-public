package server

import (
	"context"
	"encoding/json"
	"strings"

	"gardenio/kbeads/internal/model"
)

// A later occupant of a seat delivering the artifact its seat owes
// (gd-JPfZCDbKFp).
//
// A decision records its durable lane in owner_seat, stamped from the filing
// agent's seat_key so ownership survives respawn (ka-0000005031). gb stop's
// required-artifact gate (checkRequiredArtifact) keys on that seat: a respawn
// INHERITS the obligation to deliver the chosen option's artifact. But the
// write that discharges it, gb decision report's artifact_status pending →
// submitted, went through authorizeOwnership, which knows only created_by /
// assignee / owner — the EARLIER pod's name. So the successor that owed the
// artifact could create and close the report bead and still not mark it
// delivered: "HTTP 403: ownership denied ... created_by=...-qv9m", and every
// later gb stop on the seat failed with "decision ... owes a plan artifact",
// leaving only --force.
//
// So the seat that owes the artifact may mark it delivered, scoped four ways:
//
//   - identity: a per-agent token whose open agent bead's seat_key equals the
//     decision's owner_seat, AND whose server-minted subject (the agent name)
//     is that seat's name base plus a generated suffix. The second check
//     binds the claim to the credential: seat_key is an ordinary field on the
//     agent bead, while the subject comes from the registered token;
//   - type: decision only, and only one that declared a required_artifact;
//   - fields: exactly artifact_status, and no column (title, status, labels,
//     assignee...);
//   - value: "submitted" only, the one transition gb decision report makes.
//
// Ownership is not the right to answer: chosen / responded_by and the other
// answer fields stay refused by authorizeDecisionAnswerFields, which runs
// before this, and the patch allowlist keeps them out regardless.

// decisionArtifactSeatFields is the one field a seat successor may write.
var decisionArtifactSeatFields = []string{"artifact_status"}

// authorizeDecisionArtifactBySeat reports whether this update is a later
// occupant of the decision's owner_seat marking its required artifact
// submitted, and so may skip the ownership gate.
func (s *BeadsServer) authorizeDecisionArtifactBySeat(ctx context.Context, bead *model.Bead, in *updateBeadInput) bool {
	if bead == nil || bead.Type != model.TypeDecision {
		return false
	}
	if !patchOnlyChangesFields(decisionArtifactSeatFields, in, bead.Fields) {
		return false
	}
	var patch map[string]any
	if json.Unmarshal(in.Fields, &patch) != nil || patch["artifact_status"] != "submitted" {
		return false
	}
	ownerSeat, _, _ := exactFieldString(bead.Fields, "owner_seat")
	required, _, _ := exactFieldString(bead.Fields, "required_artifact")
	if ownerSeat == "" || required == "" {
		return false
	}
	agent, err := s.callerAgentBead(ctx)
	if err != nil || agent == nil {
		return false
	}
	seat, _, _ := exactFieldString(agent.Fields, "seat_key")
	if seat != ownerSeat {
		return false
	}
	return subjectOccupiesSeat(AuthIdentityFromContext(ctx).Subject, ownerSeat)
}

// subjectOccupiesSeat reports whether an agent name is an occupant of a seat
// key "<project>/<role>/<name base>": the name is the base plus a "-<suffix>"
// tail (beadsapi.SeatKey strips a 3-6 character generated token), or the base
// itself for a name the seat derivation left unstripped.
func subjectOccupiesSeat(subject, seat string) bool {
	i := strings.LastIndex(seat, "/")
	if i < 0 || subject == "" {
		return false
	}
	base := seat[i+1:]
	if base == "" {
		return false
	}
	if subject == base {
		return true
	}
	suffix, ok := strings.CutPrefix(subject, base+"-")
	return ok && len(suffix) >= 3 && len(suffix) <= 6 && !strings.Contains(suffix, "-")
}
