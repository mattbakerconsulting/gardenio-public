package commons

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/kin"
)

// The Commons write routes (Commons I step 9): a person says something in a
// channel, replies in a thread, or joins or leaves a user channel. Every write
// goes to the daemon on the request context, so on the person's own bearer,
// and kbeads checks it again against the same action table (spec C6, 6.4):
// the Gate here only decides before the write is attempted, and what the
// fragments offer.
//
// A message is a closed type=message bead with origin=commons, attributed to
// the person (spec 2.3). A reply in a wish head bound to a Slack thread also
// carries the head's Slack binding and the thread-say label, as the /threads
// composer's do: the bridge's thread forward posts it into that thread and
// hands it to the thread's agent (a nudge, or a respawn with resume). Any
// other origin=commons reply reaches the agent through the bridge's inbound
// sweep (internal/bridge/inbound_reply.go).

// Writer is the slice of the daemon client the write routes use.
// *beadsapi.Client satisfies it.
type Writer interface {
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
}

// Actions the Gate is asked about: conversation.ActionSay and ActionReply.
// This package keeps the names, not the bridge-side types.
const (
	ActionSay   = "say"
	ActionReply = "reply"
)

// Gate is what the viewer may write. beads-viewer builds it from its
// viewerScope: the land-role action table plus channel membership.
type Gate struct {
	// Self is the viewer's principal, oidc:<email>, or "" when anonymous.
	Self func(*http.Request) string
	// May reports whether the viewer may take action (ActionSay, ActionReply)
	// in ch, membership included.
	May func(r *http.Request, ch Channel, action string) bool
	// MayMembers reports whether the viewer may change ch's members to next.
	MayMembers func(r *http.Request, ch Channel, next []string) bool
	// MayAct reports whether the viewer may take action (an action-table
	// entry, conversation.Action: decision_respond, agent_control, ...) in
	// land: the card buttons and lightboxes (card_actions.go).
	MayAct func(r *http.Request, land, action string) bool
}

// Write bounds (and files.go's, for attachments).
const (
	// textMaxBytes caps one message, the /threads composer's limit.
	textMaxBytes = 10000
	// formMaxBytes caps a url-encoded write request body.
	formMaxBytes = 64 << 10
	// titleMax is the bead title cap; the whole text stays in fields.text.
	titleMax     = 500
	writeTimeout = 10 * time.Second
)

// access is what one viewer may do in one channel: the composer or Join the
// thread fragment renders.
type access struct {
	Self string
	// Post: the composer is offered (a reply in a thread).
	Post bool
	// Join: a user channel the viewer is not in and may join.
	Join bool
	// MembersOnly: only members post here (a user channel or a DM).
	MembersOnly bool
	// Frozen: the land is frozen (freeze.go). For anyone but the operator,
	// Post and Join are then false and a banner stands in, except
	// that Post may hold in the land's #support.
	Frozen bool
	// FrozenNote is the banner over the composer when the land is frozen and
	// the viewer may still post.
	FrozenNote string
}

func (h *Handler) self(r *http.Request) string {
	if h.Gate.Self == nil {
		return ""
	}
	return h.Gate.Self(r)
}

func (h *Handler) may(r *http.Request, ch Channel, action string) bool {
	return h.Gate.May != nil && h.self(r) != "" && h.Gate.May(r, ch, action)
}

func (h *Handler) mayMembers(r *http.Request, ch Channel, next []string) bool {
	return h.Gate.MayMembers != nil && h.self(r) != "" && h.Gate.MayMembers(r, ch, next)
}

// accessTo is the viewer's access to ch for action.
func (h *Handler) accessTo(r *http.Request, ch Channel, action string) access {
	a := access{Self: h.self(r), MembersOnly: ch.MembersOnly()}
	if h.Writes == nil {
		return a
	}
	a.Post = h.may(r, ch, action)
	if ch.Kind == "user" && a.Self != "" && !ch.HasMember(a.Self) {
		a.Join = h.mayMembers(r, ch, append(slices.Clone(ch.Members), a.Self))
	}
	h.switchAccess(r, ch, action, &a)
	return a
}

func without(members []string, who string) []string {
	return slices.DeleteFunc(slices.Clone(members), func(m string) bool { return m == who })
}

// say serves POST /garden/c/{land}/{channel}/say: a new top-level message. It
// answers 201 with the message as the timeline renders it.
func (h *Handler) say(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) || !h.writable(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), writeTimeout)
	defer cancel()
	ch, err := readChannel(ctx, h.Beads, h.scope(r), r.PathValue("land"), r.PathValue("channel"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if !h.mayPost(w, r, ch, ActionSay) {
		return
	}
	if !h.rated(w, r, ch) {
		return
	}
	m, again, ok := h.post(w, r, ch, nil)
	if !ok {
		return
	}
	h.Presence.StopTyping(h.viewer(r), Spot{Land: ch.Land, Channel: ch.ID}, "")
	if !again {
		h.notifyMentioned(r.Context(), ch, m)
		h.mentionNote(w, r, m)
	}
	h.created(w, r, newRow(m, h.BasePath, ch.Name))
}

// reply serves POST /garden/c/t/{thread}/reply: a reply in the thread whose
// root is {thread}. It answers 201 with the reply as the thread renders it.
func (h *Handler) reply(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) || !h.writable(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("thread"))
	if id == "" || len(id) > 200 {
		http.Error(w, "bad thread id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), writeTimeout)
	defer cancel()
	sc := h.scope(r)
	b, err := h.Beads.GetBead(ctx, id)
	if err != nil {
		h.fail(w, readErr("thread "+id, err))
		return
	}
	if b == nil {
		h.fail(w, errNotFound)
		return
	}
	root, ok := messageFromBead(b)
	if !ok || root.Thread != "" || !sc.Allows(root.Land) {
		h.fail(w, errNotFound)
		return
	}
	// A chat root a mention promoted: the thread is its wish head's now.
	if head, ok := promotedHeadOf(ctx, h.Beads, root); ok {
		if hb, err := h.Beads.GetBead(ctx, head.ID); err == nil && hb != nil {
			b, root = hb, head
		}
	}
	ch, err := readChannel(ctx, h.Beads, sc, root.Land, root.Channel)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !h.mayPost(w, r, ch, ActionReply) {
		return
	}
	if !h.rated(w, r, ch) {
		return
	}
	m, again, ok := h.post(w, r, ch, b)
	if !ok {
		return
	}
	h.Presence.StopTyping(h.viewer(r), Spot{Land: ch.Land, Channel: ch.ID}, root.ID)
	if !again {
		h.notifyMentioned(r.Context(), ch, m)
		h.mentionNote(w, r, m)
	}
	h.created(w, r, row{M: m, Base: h.BasePath})
}

// post reads a say or reply form the Gate has let through, stores its files
// and writes the message: in ch, a reply when head is set. The body is read
// only now, so a refused upload is never stored, and the store and the daemon
// get their own time after it, however long the body took to arrive. A form
// whose nonce an earlier attempt wrote a message under answers that message,
// again true, and writes nothing (sendonce.go). It answers a failure itself.
func (h *Handler) post(w http.ResponseWriter, r *http.Request, ch Channel, head *beadsapi.BeadDetail) (m Message, again, ok bool) {
	in, ok := h.readPost(w, r)
	if !ok {
		return Message{}, false, false
	}
	snap, ok := h.snapField(w, r, in.snap, len(in.files))
	if !ok {
		return Message{}, false, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), writeTimeout+time.Duration(len(in.files))*fileTimeout)
	defer cancel()
	if key := sendKey(h.self(r), in.nonce); key != "" {
		prev, again, err := h.sends.claim(ctx, key, h.now())
		if err != nil {
			http.Error(w, "That message is still being sent. Try again.", http.StatusServiceUnavailable)
			return Message{}, false, false
		}
		if again {
			return prev, true, true
		}
		defer func() { h.sends.end(key, m, h.now()) }()
	} else {
		in.nonce = ""
	}
	var files []stored
	if len(in.files) > 0 {
		var err error
		if files, err = h.storeFiles(ctx, r, ch, in.files); err != nil {
			slog.Default().Warn("commons: attachments were not stored", "channel", ch.ID, "error", err)
			http.Error(w, "The files could not be stored. Try again.", http.StatusBadGateway)
			return Message{}, false, false
		}
	}
	m, err := h.writeMessage(ctx, ch, h.self(r), in, files, snap, head)
	if err != nil {
		h.dropFiles(r, files)
		h.writeFail(w, err)
		return Message{}, false, false
	}
	return m, false, true
}

// join serves POST /garden/c/{land}/{channel}/join and leave .../leave: the
// viewer adds itself to, or takes itself off, a user channel's members (spec
// 2.2.1). The patch replaces the whole list, as gb channel join does, and
// kbeads refuses one that changes anyone else. It answers 204 No Content, also
// when the viewer is already in (or out); a thread fragment read next offers
// the composer (or Join).
func (h *Handler) join(w http.ResponseWriter, r *http.Request)  { h.membership(w, r, true) }
func (h *Handler) leave(w http.ResponseWriter, r *http.Request) { h.membership(w, r, false) }

func (h *Handler) membership(w http.ResponseWriter, r *http.Request, join bool) {
	if !h.ready(w) || !h.writable(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), writeTimeout)
	defer cancel()
	ch, err := readChannel(ctx, h.Beads, h.scope(r), r.PathValue("land"), r.PathValue("channel"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if ch.Kind != "user" {
		http.Error(w, "Only a user channel has members to join or leave.", http.StatusConflict)
		return
	}
	if h.refuseFrozen(w, r, ch) {
		return
	}
	self := h.self(r)
	next := without(ch.Members, self)
	if join {
		next = append(next, self)
	}
	if slices.Equal(next, ch.Members) || (!join && !ch.HasMember(self)) {
		w.WriteHeader(http.StatusNoContent) // already so
		return
	}
	if !h.mayMembers(r, ch, next) {
		verb := "leave"
		if join {
			verb = "join"
		}
		http.Error(w, "You may not "+verb+" #"+ch.Name+".", http.StatusForbidden)
		return
	}
	if err := h.Writes.UpdateBeadFieldsTyped(ctx, ch.ID, map[string]any{fieldMembers: next}); err != nil {
		h.writeFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writable checks what every write needs before its body is read: a writer
// and a signed-in viewer.
func (h *Handler) writable(w http.ResponseWriter, r *http.Request) bool {
	if h.Writes == nil {
		http.Error(w, "commons writes not enabled", http.StatusServiceUnavailable)
		return false
	}
	if h.self(r) == "" {
		http.Error(w, "Sign in to post.", http.StatusUnauthorized)
		return false
	}
	// Say, reply, join, leave and typing all write as the signed-in person,
	// so a page on another site must not be able to drive them (gd-OPACzpdlZU).
	if crossOrigin(r) {
		http.Error(w, "Posts must come from this page.", http.StatusForbidden)
		return false
	}
	return true
}

// formText reads the text of a url-encoded say or reply form.
func (h *Handler) formText(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, formMaxBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return "", false
	}
	text := strings.TrimSpace(r.PostForm.Get("text"))
	return text, checkText(w, text)
}

// checkText checks the text of a message, which says something unless files do.
func checkText(w http.ResponseWriter, text string) bool {
	switch {
	case text == "":
		http.Error(w, "Write something first.", http.StatusBadRequest)
		return false
	case len(text) > textMaxBytes:
		http.Error(w, "That message is too long (10000 characters at most).", http.StatusRequestEntityTooLarge)
		return false
	case !utf8.ValidString(text):
		http.Error(w, "That message is not valid text.", http.StatusBadRequest)
		return false
	}
	return true
}

// writeMessage creates the message bead: in ch, by author, with in's text and
// nonce, files and a snapshot's context, a reply when head (the thread's root
// bead) is set. A message that is only files has no text field, and its title
// names the files, or the place for a snapshot.
func (h *Handler) writeMessage(ctx context.Context, ch Channel, author string, in posted, files []stored, snap string, head *beadsapi.BeadDetail) (Message, error) {
	text := in.text
	fields := map[string]string{
		fieldChannelID: ch.ID,
		fieldAuthor:    author,
		fieldKind:      "text",
		fieldOrigin:    originCommons,
	}
	title := text
	switch {
	case text != "":
		fields[fieldText] = text
	case snap != "":
		title = snapTitle(snap)
	default:
		title = fileTitle(files)
	}
	labels := []string{"project:" + ch.Land}
	m := Message{Land: ch.Land, Channel: ch.ID, Kind: "text", Author: author, Voice: kin.VoiceOf(author, ch.Land), Via: kin.ViaCommons, Text: text, At: h.now(), Nonce: in.nonce}
	if in.nonce != "" {
		fields[fieldNonce] = in.nonce
	}
	if a := strings.TrimSpace(in.about); snapBeadID.MatchString(a) {
		fields[fieldAbout] = a
	}
	if len(files) > 0 {
		fields[fieldFiles] = filesField(files)
		m.Files = filesFrom(fields[fieldFiles], ch.Land, ch.ID)
	}
	if snap != "" {
		fields[fieldSnap], m.Snap = snap, snapFrom(snap, m.Files)
	}
	if head != nil {
		fields[fieldThreadID] = head.ID
		m.Thread = head.ID
		if c, ts := head.Fields[fieldSlackChannel], head.Fields[fieldSlackTS]; slackThread(c, ts) {
			fields[fieldSlackChannel], fields[fieldSlackTS] = c, ts
			labels = append(labels, LabelThreadSay, "thread:"+head.ID)
		}
	}
	raw, _ := json.Marshal(fields) // a map[string]string always marshals
	id, err := h.Writes.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     clipBytes(title, titleMax),
		Type:      typeMessage,
		Kind:      "data",
		Status:    "closed", // a message is said once; the Commons never edits it open
		Labels:    labels,
		CreatedBy: author,
		Fields:    raw,
	})
	if err != nil {
		return Message{}, err
	}
	m.ID = id
	return m, nil
}

// slackThread mirrors bridge.isValidThreadBinding: a Slack channel id and a
// seconds.microseconds timestamp.
func slackThread(channel, ts string) bool {
	return len(channel) >= 2 && strings.Contains(ts, ".")
}

// clipBytes cuts s to at most n bytes on a rune boundary, with an ellipsis.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// refuse answers a write the Gate does not allow, saying why when it is
// membership.
func (h *Handler) refuse(w http.ResponseWriter, ch Channel, action string) {
	msg := "You may not post in #" + ch.Name + "."
	if action == ActionReply {
		msg = "You may not reply in #" + ch.Name + "."
	}
	switch {
	case ch.Kind == "dm":
		msg = "Only the members of this conversation post in it."
	case ch.MembersOnly():
		msg = "Join #" + ch.Name + " to post in it."
	}
	http.Error(w, msg, http.StatusForbidden)
}

// rated takes one message from the viewer's Rate in ch's land, answering 429
// when they are past it (Lands Track B §5.4, step 10.4). The key is the land
// and the member, so each member of each land has their own cap.
func (h *Handler) rated(w http.ResponseWriter, r *http.Request, ch Channel) bool {
	if ok, _ := h.Rate.Allow(ch.Land+"\x00"+h.self(r), ""); ok {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(h.Rate.Window().Seconds())))
	http.Error(w, fmt.Sprintf("You are posting too fast: %d messages in %s at most. Wait a moment, then send it again.",
		h.Rate.Max(), rateWindow(h.Rate.Window())), http.StatusTooManyRequests)
	return false
}

// rateWindow names a rate window in words: "a minute", "30s".
func rateWindow(d time.Duration) string {
	if d == time.Minute {
		return "a minute"
	}
	return d.String()
}

// writeFail maps the daemon's refusal of a write to the viewer's status: its
// 403 (the action table, membership) and 404 pass through with its words.
func (h *Handler) writeFail(w http.ResponseWriter, err error) {
	var ae *beadsapi.APIError
	if errors.As(err, &ae) {
		switch ae.StatusCode {
		case http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusBadRequest:
			msg := strings.TrimSpace(ae.Message)
			if msg == "" {
				msg = http.StatusText(ae.StatusCode)
			}
			http.Error(w, msg, ae.StatusCode)
			return
		}
	}
	http.Error(w, "The beads daemon did not take that. Try again.", http.StatusBadGateway)
}

func (h *Handler) created(w http.ResponseWriter, req *http.Request, r row) {
	ctx, cancel := context.WithTimeout(req.Context(), readTimeout)
	rows := []row{r}
	h.withNames(ctx, req, rows)
	cancel()
	body, err := render("message", rows[0])
	if err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

// ErrMayNotReply is SayInThread's answer when the Gate does not let the viewer
// reply in the thread's channel.
var ErrMayNotReply = errors.New("commons: the viewer may not reply in that thread")

// SayInThread writes text from the viewer into the thread bead id belongs to
// (Locate's thread: an agent's is its gardener thread) and returns the new
// message's id. It is how a nudge from the Glass shows in the conversation
// (Lands workbench-glass GL-7, Commons D6): the Glass has delivered the words
// to the agent itself, so the message is marked with origin, not
// origin=commons, and carries no Slack thread binding, so neither the bridge's
// inbound sweep nor its thread forward hands it to the agent a second time.
// The Gate decides as for a reply (ErrMayNotReply); a bead with no thread the
// viewer may see is IsNotFound.
func (h *Handler) SayInThread(r *http.Request, id, text, origin string) (string, error) {
	if h.Beads == nil || h.Writes == nil {
		return "", errNotFound
	}
	if origin == "" || origin == originCommons {
		return "", fmt.Errorf("commons: SayInThread needs an origin other than %q", originCommons)
	}
	if text = strings.TrimSpace(text); text == "" || len(text) > textMaxBytes {
		return "", errors.New("commons: the message is empty or too long")
	}
	ctx, cancel := context.WithTimeout(r.Context(), writeTimeout)
	defer cancel()
	sc := h.scope(r)
	root, ch, err := locate(ctx, h.Beads, sc, id)
	if err != nil {
		return "", err
	}
	if head, ok := promotedHeadOf(ctx, h.Beads, root); ok {
		root = head
	}
	if !h.may(r, ch, ActionReply) {
		return "", ErrMayNotReply
	}
	author := h.self(r)
	raw, _ := json.Marshal(map[string]string{ // a map[string]string always marshals
		fieldChannelID: ch.ID, fieldThreadID: root.ID, fieldAuthor: author, fieldKind: "text", fieldOrigin: origin, fieldText: text,
	})
	return h.Writes.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     clipBytes(text, titleMax),
		Type:      typeMessage,
		Kind:      "data",
		Status:    "closed",
		Labels:    []string{"project:" + ch.Land},
		CreatedBy: author,
		Fields:    raw,
	})
}
