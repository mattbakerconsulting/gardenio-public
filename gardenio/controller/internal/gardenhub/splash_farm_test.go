package gardenhub

import "testing"

// farmLayoutHash is the sha256 of where web/splash/farm.js's layout() puts everything: the house and the barn, their
// windows and chimneys, the gate, the walls and their coping, the well and the cart, the bales, the paddocks with their
// posts and beasts, the footbridge, the pond and the birds' loops. The layout uses only arithmetic, Math.floor, Math.sqrt
// and Math.imul on the land's drawn surface, so it is the same on every engine and machine; a change to the farm's plan
// changes it, and is pinned here in the same commit.
const farmLayoutHash = "1bf9eabbc12e591967750d1c8e8d1741b6e36d89017002f59332a5d5c0e799b0"

// The farm (web/splash/farm.js, plan gd-EJGA3zDGCk step 6) runs under node (testdata/splash_farm_test.js): its builder lays out
// the same numbers twice and from a fresh module, and those are farmLayoutHash; isLit(t) flips at civil dusk and civil dawn,
// the sun 6 degrees down, at 45 N in October and 52 N at midsummer, to the second; the chimney smokes from an hour before
// sunset to sunrise, swifts fly by day and bats after dusk; the farmstead stands where the eye sees it, the gate across the
// lane, the walls, bales, beasts, bridge and pond where they belong; and build() makes its meshes and sets its hour.
func TestFarmUnderNode(t *testing.T) {
	r := nodeScript(t, "splash_farm_test.js")
	if r["hash"] != farmLayoutHash {
		t.Errorf("farm layout sha256 %v, want %s: the farm's plan changed; if you meant it, pin the new hash", r["hash"], farmLayoutHash)
	}
	t.Logf("farm: clock %v; windows %v lit of all (front seen %v), wall %v points, coping %v, bales %v, paddocks %v, beasts %v, posts %v, pond %v under the eye at %v, %v triangles, laid out in %v ms",
		r["clock"], r["windows"], r["front"], r["wall"], r["coping"], r["bales"], r["paddocks"], r["beasts"], r["posts"], r["pond"], r["eye"], r["triangles"], r["ms"])
}
