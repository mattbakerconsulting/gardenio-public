package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestBuildWebhookDedupKey_GitHub pins the GitHub-specific keys
// (gd-ExCBfoRCTA): workflow_run / check_suite payloads have no top-level id,
// so without them every event collapsed to "<token>:completed".
func TestBuildWebhookDedupKey_GitHub(t *testing.T) {
	run := func(id, attempt float64, action string) map[string]any {
		return map[string]any{
			"action":       action,
			"workflow_run": map[string]any{"id": id, "run_attempt": attempt, "head_sha": "abc"},
		}
	}
	tests := []struct {
		name    string
		payload map[string]any
		headers map[string]string
		want    string
	}{
		{
			"workflow_run keyed on id, attempt and action",
			run(12345678901, 1, "completed"),
			map[string]string{"x-github-delivery": "guid-1"},
			"tok:workflow_run:12345678901:1:completed",
		},
		{
			"workflow_run re-run is a distinct attempt",
			run(12345678901, 2, "completed"),
			nil,
			"tok:workflow_run:12345678901:2:completed",
		},
		{
			"workflow_run requested does not share the completed key",
			run(12345678901, 1, "requested"),
			nil,
			"tok:workflow_run:12345678901:1:requested",
		},
		{
			"workflow_run without run_attempt defaults to attempt 1",
			map[string]any{"action": "completed", "workflow_run": map[string]any{"id": float64(7)}},
			nil,
			"tok:workflow_run:7:1:completed",
		},
		{
			"check_suite keyed on id and action",
			map[string]any{"action": "completed", "check_suite": map[string]any{"id": float64(555)}},
			nil,
			"tok:check_suite:555:completed",
		},
		{
			"other GitHub event falls back to the delivery GUID, not action",
			map[string]any{"action": "opened", "pull_request": map[string]any{"number": float64(3)}},
			map[string]string{"x-github-event": "pull_request", "x-github-delivery": "guid-9"},
			"tok:delivery=guid-9",
		},
		{
			"top-level id still wins over the delivery GUID",
			map[string]any{"id": "evt-1", "action": "opened"},
			map[string]string{"x-github-delivery": "guid-9"},
			"tok:evt-1",
		},
		{
			"action fallback kept for non-GitHub senders",
			map[string]any{"action": "opened"},
			nil,
			"tok:opened",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildWebhookDedupKey(webhookEvent{Token: "tok", Payload: tt.payload, Headers: tt.headers})
			if got != tt.want {
				t.Errorf("key = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestWebhookHandler_CapturesGitHubDelivery: the handler records the
// X-GitHub-Delivery header so the dedup fallback and {{header_x-github-delivery}}
// can use it.
func TestWebhookHandler_CapturesGitHubDelivery(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())
	src.mu.Lock()
	src.triggers["gh-tok"] = TriggerDef{
		ID:         "gd-wh-gh",
		Source:     "webhook",
		SourceData: webhookTriggerData{Token: "gh-tok", AllowUnsigned: true},
	}
	src.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/webhooks/gh-tok", strings.NewReader(`{"action":"opened"}`))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "72d3162e-cc78-11e3-81ab-4c9367dc0958")
	rr := httptest.NewRecorder()
	src.HandleWebhook().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	src.mu.Lock()
	evt := src.pending[0]
	src.mu.Unlock()
	if got := evt.Headers["x-github-delivery"]; got != "72d3162e-cc78-11e3-81ab-4c9367dc0958" {
		t.Errorf("x-github-delivery = %q", got)
	}
	m := webhookEventToMatch(evt, "webhook")
	if m.Key != "gh-tok:delivery=72d3162e-cc78-11e3-81ab-4c9367dc0958" {
		t.Errorf("key = %q", m.Key)
	}
	if m.TemplateVars["{{header_x-github-delivery}}"] != "72d3162e-cc78-11e3-81ab-4c9367dc0958" {
		t.Errorf("template var header_x-github-delivery = %q", m.TemplateVars["{{header_x-github-delivery}}"])
	}
}

// TestE2E_WebhookTrigger_GitHubWorkflowRunDedup drives signed workflow_run
// deliveries through HandleWebhook and the engine: two distinct runs on one
// token both fire, and a redelivery of the first run is deduped
// (gd-ExCBfoRCTA).
func TestE2E_WebhookTrigger_GitHubWorkflowRunDedup(t *testing.T) {
	const (
		triggerID  = "gd-trig-webhook-gh"
		webhookTok = "gh123"
		webhookSec = "shhh"
	)
	trigBead := &beadsapi.BeadDetail{
		ID:     triggerID,
		Type:   "trigger",
		Status: "open",
		Title:  "wake on workflow_run",
		Labels: []string{"trigger:webhook"},
		Fields: map[string]string{
			"webhook_token":  webhookTok,
			"webhook_secret": webhookSec,
			"action":         "spawn_agent",
			"project":        "gardenio",
			"role":           "crew",
			"prompt":         "Run {{workflow_run.id}} {{workflow_run.conclusion}}",
			"enabled":        "true",
			"max_concurrent": "5",
		},
	}
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{trigBead}}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	src := NewWebhookTriggerSource(slog.Default())
	engine.RegisterSource(src)
	engine.CatchUpSource(context.Background(), "webhook")

	post := func(payload, delivery string) {
		t.Helper()
		mac := hmac.New(sha256.New, []byte(webhookSec))
		mac.Write([]byte(payload))
		req := httptest.NewRequest(http.MethodPost, "/webhooks/"+webhookTok, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("X-GitHub-Event", "workflow_run")
		req.Header.Set("X-GitHub-Delivery", delivery)
		rr := httptest.NewRecorder()
		src.HandleWebhook().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("webhook POST returned %d (body=%s)", rr.Code, rr.Body.String())
		}
	}
	// evaluate runs one engine pass with last_checked well outside the
	// cooldown, so a pass that fires nothing is a dedup, not a cooldown skip.
	evaluate := func() {
		t.Helper()
		daemon.mu.Lock()
		trigBead.Fields["last_checked"] = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		daemon.mu.Unlock()
		engine.EvaluateSource(context.Background(), "webhook")
	}
	spawns := func() int {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		return len(daemon.spawnedNames)
	}

	const runA = `{"action":"completed","workflow_run":{"id":11111111111,"run_attempt":1,"name":"ci","head_sha":"abc","conclusion":"success"}}`
	const runB = `{"action":"completed","workflow_run":{"id":22222222222,"run_attempt":1,"name":"checks","head_sha":"abc","conclusion":"failure"}}`

	post(runA, "delivery-a")
	evaluate()
	post(runB, "delivery-b")
	evaluate()
	if got := spawns(); got != 2 {
		t.Fatalf("two distinct runs on one token: spawned %d, want 2", got)
	}

	// GitHub redelivery: same payload, same delivery GUID.
	post(runA, "delivery-a")
	evaluate()
	if got := spawns(); got != 2 {
		t.Fatalf("redelivery of run A: spawned %d total, want still 2", got)
	}
}
