package podmanager

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OpenZiti per-pod overlay access (epic ka-0000000990).
//
// This file implements the gardenio-side seam of the network-fabric
// ziti-access-broker contract (docs/gardenio-ziti-access-broker.md). When
// AgentPodSpec.ZitiAccess is non-nil, the pod is given task-scoped OpenZiti
// access via three additions, all of which are no-ops when ZitiAccess is nil:
//
//   1. A projected ServiceAccount-token volume carrying the pod's
//      gardenio-ziti-broker-audience identity proof, plus a Memory-backed
//      emptyDir shared between the init container and the tunnel sidecar.
//   2. A "ziti-lease" PLAIN init container that POSTs /lease to the broker
//      using the pod's OWN token (so the broker attributes the lease to this
//      pod's ServiceAccount — the confused-deputy defense) and writes the
//      returned enrollment JWT into the shared emptyDir. It is fail-closed
//      unless ZitiAccessSpec.Optional is set.
//   3. A "ziti-tunnel" NATIVE sidecar (restartPolicy: Always) that enrolls
//      the JWT into a pod-local identity and runs ziti-edge-tunnel.
//
// The controller never holds the enrollment JWT; it only revokes the lease by
// task_id when the agent's bead reaches a terminal state (see main.go).
//
// Trust / privilege notes:
//   - The broker token is audience-pinned to gardenio-ziti-broker. A token
//     minted for any other audience (e.g. the IRSA sts.amazonaws.com token a
//     pod already carries) is rejected by the broker.
//   - The tunnel sidecar runs ziti-edge-tunnel in TUN mode, which needs the
//     NET_ADMIN capability and a /dev/net/tun hostPath volume. PSA "baseline"
//     rejects BOTH (baseline permits only the default capability set plus
//     NET_BIND_SERVICE, and bars hostPath outright), and "restricted" rejects
//     them too — so the fleet namespace must be PSA "privileged" to admit this
//     pod shape (checkZitiAdmittable enforces this before pod create). The
//     SDK/proxy enrollment shape (workload links the Ziti SDK and dials the
//     overlay directly, no tun device, no NET_ADMIN) is the
//     non-privileged-namespace-safe alternative and drops in behind the same
//     lease/JWT handoff.

// zitiEnabled reports whether per-pod Ziti overlay access is configured.
func zitiEnabled(spec AgentPodSpec) bool {
	return spec.ZitiAccess != nil
}

// checkZitiAdmittable verifies the target namespace permits the TUN-mode Ziti
// tunnel sidecar before pod creation. The sidecar adds the NET_ADMIN
// capability and mounts the /dev/net/tun hostPath volume, BOTH of which PSA
// "baseline" and "restricted" reject (baseline permits only the default
// capability set plus NET_BIND_SERVICE and bars hostPath outright). Only PSA
// "privileged" admits this pod shape.
//
// Without this preflight the pod is rejected at admission with a cryptic
// PodSecurity webhook denial ("violates PodSecurity ... must not include
// NET_ADMIN ... hostPath volumes"), and applyZitiAccess's fail-safe (which
// only covers a missing service-id / tunneler image) does NOT catch it — so
// an opted-in agent in a baseline namespace fails to start with no graceful
// degradation. This mirrors checkDindAdmittable for the DinD sidecar.
//
// Returns an actionable error when the namespace would reject admission. If
// the namespace is missing the enforce label, admission is permitted (PSA
// defaults to no enforcement) — we do not invent a denial the cluster would
// not produce.
func (m *K8sManager) checkZitiAdmittable(ctx context.Context, namespace string) error {
	ns, err := m.client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("ziti preflight: namespace %q not found", namespace)
		}
		// Don't block pod creation on a transient namespace-get failure. A
		// real PSA denial will surface from the Pod create call with the
		// cluster's own error message.
		m.logger.Warn("ziti preflight: namespace lookup failed, skipping PSA check",
			"namespace", namespace, "error", err)
		return nil
	}
	enforce := ns.Labels[PSAEnforceLabel]
	switch enforce {
	case "", "privileged":
		return nil
	case "baseline", "restricted":
		return fmt.Errorf(
			"ziti preflight: namespace %q has %s=%q which rejects the TUN-mode ziti-tunnel sidecar "+
				"(it adds NET_ADMIN and a /dev/net/tun hostPath, both barred by PSA baseline/restricted); "+
				"route the opted-in agent to a PSA=privileged namespace, or set ziti_optional=true on the "+
				"bead to degrade gracefully (pod starts without overlay access). "+
				"See docs/ziti-access-broker-integration.md#namespace-psa for the opt-in path",
			namespace, PSAEnforceLabel, enforce,
		)
	default:
		// Unknown profile value — treat as permissive but log so an operator
		// notices the typo.
		m.logger.Warn("ziti preflight: unknown PSA enforce value, allowing pod create",
			"namespace", namespace, "enforce", enforce)
		return nil
	}
}

// zitiTokenAudience returns the configured broker-token audience, defaulting
// to ZitiDefaultTokenAudience when unset.
func zitiTokenAudience(z *ZitiAccessSpec) string {
	if z != nil && z.TokenAudience != "" {
		return z.TokenAudience
	}
	return ZitiDefaultTokenAudience
}

// buildZitiVolumes returns the projected broker-token volume, the Memory-backed
// identity emptyDir, and (for the TUN-mode tunneler) the /dev/net/tun host
// device volume. Returns nil when Ziti access is not configured.
func (m *K8sManager) buildZitiVolumes(spec AgentPodSpec) []corev1.Volume {
	if !zitiEnabled(spec) {
		return nil
	}
	tokenMode := ZitiJWTFileMode // 0600 for the projected token file
	expiration := ZitiBrokerTokenExpirationSeconds
	tunDevice := corev1.HostPathCharDev
	return []corev1.Volume{
		{
			// Projected ServiceAccount token bound to the broker audience. The
			// kubelet rotates it; it is tmpfs-backed so the proof never lands
			// on node disk.
			Name: VolumeZitiBrokerToken,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					DefaultMode: &tokenMode,
					Sources: []corev1.VolumeProjection{{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Path:              ZitiBrokerTokenFileName,
							Audience:          zitiTokenAudience(spec.ZitiAccess),
							ExpirationSeconds: &expiration,
						},
					}},
				},
			},
		},
		{
			// Shared, Memory-backed scratch dir for the enrollment JWT and the
			// enrolled identity. Memory medium keeps both off node disk.
			Name: VolumeZitiIdentity,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMediumMemory,
				},
			},
		},
		{
			// Host TUN device for ziti-edge-tunnel's TUN-mode interception.
			Name: "ziti-tun-device",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/dev/net/tun",
					Type: &tunDevice,
				},
			},
		},
	}
}

// buildZitiLeaseInit returns the plain init container that mints the
// task-scoped lease and writes the enrollment JWT into the shared identity
// dir. Returns nil when Ziti access is not configured.
//
// It runs as the agent UID (the broker only needs the projected token, no
// elevated privilege) and uses the agent image by default — which carries
// curl + jq — so no extra image pull is needed. The lease is requested with
// the pod's OWN broker-audience token, so the broker attributes the grant to
// this pod's ServiceAccount.
func (m *K8sManager) buildZitiLeaseInit(spec AgentPodSpec) *corev1.Container {
	if !zitiEnabled(spec) {
		return nil
	}
	z := spec.ZitiAccess
	image := spec.Image // agent image: has curl + jq, already pulled

	optional := "false"
	if z.Optional {
		optional = "true"
	}
	bind := "false"
	if z.Bind {
		bind = "true"
	}

	env := []corev1.EnvVar{
		{Name: "ZITI_ACCESS_BROKER_URL", Value: z.BrokerURL},
		{Name: "ZITI_ACCESS_BROKER_TOKEN_FILE", Value: MountZitiBrokerToken + "/" + ZitiBrokerTokenFileName},
		{Name: "ZITI_ACCESS_SERVICE_ID", Value: z.ServiceID},
		{Name: "ZITI_ACCESS_TASK_ID", Value: spec.BeadID},
		{Name: "ZITI_ACCESS_TTL", Value: z.TTL},
		{Name: "ZITI_ACCESS_BIND", Value: bind},
		{Name: "ZITI_ACCESS_OPTIONAL", Value: optional},
		{Name: "ZITI_IDENTITY_DIR", Value: MountZitiIdentity},
		{Name: "ZITI_ENROLLMENT_JWT_FILE", Value: MountZitiIdentity + "/" + ZitiEnrollmentJWTFileName},
		{Name: "ZITI_LEASE_META_FILE", Value: MountZitiIdentity + "/" + ZitiLeaseMetaFileName},
	}

	return &corev1.Container{
		Name:            ZitiLeaseInitName,
		Image:           image,
		ImagePullPolicy: m.agentPullPolicy(),
		Command:         []string{"/bin/sh", "-c", zitiLeaseScript},
		Env:             env,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: VolumeZitiBrokerToken, MountPath: MountZitiBrokerToken, ReadOnly: true},
			{Name: VolumeZitiIdentity, MountPath: MountZitiIdentity},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}
}

// buildZitiTunnelSidecar returns the native sidecar (initContainer with
// restartPolicy: Always) that enrolls the JWT written by the lease init
// container and runs ziti-edge-tunnel. Returns nil when Ziti access is not
// configured.
func (m *K8sManager) buildZitiTunnelSidecar(spec AgentPodSpec) *corev1.Container {
	if !zitiEnabled(spec) {
		return nil
	}
	z := spec.ZitiAccess
	restartAlways := corev1.ContainerRestartPolicyAlways

	env := []corev1.EnvVar{
		{Name: "ZITI_IDENTITY_DIR", Value: MountZitiIdentity},
		{Name: "ZITI_ENROLLMENT_JWT_FILE", Value: MountZitiIdentity + "/" + ZitiEnrollmentJWTFileName},
		{Name: "ZITI_IDENTITY_FILE", Value: MountZitiIdentity + "/" + ZitiIdentityFileName},
		{Name: "ZITI_ACCESS_OPTIONAL", Value: boolStr(z.Optional)},
	}

	return &corev1.Container{
		Name:            ZitiTunnelContainerName,
		Image:           z.TunnelerImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c", zitiTunnelScript},
		Env:             env,
		RestartPolicy:   &restartAlways,
		SecurityContext: &corev1.SecurityContext{
			// TUN-mode interception needs NET_ADMIN and the host tun device.
			// This is strictly less privileged than the DinD sidecar (no
			// privileged=true, no root requirement beyond the device), but the
			// NET_ADMIN capability and the /dev/net/tun hostPath volume are
			// both rejected by PSA "baseline" AND "restricted" — so this pod
			// shape requires a PSA "privileged" namespace (enforced by
			// checkZitiAdmittable before pod create).
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"NET_ADMIN"},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: VolumeZitiIdentity, MountPath: MountZitiIdentity},
			{Name: "ziti-tun-device", MountPath: "/dev/net/tun"},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("25m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}
}

// buildZitiAgentEnv returns the env vars the AGENT container needs to discover
// the broker and its pod-local Ziti identity. Returns nil when Ziti access is
// not configured.
func (m *K8sManager) buildZitiAgentEnv(spec AgentPodSpec) []corev1.EnvVar {
	if !zitiEnabled(spec) {
		return nil
	}
	z := spec.ZitiAccess
	return []corev1.EnvVar{
		{Name: "ZITI_ACCESS_BROKER_URL", Value: z.BrokerURL},
		{Name: "ZITI_ACCESS_BROKER_TOKEN_FILE", Value: MountZitiBrokerToken + "/" + ZitiBrokerTokenFileName},
		{Name: "ZITI_ACCESS_SERVICE_ID", Value: z.ServiceID},
		{Name: "ZITI_IDENTITY_DIR", Value: MountZitiIdentity},
	}
}

// buildZitiAgentMounts returns the read-only volume mounts the AGENT container
// gets so it can read its broker token (e.g. to self-revoke) and observe the
// enrolled identity dir. Returns nil when Ziti access is not configured.
func (m *K8sManager) buildZitiAgentMounts(spec AgentPodSpec) []corev1.VolumeMount {
	if !zitiEnabled(spec) {
		return nil
	}
	return []corev1.VolumeMount{
		{Name: VolumeZitiBrokerToken, MountPath: MountZitiBrokerToken, ReadOnly: true},
		{Name: VolumeZitiIdentity, MountPath: MountZitiIdentity, ReadOnly: true},
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// zitiLeaseScript mints the task-scoped lease and writes the enrollment JWT.
// It distinguishes broker-unreachable (curl transport error) from a broker
// 4xx/5xx (HTTP code + error body), and NEVER echoes the token or the JWT — so
// the logs can debug "stuck enrollment vs broker mint" without leaking
// secrets. Fail-closed unless ZITI_ACCESS_OPTIONAL=true.
const zitiLeaseScript = `set -eu
log() { echo "[ziti-lease] $*"; }
fail() {
  echo "[ziti-lease] ERROR: $*" >&2
  if [ "${ZITI_ACCESS_OPTIONAL:-false}" = "true" ]; then
    log "optional=true; continuing without overlay access"
    exit 0
  fi
  exit 1
}

[ -n "${ZITI_ACCESS_BROKER_URL:-}" ] || fail "ZITI_ACCESS_BROKER_URL is empty"
[ -n "${ZITI_ACCESS_SERVICE_ID:-}" ] || fail "ZITI_ACCESS_SERVICE_ID is empty"
[ -s "$ZITI_ACCESS_BROKER_TOKEN_FILE" ] || fail "broker token file $ZITI_ACCESS_BROKER_TOKEN_FILE missing/empty"
command -v curl >/dev/null 2>&1 || fail "curl not found in lease-init image"
command -v jq >/dev/null 2>&1 || fail "jq not found in lease-init image"

TOKEN="$(cat "$ZITI_ACCESS_BROKER_TOKEN_FILE")"
body="$(jq -nc \
  --arg svc "$ZITI_ACCESS_SERVICE_ID" \
  --arg task "$ZITI_ACCESS_TASK_ID" \
  --arg ttl "$ZITI_ACCESS_TTL" \
  --argjson bind "${ZITI_ACCESS_BIND:-false}" \
  '{service_id:$svc, task_id:$task, bind:$bind} + (if $ttl=="" then {} else {ttl:$ttl} end)')"

log "requesting lease service=$ZITI_ACCESS_SERVICE_ID task=$ZITI_ACCESS_TASK_ID ttl=${ZITI_ACCESS_TTL:-<broker-default>} bind=${ZITI_ACCESS_BIND:-false} broker=$ZITI_ACCESS_BROKER_URL"
resp_body="$ZITI_IDENTITY_DIR/.lease-resp.json"
code="$(curl -sS -o "$resp_body" -w '%{http_code}' \
  -X POST "$ZITI_ACCESS_BROKER_URL/lease" \
  -H "authorization: Bearer $TOKEN" \
  -H "content-type: application/json" \
  --data "$body")" || fail "broker unreachable at $ZITI_ACCESS_BROKER_URL (curl transport error)"

log "broker http=$code"
if [ "$code" != "200" ]; then
  echo "[ziti-lease] broker error body: $(cat "$resp_body" 2>/dev/null)" >&2
  rm -f "$resp_body"
  fail "broker returned http $code (see error body above — 401=token/audience, 403=policy, 400=request, 502=ziti mint)"
fi

jwt="$(jq -r '.enrollment_jwt // empty' < "$resp_body")"
[ -n "$jwt" ] || { rm -f "$resp_body"; fail "broker 200 but response had no enrollment_jwt"; }

umask 077
printf '%s' "$jwt" > "$ZITI_ENROLLMENT_JWT_FILE"
jq 'del(.enrollment_jwt)' < "$resp_body" > "$ZITI_LEASE_META_FILE" 2>/dev/null || true
rm -f "$resp_body"
log "lease minted: $(jq -rc '{lease_id,identity_id,service_id,direction,expires_at,granted_ttl}' < "$ZITI_LEASE_META_FILE" 2>/dev/null)"
log "wrote enrollment jwt to $ZITI_ENROLLMENT_JWT_FILE (mode 0600); handing off to ziti-tunnel sidecar"
`

// zitiTunnelScript enrolls the JWT into a pod-local identity and runs the
// tunneler. It waits briefly for the lease-init container's output (so the
// optional-lease path, where no JWT was written, degrades to an idle sidecar
// rather than a crashloop that wedges the pod). The JWT is shredded after a
// successful enroll so only the enrolled identity remains.
const zitiTunnelScript = `set -u
log() { echo "[ziti-tunnel] $*"; }

# Wait up to 30s for the lease-init container to drop the JWT (or a
# pre-enrolled identity). Under normal ordering the init container has already
# completed before this native sidecar starts, so this returns immediately.
i=0
while [ ! -s "$ZITI_ENROLLMENT_JWT_FILE" ] && [ ! -s "$ZITI_IDENTITY_FILE" ] && [ "$i" -lt 30 ]; do
  sleep 1; i=$((i+1))
done

if [ ! -s "$ZITI_IDENTITY_FILE" ] && [ -s "$ZITI_ENROLLMENT_JWT_FILE" ]; then
  log "enrolling identity from JWT"
  if ! ziti-edge-tunnel enroll --jwt "$ZITI_ENROLLMENT_JWT_FILE" --identity "$ZITI_IDENTITY_FILE"; then
    log "ERROR: enroll failed (JWT may be expired/already-consumed, or controller unreachable)"
    exit 1
  fi
  # Shred the one-time JWT so only the enrolled identity (with its private key)
  # remains in the Memory-backed dir.
  rm -f "$ZITI_ENROLLMENT_JWT_FILE"
  log "enroll succeeded; shredded one-time JWT"
fi

if [ ! -s "$ZITI_IDENTITY_FILE" ]; then
  if [ "${ZITI_ACCESS_OPTIONAL:-false}" = "true" ]; then
    log "no identity present and optional=true; overlay access unavailable, idling"
    exec sleep infinity
  fi
  log "ERROR: no enrolled identity at $ZITI_IDENTITY_FILE and lease was required"
  exit 1
fi

log "starting ziti-edge-tunnel (identity-dir=$ZITI_IDENTITY_DIR)"
exec ziti-edge-tunnel run --identity-dir "$ZITI_IDENTITY_DIR"
`
