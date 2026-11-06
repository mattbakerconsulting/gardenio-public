// Integration-style tests for the gitlab-bridge trigger wiring added in
// Phase 1 of the GitLab pipeline-failure trigger plan. The tests cover:
//
//  1. Pipeline trigger fires notify action on first failed pipeline match.
//  2. Same pipeline ID is deduped on subsequent evaluations.
//  3. cooldown_minutes (global triggerCooldown) prevents refire inside the
//     cooldown window.
//  4. max_concurrent honored — only N actions fire per evaluation tick.
//  5. CatchUp-then-Evaluate produces 0 actions when pre-existing
//     trigger-created beads cover the matches (fire-on-startup guard).
//  6. runTriggerEngine loop calls Evaluate repeatedly until ctx is cancelled.
//
// Tests stand up a real GitLabClient pointed at an httptest server so the
// trigger-source code exercises the HTTP path; the bead daemon is replaced
// with a mock implementing bridge.TriggerBeadClient.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// fakeDaemon is a minimal bridge.TriggerBeadClient used by these tests.
// It returns a fixed list of beads from ListBeadsFiltered (filtered by the
// requested types/labels) and records every action the trigger engine
// performs against it so assertions can be made post-evaluation.
type fakeDaemon struct {
	mu sync.Mutex

	// Seed data — beads that the daemon "knows about" (trigger defs, existing
	// trigger-created task beads used for CatchUp, etc.).
	beads []*beadsapi.BeadDetail

	// Recorded calls.
	createdBeads  []beadsapi.CreateBeadRequest
	updatedFields []updateCall
	spawnedNames  []string
	notifierCalls []notifierCall
}

type updateCall struct {
	BeadID string
	Fields map[string]string
}

type notifierCall struct {
	Title   string
	Message string
	Channel string
}

func (f *fakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if !matchesTypes(b, q.Types) {
			continue
		}
		if !matchesLabels(b, q.Labels) {
			continue
		}
		out = append(out, b)
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func matchesTypes(b *beadsapi.BeadDetail, types []string) bool {
	if len(types) == 0 {
		return true
	}
	return slices.Contains(types, b.Type)
}

func matchesLabels(b *beadsapi.BeadDetail, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := make(map[string]bool, len(b.Labels))
	for _, l := range b.Labels {
		have[l] = true
	}
	for _, r := range required {
		if !have[r] {
			return false
		}
	}
	return true
}

func (f *fakeDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdBeads = append(f.createdBeads, req)
	return fmt.Sprintf("gd-fake-%d", len(f.createdBeads)), nil
}

func (f *fakeDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updatedFields = append(f.updatedFields, updateCall{BeadID: beadID, Fields: fields})
	return nil
}

// UpdateBeadFieldsTyped is a stub; this binary's tests do not exercise the
// typed-field patch path (added to TriggerBeadClient for the gardener run-counter
// mirror, ka-0000007976).
func (f *fakeDaemon) UpdateBeadFieldsTyped(_ context.Context, _ string, _ map[string]any) error {
	return nil
}

// UpdateBead is a stub; this binary's tests do not exercise the credential+NATS
// rehydration trigger path that calls it (added to TriggerBeadClient in C3).
func (f *fakeDaemon) UpdateBead(_ context.Context, _ string, _ beadsapi.UpdateBeadRequest) error {
	return nil
}

func (f *fakeDaemon) SpawnAgent(_ context.Context, agentName, _, _, _, _ string, _ ...map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawnedNames = append(f.spawnedNames, agentName)
	return "gd-fake-agent", nil
}

func (f *fakeDaemon) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return map[string]beadsapi.ProjectInfo{}, nil
}

func (f *fakeDaemon) AddDependency(_ context.Context, _, _, _, _ string) error {
	return nil
}

func (f *fakeDaemon) AddComment(_ context.Context, _, _, _ string) error {
	return nil
}

func (f *fakeDaemon) AddLabel(_ context.Context, _, _ string) error {
	return nil
}

func (f *fakeDaemon) CloseBead(_ context.Context, _ string, _ map[string]string) error {
	return nil
}

func (f *fakeDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.beads {
		if b != nil && b.ID == beadID {
			return b, nil
		}
	}
	return nil, fmt.Errorf("bead %s not found", beadID)
}

// MRBeadForTask is a stub; this binary's tests do not exercise the gardener
// progress mr_merged kind.
func (f *fakeDaemon) MRBeadForTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

func (f *fakeDaemon) recordNotify(_ context.Context, title, message, channel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifierCalls = append(f.notifierCalls, notifierCall{Title: title, Message: message, Channel: channel})
}

// newPipelineServer returns an httptest.Server that serves
// GET /api/v4/projects/<encoded>/pipelines with the supplied pipeline list.
// It records the number of pipeline list calls so tests can confirm the
// evaluator polls GitLab as expected.
func newPipelineServer(t *testing.T, pipelines []bridge.GitLabPipeline) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || r.URL.Path[len(r.URL.Path)-len("/pipelines"):] != "/pipelines" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pipelines)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// helper — build a pipeline trigger bead on a given project path.
func pipelineTriggerBead(id string, overrides map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"enabled":         "true",
		"project_path":    "acme/widgets",
		"event_type":      "pipeline",
		"pipeline_status": "failed",
		"action":          "notify",
		"max_concurrent":  "10",
		// spawn_agent overrides need a project or the fire-time project
		// guard (D6) refuses the spawn; notify ignores it.
		"project": "gardenio",
		"prompt":  "Pipeline {{gitlab_pipeline_id}} failed on {{gitlab_pipeline_ref}}",
	}
	maps.Copy(fields, overrides)
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Title:  "Failed pipelines " + id,
		Labels: []string{"trigger:gitlab"},
		Fields: fields,
	}
}

func newEngine(daemon *fakeDaemon, client *bridge.GitLabClient) *bridge.TriggerEngine {
	engine := bridge.NewTriggerEngine(bridge.TriggerEngineConfig{
		Daemon:   daemon,
		Notifier: daemon.recordNotify,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	engine.RegisterSource(bridge.NewGitLabTriggerSourceWithLogger(client, slog.Default()))
	return engine
}

// TestPipelineTrigger_NotifyFiresOnce verifies the primary happy path for
// Phase 1: a failed pipeline matches a trigger bead and the notify action
// is dispatched exactly once. A second Evaluate must not refire.
func TestPipelineTrigger_NotifyFiresOnce(t *testing.T) {
	// Set last_checked in the past (beyond cooldown) so the trigger is past
	// its bootstrap guard — simulating a trigger that has been evaluated
	// before and now sees a genuinely new failed pipeline on the next tick.
	pastChecked := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	daemon := &fakeDaemon{
		beads: []*beadsapi.BeadDetail{pipelineTriggerBead("gd-t1", map[string]string{
			"last_checked": pastChecked,
		})},
	}
	pipelines := []bridge.GitLabPipeline{
		{ID: 42, Status: "failed", Ref: "main", SHA: "abc123", WebURL: "https://gitlab.example/pipelines/42"},
	}
	srv, _ := newPipelineServer(t, pipelines)
	client := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "test"})

	engine := newEngine(daemon, client)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	got := len(daemon.notifierCalls)
	daemon.mu.Unlock()
	if got != 1 {
		t.Fatalf("notifier invoked %d times after first Evaluate, want 1", got)
	}

	// Second Evaluate with the same pipeline must dedup.
	engine.Evaluate(context.Background())
	daemon.mu.Lock()
	got = len(daemon.notifierCalls)
	daemon.mu.Unlock()
	if got != 1 {
		t.Fatalf("notifier invoked %d times after second Evaluate, want 1 (dedup)", got)
	}

	// Seen map should contain the pipeline key.
	seen := engine.SeenKeys("gd-t1")
	wantKey := "acme/widgets/pipeline/42"
	if !seen[wantKey] {
		t.Fatalf("seen map missing %q, got %#v", wantKey, seen)
	}
}

// TestPipelineTrigger_CooldownPreventsRefire verifies that the engine-level
// cooldown window (triggerCooldown, 30s) prevents a second Evaluate within
// the cooldown from running the source.Evaluate path at all. This protects
// GitLab from hot-loop polling when a runTriggerEngine tick fires faster
// than the cooldown.
func TestPipelineTrigger_CooldownPreventsRefire(t *testing.T) {
	// Seed last_checked to now so the trigger is inside its cooldown window.
	now := time.Now().UTC().Format(time.RFC3339)
	trig := pipelineTriggerBead("gd-t-cd", map[string]string{"last_checked": now})
	daemon := &fakeDaemon{beads: []*beadsapi.BeadDetail{trig}}

	pipelines := []bridge.GitLabPipeline{
		{ID: 101, Status: "failed", Ref: "main", SHA: "x", WebURL: "https://gitlab.example/p/101"},
	}
	srv, hits := newPipelineServer(t, pipelines)
	client := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "test"})

	engine := newEngine(daemon, client)
	engine.Evaluate(context.Background())

	if h := hits.Load(); h != 0 {
		t.Fatalf("GitLab /pipelines hit %d times during cooldown, want 0", h)
	}
	daemon.mu.Lock()
	got := len(daemon.notifierCalls)
	daemon.mu.Unlock()
	if got != 0 {
		t.Fatalf("notifier invoked %d times during cooldown, want 0", got)
	}
}

// TestPipelineTrigger_MaxConcurrentHonored verifies that when N failed
// pipelines match but the trigger declares max_concurrent=1, only one
// action fires per evaluation. The remaining pipeline(s) should remain
// unseen so they can be picked up on the next tick.
func TestPipelineTrigger_MaxConcurrentHonored(t *testing.T) {
	// Set last_checked in the past so the bootstrap guard (which suppresses
	// first-evaluation firing for brand-new triggers) does not kick in.
	pastChecked := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	trig := pipelineTriggerBead("gd-t-mc", map[string]string{
		"max_concurrent": "1",
		"last_checked":   pastChecked,
	})
	daemon := &fakeDaemon{beads: []*beadsapi.BeadDetail{trig}}

	pipelines := []bridge.GitLabPipeline{
		{ID: 1, Status: "failed", Ref: "main", SHA: "a"},
		{ID: 2, Status: "failed", Ref: "main", SHA: "b"},
		{ID: 3, Status: "failed", Ref: "main", SHA: "c"},
	}
	srv, _ := newPipelineServer(t, pipelines)
	client := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "test"})

	engine := newEngine(daemon, client)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	got := len(daemon.notifierCalls)
	daemon.mu.Unlock()
	if got != 1 {
		t.Fatalf("notifier invoked %d times, want 1 (max_concurrent=1)", got)
	}

	seen := engine.SeenKeys("gd-t-mc")
	if len(seen) != 1 {
		t.Fatalf("seen map size = %d, want 1 (deferred matches must not be marked seen)", len(seen))
	}
}

// TestPipelineTrigger_CatchUpSuppressesStartupStorm is the critical Phase 1
// regression test. It simulates a bridge restart: pre-existing task beads
// (created by a previous run of this trigger) exist for some failed pipelines.
// After CatchUp seeds the dedup state, Evaluate must NOT refire notify for
// those pipelines.
func TestPipelineTrigger_CatchUpSuppressesStartupStorm(t *testing.T) {
	trig := pipelineTriggerBead("gd-t-cu", map[string]string{
		"action": "create_bead", // ensure we have bead-producing action so CatchUp has data
	})
	// Pre-existing task beads that a previous run of this trigger created.
	existing := []*beadsapi.BeadDetail{
		{
			ID:     "gd-task-1",
			Type:   "task",
			Kind:   "issue",
			Status: "open",
			Labels: []string{"trigger:gd-t-cu"},
			Fields: map[string]string{"gitlab_key": "acme/widgets/pipeline/10"},
		},
		{
			ID:     "gd-task-2",
			Type:   "task",
			Kind:   "issue",
			Status: "closed",
			Labels: []string{"trigger:gd-t-cu"},
			Fields: map[string]string{"gitlab_key": "acme/widgets/pipeline/11"},
		},
	}
	daemon := &fakeDaemon{beads: append([]*beadsapi.BeadDetail{trig}, existing...)}

	pipelines := []bridge.GitLabPipeline{
		{ID: 10, Status: "failed", Ref: "main"},
		{ID: 11, Status: "failed", Ref: "main"},
	}
	srv, _ := newPipelineServer(t, pipelines)
	client := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "test"})

	engine := newEngine(daemon, client)
	engine.CatchUp(context.Background())
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	createdAfter := len(daemon.createdBeads)
	daemon.mu.Unlock()
	if createdAfter != 0 {
		t.Fatalf("engine created %d beads on startup, want 0 (CatchUp should have suppressed refire)", createdAfter)
	}

	// A new failed pipeline that was NOT in the pre-existing beads should still
	// fire on the next Evaluate — CatchUp must not over-seed.
	pipelines = append(pipelines, bridge.GitLabPipeline{ID: 12, Status: "failed", Ref: "main"})
	// Spin up a fresh pipeline server with the new list so the next eval sees it.
	srv2, _ := newPipelineServer(t, pipelines)
	client2 := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv2.URL, Token: "test"})

	// Reset last_checked so cooldown does not block the next Evaluate.
	daemon.mu.Lock()
	daemon.beads[0].Fields["last_checked"] = ""
	daemon.mu.Unlock()

	engine2 := newEngine(daemon, client2)
	engine2.CatchUp(context.Background())
	engine2.Evaluate(context.Background())

	daemon.mu.Lock()
	createdFinal := len(daemon.createdBeads)
	daemon.mu.Unlock()
	if createdFinal != 1 {
		t.Fatalf("engine created %d beads for new pipeline, want 1", createdFinal)
	}
}

// TestPipelineTrigger_BootstrapSeedsWithoutFiring verifies the thundering-herd
// guard: when CatchUp runs for a brand-new trigger (no last_checked, no prior
// beads), it evaluates the source and seeds all current matches into the seen
// map WITHOUT firing any actions. Only truly new events appearing after the
// bootstrap should fire on subsequent Evaluate calls.
func TestPipelineTrigger_BootstrapSeedsWithoutFiring(t *testing.T) {
	// Trigger with no last_checked — simulates a brand-new trigger.
	trig := pipelineTriggerBead("gd-t-boot", map[string]string{
		"action": "spawn_agent",
	})
	daemon := &fakeDaemon{beads: []*beadsapi.BeadDetail{trig}}

	// Three pre-existing failed pipelines that exist BEFORE the trigger was created.
	pipelines := []bridge.GitLabPipeline{
		{ID: 100, Status: "failed", Ref: "main", SHA: "aaa"},
		{ID: 101, Status: "failed", Ref: "main", SHA: "bbb"},
		{ID: 102, Status: "failed", Ref: "main", SHA: "ccc"},
	}
	srv, _ := newPipelineServer(t, pipelines)
	client := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "test"})

	engine := newEngine(daemon, client)
	// CatchUp should bootstrap — seed all current matches without firing.
	engine.CatchUp(context.Background())

	// Bootstrap must NOT fire any actions.
	daemon.mu.Lock()
	spawned := len(daemon.spawnedNames)
	notified := len(daemon.notifierCalls)
	created := len(daemon.createdBeads)
	daemon.mu.Unlock()
	if spawned != 0 {
		t.Fatalf("bootstrap spawned %d agents, want 0 (thundering herd!)", spawned)
	}
	if notified != 0 {
		t.Fatalf("bootstrap sent %d notifications, want 0", notified)
	}
	if created != 0 {
		t.Fatalf("bootstrap created %d beads, want 0", created)
	}

	// All pre-existing matches must be in the seen map.
	seen := engine.SeenKeys("gd-t-boot")
	if len(seen) != 3 {
		t.Fatalf("seen map size = %d after bootstrap, want 3", len(seen))
	}
	for _, id := range []int{100, 101, 102} {
		key := fmt.Sprintf("acme/widgets/pipeline/%d", id)
		if !seen[key] {
			t.Errorf("seen map missing %q after bootstrap", key)
		}
	}

	// Now simulate a NEW pipeline failure appearing after the trigger was created.
	// Add pipeline 103 to the server response.
	pipelines = append(pipelines, bridge.GitLabPipeline{
		ID: 103, Status: "failed", Ref: "main", SHA: "ddd",
	})
	srv2, _ := newPipelineServer(t, pipelines)
	client2 := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv2.URL, Token: "test"})

	// Build a new engine but copy the seen state from the first one.
	engine2 := newEngine(daemon, client2)
	for _, id := range []int{100, 101, 102} {
		key := fmt.Sprintf("acme/widgets/pipeline/%d", id)
		engine2.SeedSeen("gd-t-boot", key)
	}
	// Reset last_checked so cooldown doesn't block (simulate next poll tick).
	daemon.mu.Lock()
	daemon.beads[0].Fields["last_checked"] = ""
	daemon.mu.Unlock()

	engine2.Evaluate(context.Background())

	// Only the NEW pipeline (103) should trigger a spawn.
	daemon.mu.Lock()
	spawnedAfter := len(daemon.spawnedNames)
	daemon.mu.Unlock()
	if spawnedAfter != 1 {
		t.Fatalf("second eval spawned %d agents, want 1 (only pipeline 103)", spawnedAfter)
	}
}

// TestPipelineTrigger_RuntimeBootstrapSeedsWithoutFiring verifies the
// thundering-herd guard for triggers created AFTER engine startup (so
// catchUpSource never saw them). When Evaluate runs for a brand-new trigger
// — no last_checked, no prior beads, no in-memory seen state — the engine
// must seed all current matches into the seen map WITHOUT firing any
// actions. Only truly new events appearing after the bootstrap should fire
// on subsequent Evaluate calls.
//
// Regression test for the "trigger going crazy" incident where a newly
// created pipeline-failure trigger fired once per poll cycle for every one
// of the 100 historical failed pipelines GitLab returned.
func TestPipelineTrigger_RuntimeBootstrapSeedsWithoutFiring(t *testing.T) {
	// Trigger with no last_checked — simulates a brand-new trigger created
	// at runtime, AFTER the bridge (and its startup CatchUp) has booted.
	trig := pipelineTriggerBead("gd-t-runtime", map[string]string{
		"action":         "spawn_agent",
		"max_concurrent": "1",
	})
	daemon := &fakeDaemon{beads: []*beadsapi.BeadDetail{trig}}

	// Three pre-existing failed pipelines visible to GitLab's list API.
	pipelines := []bridge.GitLabPipeline{
		{ID: 200, Status: "failed", Ref: "main", SHA: "aaa"},
		{ID: 201, Status: "failed", Ref: "main", SHA: "bbb"},
		{ID: 202, Status: "failed", Ref: "main", SHA: "ccc"},
	}
	srv, _ := newPipelineServer(t, pipelines)
	client := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "test"})

	engine := newEngine(daemon, client)
	// NOTE: intentionally NOT calling engine.CatchUp — this simulates a
	// trigger that was created after the bridge started, so the startup
	// CatchUp never saw it. Evaluate must still bootstrap-seed.
	engine.Evaluate(context.Background())

	// First Evaluate on a brand-new trigger must NOT fire any actions.
	daemon.mu.Lock()
	spawned := len(daemon.spawnedNames)
	notified := len(daemon.notifierCalls)
	created := len(daemon.createdBeads)
	daemon.mu.Unlock()
	if spawned != 0 {
		t.Fatalf("runtime bootstrap spawned %d agents, want 0 (thundering herd!)", spawned)
	}
	if notified != 0 {
		t.Fatalf("runtime bootstrap sent %d notifications, want 0", notified)
	}
	if created != 0 {
		t.Fatalf("runtime bootstrap created %d beads, want 0", created)
	}

	// All pre-existing matches must be in the seen map.
	seen := engine.SeenKeys("gd-t-runtime")
	if len(seen) != 3 {
		t.Fatalf("seen map size = %d after runtime bootstrap, want 3", len(seen))
	}
	for _, id := range []int{200, 201, 202} {
		key := fmt.Sprintf("acme/widgets/pipeline/%d", id)
		if !seen[key] {
			t.Errorf("seen map missing %q after runtime bootstrap", key)
		}
	}

	// last_checked must be stamped so the next tick exercises the normal
	// (non-bootstrap) path and respects cooldown.
	daemon.mu.Lock()
	var lastChecked string
	for _, uc := range daemon.updatedFields {
		if uc.BeadID == "gd-t-runtime" {
			if v, ok := uc.Fields["last_checked"]; ok {
				lastChecked = v
			}
		}
	}
	daemon.mu.Unlock()
	if lastChecked == "" {
		t.Fatal("runtime bootstrap did not stamp last_checked on the trigger bead")
	}
}

// TestRunTriggerEngine_LoopsAndStops drives runTriggerEngine with a short
// interval to confirm (a) at least one Evaluate runs (the immediate call),
// (b) the ticker continues firing until ctx cancellation, and (c) the
// goroutine exits promptly when ctx is cancelled.
func TestRunTriggerEngine_LoopsAndStops(t *testing.T) {
	daemon := &fakeDaemon{
		beads: []*beadsapi.BeadDetail{pipelineTriggerBead("gd-t-loop", nil)},
	}
	pipelines := []bridge.GitLabPipeline{
		{ID: 77, Status: "failed", Ref: "main"},
	}
	srv, hits := newPipelineServer(t, pipelines)
	client := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "test"})
	engine := newEngine(daemon, client)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runTriggerEngine(ctx, engine, 25*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	// Wait until at least one Evaluate (the immediate one) has hit GitLab.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h := hits.Load(); h < 1 {
		cancel()
		<-done
		t.Fatalf("runTriggerEngine never evaluated: GitLab hits = %d", h)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runTriggerEngine did not stop within 1s of ctx cancel")
	}
}

// TestParseConfig_CICDMetrics covers the Phase G CI/CD-minutes env wiring: the
// two per-series enable flags, the project CSV, the interval/window/lookback
// overrides, and the first-party namespace/group knobs (task ka-0000002378).
func TestParseConfig_CICDMetrics(t *testing.T) {
	t.Setenv("CICD_GITLAB_METRICS_ENABLED", "true")
	t.Setenv("CICD_GITLAB_FIRSTPARTY_ENABLED", "true")
	t.Setenv("CICD_METRICS_PROJECTS", "Acme/Platform/gardenio, Acme/Platform/acme-helm-chart ")
	t.Setenv("CICD_METRICS_INTERVAL", "90s")
	t.Setenv("CICD_METRICS_WINDOW", "45m")
	t.Setenv("CICD_LOOKBACK_MONTHS", "6")
	t.Setenv("CICD_FIRSTPARTY_NAMESPACE_ID", "gid://gitlab/Group/14331212")
	t.Setenv("CICD_FIRSTPARTY_GROUP_PATH", "Acme")

	cfg := parseConfig()

	if !cfg.cicdGitLabMetricsEnabled {
		t.Error("cicdGitLabMetricsEnabled = false, want true")
	}
	if !cfg.cicdGitLabFirstPartyEnabled {
		t.Error("cicdGitLabFirstPartyEnabled = false, want true")
	}
	wantProjects := []string{"Acme/Platform/gardenio", "Acme/Platform/acme-helm-chart"}
	if len(cfg.cicdMetricsProjects) != len(wantProjects) {
		t.Fatalf("cicdMetricsProjects = %v, want %v", cfg.cicdMetricsProjects, wantProjects)
	}
	for i, p := range wantProjects {
		if cfg.cicdMetricsProjects[i] != p {
			t.Errorf("project[%d] = %q, want %q (trimmed)", i, cfg.cicdMetricsProjects[i], p)
		}
	}
	if cfg.cicdMetricsInterval != 90*time.Second {
		t.Errorf("interval = %v, want 90s", cfg.cicdMetricsInterval)
	}
	if cfg.cicdMetricsWindow != 45*time.Minute {
		t.Errorf("window = %v, want 45m", cfg.cicdMetricsWindow)
	}
	if cfg.cicdLookbackMonths != 6 {
		t.Errorf("lookbackMonths = %d, want 6", cfg.cicdLookbackMonths)
	}
	if cfg.cicdFirstPartyNamespaceID != "gid://gitlab/Group/14331212" {
		t.Errorf("namespaceID = %q, want the gid", cfg.cicdFirstPartyNamespaceID)
	}
	if cfg.cicdFirstPartyGroupPath != "Acme" {
		t.Errorf("groupPath = %q, want Acme", cfg.cicdFirstPartyGroupPath)
	}
}

// TestParseConfig_CICDMetricsDefaultsOff confirms the collector is inert by
// default: both flags off and the interval/window/lookback fall back to their
// defaults so a plain deployment is unchanged.
func TestParseConfig_CICDMetricsDefaultsOff(t *testing.T) {
	cfg := parseConfig()
	if cfg.cicdGitLabMetricsEnabled || cfg.cicdGitLabFirstPartyEnabled {
		t.Errorf("CI/CD metrics should be off by default, got job=%v firstparty=%v",
			cfg.cicdGitLabMetricsEnabled, cfg.cicdGitLabFirstPartyEnabled)
	}
	if cfg.cicdMetricsInterval != 120*time.Second {
		t.Errorf("default interval = %v, want 120s", cfg.cicdMetricsInterval)
	}
	if cfg.cicdMetricsWindow != 60*time.Minute {
		t.Errorf("default window = %v, want 60m", cfg.cicdMetricsWindow)
	}
	if cfg.cicdLookbackMonths != 12 {
		t.Errorf("default lookbackMonths = %d, want 12", cfg.cicdLookbackMonths)
	}
}

// TestBuildTriggerNotifier_FallsBackToNoticeRelay is the regression guard for
// ka-0000008124: without SLACK_BOT_TOKEN/SLACK_CHANNEL (the prod default — the
// gitlab-bridge Deployment sets neither), buildTriggerNotifier must still return
// a non-nil notifier so TriggerEngine.actionNotify does not silently no-op. The
// fallback notifier is the trigger-notice bead relay, so firing it publishes a
// closed type=message bead labeled trigger-notice that slack-bridge posts.
func TestBuildTriggerNotifier_FallsBackToNoticeRelay(t *testing.T) {
	daemon := &fakeDaemon{}
	// No slackBotToken / slackChannel — the prod-default config.
	notifier := buildTriggerNotifier(&config{}, daemon, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if notifier == nil {
		t.Fatal("buildTriggerNotifier returned nil with no Slack env — actionNotify would no-op (ka-0000008124)")
	}

	// Firing the notifier must route through the trigger-notice relay: a closed
	// type=message bead labeled trigger-notice carrying the message text.
	notifier(context.Background(), "my-trigger", "pipeline failed on main", "")

	daemon.mu.Lock()
	created := slices.Clone(daemon.createdBeads)
	daemon.mu.Unlock()
	if len(created) != 1 {
		t.Fatalf("expected 1 notice bead created, got %d", len(created))
	}
	req := created[0]
	if req.Type != "message" {
		t.Errorf("notice bead type = %q, want message", req.Type)
	}
	if !slices.Contains(req.Labels, bridge.NoticeLabel) {
		t.Errorf("notice bead labels %v missing %q", req.Labels, bridge.NoticeLabel)
	}
	var fields map[string]string
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		t.Fatalf("unmarshal notice fields: %v", err)
	}
	if fields["text"] != "pipeline failed on main" {
		t.Errorf("notice text = %q, want %q", fields["text"], "pipeline failed on main")
	}
	if fields["source_agent"] != "gitlab-bridge" {
		t.Errorf("notice source_agent = %q, want gitlab-bridge", fields["source_agent"])
	}
}

// TestBuildTriggerNotifier_DirectSlackWhenConfigured verifies the direct-Slack
// path is still selected when both SLACK_BOT_TOKEN and SLACK_CHANNEL are set:
// the notifier is non-nil and does NOT touch the notice-bead relay (no bead is
// created — it would post to Slack directly, which the fake daemon never sees).
func TestBuildTriggerNotifier_DirectSlackWhenConfigured(t *testing.T) {
	daemon := &fakeDaemon{}
	cfg := &config{slackBotToken: "xoxb-test", slackChannel: "C123"}
	notifier := buildTriggerNotifier(cfg, daemon, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if notifier == nil {
		t.Fatal("buildTriggerNotifier returned nil with Slack env set")
	}
	// Fire it; the direct-Slack path posts via the slack client (unreachable in
	// tests, error swallowed) and must NOT create a trigger-notice bead.
	notifier(context.Background(), "my-trigger", "pipeline failed on main", "")
	daemon.mu.Lock()
	n := len(daemon.createdBeads)
	daemon.mu.Unlock()
	if n != 0 {
		t.Errorf("direct-Slack path created %d notice beads, want 0", n)
	}
}

// pouring-stall dependency-graph stubs (ka-0000000957).
func (f *fakeDaemon) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}

func (f *fakeDaemon) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}

func (f *fakeDaemon) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}
