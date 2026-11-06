package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/decisionview"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/spawnguard"
)

// commonsMessagesPerMinuteEnv caps how many messages one member posts in one
// land per minute (Lands Track B §5.4, step 10.4). Unset or unparseable is
// commonsMessagesPerMinuteDefault; 0 turns the cap off.
const (
	commonsMessagesPerMinuteEnv     = "COMMONS_MESSAGES_PER_MINUTE"
	commonsMessagesPerMinuteDefault = 20
)

// commonsMessageRate is the Commons per-member message rate from getenv.
func commonsMessageRate(getenv func(string) string) *spawnguard.Keyed {
	n := commonsMessagesPerMinuteDefault
	if v, err := strconv.Atoi(strings.TrimSpace(getenv(commonsMessagesPerMinuteEnv))); err == nil && v >= 0 {
		n = v
	}
	return spawnguard.NewKeyed(n, time.Minute)
}

// commonsDaemon is what the Commons reads and writes through: the daemon
// client, on the request context.
type commonsDaemon interface {
	commons.Reader
	commons.Writer
}

// newCommonsHandler builds the Commons routes (Lands Commons I steps 8 and 9,
// gd-LCtVzVbuWM, gd-b4IDsCKWbB) over the daemon and hub. Reads and writes go
// out on the request context, so on the viewer's own bearer; the handler's
// land cut is the viewer's scope (commonsScope), its writes are gated by the
// land-role action table (commonsGate), and each event stream ends with the
// bearer it opened on, so the reconnect carries a fresh token and a freshly
// read scope.
//
// page, when set, serves the /garden page for a browser that navigates to a
// channel or thread URL, so the link opens the game with the pane there. A
// scoped viewer gets it too, the same page GET /garden serves them: its lists
// cut to their projects (Hub.CutSnapshot) and read only (gd-l7yYxZpoZo). Its
// land is checked before the handler runs (viewerProjectParams), so another
// land's channel is refused at the gate, never answered with the page. A
// gatehouse guest's "/" lands here, on the gatehouse's #general
// (lands_gatehouse.go, gd-xopPYSZMbG).
//
// presence (Lands Commons III step 4.3) records each stream's viewer as
// commonsViewer names it. Read positions (Commons III step 5, gd-q20kp0MRzh)
// go through the same client when it keeps them, on the same bearer, to the
// viewer's own user bead, and so do a land's people for the @ list (step 6,
// gd-4fmDZb5C52), which kbeads answers only for the viewer's own lands.
func newCommonsHandler(beads commonsDaemon, hub *commons.Hub, presence *commons.Presence, basePath string, page http.HandlerFunc) *commons.Handler {
	cursors, _ := beads.(commons.Cursors)
	people, _ := beads.(commons.People)
	// A daemon client without can-approve leaves Approvals nil: no escalation
	// card offers Approve or Reject.
	approvals, _ := beads.(commons.Approvals)
	// A daemon client without batch reads leaves Nodes nil: the node route
	// answers 503.
	nodes, _ := beads.(commons.Nodes)
	h := &commons.Handler{
		Beads:         beads,
		Nodes:         nodes,
		Evidence:      decisionview.NewEvidenceCache(0),
		Writes:        beads,
		Cursors:       cursors,
		People:        people,
		Approvals:     approvals,
		Hub:           hub,
		Presence:      presence,
		Viewer:        commonsViewer,
		BasePath:      basePath,
		Scope:         commonsScope,
		Gate:          commonsGate,
		EmptyPrompt:   gardenhub.EmptyPrompt,
		StreamContext: ctxUntilBearerExpiry,
		Machine:       commonsMachine,
	}
	if page != nil {
		h.Page = func(w http.ResponseWriter, r *http.Request) bool {
			page(w, r)
			return true
		}
	}
	return h
}

// commonsMachine reports whether machineMiddleware admitted the request as a
// machine principal that may read land's files: the only caller of the
// Commons' machine file route. An agent reads only its own land.
func commonsMachine(r *http.Request, land string) bool {
	c, ok := machineCallerFromContext(r.Context())
	return ok && c.mayRead(land)
}

// commonsScope is the viewer's scope as the Commons reads it: an unrestricted
// caller sees every land, a scoped principal only its projects. The principal
// is the signed-in email as an author field names it.
func commonsScope(r *http.Request) commons.Scope {
	sc := viewerScopeFromContext(r.Context())
	return commons.Scope{Admin: sc.Admin, Lands: sc.Projects, Principal: commonsSelf(r)}
}

// commonsGate is what the viewer may write in the Commons: viewerScope's
// action table and channel membership (spec §2.2.1, §6.4), the same rules
// kbeads applies to the write on the viewer's own bearer.
var commonsGate = commons.Gate{
	Self: commonsSelf,
	May: func(r *http.Request, ch commons.Channel, action string) bool {
		return viewerScopeFromContext(r.Context()).mayInChannel(ch.Land, commonsSelf(r), commonsChannel(ch), conversation.Action(action))
	},
	MayMembers: func(r *http.Request, ch commons.Channel, next []string) bool {
		role := viewerScopeFromContext(r.Context()).landRole(ch.Land)
		return conversation.MembershipChangeAllows(role, commonsSelf(r), commonsChannel(ch), next)
	},
	// A card's buttons and lightboxes: the land-role action table alone,
	// whatever channel the card sits in (Commons II step 5).
	MayAct: func(r *http.Request, land, action string) bool {
		return viewerScopeFromContext(r.Context()).may(land, conversation.Action(action))
	},
}

// commonsBridgeURLEnv is the slack-bridge's service URL, the way to its
// POST /api/conversation/inbound for card buttons and lightboxes (Commons II
// step 5). Unset leaves them answering 501, as before the bridge took them.
const commonsBridgeURLEnv = "SLACK_BRIDGE_URL"

// commonsEscalations approves and rejects an escalation from its Commons card
// as the bead page does (escalation_actions.go): the daemon's can-approve
// (escalationApproveGate), then the daemon's own escalation route on the
// viewer's bearer.
type commonsEscalations struct{ s *Server }

func (e commonsEscalations) Approve(r *http.Request, id string) error {
	return e.act(r, id, "", false)
}

func (e commonsEscalations) Reject(r *http.Request, id, reason string) error {
	return e.act(r, id, reason, true)
}

func (e commonsEscalations) act(r *http.Request, id, reason string, reject bool) error {
	email := oidcauth.EmailFromContext(r.Context())
	bead, err := e.s.daemon.GetBead(r.Context(), id)
	if err != nil {
		return err
	}
	if bead.Type != "escalation" {
		return &commons.CardRefusal{Status: http.StatusBadRequest, Message: "That is not an escalation."}
	}
	if bead.Status != "open" && bead.Status != "in_progress" {
		return &commons.CardRefusal{Status: http.StatusConflict, Message: "This escalation is already " + bead.Status + "."}
	}
	// After the status checks, so a wrong-type or closed bead still answers
	// 400 or 409, as the bead page does.
	if ok, why := e.s.escalationApproveGate(r.Context(), id, email); !ok {
		return &commons.CardRefusal{Status: http.StatusForbidden, Message: why}
	}
	verb := "approve"
	if reject {
		verb = "reject"
		err = e.s.daemon.RejectEscalation(r.Context(), id, beadsapi.RejectEscalationRequest{RejectedBy: email, Reason: reason})
	} else {
		err = e.s.daemon.ApproveEscalation(r.Context(), id, beadsapi.ApproveEscalationRequest{ApprovedBy: email, ApprovalMethod: "commons"})
	}
	var apiErr *beadsapi.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusUnauthorized) {
		return &commons.CardRefusal{Status: http.StatusForbidden, Message: escalationRefusalMessage(verb, apiErr)}
	}
	if err == nil {
		e.s.logger.Info("escalation resolved from its Commons card", "id", id, "reject", reject, "actor", email)
	}
	return err
}

// commonsSelf is the viewer as a conversation principal, oidc:<email>, the
// form kbeads and channel members use.
func commonsSelf(r *http.Request) string {
	return canonicalOIDC(oidcauth.EmailFromContext(r.Context()))
}

func commonsChannel(ch commons.Channel) conversation.Channel {
	return conversation.Channel{Kind: ch.Kind, Members: ch.Members}
}

// commonsViewer is the signed-in viewer as a conversation principal
// (oidc:<email>, the author form of a message they write), "" for none.
func commonsViewer(r *http.Request) string {
	if p := commonsSelf(r); strings.HasPrefix(p, "oidc:") {
		return p
	}
	return ""
}

// commonsAgents is the Commons' agent presence source: the kbeads presence
// tracker's live agents, each in the channel its card is placed in by the
// shared card placement (conversation.Place, spec section 2.5).
func commonsAgents(daemon *beadsapi.Client) commons.AgentSource {
	a := &commons.Agents{
		Daemon: daemon,
		Place: func(ctx context.Context, agent *beadsapi.BeadDetail) (string, error) {
			p, err := conversation.Place(ctx, daemon, conversation.SubjectFromBead(agent))
			return p.Channel.ID, err
		},
	}
	return a.Spots
}
