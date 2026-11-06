package main

import (
	"strings"
	"testing"
)

// TestWrapupHasOperatorAsk_Positives covers the real stranded-ask phrasings
// from the audit (report /bundle/ka-0000002636, taxonomy buckets A/C/D). Each
// MUST fire the detector. The asks live in forward-looking fields only.
func TestWrapupHasOperatorAsk_Positives(t *testing.T) {
	cases := []struct {
		name string
		w    *WrapUp
	}{
		// Bucket A — choice posed to the operator (decision-shaped). These name
		// the operator explicitly or use a direct second-person request — a bare
		// "should we…?" musing with no operator reference is deliberately NOT a
		// positive (see the Negatives table) to keep precision high.
		{"await matt choose", &WrapUp{Accomplishments: "did x", HandoffNotes: "Awaiting Matt to choose (a) or (b)."}},
		{"which option do you want", &WrapUp{Accomplishments: "did x", HandoffNotes: "Which option do you want — JWT or sessions?"}},
		{"do you prefer A or B", &WrapUp{Accomplishments: "x", HandoffNotes: "Which approach do you prefer — A or B?"}},
		{"pick between", &WrapUp{Accomplishments: "done", TLDR: "operator to pick between the two migration paths"}},
		// Bucket D — operator must DO (escalation-shaped).
		{"mint secret", &WrapUp{Accomplishments: "wired ESO", Blockers: "A human must mint AWS Secrets Manager secret gardenio/foo before this works."}},
		{"transition jira", &WrapUp{Accomplishments: "fixed bug", HandoffNotes: "Operator: please transition DEVOPS-724 to Done in JIRA."}},
		{"link jira tickets", &WrapUp{Accomplishments: "x", HandoffNotes: "Need a human to link FSM-395 to PE-8269 in JIRA."}},
		{"rotate credential", &WrapUp{Accomplishments: "x", Blockers: "Awaiting operator to rotate the prod DB credential."}},
		// Bucket C — MR awaiting human approval (escalation/known-wait).
		{"mr approve merge", &WrapUp{Accomplishments: "opened MR", Blockers: "MR !4821 is not_approved — a human must approve and merge it."}},
		{"approve mr generic", &WrapUp{Accomplishments: "x", HandoffNotes: "Reviewer needs to approve the merge request before this lands."}},
		// Obligation phrasings — explicit operator/human reference.
		{"operator must", &WrapUp{Accomplishments: "x", Blockers: "The operator must grant IAM permission for the role."}},
		{"pending your approval", &WrapUp{Accomplishments: "x", HandoffNotes: "Pending your approval to deploy to prod."}},
		{"needs human", &WrapUp{Accomplishments: "x", HandoffNotes: "This needs a human to confirm the Atlassian plan tier."}},
		{"for matt to decide", &WrapUp{Accomplishments: "x", HandoffNotes: "Left for Matt to decide whether to enforce now."}},
		{"direct question to you", &WrapUp{Accomplishments: "x", Blockers: "Do you want me to also patch the bridge path?"}},
		// Bare JIRA-transition imperatives — the audit's VERBATIM stranded
		// examples. These have no operator token but are operator-directed by
		// construction (the agent cannot self-serve a JIRA transition).
		{"bare transition jira", &WrapUp{Accomplishments: "x", HandoffNotes: "Transition DEVOPS-724 to Done."}},
		{"bare link jira", &WrapUp{Accomplishments: "x", HandoffNotes: "Link FSM-395 to PE-8269 in JIRA."}},
		{"jira arrow done", &WrapUp{Accomplishments: "x", HandoffNotes: "DEVOPS-724 -> Done"}},
		{"jira needs moving", &WrapUp{Accomplishments: "x", Blockers: "DEVOPS-724 needs moving to Done."}},
		// Bare two-way fork questions (decision asks even without an operator ref).
		{"bare fork soft hard", &WrapUp{Accomplishments: "x", TLDR: "Ship soft or hard?"}},
		{"bare fork enforce warn", &WrapUp{Accomplishments: "x", Blockers: "Enforce now or warn first?"}},
		// Explicit "your call / up to you" decision handoffs.
		{"up to you", &WrapUp{Accomplishments: "x", HandoffNotes: "Rollout aggressiveness is up to you."}},
		{"your call", &WrapUp{Accomplishments: "x", HandoffNotes: "Whether to enforce now is your call."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !wrapupHasOperatorAsk(c.w) {
				t.Errorf("expected operator-ask detected for %q (fields: tldr=%q blockers=%q handoff=%q)",
					c.name, c.w.TLDR, c.w.Blockers, c.w.HandoffNotes)
			}
		})
	}
}

// TestWrapupHasOperatorAsk_Negatives covers the false-positive guardrails: a
// bare "?", a URL query string, past-tense accomplishments, a no-op/status
// wrap-up, and a wrap-up that merely references an already-open handle. None
// MUST fire. These are the cases that keep clean stops from wedging.
func TestWrapupHasOperatorAsk_Negatives(t *testing.T) {
	cases := []struct {
		name string
		w    *WrapUp
	}{
		{"nil", nil},
		{"empty", &WrapUp{}},
		{"clean done", &WrapUp{Accomplishments: "Merged MR 3286; spawned 2 crew; merged_today 0->1."}},
		{"no-op cycle", &WrapUp{Accomplishments: "release cycle: no cut, main +0", TLDR: "no-op cycle"}},
		// The crux: an operator-ask-shaped sentence in ACCOMPLISHMENTS (past
		// tense) must NOT fire, because accomplishments is never scanned.
		{"ask-shaped accomplishment", &WrapUp{Accomplishments: "Asked Matt whether to use JWT; he picked sessions. Awaiting nothing."}},
		{"past tense in accomplishments", &WrapUp{Accomplishments: "Operator approved the MR and I merged it. Should we celebrate? done."}},
		// A bare question mark with no operator token (rhetorical / status).
		{"bare question no operator", &WrapUp{Accomplishments: "x", TLDR: "fixed the flaky test (was it a race? yes)"}},
		{"rhetorical", &WrapUp{Accomplishments: "x", HandoffNotes: "Turns out the cache was stale. Who knew?"}},
		// A URL with a query string must not trip the question detector.
		{"url query", &WrapUp{Accomplishments: "x", HandoffNotes: "Report at https://gardenio.app/beads/x?foo=1&bar=2 for details."}},
		// ── Next-agent "you" idioms (the dominant false-positive class the
		// precision redesign fixed — adversarial review ka-0000002541). "you"
		// in handoff_notes/blockers addresses the NEXT AGENT, not the operator.
		{"status with you can", &WrapUp{Accomplishments: "x", HandoffNotes: "Next agent: you can pick up the helm bump; it's straightforward."}},
		{"you should next", &WrapUp{Accomplishments: "x", HandoffNotes: "You should run make preflight before pushing."}},
		{"you must rebase", &WrapUp{Accomplishments: "x", HandoffNotes: "You must rebase on main first; main moved."}},
		{"you take it from here", &WrapUp{Accomplishments: "x", HandoffNotes: "Handed the bead off. You take it from here?"}},
		{"left for you to review", &WrapUp{Accomplishments: "x", HandoffNotes: "Left the diff for you to review."}},
		{"someone should eventually", &WrapUp{Accomplishments: "x", HandoffNotes: "Someone should refactor the helper eventually; works fine now."}},
		// ── Status blockers (not operator asks): "blocked on review", "pending
		// decision", "waiting for CI" are the natural words for a dependency.
		{"blocked on review", &WrapUp{Accomplishments: "x", Blockers: "Change 2 is blocked on review of the reconciler refactor."}},
		{"waiting for CI", &WrapUp{Accomplishments: "x", Blockers: "Tests pending; waiting for the CI input to settle, no action needed."}},
		{"pending decision generic", &WrapUp{Accomplishments: "x", Blockers: "Pending decision on naming, leaning toward foo."}},
		// ── Capability / permissive ("you can deploy") and past-tense status.
		{"capability you can deploy", &WrapUp{Accomplishments: "x", HandoffNotes: "When ready you can deploy to prod via the release pipeline; nothing blocking."}},
		{"past tense closed", &WrapUp{Accomplishments: "x", HandoffNotes: "You closed DEVOPS-724 to done already; nothing pending."}},
		{"deployed status", &WrapUp{Accomplishments: "x", HandoffNotes: "Deployed to the prod cluster; all green."}},
		// ── Rhetorical / self-reflective musings with no operator address and no
		// explicit fork. (A genuine "X or Y?" fork DOES fire — see the Positives
		// and Routing tables — so these negatives are deliberately fork-free or
		// uncertainty-framed.)
		{"should we no fork", &WrapUp{Accomplishments: "x", TLDR: "Should we have shipped on Friday?"}},
		{"which path unclear", &WrapUp{Accomplishments: "x", HandoffNotes: "Which path the reconciler takes on conflict is still unclear?"}},
		{"uncertainty fork musing", &WrapUp{Accomplishments: "x", HandoffNotes: "Not sure if it was a race or a deadlock?"}},
		{"jira past tense done", &WrapUp{Accomplishments: "x", HandoffNotes: "Moved DEVOPS-724 to Done already; nothing pending."}},
		// Pure progress handoff, forward-looking but no operator ask.
		{"plain handoff", &WrapUp{Accomplishments: "x", HandoffNotes: "Change 3/4 still blocked on Change 2; next cycle walk crew MRs to green."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if wrapupHasOperatorAsk(c.w) {
				k := classifyAskKind(c.w)
				t.Errorf("expected NO operator-ask for %q but detector fired (kind=%s); fields: tldr=%q blockers=%q handoff=%q accomplishments=%q",
					c.name, k, c.w.TLDR, c.w.Blockers, c.w.HandoffNotes, c.w.Accomplishments)
			}
		})
	}
}

// TestClassifyAskKind_Routing pins the decision-vs-escalation routing. DO-asks
// (mint/rotate secret, approve MR, transition JIRA, grant IAM, deploy) route to
// escalation; CHOOSE-asks (should-we / which / A-or-B / pick) route to decision.
func TestClassifyAskKind_Routing(t *testing.T) {
	cases := []struct {
		name string
		w    *WrapUp
		want AskKind
	}{
		{"mint secret -> escalation", &WrapUp{Blockers: "A human must mint AWS Secrets Manager secret X."}, AskEscalation},
		{"transition jira -> escalation", &WrapUp{HandoffNotes: "Operator must transition DEVOPS-724 to Done."}, AskEscalation},
		{"approve mr -> escalation", &WrapUp{Blockers: "MR !4821 needs a human to approve and merge."}, AskEscalation},
		{"grant iam -> escalation", &WrapUp{Blockers: "The operator must grant IAM access to the role."}, AskEscalation},
		{"rotate cred -> escalation", &WrapUp{Blockers: "Awaiting operator to rotate the prod credential."}, AskEscalation},
		{"do you prefer -> decision", &WrapUp{HandoffNotes: "Which approach do you prefer — A or B?"}, AskDecision},
		{"matt to pick -> decision", &WrapUp{HandoffNotes: "For Matt to pick between the two paths."}, AskDecision},
		{"can you confirm -> ask", &WrapUp{HandoffNotes: "Can you confirm the plan tier?"}, AskEscalation},
		{"bare jira transition -> escalation", &WrapUp{HandoffNotes: "Transition DEVOPS-724 to Done."}, AskEscalation},
		{"bare jira link -> escalation", &WrapUp{HandoffNotes: "Link FSM-395 to PE-8269 in JIRA."}, AskEscalation},
		{"your call -> decision", &WrapUp{HandoffNotes: "Whether to enforce is your call."}, AskDecision},
		{"no ask -> none", &WrapUp{Accomplishments: "merged MR 1"}, AskNone},
		// A bare two-way FORK question ("X or Y?") IS a decision ask — presenting
		// alternatives for a pick is operator-directed by construction.
		{"bare fork -> decision", &WrapUp{Blockers: "Should we enforce now or warn first?"}, AskDecision},
		// A single-clause "should we…?" musing with NO fork and no operator
		// reference is intentionally NOT an ask (precision: rhetorical musing).
		{"bare should-we no fork -> none", &WrapUp{TLDR: "Should we have shipped this on Friday?"}, AskNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyAskKind(c.w); got != c.want {
				t.Errorf("classifyAskKind = %s, want %s (blockers=%q handoff=%q)",
					got, c.want, c.w.Blockers, c.w.HandoffNotes)
			}
		})
	}
}

// TestWrapupHasOperatorAsk_PassiveEscalation covers OBJECT-FIRST / passive-voice
// secret-and-credential DO-asks — the audit's most-damaging class is frequently
// phrased passively ("the secret needs to be created", "the credential is
// rotated") rather than actively ("a human must mint the secret"). These MUST
// fire (a missed secret-mint is a permanent stranded ask), while the same nouns
// in COMPLETED-status prose must NOT (regression guard).
func TestWrapupHasOperatorAsk_PassiveEscalation(t *testing.T) {
	fire := map[string]*WrapUp{
		"secret needs to be created":   {Blockers: "The gardenio/foo secret needs to be created in Secrets Manager."},
		"credential is rotated":        {Blockers: "This is blocked until the prod database credential is rotated."},
		"api key being provisioned":    {Blockers: "Cannot proceed without the API key being provisioned."},
		"secret still to be populated": {HandoffNotes: "The secret in Secrets Manager still has to be populated."},
		"secret must be created":       {Blockers: "The AWS secret must be created before integration tests pass."},
	}
	for name, w := range fire {
		if got := classifyAskKind(w); got != AskEscalation {
			t.Errorf("%q: want AskEscalation, got %s", name, got)
		}
	}
	noFire := map[string]*WrapUp{
		"secret created done":   {HandoffNotes: "The secret was created and synced; nothing pending."},
		"rotated already":       {HandoffNotes: "Credential rotated already; all green."},
		"you can create secret": {HandoffNotes: "You can create the secret later if needed; not blocking."},
		"accomplishment create": {Accomplishments: "Created the secret and merged MR 1."},
	}
	for name, w := range noFire {
		if wrapupHasOperatorAsk(w) {
			t.Errorf("%q: must NOT fire (kind=%s)", name, classifyAskKind(w))
		}
	}
}

// TestWrapupHasOperatorAsk_VerbInflection covers inflected privileged-action
// verbs (-ing/-ed) in obligation-anchored asks, and the matching past-tense
// status that must NOT fire (the self-firing JIRA cue is restricted to
// present-tense + a clause-initial past-tense exclusion).
func TestWrapupHasOperatorAsk_VerbInflection(t *testing.T) {
	fire := []string{
		"Operator should be moving DEVOPS-724 to Done.",
		"A human must be granting IAM access to the role.",
		"DEVOPS-724 needs transitioning to Done.",
		"Needs approving by a human: MR !55.",
	}
	for _, s := range fire {
		if !wrapupHasOperatorAsk(&WrapUp{HandoffNotes: s}) {
			t.Errorf("inflected ask %q should fire", s)
		}
	}
	noFire := []string{
		"Moved DEVOPS-724 to Done; all set.",
		"Linked FSM-395 to PE-8269 earlier today.",
		"Closed DEVOPS-724 after the fix landed.",
		"Granted access last week; confirmed working.",
		"Fixed the bug in DEVOPS-724; tests pass.",
	}
	for _, s := range noFire {
		if wrapupHasOperatorAsk(&WrapUp{Accomplishments: "x", HandoffNotes: s}) {
			t.Errorf("past-tense status %q must NOT fire (kind=%s)", s, classifyAskKind(&WrapUp{HandoffNotes: s}))
		}
	}
}

// TestClassifyAskKind_EscalationWinsTie verifies that when a wrap-up carries
// both a DO-cue and a CHOOSE-cue, escalation wins (higher-confidence, more
// damaging class).
func TestClassifyAskKind_EscalationWinsTie(t *testing.T) {
	w := &WrapUp{Blockers: "Should we ask the operator to mint the AWS secret, or skip it?"}
	if got := classifyAskKind(w); got != AskEscalation {
		t.Errorf("escalation should win the tie, got %s", got)
	}
}

// TestWrapupReferencesKDHandle verifies gd- id extraction from forward-looking
// fields only.
func TestWrapupReferencesKDHandle(t *testing.T) {
	w := &WrapUp{
		Accomplishments: "see gd-V5VVSLWVHV (must not be picked up — accomplishments not scanned)",
		HandoffNotes:    "Deferring to open decision gd-16AZKZP6LX.",
	}
	refs := wrapupReferencesKDHandle(w)
	if len(refs) != 1 || refs[0] != "gd-16AZKZP6LX" {
		t.Errorf("expected [gd-16AZKZP6LX], got %v", refs)
	}
}

// TestAskGateMode covers the rollout-mode env parsing and the safe default.
func TestAskGateMode(t *testing.T) {
	cases := []struct {
		env  string
		want AskGateMode
	}{
		{"", AskGateWarn},
		{"warn", AskGateWarn},
		{"WARN", AskGateWarn},
		{"enforce", AskGateEnforce},
		{"Enforce", AskGateEnforce},
		{"off", AskGateOff},
		{"disabled", AskGateOff},
		{"garbage", AskGateWarn},
	}
	for _, c := range cases {
		t.Run("env="+c.env, func(t *testing.T) {
			t.Setenv("GB_STOP_ASK_GATE", c.env)
			if got := askGateMode(); got != c.want {
				t.Errorf("askGateMode(%q) = %s, want %s", c.env, got, c.want)
			}
		})
	}
}

// TestAskKindString covers the String() methods used in telemetry.
func TestAskKindString(t *testing.T) {
	if AskNone.String() != "none" || AskDecision.String() != "decision" || AskEscalation.String() != "escalation" {
		t.Errorf("AskKind.String mismatch: %s/%s/%s", AskNone, AskDecision, AskEscalation)
	}
	if AskGateWarn.String() != "warn" || AskGateEnforce.String() != "enforce" || AskGateOff.String() != "off" {
		t.Errorf("AskGateMode.String mismatch")
	}
}

// TestWrapupPuntsToGardener covers the crew-punt-to-gardener detector (T7).
func TestWrapupPuntsToGardener(t *testing.T) {
	pos := []*WrapUp{
		{HandoffNotes: "Gardener to file the gb decision on rollout aggressiveness."},
		{Blockers: "Punting to first-mate to decide whether to enforce."},
		{HandoffNotes: "Handing off to the gardener to escalate the secret mint."},
	}
	for i, w := range pos {
		if !wrapupPuntsToGardener(w) {
			t.Errorf("pos[%d] should detect gardener punt: %+v", i, w)
		}
	}
	neg := []*WrapUp{
		nil,
		{Accomplishments: "gardener merged my MR"}, // accomplishments not scanned
		{HandoffNotes: "Next agent can pick up the helm bump."},
		{TLDR: "fixed flaky test"},
	}
	for i, w := range neg {
		if wrapupPuntsToGardener(w) {
			t.Errorf("neg[%d] should NOT detect gardener punt: %+v", i, w)
		}
	}
}

// TestEmitAskRemediation_GardenerPuntNote verifies the visibility note appears
// when the wrap-up punts to a gardener.
func TestEmitAskRemediation_GardenerPuntNote(t *testing.T) {
	var sb strings.Builder
	emitAskRemediation(&sb, AskDecision, &WrapUp{HandoffNotes: "Gardener to decide whether to enforce now or warn first?"})
	got := sb.String()
	if !strings.Contains(got, "punting this to your gardener") {
		t.Errorf("expected gardener-punt visibility note, got:\n%s", got)
	}
	// And it must NOT appear for a plain operator ask.
	var sb2 strings.Builder
	emitAskRemediation(&sb2, AskEscalation, &WrapUp{Blockers: "A human must mint the AWS secret."})
	if strings.Contains(sb2.String(), "punting this to your gardener") {
		t.Errorf("gardener-punt note must not appear for a plain operator ask")
	}
}

// TestEmitAskRemediation_AlwaysOffersExit verifies the remediation always names
// a zero-decision exit (rephrase / --force) and the correct command per kind —
// the anti-ka-0000007734 loop escape.
func TestEmitAskRemediation_AlwaysOffersExit(t *testing.T) {
	for _, kind := range []AskKind{AskDecision, AskEscalation} {
		var sb strings.Builder
		emitAskRemediation(&sb, kind, &WrapUp{Blockers: "Should we do X? Operator must approve."})
		got := sb.String()
		if !strings.Contains(got, "--force") {
			t.Errorf("kind=%s remediation must offer --force exit, got:\n%s", kind, got)
		}
		if !strings.Contains(got, "restate the wrap-up") {
			t.Errorf("kind=%s remediation must offer rephrase exit, got:\n%s", kind, got)
		}
		if kind == AskEscalation && !strings.Contains(got, "gb escalation create") {
			t.Errorf("escalation remediation must name gb escalation create, got:\n%s", got)
		}
		if kind == AskDecision && !strings.Contains(got, "gb decision create") {
			t.Errorf("decision remediation must name gb decision create, got:\n%s", got)
		}
	}
}
