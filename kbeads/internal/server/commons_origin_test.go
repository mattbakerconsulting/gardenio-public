package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/edgeauth"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// origin=commons is a person's own words (commons_origin.go, gd-Wu4U4hZbY0):
// only an OIDC human writes it, as themselves.

const (
	coStatic = "commons-origin-static"
	coAda    = "commons-origin-ada-tok"   // OIDC operator, ada@example.com
	coAdmin  = "commons-origin-admin-tok" // OIDC admin, owner@example.com
	coAgent  = "commons-origin-agent-tok" // fleet token of agent gardenio-crew-x
	coLand   = "commons-origin-land-tok"  // land principal for gardenio
)

type commonsOriginFixture struct {
	srv      *BeadsServer
	ms       *mockStore
	fc       *FleetConfig
	verifier TokenVerifier
	ts       *httptest.Server
}

func newCommonsOriginFixture(t *testing.T) *commonsOriginFixture {
	t.Helper()
	verifier := &multiIssuerMock{issuers: []*oidcIssuerMock{{tokens: map[string]tokenInfo{
		coAda:   {email: "ada@example.com", roles: []string{RoleViewer, RoleOperator}, azp: testBrowserClient},
		coAdmin: {email: "owner@example.com", roles: []string{RoleViewer, RoleOperator, RoleAdmin}, azp: testBrowserClient},
	}}}}
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenFull(coAgent, "gardenio", "gardenio", nil, nil, time.Time{}, "gardenio-crew-x", false)
	registerLandPrincipal(fc, coLand, "gardenio")
	srv.SetFleetAuth(fc)
	srv.SetRateLimit(RateLimitConfig{})
	ts := httptest.NewServer(srv.NewHTTPHandler(verifier, withTestToken(fc, coStatic)))
	t.Cleanup(ts.Close)
	return &commonsOriginFixture{srv: srv, ms: ms, fc: fc, verifier: verifier, ts: ts}
}

// do sends one request as token, with X-Actor when actor is set.
func (f *commonsOriginFixture) do(t *testing.T, token, actor, method, path string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, f.ts.URL+path, strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-No-Auto-Parent", "1")
	if actor != "" {
		req.Header.Set("X-Actor", actor)
	}
	if token == coStatic {
		req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func commonsMessageBody(fields map[string]any) map[string]any {
	return map[string]any{
		"title": "hi", "type": "message", "status": "closed",
		"labels": []string{"project:gardenio"}, "fields": fields,
	}
}

func TestCommonsOrigin_CreateHTTP(t *testing.T) {
	f := newCommonsOriginFixture(t)
	for _, c := range []struct {
		name, token, actor string
		fields             map[string]any
		want               int
	}{
		{"person as themselves", coAda, "", map[string]any{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com"}, http.StatusCreated},
		{"person, author case differs", coAda, "", map[string]any{"text": "hi", "origin": "commons", "author": "oidc:Ada@Example.com"}, http.StatusCreated},
		{"person, no author field", coAda, "", map[string]any{"text": "hi", "origin": "commons"}, http.StatusCreated},
		{"person naming another", coAda, "", map[string]any{"text": "hi", "origin": "commons", "author": "oidc:bob@example.com"}, http.StatusForbidden},
		{"admin naming another", coAdmin, "", map[string]any{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com"}, http.StatusForbidden},
		{"agent fleet token", coAgent, "", map[string]any{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com"}, http.StatusForbidden},
		{"agent fleet token, no author", coAgent, "", map[string]any{"text": "hi", "origin": "commons"}, http.StatusForbidden},
		{"static token", coStatic, "", map[string]any{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com"}, http.StatusForbidden},
		// #670: X-Actor names no one (it is not honoured since the shared
		// static token was retired, gd-ke6PC7bRto), so these are refused the
		// same way.
		{"static token, X-Actor names the person", coStatic, "oidc:ada@example.com", map[string]any{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com"}, http.StatusForbidden},
		{"static token, X-Actor bare email", coStatic, "ada@example.com", map[string]any{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com"}, http.StatusForbidden},
		{"static token, X-Actor agent name", coStatic, "gardenio-crew-x", map[string]any{"text": "hi", "origin": "commons"}, http.StatusForbidden},
		{"land principal", coLand, "", map[string]any{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com"}, http.StatusForbidden},
		// Other origins are not this gate's.
		{"agent, origin=cli", coAgent, "", map[string]any{"text": "hi", "origin": "cli"}, http.StatusCreated},
		// A forged author on another origin is message_author.go's to refuse.
		{"static, origin=slack", coStatic, "", map[string]any{"text": "hi", "origin": "slack"}, http.StatusCreated},
		{"agent, no origin", coAgent, "", map[string]any{"text": "hi"}, http.StatusCreated},
		// The purse watcher's posts (lands/watcher OriginNotice/OriginRefill)
		// are declared origins, so create accepts them (#956). The writers run
		// on the controller's machine principal, which may name its author
		// (message_author.go); here a service fleet token names no author.
		{"static, origin=purse_notice", coStatic, "", map[string]any{"text": "hi", "origin": "purse_notice"}, http.StatusCreated},
		{"static, origin=purse_refill", coStatic, "", map[string]any{"text": "hi", "origin": "purse_refill"}, http.StatusCreated},
		{"agent, origin=purse_notice", coAgent, "", map[string]any{"text": "hi", "origin": "purse_notice"}, http.StatusCreated},
		// The provisioner's wake post in a new land (steward ProvisionHook.Wake).
		{"static, origin=land_opened", coStatic, "", map[string]any{"text": "hi", "origin": "land_opened"}, http.StatusCreated},
		{"static, origin=person_joined", coStatic, "", map[string]any{"text": "hi", "origin": "person_joined"}, http.StatusCreated},
		// A Glass nudge's copy in its agent's thread, written on the person's
		// own bearer (gardenhub GlassOrigin, gd-Ik3rTCqfxp).
		{"person, origin=glass", coAda, "", map[string]any{"text": "hi", "origin": "glass", "author": "oidc:ada@example.com"}, http.StatusCreated},
		{"undeclared origin", coAgent, "", map[string]any{"text": "hi", "origin": "purse"}, http.StatusBadRequest},
	} {
		code, body := f.do(t, c.token, c.actor, http.MethodPost, "/v1/beads", commonsMessageBody(c.fields))
		if code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.want)
			continue
		}
		if code == http.StatusForbidden && !strings.Contains(body, "origin=commons") {
			t.Errorf("%s: refusal %q does not say why", c.name, body)
		}
		if code == http.StatusCreated && c.fields["origin"] == "commons" {
			var got model.Bead
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatal(err)
			}
			if got.CreatedBy != "ada@example.com" {
				t.Errorf("%s: created_by = %q, want the person's email", c.name, got.CreatedBy)
			}
		}
	}
}

func TestCommonsOrigin_CreateGRPC(t *testing.T) {
	f := newCommonsOriginFixture(t)
	create := func(token, actor, fields string) error {
		md := metadata.Pairs("authorization", "Bearer "+token)
		if actor != "" {
			md.Append("x-actor", actor)
		}
		if token == coStatic {
			md.Append("x-operated-by", "slack:U0OPERATOR2")
		}
		ctx := metadata.NewIncomingContext(context.Background(), md)
		_, err := AuthInterceptor(withTestToken(f.fc, coStatic), f.verifier)(ctx,
			&beadsv1.CreateBeadRequest{Title: "hi", Type: "message", Labels: []string{"project:gardenio"}, Fields: []byte(fields)},
			&grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/CreateBead"},
			func(ctx context.Context, req any) (any, error) {
				return f.srv.CreateBead(ctx, req.(*beadsv1.CreateBeadRequest))
			})
		return err
	}
	const forged = `{"text":"hi","origin":"commons","author":"oidc:ada@example.com"}`
	for _, c := range []struct{ name, token, actor, fields string }{
		{"agent fleet token", coAgent, "", forged},
		{"static token", coStatic, "", forged},
		{"static token, X-Actor names the person", coStatic, "oidc:ada@example.com", forged},
		{"person naming another", coAda, "", `{"text":"hi","origin":"commons","author":"oidc:bob@example.com"}`},
	} {
		if err := create(c.token, c.actor, c.fields); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "origin=commons") {
			t.Errorf("%s: err = %v, want PermissionDenied", c.name, err)
		}
	}
	if err := create(coAda, "", forged); err != nil {
		t.Errorf("person as themselves: %v, want created", err)
	}
	if err := create(coAgent, "", `{"text":"hi","origin":"cli"}`); err != nil {
		t.Errorf("agent, origin=cli: %v, want created", err)
	}
}

// An update may not make a message origin=commons in someone's name either.
func TestCommonsOrigin_UpdateHTTP(t *testing.T) {
	f := newCommonsOriginFixture(t)
	seed := func(id, createdBy string, fields map[string]any) {
		fields["kind"] = "text"
		raw, _ := json.Marshal(fields)
		f.ms.beads[id] = &model.Bead{ID: id, Kind: model.KindData, Type: typeMessage, Title: "hi", Status: model.StatusOpen,
			CreatedBy: createdBy, Fields: raw, Fleet: "gardenio"}
		f.ms.labels[id] = []string{"project:gardenio"}
	}
	for _, c := range []struct {
		name, token, createdBy string
		fields, patch          map[string]any
		want                   int
	}{
		{"agent sets origin=commons on its own message", coAgent, "gardenio-crew-x",
			map[string]any{"text": "hi", "author": "oidc:ada@example.com"}, map[string]any{"origin": "commons"}, http.StatusForbidden},
		{"static sets origin=commons", coStatic, "static",
			map[string]any{"text": "hi"}, map[string]any{"origin": "commons", "author": "oidc:ada@example.com"}, http.StatusForbidden},
		{"admin renames a person's commons message", coAdmin, "ada@example.com",
			map[string]any{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com"}, map[string]any{"author": "oidc:owner@example.com"}, http.StatusForbidden},
		{"person names another on their own", coAda, "ada@example.com",
			map[string]any{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com"}, map[string]any{"author": "oidc:bob@example.com"}, http.StatusForbidden},
		{"person sets origin on their own", coAda, "ada@example.com",
			map[string]any{"text": "hi"}, map[string]any{"origin": "commons", "author": "oidc:ada@example.com"}, http.StatusOK},
		// The bridge stamps its own fields on a person's message.
		{"static stamps the Slack ref", coStatic, "ada@example.com",
			map[string]any{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com"}, map[string]any{"slack_channel": "C0OUT", "slack_ts": "1690000000.000100"}, http.StatusOK},
		{"static repeats the same origin", coStatic, "ada@example.com",
			map[string]any{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com"}, map[string]any{"origin": "commons", "slack_ts": "1.2"}, http.StatusOK},
		{"agent moves its message off commons", coAgent, "gardenio-crew-x",
			map[string]any{"text": "hi", "origin": "commons"}, map[string]any{"origin": "cli"}, http.StatusOK},
	} {
		id := "gd-co-" + strings.ReplaceAll(c.name, " ", "-")
		seed(id, c.createdBy, c.fields)
		code, body := f.do(t, c.token, "", http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": c.patch})
		if code != c.want || (code == http.StatusForbidden && !strings.Contains(body, "origin=commons")) {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.want)
		}
	}
}

func TestCommonsOriginDenial_AuthDisabled(t *testing.T) {
	ctx := withAuthMethod(context.Background(), authMethodNone)
	if d := commonsOriginDenial(ctx, typeMessage, json.RawMessage(`{"origin":"commons","author":"oidc:ada@example.com"}`)); d != nil {
		t.Fatalf("auth-disabled mode refused: %v", d)
	}
}

// A person's Commons message keeps its words (gd-74wnmpQgyT): only its writer
// changes its text, files or title, or a Slack ref it already has.

// seedAdaMessage seeds a message ada@example.com wrote in the Commons and
// already mirrored into Slack, with extra fields on top.
func (f *commonsOriginFixture) seedAdaMessage(id string, extra map[string]any) {
	fields := map[string]any{"kind": "text", "text": "hi", "origin": "commons", "author": "oidc:ada@example.com",
		"channel_id": "gd-chan", "slack_channel": "C0OUT", "slack_ts": "1690000000.000100"}
	for k, v := range extra {
		if v == nil {
			delete(fields, k)
		} else {
			fields[k] = v
		}
	}
	if _, ok := f.ms.beads["gd-chan"]; !ok {
		f.ms.beads["gd-chan"] = &model.Bead{ID: "gd-chan", Kind: model.KindData, Type: typeChannel, Title: "#general",
			Status: model.StatusOpen, Fields: json.RawMessage(`{"name":"general"}`), Fleet: "gardenio"}
		f.ms.labels["gd-chan"] = []string{"project:gardenio"}
	}
	raw, _ := json.Marshal(fields)
	f.ms.beads[id] = &model.Bead{ID: id, Kind: model.KindData, Type: typeMessage, Title: "hi", Status: model.StatusOpen,
		CreatedBy: "ada@example.com", Fields: raw, Fleet: "gardenio"}
	f.ms.labels[id] = []string{"project:gardenio"}
}

func TestCommonsMessage_EditHTTP(t *testing.T) {
	f := newCommonsOriginFixture(t)
	forged := map[string]any{"fields": map[string]any{"text": "forged words", "slack_ts": ""}}
	for _, c := range []struct {
		name, token string
		seed        map[string]any // over seedAdaMessage's fields; nil value drops one
		body        map[string]any
		want        int
	}{
		{"static rewrites text and clears slack_ts", coStatic, nil, forged, http.StatusForbidden},
		{"agent rewrites text and clears slack_ts", coAgent, nil, forged, http.StatusForbidden},
		{"land rewrites text and clears slack_ts", coLand, nil, forged, http.StatusForbidden},
		{"admin rewrites text and clears slack_ts", coAdmin, nil, forged, http.StatusForbidden},
		{"static rewrites text", coStatic, nil, map[string]any{"fields": map[string]any{"text": "forged words"}}, http.StatusForbidden},
		{"static clears slack_ts", coStatic, nil, map[string]any{"fields": map[string]any{"slack_ts": ""}}, http.StatusForbidden},
		{"static nulls slack_ts", coStatic, nil, map[string]any{"fields": map[string]any{"slack_ts": nil}}, http.StatusForbidden},
		{"static moves the Slack ref", coStatic, nil, map[string]any{"fields": map[string]any{"slack_channel": "C0OTHER"}}, http.StatusForbidden},
		{"static moves it to another channel", coStatic, nil, map[string]any{"fields": map[string]any{"channel_id": "gd-other"}}, http.StatusForbidden},
		{"static swaps its files", coStatic, nil, map[string]any{"fields": map[string]any{"files": []any{map[string]any{"name": "x.png", "url": "https://x"}}}}, http.StatusForbidden},
		{"static moves it off commons", coStatic, nil, map[string]any{"fields": map[string]any{"origin": "cli"}}, http.StatusForbidden},
		{"static renames it", coStatic, nil, map[string]any{"title": "forged"}, http.StatusForbidden},
		{"static rewrites its description", coStatic, nil, map[string]any{"description": "forged"}, http.StatusForbidden},
		// The bridge's own writes, and anything that changes nothing.
		{"static stamps an empty Slack ref", coStatic, map[string]any{"slack_channel": nil, "slack_ts": nil},
			map[string]any{"fields": map[string]any{"slack_channel": "C0OUT", "slack_ts": "1690000000.000200"}}, http.StatusOK},
		{"static stamps the same Slack ref again", coStatic, nil,
			map[string]any{"fields": map[string]any{"slack_channel": "C0OUT", "slack_ts": "1690000000.000100"}}, http.StatusOK},
		{"static stamps channel_id on a thread head", coStatic, map[string]any{"channel_id": nil},
			map[string]any{"fields": map[string]any{"channel_id": "gd-chan"}}, http.StatusOK},
		{"static stamps a field of its own", coStatic, nil, map[string]any{"fields": map[string]any{"slack_attachment_count": "0"}}, http.StatusOK},
		{"static repeats its words", coStatic, nil, map[string]any{"title": "hi", "fields": map[string]any{"text": "hi", "origin": "commons"}}, http.StatusOK},
		// Its writer may edit it.
		{"person edits their own words", coAda, nil, forged, http.StatusOK},
		{"person renames their own", coAda, nil, map[string]any{"title": "hello"}, http.StatusOK},
		// A message no person wrote is not a person's words.
		{"agent edits its own pre-gate commons message", coAgent, map[string]any{"author": nil}, map[string]any{"fields": map[string]any{"text": "edited"}}, http.StatusOK},
	} {
		id := "gd-cm-" + strings.ReplaceAll(c.name, " ", "-")
		f.seedAdaMessage(id, c.seed)
		if c.name == "agent edits its own pre-gate commons message" {
			f.ms.beads[id].CreatedBy = "gardenio-crew-x"
		}
		before := string(f.ms.beads[id].Fields)
		code, body := f.do(t, c.token, "", http.MethodPatch, "/v1/beads/"+id, c.body)
		if code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.want)
			continue
		}
		if code == http.StatusForbidden {
			if c.token != coLand && !strings.Contains(body, "origin=commons") {
				t.Errorf("%s: refusal %q does not say why", c.name, body)
			}
			if string(f.ms.beads[id].Fields) != before || f.ms.beads[id].Title != "hi" {
				t.Errorf("%s: refused, but the message changed", c.name)
			}
		}
	}
}

// The field endpoints and a close body are write paths too.
func TestCommonsMessage_FieldEndpointsAndClose(t *testing.T) {
	f := newCommonsOriginFixture(t)
	for _, c := range []struct {
		name, token, path string
		body              any
		want              int
	}{
		{"static appends a file", coStatic, "/fields/files/append", map[string]any{"element": "x.png"}, http.StatusForbidden},
		{"agent appends a file", coAgent, "/fields/files/append", map[string]any{"element": "x.png"}, http.StatusForbidden},
		{"static edits text", coStatic, "/fields/text/edit", map[string]any{"op": "append", "element": "forged"}, http.StatusForbidden},
		{"static edits slack_ts", coStatic, "/fields/slack_ts/edit", map[string]any{"op": "remove", "element": "1690000000.000100"}, http.StatusForbidden},
		{"static closes with new text", coStatic, "/close", map[string]any{"text": "forged words", "slack_ts": ""}, http.StatusForbidden},
		{"agent closes with new text", coAgent, "/close", map[string]any{"text": "forged words"}, http.StatusForbidden},
	} {
		id := "gd-cm-" + strings.ReplaceAll(c.name, " ", "-")
		f.seedAdaMessage(id, nil)
		before := string(f.ms.beads[id].Fields)
		code, body := f.do(t, c.token, "", http.MethodPost, "/v1/beads/"+id+c.path, c.body)
		// The ownership gate refuses an agent's close first.
		if code != c.want || (c.token == coStatic && !strings.Contains(body, "origin=commons")) {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.want)
		}
		if string(f.ms.beads[id].Fields) != before {
			t.Errorf("%s: refused, but the message changed", c.name)
		}
	}
}

func TestCommonsMessage_EditGRPC(t *testing.T) {
	f := newCommonsOriginFixture(t)
	update := func(token, id, fields string) error {
		md := metadata.Pairs("authorization", "Bearer "+token)
		if token == coStatic {
			md.Append("x-operated-by", "slack:U0OPERATOR2")
		}
		ctx := metadata.NewIncomingContext(context.Background(), md)
		_, err := AuthInterceptor(withTestToken(f.fc, coStatic), f.verifier)(ctx,
			&beadsv1.UpdateBeadRequest{Id: id, Fields: []byte(fields)},
			&grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/UpdateBead"},
			func(ctx context.Context, req any) (any, error) {
				return f.srv.UpdateBead(ctx, req.(*beadsv1.UpdateBeadRequest))
			})
		return err
	}
	const forged = `{"text":"forged words","slack_ts":""}`
	for _, c := range []struct{ name, token string }{
		{"static token", coStatic},
		{"agent fleet token", coAgent},
		{"admin", coAdmin},
	} {
		id := "gd-cmg-" + strings.ReplaceAll(c.name, " ", "-")
		f.seedAdaMessage(id, nil)
		if err := update(c.token, id, forged); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "origin=commons") {
			t.Errorf("%s: err = %v, want PermissionDenied", c.name, err)
		}
	}
	f.seedAdaMessage("gd-cmg-stamp", map[string]any{"slack_channel": nil, "slack_ts": nil})
	if err := update(coStatic, "gd-cmg-stamp", `{"slack_channel":"C0OUT","slack_ts":"1.2"}`); err != nil {
		t.Errorf("bridge stamps an empty Slack ref: %v", err)
	}
	f.seedAdaMessage("gd-cmg-own", nil)
	if err := update(coAda, "gd-cmg-own", forged); err != nil {
		t.Errorf("person edits their own: %v", err)
	}
}

// Every caller but the writer, whatever its auth method, is refused.
func TestCommonsMessageEditDenial_Callers(t *testing.T) {
	f := newCommonsOriginFixture(t)
	f.seedAdaMessage("gd-cm-ctx", nil)
	bead := f.ms.beads["gd-cm-ctx"]
	in := &updateBeadInput{Fields: json.RawMessage(`{"text":"forged words","slack_ts":""}`)}
	for name, ctx := range map[string]context.Context{
		"machine":      WithAuthIdentity(withAuthMethod(context.Background(), authMethodMachine), AuthIdentity{Type: "machine", Subject: "machine:beads-viewer"}),
		"edge":         withAuthMethod(context.Background(), authMethodEdge),
		"edge v2":      edgeV2Ctx("ada@example.com"),
		"land":         withAuthMethod(context.Background(), authMethodLand),
		"static":       machineUserCtx(),
		"fleet":        fleetUserCtx(),
		"other person": oidcCtx("bob@example.com", nil),
	} {
		if d := commonsMessageEditDenial(ctx, bead, in); d == nil {
			t.Errorf("%s: rewrite allowed", name)
		}
		if d := commonsMessageFieldKeysDenial(ctx, "update", bead, "files"); d == nil {
			t.Errorf("%s: files append allowed", name)
		}
		if d := commonsMessageLockDenial(ctx, "close", "close it", bead); d == nil {
			t.Errorf("%s: close allowed", name)
		}
	}
	for name, ctx := range map[string]context.Context{
		"writer":        oidcCtx("ada@example.com", nil),
		"writer, case":  oidcCtx("Ada@Example.com", nil),
		"auth disabled": withAuthMethod(context.Background(), authMethodNone),
	} {
		if d := commonsMessageEditDenial(ctx, bead, in); d != nil {
			t.Errorf("%s: refused: %v", name, d)
		}
		if d := commonsMessageLockDenial(ctx, "close", "close it", bead); d != nil {
			t.Errorf("%s: close refused: %v", name, d)
		}
	}
}

// edgeV2Ctx is the context EdgeAuthMiddleware gives a verified v2 envelope
// for operator: OIDC-equivalent for role checks, but an edge identity.
func edgeV2Ctx(operator string) context.Context {
	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithRoles(ctx, []string{RoleViewer, RoleOperator})
	return WithAuthIdentity(ctx, AuthIdentity{Type: "edge", Subject: operator})
}

// laptop-claude's review of ac827da0: everything that makes a person's
// message say something else, or go somewhere else, under their name.

// commonsCallers are the non-writers every refusal is checked for.
var commonsCallers = []struct{ name, token string }{
	{"static", coStatic}, {"agent", coAgent}, {"land", coLand}, {"admin", coAdmin},
}

// snapshot is everything about a seeded message a refused write must leave.
func (f *commonsOriginFixture) snapshot(id string) string {
	b := f.ms.beads[id]
	labels := slices.Clone(f.ms.labels[id])
	slices.Sort(labels)
	return fmt.Sprintf("%s|%s|%s|%s|%v|%v", b.Title, b.Status, b.Fields, strings.Join(labels, ","), b.DeletedAt != nil, b.ClosedAt != nil)
}

// refused checks a write was refused and changed nothing, and, when why is
// set, that this gate refused it (a land principal or a fleet token may be
// refused by an earlier one).
func (f *commonsOriginFixture) refused(t *testing.T, what string, why bool, id, before string, code int, body string) {
	t.Helper()
	if code != http.StatusForbidden {
		t.Errorf("%s: %d %s, want 403", what, code, body)
		return
	}
	if why && !strings.Contains(body, "origin=commons") {
		t.Errorf("%s: refusal %q does not say why", what, body)
	}
	if after := f.snapshot(id); after != before {
		t.Errorf("%s: refused, but the message changed:\n%s\n%s", what, before, after)
	}
}

func TestCommonsMessage_SentBackPatchHTTP(t *testing.T) {
	f := newCommonsOriginFixture(t)
	bound := map[string]any{"slack_thread_channel": "C0THREAD", "slack_thread_ts": "1690000000.000300", "thread_id": "gd-head"}
	for _, c := range []struct {
		name string
		seed map[string]any
		body map[string]any
	}{
		{"relabels it thread-say and closes it", nil, map[string]any{"labels": []string{"project:gardenio", "thread-say"}, "status": "closed",
			"fields": map[string]any{"slack_thread_channel": "C0OTHER", "slack_thread_ts": "1690000000.000900"}}},
		{"relabels it", nil, map[string]any{"labels": []string{"project:gardenio", "thread-say"}}},
		{"closes it", nil, map[string]any{"status": "closed"}},
		{"binds it to a Slack thread", nil, map[string]any{"fields": map[string]any{"slack_thread_channel": "C0OTHER", "slack_thread_ts": "1690000000.000900"}}},
		{"moves its Slack thread", bound, map[string]any{"fields": map[string]any{"slack_thread_ts": "1690000000.000900"}}},
		{"sets thread_id once", nil, map[string]any{"fields": map[string]any{"thread_id": "gd-head"}}},
		{"moves thread_id", bound, map[string]any{"fields": map[string]any{"thread_id": "gd-other"}}},
		{"writes slack_root_text", nil, map[string]any{"fields": map[string]any{"slack_root_text": "forged words"}}},
		{"marks it deleted", nil, map[string]any{"fields": map[string]any{"deleted": "true"}}},
	} {
		for _, who := range commonsCallers {
			id := "gd-sb-" + who.name + "-" + strings.ReplaceAll(c.name, " ", "-")
			f.seedAdaMessage(id, c.seed)
			before := f.snapshot(id)
			code, body := f.do(t, who.token, "", http.MethodPatch, "/v1/beads/"+id, c.body)
			f.refused(t, who.name+" "+c.name, who.token == coStatic || who.token == coAdmin, id, before, code, body)
		}
	}
	// Its writer may.
	f.seedAdaMessage("gd-sb-own", nil)
	if code, body := f.do(t, coAda, "", http.MethodPatch, "/v1/beads/gd-sb-own", map[string]any{
		"labels": []string{"project:gardenio", "thread-say"}, "fields": map[string]any{"thread_id": "gd-head"},
	}); code != http.StatusOK {
		t.Errorf("person relabels their own: %d %s", code, body)
	}
}

// The other write paths: the field endpoints, labels, close and delete.
func TestCommonsMessage_SentBackOtherPathsHTTP(t *testing.T) {
	f := newCommonsOriginFixture(t)
	for _, c := range []struct {
		name, method, path string
		body               any
	}{
		{"appends to slack_thread_ts", http.MethodPost, "/fields/slack_thread_ts/append", map[string]any{"element": "1"}},
		{"edits slack_thread_channel", http.MethodPost, "/fields/slack_thread_channel/edit", map[string]any{"op": "append", "element": "C0OTHER"}},
		{"edits thread_id", http.MethodPost, "/fields/thread_id/edit", map[string]any{"op": "append", "element": "gd-head"}},
		{"edits slack_root_text", http.MethodPost, "/fields/slack_root_text/edit", map[string]any{"op": "append", "element": "forged"}},
		{"edits deleted", http.MethodPost, "/fields/deleted/edit", map[string]any{"op": "append", "element": "true"}},
		{"increments deleted", http.MethodPost, "/increment", map[string]any{"deleted": 1}},
		{"adds the thread-say label", http.MethodPost, "/labels", map[string]any{"label": "thread-say"}},
		{"removes its land label", http.MethodDelete, "/labels/project:gardenio", nil},
		{"closes it", http.MethodPost, "/close", map[string]any{}},
		{"closes it with new text", http.MethodPost, "/close", map[string]any{"text": "forged words", "slack_ts": ""}},
		{"deletes it", http.MethodDelete, "", nil},
	} {
		for _, who := range commonsCallers {
			id := "gd-sbo-" + who.name + "-" + strings.ReplaceAll(c.name, " ", "-")
			f.seedAdaMessage(id, nil)
			before := f.snapshot(id)
			code, body := f.do(t, who.token, "", c.method, "/v1/beads/"+id+c.path, c.body)
			// DELETE is admin-only, so it refuses the static token first.
			why := who.token == coAdmin || (who.token == coStatic && c.method+c.path != http.MethodDelete)
			f.refused(t, who.name+" "+c.name, why, id, before, code, body)
		}
	}
}

func TestCommonsMessage_SentBackGRPC(t *testing.T) {
	f := newCommonsOriginFixture(t)
	call := func(token, method string, req any, h func(context.Context, any) (any, error)) error {
		md := metadata.Pairs("authorization", "Bearer "+token)
		if token == coStatic {
			md.Append("x-operated-by", "slack:U0OPERATOR2")
		}
		ctx := metadata.NewIncomingContext(context.Background(), md)
		_, err := AuthInterceptor(withTestToken(f.fc, coStatic), f.verifier)(ctx, req,
			&grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/" + method}, h)
		return err
	}
	for _, c := range []struct {
		name, method string
		req          func(id string) any
		h            func(context.Context, any) (any, error)
	}{
		{"relabels and closes", "UpdateBead", func(id string) any {
			return &beadsv1.UpdateBeadRequest{Id: id, Labels: []string{"project:gardenio", "thread-say"}, Status: strPtr("closed")}
		}, func(ctx context.Context, r any) (any, error) {
			return f.srv.UpdateBead(ctx, r.(*beadsv1.UpdateBeadRequest))
		}},
		{"binds a Slack thread", "UpdateBead", func(id string) any {
			return &beadsv1.UpdateBeadRequest{Id: id, Fields: []byte(`{"slack_thread_channel":"C0OTHER","slack_thread_ts":"1.2","thread_id":"gd-head"}`)}
		}, func(ctx context.Context, r any) (any, error) {
			return f.srv.UpdateBead(ctx, r.(*beadsv1.UpdateBeadRequest))
		}},
		{"marks it deleted", "UpdateBead", func(id string) any {
			return &beadsv1.UpdateBeadRequest{Id: id, Fields: []byte(`{"deleted":"true","slack_root_text":"forged"}`)}
		}, func(ctx context.Context, r any) (any, error) {
			return f.srv.UpdateBead(ctx, r.(*beadsv1.UpdateBeadRequest))
		}},
		{"closes", "CloseBead", func(id string) any { return &beadsv1.CloseBeadRequest{Id: id} },
			func(ctx context.Context, r any) (any, error) {
				return f.srv.CloseBead(ctx, r.(*beadsv1.CloseBeadRequest))
			}},
		{"deletes", "DeleteBead", func(id string) any { return &beadsv1.DeleteBeadRequest{Id: id} },
			func(ctx context.Context, r any) (any, error) {
				return f.srv.DeleteBead(ctx, r.(*beadsv1.DeleteBeadRequest))
			}},
		{"adds a label", "AddLabel", func(id string) any { return &beadsv1.AddLabelRequest{BeadId: id, Label: "thread-say"} },
			func(ctx context.Context, r any) (any, error) {
				return f.srv.AddLabel(ctx, r.(*beadsv1.AddLabelRequest))
			}},
		{"removes a label", "RemoveLabel", func(id string) any { return &beadsv1.RemoveLabelRequest{BeadId: id, Label: "project:gardenio"} },
			func(ctx context.Context, r any) (any, error) {
				return f.srv.RemoveLabel(ctx, r.(*beadsv1.RemoveLabelRequest))
			}},
	} {
		for _, who := range commonsCallers {
			id := "gd-sbg-" + who.name + "-" + strings.ReplaceAll(c.name, " ", "-")
			f.seedAdaMessage(id, nil)
			before := f.snapshot(id)
			err := call(who.token, c.method, c.req(id), c.h)
			if status.Code(err) != codes.PermissionDenied && status.Code(err) != codes.Unauthenticated {
				t.Errorf("%s %s: err = %v, want PermissionDenied", who.name, c.name, err)
			} else if (who.token == coStatic || who.token == coAdmin) && !strings.Contains(err.Error(), "origin=commons") {
				t.Errorf("%s %s: refusal %v does not say why", who.name, c.name, err)
			}
			if after := f.snapshot(id); after != before {
				t.Errorf("%s %s: refused, but the message changed:\n%s\n%s", who.name, c.name, before, after)
			}
		}
	}
}

// A verified v2 edge envelope naming the writer is the edge vouching for her,
// not her own bearer: it neither writes origin=commons nor edits her message.
func TestCommonsMessage_EdgeV2HTTP(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.SetEdgeAuth(edgeauth.Config{HMACKey: []byte(testEdgeAuthKey), Now: func() time.Time { return now }})
	srv.SetRateLimit(RateLimitConfig{})
	ts := httptest.NewServer(srv.NewHTTPHandler(nil, withTestToken(nil, coStatic)))
	t.Cleanup(ts.Close)
	f := &commonsOriginFixture{srv: srv, ms: ms}
	f.seedAdaMessage("gd-edge-ada", nil)
	before := f.snapshot("gd-edge-ada")

	send := func(method, path string, body any) (int, string) {
		stamp := strconv.FormatInt(now.Unix(), 10)
		b, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(string(b)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-No-Auto-Parent", "1")
		roles := RoleOperator
		setV2EdgeHeaders(t, r, "ada@example.com", "entra", roles, "audit-cm", stamp,
			signEdgeV2(t, "ada@example.com", "entra", roles, "audit-cm", method, path, stamp))
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	code, body := send(http.MethodPatch, "/v1/beads/gd-edge-ada", map[string]any{"fields": map[string]any{"text": "forged words", "slack_ts": ""}})
	f.refused(t, "edge v2 as ada rewrites her message", true, "gd-edge-ada", before, code, body)
	code, body = send(http.MethodPost, "/v1/beads", commonsMessageBody(map[string]any{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com", "channel_id": "gd-chan"}))
	if code != http.StatusForbidden || !strings.Contains(body, "origin=commons") {
		t.Errorf("edge v2 as ada writes origin=commons: %d %s, want 403", code, body)
	}
}
