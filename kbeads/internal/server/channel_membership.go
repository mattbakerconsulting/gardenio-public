package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// Channel membership, Lands Commons spec §7.3 step 4.8 (gd-Yxtnphljt4).
//
// A user channel (channel_kind=user) and a DM are members-only:
// fields.members lists the principals (oidc:<email>, agent:<name>) who may
// post in them. Everyone in the land still reads them; hiding a channel from
// non-members is a private channel, which stays LATER. The land's default
// channels (general, crew, decisions, escalations) carry no members and are
// open to everyone in the land.
//
// The rules bind scoped principals, the people a land holds. An admin acts as
// the operator in every land, and fleet callers (agents, the bridge) post
// where routing puts them, so neither is checked here.
//
//   - Posting: a scoped principal's message whose channel_id names a
//     members-only channel needs oidc:<self> in that channel's members.
//   - Joining: a scoped principal changes a user channel's members only with a
//     patch that touches fields.members and nothing else (scopedActionJoin).
//     A member or owner adds or removes itself; a guest may only leave (an
//     owner adds guests); an owner adds or removes anyone. A DM's members are
//     fixed at create.
//
// The controller's twin is conversation.ChannelAllows and
// conversation.MembershipChangeAllows.

// scopedActionJoin is the scoped-gate action for a members-only patch of a
// channel bead. Only updateBead produces it (scopedUpdateAction).
const scopedActionJoin = "join"

// channelMembersOnly reports whether a channel of this kind admits only its
// members as posters.
func channelMembersOnly(kind string) bool {
	return kind == "user" || kind == "dm"
}

// validateChannelMembers checks the members list of a channel of this kind:
// each entry a principal, none twice.
func validateChannelMembers(kind string, members []string) error {
	seen := map[string]bool{}
	for _, m := range members {
		if !validMemberPrincipal(m) {
			return fmt.Errorf("%s member %q must be oidc:<email> or agent:<name>", kind, m)
		}
		if seen[m] {
			return fmt.Errorf("%s members lists %q twice", kind, m)
		}
		seen[m] = true
	}
	return nil
}

// channelMembershipOnly reports whether a patch on a channel bead changes
// fields.members and nothing else. bead must be the pre-patch state.
func channelMembershipOnly(bead *model.Bead, in *updateBeadInput) bool {
	return bead.Type == typeChannel && patchOnlyChangesFields([]string{"members"}, in, bead.Fields)
}

// scopedSelfPrincipal is the member principal of the request's OIDC caller,
// or "" when the token names nobody.
func scopedSelfPrincipal(ctx context.Context) string {
	if sub := AuthIdentityFromContext(ctx).Subject; sub != "" {
		return "oidc:" + sub
	}
	return ""
}

// readChannelFields decodes a channel bead's fields; a malformed value reads
// as empty, which every rule below treats as the least it can.
func readChannelFields(raw json.RawMessage) channelFields {
	var f channelFields
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &f)
	}
	return f
}

// scopedMembershipDenial checks a scoped principal's members-only patch of a
// channel (scopedActionJoin) against its role in the channel's land. The patch
// replaces the whole list, so one built from a stale read that drops or adds
// someone else lands here too, as a deny that says to re-read.
func scopedMembershipDenial(ctx context.Context, bead *model.Bead, patch json.RawMessage) *AuthzDenial {
	deny := func(msg string) *AuthzDenial {
		return &AuthzDenial{Action: scopedActionJoin, BeadType: string(bead.Type), Message: msg}
	}
	cur := readChannelFields(bead.Fields)
	if cur.ChannelKind != "user" {
		return deny("only a user channel can be joined or left (this is channel_kind=" + cur.ChannelKind + ")")
	}
	var p struct {
		Members []string `json:"members"`
	}
	if err := json.Unmarshal(patch, &p); err != nil {
		return deny("fields.members must be a list of principals")
	}
	self := scopedSelfPrincipal(ctx)
	if self == "" {
		return deny("scoped principal has no identity to join as")
	}
	lands := projectLabelValues(bead.Labels)
	if len(lands) != 1 {
		return deny("channel has no single land")
	}
	land := lands[0]
	t := scopeFromContext(ctx).Tiers[land]
	if t == tierOwner {
		return nil
	}
	for _, m := range p.Members {
		if slices.Contains(cur.Members, m) {
			continue
		}
		if m != self {
			return deny("only an owner of land " + land + " may add someone else to a channel (" + m + "); re-read the channel if you meant to join it")
		}
		if t == tierGuest {
			return deny("a guest in land " + land + " may not join a channel; an owner of the land adds guests")
		}
	}
	for _, m := range cur.Members {
		if m != self && !slices.Contains(p.Members, m) {
			return deny("only an owner of land " + land + " may remove someone else from a channel (" + m + "); re-read the channel if you meant to leave it")
		}
	}
	return nil
}

// channelPostDenial refuses a scoped principal's message into a members-only
// channel it is not a member of. It runs after validateConversationLand, so a
// channel_id it reads is a channel in the message's land the caller can see.
func channelPostDenial(ctx context.Context, st store.Store, msg *model.Bead) *AuthzDenial {
	if !isScopedPrincipal(ctx) || msg.Type != typeMessage {
		return nil
	}
	chID := messageChannelID(msg.Fields)
	if chID == "" {
		return nil
	}
	ch, err := st.GetBead(ctx, chID)
	if err != nil || ch == nil {
		return nil
	}
	f := readChannelFields(ch.Fields)
	if !channelMembersOnly(f.ChannelKind) {
		return nil
	}
	if self := scopedSelfPrincipal(ctx); self != "" && slices.Contains(f.Members, self) {
		return nil
	}
	msgText := "not a member of #" + f.Name + ": join the channel to post in it"
	if f.ChannelKind == "dm" {
		msgText = "not a member of this dm: only its members post in it"
	}
	return &AuthzDenial{Action: "create", BeadType: string(msg.Type), Message: msgText}
}
