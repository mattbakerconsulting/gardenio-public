package server

import (
	"context"
	"encoding/json"
	"strings"

	"gardenio/kbeads/internal/model"
)

// A message's author is its writer unless a relay names it (gd-PjNjRhAHPn).
//
// The Commons shows a message as written by fields.author. The origin=commons
// gate (commons_origin.go) checks author only on a person's Commons message,
// so before this gate any agent, on the shared static token or its own fleet
// token, could write a message with no origin and author=oidc:<owner>, and the
// Commons showed it as the owner's.
//
// So a caller that is not an OIDC human may set a message's author only to
// itself: its own subject, or agent:<subject>. Two kinds of caller write in
// other principals' names by design and may name anyone:
//
//   - a bridge (isBridgeFleetIdentity: a "<name>-bridge" fleet or a
//     "bridge:<name>" subject), which mirrors a person's Slack message into the
//     Commons as theirs (commons_mirror.go mirrorSlackMessage) and writes an
//     agent's says and cards as the agent's (conversation.Router);
//   - a machine principal (a registered Keycloak service account), such as the
//     controller posting the purse watcher's notices as agent:gardenio-purse.
//
// author is read by its exact key (readMessageOriginFields); a fields object
// that spells it in another letter case or gives it twice is refused before
// this gate, by the origin=commons gate that runs first (messageOriginKeyDenial).
//
// Neither credential is held by an agent pod. The shared static token is,
// whatever its X-Actor, so it is held to its own name like any other caller.
// An OIDC caller is not checked here (a person's Commons message is checked by
// the origin=commons gate, and the Commons shows a person's other messages by
// created_by), nor is auth-disabled mode.

// messageAuthorRelay reports whether the caller may name another principal as
// a message's author, and the caller's subject when it may not. ok is true for
// a caller this gate does not check.
func messageAuthorRelay(ctx context.Context) (subject string, ok bool) {
	switch authMethodFromContext(ctx) {
	case authMethodNone, authMethodOIDC, authMethodMachine:
		return "", true
	}
	id := AuthIdentityFromContext(ctx)
	if id.Type == "fleet" && isBridgeFleetIdentity(FleetFromContext(ctx), id.Subject) {
		return "", true
	}
	return id.Subject, false
}

// authorIsCaller reports whether author names the caller with this subject:
// the subject itself or agent:<subject>. The bare primary static token
// ("static", no agent named) names no one.
func authorIsCaller(author, subject string) bool {
	if subject == "" || subject == "static" {
		return false
	}
	return strings.EqualFold(author, subject) || strings.EqualFold(author, "agent:"+subject)
}

func messageAuthorDeny(action, author, subject string) *AuthzDenial {
	msg := "a message's author must be its writer: you may not write as " + author +
		" (only a person signed in as themselves, a bridge or a machine principal names another author)"
	if subject != "" && subject != "static" {
		msg += "; write author agent:" + subject + " or leave it unset"
	}
	return &AuthzDenial{Action: action, BeadType: string(typeMessage), Message: msg}
}

// messageAuthorDenial refuses a message create whose author names someone
// other than a caller that may not write as others.
func messageAuthorDenial(ctx context.Context, beadType model.BeadType, fields json.RawMessage) *AuthzDenial {
	if beadType != typeMessage {
		return nil
	}
	a := readMessageOriginFields(fields).author()
	if a == "" {
		return nil
	}
	subject, ok := messageAuthorRelay(ctx)
	if ok || authorIsCaller(a, subject) {
		return nil
	}
	return messageAuthorDeny("create", a, subject)
}

// messageAuthorPatchDenial is messageAuthorDenial for an update: a patch that
// changes author to someone other than the caller. A patch that repeats the
// author already there, or clears it, is not checked.
func messageAuthorPatchDenial(ctx context.Context, bead *model.Bead, patch json.RawMessage) *AuthzDenial {
	if bead == nil || bead.Type != typeMessage || len(patch) == 0 {
		return nil
	}
	p := readMessageOriginFields(patch)
	a := p.author()
	if a == "" || a == readMessageOriginFields(bead.Fields).author() {
		return nil
	}
	subject, ok := messageAuthorRelay(ctx)
	if ok || authorIsCaller(a, subject) {
		return nil
	}
	return messageAuthorDeny("update", a, subject)
}

// messageAuthorFieldKeysDenial refuses the field endpoints (increment, append,
// reorder, edit) on a message's author to a caller that may not write as
// others: they write no whole value to check.
func messageAuthorFieldKeysDenial(ctx context.Context, action string, bead *model.Bead, keys ...string) *AuthzDenial {
	if bead == nil || bead.Type != typeMessage {
		return nil
	}
	for _, k := range keys {
		if !strings.EqualFold(k, "author") {
			continue
		}
		subject, ok := messageAuthorRelay(ctx)
		if ok {
			return nil
		}
		return &AuthzDenial{Action: action, BeadType: string(typeMessage),
			Message: "a message's author must be its writer: set it with an update naming yourself (agent:" + subject + "), not a field edit"}
	}
	return nil
}
