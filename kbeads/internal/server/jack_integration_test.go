package server

import (
	"encoding/json"
	"testing"
	"time"
)

// --- Jack Integration Helpers ---

// createJack creates a jack bead via HTTP and returns its ID. It creates the bead
// with type=jack, sets status to in_progress, and adds the jack:general label.
func createJack(t *testing.T, serverURL, target, reason, revertPlan, ttl string) string {
	t.Helper()
	expiresAt := time.Now().UTC().Add(mustParseDuration(t, ttl))
	fields := map[string]any{
		"jack_target":          target,
		"jack_reason":          reason,
		"jack_revert_plan":     revertPlan,
		"jack_ttl":             ttl,
		"jack_expires_at":      expiresAt.Format(time.RFC3339),
		"jack_extension_count": 0,
		"jack_cumulative_ttl":  ttl,
		"jack_reverted":        false,
		"jack_escalated":       false,
		"jack_changes":         []any{},
	}
	fieldsJSON, _ := json.Marshal(fields)

	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title":      "Jack: " + target,
		"type":       "jack",
		"created_by": "test-agent",
		"fields":     json.RawMessage(fieldsJSON),
		"labels":     []string{"jack:general"},
	})
	requireHTTPStatus(t, resp, 201)
	var bead struct {
		ID string `json:"id"`
	}
	decodeHTTPJSON(t, resp, &bead)

	// Set to in_progress (jacks start active).
	resp = doHTTPJSON(t, "PATCH", serverURL+"/v1/beads/"+bead.ID, map[string]any{
		"status": "in_progress",
	})
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	return bead.ID
}

// getJackFields fetches a jack bead and returns its parsed Fields map.
func getJackFields(t *testing.T, serverURL, id string) map[string]any {
	t.Helper()
	resp := doHTTPJSON(t, "GET", serverURL+"/v1/beads/"+id, nil)
	requireHTTPStatus(t, resp, 200)
	var bead struct {
		Fields json.RawMessage `json:"fields"`
	}
	decodeHTTPJSON(t, resp, &bead)
	var fields map[string]any
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("failed to parse jack fields: %v", err)
	}
	return fields
}

func mustParseDuration(t *testing.T, s string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatalf("invalid duration %q: %v", s, err)
	}
	return d
}

// --- Full Lifecycle ---

func TestJackIntegration_FullLifecycle(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Connect SSE client to verify events.
	sseEvents, sseCancel := startSSEClient(t, serverURL, "")
	defer sseCancel()
	time.Sleep(50 * time.Millisecond)

	// 1. Create jack (up).
	jackID := createJack(t, serverURL, "pod/my-app", "Debug logging", "Restore config", "1h")
	if jackID == "" {
		t.Fatal("expected jack to have an ID")
	}

	// Verify SSE events: bead.created + bead.updated (status change).
	waitForEvent(t, sseEvents, "beads.bead.created", 2*time.Second)
	waitForEvent(t, sseEvents, "beads.bead.updated", 2*time.Second)

	// Verify fields persisted.
	fields := getJackFields(t, serverURL, jackID)
	if fields["jack_target"] != "pod/my-app" {
		t.Fatalf("expected jack_target=%q, got %q", "pod/my-app", fields["jack_target"])
	}
	if fields["jack_reason"] != "Debug logging" {
		t.Fatalf("expected jack_reason=%q, got %q", "Debug logging", fields["jack_reason"])
	}
	if fields["jack_revert_plan"] != "Restore config" {
		t.Fatalf("expected jack_revert_plan=%q, got %q", "Restore config", fields["jack_revert_plan"])
	}

	// 2. Log a change.
	changes := []any{
		map[string]any{
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"action":    "edit",
			"target":    "pod/my-app:/app/config.yaml",
			"before":    "log_level: info",
			"after":     "log_level: debug",
			"agent":     "test-agent",
		},
	}
	fields["jack_changes"] = changes
	fieldsJSON, _ := json.Marshal(fields)
	resp := doHTTPJSON(t, "PATCH", serverURL+"/v1/beads/"+jackID, map[string]any{
		"fields": json.RawMessage(fieldsJSON),
	})
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	// Verify change persisted.
	fields = getJackFields(t, serverURL, jackID)
	changesArr, ok := fields["jack_changes"].([]any)
	if !ok || len(changesArr) != 1 {
		t.Fatalf("expected 1 change, got %v", fields["jack_changes"])
	}

	// 3. Extend TTL.
	newExpiry := time.Now().UTC().Add(2 * time.Hour)
	fields["jack_expires_at"] = newExpiry.Format(time.RFC3339)
	fields["jack_extension_count"] = float64(1)
	fields["jack_original_ttl"] = "1h"
	fields["jack_cumulative_ttl"] = "3h"
	fieldsJSON, _ = json.Marshal(fields)
	resp = doHTTPJSON(t, "PATCH", serverURL+"/v1/beads/"+jackID, map[string]any{
		"fields": json.RawMessage(fieldsJSON),
	})
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	// Add extension comment.
	resp = doHTTPJSON(t, "POST", serverURL+"/v1/beads/"+jackID+"/comments", map[string]any{
		"author": "test-agent",
		"text":   "Extended TTL by 2h (extension 1/5): need more time",
	})
	requireHTTPStatus(t, resp, 201)
	resp.Body.Close()

	// Verify extension fields.
	fields = getJackFields(t, serverURL, jackID)
	if fields["jack_extension_count"] != float64(1) {
		t.Fatalf("expected extension_count=1, got %v", fields["jack_extension_count"])
	}
	if fields["jack_original_ttl"] != "1h" {
		t.Fatalf("expected original_ttl=%q, got %v", "1h", fields["jack_original_ttl"])
	}

	// 4. Close jack (down).
	fields["jack_reverted"] = true
	fields["jack_closed_reason"] = "Found root cause"
	fields["jack_closed_at"] = time.Now().UTC().Format(time.RFC3339)
	fieldsJSON, _ = json.Marshal(fields)
	resp = doHTTPJSON(t, "PATCH", serverURL+"/v1/beads/"+jackID, map[string]any{
		"fields": json.RawMessage(fieldsJSON),
	})
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	resp = doHTTPJSON(t, "POST", serverURL+"/v1/beads/"+jackID+"/close", map[string]any{
		"closed_by": "test-agent",
	})
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	// 5. Verify final state.
	finalResp := doHTTPJSON(t, "GET", serverURL+"/v1/beads/"+jackID, nil)
	requireHTTPStatus(t, finalResp, 200)
	var finalBead struct {
		ID     string          `json:"id"`
		Status string          `json:"status"`
		Type   string          `json:"type"`
		Fields json.RawMessage `json:"fields"`
	}
	decodeHTTPJSON(t, finalResp, &finalBead)
	if finalBead.Status != "closed" {
		t.Fatalf("expected status=closed, got %q", finalBead.Status)
	}
	if finalBead.Type != "jack" {
		t.Fatalf("expected type=jack, got %q", finalBead.Type)
	}

	var finalFields map[string]any
	if err := json.Unmarshal(finalBead.Fields, &finalFields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if finalFields["jack_reverted"] != true {
		t.Fatalf("expected jack_reverted=true, got %v", finalFields["jack_reverted"])
	}
	if finalFields["jack_closed_reason"] != "Found root cause" {
		t.Fatalf("expected jack_closed_reason=%q, got %v", "Found root cause", finalFields["jack_closed_reason"])
	}

	// Verify events recorded.
	eventsResp := doHTTPJSON(t, "GET", serverURL+"/v1/beads/"+jackID+"/events", nil)
	requireHTTPStatus(t, eventsResp, 200)
	var eventsResult struct {
		Events []struct {
			Topic string `json:"topic"`
		} `json:"events"`
	}
	decodeHTTPJSON(t, eventsResp, &eventsResult)
	if len(eventsResult.Events) < 4 {
		t.Fatalf("expected at least 4 events (create, updates, close), got %d", len(eventsResult.Events))
	}
}

// --- TTL Enforcement ---

func TestJackIntegration_ExpiredJackDetection(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Create a jack with expired TTL (expires_at in the past).
	pastExpiry := time.Now().UTC().Add(-1 * time.Hour)
	fields := map[string]any{
		"jack_target":          "pod/expired-app",
		"jack_reason":          "Testing expiry",
		"jack_revert_plan":     "N/A",
		"jack_ttl":             "1h",
		"jack_expires_at":      pastExpiry.Format(time.RFC3339),
		"jack_extension_count": 0,
		"jack_cumulative_ttl":  "1h",
		"jack_reverted":        false,
		"jack_escalated":       false,
		"jack_changes":         []any{},
	}
	fieldsJSON, _ := json.Marshal(fields)

	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title":      "Jack: pod/expired-app",
		"type":       "jack",
		"created_by": "test-agent",
		"fields":     json.RawMessage(fieldsJSON),
		"labels":     []string{"jack:debug"},
	})
	requireHTTPStatus(t, resp, 201)
	var bead struct{ ID string `json:"id"` }
	decodeHTTPJSON(t, resp, &bead)

	// Set in_progress.
	resp = doHTTPJSON(t, "PATCH", serverURL+"/v1/beads/"+bead.ID, map[string]any{
		"status": "in_progress",
	})
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	// List in_progress jacks — should include our expired one.
	resp = doHTTPJSON(t, "GET", serverURL+"/v1/beads?type=jack&status=in_progress", nil)
	requireHTTPStatus(t, resp, 200)
	var listResult struct {
		Beads []struct {
			ID     string          `json:"id"`
			Fields json.RawMessage `json:"fields"`
		} `json:"beads"`
	}
	decodeHTTPJSON(t, resp, &listResult)

	found := false
	for _, b := range listResult.Beads {
		if b.ID == bead.ID {
			found = true
			var f map[string]any
			if err := json.Unmarshal(b.Fields, &f); err != nil {
				t.Fatalf("unmarshal fields: %v", err)
			}
			expiresStr, _ := f["jack_expires_at"].(string)
			expiresAt, err := time.Parse(time.RFC3339, expiresStr)
			if err != nil {
				t.Fatalf("failed to parse expires_at: %v", err)
			}
			if !time.Now().UTC().After(expiresAt) {
				t.Fatal("expected jack to be expired")
			}
		}
	}
	if !found {
		t.Fatalf("expired jack %s not found in in_progress list", bead.ID)
	}

	// Simulate auto-escalation: mark jack_escalated=true.
	fields = getJackFields(t, serverURL, bead.ID)
	fields["jack_escalated"] = true
	fields["jack_escalated_at"] = time.Now().UTC().Format(time.RFC3339)
	fieldsJSON, _ = json.Marshal(fields)
	resp = doHTTPJSON(t, "PATCH", serverURL+"/v1/beads/"+bead.ID, map[string]any{
		"fields": json.RawMessage(fieldsJSON),
	})
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	// Verify escalation persisted.
	fields = getJackFields(t, serverURL, bead.ID)
	if fields["jack_escalated"] != true {
		t.Fatalf("expected jack_escalated=true, got %v", fields["jack_escalated"])
	}
	if fields["jack_escalated_at"] == nil {
		t.Fatal("expected jack_escalated_at to be set")
	}
}

// --- Extension Limits ---

func TestJackIntegration_ExtensionLimitEnforcement(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	jackID := createJack(t, serverURL, "pod/extend-test", "Extension test", "Undo", "1h")

	// Simulate 5 extensions (the maximum).
	fields := getJackFields(t, serverURL, jackID)
	fields["jack_extension_count"] = float64(5)
	fields["jack_cumulative_ttl"] = "6h"
	fields["jack_original_ttl"] = "1h"
	fieldsJSON, _ := json.Marshal(fields)
	resp := doHTTPJSON(t, "PATCH", serverURL+"/v1/beads/"+jackID, map[string]any{
		"fields": json.RawMessage(fieldsJSON),
	})
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	// Verify fields show max extensions reached.
	fields = getJackFields(t, serverURL, jackID)
	if fields["jack_extension_count"] != float64(5) {
		t.Fatalf("expected extension_count=5, got %v", fields["jack_extension_count"])
	}

	// Verify cumulative TTL tracking.
	cumTTL, _ := fields["jack_cumulative_ttl"].(string)
	if cumTTL != "6h" {
		t.Fatalf("expected cumulative_ttl=%q, got %q", "6h", cumTTL)
	}
}

// --- Change Recording ---

func TestJackIntegration_MultipleChanges(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	jackID := createJack(t, serverURL, "deployment/api", "Hotfix test", "Rollback", "30m")

	// Log 3 changes of different action types.
	changeActions := []struct {
		action string
		target string
		cmd    string
	}{
		{"edit", "deployment/api:/app/config.yaml", ""},
		{"exec", "", "kubectl rollout restart deployment/api"},
		{"patch", "deployment/api", ""},
	}

	fields := getJackFields(t, serverURL, jackID)
	changes := make([]any, 0)

	for _, ca := range changeActions {
		change := map[string]any{
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"action":    ca.action,
			"agent":     "test-agent",
		}
		if ca.target != "" {
			change["target"] = ca.target
		}
		if ca.cmd != "" {
			change["cmd"] = ca.cmd
		}
		if ca.action == "edit" {
			change["before"] = "replicas: 2"
			change["after"] = "replicas: 3"
		}
		changes = append(changes, change)
	}

	fields["jack_changes"] = changes
	fieldsJSON, _ := json.Marshal(fields)
	resp := doHTTPJSON(t, "PATCH", serverURL+"/v1/beads/"+jackID, map[string]any{
		"fields": json.RawMessage(fieldsJSON),
	})
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	// Verify all changes persisted.
	fields = getJackFields(t, serverURL, jackID)
	changesArr, ok := fields["jack_changes"].([]any)
	if !ok {
		t.Fatal("expected jack_changes to be an array")
	}
	if len(changesArr) != 3 {
		t.Fatalf("expected 3 changes, got %d", len(changesArr))
	}

	// Verify first change has correct action and before/after.
	firstChange, ok := changesArr[0].(map[string]any)
	if !ok {
		t.Fatal("expected first change to be a map")
	}
	if firstChange["action"] != "edit" {
		t.Fatalf("expected action=%q, got %v", "edit", firstChange["action"])
	}
	if firstChange["before"] != "replicas: 2" {
		t.Fatalf("expected before=%q, got %v", "replicas: 2", firstChange["before"])
	}

	// Verify exec change has cmd.
	secondChange, ok := changesArr[1].(map[string]any)
	if !ok {
		t.Fatal("expected second change to be a map")
	}
	if secondChange["cmd"] != "kubectl rollout restart deployment/api" {
		t.Fatalf("expected cmd=%q, got %v", "kubectl rollout restart deployment/api", secondChange["cmd"])
	}
}

// --- Label Assignment ---

func TestJackIntegration_LabelAssignment(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Create jack with jack:debug label.
	fields := map[string]any{
		"jack_target":          "pod/label-test",
		"jack_reason":          "Test labels",
		"jack_revert_plan":     "N/A",
		"jack_ttl":             "30m",
		"jack_expires_at":      time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339),
		"jack_extension_count": 0,
		"jack_cumulative_ttl":  "30m",
		"jack_reverted":        false,
		"jack_escalated":       false,
		"jack_changes":         []any{},
	}
	fieldsJSON, _ := json.Marshal(fields)

	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title":      "Jack: pod/label-test",
		"type":       "jack",
		"created_by": "test-agent",
		"fields":     json.RawMessage(fieldsJSON),
		"labels":     []string{"jack:debug", "jack:hotfix"},
	})
	requireHTTPStatus(t, resp, 201)
	var bead struct{ ID string `json:"id"` }
	decodeHTTPJSON(t, resp, &bead)

	// Verify labels.
	labelsResp := doHTTPJSON(t, "GET", serverURL+"/v1/beads/"+bead.ID+"/labels", nil)
	requireHTTPStatus(t, labelsResp, 200)
	var labelsResult struct {
		Labels []string `json:"labels"`
	}
	decodeHTTPJSON(t, labelsResp, &labelsResult)
	if len(labelsResult.Labels) != 2 {
		t.Fatalf("expected 2 labels, got %d: %v", len(labelsResult.Labels), labelsResult.Labels)
	}
	labelSet := map[string]bool{}
	for _, l := range labelsResult.Labels {
		labelSet[l] = true
	}
	if !labelSet["jack:debug"] || !labelSet["jack:hotfix"] {
		t.Fatalf("expected jack:debug and jack:hotfix, got %v", labelsResult.Labels)
	}

	// Add another label after creation.
	resp = doHTTPJSON(t, "POST", serverURL+"/v1/beads/"+bead.ID+"/labels", map[string]any{
		"label": "jack:config",
	})
	requireHTTPStatus(t, resp, 201)
	resp.Body.Close()

	// Verify 3 labels now.
	labelsResp = doHTTPJSON(t, "GET", serverURL+"/v1/beads/"+bead.ID+"/labels", nil)
	requireHTTPStatus(t, labelsResp, 200)
	decodeHTTPJSON(t, labelsResp, &labelsResult)
	if len(labelsResult.Labels) != 3 {
		t.Fatalf("expected 3 labels, got %d", len(labelsResult.Labels))
	}
}

// --- Jack with Dependency (blocks) ---

func TestJackIntegration_BlocksDependency(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Create a task bead that will be blocked by the jack.
	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title": "Task blocked by jack", "type": "task",
	})
	requireHTTPStatus(t, resp, 201)
	var taskBead struct{ ID string `json:"id"` }
	decodeHTTPJSON(t, resp, &taskBead)

	// Create jack.
	jackID := createJack(t, serverURL, "deployment/api", "Emergency fix", "Rollback", "1h")

	// Add blocks dependency: task depends on jack.
	resp = doHTTPJSON(t, "POST", serverURL+"/v1/beads/"+taskBead.ID+"/dependencies", map[string]any{
		"depends_on_id": jackID,
		"type":          "blocks",
		"created_by":    "test-agent",
	})
	requireHTTPStatus(t, resp, 201)
	resp.Body.Close()

	// Verify dependency exists.
	depsResp := doHTTPJSON(t, "GET", serverURL+"/v1/beads/"+taskBead.ID+"/dependencies", nil)
	requireHTTPStatus(t, depsResp, 200)
	var depsResult struct {
		Dependencies []struct {
			BeadID      string `json:"bead_id"`
			DependsOnID string `json:"depends_on_id"`
			Type        string `json:"type"`
		} `json:"dependencies"`
	}
	decodeHTTPJSON(t, depsResp, &depsResult)
	if len(depsResult.Dependencies) != 1 {
		t.Fatalf("expected 1 dependency, got %d", len(depsResult.Dependencies))
	}
	if depsResult.Dependencies[0].DependsOnID != jackID {
		t.Fatalf("expected depends_on_id=%q, got %q", jackID, depsResult.Dependencies[0].DependsOnID)
	}
}

// --- Close without revert (skip-revert-check) ---

func TestJackIntegration_CloseSkipRevert(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	jackID := createJack(t, serverURL, "pod/recycled", "Test skip revert", "Restore config", "30m")

	// Close with skip-revert-check (jack_reverted=false).
	fields := getJackFields(t, serverURL, jackID)
	fields["jack_reverted"] = false
	fields["jack_closed_reason"] = "Pod recycled, modifications already gone"
	fields["jack_closed_at"] = time.Now().UTC().Format(time.RFC3339)
	fieldsJSON, _ := json.Marshal(fields)

	resp := doHTTPJSON(t, "PATCH", serverURL+"/v1/beads/"+jackID, map[string]any{
		"fields": json.RawMessage(fieldsJSON),
	})
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	resp = doHTTPJSON(t, "POST", serverURL+"/v1/beads/"+jackID+"/close", map[string]any{
		"closed_by": "test-agent",
	})
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	// Verify jack_reverted=false persisted.
	fields = getJackFields(t, serverURL, jackID)
	if fields["jack_reverted"] != false {
		t.Fatalf("expected jack_reverted=false, got %v", fields["jack_reverted"])
	}
	if fields["jack_closed_reason"] != "Pod recycled, modifications already gone" {
		t.Fatalf("expected specific close reason, got %v", fields["jack_closed_reason"])
	}
}

// --- SSE Events for Jack Lifecycle ---

func TestJackIntegration_SSEEventsForLifecycle(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	sseEvents, sseCancel := startSSEClient(t, serverURL, "topics=beads.bead.*")
	defer sseCancel()
	time.Sleep(50 * time.Millisecond)

	// Create jack.
	jackID := createJack(t, serverURL, "pod/sse-test", "SSE test", "Revert", "1h")

	// Should see created + updated (status to in_progress).
	createEvt := waitForEvent(t, sseEvents, "beads.bead.created", 2*time.Second)
	var createPayload struct {
		Bead struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"bead"`
	}
	if err := json.Unmarshal([]byte(createEvt.Data), &createPayload); err != nil {
		t.Fatalf("failed to parse create event: %v", err)
	}
	if createPayload.Bead.ID != jackID {
		t.Fatalf("expected bead ID=%q, got %q", jackID, createPayload.Bead.ID)
	}
	if createPayload.Bead.Type != "jack" {
		t.Fatalf("expected type=jack, got %q", createPayload.Bead.Type)
	}

	updateEvt := waitForEvent(t, sseEvents, "beads.bead.updated", 2*time.Second)
	var updatePayload struct {
		Bead struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"bead"`
	}
	if err := json.Unmarshal([]byte(updateEvt.Data), &updatePayload); err != nil {
		t.Fatalf("failed to parse update event: %v", err)
	}
	if updatePayload.Bead.Status != "in_progress" {
		t.Fatalf("expected status=in_progress, got %q", updatePayload.Bead.Status)
	}

	// Close jack.
	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads/"+jackID+"/close", map[string]any{
		"closed_by": "test-agent",
	})
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	closeEvt := waitForEvent(t, sseEvents, "beads.bead.closed", 2*time.Second)
	var closePayload struct {
		Bead struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"bead"`
	}
	if err := json.Unmarshal([]byte(closeEvt.Data), &closePayload); err != nil {
		t.Fatalf("failed to parse close event: %v", err)
	}
	if closePayload.Bead.ID != jackID {
		t.Fatalf("expected bead ID=%q in close event, got %q", jackID, closePayload.Bead.ID)
	}
	if closePayload.Bead.Status != "closed" {
		t.Fatalf("expected status=closed, got %q", closePayload.Bead.Status)
	}
}

// --- List filtering by jack type ---

func TestJackIntegration_ListFilterByType(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Create a jack and a regular task.
	createJack(t, serverURL, "pod/filter-test", "Filter test", "Revert", "1h")
	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title": "Regular task", "type": "task",
	})
	requireHTTPStatus(t, resp, 201)
	resp.Body.Close()

	// List only jacks.
	resp = doHTTPJSON(t, "GET", serverURL+"/v1/beads?type=jack", nil)
	requireHTTPStatus(t, resp, 200)
	var result struct {
		Beads []struct {
			Type string `json:"type"`
		} `json:"beads"`
		Total int `json:"total"`
	}
	decodeHTTPJSON(t, resp, &result)
	if result.Total != 1 {
		t.Fatalf("expected 1 jack, got %d", result.Total)
	}
	if result.Beads[0].Type != "jack" {
		t.Fatalf("expected type=jack, got %q", result.Beads[0].Type)
	}
}

// --- Concurrent jacks on different targets ---

func TestJackIntegration_ConcurrentJacks(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	jack1 := createJack(t, serverURL, "pod/app-1", "Debug app-1", "Revert app-1", "1h")
	jack2 := createJack(t, serverURL, "pod/app-2", "Debug app-2", "Revert app-2", "30m")

	// Both should be in_progress.
	resp := doHTTPJSON(t, "GET", serverURL+"/v1/beads?type=jack&status=in_progress", nil)
	requireHTTPStatus(t, resp, 200)
	var result struct {
		Beads []struct{ ID string `json:"id"` } `json:"beads"`
		Total int                                `json:"total"`
	}
	decodeHTTPJSON(t, resp, &result)
	if result.Total != 2 {
		t.Fatalf("expected 2 active jacks, got %d", result.Total)
	}

	// Verify they have different targets.
	fields1 := getJackFields(t, serverURL, jack1)
	fields2 := getJackFields(t, serverURL, jack2)
	if fields1["jack_target"] == fields2["jack_target"] {
		t.Fatal("expected different targets for concurrent jacks")
	}
}
