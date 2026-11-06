package references

import (
	"regexp"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// TypedReference is a bead-id reference together with an optional
// relationship verb extracted from prose. It is the parser-level shape;
// callers (e.g. the write path) translate Verb into a DependencyType
// via CanonicalVerb and decide whether to materialize the edge.
//
// Verb is the canonical, lowercased surface form of the relationship
// (e.g. "parent", "child-of"). It is empty for unknown verbs that
// degrade to a flat mention; in that case RawVerb captures exactly what
// the author wrote so the caller can persist it for later promotion.
type TypedReference struct {
	TargetID string
	Verb     string
	RawVerb  string
	Raw      string
	Position int
}

// canonicalVerbs is the closed-core relationship-verb vocabulary
// recognized by the typed parser. Keys are surface forms authors write
// (after lowercasing); values are the model.DependencyType the verb
// maps to. Inverse forms (child-of, blocked-by, duplicated-by,
// superseded-by) share a DependencyType with their forward form. The
// edge-direction reversal for inverse verbs is a consumer-side concern
// (handled by the write path); the parser only records which surface
// verb was used.
var canonicalVerbs = map[string]model.DependencyType{
	"parent":        model.DepParentChild,
	"child-of":      model.DepParentChild,
	"blocks":        model.DepBlocks,
	"blocked-by":    model.DepBlocks,
	"related":       model.DepRelated,
	"duplicates":    model.DepDuplicates,
	"duplicated-by": model.DepDuplicates,
	"supersedes":    model.DepSupersedes,
	"superseded-by": model.DepSupersedes,
	"triggered-by":  model.DepTriggeredBy,
	"triggers":      model.DepTriggeredBy,
	// Gardener chaining / succession (gardenio gardener-chaining design §3.1).
	// `chains_to` is the forward edge (predecessor → heir); `succeeds` is its
	// inverse (heir → predecessor). `chains-to` is accepted as a hyphen alias
	// of `chains_to` to match the existing hyphenated verb style. All three
	// map to the single DepChainsTo type; direction is recovered from the
	// surface verb (succeeds is inverse) by the write path, exactly like
	// triggers/triggered-by.
	"chains_to": model.DepChainsTo,
	"chains-to": model.DepChainsTo,
	"succeeds":  model.DepChainsTo,
}

// CanonicalVerb returns the DependencyType associated with a recognized
// verb. Lookup is case-insensitive on v. The second return value is
// false when v is not a canonical verb.
func CanonicalVerb(v string) (model.DependencyType, bool) {
	t, ok := canonicalVerbs[strings.ToLower(v)]
	return t, ok
}

// verbCallPattern matches "verb(ka-0000001713)" — the function-call
// form. Verb is 2..31 chars from [A-Za-z0-9_-] starting with a letter.
// The underscore is permitted so snake_case verbs (e.g. `chains_to`) parse;
// the hyphen continues to cover the existing kebab-case verbs. Whitespace
// inside the parens is tolerated.
var verbCallPattern = regexp.MustCompile(
	`\b([A-Za-z][A-Za-z0-9_-]{1,30})\(\s*((?:gd|ka)-[A-Za-z0-9]{10})\s*\)`,
)

// verbColonPattern matches a line of the form
//
//	[bullet] verb: ka-0000001713
//
// — line-anchored, with an optional Markdown bullet ("-" or "*") and
// at least one space after the bullet. The ka-id must immediately
// follow the colon (whitespace allowed).
var verbColonPattern = regexp.MustCompile(
	`(?m)^[ \t]*(?:[-*][ \t]+)?([A-Za-z][A-Za-z0-9_-]{1,30})[ \t]*:[ \t]*((?:gd|ka)-[A-Za-z0-9]{10})\b`,
)

// ExtractTyped returns every typed bead-id reference found in s, in
// document (byte-offset) order. Both the call form ("verb(ka-X)") and
// the colon-line form ("verb: ka-X" on its own line, optionally
// bulleted) are recognized. Bare "ka-X" mentions without a verb are
// not returned by this function — they remain the responsibility of
// the existing untyped Extract.
//
// Recognized canonical verbs populate Verb (lowercased) and leave
// RawVerb empty. Unknown verbs degrade to a flat mention: Verb is "",
// and RawVerb captures the verb exactly as the author wrote it so the
// caller can persist it for later promotion into the closed core.
//
// The parser does not deduplicate — each occurrence is reported with
// its own Position and Raw substring so the caller can attribute
// per-occurrence provenance.
func ExtractTyped(s string) []TypedReference {
	if s == "" {
		return nil
	}
	var out []TypedReference
	for _, m := range verbCallPattern.FindAllStringSubmatchIndex(s, -1) {
		out = append(out, buildTyped(s, m))
	}
	for _, m := range verbColonPattern.FindAllStringSubmatchIndex(s, -1) {
		out = append(out, buildTyped(s, m))
	}
	if len(out) > 1 {
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].Position < out[j].Position
		})
	}
	return out
}

func buildTyped(s string, m []int) TypedReference {
	full := s[m[0]:m[1]]
	rawVerb := s[m[2]:m[3]]
	target := s[m[4]:m[5]]
	lc := strings.ToLower(rawVerb)
	ref := TypedReference{
		TargetID: target,
		Raw:      full,
		Position: m[0],
	}
	if _, ok := canonicalVerbs[lc]; ok {
		ref.Verb = lc
	} else {
		ref.RawVerb = rawVerb
	}
	return ref
}
