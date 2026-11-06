package beadsapi

import "testing"

func TestHumanOnly(t *testing.T) {
	for _, c := range []struct {
		name string
		b    *BeadDetail
		want bool
	}{
		{"owner at a device", &BeadDetail{Labels: []string{"project:gardenio", "owner-at-device"}}, true},
		{"a crew's step", &BeadDetail{Labels: []string{"project:gardenio"}}, false},
		{"nil", nil, false},
	} {
		if got := c.b.HumanOnly(); got != c.want {
			t.Errorf("%s: HumanOnly() = %v, want %v", c.name, got, c.want)
		}
	}
}
