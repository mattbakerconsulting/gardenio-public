// Package planwindow is the Window's read model (Lands: the Window, plan
// gd-pz87vf3xsB, step PW.2 gd-jX6ogU9l5T): what a /w/<token> page shows of one
// plan to a visitor with no account.
//
// The wall is in kbeads. A window token opens one daemon route, GET
// /v1/windows/self/view, which walks the plan's closure and returns an
// allowlisted projection of it (narrative §3 PW-D2, PW-D4): Payload, which
// decodes kbeads' windowView field for field. This package turns that payload
// into the View the page renders and plan.json serves, with pure functions and
// nothing else:
//
//   - Build shapes the payload for the page: each phase's status, the
//     decisions (open first), the "waiting on the operator" summary, the
//     crew (live first) and the Recent strip (the newest RecentLimit changes
//     in the closure, newest first). Progress and the current phase are the
//     daemon's; Build works them out itself only when the payload has steps
//     but no steps_total.
//   - Every free-text field of the View goes through the render-time hygiene
//     check (PW-D5): the Lands contract's hygiene_denylist.txt plus an email
//     pattern. A hit withholds that field (WithheldText), counts it in
//     WithheldTotal and lists it in View.Withheld for the owner's preview.
//     The narrative is checked block by block, so one internal reference
//     withholds a paragraph, not the whole narrative.
//
// A land window (plan gd-4iVYF9tGsn) gets a land view from the same route
// instead: LandPayload, kbeads' windowLandView, read by DecodeLand. CleanLand
// runs the same hygiene check over its every free-text field, and ToScene
// shapes the result as the part of the farm document the World scene reads
// (Scene), so gardenhub's world.js lays a land out with no new layout code.
// testdata/land holds the land payloads (daemon.json is LW.1's golden) and
// testdata/scene their Scene goldens (go test -run TestSceneGoldens -update).
//
// It is a leaf: it imports nothing from this repo (TestLeaf), calls no daemon
// and does not reuse the cockpit's *Server methods in cmd/beads-viewer, so the
// page (PW.3) and the owner's preview (PW.4) cannot read anything the payload
// did not carry. testdata/payload holds the agreed payload fixtures and
// testdata/plan the plan.json goldens (go test -run TestGoldens -update
// rewrites them).
package planwindow
