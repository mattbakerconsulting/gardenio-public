package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// poolMux serves both project and agent list endpoints based on ?type= query.
func poolMuxServer(t *testing.T, projects, agents []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		typeParam := r.URL.Query().Get("type")
		switch {
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case strings.Contains(typeParam, "project"):
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": projects})
		case strings.Contains(typeParam, "agent"):
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": agents})
		default:
			// fallback: return whichever matches path
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setPoolProject(t *testing.T, p string) {
	t.Helper()
	old := poolProject
	poolProject = p
	t.Cleanup(func() { poolProject = old })
}

// ── runPoolStatus ────────────────────────────────────────────────────────

func TestRunPoolStatus_Empty(t *testing.T) {
	srv := poolMuxServer(t, nil, nil)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolStatus(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunPoolStatus_WithPool(t *testing.T) {
	projects := []map[string]any{
		{
			"id": "gd-P1", "title": "Project: myproj", "type": "project",
			"status": "open",
			"fields": json.RawMessage(`{"prefix":"myp","prewarmed_pool":"{\"enabled\":true,\"min_size\":2,\"max_size\":5,\"role\":\"thread\",\"mode\":\"crew\"}"}`),
		},
	}
	agents := []map[string]any{
		{
			"id": "gd-A1", "title": "prewarmed-agent", "type": "agent",
			"status": "open",
			"fields": json.RawMessage(`{"project":"myproj","agent":"agent-1","agent_state":"prewarmed","role":"thread","mode":"crew"}`),
		},
	}
	srv := poolMuxServer(t, projects, agents)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolStatus(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunPoolStatus_SpecificProject(t *testing.T) {
	projects := []map[string]any{
		{
			"id": "gd-P1", "title": "Project: myproj", "type": "project",
			"status": "open",
			"fields": json.RawMessage(`{"prefix":"myp"}`),
		},
	}
	srv := poolMuxServer(t, projects, nil)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolStatus(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunPoolStatus_ProjectsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)
	setPoolProject(t, "")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolStatus(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

// ── runPoolDisable / runPoolEnable ───────────────────────────────────────

func TestRunPoolDisable_NoProject(t *testing.T) {
	setPoolProject(t, "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("KD_PROJECT", "")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolDisable(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

func TestRunPoolDisable_OK(t *testing.T) {
	projects := []map[string]any{
		{
			"id": "gd-P1", "title": "Project: myproj", "type": "project",
			"status": "open",
			"fields": json.RawMessage(`{"prewarmed_pool":"{\"enabled\":true,\"min_size\":2,\"max_size\":5,\"role\":\"thread\",\"mode\":\"crew\"}"}`),
		},
	}
	srv := poolMuxServer(t, projects, nil)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolDisable(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunPoolEnable_OK(t *testing.T) {
	projects := []map[string]any{
		{
			"id": "gd-P1", "title": "Project: myproj", "type": "project",
			"status": "open",
			"fields": json.RawMessage(`{}`),
		},
	}
	srv := poolMuxServer(t, projects, nil)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolEnable(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

// poolCaptureServer serves project beads and captures the prewarmed_pool field
// from the PATCH that gb pool enable/disable writes, so tests can assert the
// read-modify-write preserves custom config.
func poolCaptureServer(t *testing.T, projects []map[string]any, captured *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/") {
			var body struct {
				Fields map[string]string `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if v, ok := body.Fields["prewarmed_pool"]; ok {
				*captured = v
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": projects})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunPoolEnable_PreservesCustomConfig is the regression test for gd-yN9oPepLhb:
// enabling a pool that already has custom role/min/max (and is currently
// disabled) must flip only enabled and keep the existing values.
func TestRunPoolEnable_PreservesCustomConfig(t *testing.T) {
	projects := []map[string]any{
		{
			"id": "gd-P1", "title": "Project: myproj", "type": "project",
			"status": "open",
			// Currently disabled, with custom config that must survive enable.
			"fields": json.RawMessage(`{"prewarmed_pool":"{\"enabled\":false,\"min_size\":7,\"max_size\":9,\"role\":\"gardener\",\"mode\":\"solo\"}"}`),
		},
	}
	var captured string
	srv := poolCaptureServer(t, projects, &captured)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolEnable(cmd, nil); err != nil {
		t.Fatalf("err: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(captured), &got); err != nil {
		t.Fatalf("captured pool config not JSON: %q (%v)", captured, err)
	}
	if got["enabled"] != true {
		t.Errorf("enabled = %v, want true", got["enabled"])
	}
	if got["role"] != "gardener" {
		t.Errorf("role = %v, want gardener (custom value clobbered)", got["role"])
	}
	if got["mode"] != "solo" {
		t.Errorf("mode = %v, want solo (custom value clobbered)", got["mode"])
	}
	if got["min_size"] != float64(7) {
		t.Errorf("min_size = %v, want 7 (custom value clobbered)", got["min_size"])
	}
	if got["max_size"] != float64(9) {
		t.Errorf("max_size = %v, want 9 (custom value clobbered)", got["max_size"])
	}
}

// TestRunPoolEnable_DefaultsWhenUnset confirms a brand-new pool (no stored
// config) still gets sensible defaults.
func TestRunPoolEnable_DefaultsWhenUnset(t *testing.T) {
	projects := []map[string]any{
		{
			"id": "gd-P1", "title": "Project: myproj", "type": "project",
			"status": "open",
			"fields": json.RawMessage(`{}`),
		},
	}
	var captured string
	srv := poolCaptureServer(t, projects, &captured)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolEnable(cmd, nil); err != nil {
		t.Fatalf("err: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(captured), &got); err != nil {
		t.Fatalf("captured pool config not JSON: %q (%v)", captured, err)
	}
	if got["enabled"] != true || got["role"] != "thread" || got["mode"] != "crew" ||
		got["min_size"] != float64(2) || got["max_size"] != float64(5) {
		t.Errorf("defaults wrong: %v", got)
	}
}

// TestRunPoolEnable_MicrovmProjectStaysOff confirms enable on a lambda_microvm
// project flips only the flag and does NOT fill min_size — the pool stays 0/off
// (idle MicroVMs bill continuously; opt-in is an explicit min_size, feature
// ka-0000005825). Without this, enable would write min_size=2 and the reconciler
// would read it as an opt-in nobody asked for.
func TestRunPoolEnable_MicrovmProjectStaysOff(t *testing.T) {
	projects := []map[string]any{
		{
			"id": "gd-P1", "title": "Project: myproj", "type": "project",
			"status": "open",
			// MicroVM project, brand-new pool (no stored prewarmed_pool).
			"fields": json.RawMessage(`{"execution_target":"lambda_microvm"}`),
		},
	}
	var captured string
	srv := poolCaptureServer(t, projects, &captured)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolEnable(cmd, nil); err != nil {
		t.Fatalf("err: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(captured), &got); err != nil {
		t.Fatalf("captured pool config not JSON: %q (%v)", captured, err)
	}
	if got["enabled"] != true {
		t.Errorf("enabled = %v, want true", got["enabled"])
	}
	// min_size must be 0 (absent/zero) — NOT the K8s default of 2.
	if v, ok := got["min_size"]; ok && v != float64(0) {
		t.Errorf("min_size = %v, want 0 (MicroVM pool must not be auto-sized)", v)
	}
	// role/mode/max still get their non-cost defaults so the config is well-formed.
	if got["role"] != "thread" || got["mode"] != "crew" || got["max_size"] != float64(5) {
		t.Errorf("non-cost defaults wrong: %v", got)
	}
}

// TestRunPoolEnable_MicrovmPreservesExplicitMin confirms an operator who HAS set
// a min_size on a MicroVM pool keeps it through enable — the guard only suppresses
// the auto-fill, never an explicit opt-in.
func TestRunPoolEnable_MicrovmPreservesExplicitMin(t *testing.T) {
	projects := []map[string]any{
		{
			"id": "gd-P1", "title": "Project: myproj", "type": "project",
			"status": "open",
			"fields": json.RawMessage(`{"execution_target":"lambda_microvm","prewarmed_pool":"{\"enabled\":false,\"min_size\":1,\"max_size\":3,\"role\":\"thread\",\"mode\":\"crew\"}"}`),
		},
	}
	var captured string
	srv := poolCaptureServer(t, projects, &captured)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolEnable(cmd, nil); err != nil {
		t.Fatalf("err: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(captured), &got); err != nil {
		t.Fatalf("captured pool config not JSON: %q (%v)", captured, err)
	}
	if got["enabled"] != true || got["min_size"] != float64(1) || got["max_size"] != float64(3) {
		t.Errorf("explicit MicroVM opt-in not preserved: %v", got)
	}
}

// TestRunPoolDisable_PreservesCustomConfig confirms disable flips only enabled
// and keeps custom role/min/max so re-enabling restores them.
func TestRunPoolDisable_PreservesCustomConfig(t *testing.T) {
	projects := []map[string]any{
		{
			"id": "gd-P1", "title": "Project: myproj", "type": "project",
			"status": "open",
			"fields": json.RawMessage(`{"prewarmed_pool":"{\"enabled\":true,\"min_size\":7,\"max_size\":9,\"role\":\"gardener\",\"mode\":\"solo\"}"}`),
		},
	}
	var captured string
	srv := poolCaptureServer(t, projects, &captured)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolDisable(cmd, nil); err != nil {
		t.Fatalf("err: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(captured), &got); err != nil {
		t.Fatalf("captured pool config not JSON: %q (%v)", captured, err)
	}
	if got["enabled"] != false {
		t.Errorf("enabled = %v, want false", got["enabled"])
	}
	if got["role"] != "gardener" || got["mode"] != "solo" ||
		got["min_size"] != float64(7) || got["max_size"] != float64(9) {
		t.Errorf("disable clobbered custom config: %v", got)
	}
}

func TestRunPoolEnable_ProjectNotFound(t *testing.T) {
	srv := poolMuxServer(t, []map[string]any{}, nil)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "missing")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolEnable(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

// ── runPoolFlush ─────────────────────────────────────────────────────────

func TestRunPoolFlush_NoProject(t *testing.T) {
	setPoolProject(t, "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("KD_PROJECT", "")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolFlush(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

func TestRunPoolFlush_OK(t *testing.T) {
	agents := []map[string]any{
		{
			"id": "gd-A1", "title": "a", "type": "agent",
			"status": "open",
			"fields": json.RawMessage(`{"project":"myproj","agent":"a1","agent_state":"prewarmed","role":"thread","mode":"crew"}`),
		},
		{
			"id": "gd-A2", "title": "b", "type": "agent",
			"status": "open",
			"fields": json.RawMessage(`{"project":"other","agent":"a2","agent_state":"prewarmed","role":"thread","mode":"crew"}`),
		},
	}
	srv := poolMuxServer(t, nil, agents)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolFlush(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

// ── runPoolAssign ────────────────────────────────────────────────────────

func TestRunPoolAssign_NoProject(t *testing.T) {
	setPoolProject(t, "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("KD_PROJECT", "")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolAssign(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

func TestRunPoolAssign_NoPrewarmed(t *testing.T) {
	agents := []map[string]any{
		{
			"id": "gd-A1", "title": "a", "type": "agent",
			"status": "open",
			"fields": json.RawMessage(`{"project":"myproj","agent":"a1","agent_state":"running","role":"thread"}`),
		},
	}
	srv := poolMuxServer(t, nil, agents)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolAssign(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

func TestRunPoolAssign_OK(t *testing.T) {
	agents := []map[string]any{
		{
			"id": "gd-A1", "title": "a", "type": "agent",
			"status": "open",
			"fields": json.RawMessage(`{"project":"myproj","agent":"a1","agent_state":"prewarmed","role":"thread"}`),
		},
	}
	srv := poolMuxServer(t, nil, agents)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")

	oldTaskID := poolAssignTaskID
	poolAssignTaskID = "gd-T1"
	t.Cleanup(func() { poolAssignTaskID = oldTaskID })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolAssign(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

// TestRunPoolAssign_StampsModel verifies the `gb pool assign --model` flag
// (design D1, ka-0000000406 / ka-0000006250): a non-empty --model stamps
// fields.model in the agent_state PATCH so applyAgentModelOverride bakes the
// model into the assigned pod. The value is passed verbatim (it is an env-var
// value, not a k8s label — no sanitization).
func TestRunPoolAssign_StampsModel(t *testing.T) {
	agents := []map[string]any{
		{
			"id": "gd-A1", "title": "a", "type": "agent",
			"status": "open",
			"fields": json.RawMessage(`{"project":"myproj","agent":"a1","agent_state":"prewarmed","role":"thread"}`),
		},
	}
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/") {
			b, _ := io.ReadAll(r.Body)
			captured = string(b)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if strings.Contains(r.URL.Query().Get("type"), "agent") {
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": agents})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	setPoolProject(t, "myproj")

	oldModel := poolAssignModel
	poolAssignModel = "claude-opus-4-8"
	t.Cleanup(func() { poolAssignModel = oldModel })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runPoolAssign(cmd, nil); err != nil {
		t.Fatalf("err: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(captured), &got); err != nil {
		t.Fatalf("captured PATCH not JSON: %q (%v)", captured, err)
	}
	// The PATCH nests field updates under "fields" (UpdateBeadFields shape).
	fields, _ := got["fields"].(map[string]any)
	if fields == nil {
		// Some client shapes flatten the fields onto the top-level object.
		fields = got
	}
	if m, _ := fields["model"].(string); m != "claude-opus-4-8" {
		t.Errorf("model = %q, want %q (captured: %s)", m, "claude-opus-4-8", captured)
	}
	if s, _ := fields["agent_state"].(string); s != "assigning" {
		t.Errorf("agent_state = %q, want assigning (captured: %s)", s, captured)
	}
}
