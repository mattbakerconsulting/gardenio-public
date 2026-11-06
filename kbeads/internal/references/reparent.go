package references

import (
	"regexp"
	"strings"
)

// parentEdgeVerbs are the typed-mention verbs that, appearing in a bead's OWN
// text, materialize a parent-child dep row with THAT bead as the child and the
// mentioned bead as the parent — i.e. "the mentioned bead is my parent".
//
// depRowForTypedVerb (internal/store/postgres/queries_references.go) maps BOTH
// `parent` and its inverse `child-of` to (source=child, target=parent,
// parent-child): `parent(ka-X)` and `child-of(ka-X)` in a bead's description
// produce the identical dep row. Re-parenting must therefore repoint BOTH
// surface forms, or a `child-of: ka-OLD` left behind resurrects the old edge on
// the next re-extract (the resurrection footgun this command exists to fix).
var parentEdgeVerbs = map[string]struct{}{
	"parent":   {},
	"child-of": {},
}

// IsParentEdgeVerb reports whether verb (canonical, lowercased — as emitted by
// ExtractTyped) materializes a parent-child edge FROM the source bead TO the
// mentioned bead, i.e. the mentioned bead becomes the source's parent. Used by
// `kd reparent` to find every mention that must move when a bead changes
// parent, and to assert none survive the rewrite.
func IsParentEdgeVerb(verb string) bool {
	_, ok := parentEdgeVerbs[verb]
	return ok
}

// RewriteParentResult is the outcome of RewriteParentMention: the new text
// plus a record of what actually changed so callers can report accurately
// and decide whether a write is even needed.
type RewriteParentResult struct {
	// Text is the rewritten source. When no parent-edge mention of the old
	// parent was present it equals the input verbatim.
	Text string
	// Rewrote is the number of parent-edge typed mentions of oldParent that
	// were repointed at newParent (across the call form `parent(ka-X)` /
	// `child-of(ka-X)` and the colon form `parent: ka-X` / `child-of: ka-X`).
	Rewrote int
}

// rewriteParentCallPattern and rewriteParentColonPattern narrow the generic
// typed-mention regexps (see typed.go) to the parent-edge verbs specifically
// (`parent` and its inverse `child-of`). We deliberately re-derive them here
// rather than reuse verbCallPattern / verbColonPattern so the substitution can
// key off the exact byte spans of the verb+target.
//
// The ka-id and surrounding syntax must match what ExtractTyped recognizes so
// that a rewrite provably changes what the next re-extract materializes — the
// two are exercised together in reparent_test.go against ExtractTyped.
var (
	// parent(ka-X) / child-of(ka-X) — case-insensitive verb, whitespace-tolerant parens.
	rewriteParentCallPattern = regexp.MustCompile(
		`(?i)\b(parent|child-of)\(\s*((?:gd|ka)-[A-Za-z0-9]{10})\s*\)`,
	)
	// [bullet] parent: ka-X / child-of: ka-X — line-anchored colon form.
	rewriteParentColonPattern = regexp.MustCompile(
		`(?im)^([ \t]*(?:[-*][ \t]+)?)(parent|child-of)([ \t]*:[ \t]*)((?:gd|ka)-[A-Za-z0-9]{10})\b`,
	)
)

// RewriteParentMention repoints every parent-edge typed mention of oldParent in
// s to newParent, in both the call form (`parent(ka-X)` / `child-of(ka-X)`) and
// the colon-line form (`parent: ka-X` / `child-of: ka-X`). The matched surface
// verb and its casing are preserved — only the target id is changed. Bare
// mentions, `related:`/`blocks:`/other verbs, and parent-edge mentions of any
// OTHER bead are left untouched.
//
// This is the surgical half of `kd reparent`: removing the old parent-child
// dep row is not enough, because replaceBeadReferences re-runs the typed
// parser on every subsequent write and re-materializes any parent-edge mention
// of the old parent it still finds (the "resurrection footgun",
// ka-0000007756). Rewriting the text so the parser no longer sees the old
// parent is what makes the edge removal durable.
//
// The function is pure and deterministic: same input always yields the same
// output, and running it twice is a no-op on the second pass (the first pass
// leaves no parent-edge mention of oldParent for the second to find). The
// `related:` back-reference for --keep-related is a caller concern (see
// HasRelatedMention / AppendRelatedMention) because it targets one field
// while the rewrite spans all of a bead's text fields.
func RewriteParentMention(s, oldParent, newParent string) RewriteParentResult {
	res := RewriteParentResult{Text: s}
	if s == "" || oldParent == "" || newParent == "" || oldParent == newParent {
		return res
	}

	rewrote := 0

	out := rewriteParentCallPattern.ReplaceAllStringFunc(s, func(match string) string {
		sub := rewriteParentCallPattern.FindStringSubmatch(match)
		// sub[1]=verb, sub[2]=target.
		if len(sub) < 3 || sub[2] != oldParent {
			return match // different target — leave verbatim.
		}
		rewrote++
		return strings.Replace(match, oldParent, newParent, 1)
	})

	out = rewriteParentColonPattern.ReplaceAllStringFunc(out, func(match string) string {
		sub := rewriteParentColonPattern.FindStringSubmatch(match)
		// sub[1]=bullet+ws prefix, sub[2]=verb, sub[3]=colon+ws, sub[4]=target.
		if len(sub) < 5 || sub[4] != oldParent {
			return match
		}
		rewrote++
		return sub[1] + sub[2] + sub[3] + newParent
	})

	res.Text = out
	res.Rewrote = rewrote
	return res
}

// ParentEdgeTargetsExcept returns the distinct set of bead ids that appear as a
// parent-edge typed mention (parent or child-of, call or colon form) in text
// and are NOT except. This is the authoritative source of "which parents does
// this text still name": it drives both the rewrite target list and the
// post-rewrite residual check that guards against an unhandled surface form
// silently resurrecting an edge.
func ParentEdgeTargetsExcept(text, except string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, ref := range ExtractTyped(text) {
		if !IsParentEdgeVerb(ref.Verb) || ref.TargetID == except {
			continue
		}
		if _, dup := seen[ref.TargetID]; dup {
			continue
		}
		seen[ref.TargetID] = struct{}{}
		out = append(out, ref.TargetID)
	}
	return out
}

// HasRelatedMention reports whether text already carries a `related` typed
// mention (call or colon form) of target. `kd reparent --keep-related` uses it
// to avoid appending a duplicate related back-reference when the bead already
// relates to the old parent in some field.
func HasRelatedMention(text, target string) bool {
	for _, ref := range ExtractTyped(text) {
		if ref.Verb == "related" && ref.TargetID == target {
			return true
		}
	}
	return false
}

// AppendRelatedMention appends a `related: <target>` colon-form line to text,
// normalizing trailing whitespace so the new line is cleanly separated. The
// appended line round-trips through ExtractTyped as a `related` mention.
func AppendRelatedMention(text, target string) string {
	trimmed := strings.TrimRight(text, "\n")
	if trimmed == "" {
		return "related: " + target
	}
	return trimmed + "\n\nrelated: " + target
}
