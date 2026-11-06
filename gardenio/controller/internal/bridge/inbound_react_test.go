package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"
)

var updateInboundReactGolden = flag.Bool("update-inbound-react", false,
	"rewrite testdata/inbound_react_calls.golden.json from the wish-close handlers' recorded calls")

// commonsWishBead is an open wish head in channel gd-chgen with labels.
func commonsWishBead(id string, labels ...string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Type: "task", Status: "open", Title: "a wish",
		Labels: append([]string{threadKindLabel}, labels...),
		Fields: map[string]string{conversation.BeadChannelID: "gd-chgen"},
	}
}

// TestConversationInbound_Done drives a wish head's Done through POST
// /api/conversation/inbound: the land-role gate, the close on the person's
// bearer, and the refusals.
func TestConversationInbound_Done(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	land := "project:gardenio"
	for _, id := range []string{"gd-wish1", "gd-wish2", "gd-wish3"} {
		daemon.beads[id] = commonsWishBead(id, land)
	}
	daemon.beads["gd-triage"] = commonsWishBead("gd-triage", land, triageLabel)
	daemon.beads["gd-done"] = commonsWishBead("gd-done", land)
	daemon.beads["gd-done"].Status = "closed"
	daemon.beads["gd-nolabel"] = commonsWishBead("gd-nolabel")
	daemon.beads["gd-twoland"] = commonsWishBead("gd-twoland", land, "project:claimlayer")
	daemon.beads["gd-plain"] = &beadsapi.BeadDetail{ID: "gd-plain", Type: "message", Labels: []string{land, threadKindLabel},
		Fields: map[string]string{"text": "hi"}}
	daemon.beads["gd-task"] = &beadsapi.BeadDetail{ID: "gd-task", Type: "task", Status: "open", Labels: []string{land}}
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	for _, c := range []struct {
		name   string
		caller cardCaller
		body   string
		code   int
		want   string
	}{
		{"member", cardMember, `{"kind":"react","message":"gd-wish1","reaction":"done"}`, http.StatusOK, "Marked done."},
		{"operator", cardAdmin, `{"kind":"react","message":"gd-wish2","reaction":"done"}`, http.StatusOK, "Marked done."},
		{"guest", cardGuest, `{"kind":"react","message":"gd-wish3","reaction":"done"}`, http.StatusForbidden, "does not let you"},
		{"another land", cardOther, `{"kind":"react","message":"gd-wish3","reaction":"done"}`, http.StatusForbidden, "does not let you"},
		{"viewer", cardViewer, `{"kind":"react","message":"gd-wish3","reaction":"done"}`, http.StatusForbidden, "does not let you"},
		{"done already", cardMember, `{"kind":"react","message":"gd-done","reaction":"done"}`, http.StatusConflict, "done already"},
		{"needs triage", cardMember, `{"kind":"react","message":"gd-triage","reaction":"done"}`, http.StatusConflict, "needs triage"},
		{"no land", cardAdmin, `{"kind":"react","message":"gd-nolabel","reaction":"done"}`, http.StatusUnprocessableEntity, "no single land"},
		{"two lands", cardAdmin, `{"kind":"react","message":"gd-twoland","reaction":"done"}`, http.StatusUnprocessableEntity, "no single land"},
		{"a message", cardAdmin, `{"kind":"react","message":"gd-plain","reaction":"done"}`, http.StatusUnprocessableEntity, "not a wish"},
		{"a plain task", cardAdmin, `{"kind":"react","message":"gd-task","reaction":"done"}`, http.StatusUnprocessableEntity, "not a wish"},
		{"other reaction", cardAdmin, `{"kind":"react","message":"gd-wish3","reaction":"tada"}`, http.StatusUnprocessableEntity, "reaction"},
		{"no message", cardAdmin, `{"kind":"react","reaction":"done"}`, http.StatusBadRequest, "message"},
	} {
		rr := postCard(t, mux, c.caller, c.body)
		if rr.Code != c.code || !strings.Contains(rr.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want %d containing %q", c.name, rr.Code, rr.Body.String(), c.code, c.want)
		}
	}

	for _, id := range []string{"gd-wish1", "gd-wish2"} {
		fields, ok := closeOf(daemon, id)
		if !ok || fields["close_reason"] != wishCloseReason {
			t.Errorf("%s: closed=%v fields=%v, want close_reason %s", id, ok, fields, wishCloseReason)
		}
	}
	for _, id := range []string{"gd-wish3", "gd-triage", "gd-done", "gd-nolabel", "gd-twoland", "gd-plain", "gd-task"} {
		if _, ok := closeOf(daemon, id); ok {
			t.Errorf("%s was closed, want it left alone", id)
		}
	}
	// The close runs as the person, on their own bearer (kbeads checks again).
	if op := beadsapi.OperatorFromContext(daemon.lastCloseBeadCtx); op != "oidc:"+cardAdmin.email {
		t.Errorf("close operator %q, want oidc:%s", op, cardAdmin.email)
	}
	if bearer := beadsapi.UserBearerFromContext(daemon.lastCloseBeadCtx); bearer != "tok-"+cardAdmin.email {
		t.Errorf("close bearer %q, want the caller's", bearer)
	}
	if calls := twin.Calls(); len(calls) != 0 {
		t.Errorf("a Commons Done called Slack: %+v", calls)
	}
}

// TestHandleReact_UnknownReaction: only done is a reaction the dispatcher
// takes; anything else is no handler's.
func TestHandleReact_UnknownReaction(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, _ := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	_, err := b.inbound().Dispatch(context.Background(), conversation.Inbound{
		Kind: conversation.InboundReact, Surface: conversation.SurfaceCommons, Message: "gd-x", Reaction: "tada"})
	if !errors.Is(err, conversation.ErrNoHandler) {
		t.Fatalf("err = %v, want ErrNoHandler", err)
	}
}

// inboundReactOutcome is what the slice-4b wish close did on both surfaces:
// the Slack Web API calls (none) and the beads closed.
type inboundReactOutcome struct {
	Slack  []surfacetest.TwinCall `json:"slack"`
	Closed []goldenClose          `json:"closed"`
}

// TestInboundReactCallsGolden pins slice 4b: a Slack ⏹️ on a thread card, a ⏹️
// on a message that is no card (the reaction rules' then, never a close), a
// re-react on the closed wish, and a Commons Done close each wish once with
// the same close reason and post nothing to Slack (the one call is the
// reactor's users.info, the account-link lookup every reaction makes).
func TestInboundReactCallsGolden(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	seedWishHead(daemon, "gd-slackwish", "C0WISH", "1690000000.000100", "1690000000.000200")
	daemon.beads["gd-commonswish"] = commonsWishBead("gd-commonswish", "project:gardenio")
	ctx := context.Background()

	b.handleReactionAdded(ctx, makeReactionEvent("U0ADA", "black_square_for_stop", "C0WISH", "1690000000.000200"))
	b.handleReactionAdded(ctx, makeReactionEvent("U0ADA", "black_square_for_stop", "C0WISH", "1690000000.000999"))
	b.handleReactionAdded(ctx, makeReactionEvent("U0ADA", "black_square_for_stop", "C0WISH", "1690000000.000200"))
	if _, err := b.inbound().Dispatch(beadsapi.WithOperator(ctx, "oidc:ada@example.com"), conversation.Inbound{
		Kind: conversation.InboundReact, Surface: conversation.SurfaceCommons, Message: "gd-commonswish",
		Actor: conversation.Principal{ID: "oidc:ada@example.com"}, Reaction: conversation.ReactionDone,
	}); err != nil {
		t.Fatalf("Commons Done: %v", err)
	}

	out := inboundReactOutcome{Slack: twin.Calls()}
	for _, c := range daemon.getClosed() {
		out.Closed = append(out.Closed, goldenClose{ID: c.BeadID, Fields: c.Fields})
	}
	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "inbound_react_calls.golden.json")
	if *updateInboundReactGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("INBOUND_REACT_GOLDEN_B64:%s", base64.StdEncoding.EncodeToString(got))
		t.Fatalf("%s missing (rerun with -update-inbound-react)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("wish close drifted from %s (rerun with -update-inbound-react only if the change is intended):\n%s", path, got)
	}
}
