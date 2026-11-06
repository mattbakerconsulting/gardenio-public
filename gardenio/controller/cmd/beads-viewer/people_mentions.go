package main

import (
	"context"
	"regexp"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// slackMentionRe matches a raw Slack user-mention token as it appears in a
// message body: `<@U…>` where the id is Slack's uppercase-alphanumeric user id
// (a leading `U` — occasionally `W` for enterprise-grid accounts — followed by
// [A-Z0-9]). Slack also emits a labeled form `<@U123|display>` when it already
// knows the name; we capture the id in group 1 and ignore any `|label` so both
// forms resolve identically.
//
// The character class is UPPERCASE-only on purpose: it never matches the
// placeholder junk `<@Uxxx>` / `<@Uxxxx>` that squawks and templated copy use
// as a stand-in for "some user" (lowercase x), so those literal placeholders
// are left exactly as written rather than mangled into a bogus lookup.
var slackMentionRe = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)

// externalMentionPlaceholder is what an otherwise-valid `<@U…>` token renders as
// when the (healthy) resolver has no entry for the id — i.e. a genuinely external
// user, present in neither the operator roster nor the slack-alias feed. Emitting
// a neutral placeholder rather than the raw token guarantees no card ever shows a
// bracketed Slack id to a human (ka-0000005382), while still never inventing a
// specific name for someone we can't identify.
const externalMentionPlaceholder = "@external-user"

// slackMentionResolver maps a Slack user id (e.g. "U0OPERATOR1") to that
// person's human display name (e.g. "gardenio" / "Austin Tobitt"). It backs the
// /people "recent requests" snippet renderer so a raw `<@U…>` token in a
// forwarded Slack message resolves to `@Name` instead of leaking the opaque id
// (ka-0000003119). An empty/nil resolver is a no-op — resolveSlackMentions then
// returns its input unchanged, so the strip degrades to the raw token rather
// than failing.
type slackMentionResolver map[string]string

// buildSlackMentionResolver projects the kind=slack operator-alias feed into an
// id→display map. Only aliases with a genuine human display name are indexed:
// the resync sentinel rows carry an empty Display and a synthetic
// `slack-U…@noreply.kbeads.local` Email (deriveOperatorEmail), and surfacing
// either as a mention would just swap one opaque token for another — so those
// rows are skipped and their tokens render raw.
//
// The aliases feed (not the slug'd /people directory) is the source on purpose:
// a human who has never logged into the viewer via OIDC has no slug'd operators
// row, so ListOperators misses them, but the Slack bridge still recorded their
// alias — and it's precisely those un-slugged operators (Hong Jamie Cole,
// Perry Tate, jean-marc) whose tokens the audit saw leak. The feed resolves
// all of them from one bounded round-trip.
func buildSlackMentionResolver(aliases []beadsapi.SlackOperatorAlias) slackMentionResolver {
	if len(aliases) == 0 {
		return nil
	}
	out := make(slackMentionResolver, len(aliases))
	for _, a := range aliases {
		subject := strings.TrimSpace(a.Subject)
		if subject == "" {
			continue
		}
		name := strings.TrimSpace(a.Display)
		if name == "" {
			// No real name — skip rather than emit a synthetic
			// slack-U…@noreply handle that reads no better than the raw id.
			continue
		}
		out[subject] = name
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// lookupSlackMentionResolver loads the kind=slack alias feed and projects it to
// an id→display resolver for the /people recent-requests strip. One bounded
// round-trip, matching the page's other lookups; returns nil (a benign no-op
// resolver) on any error so the directory degrades to raw tokens rather than
// failing the page.
func (s *Server) lookupSlackMentionResolver(ctx context.Context) slackMentionResolver {
	resp, err := s.daemon.ListSlackOperatorAliases(ctx)
	if err != nil || resp == nil {
		if err != nil {
			s.logger.Debug("people: slack alias resolver load failed", "error", err)
		}
		return nil
	}
	return buildSlackMentionResolver(resp.Aliases)
}

// resolveSlackMentions rewrites every `<@U…>` token in text to `@Name` using the
// resolver. A token whose id IS in the resolver renders as `@Name`. A valid
// `<@U…>` token whose id is NOT in the (healthy, non-empty) resolver — a
// genuinely external user, absent from both the operator roster and the alias
// feed — renders as the neutral externalMentionPlaceholder rather than leaking
// the raw bracketed id to a human (ka-0000005382); we still never invent a
// specific name. A nil/empty resolver (the load-error degrade path) returns text
// unchanged: we can't tell an external id from a real operator whose feed failed
// to load, so we keep the raw token rather than mislabel someone as external.
// This runs on the RAW body BEFORE truncation so the visible preview clips a
// resolved `@Name`, never a half-eaten `<@U0BFF…` id.
func resolveSlackMentions(text string, r slackMentionResolver) string {
	if text == "" || len(r) == 0 || !strings.Contains(text, "<@") {
		return text
	}
	return slackMentionRe.ReplaceAllStringFunc(text, func(match string) string {
		m := slackMentionRe.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		if name, ok := r[m[1]]; ok {
			return "@" + name
		}
		// Valid id, healthy resolver, no match → genuinely external user.
		// Emit the neutral placeholder so no bracketed Slack id ever reaches
		// a human-facing snippet.
		return externalMentionPlaceholder
	})
}
