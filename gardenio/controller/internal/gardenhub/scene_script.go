package gardenhub

// SceneScript is the page-scene.js bundle exactly as /garden serves it: the
// World's drawing (web/scene.js, LW.3 gd-VEbAvsTNDe) and the scripts it needs,
// with none of the signed-in page. A land window (beads-viewer's /w/{token},
// LW.4 gd-OJOsIH52NE) serves it under /w/ so its page draws what /garden
// draws. ok is false when the bundle did not build.
func SceneScript() (js string, ok bool) {
	if pageAssets.broken["page-scene.js"] != nil {
		return "", false
	}
	a := pageAssets.byName["page-scene.js"]
	if a == nil {
		return "", false
	}
	return a.body, true
}
