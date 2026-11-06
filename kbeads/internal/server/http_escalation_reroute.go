package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// escalationRerouteCap bounds one reroute pass's working set.
const escalationRerouteCap = 1000

// rerouteEscalationsRequest is the body for POST /v1/escalations/reroute.
type rerouteEscalationsRequest struct {
	ApprovedUnexecuted bool `json:"approved_unexecuted"`
	DryRun             bool `json:"dry_run"`
}

// reroutedEscalation is one escalation a reroute moved (or would move).
type reroutedEscalation struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	ExecuteBy string `json:"execute_by"`
	HeldBy    string `json:"held_by"`
}

const escalationRerouteDenied = "rerouting escalations requires gardenio-admin on your own sign-in; an agent token may not"

// handleRerouteEscalations handles POST /v1/escalations/reroute, P7.1's
// one-shot backfill (admin only). With approved_unexecuted it takes every
// open, approved, unclaimed escalation the operator holds that a machine lane
// should be executing (strandedEscalationReroute) and gives it to that lane:
// the ones an idle hand-up stranded with the operator. One whose operation is
// already satisfied is closed instead (closed_satisfied). dry_run writes
// nothing.
func (s *BeadsServer) handleRerouteEscalations(w http.ResponseWriter, r *http.Request) {
	var req rerouteEscalationsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	ctx := r.Context()
	if !callerMayApproveAsAdmin(ctx) {
		writeError(w, http.StatusForbidden, escalationRerouteDenied)
		return
	}
	if !req.ApprovedUnexecuted {
		writeError(w, http.StatusBadRequest, "approved_unexecuted is required (the only reroute there is)")
		return
	}
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		writeServerError(ctx, w, "handleRerouteEscalations", err, "failed to read the escalation policy")
		return
	}
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeEscalation},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress},
		Limit:  escalationRerouteCap,
		Sort:   "created_at",
	})
	if err != nil {
		writeServerError(ctx, w, "handleRerouteEscalations", err, "failed to list open escalations")
		return
	}
	actor := laneActor(ctx)
	out := []reroutedEscalation{}
	closed := []string{}
	for _, b := range beads {
		fields, from, ok := s.strandedEscalationReroute(ctx, b, p)
		if !ok {
			continue
		}
		if req.DryRun {
			if _, satisfied := s.escalationAlreadySatisfied(ctx, b); satisfied {
				closed = append(closed, b.ID)
				continue
			}
		} else if s.closeSatisfiedEscalation(ctx, b, actor) {
			closed = append(closed, b.ID)
			continue
		}
		if !req.DryRun {
			if err := s.applyEscalationReroute(ctx, b, actor, from, fields); err != nil {
				slog.Warn("escalation reroute: write failed", "escalation", b.ID, "error", err)
				continue
			}
		}
		out = append(out, reroutedEscalation{ID: b.ID, From: from, ExecuteBy: fields["execute_by"].(string), HeldBy: fields["held_by"].(string)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"considered": len(beads), "rerouted": out, "closed_satisfied": closed, "dry_run": req.DryRun,
		"truncated": len(beads) == escalationRerouteCap,
	})
}

// strandedEscalationReroute reports whether b is approved, unclaimed, held by
// the operator and stranded there from a machine lane, and if so the fields
// that give it to a lane and the execute_by it leaves:
//   - executed by a coordinator lane another machine lane may take over
//     (P7.1: bead-only work to coordinator:cluster): moved there;
//   - executed by a staffed coordinator lane the idle sweep handed it up
//     from: held by that lane again (gd-YnsSlh9239);
//   - executed by operator only because the idle sweep moved execution off
//     a staffed coordinator lane: that lane executes and holds it again.
//
// A manual hand-up ("needs you") is the lane's own call and stays.
func (s *BeadsServer) strandedEscalationReroute(ctx context.Context, b *model.Bead, p escalationPolicy) (map[string]any, string, bool) {
	if !escalationHasServerApproval(b) || escalationHeldBy(b) != string(laneOperator) {
		return nil, "", false
	}
	if c, _, _ := exactFieldString(b.Fields, "claimed_by"); strings.TrimSpace(c) != "" {
		return nil, "", false
	}
	was, _, _ := exactFieldString(b.Fields, "execute_by")
	was = strings.TrimSpace(was)
	swept := false
	if by, _, _ := exactFieldString(b.Fields, "handed_up_by"); strings.TrimSpace(by) == escalationRouteSweepActor {
		swept = true
	}
	if was == string(laneOperator) {
		lane, ok := sweptExecutionFrom(b)
		if !swept || !ok || !s.laneMayHold(ctx, lane, b, p) {
			return nil, "", false
		}
		return map[string]any{"execute_by": lane.String(), "held_by": lane.String(), escalationAudienceField: laneAudience(lane.String(), p)}, was, true
	}
	from, err := parseLaneRef(was)
	if err != nil || from.Kind != laneCoordinator {
		return nil, "", false
	}
	fields := map[string]any{"held_by": string(laneOperator)}
	if s.retargetExecution(ctx, b, from, p, fields) != "" && fields["execute_by"] != string(laneOperator) {
		return fields, from.String(), true
	}
	if !swept || !s.laneMayHold(ctx, from, b, p) {
		return nil, "", false
	}
	return map[string]any{"execute_by": from.String(), "held_by": from.String(), escalationAudienceField: laneAudience(from.String(), p)}, from.String(), true
}

// sweptExecutionFrom is the coordinator lane the idle sweep moved b's
// execution off, read from the note it appends to hand_up_reason
// ("execution moved from <lane> to operator").
func sweptExecutionFrom(b *model.Bead) (laneRef, bool) {
	reason, _, _ := exactFieldString(b.Fields, "hand_up_reason")
	const marker = "execution moved from "
	i := strings.LastIndex(reason, marker)
	if i < 0 {
		return laneRef{}, false
	}
	ref, rest, ok := strings.Cut(reason[i+len(marker):], " to ")
	if !ok || !strings.HasPrefix(rest, string(laneOperator)) {
		return laneRef{}, false
	}
	lane, err := parseLaneRef(ref)
	if err != nil || lane.Kind != laneCoordinator {
		return laneRef{}, false
	}
	return lane, true
}

// applyEscalationReroute writes a reroute with a pingback, so the bridge
// re-notifies the new holder, and publishes beads.bead.updated.
func (s *BeadsServer) applyEscalationReroute(ctx context.Context, b *model.Bead, actor, from string, fields map[string]any) error {
	now := time.Now().UTC().Format(time.RFC3339)
	count := 0
	if v, _, _ := exactFieldString(b.Fields, "pingback_count"); v != "" {
		count, _ = strconv.Atoi(v)
	}
	note := "approved but never executed; execution moved from " + from + " to " + fields["execute_by"].(string)
	if fields["execute_by"] == from {
		note = "approved but never executed; back with " + from + ", which executes it"
	}
	for k, v := range map[string]any{
		"held_at": now, "pingback_requested_at": now, "pingback_count": strconv.Itoa(count + 1),
		"pingback_by": actor, "pingback_source": "reroute", "pingback_reason": note,
	} {
		fields[k] = v
	}
	if err := s.mergeBeadFields(ctx, b.ID, fields); err != nil {
		return err
	}
	bead, err := s.store.GetBead(ctx, b.ID)
	if err != nil || bead == nil {
		return err
	}
	s.recordAndPublish(ctx, events.TopicBeadUpdated, bead.ID, actor, events.BeadUpdated{
		Bead:    bead,
		Changes: map[string]any{"held_by": fields["held_by"], "execute_by": fields["execute_by"]},
	})
	return nil
}
