package server

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Escalation requesting_agent is the agent BEAD ID, stamped from the per-agent
// credential, and the bridge and the continuation gate agree on it (gd-9VcIXkMAaq).
// Before this, a per-agent token stamped the agent NAME: the bridge resolves the
// requester with GetBead(requesting_agent), which has no name fallback, and the
// gate compared the field to the prior's name, so no value satisfied both.

// fileEscalation creates an escalation as ctx's caller the way `gb escalation
// create` does (requesting_agent from BOAT_AGENT_BEAD_ID, client-controlled),
// closes it with a successful result as an approver would, and returns its id.
func fileEscalation(t *testing.T, f *contFixture, ctx context.Context, body map[string]any) string {
	t.Helper()
	fields := map[string]any{"operation_type": "manual_intervention", "operation_spec": map[string]any{"action": "x"}}
	for k, v := range body {
		fields[k] = v
	}
	raw, _ := json.Marshal(fields)
	bead, err := f.srv.createBead(ctx, createBeadInput{
		Title: "esc", Type: "escalation", Kind: "data", Labels: []string{"project:gardenio"}, Fields: raw,
	})
	if err != nil {
		t.Fatalf("create escalation: %v", err)
	}
	stored := f.ms.beads[bead.ID]
	stored.Fields = setField(stored.Fields, "result_status", "success")
	stored.Status = model.StatusClosed
	return bead.ID
}

// The privileged escalation resume works end to end: the crew files on its own
// token, the bridge names the escalation and spawns by the requester's bead id.
func TestEscalationResume_PerAgentFiledPasses(t *testing.T) {
	f := newContFixture(t)
	f.agentIn("gd-prior", "gardenio", "reviewer", model.StatusClosed, "done")

	// The body carries what gb sends: the agent bead id. A forged one is overridden.
	esc := fileEscalation(t, f, agentFleetCtx("gd-prior", "reviewer-a"),
		map[string]any{"requesting_agent": "gd-captain"})
	got := parseFieldsToStringMap(f.ms.beads[esc].Fields)
	if got["requesting_agent"] != "gd-prior" || got["requesting_agent_verified"] != "true" {
		t.Fatalf("stamp = %v, want the filer's bead id, verified", got)
	}

	fields := bridgeFields("gardenio", "reviewer", "gd-prior", "escalation-resume", "escalation_bead_id", esc)
	if err := f.gate(slackBridgeMachineCtx(), fields); err != nil {
		t.Fatalf("privileged escalation resume through the bridge must pass, got %v", err)
	}
	// And the other agent's bead is not a prior this escalation proves.
	f.agentIn("gd-other", "gardenio", "reviewer", model.StatusClosed, "done")
	other := bridgeFields("gardenio", "reviewer", "gd-other", "escalation-resume", "escalation_bead_id", esc)
	requireEscalates(t, f.gate(slackBridgeMachineCtx(), other))
}

// A static-token filer can type any requesting_agent, including a captain's
// bead id, and with X-Actor can name itself or anyone; none of it resumes a
// privileged role.
func TestEscalationResume_StaticTokenFiledNamingCaptainDoesNotPass(t *testing.T) {
	f := newContFixture(t)
	f.agentIn("gd-cap", "gardenio", "captain", model.StatusClosed, "done")

	for name, ctx := range map[string]context.Context{
		"shared static token": WithOperatedByHeader(staticCtx(), "slack:U0OPERATOR1"),
	} {
		t.Run(name, func(t *testing.T) {
			id := fileEscalation(t, f, ctx, map[string]any{"requesting_agent": "gd-cap"})
			if got := parseFieldsToStringMap(f.ms.beads[id].Fields); got["requesting_agent_verified"] == "true" {
				t.Fatalf("static-token escalation was marked verified: %v", got)
			}
			fields := bridgeFields("gardenio", "captain", "gd-cap", "escalation-resume", "escalation_bead_id", id)
			requireEscalates(t, f.gate(slackBridgeMachineCtx(), fields))
		})
	}
}

// A caller cannot pre-set the marker, and cannot add or change it later.
func TestEscalationRequestingAgentVerified_NotCallerSettable(t *testing.T) {
	f := newContFixture(t)
	raw, _ := json.Marshal(map[string]any{
		"operation_type": "manual_intervention", "operation_spec": map[string]any{"action": "x"},
		"requesting_agent": "gd-cap", "requesting_agent_verified": "true",
	})
	if _, err := f.srv.createBead(WithOperatedByHeader(staticCtx(), "slack:U0OPERATOR1"), createBeadInput{
		Title: "forged", Type: "escalation", Kind: "data", Labels: []string{"project:gardenio"}, Fields: raw,
	}); err == nil {
		t.Fatal("create carrying requesting_agent_verified must be refused for a non-admin caller")
	}

	// The create guard also strips it for a caller the stamped-field check exempts.
	srv := f.srv
	b := &model.Bead{Type: model.TypeEscalation, Fields: raw}
	srv.applyEscalationRequestingAgentGuard(staticCtx(), b)
	if got := parseFieldsToStringMap(b.Fields); got["requesting_agent_verified"] != "" {
		t.Errorf("guard left a caller-supplied marker: %v", got)
	}

	// Immutable after create, whichever way it would move.
	prior := json.RawMessage(`{"requesting_agent":"gd-a","requesting_agent_verified":"true"}`)
	for name, post := range map[string]string{
		"removed": `{"requesting_agent":"gd-a"}`,
		"changed": `{"requesting_agent":"gd-a","requesting_agent_verified":"false"}`,
	} {
		if err := checkEscalationFieldsImmutable(context.Background(), &model.Bead{Type: model.TypeEscalation, Fields: json.RawMessage(post)}, prior, model.StatusOpen); err == nil {
			t.Errorf("%s: marker change must be refused", name)
		}
	}
	added := json.RawMessage(`{"requesting_agent":"gd-a"}`)
	if err := checkEscalationFieldsImmutable(context.Background(), &model.Bead{Type: model.TypeEscalation, Fields: prior}, added, model.StatusOpen); err == nil {
		t.Error("adding the marker to an unverified escalation must be refused")
	}
}

// requesting_agent_project comes from the filer's token scope, never the body,
// and is immutable after create: the spawn replay's task pre-claim runs in it
// (gd-G5PmhWXsF9).
func TestEscalationRequestingAgentProject_StampedFromToken(t *testing.T) {
	srv := newContFixture(t).srv
	raw := json.RawMessage(`{"requesting_agent":"gd-x","requesting_agent_project":"forged"}`)
	for name, tc := range map[string]struct {
		ctx  context.Context
		want string
	}{
		"project-bound per-agent token":   {WithTokenProject(agentFleetCtx("gd-a", "crew-a"), "gardenio"), "gardenio"},
		"per-agent token with no project": {agentFleetCtx("gd-a", "crew-a"), ""},
		"static token":                    {staticCtx(), ""},
	} {
		t.Run(name, func(t *testing.T) {
			b := &model.Bead{Type: model.TypeEscalation, Fields: raw}
			srv.applyEscalationRequestingAgentGuard(tc.ctx, b)
			if got := parseFieldsToStringMap(b.Fields)[escalationRequestingAgentProjectField]; got != tc.want {
				t.Errorf("requesting_agent_project = %q, want %q", got, tc.want)
			}
		})
	}

	prior := json.RawMessage(`{"requesting_agent":"gd-a","requesting_agent_verified":"true","requesting_agent_project":"gardenio"}`)
	for name, post := range map[string]string{
		"removed": `{"requesting_agent":"gd-a","requesting_agent_verified":"true"}`,
		"changed": `{"requesting_agent":"gd-a","requesting_agent_verified":"true","requesting_agent_project":"other"}`,
	} {
		if err := checkEscalationFieldsImmutable(context.Background(), &model.Bead{Type: model.TypeEscalation, Fields: json.RawMessage(post)}, prior, model.StatusOpen); err == nil {
			t.Errorf("%s: requesting_agent_project change must be refused", name)
		}
	}
}
