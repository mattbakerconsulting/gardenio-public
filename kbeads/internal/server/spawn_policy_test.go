package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// seedSpawnPolicyRequesterAgent seeds the type:agent bead resolveRequesterContext
// looks up by the requesting agent's own name (mirrors http_graph.go's
// Fields{"agent": name} lookup pattern).
func seedSpawnPolicyRequesterAgent(ms *mockStore, id, agentName, role, project, podNamespace string) {
	seedBead(ms, id, "agent", model.StatusOpen, nil, map[string]any{
		"agent":         agentName,
		"role":          role,
		"project":       project,
		"pod_namespace": podNamespace,
	})
}

// seedSpawnPolicyBead seeds a type:spawn_policy config bead for role, with the
// given rules marshaled verbatim as the `rules` field.
func seedSpawnPolicyBead(ms *mockStore, id, role string, rules []map[string]any) {
	seedBead(ms, id, "spawn_policy", model.StatusOpen, nil, map[string]any{
		"role":  role,
		"rules": rules,
	})
	ms.beads[id].Kind = model.KindConfig
}

// spawnPolicyAutoRule builds an "auto" rule literal for use in
// seedSpawnPolicyBead's rules slice.
func spawnPolicyAutoRule(when map[string]any, maxPerDay, maxConcurrent int) map[string]any {
	return map[string]any{
		"when":           when,
		"mode":           "auto",
		"max_per_day":    maxPerDay,
		"max_concurrent": maxConcurrent,
	}
}

// spawnPolicyHumanRule builds a "human" catch-all rule literal.
func spawnPolicyHumanRule(when map[string]any) map[string]any {
	return map[string]any{"when": when, "mode": "human"}
}

// seedSpawnEscalation seeds an open type:escalation bead shaped the way
// write_authz.go's handleEscalation creates one for a denied privileged
// agent spawn: operation_type "create", spawn_role set, operation_spec
// carrying the createBeadInput the caller originally attempted.
func seedSpawnEscalation(ms *mockStore, id, requestingAgent, spawnRole, agentName, project string) *model.Bead {
	spec := createBeadInput{
		Title: agentName,
		Type:  "agent",
		Labels: []string{
			"project:" + project,
		},
		Fields: mustMarshal(map[string]any{
			"agent":   agentName,
			"mode":    "crew",
			"role":    spawnRole,
			"project": project,
		}),
	}
	specRaw, _ := json.Marshal(spec)
	seedBead(ms, id, model.TypeEscalation, model.StatusOpen, []string{"auto:authz-escalation"}, map[string]any{
		"operation_type":   "create",
		"requesting_agent": requestingAgent,
		"context":          "project-scoped write denied: privileged role",
		"operation_spec":   json.RawMessage(specRaw),
		"spawn_role":       spawnRole,
	})
	// Mirrors handleEscalation (write_authz.go), which always stamps
	// CreatedBy from the authenticated identity — the same value it puts in
	// fields.requesting_agent on this path. evaluateSpawnEscalation resolves
	// requester context from CreatedBy (the trustworthy, server-derived
	// value), not the client-shaped requesting_agent field, so a fixture
	// that only sets the latter would silently fail requester_role/namespace
	// matching.
	ms.beads[id].CreatedBy = requestingAgent
	return ms.beads[id]
}

// newSpawnPolicyServer builds a mockStore-backed server with the requesting
// role bead required so replayEscalation's authorizeAgentSpawn bypass check
// (withApprovedSpawnReplay) finds a privileged type:role bead to compare
// against, mirroring escalation_privileged_spawn_e2e_test.go's makeRoleBead.
func newSpawnPolicyServer(t *testing.T, spawnRole string) (*BeadsServer, *mockStore) {
	t.Helper()
	s, ms := newAutoCloseServer()
	makeRoleBead(ms, "gd-role-"+spawnRole, spawnRole, true)
	return s, ms
}

// TestEvaluateSpawnEscalation_AutoApprovesWithinCap is the core gc-dogfood
// acceptance case (gd-vmLhgqydEa): a crew's k8s-live request matching an auto
// rule within its caps is approved within a single evaluator call, stamped
// system:spawn-policy/policy, and the agent bead is actually created.
func TestEvaluateSpawnEscalation_AutoApprovesWithinCap(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	s, ms := newSpawnPolicyServer(t, "k8s-live")
	ctx := context.Background()

	seedSpawnPolicyRequesterAgent(ms, "gd-gardener-1", "gardenio-gardener-1", "gardener", "gardenio", "gc-dogfood")
	seedSpawnPolicyBead(ms, "gd-policy-k8slive", "k8s-live", []map[string]any{
		spawnPolicyAutoRule(map[string]any{
			"project": "gardenio", "namespace": "gc-dogfood", "requester_role": "gardener",
		}, 6, 1),
	})
	esc := seedSpawnEscalation(ms, "gd-esc-1", "gardenio-gardener-1", "k8s-live", "gardenio-k8slive-crew-1", "gardenio")

	s.evaluateSpawnEscalation(ctx, esc)

	got := ms.beads["gd-esc-1"]
	if got.Status != model.StatusClosed {
		t.Fatalf("escalation status = %s, want closed (replay should close on success)", got.Status)
	}
	var f map[string]any
	_ = json.Unmarshal(got.Fields, &f)
	if f["approved_by"] != spawnPolicyActor {
		t.Fatalf("approved_by = %v, want %q", f["approved_by"], spawnPolicyActor)
	}
	if f["approval_method"] != "policy" {
		t.Fatalf("approval_method = %v, want \"policy\"", f["approval_method"])
	}
	if f["policy_rule"] != "k8s-live#0" {
		t.Fatalf("policy_rule = %v, want \"k8s-live#0\"", f["policy_rule"])
	}

	// The originally-denied agent spawn actually happened.
	var found *model.Bead
	for _, b := range ms.beads {
		if b.Type == "agent" && b.Title == "gardenio-k8slive-crew-1" {
			found = b
			break
		}
	}
	if found == nil {
		t.Fatal("expected the replayed agent bead to exist")
	}
	var agentFields map[string]any
	_ = json.Unmarshal(found.Fields, &agentFields)
	if agentFields["spawn_approval_mode"] != "auto" {
		t.Fatalf("spawned agent spawn_approval_mode = %v, want \"auto\"", agentFields["spawn_approval_mode"])
	}
}

// TestEvaluateSpawnEscalation_SecondConcurrentWaits covers acceptance
// criterion #2: a second concurrent request over max_concurrent is not
// auto-approved and stays open for an operator (no catch-all human rule
// here, so it falls through to "no match" and is denied+closed — the
// no-catch-all-rule policy shape).
func TestEvaluateSpawnEscalation_SecondConcurrentFallsThroughCap(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	s, ms := newSpawnPolicyServer(t, "k8s-live")
	ctx := context.Background()

	seedSpawnPolicyRequesterAgent(ms, "gd-gardener-1", "gardenio-gardener-1", "gardener", "gardenio", "gc-dogfood")
	seedSpawnPolicyBead(ms, "gd-policy-k8slive", "k8s-live", []map[string]any{
		spawnPolicyAutoRule(map[string]any{
			"project": "gardenio", "namespace": "gc-dogfood", "requester_role": "gardener",
		}, 6, 1),
	})

	// An already-running crew consumes the max_concurrent=1 budget.
	seedBead(ms, "gd-existing-crew", "agent", model.StatusInProgress, nil, map[string]any{
		"role": "k8s-live", "project": "gardenio", "spawn_approval_mode": "auto",
	})

	esc := seedSpawnEscalation(ms, "gd-esc-2", "gardenio-gardener-1", "k8s-live", "gardenio-k8slive-crew-2", "gardenio")

	s.evaluateSpawnEscalation(ctx, esc)

	got := ms.beads["gd-esc-2"]
	if got.Status != model.StatusClosed {
		t.Fatalf("over-cap escalation status = %s, want closed (denied — no catch-all rule)", got.Status)
	}
	var f map[string]any
	_ = json.Unmarshal(got.Fields, &f)
	if f["approved_by"] != nil {
		t.Fatalf("over-cap escalation must not carry approved_by, got %v", f["approved_by"])
	}
	if f["result_status"] != "failure" {
		t.Fatalf("result_status = %v, want \"failure\"", f["result_status"])
	}

	// No second agent bead was spawned.
	for _, b := range ms.beads {
		if b.Type == "agent" && b.Title == "gardenio-k8slive-crew-2" {
			t.Fatalf("over-cap request must not have spawned an agent, found %s", b.ID)
		}
	}
}

// TestEvaluateSpawnEscalation_OverCapFallsThroughToHumanRule covers
// acceptance criterion #3 (the Nth-in-a-day / over-cap shape): when an auto
// rule is over cap but a human catch-all rule follows it, the request falls
// through to that rule and is left open for an operator rather than denied.
func TestEvaluateSpawnEscalation_OverCapFallsThroughToHumanRule(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	s, ms := newSpawnPolicyServer(t, "k8s-live")
	ctx := context.Background()

	seedSpawnPolicyRequesterAgent(ms, "gd-gardener-1", "gardenio-gardener-1", "gardener", "gardenio", "gc-dogfood")
	seedSpawnPolicyBead(ms, "gd-policy-k8slive", "k8s-live", []map[string]any{
		spawnPolicyAutoRule(map[string]any{
			"project": "gardenio", "namespace": "gc-dogfood", "requester_role": "gardener",
		}, 6, 10), // max_per_day 6 — the 7th today is over cap.
		spawnPolicyHumanRule(map[string]any{}),
	})

	// Six agents already spawned today under this rule (role+project+auto).
	for i := 0; i < 6; i++ {
		id := seedID("gd-today-", i)
		seedBead(ms, id, "agent", model.StatusClosed, nil, map[string]any{
			"role": "k8s-live", "project": "gardenio", "spawn_approval_mode": "auto",
		})
		// seedBead leaves CreatedAt at its zero value; spawnPolicyWithinCaps'
		// daily-count query filters on CreatedAfter=start-of-today, so these
		// fixtures must be stamped "created today" to be counted at all.
		ms.beads[id].CreatedAt = time.Now().UTC()
	}

	esc := seedSpawnEscalation(ms, "gd-esc-7", "gardenio-gardener-1", "k8s-live", "gardenio-k8slive-crew-7", "gardenio")

	s.evaluateSpawnEscalation(ctx, esc)

	got := ms.beads["gd-esc-7"]
	if got.Status != model.StatusOpen {
		t.Fatalf("7th-today escalation status = %s, want open (parked on the human catch-all rule)", got.Status)
	}
	var f map[string]any
	_ = json.Unmarshal(got.Fields, &f)
	if f["approved_by"] != nil {
		t.Fatalf("7th-today escalation must not be auto-approved, got approved_by=%v", f["approved_by"])
	}
}

// TestEvaluateSpawnEscalation_NoPolicyBeadLeavesUntouched pins the opt-in
// model: a role with no type:spawn_policy bead is not touched by the
// evaluator at all — it stays exactly as handleEscalation left it.
func TestEvaluateSpawnEscalation_NoPolicyBeadLeavesUntouched(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	s, ms := newSpawnPolicyServer(t, "infra")
	ctx := context.Background()

	seedSpawnPolicyRequesterAgent(ms, "gd-gardener-1", "gardenio-gardener-1", "gardener", "gardenio", "gc-dogfood")
	esc := seedSpawnEscalation(ms, "gd-esc-nopolicy", "gardenio-gardener-1", "infra", "gardenio-infra-1", "gardenio")

	s.evaluateSpawnEscalation(ctx, esc)

	if got := ms.beads["gd-esc-nopolicy"].Status; got != model.StatusOpen {
		t.Fatalf("no-policy escalation status = %s, want open (untouched)", got)
	}
}

// TestEvaluateSpawnEscalation_DisabledIsNoop verifies the evaluator ships
// dark: with the flag unset, even a matching auto rule leaves the escalation
// untouched.
func TestEvaluateSpawnEscalation_DisabledIsNoop(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "")
	s, ms := newSpawnPolicyServer(t, "k8s-live")
	ctx := context.Background()

	seedSpawnPolicyRequesterAgent(ms, "gd-gardener-1", "gardenio-gardener-1", "gardener", "gardenio", "gc-dogfood")
	seedSpawnPolicyBead(ms, "gd-policy-k8slive", "k8s-live", []map[string]any{
		spawnPolicyAutoRule(map[string]any{}, 6, 1),
	})
	esc := seedSpawnEscalation(ms, "gd-esc-disabled", "gardenio-gardener-1", "k8s-live", "gardenio-k8slive-crew-d", "gardenio")

	s.evaluateSpawnEscalation(ctx, esc)

	if got := ms.beads["gd-esc-disabled"].Status; got != model.StatusOpen {
		t.Fatalf("disabled evaluator must not touch anything, got status=%s", got)
	}
}

// TestValidateSpawnPolicyFields_AutoRuleWithoutCapsRefused covers acceptance
// criterion #4: a type:spawn_policy rule with mode "auto" and no positive
// caps is refused at write time.
func TestValidateSpawnPolicyFields_AutoRuleWithoutCapsRefused(t *testing.T) {
	tests := []struct {
		name    string
		rules   []map[string]any
		wantErr bool
	}{
		{
			name:    "auto with both caps positive is accepted",
			rules:   []map[string]any{spawnPolicyAutoRule(map[string]any{}, 6, 1)},
			wantErr: false,
		},
		{
			name:    "human rule needs no caps",
			rules:   []map[string]any{spawnPolicyHumanRule(map[string]any{})},
			wantErr: false,
		},
		{
			name:    "auto with no caps at all is refused",
			rules:   []map[string]any{{"when": map[string]any{}, "mode": "auto"}},
			wantErr: true,
		},
		{
			name:    "auto with zero max_concurrent is refused",
			rules:   []map[string]any{spawnPolicyAutoRule(map[string]any{}, 6, 0)},
			wantErr: true,
		},
		{
			name:    "auto with negative max_per_day is refused",
			rules:   []map[string]any{spawnPolicyAutoRule(map[string]any{}, -1, 1)},
			wantErr: true,
		},
		{
			name:    "unknown mode is refused",
			rules:   []map[string]any{{"when": map[string]any{}, "mode": "sometimes"}},
			wantErr: true,
		},
		{
			name:    "missing mode is refused",
			rules:   []map[string]any{{"when": map[string]any{}}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := mustMarshal(map[string]any{"role": "k8s-live", "rules": tt.rules})
			reason := validateSpawnPolicyFields(raw)
			if tt.wantErr && reason == "" {
				t.Fatal("expected a rejection reason, got none")
			}
			if !tt.wantErr && reason != "" {
				t.Fatalf("expected no rejection, got %q", reason)
			}
		})
	}
}

// TestCreateSpawnPolicyBead_AutoRuleWithoutCapsRefusedAtWrite exercises the
// create-path wiring in beads.go end to end (not just the validator func in
// isolation), matching how gd-vmLhgqydEa's acceptance criterion is phrased:
// "a rule without caps refused at write".
func TestCreateSpawnPolicyBead_AutoRuleWithoutCapsRefusedAtWrite(t *testing.T) {
	s, _ := newAutoCloseServer()
	ctx := context.Background()

	_, err := s.createBead(ctx, createBeadInput{
		Title: "k8s-live spawn policy",
		Type:  "spawn_policy",
		Kind:  "config",
		Fields: mustMarshal(map[string]any{
			"role": "k8s-live",
			"rules": []map[string]any{
				{"when": map[string]any{}, "mode": "auto"},
			},
		}),
	})
	if err == nil {
		t.Fatal("expected create to be refused for an auto rule with no caps")
	}
}

// TestEvaluateSpawnEscalation_ForgedOperationSpecTypeNotReplayed is a
// regression test for the operator-confirmed exploit on PR #146: an
// escalation whose spawn_role matches a wildcard auto rule, but whose
// operation_spec describes an unrelated create (e.g. Type:"team"), must not
// be auto-approved and replayed — spawn_role alone said nothing about what
// operation_spec actually contains.
func TestEvaluateSpawnEscalation_ForgedOperationSpecTypeNotReplayed(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	s, ms := newSpawnPolicyServer(t, "k8s-live")
	ctx := context.Background()

	seedSpawnPolicyRequesterAgent(ms, "gd-gardener-1", "gardenio-gardener-1", "gardener", "gardenio", "gc-dogfood")
	seedSpawnPolicyBead(ms, "gd-policy-k8slive", "k8s-live", []map[string]any{
		spawnPolicyAutoRule(map[string]any{}, 6, 1), // wildcard auto rule
	})

	forgedSpec := createBeadInput{
		Type: "team",
		Fields: mustMarshal(map[string]any{
			"members": []string{"attacker"},
		}),
	}
	specRaw, _ := json.Marshal(forgedSpec)
	seedBead(ms, "gd-esc-forged-type", model.TypeEscalation, model.StatusOpen, []string{"auto:authz-escalation"}, map[string]any{
		"operation_type":   "create",
		"requesting_agent": "gardenio-gardener-1",
		"operation_spec":   json.RawMessage(specRaw),
		"spawn_role":       "k8s-live",
	})
	ms.beads["gd-esc-forged-type"].CreatedBy = "gardenio-gardener-1"

	s.evaluateSpawnEscalation(ctx, ms.beads["gd-esc-forged-type"])

	if got := ms.beads["gd-esc-forged-type"].Status; got != model.StatusOpen {
		t.Fatalf("forged operation_spec.Type escalation status = %s, want open (must not be auto-approved/replayed)", got)
	}
	for _, b := range ms.beads {
		if b.Type == "team" {
			t.Fatalf("forged team create must never be replayed, found team bead %s", b.ID)
		}
	}
}

// TestEvaluateSpawnEscalation_OperationSpecRoleMismatchNotReplayed covers the
// related shape: operation_spec.Type IS "agent" but its own role field
// disagrees with the escalation's spawn_role — spawn_role is what the
// evaluator's cap/rule matching keys on, so the two must agree or the
// caps being enforced don't describe the create actually being replayed.
func TestEvaluateSpawnEscalation_OperationSpecRoleMismatchNotReplayed(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	s, ms := newSpawnPolicyServer(t, "k8s-live")
	ctx := context.Background()

	seedSpawnPolicyRequesterAgent(ms, "gd-gardener-1", "gardenio-gardener-1", "gardener", "gardenio", "gc-dogfood")
	seedSpawnPolicyBead(ms, "gd-policy-k8slive", "k8s-live", []map[string]any{
		spawnPolicyAutoRule(map[string]any{}, 6, 1),
	})

	mismatchedSpec := createBeadInput{
		Type: "agent",
		Fields: mustMarshal(map[string]any{
			"agent": "gardenio-privileged-1", "role": "gardener", "project": "gardenio",
		}),
	}
	specRaw, _ := json.Marshal(mismatchedSpec)
	seedBead(ms, "gd-esc-role-mismatch", model.TypeEscalation, model.StatusOpen, []string{"auto:authz-escalation"}, map[string]any{
		"operation_type":   "create",
		"requesting_agent": "gardenio-gardener-1",
		"operation_spec":   json.RawMessage(specRaw),
		"spawn_role":       "k8s-live",
	})
	ms.beads["gd-esc-role-mismatch"].CreatedBy = "gardenio-gardener-1"

	s.evaluateSpawnEscalation(ctx, ms.beads["gd-esc-role-mismatch"])

	if got := ms.beads["gd-esc-role-mismatch"].Status; got != model.StatusOpen {
		t.Fatalf("operation_spec role/spawn_role mismatch escalation status = %s, want open (must not be auto-approved)", got)
	}
}

// TestEvaluateSpawnEscalation_ForgedRequestingAgentFieldIgnored verifies
// requester context is resolved from the escalation's CreatedBy (the
// server-derived, authenticated identity), never from the client-shaped
// fields.requesting_agent — otherwise an attacker whose actual identity
// carries no privileged role could still auto-approve by naming a gardener's
// agent bead as requesting_agent to borrow its role/namespace match keys.
func TestEvaluateSpawnEscalation_ForgedRequestingAgentFieldIgnored(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	s, ms := newSpawnPolicyServer(t, "k8s-live")
	ctx := context.Background()

	// The real (server-authenticated) creator has no privileged role.
	seedSpawnPolicyRequesterAgent(ms, "gd-attacker", "gardenio-attacker", "crew", "gardenio", "untrusted-ns")
	// A legit gardener whose role/namespace the attacker forges into
	// fields.requesting_agent.
	seedSpawnPolicyRequesterAgent(ms, "gd-gardener-1", "gardenio-gardener-1", "gardener", "gardenio", "gc-dogfood")

	seedSpawnPolicyBead(ms, "gd-policy-k8slive", "k8s-live", []map[string]any{
		spawnPolicyAutoRule(map[string]any{
			"project": "gardenio", "namespace": "gc-dogfood", "requester_role": "gardener",
		}, 6, 1),
	})

	esc := seedSpawnEscalation(ms, "gd-esc-forged-agent", "gardenio-gardener-1", "k8s-live", "gardenio-k8slive-crew-x", "gardenio")
	// seedSpawnEscalation stamps CreatedBy == requestingAgent, mirroring the
	// legitimate path; override it to the actual (untrusted) authenticated
	// creator to simulate the forgery — fields.requesting_agent still names
	// the gardener.
	esc.CreatedBy = "gardenio-attacker"

	s.evaluateSpawnEscalation(ctx, esc)

	got := ms.beads["gd-esc-forged-agent"]
	var f map[string]any
	_ = json.Unmarshal(got.Fields, &f)
	if f["approved_by"] == spawnPolicyActor {
		t.Fatalf("forged requesting_agent must not borrow the gardener's role/namespace, got approved_by=%v", f["approved_by"])
	}
}

// TestLookupSpawnPolicy_IgnoresClosedPolicyBead pins the Status:open filter:
// a closed (or otherwise non-open) type:spawn_policy bead must not be
// matched — a role whose only policy bead is closed is exactly the opt-in
// "no policy declared" case, not a policy that happens to deny everything.
func TestLookupSpawnPolicy_IgnoresClosedPolicyBead(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	s, ms := newSpawnPolicyServer(t, "k8s-live")
	ctx := context.Background()

	seedSpawnPolicyRequesterAgent(ms, "gd-gardener-1", "gardenio-gardener-1", "gardener", "gardenio", "gc-dogfood")
	seedSpawnPolicyBead(ms, "gd-policy-k8slive-closed", "k8s-live", []map[string]any{
		spawnPolicyAutoRule(map[string]any{}, 6, 1),
	})
	ms.beads["gd-policy-k8slive-closed"].Status = model.StatusClosed

	esc := seedSpawnEscalation(ms, "gd-esc-closedpolicy", "gardenio-gardener-1", "k8s-live", "gardenio-k8slive-crew-c", "gardenio")

	s.evaluateSpawnEscalation(ctx, esc)

	if got := ms.beads["gd-esc-closedpolicy"].Status; got != model.StatusOpen {
		t.Fatalf("a closed spawn_policy bead must not be matched, got status=%s (want open/untouched)", got)
	}
}

// TestEvaluateSpawnEscalationSynchronous_SkipsWhenSweepLockHeld pins the
// create-time path's use of spawnPolicyAdvisoryLockKey: when another
// replica (simulated here as the sweep tick) already holds the lock, the
// synchronous create-time evaluation must skip rather than run unlocked and
// risk double-counting a cap slot against a concurrent sweep evaluation.
func TestEvaluateSpawnEscalationSynchronous_SkipsWhenSweepLockHeld(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	s, ms := newSpawnPolicyServer(t, "k8s-live")
	ctx := context.Background()

	seedSpawnPolicyRequesterAgent(ms, "gd-gardener-1", "gardenio-gardener-1", "gardener", "gardenio", "gc-dogfood")
	seedSpawnPolicyBead(ms, "gd-policy-k8slive", "k8s-live", []map[string]any{
		spawnPolicyAutoRule(map[string]any{}, 6, 1),
	})
	esc := seedSpawnEscalation(ms, "gd-esc-locked", "gardenio-gardener-1", "k8s-live", "gardenio-k8slive-crew-l", "gardenio")

	ms.advisoryLocksMu.Lock()
	if ms.advisoryLocks == nil {
		ms.advisoryLocks = map[int64]bool{}
	}
	ms.advisoryLocks[spawnPolicyAdvisoryLockKey] = true
	ms.advisoryLocksMu.Unlock()

	s.evaluateSpawnEscalationSynchronous(ctx, esc)

	if got := ms.beads["gd-esc-locked"].Status; got != model.StatusOpen {
		t.Fatalf("create-time evaluation must skip while the sweep lock is held elsewhere, got status=%s", got)
	}
}

// seedID builds a deterministic bead id for loop-seeded fixtures.
func seedID(prefix string, i int) string {
	return prefix + string(rune('a'+i))
}
