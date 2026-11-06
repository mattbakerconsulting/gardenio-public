package gardenhub

import "testing"

// The land header (web/landhd.js, Lands Track B step 8.8) runs under node (testdata/landhd_test.js): ?land= read from the
// address, the purse in words at every level (ok, half used, low, the wall, unknown, none granted, an operator land's
// none), and the plaque left alone off a land and on one the farm does not show, then named, its purse said and linked
// to the land's credits once the farm document shows the land, and moved by the next document; and the steward's run cap
// beside it while it is hit (gd-OvKMtyWtrp), hidden while the steward runs or has none.
func TestLandHeaderUnderNode(t *testing.T) {
	r := nodeScript(t, "landhd_test.js")
	for _, k := range []string{"pure", "page"} {
		if r[k] != true {
			t.Errorf("landhd_test.js did not reach %s: %v", k, r)
		}
	}
}
