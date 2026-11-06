package commons

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Place is where a bead's conversation is in the Commons: a thread of a
// channel in a land. It is how the retired /agents/chat routes and the porch
// chat find an agent's thread (Lands Commons I step 10, gd-EwISaXh6Xk).
type Place struct {
	Land    string `json:"land"`
	Channel string `json:"channel"` // the channel's name, as the pane's URLs carry it
	Thread  string `json:"thread"`  // the thread head's id
	Href    string `json:"href"`    // the thread's URL under the base path
	// Reply and Note are the thread's reply composer as its fragment offers it (composeFor), sent on the JSON answer
	// only: Reply is the POST target when the viewer may post there, Note the line the fragment shows above the
	// composer (a frozen land) or in its place. The bead panel's conversation posts through them (gardenhub
	// web/workbench.js, gd-tx2wMH48MV), so it writes where and as the thread pane does.
	Reply string `json:"reply,omitempty"`
	Note  string `json:"note,omitempty"`
}

// Locate returns the thread a bead belongs to, read on the request's bearer
// and cut by the viewer's scope:
//   - a message or card: the thread it replies in, else the thread it roots;
//   - a wish head: its own thread;
//   - any other bead bound to a thread (an agent, a thread's task): its
//     thread_id, else an agent's legacy slack_thread_bead_id;
//   - a bead bound to no thread that a card shows (a decision, a permission,
//     an escalation): the place of its first card the viewer may see, so a
//     push about it lands on the card (Commons on a phone M6).
//
// A bead bound to no thread, or to one in no channel, has no place: errNotFound.
func (h *Handler) Locate(r *http.Request, id string) (Place, error) {
	p, _, err := h.locatePlace(r, id)
	return p, err
}

func (h *Handler) locatePlace(r *http.Request, id string) (Place, Channel, error) {
	if h.Beads == nil {
		return Place{}, Channel{}, errNotFound
	}
	root, ch, err := locate(r.Context(), h.Beads, h.scope(r), id)
	if err != nil {
		return Place{}, Channel{}, err
	}
	return Place{
		Land: ch.Land, Channel: ch.Name, Thread: root.ID,
		Href: h.BasePath + "/garden/c/" + url.PathEscape(ch.Land) + "/" + url.PathEscape(ch.Name) + "/t/" + url.PathEscape(root.ID),
	}, ch, nil
}

// replyOf is the reply composer of ch's thread as the "compose" template draws it: the action when the viewer may
// post, and the note it shows (the frozen banner over it, or why there is none).
func (h *Handler) replyOf(r *http.Request, ch Channel, thread string) (action, note string) {
	c := h.composeFor(r, ch, thread)
	a := c.Access
	switch {
	case a.Post && a.Frozen:
		return c.Action, a.FrozenNote
	case a.Post:
		return c.Action, ""
	case a.Frozen:
		return "", FrozenNote
	case a.Join:
		return "", "You are not in " + channelLabel(ch) + ". Join it to post here."
	case a.Self == "":
		return "", "Sign in to post here."
	case a.MembersOnly:
		return "", "Only the members of " + channelLabel(ch) + " post here."
	}
	return "", "You can read " + channelLabel(ch) + " but not post in it."
}

func locate(ctx context.Context, rd Reader, sc Scope, id string) (Message, Channel, error) {
	b, err := rd.GetBead(ctx, id)
	if err != nil {
		return Message{}, Channel{}, readErr("bead "+id, err)
	}
	if b == nil {
		return Message{}, Channel{}, errNotFound
	}
	head := firstNonEmpty(b.Fields[fieldThreadID], b.Fields["slack_thread_bead_id"])
	if m, ok := messageFromBead(b); ok && m.Thread == "" {
		head = m.ID
	}
	if head == "" {
		return locateCard(ctx, rd, sc, b.ID)
	}
	if head != b.ID {
		if b, err = rd.GetBead(ctx, head); err != nil {
			return Message{}, Channel{}, readErr("thread "+head, err)
		}
		if b == nil {
			return Message{}, Channel{}, errNotFound
		}
	}
	root, ok := messageFromBead(b)
	if !ok || root.Thread != "" || !sc.Allows(root.Land) {
		return Message{}, Channel{}, errNotFound
	}
	ch, err := readChannel(ctx, rd, sc, root.Land, root.Channel)
	if err != nil {
		return Message{}, Channel{}, err
	}
	return root, ch, nil
}

// locateCard returns the place of the first card of subject that has one the
// viewer may see. A decision delivered DM-first has a card in its channel and
// one for the DM copy (internal/bridge/decision_cards.go); the DM copy is in no
// Commons channel, so it never wins.
func locateCard(ctx context.Context, rd Reader, sc Scope, subject string) (Message, Channel, error) {
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{typeMessage},
		Fields: map[string]string{fieldKind: kindCard, fieldSubjectID: subject},
		Limit:  20,
	})
	if err != nil {
		return Message{}, Channel{}, readErr("cards of "+subject, err)
	}
	for _, c := range res.Beads {
		if c == nil || c.Fields[fieldSubjectID] != subject {
			continue
		}
		if root, ch, err := locate(ctx, rd, sc, c.ID); err == nil {
			return root, ch, nil
		}
	}
	return Message{}, Channel{}, errNotFound
}

// place serves GET /garden/c/bead/{id}: where the bead's conversation is. A
// client that asks for JSON gets the Place (the porch chat opens the pane
// with it); anything else is sent there, so a link to an agent lands in its
// thread.
func (h *Handler) place(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 200 {
		http.Error(w, "bad bead id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	p, ch, err := h.locatePlace(r.WithContext(ctx), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Add("Vary", "Accept")
	w.Header().Set("Cache-Control", "no-store")
	if !strings.Contains(r.Header.Get("Accept"), "application/json") {
		http.Redirect(w, r, p.Href, http.StatusSeeOther)
		return
	}
	p.Reply, p.Note = h.replyOf(r, ch, p.Thread)
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, p)
}
