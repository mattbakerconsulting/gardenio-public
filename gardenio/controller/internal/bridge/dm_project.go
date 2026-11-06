// Package bridge — per-user DM project resolution.
//
// dm_project is a single string User preference: the project a user's DMs
// with the bot spawn into when no explicit override (`project:<name>` /
// `--project`) is given. It outranks the fleet-wide DEFAULT_DM_PROJECT env
// default and the project bead's dm_default flag, because those are one
// setting for every user while this one is the user's own choice. Operator
// ask (Matt Baker, Slack thread 1792455150.151339): "make sure <user> has his
// DMs with gardenio go to the claimlayer project".
//
// The preference lives in the existing `preferences` JSON blob on the user
// bead and is written by `gb prefs set --user <slack-id> dm_project <name>`.
package bridge

import (
	"context"
	"encoding/json"
	"strings"
)

// parseDMProjectFromPreferences extracts dm_project from a user bead's
// `preferences` field. Returns "" on empty input, a parse failure, a missing
// key, or a non-string value — a malformed blob must never route a DM.
func parseDMProjectFromPreferences(raw string) string {
	if raw == "" {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return ""
	}
	v, ok := obj["dm_project"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// dmProjectForSlackUser returns the Slack user's dm_project preference, or ""
// when the id is empty, no user bead resolves, or the pref is unset.
func (b *Bot) dmProjectForSlackUser(ctx context.Context, slackUserID string) string {
	if slackUserID == "" {
		return ""
	}
	u := b.UserDirectory().LookupBySlackID(ctx, slackUserID)
	if u == nil {
		return ""
	}
	return u.DMProject
}
