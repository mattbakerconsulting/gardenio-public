package gardenhub

import "testing"

// floraLayoutHash is the sha256 of where web/splash/flora.js's layout() puts everything: the blades, the flowers, the hedge
// clumps, the impostors and the rooks, then each tree's limbs and leaves. The layout uses only arithmetic, Math.floor,
// Math.sqrt and Math.imul on the land's heights, so it is the same on every engine and machine; a change to what grows where
// changes it, and is pinned here in the same commit.
const floraLayoutHash = "2a00f7e79250c214c98de847945dd9a0d7341ff7e75a433a9d243bb075c335c2"

// What grows on the splash's land (web/splash/flora.js, plan gd-EJGA3zDGCk step 5) runs under node (testdata/flora_test.js):
// its layout is the same bytes twice and from a fresh module, and those bytes are floraLayoutHash; the grass fills the near
// meadow off the path, the lane and the stone; the wildflowers are thickest along the path and the hedge's foot; the
// hedgerows are where the eye sees them; the oak frames the left and the top of a phone's and a desktop's view without
// covering the stone; poplars line the river and apples the orchard; the woods and the rooks are on the far slopes; and
// build() makes the meshes, every one casting shadow but the impostors, with the wind in their shaders.
func TestFloraUnderNode(t *testing.T) {
	r := nodeScript(t, "flora_test.js")
	if r["hash"] != floraLayoutHash {
		t.Errorf("flora layout sha256 %v, want %s: what grows where changed; if you meant it, pin the new hash", r["hash"], floraLayoutHash)
	}
	t.Logf("flora: %v blades, %v flowers %v (per m2 %v), hedges %v, trees %v, %v leaf cards, %v impostors, %v rooks, %v triangles, laid out in %v ms; oak %v",
		r["blades"], r["flowers"], r["kinds"], r["density"], r["hedges"], r["trees"], r["leaves"], r["impostors"], r["rooks"], r["triangles"], r["ms"], r["oak"])
}
