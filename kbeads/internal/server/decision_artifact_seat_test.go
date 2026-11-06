package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// gd-JPfZCDbKFp: run -quc2 of a seat could not mark the artifact delivered on
// a decision run -qv9m of the same seat filed; the ownership gate saw only
// created_by=...-qv9m and every later gb stop on the seat was wedged.

const (
	dasSeat     = "gardenio/plan-gardener/gardenio-plan-gardener-for-gd-kax4iuai42"
	dasFiler    = "gardenio-plan-gardener-for-gd-kax4iuai42-qv9m"
	dasHeir     = "gardenio-plan-gardener-for-gd-kax4iuai42-quc2"
	dasDecision = "gd-x7Sgk4BVsJ"
)

// decisionArtifactSeatFixture seeds the closed, approved decision the earlier
// occupant filed, plus two agent beads: the seat's later occupant and an agent
// of another seat.
func decisionArtifactSeatFixture(t *testing.T, decisionFields map[string]any) (*mockStore, http.Handler) {
	t.Helper()
	ms, h, _ := decisionFilerFixture(t)
	fields := map[string]any{
		"prompt":            "Approve?",
		"chosen":            "approve",
		"owner_seat":        dasSeat,
		"required_artifact": "plan",
		"artifact_status":   "pending",
	}
	for k, v := range decisionFields {
		if v == nil {
			delete(fields, k)
		} else {
			fields[k] = v
		}
	}
	raw, _ := json.Marshal(fields)
	ms.beads[dasDecision] = &model.Bead{
		ID: dasDecision, Kind: model.KindData, Type: model.TypeDecision,
		Title: "Approve?", Status: model.StatusClosed, CreatedBy: dasFiler, Assignee: dasFiler,
		Fields: raw,
	}
	ms.beads["gd-heir"] = &model.Bead{
		ID: "gd-heir", Kind: model.KindConfig, Type: model.BeadType("agent"), Status: model.StatusOpen,
		Fields: json.RawMessage(`{"seat_key":"` + dasSeat + `"}`),
	}
	ms.beads["gd-other"] = &model.Bead{
		ID: "gd-other", Kind: model.KindConfig, Type: model.BeadType("agent"), Status: model.StatusOpen,
		Fields: json.RawMessage(`{"seat_key":"gardenio/crew/gardenio-something-else"}`),
	}
	return ms, h
}

// agentToken authenticates as a per-agent fleet token for agent bead beadID
// with the given server-minted subject.
func agentToken(beadID, subject string) func(*http.Request) {
	return func(r *http.Request) {
		ctx := withAuthMethod(r.Context(), authMethodStatic)
		ctx = WithFleet(ctx, agentFleetPrefix+beadID)
		ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: subject})
		*r = *r.WithContext(ctx)
	}
}

func artifactStatusOf(t *testing.T, ms *mockStore) string {
	t.Helper()
	got, _, _ := exactFieldString(ms.beads[dasDecision].Fields, "artifact_status")
	return got
}

func TestDecisionArtifactBySeat_SuccessorMarksSubmitted(t *testing.T) {
	ms, h := decisionArtifactSeatFixture(t, nil)
	rec := doFilerJSON(h, "PATCH", "/v1/beads/"+dasDecision,
		map[string]any{"fields": map[string]any{"artifact_status": "submitted"}}, agentToken("gd-heir", dasHeir))
	if rec.Code != http.StatusOK {
		t.Fatalf("seat successor should mark the artifact submitted, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := artifactStatusOf(t, ms); got != "submitted" {
		t.Fatalf("artifact_status = %q, want submitted", got)
	}
}

func TestDecisionArtifactBySeat_Refusals(t *testing.T) {
	submitted := map[string]any{"artifact_status": "submitted"}
	for _, c := range []struct {
		name    string
		seed    map[string]any // over the decision's fields; nil value drops one
		body    map[string]any
		beadID  string
		subject string
	}{
		{"agent of another seat", nil, map[string]any{"fields": submitted}, "gd-other", "gardenio-something-else-ab12"},
		// seat_key is a plain field on the agent bead; the subject comes from
		// the registered token, so rewriting one's own seat_key is not enough.
		{"seat_key matches but the token names another agent", nil, map[string]any{"fields": submitted}, "gd-heir", "gardenio-something-else-ab12"},
		{"subject extends the seat base with a further segment", nil, map[string]any{"fields": submitted}, "gd-heir", dasHeir + "-x1y2"},
		{"agent bead missing", nil, map[string]any{"fields": submitted}, "gd-nope", dasHeir},
		{"value other than submitted", nil, map[string]any{"fields": map[string]any{"artifact_status": "accepted"}}, "gd-heir", dasHeir},
		{"another field rides along", nil, map[string]any{"fields": map[string]any{"artifact_status": "submitted", "context": "x"}}, "gd-heir", dasHeir},
		{"answer field rides along", nil, map[string]any{"fields": map[string]any{"artifact_status": "submitted", "chosen": "change"}}, "gd-heir", dasHeir},
		{"a column rides along", nil, map[string]any{"fields": submitted, "status": "open"}, "gd-heir", dasHeir},
		{"decision declared no artifact", map[string]any{"required_artifact": nil}, map[string]any{"fields": submitted}, "gd-heir", dasHeir},
		{"decision has no owner_seat", map[string]any{"owner_seat": nil}, map[string]any{"fields": submitted}, "gd-heir", dasHeir},
	} {
		t.Run(c.name, func(t *testing.T) {
			ms, h := decisionArtifactSeatFixture(t, c.seed)
			rec := doFilerJSON(h, "PATCH", "/v1/beads/"+dasDecision, c.body, agentToken(c.beadID, c.subject))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
			if got := artifactStatusOf(t, ms); got != "pending" && got != "" {
				t.Fatalf("artifact_status = %q, want it untouched", got)
			}
		})
	}
}

// A closed agent bead is a past occupant, not the caller's live seat.
func TestDecisionArtifactBySeat_ClosedAgentBeadRefused(t *testing.T) {
	ms, h := decisionArtifactSeatFixture(t, nil)
	ms.beads["gd-heir"].Status = model.StatusClosed
	rec := doFilerJSON(h, "PATCH", "/v1/beads/"+dasDecision,
		map[string]any{"fields": map[string]any{"artifact_status": "submitted"}}, agentToken("gd-heir", dasHeir))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSubjectOccupiesSeat(t *testing.T) {
	for _, c := range []struct {
		subject, seat string
		want          bool
	}{
		{dasHeir, dasSeat, true},
		{dasFiler, dasSeat, true},
		{"gardenio-plan-gardener-for-gd-kax4iuai42", dasSeat, true},
		{"gardenio-plan-gardener-for-gd-kax4iuai42-ab", dasSeat, false},
		{"gardenio-plan-gardener-for-gd-kax4iuai42-abcdefg", dasSeat, false},
		{"gardenio-plan-gardener-for-gd-kax4iuai4", dasSeat, false},
		{"gardenio-plan-gardener-for-gd-kax4iuai42x-ab12", dasSeat, false},
		{dasHeir, "no-slash-seat", false},
		{"", dasSeat, false},
		{"x-abc", "proj/role/", false},
	} {
		if got := subjectOccupiesSeat(c.subject, c.seat); got != c.want {
			t.Errorf("subjectOccupiesSeat(%q, %q) = %v, want %v", c.subject, c.seat, got, c.want)
		}
	}
}
