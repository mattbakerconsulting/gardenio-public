package gardenhub

import "testing"

// landHeightHash is the sha256 of web/splash/land.js's heightfield() (257 x 257 float32 heights). The height function uses only
// arithmetic, Math.floor, Math.sqrt and Math.imul, so it is the same on every engine and machine; a change to the land's
// shape changes it, and is pinned here in the same commit.
const landHeightHash = "98123cda7ded05a82b41a2fd23d4e224b9874df50954b8d90ab7014520ebb488"

// The land (web/splash/land.js, plan gd-EJGA3zDGCk step 3) runs under node (testdata/land_test.js): the heightfield is the same
// bytes twice and from a fresh module, and those bytes are landHeightHash; the camera lies 1.4 m above the near slope, the
// crest about 20 m ahead and its summit higher than the eye, the valley and the river far below and the two ridges above;
// through three.js's camera the crest sits a third of the way up the frame with the stone right of centre on a phone and a
// desktop; the painter lays out every crop and feature alike in both layers; and the far ring lies under the field.
func TestLandUnderNode(t *testing.T) {
	r := nodeScript(t, "land_test.js")
	if r["hash"] != landHeightHash {
		t.Errorf("heightfield sha256 %v, want %s: the land's shape changed; if you meant it, pin the new hash", r["hash"], landHeightHash)
	}
	t.Logf("land: eye %vm over feet %vm, crest %v, summit %vm, valley floor %vm, ridges %v, pitch %v deg, frames %v, %v fields",
		r["eye"], r["feet"], r["crest"], r["summit"], r["valley"], r["ridges"], r["pitch"], r["frames"], r["fields"])
}
