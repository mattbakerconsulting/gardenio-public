package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// gd-Nu4lxqkWwL: gardener run -iph2 could not reflag gd-jwh6fT9rik, which run
// -xofj of the same seat filed; the ownership gate saw only
// created_by=...-xofj. Reuses the seat / agent beads of
// decision_artifact_seat_test.go.

const rsBead = "gd-r3flagSeat"

var rsLabels = []string{"project:gardenio"}

// rsPingbackFields is the field patch gb escalation reflag sends.
var rsPingbackFields = map[string]any{
	"reflag_count": "1", "reflagged_at": "2026-10-30T12:06:00Z", "reflagged_by": dasHeir,
	"pingback_requested_at": "2026-10-30T12:06:00Z", "pingback_count": "1", "pingback_by": dasHeir,
	"pingback_source": "reflag", "pingback_reason": "stale",
}

// reflagSeatFixture seeds an open P2 bead of the given type that the earlier
// occupant filed on dasSeat, plus the seat's later occupant (gd-heir) and an
// agent of another seat (gd-other).
func reflagSeatFixture(t *testing.T, typ model.BeadType, status model.Status) (*mockStore, http.Handler) {
	t.Helper()
	ms, h := decisionArtifactSeatFixture(t, nil)
	fields := map[string]any{"prompt": "Grant access?", "owner_seat": dasSeat}
	if typ == model.TypeEscalation {
		fields["operation_type"] = "manual_operator_action"
		fields["operation_spec"] = map[string]any{"x": 1}
	}
	raw, _ := json.Marshal(fields)
	ms.beads[rsBead] = &model.Bead{
		ID: rsBead, Kind: model.KindData, Type: typ, Title: "Grant access?", Status: status,
		Priority: 2, CreatedBy: dasFiler, Assignee: dasFiler, Fields: raw,
	}
	ms.labels[rsBead] = slices.Clone(rsLabels)
	return ms, h
}

func TestReflagBySeat(t *testing.T) {
	heir := agentToken("gd-heir", dasHeir)
	reflagFields := map[string]any{"fields": map[string]any{
		"reflag_count": "1", "reflagged_at": "2026-10-30T12:06:00Z", "reflagged_by": dasHeir, "reflag_reason": "stale",
	}}
	pingbackFields := map[string]any{"fields": rsPingbackFields}
	escalatedLabels := map[string]any{"labels": append(slices.Clone(rsLabels), "escalated")}

	for _, c := range []struct {
		name   string
		typ    model.BeadType
		status model.Status
		body   map[string]any
		auth   func(*http.Request)
		want   int
	}{
		// runReflag's three writes, each its own PATCH, by the seat's successor.
		{"successor lowers decision priority", model.TypeDecision, model.StatusOpen, map[string]any{"priority": 1}, heir, http.StatusOK},
		{"successor jumps decision to P0", model.TypeDecision, model.StatusOpen, map[string]any{"priority": 0}, heir, http.StatusOK},
		{"successor writes decision reflag fields", model.TypeDecision, model.StatusOpen, reflagFields, heir, http.StatusOK},
		{"successor adds escalated label", model.TypeDecision, model.StatusOpen, escalatedLabels, heir, http.StatusOK},
		{"successor on an in_progress decision", model.TypeDecision, model.StatusInProgress, map[string]any{"priority": 1}, heir, http.StatusOK},

		// Identity.
		{"agent of another seat", model.TypeDecision, model.StatusOpen, map[string]any{"priority": 1}, agentToken("gd-other", "gardenio-something-else-ab12"), http.StatusForbidden},
		{"seat_key matches but subject does not occupy the seat", model.TypeDecision, model.StatusOpen, map[string]any{"priority": 1}, agentToken("gd-heir", "gardenio-something-else-ab12"), http.StatusForbidden},
		{"agent bead missing", model.TypeDecision, model.StatusOpen, map[string]any{"priority": 1}, agentToken("gd-nope", dasHeir), http.StatusForbidden},

		// Priority moves only toward P0.
		{"priority raised", model.TypeDecision, model.StatusOpen, map[string]any{"priority": 3}, heir, http.StatusForbidden},
		{"priority equal", model.TypeDecision, model.StatusOpen, map[string]any{"priority": 2}, heir, http.StatusForbidden},
		{"priority negative", model.TypeDecision, model.StatusOpen, map[string]any{"priority": -1}, heir, http.StatusForbidden},

		// Nothing else rides along.
		{"pingback fields on a decision", model.TypeDecision, model.StatusOpen, pingbackFields, heir, http.StatusForbidden},
		{"answer field", model.TypeDecision, model.StatusOpen, map[string]any{"fields": map[string]any{"reflag_count": "1", "chosen": "grant"}}, heir, http.StatusForbidden},
		{"responded_by", model.TypeDecision, model.StatusOpen, map[string]any{"fields": map[string]any{"responded_by": dasHeir}}, heir, http.StatusForbidden},
		{"another field", model.TypeDecision, model.StatusOpen, map[string]any{"fields": map[string]any{"reflag_count": "1", "prompt": "x"}}, heir, http.StatusForbidden},
		{"owner_seat rewrite", model.TypeDecision, model.StatusOpen, map[string]any{"fields": map[string]any{"owner_seat": "gardenio/crew/x"}}, heir, http.StatusForbidden},
		{"status", model.TypeDecision, model.StatusOpen, map[string]any{"priority": 1, "status": "closed"}, heir, http.StatusForbidden},
		{"title", model.TypeDecision, model.StatusOpen, map[string]any{"priority": 1, "title": "x"}, heir, http.StatusForbidden},
		{"assignee", model.TypeDecision, model.StatusOpen, map[string]any{"assignee": dasHeir}, heir, http.StatusForbidden},
		{"priority with fields", model.TypeDecision, model.StatusOpen, map[string]any{"priority": 1, "fields": map[string]any{"reflag_count": "1"}}, heir, http.StatusForbidden},
		{"priority with labels", model.TypeDecision, model.StatusOpen, map[string]any{"priority": 1, "labels": append(slices.Clone(rsLabels), "escalated")}, heir, http.StatusForbidden},
		{"label other than escalated", model.TypeDecision, model.StatusOpen, map[string]any{"labels": append(slices.Clone(rsLabels), "urgent")}, heir, http.StatusForbidden},
		{"escalated plus another label", model.TypeDecision, model.StatusOpen, map[string]any{"labels": append(slices.Clone(rsLabels), "escalated", "urgent")}, heir, http.StatusForbidden},
		{"label removed", model.TypeDecision, model.StatusOpen, map[string]any{"labels": []string{"escalated"}}, heir, http.StatusForbidden},
		{"labels unchanged", model.TypeDecision, model.StatusOpen, map[string]any{"labels": slices.Clone(rsLabels)}, heir, http.StatusForbidden},

		// Bead shape.
		{"closed decision", model.TypeDecision, model.StatusClosed, map[string]any{"priority": 1}, heir, http.StatusForbidden},
		{"task with owner_seat", model.TypeTask, model.StatusOpen, map[string]any{"priority": 1}, heir, http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			ms, h := reflagSeatFixture(t, c.typ, c.status)
			rec := doFilerJSON(h, "PATCH", "/v1/beads/"+rsBead, c.body, c.auth)
			if rec.Code != c.want {
				t.Fatalf("expected %d, got %d: %s", c.want, rec.Code, rec.Body.String())
			}
			if c.want != http.StatusForbidden {
				return
			}
			got := ms.beads[rsBead]
			if got.Priority != 2 || got.Status != c.status || !slices.Equal(ms.labels[rsBead], rsLabels) || got.Assignee != dasFiler {
				t.Fatalf("refused write changed the bead: priority=%d status=%s labels=%v assignee=%s",
					got.Priority, got.Status, ms.labels[rsBead], got.Assignee)
			}
		})
	}
}

// The escalation arm, called directly: the shared-queue carve-out in
// authorizeOwnership (ka-0000002032) already admits any fleet token to an
// escalation, so a PATCH cannot show this grant refusing one.
func TestReflagBySeat_Escalation(t *testing.T) {
	ms, _ := reflagSeatFixture(t, model.TypeEscalation, model.StatusOpen)
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	ctxFor := func(beadID, subject string) context.Context {
		ctx := withAuthMethod(context.Background(), authMethodStatic)
		ctx = WithFleet(ctx, agentFleetPrefix+beadID)
		return WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: subject})
	}
	heir := ctxFor("gd-heir", dasHeir)
	fields := func(m map[string]any) json.RawMessage { raw, _ := json.Marshal(m); return raw }
	prio := func(p int) *int { return &p }
	for _, c := range []struct {
		name   string
		ctx    context.Context
		status model.Status
		in     updateBeadInput
		want   bool
	}{
		{"priority down", heir, model.StatusOpen, updateBeadInput{Priority: prio(1)}, true},
		{"pingback fields", heir, model.StatusOpen, updateBeadInput{Fields: fields(rsPingbackFields)}, true},
		{"escalated label", heir, model.StatusOpen, updateBeadInput{Labels: append(slices.Clone(rsLabels), "escalated"), labelsSet: true}, true},
		{"other seat", ctxFor("gd-other", "gardenio-something-else-ab12"), model.StatusOpen, updateBeadInput{Priority: prio(1)}, false},
		{"priority up", heir, model.StatusOpen, updateBeadInput{Priority: prio(3)}, false},
		{"operation_spec", heir, model.StatusOpen, updateBeadInput{Fields: fields(map[string]any{"operation_spec": map[string]any{"y": 2}})}, false},
		{"owner_seat", heir, model.StatusOpen, updateBeadInput{Fields: fields(map[string]any{"owner_seat": "gardenio/crew/x"})}, false},
		{"closed", heir, model.StatusClosed, updateBeadInput{Priority: prio(1)}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			bead, _ := ms.GetBead(context.Background(), rsBead)
			bead.Status = c.status
			if got := s.authorizeReflagBySeat(c.ctx, bead, &c.in); got != c.want {
				t.Fatalf("authorizeReflagBySeat = %v, want %v", got, c.want)
			}
		})
	}
}

// A bead with no owner_seat has no seat to succeed to.
func TestReflagBySeat_NoOwnerSeatRefused(t *testing.T) {
	ms, h := reflagSeatFixture(t, model.TypeDecision, model.StatusOpen)
	ms.beads[rsBead].Fields = json.RawMessage(`{"prompt":"Grant access?"}`)
	rec := doFilerJSON(h, "PATCH", "/v1/beads/"+rsBead, map[string]any{"priority": 1}, agentToken("gd-heir", dasHeir))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLabelsAddOnlyEscalated(t *testing.T) {
	cur := []string{"a", "b"}
	for _, c := range []struct {
		name    string
		current []string
		next    []string
		want    bool
	}{
		{"adds escalated", cur, []string{"a", "b", "escalated"}, true},
		{"adds escalated, reordered", cur, []string{"escalated", "b", "a"}, true},
		{"adds escalated to none", nil, []string{"escalated"}, true},
		{"already escalated", []string{"a", "escalated"}, []string{"a", "escalated"}, false},
		{"no escalated", cur, []string{"a", "b"}, false},
		{"drops one", cur, []string{"a", "escalated"}, false},
		{"adds another", cur, []string{"a", "b", "escalated", "c"}, false},
	} {
		if got := labelsAddOnlyEscalated(c.current, c.next); got != c.want {
			t.Errorf("%s: labelsAddOnlyEscalated(%v, %v) = %v, want %v", c.name, c.current, c.next, got, c.want)
		}
	}
}
