package main

// /cassettes wire-audit surface tests (Phase-7 S6, ka-0000006302).
// Covers: degraded mode (no store → explicit empty state, 200 not 5xx),
// enumeration error, empty store, native-identity render, tool-choice /
// reasoning / usage projection, thinking preview clipping, truncation cap,
// fetch-failure coverage-gap counting, and the identity sort order.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/claudemore"
)

// fakeCassetteReader is an in-memory cassetteReader for tests that need to
// inject enumeration/fetch errors the real FS store can't easily produce.
type fakeCassetteReader struct {
	refs     []claudemore.CassetteRef
	byKey    map[string]claudemore.Cassette
	listErr  error
	getErrOn map[string]error // key "model/hash" → error to return from Get
}

func keyOf(model, hash string) string { return model + "/" + hash }

func (f *fakeCassetteReader) List(context.Context) ([]claudemore.CassetteRef, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.refs, nil
}

func (f *fakeCassetteReader) Put(context.Context, claudemore.Cassette) error { return nil }

func (f *fakeCassetteReader) Get(_ context.Context, model, hash string) (claudemore.Cassette, error) {
	if err, ok := f.getErrOn[keyOf(model, hash)]; ok {
		return claudemore.Cassette{}, err
	}
	c, ok := f.byKey[keyOf(model, hash)]
	if !ok {
		return claudemore.Cassette{}, claudemore.ErrCassetteNotFound
	}
	return c, nil
}

// jsonCassette builds a non-streaming cassette with the given identity, model,
// and a response body carrying a tool_use + text + thinking + usage.
func jsonCassette(userID, model, hash string) claudemore.Cassette {
	reqBody := fmt.Sprintf(`{"model":%q,"metadata":{"user_id":%q},"messages":[]}`, model, userID)
	respBody := `{"content":[` +
		`{"type":"thinking","thinking":"let me think about this carefully"},` +
		`{"type":"text","text":"hello world"},` +
		`{"type":"tool_use","name":"Bash","id":"toolu_1"}` +
		`],"stop_reason":"tool_use","usage":{"input_tokens":42,"output_tokens":7,"cache_read_input_tokens":100}}`
	return claudemore.Cassette{
		Version:     claudemore.CassetteVersion,
		RequestHash: hash,
		Model:       model,
		Request:     claudemore.CassetteRequest{Method: "POST", Path: "/v1/messages", Body: []byte(reqBody)},
		Response:    claudemore.CassetteResponse{StatusCode: 200, Body: []byte(respBody)},
	}
}

func newCassetteTestServer(t *testing.T, store cassetteReader) *Server {
	t.Helper()
	srv := NewServer(nil, slog.Default(), "", "", "", nil)
	srv.cassetteStore = store
	return srv
}

func getCassettes(t *testing.T, srv *Server) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/cassettes", nil)
	rec := httptest.NewRecorder()
	srv.handleCassettes(rec, req)
	return rec, rec.Body.String()
}

func TestCassettes_DegradedMode_NoStore(t *testing.T) {
	srv := newCassetteTestServer(t, nil)
	// A nil cassetteReader interface value: SetCassetteStore not called.
	srv.cassetteStore = nil
	rec, body := getCassettes(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (degraded, never 5xx), got %d", rec.Code)
	}
	if !strings.Contains(body, "No cassette store is wired") {
		t.Errorf("degraded page should explain the missing store; body:\n%s", body)
	}
}

func TestCassettes_EnumerationError(t *testing.T) {
	store := &fakeCassetteReader{listErr: errors.New("s3 boom")}
	srv := newCassetteTestServer(t, store)
	rec, body := getCassettes(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 even on list error, got %d", rec.Code)
	}
	if !strings.Contains(body, "Could not list cassettes") {
		t.Errorf("expected list-error banner; body:\n%s", body)
	}
}

func TestCassettes_EmptyStore(t *testing.T) {
	store := &fakeCassetteReader{refs: nil, byKey: map[string]claudemore.Cassette{}}
	srv := newCassetteTestServer(t, store)
	rec, body := getCassettes(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(body, "No cassettes recorded yet") {
		t.Errorf("expected empty-state notice; body:\n%s", body)
	}
}

func TestCassettes_RendersWireAudit(t *testing.T) {
	c := jsonCassette("agent-alpha", "claude-opus-4-8", "hashaaa")
	store := &fakeCassetteReader{
		refs:  []claudemore.CassetteRef{{Model: "claude-opus-4-8", Hash: "hashaaa"}},
		byKey: map[string]claudemore.Cassette{keyOf("claude-opus-4-8", "hashaaa"): c},
	}
	srv := newCassetteTestServer(t, store)
	rec, body := getCassettes(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	for _, want := range []string{
		"agent-alpha",             // native session identity
		"claude-opus-4-8",         // model
		"Bash",                    // tool-choice audit chip
		"let me think about this", // reasoning preview
		"tool_use",                // stop reason
		">42<",                    // input tokens rendered
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q; body:\n%s", want, body)
		}
	}
}

func TestCassettes_AnonymousIdentity(t *testing.T) {
	// A cassette whose request carries no metadata.user_id renders the explicit
	// anonymous marker, not a blank cell.
	c := claudemore.Cassette{
		Version:     claudemore.CassetteVersion,
		RequestHash: "hashnometa",
		Model:       "claude-sonnet-5",
		Request:     claudemore.CassetteRequest{Method: "POST", Path: "/v1/messages", Body: []byte(`{"model":"claude-sonnet-5","messages":[]}`)},
		Response:    claudemore.CassetteResponse{StatusCode: 200, Body: []byte(`{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`)},
	}
	store := &fakeCassetteReader{
		refs:  []claudemore.CassetteRef{{Model: "claude-sonnet-5", Hash: "hashnometa"}},
		byKey: map[string]claudemore.Cassette{keyOf("claude-sonnet-5", "hashnometa"): c},
	}
	srv := newCassetteTestServer(t, store)
	_, body := getCassettes(t, srv)
	if !strings.Contains(body, "no metadata.user_id") {
		t.Errorf("expected anonymous-identity marker; body:\n%s", body)
	}
	if !strings.Contains(body, "no reasoning") {
		t.Errorf("expected no-reasoning marker for a pure-text turn; body:\n%s", body)
	}
}

func TestCassettes_TruncationCap(t *testing.T) {
	refs := make([]claudemore.CassetteRef, 0, cassetteAuditMaxRows+5)
	byKey := map[string]claudemore.Cassette{}
	for i := 0; i < cassetteAuditMaxRows+5; i++ {
		hash := fmt.Sprintf("hash%04d", i)
		refs = append(refs, claudemore.CassetteRef{Model: "m", Hash: hash})
		byKey[keyOf("m", hash)] = jsonCassette("agent", "m", hash)
	}
	store := &fakeCassetteReader{refs: refs, byKey: byKey}
	srv := newCassetteTestServer(t, store)
	rec, body := getCassettes(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(body, "Showing the first") {
		t.Errorf("expected truncation banner when store exceeds cap; body head:\n%s", body[:min(600, len(body))])
	}
}

func TestCassettes_FetchFailureCountedNotFatal(t *testing.T) {
	// Two refs; one Get fails. The good one renders; the bad one is flagged as a
	// coverage gap, and the page is still 200.
	good := jsonCassette("agent-good", "m", "good")
	store := &fakeCassetteReader{
		refs: []claudemore.CassetteRef{
			{Model: "m", Hash: "good"},
			{Model: "m", Hash: "bad"},
		},
		byKey:    map[string]claudemore.Cassette{keyOf("m", "good"): good},
		getErrOn: map[string]error{keyOf("m", "bad"): errors.New("read boom")},
	}
	srv := newCassetteTestServer(t, store)
	rec, body := getCassettes(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 despite a fetch failure, got %d", rec.Code)
	}
	if !strings.Contains(body, "agent-good") {
		t.Errorf("good cassette should still render; body:\n%s", body)
	}
	if !strings.Contains(body, "could not be fetched") {
		t.Errorf("expected fetch-failure coverage-gap banner; body:\n%s", body)
	}
}

func TestCassettes_ThinkingPreviewClips(t *testing.T) {
	long := strings.Repeat("x", cassetteThinkingPreviewChars+50)
	row := newCassetteAuditRow(claudemore.WireAudit{
		ThinkingText:  long,
		ThinkingChars: len(long),
	})
	if !row.ThinkingClipped {
		t.Errorf("preview over %d chars should clip", cassetteThinkingPreviewChars)
	}
	if len([]rune(row.ThinkingPreview)) > cassetteThinkingPreviewChars {
		t.Errorf("clipped preview should be ≤ %d runes, got %d", cassetteThinkingPreviewChars, len([]rune(row.ThinkingPreview)))
	}
}

func TestCassettes_RealFSStore_EndToEnd(t *testing.T) {
	// Exercise the real FSCassetteStore round-trip (Put → List → Get → render)
	// so the surface is proven against a concrete store, not only the fake.
	dir := t.TempDir()
	store, err := claudemore.NewFSCassetteStore(dir)
	if err != nil {
		t.Fatalf("NewFSCassetteStore: %v", err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, jsonCassette("agent-fs", "claude-haiku-4-5", "fshash1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	srv := newCassetteTestServer(t, store)
	rec, body := getCassettes(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(body, "agent-fs") || !strings.Contains(body, "claude-haiku-4-5") {
		t.Errorf("FS-store round-trip should render the recorded cassette; body:\n%s", body)
	}
}
