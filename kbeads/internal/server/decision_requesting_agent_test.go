package server

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Server-stamped requesting agent on a decision (gd-VuYwGGYLLT): the same
// never-trust-the-body treatment escalation_requesting_agent.go gives
// escalations, so the bridge's respawn and the continuation gate can rely on it.

func decisionFields(t *testing.T, extra map[string]any) json.RawMessage {
	t.Helper()
	opts := []map[string]string{{"id": "a", "short": "A", "label": "do A"}, {"id": "b", "short": "B", "label": "do B"}}
	f := map[string]any{"prompt": "which?", "options": opts}
	for k, v := range extra {
		f[k] = v
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// createDecision runs the create guard on a decision body as ctx's caller.
func createDecision(t *testing.T, ctx context.Context, extra map[string]any) map[string]string {
	t.Helper()
	srv := NewBeadsServer(newMockStore(), &events.NoopPublisher{})
	bead := &model.Bead{Type: model.TypeDecision, Fields: decisionFields(t, extra)}
	srv.applyDecisionRequestingAgentGuard(ctx, bead)
	return parseFieldsToStringMap(bead.Fields)
}

// The guard is wired into createBead, not just correct in isolation.
func TestDecisionCreate_GuardRunsOnCreateBead(t *testing.T) {
	srv := NewBeadsServer(newMockStore(), &events.NoopPublisher{})
	bead, err := srv.createBead(agentFleetCtx("gd-crew", "crew-a"), createBeadInput{
		Title: "which?", Type: "decision", Kind: "data", Labels: []string{"project:gardenio"},
		Fields: decisionFields(t, map[string]any{"requesting_agent_bead_id": "gd-captain", "chosen": "a"}),
	})
	if err != nil {
		t.Fatalf("create decision: %v", err)
	}
	f := parseFieldsToStringMap(bead.Fields)
	if f["requesting_agent_bead_id"] != "gd-crew" || f["requesting_agent_verified"] != "true" || f["chosen"] != "" {
		t.Errorf("createBead did not apply the guard: %v", f)
	}
}

func TestDecisionCreate_RequestingAgentStampedFromAgentCredential(t *testing.T) {
	// The body names a captain's bead; the credential is a crew agent's.
	f := createDecision(t, agentFleetCtx("gd-crew", "crew-a"), map[string]any{
		"requesting_agent_bead_id":  "gd-captain",
		"requesting_agent_verified": "true",
	})
	if f["requesting_agent_bead_id"] != "gd-crew" {
		t.Errorf("requesting_agent_bead_id = %q, want the credential's gd-crew", f["requesting_agent_bead_id"])
	}
	if f["requesting_agent_verified"] != "true" {
		t.Errorf("requesting_agent_verified = %q, want true for a credential stamp", f["requesting_agent_verified"])
	}
}

func TestDecisionCreate_NoVerifiedMarkerWithoutAgentCredential(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"shared static token":       WithAuthIdentity(staticCtx(), AuthIdentity{Type: "static", Subject: "static"}),
		"static token with X-Actor": WithAuthIdentity(staticCtx(), AuthIdentity{Type: "static", Subject: "crew-a"}),
		"oidc human":                oidcCtx("alice@example.com", []string{RoleOperator}),
		"non-agent fleet":           fleetCtx("some-bridge"),
	} {
		t.Run(name, func(t *testing.T) {
			f := createDecision(t, ctx, map[string]any{
				"requesting_agent_bead_id":  "gd-captain",
				"requesting_agent_verified": "true", // a client-supplied marker must not survive
			})
			if f["requesting_agent_verified"] != "" {
				t.Errorf("requesting_agent_verified = %q, want it cleared", f["requesting_agent_verified"])
			}
			if f["requesting_agent_bead_id"] != "gd-captain" {
				t.Errorf("requesting_agent_bead_id = %q, want the supplied value kept for routing", f["requesting_agent_bead_id"])
			}
		})
	}
}

// A decision cannot be born already answered by a caller that may not answer it,
// or a bare close would carry a forged answer to the bridge.
func TestDecisionCreate_AnswerFieldsStrippedForNonResolvers(t *testing.T) {
	answered := map[string]any{"chosen": "a", "responded_by": "alice@example.com", "responded_at": "2026-10-17T00:00:00Z", "response_text": "yes", "rationale": "because"}
	for name, ctx := range map[string]context.Context{
		"agent token":  agentFleetCtx("gd-crew", "crew-a"),
		"static token": WithAuthIdentity(staticCtx(), AuthIdentity{Type: "static", Subject: "static"}),
	} {
		t.Run(name, func(t *testing.T) {
			f := createDecision(t, ctx, answered)
			for _, k := range []string{"chosen", "responded_by", "responded_at", "response_text"} {
				if f[k] != "" {
					t.Errorf("%s = %q, want it stripped", k, f[k])
				}
			}
			if f["rationale"] != "because" {
				t.Errorf("rationale = %q, want it kept (it carries no authority)", f["rationale"])
			}
		})
	}
	t.Run("bridge keeps its answer fields", func(t *testing.T) {
		f := createDecision(t, slackBridgeMachineCtx(), map[string]any{"chosen": "a"})
		if f["chosen"] != "a" {
			t.Errorf("chosen = %q, a resolver principal may create an answered decision", f["chosen"])
		}
	})
}

func TestDecisionRequestingAgent_ImmutableOnPatch(t *testing.T) {
	prior := json.RawMessage(`{"prompt":"x","requesting_agent_bead_id":"gd-crew","requesting_agent_verified":"true"}`)
	mk := func(fields string) *model.Bead {
		return &model.Bead{Type: model.TypeDecision, Fields: json.RawMessage(fields)}
	}
	for name, tc := range map[string]struct {
		fields  string
		wantErr bool
	}{
		"unchanged echo":            {`{"prompt":"y","requesting_agent_bead_id":"gd-crew","requesting_agent_verified":"true"}`, false},
		"repointed at a captain":    {`{"prompt":"x","requesting_agent_bead_id":"gd-captain","requesting_agent_verified":"true"}`, true},
		"requester removed":         {`{"prompt":"x","requesting_agent_verified":"true"}`, true},
		"marker removed":            {`{"prompt":"x","requesting_agent_bead_id":"gd-crew"}`, true},
		"marker set on unverified":  {`{"prompt":"x","requesting_agent_bead_id":"gd-crew","requesting_agent_verified":"yes"}`, true},
		"unrelated field edit only": {`{"prompt":"edited","requesting_agent_bead_id":"gd-crew","requesting_agent_verified":"true"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkDecisionRequestingAgentImmutable(mk(tc.fields), prior)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
	// Marker cannot be added later to a decision that never had one.
	unstamped := json.RawMessage(`{"prompt":"x","requesting_agent_bead_id":"gd-captain"}`)
	if err := checkDecisionRequestingAgentImmutable(mk(`{"prompt":"x","requesting_agent_bead_id":"gd-captain","requesting_agent_verified":"true"}`), unstamped); err == nil {
		t.Error("adding the verified marker after create must be refused")
	}
	// Other bead types are untouched.
	other := &model.Bead{Type: "task", Fields: json.RawMessage(`{"requesting_agent_bead_id":"a"}`)}
	if err := checkDecisionRequestingAgentImmutable(other, json.RawMessage(`{"requesting_agent_bead_id":"b"}`)); err != nil {
		t.Errorf("non-decision bead must not be gated, got %v", err)
	}
}

func TestDecisionRequestingAgent_KeyWritesDeniedOnFieldEndpoints(t *testing.T) {
	dec := &model.Bead{Type: model.TypeDecision}
	for _, key := range []string{"requesting_agent_bead_id", "requesting_agent_verified"} {
		if d := denyDecisionStampedKeyWrite("update", dec, []string{"urgency", key}); d == nil {
			t.Errorf("writing %s by key must be denied", key)
		}
	}
	if d := denyDecisionStampedKeyWrite("update", dec, []string{"urgency", "rationale"}); d != nil {
		t.Errorf("unrelated keys must pass, got %v", d)
	}
	if d := denyDecisionStampedKeyWrite("update", &model.Bead{Type: "task"}, []string{"requesting_agent_bead_id"}); d != nil {
		t.Errorf("non-decision bead must not be gated, got %v", d)
	}
}
