package landmap

import (
	"regexp"
	"strings"
)

// The prose scrub (contract design gd-EWSKPH7pa5 §2 "Id model" and §6,
// D.8 gd-enMSApFne1). Free text a tenant is shown (titles, bodies, a
// decision's question, context and option labels, an escalation's reason,
// comments) is written by agents and operators as well as by the tenant, so it
// can name things outside the land. The scrub replaces what it recognises:
//
//   - another land's bead ids, which read [ref]. The land's own ids are kept:
//     an id is foreign when its prefix is not the prefix of the record being
//     shown, the land-aware rule the design asks for (references.Pattern is
//     fixed to gd|ka and cannot tell). Until each land is minted with its own
//     prefix (owner question 5) every land shares gd-, so this keeps them all.
//   - the daemon's route paths, which read [path].
//   - in-cluster service hosts, which read [host].
//
// It is advisory, a tripwire like the denylist: it catches literals, not
// meaning, and its list will be tuned for as long as agents write prose. The
// sound walls are the land scope at the daemon and the map's closed tables.

var (
	// proseID is a bead id reference: a lower-case prefix, a dash and ten
	// base62 characters.
	proseID = regexp.MustCompile(`\b[a-z]{2,8}-[A-Za-z0-9]{10}\b`)
	// prosePath is a daemon route: /v1/ and one of its top-level segments.
	// The contract's own paths begin /lands/v1/, which this never matches.
	// A trailing full stop, comma or colon is the sentence's, not the path's.
	prosePath = regexp.MustCompile(`/v1/(?:beads|decisions|escalations|events|bus|search|accounts|plans|threads|me|admin|projects|agents|auth|embeddings|health)\b(?:[^\s"'<>()\[\],;]*[^\s"'<>()\[\],;.:!?])?`)
	// proseHost is a Kubernetes service host.
	proseHost = regexp.MustCompile(`(?i)\b[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*\.svc(?:\.cluster\.local)?\b`)
)

// scrubProse returns s with foreign ids, daemon paths and service hosts
// replaced. own is the id of the record s belongs to.
func scrubProse(own, s string) string {
	if s == "" {
		return s
	}
	// Paths and hosts first: an id inside a path goes with the path.
	s = prosePath.ReplaceAllString(s, "[path]")
	s = proseHost.ReplaceAllString(s, "[host]")
	prefix, _, _ := strings.Cut(own, "-")
	return proseID.ReplaceAllStringFunc(s, func(id string) string {
		p, rest, _ := strings.Cut(id, "-")
		// A generated id is mixed-case base62; ten lower-case letters after a
		// dash is a hyphenated word ("re-initialize").
		if p == prefix || !strings.ContainsAny(rest, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			return id
		}
		return "[ref]"
	})
}

// scrubOpt scrubs an optional field in place.
func scrubOpt(own string, s *string) *string {
	if s == nil {
		return nil
	}
	v := scrubProse(own, *s)
	return &v
}
