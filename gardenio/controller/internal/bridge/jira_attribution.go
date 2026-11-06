package bridge

import "strings"

// jira_attribution.go stamps "Acted by <operator>" onto every outbound Jira
// comment the bridge posts on behalf of an agent, so the Jira issue history
// stays honest about WHICH human triggered the work despite the bridge using a
// single shared service account (epic ka-0000004904 decision #4 / E2E Check 4,
// task ka-0000006003).
//
// The acting human rides on the bead's operated_by field (set by the spawning
// Slack/GitLab/Jira path — see human_operator.go) and is mirrored into
// BeadDetail.OperatedBy / .OperatedByDisplay by the daemon. Each outbound write
// path resolves the operated_by it has in hand and appends the attribution line
// to the comment body it was already going to post. Pure additive payload
// formatting — no new gate, account, or operator-gated logic (the attribution
// is built only AFTER the existing project-gate / allowlist / scope / throttle
// / dedup guards have passed, inside the block that already posts the comment).
//
// Attribution is verified by grepping bot-authored comment bodies for the
// verbatim operated_by id (e.g. "slack:U0OPERATOR1"). So the RAW operated_by
// string MUST appear unmodified in the emitted body; a resolved display name is
// added for human legibility but never replaces the raw id.

// operatorAttributionLine builds the "Acted by <operator>" line stamped onto an
// outbound Jira comment, or "" when no honest attribution is available.
//
//   - operatedBy is the bead's operated_by id ("slack:U…" / "oidc:<email>").
//   - display is the daemon-resolved human display name when known, else "".
//
// Rules (task ka-0000006003 SCOPE §1):
//   - empty operatedBy            -> "" (omit; never fabricate an operator)
//   - non-human sentinel          -> "" (fleet:slack-bridge, system:*, bare
//     agent names, jira:/gitlab: provider ids — these are not a real person, so
//     attributing work to them would be noise, not honesty)
//   - human + resolved display    -> "Acted by <display> (<operatedBy>)"
//   - human, no display mapping   -> "Acted by <operatedBy>" (raw fallback)
//
// The raw operatedBy is ALWAYS present in a non-empty result: it is both the
// audit-faithful identity and the literal token the E2E harness greps for.
func operatorAttributionLine(operatedBy, display string) string {
	operatedBy = strings.TrimSpace(operatedBy)
	// Only a well-formed HUMAN operator (the two shapes the kbeads daemon
	// accepts, IsHumanOperator) earns an attribution line. Sentinels and
	// provider ids fall through to "" so we never stamp a fake actor.
	if !IsHumanOperator(operatedBy) {
		return ""
	}
	display = strings.TrimSpace(display)
	if display != "" && display != operatedBy {
		return "Acted by " + display + " (" + operatedBy + ")"
	}
	return "Acted by " + operatedBy
}

// withAttribution appends the attribution line to a comment body as its own
// line, or returns body unchanged when line is "". The double newline renders
// as a fresh paragraph once adfParagraph promotes each newline-delimited run to
// its own ADF text node + hardBreak, keeping the attribution visually distinct
// in Jira history.
func withAttribution(body, line string) string {
	if line == "" {
		return body
	}
	return body + "\n\n" + line
}
