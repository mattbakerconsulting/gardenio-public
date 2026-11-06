package server

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// origin=commons is a person's own words (gd-Wu4U4hZbY0).
//
// A message with fields.origin=commons is one a person wrote in the Commons
// (Lands Commons spec 2.3). The bridge acts on it in that person's name: it
// mirrors it into Slack under their name and avatar, and hands it to the
// thread's agent as their reply. The only writer meant to set it is the
// viewer's Commons composer, which writes on the person's own OIDC bearer
// (beadsViewer.requireUserBearerForWrites), with fields.author naming them.
// fields.author is free text, so before this gate any caller (an agent on the
// shared static token or its own fleet token) could write a message naming a
// person and have it appear as theirs.
//
// So a message carries origin=commons only when an OIDC human writes it as
// themselves: the caller authenticated with a Keycloak bearer (authMethodOIDC,
// never a land principal, a machine, an edge envelope or a static or fleet
// token, whose X-Actor cannot name a person since gd-qZER8OOiIb), and
// fields.author, when set, names that caller. created_by is then their email.
// An update may not move an existing message into that state either: a patch
// that sets origin or author on a message that will carry origin=commons must
// come from its creator, writing as themselves. Auth-disabled mode
// (authMethodNone) has no callers to tell apart and is not checked.
//
// The bridge makes the same check against the writer for messages written
// before this gate (commonsMessage.writtenByAuthor), and posts any it fails as
// the bot.

// originCommons is fields.origin of a message written in the Commons.
const originCommons = "commons"

// messageOriginFields is the part of a message's fields this gate reads.
type messageOriginFields struct {
	Origin *string
	Author *string
	// BadKey is a top-level key that spells origin or author in another
	// letter case, or repeats one: readers disagree on which value it is.
	BadKey string
}

// messageOriginKeys are the keys readMessageOriginFields reads.
var messageOriginKeys = []string{"origin", "author"}

// readMessageOriginFields reads origin and author by their exact keys, as the
// Commons does (b.Fields["author"]). It does not decode into a struct:
// encoding/json matches struct tags case-insensitively and the last match
// wins, so {"author":"oidc:<owner>","Author":"agent:<self>"} would read as
// the caller while the store keeps both keys and the Commons shows the owner
// (PR #745 review). A key that case-folds to origin or author without matching
// it, or one given twice, is reported in BadKey for messageOriginKeyDenial to
// refuse. A value that is not a string reads as unset.
func readMessageOriginFields(raw json.RawMessage) messageOriginFields {
	var f messageOriginFields
	if len(raw) == 0 {
		return f
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return f
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return f
		}
		key, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return f
		}
		for _, k := range messageOriginKeys {
			if !strings.EqualFold(key, k) {
				continue
			}
			if key != k || seen[k] {
				if f.BadKey == "" {
					f.BadKey = key
				}
				break
			}
			seen[k] = true
			var str string
			if json.Unmarshal(v, &str) != nil {
				break
			}
			if k == "origin" {
				f.Origin = &str
			} else {
				f.Author = &str
			}
		}
	}
	return f
}

// messageOriginKeyDenial refuses a message write whose fields spell origin or
// author in another letter case, or give one twice.
func messageOriginKeyDenial(action string, f messageOriginFields) *AuthzDenial {
	if f.BadKey == "" {
		return nil
	}
	return commonsOriginDeny(action, "a message's fields."+f.BadKey+" repeats origin or author, or spells one in another letter case: write each once, as \"origin\" and \"author\"")
}

func (f messageOriginFields) origin() string {
	if f.Origin == nil {
		return ""
	}
	return strings.TrimSpace(*f.Origin)
}

func (f messageOriginFields) author() string {
	if f.Author == nil {
		return ""
	}
	return strings.TrimSpace(*f.Author)
}

// commonsHumanSelf returns the caller's email when it is an OIDC human, or ""
// when it is anything else (a machine, a land principal and a v1 edge
// envelope each have an auth method of their own). A verified v2 edge
// envelope is stamped authMethodOIDC for role checks (edge_auth.go), but it is
// the edge vouching for an operator, not that person's own bearer, so only an
// identity of type oidc counts.
func commonsHumanSelf(ctx context.Context) string {
	if authMethodFromContext(ctx) != authMethodOIDC {
		return ""
	}
	id := AuthIdentityFromContext(ctx)
	if id.Type != "oidc" {
		return ""
	}
	sub := id.Subject
	if !strings.Contains(sub, "@") || strings.Contains(sub, ":") {
		return ""
	}
	return sub
}

// authorNames reports whether a message's author field names the person with
// this email: oidc:<email>, or the bare email.
func authorNames(author, email string) bool {
	return strings.EqualFold(strings.TrimPrefix(author, "oidc:"), email)
}

func commonsOriginDeny(action string, msg string) *AuthzDenial {
	return &AuthzDenial{Action: action, BeadType: string(typeMessage), Message: msg}
}

// commonsOriginDenial refuses a message create with origin=commons from anyone
// but an OIDC human whose author field, when set, names themselves.
func commonsOriginDenial(ctx context.Context, beadType model.BeadType, fields json.RawMessage) *AuthzDenial {
	if beadType != typeMessage {
		return nil
	}
	f := readMessageOriginFields(fields)
	if denial := messageOriginKeyDenial("create", f); denial != nil {
		return denial
	}
	if authMethodFromContext(ctx) == authMethodNone || f.origin() != originCommons {
		return nil
	}
	self := commonsHumanSelf(ctx)
	if self == "" {
		return commonsOriginDeny("create", "origin=commons is a person's own words: only a signed-in person writes it, on their own bearer")
	}
	if a := f.author(); a != "" && !authorNames(a, self) {
		return commonsOriginDeny("create", "origin=commons author must be you (oidc:"+self+"), not "+a)
	}
	return nil
}

// commonsOriginPatchDenial refuses a patch that sets origin or author on a
// message that will carry origin=commons, unless its creator makes it as
// themselves. A patch that leaves both as they are (the bridge stamping
// handled_at or its Slack ref, a reaction) is not checked.
func commonsOriginPatchDenial(ctx context.Context, bead *model.Bead, patch json.RawMessage) *AuthzDenial {
	if bead == nil || bead.Type != typeMessage || len(patch) == 0 {
		return nil
	}
	p := readMessageOriginFields(patch)
	if denial := messageOriginKeyDenial("update", p); denial != nil {
		return denial
	}
	if authMethodFromContext(ctx) == authMethodNone || (p.Origin == nil && p.Author == nil) {
		return nil
	}
	cur := readMessageOriginFields(bead.Fields)
	next := cur
	if p.Origin != nil {
		next.Origin = p.Origin
	}
	if p.Author != nil {
		next.Author = p.Author
	}
	if next.origin() != originCommons || (next.origin() == cur.origin() && next.author() == cur.author()) {
		return nil
	}
	self := commonsHumanSelf(ctx)
	if self == "" || !strings.EqualFold(strings.TrimPrefix(bead.CreatedBy, "oidc:"), self) {
		return commonsOriginDeny("update", "origin=commons is a person's own words: only the person who wrote this message may set its origin or author")
	}
	if a := next.author(); a != "" && !authorNames(a, self) {
		return commonsOriginDeny("update", "origin=commons author must be you (oidc:"+self+"), not "+a)
	}
	return nil
}

// A person's Commons message keeps its words (gd-74wnmpQgyT).
//
// The gates above stop a caller making a message look like a person's. They
// do not stop one changing what a person's message says or where it goes: a
// patch that rewrites the text of ada's message and clears its slack_ts
// leaves created_by and author as they were, so the bridge still takes the
// message as hers (writtenByAuthor), finds no Slack copy of it
// (mirrorCandidate), and posts the new words under her persona. Relabelling
// it thread-say and closing it, or pointing its slack_thread_channel/ts at
// another thread, has ThreadMessageForward post it there as hers. So on a
// message a person wrote with origin=commons, only that person, writing as
// themselves, may:
//
//   - change its words, how they read and where they go (commonsWriterKeys,
//     and the bead's title, description, status and labels);
//   - close or delete it, or add or remove a label;
//   - change its Slack copy once it is set (commonsSetOnceKeys). Setting one
//     that is empty stays open to anyone who may write the bead: the bridge
//     stamps slack_channel/slack_ts after it posts (commons_mirror.go) and
//     channel_id on a thread head (channel_stamp.go).
//
// Other fields (handled_at, reactions and the like) are not this gate's. A
// message whose creator is not a person (an agent's, written before the
// create gate) is posted by the bridge as the bot, so it is not checked.
// Nothing but its writer closes, relabels or deletes a person's message: the
// Commons pane writes it closed and labelled, and the bridge writes only its
// Slack ref and handled_at.

// commonsWriterKeys are the fields of a person's Commons message that only
// they may change.
var commonsWriterKeys = map[string]bool{
	"text": true, "files": true, "snap": true, "kind": true, "detail": true, "footnote": true,
	"origin": true, "author": true, "deleted": true, "slack_root_text": true,
	"thread_id": true, "slack_thread_channel": true, "slack_thread_ts": true,
}

// commonsSetOnceKeys are the fields of a person's Commons message that anyone
// who may write it may set while empty, and only they may change after.
var commonsSetOnceKeys = map[string]bool{
	"channel_id": true, "slack_channel": true, "slack_ts": true,
}

// commonsMessageWriter returns the email of the person who wrote bead when it
// is a message with origin=commons that a person created, or "".
func commonsMessageWriter(bead *model.Bead) string {
	if bead == nil || bead.Type != typeMessage || readMessageOriginFields(bead.Fields).origin() != originCommons {
		return ""
	}
	who := strings.TrimPrefix(bead.CreatedBy, "oidc:")
	if !strings.Contains(who, "@") || strings.Contains(who, ":") {
		return ""
	}
	return who
}

// commonsMessageGuarded reports whether the caller is someone other than the
// person who wrote bead, a Commons message, and so may not change its words.
func commonsMessageGuarded(ctx context.Context, bead *model.Bead) bool {
	who := commonsMessageWriter(bead)
	return who != "" && authMethodFromContext(ctx) != authMethodNone && !strings.EqualFold(commonsHumanSelf(ctx), who)
}

func commonsMessageEditDeny(action, what string) *AuthzDenial {
	return commonsOriginDeny(action, "origin=commons is a person's own words: only the person who wrote this message may change its "+what)
}

// commonsMessageLockDenial refuses action (close, delete, or update for a
// label change) on a person's Commons message from anyone but them; what says
// what it would do.
func commonsMessageLockDenial(ctx context.Context, action, what string, bead *model.Bead) *AuthzDenial {
	if !commonsMessageGuarded(ctx, bead) {
		return nil
	}
	return commonsOriginDeny(action, "origin=commons is a person's own words: only the person who wrote this message may "+what)
}

// commonsMessageEditDenial refuses a patch to a person's Commons message from
// anyone but them when it changes its title, short_title, description, status, labels, a
// commonsWriterKeys field, or a commonsSetOnceKeys field that is already set.
func commonsMessageEditDenial(ctx context.Context, bead *model.Bead, in *updateBeadInput) *AuthzDenial {
	const action = "update"
	if !commonsMessageGuarded(ctx, bead) {
		return nil
	}
	if in.Title != nil && *in.Title != bead.Title {
		return commonsMessageEditDeny(action, "title")
	}
	if in.ShortTitle != nil && *in.ShortTitle != bead.ShortTitle {
		return commonsMessageEditDeny(action, "short_title")
	}
	if in.Description != nil && *in.Description != bead.Description {
		return commonsMessageEditDeny(action, "description")
	}
	if in.Status != nil && *in.Status != string(bead.Status) {
		return commonsMessageEditDeny(action, "status")
	}
	if in.labelsSet && !sameStringSet(in.Labels, bead.Labels) {
		return commonsMessageEditDeny(action, "labels")
	}
	if len(in.Fields) == 0 {
		return nil
	}
	var next, cur map[string]json.RawMessage
	if err := json.Unmarshal(in.Fields, &next); err != nil {
		return commonsMessageEditDeny(action, "fields")
	}
	if len(bead.Fields) > 0 {
		_ = json.Unmarshal(bead.Fields, &cur)
	}
	keys := mapKeys(next)
	sort.Strings(keys)
	for _, k := range keys {
		if sameJSONValue(cur[k], next[k]) {
			continue
		}
		if commonsWriterKeys[k] || (commonsSetOnceKeys[k] && !emptyJSONValue(cur[k])) {
			return commonsMessageEditDeny(action, k)
		}
	}
	return nil
}

// commonsMessageFieldKeysDenial is commonsMessageEditDenial for the field
// endpoints (increment, append, reorder, edit), which name the keys they
// write but not a whole value to compare: any guarded key is refused.
func commonsMessageFieldKeysDenial(ctx context.Context, action string, bead *model.Bead, keys ...string) *AuthzDenial {
	if !commonsMessageGuarded(ctx, bead) {
		return nil
	}
	for _, k := range keys {
		if commonsGuardedKey(k) {
			return commonsMessageEditDeny(action, k)
		}
	}
	return nil
}

// commonsGuardedKey reports whether k, in any letter case, is a
// commonsWriterKeys or commonsSetOnceKeys field.
func commonsGuardedKey(k string) bool {
	for g := range commonsWriterKeys {
		if strings.EqualFold(k, g) {
			return true
		}
	}
	for g := range commonsSetOnceKeys {
		if strings.EqualFold(k, g) {
			return true
		}
	}
	return false
}

// sameStringSet reports whether a and b hold the same strings, in any order.
func sameStringSet(a, b []string) bool {
	as, bs := slices.Clone(a), slices.Clone(b)
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(slices.Compact(as), slices.Compact(bs))
}

// emptyJSONValue reports whether raw is absent, null or the empty string.
func emptyJSONValue(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null" || s == `""`
}

// sameJSONValue reports whether a and b hold the same JSON value, taking
// absent, null and "" as one.
func sameJSONValue(a, b json.RawMessage) bool {
	if emptyJSONValue(a) || emptyJSONValue(b) {
		return emptyJSONValue(a) && emptyJSONValue(b)
	}
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}
