package beadsapi

import (
	"regexp"
	"strings"
)

// Seat derivation (laurels/lineage epic ka-0000006664, task ka-0000007094).
//
// A SEAT here is a POSITION THAT KEEPS GETTING RE-OCCUPIED — nothing more. It
// is not a persona, a tenure, or a self that persists across occupants. The
// distinction matters because the fleet ALREADY runs on seats and simply does
// not record them: over 7 days, 353 agent-name bases were used by more than one
// agent (3,146 agents), and 318 of those recurring bases carried no lineage at
// all. `gardenio-gardenio-gardener-bugfix-dispatch` ran 142 times with no chain;
// `monorepo-monorepo-wedge-watch-merge-train` ran 267 times with exactly one
// pointer. The design memo (ka-0000001420 §2.1) already declares the NAME to be
// the unit of persistence — "identity is the stable agent NAME; a bead is one
// incarnation". The data plane just never wrote it down, so nothing could be
// derived from it.
//
// A seat is therefore a measurable positional fact, which is exactly why it is
// the honest version of this idea: you can verify how many times a seat has
// been occupied by counting beads. Nothing here asks anyone to believe an agent
// is a person, and nothing replays a prior occupant's self-assessment to its
// successor (see the deliberate exclusions in task_predecessor.go).
//
// WHAT THIS IS FOR: giving lineage_root_id a real definition (the first
// occupant of a seat) and letting the viewer group a recurring name's
// occupancies. That is a knowledge-retention argument, and it stands whatever
// you believe about agent experience.

// seatSuffixRE matches the trailing random token the spawn-name generators
// append. Three lengths are in live use — spawnRandomSuffix(3) on the successor
// mint, (4) on the default spawn-name generator, and randomSuffix(6) on the
// gitlab mr-<iid>-<suffix> path — so the pattern accepts 3-6 characters after
// the final hyphen, and REQUIRES at least one lowercase letter among them.
//
// The letter requirement is what keeps a meaningful numeric segment from being
// eaten. `thread-1786846496-334909` ends in a 6-digit fragment of a Slack
// thread timestamp; without the requirement it strips to
// `thread-1786846496`, silently merging every thread agent on that channel-ts
// into one bogus seat. Requiring a letter costs a small, SAFE-DIRECTION miss:
// a generated suffix drawn from the 36-char alphabet is all-digits about
// (10/36)^3 ≈ 2% of the time at length 3 and far less above that, and in that
// case the occupant simply gets its own seat key instead of joining its
// predecessors'. Splitting a seat under-reports occupancy; merging unrelated
// agents would corrupt it. Only the first is recoverable by inspection.
//
// Anchored at end-of-string and applied ONCE: the point is to strip the
// generator's token, not to peel meaningful trailing segments off a name. The
// "contains a letter" half is checked in code rather than in the pattern, since
// RE2 has no lookahead and expressing "3-6 chars, at least one a letter" as a
// bare alternation is unreadable and easy to get subtly wrong.
var seatSuffixRE = regexp.MustCompile(`-([a-z0-9]{3,6})$`)

// hasLower reports whether s contains at least one ASCII lowercase letter.
func hasLower(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= 'a' && r <= 'z' })
}

// SeatKey derives the stable seat identifier for an agent name: the name with
// its generated random suffix removed, scoped by project and role.
//
// WHY SCOPED, NOT THE BARE NAME BASE. Measured against 1,930 agent beads from a
// 72-hour window, stripping the suffix yields 407 distinct bases — and exactly
// two of them span more than one role: `monorepo-merge-train-failure-res` ran
// under both `crew` and `monorepo-cicd`, and `fix-kdl15zsggiyw-gb` under both
// `crew` and `dataplane-fixer`. Those are two DIFFERENT positions that happen
// to share a task-derived name, and collapsing them would make a seat's
// occupancy history silently wrong in a way nothing downstream could detect.
// Scoping by (project, role, base) costs nothing and removes the failure.
//
// Returns "" when there is no usable name, so callers can skip stamping rather
// than write a meaningless key.
func SeatKey(agentName, project, role string) string {
	base := SeatNameBase(agentName)
	if base == "" {
		return ""
	}
	// Empty project/role are rendered as explicit empty segments rather than
	// dropped: a seat key must be unambiguous, and "proj//base" colliding with
	// "proj/role/base" would defeat the scoping above.
	return strings.TrimSpace(project) + "/" + strings.TrimSpace(role) + "/" + base
}

// SeatNameBase strips the generated random suffix from an agent name, returning
// the stable base. It is the pure name-level half of SeatKey, exported so the
// viewer can group displayed names without re-deriving the rule.
//
// Conservative by design: it strips ONLY when the trailing segment matches the
// generator's shape, and never strips the entire name. A name with no
// generator-shaped suffix is returned unchanged — 92 of the 1,930 sampled names
// are like this (`thread-1786846496-334909` and similar), and inventing a base
// for them would fabricate seats that never existed.
func SeatNameBase(agentName string) string {
	name := strings.TrimSpace(agentName)
	if name == "" {
		return ""
	}
	m := seatSuffixRE.FindStringSubmatchIndex(name)
	if m == nil {
		return name
	}
	// Require a letter in the matched token — see seatSuffixRE's doc comment for
	// why an all-digit tail is left alone.
	if !hasLower(name[m[2]:m[3]]) {
		return name
	}
	stripped := name[:m[0]]
	// Never reduce a name to nothing: a name that IS just a suffix-shaped token
	// has no base to speak of, so it stands as its own seat.
	if stripped == "" {
		return name
	}
	return stripped
}
