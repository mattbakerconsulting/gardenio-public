// Package commons is the Commons read model: the channels, threads and
// messages of a land, read from beads and served by beads-viewer under
// /garden/c as server-rendered fragments, plus an id-only invalidation stream
// (design D8: an event names what changed, the client refetches its fragment).
//
// Every read goes to the daemon on the request context, which carries the
// signed-in viewer's own bearer, so kbeads cuts it to the viewer's projects.
// The handlers cut again by land (Scope) before rendering, and the event stream
// is cut per subscriber before anything leaves the server (spec section 6.2
// rule 3). Nothing here is a shared snapshot.
//
// A message is in the channel its channel_id names; a reply with no channel_id
// is in its thread root's channel (replyInRootChannel). A message bead with
// neither (a Slack-mirror record, a gardener turn post) is in no channel: no
// unread count adds it, #general included.
//
// Design: gardenio/docs/superpowers/specs/2026-10-19-lands-commons-design.md
// (sections 2, 3.3, 6.2, 6.3; Commons I step 8). The pane that shows these
// fragments inside /garden is step 9.
package commons

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/decisionview"
	"gardenio/controller/internal/servertiming"
	"gardenio/controller/internal/spawnguard"
)

// Reader is the slice of the daemon client the Commons reads through.
// *beadsapi.Client satisfies it.
type Reader interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	ListGlobalEvents(ctx context.Context, req beadsapi.ListGlobalEventsRequest) (*beadsapi.ListGlobalEventsResponse, error)
}

// Scope is what the viewer may read: every land when Admin, else only Lands.
// beads-viewer builds it from its viewerScope (the same verified groups kbeads
// reads), so the two agree on who is confined.
type Scope struct {
	Admin bool
	Lands []string
	// Principal is the viewer as conversation beads name an author
	// (oidc:<email>), or "" when unknown; unread counts leave out its words.
	Principal string
}

// Allows reports whether the scope admits the land. The empty land (a bead with
// no single project label) is never admitted: nothing says whose it is.
func (s Scope) Allows(land string) bool {
	if land == "" {
		return false
	}
	return s.Admin || slices.Contains(s.Lands, land)
}

// Registrar is the slice of *http.ServeMux that Register uses.
type Registrar interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Handler serves the Commons read routes.
type Handler struct {
	// Beads reads on the request context (the viewer's bearer). Nil answers 503.
	Beads Reader
	// Nodes reads a node's children and edges in batches, on the request
	// context like Beads (node.go). Nil answers 503 on the node route.
	Nodes Nodes
	// Jobs is each agent's jobs record (jobs.go): a node's job counts and an
	// agent's Jobs rail. Nil shows no jobs.
	Jobs *Jobs
	// Stewards reads a land's steward for its node (stewardnode.go). Nil
	// answers 503 on /garden/c/s, and a steward trigger's node is a bead's.
	Stewards     Stewards
	stewardReads stewardCache
	// sends answers a repeated send's nonce (sendonce.go).
	sends sendOnce
	// Evidence caches a decision card's evidence images, per caller
	// (decisionview.EvidenceCache, gd-Pi7C83262N). Nil reads them on every
	// render.
	Evidence *decisionview.EvidenceCache
	// Hub fans live invalidations out per subscriber. Nil serves the replay and
	// heartbeats only.
	Hub      *Hub
	BasePath string
	// Cursors keeps the viewer's read positions on its own user bead (the
	// viewer's bearer, on the request context). Nil counts every channel from
	// the start and answers 503 to marking one read.
	Cursors Cursors
	// Scope returns the viewer's scope. Nil admits no land (fail closed).
	Scope func(*http.Request) Scope
	// StreamContext bounds an event stream, e.g. to the expiry of the bearer it
	// opened on, so the browser reconnects with a fresh token and a fresh scope.
	// Nil leaves the stream bounded by the request only.
	StreamContext func(context.Context) (context.Context, context.CancelFunc)
	// Presence is who is in each land and channel (Commons III). Nil records
	// and shows nobody.
	Presence *Presence
	// Viewer returns the signed-in viewer's principal (oidc:<email>), "" for
	// none. Nil leaves every stream out of Presence.
	Viewer func(*http.Request) string
	// Now is the clock; nil means time.Now (tests inject one).
	Now func() time.Time

	// Writes is what the write routes (write.go) create messages and change a
	// channel's members through, on the request context like every read. Nil
	// answers 503 on them.
	Writes Writer
	// Gate decides what the viewer may write. Nil, or a nil func in it, denies.
	Gate Gate
	// Inbound carries card buttons and lightbox forms to the bridge
	// (card_actions.go). Nil answers 501 on them.
	Inbound Inbound
	// React carries a wish head's Done to the bridge (done.go). Nil offers
	// no Done and answers 501 on its route.
	React Reactor
	// Compose carries a message that mentions @gardenio or an agent to the
	// bridge (mention.go). Nil posts the words and starts no agent, and the
	// @ list offers nobody.
	Compose Composer
	// People lists a land's people for the @ list and for the notifications
	// a mention of a person writes (mention.go), on the viewer's bearer. Nil
	// offers no people and notifies nobody.
	People People
	// Escalations approves and rejects an escalation from its card, on the
	// viewer's bearer. Nil answers 501 on Approve and Reject.
	Escalations Escalations
	// Approvals answers can-approve for the viewer (escalation-routing D9), so
	// an escalation card shows Approve and Reject only when the daemon would
	// accept them. Nil shows no buttons.
	Approvals Approvals
	// Rate caps how many messages one member posts in one land per window
	// (Lands Track B §5.4, step 10.4): a say or reply past it is answered 429
	// before its body is read. beads-viewer builds it from
	// COMMONS_MESSAGES_PER_MINUTE. Nil admits every write.
	Rate *spawnguard.Keyed
	// Lands is the Lands kill switch (freeze.go, Track B §5.5): a frozen
	// land's members get a banner instead of the composer, and the operator
	// gets Freeze and Thaw on a tenant land. The zero value freezes nothing
	// and offers no control.
	Lands LandSwitch
	// Invites are a tenant land's member invites (invites.go, Track B §3.7):
	// its owner and the operator get a People control on its channel
	// headers. The zero value offers none.
	Invites LandInvites
	// Files keeps the files people attach to messages (files.go). Nil offers
	// no attaching, refuses an upload, and answers 503 on the file route.
	Files FileStore
	// Machine reports whether the request is from a machine principal the
	// viewer admitted to the machine file route (beads-viewer's routeMachine)
	// that may read the files of the land given: a service account any land,
	// an agent its own. Nil refuses every caller there.
	Machine func(r *http.Request, land string) bool
	// EmptyPrompt is the placeholder of an empty composer: the house voice,
	// gardenhub.EmptyPrompt, passed in by beads-viewer (gd-5e6xgIOH6B).
	EmptyPrompt string
	// Reason says why agent bead id is not simply working, as the farm does
	// (gardenhub FarmReason: kind "down", "parked", "sleeping", "stale" or
	// "starting", and its words), or "" for a working gardener or one the farm
	// does not draw. An agent's node head carries it, and the workbench offers
	// Wake or Restart from it (gd-7DyHdnx8fC). It performs no I/O; beads-viewer
	// passes the farm's. Nil carries none.
	Reason func(id string) (kind, text string)
	// Page serves the /garden page to a browser that navigates to a channel or
	// thread URL, so the link opens the game with the Commons pane there (spec
	// section 8). It reports false to decline, and the URL then answers with
	// its fragment. Nil always answers with the fragment.
	Page func(http.ResponseWriter, *http.Request) bool
}

// Register mounts the Commons read routes on mux. Every pattern is classified
// in beads-viewer's viewerRouteClasses (route_classes.go). Each but the event
// stream is timed (Server-Timing and the route histograms, internal/servertiming).
func (h *Handler) Register(mux Registrar) {
	mux = servertiming.Timed(mux)
	mux.HandleFunc("GET /garden/c", h.channels)
	mux.HandleFunc("GET /garden/c/events", h.events)
	mux.HandleFunc("GET /garden/c/unread", h.unread)
	mux.HandleFunc("GET /garden/c/m/{id}", h.message)
	mux.HandleFunc("GET /garden/c/bead/{id}", h.place)
	mux.HandleFunc("GET /garden/c/n/{id}", h.node)
	mux.HandleFunc("GET /garden/c/s", h.steward)
	mux.HandleFunc("GET /garden/c/s/{land}", h.steward)
	mux.HandleFunc("GET /garden/c/{land}/{channel}", h.timeline)
	mux.HandleFunc("GET /garden/c/{land}/{channel}/t/{thread}", h.thread)
	mux.HandleFunc("POST /garden/c/{land}/{channel}/say", h.say)
	mux.HandleFunc("POST /garden/c/t/{thread}/reply", h.reply)
	mux.HandleFunc("POST /garden/c/t/{thread}/done", h.done)
	mux.HandleFunc("POST /garden/c/{land}/{channel}/join", h.join)
	mux.HandleFunc("POST /garden/c/{land}/{channel}/leave", h.leave)
	mux.HandleFunc("POST /garden/c/{land}/{channel}/freeze", h.freezeLand)
	mux.HandleFunc("POST /garden/c/{land}/{channel}/thaw", h.thawLand)
	mux.HandleFunc("GET /garden/c/{land}/{channel}/presence", h.presence)
	mux.HandleFunc("GET /garden/c/{land}/{channel}/members", h.people)
	mux.HandleFunc("POST /garden/c/{land}/{channel}/invite", h.invite)
	mux.HandleFunc("POST /garden/c/{land}/{channel}/revoke", h.revokeInvite)
	mux.HandleFunc("POST /garden/c/{land}/{channel}/read", h.markRead)
	mux.HandleFunc("GET /garden/c/{land}/{channel}/typing", h.typingLine)
	mux.HandleFunc("GET /garden/c/{land}/{channel}/mentions", h.mentions)
	mux.HandleFunc("POST /garden/c/{land}/{channel}/typing", h.typingSignal)
	mux.HandleFunc("GET /garden/c/m/{id}/lightbox/{kind}", h.lightbox)
	mux.HandleFunc("POST /garden/c/action/{id}", h.cardAction)
	mux.HandleFunc("POST /garden/c/submit/{id}", h.cardSubmit)
	mux.HandleFunc("GET /garden/c/f/{land}/{channel}/{file}/{name}", h.file)
	mux.HandleFunc("GET /internal/commons/f/{land}/{channel}/{file}/{name}", h.machineFile)
}

func (h *Handler) scope(r *http.Request) Scope {
	if h.Scope == nil {
		return Scope{}
	}
	return h.Scope(r)
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Conversation bead fields and values (spec sections 2.2 and 2.3). They match
// internal/conversation's; this package keeps its own copies so the viewer's
// read model does not depend on the bridge-side router.
const (
	fieldChannelID = "channel_id"
	fieldThreadID  = "thread_id"
	fieldAuthor    = "author"
	fieldKind      = "kind"
	fieldSubjectID = "subject_id"
	fieldText      = "text"
	fieldFiles     = "files" // attachments as links (conversation.BeadFiles)
	fieldDeleted   = "deleted"
	fieldRootText  = "slack_root_text" // a Slack-origin wish head's opening message
	fieldName      = "name"
	fieldChanKind  = "channel_kind"
	fieldMirror    = "mirror"
	fieldMembers   = "members"
	fieldOrigin    = "origin"
	// fieldAbout is the bead a message asks about: the workbench's "Ask the
	// steward about this" sends it (steward view S2, gd-LisW87zrnf).
	fieldAbout = "about"
	// fieldCreatedImpersonatedBy is the admin behind the impersonated session
	// that created the bead (kbeads impersonation.go). Not impersonated_by:
	// that is the last impersonated write, which a close or a reaction on a
	// bead its author wrote themselves also sets.
	fieldCreatedImpersonatedBy = "created_impersonated_by"
	// A chat root a mention made a wish (internal/bridge/inbound_compose.go):
	// the head names the root, and the root names the head. Replies keep their
	// thread_id, so a promoted thread is every message under either.
	fieldPromotedFrom = "promoted_from"
	fieldPromotedTo   = "promoted_to"
	// A wish head's Slack thread, copied onto a reply so the bridge's thread
	// forward posts it there (internal/bridge/thread_message_forward.go).
	fieldSlackChannel = "slack_thread_channel"
	fieldSlackTS      = "slack_thread_ts"

	typeChannel = "channel"
	typeMessage = "message"
	labelThread = "thread" // the wish-head label (internal/bridge/thread_bead.go)

	kindCard = "card"

	// originCommons marks a message a person wrote here (spec 2.3, the mirror
	// loop guard); the bridge's inbound sweep keys on it.
	originCommons = "commons"
	// LabelThreadSay is the label the bridge's thread forward relays into the
	// wish head's Slack thread. It must equal bridge.ThreadSayLabel, which
	// beads-viewer's tests pin (this package does not import the bridge).
	LabelThreadSay = "thread-say"
)

// landOf returns the one project:<land> label of a bead, or "" when it has none
// or more than one. Every conversation bead carries exactly one (spec 6.2 rule
// 1); one that does not is not rendered anywhere, because its land is unknown.
func landOf(labels []string) string {
	land := ""
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" {
			if land != "" && land != v {
				return ""
			}
			land = v
		}
	}
	return land
}
