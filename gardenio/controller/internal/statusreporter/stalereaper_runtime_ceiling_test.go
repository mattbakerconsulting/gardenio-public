package statusreporter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// stubTunables is a minimal Tunables (config-watcher) stand-in for the reaper's
// dry-run arm gates. Only GetBool is consulted by the reaper.
type stubTunables struct {
	bools map[string]bool
}

func (s stubTunables) GetBool(key, _ string, compiled bool) bool {
	if v, ok := s.bools[key]; ok {
		return v
	}
	return compiled
}

// runtimeCeilingFixture builds a daemon handler serving one agent bead plus
// the graph endpoints the MR-in-flight guard walks. hasMR/mrClosed control
// whether the agent's produces graph reaches an OPEN merge-request bead. It
// also handles the GetBead probe the reap close path issues for the fallback
// wrap-up, and accepts (without capturing) the pre-reap escalation POST.
func runtimeCeilingFixture(agent beadEntry, hasMR, mrClosed bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			writeJSON(struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: []beadEntry{agent}})

		// agent → task (produces)
		case r.URL.Path == "/v1/beads/"+agent.ID+"/dependencies" && r.Method == http.MethodGet:
			deps := []beadsapi.Dependency{}
			if hasMR {
				deps = append(deps, beadsapi.Dependency{
					BeadID: agent.ID, DependsOnID: "gd-TASK-MR", Type: "produces",
				})
			}
			writeJSON(map[string]any{"dependencies": deps})

		// task → merge-request (produces)
		case r.URL.Path == "/v1/beads/gd-TASK-MR/dependencies" && r.Method == http.MethodGet:
			writeJSON(map[string]any{"dependencies": []beadsapi.Dependency{
				{BeadID: "gd-TASK-MR", DependsOnID: "gd-MR1", Type: "produces"},
			}})

		case r.URL.Path == "/v1/beads/gd-TASK-MR" && r.Method == http.MethodGet:
			writeJSON(map[string]any{
				"id": "gd-TASK-MR", "title": "task", "type": "task",
				"kind": "issue", "status": "in_progress",
			})

		case r.URL.Path == "/v1/beads/gd-MR1" && r.Method == http.MethodGet:
			status := "open"
			if mrClosed {
				status = "closed"
			}
			writeJSON(map[string]any{
				"id": "gd-MR1", "title": "MR", "type": "merge-request",
				"kind": "issue", "status": status,
			})

		// GetBead probe for the reaped agent (fallback wrap-up synth).
		case r.URL.Path == "/v1/beads/"+agent.ID && r.Method == http.MethodGet:
			writeJSON(map[string]any{
				"id": agent.ID, "title": agent.Title, "type": "agent",
				"kind": "config", "status": "open",
			})

		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// ceilingAgentEntry builds an over-/under-ceiling agent bead for the given
// primary role. spawnedBy may be empty; the per-role ceiling no longer keys
// on it (gd-X2eSin2J80), but it is still exercised as ordinary metadata.
func ceilingAgentEntry(name string, createdAgo time.Duration, role, spawnedBy string) beadEntry {
	ts := time.Now().Add(-createdAgo).UTC().Format(time.RFC3339)
	// updated_at fresh (30m ago) on purpose: a wedged-but-churning agent keeps
	// updated_at fresh, dodging the base 2h staleness rule. The ceiling must
	// catch it on created_at regardless.
	updated := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	fields := map[string]string{
		"project": "gardenio", "mode": "crew", "role": role,
		"agent": name, "agent_state": "working",
	}
	if spawnedBy != "" {
		fields["spawned_by"] = spawnedBy
	}
	return beadEntry{
		ID: "bd-" + name, Title: name, Type: "agent", Kind: "config", Status: "open",
		Fields: mustJSON(fields), CreatedAt: ts, UpdatedAt: updated,
	}
}

func newCeilingReaper(t *testing.T, srvURL string, armed bool) (*StaleReaper, *mockPodManager) {
	t.Helper()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srvURL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Close() })
	// A live pod exists for the agent so the pod-presence guard passes.
	pods := &mockPodManager{livePods: map[string]bool{"crew-gardenio-crew-wedged": true}}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	reaper.SetRuntimeCeilings(map[string]time.Duration{"crew": 6 * time.Hour})
	reaper.SetTunables(stubTunables{bools: map[string]bool{
		"STALEREAPER_RUNTIME_REAP": armed,
	}})
	return reaper, pods
}

func TestStaleReaper_RuntimeCeiling_ArmedReapsWedgedAgent(t *testing.T) {
	// 7h runtime > 6h crew ceiling, fresh updated_at (dodges the base 2h rule),
	// live pod, no MR, arm gate ON → reaped.
	agent := ceilingAgentEntry("wedged", 7*time.Hour, "crew", "trigger:gd-TRIG1")
	srv := httptest.NewServer(runtimeCeilingFixture(agent, false, false))
	defer srv.Close()

	reaper, pods := newCeilingReaper(t, srv.URL, true)
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Fatalf("reaped: got %d, want 1 (over-ceiling agent should be reaped when armed)", stats.Reaped)
	}
	if len(pods.deleted) != 1 || pods.deleted[0] != "crew-gardenio-crew-wedged" {
		t.Fatalf("deleted pods: got %v, want [crew-gardenio-crew-wedged]", pods.deleted)
	}
}

func TestStaleReaper_RuntimeCeiling_ArmedReapsWedgedAgentRegardlessOfSpawnedBy(t *testing.T) {
	// Same as above but with NO spawned_by at all (crew/thread/plan-gardener
	// agents are not trigger-spawned). The ceiling now scopes on role, not
	// spawned_by, so this must still be reaped (gd-X2eSin2J80).
	agent := ceilingAgentEntry("wedged", 7*time.Hour, "crew", "")
	srv := httptest.NewServer(runtimeCeilingFixture(agent, false, false))
	defer srv.Close()

	reaper, pods := newCeilingReaper(t, srv.URL, true)
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Fatalf("reaped: got %d, want 1 (role-scoped ceiling must not require spawned_by=trigger:*)", stats.Reaped)
	}
	if len(pods.deleted) != 1 {
		t.Fatalf("deleted pods: got %v, want 1", pods.deleted)
	}
}

func TestStaleReaper_RuntimeCeiling_DryRunDoesNotReap(t *testing.T) {
	// Same over-ceiling agent, but arm gate OFF (dry-run) → NOT reaped.
	agent := ceilingAgentEntry("wedged", 7*time.Hour, "crew", "trigger:gd-TRIG1")
	srv := httptest.NewServer(runtimeCeilingFixture(agent, false, false))
	defer srv.Close()

	reaper, pods := newCeilingReaper(t, srv.URL, false)
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (dry-run must not reap)", stats.Reaped)
	}
	if len(pods.deleted) != 0 {
		t.Fatalf("deleted pods: got %v, want none in dry-run", pods.deleted)
	}
}

func TestStaleReaper_RuntimeCeiling_OpenMRDeclinesReap(t *testing.T) {
	// Over-ceiling + armed, but the agent holds an OPEN merge-request bead →
	// the mid-flight guard declines the destructive reap.
	agent := ceilingAgentEntry("wedged", 7*time.Hour, "crew", "trigger:gd-TRIG1")
	srv := httptest.NewServer(runtimeCeilingFixture(agent, true /*hasMR*/, false /*mrClosed*/))
	defer srv.Close()

	reaper, pods := newCeilingReaper(t, srv.URL, true)
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (open MR mid-flight must decline reap)", stats.Reaped)
	}
	if len(pods.deleted) != 0 {
		t.Fatalf("deleted pods: got %v, want none with open MR", pods.deleted)
	}
}

func TestStaleReaper_RuntimeCeiling_ClosedMRAllowsReap(t *testing.T) {
	// Over-ceiling + armed; the agent's only MR is CLOSED (merged/closed), so
	// it is no longer mid-flight and the reap proceeds.
	agent := ceilingAgentEntry("wedged", 7*time.Hour, "crew", "trigger:gd-TRIG1")
	srv := httptest.NewServer(runtimeCeilingFixture(agent, true /*hasMR*/, true /*mrClosed*/))
	defer srv.Close()

	reaper, pods := newCeilingReaper(t, srv.URL, true)
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Fatalf("reaped: got %d, want 1 (closed MR is not mid-flight; reap should proceed)", stats.Reaped)
	}
	if len(pods.deleted) != 1 {
		t.Fatalf("deleted pods: got %v, want 1", pods.deleted)
	}
}

func TestStaleReaper_RuntimeCeiling_UnderCeilingNotReaped(t *testing.T) {
	// 4h runtime < 6h ceiling, fresh updated_at → left alone (legit long run).
	agent := ceilingAgentEntry("wedged", 4*time.Hour, "crew", "trigger:gd-TRIG1")
	srv := httptest.NewServer(runtimeCeilingFixture(agent, false, false))
	defer srv.Close()

	reaper, pods := newCeilingReaper(t, srv.URL, true)
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (under-ceiling agent must not be reaped)", stats.Reaped)
	}
	if len(pods.deleted) != 0 {
		t.Fatalf("deleted pods: got %v, want none under ceiling", pods.deleted)
	}
}

func TestStaleReaper_RuntimeCeiling_RoleWithoutCeilingExempt(t *testing.T) {
	// Over-ceiling by runtime, fresh updated_at, live pod, armed — but the
	// agent's role ("reviewer") carries no configured ceiling at all. Only
	// roles present (and non-zero) in the ceilings map are subject to the
	// watchdog; an unconfigured role is exempt (and its fresh updated_at keeps
	// the base staleness rule from firing too).
	agent := ceilingAgentEntry("wedged", 7*time.Hour, "reviewer", "")
	srv := httptest.NewServer(runtimeCeilingFixture(agent, false, false))
	defer srv.Close()

	reaper, pods := newCeilingReaper(t, srv.URL, true)
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (role with no configured ceiling is exempt)", stats.Reaped)
	}
	if len(pods.deleted) != 0 {
		t.Fatalf("deleted pods: got %v, want none for a role without a ceiling", pods.deleted)
	}
}

func TestStaleReaper_RuntimeCeiling_PodlessNotReapedByCeiling(t *testing.T) {
	// Over-ceiling + armed, but NO live pod: the ceiling is a spend-stopper for
	// a live pod, so a podless bead is left to the leak/orphan arms. With a
	// fresh updated_at and open source task, nothing else reaps it either, so
	// stats.Reaped stays 0.
	agent := ceilingAgentEntry("wedged", 7*time.Hour, "crew", "trigger:gd-TRIG1")
	srv := httptest.NewServer(runtimeCeilingFixture(agent, false, false))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	// livePods empty (non-nil) → podExists=false for our agent.
	pods := &mockPodManager{livePods: map[string]bool{}}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	reaper.SetRuntimeCeilings(map[string]time.Duration{"crew": 6 * time.Hour})
	reaper.SetTunables(stubTunables{bools: map[string]bool{"STALEREAPER_RUNTIME_REAP": true}})

	stats := reaper.Reap(context.Background())
	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (podless over-ceiling bead is not the ceiling's job)", stats.Reaped)
	}
}

func TestStaleReaper_RuntimeCeiling_DisabledWhenCeilingsUnset(t *testing.T) {
	// SetRuntimeCeilings never called (nil map) → the ceiling is fully
	// disabled and an over-ceiling, fresh-updated_at agent is NOT reaped even
	// armed.
	agent := ceilingAgentEntry("wedged", 9*time.Hour, "crew", "trigger:gd-TRIG1")
	srv := httptest.NewServer(runtimeCeilingFixture(agent, false, false))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	pods := &mockPodManager{livePods: map[string]bool{"crew-gardenio-crew-wedged": true}}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	// SetRuntimeCeilings deliberately NOT called → ceilings stay nil (disabled).
	reaper.SetTunables(stubTunables{bools: map[string]bool{"STALEREAPER_RUNTIME_REAP": true}})

	stats := reaper.Reap(context.Background())
	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (ceiling disabled when no ceilings configured)", stats.Reaped)
	}
}

func TestStaleReaper_RuntimeCeiling_MRWalkErrorDeclinesReap(t *testing.T) {
	// Over-ceiling + armed, but the MR graph walk ERRORS (daemon returns 500 on
	// the dependencies endpoint). The guard fails CLOSED — a transient daemon
	// hiccup must never escalate into a destructive kill — so the reap is
	// declined this pass.
	agent := ceilingAgentEntry("wedged", 7*time.Hour, "crew", "trigger:gd-TRIG1")
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: []beadEntry{agent}})
		case r.URL.Path == "/v1/beads/"+agent.ID+"/dependencies" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	pods := &mockPodManager{livePods: map[string]bool{"crew-gardenio-crew-wedged": true}}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	reaper.SetRuntimeCeilings(map[string]time.Duration{"crew": 6 * time.Hour})
	reaper.SetTunables(stubTunables{bools: map[string]bool{"STALEREAPER_RUNTIME_REAP": true}})

	stats := reaper.Reap(context.Background())
	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (MR-walk error must fail closed and decline the reap)", stats.Reaped)
	}
	if len(pods.deleted) != 0 {
		t.Fatalf("deleted pods: got %v, want none when MR check errors", pods.deleted)
	}
}

// escalationCapture records the bodies of POST /v1/beads requests whose type
// is "escalation" (as opposed to the reap's own agent-bead PATCH/POST noise).
type escalationCapture struct {
	mu    sync.Mutex
	posts []map[string]any
}

func (e *escalationCapture) record(body map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.posts = append(e.posts, body)
}

func (e *escalationCapture) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.posts)
}

// escalationFixture wraps runtimeCeilingFixture but intercepts POST /v1/beads
// calls, recording the body and returning a minimal valid bead so CreateBead
// succeeds instead of failing on an empty response body.
func escalationFixture(agent beadEntry, cap *escalationCapture) http.HandlerFunc {
	inner := runtimeCeilingFixture(agent, false, false)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/beads" {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			cap.record(body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-ESCALATION1", "title": body["title"], "type": "escalation",
				"kind": "issue", "status": "open",
			})
			return
		}
		inner(w, r)
	}
}

func TestStaleReaper_RuntimeCeiling_ArmedReapFilesEscalationFirst(t *testing.T) {
	// Matt's ask was "alert on that, do something about it" — an armed reap
	// must file an advisory escalation bead naming the agent before it kills
	// the pod (gd-X2eSin2J80), so a human gets a look at a long-running agent.
	agent := ceilingAgentEntry("wedged", 7*time.Hour, "crew", "trigger:gd-TRIG1")
	cap := &escalationCapture{}
	srv := httptest.NewServer(escalationFixture(agent, cap))
	defer srv.Close()

	reaper, pods := newCeilingReaper(t, srv.URL, true)
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Fatalf("reaped: got %d, want 1", stats.Reaped)
	}
	if len(pods.deleted) != 1 {
		t.Fatalf("deleted pods: got %v, want 1", pods.deleted)
	}
	if cap.count() != 1 {
		t.Fatalf("escalation POST count: got %d, want 1", cap.count())
	}
	posted := cap.posts[0]
	if posted["type"] != "escalation" {
		t.Fatalf("posted bead type: got %v, want escalation", posted["type"])
	}
	fields, _ := posted["fields"].(string)
	if fields == "" {
		if raw, ok := posted["fields"].(map[string]any); ok {
			if raw["requesting_agent"] != agent.ID {
				t.Fatalf("escalation requesting_agent: got %v, want %s", raw["requesting_agent"], agent.ID)
			}
		} else {
			t.Fatalf("escalation posted without fields: %v", posted)
		}
	}
}

func TestStaleReaper_RuntimeCeiling_DryRunDoesNotFileEscalation(t *testing.T) {
	// Dry-run must never file the pre-reap escalation — it hasn't decided to
	// reap anything yet.
	agent := ceilingAgentEntry("wedged", 7*time.Hour, "crew", "trigger:gd-TRIG1")
	cap := &escalationCapture{}
	srv := httptest.NewServer(escalationFixture(agent, cap))
	defer srv.Close()

	reaper, _ := newCeilingReaper(t, srv.URL, false /*armed*/)
	reaper.Reap(context.Background())

	if cap.count() != 0 {
		t.Fatalf("escalation POST count: got %d, want 0 in dry-run", cap.count())
	}
}

// patchCapture records the fields patch bodies PATCHed to a specific bead.
type patchCapture struct {
	mu      sync.Mutex
	beadID  string
	patches []map[string]any
}

func (p *patchCapture) record(body map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.patches = append(p.patches, body)
}

func (p *patchCapture) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.patches)
}

// dryRunStampFixture wraps runtimeCeilingFixture but intercepts the PATCH the
// dry-run observability stamp issues to the agent bead, recording its `fields`
// object so the test can assert what was written.
func dryRunStampFixture(agent beadEntry, cap *patchCapture) http.HandlerFunc {
	inner := runtimeCeilingFixture(agent, false, false)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/v1/beads/"+agent.ID {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			if fields, ok := body["fields"].(map[string]any); ok {
				cap.record(fields)
			} else {
				cap.record(body)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		inner(w, r)
	}
}

func TestStaleReaper_RuntimeCeiling_DryRunStampsWouldReapObservation(t *testing.T) {
	// Over-ceiling, dry-run (arm gate OFF): not reaped, but the would-reap
	// observation must be stamped onto the bead so it is queryable from the
	// beads plane (ka-0000003021, option C).
	agent := ceilingAgentEntry("wedged", 7*time.Hour, "crew", "trigger:gd-TRIG1")
	cap := &patchCapture{beadID: agent.ID}
	srv := httptest.NewServer(dryRunStampFixture(agent, cap))
	defer srv.Close()

	reaper, pods := newCeilingReaper(t, srv.URL, false /*armed*/)
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (dry-run must not reap)", stats.Reaped)
	}
	if len(pods.deleted) != 0 {
		t.Fatalf("deleted pods: got %v, want none in dry-run", pods.deleted)
	}
	if cap.count() != 1 {
		t.Fatalf("dry-run stamp PATCH count: got %d, want 1", cap.count())
	}
	fields := cap.patches[0]
	if fields[runtimeReapDryRunAtField] == nil || fields[runtimeReapDryRunAtField] == "" {
		t.Fatalf("stamp missing %s: %v", runtimeReapDryRunAtField, fields)
	}
	if fields[runtimeReapDryRunRuntimeField] == nil {
		t.Fatalf("stamp missing %s: %v", runtimeReapDryRunRuntimeField, fields)
	}
	if fields[runtimeReapDryRunReasonField] == nil {
		t.Fatalf("stamp missing %s: %v", runtimeReapDryRunReasonField, fields)
	}
}

func TestStaleReaper_RuntimeCeiling_DryRunStampIsIdempotent(t *testing.T) {
	// A bead already carrying runtime_reap_dryrun_at must NOT be re-stamped on a
	// later pass — exactly one daemon write per wedge, no per-pass amplification.
	agent := ceilingAgentEntry("wedged", 7*time.Hour, "crew", "trigger:gd-TRIG1")
	// Add the prior observation to the bead's fields.
	var fields map[string]string
	_ = json.Unmarshal(agent.Fields, &fields)
	fields[runtimeReapDryRunAtField] = time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	agent.Fields = mustJSON(fields)

	cap := &patchCapture{beadID: agent.ID}
	srv := httptest.NewServer(dryRunStampFixture(agent, cap))
	defer srv.Close()

	reaper, _ := newCeilingReaper(t, srv.URL, false /*armed*/)
	reaper.Reap(context.Background())

	if cap.count() != 0 {
		t.Fatalf("dry-run stamp PATCH count: got %d, want 0 (already-stamped bead must not be re-stamped)", cap.count())
	}
}
