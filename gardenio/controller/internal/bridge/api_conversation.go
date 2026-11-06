package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/oidcauth"
)

// maxInboundBody bounds a POST /api/conversation/inbound body: it names a bead,
// it does not carry one. A lightbox's answer in the person's own words is the
// largest part (10000 characters, as a message).
const maxInboundBody = 64 << 10

// ConversationInboundAPI serves POST /api/conversation/inbound, the Commons
// adapter's way into the dispatcher (design spec
// 2026-10-19-lands-commons-design.md, section 3.4). The Commons writes the
// human's message bead on the human's own bearer, then calls this with its id;
// the bridge reads everything else (land, thread, text, author) off that bead
// rather than trusting the request, checks that the caller wrote it, and runs
// HandleReply. A reply the call never delivers is picked up by the sweep in
// inbound_reply.go.
//
// Card buttons and lightbox submissions (slice 4a, kinds "action" and
// "submit") name the card bead they sat on. The bridge reads the card, its
// land and its subject as itself, maps the button to its action-table entry
// (CommonsCardAction) and checks it against the caller's role in the card's
// land, read off their verified token (landRoleFromToken), before it does
// anything as the fleet; then HandleAction or HandleSubmit runs on the
// person's own bearer, which kbeads checks again.
//
// A mention (slice 4d, kind "compose") names the message bead that mentions
// @gardenio or an agent of its land. The bridge reads it, checks that the
// caller wrote it, and checks "spawn" against the caller's role in its land
// ("agent_control" too for kill, stop, restart and clear) before HandleCompose
// runs on the person's own bearer.
//
// bridgeRBAC admits any signed-in person here; this handler gates every kind.
// A plain reply stays the operator's here: the Commons never sends one (the
// watchers and the sweep deliver replies), and only a mention spawns. The
// beads-viewer forwards the signed-in person's bearer.
type ConversationInboundAPI struct {
	bot    *Bot
	logger *slog.Logger
}

// NewConversationInboundAPI returns the inbound API over bot.
func NewConversationInboundAPI(bot *Bot, logger *slog.Logger) *ConversationInboundAPI {
	if logger == nil {
		logger = slog.Default()
	}
	return &ConversationInboundAPI{bot: bot, logger: logger}
}

// RegisterRoutes registers the inbound route on mux.
func (a *ConversationInboundAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/conversation/inbound", a.handleInbound)
}

// conversationInboundRequest names the Commons bead to dispatch: the message
// of a reply, the card of an action or a submission.
type conversationInboundRequest struct {
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
	// Card, Action, Value and Values are an action's or a submission's: the
	// card bead, the button ("resolve", "allow", "kill", ...) or the lightbox
	// ("other", "promote"), the button's value and the lightbox's fields.
	Card   string            `json:"card,omitempty"`
	Action string            `json:"action,omitempty"`
	Value  string            `json:"value,omitempty"`
	Values map[string]string `json:"values,omitempty"`
	// Reaction is a react's (slice 4b): "done" on the wish head Message.
	Reaction string `json:"reaction,omitempty"`
}

// conversationInboundResponse is the dispatch outcome: "handled", or
// "duplicate" when the message was already dispatched. Ephemeral carries any
// answer for the caller to show the human alone.
type conversationInboundResponse struct {
	Status    string   `json:"status"`
	Ephemeral []string `json:"ephemeral,omitempty"`
}

func (a *ConversationInboundAPI) handleInbound(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		writeInboundError(w, http.StatusUnauthorized, "no authenticated principal")
		return
	}
	var req conversationInboundRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxInboundBody)).Decode(&req); err != nil {
		writeInboundError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if a.bot == nil || a.bot.daemon == nil {
		writeInboundError(w, http.StatusServiceUnavailable, "bridge has no daemon")
		return
	}
	switch conversation.InboundKind(req.Kind) {
	case conversation.InboundReply:
	case conversation.InboundAction, conversation.InboundSubmit:
		a.handleCard(w, r, email, req)
		return
	case conversation.InboundReact:
		a.handleReact(w, r, email, req)
		return
	case conversation.InboundCompose:
		a.handleCompose(w, r, email, req)
		return
	default:
		writeInboundError(w, http.StatusUnprocessableEntity, "kind must be \"reply\", \"compose\", \"action\", \"submit\" or \"react\"")
		return
	}
	if !oidcauth.HasMinimumRole(oidcauth.RolesFromContext(r.Context()), oidcauth.RoleAdmin) {
		writeInboundError(w, http.StatusForbidden, "replies from the Commons are the operator's until spawns are gated by land role")
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		writeInboundError(w, http.StatusBadRequest, "message is required")
		return
	}

	ctx := r.Context()
	detail, err := a.bot.daemon.GetBead(ctx, req.Message)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			writeInboundError(w, http.StatusNotFound, "message not found")
			return
		}
		a.logger.Error("conversation inbound: reading message bead", "message", req.Message, "error", err)
		writeInboundError(w, http.StatusBadGateway, "failed to read message")
		return
	}
	m := commonsMessageFromDetail(detail)
	if !m.fromCommons() {
		writeInboundError(w, http.StatusUnprocessableEntity, "not a Commons message")
		return
	}
	if !strings.EqualFold(m.author(), "oidc:"+email) {
		a.logger.Warn("conversation inbound: caller is not the message author",
			"message", m.ID, "caller", email, "author", m.author())
		writeInboundError(w, http.StatusForbidden, "message was written by someone else")
		return
	}

	// The handlers act for the human, as the Slack adapter's do, and finish
	// even if the caller hangs up: a respawn cut off halfway helps nobody.
	ctx = beadsapi.WithOperator(context.WithoutCancel(ctx), "oidc:"+email)
	// The caller is an operator, whom no land role holds back: their reply
	// may spawn, as their mention in Slack would.
	answer, dispatched, err := a.bot.dispatchCommonsAs(ctx, m, conversation.InboundReply, true)
	if errors.Is(err, errCommonsNotHuman) {
		writeInboundError(w, http.StatusForbidden, "message was written by someone else")
		return
	}
	if errors.Is(err, conversation.ErrNoThread) {
		writeInboundError(w, http.StatusUnprocessableEntity, "message is not in a thread bound to an agent")
		return
	}
	if err != nil {
		writeInboundError(w, http.StatusInternalServerError, "dispatch failed")
		return
	}
	resp := conversationInboundResponse{Status: "handled"}
	if !dispatched {
		resp.Status = "duplicate"
	}
	for _, e := range answer.Ephemeral {
		resp.Ephemeral = append(resp.Ephemeral, e.Text)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleCard runs a Commons card button or lightbox submission for the person
// email (section 6.3): the card is read as the bridge, and the action is
// checked against the person's role in its land before anything runs.
func (a *ConversationInboundAPI) handleCard(w http.ResponseWriter, r *http.Request, email string, req conversationInboundRequest) {
	req.Card, req.Action = strings.TrimSpace(req.Card), strings.TrimSpace(req.Action)
	if req.Card == "" || req.Action == "" || len(req.Card) > 200 || len(req.Action) > 64 {
		writeInboundError(w, http.StatusBadRequest, "card and action are required")
		return
	}
	kind := conversation.InboundKind(req.Kind)
	card, err := a.bot.readCommonsCard(r.Context(), req.Card)
	if err != nil {
		a.writeCardError(w, req, err)
		return
	}
	action, ok := CommonsCardAction(kind, card.Subject.Type, req.Action)
	if !ok {
		writeInboundError(w, http.StatusUnprocessableEntity, "this card has no such "+req.Kind)
		return
	}
	role := landRoleFromToken(oidcauth.RolesFromContext(r.Context()), oidcauth.GroupsFromContext(r.Context()), card.Land)
	if !conversation.Allows(role, action) {
		a.logger.Info("conversation inbound: action refused by land role",
			"card", card.ID, "land", card.Land, "action", string(action), "role", string(role), "caller", email)
		writeInboundError(w, http.StatusForbidden, "Your role in "+card.Land+" does not let you do that.")
		return
	}

	// The handlers act for the person, on their own bearer where the daemon
	// client prefers it, and finish even if the caller hangs up.
	ctx := beadsapi.WithOperator(context.WithoutCancel(r.Context()), "oidc:"+email)
	ctx = beadsapi.WithUserBearer(ctx, requestBearer(r))
	answer, err := a.bot.inbound().Dispatch(ctx, conversation.Inbound{
		Kind:     kind,
		Surface:  conversation.SurfaceCommons,
		Land:     card.Land,
		Channel:  card.Channel,
		Message:  card.ID,
		Actor:    conversation.Principal{ID: "oidc:" + email},
		ActionID: req.Action,
		Value:    req.Value,
		Values:   req.Values,
		At:       conversation.Ref{Surface: conversation.SurfaceCommons, Channel: card.Channel, TS: card.ID},
		Trigger:  conversation.Trigger{Surface: conversation.SurfaceCommons, Channel: card.Channel},
	})
	if err != nil {
		a.writeCardError(w, req, err)
		return
	}
	resp := conversationInboundResponse{Status: "handled"}
	for _, e := range answer.Ephemeral {
		resp.Ephemeral = append(resp.Ephemeral, e.Text)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleReact runs a Commons reaction for the person email: today only a wish
// head's Done (slice 4b). The head is read as the bridge, and "done" is checked
// against the person's role in its land before anything runs; the close then
// runs on the person's own bearer, which kbeads checks again.
func (a *ConversationInboundAPI) handleReact(w http.ResponseWriter, r *http.Request, email string, req conversationInboundRequest) {
	req.Message, req.Reaction = strings.TrimSpace(req.Message), strings.TrimSpace(req.Reaction)
	if req.Message == "" || len(req.Message) > 200 {
		writeInboundError(w, http.StatusBadRequest, "message is required")
		return
	}
	if req.Reaction != conversation.ReactionDone {
		writeInboundError(w, http.StatusUnprocessableEntity, "reaction must be \"done\"")
		return
	}
	head, land, err := a.bot.readCommonsWish(r.Context(), req.Message)
	if err != nil {
		a.writeReactError(w, req, err)
		return
	}
	role := landRoleFromToken(oidcauth.RolesFromContext(r.Context()), oidcauth.GroupsFromContext(r.Context()), land)
	if !conversation.Allows(role, conversation.ActionDone) {
		a.logger.Info("conversation inbound: done refused by land role",
			"head", head.ID, "land", land, "role", string(role), "caller", email)
		writeInboundError(w, http.StatusForbidden, "Your role in "+land+" does not let you do that.")
		return
	}

	ctx := beadsapi.WithOperator(context.WithoutCancel(r.Context()), "oidc:"+email)
	ctx = beadsapi.WithUserBearer(ctx, requestBearer(r))
	answer, err := a.bot.inbound().Dispatch(ctx, conversation.Inbound{
		Kind:     conversation.InboundReact,
		Surface:  conversation.SurfaceCommons,
		Land:     land,
		Channel:  head.Fields[conversation.BeadChannelID],
		Thread:   head.ID,
		Message:  head.ID,
		Actor:    conversation.Principal{ID: "oidc:" + email},
		Reaction: req.Reaction,
		At:       conversation.Ref{Surface: conversation.SurfaceCommons, Channel: head.Fields[conversation.BeadChannelID], TS: head.ID},
		Trigger:  conversation.Trigger{Surface: conversation.SurfaceCommons, Channel: head.Fields[conversation.BeadChannelID]},
	})
	if err != nil {
		a.writeReactError(w, req, err)
		return
	}
	resp := conversationInboundResponse{Status: "handled"}
	for _, e := range answer.Ephemeral {
		resp.Ephemeral = append(resp.Ephemeral, e.Text)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleCompose runs a Commons mention for the person email (slice 4d). The
// message is read as the bridge; its writer must be the caller, and its land
// must let the caller spawn (and control agents, for a verb) before anything
// runs. HandleCompose then runs on the person's own bearer, which kbeads checks
// again (the spawn gate, scopedTierDenial).
func (a *ConversationInboundAPI) handleCompose(w http.ResponseWriter, r *http.Request, email string, req conversationInboundRequest) {
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" || len(req.Message) > 200 {
		writeInboundError(w, http.StatusBadRequest, "message is required")
		return
	}
	ctx := r.Context()
	detail, err := a.bot.daemon.GetBead(ctx, req.Message)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			writeInboundError(w, http.StatusNotFound, "message not found")
			return
		}
		a.logger.Error("conversation inbound: reading mention", "message", req.Message, "error", err)
		writeInboundError(w, http.StatusBadGateway, "failed to read message")
		return
	}
	m := commonsMessageFromDetail(detail)
	if m.Fields[conversation.BeadOrigin] != conversation.SurfaceCommons || m.Type != "message" {
		writeInboundError(w, http.StatusUnprocessableEntity, "not a Commons message")
		return
	}
	if !strings.EqualFold(m.author(), "oidc:"+email) || !m.writtenByAuthor() {
		a.logger.Warn("conversation inbound: caller is not the mention's author",
			"message", m.ID, "caller", email, "author", m.author())
		writeInboundError(w, http.StatusForbidden, "message was written by someone else")
		return
	}
	land, ok := onlyLand(m.Labels)
	if !ok {
		writeInboundError(w, http.StatusUnprocessableEntity, "That message belongs to no single land.")
		return
	}
	role := landRoleFromToken(oidcauth.RolesFromContext(ctx), oidcauth.GroupsFromContext(ctx), land)
	need := []conversation.Action{conversation.ActionSpawn}
	verb, _ := commonsVerb(m.Fields[conversation.BeadText])
	if verb != "" {
		need = append(need, conversation.ActionAgentControl)
	}
	dispatch := a.bot.dispatchCommonsCompose
	var note string
	for _, action := range need {
		if conversation.Allows(role, action) {
			continue
		}
		// A guest may reply but not spawn (landrole.go): their mention in a
		// thread or a DM is the reply it was before slice 4d. A verb, or a
		// top-level mention, has no reply to fall back to.
		if verb == "" && conversation.Allows(role, conversation.ActionReply) && a.bot.commonsRepliable(ctx, m) {
			dispatch = a.bot.dispatchCommonsMessage
			if a.bot.resolveCommonsMention(ctx, land, m.Fields[conversation.BeadText]).any() {
				note = "Your role in " + land + " does not let you start or call on agents, so your message went to the thread as a reply."
			}
			break
		}
		a.logger.Info("conversation inbound: mention refused by land role",
			"message", m.ID, "land", land, "action", string(action), "role", string(role), "caller", email)
		if verb == "" && !a.bot.resolveCommonsMention(ctx, land, m.Fields[conversation.BeadText]).any() {
			writeInboundError(w, http.StatusUnprocessableEntity, "That message names no agent.")
			return
		}
		writeInboundError(w, http.StatusForbidden, "Your role in "+land+" does not let you start or control agents.")
		return
	}

	// The handlers act for the person, on their own bearer, and finish even if
	// the caller hangs up: a spawn cut off halfway helps nobody.
	ctx = beadsapi.WithOperator(context.WithoutCancel(ctx), "oidc:"+email)
	ctx = beadsapi.WithUserBearer(ctx, requestBearer(r))
	answer, dispatched, err := dispatch(ctx, m)
	if note != "" && err == nil {
		answer.Ephemeral = append(answer.Ephemeral, conversation.Message{Text: note})
	}
	if err != nil {
		var refusal *InboundRefusal
		switch {
		case errors.As(err, &refusal):
			writeInboundError(w, refusal.Status, refusal.Message)
		case errors.Is(err, errCommonsNotHuman):
			writeInboundError(w, http.StatusForbidden, "message was written by someone else")
		default:
			a.logger.Error("conversation inbound: mention failed", "message", m.ID, "error", err)
			writeInboundError(w, http.StatusInternalServerError, "That did not work. Try again.")
		}
		return
	}
	resp := conversationInboundResponse{Status: "handled"}
	if !dispatched {
		resp.Status = "duplicate"
	}
	for _, e := range answer.Ephemeral {
		resp.Ephemeral = append(resp.Ephemeral, e.Text)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// writeReactError answers a reaction that did not run: a refusal with its
// status and words, anything else as a 500.
func (a *ConversationInboundAPI) writeReactError(w http.ResponseWriter, req conversationInboundRequest, err error) {
	var refusal *InboundRefusal
	if errors.As(err, &refusal) {
		writeInboundError(w, refusal.Status, refusal.Message)
		return
	}
	a.logger.Error("conversation inbound: reaction failed", "message", req.Message, "reaction", req.Reaction, "error", err)
	writeInboundError(w, http.StatusInternalServerError, "That did not work. Try again.")
}

// writeCardError answers a card action that did not run: a refusal with its
// status and words, anything else as a 500.
func (a *ConversationInboundAPI) writeCardError(w http.ResponseWriter, req conversationInboundRequest, err error) {
	var refusal *InboundRefusal
	if errors.As(err, &refusal) {
		writeInboundError(w, refusal.Status, refusal.Message)
		return
	}
	a.logger.Error("conversation inbound: card action failed", "card", req.Card, "action", req.Action, "error", err)
	writeInboundError(w, http.StatusInternalServerError, "That did not work. Try again.")
}

// requestBearer is the caller's bearer token: Authorization, else the edge's
// X-Auth-Request-Access-Token.
func requestBearer(r *http.Request) string {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(tok)
	}
	return strings.TrimSpace(r.Header.Get("X-Auth-Request-Access-Token"))
}

func writeInboundError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
