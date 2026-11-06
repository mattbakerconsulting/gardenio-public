package commons

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// The Done button (Commons II step 6, slice 4b): an open wish head offers Done,
// the Commons twin of the Slack ⏹️ reaction on a thread card. It closes the
// wish. The close is the bridge's (internal/bridge/inbound_react.go), which
// runs the same wish-close core as the Slack reaction: one close reason, the
// same refusal of a wish held for triage. Both arrive there as
// conversation.Inbound{React}.
//
// The route is POST /garden/c/t/{thread}/done. The head is read on the
// viewer's bearer and checked against the land-role action table ("done")
// here, and again by the bridge, which closes it on the viewer's bearer.

// actionDone is the action-table entry Done needs (conversation.ActionDone).
const actionDone = "done"

// ReactionDone is the reaction a Done sends the bridge
// (conversation.ReactionDone).
const ReactionDone = "done"

// Reactor carries a reaction on a message or a wish head to the bridge.
// NewBridgeReactor is the HTTP one beads-viewer builds.
type Reactor interface {
	React(ctx context.Context, req ReactRequest) (CardAnswer, error)
}

// ReactRequest is one reaction for the bridge's POST
// /api/conversation/inbound.
type ReactRequest struct {
	Kind     string `json:"kind"` // "react"
	Message  string `json:"message"`
	Reaction string `json:"reaction"`
}

// NewBridgeReactor returns a Reactor that calls the bridge at base (its
// service URL); nil when base is empty.
func NewBridgeReactor(base string, client *http.Client) Reactor {
	in, ok := NewBridgeInbound(base, client).(*bridgeInbound)
	if !ok {
		return nil
	}
	return in
}

func (b *bridgeInbound) React(ctx context.Context, req ReactRequest) (CardAnswer, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return CardAnswer{}, err
	}
	return b.send(ctx, bytes.NewReader(body))
}

// done serves POST /garden/c/t/{thread}/done: the viewer marks the wish done.
// The answer is the head as it now renders, with the bridge's words in
// X-Commons-Note.
func (h *Handler) done(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("thread"))
	if id == "" || len(id) > 200 {
		http.Error(w, "bad thread id", http.StatusBadRequest)
		return
	}
	if h.self(r) == "" {
		http.Error(w, "Sign in to mark a wish done.", http.StatusUnauthorized)
		return
	}
	if crossOrigin(r) {
		http.Error(w, "Done must come from this page.", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), cardTimeout)
	defer cancel()
	m, err := readMessage(ctx, h.Beads, h.scope(r), id)
	if err == nil && !m.Head {
		err = errNotFound
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	if m.Status == "closed" {
		http.Error(w, "This wish is done already.", http.StatusConflict)
		return
	}
	if h.frozenFor(r, m.Land) {
		http.Error(w, FrozenNote, http.StatusForbidden)
		return
	}
	if h.Gate.MayAct == nil || !h.Gate.MayAct(r, m.Land, actionDone) {
		http.Error(w, "Your role in "+m.Land+" does not let you do that.", http.StatusForbidden)
		return
	}
	if h.React == nil {
		http.Error(w, errActionsOff, http.StatusNotImplemented)
		return
	}
	a, err := h.React.React(ctx, ReactRequest{Kind: "react", Message: m.ID, Reaction: ReactionDone})
	if err != nil {
		var refusal *CardRefusal
		if errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500 {
			http.Error(w, refusal.Message, refusal.Status)
			return
		}
		slog.Default().Warn("commons: done failed", "head", id, "error", err)
		http.Error(w, "That did not go through. Try again.", http.StatusBadGateway)
		return
	}

	m, err = readMessage(ctx, h.Beads, h.scope(r), id)
	if err == nil {
		err = countReplies(ctx, h.Beads, &m)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	if note := cardNote(a.Notes); note != "" {
		w.Header().Set("X-Commons-Note", url.PathEscape(note))
	}
	rows := []row{newRow(m, h.BasePath, m.Channel)}
	h.withDone(r, rows)
	h.write(w, "message", rows[0])
}

// withDone gives each open wish head among rows its Done button, when the
// viewer may mark it done: signed in, the land's action table admits "done",
// the land is not frozen for them, and the Commons reaches the bridge.
func (h *Handler) withDone(r *http.Request, rows []row) {
	if h.React == nil || h.Gate.MayAct == nil || h.self(r) == "" {
		return
	}
	may := map[string]bool{}
	for i := range rows {
		m := rows[i].M
		if !m.Head || m.Status == "closed" || m.Deleted {
			continue
		}
		ok, seen := may[m.Land]
		if !seen {
			ok = h.Gate.MayAct(r, m.Land, actionDone) && !h.frozenFor(r, m.Land)
			may[m.Land] = ok
		}
		if ok {
			rows[i].Done = h.BasePath + "/garden/c/t/" + url.PathEscape(m.ID) + "/done"
		}
	}
}
