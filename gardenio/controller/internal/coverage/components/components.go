// Package components maps repo-root-relative paths to the component
// (gated unit) that owns them.
//
// The patch-cov library (W2b-1) and the baseline computer (W2b-6) both
// import this package so the gate-time and baseline-time component
// assignments cannot drift. The python normalizer at
// scripts/coverage/normalize-paths.py handles path rewriting to repo-root
// form; this package only classifies already-rewritten paths.
package components

import (
	"slices"
	"strings"
)

// Component identifies one of the gated units. Cross-cutting paths
// (scripts/, docs/, .github/, Makefile) map to the empty
// Component, signalling "no component, drop from patch-cov".
type Component string

const (
	None       Component = ""
	Controller Component = "controller"
	Kbeads     Component = "kbeads"
	Coop       Component = "coop"
)

// Gated lists the components the patch-cov gate evaluates.
var Gated = []Component{Controller, Kbeads, Coop}

// IsGated reports whether c is a component evaluated by the gate.
func IsGated(c Component) bool {
	return slices.Contains(Gated, c)
}

// Of returns the component owning a repo-root-relative path. Cross-cutting
// paths (scripts/, docs/, .github/, Makefile, top-level
// READMEs) return None and should be dropped from patch-cov.
func Of(p string) Component {
	p = strings.TrimPrefix(p, "/")
	switch {
	case strings.HasPrefix(p, "gardenio/controller/"):
		return Controller
	case strings.HasPrefix(p, "kbeads/"):
		return Kbeads
	case strings.HasPrefix(p, "coop/"):
		return Coop
	}
	return None
}
