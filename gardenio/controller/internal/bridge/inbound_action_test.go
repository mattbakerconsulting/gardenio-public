package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"
	"gardenio/controller/internal/oidcauth"
)

// commonsCardBead is a card bead in land showing subject.
func commonsCardBead(id, subject string, labels []string, extra map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{
		conversation.BeadKind: string(conversation.KindCard), conversation.BeadSubjectID: subject,
		conversation.BeadChannelID: "gd-chdec", conversation.BeadOrigin: conversation.SurfaceCommons,
	}
	for k, v := range extra {
		fields[k] = v
	}
	return &beadsapi.BeadDetail{ID: id, Type: "message", Status: "closed", Labels: labels, Fields: fields}
}

// cardCaller is who calls the inbound endpoint: an email and the roles and
// groups on their token.
type cardCaller struct {
	email  string
	roles  []string
	groups []string
}

var (
	cardAdmin  = cardCaller{"op@example.com", []string{oidcauth.RoleAdmin}, nil}
	cardMember = cardCaller{"mo@example.com", []string{oidcauth.RoleProjectMember}, []string{"/project-gardenio"}}
	cardGuest  = cardCaller{"gu@example.com", []string{oidcauth.RoleProjectMember}, []string{"/project-gardenio/guests"}}
	cardOther  = cardCaller{"ot@example.com", []string{oidcauth.RoleProjectMember}, []string{"/project-claimlayer"}}
	cardViewer = cardCaller{"vi@example.com", []string{oidcauth.RoleViewer}, nil}
)

func postCard(t *testing.T, mux *http.ServeMux, c cardCaller, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/conversation/inbound", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-"+c.email)
	ctx := oidcauth.ContextWithEmail(req.Context(), c.email)
	ctx = oidcauth.ContextWithRoles(ctx, c.roles)
	ctx = oidcauth.ContextWithGroups(ctx, c.groups)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req.WithContext(ctx))
	return rr
}

func closeOf(d *mockDaemon, id string) (map[string]string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.closed {
		if c.BeadID == id {
			return c.Fields, true
		}
	}
	return nil, false
}

// TestConversationInbound_CardActions drives the Commons card path of POST
// /api/conversation/inbound: the land-role gate, each decision, permission
// and agent action, and the refusals.
func TestConversationInbound_CardActions(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, coopCalls := newCoopRecorder(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	land := []string{"project:gardenio"}
	for id, subject := range map[string]string{
		"gd-dcard1": "gd-dec1", "gd-dcard2": "gd-dec2", "gd-dcard3": "gd-dec3", "gd-dcard4": "gd-dec4",
		"gd-acard": "gd-agent-live",
	} {
		daemon.beads[id] = commonsCardBead(id, subject, land, nil)
	}
	daemon.beads["gd-pcard"] = commonsCardBead("gd-pcard", "gd-perm1", land,
		map[string]string{"slack_channel": "C0DEC", "slack_ts": "1690000000.000700"})
	daemon.beads["gd-twoland"] = commonsCardBead("gd-twoland", "gd-dec1", []string{"project:gardenio", "project:claimlayer"}, nil)
	daemon.beads["gd-plain"] = &beadsapi.BeadDetail{ID: "gd-plain", Type: "message", Labels: land, Fields: map[string]string{"text": "hi"}}
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	for _, c := range []struct {
		name   string
		caller cardCaller
		body   string
		code   int
		want   string
	}{
		{"guest may not answer", cardGuest, `{"kind":"action","card":"gd-dcard2","action":"resolve","value":"1"}`, http.StatusForbidden, "does not let you"},
		{"another land's member", cardOther, `{"kind":"action","card":"gd-dcard2","action":"resolve","value":"1"}`, http.StatusForbidden, ""},
		{"a viewer is nobody in a land", cardViewer, `{"kind":"action","card":"gd-dcard2","action":"resolve","value":"1"}`, http.StatusForbidden, ""},
		{"no card", cardMember, `{"kind":"action","action":"resolve"}`, http.StatusBadRequest, ""},
		{"gone card", cardMember, `{"kind":"action","card":"gd-nope","action":"resolve"}`, http.StatusUnprocessableEntity, ""},
		{"not a card", cardMember, `{"kind":"action","card":"gd-plain","action":"resolve"}`, http.StatusUnprocessableEntity, ""},
		{"two lands", cardAdmin, `{"kind":"action","card":"gd-twoland","action":"dismiss"}`, http.StatusUnprocessableEntity, ""},
		{"no such button", cardMember, `{"kind":"action","card":"gd-dcard2","action":"approve"}`, http.StatusUnprocessableEntity, ""},
		{"a button is not a lightbox", cardMember, `{"kind":"submit","card":"gd-dcard2","action":"resolve"}`, http.StatusUnprocessableEntity, ""},
		{"option off the card", cardMember, `{"kind":"action","card":"gd-dcard2","action":"resolve","value":"9"}`, http.StatusBadRequest, ""},
		{"resolve", cardMember, `{"kind":"action","card":"gd-dcard2","action":"resolve","value":"1"}`, http.StatusOK, "Recorded your choice: Ship it."},
		{"resolve twice", cardMember, `{"kind":"action","card":"gd-dcard2","action":"resolve","value":"2"}`, http.StatusConflict, "answered already"},
		{"dismiss", cardMember, `{"kind":"action","card":"gd-dcard1","action":"dismiss","values":{"reason":" Asked twice "}}`, http.StatusOK, "Dismissed."},
		{"own words, empty", cardMember, `{"kind":"submit","card":"gd-dcard3","action":"other","values":{"text":"  "}}`, http.StatusBadRequest, ""},
		{"own words", cardMember, `{"kind":"submit","card":"gd-dcard3","action":"other","values":{"text":"Ship it on Monday"}}`, http.StatusOK, "Recorded your answer."},
		{"escalate", cardAdmin, `{"kind":"submit","card":"gd-dcard4","action":"promote"}`, http.StatusOK, "Handed back to crew-live"},
		{"allow", cardMember, `{"kind":"action","card":"gd-pcard","action":"allow"}`, http.StatusOK, `"Allowed."`},
		{"allow twice", cardMember, `{"kind":"action","card":"gd-pcard","action":"deny"}`, http.StatusConflict, "already answered"},
		{"restart with no thread", cardMember, `{"kind":"action","card":"gd-acard","action":"restart"}`, http.StatusUnprocessableEntity, "bound to no thread"},
		{"kill", cardMember, `{"kind":"action","card":"gd-acard","action":"kill"}`, http.StatusOK, "Killing thread agent *crew-live*"},
	} {
		rr := postCard(t, mux, c.caller, c.body)
		if rr.Code != c.code || !strings.Contains(rr.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want %d containing %q", c.name, rr.Code, rr.Body.String(), c.code, c.want)
		}
	}

	if f, ok := closeOf(daemon, "gd-dec2"); !ok || f["chosen"] != "yes" || f["rationale"] != "Chosen by mo@example.com via the Commons" {
		t.Errorf("resolve closed gd-dec2 with %v (%v)", f, ok)
	}
	if f, _ := closeOf(daemon, "gd-dec1"); f["chosen"] != "dismissed" || f["rationale"] != "Dismissed by mo@example.com via the Commons: Asked twice" {
		t.Errorf("dismiss closed gd-dec1 with %v", f)
	}
	if f, _ := closeOf(daemon, "gd-dec3"); f["response_text"] != "Ship it on Monday" || f["chosen"] != "" {
		t.Errorf("own words closed gd-dec3 with %v", f)
	}
	if f, _ := closeOf(daemon, "gd-dec4"); f["chosen"] != "returned_to_agent_for_escalation" ||
		f["rationale"] != "Returned to agent for structured escalation by op@example.com via the Commons" {
		t.Errorf("escalate closed gd-dec4 with %v", f)
	}
	// The answer records the verified principal, never a chosen name.
	if f, _ := closeOf(daemon, "gd-perm1"); f["behavior"] != "allow" || f["answered_by"] != "oidc:mo@example.com" {
		t.Errorf("allow closed gd-perm1 with %v", f)
	}
	deadline := time.Now().Add(5 * time.Second)
	for countContaining(coopCalls(), "gd-dec3 resolved") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	calls := coopCalls()
	if countContaining(calls, `/permission {"behavior":"allow","request_id":"req-1"}`) != 1 {
		t.Errorf("verdict not sent once: %v", calls)
	}
	if countContaining(calls, "gd-dec4 has been escalated by op@example.com via the Commons") == 0 {
		t.Errorf("escalate did not nudge the agent: %v", calls)
	}
	if countContaining(calls, "gd-dec3 resolved") == 0 {
		t.Errorf("own words did not nudge the agent: %v", calls)
	}

	// Allow edited the request's Slack prompt to its answer.
	var edited bool
	for _, call := range twin.Calls() {
		if call.Method == "chat.update" && call.Params["ts"] == "1690000000.000700" &&
			strings.Contains(call.Params["blocks"], "*Allowed* by *mo@example.com*") {
			edited = true
		}
	}
	if !edited {
		t.Errorf("the Slack prompt was not edited to the answer: %v", twin.Calls())
	}
}

// TestConversationInbound_PermissionAnsweredOnGlass: a permission prompt
// answered on the Glass closes its request through the card's "answered"
// action (Lands workbench-glass spec 3.3): the answer, the verified principal
// and where are recorded, the Slack prompt is edited to say so, and no verdict
// goes to the agent, which has its answer already. The same land-role gate
// holds, and a closed request is not answered twice.
func TestConversationInbound_PermissionAnsweredOnGlass(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, coopCalls := newCoopRecorder(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	daemon.beads["gd-pcard"] = commonsCardBead("gd-pcard", "gd-perm1", []string{"project:gardenio"},
		map[string]string{"slack_channel": "C0DEC", "slack_ts": "1690000000.000700"})
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	for _, c := range []struct {
		name   string
		caller cardCaller
		body   string
		code   int
	}{
		{"guest", cardGuest, `{"kind":"action","card":"gd-pcard","action":"answered","values":{"behavior":"deny","said":"No"}}`, http.StatusForbidden},
		{"an answer that is not one", cardMember, `{"kind":"action","card":"gd-pcard","action":"answered","values":{"behavior":"maybe"}}`, http.StatusBadRequest},
		{"answered", cardMember, `{"kind":"action","card":"gd-pcard","action":"answered","values":{"behavior":"deny","said":"No, and tell Claude <!channel>"}}`, http.StatusOK},
	} {
		if rr := postCard(t, mux, c.caller, c.body); rr.Code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, rr.Code, rr.Body.String(), c.code)
		}
	}
	f, ok := closeOf(daemon, "gd-perm1")
	if !ok || f["behavior"] != "deny" || f["answered_by"] != "oidc:mo@example.com" || f["answered_on"] != "glass" || f["answer_label"] != "No, and tell Claude <!channel>" {
		t.Errorf("answered closed gd-perm1 with %v (%v)", f, ok)
	}
	if rr := postCard(t, mux, cardMember, `{"kind":"action","card":"gd-pcard","action":"answered","values":{"behavior":"allow"}}`); rr.Code != http.StatusConflict {
		t.Errorf("answered twice: %d %s, want 409", rr.Code, rr.Body.String())
	}
	if n := countContaining(coopCalls(), `"request_id":"req-1"`); n != 0 {
		t.Errorf("a verdict went to the agent %d times: %v", n, coopCalls())
	}
	var edited bool
	for _, call := range twin.Calls() {
		if call.Method == "chat.update" && call.Params["ts"] == "1690000000.000700" {
			blocks := call.Params["blocks"]
			if strings.Contains(blocks, "<!channel>") {
				t.Errorf("the dialog's words reached Slack as mrkdwn: %s", blocks)
			}
			edited = edited || strings.Contains(blocks, "*Denied* on the Glass by *mo@example.com*")
		}
	}
	if !edited {
		t.Errorf("the Slack prompt was not edited to the answer: %v", twin.Calls())
	}
}

// TestConversationInbound_CardActsOnBearer checks the handlers run on the
// caller's own bearer and as the caller, so kbeads checks the write again.
func TestConversationInbound_CardActsOnBearer(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, _ := newCoopRecorder(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	daemon.beads["gd-dcard1"] = commonsCardBead("gd-dcard1", "gd-dec1", []string{"project:gardenio"}, nil)
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	rr := postCard(t, mux, cardMember, `{"kind":"action","card":"gd-dcard1","action":"dismiss"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("dismiss: %d %s", rr.Code, rr.Body.String())
	}
	daemon.mu.Lock()
	ctx := daemon.lastCloseBeadCtx
	daemon.mu.Unlock()
	if got := beadsapi.UserBearerFromContext(ctx); got != "tok-mo@example.com" {
		t.Errorf("close ran on bearer %q, want the caller's", got)
	}
	if got := beadsapi.OperatorFromContext(ctx); got != "oidc:mo@example.com" {
		t.Errorf("close ran as %q, want the caller", got)
	}
	var resp conversationInboundResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || resp.Status != "handled" {
		t.Errorf("response %s (%v)", rr.Body.String(), err)
	}
}

// TestConversationInbound_CardSubjectLand: the bridge acts only on a subject
// in exactly one land, its card's. A subject with no land, with two, or from
// another land is refused 422 and nothing is done; and an agent card's kill
// or restart never reaches a same-named agent in another land.
func TestConversationInbound_CardSubjectLand(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, coopCalls := newCoopRecorder(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	land := []string{"project:gardenio"}
	opts := `[{"id":"yes","short":"y","label":"Ship it"}]`
	for id, labels := range map[string][]string{
		"gd-dec-nolabel":   nil,
		"gd-dec-twolands":  {"project:gardenio", "project:claimlayer"},
		"gd-dec-otherland": {"project:claimlayer"},
	} {
		daemon.beads[id] = &beadsapi.BeadDetail{ID: id, Type: "decision", Status: "open", Assignee: "crew-live",
			Labels: labels, Fields: map[string]string{"options": opts, "question": "Ship it?"}}
		daemon.beads["card-"+id] = commonsCardBead("card-"+id, id, land, nil)
	}
	for id, labels := range map[string][]string{
		"gd-agent-nolabel":   nil,
		"gd-agent-twolands":  {"project:gardenio", "project:claimlayer"},
		"gd-agent-otherland": {"project:claimlayer"},
	} {
		daemon.beads[id] = &beadsapi.BeadDetail{ID: id, Type: "agent", Status: "open", Title: "crew-live",
			Labels: labels, Fields: map[string]string{"agent": "crew-live", "slack_thread_channel": "C0CREW", "slack_thread_ts": "1690000000.000300"}}
		daemon.beads["card-"+id] = commonsCardBead("card-"+id, id, land, nil)
	}
	// crew-twin's card is in gardenio, but the name crew-twin now resolves to
	// a live agent in claimlayer.
	daemon.beads["gd-agent-twin-gd"] = &beadsapi.BeadDetail{ID: "gd-agent-twin-gd", Type: "agent", Status: "open", Title: "crew-twin",
		Labels: land, Fields: map[string]string{"agent": "crew-twin", "slack_thread_channel": "C0CREW", "slack_thread_ts": "1690000000.000300"}}
	daemon.beads["crew-twin"] = &beadsapi.BeadDetail{ID: "gd-agent-twin-cl", Type: "agent", Status: "open", Title: "crew-twin",
		Labels: []string{"project:claimlayer"}, Notes: "coop_url: " + coop, Fields: map[string]string{"agent": "crew-twin"}}
	daemon.beads["card-twin"] = commonsCardBead("card-twin", "gd-agent-twin-gd", land, nil)
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	for _, c := range []struct {
		name, body string
		code       int
	}{
		{"decision with no land", `{"kind":"action","card":"card-gd-dec-nolabel","action":"dismiss"}`, http.StatusUnprocessableEntity},
		{"decision in two lands", `{"kind":"action","card":"card-gd-dec-twolands","action":"resolve","value":"1"}`, http.StatusUnprocessableEntity},
		{"decision in another land", `{"kind":"submit","card":"card-gd-dec-otherland","action":"other","values":{"text":"yes"}}`, http.StatusUnprocessableEntity},
		{"agent with no land", `{"kind":"action","card":"card-gd-agent-nolabel","action":"kill"}`, http.StatusUnprocessableEntity},
		{"agent in two lands", `{"kind":"action","card":"card-gd-agent-twolands","action":"restart"}`, http.StatusUnprocessableEntity},
		{"agent in another land", `{"kind":"action","card":"card-gd-agent-otherland","action":"kill"}`, http.StatusUnprocessableEntity},
		{"kill a same-named agent elsewhere", `{"kind":"action","card":"card-twin","action":"kill"}`, http.StatusConflict},
		{"restart a same-named agent elsewhere", `{"kind":"action","card":"card-twin","action":"restart"}`, http.StatusConflict},
		{"clear a same-named agent elsewhere", `{"kind":"action","card":"card-twin","action":"clear"}`, http.StatusConflict},
	} {
		if rr := postCard(t, mux, cardAdmin, c.body); rr.Code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, rr.Code, rr.Body.String(), c.code)
		}
	}

	// Give a stray kill or restart goroutine time to show itself.
	time.Sleep(100 * time.Millisecond)
	daemon.mu.Lock()
	closed := append([]closeCall(nil), daemon.closed...)
	daemon.mu.Unlock()
	if len(closed) != 0 {
		t.Errorf("a refused card action closed beads: %+v", closed)
	}
	if calls := coopCalls(); len(calls) != 0 {
		t.Errorf("a refused card action reached an agent: %v", calls)
	}
}

// TestKillAgentBead: a kill held to one agent bead does nothing when the name
// resolves to another.
func TestKillAgentBead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, coopCalls := newCoopRecorder(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	if err := b.killAgentBead(context.Background(), "crew-live", "gd-agent-elsewhere", true); !errors.Is(err, errAgentNotBead) {
		t.Fatalf("kill held to another bead: %v, want errAgentNotBead", err)
	}
	if _, ok := closeOf(daemon, "gd-agent-live"); ok || len(coopCalls()) != 0 {
		t.Fatalf("the held kill closed or reached gd-agent-live")
	}
	if err := b.killAgentBead(context.Background(), "crew-live", "gd-agent-live", true); err != nil {
		t.Fatalf("kill held to its own bead: %v", err)
	}
	if _, ok := closeOf(daemon, "gd-agent-live"); !ok {
		t.Errorf("the kill held to its own bead did not close it")
	}
}

// TestCommonsCardAction pins the action-table entry of every Commons card
// button and lightbox.
func TestCommonsCardAction(t *testing.T) {
	for _, c := range []struct {
		kind              conversation.InboundKind
		subject, actionID string
		want              conversation.Action
	}{
		{conversation.InboundAction, "decision", "resolve", conversation.ActionDecisionRespond},
		{conversation.InboundSubmit, "decision", "other", conversation.ActionDecisionRespond},
		{conversation.InboundAction, "decision", "dismiss", conversation.ActionDecisionDismiss},
		{conversation.InboundSubmit, "decision", "promote", conversation.ActionDecisionPromote},
		{conversation.InboundAction, "permission", "allow", conversation.ActionPermissionAnswer},
		{conversation.InboundAction, "permission", "deny", conversation.ActionPermissionAnswer},
		{conversation.InboundAction, "permission", "answered", conversation.ActionPermissionAnswer},
		{conversation.InboundAction, "agent", "clear", conversation.ActionAgentControl},
		{conversation.InboundAction, "agent", "kill", conversation.ActionAgentControl},
		{conversation.InboundAction, "agent", "restart", conversation.ActionAgentControl},
		{conversation.InboundAction, "escalation", "approve", ""},
		{conversation.InboundAction, "decision", "promote", ""},
		{conversation.InboundSubmit, "agent", "kill", ""},
	} {
		got, ok := CommonsCardAction(c.kind, c.subject, c.actionID)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("CommonsCardAction(%s, %s, %s) = %q, %v; want %q", c.kind, c.subject, c.actionID, got, ok, c.want)
		}
	}
}

// TestHandleAction_SlackSkipsUnknown checks an action id no handler takes
// lets the Slack adapter go on to the click's next action.
func TestHandleAction_SlackSkipsUnknown(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, _ := newCoopRecorder(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	cb := slackClick("C0DEC", "1690000000.000601", "", "escalation_open_ui", "gd-esc")
	cb.ActionCallback.BlockActions = append(cb.ActionCallback.BlockActions, slackClick("C0DEC", "1690000000.000601", "", "dismiss_decision", "gd-dec1").ActionCallback.BlockActions...)
	b.handleBlockActions(context.Background(), cb)
	if f, ok := closeOf(daemon, "gd-dec1"); !ok || f["chosen"] != "dismissed" {
		t.Errorf("the second action did not run: %v %v", f, ok)
	}
}
