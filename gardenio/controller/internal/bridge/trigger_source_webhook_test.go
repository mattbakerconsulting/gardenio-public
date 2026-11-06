package bridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestWebhookTriggerSource_Name(t *testing.T) {
	src := NewWebhookTriggerSource(nil)
	if src.Name() != "webhook" {
		t.Errorf("Name() = %q, want %q", src.Name(), "webhook")
	}
}

// TestNamedWebhookTriggerSource_AlertScope verifies the alert-scoped source
// (W3a) reports Name()=="alert" and loads only trigger:alert beads — distinct
// from the generic trigger:webhook source, so two bridge processes don't both
// turn the same payload into beads (W3 design §4).
func TestNamedWebhookTriggerSource_AlertScope(t *testing.T) {
	src := NewNamedWebhookTriggerSource(nil, "alert")
	if src.Name() != "alert" {
		t.Errorf("Name() = %q, want %q", src.Name(), "alert")
	}
	// Empty name falls back to webhook.
	if NewNamedWebhookTriggerSource(nil, "").Name() != "webhook" {
		t.Errorf("empty name should fall back to webhook")
	}

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-alert-1", Type: "trigger", Status: "open", Title: "alert trig",
				Labels: []string{"trigger:alert"},
				Fields: map[string]string{"webhook_token": "atok", "webhook_secret": "s", "enabled": "true"},
			},
			{
				ID: "gd-wh-1", Type: "trigger", Status: "open", Title: "generic webhook",
				Labels: []string{"trigger:webhook"},
				Fields: map[string]string{"webhook_token": "wtok", "webhook_secret": "s", "enabled": "true"},
			},
		},
	}
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatal(err)
	}
	if len(triggers) != 1 || triggers[0].ID != "gd-alert-1" {
		t.Fatalf("alert source loaded %d triggers (want only gd-alert-1): %+v", len(triggers), triggers)
	}
	if triggers[0].Source != "alert" {
		t.Errorf("Source = %q, want alert", triggers[0].Source)
	}
}

func TestWebhookTriggerSource_LoadTriggers(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-wh-1",
				Type:   "trigger",
				Status: "open",
				Title:  "PagerDuty alerts",
				Labels: []string{"trigger:webhook"},
				Fields: map[string]string{
					"webhook_token":  "abc123",
					"webhook_secret": "mysecret",
					"action":         "spawn_agent",
					"project":        "gardenio",
					"role":           "crew",
					"enabled":        "true",
					"conditions":     `[{"path":"event.type","op":"==","value":"incident.triggered"}]`,
				},
			},
			{
				ID:     "gd-wh-2",
				Type:   "trigger",
				Status: "open",
				Title:  "Disabled webhook",
				Labels: []string{"trigger:webhook"},
				Fields: map[string]string{
					"webhook_token": "def456",
					"enabled":       "false",
				},
			},
			{
				ID:     "gd-wh-3",
				Type:   "trigger",
				Status: "open",
				Title:  "Missing token",
				Labels: []string{"trigger:webhook"},
				Fields: map[string]string{
					"enabled": "true",
				},
			},
		},
	}

	src := NewWebhookTriggerSource(slog.Default())
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatal(err)
	}

	// Only the first trigger should be loaded (second is disabled, third has no token).
	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(triggers))
	}
	if triggers[0].ID != "gd-wh-1" {
		t.Errorf("trigger ID = %q", triggers[0].ID)
	}
	if triggers[0].Source != "webhook" {
		t.Errorf("trigger Source = %q", triggers[0].Source)
	}

	data, ok := triggers[0].SourceData.(webhookTriggerData)
	if !ok {
		t.Fatal("SourceData is not webhookTriggerData")
	}
	if data.Token != "abc123" {
		t.Errorf("Token = %q", data.Token)
	}
	if data.Secret != "mysecret" {
		t.Errorf("Secret = %q", data.Secret)
	}
	if len(data.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(data.Conditions))
	}
	if data.Conditions[0].Path != "event.type" {
		t.Errorf("condition path = %q", data.Conditions[0].Path)
	}
}

func TestWebhookTriggerSource_Evaluate(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())

	// Buffer matching and non-matching events.
	src.mu.Lock()
	src.pending = []webhookEvent{
		{
			TriggerID: "gd-wh-1",
			Token:     "abc123",
			Payload:   map[string]any{"event": map[string]any{"type": "incident.triggered", "id": "evt-1"}},
		},
		{
			TriggerID: "gd-wh-1",
			Token:     "abc123",
			Payload:   map[string]any{"event": map[string]any{"type": "incident.resolved", "id": "evt-2"}},
		},
		{
			TriggerID: "gd-other",
			Token:     "other",
			Payload:   map[string]any{"event": map[string]any{"type": "incident.triggered", "id": "evt-3"}},
		},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID:            "gd-wh-1",
		Source:        "webhook",
		MaxConcurrent: 10,
		SourceData: webhookTriggerData{
			Token: "abc123",
			Conditions: []webhookCondition{
				{Path: "event.type", Op: "==", Value: "incident.triggered"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}

	// Only the first event should match (right trigger + right condition).
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if !strings.Contains(matches[0].Key, "abc123:evt-1") {
		t.Errorf("match key = %q, want to contain abc123:evt-1", matches[0].Key)
	}
}

func TestWebhookTriggerSource_EvaluateNoConditions(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())

	src.mu.Lock()
	src.pending = []webhookEvent{
		{
			TriggerID: "gd-wh-1",
			Token:     "tok1",
			Payload:   map[string]any{"action": "test", "id": "e1"},
		},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID:            "gd-wh-1",
		Source:        "webhook",
		MaxConcurrent: 10,
		SourceData: webhookTriggerData{
			Token:      "tok1",
			Conditions: nil, // no conditions = match all
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (no conditions = match all), got %d", len(matches))
	}
}

func TestWebhookTriggerSource_DrainPending(t *testing.T) {
	src := NewWebhookTriggerSource(nil)

	src.mu.Lock()
	src.pending = []webhookEvent{{TriggerID: "a"}, {TriggerID: "b"}}
	src.mu.Unlock()

	if src.PendingCount() != 2 {
		t.Errorf("expected 2 pending, got %d", src.PendingCount())
	}

	src.DrainPending()

	if src.PendingCount() != 0 {
		t.Errorf("expected 0 pending after drain, got %d", src.PendingCount())
	}
}

func TestWebhookTriggerSource_CatchUpKeys(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-task-1",
				Type:   "task",
				Status: "open",
				Labels: []string{"trigger:gd-wh-1"},
				Fields: map[string]string{"webhook_event_key": "tok:evt-1"},
			},
			{
				ID:     "gd-task-2",
				Type:   "task",
				Status: "closed",
				Labels: []string{"trigger:gd-wh-1"},
				Fields: map[string]string{"webhook_event_key": "tok:evt-2"},
			},
		},
	}

	src := NewWebhookTriggerSource(slog.Default())
	keys, err := src.CatchUpKeys(context.Background(), daemon, "gd-wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
}

func TestWebhookHandler_Success(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())

	// Load a trigger into the cache. AllowUnsigned=true opts out of HMAC for
	// this test; the default is fail-closed.
	src.mu.Lock()
	src.triggers["test-token"] = TriggerDef{
		ID:     "gd-wh-1",
		Source: "webhook",
		SourceData: webhookTriggerData{
			Token:         "test-token",
			AllowUnsigned: true,
		},
	}
	src.mu.Unlock()

	handler := src.HandleWebhook()
	payload := `{"event":{"type":"alert","id":"a1"},"message":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/test-token", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	if src.PendingCount() != 1 {
		t.Errorf("expected 1 pending event, got %d", src.PendingCount())
	}
}

func TestWebhookHandler_HMAC(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())
	secret := "test-secret-123"

	src.mu.Lock()
	src.triggers["hmac-token"] = TriggerDef{
		ID:     "gd-wh-2",
		Source: "webhook",
		SourceData: webhookTriggerData{
			Token:  "hmac-token",
			Secret: secret,
		},
	}
	src.mu.Unlock()

	handler := src.HandleWebhook()
	payload := `{"test":true}`

	// Valid HMAC.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/hmac-token", strings.NewReader(payload))
	req.Header.Set("X-Hub-Signature-256", sig)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("valid HMAC: status = %d, want %d", rr.Code, http.StatusOK)
	}

	// Invalid HMAC.
	req2 := httptest.NewRequest(http.MethodPost, "/webhooks/hmac-token", strings.NewReader(payload))
	req2.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusForbidden {
		t.Errorf("invalid HMAC: status = %d, want %d", rr2.Code, http.StatusForbidden)
	}

	// Missing HMAC header when secret is configured.
	req3 := httptest.NewRequest(http.MethodPost, "/webhooks/hmac-token", strings.NewReader(payload))
	rr3 := httptest.NewRecorder()
	handler.ServeHTTP(rr3, req3)

	if rr3.Code != http.StatusForbidden {
		t.Errorf("missing HMAC: status = %d, want %d", rr3.Code, http.StatusForbidden)
	}
}

func TestWebhookHandler_UnknownToken(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())
	handler := src.HandleWebhook()

	req := httptest.NewRequest(http.MethodPost, "/webhooks/unknown-token", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestWebhookHandler_MethodNotAllowed(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())
	handler := src.HandleWebhook()

	req := httptest.NewRequest(http.MethodGet, "/webhooks/token", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
	}
}

func TestWebhookHandler_InvalidJSON(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())

	src.mu.Lock()
	src.triggers["tok"] = TriggerDef{
		ID: "gd-wh-1", Source: "webhook",
		SourceData: webhookTriggerData{Token: "tok", AllowUnsigned: true},
	}
	src.mu.Unlock()

	handler := src.HandleWebhook()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/tok", strings.NewReader("not json"))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestExtractJSONPath(t *testing.T) {
	payload := map[string]any{
		"event": map[string]any{
			"type": "incident.triggered",
			"id":   "evt-123",
		},
		"severity": "critical",
		"count":    float64(42),
		"active":   true,
		"tags":     []any{"urgent", "infra"},
	}

	tests := []struct {
		path     string
		expected string
	}{
		{"event.type", "incident.triggered"},
		{"event.id", "evt-123"},
		{"severity", "critical"},
		{"count", "42"},
		{"active", "true"},
		{"tags", `["urgent","infra"]`},
		{"nonexistent", ""},
		{"event.nonexistent", ""},
		{"deeply.nested.missing", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := extractJSONPath(payload, tt.path)
			if got != tt.expected {
				t.Errorf("extractJSONPath(%q) = %q, want %q", tt.path, got, tt.expected)
			}
		})
	}
}

func TestExtractJSONPath_NilPayload(t *testing.T) {
	if got := extractJSONPath(nil, "foo"); got != "" {
		t.Errorf("expected empty for nil payload, got %q", got)
	}
}

func TestMatchWebhookConditions(t *testing.T) {
	payload := map[string]any{
		"event": map[string]any{
			"type":     "incident.triggered",
			"severity": "high",
		},
		"source": "pagerduty",
	}

	tests := []struct {
		name       string
		conditions []webhookCondition
		match      bool
	}{
		{"no conditions", nil, true},
		{"eq match", []webhookCondition{{Path: "event.type", Op: "==", Value: "incident.triggered"}}, true},
		{"eq no match", []webhookCondition{{Path: "event.type", Op: "==", Value: "incident.resolved"}}, false},
		{"ne match", []webhookCondition{{Path: "event.type", Op: "!=", Value: "incident.resolved"}}, true},
		{"ne no match", []webhookCondition{{Path: "event.type", Op: "!=", Value: "incident.triggered"}}, false},
		{"in match", []webhookCondition{{Path: "event.severity", Op: "in", Value: "low,medium,high"}}, true},
		{"in no match", []webhookCondition{{Path: "event.severity", Op: "in", Value: "low,medium"}}, false},
		{"not_in match", []webhookCondition{{Path: "event.severity", Op: "not_in", Value: "low,medium"}}, true},
		{"not_in no match", []webhookCondition{{Path: "event.severity", Op: "not_in", Value: "low,high"}}, false},
		{"contains match", []webhookCondition{{Path: "event.type", Op: "contains", Value: "triggered"}}, true},
		{"contains no match", []webhookCondition{{Path: "event.type", Op: "contains", Value: "resolved"}}, false},
		{"present match", []webhookCondition{{Path: "source", Op: "present"}}, true},
		{"present no match", []webhookCondition{{Path: "missing", Op: "present"}}, false},
		{"absent match", []webhookCondition{{Path: "missing", Op: "absent"}}, true},
		{"absent no match", []webhookCondition{{Path: "source", Op: "absent"}}, false},
		{"multiple all match", []webhookCondition{
			{Path: "event.type", Op: "==", Value: "incident.triggered"},
			{Path: "source", Op: "==", Value: "pagerduty"},
		}, true},
		{"multiple one fails", []webhookCondition{
			{Path: "event.type", Op: "==", Value: "incident.triggered"},
			{Path: "source", Op: "==", Value: "opsgenie"},
		}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchWebhookConditions(tt.conditions, payload)
			if got != tt.match {
				t.Errorf("matchWebhookConditions() = %v, want %v", got, tt.match)
			}
		})
	}
}

func TestVerifyWebhookHMAC(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{"test":true}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validSig := hex.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name    string
		header  string
		value   string
		valid   bool
	}{
		{"X-Hub-Signature-256", "X-Hub-Signature-256", "sha256=" + validSig, true},
		{"X-Hub-Signature", "X-Hub-Signature", "sha256=" + validSig, true},
		{"X-Webhook-Signature", "X-Webhook-Signature", "sha256=" + validSig, true},
		{"invalid signature", "X-Hub-Signature-256", "sha256=deadbeef", false},
		{"no header", "", "", false},
		{"wrong secret", "X-Hub-Signature-256", "sha256=" + hex.EncodeToString([]byte("wrong")), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.header != "" {
				h.Set(tt.header, tt.value)
			}
			got := verifyWebhookHMAC(body, h, secret)
			if got != tt.valid {
				t.Errorf("verifyWebhookHMAC() = %v, want %v", got, tt.valid)
			}
		})
	}
}

func TestExtractWebhookToken(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"/webhooks/abc123", "abc123"},
		{"/webhooks/abc123/", "abc123"},
		{"/webhooks/long-token-value", "long-token-value"},
		{"/webhooks/", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := extractWebhookToken(tt.path)
			if got != tt.expected {
				t.Errorf("extractWebhookToken(%q) = %q, want %q", tt.path, got, tt.expected)
			}
		})
	}
}

func TestWebhookEventToMatch(t *testing.T) {
	evt := webhookEvent{
		TriggerID: "gd-wh-1",
		Token:     "tok-1",
		Payload: map[string]any{
			"incident": map[string]any{
				"title":    "Database connection timeout",
				"severity": "high",
				"id":       "inc-999",
			},
			"event_type": "incident.triggered",
		},
		Headers: map[string]string{
			"x-github-event": "push",
		},
	}

	m := webhookEventToMatch(evt, "webhook")

	if m.Title != "Database connection timeout" {
		t.Errorf("Title = %q", m.Title)
	}
	if !strings.Contains(m.Key, "tok-1:") {
		t.Errorf("Key should contain token prefix, got %q", m.Key)
	}
	if m.Fields["webhook_token"] != "tok-1" {
		t.Errorf("webhook_token = %q", m.Fields["webhook_token"])
	}
	if m.TemplateVars["{{incident.title}}"] != "Database connection timeout" {
		t.Errorf("template var incident.title = %q", m.TemplateVars["{{incident.title}}"])
	}
	if m.TemplateVars["{{incident.severity}}"] != "high" {
		t.Errorf("template var incident.severity = %q", m.TemplateVars["{{incident.severity}}"])
	}
	if m.TemplateVars["{{header_x-github-event}}"] != "push" {
		t.Errorf("template var header_x-github-event = %q", m.TemplateVars["{{header_x-github-event}}"])
	}
}

func TestWebhookTriggerSource_EngineIntegration(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-wh-1",
				Type:   "trigger",
				Status: "open",
				Title:  "PagerDuty incident trigger",
				Labels: []string{"trigger:webhook"},
				Fields: map[string]string{
					"webhook_token":  "pd-hook",
					"action":         "create_bead",
					"project":        "gardenio",
					"enabled":        "true",
					"max_concurrent": "5",
					"conditions":     `[{"path":"event_type","op":"==","value":"incident.triggered"}]`,
				},
			},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	src := NewWebhookTriggerSource(slog.Default())
	engine.RegisterSource(src)

	// Load triggers to populate the cache.
	engine.CatchUpSource(context.Background(), "webhook")

	// Simulate an inbound webhook event.
	src.mu.Lock()
	src.pending = append(src.pending, webhookEvent{
		TriggerID: "gd-wh-1",
		Token:     "pd-hook",
		Payload: map[string]any{
			"event_type": "incident.triggered",
			"incident": map[string]any{
				"id":    "inc-42",
				"title": "API latency spike",
			},
		},
	})
	src.mu.Unlock()

	engine.EvaluateSource(context.Background(), "webhook")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 created bead, got %d", len(daemon.createdBeads))
	}
	if !strings.Contains(daemon.createdBeads[0].Title, "API latency spike") {
		t.Errorf("bead title = %q", daemon.createdBeads[0].Title)
	}
}

func TestWebhookTriggerSource_EngineSpawnAgent(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-wh-spawn",
				Type:   "trigger",
				Status: "open",
				Title:  "GitHub PR trigger",
				Labels: []string{"trigger:webhook"},
				Fields: map[string]string{
					"webhook_token":  "gh-hook",
					"action":         "spawn_agent",
					"project":        "gardenio",
					"role":           "crew",
					"prompt":         "Review PR #{{number}}: {{pull_request.title}}",
					"enabled":        "true",
					"max_concurrent": "3",
				},
			},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	src := NewWebhookTriggerSource(slog.Default())
	engine.RegisterSource(src)
	engine.CatchUpSource(context.Background(), "webhook")

	// Simulate a GitHub webhook event.
	src.mu.Lock()
	src.pending = append(src.pending, webhookEvent{
		TriggerID: "gd-wh-spawn",
		Token:     "gh-hook",
		Payload: map[string]any{
			"action": "opened",
			"number": float64(42),
			"pull_request": map[string]any{
				"title": "Add webhook triggers",
				"id":    float64(12345),
			},
		},
	})
	src.mu.Unlock()

	engine.EvaluateSource(context.Background(), "webhook")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(daemon.spawnedNames))
	}
	if daemon.spawnedRoles[0] != "crew" {
		t.Errorf("agent role = %q", daemon.spawnedRoles[0])
	}
	if daemon.lastPrompt != "Review PR #42: Add webhook triggers" {
		t.Errorf("agent prompt = %q", daemon.lastPrompt)
	}
}

// TestWebhookTriggerSource_SpawnedTask_DependsOnTriggerBead drives a full
// inbound-webhook → engine → actionSpawnAgent path and asserts that the
// spawned tracking task carries a `triggered-by` dep to the trigger
// definition bead. Auto-link M2a (ka-0000007272, epic ka-0000000933).
func TestWebhookTriggerSource_SpawnedTask_DependsOnTriggerBead(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-wh-m2a",
				Type:   "trigger",
				Status: "open",
				Title:  "Auto-link webhook trigger",
				Labels: []string{"trigger:webhook"},
				Fields: map[string]string{
					"webhook_token":  "m2a-tok",
					"action":         "spawn_agent",
					"project":        "gardenio",
					"role":           "crew",
					"prompt":         "Handle {{event_type}}",
					"enabled":        "true",
					"max_concurrent": "5",
				},
			},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	src := NewWebhookTriggerSource(slog.Default())
	engine.RegisterSource(src)
	engine.CatchUpSource(context.Background(), "webhook")

	src.mu.Lock()
	src.pending = append(src.pending, webhookEvent{
		TriggerID: "gd-wh-m2a",
		Token:     "m2a-tok",
		Payload: map[string]any{
			"event_type": "alert.fired",
			"id":         "evt-m2a-1",
		},
	})
	src.mu.Unlock()

	engine.EvaluateSource(context.Background(), "webhook")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(daemon.spawnedNames))
	}

	var sawTriggerDep bool
	for _, d := range daemon.addedDeps {
		if d.DepType != "triggered-by" {
			t.Errorf("unexpected dep type %q (dep=%+v)", d.DepType, d)
			continue
		}
		if d.DependsOnID == "gd-wh-m2a" {
			sawTriggerDep = true
		}
	}
	if !sawTriggerDep {
		t.Errorf("missing triggered-by dep to trigger bead gd-wh-m2a (deps=%+v)", daemon.addedDeps)
	}
}

// TestWebhookTriggerSource_SpawnedTask_DepIdempotentOnReFire verifies that
// re-firing the same trigger match (across engine dedup boundaries) does not
// produce duplicate dep rows beyond the server-side ON CONFLICT guarantee.
// Engine-level dedup prevents the re-spawn; AddDependency should not be
// invoked a second time for the same (bead, target, type) tuple.
func TestWebhookTriggerSource_SpawnedTask_DepIdempotentOnReFire(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-wh-idem",
				Type:   "trigger",
				Status: "open",
				Title:  "Idempotency test",
				Labels: []string{"trigger:webhook"},
				Fields: map[string]string{
					"webhook_token":  "idem-tok",
					"action":         "spawn_agent",
					"project":        "gardenio",
					"role":           "crew",
					"enabled":        "true",
					"max_concurrent": "5",
				},
			},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	src := NewWebhookTriggerSource(slog.Default())
	engine.RegisterSource(src)
	engine.CatchUpSource(context.Background(), "webhook")

	evt := webhookEvent{
		TriggerID: "gd-wh-idem",
		Token:     "idem-tok",
		Payload:   map[string]any{"id": "dup-evt"},
	}

	src.mu.Lock()
	src.pending = append(src.pending, evt)
	src.mu.Unlock()
	engine.EvaluateSource(context.Background(), "webhook")

	src.mu.Lock()
	src.pending = append(src.pending, evt)
	src.mu.Unlock()
	engine.EvaluateSource(context.Background(), "webhook")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawn (engine dedup), got %d", len(daemon.spawnedNames))
	}
	depCount := 0
	for _, d := range daemon.addedDeps {
		if d.DepType == "triggered-by" && d.DependsOnID == "gd-wh-idem" {
			depCount++
		}
	}
	if depCount != 1 {
		t.Errorf("expected exactly 1 triggered-by dep write (deduped), got %d (deps=%+v)", depCount, daemon.addedDeps)
	}
}

func TestWebhookTriggerSource_EngineDedup(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-wh-dd",
				Type:   "trigger",
				Status: "open",
				Title:  "Dedup test",
				Labels: []string{"trigger:webhook"},
				Fields: map[string]string{
					"webhook_token":  "dd-tok",
					"action":         "notify",
					"enabled":        "true",
					"max_concurrent": "10",
				},
			},
		},
	}

	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	var notifyCount int
	engine.notifier = func(_ context.Context, _, _ string, _ string) {
		notifyCount++
	}

	src := NewWebhookTriggerSource(slog.Default())
	engine.RegisterSource(src)
	engine.CatchUpSource(context.Background(), "webhook")

	evt := webhookEvent{
		TriggerID: "gd-wh-dd",
		Token:     "dd-tok",
		Payload:   map[string]any{"id": "same-event", "action": "test"},
	}

	// First evaluation.
	src.mu.Lock()
	src.pending = append(src.pending, evt)
	src.mu.Unlock()

	engine.EvaluateSource(context.Background(), "webhook")
	if notifyCount != 1 {
		t.Errorf("expected 1 notification after first eval, got %d", notifyCount)
	}

	// Second evaluation with same event — should be deduped.
	src.mu.Lock()
	src.pending = append(src.pending, evt)
	src.mu.Unlock()

	engine.EvaluateSource(context.Background(), "webhook")
	if notifyCount != 1 {
		t.Errorf("expected still 1 notification after dedup, got %d", notifyCount)
	}
}

func TestFlattenPayload(t *testing.T) {
	payload := map[string]any{
		"event": map[string]any{
			"type": "test",
			"data": map[string]any{
				"count": float64(5),
				"ok":    true,
			},
		},
		"source": "github",
	}

	out := make(map[string]string)
	flattenPayload("", payload, out)

	expected := map[string]string{
		"{{event.type}}":       "test",
		"{{event.data.count}}": "5",
		"{{event.data.ok}}":    "true",
		"{{source}}":           "github",
	}

	for k, v := range expected {
		if out[k] != v {
			t.Errorf("flattenPayload[%q] = %q, want %q", k, out[k], v)
		}
	}
}

func TestBuildWebhookDedupKey(t *testing.T) {
	// Event with id field.
	evt := webhookEvent{
		Token:   "tok",
		Payload: map[string]any{"id": "unique-123"},
	}
	key := buildWebhookDedupKey(evt)
	if key != "tok:unique-123" {
		t.Errorf("key = %q, want %q", key, "tok:unique-123")
	}

	// Event with nested event.id.
	evt2 := webhookEvent{
		Token:   "tok",
		Payload: map[string]any{"event": map[string]any{"id": "nested-456"}},
	}
	key2 := buildWebhookDedupKey(evt2)
	if key2 != "tok:nested-456" {
		t.Errorf("key = %q, want %q", key2, "tok:nested-456")
	}

	// Event with no ID fields — falls back to hash.
	evt3 := webhookEvent{
		Token:   "tok",
		Payload: map[string]any{"data": "no-id-here"},
	}
	key3 := buildWebhookDedupKey(evt3)
	if !strings.HasPrefix(key3, "tok:") {
		t.Errorf("hash key should have token prefix, got %q", key3)
	}
}

func TestExtractWebhookTitle(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			"incident title",
			map[string]any{"incident": map[string]any{"title": "DB down"}},
			"DB down",
		},
		{
			"alert summary",
			map[string]any{"alert": map[string]any{"summary": "CPU high"}},
			"CPU high",
		},
		{
			"event type fallback",
			map[string]any{"event_type": "deploy.completed"},
			"deploy.completed",
		},
		{
			"action fallback",
			map[string]any{"action": "opened"},
			"opened",
		},
		{
			"no title fields",
			map[string]any{"data": "unknown"},
			"Webhook event",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractWebhookTitle(tt.payload)
			if got != tt.want {
				t.Errorf("extractWebhookTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

// J3: handler must reject requests when the trigger has neither a secret nor
// allow_unsigned=true. Regression for AUDIT-2026-04-19-jira-gitlab-wl.md §J3.
func TestWebhookHandler_RejectsUnsignedByDefault(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())

	src.mu.Lock()
	src.triggers["fail-closed"] = TriggerDef{
		ID: "gd-wh-fc", Source: "webhook",
		SourceData: webhookTriggerData{Token: "fail-closed"}, // no secret, AllowUnsigned=false
	}
	src.mu.Unlock()

	handler := src.HandleWebhook()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/fail-closed", strings.NewReader(`{"x":1}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("no secret and allow_unsigned=false: status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if src.PendingCount() != 0 {
		t.Errorf("expected 0 pending events (request rejected), got %d", src.PendingCount())
	}
}

// J3: handler must also reject a signed-looking request when the trigger has
// no secret and allow_unsigned is false — no signature can verify against an
// empty secret, so accepting would be equivalent to the old no-auth path.
func TestWebhookHandler_RejectsSignedButNoSecret(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())

	src.mu.Lock()
	src.triggers["no-secret"] = TriggerDef{
		ID: "gd-wh-ns", Source: "webhook",
		SourceData: webhookTriggerData{Token: "no-secret"},
	}
	src.mu.Unlock()

	handler := src.HandleWebhook()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/no-secret", strings.NewReader(`{"x":1}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

// J3: AllowUnsigned=true preserves the legacy unauthenticated behavior for
// operators who explicitly opt in.
func TestWebhookHandler_AllowUnsignedAccepts(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())

	src.mu.Lock()
	src.triggers["dev-tok"] = TriggerDef{
		ID: "gd-wh-dev", Source: "webhook",
		SourceData: webhookTriggerData{Token: "dev-tok", AllowUnsigned: true},
	}
	src.mu.Unlock()

	handler := src.HandleWebhook()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/dev-tok", strings.NewReader(`{"x":1}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("allow_unsigned=true: status = %d, want %d", rr.Code, http.StatusOK)
	}
	if src.PendingCount() != 1 {
		t.Errorf("expected 1 pending event, got %d", src.PendingCount())
	}
}

// J4: oversize body must be rejected before HMAC verification allocates over
// r.Body. Regression for AUDIT-2026-04-19-jira-gitlab-wl.md §J4.
func TestWebhookHandler_BodyTooLarge(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())
	secret := "rejsecret"

	src.mu.Lock()
	src.triggers["big-tok"] = TriggerDef{
		ID: "gd-wh-big", Source: "webhook",
		SourceData: webhookTriggerData{Token: "big-tok", Secret: secret},
	}
	src.mu.Unlock()

	oversize := bytes.Repeat([]byte("A"), webhookMaxBodyBytes+1)
	// Include a plausible (wrong) signature so we know the rejection is from
	// MaxBytesReader, not the HMAC path.
	req := httptest.NewRequest(http.MethodPost, "/webhooks/big-tok", bytes.NewReader(oversize))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rr := httptest.NewRecorder()
	src.HandleWebhook().ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatalf("oversize body accepted (status=200); MaxBytesReader not applied")
	}
	if src.PendingCount() != 0 {
		t.Errorf("expected 0 pending events after oversize reject, got %d", src.PendingCount())
	}
}

// J3: LoadTriggers must persist the allow_unsigned opt-in onto the cached
// trigger so the handler can honor it.
func TestWebhookTriggerSource_LoadTriggers_AllowUnsigned(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-wh-au",
				Type:   "trigger",
				Status: "open",
				Title:  "Internal dev trigger",
				Labels: []string{"trigger:webhook"},
				Fields: map[string]string{
					"webhook_token":  "au-tok",
					"allow_unsigned": "true",
					"enabled":        "true",
				},
			},
		},
	}

	src := NewWebhookTriggerSource(slog.Default())
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatal(err)
	}
	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(triggers))
	}
	data, ok := triggers[0].SourceData.(webhookTriggerData)
	if !ok {
		t.Fatal("SourceData is not webhookTriggerData")
	}
	if !data.AllowUnsigned {
		t.Errorf("AllowUnsigned = false, want true")
	}
}

func TestWebhookHandler_HMACHeaderVariants(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())
	secret := "variant-secret"

	src.mu.Lock()
	src.triggers["hmac-var"] = TriggerDef{
		ID: "gd-wh-var", Source: "webhook",
		SourceData: webhookTriggerData{Token: "hmac-var", Secret: secret},
	}
	src.mu.Unlock()

	handler := src.HandleWebhook()
	payload := `{"ok":true}`

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	// Test X-Webhook-Signature (PagerDuty style).
	req := httptest.NewRequest(http.MethodPost, "/webhooks/hmac-var", strings.NewReader(payload))
	req.Header.Set("X-Webhook-Signature", sig)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("X-Webhook-Signature: status = %d, want %d", rr.Code, http.StatusOK)
	}

	// Test X-Hub-Signature (Jira style).
	req2 := httptest.NewRequest(http.MethodPost, "/webhooks/hmac-var", strings.NewReader(payload))
	req2.Header.Set("X-Hub-Signature", sig)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Errorf("X-Hub-Signature: status = %d, want %d", rr2.Code, http.StatusOK)
	}
}

func TestWebhookHandler_ResponseFormat(t *testing.T) {
	src := NewWebhookTriggerSource(slog.Default())

	src.mu.Lock()
	src.triggers["resp-tok"] = TriggerDef{
		ID: "gd-resp", Source: "webhook",
		SourceData: webhookTriggerData{Token: "resp-tok", AllowUnsigned: true},
	}
	src.mu.Unlock()

	handler := src.HandleWebhook()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/resp-tok", strings.NewReader(`{"x":1}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp["status"] != "accepted" {
		t.Errorf("response status = %q, want accepted", resp["status"])
	}
}
