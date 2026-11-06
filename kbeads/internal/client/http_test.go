package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testHandler captures the incoming request details and returns a canned response.
type testHandler struct {
	// captured from the request
	method      string
	path        string
	rawPath     string // URL-encoded path (for testing PathEscape)
	requestURI  string
	query       string
	body        string
	contentType string
	headers     http.Header

	// canned response
	statusCode   int
	responseBody string
}

func (h *testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.method = r.Method
	h.path = r.URL.Path
	h.rawPath = r.URL.RawPath
	h.requestURI = r.RequestURI
	h.query = r.URL.RawQuery
	h.contentType = r.Header.Get("Content-Type")
	h.headers = r.Header.Clone()
	if r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		h.body = string(data)
	}

	w.Header().Set("Content-Type", "application/json")
	if h.statusCode != 0 {
		w.WriteHeader(h.statusCode)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if h.responseBody != "" {
		_, _ = w.Write([]byte(h.responseBody))
	}
}

// newTestClient creates an HTTPClient pointed at a test server with the given handler.
func newTestClient(h http.Handler) (*HTTPClient, *httptest.Server) {
	srv := httptest.NewServer(h)
	c := NewHTTPClient(srv.URL, "")
	return c, srv
}

// --- SetConfig ---

func TestHTTPClient_SetConfig(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"key": "view:inbox",
			"value": {"columns": ["title", "status"]},
			"created_at": "2026-01-15T10:00:00Z",
			"updated_at": "2026-01-15T10:00:00Z"
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	val := json.RawMessage(`{"columns": ["title", "status"]}`)
	cfg, err := c.SetConfig(context.Background(), "view:inbox", val)
	if err != nil {
		t.Fatalf("SetConfig() error = %v", err)
	}

	if h.method != http.MethodPut {
		t.Errorf("method = %q, want PUT", h.method)
	}
	if h.path != "/v1/configs/view:inbox" {
		t.Errorf("path = %q, want /v1/configs/view:inbox", h.path)
	}

	var reqBody map[string]json.RawMessage
	if err := json.Unmarshal([]byte(h.body), &reqBody); err != nil {
		t.Fatalf("unmarshaling request body: %v", err)
	}
	var gotVal, wantVal interface{}
	if err := json.Unmarshal(reqBody["value"], &gotVal); err != nil {
		t.Fatalf("unmarshaling request value: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"columns": ["title", "status"]}`), &wantVal); err != nil {
		t.Fatalf("unmarshaling expected value: %v", err)
	}
	gotJSON, _ := json.Marshal(gotVal)
	wantJSON, _ := json.Marshal(wantVal)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("request body value = %s, want %s", gotJSON, wantJSON)
	}

	if cfg.Key != "view:inbox" {
		t.Errorf("cfg.Key = %q, want 'view:inbox'", cfg.Key)
	}
	if cfg.Value == nil {
		t.Error("cfg.Value is nil, want non-nil")
	}
}

// --- GetConfig ---

func TestHTTPClient_GetConfig(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"key": "type:decision",
			"value": {"fields": [{"name": "status", "type": "enum"}]},
			"created_at": "2026-01-15T10:00:00Z",
			"updated_at": "2026-01-16T10:00:00Z"
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	cfg, err := c.GetConfig(context.Background(), "type:decision")
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}

	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/configs/type:decision" {
		t.Errorf("path = %q, want /v1/configs/type:decision", h.path)
	}

	if cfg.Key != "type:decision" {
		t.Errorf("cfg.Key = %q, want 'type:decision'", cfg.Key)
	}
}

// --- ListConfigs ---

func TestHTTPClient_ListConfigs(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"configs": [
				{"key": "view:inbox", "value": {}, "created_at": "2026-01-15T10:00:00Z", "updated_at": "2026-01-15T10:00:00Z"},
				{"key": "view:board", "value": {}, "created_at": "2026-01-15T10:00:00Z", "updated_at": "2026-01-15T10:00:00Z"}
			]
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	configs, err := c.ListConfigs(context.Background(), "view")
	if err != nil {
		t.Fatalf("ListConfigs() error = %v", err)
	}

	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/configs" {
		t.Errorf("path = %q, want /v1/configs", h.path)
	}
	if !strings.Contains(h.query, "namespace=view") {
		t.Errorf("query %q missing namespace=view", h.query)
	}

	if len(configs) != 2 {
		t.Fatalf("len(configs) = %d, want 2", len(configs))
	}
	if configs[0].Key != "view:inbox" {
		t.Errorf("configs[0].Key = %q, want 'view:inbox'", configs[0].Key)
	}
}

func TestHTTPClient_ListConfigs_Empty(t *testing.T) {
	h := &testHandler{
		responseBody: `{"configs": []}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	configs, err := c.ListConfigs(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("ListConfigs() error = %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("len(configs) = %d, want 0", len(configs))
	}
}

// --- DeleteConfig ---

func TestHTTPClient_DeleteConfig(t *testing.T) {
	h := &testHandler{
		statusCode: http.StatusNoContent,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	err := c.DeleteConfig(context.Background(), "view:inbox")
	if err != nil {
		t.Fatalf("DeleteConfig() error = %v", err)
	}

	if h.method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", h.method)
	}
	if h.path != "/v1/configs/view:inbox" {
		t.Errorf("path = %q, want /v1/configs/view:inbox", h.path)
	}
}

// --- Health ---

func TestHTTPClient_Health(t *testing.T) {
	h := &testHandler{
		responseBody: `{"status": "ok"}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	status, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health() error = %v", err)
	}

	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/health" {
		t.Errorf("path = %q, want /v1/health", h.path)
	}

	if status != "ok" {
		t.Errorf("status = %q, want 'ok'", status)
	}
}

// --- Error handling ---

func TestHTTPClient_Error_JSONBody(t *testing.T) {
	h := &testHandler{
		statusCode:   http.StatusBadRequest,
		responseBody: `{"error": "bead title is required"}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	_, err := c.CreateBead(context.Background(), &CreateBeadRequest{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
	if apiErr.Message != "bead title is required" {
		t.Errorf("message = %q, want 'bead title is required'", apiErr.Message)
	}
}

func TestHTTPClient_Error_NonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "")
	_, err := c.GetBead(context.Background(), "bead-123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", apiErr.StatusCode)
	}
	if apiErr.Message != "internal server error" {
		t.Errorf("message = %q, want 'internal server error'", apiErr.Message)
	}
}

func TestHTTPClient_Error_404(t *testing.T) {
	h := &testHandler{
		statusCode:   http.StatusNotFound,
		responseBody: `{"error": "bead not found"}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	_, err := c.GetBead(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", apiErr.StatusCode)
	}
	if apiErr.Message != "bead not found" {
		t.Errorf("message = %q, want 'bead not found'", apiErr.Message)
	}
}

func TestHTTPClient_Error_500(t *testing.T) {
	h := &testHandler{
		statusCode:   http.StatusInternalServerError,
		responseBody: `{"error": "database connection lost"}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	err := c.DeleteBead(context.Background(), "bead-123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", apiErr.StatusCode)
	}
}

func TestHTTPClient_Error_FormatString(t *testing.T) {
	apiErr := &APIError{StatusCode: 403, Message: "forbidden"}
	want := "HTTP 403: forbidden"
	if apiErr.Error() != want {
		t.Errorf("Error() = %q, want %q", apiErr.Error(), want)
	}
}

func TestHTTPClient_Error_EmptyJSONError(t *testing.T) {
	h := &testHandler{
		statusCode:   http.StatusUnprocessableEntity,
		responseBody: `{"error": ""}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	_, err := c.GetBead(context.Background(), "bead-123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", apiErr.StatusCode)
	}
	if apiErr.Message != `{"error": ""}` {
		t.Errorf("message = %q, want raw body", apiErr.Message)
	}
}

func TestHTTPClient_Error_CanceledContext(t *testing.T) {
	h := &testHandler{
		responseBody: `{"status": "ok"}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Health(ctx)
	if err == nil {
		t.Fatal("expected error for canceled context, got nil")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error = %q, want to contain 'context canceled'", err.Error())
	}
}

// --- 204 No Content handling ---

func TestHTTPClient_204NoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "")

	err := c.DeleteBead(context.Background(), "bead-del")
	if err != nil {
		t.Fatalf("DeleteBead() with 204 error = %v", err)
	}

	err = c.DeleteConfig(context.Background(), "some:key")
	if err != nil {
		t.Fatalf("DeleteConfig() with 204 error = %v", err)
	}

	if _, err = c.RemoveLabel(context.Background(), "bead-x", "label"); err != nil {
		t.Fatalf("RemoveLabel() with 204 error = %v", err)
	}

	if _, err = c.RemoveDependency(context.Background(), "bead-a", "bead-b", "blocks"); err != nil {
		t.Fatalf("RemoveDependency() with 204 error = %v", err)
	}
}

// --- Close ---

func TestHTTPClient_Close(t *testing.T) {
	c := NewHTTPClient("http://localhost:9999", "")
	if err := c.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

// --- NewHTTPClient base URL trimming ---

func TestNewHTTPClient_TrimsTrailingSlash(t *testing.T) {
	c := NewHTTPClient("http://localhost:8080/", "")
	if c.baseURL != "http://localhost:8080" {
		t.Errorf("baseURL = %q, want 'http://localhost:8080'", c.baseURL)
	}
}

func TestNewHTTPClient_NoTrailingSlash(t *testing.T) {
	c := NewHTTPClient("http://localhost:8080", "")
	if c.baseURL != "http://localhost:8080" {
		t.Errorf("baseURL = %q, want 'http://localhost:8080'", c.baseURL)
	}
}

// --- SemanticSearch ---

func TestHTTPClient_SemanticSearch(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"model": "nomic-embed-text",
			"results": [
				{"bead": {"id": "gd-AAA", "title": "Login retries"}, "score": 0.92, "distance": 0.08},
				{"bead": {"id": "gd-BBB", "title": "Auth timeout"},  "score": 0.81, "distance": 0.19}
			]
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.SemanticSearch(context.Background(), &SemanticSearchRequest{
		Query:    "failed login",
		Status:   []string{"open", "in_progress"},
		Type:     []string{"bug"},
		Labels:   []string{"project:gardenio"},
		Assignee: "alice",
		Limit:    25,
		MinScore: 0.5,
	})
	if err != nil {
		t.Fatalf("SemanticSearch() error = %v", err)
	}

	if h.method != http.MethodPost {
		t.Errorf("method = %q, want POST", h.method)
	}
	if h.path != "/v1/search/semantic" {
		t.Errorf("path = %q, want /v1/search/semantic", h.path)
	}
	if h.contentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", h.contentType)
	}

	// Verify the request body is the marshaled SemanticSearchRequest.
	var body SemanticSearchRequest
	if err := json.Unmarshal([]byte(h.body), &body); err != nil {
		t.Fatalf("unmarshal request body: %v\nbody=%s", err, h.body)
	}
	if body.Query != "failed login" {
		t.Errorf("body.Query = %q, want 'failed login'", body.Query)
	}
	if len(body.Status) != 2 || body.Status[0] != "open" || body.Status[1] != "in_progress" {
		t.Errorf("body.Status = %v, want [open in_progress]", body.Status)
	}
	if len(body.Type) != 1 || body.Type[0] != "bug" {
		t.Errorf("body.Type = %v, want [bug]", body.Type)
	}
	if body.Assignee != "alice" {
		t.Errorf("body.Assignee = %q, want alice", body.Assignee)
	}
	if body.Limit != 25 {
		t.Errorf("body.Limit = %d, want 25", body.Limit)
	}
	if body.MinScore != 0.5 {
		t.Errorf("body.MinScore = %v, want 0.5", body.MinScore)
	}

	if resp.Model != "nomic-embed-text" {
		t.Errorf("resp.Model = %q, want nomic-embed-text", resp.Model)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("resp.Results = %d, want 2", len(resp.Results))
	}
	if resp.Results[0].Bead.ID != "gd-AAA" || resp.Results[0].Score != 0.92 {
		t.Errorf("result 0 = %+v", resp.Results[0])
	}
	if resp.Results[1].Distance != 0.19 {
		t.Errorf("result 1 distance = %v, want 0.19", resp.Results[1].Distance)
	}
}

func TestHTTPClient_SemanticSearch_NilRequest(t *testing.T) {
	c := NewHTTPClient("http://localhost:9999", "")
	_, err := c.SemanticSearch(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil request, got nil")
	}
}

func TestHTTPClient_SemanticSearch_ServerError(t *testing.T) {
	h := &testHandler{
		statusCode:   http.StatusServiceUnavailable,
		responseBody: `{"error": "semantic search disabled (BEADS_EMBED_BACKEND=noop)"}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	_, err := c.SemanticSearch(context.Background(), &SemanticSearchRequest{Query: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Message, "semantic search disabled") {
		t.Errorf("message = %q, want contains 'semantic search disabled'", apiErr.Message)
	}
}

// --- Interface compliance ---

func TestHTTPClient_ImplementsBeadsClient(t *testing.T) {
	var _ BeadsClient = (*HTTPClient)(nil)
}

// --- Concurrent requests ---

func TestHTTPClient_ConcurrentRequests(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status": "ok"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "")

	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			_, err := c.Health(context.Background())
			errs <- err
		}()
	}

	for i := 0; i < 10; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Health() error = %v", err)
		}
	}
}

// --- GetAgentRoster ---

func TestHTTPClient_GetAgentRoster(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"actors": [
				{
					"actor": "wise-newt",
					"task_id": "bd-abc12",
					"task_title": "Fix login bug",
					"idle_secs": 30.5,
					"last_event": "Bash",
					"session_id": "sess-1"
				},
				{
					"actor": "ripe-elk",
					"idle_secs": 120.0,
					"reaped": true
				}
			],
			"unclaimed_tasks": [
				{"id": "bd-xyz99", "title": "Write docs", "priority": 2}
			]
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.GetAgentRoster(context.Background(), 600)
	if err != nil {
		t.Fatalf("GetAgentRoster() error = %v", err)
	}

	// Verify request.
	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/agents/roster" {
		t.Errorf("path = %q, want /v1/agents/roster", h.path)
	}
	if h.query != "stale_threshold_secs=600" {
		t.Errorf("query = %q, want stale_threshold_secs=600", h.query)
	}

	// Verify parsed response.
	if len(resp.Actors) != 2 {
		t.Fatalf("len(Actors) = %d, want 2", len(resp.Actors))
	}
	a0 := resp.Actors[0]
	if a0.Actor != "wise-newt" {
		t.Errorf("Actors[0].Actor = %q, want wise-newt", a0.Actor)
	}
	if a0.TaskID != "bd-abc12" {
		t.Errorf("Actors[0].TaskID = %q, want bd-abc12", a0.TaskID)
	}
	if a0.TaskTitle != "Fix login bug" {
		t.Errorf("Actors[0].TaskTitle = %q, want Fix login bug", a0.TaskTitle)
	}
	if a0.IdleSecs != 30.5 {
		t.Errorf("Actors[0].IdleSecs = %f, want 30.5", a0.IdleSecs)
	}
	if a0.LastEvent != "Bash" {
		t.Errorf("Actors[0].LastEvent = %q, want Bash", a0.LastEvent)
	}
	if a0.SessionID != "sess-1" {
		t.Errorf("Actors[0].SessionID = %q, want sess-1", a0.SessionID)
	}

	a1 := resp.Actors[1]
	if a1.Actor != "ripe-elk" {
		t.Errorf("Actors[1].Actor = %q, want ripe-elk", a1.Actor)
	}
	if !a1.Reaped {
		t.Error("Actors[1].Reaped = false, want true")
	}
	if a1.TaskID != "" {
		t.Errorf("Actors[1].TaskID = %q, want empty", a1.TaskID)
	}

	if len(resp.UnclaimedTasks) != 1 {
		t.Fatalf("len(UnclaimedTasks) = %d, want 1", len(resp.UnclaimedTasks))
	}
	ut := resp.UnclaimedTasks[0]
	if ut.ID != "bd-xyz99" || ut.Title != "Write docs" || ut.Priority != 2 {
		t.Errorf("UnclaimedTasks[0] = %+v, want {bd-xyz99, Write docs, 2}", ut)
	}
}

func TestHTTPClient_GetAgentRoster_Empty(t *testing.T) {
	h := &testHandler{
		responseBody: `{"actors": [], "unclaimed_tasks": []}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.GetAgentRoster(context.Background(), 300)
	if err != nil {
		t.Fatalf("GetAgentRoster() error = %v", err)
	}
	if len(resp.Actors) != 0 {
		t.Errorf("len(Actors) = %d, want 0", len(resp.Actors))
	}
	if len(resp.UnclaimedTasks) != 0 {
		t.Errorf("len(UnclaimedTasks) = %d, want 0", len(resp.UnclaimedTasks))
	}
}

func TestHTTPClient_GetAgentRoster_ServerError(t *testing.T) {
	h := &testHandler{
		statusCode:   http.StatusInternalServerError,
		responseBody: `{"error": "database unavailable"}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	_, err := c.GetAgentRoster(context.Background(), 600)
	if err == nil {
		t.Fatal("GetAgentRoster() expected error for 500 response")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 500 {
		t.Errorf("APIError.StatusCode = %d, want 500", apiErr.StatusCode)
	}
}

func TestHTTPClient_GetAgentRoster_DefaultThreshold(t *testing.T) {
	h := &testHandler{
		responseBody: `{"actors": [], "unclaimed_tasks": []}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	_, err := c.GetAgentRoster(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetAgentRoster() error = %v", err)
	}
	if h.query != "stale_threshold_secs=0" {
		t.Errorf("query = %q, want stale_threshold_secs=0", h.query)
	}
}

// --- Escalation context ---

func TestWithEscalate_SetsHeader(t *testing.T) {
	h := &testHandler{
		responseBody: `{"id":"gd-test","title":"test","type":"task","kind":"issue","status":"open","priority":2}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	ctx := WithEscalate(context.Background())
	_, _ = c.GetBead(ctx, "test")

	if h.headers.Get("X-Escalate") != "true" {
		t.Errorf("X-Escalate header = %q, want 'true'", h.headers.Get("X-Escalate"))
	}
}

func TestWithoutEscalate_NoHeader(t *testing.T) {
	h := &testHandler{
		responseBody: `{"id":"gd-test","title":"test","type":"task","kind":"issue","status":"open","priority":2}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	_, _ = c.GetBead(context.Background(), "test")

	if h.headers.Get("X-Escalate") != "" {
		t.Errorf("X-Escalate header = %q, want empty", h.headers.Get("X-Escalate"))
	}
}

// --- 202 Accepted / escalation created ---

func TestHTTPClient_202_EscalationCreated(t *testing.T) {
	h := &testHandler{
		statusCode: http.StatusAccepted,
		responseBody: `{
			"escalation_id": "gd-esc123",
			"message": "escalation created: pending operator approval",
			"denied_action": "create",
			"bead_type": "agent"
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	ctx := WithEscalate(context.Background())
	_, err := c.CreateBead(ctx, &CreateBeadRequest{Title: "test agent", Type: "agent"})
	if err == nil {
		t.Fatal("expected EscalationCreated error, got nil")
	}

	var esc *EscalationCreated
	if !errors.As(err, &esc) {
		t.Fatalf("expected *EscalationCreated, got %T: %v", err, err)
	}
	if esc.EscalationID != "gd-esc123" {
		t.Errorf("EscalationID = %q, want gd-esc123", esc.EscalationID)
	}
	if esc.DeniedAction != "create" {
		t.Errorf("DeniedAction = %q, want create", esc.DeniedAction)
	}
	if esc.BeadType != "agent" {
		t.Errorf("BeadType = %q, want agent", esc.BeadType)
	}
}

func TestHTTPClient_EscalationCreated_ErrorString(t *testing.T) {
	esc := &EscalationCreated{
		EscalationID: "gd-esc1",
		Message:      "pending approval",
	}
	want := "escalation created (gd-esc1): pending approval"
	if esc.Error() != want {
		t.Errorf("Error() = %q, want %q", esc.Error(), want)
	}
}
