package server

import (
	"sort"
	"strings"
)

// BuiltinBeadTypeNames returns the sorted set of built-in bead-type names —
// the `type:<name>` config keys defined in builtinConfigs, with the `type:`
// prefix stripped.
//
// This is the same registry the CREATE path validates against (via
// resolveTypeConfig, which 400s with "unknown bead type" for anything not in
// this set or a stored custom type:* config). It is exported so CLI-side
// --type filter validation (`kd list` / `kd search`) can reject unknown type
// values instead of silently returning an empty result that is
// indistinguishable from a genuinely empty set (ka-0000006244).
func BuiltinBeadTypeNames() []string {
	// builtinConfigsByNamespace["type"] is the precomputed index of every
	// type:* builtin config (built once at package init), so reuse it rather
	// than re-scanning the full builtinConfigs map.
	typeConfigs := builtinConfigsByNamespace["type"]
	out := make([]string, 0, len(typeConfigs))
	for _, c := range typeConfigs {
		if name, ok := strings.CutPrefix(c.Key, "type:"); ok && name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
