package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/server"
)

// validateTypeFilter normalizes and validates the values supplied to `--type`
// on `kd list` / `kd search`. It returns the TRIMMED values (which the caller
// must send downstream) and a non-nil error naming the first unknown value.
//
// Why this exists: the CREATE path already rejects an unknown type
// (`kd create --type=zzz` → HTTP 400 "unknown bead type zzz"), but the
// LIST/filter path historically passed the raw value straight into the query,
// so `--type=gardener` or `--type=zzznotatype` matched nothing and returned
// exit 0. An invalid filter value was indistinguishable from a genuinely empty
// result — a first-mate gardener read an empty `--type=gardener` listing as "no
// gardeners exist" and burned five query shapes before `--type=trigger` worked
// (ka-0000006244). This is the third member of a "silent empty on invalid
// filter input" family (ka-0000000987 unknown flag, ka-0000003307 comma-AND
// labels); validating here makes `--type` fail loud, matching create-path
// semantics.
//
// Normalization is load-bearing, not cosmetic. pflag splits `--type=task, bug`
// into `["task", " bug"]` (note the leading space) and preserves a trailing
// space on `--type=task `. The server matches the `type` column verbatim with
// NO trimming, so an untrimmed value silently matches nothing. We therefore
// trim each value AND return the trimmed slice so the caller queries with the
// same value we validated — otherwise validation would pass a value the query
// can never match, re-opening the very footgun this fix closes.
//
// The known-type set is the UNION of three sources so a legitimate value is
// never rejected:
//   - the compiled-in builtin floor (server.BuiltinBeadTypeNames) — the same
//     map the server's create path validates against;
//   - deprecated type aliases (model.TypeAliases, e.g. `template`→`formula`)
//     that the create path also accepts; and
//   - the live `type:*` config registry (`ListConfigs(ctx, "type")`), which
//     adds any custom types registered on this daemon.
//
// The live-registry round-trip is made ONLY when a value isn't already covered
// by the in-memory builtin floor — so the overwhelmingly common
// `--type=task`/`--type=bug` case costs no network call. The ListConfigs call
// is best-effort: a transport error degrades to the builtin floor rather than
// failing the command, so a daemon hiccup can never turn a valid `--type=task`
// into a hard error.
func validateTypeFilter(ctx context.Context, values []string) ([]string, error) {
	if len(values) == 0 {
		return values, nil
	}

	normalized := make([]string, len(values))
	for i, v := range values {
		normalized[i] = strings.TrimSpace(v)
	}

	// Fast path: the builtin floor + aliases need no network. Only consult the
	// live registry when a value isn't covered locally (a custom type).
	known := localKnownTypes()
	if !allKnown(normalized, known) {
		mergeRegistryTypes(ctx, known)
	}

	for _, name := range normalized {
		if name == "" {
			// An empty `--type=` value never matches anything and is almost
			// certainly a mistake, but it is not an "unknown type" — leave the
			// pre-existing (no-op) behaviour rather than invent a new error.
			continue
		}
		if _, ok := known[name]; !ok {
			return nil, fmt.Errorf("%s", unknownTypeMessage(name, known))
		}
	}
	return normalized, nil
}

// localKnownTypes returns the in-memory known-type set: the builtin floor that
// the create path validates against, plus deprecated aliases it accepts. No
// network.
func localKnownTypes() map[string]struct{} {
	known := make(map[string]struct{})
	for _, name := range server.BuiltinBeadTypeNames() {
		known[name] = struct{}{}
	}
	for alias := range model.TypeAliases {
		known[string(alias)] = struct{}{}
	}
	return known
}

// allKnown reports whether every non-empty value is already in known.
func allKnown(values []string, known map[string]struct{}) bool {
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := known[v]; !ok {
			return false
		}
	}
	return true
}

// mergeRegistryTypes adds custom types from the live `type:*` config registry
// into known. Best-effort: a transport error leaves known untouched (the
// builtin floor still stands), so a daemon hiccup never hard-fails a builtin.
func mergeRegistryTypes(ctx context.Context, known map[string]struct{}) {
	if beadsClient == nil {
		return
	}
	configs, err := beadsClient.ListConfigs(ctx, "type")
	if err != nil {
		return
	}
	for _, c := range configs {
		if c == nil {
			continue
		}
		if name, ok := strings.CutPrefix(c.Key, "type:"); ok && name != "" {
			known[name] = struct{}{}
		}
	}
}

// sortedKnownTypeNames returns the known type names as a sorted slice. Shared
// by unknownTypeMessage (the --type filter) and unknownFieldsTypeError
// (`kd fields <bogus>`) so both render the valid-type list the same way.
func sortedKnownTypeNames(known map[string]struct{}) []string {
	valid := make([]string, 0, len(known))
	for name := range known {
		valid = append(valid, name)
	}
	sort.Strings(valid)
	return valid
}

// unknownTypeMessage renders the stderr error body for an unknown `--type`
// value: the offending value, a sorted list of valid types, and exactly one
// hint — a `-l role:<name>` pointer when the value names a role namespace
// (e.g. `gardener`), otherwise a Levenshtein did-you-mean for a near-miss.
// The two hints are mutually exclusive so a role value like `triage` never
// emits a contradictory "did you mean trigger?" alongside the role hint.
func unknownTypeMessage(value string, known map[string]struct{}) string {
	valid := sortedKnownTypeNames(known)

	var b strings.Builder
	fmt.Fprintf(&b, "unknown bead type %q; valid types: %s", value, strings.Join(valid, ", "))

	if isLikelyRoleNamespace(value) {
		// A role-namespace value is a known label confusion, not a type typo —
		// the create path has no such type, and the user almost certainly wants
		// `-l role:<name>`. Give that hint and skip the fuzzy type suggestion.
		fmt.Fprintf(&b, " (if you meant the agent role, filter by label: -l role:%s)", value)
	} else if suggestion := closestType(value, valid); suggestion != "" {
		fmt.Fprintf(&b, "; did you mean %q?", suggestion)
	}
	return b.String()
}

// closestType returns the known type nearest to value by edit distance, or ""
// when nothing is close enough to suggest confidently. The accepted distance is
// min(2, len(value)-1): an absolute cap of 2 edits, but never more than the
// suggestion can change while still preserving the MAJORITY of the input. That
// second clause is what suppresses nonsense suggestions for very short inputs —
// without it a 1-2 char value lands within distance 2 of short type names
// (`x`→`dm`, `zz`→`dm`), changing most of the string yet still "suggesting".
// `bg`→`bug` (distance 1, preserves both input chars) and `tas`→`task` survive.
func closestType(value string, valid []string) string {
	value = strings.ToLower(value)
	maxDist := min(2, len(value)-1)
	if maxDist < 1 {
		return "" // 1-char (or empty) input: no fuzzy suggestion is confident.
	}
	best := ""
	bestDist := maxDist + 1
	for _, t := range valid {
		if d := levenshtein(value, strings.ToLower(t)); d < bestDist {
			bestDist = d
			best = t
		}
	}
	return best
}

// isLikelyRoleNamespace reports whether value names an agent ROLE rather than a
// bead type — the common confusion behind `--type=gardener` (ka-0000006244),
// where `gardener` is a role:* label namespace, not a type. Kept to a small,
// confident allow-list so the hint never fires spuriously.
func isLikelyRoleNamespace(value string) bool {
	switch strings.ToLower(value) {
	case "gardener", "crew", "thread", "triage", "first-mate", "watchdog":
		return true
	}
	return false
}

// levenshtein computes the Levenshtein edit distance between a and b using the
// standard two-row dynamic-programming table. Small inputs (type names), so the
// O(len(a)*len(b)) cost is negligible.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}
