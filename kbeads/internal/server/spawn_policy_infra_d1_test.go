package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// Pins the behaviour of the production type:spawn_policy bead for role `infra`
// (plan gd-xiV5hyhPrG P2, gd-UhObMuB6ZI), decision D1 "auto-crew": a crew or
// plan-captain asking for a gardenio infra agent is auto-approved under a
// 6/day, 2-concurrent cap; everything else waits for a person. The operator
// writes the bead by hand (type:spawn_policy is admin-gated), so this test is
// the only place its JSON is checked against the evaluator.

// infraD1PolicyJSON is the exact `fields` blob of the production bead.
const infraD1PolicyJSON = `{"role":"infra","rules":[
  {"when":{"project":"gardenio","requester_role":"crew"},"mode":"auto","max_per_day":6,"max_concurrent":2},
  {"when":{"project":"gardenio","requester_role":"plan-captain"},"mode":"auto","max_per_day":6,"max_concurrent":2},
  {"when":{},"mode":"human","approvers":["oidc:matthewbaker1110@gmail.com"]}
]}`

// seedInfraD1Policy seeds the production policy bead, optionally dropping the
// trailing catch-all human rule (the control case).
func seedInfraD1Policy(t *testing.T, ms *mockStore, withCatchAll bool) {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(infraD1PolicyJSON), &fields); err != nil {
		t.Fatalf("infraD1PolicyJSON is not valid JSON: %v", err)
	}
	if !withCatchAll {
		rules := fields["rules"].([]any)
		fields["rules"] = rules[:len(rules)-1]
	}
	seedBead(ms, "gd-policy-infra", "spawn_policy", model.StatusOpen, nil, fields)
	ms.beads["gd-policy-infra"].Kind = model.KindConfig
}

// seedInfraAgents seeds n auto-approved infra agents in the gardenio project.
// active ones count against max_concurrent; every one is stamped "created
// today" so it counts against max_per_day too.
func seedInfraAgents(ms *mockStore, prefix string, n int, status model.Status) {
	for i := 0; i < n; i++ {
		id := seedID(prefix, i)
		seedBead(ms, id, "agent", status, nil, map[string]any{
			"role": "infra", "project": "gardenio", "spawn_approval_mode": "auto",
		})
		ms.beads[id].CreatedAt = time.Now().UTC()
	}
}

func escalationFields(t *testing.T, b *model.Bead) map[string]any {
	t.Helper()
	var f map[string]any
	if err := json.Unmarshal(b.Fields, &f); err != nil {
		t.Fatalf("escalation %s fields: %v", b.ID, err)
	}
	return f
}

// assertInfraEscalationLeftOpen asserts the escalation is parked for a person:
// still open, no approver stamped, no agent spawned for it.
func assertInfraEscalationLeftOpen(t *testing.T, ms *mockStore, escID, agentName string) {
	t.Helper()
	got := ms.beads[escID]
	if got.Status != model.StatusOpen {
		t.Fatalf("escalation status = %s, want open (left for a human)", got.Status)
	}
	if f := escalationFields(t, got); f["approved_by"] != nil {
		t.Fatalf("escalation must carry no approved_by, got %v", f["approved_by"])
	}
	assertNoAgentTitled(t, ms, agentName)
}

func assertNoAgentTitled(t *testing.T, ms *mockStore, title string) {
	t.Helper()
	for _, b := range ms.beads {
		if b.Type == "agent" && b.Title == title {
			t.Fatalf("no agent %q may have been spawned, found %s", title, b.ID)
		}
	}
}

// assertInfraAutoApproved asserts the escalation was approved by the policy
// under wantRule and the agent bead was actually created.
func assertInfraAutoApproved(t *testing.T, ms *mockStore, escID, agentName, wantRule string) {
	t.Helper()
	got := ms.beads[escID]
	if got.Status != model.StatusClosed {
		t.Fatalf("escalation status = %s, want closed (replay closes on success)", got.Status)
	}
	f := escalationFields(t, got)
	if f["approved_by"] != spawnPolicyActor {
		t.Fatalf("approved_by = %v, want %q", f["approved_by"], spawnPolicyActor)
	}
	if f["approval_method"] != "policy" {
		t.Fatalf("approval_method = %v, want \"policy\"", f["approval_method"])
	}
	if f["policy_rule"] != wantRule {
		t.Fatalf("policy_rule = %v, want %q", f["policy_rule"], wantRule)
	}
	var agent *model.Bead
	for _, b := range ms.beads {
		if b.Type == "agent" && b.Title == agentName {
			agent = b
		}
	}
	if agent == nil {
		t.Fatalf("expected replayed agent %q to exist", agentName)
	}
	var af map[string]any
	_ = json.Unmarshal(agent.Fields, &af)
	if af["spawn_approval_mode"] != "auto" {
		t.Fatalf("spawned agent spawn_approval_mode = %v, want \"auto\"", af["spawn_approval_mode"])
	}
}

func newInfraD1Server(t *testing.T) (*BeadsServer, *mockStore) {
	t.Helper()
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	return newSpawnPolicyServer(t, "infra")
}

func TestSpawnPolicyInfraD1_PolicyJSONPassesWriteValidation(t *testing.T) {
	if reason := validateSpawnPolicyFields(json.RawMessage(infraD1PolicyJSON)); reason != "" {
		t.Fatalf("validateSpawnPolicyFields rejected the D1 policy: %s", reason)
	}
	// Negative control: the same validator does refuse an uncapped auto rule,
	// so the acceptance above is not vacuous.
	uncapped := `{"role":"infra","rules":[{"when":{},"mode":"auto"}]}`
	if reason := validateSpawnPolicyFields(json.RawMessage(uncapped)); reason == "" {
		t.Fatal("validator must refuse an uncapped auto rule")
	}
}

func TestSpawnPolicyInfraD1_CrewForGardenioAutoApproves(t *testing.T) {
	s, ms := newInfraD1Server(t)
	seedSpawnPolicyRequesterAgent(ms, "gd-crew-1", "gardenio-crew-1", "crew", "gardenio", "gardenio")
	seedInfraD1Policy(t, ms, true)
	esc := seedSpawnEscalation(ms, "gd-esc-crew", "gardenio-crew-1", "infra", "gardenio-infra-a", "gardenio")

	s.evaluateSpawnEscalation(context.Background(), esc)

	assertInfraAutoApproved(t, ms, "gd-esc-crew", "gardenio-infra-a", "infra#0")
}

func TestSpawnPolicyInfraD1_PlanCaptainForGardenioAutoApproves(t *testing.T) {
	s, ms := newInfraD1Server(t)
	seedSpawnPolicyRequesterAgent(ms, "gd-pc-1", "gardenio-plan-captain-1", "plan-captain", "gardenio", "gardenio")
	seedInfraD1Policy(t, ms, true)
	esc := seedSpawnEscalation(ms, "gd-esc-pc", "gardenio-plan-captain-1", "infra", "gardenio-infra-b", "gardenio")

	s.evaluateSpawnEscalation(context.Background(), esc)

	assertInfraAutoApproved(t, ms, "gd-esc-pc", "gardenio-infra-b", "infra#1")
}

// Two auto infra agents already running consume max_concurrent=2: the crew
// rule is over cap, falls through (the plan-captain rule does not match a
// crew), and parks on the human catch-all. Control: with one running it is
// still auto-approved, so the cap check is what flips the outcome.
func TestSpawnPolicyInfraD1_ConcurrentCapFallsThroughToHuman(t *testing.T) {
	t.Run("two active parks on the human rule", func(t *testing.T) {
		s, ms := newInfraD1Server(t)
		seedSpawnPolicyRequesterAgent(ms, "gd-crew-1", "gardenio-crew-1", "crew", "gardenio", "gardenio")
		seedInfraD1Policy(t, ms, true)
		seedInfraAgents(ms, "gd-active-", 2, model.StatusInProgress)
		esc := seedSpawnEscalation(ms, "gd-esc-cap2", "gardenio-crew-1", "infra", "gardenio-infra-c", "gardenio")

		s.evaluateSpawnEscalation(context.Background(), esc)

		assertInfraEscalationLeftOpen(t, ms, "gd-esc-cap2", "gardenio-infra-c")
	})
	t.Run("one active still auto-approves", func(t *testing.T) {
		s, ms := newInfraD1Server(t)
		seedSpawnPolicyRequesterAgent(ms, "gd-crew-1", "gardenio-crew-1", "crew", "gardenio", "gardenio")
		seedInfraD1Policy(t, ms, true)
		seedInfraAgents(ms, "gd-active-", 1, model.StatusInProgress)
		esc := seedSpawnEscalation(ms, "gd-esc-cap1", "gardenio-crew-1", "infra", "gardenio-infra-c", "gardenio")

		s.evaluateSpawnEscalation(context.Background(), esc)

		assertInfraAutoApproved(t, ms, "gd-esc-cap1", "gardenio-infra-c", "infra#0")
	})
}

// Six infra agents already spawned today (all finished, so max_concurrent is
// not what trips) make the 7th stay open. The caps are shared across both
// auto rules, so a plan-captain is held to the same day budget. Control: five
// spawned today still auto-approves.
func TestSpawnPolicyInfraD1_DailyCapFallsThroughToHuman(t *testing.T) {
	t.Run("seventh of the day stays open", func(t *testing.T) {
		s, ms := newInfraD1Server(t)
		seedSpawnPolicyRequesterAgent(ms, "gd-crew-1", "gardenio-crew-1", "crew", "gardenio", "gardenio")
		seedInfraD1Policy(t, ms, true)
		seedInfraAgents(ms, "gd-today-", 6, model.StatusClosed)
		esc := seedSpawnEscalation(ms, "gd-esc-7", "gardenio-crew-1", "infra", "gardenio-infra-d", "gardenio")

		s.evaluateSpawnEscalation(context.Background(), esc)

		assertInfraEscalationLeftOpen(t, ms, "gd-esc-7", "gardenio-infra-d")
	})
	t.Run("day budget is shared with the plan-captain rule", func(t *testing.T) {
		s, ms := newInfraD1Server(t)
		seedSpawnPolicyRequesterAgent(ms, "gd-pc-1", "gardenio-plan-captain-1", "plan-captain", "gardenio", "gardenio")
		seedInfraD1Policy(t, ms, true)
		seedInfraAgents(ms, "gd-today-", 6, model.StatusClosed)
		esc := seedSpawnEscalation(ms, "gd-esc-pc7", "gardenio-plan-captain-1", "infra", "gardenio-infra-d", "gardenio")

		s.evaluateSpawnEscalation(context.Background(), esc)

		assertInfraEscalationLeftOpen(t, ms, "gd-esc-pc7", "gardenio-infra-d")
	})
	t.Run("sixth of the day still auto-approves", func(t *testing.T) {
		s, ms := newInfraD1Server(t)
		seedSpawnPolicyRequesterAgent(ms, "gd-crew-1", "gardenio-crew-1", "crew", "gardenio", "gardenio")
		seedInfraD1Policy(t, ms, true)
		seedInfraAgents(ms, "gd-today-", 5, model.StatusClosed)
		esc := seedSpawnEscalation(ms, "gd-esc-6", "gardenio-crew-1", "infra", "gardenio-infra-d", "gardenio")

		s.evaluateSpawnEscalation(context.Background(), esc)

		assertInfraAutoApproved(t, ms, "gd-esc-6", "gardenio-infra-d", "infra#0")
	})
}

// A requester role outside {crew, plan-captain} is never auto-approved: it
// reaches the catch-all human rule and the escalation waits. With the
// catch-all removed the same request is denied and closed, proving the
// catch-all is what keeps it open rather than the request being ignored.
func TestSpawnPolicyInfraD1_ReviewerStaysOpenAndCatchAllIsLoadBearing(t *testing.T) {
	t.Run("reviewer parks on the human rule", func(t *testing.T) {
		s, ms := newInfraD1Server(t)
		seedSpawnPolicyRequesterAgent(ms, "gd-rev-1", "gardenio-reviewer-1", "reviewer", "gardenio", "gardenio")
		seedInfraD1Policy(t, ms, true)
		esc := seedSpawnEscalation(ms, "gd-esc-rev", "gardenio-reviewer-1", "infra", "gardenio-infra-e", "gardenio")

		s.evaluateSpawnEscalation(context.Background(), esc)

		assertInfraEscalationLeftOpen(t, ms, "gd-esc-rev", "gardenio-infra-e")
	})
	t.Run("control: without the catch-all the reviewer is denied and closed", func(t *testing.T) {
		s, ms := newInfraD1Server(t)
		seedSpawnPolicyRequesterAgent(ms, "gd-rev-1", "gardenio-reviewer-1", "reviewer", "gardenio", "gardenio")
		seedInfraD1Policy(t, ms, false)
		esc := seedSpawnEscalation(ms, "gd-esc-rev", "gardenio-reviewer-1", "infra", "gardenio-infra-e", "gardenio")

		s.evaluateSpawnEscalation(context.Background(), esc)

		got := ms.beads["gd-esc-rev"]
		if got.Status != model.StatusClosed {
			t.Fatalf("escalation status = %s, want closed (denied: no rule matched)", got.Status)
		}
		f := escalationFields(t, got)
		if f["result_status"] != "failure" {
			t.Fatalf("result_status = %v, want \"failure\"", f["result_status"])
		}
		if f["approved_by"] != nil {
			t.Fatalf("denied escalation must carry no approved_by, got %v", f["approved_by"])
		}
		assertNoAgentTitled(t, ms, "gardenio-infra-e")
	})
}

// A crew asking for an infra agent in another project is not covered by the
// gardenio auto rules. The crew requester is the same one that auto-approves
// for gardenio above, so only the project differs.
func TestSpawnPolicyInfraD1_OtherProjectStaysOpen(t *testing.T) {
	s, ms := newInfraD1Server(t)
	seedSpawnPolicyRequesterAgent(ms, "gd-crew-1", "gardenio-crew-1", "crew", "gardenio", "gardenio")
	seedInfraD1Policy(t, ms, true)
	esc := seedSpawnEscalation(ms, "gd-esc-claimlayer", "gardenio-crew-1", "infra", "claimlayer-infra-1", "claimlayer")

	s.evaluateSpawnEscalation(context.Background(), esc)

	assertInfraEscalationLeftOpen(t, ms, "gd-esc-claimlayer", "claimlayer-infra-1")
}
