package main

import (
	"testing"
	"time"
)

// TestSalvagePrefix verifies the S3 key layout with and without an agent bead.
func TestSalvagePrefix(t *testing.T) {
	cases := []struct {
		prefix, bead, session, want string
	}{
		{"coop", "gd-a", "gd-a", "coop/gd-a/gd-a/salvage"},
		{"coop", "gd-a", "sess-uuid", "coop/gd-a/sess-uuid/salvage"},
		{"coop", "", "sess-uuid", "coop/sess-uuid/salvage"},
	}
	for _, c := range cases {
		if got := salvagePrefix(c.prefix, c.bead, c.session); got != c.want {
			t.Errorf("salvagePrefix(%q,%q,%q)=%q want %q", c.prefix, c.bead, c.session, got, c.want)
		}
	}
}

// TestSalvageTimestamp is a smoke test that the manifest stamp is RFC3339-ish.
func TestSalvageTimestamp(t *testing.T) {
	s := salvageTimestamp()
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Errorf("salvageTimestamp %q not RFC3339: %v", s, err)
	}
}
