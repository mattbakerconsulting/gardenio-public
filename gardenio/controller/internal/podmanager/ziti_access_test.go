package podmanager

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// zitiSpec returns a minimal spec with Ziti access enabled.
func zitiSpec() AgentPodSpec {
	s := minimalSpec()
	s.ZitiAccess = &ZitiAccessSpec{
		BrokerURL:     "http://ziti-access-broker.ziti-access-broker.svc.cluster.local",
		ServiceID:     "svc-abc123",
		TTL:           "15m",
		TunnelerImage: "openziti/ziti-edge-tunnel:1.0.0",
	}
	return s
}

func findInit(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

func findVolume(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// TestZitiDisabled_NoAdditions verifies the feature is fully inert when
// ZitiAccess is nil — no volumes, no containers, no env. This is the
// can't-perturb-live-pods guarantee.
func TestZitiDisabled_NoAdditions(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec() // ZitiAccess == nil
	pod := m.buildPod(spec)

	if c := findInit(pod, ZitiLeaseInitName); c != nil {
		t.Error("ziti-lease init container present when ZitiAccess is nil")
	}
	if c := findInit(pod, ZitiTunnelContainerName); c != nil {
		t.Error("ziti-tunnel sidecar present when ZitiAccess is nil")
	}
	for _, vn := range []string{VolumeZitiBrokerToken, VolumeZitiIdentity, "ziti-tun-device"} {
		if v := findVolume(pod, vn); v != nil {
			t.Errorf("volume %q present when ZitiAccess is nil", vn)
		}
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if strings.HasPrefix(env.Name, "ZITI_") {
			t.Errorf("agent env %q set when ZitiAccess is nil", env.Name)
		}
	}
}

// TestZitiEnabled_Volumes verifies the projected broker token (audience-pinned,
// short TTL) and the Memory-backed identity emptyDir are added.
func TestZitiEnabled_Volumes(t *testing.T) {
	m := newTestManager()
	pod := m.buildPod(zitiSpec())

	tok := findVolume(pod, VolumeZitiBrokerToken)
	if tok == nil {
		t.Fatal("broker-token volume missing")
	}
	if tok.Projected == nil || len(tok.Projected.Sources) != 1 {
		t.Fatal("broker-token volume should be a projected volume with one source")
	}
	sat := tok.Projected.Sources[0].ServiceAccountToken
	if sat == nil {
		t.Fatal("broker-token source should be a ServiceAccountToken projection")
	}
	if sat.Audience != ZitiDefaultTokenAudience {
		t.Errorf("token audience = %q, want %q", sat.Audience, ZitiDefaultTokenAudience)
	}
	if sat.ExpirationSeconds == nil || *sat.ExpirationSeconds != ZitiBrokerTokenExpirationSeconds {
		t.Errorf("token expirationSeconds = %v, want %d", sat.ExpirationSeconds, ZitiBrokerTokenExpirationSeconds)
	}
	if sat.Path != ZitiBrokerTokenFileName {
		t.Errorf("token path = %q, want %q", sat.Path, ZitiBrokerTokenFileName)
	}

	id := findVolume(pod, VolumeZitiIdentity)
	if id == nil {
		t.Fatal("ziti-identity volume missing")
	}
	if id.EmptyDir == nil || id.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Error("ziti-identity volume should be a Memory-backed emptyDir (JWT must not hit disk)")
	}
}

// TestZitiEnabled_CustomAudience verifies a non-default audience flows through.
func TestZitiEnabled_CustomAudience(t *testing.T) {
	m := newTestManager()
	spec := zitiSpec()
	spec.ZitiAccess.TokenAudience = "custom-broker-aud"
	pod := m.buildPod(spec)

	tok := findVolume(pod, VolumeZitiBrokerToken)
	if tok == nil || tok.Projected == nil {
		t.Fatal("broker-token projected volume missing")
	}
	if got := tok.Projected.Sources[0].ServiceAccountToken.Audience; got != "custom-broker-aud" {
		t.Errorf("token audience = %q, want custom-broker-aud", got)
	}
}

// TestZitiEnabled_LeaseInitContainer verifies the lease init container shape:
// it is a PLAIN init (no restartPolicy), runs the agent image, mounts the
// broker token read-only + the identity dir read-write, and carries the
// request parameters as env.
func TestZitiEnabled_LeaseInitContainer(t *testing.T) {
	m := newTestManager()
	spec := zitiSpec()
	spec.ZitiAccess.Bind = true
	pod := m.buildPod(spec)

	c := findInit(pod, ZitiLeaseInitName)
	if c == nil {
		t.Fatal("ziti-lease init container missing")
	}
	if c.RestartPolicy != nil {
		t.Error("ziti-lease should be a PLAIN init (nil restartPolicy), not a native sidecar")
	}
	if c.Image != spec.Image {
		t.Errorf("ziti-lease image = %q, want agent image %q", c.Image, spec.Image)
	}

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["ZITI_ACCESS_SERVICE_ID"] != "svc-abc123" {
		t.Errorf("ZITI_ACCESS_SERVICE_ID = %q", env["ZITI_ACCESS_SERVICE_ID"])
	}
	if env["ZITI_ACCESS_TASK_ID"] != spec.BeadID {
		t.Errorf("ZITI_ACCESS_TASK_ID = %q, want bead id %q", env["ZITI_ACCESS_TASK_ID"], spec.BeadID)
	}
	if env["ZITI_ACCESS_BIND"] != "true" {
		t.Errorf("ZITI_ACCESS_BIND = %q, want true", env["ZITI_ACCESS_BIND"])
	}
	if env["ZITI_ACCESS_TTL"] != "15m" {
		t.Errorf("ZITI_ACCESS_TTL = %q, want 15m", env["ZITI_ACCESS_TTL"])
	}

	// Broker token mounted read-only; identity dir mounted read-write.
	var tokRO, idRW bool
	for _, vm := range c.VolumeMounts {
		if vm.Name == VolumeZitiBrokerToken && vm.MountPath == MountZitiBrokerToken && vm.ReadOnly {
			tokRO = true
		}
		if vm.Name == VolumeZitiIdentity && vm.MountPath == MountZitiIdentity && !vm.ReadOnly {
			idRW = true
		}
	}
	if !tokRO {
		t.Error("broker token should be mounted read-only in lease init")
	}
	if !idRW {
		t.Error("identity dir should be mounted read-write in lease init")
	}
}

// TestZitiEnabled_TunnelSidecar verifies the tunnel sidecar is a native sidecar
// (restartPolicy: Always) with the tunneler image, NET_ADMIN, and the tun
// device + identity-dir mounts.
func TestZitiEnabled_TunnelSidecar(t *testing.T) {
	m := newTestManager()
	spec := zitiSpec()
	pod := m.buildPod(spec)

	c := findInit(pod, ZitiTunnelContainerName)
	if c == nil {
		t.Fatal("ziti-tunnel sidecar missing")
	}
	if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("ziti-tunnel must be a native sidecar (restartPolicy: Always)")
	}
	if c.Image != spec.ZitiAccess.TunnelerImage {
		t.Errorf("ziti-tunnel image = %q, want %q", c.Image, spec.ZitiAccess.TunnelerImage)
	}
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
		t.Fatal("ziti-tunnel needs a security context with capabilities")
	}
	var hasNetAdmin bool
	for _, cap := range c.SecurityContext.Capabilities.Add {
		if cap == "NET_ADMIN" {
			hasNetAdmin = true
		}
	}
	if !hasNetAdmin {
		t.Error("ziti-tunnel needs NET_ADMIN for TUN-mode interception")
	}
	var hasTun, hasID bool
	for _, vm := range c.VolumeMounts {
		if vm.Name == "ziti-tun-device" {
			hasTun = true
		}
		if vm.Name == VolumeZitiIdentity {
			hasID = true
		}
	}
	if !hasTun {
		t.Error("ziti-tunnel needs the /dev/net/tun device mount")
	}
	if !hasID {
		t.Error("ziti-tunnel needs the shared identity-dir mount")
	}
}

// TestZitiEnabled_AgentEnvAndMounts verifies the agent container is told where
// to find the broker and its identity, with read-only mounts (it must not be
// able to clobber the enrolled identity).
func TestZitiEnabled_AgentEnvAndMounts(t *testing.T) {
	m := newTestManager()
	spec := zitiSpec()
	pod := m.buildPod(spec)
	agent := pod.Spec.Containers[0]

	env := map[string]string{}
	for _, e := range agent.Env {
		env[e.Name] = e.Value
	}
	if env["ZITI_ACCESS_BROKER_URL"] != spec.ZitiAccess.BrokerURL {
		t.Errorf("agent ZITI_ACCESS_BROKER_URL = %q", env["ZITI_ACCESS_BROKER_URL"])
	}
	if env["ZITI_ACCESS_SERVICE_ID"] != "svc-abc123" {
		t.Errorf("agent ZITI_ACCESS_SERVICE_ID = %q", env["ZITI_ACCESS_SERVICE_ID"])
	}
	if env["ZITI_IDENTITY_DIR"] != MountZitiIdentity {
		t.Errorf("agent ZITI_IDENTITY_DIR = %q", env["ZITI_IDENTITY_DIR"])
	}

	var tokRO, idRO bool
	for _, vm := range agent.VolumeMounts {
		if vm.Name == VolumeZitiBrokerToken && vm.ReadOnly {
			tokRO = true
		}
		if vm.Name == VolumeZitiIdentity && vm.ReadOnly {
			idRO = true
		}
	}
	if !tokRO {
		t.Error("agent broker-token mount should be read-only")
	}
	if !idRO {
		t.Error("agent identity mount should be read-only")
	}
}

// TestZitiLeaseScript_NoSecretEcho is a guard against the most damaging
// regression: the lease script must never echo the token or the JWT. The
// contract explicitly forbids logging the JWT.
func TestZitiLeaseScript_NoSecretEcho(t *testing.T) {
	// The script reads $TOKEN and writes $jwt to a file, but must never echo
	// either. Assert no "echo ... $TOKEN" / "echo ... $jwt" patterns.
	for _, bad := range []string{"echo \"$TOKEN", "echo $TOKEN", "echo \"$jwt", "echo $jwt", "log \"$TOKEN", "log \"$jwt"} {
		if strings.Contains(zitiLeaseScript, bad) {
			t.Errorf("lease script must never echo secrets, found %q", bad)
		}
	}
	// It SHOULD write the jwt to the enrollment file path.
	if !strings.Contains(zitiLeaseScript, "$ZITI_ENROLLMENT_JWT_FILE") {
		t.Error("lease script should write the JWT to $ZITI_ENROLLMENT_JWT_FILE")
	}
	// And it should strip the jwt from the metadata file.
	if !strings.Contains(zitiLeaseScript, "del(.enrollment_jwt)") {
		t.Error("lease script should strip enrollment_jwt from the persisted lease metadata")
	}
}

// TestZitiTunnelScript_ShredsJWT verifies the tunnel script removes the
// one-time JWT after a successful enroll so only the enrolled identity remains.
func TestZitiTunnelScript_ShredsJWT(t *testing.T) {
	if !strings.Contains(zitiTunnelScript, "rm -f \"$ZITI_ENROLLMENT_JWT_FILE\"") {
		t.Error("tunnel script should shred the one-time JWT after enroll")
	}
	if !strings.Contains(zitiTunnelScript, "ziti-edge-tunnel enroll") {
		t.Error("tunnel script should enroll via ziti-edge-tunnel")
	}
	if !strings.Contains(zitiTunnelScript, "ziti-edge-tunnel run --identity-dir") {
		t.Error("tunnel script should run the tunneler with --identity-dir")
	}
}

// ── Ziti PSA preflight (ka-0000002245) ─────────────────────────────────────

func TestCheckZitiAdmittable(t *testing.T) {
	cases := []struct {
		name       string
		nsLabel    string // pod-security.kubernetes.io/enforce; "" = label absent
		nsMissing  bool
		wantErr    bool
		wantSubstr string
	}{
		{name: "privileged_ok", nsLabel: "privileged"},
		{name: "no_label_ok", nsLabel: ""},
		{name: "baseline_rejected", nsLabel: "baseline", wantErr: true, wantSubstr: "baseline"},
		{name: "restricted_rejected", nsLabel: "restricted", wantErr: true, wantSubstr: "restricted"},
		{name: "unknown_value_allowed", nsLabel: "wat"},
		{name: "namespace_missing", nsMissing: true, wantErr: true, wantSubstr: "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset()
			if !tc.nsMissing {
				labels := map[string]string{}
				if tc.nsLabel != "" {
					labels[PSAEnforceLabel] = tc.nsLabel
				}
				_, err := client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: "gardenio-system", Labels: labels},
				}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("seed namespace: %v", err)
				}
			}
			m := New(client, slog.Default())
			err := m.checkZitiAdmittable(context.Background(), "gardenio-system")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
					t.Errorf("error %q does not contain %q", err, tc.wantSubstr)
				}
				if tc.nsLabel == "baseline" || tc.nsLabel == "restricted" {
					if !strings.Contains(err.Error(), "NET_ADMIN") || !strings.Contains(err.Error(), "/dev/net/tun") {
						t.Errorf("PSA-rejection error should name NET_ADMIN + /dev/net/tun; got: %v", err)
					}
				}
			} else if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

// TestCreateAgentPod_ZitiInBaselineNamespace: an opted-in (non-optional) pod in
// a PSA=baseline namespace must fail preflight and NOT be created.
func TestCreateAgentPod_ZitiInBaselineNamespace(t *testing.T) {
	client := fake.NewClientset()
	_, err := client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gardenio-system",
			Labels: map[string]string{PSAEnforceLabel: "baseline"},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	m := New(client, slog.Default())
	spec := zitiSpec()
	spec.Namespace = "gardenio-system"
	err = m.CreateAgentPod(context.Background(), spec)
	if err == nil {
		t.Fatal("expected CreateAgentPod to fail preflight in baseline namespace")
	}
	if !strings.Contains(err.Error(), "ziti preflight") {
		t.Errorf("error should be ziti preflight; got: %v", err)
	}
	if _, err := client.CoreV1().Pods("gardenio-system").Get(context.Background(), spec.PodName(), metav1.GetOptions{}); err == nil {
		t.Error("pod should not have been created when preflight fails")
	}
}

// TestCreateAgentPod_ZitiOptionalDegradesInBaseline: an opted-in pod with
// ziti_optional=true in a baseline namespace must be created WITHOUT any Ziti
// wiring (graceful degradation) rather than rejected.
func TestCreateAgentPod_ZitiOptionalDegradesInBaseline(t *testing.T) {
	client := fake.NewClientset()
	_, err := client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gardenio-system",
			Labels: map[string]string{PSAEnforceLabel: "baseline"},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	m := New(client, slog.Default())
	spec := zitiSpec()
	spec.Namespace = "gardenio-system"
	spec.ZitiAccess.Optional = true
	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("optional Ziti pod should admit in baseline namespace, got: %v", err)
	}
	pod, err := client.CoreV1().Pods("gardenio-system").Get(context.Background(), spec.PodName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod should have been created: %v", err)
	}
	// No Ziti wiring should remain on the degraded pod.
	if c := findInit(pod, ZitiTunnelContainerName); c != nil {
		t.Error("degraded pod should not have the ziti-tunnel sidecar")
	}
	if c := findInit(pod, ZitiLeaseInitName); c != nil {
		t.Error("degraded pod should not have the ziti-lease init container")
	}
	if v := findVolume(pod, "ziti-tun-device"); v != nil {
		t.Error("degraded pod should not have the ziti-tun-device hostPath volume")
	}
}

// TestCreateAgentPod_ZitiInPrivilegedNamespace: an opted-in pod in a
// PSA=privileged namespace admits with the full Ziti wiring intact.
func TestCreateAgentPod_ZitiInPrivilegedNamespace(t *testing.T) {
	client := fake.NewClientset()
	_, err := client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gardenio-gvisor",
			Labels: map[string]string{PSAEnforceLabel: "privileged"},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	m := New(client, slog.Default())
	spec := zitiSpec()
	spec.Namespace = "gardenio-gvisor"
	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("Ziti pod should admit in privileged namespace, got: %v", err)
	}
	pod, err := client.CoreV1().Pods("gardenio-gvisor").Get(context.Background(), spec.PodName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod should have been created: %v", err)
	}
	if c := findInit(pod, ZitiTunnelContainerName); c == nil {
		t.Error("privileged-namespace pod should retain the ziti-tunnel sidecar")
	}
}
