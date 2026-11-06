package gardenhub

import "strings"

// The names the tests read the page's sources by, from before page.go embedded web/ whole (gd-MMuj3Rj6tn). A new
// file needs no name here: its tests read pageSources["<file>"].
var (
	pageCSS           = gardenCSS()
	pageArtJS         = pageSources["art.js"]
	pageWorldJS       = pageSources["world.js"]
	pageCursorJS      = pageSources["cursor.js"]
	pageWeedsJS       = pageSources["weeds.js"]
	pageBeaconsJS     = pageSources["beacons.js"]
	pageTransitionsJS = pageSources["transitions.js"]
	pageBirdsJS       = pageSources["birds.js"]
	pageGoatsJS       = pageSources["goats.js"]
	pageTruckJS       = pageSources["truck.js"]
	pageSceneJS       = pageSources["scene.js"]
	pageSnapJS        = pageSources["snap.js"]
	pageStipJS        = pageSources["stip.js"]
	pageSignFitJS     = pageSources["signfit.js"]
	pagePorchJS       = pageSources["porch.js"]
	pageTitlesJS      = pageSources["titles.js"]
	pageReasonJS      = pageSources["reason.js"]
	pageNeighboursJS  = pageSources["neighbours.js"]
	pageEdgesJS       = pageSources["edges.js"]
	pageTourJS        = pageSources["tour.js"]
	pageAnswerJS      = pageSources["answer.js"]
	pageLazyJS        = pageSources["lazy.js"]
	pageAppJS         = pageSources["garden.js"]
	pagePlansJS       = pageSources["plans.js"]
	pageChatJS        = pageSources["chat.js"]
	pageWorkbenchJS   = pageSources["workbench.js"]
	pageBeadWriteJS   = pageSources["beadwrite.js"]
	pageArmsJS        = pageSources["arms.js"]
	pageBundleJS      = pageSources["bundle.js"]
	pageReleaseJS     = pageSources["release.js"]
	pageProjectJS     = pageSources["project.js"]
	pageGardenerJS    = pageSources["gardener.js"]
	pageSettingsJS    = pageSources["settings.js"]
	pageCostsJS       = pageSources["costs.js"]
	pageCreditsJS     = pageSources["credits.js"]
	pageLandHdJS      = pageSources["landhd.js"]
	pageBecomeJS      = pageSources["become.js"]
	pageGlassJS       = pageSources["glass.js"]
	pageGlassActJS    = pageSources["glassact.js"]
	pageGlassKeyJS    = pageSources["glasskey.js"]
	pageCredsJS       = pageSources["creds.js"]
	pageWallJS        = pageSources["wall.js"]
	pageCommons2JS    = pageSources["commons2.js"]
	pagePicturesJS    = pageSources["pictures.js"]
	pageTranscriptJS  = pageSources["transcript.js"]
)

// gardenCSS is web/css/*.css joined in the page's order: the one stylesheet the page's styles were before they were split
// one file per feature (gd-MMuj3Rj6tn), for the tests that look for a rule anywhere in them.
func gardenCSS() string {
	names, err := bundleNames("page.css", `{{asset "css/*.css"}}`, pageSources)
	if err != nil {
		panic(err)
	}
	var b strings.Builder
	for _, n := range names {
		b.WriteString(pageSources[n])
	}
	return b.String()
}
