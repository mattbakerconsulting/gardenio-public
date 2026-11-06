package main

// ask_gate.go — the gb stop "stranded operator-ask" soft-gate.
//
// Background (epic ka-0000002541, report /bundle/ka-0000002636): an audit of
// 4,000 closed sessions found 17.3% pose an operator-directed ask at gb stop;
// 23% of those (158, ~51 in never-respawn roles = permanent) file NO decision
// and NO escalation and reference no open handle — the operator gets no
// re-entry point and the ask dies as dead text in a closed bead.
//
// Root cause: decisionGateRequired() returns "clean done" whenever
// isDoneWrapUp() is true, and isDoneWrapUp == (accomplishments != ""). So a
// question parked in tldr / blockers / handoff_notes sails straight through
// the gate as long as accomplishments is non-empty.
//
// This file adds the load-bearing fix:
//
//   1. wrapupHasOperatorAsk(*WrapUp) — scans ONLY the forward-looking fields
//      (tldr, blockers, handoff_notes), NEVER accomplishments (past-tense), for
//      an anchored, second-person/fork-shaped operator ask. A bare "?" does not
//      fire. This tightens the clean-done carve-out to
//      isDoneWrapUp(w) && !wrapupHasOperatorAsk(w): a strict subset of today's
//      behavior, so no no-ask clean stop regresses.
//
//   2. classifyAskKind(*WrapUp) — routes a detected ask to escalation (the
//      operator must DO a concrete privileged action: mint/rotate a secret,
//      approve/merge an MR, transition a JIRA ticket, grant IAM, deploy) vs
//      decision (the operator must CHOOSE between divergent next actions). The
//      boundary is fuzzy; requireOperatorHandle accepts EITHER handle.
//
// The cue tables are deliberately small, anchored, and unit-tested against the
// real stranded/healthy phrasings from the audit (see ask_gate_test.go). Tuning
// happens from real post-ship telemetry (T6), not from speculation.

import (
	"os"
	"regexp"
	"slices"
	"strings"
)

// AskKind classifies the shape of a detected operator-directed ask so the gate
// can route it to the right handle and word its remediation precisely.
type AskKind int

const (
	// AskNone — no operator-directed ask detected in the wrap-up.
	AskNone AskKind = iota
	// AskDecision — the operator must CHOOSE between divergent next actions
	// ("should we", "which", "A or B", "pick"). Right artifact: gb decision.
	AskDecision
	// AskEscalation — the operator must DO a concrete privileged action only a
	// human/system with elevation can perform (mint/rotate a secret, approve an
	// MR, transition a JIRA ticket, grant IAM, deploy). Right artifact:
	// gb escalation.
	AskEscalation
)

func (k AskKind) String() string {
	switch k {
	case AskDecision:
		return "decision"
	case AskEscalation:
		return "escalation"
	default:
		return "none"
	}
}

// operatorToken matches an EXPLICIT operator/human reference. Critically it
// does NOT include a bare second-person "you"/"your": in blockers/handoff_notes
// the "you" is overwhelmingly the NEXT AGENT ("you should rebase", "you can
// deploy"), not the operator. Conflating the two was the dominant false-positive
// (an adversarial review found ~9/10 normal handoffs tripped the old gate). An
// operator ask must name the operator explicitly — Matt, the operator, a human,
// the reviewer, etc. The second-person REQUEST shapes that DO address the
// operator ("can you", "could you", "would you", "please") are matched
// separately by requestQuestionCue, not by this token.
var operatorToken = regexp.MustCompile(`(?i)\b(matt|matt baker|the operator|operator|a human|the human|reviewer|admin|on-call|maintainer)\b`)

// pastOrPermissive marks a clause that is NARRATING completed work or stating a
// CAPABILITY rather than parking a live ask. A clause matching this is excluded
// from per-clause escalation/decision detection: "you can deploy to prod"
// (capability), "nothing pending" / "no action needed" (explicit non-ask),
// "already done" (completed). It is deliberately NARROW and anchored to clear
// capability/completion markers — it does NOT use a broad `\w+ed` past-tense
// heuristic, because that wrongly suppressed real asks whose action is the
// object of an obligation ("must be approved", "needs to be linked"). The
// escalation cues already require an obligation anchor (must/needs to/please),
// which excludes pure past-tense narration on its own.
var pastOrPermissive = regexp.MustCompile(`(?i)(\byou (can|could|may)\b|\b(already|no action needed|nothing (pending|blocking|left)|fyi|feel free|free to|when ready|when green|once (ci|it|the))\b|\b(done|merged|deployed|closed|completed|finished|resolved) (already|and|—|-|,|\.))`)

// pastTenseLead matches a clause that OPENS with a past-tense action verb —
// status narration ("Moved DEVOPS-724 to Done", "Linked FSM-395 to PE-8269",
// "Closed the ticket", "Granted access", "Deployed to prod"). Real asks lead
// with an obligation ("Operator must move…") or the ticket id + "needs", not a
// bare past-tense verb, so excluding leading past-tense is safe and catches the
// self-firing-cue past-tense leak. Anchored to the clause start (after optional
// list bullets / whitespace).
var pastTenseLead = regexp.MustCompile(`(?i)^\s*[-*>\s]*\b(moved|linked|closed|transitioned|merged|approved|deployed|granted|rotated|minted|provisioned|created|resolved|reopened|related|populated|seeded|generated)\b`)

// awaitCues fire on their own — each phrase unambiguously parks a live ask on
// the OPERATOR. Every entry now requires an EXPLICIT operator/human reference
// (not a bare "you"), so a next-agent handoff ("you should walk the MRs",
// "blocked on review") does not fire. Anchored to await/obligation verbs.
var awaitCues = []*regexp.Regexp{
	// awaiting / waiting on / blocked on / pending — but only when an explicit
	// operator/human reference (or an unambiguously operator-only artifact like
	// a secret/credential/sign-off) is named. Generic "blocked on review/CI/
	// input/decision" is a status blocker, not an operator ask, and is excluded.
	regexp.MustCompile(`(?i)\b(awaiting|await|waiting on|waiting for|blocked on|pending|need|needs)\b[^.?!]{0,60}\b(matt|the operator|operator|a human|the human|human approval|operator approval|operator decision|sign[- ]?off|secret|credential|provisioning|manual intervention)\b`),
	// Matt / the operator / a human must|should|needs to|has to <verb>.
	// Requires an explicit operator reference immediately before the obligation.
	regexp.MustCompile(`(?i)\b(matt|the operator|operator|a human|the human|reviewer|admin|on-call)\b[^.?!]{0,24}\b(must|should|needs? to|has to|will need to|to please|please)\b`),
	// needs/requires Matt/the operator/a human/approval/sign-off/manual...
	regexp.MustCompile(`(?i)\b(needs?|requires?|require)\b[^.?!]{0,30}\b(matt|the operator|operator|a human|human approval|operator approval|sign[- ]?off|manual (intervention|step))\b`),
	// explicit "operator/human/manual decision|approval|sign-off ... required|needed"
	regexp.MustCompile(`(?i)\b(operator|human|manual)\b[^.?!]{0,24}\b(decision|approval|sign[- ]?off|intervention|action|input)\b[^.?!]{0,20}\b(required|needed|requested)\b`),
	// "left for Matt/the operator to decide/approve/action" — explicit operator only.
	regexp.MustCompile(`(?i)\bfor\b\s+(matt|the operator|operator|a human|the human)\s+to\s+\w+`),
	// "your approval/sign-off/go-ahead/decision/review" — a second-person
	// POSSESSIVE attached to an operator-action noun is a genuine request to the
	// reader to grant it. This is narrower than bare "you" (which addresses the
	// next agent) — "your approval" specifically asks the reader to approve.
	regexp.MustCompile(`(?i)\byour\b\s+\b(approval|sign[- ]?off|go[- ]?ahead|decision|review|confirmation|blessing|ok|okay|green[- ]?light)\b`),
}

// escalationCues identify a DO-ask: a concrete privileged action only an
// elevated human/system can perform. Each requires an OBLIGATION/REQUEST anchor
// (must/needs to/please/awaiting/required) co-located with the privileged
// action, so permissive ("you can deploy") and past-tense ("deployed",
// "closed") framings do NOT fire. Mirrors the escalation.go Long-help litmus.
var escalationCues = []*regexp.Regexp{
	// secret/credential lifecycle the agent cannot self-serve — active voice,
	// anchored to an ask ("must mint the secret", "need to rotate the cred").
	regexp.MustCompile(`(?i)\b(must|need|needs?|please|awaiting|required to|to)\b[^.?!]{0,24}\b(mint|rotate|provision|seed|generate|create)\b[^.?!]{0,30}\b(secret|credential|cred|token|api key|access key|password)\b`),
	regexp.MustCompile(`(?i)\b(mint|rotate|provision|seed)\b[^.?!]{0,30}\b(secret|credential|cred|token|access key|password)\b[^.?!]{0,30}\b(required|needed|please|by a human|manually)\b`),
	regexp.MustCompile(`(?i)\b(a human|the operator|matt|someone)\b[^.?!]{0,24}\b(must|needs? to|to)\b[^.?!]{0,24}\b(mint|rotate|provision|approve|merge|transition|grant|deploy)\b`),
	// secret/credential — PASSIVE / object-first voice ("the secret needs to be
	// created", "the credential is rotated", "the api key being provisioned",
	// "the secret still has to be populated"). The audit's most-damaging class
	// is frequently phrased object-first; a missed secret-mint is a permanent
	// stranded ask. Anchored to a passive obligation ("needs/has/to be" or
	// "being/is" + the privileged verb) so completed-status ("secret was
	// created") still does not fire — the pastOrPermissive clause exclusion and
	// the required obligation token together keep this tight.
	regexp.MustCompile(`(?i)\b(secret|credential|cred|token|api key|access key|password)\b[^.?!]{0,40}\b(needs? to be|has to be|have to be|must be|to be|being|still (needs?|has) to be)\b[^.?!]{0,12}\b(created|minted|rotated|provisioned|seeded|generated|populated|set up|added|granted)\b`),
	regexp.MustCompile(`(?i)\b(needs?|need|cannot proceed without|blocked (until|on)|requires?)\b[^.?!]{0,30}\b(secret|credential|cred|token|api key|access key|password)\b[^.?!]{0,24}\b(created|minted|rotated|provisioned|seeded|generated|populated|being (created|provisioned|rotated))\b`),
	// MR / PR approval + merge — anchored to an obligation, not a capability.
	// Verb stems carry optional inflection suffixes (approve/approved/approving).
	regexp.MustCompile(`(?i)\b(must|needs?|need|please|awaiting|require[sd]?)\b[^.?\n]{0,30}\b(approv\w*|merg\w*|sign[- ]?off on)\b[^.?\n]{0,24}\b(mr|!?\d{2,}|pr|merge request|pull request|it|this)\b`),
	regexp.MustCompile(`(?i)\bmr\b[^.?\n]{0,30}\b(not_approved|needs? (a )?human|awaiting (approval|review)|must be (approved|merged))\b`),
	// JIRA / ticket transitions and links — anchored to an obligation/request.
	// Verb stems allow inflections (transition/transitioned/transitioning,
	// move/moved/moving, link/linked/linking).
	regexp.MustCompile(`(?i)\b(must|need|needs? to|please|awaiting|to)\b[^.?!]{0,24}\b(transition\w*|mov\w*|clos\w*|link\w*|relat\w*)\b[^.?!]{0,30}\b([A-Z][A-Z0-9]+-\d+|jira|ticket)\b`),
	regexp.MustCompile(`(?i)\b([A-Z][A-Z0-9]+-\d+)\b[^.?!]{0,30}\b(needs? (to be |a )?(mov\w*|transition\w*|link\w*)|must be (mov\w*|transition\w*|link\w*)|to be (mov\w*|transition\w*|link\w*))\b`),
	// IAM / RBAC / access grants — anchored to an ask. Stems allow inflection.
	regexp.MustCompile(`(?i)\b(must|need|needs? to|please|awaiting)\b[^.?!]{0,24}\b(grant\w*|widen\w*|extend\w*|provision\w*)\b[^.?!]{0,24}\b(iam|rbac|role|policy|permission|access)\b`),
	// confirm/verify an external account/plan tier — anchored to an ask.
	regexp.MustCompile(`(?i)\b(must|need|needs? to|please|awaiting|can you|could you)\b[^.?!]{0,24}\b(confirm|verify|enable)\b[^.?!]{0,30}\b(plan tier|subscription|account|license|seat)\b`),
}

// selfFiringEscalationCues are privileged DO-actions whose mere presence in a
// forward-looking field is operator-directed BY CONSTRUCTION — the agent cannot
// self-serve them, so a terse imperative ("transition DEVOPS-724 to Done",
// "link FSM-395 to PE-8269", "DEVOPS-724 -> Done") needs no operator token. The
// audit's verbatim most-damaging stranded examples are exactly this shape. They
// fire WITHOUT an operator token (unlike escalationCues, which need one of the
// obligation anchors) but are STILL subject to the per-clause pastOrPermissive
// exclusion, so a completed-status clause ("moved DEVOPS-724 to Done already")
// does not fire. The ticket-id pattern ([A-Z]{2,}-\d+) keeps these specific.
var selfFiringEscalationCues = []*regexp.Regexp{
	// JIRA ticket transition / link: a TICKET-ID adjacent to a transition or
	// link verb, in either order. The verb is restricted to PRESENT-TENSE
	// imperative forms (transition/move/close/link, not transitioned/moved/
	// closed/linked) because this cue is SELF-FIRING (no obligation anchor) — a
	// past-tense clause-initial verb ("Moved DEVOPS-724 to Done") is status
	// narration, not an ask, and must not fire. Past-tense inflections that ARE
	// asks ("needs moving to Done") are caught by the obligation-anchored
	// escalationCues above. The "-s" present form (moves/links) is allowed.
	regexp.MustCompile(`(?i)\b(transitions?|moves?|closes?|links?|relates?|reopens?|resolves?)\b[^.?\n]{0,30}\b[A-Z][A-Z0-9]+-\d+\b`),
	regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9]+-\d+\b[^.?\n]{0,30}\b(to (done|in[- ]review|closed|in[- ]progress|backlog|qa)|transition|link(ed)? to|needs? moving|moving to)\b`),
	regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9]+-\d+\b\s*(->|→|=>)\s*\w+`),
	regexp.MustCompile(`(?i)\blink\b[^.?\n]{0,24}\b[A-Z][A-Z0-9]+-\d+\b[^.?\n]{0,24}\b(to|with|and)\b[^.?\n]{0,12}\b[A-Z][A-Z0-9]+-\d+\b`),
}

// decisionCues identify a CHOOSE-ask: a judgment fork. These are intentionally
// NOT self-firing — they fire only when co-located with an operator token or a
// request-question shape (see classifyAskKind), because a bare "should we…?" /
// "which path…?" in handoff prose is overwhelmingly a rhetorical/self-answered
// musing, not a parked operator ask.
var decisionCues = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bshould (we|i|it|the)\b`),
	regexp.MustCompile(`(?i)\b(which|what)\b[^.?!]{0,40}\b(option|approach|path|strategy|one)\b`),
	regexp.MustCompile(`(?i)\b(a or b|option a|option b|\(a\)|\(b\))`),
	regexp.MustCompile(`(?i)\b(pick|choose|decide|select)\b[^.?!]{0,30}\b(between|which|option|approach|path|one|whether)\b`),
	regexp.MustCompile(`(?i)\bwhether to\b`),
}

// requestQuestionCue matches a SECOND-PERSON REQUEST directed at the operator —
// the legitimate way a wrap-up question addresses the human ("can you confirm",
// "could you approve", "would you", "do you want me to", "please …?"). These DO
// fire (co-located with a "?") because they are unambiguously operator-directed,
// unlike a bare "you …?" conversational sign-off ("you good?", "right?").
var requestQuestionCue = regexp.MustCompile(`(?i)\b(can you|could you|would you|will you|do you want|do you prefer|would you prefer|which do you|what do you want|should i|shall i|may i|please (confirm|approve|review|decide|advise|let me know|pick|choose|merge|deploy))\b`)

// forkQuestionCue matches a bare two-way fork phrased as a question — "ship soft
// or hard?", "enforce now or warn first?", "A or B?". A question line that
// offers an explicit "X or Y" choice is a decision ask even without an operator
// token, because presenting alternatives for a pick is operator-directed by
// construction. The line must END in "?" (questionLine guarantees this) and
// contain an " or " fork. The negative-lookahead-free guard against a
// self-answered rhetorical ("was it a race or a deadlock? cleared it") is the
// per-line application plus rhetoricalSignoff/"not sure" handled by callers.
var forkQuestionCue = regexp.MustCompile(`(?i)\b\w+\b\s+or\s+\b\w+\b`)

// uncertaintyMusing suppresses a fork question that is really a self-reflective
// uncertainty ("not sure if X or Y?", "was it a race or a deadlock?", "wonder
// if A or B?") rather than a choice presented to the operator.
var uncertaintyMusing = regexp.MustCompile(`(?i)\b(not sure|unsure|wonder(ing)?|was it|is it|curious|no idea|unclear|who knows)\b`)

// decisionHandoffCue matches an explicit hand-of-the-decision-to-the-reader
// idiom ("up to you", "your call", "your decision", "you decide", "leaving the
// call to you", "needs your go-ahead"). Unlike a bare "you", these specifically
// delegate the CHOICE to the operator, so they are operator-directed asks.
var decisionHandoffCue = regexp.MustCompile(`(?i)\b(up to you|your call|your decision|you decide|you to decide|leaving (it|the call|the decision) to you|needs? your (go[- ]?ahead|call|decision|sign[- ]?off|approval|input))\b`)

// rhetoricalSignoff matches conversational filler questions that are NOT asks
// ("right?", "you good?", "you know?", "make sense?", "who knew?"). A question
// line matching this is suppressed even if it contains a stray token.
var rhetoricalSignoff = regexp.MustCompile(`(?i)\b(right|you good|you know|make[s]? sense|who knew|isn'?t it|capisce|ok|okay|cool|sound good|good to go|take it from here)\b\??\s*$`)

// questionLine matches a clause ending in "?". URL query strings are stripped
// first so "?query=" never reaches here.
var questionLine = regexp.MustCompile(`(?m)[^.\n!]*\?\s*$`)

// urlPattern is stripped before scanning so a "?" inside a query string
// ("...beads/x?foo=1") and an "&" do not trip the question detector.
var urlPattern = regexp.MustCompile(`https?://\S+`)

// openHandleRef matches an inline reference to an already-open handle the agent
// is deferring to ("see ka-ABC123", "decision ka-ABC123 open"). The presence of
// a gd- reference is corroborated against the live decision/escalation sets by
// requireOperatorHandle; here it only informs detection nuance, not exemption.
var kdRefPattern = regexp.MustCompile(`\b(?:gd|ka)-[A-Za-z0-9]{6,}\b`)

// wrapupHasOperatorAsk reports whether the wrap-up parks an unhandled
// operator-directed ask in its forward-looking fields. It scans ONLY
// tldr + blockers + handoff_notes — never accomplishments, which is past-tense
// and would produce false positives from completed-work narration.
//
// Returns false for a nil wrap-up. The detection is the union of:
//   - any awaitCue (self-contained await/obligation phrase), OR
//   - a direct question line co-located with an operator token, OR
//   - an escalation cue or decision cue co-located with an operator token.
//
// A bare "?" alone, a URL query string, and a past-tense narration do not fire.
func wrapupHasOperatorAsk(w *WrapUp) bool {
	return classifyAskKind(w) != AskNone
}

// classifyAskKind returns AskNone if no operator ask is detected, else routes
// the detected ask to AskEscalation (DO-ask) or AskDecision (CHOOSE-ask).
// Escalation cues win ties: a "DO this privileged thing" phrasing is the
// higher-confidence, more-damaging class (all 4 escalation-shaped strands in
// the audit verified genuine), so when both a DO-cue and a CHOOSE-cue match we
// route to escalation.
func classifyAskKind(w *WrapUp) AskKind {
	if w == nil {
		return AskNone
	}
	// Scan ONLY forward-looking fields. Accomplishments (past-tense) is
	// deliberately excluded — that is the whole point of the tightened
	// carve-out.
	scanned := []string{w.TLDR, w.Blockers, w.HandoffNotes}
	text := strings.Join(scanned, "\n")
	// Strip URLs so "?query=" and "&" don't trip the question detector.
	clean := urlPattern.ReplaceAllString(text, " ")
	if strings.TrimSpace(clean) == "" {
		return AskNone
	}

	// Detection is per-clause so the past/permissive exclusion applies locally:
	// a sentence narrating completed work or stating a capability ("you can
	// deploy to prod", "you closed DEVOPS-724 already") is skipped even if a
	// privileged-action verb appears in it. Clauses are split on sentence
	// punctuation and newlines.
	escalationHit := false
	decisionHit := false

	// (1) Await/obligation phrases — each already requires an explicit operator
	// reference, so they may fire across the whole blob (they cannot match a
	// bare next-agent "you").
	awaitHit := matchesAny(clean, awaitCues)

	// (2) Per-clause escalation/decision detection with the status exclusion.
	for _, clause := range splitClauses(clean) {
		if strings.TrimSpace(clause) == "" {
			continue
		}
		// Skip clauses that are status/capability narration, or that OPEN with a
		// past-tense action verb ("Moved DEVOPS-724 to Done" = status, not ask).
		if pastOrPermissive.MatchString(clause) || pastTenseLead.MatchString(clause) {
			continue
		}
		// Obligation-anchored escalation cues, OR a self-firing privileged
		// imperative (a JIRA ticket transition/link) which is operator-directed
		// by construction and needs no operator token — these are the audit's
		// most-damaging stranded shape ("transition DEVOPS-724 to Done").
		if matchesAny(clause, escalationCues) || matchesAny(clause, selfFiringEscalationCues) {
			escalationHit = true
		}
		// A decision cue counts when the clause names the operator ("Matt to
		// pick between A and B") OR explicitly hands the CHOICE to the reader
		// ("up to you", "your call") — a bare "should we…" musing does not fire.
		if matchesAny(clause, decisionCues) && (operatorToken.MatchString(clause) || decisionHandoffCue.MatchString(clause)) {
			decisionHit = true
		}
		if decisionHandoffCue.MatchString(clause) {
			decisionHit = true
		}
	}

	// (3) Operator-directed question lines. A question fires when it is a
	// second-person REQUEST to the operator ("can you confirm…?", "should I…?",
	// "please approve…?"), an explicit two-way FORK ("ship soft or hard?",
	// "enforce now or warn first?"), OR it co-locates an operator token with a
	// fork cue ("Matt — which option?"). A bare "you …?" conversational sign-off
	// ("you good?", "take it from here?") is suppressed by rhetoricalSignoff,
	// and a token-less single-clause "should we…?" musing does not fire.
	questionHit := false
	for _, line := range questionLine.FindAllString(clean, -1) {
		if rhetoricalSignoff.MatchString(line) {
			continue
		}
		if requestQuestionCue.MatchString(line) {
			questionHit = true
			break
		}
		// A bare "X or Y?" fork is a decision ask — unless it is a
		// self-reflective uncertainty musing ("not sure if X or Y?").
		if forkQuestionCue.MatchString(line) && !uncertaintyMusing.MatchString(line) {
			questionHit = true
			decisionHit = true
			break
		}
		if operatorToken.MatchString(line) && matchesAny(line, decisionCues) {
			questionHit = true
			decisionHit = true
			break
		}
	}

	detected := awaitHit || questionHit || escalationHit || decisionHit
	if !detected {
		return AskNone
	}

	// Routing: escalation (DO) wins over decision (CHOOSE) on a tie because it
	// is the higher-confidence, more-damaging class.
	if escalationHit {
		return AskEscalation
	}
	if decisionHit {
		return AskDecision
	}
	// An await-only ask ("awaiting the operator to mint the secret"): the
	// per-clause pass may have been suppressed by the status exclusion, so
	// classify the kind directly from the await content. A DO-shaped await
	// (mint/rotate/approve/transition/grant/deploy a secret/MR/ticket/IAM/prod)
	// routes to escalation; otherwise default to decision (the lower-privilege
	// handle — requireOperatorHandle accepts either, so a mis-route still passes
	// once any handle exists).
	if awaitEscalationShape.MatchString(clean) {
		return AskEscalation
	}
	return AskDecision
}

// awaitEscalationShape recognizes a DO-shaped await/obligation so an await-only
// detection still routes to escalation. Looser than escalationCues (no separate
// obligation anchor) because awaitHit already established the operator-directed
// obligation context.
var awaitEscalationShape = regexp.MustCompile(`(?i)\b(mint|rotate|provision|seed|generate|approve|merge|sign[- ]?off|transition|grant|widen|deploy|promote)\b[^.?!]{0,40}\b(secret|credential|cred|token|key|password|mr|!?\d{2,}|pr|merge request|[A-Z][A-Z0-9]+-\d+|jira|ticket|iam|rbac|role|policy|permission|access|prod|production|cluster)\b`)

// matchesAny reports whether s matches any of the given patterns.
func matchesAny(s string, pats []*regexp.Regexp) bool {
	return slices.ContainsFunc(pats, func(p *regexp.Regexp) bool {
		return p.MatchString(s)
	})
}

// clauseSplit splits a blob into clauses on sentence punctuation and newlines,
// so the past/permissive status exclusion can be applied locally — a privileged
// verb in a "done" or "you can" clause must not borrow an ask shape from a
// different clause in the same field.
var clauseSplit = regexp.MustCompile(`[.!?\n;]+`)

// splitClauses breaks text into trimmed clauses for per-clause cue matching.
func splitClauses(text string) []string {
	return clauseSplit.Split(text, -1)
}

// gardenerPuntCues match a crew handoff that punts a decision/ask UP to its
// gardener rather than to the operator (audit taxonomy bucket B). The data
// showed gardeners then ALSO sometimes never file — so a punt is recognized but
// the remediation reminds the agent to leave the gardener a VISIBLE handle
// (T7, ka-0000000039). Advisory only; never blocks.
var gardenerPuntCues = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(gardener|first[- ]?mate|co[- ]?gardener|lead agent)\b[^.?!]{0,40}\b(to file|will file|to decide|should decide|to escalate|to pick|handoff|inherit|take over|follow up)\b`),
	regexp.MustCompile(`(?i)\b(punt|defer|hand(ing|ed)? off|leaving|escalat\w+)\b[^.?!]{0,30}\b(to|for)\b[^.?!]{0,15}\b(gardener|first[- ]?mate|co[- ]?gardener)\b`),
}

// wrapupPuntsToGardener reports whether the wrap-up's forward-looking fields
// indicate the ask was punted to a gardener/first-mate rather than the operator.
func wrapupPuntsToGardener(w *WrapUp) bool {
	if w == nil {
		return false
	}
	text := urlPattern.ReplaceAllString(strings.Join([]string{w.TLDR, w.Blockers, w.HandoffNotes}, "\n"), " ")
	return matchesAny(text, gardenerPuntCues)
}

// wrapupReferencesKDHandle reports whether the wrap-up's forward-looking fields
// name at least one gd- bead id. Used by requireOperatorHandle to corroborate a
// "deferring to an already-open decision/escalation" claim against the live
// handle sets (the healthy 47% bucket from the audit).
func wrapupReferencesKDHandle(w *WrapUp) []string {
	if w == nil {
		return nil
	}
	text := strings.Join([]string{w.TLDR, w.Blockers, w.HandoffNotes}, "\n")
	return kdRefPattern.FindAllString(text, -1)
}

// ── rollout mode ────────────────────────────────────────────────────────────

// AskGateMode is the staged-rollout mode for the ask-gate, read from
// GB_STOP_ASK_GATE. Default is "warn" (ship_soft, per decision ka-0000001640):
// detect + nudge + telemetry, never block. Flip to "enforce" once the
// false-positive rate (forced-after-flag) is measured ~0. "off" disables the
// gate entirely (kill switch).
type AskGateMode int

const (
	// AskGateWarn — detect + emit a one-shot nudge + telemetry, but ALWAYS
	// allow the stop. The shipped default (ship_soft).
	AskGateWarn AskGateMode = iota
	// AskGateEnforce — soft one-shot block on first detection, self-release on
	// the second identical stop. Still never restart-loops.
	AskGateEnforce
	// AskGateOff — gate disabled (kill switch).
	AskGateOff
)

func (m AskGateMode) String() string {
	switch m {
	case AskGateEnforce:
		return "enforce"
	case AskGateOff:
		return "off"
	default:
		return "warn"
	}
}

// askGateMode resolves the rollout mode from GB_STOP_ASK_GATE. Unknown/empty
// values default to warn (the safe ship_soft default).
func askGateMode() AskGateMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GB_STOP_ASK_GATE"))) {
	case "enforce":
		return AskGateEnforce
	case "off", "disabled", "none":
		return AskGateOff
	default:
		return AskGateWarn
	}
}
