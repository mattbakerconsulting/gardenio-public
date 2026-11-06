package podmanager

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"
)

// --- buildPod tests ---

func TestBuildPod_BasicFields(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	pod := m.buildPod(spec)

	if pod.Name != "crew-gardenio-dev-test-1" {
		t.Errorf("pod.Name = %q, want %q", pod.Name, "crew-gardenio-dev-test-1")
	}
	if pod.Namespace != "default" {
		t.Errorf("pod.Namespace = %q, want %q", pod.Namespace, "default")
	}
	if got := pod.Annotations[AnnotationBeadID]; got != "bead-abc" {
		t.Errorf("bead-id annotation = %q, want %q", got, "bead-abc")
	}
	// Prometheus scrape annotations point vmagent at coop's /metrics on the
	// dedicated metrics port.
	if got := pod.Annotations["prometheus.io/scrape"]; got != "true" {
		t.Errorf("prometheus.io/scrape = %q, want %q", got, "true")
	}
	if got := pod.Annotations["prometheus.io/port"]; got != fmt.Sprintf("%d", CoopDefaultMetricsPort) {
		t.Errorf("prometheus.io/port = %q, want %d", got, CoopDefaultMetricsPort)
	}
	if got := pod.Annotations["prometheus.io/path"]; got != "/metrics" {
		t.Errorf("prometheus.io/path = %q, want %q", got, "/metrics")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("RestartPolicy = %q, want Always", pod.Spec.RestartPolicy)
	}
}

func TestBuildPod_JobMode(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.Mode = "job"
	pod := m.buildPod(spec)

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never for job mode", pod.Spec.RestartPolicy)
	}
}

func TestBuildPod_SecurityContext(t *testing.T) {
	m := newTestManager()
	pod := m.buildPod(minimalSpec())

	psc := pod.Spec.SecurityContext
	if psc == nil {
		t.Fatal("pod security context is nil")
	}
	if *psc.RunAsUser != AgentUID {
		t.Errorf("RunAsUser = %d, want %d", *psc.RunAsUser, AgentUID)
	}
	if *psc.RunAsGroup != AgentGID {
		t.Errorf("RunAsGroup = %d, want %d", *psc.RunAsGroup, AgentGID)
	}
	if !*psc.RunAsNonRoot {
		t.Error("RunAsNonRoot should be true")
	}
	if *psc.FSGroup != AgentGID {
		t.Errorf("FSGroup = %d, want %d", *psc.FSGroup, AgentGID)
	}
}

func TestBuildPod_TerminationGracePeriod(t *testing.T) {
	m := newTestManager()
	pod := m.buildPod(minimalSpec())

	if pod.Spec.TerminationGracePeriodSeconds == nil {
		t.Fatal("TerminationGracePeriodSeconds is nil")
	}
	if *pod.Spec.TerminationGracePeriodSeconds != 45 {
		t.Errorf("TerminationGracePeriodSeconds = %d, want 45",
			*pod.Spec.TerminationGracePeriodSeconds)
	}
}

func TestBuildPod_ServiceAccount(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.ServiceAccountName = "custom-sa"
	pod := m.buildPod(spec)

	if pod.Spec.ServiceAccountName != "custom-sa" {
		t.Errorf("ServiceAccountName = %q, want %q", pod.Spec.ServiceAccountName, "custom-sa")
	}
}

func TestBuildPod_RuntimeClassName(t *testing.T) {
	m := newTestManager()

	// Default: no RuntimeClassName means the cluster default runtime.
	pod := m.buildPod(minimalSpec())
	if pod.Spec.RuntimeClassName != nil {
		t.Errorf("RuntimeClassName = %v, want nil (cluster default)", *pod.Spec.RuntimeClassName)
	}

	// Explicit: pin to runsc (gVisor) for the freeze/thaw PoC.
	spec := minimalSpec()
	spec.RuntimeClassName = "runsc"
	pod = m.buildPod(spec)
	if pod.Spec.RuntimeClassName == nil {
		t.Fatal("RuntimeClassName is nil; want runsc")
	}
	if *pod.Spec.RuntimeClassName != "runsc" {
		t.Errorf("RuntimeClassName = %q, want %q", *pod.Spec.RuntimeClassName, "runsc")
	}
}

func TestBuildPod_AutomountServiceAccountTokenDefaultFalse(t *testing.T) {
	m := newTestManager()
	pod := m.buildPod(minimalSpec())

	if pod.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("AutomountServiceAccountToken is nil; want explicit false")
	}
	if *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = true, want false by default (ka-0000006344)")
	}
}

func TestBuildPod_AutomountServiceAccountTokenOptIn(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.NeedsK8sAPI = true
	pod := m.buildPod(spec)

	if pod.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("AutomountServiceAccountToken is nil; want explicit true")
	}
	if !*pod.Spec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = false, want true when NeedsK8sAPI is set")
	}
}

func TestBuildPod_NodeSelectorAndTolerations(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}
	spec.Tolerations = []corev1.Toleration{{Key: "gpu", Effect: corev1.TaintEffectNoSchedule}}
	pod := m.buildPod(spec)

	if got := pod.Spec.NodeSelector["kubernetes.io/arch"]; got != "amd64" {
		t.Errorf("NodeSelector[arch] = %q, want amd64", got)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "gpu" {
		t.Errorf("Tolerations not set correctly: %+v", pod.Spec.Tolerations)
	}
}

func TestBuildPod_KarpenterDoNotDisrupt(t *testing.T) {
	m := newTestManager()

	// EVERY agent role is protected from node consolidation — no role is
	// disruptable (operator directive, investigation ka-0000006340).
	for _, role := range []string{"dev", "crew", "job", "ops", "polecat", "thread", "dm", "gardener", "thread,crew"} {
		spec := minimalSpec()
		spec.Role = role
		pod := m.buildPod(spec)
		if got := pod.Annotations[AnnotationKarpenterDoNotDisrupt]; got != "true" {
			t.Errorf("role %q: do-not-disrupt = %q, want %q", role, got, "true")
		}
	}
}

func TestBuildPod_SpawnProvenanceAnnotations(t *testing.T) {
	m := newTestManager()

	// All three set: an escalation-approved spawn (gd-h7fgTgcuyn).
	spec := minimalSpec()
	spec.SpawnedBy = "esc-123"
	spec.SpawnApprovalMode = "human"
	spec.SpawnApprovedBy = "oidc:operator@example.com"
	pod := m.buildPod(spec)

	if got := pod.Annotations[AnnotationSpawnedBy]; got != "esc-123" {
		t.Errorf("%s = %q, want %q", AnnotationSpawnedBy, got, "esc-123")
	}
	if got := pod.Annotations[AnnotationSpawnApprovalMode]; got != "human" {
		t.Errorf("%s = %q, want %q", AnnotationSpawnApprovalMode, got, "human")
	}
	if got := pod.Annotations[AnnotationSpawnApprovedBy]; got != "oidc:operator@example.com" {
		t.Errorf("%s = %q, want %q", AnnotationSpawnApprovedBy, got, "oidc:operator@example.com")
	}

	// Ordinary, non-escalated spawn: none of the three annotations appear.
	ordinary := m.buildPod(minimalSpec())
	for _, key := range []string{AnnotationSpawnedBy, AnnotationSpawnApprovalMode, AnnotationSpawnApprovedBy} {
		if _, ok := ordinary.Annotations[key]; ok {
			t.Errorf("ordinary spawn: annotation %s unexpectedly set to %q", key, ordinary.Annotations[key])
		}
	}
}

func TestBuildPod_NoInitContainerWithoutGitURL(t *testing.T) {
	m := newTestManager()
	pod := m.buildPod(minimalSpec())

	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("expected no init containers without GitURL, got %d", len(pod.Spec.InitContainers))
	}
}

func TestBuildPod_InitContainerWithGitURL(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.GitURL = "https://github.com/example/repo.git"
	pod := m.buildPod(spec)

	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}
	ic := pod.Spec.InitContainers[0]
	if ic.Name != InitCloneName {
		t.Errorf("init container name = %q, want %q", ic.Name, InitCloneName)
	}
	if ic.Image != InitCloneImage {
		t.Errorf("init container image = %q, want %q", ic.Image, InitCloneImage)
	}
}

func TestBuildPod_OperatedByEnvVar(t *testing.T) {
	m := newTestManager()

	// Unset: BEADS_OPERATED_BY must NOT appear in env. Empty values would
	// still count as header_present=false on the server, so we omit entirely.
	pod := m.buildPod(minimalSpec())
	c := pod.Spec.Containers[0]
	for _, e := range c.Env {
		if e.Name == "BEADS_OPERATED_BY" {
			t.Errorf("BEADS_OPERATED_BY should be absent when spec.OperatedBy is empty, got %q", e.Value)
		}
	}

	// Set: BEADS_OPERATED_BY must be injected with the exact opaque id.
	spec := minimalSpec()
	spec.OperatedBy = "slack:U12345"
	pod = m.buildPod(spec)
	c = pod.Spec.Containers[0]
	var got string
	var found bool
	for _, e := range c.Env {
		if e.Name == "BEADS_OPERATED_BY" {
			got = e.Value
			found = true
		}
	}
	if !found {
		t.Fatal("BEADS_OPERATED_BY env var missing when spec.OperatedBy is set")
	}
	if got != "slack:U12345" {
		t.Errorf("BEADS_OPERATED_BY = %q, want %q", got, "slack:U12345")
	}
}

// --- buildContainer tests ---

func TestBuildContainer_PreStopHook(t *testing.T) {
	m := newTestManager()
	c := m.buildContainer(minimalSpec())

	if c.Lifecycle == nil || c.Lifecycle.PreStop == nil {
		t.Fatal("container missing PreStop lifecycle hook")
	}
	if c.Lifecycle.PreStop.Exec == nil {
		t.Fatal("PreStop hook should use Exec action")
	}
	cmd := c.Lifecycle.PreStop.Exec.Command
	if len(cmd) < 3 {
		t.Fatalf("PreStop command too short: %v", cmd)
	}
	if cmd[0] != "sh" || cmd[1] != "-c" {
		t.Errorf("PreStop command prefix = %v, want [sh -c ...]", cmd[:2])
	}
	// Salvage must run FIRST — before the Escape/shutdown drain sequence — so it
	// executes at t≈0 with the worktree intact, before coop drain eats the grace
	// budget (bug ka-0000000146). Guarded by `|| true` so it never blocks drain.
	script := cmd[2]
	salvageIdx := strings.Index(script, "gb salvage")
	shutdownIdx := strings.Index(script, "/api/v1/shutdown")
	if salvageIdx < 0 {
		t.Errorf("PreStop command missing `gb salvage`: %q", script)
	}
	if shutdownIdx >= 0 && salvageIdx > shutdownIdx {
		t.Errorf("PreStop must run `gb salvage` before the shutdown drain: %q", script)
	}
	if salvageIdx >= 0 && !strings.Contains(script[salvageIdx:], "|| true") {
		t.Errorf("PreStop `gb salvage` must be guarded by `|| true`: %q", script)
	}
}

func TestBuildContainer_Probes(t *testing.T) {
	m := newTestManager()
	c := m.buildContainer(minimalSpec())

	if c.LivenessProbe == nil {
		t.Error("missing liveness probe")
	}
	if c.ReadinessProbe == nil {
		t.Error("missing readiness probe")
	}
	if c.StartupProbe == nil {
		t.Error("missing startup probe")
	}

	// Verify probe paths.
	wantPaths := map[string]string{
		"liveness":  "/api/v1/livez",
		"readiness": "/api/v1/ready",
		"startup":   "/api/v1/health",
	}
	for name, probe := range map[string]*corev1.Probe{
		"liveness":  c.LivenessProbe,
		"readiness": c.ReadinessProbe,
		"startup":   c.StartupProbe,
	} {
		if probe.HTTPGet == nil {
			t.Errorf("%s probe should be HTTPGet", name)
			continue
		}
		if probe.HTTPGet.Path != wantPaths[name] {
			t.Errorf("%s probe path = %q, want %q", name, probe.HTTPGet.Path, wantPaths[name])
		}
	}

	// Startup budget = FailureThreshold * PeriodSeconds. gb init runs the
	// workspace git clone before coop launches, so this window must be large
	// enough for a slow throttled clone of a 250MB+ repo to finish. Raised
	// 60->180 (300s->900s = 15min) for ka-0000000649 (prod crashloop).
	if got := c.StartupProbe.FailureThreshold; got != 180 {
		t.Errorf("startup probe FailureThreshold = %d, want 180 (ka-0000000649: slow clone needs headroom)", got)
	}
	if got := c.StartupProbe.PeriodSeconds; got != 5 {
		t.Errorf("startup probe PeriodSeconds = %d, want 5", got)
	}
}

// TestBuildContainer_StartupProbeOverride verifies the operator knob
// (ka-0000000649): StartupProbeFailureThresholdOverride > 0 replaces the
// compiled default so a slow-egress cluster can widen the cold-start budget via
// agents.startupProbeFailureThreshold without re-cutting a controller release;
// <= 0 keeps the default.
func TestBuildContainer_StartupProbeOverride(t *testing.T) {
	t.Run("override wins when positive", func(t *testing.T) {
		m := newTestManager()
		m.StartupProbeFailureThresholdOverride = 360
		c := m.buildContainer(minimalSpec())
		if got := c.StartupProbe.FailureThreshold; got != 360 {
			t.Errorf("FailureThreshold = %d, want 360 (override)", got)
		}
	})
	t.Run("zero keeps compiled default", func(t *testing.T) {
		m := newTestManager()
		m.StartupProbeFailureThresholdOverride = 0
		c := m.buildContainer(minimalSpec())
		if got := c.StartupProbe.FailureThreshold; got != int32(defaultAgentStartupProbeFailureThreshold) {
			t.Errorf("FailureThreshold = %d, want %d (default)", got, defaultAgentStartupProbeFailureThreshold)
		}
	})
}

func TestBuildContainer_Ports(t *testing.T) {
	m := newTestManager()
	c := m.buildContainer(minimalSpec())

	if len(c.Ports) != 4 {
		t.Fatalf("expected 4 ports, got %d", len(c.Ports))
	}

	portMap := make(map[string]int32)
	for _, p := range c.Ports {
		portMap[p.Name] = p.ContainerPort
	}
	if portMap["api"] != CoopDefaultPort {
		t.Errorf("api port = %d, want %d", portMap["api"], CoopDefaultPort)
	}
	if portMap["health"] != CoopDefaultHealthPort {
		t.Errorf("health port = %d, want %d", portMap["health"], CoopDefaultHealthPort)
	}
	if portMap["metrics"] != CoopDefaultMetricsPort {
		t.Errorf("metrics port = %d, want %d", portMap["metrics"], CoopDefaultMetricsPort)
	}
	if portMap["channel"] != ChannelPort {
		t.Errorf("channel port = %d, want %d", portMap["channel"], ChannelPort)
	}
}

func TestBuildContainer_SecurityContext(t *testing.T) {
	m := newTestManager()
	c := m.buildContainer(minimalSpec())

	sc := c.SecurityContext
	if sc == nil {
		t.Fatal("container security context is nil")
	}
	if *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation should be false (agent runs unprivileged; no sudo)")
	}
	if *sc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem should be false")
	}
	if sc.Capabilities == nil {
		t.Fatal("Capabilities is nil")
	}
	if len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("Drop = %v, want [ALL]", sc.Capabilities.Drop)
	}
	if len(sc.Capabilities.Add) != 0 {
		t.Errorf("Add caps = %v, want none (no sudo/SETUID/SETGID)", sc.Capabilities.Add)
	}
}

func TestBuildContainer_DefaultResources(t *testing.T) {
	m := newTestManager()
	c := m.buildContainer(minimalSpec())

	cpuReq := c.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != DefaultCPURequest {
		t.Errorf("CPU request = %s, want %s", cpuReq.String(), DefaultCPURequest)
	}
	memLimit := c.Resources.Limits[corev1.ResourceMemory]
	if memLimit.String() != DefaultMemoryLimit {
		t.Errorf("Memory limit = %s, want %s", memLimit.String(), DefaultMemoryLimit)
	}
}

func TestBuildContainer_CustomResources(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"),
		},
	}
	c := m.buildContainer(spec)

	cpuReq := c.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "500m" {
		t.Errorf("CPU request = %s, want 500m", cpuReq.String())
	}
}

// --- buildEnvVars tests ---

func TestBuildEnvVars_StandardVars(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	envVars := m.buildEnvVars(spec)

	envMap := make(map[string]corev1.EnvVar)
	for _, ev := range envVars {
		envMap[ev.Name] = ev
	}

	checks := map[string]string{
		"BOAT_ROLE":    "dev",
		"BOAT_PROJECT": "gardenio",
		"BOAT_AGENT":   "test-1",
		"BOAT_MODE":    "crew",
		"HOME":         "/home/agent",
		"BEADS_ACTOR":  "test-1",
		"KD_ACTOR":     "test-1",
		"KD_AGENT_ID":  "bead-abc",
	}
	for name, want := range checks {
		ev, ok := envMap[name]
		if !ok {
			t.Errorf("missing env var %s", name)
			continue
		}
		if ev.Value != want {
			t.Errorf("env %s = %q, want %q", name, ev.Value, want)
		}
	}

	// POD_IP should use downward API.
	podIP, ok := envMap["POD_IP"]
	if !ok {
		t.Fatal("missing POD_IP env var")
	}
	if podIP.ValueFrom == nil || podIP.ValueFrom.FieldRef == nil {
		t.Error("POD_IP should use fieldRef downward API")
	}

	// POD_NAME must be injected via the downward API (fieldPath: metadata.name)
	// so `gb agent start` registers the coopmux session under the FULL,
	// untruncated pod name. $HOSTNAME is capped at 63 bytes by the kernel, so a
	// 64+ char pod name registered via hostname is silently truncated and can
	// never match the full-name terminal deep-link on the Slack agent card
	// (ka-0000003655). A plain-value POD_NAME would defeat the purpose — it must
	// come from the downward API, which is not length-capped.
	podName, ok := envMap["POD_NAME"]
	if !ok {
		t.Fatal("missing POD_NAME env var")
	}
	if podName.ValueFrom == nil || podName.ValueFrom.FieldRef == nil {
		t.Fatal("POD_NAME should use fieldRef downward API")
	}
	if got := podName.ValueFrom.FieldRef.FieldPath; got != "metadata.name" {
		t.Errorf("POD_NAME fieldPath = %q, want %q", got, "metadata.name")
	}
	if podName.Value != "" {
		t.Errorf("POD_NAME must be empty apart from ValueFrom, got Value=%q", podName.Value)
	}
}

func TestBuildEnvVars_MultiRolePreservesComma(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.Role = "crew,thread"
	envVars := m.buildEnvVars(spec)

	for _, ev := range envVars {
		if ev.Name == "BOAT_ROLE" {
			if ev.Value != "crew,thread" {
				t.Errorf("BOAT_ROLE = %q, want %q (original comma-separated value)", ev.Value, "crew,thread")
			}
			return
		}
	}
	t.Error("BOAT_ROLE env var not found")
}

// TestBuildEnvVars_GoMemLimit pins GOMEMLIMIT to GoMemLimitPercent of the
// agent container's memory limit (gd-zBUZ9bwCKl): a 3Gi crew pod gets 1382MiB,
// so one big-package test compile no longer OOM-kills the pod.
func TestBuildEnvVars_GoMemLimit(t *testing.T) {
	m := newTestManager()
	cases := []struct {
		name string
		res  *corev1.ResourceRequirements
		env  map[string]string
		want string
	}{
		{name: "default 16Gi limit", want: "7372MiB"},
		{name: "3Gi project limit", res: &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("3Gi")},
		}, want: "1382MiB"},
		{name: "no memory limit", res: &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		}},
		{name: "spec.Env override wins", res: &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("3Gi")},
		}, env: map[string]string{"GOMEMLIMIT": "2GiB"}, want: "2GiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := minimalSpec()
			spec.Resources = tc.res
			spec.Env = tc.env
			var got []string
			for _, ev := range m.buildEnvVars(spec) {
				if ev.Name == "GOMEMLIMIT" {
					got = append(got, ev.Value)
				}
			}
			switch {
			case tc.want == "" && len(got) != 0:
				t.Errorf("GOMEMLIMIT = %v, want unset", got)
			case tc.want != "" && (len(got) != 1 || got[0] != tc.want):
				t.Errorf("GOMEMLIMIT = %v, want exactly [%s]", got, tc.want)
			}
		})
	}
}

func TestBuildEnvVars_SessionResume(t *testing.T) {
	m := newTestManager()

	// Without workspace storage — no BOAT_SESSION_RESUME.
	spec := minimalSpec()
	envVars := m.buildEnvVars(spec)
	for _, ev := range envVars {
		if ev.Name == "BOAT_SESSION_RESUME" {
			t.Error("BOAT_SESSION_RESUME should not be set without WorkspaceStorage")
		}
	}

	// With workspace storage — BOAT_SESSION_RESUME=1.
	spec.WorkspaceStorage = &WorkspaceStorageSpec{Size: "5Gi"}
	envVars = m.buildEnvVars(spec)
	found := false
	for _, ev := range envVars {
		if ev.Name == "BOAT_SESSION_RESUME" {
			found = true
			if ev.Value != "1" {
				t.Errorf("BOAT_SESSION_RESUME = %q, want %q", ev.Value, "1")
			}
		}
	}
	if !found {
		t.Error("BOAT_SESSION_RESUME not set with WorkspaceStorage")
	}

	// Explicit opt-out — BOAT_SESSION_RESUME=0.
	optOut := false
	spec.SessionResume = &optOut
	envVars = m.buildEnvVars(spec)
	for _, ev := range envVars {
		if ev.Name == "BOAT_SESSION_RESUME" {
			if ev.Value != "0" {
				t.Errorf("BOAT_SESSION_RESUME = %q, want %q (opt-out)", ev.Value, "0")
			}
			return
		}
	}
	t.Error("BOAT_SESSION_RESUME not set with SessionResume=&false")
}

func TestBuildEnvVars_SessionResumeExplicitTrue(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.WorkspaceStorage = &WorkspaceStorageSpec{Size: "5Gi"}
	optIn := true
	spec.SessionResume = &optIn

	envVars := m.buildEnvVars(spec)
	for _, ev := range envVars {
		if ev.Name == "BOAT_SESSION_RESUME" {
			if ev.Value != "1" {
				t.Errorf("BOAT_SESSION_RESUME = %q, want %q (opt-in)", ev.Value, "1")
			}
			return
		}
	}
	t.Error("BOAT_SESSION_RESUME not set with SessionResume=&true")
}

func TestBuildEnvVars_SessionResumeOptOutWithoutPVC(t *testing.T) {
	// Without WorkspaceStorage, the env var is never emitted regardless of
	// SessionResume — no PVC means no resume to control.
	m := newTestManager()
	spec := minimalSpec()
	optOut := false
	spec.SessionResume = &optOut

	envVars := m.buildEnvVars(spec)
	for _, ev := range envVars {
		if ev.Name == "BOAT_SESSION_RESUME" {
			t.Error("BOAT_SESSION_RESUME should not be set without WorkspaceStorage")
		}
	}
}

// TestBuildEnvVars_S3ResumeContractEnvVars asserts that every env var read by
// gardenio/controller/cmd/gb init's S3 resume code path is present in the
// rendered agent pod env.
//
// The S3 resume contract (see docs/research/s3-resume-e2e.md, gap G14)
// depends on a fan-out of env vars set across helm chart → controller pod
// env → controller config → AgentPodSpec.Env → rendered corev1.EnvVar:
//
//   - COOP_S3_BUCKET / COOP_S3_PREFIX / COOP_S3_REGION are set on the
//     controller pod by helm/gardenio/templates/controller/deployment.yaml
//     and forwarded to agent pods by cmd/controller/podspec.go via
//     spec.Env (applyCommonConfig).
//   - BOAT_SESSION_RESUME is emitted here in buildEnvVars when a workspace
//     PVC is present, gated by SessionResume.
//   - KD_AGENT_ID and BOAT_AGENT_BEAD_ID are emitted here in buildEnvVars
//     unconditionally, both pointing at spec.BeadID.
//
// COOP_S3_SESSION_KEY is intentionally NOT in this list: it is set by
// `gb init` itself at runtime (init_agent_loop.go pins it to the agent
// bead ID before exec'ing coop), not by the pod spec. See coop's
// resolve_session_key precedence in coop/crates/cli/src/s3.rs.
//
// A typo in any layer (e.g. COOP_S3_BUCKETT, BOAT_SESSION_RESUMEE) would
// silently disable archival or resume; this test fails loudly instead.
func TestBuildEnvVars_S3ResumeContractEnvVars(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	// PVC present → BOAT_SESSION_RESUME is emitted by buildEnvVars.
	spec.WorkspaceStorage = &WorkspaceStorageSpec{Size: "5Gi"}
	// Mirror what cmd/controller/podspec.go's applyCommonConfig writes
	// when the controller pod has CoopS3Bucket / CoopS3Prefix / CoopS3Region
	// set from helm values agents.s3.bucket / .prefix / .region.
	spec.Env = map[string]string{
		"COOP_S3_BUCKET": "gardenio-sessions",
		"COOP_S3_PREFIX": "coop/sessions",
		"COOP_S3_REGION": "us-west-2",
	}

	envVars := m.buildEnvVars(spec)
	got := make(map[string]string, len(envVars))
	for _, ev := range envVars {
		got[ev.Name] = ev.Value
	}

	// The full canonical set that gb init reads. Keep this list aligned
	// with docs/research/s3-resume-e2e.md §1.2 and §2.1.
	want := map[string]string{
		"COOP_S3_BUCKET":      "gardenio-sessions",
		"COOP_S3_PREFIX":      "coop/sessions",
		"COOP_S3_REGION":      "us-west-2",
		"BOAT_SESSION_RESUME": "1",
		"KD_AGENT_ID":         spec.BeadID,
		"BOAT_AGENT_BEAD_ID":  spec.BeadID,
	}
	for name, wantValue := range want {
		gotValue, ok := got[name]
		if !ok {
			t.Errorf("S3 resume contract env var %q missing from rendered pod env (gap G14)", name)
			continue
		}
		if gotValue != wantValue {
			t.Errorf("env %s = %q, want %q", name, gotValue, wantValue)
		}
	}
}

func TestBuildEnvVars_CustomEnv(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.Env = map[string]string{"CUSTOM_KEY": "custom_value"}
	envVars := m.buildEnvVars(spec)

	found := false
	for _, ev := range envVars {
		if ev.Name == "CUSTOM_KEY" && ev.Value == "custom_value" {
			found = true
		}
	}
	if !found {
		t.Error("custom env var not found")
	}
}

func TestBuildEnvVars_SecretEnv(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.SecretEnv = []SecretEnvSource{
		{EnvName: "MY_SECRET", SecretName: "my-secret", SecretKey: "key"},
	}
	envVars := m.buildEnvVars(spec)

	found := false
	for _, ev := range envVars {
		if ev.Name == "MY_SECRET" {
			found = true
			if ev.ValueFrom == nil || ev.ValueFrom.SecretKeyRef == nil {
				t.Error("MY_SECRET should use SecretKeyRef")
			} else if ev.ValueFrom.SecretKeyRef.Name != "my-secret" {
				t.Errorf("secret name = %q, want my-secret", ev.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	if !found {
		t.Error("secret env var not found")
	}
}

func TestBuildEnvVars_DaemonToken(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.DaemonTokenSecret = "daemon-token-secret"
	envVars := m.buildEnvVars(spec)

	found := false
	for _, ev := range envVars {
		if ev.Name == "BEADS_DAEMON_TOKEN" {
			found = true
			if ev.ValueFrom == nil || ev.ValueFrom.SecretKeyRef == nil {
				t.Error("BEADS_DAEMON_TOKEN should use SecretKeyRef")
			} else {
				ref := ev.ValueFrom.SecretKeyRef
				if ref.Name != "daemon-token-secret" || ref.Key != "token" {
					t.Errorf("secret ref = %s/%s, want daemon-token-secret/token", ref.Name, ref.Key)
				}
			}
		}
	}
	if !found {
		t.Error("BEADS_DAEMON_TOKEN env var not found")
	}
}

// --- buildResources tests ---

func TestBuildResources_Default(t *testing.T) {
	mgr := New(fake.NewSimpleClientset(), testLogger())
	spec := AgentPodSpec{}

	res := mgr.buildResources(spec)

	cpuReq := res.Requests[corev1.ResourceCPU]
	if cpuReq.String() != DefaultCPURequest {
		t.Errorf("default CPU request = %s, want %s", cpuReq.String(), DefaultCPURequest)
	}
	memLimit := res.Limits[corev1.ResourceMemory]
	if memLimit.String() != DefaultMemoryLimit {
		t.Errorf("default memory limit = %s, want %s", memLimit.String(), DefaultMemoryLimit)
	}
}

func TestBuildResources_Custom(t *testing.T) {
	mgr := New(fake.NewSimpleClientset(), testLogger())
	custom := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"),
		},
	}
	spec := AgentPodSpec{Resources: custom}

	res := mgr.buildResources(spec)
	cpuReq := res.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "500m" {
		t.Errorf("custom CPU request = %s, want 500m", cpuReq.String())
	}
}

// ── ApplyDefaults ──────────────────────────────────────────────────────────

func TestApplyDefaults_Nil(t *testing.T) {
	spec := minimalSpec()
	original := spec.Image
	ApplyDefaults(&spec, nil)
	if spec.Image != original {
		t.Error("nil defaults should not change spec")
	}
}

func TestApplyDefaults_FillEmpty(t *testing.T) {
	spec := AgentPodSpec{}
	defaults := &PodDefaults{
		Image:              "default-image:latest",
		ServiceAccountName: "default-sa",
		ConfigMapName:      "default-cm",
		NodeSelector:       map[string]string{"zone": "us-east-1a"},
		Tolerations:        []corev1.Toleration{{Key: "spot"}},
	}
	ApplyDefaults(&spec, defaults)

	if spec.Image != "default-image:latest" {
		t.Errorf("Image = %q, want default-image:latest", spec.Image)
	}
	if spec.ServiceAccountName != "default-sa" {
		t.Errorf("ServiceAccountName = %q, want default-sa", spec.ServiceAccountName)
	}
	if spec.ConfigMapName != "default-cm" {
		t.Errorf("ConfigMapName = %q, want default-cm", spec.ConfigMapName)
	}
	if spec.NodeSelector["zone"] != "us-east-1a" {
		t.Error("NodeSelector not applied")
	}
	if len(spec.Tolerations) != 1 {
		t.Error("Tolerations not applied")
	}
}

func TestApplyDefaults_SpecTakesPrecedence(t *testing.T) {
	spec := AgentPodSpec{
		Image:              "custom:v1",
		ServiceAccountName: "custom-sa",
		Env:                map[string]string{"KEY": "spec-value"},
	}
	defaults := &PodDefaults{
		Image:              "default:latest",
		ServiceAccountName: "default-sa",
		Env:                map[string]string{"KEY": "default-value", "OTHER": "default-other"},
	}
	ApplyDefaults(&spec, defaults)

	if spec.Image != "custom:v1" {
		t.Errorf("Image = %q, want custom:v1 (spec should win)", spec.Image)
	}
	if spec.ServiceAccountName != "custom-sa" {
		t.Errorf("ServiceAccountName = %q, want custom-sa (spec should win)", spec.ServiceAccountName)
	}
	if spec.Env["KEY"] != "spec-value" {
		t.Errorf("Env[KEY] = %q, want spec-value (spec should win)", spec.Env["KEY"])
	}
	if spec.Env["OTHER"] != "default-other" {
		t.Errorf("Env[OTHER] = %q, want default-other (should be merged from defaults)", spec.Env["OTHER"])
	}
}

func TestApplyDefaults_WorkspaceStorage(t *testing.T) {
	spec := AgentPodSpec{}
	defaults := &PodDefaults{
		WorkspaceStorage: &WorkspaceStorageSpec{Size: "20Gi"},
	}
	ApplyDefaults(&spec, defaults)
	if spec.WorkspaceStorage == nil {
		t.Fatal("WorkspaceStorage should be applied from defaults")
	}
	if spec.WorkspaceStorage.Size != "20Gi" {
		t.Errorf("Size = %q, want 20Gi", spec.WorkspaceStorage.Size)
	}

	// Spec already has WorkspaceStorage — should not be overwritten.
	spec2 := AgentPodSpec{
		WorkspaceStorage: &WorkspaceStorageSpec{Size: "5Gi"},
	}
	ApplyDefaults(&spec2, defaults)
	if spec2.WorkspaceStorage.Size != "5Gi" {
		t.Errorf("Size = %q, want 5Gi (spec should win)", spec2.WorkspaceStorage.Size)
	}
}

// TestBuildEnvVars_TriggerBeadID covers auto-link M3 (ka-0000003164):
// when AgentPodSpec.TriggerBeadID is set, the pod must carry
// BOAT_TRIGGER_BEAD_ID so the agent harness can propagate it as
// X-Trigger-Bead on its first kd claim.
func TestBuildEnvVars_TriggerBeadID(t *testing.T) {
	m := newTestManager()

	// Empty TriggerBeadID — env var must be absent.
	spec := minimalSpec()
	envVars := m.buildEnvVars(spec)
	for _, ev := range envVars {
		if ev.Name == "BOAT_TRIGGER_BEAD_ID" {
			t.Fatalf("BOAT_TRIGGER_BEAD_ID should not be set when TriggerBeadID is empty; got %q", ev.Value)
		}
	}

	// Populated TriggerBeadID — env var must carry the value.
	spec.TriggerBeadID = "gd-trig-99"
	envVars = m.buildEnvVars(spec)
	var found *corev1.EnvVar
	for i := range envVars {
		if envVars[i].Name == "BOAT_TRIGGER_BEAD_ID" {
			found = &envVars[i]
			break
		}
	}
	if found == nil {
		t.Fatal("BOAT_TRIGGER_BEAD_ID env var not found")
	}
	if found.Value != "gd-trig-99" {
		t.Errorf("BOAT_TRIGGER_BEAD_ID = %q, want %q", found.Value, "gd-trig-99")
	}
}

func TestBuildEnvVars_RuntimeClass(t *testing.T) {
	m := newTestManager()

	// No RuntimeClassName (cluster default runtime) — env var must be absent.
	spec := minimalSpec()
	envVars := m.buildEnvVars(spec)
	for _, ev := range envVars {
		if ev.Name == "BOAT_RUNTIME_CLASS" {
			t.Fatalf("BOAT_RUNTIME_CLASS should not be set when RuntimeClassName is empty; got %q", ev.Value)
		}
	}

	// Pinned runtime class — env var must carry the resolved value so coop can
	// surface it in its coopmux registration metadata.
	spec.RuntimeClassName = "runsc"
	envVars = m.buildEnvVars(spec)
	var found *corev1.EnvVar
	for i := range envVars {
		if envVars[i].Name == "BOAT_RUNTIME_CLASS" {
			found = &envVars[i]
			break
		}
	}
	if found == nil {
		t.Fatal("BOAT_RUNTIME_CLASS env var not found")
	}
	if found.Value != "runsc" {
		t.Errorf("BOAT_RUNTIME_CLASS = %q, want %q", found.Value, "runsc")
	}
}

// --- prewarmed PriorityClass (ka-0000000722) ---

// A prewarmed pod with a configured PrewarmedPriorityClass carries it so the
// scheduler can preempt the idle pool member for a cold real agent under node
// pressure.
func TestBuildPod_PrewarmedPriorityClass(t *testing.T) {
	m := newTestManager()
	m.PrewarmedPriorityClass = "gardenio-prewarmed"
	spec := minimalSpec()
	spec.Prewarmed = true
	pod := m.buildPod(spec)

	if got := pod.Spec.PriorityClassName; got != "gardenio-prewarmed" {
		t.Errorf("PriorityClassName = %q, want %q", got, "gardenio-prewarmed")
	}
}

// A real (non-prewarmed) agent must NEVER get the low-priority class, even when
// the override is configured — only parked pool pods are demotable.
func TestBuildPod_PriorityClassOnlyWhenPrewarmed(t *testing.T) {
	m := newTestManager()
	m.PrewarmedPriorityClass = "gardenio-prewarmed"
	spec := minimalSpec() // Prewarmed defaults false
	pod := m.buildPod(spec)

	if got := pod.Spec.PriorityClassName; got != "" {
		t.Errorf("PriorityClassName = %q, want empty for non-prewarmed pod", got)
	}
}

// With no override configured (legacy / chart create=false), even a prewarmed
// pod must stay at default priority — never reference a non-existent class.
func TestBuildPod_PriorityClassUnsetWhenNoOverride(t *testing.T) {
	m := newTestManager() // PrewarmedPriorityClass empty
	spec := minimalSpec()
	spec.Prewarmed = true
	pod := m.buildPod(spec)

	if got := pod.Spec.PriorityClassName; got != "" {
		t.Errorf("PriorityClassName = %q, want empty when no override set", got)
	}
}

// --- agent PriorityClass tiers (gd-2qjxTVMBkC) ---

// A crew pod gets the lowest-tier agent class; an interactive pod keeps the
// interactive class; a prewarmed pod keeps the prewarmed class.
func TestBuildPod_AgentPriorityClassTiers(t *testing.T) {
	m := newTestManager()
	m.PrewarmedPriorityClass = "gardenio-prewarmed"
	m.InteractivePriorityClass = "gardenio-interactive"
	m.AgentPriorityClass = "gardenio-agent"

	cases := []struct {
		name      string
		role      string
		prewarmed bool
		want      string
	}{
		{"crew", "crew", false, "gardenio-agent"},
		{"reviewer", "reviewer", false, "gardenio-agent"},
		{"thread", "thread", false, "gardenio-interactive"},
		{"gardener", "plan-gardener", false, "gardenio-interactive"},
		{"prewarmed crew", "crew", true, "gardenio-prewarmed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := minimalSpec()
			spec.Role = tc.role
			spec.Prewarmed = tc.prewarmed
			if got := m.buildPod(spec).Spec.PriorityClassName; got != tc.want {
				t.Errorf("PriorityClassName = %q, want %q", got, tc.want)
			}
		})
	}
}

// A tenant land's pod gets the lowest agent class whatever its role, so a
// tenant thread or gardener cannot preempt fleet crew (gd-NDJIOutLsv).
func TestBuildPod_LandFencedNeverInteractivePriority(t *testing.T) {
	m := newTestManager()
	m.InteractivePriorityClass = "gardenio-interactive"
	m.ScheduledPriorityClass = "gardenio-scheduled"
	m.AgentPriorityClass = "gardenio-agent"
	for _, role := range []string{"crew", "thread", "dm", "crew,thread", "gardener", "plan-gardener"} {
		t.Run(role, func(t *testing.T) {
			spec := minimalSpec()
			spec.Role = role
			spec.SpawnSource = "schedule"
			spec.LandFenced = true
			if got := m.buildPod(spec).Spec.PriorityClassName; got != "gardenio-agent" {
				t.Errorf("PriorityClassName = %q, want gardenio-agent", got)
			}
		})
	}
	m.AgentPriorityClass = ""
	spec := minimalSpec()
	spec.Role = "thread"
	spec.LandFenced = true
	if got := m.buildPod(spec).Spec.PriorityClassName; got != "" {
		t.Errorf("PriorityClassName with no agent class = %q, want none", got)
	}
}

// A gardener a schedule or fire-now started gets the non-preempting scheduled
// class; a thread agent, an operator-woken gardener and a gardener sent by a
// person keep the preempting interactive class (gd-Y2KaNE233g).
func TestBuildPod_ScheduledGardenerDoesNotPreempt(t *testing.T) {
	m := newTestManager()
	m.InteractivePriorityClass = "gardenio-interactive"
	m.ScheduledPriorityClass = "gardenio-scheduled"
	m.AgentPriorityClass = "gardenio-agent"

	cases := []struct {
		name, role, source, wake, want string
	}{
		{"scheduled plan-gardener", "plan-gardener", "schedule", "", "gardenio-scheduled"},
		{"fire-now gardener", "gardener", "fire-now", "", "gardenio-scheduled"},
		{"timer-woken scheduled gardener", "plan-gardener", "schedule", "timer", "gardenio-scheduled"},
		{"operator-woken gardener", "plan-gardener", "schedule", "manual", "gardenio-interactive"},
		{"mention-woken gardener", "plan-gardener", "fire-now", "mention", "gardenio-interactive"},
		{"thread gardener", "thread,plan-gardener", "schedule", "", "gardenio-interactive"},
		{"slack-sent gardener", "plan-gardener", "slack-thread", "", "gardenio-interactive"},
		{"thread agent", "thread", "slack-thread", "", "gardenio-interactive"},
		{"scheduled crew", "crew", "schedule", "", "gardenio-agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := minimalSpec()
			spec.Role = tc.role
			spec.SpawnSource = tc.source
			spec.WakeReason = tc.wake
			if got := m.buildPod(spec).Spec.PriorityClassName; got != tc.want {
				t.Errorf("PriorityClassName = %q, want %q", got, tc.want)
			}
		})
	}

	// Without the scheduled class (chart not rendering it), a scheduled
	// gardener stays on the interactive class.
	m.ScheduledPriorityClass = ""
	spec := minimalSpec()
	spec.Role = "plan-gardener"
	spec.SpawnSource = "schedule"
	if got := m.buildPod(spec).Spec.PriorityClassName; got != "gardenio-interactive" {
		t.Errorf("no scheduled class: PriorityClassName = %q, want gardenio-interactive", got)
	}
}

// A plan-gardener pod takes the batch capacity affinity (Spot preferred,
// on-demand allowed off the k3s pair) yet keeps the interactive PriorityClass, so it can still
// preempt crew when nodes are full (gd-Fyz3w0tOnf). A thread pod stays pinned
// on-demand (off k3s) and prefers it.
func TestBuildPod_GardenerOnSpotKeepsInteractivePriority(t *testing.T) {
	m := newTestManager()
	m.InteractivePriorityClass = "gardenio-interactive"
	m.AgentPriorityClass = "gardenio-agent"

	cases := []struct {
		role         string
		wantRequired []string
		wantPrefer   []string
	}{
		{"plan-gardener", nil, []string{"spot"}},
		{"gardener", nil, []string{"spot"}},
		{"thread", []string{"on-demand"}, []string{"on-demand"}},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			spec := minimalSpec()
			spec.Role = tc.role
			ApplyDefaults(&spec, DefaultPodDefaults("crew", tc.role))
			pod := m.buildPod(spec)

			if got := pod.Spec.PriorityClassName; got != "gardenio-interactive" {
				t.Errorf("PriorityClassName = %q, want gardenio-interactive", got)
			}
			aff := &PodDefaults{Affinity: pod.Spec.Affinity}
			if got := requiredCapacityType(aff); fmt.Sprint(got) != fmt.Sprint(tc.wantRequired) {
				t.Errorf("required capacity-type = %v, want %v", got, tc.wantRequired)
			}
			if got := preferredCapacityType(aff); fmt.Sprint(got) != fmt.Sprint(tc.wantPrefer) {
				t.Errorf("preferred capacity-type = %v, want %v", got, tc.wantPrefer)
			}
			if !requiredExcludesPerMR(aff) {
				t.Error("expected per-mr DoesNotExist to remain in required scheduling")
			}
		})
	}
}
