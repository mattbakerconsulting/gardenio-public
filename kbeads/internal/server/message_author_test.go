package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// A message's author is its writer unless a bridge or a machine names it
// (message_author.go, gd-PjNjRhAHPn).

const (
	maBridge  = "message-author-slack-bridge-tok" // the slack-bridge fleet token
	maRotated = "message-author-rotated-tok"      // a rotator-minted bridge:slack token
)

func newMessageAuthorFixture(t *testing.T) *commonsOriginFixture {
	t.Helper()
	f := newCommonsOriginFixture(t)
	f.fc.RegisterTokenFull(maBridge, "slack-bridge", "", nil, nil, time.Time{}, "", false)
	f.fc.RegisterTokenFull(maRotated, "slack", "", nil, nil, time.Time{}, "bridge:slack", false)
	return f
}

// slackMirrorFields are the fields mirrorSlackMessage writes for a person's
// Slack message (gardenio/controller/internal/bridge/commons_mirror.go).
func slackMirrorFields(author string) map[string]any {
	return map[string]any{
		"text": "hi from slack", "kind": "text", "author": author, "origin": "slack",
		"slack_channel": "C0MIRROR", "slack_ts": "1792500000.000100", "slack_user_id": "U0ADA",
	}
}

func TestMessageAuthor_CreateHTTP(t *testing.T) {
	f := newMessageAuthorFixture(t)
	for _, c := range []struct {
		name, token, actor string
		fields             map[string]any
		want               int
	}{
		// The hole: no origin, author names the owner.
		{"agent names the owner, no origin", coAgent, "", map[string]any{"text": "hi", "author": "oidc:owner@example.com"}, http.StatusForbidden},
		{"agent names the owner by bare email", coAgent, "", map[string]any{"text": "hi", "author": "owner@example.com"}, http.StatusForbidden},
		{"agent names another agent", coAgent, "", map[string]any{"text": "hi", "author": "agent:gardenio-crew-y"}, http.StatusForbidden},
		{"agent names a bridge", coAgent, "", map[string]any{"text": "hi", "author": "slack-bridge"}, http.StatusForbidden},
		{"agent names the owner, origin=slack", coAgent, "", slackMirrorFields("oidc:owner@example.com"), http.StatusForbidden},
		{"static names the owner", coStatic, "", map[string]any{"text": "hi", "author": "oidc:owner@example.com"}, http.StatusForbidden},
		{"static names an agent", coStatic, "", map[string]any{"text": "hi", "author": "agent:gardenio-crew-x"}, http.StatusForbidden},
		{"static, X-Actor agent, names the owner", coStatic, "gardenio-crew-x", map[string]any{"text": "hi", "author": "oidc:owner@example.com"}, http.StatusForbidden},
		{"land names the owner", coLand, "", map[string]any{"text": "hi", "author": "oidc:owner@example.com"}, http.StatusForbidden},
		// A caller naming itself, or no one.
		{"agent as agent:<self>", coAgent, "", map[string]any{"text": "hi", "author": "agent:gardenio-crew-x"}, http.StatusCreated},
		{"agent as its bare name", coAgent, "", map[string]any{"text": "hi", "author": "gardenio-crew-x"}, http.StatusCreated},
		{"agent, no author", coAgent, "", map[string]any{"text": "hi"}, http.StatusCreated},
		// coStatic is a privileged fleet token now (withTestToken): X-Actor is
		// not honoured (gd-ke6PC7bRto), so it cannot write as the agent it names.
		{"static, X-Actor agent, as that agent", coStatic, "gardenio-crew-x", map[string]any{"text": "hi", "author": "agent:gardenio-crew-x"}, http.StatusForbidden},
		{"static, no author", coStatic, "", map[string]any{"text": "hi"}, http.StatusCreated},
		// A bridge relays others' words: the Slack mirror and an agent's says.
		{"slack-bridge mirrors a person's Slack message", maBridge, "", slackMirrorFields("oidc:ada@example.com"), http.StatusCreated},
		{"slack-bridge writes an agent's say", maBridge, "", map[string]any{"text": "on it", "kind": "text", "author": "agent:gardenio-crew-x"}, http.StatusCreated},
		{"rotated bridge mirrors a person's Slack message", maRotated, "", slackMirrorFields("oidc:ada@example.com"), http.StatusCreated},
		// A person is not this gate's (the Commons shows their other
		// messages by created_by).
		{"person, no origin", coAda, "", map[string]any{"text": "hi", "author": "oidc:ada@example.com"}, http.StatusCreated},
	} {
		code, body := f.do(t, c.token, c.actor, http.MethodPost, "/v1/beads", commonsMessageBody(c.fields))
		if code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.want)
			continue
		}
		if code == http.StatusForbidden && !strings.Contains(body, "author must be its writer") {
			t.Errorf("%s: refusal %q does not say why", c.name, body)
		}
	}
}

func TestMessageAuthor_CreateGRPC(t *testing.T) {
	f := newMessageAuthorFixture(t)
	create := func(token, actor, fields string) error {
		md := metadata.Pairs("authorization", "Bearer "+token)
		if actor != "" {
			md.Append("x-actor", actor)
		}
		if token == coStatic {
			md.Append("x-operated-by", "slack:U0OPERATOR2")
		}
		ctx := metadata.NewIncomingContext(context.Background(), md)
		_, err := AuthInterceptor(f.fc, f.verifier)(ctx,
			&beadsv1.CreateBeadRequest{Title: "hi", Type: "message", Labels: []string{"project:gardenio"}, Fields: []byte(fields)},
			&grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/CreateBead"},
			func(ctx context.Context, req any) (any, error) {
				return f.srv.CreateBead(ctx, req.(*beadsv1.CreateBeadRequest))
			})
		return err
	}
	const forged = `{"text":"hi","author":"oidc:owner@example.com"}`
	for _, c := range []struct{ name, token, actor string }{
		{"agent fleet token", coAgent, ""},
		{"static token", coStatic, ""},
		{"static token, X-Actor agent", coStatic, "gardenio-crew-x"},
	} {
		if err := create(c.token, c.actor, forged); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "author must be its writer") {
			t.Errorf("%s: err = %v, want PermissionDenied", c.name, err)
		}
	}
	for _, c := range []struct{ name, token, fields string }{
		{"agent as itself", coAgent, `{"text":"hi","author":"agent:gardenio-crew-x"}`},
		{"slack-bridge mirrors a person", maBridge, `{"text":"hi","kind":"text","origin":"slack","author":"oidc:ada@example.com","slack_channel":"C0MIRROR","slack_ts":"1.2"}`},
	} {
		if err := create(c.token, "", c.fields); err != nil {
			t.Errorf("%s: %v, want created", c.name, err)
		}
	}
}

func (f *commonsOriginFixture) seedMessage(id, createdBy string, fields map[string]any) {
	fields["kind"] = "text"
	raw, _ := json.Marshal(fields)
	f.ms.beads[id] = &model.Bead{ID: id, Kind: model.KindData, Type: typeMessage, Title: "hi", Status: model.StatusOpen,
		CreatedBy: createdBy, Fields: raw, Fleet: "gardenio"}
	f.ms.labels[id] = []string{"project:gardenio"}
}

func TestMessageAuthor_UpdateHTTP(t *testing.T) {
	f := newMessageAuthorFixture(t)
	for _, c := range []struct {
		name, token, createdBy string
		fields, patch          map[string]any
		want                   int
	}{
		{"agent renames its own message as the owner", coAgent, "gardenio-crew-x",
			map[string]any{"text": "hi"}, map[string]any{"author": "oidc:owner@example.com"}, http.StatusForbidden},
		{"static renames a message as the owner", coStatic, "static",
			map[string]any{"text": "hi", "author": "agent:gardenio-crew-x"}, map[string]any{"author": "oidc:owner@example.com"}, http.StatusForbidden},
		{"agent sets itself as author", coAgent, "gardenio-crew-x",
			map[string]any{"text": "hi"}, map[string]any{"author": "agent:gardenio-crew-x"}, http.StatusOK},
		// The bridge repeats or stamps fields on a mirrored message.
		{"static repeats a mirrored author", coStatic, "slack-bridge",
			slackMirrorFields("oidc:ada@example.com"), map[string]any{"author": "oidc:ada@example.com", "text": "edited in slack"}, http.StatusOK},
		{"slack-bridge restamps a mirrored author", maBridge, "slack-bridge",
			slackMirrorFields("oidc:ada@example.com"), map[string]any{"author": "oidc:Ada@example.com"}, http.StatusOK},
	} {
		id := "gd-ma-" + strings.ReplaceAll(c.name, " ", "-")
		f.seedMessage(id, c.createdBy, c.fields)
		before := string(f.ms.beads[id].Fields)
		code, body := f.do(t, c.token, "", http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": c.patch})
		if code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.want)
			continue
		}
		if code == http.StatusForbidden {
			if !strings.Contains(body, "author must be its writer") {
				t.Errorf("%s: refusal %q does not say why", c.name, body)
			}
			if string(f.ms.beads[id].Fields) != before {
				t.Errorf("%s: refused, but the message changed", c.name)
			}
		}
	}
}

func TestMessageAuthor_UpdateGRPC(t *testing.T) {
	f := newMessageAuthorFixture(t)
	update := func(token, id, fields string) error {
		md := metadata.Pairs("authorization", "Bearer "+token)
		if token == coStatic {
			md.Append("x-operated-by", "slack:U0OPERATOR2")
		}
		ctx := metadata.NewIncomingContext(context.Background(), md)
		_, err := AuthInterceptor(f.fc, f.verifier)(ctx,
			&beadsv1.UpdateBeadRequest{Id: id, Fields: []byte(fields)},
			&grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/UpdateBead"},
			func(ctx context.Context, req any) (any, error) {
				return f.srv.UpdateBead(ctx, req.(*beadsv1.UpdateBeadRequest))
			})
		return err
	}
	f.seedMessage("gd-mag-agent", "gardenio-crew-x", map[string]any{"text": "hi"})
	if err := update(coAgent, "gd-mag-agent", `{"author":"oidc:owner@example.com"}`); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "author must be its writer") {
		t.Errorf("agent names the owner: err = %v, want PermissionDenied", err)
	}
	f.seedMessage("gd-mag-static", "static", map[string]any{"text": "hi"})
	if err := update(coStatic, "gd-mag-static", `{"author":"oidc:owner@example.com"}`); status.Code(err) != codes.PermissionDenied {
		t.Errorf("static names the owner: err = %v, want PermissionDenied", err)
	}
	f.seedMessage("gd-mag-bridge", "slack-bridge", slackMirrorFields("oidc:ada@example.com"))
	if err := update(maBridge, "gd-mag-bridge", `{"author":"oidc:Ada@example.com"}`); err != nil {
		t.Errorf("slack-bridge restamps a mirrored author: %v", err)
	}
}

// The field endpoints write no whole value to check, so author is refused
// through them.
func TestMessageAuthor_FieldEndpoints(t *testing.T) {
	f := newMessageAuthorFixture(t)
	f.seedMessage("gd-maf-1", "gardenio-crew-x", map[string]any{"text": "hi", "author": "agent:gardenio-crew-x"})
	before := string(f.ms.beads["gd-maf-1"].Fields)
	code, body := f.do(t, coAgent, "", http.MethodPost, "/v1/beads/gd-maf-1/fields/author/edit", map[string]any{"op": "append", "element": "x"})
	if code != http.StatusForbidden || !strings.Contains(body, "author must be its writer") {
		t.Errorf("agent edits author through a field endpoint: %d %s, want 403", code, body)
	}
	if string(f.ms.beads["gd-maf-1"].Fields) != before {
		t.Errorf("refused, but the message changed")
	}
}

// Who may name another author, whatever its auth method.
func TestMessageAuthorDenial_Callers(t *testing.T) {
	fields := json.RawMessage(`{"text":"hi","author":"oidc:owner@example.com"}`)
	fleetCtx := func(fleet, subject string) context.Context {
		ctx := withAuthMethod(context.Background(), authMethodStatic)
		ctx = WithFleet(ctx, fleet)
		return WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: subject})
	}
	for name, ctx := range map[string]context.Context{
		"static":                   machineUserCtx(),
		"agent fleet":              fleetCtx("agent-gd-abc", "gardenio-crew-x"),
		"agent fleet named bridge": fleetCtx("agent-gd-abc", "bridge:slack"),
		"ordinary fleet":           fleetCtx("gardenio", "gardenio-crew-x"),
		"land":                     WithAuthIdentity(withAuthMethod(context.Background(), authMethodLand), AuthIdentity{Type: "land", Subject: "land:gardenio"}),
		"edge v1":                  WithAuthIdentity(withAuthMethod(context.Background(), authMethodEdge), AuthIdentity{Type: "edge", Subject: "slack:U0ADA"}),
	} {
		if d := messageAuthorDenial(ctx, typeMessage, fields); d == nil {
			t.Errorf("%s: may name the owner", name)
		}
	}
	for name, ctx := range map[string]context.Context{
		"slack-bridge fleet": fleetCtx("slack-bridge", "slack-bridge"),
		"rotated bridge":     fleetCtx("slack", "bridge:slack"),
		"machine":            WithAuthIdentity(withAuthMethod(context.Background(), authMethodMachine), AuthIdentity{Type: "machine", Subject: "machine:gardenio-controller"}),
		"person":             oidcCtx("ada@example.com", nil),
		"edge v2":            edgeV2Ctx("ada@example.com"),
		"auth disabled":      withAuthMethod(context.Background(), authMethodNone),
	} {
		if d := messageAuthorDenial(ctx, typeMessage, fields); d != nil {
			t.Errorf("%s: refused: %v", name, d)
		}
	}
	// Not a message: not this gate's.
	if d := messageAuthorDenial(machineUserCtx(), "task", fields); d != nil {
		t.Errorf("task refused: %v", d)
	}
}

// The gates read origin and author by their exact keys and refuse a fields
// object that gives one twice or spells it in another letter case: a struct
// decode reads {"author":"oidc:<owner>","Author":"agent:<self>"} as the
// caller, while the store keeps both keys and the Commons shows the owner
// (PR #745 send-back). Main's case-fold guard (field_key_case.go) may refuse a
// pair first with 400; a repeated key or a lone variant reaches these gates.

// caseBypassFields are fields objects that try to slip a forged origin or
// author past the gates, each with the token that tries it.
var caseBypassFields = []struct{ name, token, fields string }{
	{"author, then Author naming the agent", coAgent, `{"text":"hi","author":"oidc:owner@example.com","Author":"agent:gardenio-crew-x"}`},
	{"Author naming the agent, then author", coAgent, `{"text":"hi","Author":"agent:gardenio-crew-x","author":"oidc:owner@example.com"}`},
	{"author given twice", coAgent, `{"text":"hi","author":"agent:gardenio-crew-x","author":"oidc:owner@example.com"}`},
	{"AUTHOR alone", coAgent, `{"text":"hi","AUTHOR":"oidc:owner@example.com"}`},
	{"author escaped and repeated", coAgent, `{"text":"hi","author":"agent:gardenio-crew-x","author":"oidc:owner@example.com"}`},
	{"static: author, then Author", coStatic, `{"text":"hi","author":"oidc:owner@example.com","Author":"static"}`},
	// A bridge may name any author, so origin is what it must not forge.
	{"bridge: origin commons, then Origin slack", maBridge, `{"text":"hi","kind":"text","origin":"commons","Origin":"slack","author":"oidc:ada@example.com"}`},
	{"bridge: origin given twice", maBridge, `{"text":"hi","kind":"text","origin":"slack","origin":"commons","author":"oidc:ada@example.com"}`},
	{"bridge: ORIGIN alone", maBridge, `{"text":"hi","kind":"text","ORIGIN":"commons","author":"oidc:ada@example.com"}`},
	{"agent: origin commons, then Origin slack", coAgent, `{"text":"hi","origin":"commons","Origin":"slack"}`},
}

// refusedCaseBypass reports whether code and body are a refusal of a
// case-bypass write: this gate's 403, or the case-fold guard's 400.
func refusedCaseBypass(code int, body string) bool {
	switch code {
	case http.StatusForbidden:
		return strings.Contains(body, "letter case")
	case http.StatusBadRequest:
		return strings.Contains(body, "case")
	}
	return false
}

// messageCount is the number of message beads stored. A refused write may
// file an escalation bead, which is not one.
func (f *commonsOriginFixture) messageCount() int {
	n := 0
	for _, b := range f.ms.beads {
		if b.Type == typeMessage {
			n++
		}
	}
	return n
}

func TestMessageOriginKeys_CreateHTTP(t *testing.T) {
	f := newMessageAuthorFixture(t)
	for _, c := range caseBypassFields {
		n := f.messageCount()
		body := map[string]any{"title": "hi", "type": "message", "status": "closed",
			"labels": []string{"project:gardenio"}, "fields": json.RawMessage(c.fields)}
		code, out := f.do(t, c.token, "", http.MethodPost, "/v1/beads", body)
		if !refusedCaseBypass(code, out) {
			t.Errorf("%s: %d %s, want refused", c.name, code, out)
		}
		if f.messageCount() != n {
			t.Errorf("%s: refused, but a message was stored", c.name)
		}
	}
}

func TestMessageOriginKeys_CreateGRPC(t *testing.T) {
	f := newMessageAuthorFixture(t)
	for _, c := range caseBypassFields {
		md := metadata.Pairs("authorization", "Bearer "+c.token)
		if c.token == coStatic {
			md.Append("x-operated-by", "slack:U0OPERATOR2")
		}
		ctx := metadata.NewIncomingContext(context.Background(), md)
		_, err := AuthInterceptor(f.fc, f.verifier)(ctx,
			&beadsv1.CreateBeadRequest{Title: "hi", Type: "message", Labels: []string{"project:gardenio"}, Fields: []byte(c.fields)},
			&grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/CreateBead"},
			func(ctx context.Context, req any) (any, error) {
				return f.srv.CreateBead(ctx, req.(*beadsv1.CreateBeadRequest))
			})
		if code := status.Code(err); (code != codes.PermissionDenied && code != codes.InvalidArgument) || !strings.Contains(err.Error(), "case") {
			t.Errorf("%s: err = %v, want refused", c.name, err)
		}
	}
}

func TestMessageOriginKeys_UpdateHTTP(t *testing.T) {
	f := newMessageAuthorFixture(t)
	for _, c := range caseBypassFields {
		id := "gd-mok-" + strings.ReplaceAll(c.name, " ", "-")
		f.seedMessage(id, "gardenio-crew-x", map[string]any{"text": "hi"})
		before := string(f.ms.beads[id].Fields)
		code, out := f.do(t, c.token, "", http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": json.RawMessage(c.fields)})
		if !refusedCaseBypass(code, out) {
			t.Errorf("%s: %d %s, want refused", c.name, code, out)
		}
		if string(f.ms.beads[id].Fields) != before {
			t.Errorf("%s: refused, but the message changed", c.name)
		}
	}
}

func TestMessageOriginKeys_UpdateGRPC(t *testing.T) {
	f := newMessageAuthorFixture(t)
	for _, c := range caseBypassFields {
		id := "gd-mokg-" + strings.ReplaceAll(c.name, " ", "-")
		f.seedMessage(id, "gardenio-crew-x", map[string]any{"text": "hi"})
		before := string(f.ms.beads[id].Fields)
		md := metadata.Pairs("authorization", "Bearer "+c.token)
		if c.token == coStatic {
			md.Append("x-operated-by", "slack:U0OPERATOR2")
		}
		ctx := metadata.NewIncomingContext(context.Background(), md)
		_, err := AuthInterceptor(f.fc, f.verifier)(ctx,
			&beadsv1.UpdateBeadRequest{Id: id, Fields: []byte(c.fields)},
			&grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/UpdateBead"},
			func(ctx context.Context, req any) (any, error) {
				return f.srv.UpdateBead(ctx, req.(*beadsv1.UpdateBeadRequest))
			})
		if code := status.Code(err); (code != codes.PermissionDenied && code != codes.InvalidArgument) || !strings.Contains(err.Error(), "case") {
			t.Errorf("%s: err = %v, want refused", c.name, err)
		}
		if string(f.ms.beads[id].Fields) != before {
			t.Errorf("%s: refused, but the message changed", c.name)
		}
	}
}

// The field endpoints name a key, in any letter case; a close body carries
// fields of its own.
func TestMessageOriginKeys_FieldEndpointsAndClose(t *testing.T) {
	f := newMessageAuthorFixture(t)
	for _, c := range []struct {
		name, token, path string
		body              any
		want              int
	}{
		{"agent edits Author", coAgent, "/fields/Author/edit", map[string]any{"op": "append", "element": "x"}, http.StatusForbidden},
		{"agent edits AUTHOR", coAgent, "/fields/AUTHOR/edit", map[string]any{"op": "append", "element": "x"}, http.StatusForbidden},
		{"agent closes naming the owner", coAgent, "/close", map[string]any{"author": "oidc:owner@example.com"}, http.StatusForbidden},
		{"agent closes naming the owner as Author", coAgent, "/close", json.RawMessage(`{"Author":"oidc:owner@example.com"}`), http.StatusForbidden},
		{"agent closes with author twice", coAgent, "/close", json.RawMessage(`{"author":"agent:gardenio-crew-x","author":"oidc:owner@example.com"}`), http.StatusForbidden},
		{"agent closes with origin commons", coAgent, "/close", map[string]any{"origin": "commons"}, http.StatusForbidden},
		{"bridge closes with ORIGIN commons", maBridge, "/close", json.RawMessage(`{"ORIGIN":"commons"}`), http.StatusForbidden},
		{"agent closes as itself", coAgent, "/close", map[string]any{"author": "agent:gardenio-crew-x"}, http.StatusOK},
		{"agent closes with a reason", coAgent, "/close", map[string]any{"reason": "done"}, http.StatusOK},
	} {
		id := "gd-mokf-" + strings.ReplaceAll(c.name, " ", "-")
		f.seedMessage(id, "gardenio-crew-x", map[string]any{"text": "hi"})
		before := string(f.ms.beads[id].Fields)
		code, out := f.do(t, c.token, "", http.MethodPost, "/v1/beads/"+id+c.path, c.body)
		if code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, out, c.want)
			continue
		}
		if code == http.StatusForbidden && string(f.ms.beads[id].Fields) != before {
			t.Errorf("%s: refused, but the message changed", c.name)
		}
	}
	// A person's Commons message: its origin, in any letter case, is theirs.
	f.seedAdaMessage("gd-mokf-ada", nil)
	if code, out := f.do(t, coStatic, "", http.MethodPost, "/v1/beads/gd-mokf-ada/fields/ORIGIN/edit", map[string]any{"op": "append", "element": "x"}); code != http.StatusForbidden || !strings.Contains(out, "origin=commons") {
		t.Errorf("static edits ORIGIN on a person's message: %d %s, want 403", code, out)
	}
}

func TestReadMessageOriginFields_ExactKeys(t *testing.T) {
	for _, c := range []struct {
		name, raw, origin, author, bad string
	}{
		{"plain", `{"origin":"commons","author":"oidc:ada@example.com"}`, "commons", "oidc:ada@example.com", ""},
		{"neither", `{"text":"hi"}`, "", "", ""},
		{"author then Author", `{"author":"oidc:owner@example.com","Author":"agent:x"}`, "", "oidc:owner@example.com", "Author"},
		{"Author alone", `{"Author":"oidc:owner@example.com"}`, "", "", "Author"},
		{"author twice", `{"author":"agent:x","author":"oidc:owner@example.com"}`, "", "agent:x", "author"},
		{"Origin then origin", `{"Origin":"slack","origin":"commons"}`, "commons", "", "Origin"},
		{"author not a string", `{"author":7}`, "", "", ""},
		{"not an object", `["author"]`, "", "", ""},
		{"nested author is not top-level", `{"snap":{"author":"oidc:owner@example.com","Author":"x"}}`, "", "", ""},
	} {
		f := readMessageOriginFields(json.RawMessage(c.raw))
		if f.origin() != c.origin || f.author() != c.author || f.BadKey != c.bad {
			t.Errorf("%s: origin %q author %q bad %q, want %q %q %q", c.name, f.origin(), f.author(), f.BadKey, c.origin, c.author, c.bad)
		}
	}
}
