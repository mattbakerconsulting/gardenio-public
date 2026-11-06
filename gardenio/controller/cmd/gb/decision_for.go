package main

import (
	"context"
	"fmt"
	"strings"
)

// decision_for.go implements `gb decision create --for <human>` — addressing a
// decision to a SPECIFIC human rather than to whoever happens to operate the
// filing agent.
//
// Operator ruling (Matt Baker, 2026-08-27, thread ka-0000006045):
//
//	"design and build the 'operated_by' logic for decisions since we need
//	 that; and we should also create an exemption so that a user can author a
//	 decision that is operated_by someone else; essentially giving it to them."
//
// THE EXEMPTION IS THE POINT. `--for` is explicitly allowed to name someone
// other than the author — an agent that fixed a broken pipeline must be able to
// ask the MR's AUTHOR whether to push the fix, and that human is somebody
// else's operator or nobody's. So there is no "may I address this to them?"
// check here. What there IS: when the decision is addressed to a human other
// than its author, `gifted_by` records who handed it over, so a decision that
// shows up in your DMs always says who put it there.

// normalizeOperatedBy converts a human-supplied `--for` value into the
// operated_by vocabulary the rest of the fleet already speaks (the same shapes
// escalations and agent beads carry, parsed by emailFromOperator /
// extractSlackUserIDOperator in the bridge):
//
//	oidc:<email>   an email, however it was typed
//	slack:U…       a raw Slack user id, an <@U…> mention, or @U…
//
// An already-prefixed value (oidc:, slack:, fleet:, system:, jira:, gitlab:,
// agent:) passes through untouched, so a caller who knows the exact identity is
// never second-guessed.
func normalizeOperatedBy(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("--for: empty value")
	}

	// Already in operated_by vocabulary → pass through.
	if kind, rest, ok := strings.Cut(v, ":"); ok {
		switch kind {
		case "oidc", "slack", "fleet", "system", "jira", "gitlab", "agent":
			if strings.TrimSpace(rest) == "" {
				return "", fmt.Errorf("--for: %q has an empty identity after %q:", v, kind)
			}
			return v, nil
		}
	}

	// A Slack mention or raw Slack id: <@U123ABC>, @U123ABC, U123ABC.
	id := strings.TrimSuffix(strings.TrimPrefix(v, "<@"), ">")
	id = strings.TrimPrefix(id, "@")
	if looksLikeSlackUserID(id) {
		return "slack:" + id, nil
	}

	if strings.Contains(v, "@") && !strings.HasPrefix(v, "@") {
		return "oidc:" + strings.ToLower(v), nil
	}

	return "", fmt.Errorf("--for: cannot interpret %q as a human identity\n\nAccepted: an email (matt@example.com), a Slack id or mention (U0AB1CD2EF, <@U0AB1CD2EF>), or an explicit identity (oidc:matt@example.com, slack:U0AB1CD2EF)", raw)
}

// looksLikeSlackUserID matches Slack's user-id shape (U or W, then uppercase
// alphanumerics). Deliberately shape-only: the bridge resolves and validates the
// id against the operator alias feed at delivery time, and refusing a valid id
// here would be worse than passing an unknown one through.
func looksLikeSlackUserID(s string) bool {
	if len(s) < 6 || len(s) > 32 {
		return false
	}
	if s[0] != 'U' && s[0] != 'W' {
		return false
	}
	for _, r := range s[1:] {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return true
}

// resolveAuthorOperator returns the operated_by of the agent filing this
// decision, so a decision addressed to someone ELSE can be recognised as a
// handover (and so a decision with no --for still carries a durable addressee
// rather than depending on a runtime walk of the agent bead).
//
// Best-effort by design: an unresolvable author must never fail a create.
func resolveAuthorOperator(ctx context.Context, agentBeadID string) string {
	if agentBeadID == "" || daemon == nil {
		return ""
	}
	bead, err := daemon.GetBead(ctx, agentBeadID)
	if err != nil || bead == nil {
		return ""
	}
	return strings.TrimSpace(bead.Fields["operated_by"])
}

// decisionAddresseeFields computes the operated_by / gifted_by stamps for a new
// decision.
//
//   - No --for: the decision inherits the author's own operator. This changes
//     nothing about who is asked; it just STATES it durably instead of leaving
//     it to be re-derived from the agent bead at delivery time.
//   - --for naming the author's own operator: same as above, no gift.
//   - --for naming anyone else: the exemption — the decision is given to them,
//     and gifted_by records the author.
func decisionAddresseeFields(ctx context.Context, forValue, agentBeadID string) (map[string]string, error) {
	author := resolveAuthorOperator(ctx, agentBeadID)
	out := map[string]string{}

	if strings.TrimSpace(forValue) == "" {
		if author != "" {
			out["operated_by"] = author
		}
		return out, nil
	}

	target, err := normalizeOperatedBy(forValue)
	if err != nil {
		return nil, err
	}
	out["operated_by"] = target
	if author != "" && !strings.EqualFold(author, target) {
		out["gifted_by"] = author
	}
	return out, nil
}
