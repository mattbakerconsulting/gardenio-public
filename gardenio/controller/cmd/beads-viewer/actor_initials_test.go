package main

import "testing"

func TestActorInitials(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"alice", "AL"},
		{"a", "A"},
		{"Bob", "BO"},
		{"mike.tilkov", "MT"},
		{"alice-bob", "AB"},
		{"gardenio-inbox-pushdense-condense-71z", "GIPC"},
		{"sched-gardenio-gardener-delivery-4gr3", "SGGD"},
		{"mr-887-z0ot22", "M8Z"},
		{"jane_doe", "JD"},
		// Leading punctuation in a segment is skipped, not emitted — guards the
		// fleet-initials paren-garbage regression where "(gardenio," yielded "(".
		{"Bug-Response Gardener (gardenio, aggressive)", "BRGG"},
		{"MR Relevance Gardener (epic", "MRGE"},
		{"(scoped)", "SC"},
	}
	for _, tc := range cases {
		got := actorInitials(tc.in)
		if got != tc.want {
			t.Errorf("actorInitials(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
