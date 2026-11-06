package main

import "strings"

// routeClass is how one viewer route behaves for a scoped principal (see
// viewerScope): the viewer-side twin of kbeads' routeScopeClass
// (kbeads/internal/server/scoped_route_classes.go).
type routeClass int

const (
	// routeAdminOnly: the route is global, cross-project or privileged, or its
	// data does not come through the user's own bearer. A scoped principal gets
	// 403. This is also what a route missing from the table is treated as.
	routeAdminOnly routeClass = iota + 1
	// routeScoped: every read the handler makes goes to the daemon on the
	// request context, so the daemon authenticates it as the signed-in user and
	// narrows it to that user's projects; nothing on the page comes from a shared
	// cache, hub or non-daemon backend without a per-principal cut. A page that
	// needs an admin-only daemon route (kbeads route class) is not in this class.
	routeScoped
	// routePublic: no per-user data at all (static assets, the web manifest,
	// the Lands front of house and its documents).
	routePublic
	// routeApplicant: any signed-in OIDC caller may reach it, including an
	// applicant with no gardenio role (Lands Track B B-D1, §2.3). The handler
	// touches only the caller's own records, on the caller's own bearer (kbeads
	// POST /v1/users/me/terms), so it shows nothing of anyone else. Scoped
	// principals and unrestricted callers reach it too.
	routeApplicant
	// routeMachine: only a machine principal (a Keycloak service account,
	// machine:<client>, or an agent's own per-agent token, held to its land)
	// reaches it, and every other caller gets 403: a human of any rank, scoped
	// or not, a land or shared fleet token, no credential at all.
	// The viewer cannot verify a service-account token itself (behind the
	// edge it runs devAuthBypass, and its OIDC mode wants a human's audience
	// and email), so machineMiddleware asks kbeads who the request's own
	// bearer is (machine_route.go) and scopeMiddleware refuses the route to
	// a request it did not admit. The handler then reads on that bearer.
	routeMachine
)

// viewerRouteClasses classifies every route RegisterRoutes registers, keyed by
// the exact mux pattern. TestEveryViewerRouteHasClass fails the build when a
// registered route is missing here (or a key here is no longer registered), and
// scopeMiddleware treats a missing route as routeAdminOnly, so a new route is
// closed to scoped principals until someone classifies it.
//
// The routes on the outer mux in main.go (/healthz, /readyz and GET /login)
// sit outside the auth chain entirely and are public by construction; they are
// not in this table.
//
// What is scoped, and why the rest is not:
//   - bead list and show, /active, /decisions, /escalations, /plans*, /agents
//     (list), /p/<own project>, /releases, /release/{id}, the /b/{id} read
//     fragments and the feed: reads
//     on the user's bearer, narrowed by the daemon. The feed's SSE hub and the
//     first-paint seed cache are cut per principal (bead_feed_hub.go, feed.go).
//   - every POST is admin-only. Viewer writes are operator-gated already; a
//     scoped human acting on their own project's decisions and approvals is
//     gd-tUi5IuttJn's job, which should move the specific routes it needs.
//     The Commons write routes are the exception: say, reply, join and leave
//     (Commons I step 9), and the typing signal (Commons III step 4.4, which
//     writes nothing to the daemon), are routeScoped, each gated in-handler by
//     viewerScope.mayInChannel or conversation.MembershipChangeAllows, the
//     land-role action table (commonsGate; Lands Commons spec §2.2.1, §6.4),
//     and written on the user's own bearer, which kbeads checks again. The
//     card action and lightbox submit routes (POST /garden/c/action|submit,
//     Commons II step 5) are scoped too: the card is read on the user's
//     bearer, the button is gated by Gate.MayAct (the action table for the
//     card's land), and the action runs on the user's bearer, at the bridge's
//     inbound endpoint (which checks the table again) or, for an escalation's
//     Approve and Reject, at the daemon behind the bead page's capability
//     check. A wish head's Done (POST /garden/c/t/{thread}/done, Commons II
//     step 6) is scoped the same way: the head is read on the user's bearer,
//     gated by Gate.MayAct ("done"), and closed by the bridge on that bearer.
//     POST /garden/c/{land}/{channel}/read is scoped too: it writes only
//     the viewer's own read position, on the viewer's bearer, to their own
//     user bead (kbeads /v1/users/me/read-cursors).
//     Freeze and thaw (the Lands kill switch, step 10.5) are scoped too:
//     internal/commons answers 403 for anyone but an admin, and they write
//     project fields kbeads lets only an admin write. So are a land's member
//     invites (POST .../invite and .../revoke, step 8.9): internal/commons
//     answers 403 without member_manage in the land (Gate.MayAct, the
//     owner's), and lands_invites.go writes the admin-gated land_invite as
//     the Gate's machine principal, never on the owner's bearer.
//     A land's steward-notes Holds save (POST /garden/notes/{land}/holds,
//     gd-q4NgYNGpy8) is scoped too: viewerScope.allows the land and
//     may(edit_holds) (owner and members) in-handler, the patch on the
//     user's bearer, which kbeads checks again (content-only, Holds-only).
//   - the Commons read routes, /garden/c*: every read is on the user's bearer
//     and cut again by land in internal/commons; its event stream is cut per
//     subscriber, with each bead's land read from the daemon (Lands Commons
//     spec §6.2 rule 3, §6.3). The rest of /garden* stays admin-only below,
//     but for the Homestead itself (plan gd-gh5J0vVbJP phase 3, gd-l7yYxZpoZo):
//   - GET /garden, GET /garden/farm, GET /garden/farm/events and POST
//     /garden/farm/events/view: the page and its farm document are cut per
//     viewer to the viewer's projects (gardenhub/farm_cut.go: plans, seeds,
//     gardeners, meadow, wishes, piles, meta, and the cost rows of the set
//     alone, a tenant land's in credits with no at-cost dollars, never a
//     fleet figure), the farm stream per subscriber (farm_cut_stream.go), the
//     page's lists per viewer (Hub.CutSnapshot), and every write the page
//     offers is hidden (readonly.go). The view POST only narrows which calls
//     the requester's own stream sends. GET /garden/events and GET
//     /garden/state stay admin-only: the hub's stream and stats carry every
//     project's items with no per-viewer cut, and a scoped page does not open
//     the stream (body data-cut, web/garden.js).
//   - the page's one connection, GET /garden/live/events and POST
//     /garden/live/sub (gardenhub/livemux.go, P2.4): it reads nothing itself.
//     Each stream it carries is one of the page's event-stream routes, run
//     through scopeMiddleware again on the connection's own request
//     (Handler.Live, main.go), so a stream a scoped principal may not open is
//     refused on the connection exactly as alone. The POST writes nothing: it
//     only adds or lets go streams on a connection its sid names.
//   - POST /garden/welcome (step 9.14b): the welcome tour was had. It writes
//     only welcome_tour_seen_at on the caller's own user bead, on their own
//     bearer alone (kbeads /v1/users/me/welcome-tour, garden_welcome.go).
//   - /costs*, /grafana*, /charts*, /garden/charts/*, /dashboards, /api/v1/*:
//     spend and telemetry with no project cut (and the Grafana proxy speaks as
//     the viewer). The garden's chart sets frame grafana.clonk.tv, which runs
//     with defaultOrgRole Admin behind the admin edge gate, and costs-project
//     takes any project; a tenant's own set will be a new scoped route, not a
//     reclassification of this one (plan gd-gh5J0vVbJP).
//     /api/lands/{land}/purse too: it reads account and credit_rate beads and
//     carries the at-cost dollars (lands_purse.go).
//     /api/releases[/{id}] (releases_api.go) are the exception, routeScoped:
//     every read, the titles too, is on the user's bearer, and the daemon's
//     /v1/releases routes are filtered by the caller's projects.
//   - /taints*, /me/taints, /teams*, /triggers, /telepathy*, /settings,
//     /claude-config*, /advice*, /gardener*, /roles, /role/*:
//     global or privileged, and /taints*, /teams*, /people, /u/*, /fleet* lean on
//     admin-only daemon routes (taints, operators directory).
//   - /agents/{id}/prime|screen|events, /agents/events*: agent terminals and
//     transcripts come from coopmux/NATS, not the daemon. /agents/chat* only
//     redirects to the Commons now, and stays admin-only as it always was.
//   - the rest of /garden* (bead, plan, project, gardener, queue, search,
//     bundle, arms, activity, settings), /index, /inbox*, /threads*, /formulas*, /views*, /me, /deck,
//     /health*: shared snapshot hubs, cross-project rollups or
//     per-operator directories; not yet audited for a per-principal cut.
//   - /internal/*: routeMachine, for in-cluster callers holding a machine
//     principal. With the dev bypass's edge secret set, these and the public
//     routes are all a direct call without the secret reaches (scope.go,
//     gd-ozc7DSwCXO). GET /internal/commons/f/... is the Commons file route for the
//     slack-bridge, which uploads the bytes into Slack (gd-qnF6mJZtkQ), and
//     for an agent's `gb commons file`, its own land only (gd-WXY7Jm5ugY).
//   - /garden/splash and its two files: the splash (plan gd-EJGA3zDGCk) is one
//     document for everyone that reads no bead and no viewer, public, and
//     main.go serves it without a session (noSessionPatterns).
//   - /lands/*: the Lands front of house (lands.go). The landing page, the
//     three legal documents and their stylesheet are public (main.go also
//     serves them without a session); the acceptance page, POST
//     /lands/accept and the GitHub App's install and setup legs
//     (lands_github.go) and the demo land's door (lands_demo.go) are
//     routeApplicant, the only routes an applicant reaches besides the
//     public ones (TestApplicantReachesOnlyLandsRoutes).
var viewerRouteClasses = map[string]routeClass{
	"GET /a/{name}/preview":                     routeAdminOnly,
	"POST /actions/{id}":                        routeAdminOnly,
	"GET /active":                               routeScoped,
	"GET /advice":                               routeAdminOnly,
	"POST /advice":                              routeAdminOnly,
	"GET /advice/new":                           routeAdminOnly,
	"GET /agents":                               routeScoped,
	"GET /agents/chat":                          routeAdminOnly,
	"GET /agents/chat/card/{id}":                routeAdminOnly,
	"GET /agents/chat/conversation/{id}":        routeAdminOnly,
	"GET /agents/chat/message/{id}":             routeAdminOnly,
	"GET /agents/chat/rail":                     routeAdminOnly,
	"GET /agents/events":                        routeAdminOnly,
	"GET /agents/events-status":                 routeAdminOnly,
	"POST /agents/{id}/compact":                 routeAdminOnly,
	"POST /agents/{id}/compact/cancel":          routeAdminOnly,
	"POST /agents/{id}/control/claim":           routeAdminOnly,
	"GET /agents/{id}/events":                   routeAdminOnly,
	"POST /agents/{id}/input":                   routeAdminOnly,
	"POST /agents/{id}/keys":                    routeAdminOnly,
	"POST /agents/{id}/keys/cancel":             routeAdminOnly,
	"POST /agents/{id}/nudge":                   routeAdminOnly,
	"GET /agents/{id}/prime":                    routeAdminOnly,
	"POST /agents/{id}/respond":                 routeAdminOnly,
	"GET /agents/{id}/screen":                   routeAdminOnly,
	"GET /api/lands/{land}/purse":               routeAdminOnly,
	"GET /api/releases":                         routeScoped,
	"GET /api/releases/{id}":                    routeScoped,
	"GET /api/v1/charts":                        routeAdminOnly,
	"GET /api/v1/charts/engagement/{panel}":     routeAdminOnly,
	"GET /api/v1/charts/security/{panel}":       routeAdminOnly,
	"GET /api/v1/charts/uniqueness":             routeAdminOnly,
	"GET /api/v1/charts/{chart_id}":             routeAdminOnly,
	"GET /api/v1/inbox/{id}/subgraph":           routeAdminOnly,
	"GET /api/v1/query_range":                   routeAdminOnly,
	"GET /assets/static/":                       routePublic,
	"GET /b/project/{name}/accounts":            routeAdminOnly,
	"GET /b/project/{name}/activity":            routeScoped,
	"GET /b/project/{name}/agents":              routeScoped,
	"GET /b/project/{name}/captains":            routeAdminOnly,
	"GET /b/project/{name}/costs":               routeAdminOnly,
	"GET /b/project/{name}/decisions":           routeScoped,
	"GET /b/project/{name}/gardeners":           routeAdminOnly,
	"GET /b/role/{role}/agents":                 routeAdminOnly,
	"GET /b/{id}/activity":                      routeScoped,
	"GET /b/{id}/permission-diff":               routeScoped,
	"POST /b/{id}/apply-taint":                  routeAdminOnly,
	"GET /b/{id}/assignee":                      routeScoped,
	"POST /b/{id}/assignee":                     routeAdminOnly,
	"GET /b/{id}/assignee/edit":                 routeAdminOnly,
	"POST /b/{id}/claim":                        routeAdminOnly,
	"POST /b/{id}/close":                        routeAdminOnly,
	"POST /b/{id}/defer":                        routeAdminOnly,
	"GET /b/{id}/comments":                      routeScoped,
	"POST /b/{id}/comments":                     routeAdminOnly,
	"POST /b/{id}/comments/preview":             routeAdminOnly,
	"POST /b/{id}/decision/dismiss":             routeAdminOnly,
	"POST /b/{id}/decision/reflag":              routeAdminOnly,
	"POST /b/{id}/decision/resolve":             routeAdminOnly,
	"GET /b/{id}/deps":                          routeScoped,
	"POST /b/{id}/deps":                         routeAdminOnly,
	"POST /b/{id}/deps/remove":                  routeAdminOnly,
	"GET /b/{id}/description":                   routeScoped,
	"POST /b/{id}/description":                  routeAdminOnly,
	"GET /b/{id}/description/edit":              routeAdminOnly,
	"POST /b/{id}/description/preview":          routeAdminOnly,
	"POST /b/{id}/escalation/approve":           routeAdminOnly,
	"POST /b/{id}/escalation/reject":            routeAdminOnly,
	"POST /b/{id}/escalation/route":             routeAdminOnly,
	"GET /b/{id}/forward":                       routeAdminOnly,
	"POST /b/{id}/forward":                      routeAdminOnly,
	"POST /b/{id}/forward/remove":               routeAdminOnly,
	"POST /b/{id}/inbox-pin/toggle":             routeAdminOnly,
	"POST /b/{id}/inbox-worthy/toggle":          routeAdminOnly,
	"GET /b/{id}/labels":                        routeScoped,
	"POST /b/{id}/labels":                       routeAdminOnly,
	"GET /b/{id}/labels/edit":                   routeAdminOnly,
	"GET /b/{id}/owner":                         routeScoped,
	"POST /b/{id}/owner":                        routeAdminOnly,
	"GET /b/{id}/owner/edit":                    routeAdminOnly,
	"GET /b/{id}/ownership":                     routeScoped,
	"POST /b/{id}/parent":                       routeAdminOnly,
	"GET /b/{id}/preview":                       routeScoped,
	"GET /b/{id}/priority":                      routeScoped,
	"POST /b/{id}/priority":                     routeAdminOnly,
	"GET /b/{id}/priority/edit":                 routeAdminOnly,
	"GET /b/{id}/rating":                        routeScoped,
	"POST /b/{id}/rating":                       routeAdminOnly,
	"POST /b/{id}/release":                      routeAdminOnly,
	"POST /b/{id}/reopen":                       routeAdminOnly,
	"POST /b/{id}/status":                       routeAdminOnly,
	"POST /b/{id}/taints/{taint}/request-scrub": routeAdminOnly,
	"POST /b/{id}/thread-reply":                 routeAdminOnly,
	"GET /b/{id}/title":                         routeScoped,
	"POST /b/{id}/title":                        routeAdminOnly,
	"POST /b/{id}/heraldry":                     routeAdminOnly,
	"GET /b/{id}/title/edit":                    routeAdminOnly,
	"POST /banner/inbox-homepage/dismiss":       routeAdminOnly,
	"POST /banner/migration/dismiss":            routeAdminOnly,
	"GET /bundle/{id}":                          routeScoped,
	"GET /captain/{rest...}":                    routeAdminOnly,
	"GET /captains":                             routeAdminOnly,
	"GET /captains/{rest...}":                   routeAdminOnly,
	"GET /cassettes":                            routeAdminOnly,
	"GET /charts":                               routeAdminOnly,
	"GET /claude-config":                        routeAdminOnly,
	"GET /claude-config/{role}":                 routeAdminOnly,
	"GET /cmdk/suggest":                         routeScoped,
	"GET /costs":                                routeAdminOnly,
	"GET /costs/rollup":                         routeAdminOnly,
	"GET /create-gardener":                      routeAdminOnly,
	"POST /create-gardener":                     routeAdminOnly,
	"GET /dashboards":                           routeAdminOnly,
	"GET /decisions":                            routeScoped,
	"GET /decisions/{id}":                       routeScoped,
	"GET /deck":                                 routeAdminOnly,
	"GET /dev/incident-intel/gallery":           routeAdminOnly,
	"POST /dev/incident-intel/telemetry":        routeAdminOnly,
	"GET /escalations":                          routeScoped,
	"GET /extern":                               routeAdminOnly,
	"GET /feed":                                 routeScoped,
	"GET /feed/backfill":                        routeScoped,
	"GET /feed/events":                          routeScoped,
	"GET /fleet":                                routeAdminOnly,
	"GET /fleet/{bridge}":                       routeAdminOnly,
	"GET /formulas":                             routeAdminOnly,
	"GET /formulas/{id}/pour":                   routeAdminOnly,
	"POST /formulas/{id}/pour":                  routeAdminOnly,
	"GET /garden":                               routeScoped,
	"GET /garden/bead/{id}":                     routeAdminOnly,
	"GET /garden/bead/{id}/events":              routeAdminOnly,
	"GET /garden/arms.svg":                      routeAdminOnly, // the bead view's Heraldry section and Change arms (heraldry 3.2)
	"GET /garden/arms/charge/{cp}":              routeAdminOnly,
	"GET /garden/arms/library":                  routeAdminOnly,
	"GET /garden/arms/register":                 routeAdminOnly,
	"GET /garden/queue/{kind}":                  routeAdminOnly,
	"GET /garden/search":                        routeAdminOnly,
	"GET /garden/names":                         routeAdminOnly, // the ids a page shows untitled, named (gardenhub/names.go)
	"GET /garden/bundle/{id}":                   routeAdminOnly,
	"GET /garden/plan/{id}":                     routeAdminOnly,
	"GET /garden/project/{name}":                routeAdminOnly,
	"GET /garden/gardener/{id}":                 routeAdminOnly,
	"GET /garden/settings":                      routeAdminOnly,
	"GET /garden/charts/{set}":                  routeAdminOnly,
	// A tenant land's credit charts (plan gd-gh5J0vVbJP step 3.3): scoped, though the handler reads vmsingle, a
	// non-daemon backend. Its per-principal cut is in the handler (gardenhub/land_credits.go): the land is checked
	// against viewerScope.allows before any read (404 outside it), the PromQL is fixed with that land as an exact
	// matcher, and the answer has no dollar or URL field.
	"GET /garden/charts/costs-land": routeScoped,
	// A land's steward notes (gd-q4NgYNGpy8, gardenhub/land_notes.go): the land is checked against viewerScope.allows
	// before any read (404 outside it); the read and the Holds save go to the daemon on the user's own bearer, the save
	// gated in-handler by viewerScope.may(edit_holds) (owner and members) and again by kbeads' edit_holds check.
	"GET /garden/notes/{land}":        routeScoped,
	"POST /garden/notes/{land}/holds": routeScoped,
	// The Glass reads (Lands workbench-glass §3.4 #6): scoped, because the handler's land-scope check is the real wall
	// (an agent outside the viewer's lands is a 404) and it also holds them to the operator tier. The credential pool
	// (GL-15) is the whole fleet's, as no land owns an account, so it is admin-only (and its handler holds it to the admin
	// tier, as coopmux holds /api/v1/credentials/*).
	"GET /garden/glass/wall":           routeScoped,
	"GET /garden/glass/{agent}":        routeScoped,
	"GET /garden/glass/{agent}/stream": routeScoped,
	// The Glass's prompt (GL-8): read from coopmux after the same scope check, as the stream is.
	"GET /garden/glass/{agent}/prompt": routeScoped,
	// The Glass's writes (nudge, respond; GL-7/8) stay admin-only until the Foundation action table gives them a
	// scoped class (spec 3.4 #6).
	"POST /garden/glass/{agent}/send": routeAdminOnly,
	// Attach's keyboard claim (GL-9): a write, so admin-only by class like /send; the handler holds a tenant land to admin.
	"POST /garden/glass/{agent}/control":              routeAdminOnly,
	"GET /garden/glass/credentials":                   routeAdminOnly,
	"GET /garden/plan/{id}/events":                    routeAdminOnly,
	"GET /garden/plan/{id}/pour":                      routeAdminOnly,
	"POST /garden/bead/{id}/adopt":                    routeAdminOnly,
	"POST /garden/gardener/{id}/wake":                 routeAdminOnly,
	"GET /garden/c":                                   routeScoped,
	"GET /garden/c/events":                            routeScoped,
	"GET /garden/c/m/{id}":                            routeScoped,
	"GET /garden/c/bead/{id}":                         routeScoped,
	"GET /garden/c/n/{id}":                            routeScoped,
	"GET /garden/c/s":                                 routeScoped,
	"GET /garden/c/s/{land}":                          routeScoped,
	"GET /garden/c/unread":                            routeScoped,
	"GET /garden/c/{land}/{channel}":                  routeScoped,
	"GET /garden/c/{land}/{channel}/t/{thread}":       routeScoped,
	"GET /garden/c/{land}/{channel}/presence":         routeScoped,
	"GET /garden/c/{land}/{channel}/typing":           routeScoped,
	"POST /garden/c/{land}/{channel}/typing":          routeScoped,
	"GET /garden/c/{land}/{channel}/mentions":         routeScoped,
	"GET /garden/c/f/{land}/{channel}/{file}/{name}":  routeScoped,
	"GET /garden/c/m/{id}/lightbox/{kind}":            routeScoped,
	"POST /garden/c/action/{id}":                      routeScoped,
	"POST /garden/c/submit/{id}":                      routeScoped,
	"POST /garden/c/t/{thread}/reply":                 routeScoped,
	"POST /garden/c/t/{thread}/done":                  routeScoped,
	"POST /garden/c/{land}/{channel}/join":            routeScoped,
	"POST /garden/c/{land}/{channel}/leave":           routeScoped,
	"POST /garden/c/{land}/{channel}/freeze":          routeScoped,
	"POST /garden/c/{land}/{channel}/thaw":            routeScoped,
	"GET /garden/c/{land}/{channel}/members":          routeScoped,
	"POST /garden/c/{land}/{channel}/invite":          routeScoped,
	"POST /garden/c/{land}/{channel}/revoke":          routeScoped,
	"POST /garden/c/{land}/{channel}/read":            routeScoped,
	"POST /garden/c/{land}/{channel}/say":             routeScoped,
	"GET /garden/events":                              routeAdminOnly,
	"GET /garden/farm":                                routeScoped,
	"GET /garden/farm/events":                         routeScoped,
	"POST /garden/farm/events/view":                   routeScoped,
	"GET /garden/live/events":                         routeScoped,
	"POST /garden/live/sub":                           routeScoped,
	"POST /garden/welcome":                            routeScoped,    // the welcome tour was had: stamps only the caller's own user bead, on their own bearer (garden_welcome.go)
	"GET /garden/farm/activity":                       routeAdminOnly, // the transcripts (bird captions spec 11.2): no per-viewer cut yet, so admin-only though /garden/farm is scoped
	"GET /garden/farm/activity/{id}":                  routeAdminOnly,
	"GET /garden/arms.png":                            routeAdminOnly, // its only caller is GET /garden; public would let an applicant drive the renderer and churn armsCache (heraldry 4.1)
	"GET /garden/assets/{file}":                       routePublic,    // the page's scripts and stylesheets, content-hashed (gardenhub/assets.go): embedded code, no viewer data; the Commons page a scoped human opens loads them
	"GET /garden/icon-180.png":                        routePublic,
	"GET /garden/icon-192.png":                        routePublic,
	"GET /garden/icon-512.png":                        routePublic,
	"GET /garden/manifest.webmanifest":                routePublic,
	"GET /garden/state":                               routeAdminOnly,
	"GET /garden/splash":                              routePublic, // the splash (plan gd-EJGA3zDGCk): one document for everyone, reads no bead and no viewer; served without a session (noSessionPatterns, gd-hzRHm1KB5r)
	"GET /garden/splash/three.module.min.js":          routePublic, // the splash's vendored three.js, public with the page
	"GET /garden/splash/still.png":                    routePublic, // the still the splash shows without WebGL, public with the page
	"GET /gardener/{id}":                              routeAdminOnly,
	"GET /gardener/{id}/beads":                        routeAdminOnly,
	"POST /gardener/{id}/cancel-run":                  routeAdminOnly,
	"POST /gardener/{id}/config":                      routeAdminOnly,
	"POST /gardener/{id}/disable":                     routeAdminOnly,
	"GET /gardener/{id}/edit":                         routeAdminOnly,
	"POST /gardener/{id}/enable":                      routeAdminOnly,
	"GET /gardener/{id}/invocations":                  routeAdminOnly,
	"POST /gardener/{id}/mode":                        routeAdminOnly,
	"POST /gardener/{id}/pause-toggle":                routeAdminOnly,
	"POST /gardener/{id}/retire":                      routeAdminOnly,
	"POST /gardener/{id}/run-now":                     routeAdminOnly,
	"GET /gardener/{id}/runs":                         routeAdminOnly,
	"GET /gardener/{id}/successor":                    routeAdminOnly,
	"POST /gardener/{id}/successor":                   routeAdminOnly,
	"GET /gardeners":                                  routeAdminOnly,
	"GET /grafana":                                    routeAdminOnly,
	"/grafana/{path...}":                              routeAdminOnly,
	"GET /health":                                     routeAdminOnly,
	"GET /health/tile/bridges":                        routeAdminOnly,
	"GET /health/tile/build":                          routeAdminOnly,
	"GET /health/tile/costs":                          routeAdminOnly,
	"GET /health/tile/decisions":                      routeAdminOnly,
	"GET /health/tile/escalations":                    routeAdminOnly,
	"GET /health/tile/events":                         routeAdminOnly,
	"GET /health/tile/fleet":                          routeAdminOnly,
	"GET /health/tile/gardeners":                      routeAdminOnly,
	"GET /health/tile/incidents":                      routeAdminOnly,
	"GET /health/tile/open-beads":                     routeAdminOnly,
	"GET /health/tile/operator-signals":               routeAdminOnly,
	"GET /htmx/operator-hover/{slug}":                 routeAdminOnly,
	"GET /htmx/widget-types/{slug}/diagnostics":       routeAdminOnly,
	"GET /htmx/widget-types/{slug}/preview":           routeAdminOnly,
	"POST /htmx/widget-types/{slug}/preview":          routeAdminOnly,
	"POST /htmx/widget/csp-report":                    routeAdminOnly,
	"GET /htmx/widget/{widget}/{instance}/{action}":   routeAdminOnly,
	"POST /htmx/widget/{widget}/{instance}/{action}":  routeAdminOnly,
	"GET /image/{key...}":                             routeAdminOnly,
	"GET /inbox":                                      routeAdminOnly,
	"GET /inbox/orphans/{scope}":                      routeAdminOnly,
	"GET /inbox/{id}":                                 routeAdminOnly,
	"GET /index":                                      routeAdminOnly,
	"GET /lands/{$}":                                  routePublic,
	"GET /lands/accept":                               routeApplicant,
	"POST /lands/accept":                              routeApplicant,
	"GET /lands/aup":                                  routePublic,
	"GET /lands/github/install":                       routeApplicant,
	"GET /lands/github/setup":                         routeApplicant,
	"GET /lands/apply":                                routeApplicant,
	"POST /lands/apply":                               routeApplicant,
	"GET /lands/applications":                         routeApplicant,
	"POST /lands/applications/{id}/withdraw":          routeApplicant,
	"GET /lands/demo":                                 routeApplicant,
	"POST /lands/demo":                                routeApplicant,
	"GET /lands/mine":                                 routeApplicant, // the caller's own lands, from its own token's groups (lands_home.go)
	"GET /lands/privacy":                              routePublic,
	"GET /lands/static/meadow/":                       routePublic,
	"GET /lands/static/homestead/":                    routePublic,
	"GET /lands/terms":                                routePublic,
	"GET /w/{token}":                                  routePublic, // the Window (window.go, PW.3): served without a session (windowPublicPatterns); authorized on the window token alone, never the request's scope
	"GET /w/{token}/plan.json":                        routePublic, // the Window's 20 s refresh, same token
	"GET /w/{token}/live":                             routePublic, // the Window's live stream (PW.7), same token
	"GET /w/static/window.js":                         routePublic, // the Window's one script
	"GET /w/{token}/land.json":                        routePublic, // a land window's 20 s refresh (LW.4), same token
	"GET /w/static/land.js":                           routePublic, // a land window's one script
	"/w/":                                             routePublic, // every other /w/ path: the Window's uniform 404
	"GET /manifest.webmanifest":                       routePublic,
	"GET /maps":                                       routeAdminOnly,
	"GET /me":                                         routeAdminOnly,
	"GET /me/taints":                                  routeAdminOnly,
	"POST /me/taints/scrub":                           routeAdminOnly,
	"GET /molecule/{id}/cost":                         routeAdminOnly,
	"GET /molecule/{id}/events":                       routeAdminOnly,
	"GET /old-viewer":                                 routeAdminOnly,
	"GET /operator-inbox":                             routeAdminOnly,
	"GET /operator-inbox/{rest...}":                   routeAdminOnly,
	"GET /p/{project}":                                routeScoped,
	"POST /p/{project}/bulk/assignee":                 routeAdminOnly,
	"POST /p/{project}/bulk/labels":                   routeAdminOnly,
	"POST /p/{project}/bulk/status":                   routeAdminOnly,
	"POST /p/{project}/config":                        routeAdminOnly,
	"GET /p/{project}/edit":                           routeAdminOnly,
	"GET /peel-catalog":                               routeAdminOnly,
	"POST /peel-catalog/{id}/request":                 routeAdminOnly,
	"GET /people":                                     routeAdminOnly,
	"GET /plans":                                      routeScoped,
	"POST /plans":                                     routeAdminOnly,
	"POST /plans/bulk/abandon":                        routeAdminOnly,
	"POST /plans/bulk/complete":                       routeAdminOnly,
	"POST /plans/bulk/disable":                        routeAdminOnly,
	"POST /plans/bulk/enable":                         routeAdminOnly,
	"POST /plans/master-switch/toggle":                routeAdminOnly,
	"GET /plans/{id}":                                 routeScoped,
	"POST /plans/{id}/abandon":                        routeAdminOnly,
	"GET /plans/{id}/agent":                           routeScoped,
	"POST /plans/{id}/assistant/prompt":               routeAdminOnly,
	"POST /plans/{id}/build":                          routeAdminOnly,
	"GET /plans/{id}/card":                            routeScoped,
	"GET /plans/{id}/comments":                        routeScoped,
	"POST /plans/{id}/comments":                       routeAdminOnly,
	"POST /plans/{id}/complete":                       routeAdminOnly,
	"POST /plans/{id}/decisions/{decision}/dismiss":   routeAdminOnly,
	"POST /plans/{id}/decisions/{decision}/resolve":   routeAdminOnly,
	"POST /plans/{id}/dispatch-next":                  routeAdminOnly,
	"POST /plans/{id}/docs/{doc}":                     routeAdminOnly,
	"GET /plans/{id}/events":                          routeScoped,
	"POST /plans/{id}/gardener/cadence":               routeAdminOnly,
	"POST /plans/{id}/gardener/enable":                routeAdminOnly,
	"POST /plans/{id}/gardener/pause-toggle":          routeAdminOnly,
	"POST /plans/{id}/gardener/toggle":                routeAdminOnly,
	"POST /plans/{id}/gardener/wake":                  routeAdminOnly,
	"POST /plans/{id}/mark":                           routeAdminOnly,
	"GET /plans/{id}/narrative":                       routeScoped,
	"POST /plans/{id}/narrative":                      routeAdminOnly,
	"GET /plans/{id}/peek":                            routeScoped,
	"POST /plans/{id}/phases":                         routeAdminOnly,
	"POST /plans/{id}/phases/{phase}/reorder":         routeAdminOnly,
	"POST /plans/{id}/pours":                          routeAdminOnly,
	"GET /plans/{id}/pours/new":                       routeAdminOnly,
	"POST /plans/{id}/reopen":                         routeAdminOnly,
	"POST /plans/{id}/state":                          routeAdminOnly,
	"POST /plans/{id}/steps":                          routeAdminOnly,
	"POST /plans/{id}/steps/{step}/close":             routeAdminOnly,
	"POST /plans/{id}/steps/{step}/dispatch":          routeAdminOnly,
	"POST /plans/{id}/steps/{step}/reopen":            routeAdminOnly,
	"POST /plans/{id}/steps/{step}/reorder":           routeAdminOnly,
	"POST /plans/{id}/steps/{step}/title":             routeAdminOnly,
	"GET /pool":                                       routeAdminOnly,
	"GET /quick-spawn":                                routeAdminOnly,
	"POST /quick-spawn":                               routeAdminOnly,
	"GET /quick-spawn-inline":                         routeAdminOnly,
	"GET /ratings":                                    routeAdminOnly,
	"GET /role/{role}":                                routeAdminOnly,
	"GET /roles":                                      routeAdminOnly,
	"GET /schedules":                                  routeAdminOnly,
	"GET /search":                                     routeScoped,
	"GET /settings":                                   routeAdminOnly,
	"POST /settings":                                  routeAdminOnly,
	"POST /settings/skin":                             routeAdminOnly,
	"GET /settings/edit":                              routeAdminOnly,
	"GET /settings/view":                              routeAdminOnly,
	"GET /taints":                                     routeAdminOnly,
	"POST /taints":                                    routeAdminOnly,
	"GET /taints/new":                                 routeAdminOnly,
	"GET /taints/{name}":                              routeAdminOnly,
	"POST /taints/{name}/grants":                      routeAdminOnly,
	"POST /taints/{name}/grants/revoke":               routeAdminOnly,
	"POST /taints/{name}/policy":                      routeAdminOnly,
	"POST /taints/{name}/scrub-requests/{id}/approve": routeAdminOnly,
	"POST /taints/{name}/scrub-requests/{id}/reject":  routeAdminOnly,
	"GET /teams/{team}":                               routeAdminOnly,
	"GET /releases":                                   routeScoped,
	"GET /release/{id}":                               routeScoped,
	"POST /teams/{team}/members":                      routeAdminOnly,
	"GET /telepathy":                                  routeAdminOnly,
	"GET /telepathy/events":                           routeAdminOnly,
	"GET /threads":                                    routeAdminOnly,
	"POST /threads/compose":                           routeAdminOnly,
	"GET /threads/{id}":                               routeAdminOnly,
	"GET /threads/{id}/audit":                         routeAdminOnly,
	"GET /threads/{id}/timeline":                      routeAdminOnly,
	"GET /tool-latency":                               routeAdminOnly,
	"GET /triggers":                                   routeAdminOnly,
	"POST /triggers/bulk/disable":                     routeAdminOnly,
	"POST /triggers/bulk/enable":                      routeAdminOnly,
	"POST /triggers/{id}/toggle":                      routeAdminOnly,
	"GET /trust":                                      routeAdminOnly,
	"GET /u/{slug}":                                   routeAdminOnly,
	"GET /u/{slug}/inbox":                             routeAdminOnly,
	"GET /u/{slug}/section/forwarded":                 routeAdminOnly,
	"GET /u/{slug}/section/inbox":                     routeAdminOnly,
	"GET /u/{slug}/section/recent-comments":           routeAdminOnly,
	"GET /u/{slug}/status-message":                    routeAdminOnly,
	"POST /u/{slug}/status-message":                   routeAdminOnly,
	"GET /u/{slug}/status-message/edit":               routeAdminOnly,
	"GET /ui/{page}":                                  routeAdminOnly,
	"GET /up":                                         routeAdminOnly,
	"GET /up/{id}":                                    routeAdminOnly,
	"GET /views":                                      routeAdminOnly,
	"POST /views":                                     routeAdminOnly,
	"GET /views/{id}":                                 routeAdminOnly,
	"POST /views/{id}/delete":                         routeAdminOnly,
	"GET /widget-types/{$}":                           routeAdminOnly,
	"GET /widget-types/{slug}":                        routeAdminOnly,
	"GET /widgets/{slug}/{bead}":                      routeAdminOnly,
	"GET /{$}":                                        routeScoped,
	"GET /{id}":                                       routeScoped,

	// The machine routes: only a machine principal machineMiddleware admitted.
	"GET /internal/commons/f/{land}/{channel}/{file}/{name}": routeMachine,

	// The plan cockpit's Share panel (Lands: the Window, PW.4, plan_share.go):
	// every daemon call runs on the person's own bearer alone, and kbeads'
	// windowManageDenial admits only the owner of the plan's land or an
	// operator, so a scoped owner reaches it and a member is refused there.
	"GET /plans/{id}/share":                  routeScoped,
	"GET /plans/{id}/share/preview":          routeScoped,
	"POST /plans/{id}/share":                 routeScoped,
	"POST /plans/{id}/share/{window}/revoke": routeScoped,
	// The land's Share tab (Lands: the land window, LW.5, land_share.go), the
	// same gate on a whole land.
	"GET /p/{project}/share":                  routeScoped,
	"GET /p/{project}/share/preview":          routeScoped,
	"POST /p/{project}/share":                 routeScoped,
	"POST /p/{project}/share/{window}/revoke": routeScoped,
}

// viewerProjectParams names the path segment that carries a project for the
// scoped routes that are per-project pages. A scoped principal reaches such a
// route only for a project in its own scope: the daemon would return nothing
// for another one, but the project name itself (a typo-squat, an enumeration
// probe) is not the viewer's to echo back.
var viewerProjectParams = map[string]string{
	"GET /p/{project}":                               "project",
	"GET /p/{project}/share":                         "project",
	"GET /p/{project}/share/preview":                 "project",
	"POST /p/{project}/share":                        "project",
	"POST /p/{project}/share/{window}/revoke":        "project",
	"GET /b/project/{name}/activity":                 "name",
	"GET /b/project/{name}/agents":                   "name",
	"GET /b/project/{name}/decisions":                "name",
	"GET /garden/c/s/{land}":                         "land",
	"GET /garden/c/{land}/{channel}":                 "land",
	"GET /garden/c/{land}/{channel}/t/{thread}":      "land",
	"GET /garden/c/{land}/{channel}/presence":        "land",
	"GET /garden/c/{land}/{channel}/typing":          "land",
	"POST /garden/c/{land}/{channel}/typing":         "land",
	"GET /garden/c/{land}/{channel}/mentions":        "land",
	"GET /garden/c/f/{land}/{channel}/{file}/{name}": "land",
	"POST /garden/c/{land}/{channel}/join":           "land",
	"POST /garden/c/{land}/{channel}/leave":          "land",
	"POST /garden/c/{land}/{channel}/freeze":         "land",
	"POST /garden/c/{land}/{channel}/thaw":           "land",
	"GET /garden/c/{land}/{channel}/members":         "land",
	"POST /garden/c/{land}/{channel}/invite":         "land",
	"POST /garden/c/{land}/{channel}/revoke":         "land",
	"POST /garden/c/{land}/{channel}/read":           "land",
	"POST /garden/c/{land}/{channel}/say":            "land",
}

// scopedRouteAllowed reports whether a scoped principal may reach the route that
// pattern names for the given request path.
func scopedRouteAllowed(sc viewerScope, pattern, path string) bool {
	switch viewerRouteClasses[pattern] {
	case routePublic, routeApplicant:
		return true
	case routeScoped:
		if sc.Applicant {
			return false
		}
		if param, ok := viewerProjectParams[pattern]; ok {
			project, found := patternPathValue(pattern, path, param)
			return found && sc.allows(project)
		}
		return true
	default:
		return false
	}
}

// patternPathValue extracts the value of the named {wildcard} from path by
// aligning it with pattern's segments (method prefix ignored). It exists
// because the mux fills in r.PathValue only on the request it dispatches.
func patternPathValue(pattern, path, name string) (string, bool) {
	if _, p, ok := strings.Cut(pattern, " "); ok {
		pattern = p
	}
	ps := strings.Split(strings.Trim(pattern, "/"), "/")
	vs := strings.Split(strings.Trim(path, "/"), "/")
	if len(ps) != len(vs) {
		return "", false
	}
	for i, seg := range ps {
		if seg == "{"+name+"}" {
			return vs[i], vs[i] != ""
		}
	}
	return "", false
}
