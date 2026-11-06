package gardenhub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The splash's light (web/splash/light.js, plan gd-EJGA3zDGCk step 7) runs under node (testdata/splash_light_test.js) with the
// vendored three.js: the sun or the moon in two shadow cascades (2048 px round the stone, 1024 px fitted round the near hill
// and the valley to the farm) with three's light loop patched so they sum to one light; a day a minute at a time at 45 N and
// 52 N with nothing popping; the exposure, the stars, the fireflies and the sky's warmth by the twilight; the moon lit towards
// the sun as far as its lit fraction; the stars seeded and turning about the pole; and the stone's shadow read from the scene,
// west at 07:00 and east at 17:00 at the equinox.
func TestSplashLightUnderNode(t *testing.T) {
	r := nodeScript(t, "splash_light_test.js")
	t.Logf("light: far texel %v m, largest steps a minute %v, exposure %v, shadow (bearing, m) %v", r["farTexel"], r["steps"], r["exposure"], r["shadow"])
}

// The splash's shadows in Chromium (plan gd-EJGA3zDGCk step 7): the page drawn in WebGL at 07:00 at the equinox, 45 N 0 E,
// whose probe (window.__splash) reads the stone's shadow from the scene, west of the stone; relit at 17:00, east; no shader
// fails to compile (three's light loop is patched for the two cascades). What is asserted lives in
// testdata/splash_shadows_browser.js.
func TestSplashShadowsUnderChromium(t *testing.T) {
	underChromium(t, "splash_shadows_browser.js", func() *httptest.Server {
		mux := http.NewServeMux()
		(&Handler{}).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	})
}
