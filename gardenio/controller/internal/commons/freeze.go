package commons

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// The Lands kill switch in the Commons (Track B spec §5.5, step 10.5). A
// frozen land (land_frozen) takes no new say, reply, join or leave from its
// members: the composer gives way to a banner, and a write that arrives anyway
// is refused with the banner's words. The operator (an Admin scope) still
// posts, so they can say why, and gets the Freeze and Thaw controls on a
// tenant land's channel header. kbeads refuses a scoped principal's create in
// a frozen land as well, so this is the fragment's half, not the fence.
//
// The land's #support stays open to its members' says and replies: the Terms
// (§9.1) promise a frozen land's members can still talk to us there
// (gd-mJw9pxK2wu). #support is a user channel and nobody can join during a
// freeze, so there the land-role action table alone decides, as kbeads does
// (frozenSupportPost).

// LandSwitch is the land kill switch as the Commons sees it. beads-viewer
// wires it to internal/lands/freeze.
type LandSwitch struct {
	// State reports whether land is a tenant land and whether it is frozen.
	// Nil, or a land it cannot read, is neither: the Commons fails open here
	// because kbeads refuses the write itself.
	State func(ctx context.Context, land string) (tenant, frozen bool)
	// Set freezes (frozen, with reason) or thaws land on the viewer's own
	// bearer. Nil refuses every freeze and thaw.
	Set func(r *http.Request, land string, frozen bool, reason string) error
}

// FrozenNote is the banner a member of a frozen land sees in place of the
// composer, and the refusal of a write that comes anyway. It does not give
// the operator's reason.
const FrozenNote = "This land is frozen. Its agents are paused, and nothing new can be posted here until the operator thaws it."

// FrozenOperatorNote is the banner over the operator's composer in a frozen
// land.
const FrozenOperatorNote = "This land is frozen: its members cannot post here. You still can, as the operator."

// FrozenSupportNote is the banner over a member's composer in a frozen land's
// #support.
const FrozenSupportNote = "This land is frozen. Its agents are paused and nothing new can be posted in it until the operator thaws it, except here: you can still write to us in #support."

// FrozenSupportOperatorNote is the banner over the operator's composer in a
// frozen land's #support.
const FrozenSupportOperatorNote = "This land is frozen: #support is the one channel its members can still post in."

// supportChannel is the channel a tenant land's people reach the operator in
// (the Lands provisioner seeds it, Track B spec §6.1).
const supportChannel = "support"

// reasonMax caps a freeze reason.
const reasonMax = 500

// landState is the land's kill switch, read through Lands.
func (h *Handler) landState(ctx context.Context, land string) (tenant, frozen bool) {
	if h.Lands.State == nil {
		return false, false
	}
	return h.Lands.State(ctx, land)
}

// frozenFor reports whether the viewer is held by a freeze in land: the land
// is frozen and the viewer is not the operator.
func (h *Handler) frozenFor(r *http.Request, land string) bool {
	if h.scope(r).Admin {
		return false
	}
	_, frozen := h.landState(r.Context(), land)
	return frozen
}

// refuseFrozen answers a write held by a freeze, reporting whether it did.
func (h *Handler) refuseFrozen(w http.ResponseWriter, r *http.Request, ch Channel) bool {
	if !h.frozenFor(r, ch.Land) {
		return false
	}
	http.Error(w, FrozenNote, http.StatusForbidden)
	return true
}

// mayPost reports whether the viewer may say or reply (action) in ch,
// answering a refusal itself: a freeze refuses with the banner's words, except
// in the land's #support.
func (h *Handler) mayPost(w http.ResponseWriter, r *http.Request, ch Channel, action string) bool {
	switch {
	case !h.frozenFor(r, ch.Land):
		if h.may(r, ch, action) {
			return true
		}
		h.refuse(w, ch, action)
	case ch.Name != supportChannel:
		http.Error(w, FrozenNote, http.StatusForbidden)
	case h.mayFrozenSupport(r, ch, action):
		return true
	default:
		http.Error(w, "You may not post in #"+ch.Name+".", http.StatusForbidden)
	}
	return false
}

// mayFrozenSupport reports whether the viewer may say or reply (action) in a
// frozen land's #support: the land-role action table alone, joined or not.
func (h *Handler) mayFrozenSupport(r *http.Request, ch Channel, action string) bool {
	return h.Gate.MayAct != nil && h.self(r) != "" && h.Gate.MayAct(r, ch.Land, action)
}

// switchAccess fills a's freeze fields for ch and action.
func (h *Handler) switchAccess(r *http.Request, ch Channel, action string, a *access) {
	_, frozen := h.landState(r.Context(), ch.Land)
	a.Frozen = frozen
	support := ch.Name == supportChannel
	if !h.scope(r).Admin {
		if frozen {
			a.Post, a.Join = false, false
			if support {
				a.Post, a.FrozenNote = h.mayFrozenSupport(r, ch, action), FrozenSupportNote
			}
		}
		return
	}
	a.FrozenNote = FrozenOperatorNote
	if support {
		a.FrozenNote = FrozenSupportOperatorNote
	}
}

// freezeLand serves POST /garden/c/{land}/{channel}/freeze and thawLand
// .../thaw: the operator freezes or thaws a tenant land. It answers 204 No
// Content.
func (h *Handler) freezeLand(w http.ResponseWriter, r *http.Request) { h.setSwitch(w, r, true) }
func (h *Handler) thawLand(w http.ResponseWriter, r *http.Request)   { h.setSwitch(w, r, false) }

func (h *Handler) setSwitch(w http.ResponseWriter, r *http.Request, frozen bool) {
	if !h.ready(w) {
		return
	}
	if !h.scope(r).Admin || h.Lands.Set == nil || h.self(r) == "" {
		http.Error(w, "Only the operator freezes or thaws a land.", http.StatusForbidden)
		return
	}
	if crossOrigin(r) {
		http.Error(w, "A freeze or thaw must come from this page.", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), writeTimeout)
	defer cancel()
	ch, err := readChannel(ctx, h.Beads, h.scope(r), r.PathValue("land"), r.PathValue("channel"))
	if err != nil {
		h.fail(w, err)
		return
	}
	reason := ""
	if frozen {
		r.Body = http.MaxBytesReader(w, r.Body, formMaxBytes)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad form.", http.StatusBadRequest)
			return
		}
		reason = strings.TrimSpace(r.PostForm.Get("reason"))
		switch {
		case reason == "":
			http.Error(w, "Say why the land is frozen first.", http.StatusBadRequest)
			return
		case len(reason) > reasonMax || !utf8.ValidString(reason):
			http.Error(w, "That reason is too long (500 characters at most).", http.StatusRequestEntityTooLarge)
			return
		}
	}
	if tenant, _ := h.landState(ctx, ch.Land); !tenant {
		http.Error(w, "Only a tenant land can be frozen.", http.StatusConflict)
		return
	}
	if err := h.Lands.Set(r.WithContext(ctx), ch.Land, frozen, reason); err != nil {
		h.writeFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// crossOrigin reports whether a browser says r came from another origin. The
// edge session cookie is SameSite, so a cross-site form should not arrive
// signed in; the kill switch refuses one that does anyway. A browser that
// sends Sec-Fetch-Site must say same-origin (the Commons posts with fetch
// from its own page); one that only sends Origin must name this host. A
// request with neither is not a browser's (gb, tests) and is not refused here.
func crossOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site != "same-origin"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return true
	}
	host := r.Host
	if fwd := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); fwd != "" {
		host = fwd
	}
	return !strings.EqualFold(u.Host, host)
}
