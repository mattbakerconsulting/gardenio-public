package beadsapi

import "testing"

func TestSeatNameBase(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// The three generator suffix lengths in live use: spawnRandomSuffix(3)
		// on the successor mint, (4) on the default name generator, and
		// randomSuffix(6) on the gitlab mr-<iid> path.
		{"strips 3-char suffix", "gardenio-bridge-jacks-watcher-bud", "gardenio-bridge-jacks-watcher"},
		{"strips 4-char suffix", "gardenio-gardenio-gardener-bugfix-dispatch-l369", "gardenio-gardenio-gardener-bugfix-dispatch"},
		{"strips 6-char suffix", "mr-1212-0wdkiq", "mr-1212"},

		// The whole point of a seat: two occupants of the same position reduce
		// to the same base.
		{"two occupants same seat A", "gardenio-gardenio-gardener-delivery-drive-r-0n76", "gardenio-gardenio-gardener-delivery-drive-r"},
		{"two occupants same seat B", "gardenio-gardenio-gardener-delivery-drive-r-33jk", "gardenio-gardenio-gardener-delivery-drive-r"},

		// Conservative: a name with no generator-shaped suffix is left alone.
		// Inventing a base for these would fabricate seats that never existed —
		// 92 of 1,930 sampled fleet names are like this.
		//
		// The thread case is the regression this rule exists for: the tail is a
		// 6-digit fragment of a Slack thread timestamp, and stripping it would
		// merge every thread agent on that channel-ts into one bogus seat.
		{"all-digit tail is not a suffix", "thread-1786846496-334909", "thread-1786846496-334909"},
		{"short all-digit tail is not a suffix", "mr-1212", "mr-1212"},
		{"no hyphen at all", "concierge", "concierge"},

		// A 7+ char tail is longer than any generator produces, so it is a real
		// name segment.
		{"tail too long to be a generated suffix", "gardenio-fix-deployment", "gardenio-fix-deployment"},

		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"whitespace trimmed", "  gardenio-thing-ab12  ", "gardenio-thing"},

		// A name that is nothing BUT a suffix-shaped token has no base to
		// speak of; it must stand as its own seat rather than reduce to "".
		{"bare suffix-shaped name survives", "a1b2", "a1b2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SeatNameBase(tt.in); got != tt.want {
				t.Errorf("SeatNameBase(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSeatKey_ScopesByProjectAndRole pins the reason the key is scoped rather
// than being the bare name base. Measured against 1,930 real agent beads, two
// name bases spanned more than one role — the same task-derived name run by
// different kinds of agent. Those are different positions, and collapsing them
// would corrupt a seat's occupancy history undetectably.
func TestSeatKey_ScopesByProjectAndRole(t *testing.T) {
	const name = "monorepo-monorepo-merge-train-failure-res-ab12"

	crew := SeatKey(name, "monorepo", "crew")
	cicd := SeatKey(name, "monorepo", "monorepo-cicd")
	if crew == cicd {
		t.Fatalf("same name under different roles must not share a seat: both %q", crew)
	}
	if crew != "monorepo/crew/monorepo-monorepo-merge-train-failure-res" {
		t.Errorf("unexpected seat key: %q", crew)
	}

	// Same name + role in a different project is likewise a different seat.
	if SeatKey(name, "gardenio", "crew") == crew {
		t.Error("same name+role across projects must not share a seat")
	}
}

// TestSeatKey_SameSeatAcrossOccupants is the property the whole change exists
// for: successive occupants of one position derive one stable key.
func TestSeatKey_SameSeatAcrossOccupants(t *testing.T) {
	a := SeatKey("gardenio-gardenio-gardener-bugfix-dispatch-l369", "gardenio", "gardener")
	b := SeatKey("gardenio-gardenio-gardener-bugfix-dispatch-py0w", "gardenio", "gardener")
	c := SeatKey("gardenio-gardenio-gardener-bugfix-dispatch-x3e2", "gardenio", "gardener")
	if a != b || b != c {
		t.Errorf("occupants of one seat derived different keys: %q, %q, %q", a, b, c)
	}
	if a == "" {
		t.Error("expected a non-empty seat key")
	}
}

// TestSeatKey_EmptyNameYieldsNoKey — callers skip the stamp rather than writing
// a meaningless "proj/role/" key.
func TestSeatKey_EmptyNameYieldsNoKey(t *testing.T) {
	if got := SeatKey("", "gardenio", "crew"); got != "" {
		t.Errorf("SeatKey with empty name = %q, want empty", got)
	}
}

// TestSeatKey_EmptyScopeSegmentsStayExplicit guards the ambiguity the scoping
// is meant to remove: an absent project must not let two different seats
// collide by segment-shifting.
func TestSeatKey_EmptyScopeSegmentsStayExplicit(t *testing.T) {
	noProject := SeatKey("thing-ab12", "", "crew")
	noRole := SeatKey("thing-ab12", "crew", "")
	if noProject == noRole {
		t.Errorf("empty project and empty role must not collide: both %q", noProject)
	}
}
