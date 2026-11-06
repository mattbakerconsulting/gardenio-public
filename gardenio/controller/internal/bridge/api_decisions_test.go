package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"log/slog"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// mockDecisionClient implements DecisionAPIClient for testing.
type mockDecisionClient struct {
	decisions    []beadsapi.DecisionDetail
	beads        map[string]*beadsapi.BeadDetail
	resolveID    string
	respondedBy  string
	cancelID     string
	canceledBy   string
	createdID    string
	closedID     string
	closedFields map[string]string
	depAdded     []string // "from:to:type"
	depBy        string
	err          error
}

func (m *mockDecisionClient) ListDecisions(_ context.Context, status string, limit int) ([]beadsapi.DecisionDetail, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.decisions, nil
}

func (m *mockDecisionClient) GetDecision(_ context.Context, id string) (*beadsapi.DecisionDetail, error) {
	if m.err != nil {
		return nil, m.err
	}
	for _, d := range m.decisions {
		if d.Decision != nil && d.Decision.ID == id {
			return &d, nil
		}
	}
	return nil, &beadsapi.APIError{StatusCode: 404, Message: "not found"}
}

func (m *mockDecisionClient) ResolveDecision(_ context.Context, id string, req beadsapi.ResolveDecisionRequest) error {
	m.resolveID = id
	m.respondedBy = req.RespondedBy
	return m.err
}

func (m *mockDecisionClient) CancelDecision(_ context.Context, id, reason, canceledBy string) error {
	m.cancelID = id
	m.canceledBy = canceledBy
	return m.err
}

func (m *mockDecisionClient) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if m.beads != nil {
		if b, ok := m.beads[id]; ok {
			return b, nil
		}
	}
	return nil, &beadsapi.APIError{StatusCode: 404, Message: "not found"}
}

func (m *mockDecisionClient) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	m.createdID = "gd-esc-new"
	return m.createdID, nil
}

func (m *mockDecisionClient) AddDependency(_ context.Context, beadID, dependsOnID, depType, createdBy string) error {
	m.depAdded = append(m.depAdded, beadID+":"+dependsOnID+":"+depType)
	m.depBy = createdBy
	return nil
}

func (m *mockDecisionClient) CloseBead(_ context.Context, id string, fields map[string]string) error {
	m.closedID = id
	m.closedFields = fields
	return nil
}

// signedIn marks req as the OIDC middleware leaves a verified caller: the
// email in the context and the bearer it verified on the request.
func signedIn(req *http.Request, email string) *http.Request {
	req.Header.Set("Authorization", "Bearer jwt-"+email)
	return req.WithContext(oidcauth.ContextWithEmail(req.Context(), email))
}

func TestDecisionAPI_List(t *testing.T) {
	client := &mockDecisionClient{
		decisions: []beadsapi.DecisionDetail{
			{Decision: &beadsapi.BeadDetail{
				ID:       "gd-test1",
				Title:    "Test decision",
				Type:     "decision",
				Status:   "open",
				Priority: 2,
				Fields:   map[string]string{"prompt": "Which approach?"},
			}},
		},
	}
	api := NewDecisionAPI(client, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/decisions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}

	var resp struct {
		Decisions []beadsapi.DecisionDetail `json:"decisions"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Decisions) != 1 {
		t.Fatalf("got %d decisions, want 1", len(resp.Decisions))
	}
	if resp.Decisions[0].Decision.ID != "gd-test1" {
		t.Errorf("got id=%q, want gd-test1", resp.Decisions[0].Decision.ID)
	}
}

func TestDecisionAPI_Show(t *testing.T) {
	client := &mockDecisionClient{
		decisions: []beadsapi.DecisionDetail{
			{Decision: &beadsapi.BeadDetail{
				ID:     "gd-show1",
				Title:  "Show me",
				Type:   "decision",
				Status: "open",
			}},
		},
	}
	api := NewDecisionAPI(client, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/decisions/gd-show1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}

	var resp beadsapi.DecisionDetail
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Decision == nil || resp.Decision.ID != "gd-show1" {
		t.Errorf("got unexpected decision detail")
	}
}

func TestDecisionAPI_Resolve(t *testing.T) {
	client := &mockDecisionClient{}
	api := NewDecisionAPI(client, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	body := `{"chosen":"option-a","rationale":"because","respondedBy":"mallory"}`
	req := httptest.NewRequest(http.MethodPost, "/api/decisions/gd-res1/resolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedIn(req, "alice@example.com"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if client.resolveID != "gd-res1" {
		t.Errorf("resolveID=%q, want gd-res1", client.resolveID)
	}
	if client.respondedBy != "oidc:alice@example.com" {
		t.Errorf("respondedBy=%q, want the verified caller, not the body name", client.respondedBy)
	}
}

func TestDecisionAPI_Resolve_MissingChosen(t *testing.T) {
	client := &mockDecisionClient{}
	api := NewDecisionAPI(client, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	body := `{"rationale":"no choice"}`
	req := httptest.NewRequest(http.MethodPost, "/api/decisions/gd-x/resolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
}

func TestDecisionAPI_Dismiss(t *testing.T) {
	client := &mockDecisionClient{}
	api := NewDecisionAPI(client, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	body := `{"reason":"not needed","canceledBy":"mallory"}`
	req := httptest.NewRequest(http.MethodPost, "/api/decisions/gd-dis1/dismiss", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedIn(req, "alice@example.com"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if client.cancelID != "gd-dis1" {
		t.Errorf("cancelID=%q, want gd-dis1", client.cancelID)
	}
	if client.canceledBy != "oidc:alice@example.com" {
		t.Errorf("canceledBy=%q, want the verified caller, not the body name", client.canceledBy)
	}
}

// A write with no verified identity and bearer is refused before the daemon
// is called; a forged X-Auth-Request-Email header is not an identity.
func TestDecisionAPI_WritesRequireVerifiedCaller(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"resolve", "/api/decisions/gd-x/resolve", `{"chosen":"a"}`},
		{"dismiss", "/api/decisions/gd-x/dismiss", `{}`},
		{"promote", "/api/decisions/gd-x/promote", `{}`},
	} {
		for _, auth := range []struct {
			name string
			set  func(*http.Request) *http.Request
		}{
			{"forged email header", func(r *http.Request) *http.Request {
				r.Header.Set("X-Auth-Request-Email", "alice@example.com")
				return r
			}},
			{"email without bearer", func(r *http.Request) *http.Request {
				return r.WithContext(oidcauth.ContextWithEmail(r.Context(), "alice@example.com"))
			}},
		} {
			t.Run(tc.name+"/"+auth.name, func(t *testing.T) {
				client := &mockDecisionClient{}
				mux := http.NewServeMux()
				NewDecisionAPI(client, slog.Default()).RegisterRoutes(mux)
				req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, auth.set(req))
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("status=%d, want 401", w.Code)
				}
				if client.resolveID != "" || client.cancelID != "" || client.createdID != "" {
					t.Fatalf("daemon was called: %+v", client)
				}
			})
		}
	}
}

// End to end through a real daemon client: the resolve reaches the daemon on
// the caller's own bearer, not the bridge's token, so the daemon authorizes
// and attributes it to them (gd-Fhizmbk9Ee).
func TestDecisionAPI_ResolveUsesCallerBearer(t *testing.T) {
	var gotAuth string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemon.URL, Token: "bridge-fleet-token"})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	mux := http.NewServeMux()
	NewDecisionAPI(client, slog.Default()).RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/decisions/gd-d1/resolve", strings.NewReader(`{"chosen":"a"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedIn(req, "alice@example.com"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer jwt-alice@example.com" {
		t.Fatalf("daemon saw Authorization %q, want the caller's bearer", gotAuth)
	}
}

func TestDecisionAPI_MethodNotAllowed(t *testing.T) {
	client := &mockDecisionClient{}
	api := NewDecisionAPI(client, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/decisions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", w.Code)
	}
}

func TestWebHandler_ServesIndexHTML(t *testing.T) {
	handler := http.StripPrefix("/ui/", WebHandler())

	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type=%q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "Decisions") {
		t.Error("response does not contain expected HTML content")
	}
}

func TestDecisionSSEProxy_FilterDecisions(t *testing.T) {
	// Create a mock SSE server that sends one decision and one non-decision event.
	sseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events/stream" {
			http.NotFound(w, r)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher.Flush()

		// Decision event.
		decBead := `{"bead":{"id":"gd-d1","type":"decision","title":"test","status":"open","fields":{}}}`
		_, _ = w.Write([]byte("event: beads.bead.created\ndata: " + decBead + "\n\n"))
		flusher.Flush()

		// Non-decision event (task).
		taskBead := `{"bead":{"id":"gd-t1","type":"task","title":"task","status":"open","fields":{}}}`
		_, _ = w.Write([]byte("event: beads.bead.created\ndata: " + taskBead + "\n\n"))
		flusher.Flush()

		// Wait for client disconnect.
		<-r.Context().Done()
	}))
	defer sseSrv.Close()

	proxy := NewDecisionSSEProxy(sseSrv.URL, "", nil, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/api/decisions/events", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		proxy.ServeHTTP(w, req)
		close(done)
	}()

	<-done

	body := w.Body.String()
	// Should contain the decision event.
	if !strings.Contains(body, "gd-d1") {
		t.Error("expected decision event gd-d1 in SSE output")
	}
	// Should NOT contain the task event.
	if strings.Contains(body, "gd-t1") {
		t.Error("task event gd-t1 should have been filtered out")
	}
}

func TestDecisionAPI_Promote(t *testing.T) {
	client := &mockDecisionClient{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-dec1": {
				ID:       "gd-dec1",
				Title:    "Deploy to production?",
				Type:     "decision",
				Status:   "open",
				Priority: 1,
				Labels:   []string{"project:gardenio"},
				Fields: map[string]string{
					"prompt":                    "Should we deploy v2.0 to production?",
					"options":                   `[{"id":"yes","label":"Yes"},{"id":"no","label":"No"}]`,
					"context":                   "The staging tests pass.",
					"requesting_agent_bead_id":  "gd-agent1",
					"session":                   "gd-session1",
				},
			},
		},
	}
	api := NewDecisionAPI(client, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	body := `{"operationType":"helm_deploy","executionTarget":"local","promotedBy":"mallory"}`
	req := httptest.NewRequest(http.MethodPost, "/api/decisions/gd-dec1/promote", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedIn(req, "alice@example.com"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "promoted" {
		t.Errorf("status=%q, want promoted", resp["status"])
	}
	if resp["escalationId"] != "gd-esc-new" {
		t.Errorf("escalationId=%q, want gd-esc-new", resp["escalationId"])
	}

	// Verify escalation bead was created.
	if client.createdID != "gd-esc-new" {
		t.Errorf("createdID=%q, want gd-esc-new", client.createdID)
	}

	// Verify dependency was added.
	if len(client.depAdded) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(client.depAdded))
	}
	if client.depAdded[0] != "gd-esc-new:gd-dec1:promoted" {
		t.Errorf("dep=%q, want gd-esc-new:gd-dec1:promoted", client.depAdded[0])
	}

	// Verify decision was closed.
	if client.closedID != "gd-dec1" {
		t.Errorf("closedID=%q, want gd-dec1", client.closedID)
	}
	if client.closedFields["chosen"] != "promoted_to_escalation" {
		t.Errorf("chosen=%q, want promoted_to_escalation", client.closedFields["chosen"])
	}
	if client.depBy != "oidc:alice@example.com" || !strings.Contains(client.closedFields["rationale"], "by oidc:alice@example.com") {
		t.Errorf("promoted by %q, rationale %q: want the verified caller, not the body name",
			client.depBy, client.closedFields["rationale"])
	}
}

func TestDecisionAPI_Promote_ClosedDecision(t *testing.T) {
	client := &mockDecisionClient{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-closed1": {
				ID:     "gd-closed1",
				Type:   "decision",
				Status: "closed",
				Fields: map[string]string{"prompt": "Old question"},
			},
		},
	}
	api := NewDecisionAPI(client, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/api/decisions/gd-closed1/promote", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedIn(req, "alice@example.com"))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 for closed decision", w.Code)
	}
	if !strings.Contains(w.Body.String(), "already closed") {
		t.Errorf("expected 'already closed' in body, got %s", w.Body.String())
	}
}

func TestDecisionAPI_Promote_DefaultFields(t *testing.T) {
	client := &mockDecisionClient{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-dec2": {
				ID:     "gd-dec2",
				Type:   "decision",
				Status: "open",
				Fields: map[string]string{"prompt": "Simple question"},
			},
		},
	}
	api := NewDecisionAPI(client, slog.Default())
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// Promote with no operationType or executionTarget — defaults should apply.
	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/api/decisions/gd-dec2/promote", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedIn(req, "alice@example.com"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
}
