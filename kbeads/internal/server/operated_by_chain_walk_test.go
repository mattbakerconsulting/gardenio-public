package server

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newChainWalkServer builds a BeadsServer whose store contains the given
// beads. Used by chain-walk + user_bead_id resolver tests.
func newChainWalkServer(beads ...*model.Bead) *BeadsServer {
	ms := &mockStore{beads: make(map[string]*model.Bead)}
	for _, b := range beads {
		ms.beads[b.ID] = b
	}
	return &BeadsServer{store: ms}
}

// TestOperatedByFromAgentBead_SpawnChain_TwoHops covers the ka-0000006147
// acceptance signal: a crew agent two hops removed from Matt (sched →
// gardener → crew) resolves to Matt's slack id by walking spawned_by.
//
// Topology:
//
//	crew    Fields: spawned_by=gardener                         (no direct attribution)
//	gardener Fields: spawned_by=sched                           (no direct attribution)
//	sched   Fields: slack_user_id=U0OPERATOR1                  (human root)
func TestOperatedByFromAgentBead_SpawnChain_TwoHops(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"spawned_by":"gardener"}`),
		},
		&model.Bead{
			ID: "gardener", Type: "agent",
			Fields: []byte(`{"spawned_by":"sched"}`),
		},
		&model.Bead{
			ID: "sched", Type: "agent",
			Fields: []byte(`{"slack_user_id":"U0OPERATOR1"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "slack:U0OPERATOR1" {
		t.Fatalf("two-hop walk: got %q; want slack:U0OPERATOR1", got)
	}
}

// TestOperatedByFromAgentBead_SpawnChain_TerminatesOnHumanOpsOnFirstHop
// pins that the resolver does NOT walk past the current agent when its
// own fields.operated_by is already a human shape. The chain walk is the
// LAST-RESORT path, not the default.
func TestOperatedByFromAgentBead_SpawnChain_TerminatesOnHumanOpsOnFirstHop(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"operated_by":"slack:U0OPERATOR1","spawned_by":"gardener"}`),
		},
		&model.Bead{
			ID: "gardener", Type: "agent",
			Fields: []byte(`{"operated_by":"slack:UOTHER1234"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "slack:U0OPERATOR1" {
		t.Fatalf("got %q; want slack:U0OPERATOR1 (must use own field, not walk)", got)
	}
}

// TestOperatedByFromAgentBead_SpawnChain_SkipsAgentShapedOperatedBy pins
// the ka-0000006809 Invariant §1 inside the walk: a gardener that pushed
// its own agent id into fields.operated_by must NOT win — the walk
// continues to the spawner.
func TestOperatedByFromAgentBead_SpawnChain_SkipsAgentShapedOperatedBy(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"operated_by":"sched-gardenio-gardener-release-qjj","spawned_by":"gardener"}`),
		},
		&model.Bead{
			ID: "gardener", Type: "agent",
			Fields: []byte(`{"slack_user_id":"U0OPERATOR1"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "slack:U0OPERATOR1" {
		t.Fatalf("got %q; want slack:U0OPERATOR1 (agent-shaped operated_by must not short-circuit walk)", got)
	}
}

// TestOperatedByFromAgentBead_SpawnChain_CycleSafe pins that a cyclic
// spawn graph (a → b → a) returns "" without overflowing — visited-set
// stops the loop before maxSpawnChainHops.
func TestOperatedByFromAgentBead_SpawnChain_CycleSafe(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "a", Type: "agent",
			Fields: []byte(`{"spawned_by":"b"}`),
		},
		&model.Bead{
			ID: "b", Type: "agent",
			Fields: []byte(`{"spawned_by":"a"}`),
		},
	)

	operatedByResolverErrorsTotal.Reset()
	got := srv.operatedByFromAgentBead(context.Background(), "a")
	if got != "" {
		t.Fatalf("cycle: got %q; want \"\"", got)
	}
	// Cycle is distinct from overflow — must NOT bump the overflow bucket.
	if n := testutil.ToFloat64(operatedByResolverErrorsTotal.WithLabelValues("spawn_chain_overflow")); n != 0 {
		t.Fatalf("spawn_chain_overflow on cycle = %v; want 0 (cycle is silent)", n)
	}
}

// TestOperatedByFromAgentBead_SpawnChain_SelfLoopStops pins that an agent
// whose spawned_by points to itself stops the walk without bumping the
// overflow bucket. The visited-set catches this on the second iteration.
func TestOperatedByFromAgentBead_SpawnChain_SelfLoopStops(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "lonely", Type: "agent",
			Fields: []byte(`{"spawned_by":"lonely"}`),
		},
	)
	operatedByResolverErrorsTotal.Reset()
	got := srv.operatedByFromAgentBead(context.Background(), "lonely")
	if got != "" {
		t.Fatalf("self-loop: got %q; want \"\"", got)
	}
	if n := testutil.ToFloat64(operatedByResolverErrorsTotal.WithLabelValues("spawn_chain_overflow")); n != 0 {
		t.Fatalf("spawn_chain_overflow on self-loop = %v; want 0", n)
	}
}

// TestOperatedByFromAgentBead_SpawnChain_Overflow pins that a chain
// longer than maxSpawnChainHops without a human root bumps the
// spawn_chain_overflow resolver-error bucket and returns "".
func TestOperatedByFromAgentBead_SpawnChain_Overflow(t *testing.T) {
	// Six-deep chain: a0 → a1 → a2 → a3 → a4 → a5, none carrying
	// attribution. maxSpawnChainHops=5 so walking from a0 visits
	// a0..a4 and then exits via the loop bound — a5 is never reached.
	beads := []*model.Bead{
		{ID: "a0", Type: "agent", Fields: []byte(`{"spawned_by":"a1"}`)},
		{ID: "a1", Type: "agent", Fields: []byte(`{"spawned_by":"a2"}`)},
		{ID: "a2", Type: "agent", Fields: []byte(`{"spawned_by":"a3"}`)},
		{ID: "a3", Type: "agent", Fields: []byte(`{"spawned_by":"a4"}`)},
		{ID: "a4", Type: "agent", Fields: []byte(`{"spawned_by":"a5"}`)},
		{
			ID: "a5", Type: "agent",
			// Human root sits PAST the hop bound — must not resolve.
			Fields: []byte(`{"slack_user_id":"U0OPERATOR1"}`),
		},
	}
	srv := newChainWalkServer(beads...)

	operatedByResolverErrorsTotal.Reset()
	got := srv.operatedByFromAgentBead(context.Background(), "a0")
	if got != "" {
		t.Fatalf("overflow: got %q; want \"\" (chain longer than maxSpawnChainHops)", got)
	}
	if n := testutil.ToFloat64(operatedByResolverErrorsTotal.WithLabelValues("spawn_chain_overflow")); n != 1 {
		t.Fatalf("spawn_chain_overflow = %v; want 1", n)
	}
}

// TestOperatedByFromAgentBead_SpawnChain_ResolvesAtHopBoundary pins that
// a chain exactly maxSpawnChainHops long DOES resolve — the human root
// must sit on the last reachable hop, not one past it.
func TestOperatedByFromAgentBead_SpawnChain_ResolvesAtHopBoundary(t *testing.T) {
	// Five-deep chain a0..a4 with the human root on a4.
	beads := []*model.Bead{
		{ID: "a0", Type: "agent", Fields: []byte(`{"spawned_by":"a1"}`)},
		{ID: "a1", Type: "agent", Fields: []byte(`{"spawned_by":"a2"}`)},
		{ID: "a2", Type: "agent", Fields: []byte(`{"spawned_by":"a3"}`)},
		{ID: "a3", Type: "agent", Fields: []byte(`{"spawned_by":"a4"}`)},
		{
			ID: "a4", Type: "agent",
			Fields: []byte(`{"slack_user_id":"U0OPERATOR1"}`),
		},
	}
	srv := newChainWalkServer(beads...)

	operatedByResolverErrorsTotal.Reset()
	got := srv.operatedByFromAgentBead(context.Background(), "a0")
	if got != "slack:U0OPERATOR1" {
		t.Fatalf("boundary: got %q; want slack:U0OPERATOR1", got)
	}
	if n := testutil.ToFloat64(operatedByResolverErrorsTotal.WithLabelValues("spawn_chain_overflow")); n != 0 {
		t.Fatalf("spawn_chain_overflow at boundary = %v; want 0", n)
	}
}

// TestOperatedByFromAgentBead_SpawnChain_MissingParentStopsCleanly pins
// that a dangling spawned_by pointer (parent bead does not exist) stops
// the walk and returns "" without an error metric bump — GetBead-not-
// found is signaled by (nil, nil) from the mockStore, not by an error.
func TestOperatedByFromAgentBead_SpawnChain_MissingParentStopsCleanly(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"spawned_by":"missing-gardener"}`),
		},
	)
	operatedByResolverErrorsTotal.Reset()
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "" {
		t.Fatalf("missing parent: got %q; want \"\"", got)
	}
	if n := testutil.ToFloat64(operatedByResolverErrorsTotal.WithLabelValues("agent_lookup_fail")); n != 0 {
		t.Fatalf("agent_lookup_fail = %v; want 0 (not-found is not a lookup error)", n)
	}
	if n := testutil.ToFloat64(operatedByResolverErrorsTotal.WithLabelValues("spawn_chain_overflow")); n != 0 {
		t.Fatalf("spawn_chain_overflow = %v; want 0", n)
	}
}

// TestOperatedByFromAgentBead_UserBeadID_SlackPreferred covers the
// ka-0000006147 user_bead_id path: an agent with no direct attribution
// resolves via fields.user_bead_id → type=user bead → slack_id.
func TestOperatedByFromAgentBead_UserBeadID_SlackPreferred(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"user_bead_id":"user-matt"}`),
		},
		&model.Bead{
			ID: "user-matt", Type: "user",
			Fields: []byte(`{"slack_id":"U0OPERATOR1","email":"matt@acme.example"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "slack:U0OPERATOR1" {
		t.Fatalf("got %q; want slack:U0OPERATOR1 (slack_id preferred over email)", got)
	}
}

// TestOperatedByFromAgentBead_UserBeadID_EmailFallback covers the email
// fallback: a user bead with no slack_id resolves to oidc:<email>.
func TestOperatedByFromAgentBead_UserBeadID_EmailFallback(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"user_bead_id":"user-matt"}`),
		},
		&model.Bead{
			ID: "user-matt", Type: "user",
			Fields: []byte(`{"email":"matt@acme.example"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "oidc:matt@acme.example" {
		t.Fatalf("got %q; want oidc:matt@acme.example", got)
	}
}

// TestOperatedByFromAgentBead_UserBeadID_RejectsWrongType pins that the
// user_bead_id pointer is verified to point at type=user — pointing at,
// say, a task bead with a "slack_id" field must NOT round-trip as a
// human operator.
func TestOperatedByFromAgentBead_UserBeadID_RejectsWrongType(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"user_bead_id":"some-task"}`),
		},
		&model.Bead{
			ID: "some-task", Type: "task",
			Fields: []byte(`{"slack_id":"U0OPERATOR1"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "" {
		t.Fatalf("got %q; want \"\" (user_bead_id must point at type=user)", got)
	}
}

// TestOperatedByFromAgentBead_UserBeadID_RejectsMalformedShape pins
// that a user bead whose slack_id is not a valid Slack U… id falls
// through to email, and on bad email shape returns "" — the
// isHumanOperator gate stays authoritative.
func TestOperatedByFromAgentBead_UserBeadID_RejectsMalformedShape(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"user_bead_id":"user-bad"}`),
		},
		&model.Bead{
			ID: "user-bad", Type: "user",
			Fields: []byte(`{"slack_id":"bogus","email":"notanemail"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "" {
		t.Fatalf("got %q; want \"\" (malformed shapes must not pass isHumanOperator)", got)
	}
}

// TestOperatedByFromAgentBead_UserBeadID_OnAncestorResolves pins that
// the chain walk consults user_bead_id at every hop — Matt's user bead
// hangs off the SCHEDULER, not the crew, and a two-hop crew still
// resolves.
func TestOperatedByFromAgentBead_UserBeadID_OnAncestorResolves(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"spawned_by":"gardener"}`),
		},
		&model.Bead{
			ID: "gardener", Type: "agent",
			Fields: []byte(`{"spawned_by":"sched"}`),
		},
		&model.Bead{
			ID: "sched", Type: "agent",
			Fields: []byte(`{"user_bead_id":"user-matt"}`),
		},
		&model.Bead{
			ID: "user-matt", Type: "user",
			Fields: []byte(`{"slack_id":"U0OPERATOR1"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "slack:U0OPERATOR1" {
		t.Fatalf("got %q; want slack:U0OPERATOR1 (user_bead_id at chain root)", got)
	}
}

// TestOperatedByFromAgentBead_ResolverOutputPassesIsHumanOperator is the
// ka-0000006809 Invariant §1 guard: every non-empty value the resolver
// emits MUST satisfy isHumanOperator, regardless of which fallback
// branch produced it. This pins the contract across all chain-walk
// inputs in one place so a future fallback addition cannot regress it.
//
// Single documented exception: the legacy-pre-attribution sentinel
// (ka-0000005841). When an agent (or any ancestor in the spawn chain)
// is stamped with system:legacy-pre-attribution and the walk yields no
// human attribution, the walker returns the sentinel so the seeded
// operators row from migration 016 carries the write. The test cases
// below do NOT include the sentinel-only topology — see
// TestOperatedByFromAgentBead_LegacySentinel_* for explicit coverage.
func TestOperatedByFromAgentBead_ResolverOutputPassesIsHumanOperator(t *testing.T) {
	cases := []struct {
		name  string
		beads []*model.Bead
		start string
	}{
		{
			name: "direct operated_by",
			beads: []*model.Bead{{
				ID: "a", Type: "agent",
				Fields: []byte(`{"operated_by":"slack:U0OPERATOR1"}`),
			}},
			start: "a",
		},
		{
			name: "user_bead_id slack_id",
			beads: []*model.Bead{
				{ID: "a", Type: "agent", Fields: []byte(`{"user_bead_id":"u"}`)},
				{ID: "u", Type: "user", Fields: []byte(`{"slack_id":"U0OPERATOR1"}`)},
			},
			start: "a",
		},
		{
			name: "user_bead_id email",
			beads: []*model.Bead{
				{ID: "a", Type: "agent", Fields: []byte(`{"user_bead_id":"u"}`)},
				{ID: "u", Type: "user", Fields: []byte(`{"email":"matt@acme.example"}`)},
			},
			start: "a",
		},
		{
			name: "slack_user_id",
			beads: []*model.Bead{{
				ID: "a", Type: "agent",
				Fields: []byte(`{"slack_user_id":"U0OPERATOR1"}`),
			}},
			start: "a",
		},
		{
			name: "via two-hop spawned_by",
			beads: []*model.Bead{
				{ID: "a", Type: "agent", Fields: []byte(`{"spawned_by":"b"}`)},
				{ID: "b", Type: "agent", Fields: []byte(`{"spawned_by":"c"}`)},
				{
					ID: "c", Type: "agent",
					Fields: []byte(`{"slack_user_id":"U0OPERATOR1"}`),
				},
			},
			start: "a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newChainWalkServer(tc.beads...)
			got := srv.operatedByFromAgentBead(context.Background(), tc.start)
			if got == "" {
				t.Fatalf("got empty; expected human-shaped operator")
			}
			if !isHumanOperator(got) {
				t.Fatalf("resolver emitted non-human shape %q — Invariant §1 violated", got)
			}
		})
	}
}

// TestOperatedByFromAgentBead_LegacySentinel_SoleAttribution pins the
// ka-0000005841 rescue: an agent whose ONLY attribution field is
// fields.operated_by=system:legacy-pre-attribution (no slack_user_id,
// no user_bead_id, no human ancestor) resolves to the legacy sentinel
// rather than failing closed. This is the FSM-353 jira-bridge spawn
// case where the controller propagates the sentinel from
// sched.OperatedBy → BEADS_OPERATED_BY → agent bead.
func TestOperatedByFromAgentBead_LegacySentinel_SoleAttribution(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"operated_by":"system:legacy-pre-attribution"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != operatorLegacyPreAttribution {
		t.Fatalf("sentinel-only: got %q; want %q", got, operatorLegacyPreAttribution)
	}
}

// TestOperatedByFromAgentBead_LegacySentinel_HumanWins pins that the
// sentinel rescue is LAST-RESORT only. When the same agent carries
// both the sentinel AND a usable slack_user_id, the human wins.
// (Regression guard for the operated_by_test.go:302 case.)
func TestOperatedByFromAgentBead_LegacySentinel_HumanWins(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"operated_by":"system:legacy-pre-attribution","slack_user_id":"U0OPERATOR1"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "slack:U0OPERATOR1" {
		t.Fatalf("sentinel + slack: got %q; want slack:U0OPERATOR1 (human must win)", got)
	}
}

// TestOperatedByFromAgentBead_LegacySentinel_OnAncestor pins that the
// sentinel rescue fires when the sentinel appears anywhere in the
// spawn chain, not only on the current agent. A crew stamped with no
// attribution whose gardener carries the sentinel still resolves.
func TestOperatedByFromAgentBead_LegacySentinel_OnAncestor(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"spawned_by":"gardener"}`),
		},
		&model.Bead{
			ID: "gardener", Type: "agent",
			Fields: []byte(`{"operated_by":"system:legacy-pre-attribution"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != operatorLegacyPreAttribution {
		t.Fatalf("ancestor sentinel: got %q; want %q", got, operatorLegacyPreAttribution)
	}
}

// TestOperatedByFromAgentBead_LegacySentinel_HumanAncestorWins pins
// that the rescue does NOT fire when a human ancestor exists past the
// sentinel hop — the walk continues and the human wins.
func TestOperatedByFromAgentBead_LegacySentinel_HumanAncestorWins(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"operated_by":"system:legacy-pre-attribution","spawned_by":"gardener"}`),
		},
		&model.Bead{
			ID: "gardener", Type: "agent",
			Fields: []byte(`{"slack_user_id":"U0OPERATOR1"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "slack:U0OPERATOR1" {
		t.Fatalf("got %q; want slack:U0OPERATOR1 (human ancestor must win over sentinel rescue)", got)
	}
}

// TestOperatedByFromAgentBead_LegacySentinel_OtherSystemSentinelDoesNotRescue
// pins that ONLY system:legacy-pre-attribution triggers the rescue.
// Other system sentinels (system:scheduler, system:foo, etc.) must
// still fail closed so they cannot smuggle attribution.
func TestOperatedByFromAgentBead_LegacySentinel_OtherSystemSentinelDoesNotRescue(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"operated_by":"system:scheduler"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "" {
		t.Fatalf("other system sentinel: got %q; want \"\"", got)
	}
}

// TestApplyOperatedByHeader_LegacySentinel pins the header-path carve-out:
// when X-Operated-By is the legacy sentinel verbatim, the value routes
// through WithSystemOperator instead of being rejected by the sanitizer.
// Downstream resolveAuditOperatorRequired then FK-binds the seeded
// sentinel operators row from migration 016 (ka-0000005841).
func TestApplyOperatedByHeader_LegacySentinel(t *testing.T) {
	ctx := applyOperatedByHeader(context.Background(), operatorLegacyPreAttribution, "static", "test")
	// Header should NOT be stamped (sanitizer would have rejected the
	// sentinel; the carve-out routes via the system-operator override).
	if h := operatedByHeaderFromContext(ctx); h != "" {
		t.Errorf("operatedByHeaderFromContext = %q; want \"\" (sentinel must not land in header pipeline)", h)
	}
	if sys := systemOperatorFromContext(ctx); sys != operatorLegacyPreAttribution {
		t.Errorf("systemOperatorFromContext = %q; want %q", sys, operatorLegacyPreAttribution)
	}
	// Refusal trace should NOT mark the header as rejected — the
	// sentinel is an accepted routing, not a sanitizer failure.
	if trace := operatorRefusalTraceFromContext(ctx); trace != nil {
		if !trace.headerSent {
			t.Errorf("trace.headerSent = false; want true (header was sent)")
		}
		if trace.headerRejected {
			t.Errorf("trace.headerRejected = true; want false (sentinel routes via override, not rejection)")
		}
	}
}

// TestOperatedByFromAgentBead_SentinelOperatedBy_WalksToHuman pins the
// ka-0000005841 fix: a sentinel-class operated_by on a spawn-chain hop
// (e.g. fsm-triage agents carrying the legacy-pre-attribution stamp,
// jira-bridge fleet defaults, or scheduler system:* fallbacks) must NOT
// pollute the refusal trace or short-circuit the walk. The chain walk
// must continue past the sentinel and resolve the real human ancestor.
func TestOperatedByFromAgentBead_SentinelOperatedBy_WalksToHuman(t *testing.T) {
	cases := []struct {
		name     string
		sentinel string
	}{
		{"system:legacy-pre-attribution", "system:legacy-pre-attribution"},
		{"system:scheduler", "system:scheduler"},
		{"fleet:jira-bridge", "fleet:jira-bridge"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newChainWalkServer(
				&model.Bead{
					ID: "crew", Type: "agent",
					Fields: []byte(`{"operated_by":"` + tc.sentinel + `","spawned_by":"gardener"}`),
				},
				&model.Bead{
					ID: "gardener", Type: "agent",
					Fields: []byte(`{"slack_user_id":"U0OPERATOR1"}`),
				},
			)
			ctx := withOperatorRefusalTrace(context.Background())
			got := srv.operatedByFromAgentBead(ctx, "crew")
			if got != "slack:U0OPERATOR1" {
				t.Fatalf("sentinel %q on crew: got %q; want slack:U0OPERATOR1 (walk must continue)",
					tc.sentinel, got)
			}
			// The sentinel must NOT have set the fallbackFoundNonHuman
			// breadcrumb — that is reserved for genuinely malformed values
			// (e.g. raw agent IDs), so classifyOperatorRefusal can still
			// label a downstream refusal as something other than
			// fallback_agent_only when a sentinel is in the chain.
			if tr := operatorRefusalTraceFromContext(ctx); tr != nil && tr.fallbackFoundNonHuman {
				t.Fatalf("sentinel %q set fallbackFoundNonHuman; expected silent skip", tc.sentinel)
			}
		})
	}
}

// --- ka-0000003065: agent-bead fallback must resolve by NAME, not just id ---
//
// Real controller-spawned agents key by a random nanoid bead id with their
// name in fields.agent (SpawnAgent passes only Title=agentName to CreateBead;
// the pod env sets KD_ACTOR = spec.AgentName). The static-token subject and
// every fields.spawned_by pointer are therefore NAMES, not bead ids. Before
// the fix operatedByFromAgentBead resolved each hop with GetBead(name), which
// 404s for every real agent, so the whole agent_bead_fallback operator source
// silently no-opped (masked in prod only because X-Operated-By short-circuits
// the resolver before this step runs). These tests seed the REAL shape
// (id=nanoid, fields.agent=name, spawned_by=name) so they fail on the old
// by-id-only path and pass once the by-name fallback lands.

// TestOperatedByFromAgentBead_FirstHop_ResolvesByName pins the first-hop gap:
// a lone agent whose id is a nanoid (!= its name) and whose own
// fields.slack_user_id is a human root must resolve when the subject is the
// agent NAME. Old code: GetBead("gardenio-crew-abc") 404s → "".
func TestOperatedByFromAgentBead_FirstHop_ResolvesByName(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "gd-nanoidCrew1", Type: "agent",
			Fields: []byte(`{"agent":"gardenio-crew-abc","slack_user_id":"U0OPERATOR1"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "gardenio-crew-abc")
	if got != "slack:U0OPERATOR1" {
		t.Fatalf("first-hop by-name: got %q; want slack:U0OPERATOR1 (agent keyed by nanoid, name in fields.agent)", got)
	}
}

// TestOperatedByFromAgentBead_SpawnChain_ResolvesByName pins the chain-walk
// gap: a crew whose spawned_by names its gardener (also keyed by nanoid) must
// walk to the gardener's human root. Both hops key by name, both beads key by
// nanoid — every hop needs the by-name fallback.
func TestOperatedByFromAgentBead_SpawnChain_ResolvesByName(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "gd-nanoidCrew2", Type: "agent",
			Fields: []byte(`{"agent":"gardenio-crew-xyz","spawned_by":"gardenio-gardener-qqq"}`),
		},
		&model.Bead{
			ID: "gd-nanoidCapt2", Type: "agent",
			Fields: []byte(`{"agent":"gardenio-gardener-qqq","slack_user_id":"U0OPERATOR1"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "gardenio-crew-xyz")
	if got != "slack:U0OPERATOR1" {
		t.Fatalf("chain-walk by-name: got %q; want slack:U0OPERATOR1 (spawned_by is a name, gardener keyed by nanoid)", got)
	}
}

// TestOperatedByFromAgentBead_ByName_MissStopsCleanly pins that a subject that
// matches NO bead by id AND no agent by name stops the walk and returns ""
// without bumping agent_lookup_fail (a clean not-found is not a lookup error).
func TestOperatedByFromAgentBead_ByName_MissStopsCleanly(t *testing.T) {
	srv := newChainWalkServer(
		&model.Bead{
			ID: "gd-nanoidOther", Type: "agent",
			Fields: []byte(`{"agent":"some-other-agent","slack_user_id":"U0OPERATOR1"}`),
		},
	)
	operatedByResolverErrorsTotal.Reset()
	got := srv.operatedByFromAgentBead(context.Background(), "nonexistent-agent")
	if got != "" {
		t.Fatalf("unknown subject: got %q; want \"\"", got)
	}
	if n := testutil.ToFloat64(operatedByResolverErrorsTotal.WithLabelValues("agent_lookup_fail")); n != 0 {
		t.Fatalf("agent_lookup_fail = %v; want 0 (clean by-name miss is not a lookup error)", n)
	}
}

// TestOperatedByFromAgentBead_ByName_PrefersByIDWhenIDMatches pins that the
// by-id rung still wins when the subject already IS a bead id (legacy fixtures
// / a caller that stamped X-Actor with its own KD_AGENT_ID) — the by-name
// fallback only fires when GetBead misses.
func TestOperatedByFromAgentBead_ByName_PrefersByIDWhenIDMatches(t *testing.T) {
	srv := newChainWalkServer(
		// id == name: resolvable by GetBead directly. A DIFFERENT nanoid bead
		// also carries fields.agent="crew" — if we fell through to by-name we
		// might pick the wrong one; GetBead(id) must win.
		&model.Bead{
			ID: "crew", Type: "agent",
			Fields: []byte(`{"agent":"crew","slack_user_id":"U0OPERATOR1"}`),
		},
		&model.Bead{
			ID: "gd-nanoidDup", Type: "agent",
			Fields: []byte(`{"agent":"crew","slack_user_id":"UWRONG99999"}`),
		},
	)
	got := srv.operatedByFromAgentBead(context.Background(), "crew")
	if got != "slack:U0OPERATOR1" {
		t.Fatalf("by-id subject: got %q; want slack:U0OPERATOR1 (GetBead-by-id must win over by-name)", got)
	}
}
