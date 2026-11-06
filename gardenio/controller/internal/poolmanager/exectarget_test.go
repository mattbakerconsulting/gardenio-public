package poolmanager

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// createCapture is a minimal daemon that records the fields of the last bead
// POSTed to /v1/beads, so we can assert what createPrewarmedAgent stamps.
type createCapture struct {
	mu     sync.Mutex
	fields map[string]any
}

func (c *createCapture) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			c.mu.Lock()
			if f, ok := body["fields"].(map[string]any); ok {
				c.fields = f
			}
			c.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-created"})
		default:
			// AddLabel and everything else are no-ops for this test.
			w.WriteHeader(http.StatusNoContent)
		}
	}))
}

func (c *createCapture) field(k string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.fields[k].(string)
	return v, ok
}

func newManagerWithCfg(t *testing.T, srvURL string, cfg *config.Config) *Manager {
	t.Helper()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srvURL})
	if err != nil {
		t.Fatal(err)
	}
	return New(daemon, cfg, slog.Default())
}

// A lambda_microvm project must stamp execution_target on its pool beads so the
// spawn routes to the MicroVM backend rather than silently launching a K8s pod.
func TestCreatePrewarmedAgent_StampsMicrovmExecutionTarget(t *testing.T) {
	cc := &createCapture{}
	srv := cc.server(t)
	defer srv.Close()

	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM},
		},
	}
	pm := newManagerWithCfg(t, srv.URL, cfg)

	if err := pm.createPrewarmedAgent(context.Background(), "gardenio",
		beadsapi.PrewarmedRolePool{Role: "thread", Mode: "crew"}); err != nil {
		t.Fatalf("createPrewarmedAgent: %v", err)
	}

	got, ok := cc.field("execution_target")
	if !ok {
		t.Fatalf("expected execution_target field on the pool bead, got none (fields=%v)", cc.fields)
	}
	if got != podmanager.ExecutionTargetLambdaMicroVM {
		t.Errorf("execution_target = %q, want %q", got, podmanager.ExecutionTargetLambdaMicroVM)
	}
}

// A Kubernetes project (the default) leaves execution_target unstamped, matching
// the pre-existing K8s pool behaviour and keeping the field absent for runc pools.
func TestCreatePrewarmedAgent_KubernetesLeavesTargetUnstamped(t *testing.T) {
	cc := &createCapture{}
	srv := cc.server(t)
	defer srv.Close()

	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {}, // no ExecutionTarget → kubernetes
		},
	}
	pm := newManagerWithCfg(t, srv.URL, cfg)

	if err := pm.createPrewarmedAgent(context.Background(), "gardenio",
		beadsapi.PrewarmedRolePool{Role: "thread", Mode: "crew"}); err != nil {
		t.Fatalf("createPrewarmedAgent: %v", err)
	}

	if got, ok := cc.field("execution_target"); ok {
		t.Errorf("expected no execution_target field for a K8s pool, got %q", got)
	}
}

// Role override supersedes the project layer, mirroring resolveExecutionTarget.
func TestEffectiveExecutionTarget_RoleOverridesProject(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {ExecutionTarget: podmanager.ExecutionTargetKubernetes},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"gardenio-microvms": {ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM},
		},
	}
	pm := New(nil, cfg, slog.Default())

	if got := pm.effectiveExecutionTarget("gardenio", "gardenio-microvms"); got != podmanager.ExecutionTargetLambdaMicroVM {
		t.Errorf("role override: got %q, want %q", got, podmanager.ExecutionTargetLambdaMicroVM)
	}
	// Unknown value falls back to kubernetes.
	cfg.ProjectCache["weird"] = config.ProjectCacheEntry{ExecutionTarget: "gvisor-nonsense"}
	if got := pm.effectiveExecutionTarget("weird", "none"); got != podmanager.ExecutionTargetKubernetes {
		t.Errorf("unknown target: got %q, want kubernetes", got)
	}
}

// A MicroVM member has no K8s RuntimeClass — effectiveRuntimeClass must return
// empty so the runsc-channel assign guard never compares it against a pod
// runtime requirement.
func TestEffectiveRuntimeClass_MicrovmIsEmpty(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {
				ExecutionTarget:  podmanager.ExecutionTargetLambdaMicroVM,
				RuntimeClassName: "runsc", // must be ignored for a MicroVM member
			},
		},
	}
	pm := New(nil, cfg, slog.Default())
	if got := pm.effectiveRuntimeClass("gardenio", "thread"); got != "" {
		t.Errorf("effectiveRuntimeClass for a MicroVM member = %q, want empty", got)
	}
}

// A gVisor pool (role runtime_class=runsc, mirroring the gvisor-poc role bead
// ka-0000003266) must stamp runtime_class=runsc on its standby beads so
// BuildSpecFromBeadInfo pins the pod to runsc and routeGVisorNamespace routes it
// into gardenio-gvisor even if the RoleCache drifts before the reconciler builds
// the pod (ka-0000003859 gap 3a).
func TestCreatePrewarmedAgent_StampsRunscRuntimeClass(t *testing.T) {
	cc := &createCapture{}
	srv := cc.server(t)
	defer srv.Close()

	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"gvisor-poc": {RuntimeClassName: podmanager.RuntimeClassRunsc},
		},
	}
	pm := newManagerWithCfg(t, srv.URL, cfg)

	if err := pm.createPrewarmedAgent(context.Background(), "gardenio",
		beadsapi.PrewarmedRolePool{Role: "gvisor-poc", Mode: "crew"}); err != nil {
		t.Fatalf("createPrewarmedAgent: %v", err)
	}

	got, ok := cc.field("runtime_class")
	if !ok {
		t.Fatalf("expected runtime_class field on the gVisor pool bead, got none (fields=%v)", cc.fields)
	}
	if got != podmanager.RuntimeClassRunsc {
		t.Errorf("runtime_class = %q, want %q", got, podmanager.RuntimeClassRunsc)
	}
}

// A default (runc) pool leaves runtime_class unstamped, keeping the field absent
// exactly as before — only gVisor/runsc pools carry the pin.
func TestCreatePrewarmedAgent_RuncLeavesRuntimeClassUnstamped(t *testing.T) {
	cc := &createCapture{}
	srv := cc.server(t)
	defer srv.Close()

	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"thread": {}, // no RuntimeClassName → cluster default (runc)
		},
	}
	pm := newManagerWithCfg(t, srv.URL, cfg)

	if err := pm.createPrewarmedAgent(context.Background(), "gardenio",
		beadsapi.PrewarmedRolePool{Role: "thread", Mode: "crew"}); err != nil {
		t.Fatalf("createPrewarmedAgent: %v", err)
	}

	if got, ok := cc.field("runtime_class"); ok {
		t.Errorf("expected no runtime_class field for a runc pool, got %q", got)
	}
}

// A MicroVM member must NOT stamp runtime_class even when the role pins runsc —
// a Firecracker VM has no K8s pod RuntimeClass, so effectiveRuntimeClass (and
// thus the stamp) is empty, leaving the field absent.
func TestCreatePrewarmedAgent_MicrovmLeavesRuntimeClassUnstamped(t *testing.T) {
	cc := &createCapture{}
	srv := cc.server(t)
	defer srv.Close()

	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"gvisor-poc": {RuntimeClassName: podmanager.RuntimeClassRunsc},
		},
	}
	pm := newManagerWithCfg(t, srv.URL, cfg)

	if err := pm.createPrewarmedAgent(context.Background(), "gardenio",
		beadsapi.PrewarmedRolePool{Role: "gvisor-poc", Mode: "crew"}); err != nil {
		t.Fatalf("createPrewarmedAgent: %v", err)
	}

	if got, ok := cc.field("runtime_class"); ok {
		t.Errorf("expected no runtime_class field for a MicroVM member, got %q", got)
	}
}

// memberRuntimeClass prefers the runtime stamped on the member over a live
// RoleCache re-resolution, so a mid-flight RoleCache change (or an emptied
// cache) can never mismatch a runsc standby against the runtime it is really
// running. Members minted before stamping fall back to the (project,role)
// resolution.
func TestMemberRuntimeClass_PrefersStampThenFallsBack(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"gvisor-poc": {RuntimeClassName: podmanager.RuntimeClassRunsc},
		},
	}
	pm := New(nil, cfg, slog.Default())

	// Stamped member: honored even though the RoleCache no longer knows the role.
	stamped := prewarmedAgent{Project: "gardenio", Role: "unknown-role", RuntimeClass: podmanager.RuntimeClassRunsc}
	if got := pm.memberRuntimeClass(stamped); got != podmanager.RuntimeClassRunsc {
		t.Errorf("stamped member: got %q, want %q", got, podmanager.RuntimeClassRunsc)
	}

	// Legacy member (no stamp): fall back to the (project,role) resolution.
	legacy := prewarmedAgent{Project: "gardenio", Role: "gvisor-poc"}
	if got := pm.memberRuntimeClass(legacy); got != podmanager.RuntimeClassRunsc {
		t.Errorf("legacy member fallback: got %q, want %q", got, podmanager.RuntimeClassRunsc)
	}

	// Legacy runc member (no stamp, no role pin): empty, matches any channel.
	runc := prewarmedAgent{Project: "gardenio", Role: "thread"}
	if got := pm.memberRuntimeClass(runc); got != "" {
		t.Errorf("legacy runc member: got %q, want empty", got)
	}
}
