package podmanager

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestManager() *K8sManager {
	return New(fake.NewClientset(), slog.Default())
}

func minimalSpec() AgentPodSpec {
	return AgentPodSpec{
		Project:   "gardenio",
		Mode:      "crew",
		Role:      "dev",
		AgentName: "test-1",
		BeadID:    "bead-abc",
		Image:     "ghcr.io/agent:latest",
		Namespace: "default",
	}
}

// ── AgentPodSpec.PodName / Labels ──────────────────────────────────────────

func TestPodName(t *testing.T) {
	spec := AgentPodSpec{
		Mode: "crew", Project: "gardenio", Role: "dev", AgentName: "matt-1",
	}
	want := "crew-gardenio-dev-matt-1"
	if got := spec.PodName(); got != want {
		t.Errorf("PodName() = %q, want %q", got, want)
	}
}

func TestLabels(t *testing.T) {
	spec := AgentPodSpec{
		Mode: "job", Project: "acme", Role: "qa", AgentName: "bot-7",
	}
	labels := spec.Labels()
	checks := map[string]string{
		LabelApp:         LabelAppValue,
		LabelGardenioApp: LabelAppValue, // chart NetworkPolicies select agent pods on this (ka-0000002093)
		LabelProject:     "acme",
		LabelMode:        "job",
		LabelRole:        "qa",
		LabelAgent:       "bot-7",
	}
	for k, want := range checks {
		if got := labels[k]; got != want {
			t.Errorf("Labels()[%q] = %q, want %q", k, got, want)
		}
	}
}

func TestLabels_TruncatesLongAgentName(t *testing.T) {
	longName := "monorepo-003formbuilder14copyformrulebuilderdropdownspects-read-spec-w7p"
	spec := AgentPodSpec{
		Mode: "crew", Project: "monorepo", Role: "crew", AgentName: longName,
	}
	labels := spec.Labels()
	agent := labels[LabelAgent]
	if len(agent) > 63 {
		t.Errorf("Labels()[%q] length %d exceeds K8s limit of 63: %q", LabelAgent, len(agent), agent)
	}
	// After truncation, trailing non-alphanumeric chars are stripped.
	want := strings.TrimRight(longName[:63], "-_.")
	if agent != want {
		t.Errorf("Labels()[%q] = %q, want %q", LabelAgent, agent, want)
	}
}

func TestLabels_TruncatesTrailingDash(t *testing.T) {
	// Name that ends in a dash at exactly the 63-char boundary.
	longName := "monorepo-050conductsite05entercrfverifysponsordatareviewspects-xyz"
	spec := AgentPodSpec{
		Mode: "crew", Project: "monorepo", Role: "crew", AgentName: longName,
	}
	labels := spec.Labels()
	agent := labels[LabelAgent]
	if len(agent) > 63 {
		t.Errorf("Labels()[%q] length %d exceeds K8s limit of 63: %q", LabelAgent, len(agent), agent)
	}
	// The 63rd char is '-', which must be stripped.
	if agent[len(agent)-1] == '-' || agent[len(agent)-1] == '_' || agent[len(agent)-1] == '.' {
		t.Errorf("Labels()[%q] ends with non-alphanumeric char: %q", LabelAgent, agent)
	}
}

// ── SanitizeRole ───────────────────────────────────────────────────────────

func TestSanitizeRole(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"crew", "crew"},
		{"crew,thread", "crew-thread"},
		{"crew,thread,admin", "crew-thread-admin"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := SanitizeRole(tt.input); got != tt.want {
			t.Errorf("SanitizeRole(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPodName_NoDoublePrefix(t *testing.T) {
	// Agent names often include the project as a prefix (e.g. "gardenio-fix-ci-xyz").
	// PodName() must strip it so the name doesn't become
	// "crew-gardenio-dev-gardenio-fix-ci-xyz". Regression test for 92f57773.
	tests := []struct {
		project   string
		agentName string
		want      string
	}{
		{"gardenio", "gardenio-fix-ci-xyz", "crew-gardenio-dev-fix-ci-xyz"},
		{"monorepo", "monorepo-003formbuilder", "crew-monorepo-dev-003formbuilder"},
		// Agent name that does NOT start with project prefix — unchanged.
		{"gardenio", "alpha-1", "crew-gardenio-dev-alpha-1"},
		// Agent name equals project prefix exactly — stripped to empty, fmt still works.
		{"proj", "proj-", "crew-proj-dev-"},
	}
	for _, tt := range tests {
		spec := AgentPodSpec{
			Mode: "crew", Project: tt.project, Role: "dev", AgentName: tt.agentName,
		}
		if got := spec.PodName(); got != tt.want {
			t.Errorf("PodName(%q, %q) = %q, want %q", tt.project, tt.agentName, got, tt.want)
		}
	}
}

func TestPodName_MultiRole(t *testing.T) {
	spec := AgentPodSpec{
		Mode: "crew", Project: "gardenio", Role: "crew,thread", AgentName: "test-1",
	}
	want := "crew-gardenio-crew-thread-test-1"
	if got := spec.PodName(); got != want {
		t.Errorf("PodName() = %q, want %q", got, want)
	}
}

func TestLabels_MultiRole(t *testing.T) {
	spec := AgentPodSpec{
		Mode: "crew", Project: "gardenio", Role: "crew,thread", AgentName: "test-1",
	}
	labels := spec.Labels()
	if got := labels[LabelRole]; got != "crew-thread" {
		t.Errorf("Labels()[%q] = %q, want %q", LabelRole, got, "crew-thread")
	}
}

// ── restartPolicyForMode ───────────────────────────────────────────────────

func TestRestartPolicyForMode(t *testing.T) {
	tests := []struct {
		mode string
		want corev1.RestartPolicy
	}{
		{"crew", corev1.RestartPolicyAlways},
		{"job", corev1.RestartPolicyNever},
		{"unknown", corev1.RestartPolicyAlways},
	}
	for _, tt := range tests {
		if got := restartPolicyForMode(tt.mode); got != tt.want {
			t.Errorf("restartPolicyForMode(%q) = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

// ── K8sManager CRUD (with fake client) ─────────────────────────────────────

func TestCreateAgentPod(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())
	spec := minimalSpec()

	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("CreateAgentPod: %v", err)
	}

	pod, err := client.CoreV1().Pods("default").Get(context.Background(), spec.PodName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Name != spec.PodName() {
		t.Errorf("pod name = %q, want %q", pod.Name, spec.PodName())
	}
}

// TestCreateAgentPod_MissingSecret verifies the admission gate (ka-0000001230):
// when the pod spec references a K8s Secret that does not exist in the
// namespace, CreateAgentPod returns a *MissingSecretsError without creating
// the pod. This closes the ProjectScoped ExternalSecret reconciliation race
// where a pod could otherwise be scheduled before external-secrets-operator
// synced the K8s Secret from AWS Secrets Manager.
func TestCreateAgentPod_MissingSecret(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, testLogger())
	spec := minimalSpec()
	spec.SecretEnv = []SecretEnvSource{
		{EnvName: "JIRA_API_TOKEN", SecretName: "gardenio-jira", SecretKey: "api-token"},
	}

	err := m.CreateAgentPod(context.Background(), spec)
	if err == nil {
		t.Fatal("CreateAgentPod: expected MissingSecretsError, got nil")
	}
	var missErr *MissingSecretsError
	if !errors.As(err, &missErr) {
		t.Fatalf("CreateAgentPod: error = %v (%T), want *MissingSecretsError", err, err)
	}
	if len(missErr.Names) != 1 || missErr.Names[0] != "gardenio-jira" {
		t.Errorf("missing names = %v, want [gardenio-jira]", missErr.Names)
	}

	// Pod must not have been created.
	if _, err := client.CoreV1().Pods("default").Get(context.Background(), spec.PodName(), metav1.GetOptions{}); err == nil {
		t.Errorf("pod %s was created despite missing secret", spec.PodName())
	}
}

// TestCreateAgentPod_SecretExists verifies the admission gate is a no-op
// once the referenced K8s Secret exists — i.e. the pod is created normally.
func TestCreateAgentPod_SecretExists(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gardenio-jira", Namespace: "default"},
	})
	m := New(client, testLogger())
	spec := minimalSpec()
	spec.SecretEnv = []SecretEnvSource{
		{EnvName: "JIRA_API_TOKEN", SecretName: "gardenio-jira", SecretKey: "api-token"},
	}

	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("CreateAgentPod: %v", err)
	}
	if _, err := client.CoreV1().Pods("default").Get(context.Background(), spec.PodName(), metav1.GetOptions{}); err != nil {
		t.Errorf("pod not created: %v", err)
	}
}

// TestCreateAgentPod_OptionalSecretMissing_DoesNotBlock verifies that an
// OPTIONAL SecretEnv source whose Secret does not exist does NOT defer pod
// creation. The SecretKeyRef is marked optional in the pod spec (kubelet omits
// the env var when the secret is absent), so the admission gate must skip it —
// otherwise an intentionally-optional bundle like the env-side grafana-api-key
// (provisioned per-env over time) wedges all agent pod creation fleet-wide.
func TestCreateAgentPod_OptionalSecretMissing_DoesNotBlock(t *testing.T) {
	client := fake.NewClientset() // no secrets exist
	m := New(client, testLogger())
	spec := minimalSpec()
	spec.SecretEnv = []SecretEnvSource{
		{EnvName: "GRAFANA_API_KEY_PROD", SecretName: "grafana-api-key", SecretKey: "token-prod", Optional: true},
	}

	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("CreateAgentPod with a missing OPTIONAL secret should not error, got: %v", err)
	}
	if _, err := client.CoreV1().Pods("default").Get(context.Background(), spec.PodName(), metav1.GetOptions{}); err != nil {
		t.Errorf("pod not created despite the only missing secret being optional: %v", err)
	}
}

// TestCreateAgentPod_MissingSecret_AggregatesAllRefs verifies the admission
// gate considers every secret-bearing field on the spec (SecretEnv,
// CredentialsSecret, DaemonTokenSecret, NatsTokenSecret,
// ObjectStoreCredentialsSecret, GitCredentialsSecret, GitlabTokenSecret,
// ImagePullSecrets, SshKeys) and reports every missing name in a single
// error.
func TestCreateAgentPod_MissingSecret_AggregatesAllRefs(t *testing.T) {
	// Only the daemon token secret exists; everything else is missing.
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "beads-daemon-token", Namespace: "default"},
	})
	m := New(client, testLogger())
	spec := minimalSpec()
	spec.SecretEnv = []SecretEnvSource{
		{EnvName: "JIRA_API_TOKEN", SecretName: "gardenio-jira", SecretKey: "api-token"},
	}
	spec.CredentialsSecret = "claude-creds"
	spec.DaemonTokenSecret = "beads-daemon-token" // exists
	spec.NatsTokenSecret = "gardenio-nats-agent-creds"
	spec.ObjectStoreCredentialsSecret = "gardenio-minio-agent-credentials"
	spec.GitCredentialsSecret = "gardenio-git"
	spec.GitlabTokenSecret = "gardenio-gitlab"
	spec.ImagePullSecrets = []string{"ghcr-pull"}
	spec.SshKeys = map[string]SshKeySource{
		"perf-bastion": {SecretName: "gardenio-perf-bastion-key", SecretKey: "id_ed25519"},
	}

	err := m.CreateAgentPod(context.Background(), spec)
	var missErr *MissingSecretsError
	if !errors.As(err, &missErr) {
		t.Fatalf("CreateAgentPod: error = %v (%T), want *MissingSecretsError", err, err)
	}
	want := []string{
		"claude-creds",
		"gardenio-git",
		"gardenio-gitlab",
		"gardenio-jira",
		"gardenio-minio-agent-credentials",
		"gardenio-nats-agent-creds",
		"gardenio-perf-bastion-key",
		"ghcr-pull",
	}
	if !reflect.DeepEqual(missErr.Names, want) {
		t.Errorf("missing names = %v, want %v", missErr.Names, want)
	}
}

func TestCreateAgentPod_Idempotent(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())
	spec := minimalSpec()
	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("first CreateAgentPod: %v", err)
	}
	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("second CreateAgentPod should be idempotent: %v", err)
	}
}

func TestCreateAgentPod_WithPVC(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())
	spec := minimalSpec()
	spec.WorkspaceStorage = &WorkspaceStorageSpec{
		Size:             "10Gi",
		StorageClassName: "gp3",
	}

	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("CreateAgentPod: %v", err)
	}

	// PVC should be created.
	pvcName := spec.PodName() + "-workspace"
	pvc, err := client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), pvcName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "gp3" {
		t.Error("PVC storage class not set")
	}

	// PVC must NOT have an ownerReference pointing to the pod — the pod-UID
	// ownerReference would cause cascading GC of the PVC on pod termination
	// and wipe workspace state required for session resume.
	if len(pvc.OwnerReferences) != 0 {
		t.Errorf("PVC ownerReferences: got %d, want 0 (PVC lifetime decoupled from pod)", len(pvc.OwnerReferences))
	}

	// PVC must carry the last-active annotation so the GC reconciler can
	// reap it once the TTL expires.
	if got := pvc.Annotations[AnnotationPVCLastActive]; got == "" {
		t.Errorf("PVC %s annotation missing", AnnotationPVCLastActive)
	}
}

func TestCreateAgentPod_PVCIdempotent(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())
	spec := minimalSpec()
	spec.WorkspaceStorage = &WorkspaceStorageSpec{Size: "5Gi"}

	// Pre-create the PVC.
	pvcName := spec.PodName() + "-workspace"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("5Gi"),
				},
			},
		},
	}
	_, err := client.CoreV1().PersistentVolumeClaims("default").Create(context.Background(), pvc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("pre-create PVC: %v", err)
	}

	// CreateAgentPod should not fail on existing PVC.
	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("CreateAgentPod with existing PVC: %v", err)
	}
}

func TestEnsurePVC_Idempotent(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())
	spec := minimalSpec()
	spec.WorkspaceStorage = &WorkspaceStorageSpec{Size: "10Gi"}
	if err := m.ensurePVC(context.Background(), spec); err != nil {
		t.Fatalf("first ensurePVC: %v", err)
	}
	if err := m.ensurePVC(context.Background(), spec); err != nil {
		t.Fatalf("second ensurePVC should be idempotent: %v", err)
	}
}

func TestEnsurePVC_RefreshesLastActive(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())

	// Pin the clock so we can tell the refresh patch landed.
	t0 := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	m.nowFunc = func() time.Time { return t0 }

	spec := minimalSpec()
	spec.WorkspaceStorage = &WorkspaceStorageSpec{Size: "10Gi"}

	if err := m.ensurePVC(context.Background(), spec); err != nil {
		t.Fatalf("first ensurePVC: %v", err)
	}
	pvcName := spec.PodName() + "-workspace"
	pvc, err := client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), pvcName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	if got := pvc.Annotations[AnnotationPVCLastActive]; got != t0.Format(time.RFC3339) {
		t.Fatalf("first last-active = %q, want %q", got, t0.Format(time.RFC3339))
	}

	// Advance the clock; second ensurePVC (PVC already exists) must patch
	// the annotation forward so the GC reconciler resets its TTL.
	t1 := t0.Add(2 * time.Hour)
	m.nowFunc = func() time.Time { return t1 }

	if err := m.ensurePVC(context.Background(), spec); err != nil {
		t.Fatalf("second ensurePVC: %v", err)
	}
	pvc, err = client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), pvcName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-get PVC: %v", err)
	}
	if got := pvc.Annotations[AnnotationPVCLastActive]; got != t1.Format(time.RFC3339) {
		t.Errorf("refreshed last-active = %q, want %q", got, t1.Format(time.RFC3339))
	}
}

func TestDeleteAgentPod(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())
	spec := minimalSpec()

	// Create then delete.
	_ = m.CreateAgentPod(context.Background(), spec)
	if err := m.DeleteAgentPod(context.Background(), spec.PodName(), "default"); err != nil {
		t.Fatalf("DeleteAgentPod: %v", err)
	}

	_, err := client.CoreV1().Pods("default").Get(context.Background(), spec.PodName(), metav1.GetOptions{})
	if err == nil {
		t.Error("pod should be deleted")
	}
}

func TestDeleteAgentPod_NotFound(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())
	if err := m.DeleteAgentPod(context.Background(), "nonexistent", "default"); err != nil {
		t.Fatalf("DeleteAgentPod for missing pod should be idempotent: %v", err)
	}
}

func TestListAgentPods(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())

	spec1 := minimalSpec()
	spec2 := minimalSpec()
	spec2.AgentName = "test-2"

	_ = m.CreateAgentPod(context.Background(), spec1)
	_ = m.CreateAgentPod(context.Background(), spec2)

	pods, err := m.ListAgentPods(context.Background(), "default", map[string]string{
		LabelApp: LabelAppValue,
	})
	if err != nil {
		t.Fatalf("ListAgentPods: %v", err)
	}
	if len(pods) != 2 {
		t.Errorf("expected 2 pods, got %d", len(pods))
	}
}

func TestGetAgentPod(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())
	spec := minimalSpec()

	_ = m.CreateAgentPod(context.Background(), spec)

	pod, err := m.GetAgentPod(context.Background(), spec.PodName(), "default")
	if err != nil {
		t.Fatalf("GetAgentPod: %v", err)
	}
	if pod.Name != spec.PodName() {
		t.Errorf("pod name = %q, want %q", pod.Name, spec.PodName())
	}
}

func TestGetAgentPod_NotFound(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())

	_, err := m.GetAgentPod(context.Background(), "nonexistent", "default")
	if err == nil {
		t.Error("expected error for nonexistent pod")
	}
}

// ── DeleteAllAgentPods ──────────────────────────────────────────────────────

func TestDeleteAllAgentPods(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())

	for _, name := range []string{"a-1", "a-2", "a-3"} {
		spec := minimalSpec()
		spec.AgentName = name
		if err := m.CreateAgentPod(context.Background(), spec); err != nil {
			t.Fatalf("create pod %s: %v", name, err)
		}
	}

	deleted, err := m.DeleteAllAgentPods(context.Background(), "default", false)
	if err != nil {
		t.Fatalf("DeleteAllAgentPods: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}

	pods, _ := m.ListAgentPods(context.Background(), "default", map[string]string{LabelApp: LabelAppValue})
	if len(pods) != 0 {
		t.Errorf("expected 0 pods remaining, got %d", len(pods))
	}
}

func TestDeleteAllAgentPods_Force(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())

	spec := minimalSpec()
	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	deleted, err := m.DeleteAllAgentPods(context.Background(), "default", true)
	if err != nil {
		t.Fatalf("DeleteAllAgentPods force: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}

	pods, _ := m.ListAgentPods(context.Background(), "default", map[string]string{LabelApp: LabelAppValue})
	if len(pods) != 0 {
		t.Errorf("expected 0 pods remaining, got %d", len(pods))
	}
}

func TestDeleteAllAgentPods_NoPods(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())

	deleted, err := m.DeleteAllAgentPods(context.Background(), "default", false)
	if err != nil {
		t.Fatalf("DeleteAllAgentPods empty: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
}
