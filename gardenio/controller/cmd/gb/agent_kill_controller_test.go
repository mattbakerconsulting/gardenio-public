package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/reconciler"
)

// The kill path is controller-side (gd-ZqcqDbjyt9, gd-vCFepmDZoL item 2):
// `gb agent kill` closes the agent bead as the operator, and the reconciler
// deletes the pod of a closed agent bead on its next pass, whatever the pod
// does. This test runs that path against an agent whose own token 403s on its
// own bead, so its cooperative stop poll can never see stop_requested.

const (
	killTestAgentToken    = "agent-own-token"
	killTestOperatorToken = "operator-token"
)

// killDaemonStub is a stateful beads daemon: a close really closes the bead,
// and the agent list (what the reconciler reads) leaves closed beads out. The
// agent's own token is refused on every read and write of its bead, the way a
// label-less agent bead refuses its project-scoped token.
type killDaemonStub struct {
	mu     sync.Mutex
	beads  map[string]map[string]any
	fields map[string]map[string]string
}

func newKillDaemonStub(beads ...map[string]any) *killDaemonStub {
	s := &killDaemonStub{beads: map[string]map[string]any{}, fields: map[string]map[string]string{}}
	for _, b := range beads {
		id := b["id"].(string)
		var f map[string]string
		_ = json.Unmarshal(b["fields"].(json.RawMessage), &f)
		s.beads[id] = b
		s.fields[id] = f
	}
	return s
}

// render returns bead id as the daemon would send it. Caller holds mu.
func (s *killDaemonStub) render(id string) map[string]any {
	out := maps.Clone(s.beads[id])
	raw, _ := json.Marshal(s.fields[id])
	out["fields"] = json.RawMessage(raw)
	return out
}

func (s *killDaemonStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+killTestAgentToken &&
			strings.HasPrefix(r.URL.Path, "/v1/beads/") {
			http.Error(w, `{"error":"token not authorized for this bead's project"}`, http.StatusForbidden)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			var list []map[string]any
			for id, b := range s.beads {
				if b["status"] != "closed" {
					list = append(list, s.render(id))
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": list, "total": len(list)})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			if _, ok := s.beads[id]; !ok {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(s.render(id))

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/close")
			if _, ok := s.beads[id]; !ok {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			for k, v := range payload {
				if str, ok := v.(string); ok && k != "closed_by" {
					s.fields[id][k] = str
				}
			}
			s.beads[id]["status"] = "closed"
			_ = json.NewEncoder(w).Encode(s.render(id))

		default:
			http.NotFound(w, r)
		}
	})
}

// killPods is a podmanager.Manager over an in-memory pod set.
type killPods struct {
	mu      sync.Mutex
	pods    map[string]corev1.Pod
	deleted []string
}

func (m *killPods) CreateAgentPod(_ context.Context, spec podmanager.AgentPodSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pods[spec.PodName()] = killTestPod(spec.PodName(), spec.AgentName)
	return nil
}

func (m *killPods) DeleteAgentPod(_ context.Context, name, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pods, name)
	m.deleted = append(m.deleted, name)
	return nil
}

func (m *killPods) DeleteAllAgentPods(context.Context, string, bool) (int, error) { return 0, nil }

func (m *killPods) ListAgentPods(context.Context, string, map[string]string) ([]corev1.Pod, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]corev1.Pod, 0, len(m.pods))
	for _, p := range m.pods {
		out = append(out, p)
	}
	return out, nil
}

func (m *killPods) GetAgentPod(_ context.Context, name, _ string) (*corev1.Pod, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pods[name]; ok {
		return &p, nil
	}
	return nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
}

func (m *killPods) GetNode(_ context.Context, name string) (*corev1.Node, error) {
	return nil, apierrors.NewNotFound(corev1.Resource("nodes"), name)
}

func killTestPod(name, agent string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels: map[string]string{
				podmanager.LabelApp:   podmanager.LabelAppValue,
				podmanager.LabelAgent: agent,
			},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "img:v1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestAgentKill_ControllerDeletesPodWhoseStopPollIs403(t *testing.T) {
	stuck := makeAgentBeadWithSpawner("gd-stuck", "infra-wao3", "thread-c8c3ac04dde")
	// A bystander keeps the desired set non-empty, as in a live fleet: the
	// reconciler defers orphan cleanup while the daemon lists no agents at all.
	bystander := makeAgentBead("gd-other", "other", "gardenio", "crew", "crew", "working")
	stub := newKillDaemonStub(stuck, bystander)
	stub.fields["gd-stuck"]["operated_by"] = "oidc:operator@example.com"
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	newClient := func(token string) *beadsapi.Client {
		c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL, Token: token})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	agentClient := newClient(killTestAgentToken)
	operatorClient := newClient(killTestOperatorToken)
	controllerClient := newClient("controller-token")

	stuckPod := "crew-gardenio-crew-infra-wao3"
	pods := &killPods{pods: map[string]corev1.Pod{
		stuckPod:                   killTestPod(stuckPod, "infra-wao3"),
		"crew-gardenio-crew-other": killTestPod("crew-gardenio-crew-other", "other"),
	}}
	builder := func(cfg *config.Config, project, mode, role, agentName string, _ map[string]string) podmanager.AgentPodSpec {
		return podmanager.AgentPodSpec{Project: project, Mode: mode, Role: role, AgentName: agentName, Image: "img:v1", Namespace: cfg.Namespace}
	}
	rec := reconciler.New(controllerClient, pods, &config.Config{
		Namespace: "ns", CoopBurstLimit: 3, CoopRateLimitWindow: 5 * time.Minute,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), builder)

	ctx := context.Background()

	// The agent cannot read its own bead, so it can never see a stop request.
	if _, err := agentClient.GetBead(ctx, "gd-stuck"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("agent's own GetBead: err = %v, want HTTP 403", err)
	}

	// Before the kill the reconciler keeps the pod.
	if err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile before kill: %v", err)
	}
	if len(pods.deleted) != 0 {
		t.Fatalf("pods deleted before kill: %v", pods.deleted)
	}

	// The operator kills it. They operate the agent but a thread spawned it,
	// so no --override-owner is needed.
	oldDaemon := daemon
	daemon = operatorClient
	t.Cleanup(func() { daemon = oldDaemon })
	stubUserEmail(t, "operator@example.com")
	cmd := cmdWithCtx()
	cmd.Flags().Bool("override-owner", false, "")
	cmd.Flags().String("reason", "", "")
	if err := agentKillCmd.RunE(cmd, []string{"gd-stuck"}); err != nil {
		t.Fatalf("gb agent kill: %v", err)
	}
	stub.mu.Lock()
	status, fields := stub.beads["gd-stuck"]["status"], maps.Clone(stub.fields["gd-stuck"])
	stub.mu.Unlock()
	if status != "closed" {
		t.Fatalf("agent bead status after kill = %v, want closed", status)
	}
	if fields["stop_requested"] != "true" || fields["agent_state"] != "killed" {
		t.Errorf("fields after kill = %v, want stop_requested=true agent_state=killed", fields)
	}

	// One reconcile pass deletes the pod, with no help from the pod.
	if err := rec.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile after kill: %v", err)
	}
	if len(pods.deleted) != 1 || pods.deleted[0] != stuckPod {
		t.Fatalf("pods deleted after kill = %v, want [%s]", pods.deleted, stuckPod)
	}
	if _, ok := pods.pods["crew-gardenio-crew-other"]; !ok {
		t.Error("bystander pod was deleted")
	}
}

func TestCheckAgentOwner(t *testing.T) {
	agent := func(spawner, operator string) *beadsapi.AgentBead {
		return &beadsapi.AgentBead{AgentName: "a", Metadata: map[string]string{
			"spawned_by": spawner, "operated_by": operator,
		}}
	}
	stubUserEmail(t, "me@example.com")
	for _, tc := range []struct {
		name              string
		spawner, operator string
		ok                bool
	}{
		{"spawned by me", "me@example.com", "", true},
		{"operated by me (oidc)", "thread-abc", "oidc:me@example.com", true},
		{"operated by me (bare)", "thread-abc", "me@example.com", true},
		{"no spawner", "", "", true},
		{"someone else's", "alice@example.com", "oidc:alice@example.com", false},
		{"thread for someone else", "thread-abc", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAgentOwner(agent(tc.spawner, tc.operator))
			if (err == nil) != tc.ok {
				t.Errorf("checkAgentOwner = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
