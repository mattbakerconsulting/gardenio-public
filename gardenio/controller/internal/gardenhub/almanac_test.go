package gardenhub

import "testing"

// The almanac (web/almanac.js, plan gd-EJGA3zDGCk step 2) runs under node (testdata/almanac_test.js): noon on the June
// solstice at 45 N is about 68.4 degrees high and the sun is due south then; 26 Oct 2026 is a full moon that rises at sunset
// opposite the sun, and the phase is new again after 29.53 days; direction() of a 90 degree elevation is (0, 1, 0), north is
// -z and east +x; twilight() names its bands; place() reads ?lat=&lon= or guesses the longitude from the timezone; and
// skyPalette() is continuous from afternoon through dusk into moonlit night and back, handing the light from sun to moon
// only when neither shines.
func TestAlmanacUnderNode(t *testing.T) {
	r := nodeScript(t, "almanac_test.js")
	t.Logf("almanac: solstice noon %v at %v degrees, full moon %v lit, sunset %v, moonrise %v, worst 10 s step %v/255",
		r["noon"], r["elevation"], r["fullMoon"], r["sunset"], r["moonrise"], r["worstStep"])
}
