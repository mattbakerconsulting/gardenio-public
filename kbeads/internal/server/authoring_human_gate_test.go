package server

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestIsAutonomousBeadType pins the set of bead types the authoring-time
// human-operator gate (ka-0000006012) applies to: trigger + schedule (the
// gardener loop-specs ride on those two types, so "gardener" is covered
// transitively). Everything else — including ordinary work items, decisions,
// and the data-kind types — is NOT autonomous and is unaffected by the gate.
func TestIsAutonomousBeadType(t *testing.T) {
	cases := []struct {
		in   model.BeadType
		want bool
	}{
		{"trigger", true},
		{"schedule", true},
		{model.TypeTask, false},
		{model.TypeEpic, false},
		{model.TypeFeature, false},
		{model.TypeBug, false},
		{model.TypeDecision, false},
		{model.TypeEscalation, false},
		{model.TypeFormula, false},
		{"agent", false},
		{"message", false},
		{"gardener", false}, // not a real type — gardeners are trigger/schedule beads
		{"", false},
	}
	for _, tc := range cases {
		if got := isAutonomousBeadType(tc.in); got != tc.want {
			t.Errorf("isAutonomousBeadType(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

// TestAuthoringHumanGateEnabled pins the env-flag parse: default OFF, the
// legacy truthy set flips ENFORCE on (so authoringHumanGateEnabled — the
// resolver-collapse predicate — reports true). "warn" is an observation window
// only, so it MUST NOT collapse the resolver ladder (want=false). Mirrors
// ownerDefaultEnabled's accepted-truthy set so the two staged-rollout flags
// behave identically for operators.
func TestAuthoringHumanGateEnabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"off", false},
		{"no", false},
		{"warn", false}, // observation window — does NOT collapse the ladder
		{"observe", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"on", true},
		{"enforce", true},
		{"  on  ", true},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("BEADS_AUTHORING_HUMAN_GATE", tc.val)
			if got := authoringHumanGateEnabled(); got != tc.want {
				t.Errorf("authoringHumanGateEnabled() with %q = %v; want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestAuthoringHumanGateModeFromEnv pins the three-state staged parse (task
// ka-0000003187): off (default / any typo), warn (observation window), enforce
// (legacy truthy set + the explicit "enforce" value). A typo must never land
// in enforce.
func TestAuthoringHumanGateModeFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		want authoringHumanGateMode
	}{
		{"", authoringGateOff},
		{"0", authoringGateOff},
		{"false", authoringGateOff},
		{"off", authoringGateOff},
		{"nonsense", authoringGateOff},
		{"warn", authoringGateWarn},
		{"WARN", authoringGateWarn},
		{"  observe  ", authoringGateWarn},
		{"dry-run", authoringGateWarn},
		{"1", authoringGateEnforce},
		{"true", authoringGateEnforce},
		{"yes", authoringGateEnforce},
		{"on", authoringGateEnforce},
		{"enforce", authoringGateEnforce},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("BEADS_AUTHORING_HUMAN_GATE", tc.val)
			if got := authoringHumanGateModeFromEnv(); got != tc.want {
				t.Errorf("authoringHumanGateModeFromEnv() with %q = %v; want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestResolverLadder_FleetNoHeader_GateOff_BindsSentinel pins that with the
// gate OFF the legacy behavior is preserved exactly: a header-less fleet token
// still resolves to the "fleet:<subject>" sentinel. This is the regression
// guard ensuring the merge is a zero-behavior-change deploy until the flag is
// flipped.
func TestResolverLadder_FleetNoHeader_GateOff_BindsSentinel(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "")
	srv := newTestServerWithAgentBead(nil)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})
	value, source := srv.resolveOperatedByPure(ctx)
	if value != "fleet:slack-bridge" {
		t.Fatalf("gate off: value = %q; want fleet:slack-bridge (legacy sentinel)", value)
	}
	if source != operatedBySourceFleet {
		t.Fatalf("gate off: source = %q; want %q", source, operatedBySourceFleet)
	}
}

// TestResolverLadder_FleetNoHeader_GateOn_Refuses pins the collapse: with the
// gate ON a header-less fleet token no longer invents a fleet sentinel — the
// resolver falls through to empty so the live-state write fails closed and the
// missing human surfaces loudly (ka-0000006012). A fleet token that DOES carry
// an X-Operated-By human header still resolves to the human (bridge
// re-attribution, ka-0000001777) — that path is unchanged.
func TestResolverLadder_FleetNoHeader_GateOn_Refuses(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newTestServerWithAgentBead(nil)

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})
	value, source := srv.resolveOperatedByPure(ctx)
	if value != "" {
		t.Fatalf("gate on: value = %q; want empty (no fleet sentinel terminal branch)", value)
	}
	if source != operatedBySourceEmpty {
		t.Fatalf("gate on: source = %q; want %q", source, operatedBySourceEmpty)
	}

	// Header path still resolves to the human under the gate.
	ctx = WithOperatedByHeader(ctx, "slack:U0OPERATOR1")
	value, source = srv.resolveOperatedByPure(ctx)
	if value != "slack:U0OPERATOR1" || source != operatedBySourceHeader {
		t.Fatalf("gate on + header: (%q,%q); want (slack:U0OPERATOR1, %q)", value, source, operatedBySourceHeader)
	}
}

// TestResolverLadder_SystemOverride_GateOn_CollapsesToMachine pins the
// collapsed system-override tail (ka-0000006425): with the gate ON EVERY
// explicit system:<source> override folds into the single allowlisted
// system:machine sink. The machine sink set by WithMachineOperator and any
// legacy per-source label set by WithSystemOperator both resolve to
// system:machine — so a TRUE machine-origin write resolves cleanly instead of
// hard-refusing (the collapse, not the narrow-refuse, is what keeps MR !4813's
// genuine machine writes alive once the gate flips — gasbox RC3, ka-0000005858).
func TestResolverLadder_SystemOverride_GateOn_CollapsesToMachine(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newTestServerWithAgentBead(nil)
	base := WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "static"})

	// system:machine via WithMachineOperator → the machine sink, unchanged.
	value, source := srv.resolveOperatedByPure(WithMachineOperator(base))
	if value != operatorMachine || source != operatedBySourceHeader {
		t.Fatalf("machine override: (%q,%q); want (%q, %q)", value, source, operatorMachine, operatedBySourceHeader)
	}

	// Any legacy per-source system override → collapsed to the machine sink.
	for _, src := range []string{"audit-log", "scheduler", "gardenio-controller", "slack-bridge"} {
		value, source = srv.resolveOperatedByPure(WithSystemOperator(base, src))
		if value != operatorMachine || source != operatedBySourceHeader {
			t.Fatalf("system override %q (gate on): (%q,%q); want (%q, %q)", src, value, source, operatorMachine, operatedBySourceHeader)
		}
	}
}

// TestResolverLadder_SystemOverride_GateOff_HonorsAny pins that with the gate
// OFF every system:<source> override is honored verbatim, preserving the
// pre-change behavior for audit-log / escalation-replay paths until the flag
// flips.
func TestResolverLadder_SystemOverride_GateOff_HonorsAny(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "")
	srv := newTestServerWithAgentBead(nil)
	base := WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "static"})

	value, source := srv.resolveOperatedByPure(WithSystemOperator(base, "audit-log"))
	if value != "system:audit-log" || source != operatedBySourceHeader {
		t.Fatalf("legacy system override (gate off): (%q,%q); want (system:audit-log, %q)", value, source, operatedBySourceHeader)
	}
}

// TestChainWalk_LegacyRescue_GateOn_Suppressed pins that the sawLegacy
// last-resort rescue in operatedByFromAgentBead is suppressed under the gate:
// a spawn chain whose only attribution is the legacy sentinel now yields ""
// (→ refuse) instead of re-binding system:legacy-pre-attribution. With the
// gate OFF the rescue still fires (regression guard).
func TestChainWalk_LegacyRescue_GateToggle(t *testing.T) {
	bead := &model.Bead{
		ID:     "agent-x",
		Fields: json.RawMessage(`{"operated_by":"system:legacy-pre-attribution"}`),
	}

	t.Run("gate off → rescue fires", func(t *testing.T) {
		t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "")
		srv := newTestServerWithAgentBead(bead)
		if got := srv.operatedByFromAgentBead(context.Background(), "agent-x"); got != operatorLegacyPreAttribution {
			t.Fatalf("gate off: got %q; want %q (legacy rescue)", got, operatorLegacyPreAttribution)
		}
	})

	t.Run("gate on → rescue suppressed", func(t *testing.T) {
		t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
		srv := newTestServerWithAgentBead(bead)
		if got := srv.operatedByFromAgentBead(context.Background(), "agent-x"); got != "" {
			t.Fatalf("gate on: got %q; want empty (rescue suppressed → refuse)", got)
		}
	})

	// A real human ancestor still resolves regardless of the gate.
	humanBead := &model.Bead{
		ID:     "agent-y",
		Fields: json.RawMessage(`{"operated_by":"system:legacy-pre-attribution","slack_user_id":"U0OPERATOR1"}`),
	}
	t.Run("human ancestor wins under gate", func(t *testing.T) {
		t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
		srv := newTestServerWithAgentBead(humanBead)
		if got := srv.operatedByFromAgentBead(context.Background(), "agent-y"); got != "slack:U0OPERATOR1" {
			t.Fatalf("gate on + human: got %q; want slack:U0OPERATOR1", got)
		}
	})
}

// TestCreateBead_AuthoringGate_TriggerRequiresHuman is the end-to-end gate
// test: with BEADS_AUTHORING_HUMAN_GATE on, an attempt to author a
// type=trigger bead whose resolved operator is NOT a human is refused with an
// inputError; the same request with a human X-Operated-By header succeeds.
func TestCreateBead_AuthoringGate_TriggerRequiresHuman(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")

	triggerIn := func() createBeadInput {
		return createBeadInput{
			Title:  "cron gardener trigger",
			Type:   "trigger",
			Fields: json.RawMessage(`{"action":"notify"}`),
		}
	}

	// Machine-origin authoring (system:machine) must be refused: an
	// autonomous bead type demands a real human, not the machine sink.
	t.Run("machine operator refused", func(t *testing.T) {
		s, _, _ := newTestServer()
		ctx := WithMachineOperator(withTestService(context.Background()))
		if _, err := s.createBead(ctx, triggerIn()); err == nil {
			t.Fatalf("createBead succeeded; want refusal for machine-authored trigger")
		} else if _, ok := err.(inputError); !ok {
			t.Fatalf("err = %T (%v); want inputError", err, err)
		}
	})

	// A header-less agent whose bead carries only the legacy sentinel can no
	// longer author a trigger — the resolver refuses before the gate even
	// runs (operator_refusal), which is also a hard failure.
	t.Run("unattributed agent refused", func(t *testing.T) {
		s, ms, _ := newTestServer()
		seedOwnerAgentBead(t, ms, "crew", map[string]any{
			"operated_by": "system:legacy-pre-attribution",
		})
		ctx := staticAuthCtx("crew")
		if _, err := s.createBead(ctx, triggerIn()); err == nil {
			t.Fatalf("createBead succeeded; want refusal for unattributed trigger author")
		}
	})

	// Human X-Operated-By header → trigger authoring succeeds.
	t.Run("human operator succeeds", func(t *testing.T) {
		s, ms, _ := newTestServer()
		ctx := WithOperatedByHeader(withTestService(context.Background()), "slack:U0OPERATOR1")
		bead, err := s.createBead(ctx, triggerIn())
		if err != nil {
			t.Fatalf("createBead error: %v; want success for human-authored trigger", err)
		}
		if stored := ms.beads[bead.ID]; stored == nil {
			t.Fatalf("trigger bead %s not persisted", bead.ID)
		}
		if bead.OperatedBy != "slack:U0OPERATOR1" {
			t.Errorf("bead.OperatedBy = %q; want slack:U0OPERATOR1", bead.OperatedBy)
		}
	})
}

// TestCreateBead_AuthoringGate_OffAllowsMachineTrigger pins the regression
// guard: with the gate OFF the authoring requirement does not apply — a
// machine-attributed trigger authors successfully, preserving today's
// behavior until the flag is flipped.
func TestCreateBead_AuthoringGate_OffAllowsMachineTrigger(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "")
	s, ms, _ := newTestServer()

	ctx := WithMachineOperator(withTestService(context.Background()))
	bead, err := s.createBead(ctx, createBeadInput{
		Title:  "cron gardener trigger",
		Type:   "trigger",
		Fields: json.RawMessage(`{"action":"notify"}`),
	})
	if err != nil {
		t.Fatalf("createBead error (gate off): %v", err)
	}
	if stored := ms.beads[bead.ID]; stored == nil {
		t.Fatalf("trigger bead %s not persisted", bead.ID)
	}
}

// TestCreateBead_AuthoringGate_WarnObservesWithoutRefusing pins the middle
// rung of the staged warn→enforce rollout (ka-0000003187): in warn mode an
// autonomous bead with a NON-human operator is ALLOWED (zero behavior change
// vs off — no live spawn path hard-400s) but records M9
// action=allowed_warn so the operator can size the residual a flip to enforce
// would start refusing. A human-authored autonomous bead records
// action=allowed_human. The resolver ladder stays UN-collapsed in warn (a
// header-less machine override is honored, not refused).
func TestCreateBead_AuthoringGate_WarnObservesWithoutRefusing(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "warn")
	authoringHumanGateTotal.Reset()

	triggerIn := func() createBeadInput {
		return createBeadInput{
			Title:  "cron gardener trigger",
			Type:   "trigger",
			Fields: json.RawMessage(`{"action":"notify"}`),
		}
	}

	// Machine-attributed trigger: warn ALLOWS it (the resolver ladder is not
	// collapsed in warn, so the system:machine override resolves), and records
	// the allowed_warn residual counter.
	t.Run("non-human autonomous allowed + counted", func(t *testing.T) {
		s, ms, _ := newTestServer()
		ctx := WithMachineOperator(withTestService(context.Background()))
		bead, err := s.createBead(ctx, triggerIn())
		if err != nil {
			t.Fatalf("createBead error (warn): %v; want success (warn never refuses)", err)
		}
		if stored := ms.beads[bead.ID]; stored == nil {
			t.Fatalf("trigger bead %s not persisted", bead.ID)
		}
		if got := testutil.ToFloat64(authoringHumanGateTotal.WithLabelValues("trigger", authoringGateActionAllowedWarn)); got != 1 {
			t.Errorf("trigger/allowed_warn = %v; want 1", got)
		}
		if got := testutil.ToFloat64(authoringHumanGateTotal.WithLabelValues("trigger", authoringGateActionRefused)); got != 0 {
			t.Errorf("trigger/refused = %v; want 0 (warn never refuses)", got)
		}
	})

	// Human-authored trigger: allowed and counted under allowed_human.
	t.Run("human autonomous allowed + counted", func(t *testing.T) {
		s, ms, _ := newTestServer()
		ctx := WithOperatedByHeader(withTestService(context.Background()), "slack:U0OPERATOR1")
		bead, err := s.createBead(ctx, triggerIn())
		if err != nil {
			t.Fatalf("createBead error (warn+human): %v", err)
		}
		if stored := ms.beads[bead.ID]; stored == nil {
			t.Fatalf("trigger bead %s not persisted", bead.ID)
		}
		if got := testutil.ToFloat64(authoringHumanGateTotal.WithLabelValues("trigger", authoringGateActionAllowedHuman)); got != 1 {
			t.Errorf("trigger/allowed_human = %v; want 1", got)
		}
	})

	// A non-autonomous bead type never touches the gate, even in warn.
	t.Run("non-autonomous type untouched", func(t *testing.T) {
		s, _, _ := newTestServer()
		ctx := WithMachineOperator(withTestService(context.Background()))
		before := testutil.ToFloat64(authoringHumanGateTotal.WithLabelValues("task", authoringGateActionAllowedWarn))
		if _, err := s.createBead(ctx, createBeadInput{Title: "a task", Type: "task"}); err != nil {
			t.Fatalf("createBead task error (warn): %v", err)
		}
		if got := testutil.ToFloat64(authoringHumanGateTotal.WithLabelValues("task", authoringGateActionAllowedWarn)); got != before {
			t.Errorf("task/allowed_warn changed to %v; gate must not fire for non-autonomous types", got)
		}
	})
}

// TestResolveAuditOperatorRequired_SystemMachineSink is the live-state-path
// (FK-binding) proof of the ka-0000006425 collapse, exercising the three
// attribution outcomes that gate the sibling client change (MR !4813):
//
//  1. Human-origin write (X-Operated-By: slack:U…) resolves to the real human
//     with a non-zero operator FK — the human invariant, unchanged.
//  2. TRUE machine-origin writes — whether stamped via WithMachineOperator or
//     via a legacy per-source WithSystemOperator — ALL collapse to the single
//     system:machine sink and bind the SAME operator FK row (one machine
//     terminal, no per-source sentinel sprawl).
//  3. An operator-less service write (no header, no override) is a LOUD refusal
//     (ok=false → caller maps to HTTP 400 / InvalidArgument), never a silent
//     fallback to a system sentinel.
func TestResolveAuditOperatorRequired_SystemMachineSink(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	srv := newTestServerWithAgentBead(nil)

	// (1) Human-origin write — resolves to the real human, FK bound.
	t.Run("human operator unchanged", func(t *testing.T) {
		ctx := WithOperatedByHeader(withTestService(context.Background()), "slack:U0OPERATOR1")
		value, id, _, ok := srv.resolveAuditOperatorRequired(ctx)
		if !ok {
			t.Fatalf("human write refused; want resolved")
		}
		if value != "slack:U0OPERATOR1" {
			t.Fatalf("human value = %q; want slack:U0OPERATOR1", value)
		}
		if id == 0 {
			t.Fatalf("human operator FK = 0; want non-zero")
		}
	})

	// (2) Machine-origin writes — every source collapses to ONE system:machine
	// FK. Capture each resolved id and assert they are identical.
	t.Run("machine writes collapse to one sink", func(t *testing.T) {
		base := withTestService(context.Background())
		ctxs := map[string]context.Context{
			"WithMachineOperator":       WithMachineOperator(base),
			"WithSystemOperator/audit":  WithSystemOperator(base, "audit-log"),
			"WithSystemOperator/sched":  WithSystemOperator(base, "scheduler"),
			"WithSystemOperator/ctrl":   WithSystemOperator(base, "gardenio-controller"),
			"WithSystemOperator/bridge": WithSystemOperator(base, "slack-bridge"),
			"WithSystemOperator/retain": WithSystemOperator(base, "audit-retention"),
		}
		var machineID int64
		for label, ctx := range ctxs {
			value, id, _, ok := srv.resolveAuditOperatorRequired(ctx)
			if !ok {
				t.Fatalf("%s: machine write refused; want resolved to system:machine", label)
			}
			if value != operatorMachine {
				t.Fatalf("%s: value = %q; want %q", label, value, operatorMachine)
			}
			if id == 0 {
				t.Fatalf("%s: machine operator FK = 0; want non-zero", label)
			}
			if machineID == 0 {
				machineID = id
			} else if id != machineID {
				t.Fatalf("%s: machine FK = %d; want %d (all sources must collapse to ONE row)", label, id, machineID)
			}
		}
	})

	// (3) Operator-less service write — no header, no override → loud refusal.
	t.Run("operator-less write refused", func(t *testing.T) {
		ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"})
		value, id, source, ok := srv.resolveAuditOperatorRequired(ctx)
		if ok {
			t.Fatalf("operator-less write resolved (%q, id=%d); want refusal", value, id)
		}
		if value != "" {
			t.Fatalf("refused value = %q; want empty (no sentinel fallback)", value)
		}
		// The refusal carries a classification, not the empty resolver source.
		if source == operatedBySourceEmpty {
			t.Fatalf("refusal source = %q; want a refusal classification label", source)
		}
	})
}

// TestCreateBead_AuthoringGate_TaskUnaffected confirms the gate is scoped to
// autonomous types only: an ordinary type=task authored under a machine
// operator is NOT subject to the human requirement (the general resolver gate
// still accepts system:machine for a non-autonomous write).
func TestCreateBead_AuthoringGate_TaskUnaffected(t *testing.T) {
	t.Setenv("BEADS_AUTHORING_HUMAN_GATE", "1")
	s, ms, _ := newTestServer()

	ctx := WithMachineOperator(withTestService(context.Background()))
	bead, err := s.createBead(ctx, createBeadInput{Title: "machine task", Type: "task"})
	if err != nil {
		t.Fatalf("createBead error: %v; task must not be gated by the autonomous-type human requirement", err)
	}
	if stored := ms.beads[bead.ID]; stored == nil {
		t.Fatalf("task bead %s not persisted", bead.ID)
	}
	if bead.OperatedBy != operatorMachine {
		t.Errorf("bead.OperatedBy = %q; want %q", bead.OperatedBy, operatorMachine)
	}
}
