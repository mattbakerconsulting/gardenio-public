package commons

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Card buttons and lightboxes (D12, spec 6.3; Commons II step 5, slice 4a). A
// decision card's Other and Escalate open a server-rendered lightbox; its
// options and Dismiss, a permission card's Allow and Deny, an escalation's
// Approve and Reject and a live agent card's Restart and Kill post to the card
// action route, and a lightbox's form to the submit route. Both act on the card
// bead, read on the viewer's bearer, so the action is checked against the
// card's land: the land-role action table (Gate.MayAct) first, then the
// daemon again on the viewer's own bearer.
//
// What needs the fleet (a decision's close and the nudge to its agent, a
// permission verdict, a kill or a restart) goes to the bridge's inbound
// endpoint, which reads the card itself and checks the same table (Inbound).
// An escalation's Approve and Reject need nothing of the fleet: they are the
// daemon's own escalation routes, called here on the viewer's bearer as the
// bead page calls them (Escalations). The answer is the card as it now
// renders, with the bridge's words for the viewer in X-Commons-Note.

// lightboxKinds are the lightboxes a decision card opens: Other (a written
// answer) and Escalate (hand the decision back to its agent to file a
// structured escalation, the Slack card's "Escalate (Agent)").
var lightboxKinds = map[string]bool{"other": true, "promote": true}

// errActionsOff is what the card action and submit routes answer when the
// viewer has no way to the bridge (beads-viewer's SLACK_BRIDGE_URL unset).
const errActionsOff = "Answering cards from the Commons is not switched on yet. Answer this one in Slack or the Mailbox for now."

// lightbox serves GET /garden/c/m/{id}/lightbox/{kind}: the Other or Escalate
// form of decision card id, read on the viewer's bearer. (The spec's
// /garden/c/lightbox/{kind}/{id} would overlap /garden/c/{land}/{channel}/presence
// in the mux, so the lightbox hangs off its card's message route.)
func (h *Handler) lightbox(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	id, kind := strings.TrimSpace(r.PathValue("id")), r.PathValue("kind")
	if id == "" || len(id) > 200 || !lightboxKinds[kind] {
		http.Error(w, "bad lightbox", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	m, err := readMessage(ctx, h.Beads, h.scope(r), id)
	if err == nil && (m.Kind != kindCard || m.SubjectID == "" || m.Deleted) {
		err = errNotFound
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	b, err := h.Beads.GetBead(ctx, m.SubjectID)
	if err != nil {
		h.fail(w, readErr("subject "+m.SubjectID, err))
		return
	}
	d, ok := decisionCardFrom(b, m.Land)
	if !ok {
		h.fail(w, errNotFound)
		return
	}
	if d.Open {
		d.Evidence = h.decisionEvidence(ctx, b, m.Land, "lb")
	}
	rows := []row{{M: m, Decision: &d}}
	h.withNames(ctx, r, rows)
	h.write(w, "lightbox", struct {
		Base, Card, Kind string
		D                DecisionCard
		Names            beadNames
	}{h.BasePath, m.ID, kind, d, rows[0].Names})
}

// Card action names, as the bridge's inbound endpoint and the forms carry
// them, and the action-table entries (conversation.Action) they need.
const (
	cardKindAction = "action"
	cardKindSubmit = "submit"
)

// CardVerb is the action-table entry a button (kind action) or a lightbox
// (kind submit) named action takes on a subject of subjectType; "" for one no
// card offers.
func CardVerb(kind, subjectType, action string) string {
	switch kind + " " + subjectType + " " + action {
	case "action decision resolve", "submit decision other":
		return "decision_respond"
	case "action decision dismiss":
		return "decision_dismiss"
	case "submit decision promote":
		return "decision_promote"
	case "action permission allow", "action permission deny":
		return "permission_answer"
	case "action escalation approve", "action escalation reject":
		return "escalation_answer"
	case "action agent kill", "action agent restart":
		return "agent_control"
	}
	return ""
}

// Inbound carries a card button or a lightbox's form to the bridge.
// NewBridgeInbound is the HTTP one beads-viewer builds.
type Inbound interface {
	Card(ctx context.Context, req CardRequest) (CardAnswer, error)
}

// CardRequest is one card action for the bridge's POST
// /api/conversation/inbound.
type CardRequest struct {
	Kind   string            `json:"kind"` // "action" or "submit"
	Card   string            `json:"card"`
	Action string            `json:"action"`
	Value  string            `json:"value,omitempty"`
	Values map[string]string `json:"values,omitempty"`
}

// CardAnswer is the bridge's answer: words for the viewer alone.
type CardAnswer struct {
	Notes []string `json:"ephemeral"`
}

// CardRefusal is a card action refused, by the bridge or the daemon: its
// status and the words the viewer sees.
type CardRefusal struct {
	Status  int
	Message string
}

func (e *CardRefusal) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Message) }

// Escalations approves and rejects an escalation as the viewer, on their own
// bearer (the request's context).
type Escalations interface {
	Approve(r *http.Request, id string) error
	Reject(r *http.Request, id, reason string) error
}

// Approvals is the daemon's dry run of its approve checks for the viewer.
type Approvals interface {
	CanApproveEscalation(ctx context.Context, id string) (beadsapi.CanApproveResult, error)
}

// cardTimeout bounds a card action: the bridge's verdict delivery and nudges
// take their own time on top of the reads.
const cardTimeout = 30 * time.Second

// cardAction serves POST /garden/c/action/{id}: a card's button, named by the
// form's action (and value: a decision's option number).
func (h *Handler) cardAction(w http.ResponseWriter, r *http.Request) {
	h.cardAct(w, r, cardKindAction)
}

// cardSubmit serves POST /garden/c/submit/{id}: a lightbox's form, named by
// its kind (other, promote), with the person's words in text.
func (h *Handler) cardSubmit(w http.ResponseWriter, r *http.Request) {
	h.cardAct(w, r, cardKindSubmit)
}

func (h *Handler) cardAct(w http.ResponseWriter, r *http.Request, kind string) {
	if !h.ready(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 200 {
		http.Error(w, "bad card id", http.StatusBadRequest)
		return
	}
	if h.self(r) == "" {
		http.Error(w, "Sign in to answer cards.", http.StatusUnauthorized)
		return
	}
	// A card button kills, restarts and allows on the viewer's cookie, so
	// another page (even one on this site) must not be able to post it.
	if crossOrigin(r) {
		http.Error(w, "A card action must come from this page.", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, formMaxBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	req := CardRequest{Kind: kind, Card: id, Action: strings.TrimSpace(r.PostForm.Get("action")), Value: strings.TrimSpace(r.PostForm.Get("value"))}
	if kind == cardKindSubmit {
		req.Action = strings.TrimSpace(r.PostForm.Get("kind"))
		if req.Action == "other" {
			text := strings.TrimSpace(r.PostForm.Get("text"))
			if !checkText(w, text) {
				return
			}
			req.Values = map[string]string{"text": text}
		}
	}
	if kind == cardKindAction && req.Action == "dismiss" {
		// a decision's Dismiss may say why (optional, unlike Other's own words)
		if reason := strings.TrimSpace(r.PostForm.Get("reason")); reason != "" {
			if !checkText(w, reason) {
				return
			}
			req.Values = map[string]string{"reason": reason}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), cardTimeout)
	defer cancel()
	m, err := readMessage(ctx, h.Beads, h.scope(r), id)
	if err == nil && (m.Kind != kindCard || m.SubjectID == "" || m.Deleted) {
		err = errNotFound
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	subject, err := h.Beads.GetBead(ctx, m.SubjectID)
	if err == nil && subject == nil {
		err = errNotFound
	}
	if err != nil {
		h.fail(w, readErr("subject "+m.SubjectID, err))
		return
	}
	verb := CardVerb(kind, subject.Type, req.Action)
	if verb == "" {
		http.Error(w, "This card has no such button.", http.StatusBadRequest)
		return
	}
	if h.frozenFor(r, m.Land) {
		http.Error(w, FrozenNote, http.StatusForbidden)
		return
	}
	if h.Gate.MayAct == nil || !h.Gate.MayAct(r, m.Land, verb) {
		http.Error(w, "Your role in "+m.Land+" does not let you do that.", http.StatusForbidden)
		return
	}

	var notes []string
	switch {
	case subject.Type == "escalation":
		if h.Escalations == nil {
			http.Error(w, errActionsOff, http.StatusNotImplemented)
			return
		}
		if req.Action == "approve" {
			err = h.Escalations.Approve(r.WithContext(ctx), subject.ID)
		} else {
			err = h.Escalations.Reject(r.WithContext(ctx), subject.ID, "Rejected in the Commons")
		}
	case h.Inbound == nil:
		http.Error(w, errActionsOff, http.StatusNotImplemented)
		return
	default:
		var a CardAnswer
		a, err = h.Inbound.Card(ctx, req)
		notes = a.Notes
	}
	if err != nil {
		var refusal *CardRefusal
		if errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500 {
			http.Error(w, refusal.Message, refusal.Status)
			return
		}
		slog.Default().Warn("commons: card action failed", "card", id, "action", req.Action, "error", err)
		http.Error(w, "That did not go through. Try again.", http.StatusBadGateway)
		return
	}

	// The card as it renders now: a decision or a permission is answered
	// already; a kill or a restart changes it later, through the stream.
	m, err = readMessage(ctx, h.Beads, h.scope(r), id)
	if err == nil && m.Thread == "" {
		err = countReplies(ctx, h.Beads, &m)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	if note := cardNote(notes); note != "" {
		w.Header().Set("X-Commons-Note", url.PathEscape(note))
	}
	rows := []row{newRow(m, h.BasePath, m.Channel)}
	h.withCards(ctx, h.self(r), rows)
	h.withNames(ctx, r, rows)
	h.write(w, "message", rows[0])
}

// cardNote is the bridge's words for the viewer as one plain line: its Slack
// emphasis marks dropped, at most 300 characters.
func cardNote(notes []string) string {
	s := strings.Join(notes, " ")
	s = strings.NewReplacer("*", "", "`", "", "\n", " ").Replace(strings.TrimSpace(s))
	return clipBytes(s, 300)
}

// bridgeInbound is the HTTP Inbound: POST <bridge>/api/conversation/inbound
// on the viewer's own bearer (the request context's).
type bridgeInbound struct {
	url    string
	client *http.Client
}

// NewBridgeInbound returns an Inbound that calls the bridge at base (its
// service URL); nil when base is empty.
func NewBridgeInbound(base string, client *http.Client) Inbound {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: cardTimeout}
	}
	return &bridgeInbound{url: base + "/api/conversation/inbound", client: client}
}

func (b *bridgeInbound) Card(ctx context.Context, req CardRequest) (CardAnswer, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return CardAnswer{}, err
	}
	return b.send(ctx, bytes.NewReader(body))
}

// send posts one JSON request body to the bridge's inbound endpoint on the
// viewer's bearer: a card action (Card) or a reaction (React, done.go).
func (b *bridgeInbound) send(ctx context.Context, body io.Reader) (CardAnswer, error) {
	bearer := beadsapi.UserBearerFromContext(ctx)
	if bearer == "" {
		return CardAnswer{}, &CardRefusal{Status: http.StatusUnauthorized, Message: "Your session carries no token for the bridge. Reload the page to sign in again."}
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, body)
	if err != nil {
		return CardAnswer{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := b.client.Do(hr)
	if err != nil {
		return CardAnswer{}, fmt.Errorf("bridge inbound: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return CardAnswer{}, fmt.Errorf("bridge inbound: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden && msg == "insufficient permissions" {
			msg = "The bridge did not accept your sign-in. Reload the page and try again."
		}
		return CardAnswer{}, &CardRefusal{Status: resp.StatusCode, Message: clipBytes(msg, 500)}
	}
	var a CardAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return CardAnswer{}, fmt.Errorf("bridge inbound: decode: %w", err)
	}
	return a, nil
}
