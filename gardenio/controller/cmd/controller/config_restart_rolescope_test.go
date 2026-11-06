package main

import (
	"context"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/subscriber"
)

// selectorCapturingManager records the label selector passed to ListAgentPods
// so tests can assert that a config update never lists pods.
type selectorCapturingManager struct {
	mockPodManager
	lastSelector map[string]string
}

func (m *selectorCapturingManager) ListAgentPods(_ context.Context, _ string, selector map[string]string) ([]corev1.Pod, error) {
	m.lastSelector = selector
	return m.pods, m.listErr
}

// TestConfigUpdated_LeavesPodsInPlace is the regression test for ka-0000002053:
// a config bead update, project-wide, role-scoped or auth, must leave every
// running pod in place and must not even list pods.
func TestConfigUpdated_LeavesPodsInPlace(t *testing.T) {
	for _, tc := range []struct{ name, role, title string }{
		{"project-wide", "", "claude-instructions"},
		{"role-scoped", "security-analyst", "claude-instructions"},
		{"auth", "", "auth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &selectorCapturingManager{}
			mgr.pods = []corev1.Pod{{
				ObjectMeta: metav1.ObjectMeta{Name: "crew-gardenio-thread-a"},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			}}
			event := subscriber.Event{
				Type:     subscriber.ConfigUpdated,
				Project:  "gardenio",
				Role:     tc.role,
				BeadID:   "gd-MJ0uNY7Lno",
				Metadata: map[string]string{"title": tc.title},
			}
			err := handleEvent(
				context.Background(), slog.Default(), &config.Config{Namespace: "gardenio"}, event, mgr,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			)
			if err != nil {
				t.Fatalf("handleEvent returned error: %v", err)
			}
			if mgr.lastSelector != nil {
				t.Fatalf("config update should not list pods, got selector: %v", mgr.lastSelector)
			}
			if len(mgr.deleted) != 0 {
				t.Fatalf("config update should not delete pods, got: %v", mgr.deleted)
			}
		})
	}
}
