package badge

import (
	"context"
	"fmt"
	"strings"
)

// LineNode is one bead in a badge subject's line: its id, its arms in the
// canonical stored form ("" for none), its bead type, and its parent-child
// parents in edge order (none at the top of the line). A thread-filed task
// usually has two: the Slack/Commons thread bead and the phase epic
// (gd-NgFx1XzPAL).
type LineNode struct {
	ID       string
	Heraldry string
	Type     string
	Parents  []string
}

// LineLookup reads one bead of the line. The gb CLI backs it with the daemon
// (GetBead + GetDependencies); tests back it with a map.
type LineLookup func(ctx context.Context, id string) (LineNode, error)

// Arms is the arms a badge is drawn from and where they came from.
type Arms struct {
	// Heraldry is the canonical stored form of the arms, "" when the line
	// bears none.
	Heraldry string
	// From is the id of the bead that supplied them: the subject itself, or
	// the nearest armed ancestor. "" when the line bears none.
	From string
}

// None reports whether no bead in the line bears arms, so the badge is drawn
// from the subject's title and description alone.
func (a Arms) None() bool { return a.Heraldry == "" }

// Inherited reports whether the arms came from an ancestor rather than the
// subject itself.
func (a Arms) Inherited(subjectID string) bool { return a.From != "" && a.From != subjectID }

// maxLineReads bounds the walk up the parent-child line. Task → phase → plan
// → project is four levels; 32 mirrors beadsapi.maxAncestorDepth and ends a
// degenerate, wide or cyclic graph in a bounded number of round-trips.
const maxLineReads = 32

// ResolveArms returns the arms for the bead id: its own if it bears any, else
// the nearest arms found walking every parent-child edge upward (task to
// phase to plan to project), recording which bead supplied them. The walk is
// breadth-first, so a nearer ancestor always beats a farther one; among
// armed ancestors at the same distance an epic wins (a task under both a
// thread bead and a phase takes the phase's arms), else the first in edge
// order. A line with no arms anywhere returns the zero Arms and no error. A
// lookup error part way up returns the zero Arms with the error; the caller
// treats that as no arms, so a badge is always drawn.
func ResolveArms(ctx context.Context, id string, lookup LineLookup) (Arms, error) {
	seen := make(map[string]bool, 4)
	level := []string{strings.TrimSpace(id)}
	reads := 0
	for len(level) > 0 {
		var next []string
		var found Arms
		for _, cur := range level {
			if cur == "" || seen[cur] {
				continue
			}
			if reads == maxLineReads {
				return found, nil
			}
			seen[cur] = true
			reads++
			node, err := lookup(ctx, cur)
			if err != nil {
				return Arms{}, fmt.Errorf("badge: reading %s for arms: %w", cur, err)
			}
			if h := strings.TrimSpace(node.Heraldry); h != "" {
				if node.Type == "epic" {
					return Arms{Heraldry: h, From: cur}, nil
				}
				if found.None() {
					found = Arms{Heraldry: h, From: cur}
				}
				continue
			}
			for _, p := range node.Parents {
				next = append(next, strings.TrimSpace(p))
			}
		}
		if !found.None() {
			return found, nil
		}
		level = next
	}
	return Arms{}, nil
}
