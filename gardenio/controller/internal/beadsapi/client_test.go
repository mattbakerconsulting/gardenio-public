package beadsapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// --- Config / New tests ---

func TestNew_AutoPrependsHTTPScheme(t *testing.T) {
	c, err := New(Config{HTTPAddr: "localhost:8080"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.baseURL != "http://localhost:8080" {
		t.Errorf("expected http://localhost:8080, got %s", c.baseURL)
	}
}

func TestNew_DoesNotDoublePrependHTTP(t *testing.T) {
	c, err := New(Config{HTTPAddr: "http://already-has-scheme:8080"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.baseURL != "http://already-has-scheme:8080" {
		t.Errorf("expected http://already-has-scheme:8080, got %s", c.baseURL)
	}
}

func TestNew_DoesNotDoublePrependHTTPS(t *testing.T) {
	c, err := New(Config{HTTPAddr: "https://secure:443"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.baseURL != "https://secure:443" {
		t.Errorf("expected https://secure:443, got %s", c.baseURL)
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	c, err := New(Config{HTTPAddr: "http://host:8080/"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.baseURL != "http://host:8080" {
		t.Errorf("expected trailing slash removed, got %s", c.baseURL)
	}
}

func TestNew_EmptyAddrReturnsError(t *testing.T) {
	_, err := New(Config{HTTPAddr: ""})
	if err == nil {
		t.Fatal("expected error for empty HTTPAddr")
	}
	if !strings.Contains(err.Error(), "HTTPAddr is required") {
		t.Errorf("expected 'HTTPAddr is required' error, got: %v", err)
	}
}

func TestClose_NoOp(t *testing.T) {
	c, _ := New(Config{HTTPAddr: "localhost:1"})
	if err := c.Close(); err != nil {
		t.Errorf("Close should be no-op, got error: %v", err)
	}
}

func TestNew_ZitiConfigRequiresIdentityAndService(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "identity without service",
			cfg:  Config{HTTPAddr: "http://daemon", ZitiIdentityPath: "gb-client.json"},
		},
		{
			name: "service without identity",
			cfg:  Config{HTTPAddr: "http://daemon", ZitiService: "gardenio-beads"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil {
				t.Fatal("expected ziti config validation error")
			}
			if !strings.Contains(err.Error(), "ziti identity and ziti service must be set together") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestNew_ZitiTransportPreservesLogicalHTTPOrigin(t *testing.T) {
	origFactory := newZitiDialer
	defer func() { newZitiDialer = origFactory }()

	fake := &fakeZitiDialer{
		requests:     make(chan *http.Request, 1),
		services:     make(chan string, 1),
		responseBody: `{"beads":[],"total":0}`,
	}
	newZitiDialer = func(identityPath string) (zitiDialer, error) {
		if identityPath != "/tmp/gb-client.json" {
			t.Fatalf("identityPath = %q", identityPath)
		}
		return fake, nil
	}

	c, err := New(Config{
		HTTPAddr:         "http://logical-gardenio",
		ZitiIdentityPath: "/tmp/gb-client.json",
		ZitiService:      "gardenio-beads",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.zitiContext == nil {
		t.Fatal("expected ziti context to be installed")
	}
	// Both clients dial through the one owned base transport, which carries the
	// ziti dialer. The normal client wraps that base in otelhttp; the SSE client
	// uses it directly — so they share the SAME base pool and ziti dialer.
	if c.baseTransport == nil {
		t.Fatal("expected an owned base transport carrying the ziti dialer")
	}
	if c.sseClient.Transport != http.RoundTripper(c.baseTransport) {
		t.Fatal("expected SSE client to dial through the shared ziti base transport")
	}

	_, err = c.ListBeadsFiltered(context.Background(), ListBeadsQuery{Types: []string{"task"}, Limit: 1})
	if err != nil {
		t.Fatalf("ListBeadsFiltered: %v", err)
	}

	select {
	case service := <-fake.services:
		if service != "gardenio-beads" {
			t.Fatalf("ziti service = %q", service)
		}
	default:
		t.Fatal("expected ziti dial")
	}
	select {
	case req := <-fake.requests:
		if req.Host != "logical-gardenio" {
			t.Fatalf("request Host = %q", req.Host)
		}
		if req.URL.Path != "/v1/beads" {
			t.Fatalf("request path = %q", req.URL.Path)
		}
	default:
		t.Fatal("expected HTTP request over ziti transport")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fake.closed {
		t.Fatal("expected Close to close ziti context")
	}
}

type fakeZitiDialer struct {
	requests     chan *http.Request
	services     chan string
	responseBody string
	closed       bool
}

func (f *fakeZitiDialer) DialContext(ctx context.Context, serviceName string) (net.Conn, error) {
	select {
	case f.services <- serviceName:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return
		}
		f.requests <- req
		body := f.responseBody
		_, _ = fmt.Fprintf(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}()
	return client, nil
}

func (f *fakeZitiDialer) Close() {
	f.closed = true
}

// TestSetActor_SendsXActorHeader verifies SetActor wires the X-Actor header
// onto every subsequent write request. Without this, primary-static-token
// callers (agent pods using BEADS_DAEMON_TOKEN) are attributed as "static"
// by the server audit trail instead of the agent name.
func TestSetActor_SendsXActorHeader(t *testing.T) {
	var gotActor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActor = r.Header.Get("X-Actor")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"gd-abc"}`))
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.SetActor("thread-bot-1")

	if _, err := c.CreateBead(context.Background(), CreateBeadRequest{Title: "t", Type: "task"}); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if gotActor != "thread-bot-1" {
		t.Errorf("expected X-Actor=thread-bot-1, got %q", gotActor)
	}
}

// TestSetActor_EmptyActorOmitsHeader verifies that when SetActor is not
// called, no X-Actor header is sent — preserving existing behavior for
// callers that have no actor identity.
func TestSetActor_EmptyActorOmitsHeader(t *testing.T) {
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["X-Actor"]; ok {
			sawHeader = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"gd-abc"}`))
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.CreateBead(context.Background(), CreateBeadRequest{Title: "t", Type: "task"}); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if sawHeader {
		t.Errorf("expected no X-Actor header when actor is empty")
	}
}

// TestWithActor_SendsXActorHeader verifies the per-request WithActor(ctx, …)
// stamps X-Actor for that one call — the primitive beads-viewer uses to
// attribute each comment to its logged-in author without racing on a
// shared client's mutable SetActor state (ka-0000006262).
func TestWithActor_SendsXActorHeader(t *testing.T) {
	var gotActor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActor = r.Header.Get("X-Actor")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := WithActor(context.Background(), "e2e-t7@example.test")
	if err := c.AddComment(ctx, "gd-abc", "ignored-body-author", "hi"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if gotActor != "e2e-t7@example.test" {
		t.Errorf("expected X-Actor=e2e-t7@example.test, got %q", gotActor)
	}
}

// TestWithActor_OverridesClientDefault verifies the per-request WithActor wins
// over the client-wide SetActor default, so a shared client can attribute a
// single call to a different actor than its baseline identity.
func TestWithActor_OverridesClientDefault(t *testing.T) {
	var gotActor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActor = r.Header.Get("X-Actor")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.SetActor("client-default-actor")

	ctx := WithActor(context.Background(), "per-request-actor")
	if err := c.AddComment(ctx, "gd-abc", "ignored", "hi"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if gotActor != "per-request-actor" {
		t.Errorf("expected per-request WithActor to win, got X-Actor=%q", gotActor)
	}

	// With no per-request actor, the client default is restored.
	if err := c.AddComment(context.Background(), "gd-abc", "ignored", "hi"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if gotActor != "client-default-actor" {
		t.Errorf("expected client default when ctx has no actor, got X-Actor=%q", gotActor)
	}
}

// --- GetBead tests ---

func TestGetBead_ParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/beads/bd-abc123" {
			t.Errorf("expected path /v1/beads/bd-abc123, got %s", r.URL.Path)
		}
		bead := beadJSON{
			ID:     "bd-abc123",
			Title:  "Test bead",
			Type:   "issue",
			Status: "open",
			Labels: []string{"p1", "bug"},
			Notes:  "key: value",
			Fields: json.RawMessage(`{"priority":"high","component":"api"}`),
		}
		_ = json.NewEncoder(w).Encode(bead)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	detail, err := c.GetBead(context.Background(), "bd-abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if detail.ID != "bd-abc123" {
		t.Errorf("expected ID bd-abc123, got %s", detail.ID)
	}
	if detail.Title != "Test bead" {
		t.Errorf("expected title 'Test bead', got %s", detail.Title)
	}
	if detail.Type != "issue" {
		t.Errorf("expected type issue, got %s", detail.Type)
	}
	if detail.Status != "open" {
		t.Errorf("expected status open, got %s", detail.Status)
	}
	if len(detail.Labels) != 2 || detail.Labels[0] != "p1" || detail.Labels[1] != "bug" {
		t.Errorf("expected labels [p1, bug], got %v", detail.Labels)
	}
	if detail.Notes != "key: value" {
		t.Errorf("expected notes 'key: value', got %s", detail.Notes)
	}
	if detail.Fields["priority"] != "high" {
		t.Errorf("expected field priority=high, got %s", detail.Fields["priority"])
	}
	if detail.Fields["component"] != "api" {
		t.Errorf("expected field component=api, got %s", detail.Fields["component"])
	}
}

func TestGetBead_HandlesEmptyFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bead := beadJSON{
			ID:     "bd-empty",
			Title:  "No fields",
			Type:   "issue",
			Status: "open",
		}
		_ = json.NewEncoder(w).Encode(bead)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	detail, err := c.GetBead(context.Background(), "bd-empty")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(detail.Fields) != 0 {
		t.Errorf("expected empty fields, got %v", detail.Fields)
	}
}

func TestGetBead_EscapesBeadID(t *testing.T) {
	var gotRawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.RawPath
		bead := beadJSON{ID: "has/slash"}
		_ = json.NewEncoder(w).Encode(bead)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.GetBead(context.Background(), "has/slash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The slash should be percent-encoded in the raw URL.
	if gotRawPath != "/v1/beads/has%2Fslash" {
		t.Errorf("expected encoded path, got %s", gotRawPath)
	}
}

// --- CloseBead tests ---

func TestCloseBead_SendsCloseRequest(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.CloseBead(context.Background(), "bd-close1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/beads/bd-close1/close" {
		t.Errorf("expected path /v1/beads/bd-close1/close, got %s", gotPath)
	}
	if gotBody["closed_by"] != "gardenio" {
		t.Errorf("expected closed_by=gardenio, got %v", gotBody)
	}
}

func TestCloseBead_SendsFieldsInCloseBody(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close") {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.CloseBead(context.Background(), "bd-close2", map[string]string{
		"required_artifact": "report",
		"artifact_status":   "pending",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Fields must be included in the single POST /close request body.
	if gotBody["required_artifact"] != "report" {
		t.Errorf("expected required_artifact=report in close body, got %v", gotBody)
	}
	if gotBody["artifact_status"] != "pending" {
		t.Errorf("expected artifact_status=pending in close body, got %v", gotBody)
	}
	if gotBody["closed_by"] != "gardenio" {
		t.Errorf("expected closed_by=gardenio in close body, got %v", gotBody)
	}
}

func TestCloseBead_SkipsFieldUpdateWhenEmpty(t *testing.T) {
	var requests []struct {
		Method string
		Path   string
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, struct {
			Method string
			Path   string
		}{r.Method, r.URL.Path})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.CloseBead(context.Background(), "bd-close3", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With nil fields, only the POST /close should happen.
	if len(requests) != 1 {
		t.Fatalf("expected 1 request (POST only), got %d: %v", len(requests), requests)
	}
	if requests[0].Method != http.MethodPost {
		t.Errorf("expected POST, got %s", requests[0].Method)
	}
}

// --- Content-Type header test ---

func TestDoJSON_SetsContentTypeForBody(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_ = c.UpdateBeadNotes(context.Background(), "bd-ct", "notes")

	if gotContentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", gotContentType)
	}
}

func TestDoJSON_NoContentTypeForGET(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		bead := beadJSON{ID: "bd-get"}
		_ = json.NewEncoder(w).Encode(bead)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, _ = c.GetBead(context.Background(), "bd-get")

	if gotContentType != "" {
		t.Errorf("expected no Content-Type for GET, got %s", gotContentType)
	}
}

// --- BeadLister interface compliance ---

func TestClient_ImplementsBeadLister(t *testing.T) {
	var _ BeadLister = (*Client)(nil)
}

// --- parseTimestamp tests ---

func TestParseTimestamp_RFC3339(t *testing.T) {
	ts := parseTimestamp("2026-02-25T08:29:35Z")
	if ts.IsZero() {
		t.Fatal("expected non-zero time for RFC3339 input")
	}
	if ts.Year() != 2026 || ts.Month() != 2 || ts.Day() != 25 {
		t.Errorf("unexpected date: %v", ts)
	}
}

func TestParseTimestamp_SpaceSeparated(t *testing.T) {
	ts := parseTimestamp("2026-02-25 08:29:35")
	if ts.IsZero() {
		t.Fatal("expected non-zero time for space-separated input")
	}
	if ts.Year() != 2026 || ts.Month() != 2 || ts.Day() != 25 {
		t.Errorf("unexpected date: %v", ts)
	}
}

func TestParseTimestamp_Empty(t *testing.T) {
	ts := parseTimestamp("")
	if !ts.IsZero() {
		t.Errorf("expected zero time for empty input, got %v", ts)
	}
}

func TestParseTimestamp_Invalid(t *testing.T) {
	ts := parseTimestamp("not-a-date")
	if !ts.IsZero() {
		t.Errorf("expected zero time for invalid input, got %v", ts)
	}
}

func TestToDetail_ParsesUpdatedAt(t *testing.T) {
	b := beadJSON{
		ID:        "bd-ts",
		Title:     "Timestamp bead",
		UpdatedAt: "2026-02-25T08:29:35Z",
	}
	detail := b.toDetail()
	if detail.UpdatedAt.IsZero() {
		t.Fatal("expected UpdatedAt to be set")
	}
	if detail.UpdatedAt.Year() != 2026 {
		t.Errorf("expected year 2026, got %d", detail.UpdatedAt.Year())
	}
}

func TestToDetail_ZeroUpdatedAtWhenMissing(t *testing.T) {
	b := beadJSON{ID: "bd-no-ts", Title: "No timestamp"}
	detail := b.toDetail()
	if !detail.UpdatedAt.IsZero() {
		t.Errorf("expected zero UpdatedAt when not set, got %v", detail.UpdatedAt)
	}
}

// --- BatchGetBeads tests (gd-dfLn28ZU8O) ---

// TestBatchGetBeads_HappyPath verifies a single-shard request returns one
// entry per found bead, keyed by ID, and that the GET hits /v1/batch/beads
// with the ids= query parameter.
func TestBatchGetBeads_HappyPath(t *testing.T) {
	var gotPath, gotIDs string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotIDs = r.URL.Query().Get("ids")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[
			{"id":"gd-a","bead":{"id":"gd-a","title":"A","type":"doc"}},
			{"id":"gd-b","bead":{"id":"gd-b","title":"B","type":"doc"}}
		]}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	out, err := c.BatchGetBeads(context.Background(), []string{"gd-a", "gd-b"})
	if err != nil {
		t.Fatalf("BatchGetBeads: %v", err)
	}
	if gotPath != "/v1/batch/beads" {
		t.Errorf("path=%q, want /v1/batch/beads", gotPath)
	}
	if gotIDs != "gd-a,gd-b" {
		t.Errorf("ids=%q, want gd-a,gd-b", gotIDs)
	}
	if len(out) != 2 {
		t.Fatalf("len(out)=%d, want 2", len(out))
	}
	if out["gd-a"] == nil || out["gd-a"].Title != "A" {
		t.Errorf("out[gd-a]=%+v, want A", out["gd-a"])
	}
	if out["gd-b"] == nil || out["gd-b"].Title != "B" {
		t.Errorf("out[gd-b]=%+v, want B", out["gd-b"])
	}
}

// TestBatchGetBeads_PartialNotFound asserts that missing IDs (per-result
// error or absent bead) are simply omitted from the map — the call still
// succeeds, the present beads still resolve.
func TestBatchGetBeads_PartialNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[
			{"id":"gd-found","bead":{"id":"gd-found","title":"Found","type":"doc"}},
			{"id":"gd-missing","error":"not found"}
		]}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	out, err := c.BatchGetBeads(context.Background(), []string{"gd-found", "gd-missing"})
	if err != nil {
		t.Fatalf("BatchGetBeads should not fail on partial not-found: %v", err)
	}
	if _, ok := out["gd-missing"]; ok {
		t.Errorf("missing id should not be in result map, got %+v", out["gd-missing"])
	}
	if out["gd-found"] == nil || out["gd-found"].Title != "Found" {
		t.Errorf("found id should resolve, got %+v", out["gd-found"])
	}
}

// TestBatchGetBeads_Daemon500 asserts a 5xx surfaces as an error.
func TestBatchGetBeads_Daemon500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	out, err := c.BatchGetBeads(context.Background(), []string{"gd-a"})
	if err == nil {
		t.Fatalf("expected error on 500, got nil (out=%+v)", out)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected APIError with 500, got %v", err)
	}
}

// TestBatchGetBeads_ShardsAtCap verifies requests over BatchGetBeadsMaxIDs are
// split into multiple round-trips, each capped at the maximum.
//
// Shards are dispatched CONCURRENTLY (ka-0000000376), so this asserts the
// multiset of shard sizes rather than their arrival order — the order is
// genuinely nondeterministic now and pinning it would make the test flaky.
// The handler's append is mutex-guarded for the same reason.
func TestBatchGetBeads_ShardsAtCap(t *testing.T) {
	var mu sync.Mutex
	var shardSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids := r.URL.Query().Get("ids")
		count := 1
		for _, ch := range ids {
			if ch == ',' {
				count++
			}
		}
		mu.Lock()
		shardSizes = append(shardSizes, count)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	ids := make([]string, BatchGetBeadsMaxIDs+5)
	for i := range ids {
		ids[i] = "gd-x"
	}
	if _, err := c.BatchGetBeads(context.Background(), ids); err != nil {
		t.Fatalf("BatchGetBeads: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(shardSizes) != 2 {
		t.Fatalf("expected 2 shards for %d ids, got %d (sizes=%v)", len(ids), len(shardSizes), shardSizes)
	}
	got := slices.Clone(shardSizes)
	slices.Sort(got)
	if want := []int{5, BatchGetBeadsMaxIDs}; !slices.Equal(got, want) {
		t.Errorf("shard sizes=%v, want %v in some order", shardSizes, want)
	}
}

func TestBatchGetBeads_EmptyInput(t *testing.T) {
	c := &Client{baseURL: "http://unused", httpClient: &http.Client{}}
	out, err := c.BatchGetBeads(context.Background(), nil)
	if err != nil {
		t.Fatalf("BatchGetBeads(nil) should not error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty map, got %+v", out)
	}
}
