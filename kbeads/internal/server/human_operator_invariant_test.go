package server

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/model"
)

// This file pins the "real operator for every bead" invariant
// (epic ka-0000004835, task gd-3oTe8gtwVE). The pieces already covered by
// operated_by_test.go / audit_resolver_test.go / authoring_human_gate_test.go /
// operated_by_chain_walk_test.go are NOT re-litigated here. What this file adds:
//
//  1. isHumanOperator rejection of the two DEGENERATE operator shapes the epic
//     baseline surfaced in live RDS — agent:<name> and email:<addr> — which an
//     agent/raw-email became its own operator (anti-pattern). slack:/oidc: pass.
//  2. PER-SPAWN-PATH coverage through the live-state audit gate
//     (resolveAuditOperatorRequired): a trigger/schedule/cron-spawned agent must
//     resolve to a HUMAN ancestor or REFUSE — never silently bind a sentinel.
//  3. A single narrowly-scoped system:machine allowlist test asserting the sink
//     is NEVER reached as a resolver FALLBACK — only via an explicit, code-
//     reviewed WithMachineOperator / WithSystemOperator call at an infra site.

// TestIsHumanOperator_DegenerateShapesRejected pins the acceptance bullet that
// isHumanOperator accepts ONLY slack:U… / oidc:…@… and rejects the
// sentinel/agent/email/system families. The two epic-baseline degenerates —
// agent:<name> (an agent became its own operator) and email:<addr> (a raw email
// stamped as operator, e.g. email:gardenio2@acme.example) — are the regression
// targets: neither carries the accountable-human guarantee the invariant
// requires, so both MUST fail even though "email:" superficially looks
// human-ish. oidc: is the only email-bearing human shape.
func TestIsHumanOperator_DegenerateShapesRejected(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Pass: the only two human shapes.
		{"slack:U0OPERATOR1", true},
		{"oidc:matthew.baker@acme.example", true},
		// Fail: degenerate operators observed live (epic baseline).
		{"agent:gardenio-localmode-e2e-runner", false},
		{"email:gardenio2@acme.example", false},
		// Fail: sentinel families.
		{"system:machine", false},
		{"system:legacy-pre-attribution", false},
		{"fleet:slack-bridge", false},
		// Fail: bare agent / spawn-path identities.
		{"gardenio-tests-resolvergatesanitizer", false},
		{"sched-gardenio-gardener-release-qjj", false},
		{"thread-1782239959-818519", false},
	}
	for _, tc := range cases {
		if got := isHumanOperator(tc.in); got != tc.want {
			t.Errorf("isHumanOperator(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

// spawnPathBead builds the agent bead whose spawn-chain root is an autonomous
// (trigger/schedule) bead — modelling the controller stamping
// spawned_by → <autonomous-bead-id> on the agent it spawns. The autonomous
// bead carries the supplied fields (human attribution or none).
func spawnPathBeads(rootType model.BeadType, rootFields string) []*model.Bead {
	return []*model.Bead{
		{ID: "crew", Type: "agent", Fields: []byte(`{"spawned_by":"autobead"}`)},
		{ID: "autobead", Type: rootType, Fields: []byte(rootFields)},
	}
}

// TestAuditGate_PerSpawnPath_HumanOrRefuse is the core per-spawn-path invariant
// (acceptance bullet 2). For EACH autonomous spawn origin — trigger, schedule,
// and a cron gardener (a schedule bead carrying a gardener/cron loop-spec) — an
// agent spawned from it must, under the authoring human gate:
//
//   - resolve to the HUMAN author when the originating bead carries one, OR
//   - REFUSE (ok=false, empty value) when it does not —
//
// and NEVER silently bind a sentinel (system:legacy-pre-attribution) or the
// machine sink (system:machine). This drives the live-state audit gate
// resolveAuditOperatorRequired, which is the choke point every audit-bearing
// write (gb escalation / gb squawk / kd comment / createBead) funnels through.
func TestAuditGate_PerSpawnPath_HumanOrRefuse(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")

	spawnPaths := []struct {
		name     string
		rootType model.BeadType
		// human attribution present on the originating autonomous bead
		humanFields string
		// no human attribution: an autonomous bead authored by a machine /
		// sentinel, which is exactly the gap the directive closes.
		gaplessFields string
	}{
		{
			name:          "trigger-spawned",
			rootType:      "trigger",
			humanFields:   `{"slack_user_id":"U0OPERATOR1"}`,
			gaplessFields: `{"operated_by":"system:legacy-pre-attribution"}`,
		},
		{
			name:          "schedule-spawned",
			rootType:      "schedule",
			humanFields:   `{"operated_by":"oidc:matthew.baker@acme.example"}`,
			gaplessFields: `{"action":"notify"}`,
		},
		{
			name:          "cron-gardener-spawned",
			rootType:      "schedule",
			humanFields:   `{"user_bead_id":"user-matt","cron":"0 * * * *"}`,
			gaplessFields: `{"cron":"0 * * * *","operated_by":"fleet:scheduler"}`,
		},
	}

	for _, sp := range spawnPaths {
		t.Run(sp.name+"/human-author-resolves", func(t *testing.T) {
			beads := spawnPathBeads(sp.rootType, sp.humanFields)
			// cron-gardener path resolves via a user bead hanging off the root.
			beads = append(beads, &model.Bead{
				ID: "user-matt", Type: "user",
				Fields: []byte(`{"slack_id":"U0OPERATOR1"}`),
			})
			srv := newChainWalkServer(beads...)

			value, id, _, ok := srv.resolveAuditOperatorRequired(staticAuthCtx("crew"))
			if !ok {
				t.Fatalf("%s human author REFUSED; want resolved to a human", sp.name)
			}
			if !isHumanOperator(value) {
				t.Fatalf("%s resolved %q; want a human shape (slack:U… / oidc:…@…)", sp.name, value)
			}
			if id == 0 {
				t.Fatalf("%s human operator FK = 0; want non-zero", sp.name)
			}
		})

		t.Run(sp.name+"/no-human-refuses", func(t *testing.T) {
			srv := newChainWalkServer(spawnPathBeads(sp.rootType, sp.gaplessFields)...)

			value, id, _, ok := srv.resolveAuditOperatorRequired(staticAuthCtx("crew"))
			if ok {
				t.Fatalf("%s with no human author resolved (%q, id=%d); want REFUSE", sp.name, value, id)
			}
			if value != "" {
				t.Fatalf("%s refusal value = %q; want empty (no sentinel/machine fallback)", sp.name, value)
			}
			// The two silent-bind outcomes the directive forbids.
			if value == operatorMachine {
				t.Fatalf("%s silently bound the machine sink; want REFUSE", sp.name)
			}
			if value == operatorLegacyPreAttribution {
				t.Fatalf("%s silently bound the legacy sentinel; want REFUSE", sp.name)
			}
			if id != 0 {
				t.Fatalf("%s refusal id = %d; want 0", sp.name, id)
			}
		})
	}
}

// TestAuditGate_PerSpawnPath_SanitizerRejectsAgentHeader_ThenRefuses ties the
// sanitizer path to the per-spawn-path refusal classification. A
// trigger-spawned agent that forwards its own agent name as X-Operated-By
// (the gardener-pushed-its-own-id bug) is rejected at the sanitizer boundary;
// with no walkable human ancestor the audit gate then REFUSES and the refusal
// is classified header_non_human — NOT a silent sentinel bind.
func TestAuditGate_PerSpawnPath_SanitizerRejectsAgentHeader_ThenRefuses(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newChainWalkServer(spawnPathBeads("trigger", `{"action":"notify"}`)...)

	// Agent forwards a non-human X-Operated-By; sanitizer must discard it.
	ctx := applyOperatedByHeader(staticAuthCtx("crew"),
		"sched-gardenio-gardener-release-qjj", "static", "test")
	if h := operatedByHeaderFromContext(ctx); h != "" {
		t.Fatalf("sanitizer accepted agent-shaped header %q; want rejection", h)
	}

	value, id, source, ok := srv.resolveAuditOperatorRequired(ctx)
	if ok {
		t.Fatalf("resolved (%q, id=%d); want REFUSE after sanitizer rejection", value, id)
	}
	if value != "" {
		t.Fatalf("refusal value = %q; want empty", value)
	}
	if source != refusalSourceHeaderNonHuman {
		t.Fatalf("refusal source = %q; want %q", source, refusalSourceHeaderNonHuman)
	}
}

// TestSystemMachine_NeverAResolverFallback is the single, narrowly-scoped
// allowlist test the acceptance criteria require: system:machine
// (operatorMachine) is the ONE retained machine sink, but it is NOT a resolver
// fallback. Across every spawn path that lacks attribution — anonymous, a
// header-less fleet/service token, a static agent with no walkable human, and
// trigger/schedule/cron-spawned agents whose only attribution is a sentinel —
// the resolver MUST fall through to empty (→ refuse), never to system:machine.
// The sink is reachable ONLY via an explicit, code-reviewed WithMachineOperator
// / WithSystemOperator call (the positive case at the end).
func TestSystemMachine_NeverAResolverFallback(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")

	// A store with assorted attribution-less spawn beads to walk.
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew-sentinel", Type: "agent",
			Fields: []byte(`{"operated_by":"system:legacy-pre-attribution"}`),
		},
		&model.Bead{
			ID: "crew-trigger", Type: "agent",
			Fields: []byte(`{"spawned_by":"t1"}`),
		},
		&model.Bead{
			ID: "t1", Type: "trigger",
			Fields: []byte(`{"action":"notify"}`),
		},
		&model.Bead{
			ID: "crew-schedule", Type: "agent",
			Fields: []byte(`{"spawned_by":"s1"}`),
		},
		&model.Bead{
			ID: "s1", Type: "schedule",
			Fields: []byte(`{"operated_by":"fleet:scheduler"}`),
		},
	)

	noAttrib := []struct {
		name string
		ctx  context.Context
	}{
		{"anonymous", context.Background()},
		{"fleet-no-header", WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})},
		{"static-sentinel-agent", staticAuthCtx("crew-sentinel")},
		{"trigger-spawned-no-human", staticAuthCtx("crew-trigger")},
		{"schedule-spawned-no-human", staticAuthCtx("crew-schedule")},
	}
	for _, tc := range noAttrib {
		t.Run("fallthrough/"+tc.name, func(t *testing.T) {
			// Pure resolver must NOT invent the machine sink.
			if value, _ := srv.resolveOperatedByPure(tc.ctx); value == operatorMachine {
				t.Fatalf("%s: resolveOperatedByPure = %q; system:machine must never be a fallback", tc.name, value)
			}
			// Live-state audit gate must REFUSE, not bind the sink.
			value, id, _, ok := srv.resolveAuditOperatorRequired(tc.ctx)
			if ok {
				t.Fatalf("%s: audit gate resolved (%q, id=%d); want REFUSE", tc.name, value, id)
			}
			if value == operatorMachine {
				t.Fatalf("%s: audit gate bound %q as a fallback; forbidden", tc.name, operatorMachine)
			}
		})
	}

	// Positive control: the sink IS reachable, but ONLY via an explicit
	// WithMachineOperator call at an allowlisted infra site (no human behind
	// the write). This keeps genuine machine-origin writes alive after the
	// gate flips without making the sink a fallback for human-or-agent writes.
	t.Run("explicit-WithMachineOperator-reaches-sink", func(t *testing.T) {
		ctx := WithMachineOperator(withTestService(context.Background()))
		value, id, _, ok := srv.resolveAuditOperatorRequired(ctx)
		if !ok {
			t.Fatalf("explicit machine write REFUSED; want resolved to %q", operatorMachine)
		}
		if value != operatorMachine {
			t.Fatalf("explicit machine write value = %q; want %q", value, operatorMachine)
		}
		if id == 0 {
			t.Fatalf("machine operator FK = 0; want non-zero")
		}
	})
}
