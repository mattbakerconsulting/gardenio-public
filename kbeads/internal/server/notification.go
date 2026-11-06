package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// @mentions of people (Lands Commons spec gardenio/docs/superpowers/specs/
// 2026-10-19-lands-commons-design.md §7.3 step 6, gd-4fmDZb5C52).
//
// A land's members are the people whose type:user bead lists the land in
// allowed_projects: the UserProvisioner writes that list from the realm groups
// on every sign-in (/project-<land> and its owners and guests subgroups), so a
// guest is a member here too. Only an explicit entry counts; a wildcard grant
// is an operator's reach, not membership of a land. A scoped caller cannot
// read user beads (they carry no project label), so GET
// /v1/lands/{land}/members answers the list, read through the store, to a
// caller whose scope admits the land and to no one else.
//
// A mention of a person writes one type:notification bead per person: in the
// message's land (exactly one project label), to the person (fields.to,
// oidc:<email>), naming the message (fields.message_id). The Commons writes it
// on the mentioning person's own bearer, so the action table decides who may
// (a guest may: it may say things). The rules below decide what: the person is
// a member of that land, the message is a message of that land, and a person
// notifies only about their own words. A notification never moves to another
// land. Delivery (push, digest) is step 7's.

const typeNotification model.BeadType = "notification"

// notificationReasonMention is the one reason today.
const notificationReasonMention = "mention"

// landMembersMax caps the members one answer lists (tens of humans per land,
// spec §2.7).
const landMembersMax = 500

// LandMember is one entry of GET /v1/lands/{land}/members.
type LandMember struct {
	// Principal is the person as conversation beads name them: oidc:<email>.
	Principal string `json:"principal"`
	// Name is the user bead's title (the email's local part unless renamed).
	Name string `json:"name"`
	// Role is the person's tier in the land (owner, member or guest) as of
	// their last sign-in (land_roles, user_provision.go); empty when their
	// bead has not recorded one yet. It is for showing the crew: no access
	// decision reads it.
	Role string `json:"role,omitempty"`
}

// LandMembersResponse is the body of GET /v1/lands/{land}/members.
type LandMembersResponse struct {
	Land    string       `json:"land"`
	Members []LandMember `json:"members"`
}

// handleLandMembers handles GET /v1/lands/{land}/members.
func (s *BeadsServer) handleLandMembers(w http.ResponseWriter, r *http.Request) {
	// An applicant (an OIDC sign-in with no gardenio role and no group) is in
	// no land, as for read cursors.
	if authMethodFromContext(r.Context()) == authMethodOIDC && len(RolesFromContext(r.Context())) == 0 && len(GroupsFromContext(r.Context())) == 0 {
		writeError(w, http.StatusForbidden, "a land's members need a gardenio role or a land")
		return
	}
	land := strings.TrimSpace(r.PathValue("land"))
	if land == "" || !readCursorIDPattern.MatchString(land) {
		writeError(w, http.StatusBadRequest, "bad land")
		return
	}
	// One answer for a land outside the caller's scope and a land that does
	// not exist, so the route is no oracle for other lands' names.
	if !s.landAdmitsCaller(r.Context(), land) {
		writeError(w, http.StatusNotFound, "no such land")
		return
	}
	members, err := landMembers(r.Context(), s.store, land)
	if err != nil {
		writeServerError(r.Context(), w, "handleLandMembers", err, "failed to list land members")
		return
	}
	writeJSON(w, http.StatusOK, &LandMembersResponse{Land: land, Members: members})
}

// landAdmitsCaller reports whether the caller may read land's beads: a scoped
// principal holds it, a project-bound token is bound to it, and an OIDC user
// whose user bead confines it lists it.
func (s *BeadsServer) landAdmitsCaller(ctx context.Context, land string) bool {
	if !scopeFromContext(ctx).allows(land) {
		return false
	}
	if !beadMatchesTokenProject(ctx, []string{"project:" + land}) {
		return false
	}
	if allowed := oidcAllowedProjects(ctx, s.store); allowed != nil && !slices.Contains(allowed, land) {
		return false
	}
	return true
}

// landMembers lists the people of land, by name, read through the store.
func landMembers(ctx context.Context, st store.Store, land string) ([]LandMember, error) {
	beads, _, err := st.ListBeads(ctx, model.BeadFilter{
		Type: []model.BeadType{"user"},
		Kind: []model.Kind{"config"},
	})
	if err != nil {
		return nil, err
	}
	out := []LandMember{}
	seen := map[string]bool{}
	for _, b := range beads {
		email, ok := userBeadMemberOf(b, land)
		if !ok || seen[email] {
			continue
		}
		seen[email] = true
		name := strings.TrimSpace(b.Title)
		if name == "" {
			name, _, _ = strings.Cut(email, "@")
		}
		out = append(out, LandMember{Principal: "oidc:" + email, Name: name, Role: userBeadLandRole(b, land)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Principal < out[j].Principal
	})
	if len(out) > landMembersMax {
		out = out[:landMembersMax]
	}
	return out, nil
}

// userBeadMemberOf returns the lowercased email of a live user bead whose
// allowed_projects names land.
func userBeadMemberOf(b *model.Bead, land string) (string, bool) {
	if b == nil || b.Type != "user" || b.Status == model.StatusClosed {
		return "", false
	}
	var f struct {
		Email           string          `json:"email"`
		AllowedProjects json.RawMessage `json:"allowed_projects"`
	}
	if len(b.Fields) == 0 || json.Unmarshal(b.Fields, &f) != nil {
		return "", false
	}
	email := strings.ToLower(strings.TrimSpace(f.Email))
	if email == "" || !slices.Contains(parseStringList(f.AllowedProjects), land) {
		return "", false
	}
	return email, true
}

// userBeadLandRole is the tier a user bead's land_roles records for land, ""
// when it records none or one this code does not know.
func userBeadLandRole(b *model.Bead, land string) string {
	var f struct {
		LandRoles map[string]string `json:"land_roles"`
	}
	if len(b.Fields) == 0 || json.Unmarshal(b.Fields, &f) != nil {
		return ""
	}
	switch r := tier(f.LandRoles[land]); r {
	case tierOwner, tierMember, tierGuest:
		return string(r)
	}
	return ""
}

// isLandMember reports whether principal (oidc:<email>) is a member of land.
// Two user beads for one email count only when both name the land.
func isLandMember(ctx context.Context, st store.Store, land, principal string) (bool, error) {
	email, ok := strings.CutPrefix(principal, "oidc:")
	email = strings.ToLower(strings.TrimSpace(email))
	if !ok || email == "" {
		return false, nil
	}
	b, err := userBeadByEmail(ctx, st, email)
	if errors.Is(err, errAmbiguousUserBead) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, member := userBeadMemberOf(b, land)
	return member, nil
}

// notificationFields is the part of a notification's fields the rules read.
type notificationFields struct {
	To        string `json:"to"`
	Reason    string `json:"reason"`
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`
}

func readNotificationFields(raw json.RawMessage) notificationFields {
	var f notificationFields
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &f)
	}
	f.To = strings.TrimSpace(f.To)
	f.MessageID = strings.TrimSpace(f.MessageID)
	f.ChannelID = strings.TrimSpace(f.ChannelID)
	return f
}

// errNotificationTarget is the one answer for a person who is not a member of
// the notification's land, whether or not they are one elsewhere.
var errNotificationTarget = errors.New("to is not a member of this land")

// validateNotificationCreate runs the notification rules on create. The
// lookups read through the store; the errors follow the caller's scope, as the
// channel rules' do, so a refusal never says whether a person or a message
// exists in another land.
func validateNotificationCreate(ctx context.Context, st store.Store, bead *model.Bead, visible beadVisible) error {
	land, err := singleLand("a notification", bead.Labels)
	if err != nil {
		return err
	}
	// The fields are not required in the type config, because generic field
	// validation runs before authz and would answer an out-of-scope create
	// with a 400 instead of the project gate's 403; they are required here.
	f := readNotificationFields(bead.Fields)
	if f.Reason != notificationReasonMention {
		return errors.New("a notification needs reason=mention")
	}
	if !strings.HasPrefix(f.To, "oidc:") || !validMemberPrincipal(f.To) {
		return fmt.Errorf("a notification is to a person: to must be oidc:<email>")
	}
	member, err := isLandMember(ctx, st, land, f.To)
	if err != nil {
		return fmt.Errorf("member lookup failed: %w", err)
	}
	if !member {
		return errNotificationTarget
	}
	if f.MessageID == "" {
		return errors.New("a notification names its message: message_id is required")
	}
	msg, err := st.GetBead(ctx, f.MessageID)
	if err != nil || msg == nil || msg.Type != typeMessage || !visible(f.MessageID) {
		return errors.New("message_id is not a message in this land")
	}
	if lands := projectLabelValues(msg.Labels); len(lands) != 1 || lands[0] != land {
		return errors.New("message_id is not a message in this land")
	}
	if f.ChannelID != "" && f.ChannelID != messageChannelID(msg.Fields) {
		return errors.New("channel_id is not the message's channel")
	}
	// A person notifies about their own words only, so nobody can ping a
	// member in someone else's name.
	if self := commonsHumanSelf(ctx); self != "" {
		if !authorNames(readMessageOriginFields(msg.Fields).author(), self) {
			return errors.New("a person notifies only about a message they wrote")
		}
	}
	return nil
}

// notificationLandChangeError refuses an update that moves a notification to
// another land.
func notificationLandChangeError(prior, next []string) error {
	was, now := projectLabelValues(prior), projectLabelValues(next)
	if len(was) == 1 && len(now) == 1 && was[0] != now[0] {
		return fmt.Errorf("a notification cannot move between lands (%s to %s)", was[0], now[0])
	}
	return nil
}
