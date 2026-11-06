package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// newRunnerTestServer seeds runner gd-r1 (owner owner@example.com, projects
// [pet]) with a live token, runner gd-r2 of another owner, agent gd-a1 on
// gd-r1, agent gd-a2 on no runner, an escalation, and tasks in pet and in
// fleet-internal. It returns the handler and gd-r1's bearer.
func newRunnerTestServer(t *testing.T) (*mockStore, http.Handler, string) {
	t.Helper()
	_, ms, h, _ := newRunnerTestServerBare(t)
	return ms, h, runnerTestToken(t, ms)
}

// newRunnerTestServerBare is newRunnerTestServer that also returns the handler
// without defaultBearerMiddleware, for requests that must carry no bearer.
func newRunnerTestServerBare(t *testing.T) (*BeadsServer, *mockStore, http.Handler, http.Handler) {
	t.Helper()
	s, ms, h := newTestServer()
	return s, ms, h, s.NewHTTPHandler(&bearerSubjectVerifier{})
}

func runnerTestToken(t *testing.T, ms *mockStore) string {
	t.Helper()
	token, hash, err := mintRunnerToken()
	if err != nil {
		t.Fatal(err)
	}
	ms.runnerTokens = map[string]*model.RunnerToken{hash: {TokenHash: hash, RunnerID: "gd-r1", CreatedAt: time.Now()}}
	seed := func(b *model.Bead) {
		ms.beads[b.ID] = b
		if len(b.Labels) > 0 {
			ms.labels[b.ID] = append([]string(nil), b.Labels...)
		}
	}
	ms.beadsMu.Lock()
	seed(&model.Bead{ID: "gd-r1", Type: "runner", Kind: model.KindConfig, Title: "laptop", Status: model.StatusOpen, Labels: []string{"project:pet"},
		Fields: []byte(`{"owner":"oidc:owner@example.com","projects":["pet"],"state":"online"}`)})
	seed(&model.Bead{ID: "gd-r2", Type: "runner", Kind: model.KindConfig, Title: "other", Status: model.StatusOpen, Labels: []string{"project:pet"},
		Fields: []byte(`{"owner":"oidc:other@example.com","projects":["pet"],"state":"online"}`)})
	seed(&model.Bead{ID: "gd-a1", Type: "agent", Title: "local-laptop", Assignee: "local-laptop", Status: model.StatusOpen, Labels: []string{"project:pet"},
		Fields: []byte(`{"runtime_id":"gd-r1"}`)})
	seed(&model.Bead{ID: "gd-a2", Type: "agent", Title: "crew-cloud", Assignee: "crew-cloud", Status: model.StatusOpen, Labels: []string{"project:pet"},
		Fields: []byte(`{}`)})
	seed(&model.Bead{ID: "gd-e1", Type: "escalation", Title: "esc", Status: model.StatusOpen, Labels: []string{"project:pet"},
		Fields: []byte(`{"operation_type":"x","operation_spec":{}}`)})
	seed(&model.Bead{ID: "gd-t1", Type: "task", Kind: model.KindIssue, Title: "allowed", Status: model.StatusOpen, Labels: []string{"project:pet"},
		CreatedBy: "owner@example.com"})
	seed(&model.Bead{ID: "gd-t2", Type: "task", Kind: model.KindIssue, Title: "hidden", Status: model.StatusOpen, Labels: []string{"project:fleet-internal"}})
	ms.beadsMu.Unlock()
	return token
}

func doRunnerBearer(h http.Handler, method, path, bearer, body string) *httptest.ResponseRecorder {
	if body == "" {
		body = `{}`
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const runnerRouteDeniedMsg = "runner tokens may not call this route"

// The interceptor is the gate, not the ingress: every route outside the
// allowlist refuses a runner, whatever role it asks for and whether or not it
// is wrapped in requireRole.
func TestRunnerRoleMatrix(t *testing.T) {
	_, h, token := newRunnerTestServer(t)
	denied := []struct{ method, path string }{
		{http.MethodPost, "/v1/escalations/gd-e1/approve"},
		{http.MethodPost, "/v1/escalations/gd-e1/reject"},
		{http.MethodDelete, "/v1/beads/gd-t1"},
		{http.MethodPost, "/v1/runners/gd-r1/revoke"},
		{http.MethodPost, "/v1/hooks/publish"},
		{http.MethodPost, "/v1/bridge-tokens"},     // requireRoleMachineAllowed
		{http.MethodPost, "/v1/auth/refresh"},      // no requireRole at all
		{http.MethodGet, "/v1/beads/gd-t1/labels"}, // a viewer read off the list
		{http.MethodPost, "/v1/runners/register"},  // pre-auth, but not with a bearer
		{http.MethodGet, "/v1/agents/roster"},      // nearby read, not allowlisted
		{http.MethodGet, "/v1/beads/gd-t1/mentions"},
		{http.MethodGet, "/v1/beads/gd-t1/mentioned-by"},
		{http.MethodDelete, "/v1/agents/gd-a1/gates/decision"},
		{http.MethodPost, "/v1/escalations/gd-e1/claim"}, // nearby write, not allowlisted
	}
	for _, c := range denied {
		rec := doRunnerBearer(h, c.method, c.path, token, "")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), runnerRouteDeniedMsg) {
			t.Errorf("%s %s: want 403 %q, got %d %s", c.method, c.path, runnerRouteDeniedMsg, rec.Code, rec.Body.String())
		}
	}
	allowed := []struct{ method, path string }{
		{http.MethodPost, "/v1/runners/gd-r1/heartbeat"},
		{http.MethodGet, "/v1/runners/gd-r1"},
		{http.MethodGet, "/v1/beads"},
		{http.MethodGet, "/v1/beads/gd-t1"},
		{http.MethodGet, "/v1/beads/gd-t1/comments"},
		{http.MethodGet, "/v1/beads/gd-t1/dependencies"},
		{http.MethodGet, "/v1/beads/gd-t1/dependents"},
		{http.MethodPost, "/v1/beads/gd-t1/comments"},
		{http.MethodPost, "/v1/hooks/emit"},
		{http.MethodPost, "/v1/escalations/gd-e1/result"},
		{http.MethodGet, "/v1/escalations"},
		{http.MethodGet, "/v1/batch/beads?ids=gd-t1"},
		{http.MethodPost, "/v1/beads/gd-t1/labels"},
		{http.MethodDelete, "/v1/beads/gd-t1/labels/x"},
		{http.MethodPost, "/v1/agents/gd-a1/gates/decision/satisfy"},
	}
	for _, c := range allowed {
		rec := doRunnerBearer(h, c.method, c.path, token, "")
		if rec.Code == http.StatusUnauthorized || strings.Contains(rec.Body.String(), runnerRouteDeniedMsg) {
			t.Errorf("%s %s: allowlisted route refused the runner: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	// Positive control: an OIDC caller is not gated as a runner.
	if rec := doRunnerBearer(h, http.MethodPost, "/v1/escalations/gd-e1/approve", defaultTestSubject, ""); strings.Contains(rec.Body.String(), runnerRouteDeniedMsg) {
		t.Fatalf("OIDC caller gated as a runner: %s", rec.Body.String())
	}
}

// Every pattern on the allowlist is a registered route, so a typo cannot
// silently drop a route the runner needs (or one renamed out from under it).
func TestRunnerAllowlist_PatternsAreRegistered(t *testing.T) {
	rec := &patternRecorder{}
	(&BeadsServer{}).registerRoutes(rec)
	for p := range runnerAllowedPatterns {
		if !rec.seen[p] {
			t.Errorf("runner allowlist names %q, which registerRoutes does not register", p)
		}
	}
}

type patternRecorder struct{ seen map[string]bool }

func (p *patternRecorder) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	if p.seen == nil {
		p.seen = map[string]bool{}
	}
	p.seen[pattern] = true
}

// requireRole holds the line on its own too: a runner context on a route off
// the allowlist is refused even below RoleAdmin, where a service token passes.
func TestRequireRole_RunnerAdmittedByRouteNotRank(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	for _, c := range []struct {
		pattern, role string
		want          int
	}{
		{"GET /v1/beads", RoleViewer, http.StatusNoContent},
		{"POST /v1/runners/{id}/heartbeat", RoleAdmin, http.StatusNoContent},
		{"GET /v1/agents/roster", RoleViewer, http.StatusForbidden},
		{"POST /v1/hooks/publish", RoleOperator, http.StatusForbidden},
	} {
		for name, wrap := range map[string]func(string, http.HandlerFunc) http.HandlerFunc{"requireRole": requireRole, "requireRoleMachineAllowed": requireRoleMachineAllowed} {
			if name == "requireRoleMachineAllowed" && c.want == http.StatusNoContent && c.role == RoleAdmin {
				continue // no allowlisted route is machine-allowed; nothing to assert
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Pattern = c.pattern
			req = req.WithContext(withAuthMethod(req.Context(), authMethodRunner))
			rec := httptest.NewRecorder()
			wrap(c.role, ok)(rec, req)
			if rec.Code != c.want {
				t.Errorf("%s(%s) on %q: got %d, want %d", name, c.role, c.pattern, rec.Code, c.want)
			}
		}
	}
}

func TestRunnerHeartbeat_SelfOnly(t *testing.T) {
	_, h, token := newRunnerTestServer(t)
	if rec := doRunnerBearer(h, http.MethodPost, "/v1/runners/gd-r1/heartbeat", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("own heartbeat: %d %s", rec.Code, rec.Body.String())
	}
	rec := doRunnerBearer(h, http.MethodPost, "/v1/runners/gd-r2/heartbeat", token, "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "only heartbeat itself") {
		t.Fatalf("other runner's heartbeat: want 403, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestRunnerAuth_RefusesUnknownRevokedAndForeignBearers(t *testing.T) {
	_, ms, h, bare := newRunnerTestServerBare(t)
	token := runnerTestToken(t, ms)

	if rec := doRunnerBearer(h, http.MethodGet, "/v1/beads", "not-a-runner-token", ""); rec.Code == http.StatusUnauthorized {
		t.Fatalf("a non-runner bearer on the private listener must reach normal auth, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRunnerBearer(h, http.MethodGet, "/v1/beads", runnerTokenPrefix+"unknown", ""); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), runnerTokenUnknownMsg) {
		t.Fatalf("unknown runner token: want 401, got %d %s", rec.Code, rec.Body.String())
	}

	onListener := func(bearer, path, method string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set(runnerListenerHeader, "runner")
		rec := httptest.NewRecorder()
		bare.ServeHTTP(rec, req)
		return rec
	}
	if rec := onListener("not-a-runner-token", "/v1/beads", http.MethodGet); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "runner tokens only") {
		t.Fatalf("a shared token via the public listener must be 401, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := onListener(token, "/v1/beads", http.MethodGet); rec.Code != http.StatusOK {
		t.Fatalf("a runner token via the public listener: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	// Register carries no bearer; the listener check must let it through to
	// the handler (which answers a missing invite code with 400).
	if rec := onListener("", "/v1/runners/register", http.MethodPost); rec.Code == http.StatusUnauthorized {
		t.Fatalf("register via the public listener must reach its handler, got %d %s", rec.Code, rec.Body.String())
	}

	// Runner bead revoked (state) while the token row is still live.
	ms.beadsMu.Lock()
	ms.beads["gd-r1"].Fields = []byte(`{"owner":"oidc:owner@example.com","projects":["pet"],"state":"revoked"}`)
	ms.beadsMu.Unlock()
	if rec := doRunnerBearer(h, http.MethodGet, "/v1/beads", token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("runner in state revoked: want 401, got %d", rec.Code)
	}
	ms.beadsMu.Lock()
	ms.beads["gd-r1"].Fields = []byte(`{"owner":"oidc:owner@example.com","projects":["pet"],"state":"online"}`)
	ms.beadsMu.Unlock()
	if rec := doRunnerBearer(h, http.MethodGet, "/v1/beads", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("restored runner: want 200, got %d %s", rec.Code, rec.Body.String())
	}

	now := time.Now()
	for _, rt := range ms.runnerTokens {
		rt.RevokedAt = &now
	}
	if rec := doRunnerBearer(h, http.MethodGet, "/v1/beads", token, ""); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), runnerTokenUnknownMsg) {
		t.Fatalf("revoked runner token: want 401, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestRunnerAuth_ProjectAllowlistOnReadsAndWrites(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)

	if rec := doRunnerBearer(h, http.MethodGet, "/v1/beads/gd-t2", token, ""); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Fatalf("bead outside the allowlist must be hidden, got %d", rec.Code)
	}
	if rec := doRunnerBearer(h, http.MethodGet, "/v1/beads/gd-t1", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("allowlisted bead must be readable, got %d %s", rec.Code, rec.Body.String())
	}
	rec := doRunnerBearer(h, http.MethodGet, "/v1/beads", token, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"gd-t2"`) || !strings.Contains(rec.Body.String(), `"gd-t1"`) {
		t.Fatalf("list must hold gd-t1 and not gd-t2: %d %s", rec.Code, rec.Body.String())
	}

	rec = doRunnerBearer(h, http.MethodPost, "/v1/beads", token, `{"title":"x","type":"task","kind":"issue","labels":["project:fleet-internal"]}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not allowlisted") {
		t.Fatalf("create outside the allowlist: want 403, got %d %s", rec.Code, rec.Body.String())
	}

	// Attribution: a runner's write is the owner's, and a client
	// X-Operated-By cannot re-attribute it.
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", strings.NewReader(`{"title":"mine","type":"task","kind":"issue","labels":["project:pet"]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Operated-By", "oidc:someone-else@example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create inside the allowlist: want 201, got %d %s", rec.Code, rec.Body.String())
	}
	var created model.Bead
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.OperatedBy != "oidc:owner@example.com" {
		t.Fatalf("operated_by: want the runner's owner, got %q", created.OperatedBy)
	}

	// Update may not move a bead out of the allowlist.
	ms.beadsMu.Lock()
	ms.beads["gd-t1"].CreatedBy = "oidc:owner@example.com"
	ms.beadsMu.Unlock()
	rec = doRunnerBearer(h, http.MethodPatch, "/v1/beads/gd-t1", token, `{"labels":["project:fleet-internal"]}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not allowlisted") {
		t.Fatalf("update into a foreign project: want 403, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestRunnerEventStream_MustNameItsOwnAgent(t *testing.T) {
	_, h, token := newRunnerTestServer(t)
	for _, c := range []struct {
		query string
		want  int
	}{
		{"", http.StatusBadRequest},
		{"?agent=gd-a2", http.StatusForbidden},   // agent on no runner
		{"?agent=gd-nope", http.StatusForbidden}, // missing bead: same answer, no oracle
		{"?agent=gd-t1", http.StatusForbidden},   // not an agent of this runner
	} {
		if rec := doRunnerBearer(h, http.MethodGet, "/v1/events/stream"+c.query, token, ""); rec.Code != c.want {
			t.Errorf("stream%s: want %d, got %d %s", c.query, c.want, rec.Code, rec.Body.String())
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/events/stream?agent=gd-a1", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream for its own agent: want 200, got %d %s", rec.Code, rec.Body.String())
	}
}

// A runner never exceeds its owner: the static token's machineAllowedWrites
// pairs (create role/config/user, update config/user/project) are not a
// runner's, so these admin-gated writes deny it like a roleless OIDC caller.
func TestRunnerAuth_NoMachineAllowedWrites(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)
	ms.beadsMu.Lock()
	for _, b := range []*model.Bead{
		{ID: "gd-c1", Type: "config", Kind: model.KindConfig, Title: "some-config", Status: model.StatusOpen, Fields: []byte(`{"value":"a"}`)},
		{ID: "gd-u1", Type: "user", Kind: model.KindConfig, Title: "someone@example.com", Status: model.StatusOpen, Fields: []byte(`{}`)},
		{ID: "gd-p1", Type: "project", Kind: model.KindConfig, Title: "pet", Status: model.StatusOpen, Fields: []byte(`{}`)},
	} {
		ms.beads[b.ID] = b
		// In the runner's project, so the write policy is what refuses: a
		// bead it cannot read answers 404 first (gd-7APTAIhdC5).
		ms.labels[b.ID] = []string{"project:pet"}
	}
	ms.beadsMu.Unlock()

	for _, c := range []struct{ name, method, path, body string }{
		{"create config", http.MethodPost, "/v1/beads", `{"title":"fleet-setting","type":"config","kind":"config","fields":{"value":"x"}}`},
		{"create role", http.MethodPost, "/v1/beads", `{"title":"evil-role","type":"role","kind":"config"}`},
		{"create user", http.MethodPost, "/v1/beads", `{"title":"new@example.com","type":"user","kind":"config"}`},
		{"update config", http.MethodPatch, "/v1/beads/gd-c1", `{"fields":{"value":"b"}}`},
		{"update user", http.MethodPatch, "/v1/beads/gd-u1", `{"description":"x"}`},
		{"update project", http.MethodPatch, "/v1/beads/gd-p1", `{"description":"x"}`},
	} {
		rec := doRunnerBearer(h, c.method, c.path, token, c.body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "insufficient permissions") {
			t.Errorf("%s: want 403 insufficient permissions, got %d %s", c.name, rec.Code, rec.Body.String())
		}
	}

	// Control: the static token keeps its allow-listed pairs.
	s, _, _ := testCtx(t)
	runner := WithRunnerProjects(withAuthMethod(context.Background(), authMethodRunner), []string{"pet"})
	for _, c := range []struct{ action, typ string }{
		{"create", "config"}, {"update", "config"}, {"create", "role"}, {"create", "user"}, {"update", "user"}, {"update", "project"},
	} {
		if d := s.authorizeWrite(staticCtx(), c.action, model.BeadType(c.typ), nil); d != nil {
			t.Errorf("static %s/%s must stay allowed, got %+v", c.action, c.typ, d)
		}
		if d := s.authorizeWrite(runner, c.action, model.BeadType(c.typ), nil); d == nil {
			t.Errorf("runner %s/%s must be denied", c.action, c.typ)
		}
	}
}

// A project label inside the allowlist does not cover a fields.project outside
// it: the controller spawns an agent into fields.project.
func TestRunnerAuth_FieldsProjectMustBeAllowlisted(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)

	rec := doRunnerBearer(h, http.MethodPost, "/v1/beads", token,
		`{"title":"spy","type":"agent","kind":"config","labels":["project:pet"],"fields":{"agent":"spy","project":"fleet-internal","role":"crew"}}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not allowlisted") {
		t.Fatalf("create with label pet, fields.project fleet-internal: want 403, got %d %s", rec.Code, rec.Body.String())
	}

	rec = doRunnerBearer(h, http.MethodPatch, "/v1/beads/gd-t1", token, `{"labels":["project:pet"],"fields":{"project":"fleet-internal"}}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not allowlisted") {
		t.Fatalf("update fields.project into fleet-internal: want 403, got %d %s", rec.Code, rec.Body.String())
	}

	// A bead already labelled pet but with fields.project outside the
	// allowlist is not the runner's to write.
	ms.beadsMu.Lock()
	ms.beads["gd-t3"] = &model.Bead{ID: "gd-t3", Type: "task", Kind: model.KindIssue, Title: "mislabelled", Status: model.StatusOpen,
		Labels: []string{"project:pet"}, CreatedBy: "oidc:owner@example.com", Fields: []byte(`{"project":"fleet-internal"}`)}
	ms.labels["gd-t3"] = []string{"project:pet"}
	ms.beadsMu.Unlock()
	rec = doRunnerBearer(h, http.MethodPatch, "/v1/beads/gd-t3", token, `{"description":"x"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not allowlisted") {
		t.Fatalf("update a bead whose fields.project is foreign: want 403, got %d %s", rec.Code, rec.Body.String())
	}

	// Close and comment check fields.project too, not only the labels: on a
	// bead labelled for the runner's project but with a foreign
	// fields.project, both refuse. (An unlabelled one it cannot read at all,
	// so it answers 404 before this check, gd-7APTAIhdC5.)
	ms.beadsMu.Lock()
	ms.beads["gd-t4"] = &model.Bead{ID: "gd-t4", Type: "task", Kind: model.KindIssue, Title: "foreign fields.project", Status: model.StatusOpen,
		CreatedBy: "oidc:owner@example.com", Fields: []byte(`{"project":"fleet-internal"}`)}
	ms.labels["gd-t4"] = []string{"project:pet"}
	ms.beadsMu.Unlock()
	for _, c := range []struct{ name, path, body string }{
		{"close", "/v1/beads/gd-t4/close", `{}`},
		{"comment", "/v1/beads/gd-t4/comments", `{"text":"hi"}`},
	} {
		rec = doRunnerBearer(h, http.MethodPost, c.path, token, c.body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not allowlisted") {
			t.Errorf("%s on a bead whose fields.project is foreign: want 403, got %d %s", c.name, rec.Code, rec.Body.String())
		}
	}

	// Control: the same agent create inside the allowlist is not refused for
	// its project.
	rec = doRunnerBearer(h, http.MethodPost, "/v1/beads", token,
		`{"title":"mine","type":"agent","kind":"config","labels":["project:pet"],"fields":{"agent":"mine","project":"pet","role":"crew"}}`)
	if strings.Contains(rec.Body.String(), "not allowlisted") {
		t.Fatalf("agent create inside the allowlist refused for its project: %d %s", rec.Code, rec.Body.String())
	}
}

// runtime_id is what the stream gate trusts, so a runner may not stamp its own
// id onto an agent it does not run (agent beads are KindConfig: ownership does
// not guard them), hand its agent to another runner, or create an agent on
// another runner. The two-step bypass (PATCH runtime_id, then stream) stays
// refused at both steps.
func TestRunnerAuth_RuntimeIDOnlyOnOwnAgents(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)
	// Real agent beads are KindConfig with agent/role/project set, so neither
	// ownership nor field validation stands between the runner and the write.
	ms.beadsMu.Lock()
	ms.beads["gd-a1"].Kind = model.KindConfig
	ms.beads["gd-a1"].Fields = []byte(`{"agent":"local-laptop","role":"local","project":"pet","runtime_id":"gd-r1"}`)
	ms.beads["gd-a2"].Kind = model.KindConfig
	ms.beads["gd-a2"].Fields = []byte(`{"agent":"crew-cloud","role":"crew","project":"pet"}`)
	ms.beads["gd-a3"] = &model.Bead{ID: "gd-a3", Type: "agent", Kind: model.KindConfig, Title: "other-local", Assignee: "other-local", Status: model.StatusOpen,
		Labels: []string{"project:pet"}, Fields: []byte(`{"agent":"other-local","role":"local","project":"pet","runtime_id":"gd-r2"}`)}
	ms.labels["gd-a3"] = []string{"project:pet"}
	ms.beadsMu.Unlock()
	const refusedMsg = "runtime_id only on its own agents"

	for _, c := range []struct{ name, method, path, body string }{
		{"claim a cloud agent", http.MethodPatch, "/v1/beads/gd-a2", `{"fields":{"runtime_id":"gd-r1"}}`},
		{"claim by case-variant key", http.MethodPatch, "/v1/beads/gd-a2", `{"fields":{"Runtime_ID":"gd-r1"}}`},
		{"take another runner's agent", http.MethodPatch, "/v1/beads/gd-a3", `{"fields":{"runtime_id":"gd-r1"}}`},
		{"clear another runner's agent", http.MethodPatch, "/v1/beads/gd-a3", `{"fields":{"runtime_id":null}}`},
		{"hand its agent to another runner", http.MethodPatch, "/v1/beads/gd-a1", `{"fields":{"runtime_id":"gd-r2"}}`},
		{"create on another runner", http.MethodPost, "/v1/beads",
			`{"title":"spy","type":"agent","kind":"config","labels":["project:pet"],"fields":{"agent":"spy","project":"pet","role":"local","runtime_id":"gd-r2"}}`},
		// The close body is merged into fields too, so it is the same write.
		{"claim a cloud agent on close", http.MethodPost, "/v1/beads/gd-a2/close", `{"runtime_id":"gd-r1"}`},
		{"claim by case-variant key on close", http.MethodPost, "/v1/beads/gd-a2/close", `{"RUNTIME_ID":"gd-r1"}`},
		{"take another runner's agent on close", http.MethodPost, "/v1/beads/gd-a3/close", `{"runtime_id":"gd-r1"}`},
		{"clear another runner's agent on close", http.MethodPost, "/v1/beads/gd-a3/close", `{"runtime_id":""}`},
	} {
		rec := doRunnerBearer(h, c.method, c.path, token, c.body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), refusedMsg) {
			t.Errorf("%s: want 403 %q, got %d %s", c.name, refusedMsg, rec.Code, rec.Body.String())
		}
	}

	ms.beadsMu.Lock()
	a1, a2, a3 := string(ms.beads["gd-a1"].Fields), string(ms.beads["gd-a2"].Fields), string(ms.beads["gd-a3"].Fields)
	s2, s3 := ms.beads["gd-a2"].Status, ms.beads["gd-a3"].Status
	ms.beadsMu.Unlock()
	if strings.Contains(strings.ToLower(a2), "runtime_id") || !strings.Contains(a3, `"runtime_id":"gd-r2"`) || !strings.Contains(a1, `"runtime_id":"gd-r1"`) {
		t.Fatalf("refused writes must not land: gd-a1=%s gd-a2=%s gd-a3=%s", a1, a2, a3)
	}
	if s2 != model.StatusOpen || s3 != model.StatusOpen {
		t.Fatalf("a refused close body must not close the bead: gd-a2=%s gd-a3=%s", s2, s3)
	}
	for _, agent := range []string{"gd-a2", "gd-a3"} {
		if rec := doRunnerBearer(h, http.MethodGet, "/v1/events/stream?agent="+agent, token, ""); rec.Code != http.StatusForbidden {
			t.Errorf("stream %s after the refused PATCH and close: want 403, got %d %s", agent, rec.Code, rec.Body.String())
		}
	}

	// Control: its own id on its own agent, and a create on itself, pass this gate.
	if rec := doRunnerBearer(h, http.MethodPatch, "/v1/beads/gd-a1", token, `{"fields":{"runtime_id":"gd-r1"}}`); rec.Code != http.StatusOK {
		t.Errorf("own id on own agent: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	rec := doRunnerBearer(h, http.MethodPost, "/v1/beads", token,
		`{"title":"mine","type":"agent","kind":"config","labels":["project:pet"],"fields":{"agent":"mine","project":"pet","role":"local","runtime_id":"gd-r1"}}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("create on itself: want 201, got %d %s", rec.Code, rec.Body.String())
	}
}

// mergeBeadFields gates a runner's runtime_id write itself, so a handler that
// merges caller keys without its own check is still no way to claim an agent.
func TestRunnerAuth_MergeBeadFieldsGatesRuntimeID(t *testing.T) {
	s, ms, _, _ := newRunnerTestServerBare(t)
	runnerTestToken(t, ms)
	ctx := WithRunnerID(withAuthMethod(context.Background(), authMethodRunner), "gd-r1")
	for _, extra := range []map[string]any{{"runtime_id": "gd-r1"}, {"Runtime_Id": "gd-r1"}, {"runtime_id": nil}} {
		var denial *AuthzDenial
		if err := s.mergeBeadFields(ctx, "gd-a2", extra); !errors.As(err, &denial) {
			t.Errorf("runner merge %v onto another's agent: want an authz denial, got %v", extra, err)
		}
	}
	ms.beadsMu.Lock()
	a2 := string(ms.beads["gd-a2"].Fields)
	ms.beadsMu.Unlock()
	if strings.Contains(strings.ToLower(a2), "runtime_id") {
		t.Fatalf("refused merge landed: gd-a2=%s", a2)
	}
	// Its own agent, its own id, and a non-runner caller pass.
	if err := s.mergeBeadFields(ctx, "gd-a1", map[string]any{"runtime_id": "gd-r1"}); err != nil {
		t.Errorf("own id on own agent: %v", err)
	}
	if err := s.mergeBeadFields(context.Background(), "gd-a2", map[string]any{"runtime_id": "vm-1"}); err != nil {
		t.Errorf("non-runner merge: %v", err)
	}
}

// The stream gate reads runtime_id by its exact key: a case-variant key that a
// struct decode would fold onto it does not admit a runner.
func TestRunnerEventStream_IgnoresCaseVariantRuntimeID(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)
	ms.beadsMu.Lock()
	ms.beads["gd-a2"].Fields = []byte(`{"Runtime_ID":"gd-r1"}`)
	ms.beadsMu.Unlock()
	if rec := doRunnerBearer(h, http.MethodGet, "/v1/events/stream?agent=gd-a2", token, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("case-variant runtime_id: want 403, got %d %s", rec.Code, rec.Body.String())
	}
}
