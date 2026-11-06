package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// subsForTest returns non-enriched subscriptions for an agent ID. Production
// callers of LoadHumanSurveyConfig / roleAllowed are expected to enrich via
// advice.EnrichAgentSubscriptions; these tests use non-enriched subs because
// they exercise the loader and allowlist logic in isolation.
func subsForTest(agentID string) []string {
	return advice.BuildAgentSubscriptions(agentID, nil)
}

// --- DefaultHumanSurveyConfig / LoadHumanSurveyConfig ---

func TestDefaultHumanSurveyConfig_LandsDark(t *testing.T) {
	cfg := DefaultHumanSurveyConfig()
	if cfg.Enabled {
		t.Errorf("default human survey config must land dark (Enabled=false)")
	}
	if cfg.RatingScaleMax != 1 {
		t.Errorf("expected rating scale max 1 (canonical −1/0/+1), got %d", cfg.RatingScaleMax)
	}
	if cfg.MinRating != -1 {
		t.Errorf("expected min rating -1 (worse-than-nothing option), got %d", cfg.MinRating)
	}
	if cfg.ExpireHours != 168 {
		t.Errorf("expected expire hours 168, got %d", cfg.ExpireHours)
	}
	if len(cfg.AllowedRoles) == 0 {
		t.Errorf("expected AllowedRoles to include default roles")
	}
}

func TestLoadHumanSurveyConfig_NoBeadsReturnsDefaults(t *testing.T) {
	lister := &mockConfigBeadLister{}
	cfg := LoadHumanSurveyConfig(context.Background(), lister, subsForTest("gardenio/crew/pr1-9x"))
	if cfg.Enabled {
		t.Errorf("empty config must keep Enabled=false")
	}
	if cfg.SkipIfSessionSecondsLt != 60 {
		t.Errorf("expected skip_if_session_seconds_lt=60, got %d", cfg.SkipIfSessionSecondsLt)
	}
}

func TestLoadHumanSurveyConfig_RoleOverridesGlobal(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       HumanSurveyConfigCategory,
				Labels:      []string{"global"},
				Description: `{"enabled":false,"expire_hours":48}`,
			},
			{
				Title:       HumanSurveyConfigCategory,
				Labels:      []string{"role:crew"},
				Description: `{"enabled":true,"rating_scale_max":10,"ask_feedback":false}`,
			},
		},
	}
	cfg := LoadHumanSurveyConfig(context.Background(), lister, subsForTest("gardenio/crew/pr1-9x"))
	if !cfg.Enabled {
		t.Errorf("role override should flip Enabled=true")
	}
	if cfg.RatingScaleMax != 10 {
		t.Errorf("role override should set rating_scale_max=10, got %d", cfg.RatingScaleMax)
	}
	if cfg.AskFeedback {
		t.Errorf("role override should set ask_feedback=false")
	}
}

func TestLoadHumanSurveyConfig_RatingScaleFallbackOnZero(t *testing.T) {
	// A layer that doesn't explicitly set rating_scale_max should keep the default
	// (json.Unmarshal leaves zero-valued ints when the key is missing; the loader
	// must restore the default).
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       HumanSurveyConfigCategory,
				Labels:      []string{"global"},
				Description: `{"enabled":true}`,
			},
		},
	}
	cfg := LoadHumanSurveyConfig(context.Background(), lister, subsForTest("gardenio/crew/pr1-9x"))
	if cfg.RatingScaleMax != 1 {
		t.Errorf("missing rating_scale_max should fall back to default 1, got %d", cfg.RatingScaleMax)
	}
	if cfg.MinRating != -1 {
		t.Errorf("missing min_rating should fall back to default -1, got %d", cfg.MinRating)
	}
	if cfg.ExpireHours != 168 {
		t.Errorf("missing expire_hours should fall back to default 168, got %d", cfg.ExpireHours)
	}
}

// Regression: a flat agent ID (e.g. "thread-1776-849") has no "/" separator,
// so BuildAgentSubscriptions alone does not surface role: or project:gardenio
// labels. If the caller fails to enrich, a config bead scoped to role:thread
// (or project:gardenio) silently misses. This test documents that subs-in,
// subs-out contract by passing explicitly enriched subs.
func TestLoadHumanSurveyConfig_FlatAgentIDRequiresEnrichment(t *testing.T) {
	lister := &mockConfigBeadLister{
		beads: []*beadsapi.BeadDetail{
			{
				Title:       HumanSurveyConfigCategory,
				Labels:      []string{"role:thread"},
				Description: `{"enabled":true}`,
			},
		},
	}

	// Non-enriched subs for a flat ID do NOT include role:thread.
	nonEnriched := advice.BuildAgentSubscriptions("thread-1776-849", nil)
	cfg := LoadHumanSurveyConfig(context.Background(), lister, nonEnriched)
	if cfg.Enabled {
		t.Errorf("non-enriched flat ID must not match role:thread config")
	}

	// Enriched subs (as if the agent bead had role=thread) do match.
	enriched := append(nonEnriched, "role:thread")
	cfg = LoadHumanSurveyConfig(context.Background(), lister, enriched)
	if !cfg.Enabled {
		t.Errorf("enriched subs with role:thread should match role:thread config")
	}
}

// --- roleAllowed ---

func TestRoleAllowed_EmptyAllowlistAllowsAny(t *testing.T) {
	if !roleAllowed(subsForTest("gardenio/crew/pr1"), nil) {
		t.Errorf("empty allowlist should allow any role")
	}
	if !roleAllowed(subsForTest("gardenio/gardener/team-lead"), []string{}) {
		t.Errorf("empty allowlist should allow any role")
	}
}

func TestRoleAllowed_MatchesRoleSubscription(t *testing.T) {
	if !roleAllowed(subsForTest("gardenio/crew/pr1"), []string{"crew"}) {
		t.Errorf("agent with role:crew should pass allowlist=[crew]")
	}
	if !roleAllowed(subsForTest("gardenio/crew/pr1"), []string{"gardener", "crew"}) {
		t.Errorf("agent with role:crew should pass allowlist=[gardener,crew]")
	}
}

func TestRoleAllowed_RejectsMismatchedRole(t *testing.T) {
	if roleAllowed(subsForTest("gardenio/researcher/x"), []string{"crew", "gardener"}) {
		t.Errorf("role:researcher should not pass allowlist=[crew,gardener]")
	}
}

// Regression: roleAllowed operates on subs, so flat-ID agents only pass the
// allowlist when the caller enriches. Without enrichment, a thread-spawned
// agent with a bead-level role=gardener would be rejected by any allowlist.
func TestRoleAllowed_FlatAgentIDRequiresEnrichment(t *testing.T) {
	flatID := "thread-1776-849"
	nonEnriched := advice.BuildAgentSubscriptions(flatID, nil)
	if roleAllowed(nonEnriched, []string{"gardener"}) {
		t.Errorf("non-enriched flat ID should not satisfy allowlist=[gardener]")
	}
	enriched := append(nonEnriched, "role:gardener")
	if !roleAllowed(enriched, []string{"gardener"}) {
		t.Errorf("enriched subs with role:gardener should satisfy allowlist=[gardener]")
	}
}

// --- Bridge-resolved channel routing (ka-0000005949, ka-0000002021) ---

// surveyDaemonStub is a minimal beads-daemon HTTP stub for postHumanSurveyPrompt
// tests. It serves /v1/beads (list) and /v1/beads/<id> (get/patch).
type surveyDaemonStub struct {
	mu      sync.Mutex
	beads   []map[string]any
	patches []map[string]any
}

func (s *surveyDaemonStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			s.mu.Lock()
			resp := map[string]any{"beads": s.beads, "total": len(s.beads)}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, b := range s.beads {
				if b["id"] == id {
					_ = json.NewEncoder(w).Encode(b)
					return
				}
			}
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			body, _ := io.ReadAll(r.Body)
			var p map[string]any
			_ = json.Unmarshal(body, &p)
			s.mu.Lock()
			s.patches = append(s.patches, p)
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	})
}

// setupSurveyDaemon points the package-global daemon at a stub server and
// restores it on cleanup.
func setupSurveyDaemon(t *testing.T, stub *surveyDaemonStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

// stubBridgeRequests swaps httpPostHumanSurveyPrompt to capture requests
// without making HTTP calls.
func stubBridgeRequests(t *testing.T, returnTS string) *[]humanSurveyPromptRequest {
	t.Helper()
	captured := &[]humanSurveyPromptRequest{}
	old := httpPostHumanSurveyPrompt
	httpPostHumanSurveyPrompt = func(_ context.Context, _ string, req humanSurveyPromptRequest) (string, error) {
		*captured = append(*captured, req)
		return returnTS, nil
	}
	t.Cleanup(func() { httpPostHumanSurveyPrompt = old })
	return captured
}

// surveyConfigBead returns an agent-bead JSON object with the given fields
// and a created_at well before now (so SkipIfSessionSecondsLt does not block).
func surveyAgentBead(id string, fields map[string]string) map[string]any {
	raw, _ := json.Marshal(fields)
	return map[string]any{
		"id":         id,
		"title":      id,
		"type":       "agent",
		"status":     "open",
		"fields":     json.RawMessage(raw),
		"created_at": time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
		"updated_at": time.Now().Format(time.RFC3339),
	}
}

// surveyConfigBead returns a JSON config bead for human-survey-config, scoped
// to the given labels with the given description JSON.
func surveyConfigBead(labels []string, descJSON string) map[string]any {
	return map[string]any{
		"id":          "gd-cfg-" + strings.Join(labels, "-"),
		"title":       HumanSurveyConfigCategory,
		"type":        "config",
		"status":      "open",
		"labels":      labels,
		"description": descJSON,
		"created_at":  time.Now().Format(time.RFC3339),
		"updated_at":  time.Now().Format(time.RFC3339),
	}
}

// TestPostHumanSurveyPrompt_SendsEmptyChannelWhenNoCoords: when a
// JIRA-spawned agent has no Slack thread coords, the agent posts to the
// bridge with an empty channel; the BRIDGE is responsible for resolving the
// agent card location and routing the survey there (ka-0000002021).
func TestPostHumanSurveyPrompt_SendsEmptyChannelWhenNoCoords(t *testing.T) {
	const agentID = "gd-jira-agent-1"
	stub := &surveyDaemonStub{
		beads: []map[string]any{
			// JIRA-spawned agent bead — NO slack_thread_channel/ts fields.
			surveyAgentBead(agentID, map[string]string{
				"agent":   "fsm-306-triage",
				"project": "gardenio",
				"role":    "crew",
			}),
			surveyConfigBead([]string{"global"},
				`{"enabled":true,"skip_if_session_seconds_lt":0}`),
		},
	}
	setupSurveyDaemon(t, stub)
	captured := stubBridgeRequests(t, "1700000000.000100")

	t.Setenv("SLACK_BRIDGE_URL", "http://stub")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")

	if err := postHumanSurveyPrompt(context.Background(), agentID); err != nil {
		t.Fatalf("postHumanSurveyPrompt: %v", err)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 bridge POST, got %d", len(*captured))
	}
	got := (*captured)[0]
	if got.AgentBeadID != agentID {
		t.Errorf("expected agent_bead_id=%q, got %q", agentID, got.AgentBeadID)
	}
	if got.Channel != "" {
		t.Errorf("expected empty channel (bridge resolves), got %q", got.Channel)
	}
	if got.ThreadTS != "" {
		t.Errorf("expected empty thread_ts, got %q", got.ThreadTS)
	}
}

// TestPostHumanSurveyPrompt_SoftSkipsOnUnresolvedChannel: when the bridge
// returns "channel is required" (no thread coords AND no agent card), the
// agent soft-skips — gb stop must not surface an error.
func TestPostHumanSurveyPrompt_SoftSkipsOnUnresolvedChannel(t *testing.T) {
	const agentID = "gd-jira-agent-2"
	stub := &surveyDaemonStub{
		beads: []map[string]any{
			surveyAgentBead(agentID, map[string]string{
				"agent": "fsm-307-triage", "project": "gardenio", "role": "crew",
			}),
			surveyConfigBead([]string{"global"},
				`{"enabled":true,"skip_if_session_seconds_lt":0}`),
		},
	}
	setupSurveyDaemon(t, stub)

	// Stub the bridge call to return the bridge's 400 message verbatim.
	old := httpPostHumanSurveyPrompt
	httpPostHumanSurveyPrompt = func(_ context.Context, _ string, _ humanSurveyPromptRequest) (string, error) {
		return "", fmt.Errorf("bridge returned HTTP 400: channel is required (and no agent card found to resolve)")
	}
	t.Cleanup(func() { httpPostHumanSurveyPrompt = old })

	t.Setenv("SLACK_BRIDGE_URL", "http://stub")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")

	if err := postHumanSurveyPrompt(context.Background(), agentID); err != nil {
		t.Fatalf("expected soft-skip on unresolved channel, got error: %v", err)
	}
}

// TestPostHumanSurveyPrompt_PrefersThreadCoords: when the agent has real
// Slack thread coords, those are sent through to the bridge — bridge does
// NOT need to resolve the card location.
func TestPostHumanSurveyPrompt_PrefersThreadCoords(t *testing.T) {
	const agentID = "gd-thread-agent-3"
	stub := &surveyDaemonStub{
		beads: []map[string]any{
			surveyAgentBead(agentID, map[string]string{
				"agent":                "thread-agent",
				"project":              "gardenio",
				"role":                 "crew",
				"slack_thread_channel": "C-THREAD",
				"slack_thread_ts":      "1700000000.000001",
			}),
			surveyConfigBead([]string{"global"},
				`{"enabled":true,"skip_if_session_seconds_lt":0}`),
		},
	}
	setupSurveyDaemon(t, stub)
	captured := stubBridgeRequests(t, "1700000000.000200")

	t.Setenv("SLACK_BRIDGE_URL", "http://stub")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")

	if err := postHumanSurveyPrompt(context.Background(), agentID); err != nil {
		t.Fatalf("postHumanSurveyPrompt: %v", err)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 bridge POST, got %d", len(*captured))
	}
	got := (*captured)[0]
	if got.Channel != "C-THREAD" || got.ThreadTS != "1700000000.000001" {
		t.Errorf("expected thread coords (C-THREAD/1700000000.000001), got channel=%q ts=%q",
			got.Channel, got.ThreadTS)
	}
}

// channelUnresolvedBody is the exact body the bridge returns when it cannot
// resolve a channel/card for the agent (bot_human_survey.go handlePostPrompt).
const channelUnresolvedBody = `{"error":"channel is required (and no agent card found to resolve)"}`

// TestPostHumanSurveyPrompt_SoftSkipsViaRealHTTPOn400 exercises the REAL
// httpPostHumanSurveyPrompt (not the stubbed var) against an httptest server
// that returns the bridge's 400 channel-unresolved body. It closes the test
// gap from ka-0000006174: the prior soft-skip test stubbed
// httpPostHumanSurveyPrompt directly, so the body-discard layer was never
// exercised — the unit test passed while production printed a spurious
// "human-survey prompt skipped: bridge returned HTTP 400" on every
// channel-unresolved stop. With the body embedded, isSurveyChannelUnresolved
// matches and postHumanSurveyPrompt soft-skips (returns nil).
func TestPostHumanSurveyPrompt_SoftSkipsViaRealHTTPOn400(t *testing.T) {
	const agentID = "gd-jira-agent-real-http"
	stub := &surveyDaemonStub{
		beads: []map[string]any{
			surveyAgentBead(agentID, map[string]string{
				"agent": "fsm-307-triage", "project": "gardenio", "role": "crew",
			}),
			surveyConfigBead([]string{"global"},
				`{"enabled":true,"skip_if_session_seconds_lt":0}`),
		},
	}
	setupSurveyDaemon(t, stub)

	// A bridge that always returns the channel-unresolved 400. We do NOT stub
	// httpPostHumanSurveyPrompt — the default (real) impl runs against this.
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, channelUnresolvedBody, http.StatusBadRequest)
	}))
	t.Cleanup(bridge.Close)

	t.Setenv("SLACK_BRIDGE_URL", bridge.URL)
	t.Setenv("SLACK_BRIDGE_TOKEN", "")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")

	if err := postHumanSurveyPrompt(context.Background(), agentID); err != nil {
		t.Fatalf("expected soft-skip on real-HTTP 400 channel-unresolved, got error: %v", err)
	}
}

// TestHTTPPostHumanSurveyPrompt_EmbedsBodyOn400 asserts the helper embeds the
// bridge response body in its error so the channel-unresolved case is
// classifiable. This is the precise unit-level guard for the ka-0000006174 fix.
func TestHTTPPostHumanSurveyPrompt_EmbedsBodyOn400(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, channelUnresolvedBody, http.StatusBadRequest)
	}))
	t.Cleanup(bridge.Close)

	_, err := httpPostHumanSurveyPrompt(context.Background(), bridge.URL, humanSurveyPromptRequest{
		AgentBeadID: "gd-x",
	})
	if err == nil {
		t.Fatal("expected error on 400, got nil")
	}
	if !strings.Contains(err.Error(), "channel is required") {
		t.Errorf("error must embed bridge body containing %q, got: %v", "channel is required", err)
	}
	if !isSurveyChannelUnresolved(err) {
		t.Errorf("isSurveyChannelUnresolved must classify the embedded-body error as soft-skip, got: %v", err)
	}
}

// TestHTTPPostHumanSurveyPrompt_NonChannel400StillErrors guards the precision
// of the fix: a 400 that is NOT the channel-unresolved case (e.g. a malformed
// request) must still surface as a real error, not be silently soft-skipped.
func TestHTTPPostHumanSurveyPrompt_NonChannel400StillErrors(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
	}))
	t.Cleanup(bridge.Close)

	_, err := httpPostHumanSurveyPrompt(context.Background(), bridge.URL, humanSurveyPromptRequest{
		AgentBeadID: "gd-x",
	})
	if err == nil {
		t.Fatal("expected error on non-channel 400, got nil")
	}
	if isSurveyChannelUnresolved(err) {
		t.Errorf("a non-channel 400 must NOT be classified as soft-skip, got: %v", err)
	}
}
