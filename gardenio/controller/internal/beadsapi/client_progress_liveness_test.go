package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAgentProgressStalled exercises the progress-liveness predicate directly:
// it must fire ONLY on a demonstrably-old last_progress_at, and fail open in
// every ambiguous case so it can never evict a live agent (ka-0000000286).
func TestAgentProgressStalled(t *testing.T) {
	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)
	const thr = 90 * time.Minute

	cases := []struct {
		name      string
		fields    map[string]string
		threshold time.Duration
		want      bool
	}{
		{
			name:      "fresh progress is live",
			fields:    map[string]string{"last_progress_at": now.Add(-5 * time.Minute).Format(time.RFC3339)},
			threshold: thr,
			want:      false,
		},
		{
			name:      "just inside the threshold is live",
			fields:    map[string]string{"last_progress_at": now.Add(-89 * time.Minute).Format(time.RFC3339)},
			threshold: thr,
			want:      false,
		},
		{
			name:      "well past the threshold is stalled",
			fields:    map[string]string{"last_progress_at": now.Add(-12 * time.Hour).Format(time.RFC3339)},
			threshold: thr,
			want:      true,
		},
		{
			name:      "absent last_progress_at fails open (live)",
			fields:    map[string]string{"agent_state": "working"},
			threshold: thr,
			want:      false,
		},
		{
			name:      "empty last_progress_at fails open (live)",
			fields:    map[string]string{"last_progress_at": ""},
			threshold: thr,
			want:      false,
		},
		{
			name:      "unparseable timestamp fails open (live)",
			fields:    map[string]string{"last_progress_at": "not-a-timestamp"},
			threshold: thr,
			want:      false,
		},
		{
			name:      "zero threshold disables the check",
			fields:    map[string]string{"last_progress_at": now.Add(-12 * time.Hour).Format(time.RFC3339)},
			threshold: 0,
			want:      false,
		},
		{
			name:      "nil fields fails open (live)",
			fields:    nil,
			threshold: thr,
			want:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentProgressStalled(tc.fields, now, tc.threshold); got != tc.want {
				t.Errorf("AgentProgressStalled(%v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}

// agentRowWithFields is agentRow plus arbitrary extra fields (e.g.
// last_progress_at) merged into the row's fields map.
func agentRowWithFields(id, agentState string, extra map[string]string) map[string]any {
	fields := map[string]string{"agent_state": agentState, "task_id": "gd-step-1"}
	for k, v := range extra {
		fields[k] = v
	}
	return map[string]any{
		"id":     id,
		"title":  id,
		"type":   "agent",
		"status": "in_progress",
		"fields": fields,
	}
}

// TestSpawnAgent_SingleClaim_StalledAgentAllowsRespawn is the core regression
// for ka-0000000286: an incumbent that is open + agent_state=working but has made
// NO progress in > ProgressStaleThreshold (a livelocked zombie whose pod is still
// Running) must NOT block a fresh spawn. Before this fix the guard keyed only on
// terminal agent_state, so a working-but-wedged agent squatted the task forever.
func TestSpawnAgent_SingleClaim_StalledAgentAllowsRespawn(t *testing.T) {
	var requests []spawnReq
	stale := time.Now().Add(-12 * time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(singleClaimHandler(t, "bd-fresh",
		[]map[string]any{agentRowWithFields("bd-zombie", "working",
			map[string]string{"last_progress_at": stale})}, &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "reclaim-bot", "gardenio", "gd-step-1", "crew", "",
		map[string]string{"single_claim_task": "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "bd-fresh" {
		t.Errorf("expected fresh spawn bd-fresh (incumbent is livelocked), got %s", id)
	}
	if !didCreate(requests) {
		t.Error("expected a create POST — a progress-stalled incumbent must not block reclaim")
	}
}

// TestSpawnAgent_SingleClaim_FreshProgressBlocksDuplicate confirms the fix does
// not over-fire: an incumbent that is working AND has stamped recent progress
// still blocks a duplicate spawn (the original single-claim contract holds).
func TestSpawnAgent_SingleClaim_FreshProgressBlocksDuplicate(t *testing.T) {
	var requests []spawnReq
	fresh := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(singleClaimHandler(t, "bd-new",
		[]map[string]any{agentRowWithFields("bd-incumbent", "working",
			map[string]string{"last_progress_at": fresh})}, &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "dup-bot", "gardenio", "gd-step-1", "crew", "",
		map[string]string{"single_claim_task": "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "bd-incumbent" {
		t.Errorf("expected live incumbent bd-incumbent to still block, got %s", id)
	}
	if didCreate(requests) {
		t.Error("expected NO create POST — a progressing incumbent still holds its single claim")
	}
}

// TestSpawnAgent_SingleClaim_NoProgressFieldStillBlocks confirms the fail-open
// posture at the guard level: an incumbent with NO last_progress_at (e.g. an
// agent bead created by an older controller, or one still on its first turn) is
// treated as live, so the single-claim guard keeps blocking. The fix only ever
// RELEASES a demonstrably-stalled claim — it never manufactures a reclaim from
// missing data.
func TestSpawnAgent_SingleClaim_NoProgressFieldStillBlocks(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(singleClaimHandler(t, "bd-new",
		[]map[string]any{agentRow("bd-incumbent", "working")}, &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "dup-bot", "gardenio", "gd-step-1", "crew", "",
		map[string]string{"single_claim_task": "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "bd-incumbent" {
		t.Errorf("expected incumbent with no progress signal to still block (fail-open), got %s", id)
	}
	if didCreate(requests) {
		t.Error("expected NO create POST — a missing progress signal must not trigger a reclaim")
	}
}

// TestStampAgentProgress verifies StampAgentProgress writes last_progress_at as
// an RFC3339 timestamp via the field-patch path, and that the written value
// round-trips through the guard's parser as fresh.
func TestStampAgentProgress(t *testing.T) {
	var patch map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// No pre-existing fields, so last_progress_at is added as a string.
			_ = json.NewEncoder(w).Encode(beadJSON{ID: "bd-agent"})
		case http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]json.RawMessage
			_ = json.Unmarshal(body, &parsed)
			_ = json.Unmarshal(parsed["fields"], &patch)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	now := time.Date(2026, 8, 7, 18, 30, 0, 0, time.UTC)
	if err := c.StampAgentProgress(context.Background(), "bd-agent", now); err != nil {
		t.Fatalf("StampAgentProgress: %v", err)
	}
	got, ok := patch["last_progress_at"]
	if !ok {
		t.Fatalf("expected last_progress_at in patch, got %v", patch)
	}
	if got != now.Format(time.RFC3339) {
		t.Errorf("last_progress_at = %v, want %v", got, now.Format(time.RFC3339))
	}
	// Round-trips through the guard's parser as fresh.
	if AgentProgressStalled(map[string]string{"last_progress_at": got},
		now.Add(1*time.Minute), ProgressStaleThreshold) {
		t.Error("a just-stamped progress time must not read as stalled")
	}
}
