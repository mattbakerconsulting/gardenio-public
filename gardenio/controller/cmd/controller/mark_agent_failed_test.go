package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// markAgentFailed is the non-Kubernetes spawn-door hard-fail helper. Before
// ka-0000006530 it wrote only agent_state=failed, so a MicroVM Launch failure
// surfaced to the operator as a bare "failed" card with no cause — the
// inscrutable "failed spawn error". These tests pin the fix: a non-nil cause
// is persisted as failure_reason (which the bridge renders "Failed: <cause>"),
// and a nil cause still degrades to agent_state=failed alone.

// captureDaemon is a tiny beads-daemon stand-in that answers the GET
// UpdateBeadFields issues to preserve field types, then records the PATCH body.
type captureDaemon struct {
	mu        sync.Mutex
	lastPatch map[string]any
	patched   bool
}

func (d *captureDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// Return a bead with no existing fields so the client's type-preserving
			// merge treats every patched key as a fresh string.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "bd-x", "fields": map[string]any{}})
		case http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			var parsed struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.Unmarshal(body, &parsed)
			var fields map[string]any
			_ = json.Unmarshal(parsed.Fields, &fields)
			d.mu.Lock()
			d.lastPatch = fields
			d.patched = true
			d.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
}

func newCaptureClient(t *testing.T, url string) *beadsapi.Client {
	t.Helper()
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: url})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	return c
}

func TestMarkAgentFailed_RecordsFailureReason(t *testing.T) {
	d := &captureDaemon{}
	srv := d.server(t)
	defer srv.Close()

	daemon := newCaptureClient(t, srv.URL)
	cause := errors.New("lambda_microvm launch failed: quota exceeded")
	markAgentFailed(context.Background(), slog.Default(), daemon, "bd-x", "lambda_microvm", cause)

	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.patched {
		t.Fatal("expected a PATCH to the bead recording the failure")
	}
	if got := d.lastPatch["agent_state"]; got != "failed" {
		t.Errorf("agent_state = %v, want failed", got)
	}
	if got := d.lastPatch["failure_reason"]; got != cause.Error() {
		t.Errorf("failure_reason = %v, want %q — the operator's card needs the real cause, not a bare failed state", got, cause.Error())
	}
}

func TestMarkAgentFailed_NilCause_StateOnly(t *testing.T) {
	d := &captureDaemon{}
	srv := d.server(t)
	defer srv.Close()

	daemon := newCaptureClient(t, srv.URL)
	markAgentFailed(context.Background(), slog.Default(), daemon, "bd-x", "lambda_microvm", nil)

	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.patched {
		t.Fatal("expected a PATCH marking the agent failed even with no cause")
	}
	if got := d.lastPatch["agent_state"]; got != "failed" {
		t.Errorf("agent_state = %v, want failed", got)
	}
	if _, ok := d.lastPatch["failure_reason"]; ok {
		t.Errorf("nil cause must NOT write a failure_reason, got %v", d.lastPatch["failure_reason"])
	}
}

// TestMarkAgentFailed_NilDaemon_NoPanic guards the early-return contract: a nil
// daemon or empty bead id must be a no-op, never a panic (the spawn door calls
// this on error paths where daemon wiring may be absent).
func TestMarkAgentFailed_NilDaemon_NoPanic(t *testing.T) {
	markAgentFailed(context.Background(), slog.Default(), nil, "bd-x", "lambda_microvm", errors.New("boom"))
	markAgentFailed(context.Background(), slog.Default(), &beadsapi.Client{}, "", "lambda_microvm", errors.New("boom"))
}
