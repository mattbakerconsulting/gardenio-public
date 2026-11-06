package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// gd-Du6wHclx4g: decision resolve/cancel must refuse an agent credential even
// when it relays a human in X-Operated-By.

func decisionResolverCases() []resolutionCase {
	var out []resolutionCase
	for _, tc := range resolutionCases() {
		if tc.name == "resolve" || tc.name == "cancel" {
			out = append(out, tc)
		}
	}
	return out
}

func identityMutator(id AuthIdentity, fleet string) func(*http.Request) {
	return func(r *http.Request) {
		ctx := WithAuthIdentity(r.Context(), id)
		if fleet != "" {
			ctx = WithFleet(ctx, fleet)
		}
		*r = *r.WithContext(ctx)
		r.Header.Set("X-Operated-By", "oidc:owner@example.com")
	}
}

func TestDecisionResolve_DeniesAgentCredentials(t *testing.T) {
	denied := map[string]func(*http.Request){
		"static token":         identityMutator(AuthIdentity{Type: "static", Subject: "static"}, ""),
		"static token + actor": identityMutator(AuthIdentity{Type: "static", Subject: "some-agent"}, ""),
		"per-agent fleet":      identityMutator(AuthIdentity{Type: "fleet", Subject: "some-agent"}, "agent-gd-abc123"),
		// A per-agent token never counts as a bridge, whatever subject it carries.
		"per-agent fleet with bridge subject": identityMutator(AuthIdentity{Type: "fleet", Subject: "bridge:slack"}, "agent-gd-abc123"),
		"non-bridge fleet":                    identityMutator(AuthIdentity{Type: "fleet", Subject: "ci-runner"}, "ci"),
	}
	for name, mutate := range denied {
		for _, tc := range decisionResolverCases() {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				ms, h := resolutionAuthzFixture(t, "gardenio")
				rec := postJSON(h, tc.path, tc.body, mutate)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("expected 403, got %d; body: %s", rec.Code, rec.Body.String())
				}
				assertResolutionTargetsUntouched(t, ms)
			})
		}
	}
}

func TestDecisionResolve_AllowsHumansAndBridges(t *testing.T) {
	allowed := map[string]func(*http.Request){
		"oidc":    identityMutator(AuthIdentity{Type: "oidc", Subject: "owner@example.com"}, ""),
		"edge":    identityMutator(AuthIdentity{Type: "edge", Subject: "oidc:owner@example.com"}, ""),
		"machine": identityMutator(AuthIdentity{Type: "machine", Subject: "machine:gardenio-beads-viewer"}, ""),
		"bridge":  identityMutator(AuthIdentity{Type: "fleet", Subject: "bridge:slack"}, "slack-bridge"),
		"no-auth": nil,
	}
	for name, mutate := range allowed {
		for _, tc := range decisionResolverCases() {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				_, h := resolutionAuthzFixture(t, "gardenio")
				rec := postJSON(h, tc.path, tc.body, mutate)
				if rec.Code != http.StatusOK {
					t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

// The relay header is attribution only: an agent credential must not be able to
// rebind itself to the relayed human.
func TestDecisionResolve_AgentRelayDoesNotRebind(t *testing.T) {
	ms, h := resolutionAuthzFixture(t, "gardenio")
	rec := postJSON(h, "/v1/decisions/gd-dec-target/resolve", map[string]any{"selected_option": "y"},
		identityMutator(AuthIdentity{Type: "fleet", Subject: "some-agent"}, "agent-gd-abc123"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if dec := ms.beads["gd-dec-target"]; dec.Status != model.StatusOpen {
		t.Fatalf("decision was closed: %s", dec.Status)
	}
}

// Generic close carve-out (write_authz.go): an agent fleet token no longer gets
// the decision shared-queue ownership relaxation; a bridge still does.
func TestAuthorizeOwnership_DecisionCarveOutExcludesAgentFleet(t *testing.T) {
	srv, _, _ := testCtx(t)
	b := decBead("gd-dec-agent", "other-agent", "oidc:owner@example.com")

	agent := WithFleet(withAuthMethod(t.Context(), authMethodStatic), "agent-gd-abc123")
	agent = WithAuthIdentity(agent, AuthIdentity{Type: "fleet", Subject: "some-agent"})
	if d := srv.authorizeOwnership(agent, "close", b); d == nil {
		t.Fatal("agent fleet token must not close a decision it did not file")
	}

	bridge := WithFleet(withAuthMethod(t.Context(), authMethodStatic), "slack-bridge")
	bridge = WithAuthIdentity(bridge, AuthIdentity{Type: "fleet", Subject: "bridge:slack"})
	if d := srv.authorizeOwnership(bridge, "close", b); d != nil {
		t.Fatalf("bridge token should still close a shared decision, got %v", d)
	}
}

// The primary static token is held by every agent pod, so authorizeOwnership's
// static bypass must not reach a decision the caller did not file: otherwise
// `kd update <decision> --field chosen=... responded_by=oidc:<owner>` then
// `kd close` forges an operator's answer through the generic path.
func TestAuthorizeOwnership_StaticTokenCannotMutateForeignDecision(t *testing.T) {
	srv, _, _ := testCtx(t)
	b := decBead("gd-dec-static", "other-agent", "oidc:owner@example.com")

	// Bare static token (no X-Actor) and a static token acting as another agent.
	for name, subject := range map[string]string{"bare static": "static", "other agent via X-Actor": "some-agent"} {
		ctx := WithAuthIdentity(withAuthMethod(t.Context(), authMethodStatic), AuthIdentity{Type: "static", Subject: subject})
		for _, action := range []string{"update", "close"} {
			if d := srv.authorizeOwnership(ctx, action, b); d == nil {
				t.Fatalf("%s: static token must not %s a decision it did not file", name, action)
			}
		}
	}

	// The filer keeps ownership of its own decision (a bare withdraw close; the
	// answer fields are gated separately, see TestDecisionFiler_*).
	own := WithAuthIdentity(withAuthMethod(t.Context(), authMethodStatic), AuthIdentity{Type: "static", Subject: "other-agent"})
	if d := srv.authorizeOwnership(own, "close", b); d != nil {
		t.Fatalf("filer should close its own decision, got %v", d)
	}

	// There is no blanket bypass for other bead types either: the shared token
	// is retired (gd-ke6PC7bRto), so nothing is exempt from ownership here.
	task := &model.Bead{ID: "gd-task", Type: model.TypeTask, Kind: model.KindIssue, CreatedBy: "other-agent"}
	bare := WithAuthIdentity(withAuthMethod(t.Context(), authMethodStatic), AuthIdentity{Type: "static", Subject: "static"})
	if d := srv.authorizeOwnership(bare, "close", task); d == nil {
		t.Fatal("a legacy static identity must be ownership-gated on tasks too")
	}
}

// A decision's filer owns the bead but may not ANSWER it (requester-is-not-
// resolver): through the generic update/close path a static or per-agent fleet
// caller must not put chosen / responded_by / ... on a decision, even one it
// filed. Only a bare close (withdraw) is allowed.
func decisionFilerFixture(t *testing.T) (*mockStore, http.Handler, *BeadsServer) {
	t.Helper()
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /v1/beads/{id}", requireRole(RoleOperator, s.handleUpdateBead))
	mux.HandleFunc("POST /v1/beads/{id}/close", requireRole(RoleOperator, s.handleCloseBead))
	mux.HandleFunc("POST /v1/beads/{id}/fields/{field}/append", requireRole(RoleOperator, s.handleAppendBeadArrayField))
	mux.HandleFunc("POST /v1/beads/{id}/increment", requireRole(RoleOperator, s.handleIncrementBeadFields))
	ms.beads["gd-dec-own"] = &model.Bead{
		ID: "gd-dec-own", Kind: model.KindData, Type: model.TypeDecision,
		Title: "Deploy?", Status: model.StatusOpen, CreatedBy: "filer-agent",
		Fields: json.RawMessage(`{"prompt":"Deploy?"}`),
	}
	return ms, mux, s
}

func filerMutators() map[string]func(*http.Request) {
	static := func(r *http.Request) {
		ctx := withAuthMethod(r.Context(), authMethodStatic)
		ctx = withTestAgent(ctx, "filer-agent")
		*r = *r.WithContext(ctx)
		r.Header.Set("X-Operated-By", "oidc:owner@example.com")
	}
	fleet := func(r *http.Request) {
		ctx := withAuthMethod(r.Context(), authMethodStatic)
		ctx = WithFleet(ctx, "agent-gd-abc123")
		ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: "filer-agent"})
		*r = *r.WithContext(ctx)
		r.Header.Set("X-Operated-By", "oidc:owner@example.com")
	}
	return map[string]func(*http.Request){"filer static token": static, "filer per-agent fleet token": fleet}
}

func doFilerJSON(h http.Handler, method, path string, body map[string]any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	mutate(req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDecisionFiler_CannotAnswerOwnDecisionGenerically(t *testing.T) {
	answer := map[string]any{"chosen": "merge", "responded_by": "oidc:owner@example.com"}
	for name, mutate := range filerMutators() {
		t.Run(name+"/PATCH fields", func(t *testing.T) {
			ms, h, _ := decisionFilerFixture(t)
			rec := doFilerJSON(h, "PATCH", "/v1/beads/gd-dec-own", map[string]any{"fields": answer}, mutate)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
			assertDecisionUnanswered(t, ms)
		})
		for _, key := range []string{"chosen", "responded_by", "responded_at", "response_text", "rationale"} {
			t.Run(name+"/close body "+key, func(t *testing.T) {
				ms, h, _ := decisionFilerFixture(t)
				rec := doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/close", map[string]any{key: "x"}, mutate)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
				}
				assertDecisionUnanswered(t, ms)
			})
		}
		t.Run(name+"/close body chosen+responded_by", func(t *testing.T) {
			ms, h, _ := decisionFilerFixture(t)
			rec := doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/close", answer, mutate)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
			assertDecisionUnanswered(t, ms)
		})
		t.Run(name+"/field endpoint", func(t *testing.T) {
			ms, h, _ := decisionFilerFixture(t)
			rec := doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/increment", map[string]any{"rationale": 1}, mutate)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
			assertDecisionUnanswered(t, ms)
		})
		t.Run(name+"/bare close withdraws", func(t *testing.T) {
			ms, h, _ := decisionFilerFixture(t)
			rec := doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/close", map[string]any{}, mutate)
			if rec.Code != http.StatusOK {
				t.Fatalf("filer bare close should withdraw, got %d: %s", rec.Code, rec.Body.String())
			}
			if ms.beads["gd-dec-own"].Status != model.StatusClosed {
				t.Fatalf("decision should be closed")
			}
			assertNoAnswerFields(t, ms)
		})
		t.Run(name+"/non-answer update allowed", func(t *testing.T) {
			_, h, _ := decisionFilerFixture(t)
			rec := doFilerJSON(h, "PATCH", "/v1/beads/gd-dec-own", map[string]any{"fields": map[string]any{"reflag_count": "1"}}, mutate)
			if rec.Code == http.StatusForbidden {
				t.Fatalf("non-answer field update on own decision should not 403: %s", rec.Body.String())
			}
		})
	}
}

func TestDecisionAnswerFields_HumansAndBridgesAllowed(t *testing.T) {
	srv, _, _ := testCtx(t)
	b := decBead("gd-dec-h", "other-agent", "")
	for name, id := range map[string]AuthIdentity{
		"oidc":    {Type: "oidc", Subject: "owner@example.com"},
		"edge":    {Type: "edge", Subject: "oidc:owner@example.com"},
		"machine": {Type: "machine", Subject: "svc"},
	} {
		ctx := WithAuthIdentity(t.Context(), id)
		if d := srv.authorizeDecisionAnswerFields(ctx, "update", b, "chosen", "responded_by"); d != nil {
			t.Errorf("%s should be allowed to write answer fields, got %v", name, d)
		}
	}
	bridge := WithAuthIdentity(WithFleet(t.Context(), "slack-bridge"), AuthIdentity{Type: "fleet", Subject: "bridge:slack"})
	if d := srv.authorizeDecisionAnswerFields(bridge, "update", b, "chosen"); d != nil {
		t.Errorf("bridge should be allowed, got %v", d)
	}
	// Other bead types are untouched.
	task := &model.Bead{ID: "gd-t", Type: model.TypeTask}
	agent := withTestAgent(t.Context(), "a")
	if d := srv.authorizeDecisionAnswerFields(agent, "update", task, "chosen"); d != nil {
		t.Errorf("non-decision bead must be untouched, got %v", d)
	}
}

func assertDecisionUnanswered(t *testing.T, ms *mockStore) {
	t.Helper()
	if ms.beads["gd-dec-own"].Status == model.StatusClosed {
		t.Fatalf("decision must stay open")
	}
	assertNoAnswerFields(t, ms)
}

func assertNoAnswerFields(t *testing.T, ms *mockStore) {
	t.Helper()
	var f map[string]any
	_ = json.Unmarshal(ms.beads["gd-dec-own"].Fields, &f)
	for k := range decisionAnswerFields {
		if _, ok := f[k]; ok {
			t.Fatalf("answer field %q written: %v", k, f)
		}
	}
}

// The deployed Slack bridge authenticates with the bridge-tokens fleet-map entry
// "<token>:slack-bridge" (no Subject), so its identity is fleet "slack-bridge" /
// subject "slack-bridge", not "bridge:slack". Drive the real AuthMiddleware so
// the fallback-to-fleet-name subject is what the gate sees.
func TestDecisionResolve_DeployedSlackBridgeThroughAuthMiddleware(t *testing.T) {
	const (
		bridgeTok = "tok-slack-bridge"
		agentTok  = "tok-agent"
		forgedTok = "tok-forged"
		staticTok = "tok-static"
	)
	newHandler := func(t *testing.T) (*mockStore, http.Handler) {
		t.Helper()
		ms, mux := resolutionAuthzFixture(t, "gardenio")
		fc := NewFleetConfig("default", ParseFleetMap(bridgeTok+":slack-bridge,"+agentTok+":agent-gd-abc123,"+forgedTok+":agent-gd-forged-bridge"))
		return ms, AuthMiddleware(withTestToken(fc, staticTok), nil, mux)
	}
	call := func(h http.Handler, method, path, tok string, body map[string]any) *httptest.ResponseRecorder {
		return doFilerJSON(h, method, path, body, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+tok)
			r.Header.Set("X-Operated-By", "oidc:owner@example.com")
		})
	}

	for _, tc := range decisionResolverCases() {
		t.Run("slack-bridge fleet/"+tc.name, func(t *testing.T) {
			ms, h := newHandler(t)
			rec := call(h, "POST", tc.path, bridgeTok, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("deployed bridge must resolve decisions, got %d: %s", rec.Code, rec.Body.String())
			}
			if ms.beads["gd-dec-target"].Status != model.StatusClosed {
				t.Fatalf("decision should be closed")
			}
		})
		for name, tok := range map[string]string{"agent fleet": agentTok, "agent fleet named *-bridge": forgedTok, "static": staticTok} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				ms, h := newHandler(t)
				rec := call(h, "POST", tc.path, tok, tc.body)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
				}
				assertResolutionTargetsUntouched(t, ms)
			})
		}
	}
}

// The bridge also writes `rationale` (thread reply) and `chosen` through the
// generic update path; the deployed bridge identity must pass the field gate.
func TestDecisionAnswerFields_DeployedSlackBridgeThroughAuthMiddleware(t *testing.T) {
	const bridgeTok, agentTok = "tok-slack-bridge", "tok-agent"
	for name, tc := range map[string]struct {
		tok  string
		want int
	}{"slack-bridge": {bridgeTok, http.StatusOK}, "agent fleet": {agentTok, http.StatusForbidden}} {
		t.Run(name, func(t *testing.T) {
			ms, mux, _ := decisionFilerFixture(t)
			fc := NewFleetConfig("default", ParseFleetMap(bridgeTok+":slack-bridge,"+agentTok+":agent-gd-abc123"))
			h := AuthMiddleware(withTestToken(fc, "tok-static"), nil, mux)
			rec := doFilerJSON(h, "PATCH", "/v1/beads/gd-dec-own", map[string]any{"fields": map[string]any{"chosen": "merge", "rationale": "ok"}}, func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+tc.tok)
			})
			if rec.Code == http.StatusForbidden && tc.want == http.StatusOK {
				t.Fatalf("deployed bridge must be allowed to answer, got 403: %s", rec.Body.String())
			}
			if tc.want == http.StatusForbidden {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
				}
				assertNoAnswerFields(t, ms)
			}
		})
	}
}
