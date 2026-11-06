package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/podmanager"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestProbeAgentState is the core acceptance mapping (AC #3): exit 0 → done,
// non-zero → failed.
func TestProbeAgentState(t *testing.T) {
	tests := []struct {
		exit int
		want string
	}{
		{0, "done"},
		{1, "failed"},
		{2, "failed"},
		{-1, "failed"},
		{137, "failed"},
	}
	for _, tt := range tests {
		got := probeAgentState(probeOutcome{ExitCode: tt.exit})
		if got != tt.want {
			t.Errorf("probeAgentState(exit=%d) = %q, want %q", tt.exit, got, tt.want)
		}
	}
}

// TestProbeOutcomeFromPod decodes a pod's terminal status into an outcome. A
// non-terminal pod returns done=false so the poller keeps waiting.
func TestProbeOutcomeFromPod(t *testing.T) {
	// Running pod: not terminal yet.
	if _, done := probeOutcomeFromPod(&corev1.Pod{
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}); done {
		t.Error("a Running pod must not be treated as terminal")
	}

	// Succeeded pod: exit 0.
	o, done := probeOutcomeFromPod(&corev1.Pod{
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	})
	if !done || o.ExitCode != 0 || !o.succeeded() {
		t.Errorf("PodSucceeded → %+v (done=%v), want exit 0 success", o, done)
	}

	// Failed pod with a terminated container exit code: that code wins.
	o, done = probeOutcomeFromPod(&corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: podmanager.ProbeContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 3,
					Reason:   "Error",
				}},
			}},
		},
	})
	if !done || o.ExitCode != 3 || o.succeeded() {
		t.Errorf("PodFailed w/ exit 3 → %+v (done=%v), want exit 3 failure", o, done)
	}

	// Failed pod with NO container terminated status (deadline / unschedulable):
	// synthetic non-zero with the pod-level reason.
	o, done = probeOutcomeFromPod(&corev1.Pod{
		Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "DeadlineExceeded"},
	})
	if !done || o.succeeded() || o.ExitCode != -1 {
		t.Errorf("PodFailed w/o container status → %+v (done=%v), want synthetic failure (-1)", o, done)
	}
	if o.Reason != "DeadlineExceeded" {
		t.Errorf("reason = %q, want DeadlineExceeded", o.Reason)
	}
}

// TestWaitProbeTerminal drives the poll loop against a fake clientset holding a
// pre-terminated probe pod. It confirms the loop reads the container exit code.
func TestWaitProbeTerminal_ReadsExitCode(t *testing.T) {
	const ns, name = "gardenio", "probe-gd-x"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: podmanager.ProbeContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 42,
					Reason:   "Error",
				}},
			}},
		},
	}
	client := fake.NewSimpleClientset(pod)

	o, err := waitProbeTerminal(context.Background(), client, ns, name, 5*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("waitProbeTerminal: %v", err)
	}
	if o.ExitCode != 42 {
		t.Errorf("exit code = %d, want 42", o.ExitCode)
	}
	if probeAgentState(o) != "failed" {
		t.Errorf("state = %q, want failed", probeAgentState(o))
	}
}

// TestWaitProbeTerminal_Timeout confirms a pod that never terminates surfaces a
// harness timeout error (NOT a silent success).
func TestWaitProbeTerminal_Timeout(t *testing.T) {
	const ns, name = "gardenio", "probe-hang"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewSimpleClientset(pod)

	_, err := waitProbeTerminal(context.Background(), client, ns, name, 40*time.Millisecond, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error for a never-terminating pod")
	}
}

// TestRunProbePod_Success drives the full runner against a fake clientset. The
// fake never advances phases, so we pre-seed the pod's terminal status via a
// reactor: Create returns the pod as Succeeded so the wait loop reads exit 0.
func TestRunProbePod_CreatesAndCleansUp(t *testing.T) {
	const ns = "gardenio"
	client := fake.NewSimpleClientset()

	spec := podmanager.ProbePodSpec{
		Name:      "probe-gd-run",
		Namespace: ns,
		Image:     "img",
		Script:    "true",
		BeadID:    "gd-run",
	}

	// The fake clientset never advances pod phases, so once runProbePod has
	// Created the pod, a helper goroutine flips it to Succeeded — mimicking the
	// kubelet running the script to a clean exit — so the wait loop reads exit 0.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Poll until the pod exists, then mark it Succeeded.
		for i := 0; i < 200; i++ {
			p, err := client.CoreV1().Pods(ns).Get(context.Background(), spec.Name, metav1.GetOptions{})
			if err == nil {
				p.Status.Phase = corev1.PodSucceeded
				_, _ = client.CoreV1().Pods(ns).UpdateStatus(context.Background(), p, metav1.UpdateOptions{})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	o, err := runProbePod(context.Background(), client, spec, 5*time.Second, 10*time.Millisecond, false)
	<-done
	if err != nil {
		t.Fatalf("runProbePod: %v", err)
	}
	if o.ExitCode != 0 || !o.succeeded() {
		t.Errorf("outcome = %+v, want exit 0 success", o)
	}

	// keepPod=false → the pod should be deleted after termination.
	if _, err := client.CoreV1().Pods(ns).Get(context.Background(), spec.Name, metav1.GetOptions{}); err == nil {
		t.Error("probe pod should have been deleted (keepPod=false)")
	}
}

// TestProbePodName is a lowercase DNS-1123 label, stable, and bounded.
func TestProbePodName(t *testing.T) {
	// Mixed-case kd id → lowercased (a K8s pod name must be a DNS-1123 label).
	name := probePodName("gd-PBPTLlRY3G")
	if name != "probe-gd-pbptllry3g" {
		t.Errorf("probePodName = %q, want probe-gd-pbptllry3g (lowercased)", name)
	}
	// Over-long input is truncated to the 63-char DNS label ceiling.
	long := probePodName("gd-" + strings.Repeat("z", 100))
	if len(long) > 63 {
		t.Errorf("probePodName length = %d, want <= 63", len(long))
	}
}

// TestSplitCommaTrim covers the IMAGE_PULL_SECRETS env parser that lets an
// in-cluster `gb probe run` inherit the fleet's registry pull secret the same
// way the controller does (config.parseCommaSeparated). Empty/whitespace entries
// are dropped; an empty value yields nil.
func TestSplitCommaTrim(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"registry-pull-secret", []string{"registry-pull-secret"}},
		{" a , , b ,", []string{"a", "b"}},
	}
	for _, tt := range tests {
		got := splitCommaTrim(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("splitCommaTrim(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitCommaTrim(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
			}
		}
	}
}
