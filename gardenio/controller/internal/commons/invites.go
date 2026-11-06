package commons

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Member invites in the Commons (Track B spec §3.7, step 8.9). A tenant
// land's owner, and the operator, get a People control on the land's channel
// headers. It opens a lightbox (D12) listing the land's invites with a Revoke
// on each, above a form that invites a GitHub login as a member or a guest.
// The person joins when they next sign in with that GitHub account: the Lands
// provisioner matches their github_id and adds them (internal/lands/provision
// invites.go). Revoking takes a joined person out on their next token.
//
// The routes are owner-only (member_manage, conversation.Allows), checked
// here; beads-viewer resolves the login on GitHub and writes the land_invite
// bead as its machine principal, because kbeads gives no land role that write
// (the github_id must be the one GitHub answered, never one a caller typed).

// ActionMemberManage is the action-table entry the routes need
// (conversation.ActionMemberManage).
const ActionMemberManage = "member_manage"

// LandInvites is a land's member invites as the Commons sees them.
// beads-viewer wires it; the zero value offers no People control.
type LandInvites struct {
	// List returns the land's open invites, pending and joined, newest first.
	List func(ctx context.Context, land string) ([]Invite, error)
	// Invite invites login to land with role ("member" or "guest") on behalf
	// of the viewer. An *InviteRefusal is shown to the owner in the lightbox.
	Invite func(r *http.Request, land, login, role string) (Invite, error)
	// Revoke asks for invite id in land to be revoked. An *InviteRefusal is
	// shown in the lightbox.
	Revoke func(r *http.Request, land, id string) (Invite, error)
}

// Invite is one land_invite.
type Invite struct {
	ID        string
	Login     string
	Role      string // member or guest
	State     string // pending or joined
	ExpiresAt time.Time
	// Revoking: the owner asked; the provisioner has not acted yet.
	Revoking bool
}

// InviteRefusal is an invite or revoke the owner can fix: the words go in
// the lightbox, not a toast.
type InviteRefusal struct{ Message string }

func (e *InviteRefusal) Error() string { return e.Message }

// inviteLogin is a GitHub login (kbeads land_invite.go githubLoginPattern),
// checked before anything reaches GitHub.
var inviteLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

// peopleView is the members lightbox.
type peopleView struct {
	Base, Land, Members string
	// People are the land's members (People.LandMembers, oidc:<email>), each row carrying a Become button that
	// web/become.js shows to an admin only (plan gd-0SUIdKXWV3 step 3). NoPeople: the daemon did not answer.
	People   []personRow
	NoPeople bool
	Invites  []Invite
	// Note says what just happened; Error what the owner must fix. Login and
	// Role keep the form's values after an error.
	Note, Error, Login, Role string
}

// personRow is one member in the lightbox; Me marks the viewer's own row,
// which never carries Become.
type personRow struct {
	beadsapi.LandMember
	Me bool
}

// mayManage reports whether the viewer gets the People control on ch: a
// tenant land, invites wired, and the owner's (or operator's) member_manage.
func (h *Handler) mayManage(r *http.Request, ch Channel) bool {
	if h.Invites.List == nil || h.Invites.Invite == nil || h.Gate.MayAct == nil || h.self(r) == "" {
		return false
	}
	if tenant, _ := h.landState(r.Context(), ch.Land); !tenant {
		return false
	}
	return h.Gate.MayAct(r, ch.Land, ActionMemberManage)
}

// people serves GET /garden/c/{land}/{channel}/members: the lightbox.
func (h *Handler) people(w http.ResponseWriter, r *http.Request) {
	ch, ok := h.peopleChannel(w, r, readTimeout)
	if !ok {
		return
	}
	h.peopleOf(w, r, ch, peopleView{Role: "member"})
}

// invite serves POST /garden/c/{land}/{channel}/invite (login, role). It
// answers with the lightbox, saying what happened.
func (h *Handler) invite(w http.ResponseWriter, r *http.Request) {
	ch, ok := h.peopleChannel(w, r, writeTimeout)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, formMaxBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return
	}
	login := strings.TrimPrefix(strings.TrimSpace(r.PostForm.Get("login")), "@")
	role := r.PostForm.Get("role")
	v := peopleView{Login: login, Role: role}
	switch {
	case role != "member" && role != "guest":
		v.Role, v.Error = "member", "Pick member or guest."
	case !inviteLogin.MatchString(login):
		v.Error = "That is not a GitHub login: letters, digits and single hyphens, at most 39."
	default:
		inv, err := h.Invites.Invite(r, ch.Land, login, role)
		if !h.refused(w, err, &v) {
			return
		}
		if err == nil {
			v = peopleView{Role: "member", Note: "Invited @" + inv.Login + " as a " + inv.Role + ". They join when they next sign in with GitHub."}
		}
	}
	h.peopleOf(w, r, ch, v)
}

// revokeInvite serves POST /garden/c/{land}/{channel}/revoke (id).
func (h *Handler) revokeInvite(w http.ResponseWriter, r *http.Request) {
	ch, ok := h.peopleChannel(w, r, writeTimeout)
	if !ok {
		return
	}
	if h.Invites.Revoke == nil {
		http.Error(w, "Revoking is not switched on here.", http.StatusNotImplemented)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, formMaxBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.PostForm.Get("id"))
	v := peopleView{Role: "member"}
	if id == "" || len(id) > 200 {
		v.Error = "Pick an invite to revoke."
	} else {
		inv, err := h.Invites.Revoke(r, ch.Land, id)
		if !h.refused(w, err, &v) {
			return
		}
		if err == nil {
			v.Note = "Revoking @" + inv.Login + ". "
			if inv.State == "joined" {
				v.Note += "They lose " + ch.Land + " within five minutes."
			} else {
				v.Note += "The invite will not let them in."
			}
		}
	}
	h.peopleOf(w, r, ch, v)
}

// refused puts an *InviteRefusal into v and reports true; any other error is
// answered here (false), nil reports true.
func (h *Handler) refused(w http.ResponseWriter, err error, v *peopleView) bool {
	var ref *InviteRefusal
	switch {
	case err == nil:
		return true
	case errors.As(err, &ref):
		v.Error = ref.Message
		return true
	}
	h.writeFail(w, err)
	return false
}

// peopleChannel reads the channel a People route names and checks the viewer
// may manage its land's people. It answers the request itself when not.
func (h *Handler) peopleChannel(w http.ResponseWriter, r *http.Request, timeout time.Duration) (Channel, bool) {
	if !h.ready(w) {
		return Channel{}, false
	}
	if r.Method == http.MethodPost && crossOrigin(r) {
		http.Error(w, "An invite must come from this page.", http.StatusForbidden)
		return Channel{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	ch, err := readChannel(ctx, h.Beads, h.scope(r), r.PathValue("land"), r.PathValue("channel"))
	if err != nil {
		h.fail(w, err)
		return Channel{}, false
	}
	if !h.mayManage(r, ch) {
		http.Error(w, "Only the land's owner invites people.", http.StatusForbidden)
		return Channel{}, false
	}
	return ch, true
}

// peopleOf renders the lightbox for ch with v's note or error.
func (h *Handler) peopleOf(w http.ResponseWriter, r *http.Request, ch Channel, v peopleView) {
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	invites, err := h.Invites.List(ctx, ch.Land)
	if err != nil {
		h.fail(w, err)
		return
	}
	v.Base, v.Land, v.Invites = h.BasePath, ch.Land, invites
	if h.People != nil {
		people, err := h.People.LandMembers(ctx, ch.Land)
		v.NoPeople = err != nil // the invites still show
		self := strings.ToLower(h.self(r))
		for _, p := range people {
			v.People = append(v.People, personRow{LandMember: p, Me: strings.ToLower(p.Principal) == self})
		}
	}
	v.Members = h.BasePath + "/garden/c/" + url.PathEscape(ch.Land) + "/" + url.PathEscape(ch.Name)
	h.write(w, "people", v)
}
