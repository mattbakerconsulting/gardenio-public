// Package bridge — per-recipient "vibe mode" resolution.
//
// vibe_mode is a single boolean User preference (gb-ux epic ka-0000005059,
// plan ka-0000004304). Absent or false ⇒ the SIMPLE default surface: Slack
// drops low-signal lifecycle chatter and collapses the rest to terse lines.
// vibe_mode=true ⇒ today's full verbose blocks. We never invert the sense:
// "do nothing" must yield simple, so every resolver here defaults to false on
// any miss (unknown agent, no operated_by, unresolved user bead, unset pref) —
// this is the R5 "logged-out / no-user-bead ⇒ simple" guarantee for Slack.
//
// The preference lives in the existing `preferences` JSON blob on the user
// bead, written either natively by the beads-viewer settings form (`true`) or
// as a string by `gb prefs set <user> vibe_mode true` (`"true"`). To decode
// both without importing cmd/beads-viewer (package main), parseVibeMode mirrors
// that command's flexBool semantics — the same decouple-by-duplication pattern
// BeadEvent/DecisionAnswer already use for daemon event types.
package bridge

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

// parseVibeModeFromPreferences extracts vibe_mode from a user bead's
// `preferences` field. The kbeads HTTP layer re-marshals nested JSON values to
// strings, so `preferences` arrives as a serialized object (same shape the
// beads-viewer parsePreferences consumes). Returns false on empty input, a
// parse failure, a missing key, or an unparseable value — a malformed blob
// must never flip a recipient into vibe mode.
func parseVibeModeFromPreferences(raw string) bool {
	if raw == "" {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return false
	}
	v, ok := obj["vibe_mode"]
	if !ok {
		return false
	}
	return parseFlexBool(v)
}

// parseFlexBool mirrors beads-viewer's flexBool.UnmarshalJSON (settings.go): it
// accepts a native JSON bool (true/false), a quoted boolean string
// ("true"/"false"/"1"/"0"/…), JSON null, or an empty string, and treats
// anything it cannot parse as false. Keeping the two decoders identical means a
// value written by either the viewer checkbox or `gb prefs set` resolves the
// same way here.
func parseFlexBool(data []byte) bool {
	s := strings.TrimSpace(string(data))
	switch s {
	case "true":
		return true
	case "false", "null", "":
		return false
	}
	// Quoted string form, e.g. "true" written by `gb prefs set`.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return false
		}
		parsed, err := strconv.ParseBool(inner)
		if err != nil {
			// Unrecognized string (e.g. "yes") → false, don't flip to vibe.
			return false
		}
		return parsed
	}
	return false
}

// oidcEmailFromOperator extracts the email from an "oidc:<email>" operated_by
// identity, returning ("", false) for any other shape. Mirror of
// slackUserIDFromOperator (comment_forward.go) for the oidc: prefix.
func oidcEmailFromOperator(operatedBy string) (string, bool) {
	const prefix = "oidc:"
	if !strings.HasPrefix(operatedBy, prefix) {
		return "", false
	}
	email := operatedBy[len(prefix):]
	if email == "" {
		return "", false
	}
	return email, true
}

// vibeModeForSlackUser reports whether the given Slack user has opted into vibe
// (full-verbosity) mode. Used on the DM path where the recipient is a single
// Slack id. Returns false (simple) when the directory is unavailable, the id is
// empty, no user bead resolves, or the pref is unset.
func (b *Bot) vibeModeForSlackUser(ctx context.Context, slackUserID string) bool {
	if slackUserID == "" {
		return false
	}
	// UserDirectory() lazily creates + primes the directory; do not guard on the
	// unexported field being nil, or the very first notification before any
	// other directory access would spuriously resolve simple for a vibe user.
	u := b.UserDirectory().LookupBySlackID(ctx, slackUserID)
	if u == nil {
		return false
	}
	return u.VibeMode
}

// vibeModeForOperator resolves vibe_mode from an operated_by identity string
// ("oidc:<email>" or "slack:U…") via the user directory. Returns false on any
// miss. This is the shared tail of vibeModeForAgent and the recipient-resolution
// path for channel broadcasts keyed to an owning operator.
func (b *Bot) vibeModeForOperator(ctx context.Context, operatedBy string) bool {
	if operatedBy == "" {
		return false
	}
	dir := b.UserDirectory()
	if email, ok := oidcEmailFromOperator(operatedBy); ok {
		if u := dir.LookupByEmail(ctx, email); u != nil {
			return u.VibeMode
		}
		return false
	}
	if uid, ok := slackUserIDFromOperator(operatedBy); ok {
		if u := dir.LookupBySlackID(ctx, uid); u != nil {
			return u.VibeMode
		}
	}
	return false
}

// vibeModeForAgent reports whether the operator who owns the given agent has
// opted into vibe mode. Lifecycle notifications about a specific agent (crash,
// jacks, per-bead activity, forwarded comments) key their verbosity off the
// owning operator's preference: the person who cares about an agent's chatter is
// the human who spawned it. Returns false (simple) on any miss — unknown agent,
// no operated_by stamp, or an unresolved/pref-less operator bead.
//
// Cost (R4): this resolves the agent bead (one daemon call) to read operated_by.
// On the suppress-by-default path the caller short-circuits the Slack send after
// this single lookup; the full-verbosity (vibe) path is the rare case.
func (b *Bot) vibeModeForAgent(ctx context.Context, agent string) bool {
	agent = extractAgentName(agent)
	if agent == "" {
		return false
	}
	detail, err := b.daemon.FindAgentBead(ctx, agent)
	if err != nil || detail == nil {
		return false
	}
	return b.vibeModeForOperator(ctx, detail.Fields["operated_by"])
}

// vibeModeForAccountEvent resolves vibe_mode for an account rotation/lifecycle
// event. These post to a project (or default) channel — a broadcast with no
// single recipient — so the best per-recipient key available is the owning
// agent referenced by evt.AgentBead (a bead id). When the event carries no
// agent link (the common case for project/account-scoped cred events), this
// returns false: the routine-lifecycle bucket stays quiet in the simple
// default, matching the mission. The urgent bucket (no-fallback / probe-failed
// / throttled) always posts regardless of this result — it only decides
// full-blocks vs a terse line there.
func (b *Bot) vibeModeForAccountEvent(ctx context.Context, evt RotationEvent) bool {
	if evt.AgentBead == "" {
		return false
	}
	detail, err := b.daemon.GetBead(ctx, evt.AgentBead)
	if err != nil || detail == nil {
		return false
	}
	return b.vibeModeForOperator(ctx, detail.Fields["operated_by"])
}

// anyVibeAmongJacks reports whether any jack in the batch is owned by a
// vibe-mode operator. Used to decide whether the low-signal batch summary
// (posted to the default channel with no single recipient) surfaces at all.
func (b *Bot) anyVibeAmongJacks(ctx context.Context, beads []BeadEvent) bool {
	for _, bead := range beads {
		if b.vibeModeForAgent(ctx, bead.Assignee) {
			return true
		}
	}
	return false
}
