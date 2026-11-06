package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// newAudienceTestServer serves the real HTTP auth chain: "primary-token" is
// the static token every agent pod holds; any other bearer reaches verifier.
func newAudienceTestServer(verifier TokenVerifier, audience string) (http.Handler, *mockStore) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	s.SetForwardAuthClients([]string{testBrowserClient})
	f := map[string]any{"operation_type": "gitlab_mr_merge", "operation_spec": map[string]any{}}
	if audience != "" {
		f["audience"] = audience
	}
	fields, _ := json.Marshal(f)
	ms.beads["gd-esc-aud"] = &model.Bead{
		ID: "gd-esc-aud", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "merge PR #1", Status: model.StatusOpen, Fields: fields,
	}
	return s.NewHTTPHandler(verifier, withTestToken(nil, "primary-token")), ms
}

func doBearer(t *testing.T, h http.Handler, bearer, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func audienceOf(t *testing.T, ms *mockStore) string {
	t.Helper()
	var f map[string]any
	_ = json.Unmarshal(ms.beads["gd-esc-aud"].Fields, &f)
	s, _ := f["audience"].(string)
	return s
}

func TestEscalationAudienceFieldDeclared(t *testing.T) {
	_, _, h := newTestServer()
	for aud, code := range map[string]int{"owner": 201, "operator": 201, "desk": 201, "nobody": 400} {
		rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
			"title": "merge", "type": "escalation", "kind": "data",
			"fields": map[string]any{
				"operation_type": "gitlab_mr_merge", "operation_spec": map[string]any{},
				"audience": aud,
			},
		})
		if rec.Code != code {
			t.Errorf("create with audience=%q: status %d, want %d; body %s", aud, rec.Code, code, rec.Body.String())
		}
	}
}

// TestEscalationAudienceStampedAtCreate: the daemon derives audience at
// create. A filer may raise an escalation to the owner but not pick operator or
// desk, and the forced-owner rule wins over whatever the filer says.
func TestEscalationAudienceStampedAtCreate(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{"merge, none named", map[string]any{"operation_type": "gitlab_mr_merge"}, "desk"},
		{"helm, none named", map[string]any{"operation_type": "helm_chart_change"}, "operator"},
		{"unknown type", map[string]any{"operation_type": "something_new"}, "operator"},
		{"filer asks owner", map[string]any{"operation_type": "gitlab_mr_merge", "audience": "owner"}, "owner"},
		{"filer may not pick desk", map[string]any{"operation_type": "helm_chart_change", "audience": "desk"}, "operator"},
		{"secret forces owner", map[string]any{"operation_type": "gitlab_mr_merge", "sensitivity": "secret", "audience": "desk"}, "owner"},
		{"credentials force owner", map[string]any{"operation_type": "gitlab_mr_merge", "credential_requirements": []any{map[string]any{"type": "github_token"}}}, "owner"},
		{"empty credentials do not", map[string]any{"operation_type": "gitlab_mr_merge", "credential_requirements": []any{}}, "desk"},
	}
	// escalation-routing D11: audience follows held_by, and D7 sends a merge to
	// the merge desk (desk) only in gardenio, so every row files there.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			f := map[string]any{"operation_spec": map[string]any{}}
			for k, v := range c.fields {
				f[k] = v
			}
			rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{"title": "esc", "type": "escalation", "kind": "data", "labels": []string{"project:gardenio"}, "fields": f})
			requireStatus(t, rec, 201)
			var out struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			got := escalationFieldValues(ms.beads[out.ID].Fields, []string{"audience"})
			if len(got) != 1 || got["audience"] != c.want {
				t.Errorf("stamped %v, want audience=%s", got, c.want)
			}
		})
	}

	// D11: a merge outside gardenio falls to D7's catch-all, held by the
	// laptop coordinator, so it reads operator rather than #756's desk default.
	t.Run("merge outside gardenio is held by the laptop", func(t *testing.T) {
		_, ms, h := newTestServer()
		rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{"title": "esc", "type": "escalation", "kind": "data", "fields": map[string]any{"operation_type": "gitlab_mr_merge", "operation_spec": map[string]any{}}})
		requireStatus(t, rec, 201)
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		got := escalationFieldValues(ms.beads[out.ID].Fields, []string{"audience", "held_by"})
		if got["audience"] != "operator" || got["held_by"] != "coordinator:laptop" {
			t.Errorf("stamped %v, want audience=operator held_by=coordinator:laptop", got)
		}
	})

	t.Run("gRPC create stamps too", func(t *testing.T) {
		s, ms, _ := newTestServer()
		resp, err := s.CreateBead(oidcAudienceCtx(RoleOperator), &beadsv1.CreateBeadRequest{
			Title: "esc", Type: "escalation", Kind: "data",
			Fields: []byte(`{"operation_type":"gitlab_mr_merge","operation_spec":{},"sensitivity":"secret","audience":"desk"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := escalationFieldValues(ms.beads[resp.GetBead().GetId()].Fields, []string{"audience"}); got["audience"] != "owner" {
			t.Errorf("gRPC create stamped %v, want owner", got)
		}
	})
}

// TestEscalationAudienceForcedByApprovers: an escalation whose approvers name
// a person, or which grants an agent role that only an admin or a person named
// by a spawn_policy rule may approve, is the owner's. The role comes from
// operation_spec, as the approval gate reads it, so a hand-filed spawn with no
// spawn_role is caught.
func TestEscalationAudienceForcedByApprovers(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	makeRoleBead(ms, "gd-role-infra", "infra", true)
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	makeRoleBead(ms, "gd-role-qa", "qa", false)
	makeRoleBead(ms, "gd-role-ops", "ops", false)
	r := spawnPolicyHumanRule(map[string]any{})
	r["approvers"] = []string{"oidc:owner@example.com"}
	seedSpawnPolicyBead(ms, "gd-sp-qa", "qa", []map[string]any{r})
	tr := spawnPolicyHumanRule(map[string]any{})
	tr["approvers"] = []string{"team:ops"}
	seedSpawnPolicyBead(ms, "gd-sp-ops", "ops", []map[string]any{tr})
	spawn := func(role string) string {
		return `{"operation_type":"create","operation_spec":{"title":"a","type":"agent","fields":{"role":"` + role + `"}}}`
	}
	for _, c := range []struct {
		name, fields string
		want         bool
	}{
		{"a merge", `{"operation_type":"gitlab_mr_merge"}`, false},
		{"approvers name a person", `{"approvers":["oidc:owner@example.com"]}`, true},
		{"a person among teams", `{"approvers":["owner@example.com","team:ops"]}`, true},
		{"team-only approvers", `{"approvers":["team:ops"]}`, false},
		{"lane-ref approvers are not people", `{"approvers":["coordinator:merge-desk","operator"],"operation_type":"gitlab_mr_merge"}`, false},
		{"slack approver is a person", `{"approvers":["slack:U0ABC"]}`, true},
		{"empty approvers", `{"approvers":[]}`, false},
		{"hand-filed privileged spawn", spawn("infra"), true},
		{"hand-filed privileged re-role", `{"operation_type":"update","operation_spec":{"fields":{"role":"infra"}}}`, true},
		{"spawn_policy names the owner", spawn("qa"), true},
		{"spawn_policy names a team", spawn("ops"), false},
		{"unprivileged spawn, no policy", spawn("crew"), false},
		{"spawn role unresolvable", `{"operation_type":"create","operation_spec":{"title":"a","type":"agent","fields":{}}}`, true},
		{"spawn_role field alone is not read", `{"operation_type":"gitlab_mr_merge","spawn_role":"crew"}`, false},
		{"a non-agent create", `{"operation_type":"create","operation_spec":{"title":"a","type":"task"}}`, false},
		{"credentials not json", `{"credential_requirements":"not json"}`, true},
		{"credentials empty list string", `{"credential_requirements":"[]"}`, false},
		{"credentials null", `{"credential_requirements":null}`, false},
		{"SECRET", `{"sensitivity":"SECRET"}`, true},
	} {
		bead := &model.Bead{Type: model.TypeEscalation, CreatedBy: "gd-agent-1", Fields: json.RawMessage(c.fields)}
		if got := s.escalationAudienceForced(context.Background(), bead); got != c.want {
			t.Errorf("%s: forced(%s) = %v, want %v", c.name, c.fields, got, c.want)
		}
	}

	// The daemon's own spawn escalation stamps approvers, and so owner.
	bead := &model.Bead{Type: model.TypeEscalation, Fields: json.RawMessage(`{"operation_type":"create","spawn_role":"crew","approvers":["oidc:owner@example.com"]}`)}
	if err := s.stampEscalationAudience(withServerStampedEscalation(context.Background()), bead); err != nil {
		t.Fatal(err)
	}
	if got := escalationFieldValues(bead.Fields, []string{"audience"}); got["audience"] != "owner" {
		t.Errorf("approvers-named spawn escalation stamped %v, want owner", got)
	}
}

// TestEscalationAudienceHandFiledSpawnIsOwners: an agent's own hand-filed
// create-agent escalation for a privileged role, or for a role whose
// spawn_policy names the owner, is stamped owner at create.
func TestEscalationAudienceHandFiledSpawnIsOwners(t *testing.T) {
	for role, want := range map[string]string{"infra": "owner", "qa": "owner", "crew": "operator"} {
		t.Run(role, func(t *testing.T) {
			_, ms, h := newTestServer()
			makeRoleBead(ms, "gd-role-infra", "infra", true)
			makeRoleBead(ms, "gd-role-crew", "crew", false)
			makeRoleBead(ms, "gd-role-qa", "qa", false)
			r := spawnPolicyHumanRule(map[string]any{})
			r["approvers"] = []string{"oidc:owner@example.com"}
			seedSpawnPolicyBead(ms, "gd-sp-qa", "qa", []map[string]any{r})
			rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{"title": "spawn", "type": "escalation", "kind": "data", "fields": map[string]any{
				"operation_type": "create",
				"operation_spec": map[string]any{"title": "a", "type": "agent", "fields": map[string]any{"role": role}},
				"audience":       "operator",
			}})
			requireStatus(t, rec, 201)
			var out struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if got := escalationFieldValues(ms.beads[out.ID].Fields, []string{"audience"}); got["audience"] != want {
				t.Errorf("hand-filed %s spawn stamped %v, want %s", role, got, want)
			}
		})
	}
}

// TestEscalationCloseCannotRewriteFrozenFields: the generic close body cannot
// carry audience, sensitivity or credential_requirements onto an escalation,
// and a reject (which closes) followed by a reopen cannot move the audience.
func TestEscalationCloseCannotRewriteFrozenFields(t *testing.T) {
	body := map[string]any{"audience": "desk", "sensitivity": "internal", "credential_requirements": ""}
	secret := func(ms *mockStore) {
		ms.beads["gd-esc-aud"].Fields = json.RawMessage(`{"operation_type":"gitlab_mr_merge","operation_spec":{},"audience":"owner","sensitivity":"secret"}`)
	}
	for _, st := range []model.Status{model.StatusOpen, model.StatusInProgress, model.StatusClosed} {
		t.Run("close/"+string(st), func(t *testing.T) {
			h, ms := newAudienceTestServer(&stubOIDCVerifier{email: "op@example.com", roles: []string{RoleOperator}}, "owner")
			secret(ms)
			ms.beads["gd-esc-aud"].Status = st
			before := string(ms.beads["gd-esc-aud"].Fields)
			rec := doBearer(t, h, "primary-token", "POST", "/v1/beads/gd-esc-aud/close", body)
			if st != model.StatusClosed && rec.Code < 400 {
				t.Errorf("close of a %s escalation: status %d, want refused", st, rec.Code)
			}
			if after := string(ms.beads["gd-esc-aud"].Fields); after != before {
				t.Errorf("close rewrote fields: %s -> %s", before, after)
			}
		})
	}

	t.Run("the close body check itself", func(t *testing.T) {
		esc := &model.Bead{Type: model.TypeEscalation}
		for _, k := range []string{"audience", "sensitivity", "credential_requirements"} {
			if checkEscalationFrozenFieldEndpoint(oidcAudienceCtx(RoleAdmin), esc, k) == nil {
				t.Errorf("%s passes the close-body check", k)
			}
		}
	})

	t.Run("reject then reopen", func(t *testing.T) {
		// A floor reject needs the page session (P10.B), so the admin signs in
		// through the browser client.
		h, ms := newAudienceTestServer(&azpVerifier{clientID: testBrowserClient}, "owner")
		secret(ms)
		requireStatus(t, doBearer(t, h, "oidc-jwt", "POST", "/v1/escalations/gd-esc-aud/reject", map[string]any{"reason": "no"}), 200)
		requireStatus(t, doBearer(t, h, "primary-token", "PATCH", "/v1/beads/gd-esc-aud",
			map[string]any{"status": "open", "fields": body}), 400)
		requireStatus(t, doBearer(t, h, "primary-token", "PATCH", "/v1/beads/gd-esc-aud", map[string]any{"status": "open"}), 200)
		got := escalationFieldValues(ms.beads["gd-esc-aud"].Fields, escalationFrozenFields)
		if got["audience"] != "owner" || got["sensitivity"] != "secret" {
			t.Errorf("after reject and reopen the frozen fields are %v", got)
		}
	})
}

func oidcAudienceCtx(roles ...string) context.Context {
	ctx := WithAuthIdentity(withAuthMethod(context.Background(), authMethodOIDC), AuthIdentity{Type: "oidc", Subject: "op@example.com"})
	return WithRoles(ctx, roles)
}

// TestEscalationFrozenFields: after filing, no generic write changes audience,
// sensitivity or credential_requirements, on HTTP or gRPC, whoever asks; an
// admin re-routes through the route endpoint like an operator. Other fields
// stay writable.
func TestEscalationFrozenFields(t *testing.T) {
	writes := []struct {
		name  string
		patch map[string]any
	}{
		{"audience", map[string]any{"audience": "owner"}},
		{"audience removed", map[string]any{"audience": nil}},
		{"sensitivity", map[string]any{"sensitivity": "internal"}},
		{"sensitivity added", map[string]any{"sensitivity": "secret"}},
		{"credential_requirements", map[string]any{"credential_requirements": []any{}}},
		{"credential_requirements added", map[string]any{"credential_requirements": []any{map[string]any{"type": "x"}}}},
	}
	callers := []struct {
		name   string
		roles  []string
		bearer string
	}{
		{"agent static token", []string{RoleAdmin}, "primary-token"},
		{"oidc operator", []string{RoleOperator}, "oidc-jwt"},
		{"oidc admin", []string{RoleAdmin}, "oidc-jwt"},
	}
	for _, w := range writes {
		for _, c := range callers {
			t.Run("PATCH/"+w.name+"/"+c.name, func(t *testing.T) {
				h, ms := newAudienceTestServer(&stubOIDCVerifier{email: "op@example.com", roles: c.roles}, "desk")
				before := string(ms.beads["gd-esc-aud"].Fields)
				rec := doBearer(t, h, c.bearer, "PATCH", "/v1/beads/gd-esc-aud", map[string]any{"fields": w.patch})
				requireStatus(t, rec, 400)
				if !strings.Contains(rec.Body.String(), "fixed when an escalation is filed") {
					t.Errorf("refused for the wrong reason: %s", rec.Body.String())
				}
				if after := string(ms.beads["gd-esc-aud"].Fields); after != before {
					t.Errorf("fields changed: %s -> %s", before, after)
				}
			})
		}
		t.Run("gRPC/"+w.name, func(t *testing.T) {
			ms := newMockStore()
			s := NewBeadsServer(ms, &events.NoopPublisher{})
			ms.beads["gd-esc-aud"] = &model.Bead{ID: "gd-esc-aud", Kind: model.KindData, Type: model.TypeEscalation, Status: model.StatusOpen,
				Title: "merge PR #1", Fields: json.RawMessage(`{"operation_type":"gitlab_mr_merge","audience":"desk","sensitivity":"public"}`)}
			patch, _ := json.Marshal(w.patch)
			_, err := s.UpdateBead(oidcAudienceCtx(RoleAdmin), &beadsv1.UpdateBeadRequest{Id: "gd-esc-aud", Fields: patch})
			if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "fixed when an escalation is filed") {
				t.Fatalf("gRPC UpdateBead %s: err = %v, want InvalidArgument (frozen)", w.name, err)
			}
		})
	}

	t.Run("other fields stay writable", func(t *testing.T) {
		h, _ := newAudienceTestServer(&stubOIDCVerifier{email: "op@example.com", roles: []string{RoleAdmin}}, "desk")
		rec := doBearer(t, h, "primary-token", "PATCH", "/v1/beads/gd-esc-aud",
			map[string]any{"fields": map[string]any{"pingback_reason": "still waiting", "audience": "desk"}})
		requireStatus(t, rec, 200)
	})

	for _, field := range []string{"audience", "sensitivity", "credential_requirements"} {
		t.Run("field endpoint/"+field, func(t *testing.T) {
			h, ms := newAudienceTestServer(&stubOIDCVerifier{email: "op@example.com", roles: []string{RoleAdmin}}, "desk")
			rec := doBearer(t, h, "oidc-jwt", "POST", "/v1/beads/gd-esc-aud/fields/"+field+"/edit",
				map[string]any{"path": "", "value": "owner"})
			requireStatus(t, rec, 400)
			if got := audienceOf(t, ms); got != "desk" {
				t.Errorf("audience = %q, want desk", got)
			}
		})
		t.Run("result/"+field, func(t *testing.T) {
			h, ms := newAudienceTestServer(&stubOIDCVerifier{email: "op@example.com", roles: []string{RoleAdmin}}, "desk")
			rec := doBearer(t, h, "oidc-jwt", "POST", "/v1/escalations/gd-esc-aud/result",
				map[string]any{"result_status": "success", "fields": map[string]string{field: "owner"}})
			requireStatus(t, rec, 400)
			if ms.beads["gd-esc-aud"].Status != model.StatusOpen {
				t.Error("escalation closed despite the refused field")
			}
		})
	}
}

// TestEscalationRouteEndpoint: POST /v1/escalations/{id}/audience is the one
// way to re-route, for an operator or admin on their own sign-in. The agent
// token, a viewer and a v1 edge envelope may not, and a forced escalation
// stays with the owner.
func TestEscalationRouteEndpoint(t *testing.T) {
	cases := []struct {
		name   string
		roles  []string
		bearer string
		want   int
	}{
		{"agent static token", []string{RoleAdmin}, "primary-token", 403},
		{"oidc viewer", []string{RoleViewer}, "oidc-jwt", 403},
		{"oidc operator", []string{RoleOperator}, "oidc-jwt", 200},
		{"oidc admin", []string{RoleAdmin}, "oidc-jwt", 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, ms := newAudienceTestServer(&stubOIDCVerifier{email: "op@example.com", roles: c.roles}, "desk")
			rec := doBearer(t, h, c.bearer, "POST", "/v1/escalations/gd-esc-aud/audience",
				map[string]any{"audience": "owner", "reason": "needs a product call"})
			requireStatus(t, rec, c.want)
			want := "desk"
			if c.want == 200 {
				want = "owner"
			}
			if got := audienceOf(t, ms); got != want {
				t.Errorf("audience = %q, want %q", got, want)
			}
			comments := ms.comments["gd-esc-aud"]
			if c.want == 200 {
				if len(comments) != 1 || comments[0].Text != "Audience re-routed: desk → owner by op@example.com: needs a product call" {
					t.Errorf("audit comment = %+v", comments)
				}
			} else if len(comments) != 0 {
				t.Errorf("refused route left a comment: %+v", comments)
			}
		})
	}

	t.Run("v1 edge envelope", func(t *testing.T) {
		s := NewBeadsServer(newMockStore(), &events.NoopPublisher{})
		if callerMayRouteEscalation(v1EdgeCtx()) {
			t.Fatal("callerMayRouteEscalation(v1 edge) = true")
		}
		req := httptest.NewRequest("POST", "/v1/escalations/gd-esc-aud/audience", strings.NewReader(`{"audience":"desk"}`)).WithContext(v1EdgeCtx())
		req.SetPathValue("id", "gd-esc-aud")
		rec := httptest.NewRecorder()
		s.handleRouteEscalation(rec, req)
		requireStatus(t, rec, 403)
	})

	t.Run("v2 envelope with operator role", func(t *testing.T) {
		ctx := WithRoles(WithAuthIdentity(withAuthMethod(context.Background(), authMethodOIDC), AuthIdentity{Type: "edge", Subject: "slack:U0TESTER1"}), []string{RoleOperator})
		if !callerMayRouteEscalation(ctx) {
			t.Error("a verified v2 envelope with the operator role may re-route")
		}
	})

	t.Run("forced escalation stays with the owner", func(t *testing.T) {
		h, ms := newAudienceTestServer(&stubOIDCVerifier{email: "op@example.com", roles: []string{RoleAdmin}}, "owner")
		ms.beads["gd-esc-aud"].Fields = setFieldValue(ms.beads["gd-esc-aud"].Fields, "sensitivity", "secret")
		rec := doBearer(t, h, "oidc-jwt", "POST", "/v1/escalations/gd-esc-aud/audience", map[string]any{"audience": "desk"})
		requireStatus(t, rec, 409)
		if got := audienceOf(t, ms); got != "owner" {
			t.Errorf("audience = %q, want owner", got)
		}
	})

	t.Run("empty audience stamps the derived one", func(t *testing.T) {
		for _, c := range []struct{ stored, extra, want string }{
			{"", "", "desk"},             // the merge default
			{"operator", "", "operator"}, // a valid stored choice is kept
			{"bogus", "", "desk"},
			{"desk", `{"approvers":["oidc:owner@example.com"]}`, "owner"},
		} {
			h, ms := newAudienceTestServer(&stubOIDCVerifier{email: "op@example.com", roles: []string{RoleOperator}}, c.stored)
			if c.extra != "" {
				ms.beads["gd-esc-aud"].Fields = setFieldValue(ms.beads["gd-esc-aud"].Fields, "approvers", []string{"oidc:owner@example.com"})
			}
			rec := doBearer(t, h, "oidc-jwt", "POST", "/v1/escalations/gd-esc-aud/audience", map[string]any{})
			requireStatus(t, rec, 200)
			if got := audienceOf(t, ms); got != c.want {
				t.Errorf("stored %q %s: derived %q, want %q", c.stored, c.extra, got, c.want)
			}
			if c.stored == c.want && len(ms.comments["gd-esc-aud"]) != 0 {
				t.Errorf("a no-change route left a comment")
			}
		}
	})

	t.Run("bad audience", func(t *testing.T) {
		h, _ := newAudienceTestServer(&stubOIDCVerifier{email: "op@example.com", roles: []string{RoleAdmin}}, "desk")
		requireStatus(t, doBearer(t, h, "oidc-jwt", "POST", "/v1/escalations/gd-esc-aud/audience", map[string]any{"audience": "nobody"}), 400)
	})

	t.Run("closed escalation", func(t *testing.T) {
		h, ms := newAudienceTestServer(&stubOIDCVerifier{email: "op@example.com", roles: []string{RoleAdmin}}, "desk")
		ms.beads["gd-esc-aud"].Status = model.StatusClosed
		requireStatus(t, doBearer(t, h, "oidc-jwt", "POST", "/v1/escalations/gd-esc-aud/audience", map[string]any{"audience": "owner"}), 409)
	})
}

// TestEscalationApprovalIgnoresAudience proves audience only shapes display
// and notification: for each caller, approving gives the same answer whatever
// the escalation's audience, including none.
func TestEscalationApprovalIgnoresAudience(t *testing.T) {
	callers := []struct {
		name     string
		verifier TokenVerifier
		bearer   string
		want     int
	}{
		{"browser-signed-in admin", &azpVerifier{clientID: testBrowserClient}, "oidc-jwt", 200},
		{"agent static token", &azpVerifier{clientID: testBrowserClient}, "primary-token", 403},
		{"oidc viewer", &stubOIDCVerifier{email: "viewer@example.com", roles: []string{RoleViewer}}, "oidc-jwt", 403},
	}
	for _, c := range callers {
		for _, aud := range []string{"", "owner", "operator", "desk"} {
			t.Run(c.name+"/audience="+aud, func(t *testing.T) {
				h, ms := newAudienceTestServer(c.verifier, aud)
				rec := doBearer(t, h, c.bearer, "POST", "/v1/escalations/gd-esc-aud/approve", map[string]any{})
				requireStatus(t, rec, c.want)
				approved := ms.beads["gd-esc-aud"].Status == model.StatusInProgress
				if approved != (c.want == 200) {
					t.Errorf("approved=%v, want %v", approved, c.want == 200)
				}
			})
		}
	}
}
