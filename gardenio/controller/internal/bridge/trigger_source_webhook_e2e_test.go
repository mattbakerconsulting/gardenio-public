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

	"gardenio/controller/internal/beadsapi"
)

// TestE2E_WebhookTrigger_SpawnedTaskHasDepToTriggerBead drives the full
// HTTP-webhook → engine → actionSpawnAgent path and asserts that the
// spawned tracking task carries a `triggered-by` dep to the trigger
// definition bead. Plan gd-T5vMf08jDN §5.1 (epic ka-0000000933).
//
// Distinct from TestWebhookTriggerSource_SpawnedTask_DependsOnTriggerBead
// (M2a unit test): that test injects events directly into src.pending,
// bypassing the HMAC-signed HTTP entry. This e2e test POSTs through the
// real HandleWebhook handler with a valid signature so the wire-level
// path is also pinned.
func TestE2E_WebhookTrigger_SpawnedTaskHasDepToTriggerBead(t *testing.T) {
	const (
		triggerID   = "gd-trig-webhook-1"
		webhookTok  = "abc123"
		webhookSec  = "shhh"
		project     = "gardenio"
		role        = "crew"
	)

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     triggerID,
				Type:   "trigger",
				Status: "open",
				Title:  "auto-link e2e webhook",
				Labels: []string{"trigger:webhook"},
				Fields: map[string]string{
					"webhook_token":  webhookTok,
					"webhook_secret": webhookSec,
					"action":         "spawn_agent",
					"project":        project,
					"role":           role,
					"prompt":         "Handle {{key}}: {{title}}",
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

	// CatchUp also primes src.triggers via LoadTriggers so the HTTP handler
	// can route the inbound POST by token.
	engine.CatchUpSource(context.Background(), "webhook")

	// Sign and POST the payload through the real HMAC path.
	const payload = `{"key":"K1","title":"new build"}`
	mac := hmac.New(sha256.New, []byte(webhookSec))
	mac.Write([]byte(payload))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/"+webhookTok, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", sig)
	rr := httptest.NewRecorder()
	src.HandleWebhook().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("webhook POST returned %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if src.PendingCount() != 1 {
		t.Fatalf("expected 1 pending event after POST, got %d", src.PendingCount())
	}

	// Drive the engine to drain the buffered event into createTaskBead +
	// SpawnAgent + writeTriggerLineage.
	engine.EvaluateSource(context.Background(), "webhook")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(daemon.spawnedNames))
	}
	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 created task bead, got %d", len(daemon.createdBeads))
	}

	// Assert: spawned tracking task → triggered-by → trigger bead.
	var sawTriggerDep bool
	for _, d := range daemon.addedDeps {
		if d.DepType != "triggered-by" {
			t.Errorf("unexpected dep type %q (dep=%+v)", d.DepType, d)
			continue
		}
		if d.DependsOnID == triggerID {
			sawTriggerDep = true
			if d.CreatedBy != "trigger:"+triggerID {
				t.Errorf("triggered-by dep CreatedBy = %q, want %q", d.CreatedBy, "trigger:"+triggerID)
			}
		}
	}
	if !sawTriggerDep {
		t.Errorf("missing triggered-by dep to trigger bead %s (deps=%+v)", triggerID, daemon.addedDeps)
	}
}
