package main

import (
	"fmt"
	"strings"
)

// validateSlackUserID enforces that a value passed as a Slack *user* ID has the
// expected `U` (workspace user) or `W` (Enterprise Grid workspace user)
// prefix.
//
// Slack's "Copy member ID" UI sits next to several other "Copy ID"
// affordances — DM channel (`D`), public channel (`C`), private group (`G`),
// bot/app (`B`). Storing a `D...` id in a `slack_id` field silently breaks
// downstream identity resolution (DM routing, `users.info`, --operated-by
// lookups). Rejecting wrong-prefix ids at the boundary keeps the field's
// type invariant intact.
//
// An empty value is treated as "not provided" and returns nil — callers that
// require the field must check for empty themselves.
//
// flagDesc is a short human label for the offending source (e.g. "--slack-id"
// or "--operated-by slack:") used to anchor the error message.
func validateSlackUserID(flagDesc, value string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	if len(v) < 2 {
		return fmt.Errorf("%s %q is not a valid Slack user ID (expected U… or W… prefix)", flagDesc, value)
	}
	switch v[0] {
	case 'U', 'W':
		return nil
	case 'D':
		return fmt.Errorf(`%s %q looks like a Slack DM channel ID, not a user ID.

Did you mean:
  --slack-channel-id %s   (store as DM channel)
  --slack-id U...                  (the U-prefix user ID; get it via Slack profile → ⋮ → Copy member ID)`, flagDesc, value, v)
	case 'C':
		return fmt.Errorf("%s %q looks like a Slack public-channel ID, not a user ID (expected U… or W… prefix)", flagDesc, value)
	case 'G':
		return fmt.Errorf("%s %q looks like a Slack private-group ID, not a user ID (expected U… or W… prefix)", flagDesc, value)
	case 'B':
		return fmt.Errorf("%s %q looks like a Slack bot/app ID, not a user ID (expected U… or W… prefix)", flagDesc, value)
	default:
		return fmt.Errorf("%s %q is not a valid Slack user ID (expected U… or W… prefix)", flagDesc, value)
	}
}

// validateOperatedBy enforces the prefix rule on the `slack:` form of the
// `--operated-by` flag. Other providers (`oidc:`, `system:`, `fleet:`) pass
// through unchanged.
func validateOperatedBy(value string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	const slackPrefix = "slack:"
	if !strings.HasPrefix(v, slackPrefix) {
		return nil
	}
	subject := v[len(slackPrefix):]
	return validateSlackUserID("--operated-by slack:", subject)
}
