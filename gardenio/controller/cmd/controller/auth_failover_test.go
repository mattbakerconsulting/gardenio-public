package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func testFailoverLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// testBead is the in-memory representation held by mockStore.
type testBead struct {
	ID          string
	Title       string
	Type        string
	Status      string
	Labels      []string
	Fields      map[string]any
	Description string
	UpdatedAt   time.Time
}

// mockStore is an in-memory bead store backing the mock daemon HTTP server,
// matching the real daemon's list/GET/PATCH semantics closely enough for the
// UpdateBeadFields and UpdateBeadFieldsTyped paths.
type mockStore struct {
	mu    sync.Mutex
	beads map[string]*testBead
}

func newMockStore(beads ...*testBead) *mockStore {
	s := &mockStore{beads: make(map[string]*testBead, len(beads))}
	for _, b := range beads {
		if b.Fields == nil {
			b.Fields = map[string]any{}
		}
		s.beads[b.ID] = b
	}
	return s
}

func (s *mockStore) get(id string) *testBead {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.beads[id]
	if !ok {
		return nil
	}
	return cloneBead(b)
}

func (s *mockStore) list(typeFilter, labelFilter string) []*testBead {
	s.mu.Lock()
	defer s.mu.Unlock()
	types := splitCSV(typeFilter)
	labels := splitCSV(labelFilter)
	var out []*testBead
	for _, b := range s.beads {
		if len(types) > 0 && !containsAny(types, b.Type) {
			continue
		}
		if len(labels) > 0 && !allLabelsMatch(b.Labels, labels) {
			continue
		}
		out = append(out, cloneBead(b))
	}
	return out
}

func (s *mockStore) patchFields(id string, merged map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.beads[id]
	if !ok {
		return
	}
	b.Fields = merged
}

func (s *mockStore) patchDescription(id, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.beads[id]; ok {
		b.Description = description
	}
}

func (s *mockStore) description(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.beads[id]; ok {
		return b.Description
	}
	return ""
}

func cloneBead(b *testBead) *testBead {
	fields := make(map[string]any, len(b.Fields))
	maps.Copy(fields, b.Fields)
	labels := append([]string(nil), b.Labels...)
	return &testBead{
		ID: b.ID, Title: b.Title, Type: b.Type, Status: b.Status,
		Labels: labels, Fields: fields, Description: b.Description,
		UpdatedAt: b.UpdatedAt,
	}
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containsAny(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

func allLabelsMatch(beadLabels, required []string) bool {
	for _, r := range required {
		if !slices.Contains(beadLabels, r) {
			return false
		}
	}
	return true
}

// mockDaemonServer serves the daemon HTTP endpoints used by authFailover.
// It supports GET /v1/beads, GET /v1/beads/{id}, and PATCH /v1/beads/{id}.
func mockDaemonServer(t *testing.T, store *mockStore) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			q, _ := url.ParseQuery(r.URL.RawQuery)
			beads := store.list(q.Get("type"), q.Get("labels"))
			writeJSON(w, toListResponse(beads))

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			b := store.get(id)
			if b == nil {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, toBeadJSON(b))

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Fields      json.RawMessage `json:"fields"`
				Description *string         `json:"description,omitempty"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if len(req.Fields) > 0 {
				var merged map[string]any
				if err := json.Unmarshal(req.Fields, &merged); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				store.patchFields(id, merged)
			}
			if req.Description != nil {
				store.patchDescription(id, *req.Description)
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{}"))

		default:
			http.NotFound(w, r)
		}
	}))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func toBeadJSON(b *testBead) map[string]any {
	fields, _ := json.Marshal(b.Fields)
	out := map[string]any{
		"id":          b.ID,
		"title":       b.Title,
		"type":        b.Type,
		"status":      b.Status,
		"labels":      b.Labels,
		"fields":      json.RawMessage(fields),
		"description": b.Description,
	}
	if !b.UpdatedAt.IsZero() {
		out["updated_at"] = b.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func toListResponse(beads []*testBead) map[string]any {
	out := make([]map[string]any, 0, len(beads))
	for _, b := range beads {
		out = append(out, toBeadJSON(b))
	}
	return map[string]any{"beads": out, "total": len(out)}
}

func newClient(t *testing.T, srv *httptest.Server) *beadsapi.Client {
	t.Helper()
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// account is a concise constructor for testBead entries of type=account.
func account(id, title, project string, fields map[string]any) *testBead {
	if fields == nil {
		fields = map[string]any{}
	}
	return &testBead{
		ID: id, Title: title, Type: "account", Status: "open",
		Labels: []string{"project:" + project},
		Fields: fields,
	}
}

func TestFallbackTier(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   int
	}{
		{"active allowed", map[string]string{"status": "active", "throttle_status": "allowed"}, 0},
		{"active empty throttle", map[string]string{"status": "active"}, 0},
		{"exhausted allowed", map[string]string{"status": "exhausted", "throttle_status": "allowed"}, 1},
		{"exhausted allowed_warning", map[string]string{"status": "exhausted", "throttle_status": "allowed_warning"}, 2},
		{"exhausted rejected", map[string]string{"status": "exhausted", "throttle_status": "rejected"}, -1},
		{"exhausted empty", map[string]string{"status": "exhausted"}, -1},
		{"disabled allowed", map[string]string{"status": "disabled", "throttle_status": "allowed"}, -1},
		{"disabled empty throttle", map[string]string{"status": "disabled"}, -1},
		{"disabled rejected", map[string]string{"status": "disabled", "throttle_status": "rejected"}, -1},
		// Probe-gate (ka-0000002085): an account whose read-only probe returned
		// 401/403 (probe_auth_status="rejected") is never a usable failover
		// target, even when the rate-limit axis reads perfectly healthy.
		{"active allowed but probe auth rejected", map[string]string{"status": "active", "throttle_status": "allowed", "probe_auth_status": "rejected"}, -1},
		{"active allowed probe auth ok", map[string]string{"status": "active", "throttle_status": "allowed", "probe_auth_status": "ok"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fallbackTier(&beadsapi.BeadDetail{Fields: c.fields})
			if got != c.want {
				t.Errorf("fallbackTier(%v) = %d, want %d", c.fields, got, c.want)
			}
		})
	}
}

func TestThrottleAllows(t *testing.T) {
	if throttleAllows("rejected") {
		t.Error("rejected should not allow")
	}
	if throttleAllows("") {
		t.Error("empty should not allow")
	}
	if !throttleAllows("allowed") {
		t.Error("allowed should allow")
	}
	if !throttleAllows("allowed_warning") {
		t.Error("allowed_warning should allow")
	}
}

func TestThrottleRecovered(t *testing.T) {
	if !throttleRecovered("allowed") {
		t.Error("allowed should be recovered")
	}
	if throttleRecovered("allowed_warning") {
		t.Error("allowed_warning should NOT be recovered")
	}
	if throttleRecovered("rejected") {
		t.Error("rejected should NOT be recovered")
	}
	if throttleRecovered("") {
		t.Error("empty should NOT be recovered")
	}
}

func TestProjectPrimary(t *testing.T) {
	// A → B → C chain: A is head (not referenced by any fallback).
	accts := []*beadsapi.BeadDetail{
		{Title: "acc-a", Fields: map[string]string{"fallback": "acc-b"}},
		{Title: "acc-b", Fields: map[string]string{"fallback": "acc-c"}},
		{Title: "acc-c", Fields: map[string]string{}},
	}
	if got := projectPrimary(accts); got != "acc-a" {
		t.Errorf("projectPrimary = %q, want acc-a", got)
	}

	// Alphabetical tiebreak among unreferenced heads.
	accts = []*beadsapi.BeadDetail{
		{Title: "zebra", Fields: map[string]string{}},
		{Title: "apple", Fields: map[string]string{}},
	}
	if got := projectPrimary(accts); got != "apple" {
		t.Errorf("projectPrimary alphabetical tiebreak = %q, want apple", got)
	}

	// Explicit primary marker wins over alphabetical / fallback-graph head.
	accts = []*beadsapi.BeadDetail{
		{Title: "acc-a", Fields: map[string]string{"fallback": "acc-b"}},
		{Title: "acc-b", Fields: map[string]string{"fallback": "acc-c"}},
		{Title: "acc-c", Fields: map[string]string{"primary": "true"}},
	}
	if got := projectPrimary(accts); got != "acc-c" {
		t.Errorf("projectPrimary with marker = %q, want acc-c", got)
	}

	// Multiple marked accounts: alphabetical-first among marked wins.
	accts = []*beadsapi.BeadDetail{
		{Title: "zebra", Fields: map[string]string{"primary": "true"}},
		{Title: "apple", Fields: map[string]string{"primary": "true"}},
		{Title: "banana", Fields: map[string]string{}},
	}
	if got := projectPrimary(accts); got != "apple" {
		t.Errorf("projectPrimary multi-marked tiebreak = %q, want apple", got)
	}

	// ka-0000002933: a primary marker stranded on an exhausted account is
	// ignored — selection falls through to the healthy fallback-graph head so
	// failover/switchback do not treat a cooked account as the project home.
	accts = []*beadsapi.BeadDetail{
		{Title: "healthy", Fields: map[string]string{"fallback": "cooked", "status": "active"}},
		{Title: "cooked", Fields: map[string]string{"primary": "true", "status": "exhausted"}},
	}
	if got := projectPrimary(accts); got != "healthy" {
		t.Errorf("projectPrimary with exhausted marker = %q, want healthy", got)
	}

	// A healthy marked account is still honored even when an exhausted account
	// in the pool also (stale) carries the marker.
	accts = []*beadsapi.BeadDetail{
		{Title: "fresh", Fields: map[string]string{"primary": "true", "status": "active"}},
		{Title: "stale", Fields: map[string]string{"primary": "true", "status": "exhausted"}},
	}
	if got := projectPrimary(accts); got != "fresh" {
		t.Errorf("projectPrimary prefers healthy marked = %q, want fresh", got)
	}

	// "disabled" is likewise non-active and skipped.
	accts = []*beadsapi.BeadDetail{
		{Title: "alpha", Fields: map[string]string{"fallback": "off", "status": "active"}},
		{Title: "off", Fields: map[string]string{"primary": "true", "status": "disabled"}},
	}
	if got := projectPrimary(accts); got != "alpha" {
		t.Errorf("projectPrimary with disabled marker = %q, want alpha", got)
	}
}

// TestAuthFailover_SelectsNextPrimary verifies the core failover flow:
// Handle walks the fallback chain, updates runtime state to point at the
// healthy fallback, and marks the exhausted primary with cooldown metadata.
func TestAuthFailover_SelectsNextPrimary(t *testing.T) {
	store := newMockStore(
		// Primary is genuinely at its rolling budget (rejected + u7d>=0.99), so
		// the failover HARD-exhausts it.
		account("gd-p", "primary", "myproj", map[string]any{
			"provider": "anthropic", "fallback": "backup", "status": "active",
			"throttle_status": "rejected", "utilization_7d": "1.00",
		}),
		account("gd-b", "backup", "myproj", map[string]any{
			"provider": "anthropic", "status": "active",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	if err := f.Handle(context.Background(), "myproj", "gd-agent1"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	state, ok := f.runtime.Get("myproj")
	if !ok {
		t.Fatal("expected runtime state for myproj")
	}
	if state.Primary != "primary" {
		t.Errorf("Primary = %q, want primary", state.Primary)
	}
	if state.Current != "backup" {
		t.Errorf("Current = %q, want backup", state.Current)
	}
	if state.FailedOverAt.IsZero() {
		t.Error("FailedOverAt should be set")
	}
	if state.FailoverReason != "rate_limited" {
		t.Errorf("FailoverReason = %q, want rate_limited", state.FailoverReason)
	}

	// Exhausted primary bead should carry cooldown metadata.
	primary := store.get("gd-p")
	if primary.Fields["status"] != "exhausted" {
		t.Errorf("primary.status = %v, want exhausted", primary.Fields["status"])
	}
	if primary.Fields["cooldown_until"] == "" {
		t.Error("primary.cooldown_until should be set")
	}
	if primary.Fields["exhausted_at"] == "" {
		t.Error("primary.exhausted_at should be set")
	}
}

// TestAuthFailover_CooldownRespected verifies the per-project cooldown prevents
// a second failover within the cooldown window.
func TestAuthFailover_CooldownRespected(t *testing.T) {
	store := newMockStore(
		account("gd-p", "primary", "myproj", map[string]any{"fallback": "backup"}),
		account("gd-b", "backup", "myproj", map[string]any{}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	// Large cooldown so the second call is always rejected.
	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), time.Hour, 0)

	if err := f.Handle(context.Background(), "myproj", ""); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	firstState, _ := f.runtime.Get("myproj")

	// Second call within cooldown → no-op (state unchanged).
	if err := f.Handle(context.Background(), "myproj", ""); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	secondState, _ := f.runtime.Get("myproj")
	if !firstState.FailedOverAt.Equal(secondState.FailedOverAt) {
		t.Error("cooldown did not prevent second failover")
	}
}

// TestAuthFailover_RestartRebuild verifies that RebuildState reconstructs
// runtime failover state from account bead cooldown metadata after a restart.
func TestAuthFailover_RestartRebuild(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	exhaustedAt := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)

	store := newMockStore(
		account("gd-p", "primary", "myproj", map[string]any{
			"fallback":       "backup",
			"status":         "exhausted",
			"cooldown_until": future,
			"exhausted_at":   exhaustedAt,
		}),
		account("gd-b", "backup", "myproj", map[string]any{
			"status": "active",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	if err := f.RebuildState(context.Background()); err != nil {
		t.Fatalf("RebuildState: %v", err)
	}

	state, ok := f.runtime.Get("myproj")
	if !ok {
		t.Fatal("expected rebuilt state for myproj")
	}
	if state.Primary != "primary" {
		t.Errorf("Primary = %q, want primary", state.Primary)
	}
	if state.Current != "backup" {
		t.Errorf("Current = %q, want backup", state.Current)
	}
	if state.FailoverReason != "rebuilt_on_restart" {
		t.Errorf("FailoverReason = %q, want rebuilt_on_restart", state.FailoverReason)
	}
	if state.FailedOverAt.IsZero() {
		t.Error("FailedOverAt should be parsed from exhausted_at")
	}
}

// TestAuthFailover_RestartNoRebuildWhenPrimaryHealthy confirms RebuildState
// skips projects whose primary is NOT in active cooldown.
func TestAuthFailover_RestartNoRebuildWhenPrimaryHealthy(t *testing.T) {
	store := newMockStore(
		account("gd-p", "primary", "myproj", map[string]any{
			"fallback": "backup",
			"status":   "active",
		}),
		account("gd-b", "backup", "myproj", map[string]any{}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	if err := f.RebuildState(context.Background()); err != nil {
		t.Fatalf("RebuildState: %v", err)
	}
	if _, ok := f.runtime.Get("myproj"); ok {
		t.Error("RebuildState should not create state for a healthy project")
	}
}

// TestAuthFailover_RestartExpiredCooldownSkipped verifies cooldown_until in the
// past is treated as no active cooldown (stale state from a long-dead process).
func TestAuthFailover_RestartExpiredCooldownSkipped(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	store := newMockStore(
		account("gd-p", "primary", "myproj", map[string]any{
			"fallback":       "backup",
			"status":         "exhausted",
			"cooldown_until": past,
		}),
		account("gd-b", "backup", "myproj", map[string]any{}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	if err := f.RebuildState(context.Background()); err != nil {
		t.Fatalf("RebuildState: %v", err)
	}
	if _, ok := f.runtime.Get("myproj"); ok {
		t.Error("expired cooldown should not produce runtime state")
	}
}

func TestAuthFailover_NoFallbackDoesNothing(t *testing.T) {
	store := newMockStore(
		account("gd-p", "primary", "myproj", map[string]any{
			"status": "active", // no fallback field
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	if err := f.Handle(context.Background(), "myproj", ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if state, ok := f.runtime.Get("myproj"); ok && state.inFailover() {
		t.Errorf("expected no failover state, got %+v", state)
	}
}

func TestAuthFailover_EmptyProjectSkips(t *testing.T) {
	store := newMockStore()
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	if err := f.Handle(context.Background(), "", ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestAuthFailover_ChainSkipsExhaustedDownstream(t *testing.T) {
	store := newMockStore(
		account("gd-p", "primary", "myproj", map[string]any{"fallback": "mid"}),
		account("gd-m", "mid", "myproj", map[string]any{
			"fallback":        "tail",
			"status":          "exhausted",
			"throttle_status": "rejected",
		}),
		account("gd-t", "tail", "myproj", map[string]any{"status": "active"}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	if err := f.Handle(context.Background(), "myproj", ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	state, ok := f.runtime.Get("myproj")
	if !ok || state.Current != "tail" {
		t.Errorf("expected Current=tail, got %+v", state)
	}
}

// --- ka-0000003305: gate hard exhaustion on utilization, not raw 429 ----------

// TestShouldHardExhaust exercises the exhaustion gate directly: only a rejected
// account whose 5h OR 7d utilization is at/above nearLimitUtilization is HARD-
// exhausted. A 429 with utilization headroom (spend-cap / transient throttle)
// must NOT qualify.
func TestShouldHardExhaust(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   bool
	}{
		{"rejected + 7d maxed", map[string]string{"throttle_status": "rejected", "utilization_7d": "1.00"}, true},
		{"rejected + 5h maxed", map[string]string{"throttle_status": "rejected", "utilization_5h": "0.99"}, true},
		{"rejected + both low (spend-cap/transient)", map[string]string{"throttle_status": "rejected", "utilization_5h": "0.62", "utilization_7d": "0.85"}, false},
		{"rejected + no utilization fields", map[string]string{"throttle_status": "rejected"}, false},
		{"not rejected even at max util", map[string]string{"throttle_status": "allowed_warning", "utilization_7d": "1.00"}, false},
		{"empty throttle", map[string]string{"utilization_7d": "1.00"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bead := &beadsapi.BeadDetail{Fields: tc.fields}
			if got := shouldHardExhaust(bead); got != tc.want {
				t.Errorf("shouldHardExhaust(%v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}

// TestAuthFailover_DeprioritizesOn429WithHeadroom verifies the incident fix: an
// account that 429s while its subscription still has utilization headroom
// (matthewbaker-acme u7d 0.62 in the 2026-06-17 incident) is rotated away
// from the live session but is NOT marked status=exhausted and gets no cooldown.
// Instead it stays active/assignable with a deprioritized_until stamp so it
// ranks last for a short window rather than being pulled for ~24h.
func TestAuthFailover_DeprioritizesOn429WithHeadroom(t *testing.T) {
	store := newMockStore(
		account("gd-p", "primary", "myproj", map[string]any{
			"provider": "anthropic", "fallback": "backup", "status": "active",
			// 429 while utilization still has plenty of headroom.
			"throttle_status": "rejected", "utilization_5h": "0.40", "utilization_7d": "0.62",
		}),
		account("gd-b", "backup", "myproj", map[string]any{
			"provider": "anthropic", "status": "active",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 24*time.Hour)
	if err := f.Handle(context.Background(), "myproj", "gd-agent1"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// The session still rotates onto the backup.
	state, ok := f.runtime.Get("myproj")
	if !ok || state.Current != "backup" {
		t.Fatalf("expected rotation onto backup, got %+v (ok=%v)", state, ok)
	}

	p := store.get("gd-p").Fields
	// NOT hard-exhausted: no status=exhausted, no cooldown.
	if p["status"] == "exhausted" {
		t.Errorf("account with utilization headroom must NOT be exhausted, got status=%v", p["status"])
	}
	if p["cooldown_until"] != nil && p["cooldown_until"] != "" {
		t.Errorf("deprioritized account must not get a cooldown_until, got %v", p["cooldown_until"])
	}
	// But deprioritized for a window.
	if p["deprioritized_until"] == nil || p["deprioritized_until"] == "" {
		t.Errorf("deprioritized account must carry deprioritized_until, got %v", p["deprioritized_until"])
	}
}

// TestAuthFailover_HardExhaustsWhenAtBudget verifies the complementary case: a
// 429 with utilization AT the rolling budget (>= nearLimitUtilization) still
// hard-exhausts with a cooldown, exactly as before.
func TestAuthFailover_HardExhaustsWhenAtBudget(t *testing.T) {
	store := newMockStore(
		account("gd-p", "primary", "myproj", map[string]any{
			"provider": "anthropic", "fallback": "backup", "status": "active",
			"throttle_status": "rejected", "utilization_7d": "1.00",
		}),
		account("gd-b", "backup", "myproj", map[string]any{
			"provider": "anthropic", "status": "active",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 24*time.Hour)
	if err := f.Handle(context.Background(), "myproj", "gd-agent1"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	p := store.get("gd-p").Fields
	if p["status"] != "exhausted" {
		t.Errorf("at-budget account must be exhausted, got status=%v", p["status"])
	}
	if p["cooldown_until"] == nil || p["cooldown_until"] == "" {
		t.Errorf("exhausted account must carry cooldown_until, got %v", p["cooldown_until"])
	}
	if p["deprioritized_until"] != nil && p["deprioritized_until"] != "" {
		t.Errorf("hard-exhausted account should not also be deprioritized, got %v", p["deprioritized_until"])
	}
}

// TestFirstHealthyByUtilization_PrefersLeastUtilized verifies account selection
// picks the LOWEST-utilization healthy account rather than the alphabetically
// first one — the sub-optimality where new pods landed on gardenio3 (u7d 0.98)
// over matthewcbaker0122 (u7d 0.04) purely on the leading letter (ka-0000003305).
func TestFirstHealthyByUtilization_PrefersLeastUtilized(t *testing.T) {
	now := time.Now()
	accts := []*beadsapi.BeadDetail{
		{Title: "gardenio3", Fields: map[string]string{"utilization_7d": "0.98"}},
		{Title: "matthewcbaker0122", Fields: map[string]string{"utilization_7d": "0.04"}},
		{Title: "midrange", Fields: map[string]string{"utilization_7d": "0.50"}},
	}
	got := firstHealthyByUtilization(accts, now, "some-primary")
	if got != "matthewcbaker0122" {
		t.Errorf("expected least-utilized matthewcbaker0122, got %q (alphabetical order not replaced)", got)
	}
}

// TestFirstHealthyByUtilization_DeprioritizedSortsLast verifies a deprioritized
// account ranks last even when its utilization is the lowest, and is skipped
// while a non-deprioritized peer is available.
func TestFirstHealthyByUtilization_DeprioritizedSortsLast(t *testing.T) {
	now := time.Now()
	future := now.Add(10 * time.Minute).Format(time.RFC3339)
	accts := []*beadsapi.BeadDetail{
		// Lowest utilization but deprioritized — should NOT be picked first.
		{Title: "aaa-deprio", Fields: map[string]string{"utilization_7d": "0.01", "deprioritized_until": future}},
		{Title: "bbb-healthy", Fields: map[string]string{"utilization_7d": "0.30"}},
	}
	got := firstHealthyByUtilization(accts, now, "primary")
	if got != "bbb-healthy" {
		t.Errorf("expected non-deprioritized bbb-healthy, got %q", got)
	}
}

// TestFirstHealthyByUtilization_SkipsCooldownAndSkip verifies accounts in active
// cooldown and the skip (primary) name are excluded from selection.
func TestFirstHealthyByUtilization_SkipsCooldownAndSkip(t *testing.T) {
	now := time.Now()
	future := now.Add(2 * time.Hour).Format(time.RFC3339)
	accts := []*beadsapi.BeadDetail{
		{Title: "primary", Fields: map[string]string{"utilization_7d": "0.01"}},
		{Title: "in-cooldown", Fields: map[string]string{"status": "exhausted", "cooldown_until": future, "utilization_7d": "0.02"}},
		{Title: "healthy", Fields: map[string]string{"utilization_7d": "0.70"}},
	}
	got := firstHealthyByUtilization(accts, now, "primary")
	if got != "healthy" {
		t.Errorf("expected healthy (primary skipped, in-cooldown excluded), got %q", got)
	}
	if got := firstHealthyByUtilization(nil, now, "primary"); got != "" {
		t.Errorf("empty pool should return \"\", got %q", got)
	}
}

func TestAuthFailover_PrefersActiveOverRecoveredExhausted(t *testing.T) {
	store := newMockStore(
		account("gd-p", "primary", "myproj", map[string]any{"fallback": "recovered"}),
		account("gd-r", "recovered", "myproj", map[string]any{
			"fallback":        "active",
			"status":          "exhausted",
			"throttle_status": "allowed",
		}),
		account("gd-a", "active", "myproj", map[string]any{
			"status":          "active",
			"throttle_status": "allowed",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	if err := f.Handle(context.Background(), "myproj", ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	state, _ := f.runtime.Get("myproj")
	if state.Current != "active" {
		t.Errorf("expected Current=active (tier 0 over tier 1), got %q", state.Current)
	}
}

// TestAuthFailover_ChainSkipsDisabledDownstream verifies that fallback
// selection treats status=disabled accounts as ineligible and continues
// down the chain. Regression for ka-0000007988: prior to the fix,
// disabled accounts were tier 0 (treated as healthy) so failover would
// land on a disabled credential.
func TestAuthFailover_ChainSkipsDisabledDownstream(t *testing.T) {
	store := newMockStore(
		account("gd-p", "primary", "myproj", map[string]any{"fallback": "mid"}),
		account("gd-m", "mid", "myproj", map[string]any{
			"fallback":        "tail",
			"status":          "disabled",
			"throttle_status": "allowed",
		}),
		account("gd-t", "tail", "myproj", map[string]any{
			"status":          "active",
			"throttle_status": "allowed",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	if err := f.Handle(context.Background(), "myproj", ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	state, ok := f.runtime.Get("myproj")
	if !ok || state.Current != "tail" {
		t.Errorf("expected Current=tail (mid is disabled, must be skipped), got %+v", state)
	}
}

// ── SweepStaleExhausted ─────────────────────────────────────────────────────
//
// Repro for ka-0000002663: after a controller restart the in-memory failover
// runtime map is empty. The old switchback-driven stale-exhausted clear path
// (abolished in ka-0000008240) only ran off that map, so accounts marked
// exhausted by a previous controller process stayed stuck `status=exhausted`
// forever, even when their probe was showing allowed. SweepStaleExhausted is
// the runtime-map-independent recovery path.

func TestSweepStaleExhausted_ClearsRecoveredAccount(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	store := newMockStore(
		account("gd-stuck", "stuck", "myproj", map[string]any{
			"status":           "exhausted",
			"throttle_status":  "allowed",
			"probe_checked_at": now,
			"cooldown_until":   "2026-04-25T00:00:00Z",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.SweepStaleExhausted(context.Background())

	got := store.get("gd-stuck")
	if got.Fields["status"] != "active" {
		t.Errorf("status = %q, want %q", got.Fields["status"], "active")
	}
	if got.Fields["cooldown_until"] != "" {
		t.Errorf("cooldown_until = %q, want empty", got.Fields["cooldown_until"])
	}
}

func TestSweepStaleExhausted_LeavesActiveAlone(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	store := newMockStore(
		account("gd-ok", "ok", "myproj", map[string]any{
			"status":           "active",
			"throttle_status":  "allowed",
			"probe_checked_at": now,
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.SweepStaleExhausted(context.Background())

	got := store.get("gd-ok")
	if got.Fields["status"] != "active" {
		t.Errorf("status = %q, want %q (unchanged)", got.Fields["status"], "active")
	}
}

func TestSweepStaleExhausted_RespectsRejectedThrottle(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	store := newMockStore(
		account("gd-still-rl", "still-ratelimited", "myproj", map[string]any{
			"status":           "exhausted",
			"throttle_status":  "rejected",
			"probe_checked_at": now,
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.SweepStaleExhausted(context.Background())

	got := store.get("gd-still-rl")
	if got.Fields["status"] != "exhausted" {
		t.Errorf("status = %q, want %q (still rate-limited)", got.Fields["status"], "exhausted")
	}
}

// ka-0000003350: a SUBSCRIPTION account (billing_type unset → subscription; the
// whole fleet) whose rate-limit windows recovered (throttle_status="allowed")
// but which carries overage_disabled_reason="out_of_credits" MUST clear back to
// active. For a subscription account out_of_credits is a benign permanent
// property (ka-0000004929); holding it here on a transient 429 stranded the sole
// healthy primary and starved the fleet (the incident this fix closes).
func TestSweepStaleExhausted_ClearsSubscriptionOutOfCreditsAccount(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	store := newMockStore(
		account("gd-sub", "sub-out-of-credits", "myproj", map[string]any{
			"status":                  "exhausted",
			"throttle_status":         "allowed",
			"overage_status":          "rejected",
			"overage_disabled_reason": "out_of_credits",
			"probe_checked_at":        now,
			"cooldown_until":          "2026-07-06T06:00:00Z",
			// billing_type intentionally unset — defaults to subscription.
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.SweepStaleExhausted(context.Background())

	got := store.get("gd-sub")
	if got.Fields["status"] != "active" {
		t.Errorf("status = %q, want %q (subscription out_of_credits is benign, must clear)", got.Fields["status"], "active")
	}
	if got.Fields["cooldown_until"] != "" {
		t.Errorf("cooldown_until = %q, want empty", got.Fields["cooldown_until"])
	}
}

// ka-0000003350: a METERED account (explicit billing_type=metered) whose
// rate-limit windows recovered but which is OUT OF BILLING CREDITS
// (overage_status="rejected", reason="out_of_credits") must stay exhausted. On a
// genuine pay-as-you-go account real inference 429s on billing, so clearing here
// flips it back to active until the next 429 re-exhausts it — the
// active↔exhausted oscillation (ka-0000008164). The billing-type gate keeps the
// hold for these.
func TestSweepStaleExhausted_HoldsMeteredOutOfCreditsAccount(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	store := newMockStore(
		account("gd-broke", "metered-out-of-credits", "myproj", map[string]any{
			"status":                  "exhausted",
			"throttle_status":         "allowed",
			"overage_status":          "rejected",
			"overage_disabled_reason": "out_of_credits",
			"billing_type":            "metered",
			"probe_checked_at":        now,
			"cooldown_until":          "2026-07-06T06:00:00Z",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.SweepStaleExhausted(context.Background())

	got := store.get("gd-broke")
	if got.Fields["status"] != "exhausted" {
		t.Errorf("status = %q, want %q (metered out of billing credits, must stay parked)", got.Fields["status"], "exhausted")
	}
}

// ka-0000008164: org_spend_cap_reached is benign on healthy subscription
// accounts (ka-0000004929) — such an account must still clear normally once its
// rate-limit window recovers, or the guard would freeze the healthy fleet
// primary that permanently carries this overage reason.
func TestSweepStaleExhausted_ClearsOrgSpendCapAccount(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	store := newMockStore(
		account("gd-spendcap", "spend-cap", "myproj", map[string]any{
			"status":                  "exhausted",
			"throttle_status":         "allowed",
			"overage_status":          "rejected",
			"overage_disabled_reason": "org_spend_cap_reached",
			"probe_checked_at":        now,
			"cooldown_until":          "2026-07-06T06:00:00Z",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.SweepStaleExhausted(context.Background())

	got := store.get("gd-spendcap")
	if got.Fields["status"] != "active" {
		t.Errorf("status = %q, want %q (org_spend_cap_reached is benign, must clear)", got.Fields["status"], "active")
	}
	if got.Fields["cooldown_until"] != "" {
		t.Errorf("cooldown_until = %q, want empty", got.Fields["cooldown_until"])
	}
}

func TestSweepStaleExhausted_RequiresRecentProbe(t *testing.T) {
	stale := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	store := newMockStore(
		account("gd-stale-probe", "stale-probe", "myproj", map[string]any{
			"status":           "exhausted",
			"throttle_status":  "allowed",
			"probe_checked_at": stale,
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.SweepStaleExhausted(context.Background())

	got := store.get("gd-stale-probe")
	if got.Fields["status"] != "exhausted" {
		t.Errorf("status = %q, want %q (probe is stale, must not clear blindly)", got.Fields["status"], "exhausted")
	}
}

func TestSweepStaleExhausted_RuntimeMapEmpty(t *testing.T) {
	// The bug repro: runtime map empty (post-restart) AND account is stuck
	// exhausted. SweepStaleExhausted is runtime-map-independent, so it must still
	// recover the account even with no runtime entries (the switchback path that
	// used to depend on the runtime map is abolished — ka-0000008240).
	now := time.Now().UTC().Format(time.RFC3339)
	store := newMockStore(
		account("gd-stuck-after-restart", "stuck-post-restart", "myproj", map[string]any{
			"status":           "exhausted",
			"throttle_status":  "allowed",
			"probe_checked_at": now,
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	if got := f.runtime.state; len(got) != 0 {
		t.Fatalf("expected empty runtime map, got %d entries", len(got))
	}
	f.SweepStaleExhausted(context.Background())
	if got := store.get("gd-stuck-after-restart"); got.Fields["status"] != "active" {
		t.Errorf("status = %q, want %q (sweep should run regardless of runtime map)", got.Fields["status"], "active")
	}
}

// ── SweepStaleExhausted: cooldown_until-elapsed backstop (Phase 2) ────────────
//
// With switchback abolished (ka-0000008240), the sweep is the sole recovery
// path. To preserve the old switchback maxFailoverDur safety net in a
// primary-free form it gains a second recovery clause: an exhausted account
// recovers once its cooldown_until has elapsed, even without a fresh recovering
// probe. This is what recovers a Rust-coopmux-marked account (which sets
// cooldown_until but may never be probed) and any account behind a stale/down
// probe — the high-severity probe-liveness risk the epic flags.

// TestSweepStaleExhausted_ClearsCooldownElapsedRustStyleBead is the headline
// regression: a Rust-marked bead (status=exhausted, cooldown_until in the past,
// probe STALE/allowed) must recover via the cooldown-elapsed clause. Before this
// change the stale probe alone would have kept it parked forever.
func TestSweepStaleExhausted_ClearsCooldownElapsedRustStyleBead(t *testing.T) {
	staleProbe := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	pastCooldown := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	store := newMockStore(
		account("gd-rust", "rust-marked", "myproj", map[string]any{
			"status":           "exhausted",
			"throttle_status":  "allowed", // token-budget looks fine but probe is stale
			"probe_checked_at": staleProbe,
			"cooldown_until":   pastCooldown,
			"exhausted_at":     pastCooldown,
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.SweepStaleExhausted(context.Background())

	got := store.get("gd-rust")
	if got.Fields["status"] != "active" {
		t.Errorf("status = %q, want active (cooldown_until elapsed must recover a Rust-marked/stale-probe account)", got.Fields["status"])
	}
	if got.Fields["cooldown_until"] != "" {
		t.Errorf("cooldown_until = %q, want empty after recovery", got.Fields["cooldown_until"])
	}
}

// TestSweepStaleExhausted_ClearsCooldownElapsedNoProbe covers an account that was
// never probed at all (no probe_checked_at) but whose cooldown_until has elapsed:
// the timer backstop alone must recover it.
func TestSweepStaleExhausted_ClearsCooldownElapsedNoProbe(t *testing.T) {
	pastCooldown := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	store := newMockStore(
		account("gd-unprobed", "unprobed", "myproj", map[string]any{
			"status":         "exhausted",
			"cooldown_until": pastCooldown,
			// no throttle_status, no probe_checked_at
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.SweepStaleExhausted(context.Background())

	if got := store.get("gd-unprobed"); got.Fields["status"] != "active" {
		t.Errorf("status = %q, want active (cooldown elapsed recovers even an unprobed account)", got.Fields["status"])
	}
}

// TestSweepStaleExhausted_HoldsCooldownNotElapsed verifies the backstop does NOT
// fire early: an account whose cooldown_until is still in the future, with a
// stale probe, stays parked (neither recovery clause is satisfied).
func TestSweepStaleExhausted_HoldsCooldownNotElapsed(t *testing.T) {
	staleProbe := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	futureCooldown := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	store := newMockStore(
		account("gd-parked", "parked", "myproj", map[string]any{
			"status":           "exhausted",
			"throttle_status":  "allowed",
			"probe_checked_at": staleProbe,
			"cooldown_until":   futureCooldown,
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.SweepStaleExhausted(context.Background())

	if got := store.get("gd-parked"); got.Fields["status"] != "exhausted" {
		t.Errorf("status = %q, want exhausted (cooldown not elapsed + stale probe must stay parked)", got.Fields["status"])
	}
}

// TestSweepStaleExhausted_MeteredOutOfCreditsWinsOverCooldownElapsed verifies the
// out-of-credits hold OUTRANKS the cooldown-elapsed backstop: a metered account
// out of billing credits stays parked even after its cooldown_until has passed,
// because its blocker is billing (real inference still 429s), not a rate-limit
// timer.
func TestSweepStaleExhausted_MeteredOutOfCreditsWinsOverCooldownElapsed(t *testing.T) {
	pastCooldown := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	store := newMockStore(
		account("gd-broke", "metered-broke", "myproj", map[string]any{
			"status":                  "exhausted",
			"throttle_status":         "allowed",
			"overage_status":          "rejected",
			"overage_disabled_reason": "out_of_credits",
			"billing_type":            "metered",
			"cooldown_until":          pastCooldown,
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.SweepStaleExhausted(context.Background())

	if got := store.get("gd-broke"); got.Fields["status"] != "exhausted" {
		t.Errorf("status = %q, want exhausted (metered out-of-credits must outrank cooldown-elapsed)", got.Fields["status"])
	}
}
