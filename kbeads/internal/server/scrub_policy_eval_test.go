package server

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// --- pure evaluator: first-match rule selection -----------------------

// TestEvaluateScrubPolicy_FirstMatchWinsOverMoreSpecificLaterRule is the
// core acceptance criterion: given an intentionally ambiguous policy where a
// later rule is a more specific (and differently-moded) match than an
// earlier one, the evaluator picks the FIRST matching rule, not the most
// specific and not the last.
func TestEvaluateScrubPolicy_FirstMatchWinsOverMoreSpecificLaterRule(t *testing.T) {
	broad := 100
	specific := 200
	policy := &model.ScrubPolicy{
		Rules: []model.ScrubRule{
			{
				// Broad: matches any purge request scoped to at least 100
				// days, mode human. Listed FIRST.
				When: json.RawMessage(`{"kind":"purge","older_than_days":100}`),
				Mode: model.ScrubModeHuman,
			},
			{
				// Strictly more specific than the rule above (adds
				// requester_is_chain_root) AND would also match the same
				// request — but it is listed SECOND, so first-match must
				// never let it win.
				When:     json.RawMessage(`{"kind":"purge","older_than_days":100,"requester_is_chain_root":true}`),
				Mode:     model.ScrubModeAuto,
				MaxBeads: &specific,
			},
		},
		Default: model.ScrubModeHumanFourEyes,
	}

	ctx := ScrubRuleContext{Kind: "purge", OlderThanDays: &broad, RequesterIsChainRoot: true}
	rule, matched := EvaluateScrubPolicy(policy, ctx)
	if !matched {
		t.Fatalf("expected a match")
	}
	if rule.Mode != model.ScrubModeHuman {
		t.Fatalf("expected first-match rule (mode=human) to win, got mode=%s", rule.Mode)
	}

	mode, _ := ResolveScrubMode(policy, ctx)
	if mode != model.ScrubModeHuman {
		t.Fatalf("ResolveScrubMode: expected human (first match), got %s", mode)
	}
}

// TestEvaluateScrubPolicy_NoMatchFallsToDefault verifies a request matching
// no rule resolves to policy.Default, never "auto" (ValidateScrubPolicy
// already forbids an "auto" default at SetTaint time, so this only needs to
// confirm the fallback happens at all).
func TestEvaluateScrubPolicy_NoMatchFallsToDefault(t *testing.T) {
	policy := &model.ScrubPolicy{
		Rules: []model.ScrubRule{
			{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeHuman},
		},
		Default:   model.ScrubModeHumanFourEyes,
		Approvers: []string{"team:gd-owner-team"},
	}

	ctx := ScrubRuleContext{Kind: "purge"} // no rule names kind=purge
	rule, matched := EvaluateScrubPolicy(policy, ctx)
	if matched || rule != nil {
		t.Fatalf("expected no match, got rule=%+v", rule)
	}

	mode, approvers := ResolveScrubMode(policy, ctx)
	if mode != model.ScrubModeHumanFourEyes {
		t.Fatalf("expected default mode human-four-eyes, got %s", mode)
	}
	if len(approvers) != 1 || approvers[0] != "team:gd-owner-team" {
		t.Fatalf("expected default approvers to carry through, got %v", approvers)
	}
}

// TestScrubRuleMatches_UnrecognizedKeyNeverMatches verifies a `when` clause
// naming a predicate key the evaluator does not understand fails closed
// (never matches) rather than silently ignoring the key and over-matching.
func TestScrubRuleMatches_UnrecognizedKeyNeverMatches(t *testing.T) {
	when := json.RawMessage(`{"kind":"purge","some_future_predicate":true}`)
	ctx := ScrubRuleContext{Kind: "purge"}
	if scrubRuleMatches(when, ctx) {
		t.Fatalf("expected an unrecognized predicate key to prevent a match")
	}
}

// TestScrubRuleMatches_OlderThanDaysIsAFloor verifies older_than_days is a
// floor ("at least this old"), not an exact-equality match: a request
// scoped stricter (older) than the rule's threshold matches; one scoped
// looser (newer) does not.
func TestScrubRuleMatches_OlderThanDaysIsAFloor(t *testing.T) {
	when := json.RawMessage(`{"kind":"purge","older_than_days":30}`)

	stricter := 90
	if !scrubRuleMatches(when, ScrubRuleContext{Kind: "purge", OlderThanDays: &stricter}) {
		t.Errorf("expected a request scoped to 90 days (>= 30) to match")
	}

	looser := 10
	if scrubRuleMatches(when, ScrubRuleContext{Kind: "purge", OlderThanDays: &looser}) {
		t.Errorf("expected a request scoped to 10 days (< 30) NOT to match")
	}

	if scrubRuleMatches(when, ScrubRuleContext{Kind: "purge", OlderThanDays: nil}) {
		t.Errorf("expected a request with no older_than_days scope NOT to match a rule requiring one")
	}
}

func TestScrubRuleMatches_EmptyOrMalformedWhenNeverMatches(t *testing.T) {
	ctx := ScrubRuleContext{Kind: "purge"}
	for _, when := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage(`  `), json.RawMessage(`not json`)} {
		if scrubRuleMatches(when, ctx) {
			t.Errorf("expected when=%q to never match", when)
		}
	}
}

// --- requesterIdentityIsChainRoot ---------------------------------------

func TestRequesterIdentityIsChainRoot(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	ctx := context.Background()

	root := &model.Bead{ID: "gd-root-agent", Type: "agent", Fields: json.RawMessage(`{}`)}
	if err := ms.CreateBead(ctx, root); err != nil {
		t.Fatal(err)
	}
	sentinelRoot := &model.Bead{ID: "gd-sentinel-agent", Type: "agent", Fields: json.RawMessage(`{"spawned_by":"scheduler:daily"}`)}
	if err := ms.CreateBead(ctx, sentinelRoot); err != nil {
		t.Fatal(err)
	}
	nested := &model.Bead{ID: "gd-nested-agent", Type: "agent", Fields: json.RawMessage(`{"spawned_by":"gd-root-agent"}`)}
	if err := ms.CreateBead(ctx, nested); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		requestedBy string
		want        bool
	}{
		{"human email is trivially chain root", "alice@example.com", true},
		{"empty is never chain root", "", false},
		{"agent with no spawner is chain root", "gd-root-agent", true},
		{"agent spawned by a sentinel is chain root", "gd-sentinel-agent", true},
		{"agent spawned by another agent is NOT chain root", "gd-nested-agent", false},
		{"unknown agent id is not chain root", "gd-does-not-exist", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.requesterIdentityIsChainRoot(ctx, tc.requestedBy)
			if got != tc.want {
				t.Errorf("requesterIdentityIsChainRoot(%q) = %v, want %v", tc.requestedBy, got, tc.want)
			}
		})
	}
}

// --- create and approve call sites agree --------------------------------

// TestScrubPolicyEvaluator_CreateAndApproveAgree is acceptance criterion 3:
// the evaluator run at create (to preview) and the evaluator run at approve
// (to authorize) must resolve identically given the same request and an
// unchanged policy. This builds the ScrubRuleContext the way each call site
// would — create from the just-validated request body, approve from the
// request bead's stored Fields fetched back off the store — and asserts
// both resolve to the same rule/mode.
func TestScrubPolicyEvaluator_CreateAndApproveAgree(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	ctx := context.Background()

	requester := &model.Bead{ID: "gd-requester-agent", Type: "agent", Fields: json.RawMessage(`{}`)}
	if err := ms.CreateBead(ctx, requester); err != nil {
		t.Fatal(err)
	}

	autoMaxBeads := 200
	policy := &model.ScrubPolicy{
		Rules: []model.ScrubRule{
			{
				When:     json.RawMessage(`{"kind":"purge","older_than_days":180,"requester_is_chain_root":true}`),
				Mode:     model.ScrubModeAuto,
				MaxBeads: &autoMaxBeads,
			},
			{
				When: json.RawMessage(`{"kind":"purge"}`),
				Mode: model.ScrubModeHuman,
			},
		},
		Default: model.ScrubModeHumanFourEyes,
	}

	kind := "purge"
	olderThanDays := 200 // the request's own scope, frozen at create

	// Call site 1: CREATE. Fields as they exist the moment
	// applyScrubRequestGuard runs, before the bead is stored — attestation
	// cannot exist yet.
	createCtx := s.scrubRuleContextForRequest(ctx, kind, &olderThanDays, false, requester.ID)
	createMode, _ := ResolveScrubMode(policy, createCtx)

	// Now the request bead exists in the store, exactly as create would
	// have left it (scope/kind/requested_by frozen; still no attestation).
	requestBead := &model.Bead{
		ID:   "gd-scrub-req-1",
		Type: model.TypeScrubRequest,
		Fields: json.RawMessage(`{
			"taint": "acme-production",
			"kind": "purge",
			"scope": {"older_than_days": 200},
			"requested_by": "gd-requester-agent"
		}`),
	}
	if err := ms.CreateBead(ctx, requestBead); err != nil {
		t.Fatal(err)
	}

	// Call site 2: APPROVE. Fields read back off the stored bead — this is
	// what a real approve handler would parse.
	var stored struct {
		Kind        string          `json:"kind"`
		Scope       json.RawMessage `json:"scope"`
		RequestedBy string          `json:"requested_by"`
		Attestation json.RawMessage `json:"attestation"`
	}
	if err := json.Unmarshal(requestBead.Fields, &stored); err != nil {
		t.Fatal(err)
	}
	approveCtx := s.scrubRuleContextForRequest(
		ctx, stored.Kind, scrubRequestScopeOlderThanDays(stored.Scope),
		len(stored.Attestation) > 0 && !isJSONNull(stored.Attestation),
		stored.RequestedBy,
	)
	approveMode, approveRule := ResolveScrubMode(policy, approveCtx)

	if createCtx.Kind != approveCtx.Kind ||
		createCtx.RequesterIsChainRoot != approveCtx.RequesterIsChainRoot ||
		createCtx.Attested != approveCtx.Attested ||
		(createCtx.OlderThanDays == nil) != (approveCtx.OlderThanDays == nil) ||
		(createCtx.OlderThanDays != nil && *createCtx.OlderThanDays != *approveCtx.OlderThanDays) {
		t.Fatalf("expected create and approve to build identical contexts, got create=%+v approve=%+v", createCtx, approveCtx)
	}
	if createMode != approveMode {
		t.Fatalf("expected create-time preview (%s) to agree with approve-time authorization (%s)", createMode, approveMode)
	}
	if approveMode != model.ScrubModeAuto {
		t.Fatalf("expected the auto rule to match (scope 200 >= 180, chain root), got %s (rule=%v)", approveMode, approveRule)
	}
}
