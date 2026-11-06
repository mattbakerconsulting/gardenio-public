package gardenhub

import "testing"

// stoneGeometryHash is the sha256 of what web/splash/stone.js builds: the body's and the plinth's vertex positions, then each
// gem's facets and seat. The builders use only arithmetic, Math.floor, Math.sqrt and Math.imul, so it is the same on every
// engine and machine; a change to the stone's shape changes it, and is pinned here in the same commit.
const stoneGeometryHash = "8814d4614cd27f2ce02623dad0a7aa8246c971376eb4e48eb13ef45f311bb12c"

// The stone (web/splash/stone.js, plan gd-EJGA3zDGCk step 4) runs under node (testdata/stone_test.js): its geometry is the same
// bytes twice and from a fresh module, and those bytes are stoneGeometryHash; the body is a closed block with its normals out,
// its panel dressed true and its arrises chipped, on a plinth proud of it; the mark is logo.svg's paths, the gems frame it in
// four tinctures; the cut's maps face out of the cut; through the camera the wordmark is wide enough to read on a phone; and
// build() puts the maps on uv1 and patches the shaders.
func TestStoneUnderNode(t *testing.T) {
	r := nodeScript(t, "stone_test.js")
	if r["hash"] != stoneGeometryHash {
		t.Errorf("stone geometry sha256 %v, want %s: the stone's shape changed; if you meant it, pin the new hash", r["hash"], stoneGeometryHash)
	}
	t.Logf("stone: %v vertices, %v triangles in %v ms, plinth %v vertices in %v stones, %v gems, %v chips, panel true to %v mm, frames %v",
		r["vertices"], r["triangles"], r["ms"], r["plinth"], r["stones"], r["gems"], r["chips"], r["panel_mm"], r["frames"])
}
