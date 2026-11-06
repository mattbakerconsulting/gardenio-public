package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"gardenio/controller/internal/podmanager"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func capPod(ns, name, project string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{
			podmanager.LabelApp:     podmanager.LabelAppValue,
			podmanager.LabelAgent:   name,
			podmanager.LabelProject: project,
		}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// The event door's pod caps count a tenant land's live pods, in land-<land>,
// toward the fleet-wide CoopMaxPods (gd-NDJIOutLsv). A namespace that is not a
// cached tenant land's, and a terminal pod, are not counted.
func TestCountActiveAgentPods_CountsTenantLandPods(t *testing.T) {
	cfg := landFenceCfg()
	client := fake.NewSimpleClientset(
		capPod("gardenio", "gd-crew-1", "gardenio", corev1.PodRunning),
		capPod("gardenio", "gd-crew-2", "gardenio", corev1.PodSucceeded),
		capPod("land-acme", "acme-crew-1", "acme", corev1.PodRunning),
		capPod("land-acme", "acme-crew-2", "acme", corev1.PodPending),
		capPod("land-other", "other-crew-1", "other", corev1.PodRunning),
	)
	pods := podmanager.New(client, slog.New(slog.NewTextHandler(io.Discard, nil)))

	active, perProject, err := countActiveAgentPods(context.Background(), cfg, pods, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if active != 3 {
		t.Errorf("active = %d, want 3 (one fleet pod and two live acme pods)", active)
	}
	if perProject != 2 {
		t.Errorf("acme pods = %d, want 2", perProject)
	}
}

// A land namespace the controller cannot list (gone, or its RoleBinding not
// made yet) counts nothing and does not block the spawn; the release
// namespace still fails closed.
func TestCountActiveAgentPods_UnlistableLandSkipped(t *testing.T) {
	cfg := landFenceCfg()
	client := fake.NewSimpleClientset(capPod("gardenio", "gd-crew-1", "gardenio", corev1.PodRunning))
	client.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "land-acme" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no binding"))
		}
		return false, nil, nil
	})
	pods := podmanager.New(client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	active, _, err := countActiveAgentPods(context.Background(), cfg, pods, "acme")
	if err != nil || active != 1 {
		t.Errorf("active, err = %d, %v; want 1, nil", active, err)
	}

	cfg.Namespace = "land-acme"
	if _, _, err := countActiveAgentPods(context.Background(), cfg, pods, "acme"); err == nil {
		t.Error("an unlistable release namespace counted as empty")
	}
}
