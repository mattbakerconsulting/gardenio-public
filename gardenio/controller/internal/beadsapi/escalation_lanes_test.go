package beadsapi

import "testing"

func TestEscalationHeldBy_UnstampedIsEmpty(t *testing.T) {
	cases := []struct {
		name string
		b    *BeadDetail
		want string
	}{
		{"nil bead", nil, ""},
		{"no fields", &BeadDetail{}, ""},
		{"blank held_by", &BeadDetail{Fields: map[string]string{"held_by": "  "}}, ""},
		// Positive control: a stamped lane is returned as stamped.
		{"coordinator", &BeadDetail{Fields: map[string]string{"held_by": "coordinator:laptop"}}, "coordinator:laptop"},
	}
	for _, c := range cases {
		if got := EscalationHeldBy(c.b); got != c.want {
			t.Errorf("%s: EscalationHeldBy = %q, want %q", c.name, got, c.want)
		}
	}
	if got := EscalationExecuteBy(&BeadDetail{}); got != "" {
		t.Errorf("unstamped execute_by = %q, want empty (legacy execution_target path)", got)
	}
}

func TestLaneLabelAndLandOwner(t *testing.T) {
	for ref, want := range map[string]string{
		"":                       "",
		"operator":               "the owner", // the lane, not #756's operator audience
		"land-owner:acme":        "the acme owner",
		"coordinator:laptop":     "coordinator laptop",
		"coordinator:merge-desk": "coordinator merge-desk",
		"steward:acme":           "the acme steward",
		"requester":              "the requesting agent",
		"server":                 "the server",
	} {
		if got := LaneLabel(ref); got != want {
			t.Errorf("LaneLabel(%q) = %q, want %q", ref, got, want)
		}
	}
	if land, ok := LandOwnerLane("land-owner:acme"); !ok || land != "acme" {
		t.Errorf("LandOwnerLane(land-owner:acme) = %q, %v", land, ok)
	}
	for _, ref := range []string{"operator", "steward:acme", "land-owner:", ""} {
		if _, ok := LandOwnerLane(ref); ok {
			t.Errorf("LandOwnerLane(%q) = true, want false", ref)
		}
	}
}
