package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// The gatehouse steward's apply and demo tools (gatehouse_request.go, task
// gd-K62coPtKkX).

// seedGatehouse adds project beads to the land fixture: plot-a is the
// gatehouse, with its tools on or off, and plot-b an ordinary tenant land.
func seedGatehouse(f *landFixture, toolsOn bool) {
	fields := map[string]any{"land_kind": "tenant", "land_gatehouse": true}
	if toolsOn {
		fields["land_gatehouse_tools"] = true
	}
	raw, _ := json.Marshal(fields)
	f.ms.beads["gd-proj-a"] = &model.Bead{ID: "gd-proj-a", Type: model.TypeProject, Title: "plot-a", Status: model.StatusOpen, Fields: raw}
	f.ms.beads["gd-proj-b"] = &model.Bead{ID: "gd-proj-b", Type: model.TypeProject, Title: "plot-b", Status: model.StatusOpen, Fields: json.RawMessage(`{"land_kind":"tenant"}`)}
}

func gatehouseReq(project string) restrictedOpRequest {
	return restrictedOpRequest{Op: opGatehouseRequest, Action: "create", BeadType: typeGatehouseRequest, Project: project}
}

func TestGatehouseRequest_OnlyTheGatehouseStewardWithToolsOn(t *testing.T) {
	f := newLandFixture(t)
	srv := NewBeadsServer(f.ms, &events.NoopPublisher{})

	seedGatehouse(f, false)
	d := srv.restrictedOpDenial(f.steward, gatehouseReq("plot-a"))
	requireTenantDenial(t, d)
	if !strings.Contains(d.Message, "land_gatehouse_tools") {
		t.Fatalf("tools off: want the denial to name the switch, got %q", d.Message)
	}
	if gatehouseRequestCarveOut(f.steward, f.ms, []string{"project:plot-a"}) {
		t.Fatal("tools off: the carve-out must not lift the admin gate")
	}

	seedGatehouse(f, true)
	if d := srv.restrictedOpDenial(f.steward, gatehouseReq("plot-a")); d != nil {
		t.Fatalf("gatehouse steward, tools on: want allowed, got %+v", d)
	}
	if !gatehouseRequestCarveOut(f.steward, f.ms, []string{"project:plot-a"}) {
		t.Fatal("gatehouse steward, tools on: want the carve-out")
	}
	for name, labels := range map[string][]string{
		"another land":  {"project:plot-b"},
		"two lands":     {"project:plot-a", "project:plot-b"},
		"no land label": nil,
	} {
		if gatehouseRequestCarveOut(f.steward, f.ms, labels) {
			t.Errorf("%s: want no carve-out", name)
		}
	}
	requireTenantDenial(t, srv.restrictedOpDenial(f.steward, gatehouseReq("plot-b")))
	// The gardener of the same land is not the steward.
	requireTenantDenial(t, srv.restrictedOpDenial(f.gardener, gatehouseReq("plot-a")))
	if gatehouseRequestCarveOut(f.gardener, f.ms, []string{"project:plot-a"}) {
		t.Fatal("gardener: want no carve-out")
	}
	// A crew agent is not a tenant: the tenant rule passes it, and the
	// carve-out does not apply, so the type's admin gate holds it.
	if d := srv.restrictedOpDenial(f.crew, gatehouseReq("plot-a")); d != nil {
		t.Fatalf("crew agent is not a tenant, got %+v", d)
	}
	if gatehouseRequestCarveOut(f.crew, f.ms, []string{"project:plot-a"}) {
		t.Fatal("crew: want no carve-out")
	}

	// The steward of a land that is not the gatehouse files nothing, tools
	// field or not.
	f.ms.beads["gd-proj-a"].Fields = json.RawMessage(`{"land_kind":"tenant","land_gatehouse_tools":true}`)
	requireTenantDenial(t, srv.restrictedOpDenial(f.steward, gatehouseReq("plot-a")))
}

func gatehouseBead(labels []string, fields map[string]any) *model.Bead {
	raw, _ := json.Marshal(fields)
	return &model.Bead{Type: typeGatehouseRequest, Labels: labels, Fields: raw}
}

func TestGatehouseRequest_CreateRules(t *testing.T) {
	f := newLandFixture(t)
	seedGatehouse(f, true)
	ctx := context.Background()
	gh := []string{"project:plot-a"}
	ok := []map[string]any{
		{"kind": "demo", "message_id": "gd-m1", "state": "pending"},
		{"kind": "apply", "message_id": "gd-m1", "state": "pending", "land_name": "tomato-patch", "purpose": "grow a CLI", "credits_requested_usd": 25},
	}
	for _, fields := range ok {
		if err := validateGatehouseRequestCreate(ctx, f.ms, gatehouseBead(gh, fields)); err != nil {
			t.Errorf("%v: want accepted, got %v", fields, err)
		}
	}
	for name, tc := range map[string]struct {
		labels []string
		fields map[string]any
		want   string
	}{
		"not the gatehouse":    {[]string{"project:plot-b"}, ok[0], "not it"},
		"two projects":         {[]string{"project:plot-a", "project:plot-b"}, ok[0], "exactly one project label"},
		"not pending":          {gh, map[string]any{"kind": "demo", "message_id": "gd-m1", "state": "done"}, "created in state"},
		"outcome preset":       {gh, map[string]any{"kind": "demo", "message_id": "gd-m1", "state": "pending", "github_id": "7"}, "github_id"},
		"no message":           {gh, map[string]any{"kind": "demo", "state": "pending"}, "message_id"},
		"demo with a land":     {gh, map[string]any{"kind": "demo", "message_id": "gd-m1", "state": "pending", "land_name": "x-land"}, "carries no land_name"},
		"apply bad name":       {gh, map[string]any{"kind": "apply", "message_id": "gd-m1", "state": "pending", "land_name": "Bad_Name", "purpose": "p"}, "DNS label"},
		"apply reserved name":  {gh, map[string]any{"kind": "apply", "message_id": "gd-m1", "state": "pending", "land_name": "gardenio", "purpose": "p"}, "reserved"},
		"apply no purpose":     {gh, map[string]any{"kind": "apply", "message_id": "gd-m1", "state": "pending", "land_name": "tomato-patch"}, "purpose"},
		"apply long purpose":   {gh, map[string]any{"kind": "apply", "message_id": "gd-m1", "state": "pending", "land_name": "tomato-patch", "purpose": strings.Repeat("a", 1001)}, "limit"},
		"apply negative spend": {gh, map[string]any{"kind": "apply", "message_id": "gd-m1", "state": "pending", "land_name": "tomato-patch", "purpose": "p", "credits_requested_usd": -1}, "negative"},
		"unknown kind":         {gh, map[string]any{"kind": "grant", "message_id": "gd-m1", "state": "pending"}, "kind"},
	} {
		err := validateGatehouseRequestCreate(ctx, f.ms, gatehouseBead(tc.labels, tc.fields))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestGatehouseRequest_UpdateRules(t *testing.T) {
	apply := `{"kind":"apply","message_id":"gd-m1","land_name":"tomato-patch","purpose":"p","state":"pending"}`
	demo := `{"kind":"demo","message_id":"gd-m1","state":"pending"}`
	for name, tc := range map[string]struct {
		prior, next string
		ok          bool
	}{
		"apply drafted":      {apply, `{"kind":"apply","message_id":"gd-m1","land_name":"tomato-patch","purpose":"p","state":"drafted","outcome":"ok"}`, true},
		"demo done":          {demo, `{"kind":"demo","message_id":"gd-m1","state":"done","result_id":"gd-inv"}`, true},
		"demo drafted":       {demo, `{"kind":"demo","message_id":"gd-m1","state":"drafted"}`, true},
		"message changed":    {demo, `{"kind":"demo","message_id":"gd-m2","state":"pending"}`, false},
		"land name changed":  {apply, `{"kind":"apply","message_id":"gd-m1","land_name":"other-land","purpose":"p","state":"pending"}`, false},
		"kind changed":       {demo, `{"kind":"apply","message_id":"gd-m1","state":"pending"}`, false},
		"done is terminal":   {`{"kind":"demo","message_id":"gd-m1","state":"done"}`, `{"kind":"demo","message_id":"gd-m1","state":"pending"}`, false},
		"refused from draft": {`{"kind":"apply","message_id":"gd-m1","land_name":"tomato-patch","purpose":"p","state":"drafted"}`, `{"kind":"apply","message_id":"gd-m1","land_name":"tomato-patch","purpose":"p","state":"refused"}`, true},
		// Fixed fields compare as decoded values, type and presence included
		// (gd-R7meWqNxqr).
		"credits same":          {`{"kind":"apply","message_id":"gd-m1","credits_requested_usd":5,"state":"pending"}`, `{"kind":"apply","message_id":"gd-m1","credits_requested_usd":5.0,"state":"drafted"}`, true},
		"credits number→string": {`{"kind":"apply","message_id":"gd-m1","credits_requested_usd":5,"state":"pending"}`, `{"kind":"apply","message_id":"gd-m1","credits_requested_usd":"5","state":"pending"}`, false},
		"purpose added as null": {demo, `{"kind":"demo","message_id":"gd-m1","purpose":null,"state":"pending"}`, false},
		"purpose \"<nil>\"":     {demo, `{"kind":"demo","message_id":"gd-m1","purpose":"<nil>","state":"pending"}`, false},
		"purpose dropped":       {apply, `{"kind":"apply","message_id":"gd-m1","land_name":"tomato-patch","state":"pending"}`, false},
	} {
		err := validateGatehouseRequestUpdate(json.RawMessage(tc.prior), json.RawMessage(tc.next))
		if tc.ok != (err == nil) {
			t.Errorf("%s: ok=%v, got %v", name, tc.ok, err)
		}
	}
}

// One request per (message, kind), and a bounded number an hour
// (gd-EaTO1lFPAG): a steward led astray cannot name every message in the
// gatehouse.
func TestGatehouseRequest_OnePerMessageAndKind_HourlyLimit(t *testing.T) {
	f := newLandFixture(t)
	seedGatehouse(f, true)
	ctx := context.Background()
	now := time.Now()
	gh := []string{"project:plot-a"}
	seed := func(id, msg, kind, state string, at time.Time) {
		raw, _ := json.Marshal(map[string]any{"kind": kind, "message_id": msg, "state": state})
		f.ms.beads[id] = &model.Bead{ID: id, Type: typeGatehouseRequest, Status: model.StatusOpen, Labels: gh, Fields: raw, CreatedAt: at}
		f.ms.labels[id] = gh
	}
	newReq := func(msg, kind string) *model.Bead {
		return gatehouseBead(gh, map[string]any{"kind": kind, "message_id": msg, "state": "pending"})
	}

	seed("gd-r1", "gd-m1", "demo", "drafted", now.Add(-time.Minute))
	seed("gd-r2", "gd-m2", "demo", "refused", now.Add(-time.Minute))
	got, err := admitGatehouseRequest(ctx, f.ms, newReq("gd-m1", "demo"), now)
	if err != nil || got == nil || got.ID != "gd-r1" {
		t.Fatalf("same message and kind: want gd-r1 back, got %v, %v", got, err)
	}
	for name, b := range map[string]*model.Bead{
		"same message, other kind":  newReq("gd-m1", "apply"),
		"other message":             newReq("gd-m3", "demo"),
		"asked again after refusal": newReq("gd-m2", "demo"),
	} {
		if got, err := admitGatehouseRequest(ctx, f.ms, b, now); err != nil || got != nil {
			t.Errorf("%s: want a new request admitted, got %v, %v", name, got, err)
		}
	}

	// An hour's worth, two of them above, then the limit; older ones do not count.
	seed("gd-old", "gd-m-old", "demo", "done", now.Add(-2*time.Hour))
	for i := range gatehouseRequestsPerHour - 2 {
		seed("gd-q"+strconv.Itoa(i), "gd-mq"+strconv.Itoa(i), "demo", "pending", now.Add(-time.Duration(i+2)*time.Minute))
	}
	_, err = admitGatehouseRequest(ctx, f.ms, newReq("gd-m9", "demo"), now)
	if err == nil || !strings.Contains(err.Error(), "last hour") {
		t.Fatalf("at the hourly limit: want refused, got %v", err)
	}
	// A pair already asked for still reads its answer at the limit.
	if got, err := admitGatehouseRequest(ctx, f.ms, newReq("gd-m1", "demo"), now); err != nil || got == nil || got.ID != "gd-r1" {
		t.Fatalf("at the limit, same pair: want gd-r1 back, got %v, %v", got, err)
	}
	delete(f.ms.beads, "gd-q0")
	if _, err := admitGatehouseRequest(ctx, f.ms, newReq("gd-m9", "demo"), now); err != nil {
		t.Fatalf("under the limit: %v", err)
	}
}

func githubOIDCCtx(email string, id int64, login string) context.Context {
	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	return WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: email, GitHubID: id, GitHubLogin: login})
}

func TestMessageGitHubAuthor_StampedFromTheVerifiedToken(t *testing.T) {
	f := newLandFixture(t)
	seedGatehouse(f, true)
	gh := []string{"project:plot-a"}
	person := githubOIDCCtx("pat@example.com", 4242, "pat")
	stamp := func(ctx context.Context, b *model.Bead) *AuthzDenial {
		t.Helper()
		return stampMessageGitHubAuthor(ctx, f.ms, b)
	}
	msg := &model.Bead{Type: typeMessage, Labels: gh, Fields: json.RawMessage(`{"text":"can I try a land?","origin":"commons"}`)}
	if d := stamp(person, msg); d != nil {
		t.Fatalf("stamp: %+v", d)
	}
	got := fieldsAsMap(msg.Fields)
	if got[authorGitHubIDField] != "4242" || got[authorGitHubLoginField] != "pat" || got["text"] != "can I try a land?" {
		t.Fatalf("stamped fields = %v", got)
	}

	// Nobody names the GitHub account themselves, in any letter case and in
	// any land, not even a person naming their own.
	for _, fields := range []string{
		`{"text":"x","author_github_id":"1"}`,
		`{"text":"x","Author_GitHub_Login":"someone"}`,
		`{"text":"x","author_github_id":"4242"}`,
	} {
		for _, ctx := range []context.Context{person, withAuthMethod(context.Background(), authMethodStatic)} {
			for _, labels := range [][]string{gh, {"project:plot-b"}} {
				if d := stamp(ctx, &model.Bead{Type: typeMessage, Labels: labels, Fields: json.RawMessage(fields)}); d == nil {
					t.Errorf("%s in %v: want refused", fields, labels)
				}
			}
		}
	}

	// An agent's message, a person with no GitHub sign-in, a person's message
	// in any land but the gatehouse (gd-R7meWqNxqr) and an approved
	// escalation's replay carry none.
	for name, c := range map[string]struct {
		ctx    context.Context
		labels []string
	}{
		"agent":             {withAuthMethod(context.Background(), authMethodStatic), gh},
		"no GitHub sign-in": {githubOIDCCtx("op@example.com", 0, ""), gh},
		"another land":      {person, []string{"project:plot-b"}},
		"no land":           {person, nil},
		"escalation replay": {WithMachineOperator(person), gh},
	} {
		m := &model.Bead{Type: typeMessage, Labels: c.labels, Fields: json.RawMessage(`{"text":"hi"}`)}
		if d := stamp(c.ctx, m); d != nil {
			t.Fatalf("%s: unstamped create refused: %+v", name, d)
		}
		if _, ok := fieldsAsMap(m.Fields)[authorGitHubIDField]; ok {
			t.Fatalf("%s: stamped: %s", name, m.Fields)
		}
	}
	// Other types are not touched.
	task := &model.Bead{Type: "task", Labels: gh, Fields: json.RawMessage(`{"author_github_id":"1"}`)}
	if d := stamp(person, task); d != nil {
		t.Fatalf("task refused: %+v", d)
	}
}

func TestMessageGitHubAuthor_NeverChanges(t *testing.T) {
	stored := &model.Bead{Type: typeMessage, Fields: json.RawMessage(`{"text":"hi","author_github_id":"4242","author_github_login":"pat"}`)}
	if d := messageGitHubAuthorPatchDenial("update", stored, json.RawMessage(`{"text":"edited","author_github_id":"4242"}`)); d != nil {
		t.Fatalf("echo of the stored value refused: %+v", d)
	}
	for _, patch := range []string{
		`{"author_github_id":"1"}`,
		`{"author_github_login":"mallory"}`,
		`{"AUTHOR_GITHUB_ID":"4242"}`,
	} {
		if d := messageGitHubAuthorPatchDenial("update", stored, json.RawMessage(patch)); d == nil {
			t.Errorf("%s: want refused", patch)
		}
	}
	unstamped := &model.Bead{Type: typeMessage, Fields: json.RawMessage(`{"text":"hi"}`)}
	if d := messageGitHubAuthorPatchDenial("update", unstamped, json.RawMessage(`{"author_github_id":"4242"}`)); d == nil {
		t.Fatal("stamping an unstamped message by update: want refused")
	}
	if d := messageGitHubAuthorKeyDenial("update", stored, "Author_GitHub_ID"); d == nil {
		t.Fatal("field endpoint on the stamp: want refused")
	}
	if d := messageGitHubAuthorKeyDenial("update", stored, "text"); d != nil {
		t.Fatalf("field endpoint on text: %+v", d)
	}
}

// Over HTTP: the gatehouse's steward files a request on its own per-agent
// token while the tools are on, and cannot change its outcome; with the
// tools off it is refused, and an ordinary crew agent never gets past the
// admin gate.
func TestGatehouseRequest_HTTPStewardFiles(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	makeTierRoleBead(ms, "gd-role-steward", landsStewardRole, `{"capability_tiers":["lands-tenant"]}`)
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	// The token is bound to project gardenio, so here gardenio is the gatehouse.
	ms.beads["gd-proj-gh"] = &model.Bead{ID: "gd-proj-gh", Type: model.TypeProject, Title: "gardenio", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"land_kind":"tenant","land_gatehouse":true}`)}
	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", landsStewardRole)}

	body := map[string]any{
		"title": "Demo pass for message gd-m1", "type": "gatehouse_request", "kind": "data", "labels": []string{"project:gardenio"},
		"fields": map[string]any{"kind": "demo", "message_id": "gd-m1", "state": "pending"},
	}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, body, nil), http.StatusForbidden)

	ms.beads["gd-proj-gh"].Fields = json.RawMessage(`{"land_kind":"tenant","land_gatehouse":true,"land_gatehouse_tools":true}`)
	rec := doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, body, nil)
	requireStatus(t, rec, http.StatusCreated)
	var created model.Bead
	decodeJSON(t, rec, &created)

	// The same message and kind again is that request, not a second one.
	rec = doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, body, nil)
	var again model.Bead
	decodeJSON(t, rec, &again)
	if again.ID != created.ID {
		t.Fatalf("second request for gd-m1: got %s (status %d), want %s back", again.ID, rec.Code, created.ID)
	}

	// The outcome is the viewer's: the steward cannot mark its own request done.
	patch := map[string]any{"fields": map[string]any{"state": "done", "outcome": "here is your pass"}}
	requireStatus(t, doBearerJSON(t, h, "PATCH", "/v1/beads/"+created.ID, testAgentToken, patch, nil), http.StatusForbidden)

	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, body, nil), http.StatusForbidden)
}

// gatehouseRefusedCallers are the callers the gatehouse request path must
// refuse even with the gatehouse's tools on (merge desk, PR #1851): a person
// who is a member of the gatehouse, in a browser or on its land principal; a
// machine principal that is not the Gate; and the shared static token.
func gatehouseRefusedCallers(t *testing.T) map[string]context.Context {
	static := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "static"}), authMethodStatic)
	return map[string]context.Context{
		"land member (scoped OIDC)":    scopedCtx(t, []string{"/" + groupProjectPrefix + "plot-a"}),
		"land member (land principal)": landPrincipalContext(context.Background(), nil, "plot-a"),
		"signed-in operator":           oidcCtx("pat@example.com", []string{RoleViewer, RoleOperator}),
		"provisioner machine":          machineCtx("gardenio-controller", "gardenio-operator", "gardenio-machine-lands-provisioner", "gardenio-machine-controller-ownership"),
		"viewer without lands-gate":    machineCtx("gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-plan-reactor"),
		"canary machine":               machineCtx("gardenio-authz-canary"),
		"static token":                 static,
	}
}

func TestGatehouseRequest_RefusedCallers(t *testing.T) {
	f := newLandFixture(t)
	seedGatehouse(f, true)
	srv := NewBeadsServer(f.ms, &events.NoopPublisher{})
	gh := []string{"project:plot-a"}

	// Control: with the tools on the steward's create passes, so a refusal
	// below is the caller's and not the switch's.
	if d := srv.authorizeWrite(f.steward, "create", typeGatehouseRequest, gh); d != nil {
		t.Fatalf("control: gatehouse steward create denied: %+v", d)
	}
	for name, ctx := range gatehouseRefusedCallers(t) {
		t.Run(name, func(t *testing.T) {
			if gatehouseRequestCarveOut(ctx, f.ms, gh) {
				t.Fatal("the steward carve-out admitted a caller that is not the steward")
			}
			for _, action := range []string{"create", "update"} {
				if d := srv.authorizeWrite(ctx, action, typeGatehouseRequest, gh); d == nil {
					t.Errorf("%s gatehouse_request: admitted, want refused", action)
				}
			}
		})
	}

	// The Gate itself answers requests but never files one.
	gate := machineCtx("gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-plan-reactor", "gardenio-machine-lands-gate")
	if d := srv.authorizeWrite(gate, "create", typeGatehouseRequest, gh); d == nil {
		t.Error("lands-gate create gatehouse_request: admitted, want refused (the grant is update only)")
	}
	if d := srv.authorizeWrite(gate, "update", typeGatehouseRequest, gh); d != nil {
		t.Errorf("lands-gate update gatehouse_request: %+v", d)
	}
}

// Over HTTP, the shared static token is refused a create and an update on
// the request path, also when it names the steward in X-Actor, and nothing is
// stored.
func TestGatehouseRequest_HTTPStaticTokenRefused(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	makeTierRoleBead(ms, "gd-role-steward", landsStewardRole, `{"capability_tiers":["lands-tenant"]}`)
	ms.beads["gd-proj-gh"] = &model.Bead{ID: "gd-proj-gh", Type: model.TypeProject, Title: "gardenio", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"land_kind":"tenant","land_gatehouse":true,"land_gatehouse_tools":true}`)}
	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", landsStewardRole)}
	body := map[string]any{
		"title": "Demo pass for message gd-m1", "type": "gatehouse_request", "kind": "data", "labels": []string{"project:gardenio"},
		"fields": map[string]any{"kind": "demo", "message_id": "gd-m1", "state": "pending"},
	}
	// X-Operated-By resolves the operator, so the refusal is the write gate's.
	op := map[string]string{"X-Operated-By": "oidc:alice@example.com"}
	for _, hdr := range []map[string]string{op, {"X-Actor": "steward-a", "X-Operated-By": "oidc:alice@example.com"}} {
		requireStatus(t, doBearerJSON(t, h, "POST", "/v1/beads", testStaticToken, body, hdr), http.StatusForbidden)
	}
	for _, b := range ms.beads {
		if b.Type == typeGatehouseRequest {
			t.Fatalf("a refused create stored request %s", b.ID)
		}
	}

	rec := doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, body, nil)
	requireStatus(t, rec, http.StatusCreated)
	var created model.Bead
	decodeJSON(t, rec, &created)
	patch := map[string]any{"fields": map[string]any{"state": "done", "outcome": "here is your pass"}}
	requireStatus(t, doBearerJSON(t, h, "PATCH", "/v1/beads/"+created.ID, testStaticToken, patch, op), http.StatusForbidden)
}

// Over HTTP, a machine principal without the lands-gate grant can neither
// file a request nor answer one; the Gate answers one but files none.
func TestGatehouseRequest_HTTPMachinePrincipals(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-beads-viewer", "gardenio-controller"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	handler := srv.NewHTTPHandler(h.verifier, h.fc)
	ms.beads["gd-proj-gh"] = &model.Bead{ID: "gd-proj-gh", Type: model.TypeProject, Title: "plot-a", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"land_kind":"tenant","land_gatehouse":true,"land_gatehouse_tools":true}`)}
	ms.beads["gd-ghr1"] = &model.Bead{ID: "gd-ghr1", Type: typeGatehouseRequest, Kind: "data", Title: "Demo pass for message gd-m1",
		Status: model.StatusOpen, Labels: []string{"project:plot-a"}, CreatedBy: "steward-a",
		Fields: json.RawMessage(`{"kind":"demo","message_id":"gd-m1","state":"pending"}`)}

	body := map[string]any{
		"title": "Demo pass for message gd-m1", "type": "gatehouse_request", "kind": "data", "labels": []string{"project:plot-a"},
		"fields": map[string]any{"kind": "demo", "message_id": "gd-m1", "state": "pending"},
	}
	patch := map[string]any{"fields": map[string]any{"state": "refused", "outcome": "no"}}
	gate := h.saToken(t, "gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-plan-reactor", "gardenio-machine-lands-gate")
	for name, tok := range map[string]string{
		"viewer without lands-gate": h.saToken(t, "gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-plan-reactor"),
		"provisioner":               h.saToken(t, "gardenio-controller", "gardenio-operator", "gardenio-machine-lands-provisioner", "gardenio-machine-controller-ownership"),
	} {
		if rec := machineDo(t, handler, tok, http.MethodPost, "/v1/beads", body); rec.Code != http.StatusForbidden {
			t.Errorf("%s create: status=%d body=%s", name, rec.Code, rec.Body.String())
		}
		if rec := machineDo(t, handler, tok, http.MethodPatch, "/v1/beads/gd-ghr1", patch); rec.Code != http.StatusForbidden {
			t.Errorf("%s update: status=%d body=%s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := machineDo(t, handler, gate, http.MethodPost, "/v1/beads", body); rec.Code != http.StatusForbidden {
		t.Errorf("lands-gate create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	for id, b := range ms.beads {
		if b.Type == typeGatehouseRequest && id != "gd-ghr1" {
			t.Fatalf("a refused create stored request %s", id)
		}
	}
	if got := fieldsAsMap(ms.beads["gd-ghr1"].Fields)["state"]; got != "pending" {
		t.Fatalf("a refused update moved the request to %v", got)
	}
	if rec := machineDo(t, handler, gate, http.MethodPatch, "/v1/beads/gd-ghr1", patch); rec.Code != http.StatusOK {
		t.Errorf("lands-gate update: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// A request is answered once (gd-GPvddfQY6y): the reactor of a second viewer
// replica, working from the same pending list, cannot answer it again, while
// the person's own ask still settles the draft.
func TestGatehouseRequest_AnsweredOnce(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-beads-viewer"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(h.fc)
	handler := srv.NewHTTPHandler(h.verifier, h.fc)
	ms.beads["gd-proj-gh"] = &model.Bead{ID: "gd-proj-gh", Type: model.TypeProject, Title: "plot-a", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"land_kind":"tenant","land_gatehouse":true,"land_gatehouse_tools":true}`)}
	ms.beads["gd-ghr1"] = &model.Bead{ID: "gd-ghr1", Type: typeGatehouseRequest, Kind: "data", Title: "Demo pass for message gd-m1",
		Status: model.StatusOpen, Labels: []string{"project:plot-a"}, CreatedBy: "steward-a",
		Fields: json.RawMessage(`{"kind":"demo","message_id":"gd-m1","state":"pending"}`)}
	gate := h.saToken(t, "gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-plan-reactor", "gardenio-machine-lands-gate")
	answer := func(state, outcome string) map[string]any {
		return map[string]any{"fields": map[string]any{"state": state, "outcome": outcome, "handled_at": "2026-11-03T07:00:00Z"}}
	}

	if rec := machineDo(t, handler, gate, http.MethodPatch, "/v1/beads/gd-ghr1", answer("drafted", "ready")); rec.Code != http.StatusOK {
		t.Fatalf("first answer: status=%d body=%s", rec.Code, rec.Body.String())
	}
	for name, patch := range map[string]map[string]any{
		"the same answer again": answer("drafted", "ready"),
		"a refusal over it":     answer("refused", "full"),
	} {
		if rec := machineDo(t, handler, gate, http.MethodPatch, "/v1/beads/gd-ghr1", patch); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already answered") {
			t.Errorf("%s: status=%d body=%s, want 400 already answered", name, rec.Code, rec.Body.String())
		}
	}
	if got := fieldsAsMap(ms.beads["gd-ghr1"].Fields)["outcome"]; got != "ready" {
		t.Fatalf("a second answer changed the outcome to %v", got)
	}
	settle := map[string]any{"fields": map[string]any{"state": "done", "result_id": "gd-inv", "outcome": "asked"}}
	if rec := machineDo(t, handler, gate, http.MethodPatch, "/v1/beads/gd-ghr1", settle); rec.Code != http.StatusOK {
		t.Fatalf("settling the draft: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
