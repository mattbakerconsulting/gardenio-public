// Package podmanager handles K8s pod CRUD for Gardenio agents.
// It translates beads lifecycle decisions into pod create/delete operations.
// The pod manager never makes lifecycle decisions — it executes them.
package podmanager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/tracing"
)

// tracer instruments podmanager CRUD. No-op until tracing.Init runs.
var tracer = tracing.Tracer("gardenio/controller/podmanager")

const (
	// Label keys for agent pods.
	LabelApp     = "app.kubernetes.io/name"
	LabelProject = "gardenio.io/project"
	LabelRole    = "gardenio.io/role"
	LabelAgent   = "gardenio.io/agent"
	LabelMode    = "gardenio.io/mode"

	// LabelGardenioApp is the agent-pod marker the chart's NetworkPolicies
	// select on (templates/networkpolicies/networkpolicies.yaml: the
	// agent-egress / agent-ingress podSelectors and the "Agent pods" peer
	// rules in the beads / nats / litellm policies). Every spawned agent pod
	// MUST carry it, or default-deny-egress blocks the pod from
	// DNS/beads/coopmux/slack and init-clone fails with
	// "Could not resolve host: github.com" (ka-0000002093). Distinct from
	// LabelApp (app.kubernetes.io/name=gardenio), which every gardenio workload
	// shares — this one scopes specifically to agent pods.
	LabelGardenioApp = "gardenio.io/app"

	// LabelDark marks an agent pod as belonging to the monorepo-dark universe
	// (AgentPodSpec.Dark, derived from the bead's dark markers via
	// beadsapi.IsDark). Only dark pods carry it; it is the podSelector the
	// chart's dark-agent-egress NetworkPolicy keys on to deny egress to
	// acme.atlassian.net — the network leg of the dark-Jira write-token
	// fence (ka-0000005242). Value is always "true" when present.
	LabelDark = "gardenio.io/dark"

	// LabelThawPlaceholder marks the placeholder Pod B created by
	// FreezeThawManager.Thaw (and kept as the permanent live pod after a
	// successful thaw). Its name (…-<agent>-thaw-b) never matches the
	// reconciler's desired pod name (…-<agent>), so the reconciler maps it back
	// to the canonical name and exempts it from drift recreation — otherwise the
	// desired-vs-actual sweep deletes Pod B (mid-restore → thaw rc=128, and again
	// in steady state) and cold-starts a duplicate (ka-0000007549 / ka-0000004146).
	LabelThawPlaceholder = "gardenio.io/thaw-placeholder"

	// ThawPlaceholderSuffix is appended to the canonical agent pod name to form
	// the placeholder Pod B name (snap.PodName + "-thaw-b" in Thaw). Stripping it
	// recovers the canonical name the reconciler keys desired state by.
	ThawPlaceholderSuffix = "-thaw-b"

	// LabelWarmPlaceholder marks a warm runsc placeholder pod pre-created by the
	// poolmanager warm-B pool (sub-second-resume-integration.md C3, ka-0000004263).
	// Its value is the pool state: "ready" while the pod is Ready and unclaimed,
	// "claimed" once a thaw has grafted (or will graft) its checkpoint onto it.
	// Warm placeholders deliberately DO NOT carry the gardenio.io/agent label, so
	// the reconciler's desired-vs-actual sweep skips them (reconciler.go only
	// considers pods with LabelAgent) — they have no agent bead and must not be
	// reaped as orphans. The poolmanager owns their full lifecycle.
	LabelWarmPlaceholder = "gardenio.io/warm-placeholder"

	// WarmPlaceholderReady / WarmPlaceholderClaimed are the two LabelWarmPlaceholder
	// values. A pool reconcile counts "ready" pods toward MinSize and reaps "claimed"
	// leftovers; ClaimWarmPlaceholderForThaw takes a Ready pod straight to a live
	// agent pod in one Update, so it never parks one in "claimed".
	WarmPlaceholderReady   = "ready"
	WarmPlaceholderClaimed = "claimed"

	// LabelWarmTopology buckets a warm placeholder by the topology key of the
	// snapshot it was built from (SnapshotTopologyKey, ka-0000002219). The wake
	// path only claims a placeholder whose topology label matches the thawing
	// agent's snapshot, so a checkpoint is never grafted onto a mount-shape- or
	// repo-incompatible pod. Placeholders built blind (no snapshot known) omit
	// this label and serve only the legacy bare-/work claim path.
	LabelWarmTopology = "gardenio.io/warm-topology"

	// AnnotationWarmImage records the agent container image a warm placeholder
	// was minted from, stamped at create time (ka-0000006974). The pool's
	// upgrade-staleness reaper (ka-0000000890) compares this against the
	// controller's currently-configured agent image (PlaceholderPool.image): a
	// Ready, unclaimed placeholder whose stamped image differs from the live
	// image is running the OLD binary and must be cycled so a fresh-image
	// placeholder replaces it. Stamping the value at create time makes the
	// comparison cheap (a label/annotation read, no container-spec walk) and
	// robust to the pod's container image being mutated by a future promotion.
	// A placeholder with no stamp (minted before this field existed) is treated
	// as stale on the first reconcile after upgrade and cycled once.
	AnnotationWarmImage = "gardenio.io/warm-image"

	// AnnotationBeadID is the canonical bead ID for this pod. When set,
	// the status reporter uses it instead of constructing an ID from labels.
	// This is required for pods whose bead ID doesn't follow the
	// {mode}-{project}-{role}-{agent} pattern.
	AnnotationBeadID = "gardenio.io/bead-id"

	// AnnotationPrewarmed marks a pod as prewarmed. The status reporter
	// skips overwriting agent_state=prewarmed with "working" for these pods.
	AnnotationPrewarmed = "gardenio.io/prewarmed"

	// AnnotationCanonicalPod records the canonical agent pod name a thaw
	// placeholder serves, for the reconciler's desired-vs-actual remap
	// (sub-second-resume-integration.md C2, ka-0000005266). The create-path
	// placeholder Pod B is named "<canonical>-thaw-b", so the reconciler
	// recovers its canonical name by trimming ThawPlaceholderSuffix. A WARM-B
	// placeholder grafted onto by the wake path keeps its pool name
	// ("warm-b-<node>-…"), which the suffix-trim cannot map back — so the wake
	// path stamps this annotation with the canonical name at promotion time and
	// the reconciler prefers it over the suffix-trim. Without it a promoted
	// warm-B pod is orphan-reaped and a duplicate cold-starts (the
	// ka-0000007549 failure class).
	AnnotationCanonicalPod = "gardenio.io/canonical-pod"

	// AnnotationPVCLastActive is the RFC3339 timestamp of the most recent
	// agent pod creation that referenced this PVC. The PVC GC reconciler
	// uses this to delete PVCs whose last associated pod ran longer than
	// the configured TTL ago. Refreshed on every ensurePVC call so the
	// timer resets when an agent is re-spawned.
	AnnotationPVCLastActive = "gardenio.io/pvc-last-active"

	// AnnotationKarpenterDoNotDisrupt opts a pod out of Karpenter node
	// consolidation/disruption. Karpenter will not voluntarily drain a node
	// while it hosts a pod carrying this annotation set to "true". All gardenio
	// infra pods (postgres, nats, beads, slack-bridge, coopmux, controller)
	// carry it; agent pods historically did not, which made them the default
	// victims of "underutilized" consolidation evictions (investigation
	// ka-0000006340). We stamp it on EVERY agent pod — no role is disruptable —
	// so no agent is ever cut short by node consolidation (operator directive,
	// Matt Baker: "nothing is disruptable").
	AnnotationKarpenterDoNotDisrupt = "karpenter.sh/do-not-disrupt"

	// AnnotationSpawnedBy, AnnotationSpawnApprovalMode and
	// AnnotationSpawnApprovedBy mirror the identically-named agent bead fields
	// stamped by kbeads on replay of a privileged-role spawn escalation
	// (gd-h7fgTgcuyn, design doc gd-sGW5Y1B8sg §5): the approving escalation
	// ID, whether approval was human or auto (policy), and who/what approved
	// it. Set only when the bead carries the corresponding field — an
	// ordinary (non-escalated) agent spawn has none of the three.
	AnnotationSpawnedBy         = "gardenio.io/spawned-by"
	AnnotationSpawnApprovalMode = "gardenio.io/spawn-approval-mode"
	AnnotationSpawnApprovedBy   = "gardenio.io/spawn-approved-by"

	// LabelAppValue is the app label value for all gardenio pods.
	LabelAppValue = "gardenio"

	// Default resource values.
	//
	// Memory request was 16Gi but observed peak usage on running crew pods is
	// well under 1Gi (typical 300–850Mi), so the request was massively
	// overprovisioned. The limit stays at 16Gi so bursty workloads (npm
	// install, playwright, large test suites) are still allowed to spike.
	// See docs/research/crew-pod-resource-trim.md (ka-0000002903).
	//
	// Requests lowered again 2 CPU / 8Gi -> 500m / 2Gi (gd-TEP4ygX3dq): live
	// crews use well under 1 CPU, and a 2-CPU request alone fills a 2-vCPU
	// k3s node, so each such pod needs a node to itself and holds it above
	// the autoscaler's scale-down threshold. 2Gi stays above the VPA stamp floor (DefaultVPAMemoryFloor)
	// and the crew peak above. Limits unchanged. A project or role bead's
	// cpu_request / memory_request still overrides these (the gardenio project
	// sets 500m / 1Gi).
	DefaultCPURequest    = "500m"
	DefaultCPULimit      = "4"
	DefaultMemoryRequest = "2Gi"
	DefaultMemoryLimit   = "16Gi"

	// Volume names.
	VolumeWorkspace        = "workspace"
	VolumeTmp              = "tmp"
	VolumeBeadsConfig      = "beads-config"
	VolumeClaudeCreds      = "claude-creds"
	VolumeNatsToken        = "nats-token"
	VolumeObjectStoreCreds = "object-store-creds"

	// VolumeCoverageData is the shared coverage PVC mounted into agent pods
	// when runtime coverage instrumentation is enabled (GARDENIO_AGENT_COVERAGE).
	// It is the SAME ReadWriteMany claim the long-running daemons mount (helm
	// templates/coverage.yaml + the daemon deployments) — {fullname}-coverage —
	// so an e2e coverage drain can tar every pod's counters
	// out of one PVC. The claim name is not derivable at runtime (it embeds the
	// Helm release name), so the controller receives it via
	// GARDENIO_AGENT_COVERAGE_CLAIM. See CoverageConfig / buildCoverageEnv.
	VolumeCoverageData = "coverage-data"

	// VolumeWork / WarmPlaceholderWorkPath back the warm runsc placeholder's
	// /work emptyDir (warm-B pool, ka-0000004263). The checkpoint restore
	// repoints into /work (freezethaw_node.go preserves the warm /work mount), so
	// a placeholder onto which a thaw will graft must expose that destination.
	VolumeWork              = "work"
	WarmPlaceholderWorkPath = "/work"

	// VolumeSshKeyPrefix is prepended to AgentPodSpec.SshKeys map keys to
	// form a unique K8s Volume name per key (e.g. "ssh-key-perf-bastion").
	VolumeSshKeyPrefix = "ssh-key-"

	// VolumeGitCreds is the name of the projected volume that materializes
	// git credentials as files for the init-clone container. Using a file
	// mount (instead of GIT_USERNAME/GIT_TOKEN env vars) prevents creds from
	// leaking via process listings (`ps eww`), `kubectl describe pod`, and
	// admin log dumps. See W6 secrets-hardening (ka-0000002787).
	VolumeGitCreds = "git-creds"

	// VolumeZitiBrokerToken is the projected ServiceAccount-token volume that
	// carries the pod's gardenio-ziti-broker-audience identity proof for the
	// ziti-lease init container's POST /lease call (epic ka-0000000990).
	VolumeZitiBrokerToken = "ziti-broker-token"

	// VolumeZitiIdentity is a Memory-backed emptyDir shared between the
	// ziti-lease init container (which writes the enrollment JWT) and the
	// ziti-tunnel sidecar (which enrolls it into a pod-local identity file).
	// Memory-backed so the JWT and the resulting identity/private-key never
	// touch node disk.
	VolumeZitiIdentity = "ziti-identity"

	// Mount paths.
	MountWorkspace   = "/home/agent/bot"
	MountTmp         = "/tmp"
	MountBeadsConfig = "/etc/agent-pod"
	MountClaudeCreds = "/tmp/claude-credentials"
	MountSshKeys     = "/etc/ssh-keys"
	MountGitCreds    = "/etc/git-creds"

	// MountCoverageData is where the shared coverage PVC is mounted in agent
	// pods (matches the daemon deployments' mountPath and the chart's
	// global.coverage.mountPath default). Per-pod counters land under
	// <MountCoverageData>/agents/<pod-name>/... (see buildCoverageEnv). Kept in
	// sync with global.coverage.mountPath in values.yaml — if an operator
	// overrides that, they must also set GARDENIO_AGENT_COVERAGE_MOUNT so the two
	// halves agree.
	MountCoverageData = "/data/coverage"

	// MountZitiBrokerToken is the directory where the projected
	// gardenio-ziti-broker-audience ServiceAccount token is mounted (read-only).
	// The ziti-lease init container reads $ZITI_ACCESS_BROKER_TOKEN_FILE from
	// here to authenticate its POST /lease call. Projected SA-token volumes are
	// tmpfs-backed, so the token never touches node disk.
	MountZitiBrokerToken = "/var/run/secrets/ziti-broker"

	// ZitiBrokerTokenFileName is the basename of the projected broker token
	// file under MountZitiBrokerToken. Combined: /var/run/secrets/ziti-broker/token.
	ZitiBrokerTokenFileName = "token"

	// MountZitiIdentity is the directory shared by the ziti-lease init
	// container and the ziti-tunnel sidecar. The init container writes the
	// enrollment JWT here; the sidecar enrolls it into a pod-local identity
	// file and runs the tunneler with --identity-dir pointed at it.
	MountZitiIdentity = "/var/run/ziti"

	// ZitiEnrollmentJWTFileName / ZitiIdentityFileName are the basenames of the
	// JWT the init container writes and the enrolled identity the sidecar
	// produces, both under MountZitiIdentity. The JWT is shredded after a
	// successful enroll.
	ZitiEnrollmentJWTFileName = "enroll.jwt"
	ZitiIdentityFileName      = "identity.json"

	// ZitiLeaseMetaFileName is the basename of the (JWT-stripped) lease
	// metadata file the init container writes for in-pod debugging — lease_id,
	// service_id, direction, expires_at. Never contains the enrollment JWT.
	ZitiLeaseMetaFileName = "lease.json"

	// MountNatsToken is the directory where the NATS agent token Secret is
	// mounted into agent pods (W5, ka-0000006585). K8s Secret volumes are
	// tmpfs-backed by default, so the token never touches node disk and is
	// not visible via /proc/self/environ or `kubectl describe pod`. Consumers
	// (coop, gb hook-relay) read the token from $COOP_NATS_TOKEN_FILE.
	MountNatsToken = "/etc/nats-token"

	// NatsTokenFileName is the basename of the projected NATS token file
	// under MountNatsToken. Combined: /etc/nats-token/token.
	NatsTokenFileName = "token"

	// NatsTokenFileMode is the file mode for the projected NATS token file.
	// 0400 (read-only by owner) matches the SSH key pattern and prevents
	// any group/world access to the secret bytes.
	NatsTokenFileMode int32 = 0o400

	// MountObjectStoreCreds is the directory where the object-store (MinIO)
	// credentials Secret is mounted into agent pods, endpoint mode only
	// (gd-W8OwRoHLLn). Mirrors the NATS token pattern: a tmpfs-backed Secret
	// volume rather than a raw env value, so the access/secret key never
	// appear via /proc/self/environ or `kubectl describe pod`. coop reads
	// the two files directly (COOP_S3_ACCESS_KEY_ID_FILE /
	// COOP_S3_SECRET_ACCESS_KEY_FILE) rather than through the default AWS
	// credential chain's env-var provider.
	MountObjectStoreCreds = "/etc/object-store-creds"

	// ObjectStoreAccessKeyFileName / ObjectStoreSecretKeyFileName are the
	// basenames of the projected credential files under
	// MountObjectStoreCreds. They are projected from the Secret's
	// "accesskey"/"secretkey" keys (the same keys controller, beads-viewer,
	// claudemore, and litellm already read via secretKeyRef) so a single
	// existing Secret serves every consumer.
	ObjectStoreAccessKeyFileName = "access_key_id"
	ObjectStoreSecretKeyFileName = "secret_access_key"

	// ObjectStoreCredFileMode is the file mode for the projected object-store
	// credential files. 0400 matches the NATS token / SSH key pattern.
	ObjectStoreCredFileMode int32 = 0o400

	// SshKeyFileMode is the file mode for projected SSH private keys.
	// 0400 (read-only by owner) is required by ssh(1), which refuses to use
	// a private key whose mode permits group or world access.
	SshKeyFileMode int32 = 0o400

	// GitCredFileMode is the file mode for projected git credential files.
	// 0400 (read-only by owner) matches the SSH-key pattern so the secret
	// values are not readable by other users/processes that happen to share
	// the FSGroup. The init-clone container runs as AgentUID so 0400 is
	// sufficient — the file is owned by the projected-volume default UID
	// (FSGroup applies recursively, see pod-level SecurityContext).
	GitCredFileMode int32 = 0o400

	// VolumeCodexAuth projects the derived gardenio-codex-auth*-session Secret
	// (placeholder refresh token + current access/id tokens) into a codex-harness
	// pod. MountCodexAuth is a directory so the pod's start step can copy
	// auth.json from it to a writable $CODEX_HOME/auth.json (ADR 0004 §C);
	// CodexAuthFileName is the Secret key and the file name.
	VolumeCodexAuth   = "codex-auth"
	MountCodexAuth    = "/var/run/gardenio/codex"
	CodexAuthFileName = "auth.json"

	// VolumeGeminiAuth projects a Gemini Google-account login Secret (key
	// GeminiOAuthFileName = oauth_creds.json, the file `gemini` writes after a
	// browser login) into a gemini-harness pod. MountGeminiAuth is a read-only
	// directory: `gb setup gemini` copies the file to a writable
	// ~/.gemini/oauth_creds.json because the CLI rewrites it when it refreshes
	// the access token. Unlike Codex there is no keeper and no placeholder: the
	// CLI refreshes with the account's long-lived refresh token itself.
	VolumeGeminiAuth    = "gemini-auth"
	MountGeminiAuth     = "/var/run/gardenio/gemini"
	GeminiOAuthFileName = "oauth_creds.json"

	// CodexAuthFileMode is the projected auth.json mode. 0400 matches the other
	// credential files; the pod-level FSGroup adds group read, so the agent
	// user can still read it.
	CodexAuthFileMode int32 = 0o400

	// VolumeSlackViewSession projects the Slack test user's Playwright
	// storageState (key SlackViewSessionFileName) read-only at
	// MountSlackViewSession. The Secret volume is Optional, so a Secret that has
	// not been provisioned yet leaves the directory empty rather than blocking
	// pod creation; the agent entrypoint exports SLACK_VIEW_STORAGE_STATE only
	// when the file exists.
	VolumeSlackViewSession   = "slack-view-session"
	MountSlackViewSession    = "/var/run/gardenio/slack-view"
	SlackViewSessionFileName = "storage-state.json"

	// SlackViewSessionFileMode is the projected storageState mode (cookies, so
	// 0400; the pod-level FSGroup adds group read for the agent user).
	SlackViewSessionFileMode int32 = 0o400

	// VolumeMachineClient projects key MachineClientSecretKey of the Secret
	// AgentPodSpec.MachineClientSecret as the single file
	// MountMachineClient/MachineClientSecretFileName, mode 0400, for kd and gb
	// --as-machine (escalation routing Task 15, C5). Never env.
	VolumeMachineClient               = "machine-client"
	MountMachineClient                = "/run/gardenio/machine-client"
	MachineClientSecretFileName       = "secret"
	MachineClientSecretKey            = "clientSecret"
	MachineClientFileMode       int32 = 0o400

	// EnvMachineClientID and EnvMachineClientSecretFile tell kd and gb
	// --as-machine which client to act as and where its secret is (the
	// names oidclogin.MachineClientIDEnv and MachineClientSecretFileEnv).
	EnvMachineClientID         = "GARDENIO_MACHINE_CLIENT_ID"
	EnvMachineClientSecretFile = "GARDENIO_MACHINE_CLIENT_SECRET_FILE"

	// VolumeMachineClientCA projects only key ca.crt of
	// AgentPodSpec.MachineClientCASecret (the keycloak-runtime TLS Secret)
	// at MountMachineClientCA, optional, so --as-machine can trust the
	// in-cluster Keycloak Service. EnvOIDCDiscoveryURL and EnvOIDCCAFile are
	// oidclogin.DiscoveryURLEnv and CAFileEnv.
	VolumeMachineClientCA   = "machine-client-ca"
	MountMachineClientCA    = "/run/gardenio/machine-client-ca"
	MachineClientCAFileName = "ca.crt"
	MachineClientCAFileMode = int32(0o444)
	EnvOIDCDiscoveryURL     = "GARDENIO_OIDC_DISCOVERY_URL"
	EnvOIDCCAFile           = "GARDENIO_OIDC_CA_FILE"

	// Session persistence: state dir on the workspace PVC.
	MountStateDir = "/home/agent/bot/.state"

	// Claude state: subPath mount so .claude/ is backed by the workspace PVC.
	MountClaudeState   = "/home/agent/.claude"
	SubPathClaudeState = ".state/claude"

	// Codex state: the codex harness's CODEX_HOME. Same workspace-volume
	// subPath scheme as Claude state, mounted INSTEAD of ~/.claude for
	// harness=codex (see HarnessStateMount).
	MountCodexState   = "/home/agent/.codex"
	SubPathCodexState = ".state/codex"

	// Gemini state: the gemini harness's ~/.gemini (settings.json, GEMINI.md and
	// the chats/ session records coop uploads). Mounted INSTEAD of ~/.claude for
	// harness=gemini (see HarnessStateMount).
	MountGeminiState   = "/home/agent/.gemini"
	SubPathGeminiState = ".state/gemini"

	// gVisor thaw-epoch rendezvous (rehydration-design §3.1, ka-0000004017).
	// The thaw-epoch marker/ack files must live on a path that BOTH the
	// controller (writing the marker / reading acks over NodeExecutor, on the
	// node's host FS) and the agent's in-pod gb+coop watchers (reading the
	// marker / writing acks, through the pod mount) can reach as the same
	// underlying bytes. A per-agent hostPath gives us exactly that: the marker
	// the controller drops via `chroot /host` lands in the same inode the pod
	// sees through its mount.
	//
	// VolumeThawState is the pod volume name; MountThawState is the in-pod
	// mount point exported to the watchers as COOP_THAW_STATE_DIR;
	// ThawStateHostBase is the per-node host directory the hostPath sources
	// from (joined with the agent name to isolate agents). Only wired onto
	// runsc-pinned pods (buildVolumes/buildEnvVars gate on RuntimeClassRunsc),
	// so non-gVisor pods never carry the hostPath.
	//
	// MountThawState is CANONICAL on /run (NOT /var/run) so it survives a
	// freeze->restore intact (bug ka-0000003421). At restore time the barrier
	// script normalizes every checkpointed mount DESTINATION /var/run/*->/run/*
	// (freezethaw_node.go, "normalize ... mount DESTINATIONS /var/run/* -> /run/*"),
	// so the thaw-state hostPath always re-lands at /run/gardenio-thaw in the
	// restored sandbox. Pre-freeze that didn't matter — Pod A's rootfs had
	// /var/run -> /run as a symlink, so the env COOP_THAW_STATE_DIR=/var/run/...
	// resolved to the same inode. But the RESTORED sandbox materializes /var/run
	// as a REAL directory (no symlink), so a /var/run-rooted env pointed the
	// in-pod gb+coop watchers at an empty /var/run/gardenio-thaw while the marker
	// the controller wrote sat at the real mount /run/gardenio-thaw — the watchers
	// never acked and the C3 rehydrate gate fail-closed. Pinning the mount + env
	// on /run makes normalize(MountThawState)==MountThawState (an identity), so
	// the path the watchers poll and the path the mount lands on coincide in BOTH
	// the live and restored sandbox, with no symlink dependency.
	VolumeThawState   = "thaw-state"
	MountThawState    = "/run/gardenio-thaw"
	ThawStateHostBase = "/var/lib/gardenio/thaw"
	// EnvThawStateDir is the env var the gb + coop thaw watchers read to find
	// the marker (must match coop config COOP_THAW_STATE_DIR and the gb
	// supervisor's os.Getenv in agent_start.go).
	EnvThawStateDir = "COOP_THAW_STATE_DIR"

	// Container constants.
	ContainerName = "agent"
	AgentUID      = int64(1000)
	AgentGID      = int64(1000)

	// Init container constants.
	InitCloneName = "init-clone"
	// ThawStatePermsInitName is the root init container that chowns the
	// thaw-state hostPath to the agent UID/GID so the in-pod gb + coop
	// watchers can write their armed sentinels + acks at boot (bug
	// ka-0000001058). Only added to runsc pods, which run in the
	// PSA-privileged gVisor namespace where a root init container is
	// admissible. Pod-level FSGroup does NOT apply to hostPath volumes, so an
	// explicit chown is the minimal k8s-native way to make the dir writable.
	ThawStatePermsInitName = "thaw-state-perms"
	// InitCloneImage is the default image used by the init-clone init container.
	// It must have git pre-installed so the container can be admitted under PSA
	// "restricted" without an `apk add` at runtime. The container is run as
	// AgentUID/AgentGID via SecurityContext (overrides image USER) and uses
	// /bin/sh as its entrypoint (overrides image ENTRYPOINT). HOME is set to
	// /tmp on the container so `git config --global` writes to a writable
	// path. Operators can override this via the INIT_CLONE_IMAGE env var on
	// the controller (Helm: agents.initCloneImage).
	//
	// Pinned to a digest to avoid silent base-image drift; bump when upstream
	// alpine/git ships a CVE fix.
	InitCloneImage = "alpine/git:v2.45.2"

	// defaultAgentStartupProbeFailureThreshold is the compiled default coop
	// StartupProbe failureThreshold. Budget = threshold × 5s periodSeconds, so
	// 180 = 15min of cold-start headroom. Operators override via
	// agents.startupProbeFailureThreshold (env
	// AGENT_STARTUP_PROBE_FAILURE_THRESHOLD); see StartupProbeFailureThresholdOverride
	// and ka-0000000649.
	defaultAgentStartupProbeFailureThreshold = 180

	// DefaultInitCloneJitterMaxSeconds is the compiled default clone-storm
	// stagger window applied when AGENT_INIT_CLONE_JITTER_MAX_SECONDS is unset.
	// The init-clone container sleeps a random 0..N seconds before a COLD
	// network clone so a batch of pods scheduled together does not all saturate
	// NAT egress at once. It runs BEFORE coop (an init container), so it never
	// consumes the agent StartupProbe budget, and only on the cold-clone path
	// (warm-PVC pods that merely fetch skip it). 20s meaningfully de-syncs the
	// t=0 thundering herd while adding at most a one-time 20s to an already
	// ~200s cold start. Operators widen it on a contended cluster, or set 0 to
	// disable, via agents.initCloneJitterMaxSeconds (ka-0000000995).
	DefaultInitCloneJitterMaxSeconds = 20

	// Coop port constants.
	CoopDefaultPort       = 8080
	CoopDefaultHealthPort = 9090
	// CoopDefaultMetricsPort is the dedicated, unauthenticated Prometheus
	// /metrics port served by coop (session usage counters + tool-call/LLM
	// latency histograms). Matches the prometheus.io/port scrape annotation.
	CoopDefaultMetricsPort = 9093

	// Channel MCP server port (gardenio-channel).
	ChannelPort int32 = 8789

	// Docker-in-Docker sidecar constants.
	DindContainerName = "dind"
	DindImage         = "docker:dind"
	DindPort          = 2375
	VolumeDindStorage = "dind-storage"
	MountDindStorage  = "/var/lib/docker"

	// PostgreSQL sidecar constants.
	PostgresContainerName = "postgres"
	PostgresImage         = "postgres:16-alpine"
	PostgresPort          = 5432
	VolumePostgresData    = "postgres-data"
	MountPostgresData     = "/var/lib/postgresql/data"
	PostgresUser          = "postgres"
	PostgresDB            = "postgres"

	// OpenZiti access constants (epic ka-0000000990).
	//
	// ZitiLeaseInitName is the plain init container that mints the task-scoped
	// lease (POST /lease) and writes the enrollment JWT into the shared
	// identity emptyDir. ZitiTunnelContainerName is the native sidecar
	// (restartPolicy: Always) that enrolls that JWT and runs ziti-edge-tunnel.
	ZitiLeaseInitName       = "ziti-lease"
	ZitiTunnelContainerName = "ziti-tunnel"

	// ZitiDefaultTokenAudience is the projected ServiceAccount-token audience
	// the broker requires. It binds the proof to this specific broker; a token
	// minted for any other audience (e.g. the IRSA sts.amazonaws.com token) is
	// rejected, which is the confused-deputy defense.
	ZitiDefaultTokenAudience = "gardenio-ziti-broker"

	// ZitiBrokerTokenExpirationSeconds is the projected token lifetime. Short
	// (10m) because the token is only needed at pod start for the single
	// POST /lease; the kubelet rotates it for any later use.
	ZitiBrokerTokenExpirationSeconds int64 = 600

	// ZitiJWTFileMode / ZitiIdentityFileMode are 0600 — the enrollment JWT and
	// the enrolled identity (which embeds the identity private key) must not be
	// group/world readable.
	ZitiJWTFileMode      int32 = 0o600
	ZitiIdentityFileMode int32 = 0o600
)

// SecretEnvSource maps a K8s Secret key to a pod environment variable.
type SecretEnvSource struct {
	EnvName    string // env var name in the pod
	SecretName string // K8s Secret name
	SecretKey  string // key within the Secret
	// Optional marks the SecretKeyRef optional so a missing key does not block
	// pod creation. Used for keys that may be provisioned incrementally (e.g.
	// the per-env Grafana token bundle, task ka-0000002337).
	Optional bool
}

// SshKeySource references a single SSH private key stored in a K8s Secret.
// Used by AgentPodSpec.SshKeys to materialize keys at /etc/ssh-keys/<name>
// with mode 0400 so agents can ssh / scp without out-of-band setup. See
// advice ka-0000001600 for the perf-bastion use case (K6_PERF_BASTION_PRIVATE_KEY).
type SshKeySource struct {
	SecretName string // K8s Secret name
	SecretKey  string // key within the Secret holding the private key
}

// RepoRef describes a reference repo to clone alongside the primary workspace repo.
type RepoRef struct {
	URL    string // repository URL
	Branch string // branch to checkout (default: "main")
	Name   string // directory name under workspace/repos/
}

// AgentPodSpec describes the desired pod for an agent.
type AgentPodSpec struct {
	Project   string
	Mode      string
	Role      string // functional role from role bead (e.g., "devops", "qa")
	AgentName string
	BeadID    string // canonical bead ID (written to gardenio.io/bead-id annotation)
	TaskID    string // optional pre-assigned task bead ID (set as BOAT_TASK_ID env var)
	// TriggerBeadID is the bead whose event caused this agent to spawn
	// (e.g. JIRA mention, GitLab webhook, schedule fire). Set as
	// BOAT_TRIGGER_BEAD_ID env var so the agent harness can send
	// X-Trigger-Bead on its first kd claim, enabling auto-link M3.
	TriggerBeadID string
	Image         string
	Namespace     string
	Env           map[string]string

	// Resources sets compute requests/limits. If nil, defaults are used.
	Resources *corev1.ResourceRequirements

	// SecretEnv injects environment variables from K8s Secrets.
	// Used for git credentials and other secret env vars.
	SecretEnv []SecretEnvSource

	// ConfigMapName is the name of a ConfigMap to mount at MountBeadsConfig.
	// Contains agent configuration (role, project, daemon connection, etc.).
	ConfigMapName string

	// ServiceAccountName for the pod. Empty uses the namespace default.
	ServiceAccountName string

	// RuntimeClassName pins the pod to a Kubernetes RuntimeClass (e.g. "runsc"
	// for gVisor sandboxing). Empty uses the cluster default runtime. Resolved
	// from role beads (runtime_class field) before pod creation.
	RuntimeClassName string

	// NoThawState keeps a runsc pod off the freeze/thaw machinery: no
	// thaw-state hostPath, no root init container to chown it, no thaw env.
	// Set by the controller's land fence for tenant land pods (Lands Track B
	// §5.2), whose namespace enforces PodSecurity restricted and which never
	// get a hostPath.
	NoThawState bool

	// LandFenced marks a tenant land's pod (Lands Track B §5.3): it gets none
	// of the manager's shared mounts (the coverage PVC lives in the release
	// namespace and is shared by every agent), and always the lowest
	// AgentPriorityClass, never the interactive one, whatever its role, so it
	// cannot preempt a fleet pod (gd-NDJIOutLsv). Set by the controller's land
	// fence, which also strips the spec to the tenant allowlist.
	LandFenced bool

	// LandSecretCopies are the release-namespace Secrets a tenant land's pod
	// references: its land's own Claude credential, lands-<land>.
	// CreateAgentPod copies each one's Keys into Namespace before its
	// admission gate (CopyLandSecret). LandCoopmuxToken is that the pod reads
	// its land's coopmux token from LandCoopmuxTokenSecret, which
	// CreateAgentPod mints there first (K8sManager.MintLandCoopmuxToken), and
	// LandNatsToken that it reads its land's NATS password from
	// LandNatsTokenSecret (MintLandNatsToken, gd-sS7bSXOdGs). Set only by the
	// controller's land fence, and only acted on for a LandFenced spec
	// (gd-ApgYe740ps).
	LandSecretCopies []LandSecretCopy
	LandCoopmuxToken bool
	LandNatsToken    bool

	// DNSSearches are extra DNS search domains appended to the pod's
	// resolv.conf (pod DNSConfig.Searches, DNSPolicy ClusterFirst). Used to let
	// a runsc agent routed into the gVisor namespace resolve the controller's
	// bare cross-namespace service names (gardenio-coopmux, gardenio-nats) which
	// only resolve in the release namespace's search domain (bug
	// ka-0000001058). Empty leaves the pod on the default cluster DNS config.
	DNSSearches []string

	// NodeSelector constrains pod scheduling.
	NodeSelector map[string]string

	// Tolerations for the pod.
	Tolerations []corev1.Toleration

	// Affinity rules for pod scheduling (e.g. capacity-type split, per-mr
	// exclusion — see capacityAffinity).
	Affinity *corev1.Affinity

	// WorkspaceStorage configures a PVC for persistent workspace.
	// If nil, an EmptyDir is used.
	WorkspaceStorage *WorkspaceStorageSpec

	// CredentialsSecret is the K8s Secret name containing Claude OAuth credentials.
	// The "credentials.json" key is mounted at ~/.claude/.credentials.json.
	// Used for Claude Max/Corp accounts (no API key needed).
	CredentialsSecret string

	// DaemonTokenSecret is the K8s Secret name containing BEADS_DAEMON_TOKEN.
	// The "token" key is injected as the BEADS_DAEMON_TOKEN env var.
	DaemonTokenSecret string

	// NatsTokenSecret is the K8s Secret name containing the NATS agent token.
	// The "token" key is projected as a file at MountNatsToken/token (mode
	// 0400, tmpfs-backed) and the file path is exposed to the agent via the
	// COOP_NATS_TOKEN_FILE env var. This replaces the previous COOP_NATS_TOKEN
	// env-var injection so the token is not visible via /proc/self/environ or
	// `kubectl describe pod` (W5, ka-0000006585).
	NatsTokenSecret string

	// ObjectStoreCredentialsSecret is the K8s Secret name containing the
	// object-store (MinIO) access/secret key pair — endpoint mode only
	// (gd-W8OwRoHLLn). The "accesskey"/"secretkey" keys are projected as
	// files at MountObjectStoreCreds/{ObjectStoreAccessKeyFileName,
	// ObjectStoreSecretKeyFileName} (mode 0400, tmpfs-backed), and the file
	// paths are exposed via COOP_S3_ACCESS_KEY_ID_FILE /
	// COOP_S3_SECRET_ACCESS_KEY_FILE. AWS/IRSA mode leaves this empty — the
	// default AWS credential chain (IRSA web-identity token) is unchanged.
	ObjectStoreCredentialsSecret string

	// GitURL is the upstream repository URL (e.g., "https://github.com/...").
	// When set, an init container clones from this URL into the workspace.
	GitURL string

	// GitDefaultBranch is the branch to checkout after cloning (default: "main").
	GitDefaultBranch string

	// GitCredentialsSecret is the K8s Secret name containing git credentials.
	// The "username" and "token" keys are projected into the init-clone
	// container as files at /etc/git-creds/github/{username,token} (W6,
	// ka-0000002787) and consumed by a git credential-store helper. Not
	// injected as env vars — env-var secrets leak via /proc/<pid>/environ
	// and admin tooling.
	GitCredentialsSecret string

	// GitlabTokenSecret is the K8s Secret name containing a GitLab access
	// token. The "token" key is projected into the init-clone container as
	// a file at /etc/git-creds/gitlab/token (W6, ka-0000002787) and used
	// to write a git-credentials store line. Not injected as env var.
	GitlabTokenSecret string

	// ReferenceRepos lists additional repos to clone alongside the primary.
	ReferenceRepos []RepoRef

	// ImagePullSecrets lists K8s Secret names for pulling private container images.
	ImagePullSecrets []string

	// Prewarmed marks this pod as a prewarmed agent. The status reporter
	// will not overwrite agent_state=prewarmed with "working".
	Prewarmed bool

	// Docker enables a Docker-in-Docker (DinD) sidecar container so the
	// agent can run docker/docker-compose commands. Uses the K8s 1.29+
	// native sidecar pattern (initContainer with restartPolicy: Always).
	Docker bool

	// Postgres enables a PostgreSQL sidecar container for integration tests.
	// Uses the K8s 1.29+ native sidecar pattern with postgres:16-alpine,
	// emptyDir storage, and default credentials (PGHOST=127.0.0.1, PGUSER=postgres).
	Postgres bool

	// NeedsK8sAPI opts this pod into mounting a ServiceAccount token at
	// /var/run/secrets/kubernetes.io/serviceaccount. Default is false so agents
	// cannot reach the K8s API from inside the pod. Flip to true only for beads
	// or projects that explicitly require cluster API access (e.g. controllers,
	// fleet-admin agents).
	NeedsK8sAPI bool

	// Dark marks this pod as belonging to the monorepo-dark universe (derived
	// from the agent bead's dark markers via beadsapi.IsDark, plumbed through
	// metadata[beadsapi.MetaDarkPod]). When true the pod-spec builder fences the
	// privileged Jira write token (JIRA_API_TOKEN) out of the pod and the pod
	// carries LabelDark so the dark-agent-egress NetworkPolicy denies its egress
	// to acme.atlassian.net — the pod + network legs of the dark-Jira
	// write-token fence (ka-0000005242). Default false: a normal agent pod is
	// unaffected.
	Dark bool

	// SessionResume controls whether the agent's entrypoint passes --resume to
	// coop on pod restart. nil = default (resume when WorkspaceStorage != nil);
	// &true / &false = explicit opt-in/out. Resolved from project/role beads
	// and per-agent metadata in the controller before pod creation.
	SessionResume *bool

	// OperatedBy is the human-operator id on whose behalf this agent acts.
	// Injected as the BEADS_OPERATED_BY env var so kd/gb stamp the
	// X-Operated-By header on outbound kbeads calls. Format is the opaque id
	// used by the kbeads server-side resolver: "slack:U…", "oidc:<sub>",
	// or "" for scheduler-driven agents. See human-attribution rollout (R9).
	OperatedBy string

	// CodexAuthSecret names the derived -session Secret (key auth.json) holding a
	// Codex ChatGPT-subscription login with a PLACEHOLDER refresh token. Set by
	// the controller for provider=openai accounts; mounted read-only at
	// MountCodexAuth. The real refresh token lives only in the controller-side
	// Secret and never reaches an agent pod. Empty = no Codex login mounted.
	CodexAuthSecret string

	// GeminiOAuthSecret names the Secret (key GeminiOAuthFileName) holding a
	// Gemini Google-account login (oauth_creds.json). Set by the controller for
	// a provider=gemini account whose credential is secret:<name>#oauth_creds.json;
	// mounted read-only at MountGeminiAuth. Empty = API-key auth.
	GeminiOAuthSecret string

	// SlackViewSessionSecret names the Secret (key SlackViewSessionFileName)
	// holding the Slack test user's Playwright storageState, mounted read-only
	// and OPTIONAL at MountSlackViewSession. Deliberately absent from
	// referencedSecretNames: a missing Secret must not defer pod creation.
	SlackViewSessionSecret string

	// MachineClientSecret names the Secret holding a machine client's
	// clientSecret, mounted at MountMachineClient. Required: a missing
	// Secret defers pod creation like the other credential mounts.
	MachineClientSecret string

	// MachineClientCASecret names the Secret whose ca.crt is mounted,
	// optional, at MountMachineClientCA; set only with MachineClientSecret.
	MachineClientCASecret string

	// SshKeys materializes SSH private keys at /etc/ssh-keys/<name> with
	// file mode 0400. Map key is the filename under /etc/ssh-keys/ (e.g.
	// "perf-bastion" -> /etc/ssh-keys/perf-bastion). Each entry projects
	// a single Secret key as the named file via a per-key Secret volume.
	SshKeys map[string]SshKeySource

	// UserBeadID is the gd- id of the user bead that owns this agent (the
	// human's identity bead, not the agent bead itself). Injected as the
	// BEADS_USER_BEAD_ID env var alongside BEADS_OPERATED_BY so downstream
	// audit/attribution paths can resolve the human owner from the pod's
	// environment without re-querying the resolver. Empty when no user bead
	// is associated (scheduler/gardener-spawned agents).
	UserBeadID string

	// SpawnedBy, SpawnApprovalMode and SpawnApprovedBy mirror the agent bead
	// fields of the same name (minus the gardenio.io/ prefix) onto the pod as
	// annotations (AnnotationSpawnedBy / AnnotationSpawnApprovalMode /
	// AnnotationSpawnApprovedBy) — provenance for a privileged-role spawn that
	// went through the escalation pathway (gd-h7fgTgcuyn, design doc
	// gd-sGW5Y1B8sg §5). All three are empty for an ordinary, non-escalated
	// spawn.
	SpawnedBy         string
	SpawnApprovalMode string
	SpawnApprovedBy   string

	// SpawnSource and WakeReason mirror the agent bead's spawn_source and
	// wake_reason fields. They pick a gardener's PriorityClass
	// (scheduledGardenerRun, gd-Y2KaNE233g).
	SpawnSource string
	WakeReason  string

	// ResolvedAccount records the account name applyAccountConfig actually baked
	// into this pod's credentials (after spread selection / role-pin / Bedrock /
	// exhausted-account fallback). Not rendered into the pod object — it is an
	// out-parameter the spawn path reads to optimistically bump the per-account
	// live-pod count (ka-0000004861) so a burst of spawns within one sync window
	// keeps spreading. Empty when no account was applied.
	ResolvedAccount string

	// AccountLayer records WHICH layer of applyAccountConfig produced this pod's
	// account (Phase 0 instrumentation, ka-0000002142, epic ka-0000007694). It is
	// a bounded enum string (podspec.go AccountLayer* constants:
	// role-pin / spread / opus-fallback / project-cache / default-account /
	// redirect-changed / escape-hatch-blocked / whitelist-blocked /
	// no-credential-warn / no-credential-benign / unknown-account). Not rendered
	// into the pod object — an out-parameter the spawn path reads to increment
	// gardenio_agent_account_layer_total, the evidence that the project-cache and
	// DefaultAccount/redirect layers essentially never win on prod (§7 Phase 0 of
	// docs/account-routing.md). No behavior change: recording only.
	AccountLayer string

	// RequestedHarness is the agent-level harness override from bead metadata
	// ("harness"), captured at spec construction so applyHarness (which runs
	// under applyCommonConfig's cache lock) can fold it in as the top layer of
	// agent > role > project > default.
	RequestedHarness string

	// Harness is the resolved agent CLI for this pod ("claude", "codex" or "gemini"; empty
	// is treated as "claude"). It selects which CLI state directory is mounted
	// from the workspace volume (see HarnessStateMount).
	Harness string

	// HarnessError, when non-empty, is why the harness could not be resolved or
	// is incompatible with the pod's account (unknown value, provider mismatch).
	// The spawn path refuses the spawn with this reason rather than launching a
	// pod that cannot authenticate.
	HarnessError string

	// ResolvedModel records the model applyModelOverride baked into this pod's
	// ANTHROPIC_MODEL/CLAUDE_MODEL via a role- or project-level override (epic
	// ka-0000005488). Empty when no override applied (the pod uses the account's
	// default model). The spawn path threads this through the coopmux
	// session-register payload so a credential reload does not re-derive
	// CLAUDE_MODEL from the account default and revert the override.
	ResolvedModel string

	// RequestedModel is the agent-level model override from bead metadata
	// ("model"), captured at the TOP of spec construction — before account spread
	// runs — so the fable-aware landing gate (selectAccount) knows the group
	// the agent will actually use and can exclude accounts whose window for that
	// group is exhausted (ka-0000003806). It records only the highest-precedence
	// agent-level request; role/project overrides are resolved separately from the
	// caches at gate time. Empty when the spawn carries no per-agent model. Not
	// rendered into the pod object — an out-parameter for the landing gate only.
	RequestedModel string

	// ModelDowngrade, when non-empty, is the model the fable-aware landing gate
	// fell back to because NO account had capacity in the spec's requested model
	// group (ka-0000003806, decision ka-0000003299 = opus fallback). It is set by
	// applyAccountConfig when the requested group's pool is fully gated but an
	// opus-capable account exists, and is applied LAST by the spec builders
	// (applyModelDowngrade) so the pod actually runs the fallback model on the
	// opus-fresh account — authoritatively overriding the role/agent model that
	// asked for the exhausted group. Empty on the common path (requested group had
	// capacity, so no downgrade).
	ModelDowngrade string

	// SpawnBlocked, when true, signals that applyAccountConfig refused to resolve
	// a usable credential because EVERY account in the project's pool is
	// simultaneously capacity-exhausted (ka-0000002560). It is an out-parameter
	// (not rendered into the pod object): the spawn path reads it and declines to
	// CreateAgentPod, letting the reconciler re-attempt on the next sync cycle
	// once an account's window resets — instead of baking credentials onto a
	// known-dead key. This is a TRANSIENT refusal: the bead is NOT failed and no
	// escalation is filed (the pool recovers on its own when a window resets).
	SpawnBlocked bool

	// SpawnBlockReason is the human-readable explanation for SpawnBlocked, logged
	// at the spawn site (e.g. "project pool fully exhausted, blocking new spawns
	// until <earliest_reset_at>"). Empty when SpawnBlocked is false.
	SpawnBlockReason string

	// ZitiAccess, when non-nil, opts this pod into per-pod OpenZiti overlay
	// access via the network-fabric ziti-access-broker: a projected
	// broker-audience token, a ziti-lease init container that mints a
	// task-scoped lease, and a ziti-tunnel native sidecar that enrolls the
	// returned identity and runs the tunneler. nil = feature OFF (default).
	// See ZitiAccessSpec and epic ka-0000000990.
	ZitiAccess *ZitiAccessSpec

	// ExecutionTarget selects the runtime backend for this agent. Empty or
	// ExecutionTargetKubernetes (the default) means this spec is realized as a
	// Kubernetes pod by the K8sManager. ExecutionTargetLambdaMicroVM routes the
	// agent to the AWS Lambda MicroVM backend instead (Network Fabric handoff
	// nfd!15, epic ka-0000005010); such a spec must NOT be handed to
	// CreateAgentPod. Resolved from project/role/agent config
	// (global < project < role < agent). The spawn doors select exactly one
	// backend per agent so a single spawn event never creates both a pod and a
	// MicroVM.
	ExecutionTarget string
}

// Execution target values for AgentPodSpec.ExecutionTarget. These name the
// runtime backend that realizes an agent bead. "kubernetes" is the historical
// default (the pod path); "lambda_microvm" is the AWS Lambda MicroVM backend
// (epic ka-0000005010); "probe" is the one-off scripted pre-check backend
// (probe-pods plan ka-0000000544) — a bare `/bin/sh -lc $SCRIPT` pod with NO
// coop lifecycle and NO Claude OAuth, driven by `gb probe run`.
const (
	ExecutionTargetKubernetes    = "kubernetes"
	ExecutionTargetLambdaMicroVM = "lambda_microvm"
	ExecutionTargetProbe         = "probe"
	// ExecutionTargetLaptop is an agent running on its owner's laptop under a
	// runner (`gb runner join`, local-agents spec §5-6). The controller realizes
	// nothing for it: no pod, no MicroVM, no reaping. gb owns its lifecycle and
	// closes the bead when the session exits. It is deliberately not "local":
	// the escalation bridge stamps execution_target=local on the fleet pods it
	// spawns for a local-fallback escalation (bridge/escalations.go), and those
	// must still get a pod.
	ExecutionTargetLaptop = "laptop"
)

// IsKubernetesTarget reports whether target selects the standard Kubernetes
// agent-pod backend. Empty is treated as Kubernetes so existing agents (which
// never set the field) keep following the pod path unchanged.
//
// NOTE: probe (ExecutionTargetProbe) also realizes as a K8s pod, but a
// *different* one — a bare script pod built by BuildProbePod, not the coop
// agent pod CreateAgentPod builds. It is therefore NOT a Kubernetes target
// here: the controller spawn door must route it away from CreateAgentPod, and
// `gb probe run` owns its lifecycle instead. Keeping it out of this predicate
// preserves the invariant that IsKubernetesTarget==true ⇒ "safe to hand to
// CreateAgentPod".
func IsKubernetesTarget(target string) bool {
	return target == "" || target == ExecutionTargetKubernetes
}

// IsProbeTarget reports whether target selects the probe (scripted one-off pod)
// backend.
func IsProbeTarget(target string) bool {
	return target == ExecutionTargetProbe
}

// IsLaptopTarget reports whether target is a laptop agent the controller must
// leave alone (ExecutionTargetLaptop).
func IsLaptopTarget(target string) bool {
	return target == ExecutionTargetLaptop
}

// WorkspaceStorageSpec configures a PVC-backed workspace volume.
type WorkspaceStorageSpec struct {
	// ClaimName is the PVC name. If empty, derived from pod name.
	ClaimName string

	// Size is the requested storage (e.g., "10Gi").
	Size string

	// StorageClassName is the storage class (e.g., "gp3").
	StorageClassName string
}

// ZitiAccessSpec configures per-pod OpenZiti overlay access via the
// network-fabric ziti-access-broker (Phase-2 gardenio seam, epic
// ka-0000000990). nil on AgentPodSpec means the feature is OFF — the default
// — and no broker token, init container, or tunnel sidecar is added. When
// set, the pod gets a task-scoped Ziti identity minted at startup and revoked
// when the agent's bead reaches a terminal state.
//
// Trust model (the confused-deputy defense, see docs/gardenio-ziti-access-broker.md):
// the lease is minted IN-POD by the ziti-lease init container using the pod's
// OWN projected ServiceAccount token (audience gardenio-ziti-broker). The
// broker authorizes the proof's subject — system:serviceaccount:<ns>:<sa> —
// against the fabric-owned policy ConfigMap, so the granted overlay access is
// attributed to THIS pod's service account, not the controller's. The
// controller never holds the enrollment JWT; it only revokes the lease by
// task_id on pod exit (it is a configured revoker subject).
type ZitiAccessSpec struct {
	// BrokerURL is the in-cluster base URL of the ziti-access-broker
	// (e.g. http://ziti-access-broker.ziti-access-broker.svc.cluster.local).
	BrokerURL string

	// TokenAudience is the projected ServiceAccount token audience the broker
	// requires (default "gardenio-ziti-broker"). The proof is rejected if the
	// audience does not match, which is what binds the token to THIS broker.
	TokenAudience string

	// ServiceID is the OpenZiti service the pod requests a lease for. This is
	// the resolved Ziti service-id (not the policy's stable service_name); the
	// broker's allow-list matches on the resolved id. Required.
	ServiceID string

	// Bind requests a Bind (server) lease instead of the default Dial (client)
	// lease. Most agent consumers Dial a private service; Bind is for pods that
	// expose a service onto the overlay.
	Bind bool

	// TTL is the requested lease duration (e.g. "15m"). The broker clamps it to
	// the per-rule and global maxima. Empty lets the broker apply its default.
	TTL string

	// TunnelerImage is the ziti-edge-tunnel image the tunnel sidecar runs.
	// Required when running the tunneler shape (the default consumer path).
	TunnelerImage string

	// Optional, when true, makes a failed lease mint non-fatal: the init
	// container logs and exits 0 so the agent still starts (without overlay
	// access) instead of the pod failing closed. Default false (fail-closed).
	Optional bool
}

// SanitizeRole replaces commas in a role string with dashes so the value
// is safe for use in Kubernetes resource names and label values, which do
// not allow commas.  e.g. "crew,thread" → "crew-thread".
func SanitizeRole(role string) string {
	return strings.ReplaceAll(role, ",", "-")
}

// PodName returns the canonical pod name: {mode}-{project}-{role}-{name}.
// Commas in multi-role values are replaced with dashes for K8s compatibility.
//
// Agent names generated by the bridge/CLI include a "{project}-" prefix for
// beads-level disambiguation. That prefix is stripped here to avoid repeating
// the project in the pod name (e.g. "gardenio-rypb" → "crew-gardenio-crew-rypb"
// instead of "crew-gardenio-crew-gardenio-rypb").
func (s *AgentPodSpec) PodName() string {
	name := s.AgentName
	if s.Project != "" {
		name = strings.TrimPrefix(name, s.Project+"-")
	}
	return fmt.Sprintf("%s-%s-%s-%s", s.Mode, s.Project, SanitizeRole(s.Role), name)
}

// Labels returns the standard label set for this agent pod.
// Commas in multi-role values are replaced with dashes for K8s compatibility.
// Label values are truncated to 63 characters (K8s limit); the full agent
// name is always available via the AnnotationBeadID annotation.
func (s *AgentPodSpec) Labels() map[string]string {
	labels := map[string]string{
		LabelApp:         LabelAppValue,
		LabelGardenioApp: LabelAppValue,
		LabelProject:     s.Project,
		LabelMode:        s.Mode,
		LabelRole:        SanitizeRole(s.Role),
		LabelAgent:       truncateLabel(s.AgentName),
	}
	// Dark pods carry the dark marker so the dark-agent-egress NetworkPolicy can
	// select them and deny egress to acme.atlassian.net (ka-0000005242).
	// Only stamped when true — a normal pod is unlabeled and untouched.
	if s.Dark {
		labels[LabelDark] = "true"
	}
	return labels
}

// agentTopologySpreadConstraints returns the soft node-spread policy applied
// to every agent pod (Spot-hardening blast-radius proof, ka-0000005885).
//
// maxSkew=1 over kubernetes.io/hostname asks the scheduler to place each new
// agent on a node hosting the fewest existing agents, so agents fan out across
// nodes instead of packing onto one. whenUnsatisfiable=ScheduleAnyway keeps it
// a preference, never a hard gate — an agent still schedules (co-located) when
// spreading is impossible, so this can never wedge a spawn behind
// FailedScheduling.
//
// The label selector matches on LabelAgent (Exists), which is present on real
// agent pods only: warm placeholders (LabelWarmPlaceholder) and probe pods
// (LabelProbe) deliberately omit it, so they neither carry the constraint nor
// count toward an agent's skew.
func agentTopologySpreadConstraints() []corev1.TopologySpreadConstraint {
	return []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       corev1.LabelHostname,
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      LabelAgent,
						Operator: metav1.LabelSelectorOpExists,
					},
				},
			},
		},
	}
}

// truncateLabel truncates a string to the K8s label value limit of 63 characters.
// After truncation it strips trailing non-alphanumeric characters so the value
// satisfies the K8s label regex ([A-Za-z0-9][-A-Za-z0-9_.]*[A-Za-z0-9]).
func truncateLabel(v string) string {
	if len(v) <= 63 {
		return v
	}
	v = v[:63]
	return strings.TrimRight(v, "-_.")
}

// Manager creates, deletes, and lists agent pods in K8s.
type Manager interface {
	CreateAgentPod(ctx context.Context, spec AgentPodSpec) error
	DeleteAgentPod(ctx context.Context, name, namespace string) error
	DeleteAllAgentPods(ctx context.Context, namespace string, force bool) (int, error)
	ListAgentPods(ctx context.Context, namespace string, labelSelector map[string]string) ([]corev1.Pod, error)
	GetAgentPod(ctx context.Context, name, namespace string) (*corev1.Pod, error)
	// GetNode fetches a cluster Node by name. Used by the reconciler's
	// node-drain detector (WS1, ka-0000007969) to see whether the node hosting
	// an agent pod is being force-reclaimed (disrupted taint / DeletionTimestamp)
	// so the agent can be gracefully drained before the SIGKILL lands. Nodes are
	// cluster-scoped, so no namespace argument.
	GetNode(ctx context.Context, name string) (*corev1.Node, error)
}

// K8sManager implements Manager using client-go.
type K8sManager struct {
	client  kubernetes.Interface
	logger  *slog.Logger
	nowFunc func() time.Time

	// InitCloneImageOverride, when non-empty, replaces the InitCloneImage
	// default used by buildInitCloneContainer. Set from cfg.InitCloneImage
	// (env: INIT_CLONE_IMAGE) so operators can pin a specific tag/digest
	// via the Helm chart (agents.initCloneImage).
	InitCloneImageOverride string

	// StartupProbeFailureThresholdOverride, when > 0, replaces the compiled
	// default agent-container coop StartupProbe failureThreshold (60). Set from
	// cfg.AgentStartupProbeFailureThreshold (env:
	// AGENT_STARTUP_PROBE_FAILURE_THRESHOLD) so a slow-egress cluster can widen
	// the cold-start budget (failureThreshold × 5s) via the Helm chart
	// (agents.startupProbeFailureThreshold) without re-cutting a controller
	// release (ka-0000000649). <= 0 keeps the default.
	StartupProbeFailureThresholdOverride int

	// InitCloneJitterMaxSeconds staggers the init-clone COLD network clone by a
	// random 0..N seconds so a batch of pods scheduled together does not all hit
	// NAT egress at the same instant (a clone-storm). The sleep runs in the init
	// container BEFORE coop, so it never eats the agent StartupProbe budget, and
	// only on the cold-clone path (warm-PVC pods that merely fetch skip it).
	// <= 0 disables it. Set from cfg.AgentInitCloneJitterMaxSeconds (env:
	// AGENT_INIT_CLONE_JITTER_MAX_SECONDS) via the Helm chart
	// (agents.initCloneJitterMaxSeconds); main.go applies
	// DefaultInitCloneJitterMaxSeconds when the env is unset. Follow-up hardening
	// to the prod-gardenio-eks clone-storm incident (ka-0000000995 / ka-0000001925).
	InitCloneJitterMaxSeconds int

	// MintLandCoopmuxToken mints a tenant land's coopmux token for a spec
	// with LandCoopmuxToken. nil, CreateAgentPod refuses such a spec.
	MintLandCoopmuxToken LandCoopmuxTokenMinter
	// MintLandNatsToken mints a tenant land's NATS password for a spec with
	// LandNatsToken. nil, CreateAgentPod refuses such a spec.
	MintLandNatsToken LandTokenMinter

	// DindImageOverride, when non-empty, replaces the DindImage default used
	// by buildDindSidecar. Set from cfg.DindImage (env: DIND_IMAGE) so
	// operators can pin a specific tag/digest via the Helm chart
	// (agents.dindImage). Air-gap blocker P4.3 (ka-0000007177): the DinD
	// sidecar image was a hardcoded Go constant with no override path.
	DindImageOverride string

	// AgentPullPolicy, when non-empty, replaces the default agent-container
	// ImagePullPolicy (PullAlways). Set from cfg.AgentPullPolicy (env:
	// AGENT_PULL_POLICY) so a single-box / air-gap install can use
	// IfNotPresent — letting a seeded image be reused with no registry
	// round-trip on every spawn. Air-gap blocker P4.3 (ka-0000007177).
	// Default stays PullAlways for cloud deploys.
	AgentPullPolicy corev1.PullPolicy

	// PrewarmedPriorityClass, when non-empty, is stamped as PriorityClassName
	// on prewarmed/standby agent pods (spec.Prewarmed). Set from
	// cfg.PrewarmedPriorityClass (env: PREWARMED_PRIORITY_CLASS), which the Helm
	// chart wires to the negative-priority `gardenio-prewarmed` PriorityClass it
	// renders. A lower priority than real (assigned) agent pods — which keep the
	// cluster-default priority 0 — lets a cold agent spawn preempt an idle
	// prewarmed pod when nodes are tight, instead of the parked pool starving
	// the cold spawn (ka-0000000722). Empty leaves prewarmed pods at default
	// priority (legacy behavior). The chart only sets this env when it renders
	// the PriorityClass, so the controller never references a non-existent class.
	PrewarmedPriorityClass string

	// InteractivePriorityClass, when non-empty, is stamped as PriorityClassName
	// on interactive-priority agent pods (roleHasInteractivePriority(spec.Role):
	// thread/dm/gardener). Set from cfg.InteractivePriorityClass (env:
	// INTERACTIVE_PRIORITY_CLASS), which the Helm chart wires to the
	// positive-priority `gardenio-system-critical` PriorityClass it renders —
	// shared with the postgres/beads/nats/vmsingle platform pods. A
	// higher priority than the cluster-default (0) lets a gardener or a
	// human-waiting thread/dm pod preempt a batch crew pod when the on-demand
	// node pool is fully packed, instead of sitting Pending indefinitely behind
	// bursty crew pods (gd-Vtnt5lAGeQ, 2026-09-22 gardener-concurrency-0
	// incident). Empty leaves interactive pods at default priority (legacy
	// behavior). The chart only sets this env when it renders the
	// PriorityClass, so the controller never references a non-existent class.
	InteractivePriorityClass string

	// ScheduledPriorityClass, when non-empty, replaces InteractivePriorityClass
	// on a gardener pod a schedule or fire-now started and no operator woke
	// (scheduledGardenerRun). Set from cfg.ScheduledPriorityClass (env:
	// SCHEDULED_PRIORITY_CLASS), which the chart wires to `gardenio-scheduled`:
	// the interactive value with preemptionPolicy Never, so the pod is queued
	// ahead of crews but never kills a working one (gd-Y2KaNE233g). Empty keeps
	// those pods on InteractivePriorityClass.
	ScheduledPriorityClass string

	// AgentPriorityClass, when non-empty, is stamped as PriorityClassName on
	// every other agent pod (crew/reviewer/batch). Set from
	// cfg.AgentPriorityClass (env: AGENT_PRIORITY_CLASS), which the Helm chart
	// wires to its `gardenio-agent` class: value 0, preemptionPolicy Never, so
	// a crew burst waits for room instead of evicting anything, and every
	// infra pod outranks it (gd-2qjxTVMBkC). Empty leaves those pods at the
	// cluster-default priority (legacy behavior).
	AgentPriorityClass string

	// VPAStamper, when non-nil and enabled, rewrites the agent container's
	// memory REQUEST at spawn to the VPA recommendation for the pod's
	// (project, role) class, clamped to a safe [floor, max] band (reco-only
	// stamp, ka-0000004606). Nil or disabled leaves the request at its
	// default / project / role-override value. Never mid-run resizes.
	VPAStamper *VPAMemoryStamper

	// Coverage configures runtime coverage instrumentation for dynamically
	// built agent pods (C2 gate of the e2e-coverage epic, ka-0000000508). When
	// Enabled, buildEnvVars adds GOCOVERDIR + LLVM_PROFILE_FILE (rooted under
	// <MountPath>/agents/<pod-name>/) and buildVolumes/buildVolumeMounts wire the
	// shared coverage PVC (ClaimName) at MountPath so the counters land on the
	// same claim the e2e drain task tars. Set from cfg.AgentCoverage* (env:
	// GARDENIO_AGENT_COVERAGE / GARDENIO_AGENT_COVERAGE_CLAIM /
	// GARDENIO_AGENT_COVERAGE_MOUNT), which the chart wires only when
	// global.coverage.enabled. Zero value (Enabled=false) is a full no-op, so
	// non-coverage deploys are unchanged.
	Coverage CoverageConfig
}

// CoverageConfig holds the runtime coverage-instrumentation settings for
// dynamically built agent pods. All fields come from the controller's env
// (chart-wired, gated on global.coverage.enabled); the zero value disables the
// feature. ClaimName is required when Enabled because the shared coverage PVC
// name embeds the Helm release and is not derivable at runtime; MountPath
// defaults to MountCoverageData when unset.
type CoverageConfig struct {
	Enabled   bool
	ClaimName string
	MountPath string
}

// mountPath returns the coverage PVC mount path, defaulting to
// MountCoverageData when the operator did not override it.
func (c CoverageConfig) mountPath() string {
	if c.MountPath != "" {
		return c.MountPath
	}
	return MountCoverageData
}

// active reports whether coverage wiring should be applied: the feature is
// enabled AND a shared-PVC claim name was supplied. Without the claim there is
// nowhere for the counters to land, so we treat that as off (the chart never
// sets Enabled without the claim, but the guard keeps a partial env from
// producing an unmountable pod spec).
func (c CoverageConfig) active() bool {
	return c.Enabled && c.ClaimName != ""
}

// agentPullPolicy returns the configured agent-container ImagePullPolicy,
// defaulting to PullAlways when no override is set (preserving cloud-deploy
// behavior).
func (m *K8sManager) agentPullPolicy() corev1.PullPolicy {
	if m.AgentPullPolicy != "" {
		return m.AgentPullPolicy
	}
	return corev1.PullAlways
}

// dindImage returns the configured DinD sidecar image, defaulting to the
// compiled-in DindImage constant when no override is set.
func (m *K8sManager) dindImage() string {
	if m.DindImageOverride != "" {
		return m.DindImageOverride
	}
	return DindImage
}

// New creates a pod manager backed by a K8s client.
func New(client kubernetes.Interface, logger *slog.Logger) *K8sManager {
	return &K8sManager{client: client, logger: logger, nowFunc: time.Now}
}

// now returns the manager's current time, falling back to time.Now when
// nowFunc is unset (e.g. constructed via struct literal in tests).
func (m *K8sManager) now() time.Time {
	if m.nowFunc != nil {
		return m.nowFunc()
	}
	return time.Now()
}

// CreateAgentPod creates a pod for the given agent spec.
// If the spec includes WorkspaceStorage, the PVC is created or the
// last-active annotation refreshed before the pod is created. PVC lifetime
// is decoupled from the pod's UID so workspace state survives pod
// termination — the PVC GC reconciler reaps PVCs whose last-active
// annotation is older than the configured TTL.
func (m *K8sManager) CreateAgentPod(ctx context.Context, spec AgentPodSpec) (err error) {
	ctx, span := tracer.Start(ctx, "podmanager.CreateAgentPod")
	span.SetAttributes(
		attribute.String("gardenio.agent.project", spec.Project),
		attribute.String("gardenio.agent.role", spec.Role),
		attribute.String("gardenio.agent.name", spec.AgentName),
		attribute.String("k8s.namespace", spec.Namespace),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	if spec.Docker {
		if err := m.checkDindAdmittable(ctx, spec.Namespace); err != nil {
			return err
		}
	}

	// Ziti TUN-sidecar PSA preflight (ka-0000002245): the sidecar adds
	// NET_ADMIN + a /dev/net/tun hostPath, both rejected by PSA
	// baseline/restricted. Fail-closed (return the actionable error) unless
	// the bead set ziti_optional=true, in which case we strip the Ziti wiring
	// so the pod still admits and the agent runs without overlay access —
	// matching the init/tunnel scripts' optional-degradation contract.
	if zitiEnabled(spec) {
		if err := m.checkZitiAdmittable(ctx, spec.Namespace); err != nil {
			if spec.ZitiAccess.Optional {
				m.logger.Warn("ziti preflight: namespace rejects the TUN sidecar and ziti_optional=true; "+
					"stripping Ziti overlay access so the pod admits without it",
					"namespace", spec.Namespace, "agent", spec.AgentName, "error", err)
				spec.ZitiAccess = nil
			} else {
				return err
			}
		}
	}

	if spec.WorkspaceStorage != nil {
		if err := m.ensurePVC(ctx, spec); err != nil {
			return fmt.Errorf("ensuring workspace PVC: %w", err)
		}
	}

	// Admission gate (ka-0000001230 / W4): refuse to schedule a pod whose
	// referenced K8s Secrets do not exist yet. Without this gate a pod can
	// be created before the secretreconciler has produced the ExternalSecret
	// CRD (or before external-secrets-operator has synced the K8s Secret
	// from AWS Secrets Manager), yielding CreateContainerConfigError and a
	// confusing CrashLoopBackoff. The error returned here is not permanent —
	// the reconciler retries on the next sync pass, by which point the
	// ExternalSecret has typically synced.
	//
	// A tenant land's pod first gets its land Secrets written into the land
	// namespace (gd-gokoYMl5px); a copy source that does not exist yet is
	// left to this gate to report.
	if err := m.writeLandSecrets(ctx, spec); err != nil {
		return err
	}
	if err := m.verifySecretsExist(ctx, spec); err != nil {
		return err
	}

	// Reco-only VPA stamp (ka-0000004606): right-size the memory REQUEST from
	// the VPA recommendation for this pod's (project, role) class, when the
	// stamper is enabled and a mature recommendation exists. No-op (leaves the
	// existing request untouched) when disabled, no recommendation yet, or the
	// lookup errors — so we never under-provision below the safe default.
	m.applyVPAMemoryStamp(ctx, &spec)

	pod := m.buildPod(spec)
	m.logger.Info("creating agent pod",
		"pod", pod.Name, "project", spec.Project, "role", spec.Role, "agent", spec.AgentName)

	_, err = m.client.CoreV1().Pods(spec.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return m.adoptOrReplaceExistingPod(ctx, spec, pod)
	}
	if err != nil {
		return fmt.Errorf("creating pod %s: %w", pod.Name, err)
	}
	return nil
}

// PodNameConflictError is returned by CreateAgentPod when the canonical pod
// name is occupied by a stale pod (terminating, terminal-phase, or belonging
// to a different bead) that could not be replaced within this call — e.g. a
// force-deleted pod still pinned by finalizers, or a different pod object
// racing onto the name between inspect and delete. Transient by design: the
// reconciler treats it as retryable (isPermanentCreateError only matches K8s
// validation errors) and re-attempts on the next sync pass, by which point
// the stale object has normally finished going away.
type PodNameConflictError struct {
	Pod       string
	Namespace string
	Reason    string
}

func (e *PodNameConflictError) Error() string {
	return fmt.Sprintf("pod %s/%s: canonical name occupied by a stale pod that could not be replaced yet (%s); retrying on a later pass",
		e.Namespace, e.Pod, e.Reason)
}

// stalePodReason classifies whether an existing pod squatting on the canonical
// name is a stale husk that must be replaced, returning a human-readable
// reason, or "" when the pod is genuinely the live pod for this bead.
//
// The option-B stable-identity fix (ka-0000002360) made wake reuse the same
// agent bead id, so the SAME canonical pod name recurs across incarnations of
// one agent. A prior incarnation mid-termination therefore collides with its
// own successor — the DeletionTimestamp/terminal-phase checks catch that (the
// bead-id annotation MATCHES in this case, so it must be checked first). A
// bead-id mismatch means the name is held by some other agent's leftover
// (bead recreated under the same agent name, or a truncation collision) and is
// equally stale. An empty spec bead id skips the mismatch check — there is
// nothing to compare — leaving only the liveness checks.
func stalePodReason(existing *corev1.Pod, beadID string) string {
	if existing.DeletionTimestamp != nil {
		return "existing pod is terminating"
	}
	if existing.Status.Phase == corev1.PodSucceeded || existing.Status.Phase == corev1.PodFailed {
		return fmt.Sprintf("existing pod is in terminal phase %s", existing.Status.Phase)
	}
	if beadID != "" && existing.Annotations[AnnotationBeadID] != beadID {
		return fmt.Sprintf("existing pod belongs to bead %q, not %q",
			existing.Annotations[AnnotationBeadID], beadID)
	}
	return ""
}

// adoptOrReplaceExistingPod resolves an AlreadyExists collision on the
// canonical pod name (ka-0000006228). Blindly reporting success here wedged
// plan-agent wakes: the wake's AgentSpawn would "create" over the prior
// incarnation's Terminating husk, no coop process would start, and — because
// a dropped spawn event is never redelivered and the reconciler skips pods
// that still exist in the API — nothing resumed until the husk finished
// tearing down on its own.
//
// Instead: GET the existing pod and branch. A genuinely live pod for this
// bead (not terminating, not terminal-phase, bead-id annotation matches) is
// adopted — return nil, preserving create idempotency. Anything else is a
// stale husk: force-delete it (grace 0, UID-preconditioned so a new pod that
// raced onto the name is never killed) and create the replacement in the same
// call. If the name is still held after the delete (finalizers, races),
// return a *PodNameConflictError so the caller/reconciler retries instead of
// reporting success for a pod that was never started.
func (m *K8sManager) adoptOrReplaceExistingPod(ctx context.Context, spec AgentPodSpec, pod *corev1.Pod) error {
	existing, err := m.client.CoreV1().Pods(spec.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// The colliding pod vanished between Create and Get — the name is
		// free now, so create the replacement straight away.
		return m.createReplacementPod(ctx, spec, pod, "colliding pod vanished after AlreadyExists")
	}
	if err != nil {
		return fmt.Errorf("inspecting existing pod %s after AlreadyExists: %w", pod.Name, err)
	}

	reason := stalePodReason(existing, spec.BeadID)
	if reason == "" {
		m.logger.Info("agent pod already exists and is live for this bead — adopting",
			"pod", pod.Name, "bead", spec.BeadID, "phase", existing.Status.Phase)
		return nil
	}

	m.logger.Info("agent pod name occupied by stale pod — force-deleting and recreating",
		"pod", pod.Name, "namespace", spec.Namespace, "bead", spec.BeadID,
		"staleUID", existing.UID, "reason", reason)
	zero := int64(0)
	err = m.client.CoreV1().Pods(spec.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &zero,
		Preconditions:      &metav1.Preconditions{UID: &existing.UID},
	})
	switch {
	case err == nil, apierrors.IsNotFound(err):
		// Deleted (or already gone) — fall through to create.
	case apierrors.IsConflict(err):
		// UID precondition failed: a DIFFERENT pod object took the name since
		// we inspected it — most likely a fresh, legitimate pod. Do not fight
		// it; let the retry re-inspect.
		return &PodNameConflictError{Pod: pod.Name, Namespace: spec.Namespace,
			Reason: "a different pod object claimed the name during replacement"}
	default:
		return fmt.Errorf("deleting stale pod %s: %w", pod.Name, err)
	}
	return m.createReplacementPod(ctx, spec, pod, reason)
}

// createReplacementPod creates pod after a stale-name resolution. An
// AlreadyExists here means the force-deleted object is still pinned in the
// API (e.g. finalizers) — surface a *PodNameConflictError rather than
// swallowing it, so the caller/reconciler retries once the object is gone.
func (m *K8sManager) createReplacementPod(ctx context.Context, spec AgentPodSpec, pod *corev1.Pod, why string) error {
	_, err := m.client.CoreV1().Pods(spec.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return &PodNameConflictError{Pod: pod.Name, Namespace: spec.Namespace,
			Reason: fmt.Sprintf("stale pod still present after force-delete (%s)", why)}
	}
	if err != nil {
		return fmt.Errorf("recreating pod %s after stale-name resolution (%s): %w", pod.Name, why, err)
	}
	m.logger.Info("recreated agent pod over stale name",
		"pod", pod.Name, "namespace", spec.Namespace, "bead", spec.BeadID, "reason", why)
	return nil
}

// ensurePVC creates the workspace PVC if it does not already exist, and
// always refreshes the AnnotationPVCLastActive timestamp so the PVC GC
// reconciler treats this PVC as freshly used.
func (m *K8sManager) ensurePVC(ctx context.Context, spec AgentPodSpec) error {
	ws := spec.WorkspaceStorage
	claimName := ws.ClaimName
	if claimName == "" {
		claimName = spec.PodName() + "-workspace"
	}

	size := ws.Size
	if size == "" {
		size = "10Gi"
	}
	storageClass := ws.StorageClassName

	now := m.now().UTC().Format(time.RFC3339)
	annotations := map[string]string{
		AnnotationPVCLastActive: now,
		AnnotationBeadID:        spec.BeadID,
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        claimName,
			Namespace:   spec.Namespace,
			Labels:      spec.Labels(),
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}
	if storageClass != "" {
		pvc.Spec.StorageClassName = &storageClass
	}

	_, err := m.client.CoreV1().PersistentVolumeClaims(spec.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err == nil {
		m.logger.Info("created workspace PVC", "pvc", claimName, "size", size, "storageClass", storageClass)
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating PVC %s: %w", claimName, err)
	}

	// PVC already exists — patch the last-active annotation so the GC
	// reconciler resets its TTL timer for this re-spawn.
	patch := fmt.Appendf(nil,
		`{"metadata":{"annotations":{%q:%q}}}`,
		AnnotationPVCLastActive, now,
	)
	_, patchErr := m.client.CoreV1().PersistentVolumeClaims(spec.Namespace).Patch(
		ctx, claimName, types.MergePatchType, patch, metav1.PatchOptions{},
	)
	if patchErr != nil {
		// Non-fatal: the PVC exists, the pod will still mount it. Worst case
		// the GC reconciler may delete the PVC sooner than expected.
		m.logger.Warn("failed to refresh PVC last-active annotation",
			"pvc", claimName, "error", patchErr)
		return nil
	}
	m.logger.Info("refreshed workspace PVC last-active", "pvc", claimName, "ts", now)
	return nil
}

// MissingSecretsError is returned by CreateAgentPod when one or more K8s
// Secrets referenced by the pod spec do not exist in the target namespace.
// The reconciler treats this as a transient error (see isPermanentCreateError)
// and retries on the next sync pass.
type MissingSecretsError struct {
	Pod       string
	Namespace string
	Names     []string
}

func (e *MissingSecretsError) Error() string {
	return fmt.Sprintf("pod %s: %d secret(s) not yet synced in namespace %s: %s",
		e.Pod, len(e.Names), e.Namespace, strings.Join(e.Names, ", "))
}

// referencedSecretNames returns the sorted, deduplicated list of K8s Secret
// names that spec references and that MUST exist before the pod can be created
// (Claude credentials file, daemon token, git credentials, GitLab token, image
// pull secrets, projected SSH keys, and non-optional env-var sources).
//
// Optional SecretEnv sources are deliberately excluded: their SecretKeyRef is
// marked optional in the pod spec, so a missing secret/key does not block the
// pod at runtime — kubelet simply omits the env var. Including them in this
// admission gate would defer pod creation on a secret that, by design, may not
// exist yet. That is exactly how the env-side `grafana-api-key` bundle
// (ka-0000002337, marked Optional so it can be provisioned per-env over time)
// wedged ALL agent pod creation fleet-wide until the secret was hand-created.
// The admission gate must honor Optional the same way the runtime SecretKeyRef
// does.
func referencedSecretNames(spec AgentPodSpec) []string {
	seen := make(map[string]struct{})
	add := func(n string) {
		if n != "" {
			seen[n] = struct{}{}
		}
	}
	for _, se := range spec.SecretEnv {
		if se.Optional {
			continue
		}
		add(se.SecretName)
	}
	add(spec.CredentialsSecret)
	add(spec.DaemonTokenSecret)
	add(spec.NatsTokenSecret)
	add(spec.ObjectStoreCredentialsSecret)
	add(spec.GitCredentialsSecret)
	add(spec.GitlabTokenSecret)
	add(spec.CodexAuthSecret)
	add(spec.GeminiOAuthSecret)
	add(spec.MachineClientSecret)
	for _, n := range spec.ImagePullSecrets {
		add(n)
	}
	for _, k := range spec.SshKeys {
		add(k.SecretName)
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// verifySecretsExist returns a *MissingSecretsError when one or more K8s
// Secrets referenced by the spec do not exist in spec.Namespace. Non-NotFound
// API errors are wrapped and returned as-is so the caller treats them as
// general failures rather than missing-secret retries.
//
// This is the admission gate that closes the project-scoped ExternalSecret
// reconciliation race (ka-0000001230): the secretreconciler creates the
// ExternalSecret CRD, external-secrets-operator then materializes the K8s
// Secret from AWS Secrets Manager, and only then can pods that reference
// the secret start. Scheduling pods before the K8s Secret materializes
// yields CreateContainerConfigError and CrashLoopBackoff with a confusing
// "secret not found" message.
func (m *K8sManager) verifySecretsExist(ctx context.Context, spec AgentPodSpec) error {
	names := referencedSecretNames(spec)
	if len(names) == 0 {
		return nil
	}
	var missing []string
	for _, name := range names {
		_, err := m.client.CoreV1().Secrets(spec.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("checking secret %s/%s: %w", spec.Namespace, name, err)
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return nil
	}
	m.logger.Info("deferring pod create — referenced secrets not yet synced",
		"pod", spec.PodName(), "namespace", spec.Namespace, "missing", missing)
	return &MissingSecretsError{
		Pod:       spec.PodName(),
		Namespace: spec.Namespace,
		Names:     missing,
	}
}

// DeleteAgentPod deletes a pod by name and namespace.
// Returns nil if the pod does not exist (idempotent).
func (m *K8sManager) DeleteAgentPod(ctx context.Context, name, namespace string) error {
	ctx, span := tracer.Start(ctx, "podmanager.DeleteAgentPod")
	span.SetAttributes(
		attribute.String("k8s.pod.name", name),
		attribute.String("k8s.namespace", namespace),
	)
	defer span.End()

	m.logger.Info("deleting agent pod", "pod", name, "namespace", namespace)
	err := m.client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		m.logger.Info("agent pod already deleted", "pod", name)
		return nil
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// DeleteAllAgentPods deletes all pods matching the app.kubernetes.io/name=gardenio
// label in the given namespace. When force is true, pods are deleted with
// GracePeriodSeconds=0 and PropagationPolicy=Background for immediate termination.
// Returns the number of pods deleted and any aggregated errors.
func (m *K8sManager) DeleteAllAgentPods(ctx context.Context, namespace string, force bool) (int, error) {
	pods, err := m.ListAgentPods(ctx, namespace, map[string]string{
		LabelApp: LabelAppValue,
	})
	if err != nil {
		return 0, fmt.Errorf("listing agent pods: %w", err)
	}

	var (
		deleted int
		errs    []error
	)

	opts := metav1.DeleteOptions{}
	if force {
		zero := int64(0)
		bg := metav1.DeletePropagationBackground
		opts.GracePeriodSeconds = &zero
		opts.PropagationPolicy = &bg
	}

	for _, pod := range pods {
		m.logger.Warn("deleting agent pod", "pod", pod.Name, "namespace", namespace, "force", force)
		err := m.client.CoreV1().Pods(namespace).Delete(ctx, pod.Name, opts)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("deleting pod %s: %w", pod.Name, err))
			continue
		}
		deleted++
	}

	return deleted, errors.Join(errs...)
}

// ListAgentPods lists pods matching the given labels.
func (m *K8sManager) ListAgentPods(ctx context.Context, namespace string, labelSelector map[string]string) ([]corev1.Pod, error) {
	sel := labels.Set(labelSelector).String()
	list, err := m.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sel,
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods with selector %s: %w", sel, err)
	}
	return list.Items, nil
}

// GetAgentPod gets a single pod by name.
func (m *K8sManager) GetAgentPod(ctx context.Context, name, namespace string) (*corev1.Pod, error) {
	ctx, span := tracer.Start(ctx, "podmanager.GetAgentPod")
	span.SetAttributes(
		attribute.String("k8s.pod.name", name),
		attribute.String("k8s.namespace", namespace),
	)
	defer span.End()

	pod, err := m.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return pod, err
}

// GetNode fetches a cluster Node by name (cluster-scoped, no namespace).
func (m *K8sManager) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	ctx, span := tracer.Start(ctx, "podmanager.GetNode")
	span.SetAttributes(attribute.String("k8s.node.name", name))
	defer span.End()

	node, err := m.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return node, err
}

// BuildPod renders the Pod CreateAgentPod would create for spec, without
// touching the cluster. The land fence's allowlist test reads it.
func (m *K8sManager) BuildPod(spec AgentPodSpec) *corev1.Pod { return m.buildPod(spec) }

func (m *K8sManager) buildPod(spec AgentPodSpec) *corev1.Pod {
	container := m.buildContainer(spec)
	volumes := m.buildVolumes(spec)

	containers := []corev1.Container{container}

	var initContainers []corev1.Container
	// thaw-state-perms runs FIRST (before init-clone) so the per-agent
	// hostPath is agent-writable before any container needs it. Only runsc
	// pods carry the thaw-state volume (bug ka-0000001058).
	if ic := m.buildThawStatePermsInit(spec); ic != nil {
		initContainers = append(initContainers, *ic)
	}
	if ic := m.buildInitCloneContainer(spec); ic != nil {
		initContainers = append(initContainers, *ic)
	}
	// OpenZiti per-pod access (epic ka-0000000990): the ziti-lease PLAIN init
	// container mints the task-scoped lease and writes the enrollment JWT into
	// the shared identity dir; the ziti-tunnel NATIVE sidecar (restartPolicy:
	// Always) then enrolls that JWT and runs ziti-edge-tunnel. The lease init
	// is appended as a plain init (it must complete before the tunnel sidecar
	// has a JWT to enroll); the tunnel is a native sidecar so it stays running
	// alongside the agent. Both are no-ops when spec.ZitiAccess is nil.
	if ic := m.buildZitiLeaseInit(spec); ic != nil {
		initContainers = append(initContainers, *ic)
	}
	if sc := m.buildZitiTunnelSidecar(spec); sc != nil {
		initContainers = append(initContainers, *sc)
	}
	if spec.Docker {
		initContainers = append(initContainers, m.buildDindSidecar())
	}
	if spec.Postgres {
		initContainers = append(initContainers, m.buildPostgresSidecar())
	}

	// AutomountServiceAccountToken defaults to false so agent pods do not
	// receive a K8s API token. Opt in via spec.NeedsK8sAPI for workloads
	// that legitimately need cluster API access. See ka-0000006344 (C3).
	automount := spec.NeedsK8sAPI

	podSpec := corev1.PodSpec{
		InitContainers:               initContainers,
		Containers:                   containers,
		Volumes:                      volumes,
		RestartPolicy:                restartPolicyForMode(spec.Mode),
		AutomountServiceAccountToken: &automount,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser:    intPtr(AgentUID),
			RunAsGroup:   intPtr(AgentGID),
			RunAsNonRoot: boolPtr(true),
			FSGroup:      intPtr(AgentGID),
			// Required for admission under PSA "restricted" stamped on
			// fleet namespaces (ka-0000006703). Matches the chart's
			// pod-level standard (audit 2026-04-18).
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
	}

	if spec.ServiceAccountName != "" {
		podSpec.ServiceAccountName = spec.ServiceAccountName
	}
	if spec.RuntimeClassName != "" {
		rc := spec.RuntimeClassName
		podSpec.RuntimeClassName = &rc
	}
	// Prewarmed/standby pods get a low (negative) PriorityClass so a cold real
	// agent spawn preempts an idle pool member when nodes are tight, instead of
	// the parked pool starving the cold spawn (ka-0000000722). Real agents keep
	// the cluster-default priority 0. Only set when the chart rendered the class
	// and exported PREWARMED_PRIORITY_CLASS — otherwise referencing a missing
	// PriorityClass would make admission reject the pod.
	if spec.Prewarmed && m.PrewarmedPriorityClass != "" {
		podSpec.PriorityClassName = m.PrewarmedPriorityClass
	} else if spec.LandFenced {
		// A tenant land's pod never outranks a fleet pod, whatever its role
		// (gd-NDJIOutLsv). Empty AgentPriorityClass leaves it at priority 0.
		podSpec.PriorityClassName = m.AgentPriorityClass
	} else if scheduledGardenerRun(spec) && m.ScheduledPriorityClass != "" {
		// A gardener a schedule or fire-now started: queued ahead of crews but
		// never evicts one, as nobody is waiting on it (gd-Y2KaNE233g).
		podSpec.PriorityClassName = m.ScheduledPriorityClass
	} else if roleHasInteractivePriority(spec.Role) && m.InteractivePriorityClass != "" {
		// Gardener/thread/dm pods get a positive PriorityClass so they can
		// preempt default-priority (0) batch crew pods when nodes are fully
		// packed (gd-Vtnt5lAGeQ). Gardeners keep this even though they now run
		// on Spot like crew (gd-Fyz3w0tOnf): see roleHasInteractivePriority. Only set when the chart rendered the
		// shared gardenio-system-critical class and exported
		// INTERACTIVE_PRIORITY_CLASS.
		podSpec.PriorityClassName = m.InteractivePriorityClass
	} else if m.AgentPriorityClass != "" {
		// Crew/reviewer/batch pods: lowest tier, never preempts (gd-2qjxTVMBkC).
		podSpec.PriorityClassName = m.AgentPriorityClass
	}
	// Extra DNS search domains (bug ka-0000001058): a runsc agent routed into
	// the gVisor namespace must resolve the controller's bare cross-namespace
	// service names (gardenio-coopmux, gardenio-nats). DNSPolicy stays
	// ClusterFirst so the cluster nameserver + default searches are preserved;
	// we only APPEND the release-namespace search domain.
	if len(spec.DNSSearches) > 0 {
		podSpec.DNSPolicy = corev1.DNSClusterFirst
		podSpec.DNSConfig = &corev1.PodDNSConfig{Searches: spec.DNSSearches}
	}
	if len(spec.NodeSelector) > 0 {
		podSpec.NodeSelector = spec.NodeSelector
	}
	if len(spec.Tolerations) > 0 {
		podSpec.Tolerations = spec.Tolerations
	}
	if spec.Affinity != nil {
		podSpec.Affinity = spec.Affinity
	}
	// Spread agent pods across nodes so a single node loss (Spot reclaim,
	// AWS health event, expiry recycle) cannot take out N agents at once
	// (Spot-hardening blast-radius proof, ka-0000005885). This is the pod
	// half of the proof gate; the node half was instance-family diversity in
	// the EKS-era Karpenter NodePool (more independent Spot pools -> lower
	// correlated reclaim; that terraform module is deleted). SOFT (ScheduleAnyway) so it only ever biases placement and
	// never blocks a spawn — under capacity pressure agents still co-locate
	// rather than sitting FailedScheduling. Applied to every agent pod; a
	// warm placeholder / probe pod carries no LabelAgent and is excluded from
	// both the constraint and its selector.
	podSpec.TopologySpreadConstraints = agentTopologySpreadConstraints()
	if len(spec.ImagePullSecrets) > 0 {
		for _, name := range spec.ImagePullSecrets {
			podSpec.ImagePullSecrets = append(podSpec.ImagePullSecrets,
				corev1.LocalObjectReference{Name: name})
		}
	}

	// Use a 45s termination grace period so the preStop hook and entrypoint
	// signal handler have time to gracefully save the session before SIGKILL.
	gracePeriod := int64(45)
	podSpec.TerminationGracePeriodSeconds = &gracePeriod

	annotations := map[string]string{
		AnnotationBeadID: spec.BeadID,
		// Prometheus scrape annotations — vmagent discovers pods with
		// prometheus.io/scrape=true and scrapes the coop /metrics endpoint on
		// the dedicated (unauthenticated) metrics port.
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   fmt.Sprintf("%d", CoopDefaultMetricsPort),
		"prometheus.io/path":   "/metrics",
	}
	if spec.Prewarmed {
		annotations[AnnotationPrewarmed] = "true"
	}
	// Protect EVERY agent pod from Karpenter node consolidation, mirroring the
	// do-not-disrupt stamp on gardenio infra pods. Without this, "underutilized"
	// consolidation evicts agent pods mid-work with no replacement node, which
	// reads as an agent being killed accidentally (investigation ka-0000006340).
	// Per operator directive (Matt Baker), no agent role is disruptable — crew
	// and job pods are protected too, not just thread/DM/gardener.
	annotations[AnnotationKarpenterDoNotDisrupt] = "true"

	// Provenance stamps for a privileged-role spawn approved through the
	// escalation pathway (gd-h7fgTgcuyn, design doc gd-sGW5Y1B8sg §5). Absent
	// on an ordinary, non-escalated spawn.
	if spec.SpawnedBy != "" {
		annotations[AnnotationSpawnedBy] = spec.SpawnedBy
	}
	if spec.SpawnApprovalMode != "" {
		annotations[AnnotationSpawnApprovalMode] = spec.SpawnApprovalMode
	}
	if spec.SpawnApprovedBy != "" {
		annotations[AnnotationSpawnApprovedBy] = spec.SpawnApprovedBy
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.PodName(),
			Namespace:   spec.Namespace,
			Labels:      spec.Labels(),
			Annotations: annotations,
		},
		Spec: podSpec,
	}
}

// buildThawStatePermsInit returns a root init container that chowns the
// per-agent thaw-state hostPath to the agent UID/GID so the in-pod gb + coop
// rehydration watchers can write their armed sentinels (thaw-epoch.*.armed)
// and ack files at BOOT — long before any thaw. It returns nil for non-runsc
// pods, which never carry the thaw-state volume.
//
// Why this is needed (bug ka-0000001058): the thaw-state volume is a
// HostPathDirectoryOrCreate that the kubelet provisions as root:root 0755, and
// k8s pod-level FSGroup does NOT manage ownership of hostPath volumes — so the
// agent (uid 1000) cannot write to it. Before this init container, coop+gb
// WriteArmed got EPERM and the armed sentinels were never created, so a freeze
// (with the armed-guard on) would refuse to checkpoint and the agent could
// never prove its watchers were live. The only other chown was in the THAW
// path (NodeRehydrationTrigger.writeMarker), a chicken-and-egg (freeze must
// precede thaw). Doing the chown declaratively in-pod avoids any controller
// node-exec and survives the Pod-B deep-copy (the placeholder inherits init
// containers via the snapshot PodTemplate).
//
// Runs as root, which is admissible because runsc agents run in the
// PSA-privileged gVisor namespace (routeGVisorNamespace). Idempotent and
// fast — a single chown+chmod on an empty dir.
func (m *K8sManager) buildThawStatePermsInit(spec AgentPodSpec) *corev1.Container {
	if spec.RuntimeClassName != RuntimeClassRunsc || spec.NoThawState {
		return nil
	}
	script := fmt.Sprintf("chown %d:%d %s && chmod 0775 %s",
		AgentUID, AgentGID, MountThawState, MountThawState)
	return &corev1.Container{
		Name:            ThawStatePermsInitName,
		Image:           InitCloneImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c", script},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:    intPtr(0),
			RunAsNonRoot: boolPtr(false),
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: VolumeThawState, MountPath: MountThawState},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	}
}

// buildContainer constructs the agent container with env vars, resources,
// volume mounts, and security context.
func (m *K8sManager) buildContainer(spec AgentPodSpec) corev1.Container {
	envVars := m.buildEnvVars(spec)
	mounts := m.buildVolumeMounts(spec)
	resources := m.buildResources(spec)

	c := corev1.Container{
		Name:            ContainerName,
		Image:           spec.Image,
		Env:             envVars,
		Resources:       resources,
		VolumeMounts:    mounts,
		ImagePullPolicy: m.agentPullPolicy(),
		// ResizePolicy declares that a memory request/limit change does NOT
		// require a container restart (RestartNotRequired). This is harmless
		// today — we only stamp right-sized requests at CREATE (reco-only,
		// ka-0000004606) and never mutate a running pod — but it is the
		// prerequisite the kubelet checks for any later in-place resize path
		// (KEP-1287 / InPlacePodVerticalScaling), which is gated separately
		// (ka-0000003138). Setting it now means the enforce/in-place child does
		// not have to re-roll every long-lived pod's spec to opt in. Memory
		// only — CPU resize policy is left at its default.
		ResizePolicy: []corev1.ContainerResizePolicy{{
			ResourceName:  corev1.ResourceMemory,
			RestartPolicy: corev1.NotRequired,
		}},
		SecurityContext: &corev1.SecurityContext{
			// The agent runs unprivileged as UID 1000. No sudoers entry is
			// installed in the image, so AllowPrivilegeEscalation and the
			// SETUID/SETGID capabilities are unnecessary and left off to
			// prevent compromised agents from escalating to root.
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}

	// Agent image uses entrypoint.sh (PID 1), which launches coop internally.
	// Port args are reserved for a future transition to 'gb agent start --k8s'
	// as the container entrypoint; entrypoint.sh ignores them and uses env vars.
	c.Args = []string{
		"--port", fmt.Sprintf("%d", CoopDefaultPort),
		"--health-port", fmt.Sprintf("%d", CoopDefaultHealthPort),
		"--metrics-port", fmt.Sprintf("%d", CoopDefaultMetricsPort),
	}

	// Use HTTP probes against coop endpoints and expose ports.
	// - livez:  lock-free liveness — returns 503 for zombie agents (exited,
	//           fatal auth errors) so K8s restarts the pod.
	// - ready:  readiness — returns 503 until the agent has left Starting state.
	// - health: startup — rich health info, used only during initial boot.
	c.Ports = []corev1.ContainerPort{
		{Name: "api", ContainerPort: CoopDefaultPort},
		{Name: "health", ContainerPort: CoopDefaultHealthPort},
		{Name: "metrics", ContainerPort: CoopDefaultMetricsPort},
		{Name: "channel", ContainerPort: ChannelPort},
	}
	c.LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/api/v1/livez",
				Port: intstr.FromString("health"),
			},
		},
		InitialDelaySeconds: 10,
		PeriodSeconds:       15,
		FailureThreshold:    3,
	}
	c.ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/api/v1/ready",
				Port: intstr.FromString("health"),
			},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
	}
	// FailureThreshold*PeriodSeconds is the hard budget for coop to answer
	// /api/v1/health for the first time. gb init (PID 1) runs cold-start setup —
	// including workspace repo materialization — BEFORE coop launches, so that
	// work must finish inside this window or the kubelet SIGKILLs the container
	// and the next restart repeats it, an infinite crashloop.
	//
	// ka-0000000649 (P1 prod incident): on prod-gardenio-eks, crew pods
	// crashlooped because runSetupRepos re-cloned the multi-GB PRIMARY repo over
	// the network (a vestigial clone into {workspace}/repos that nothing reads)
	// on this critical path; when a clone-storm throttled egress to ~0.5MB/s the
	// clone blew the old 300s (60*5s) budget. The real fix is in setup_repos.go:
	// runSetupRepos now early-returns when init-clone already materialized the
	// primary, so no network re-clone happens on the startup path. This bumped
	// default (180*5s = 15min) plus the operator override below remain as
	// defense-in-depth for any other slow cold-start cause on a contended
	// cluster. Override via agents.startupProbeFailureThreshold (env
	// AGENT_STARTUP_PROBE_FAILURE_THRESHOLD) without re-cutting a release.
	startupFailureThreshold := int32(defaultAgentStartupProbeFailureThreshold)
	if m.StartupProbeFailureThresholdOverride > 0 {
		startupFailureThreshold = int32(m.StartupProbeFailureThresholdOverride)
	}
	c.StartupProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/api/v1/health",
				Port: intstr.FromString("health"),
			},
		},
		FailureThreshold: startupFailureThreshold,
		PeriodSeconds:    5,
	}

	// PreStop hook: salvage uncommitted WIP FIRST, then interrupt Claude and
	// request graceful coop shutdown before K8s sends SIGTERM to PID-1.
	//
	// `gb salvage` bundles every dirty worktree to S3 (bug ka-0000000146). It
	// runs first, at t≈0 with the worktree intact, so the WIP is captured before
	// teardown — a salvage placed in gb-init's post-drain cleanup would run last
	// with ~10s left. It is strictly self-timeboxed and always exits 0; the
	// trailing `|| true` is belt-and-suspenders so the drain sequence proceeds
	// regardless.
	//
	// Grace budget (terminationGracePeriodSeconds = 45s, shared by preStop AND
	// the post-SIGTERM coop drain): preStop worst case is salvage 15s + sleep 2s
	// + sleep 3s = 20s, leaving ~25s for coop's drain (COOP_DRAIN_TIMEOUT_MS
	// 20s) + shutdown/S3 finalize (COOP_SHUTDOWN_TIMEOUT_MS 10s, finalize ≤30s).
	// Keep salvage's --timeout small enough that this sum stays under 45s so a
	// slow salvage cannot cannibalize coop's proven session-archive finalize.
	c.Lifecycle = &corev1.Lifecycle{
		PreStop: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"sh", "-c",
					`gb salvage --timeout=15s 2>/dev/null || true; curl -sf -X POST http://localhost:8080/api/v1/input/keys -H 'Content-Type: application/json' -d '{"keys":["Escape"]}' 2>/dev/null; sleep 2; curl -sf -X POST http://localhost:8080/api/v1/shutdown 2>/dev/null; sleep 3`,
				},
			},
		},
	}

	return c
}

// IdentityEnvVars returns the runtime-neutral agent identity + correlation +
// attribution env vars every agent needs regardless of execution backend. It is
// the single source of truth for these keys so the Kubernetes pod path
// (buildEnvVars) and the lambda_microvm path (microvmAgentSpecFromPodSpec) can
// never drift: `gb init` reads BOAT_AGENT_BEAD_ID to identify its own bead, set
// agent_state, and hydrate/claim BOAT_TASK_ID; without these an agent boots but
// is invisible to the control plane and never claims work (ka-0000000401 — the
// C7 lambda_microvm live-run finding).
//
// It deliberately contains ONLY values derivable from the runtime-neutral
// AgentPodSpec fields. It does NOT include pod-specific values — HOME,
// XDG_STATE_HOME (a PVC path), POD_IP (downward API), DinD/Postgres sidecar
// endpoints, service DNS, secret mounts — those stay in buildEnvVars because
// they are meaningful only inside a Kubernetes pod. The MicroVM image owns its
// own HOME/state locations and reaches services over its local Ziti proxy.
//
// Empty attribution fields (OperatedBy/UserBeadID) and TaskID/TriggerBeadID are
// omitted so the map matches the pod path's conditional emission exactly.
func IdentityEnvVars(spec AgentPodSpec) map[string]string {
	env := map[string]string{
		"BOAT_ROLE":    spec.Role,
		"BOAT_PROJECT": spec.Project,
		"BOAT_AGENT":   spec.AgentName,
		"BOAT_MODE":    spec.Mode,
		// The agent's own bead: gb init / prime.sh look up hook_bead + set
		// agent_state via this; KD_AGENT_ID / KD_ACTOR are gate identity for
		// gb (and kd). BEADS_ACTOR / GIT_AUTHOR_NAME attribute writes + commits.
		"BEADS_ACTOR":        spec.AgentName,
		"KD_ACTOR":           spec.AgentName,
		"KD_AGENT_ID":        spec.BeadID,
		"GIT_AUTHOR_NAME":    spec.AgentName,
		"BEADS_AGENT_NAME":   fmt.Sprintf("%s/%s", spec.Project, spec.AgentName),
		"BOAT_AGENT_BEAD_ID": spec.BeadID,
		// Claude Code auto-updater corrupts the pinned install (npm atomic
		// rename race); disable it in every runtime.
		"DISABLE_AUTOUPDATER": "1",
	}
	// Pre-assigned task: gb init nudges the agent to claim this specific bead
	// instead of discovering work via gb ready.
	if spec.TaskID != "" {
		env["BOAT_TASK_ID"] = spec.TaskID
	}
	// Auto-link: the trigger bead whose event spawned this agent, so the harness
	// can send X-Trigger-Bead on its first kd claim.
	if spec.TriggerBeadID != "" {
		env["BOAT_TRIGGER_BEAD_ID"] = spec.TriggerBeadID
	}
	// Human-operator attribution (X-Operated-By). Empty for scheduler-driven.
	if spec.OperatedBy != "" {
		env["BEADS_OPERATED_BY"] = spec.OperatedBy
	}
	// User bead that owns this agent, for downstream audit/attribution.
	if spec.UserBeadID != "" {
		env["BEADS_USER_BEAD_ID"] = spec.UserBeadID
	}
	return env
}

// buildEnvVars constructs environment variables from plain values and secret references.
func (m *K8sManager) buildEnvVars(spec AgentPodSpec) []corev1.EnvVar {
	envVars := []corev1.EnvVar{
		{Name: "HOME", Value: "/home/agent"},
		// Session persistence: point XDG_STATE_HOME to the PVC so Claude
		// session logs and coop session artifacts survive pod restarts.
		{Name: "XDG_STATE_HOME", Value: MountStateDir},
		// POD_IP via downward API — used by coop to advertise its reachable URL
		// to coopmux (COOP_BROKER_URL registration).
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
		}},
		// POD_NAME via downward API — the FULL, untruncated pod name. `gb agent
		// start` registers the coopmux session under this value (preferring it
		// over os.Hostname()), and coop's own mux_client prefers it too. The
		// kernel caps gethostname()/$HOSTNAME at 63 bytes (HOST_NAME_MAX), so a
		// 64+ char pod name registered via the hostname is silently truncated
		// and can never match the full-name terminal deep-link on the Slack
		// agent card (ka-0000003655). The downward API carries the full name.
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
		}},
	}
	// Runtime-neutral agent identity/correlation/attribution — shared verbatim
	// with the lambda_microvm path so the two backends can never drift
	// (ka-0000000401). Includes BOAT_ROLE/PROJECT/AGENT/MODE, the bead-ID
	// identity vars, BOAT_TASK_ID/BOAT_TRIGGER_BEAD_ID, and attribution.
	for k, v := range IdentityEnvVars(spec) {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	// Docker-in-Docker: point the agent's Docker CLI at the DinD sidecar.
	if spec.Docker {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "DOCKER_HOST",
			Value: fmt.Sprintf("tcp://127.0.0.1:%d", DindPort),
		})
	}

	// PostgreSQL sidecar: set connection env vars for the agent.
	if spec.Postgres {
		envVars = append(envVars,
			corev1.EnvVar{Name: "PGHOST", Value: "127.0.0.1"},
			corev1.EnvVar{Name: "PGPORT", Value: fmt.Sprintf("%d", PostgresPort)},
			corev1.EnvVar{Name: "PGUSER", Value: PostgresUser},
			corev1.EnvVar{Name: "PGDATABASE", Value: PostgresDB},
		)
	}

	// Enable session resume for persistent roles (those with workspace PVC),
	// unless the project, role, or agent metadata has opted out by setting
	// session_resume=false. Default = enabled when a PVC is present.
	if spec.WorkspaceStorage != nil {
		resume := true
		if spec.SessionResume != nil {
			resume = *spec.SessionResume
		}
		if resume {
			envVars = append(envVars, corev1.EnvVar{Name: "BOAT_SESSION_RESUME", Value: "1"})
		} else {
			envVars = append(envVars, corev1.EnvVar{Name: "BOAT_SESSION_RESUME", Value: "0"})
		}
	}

	// Agent identity, correlation, task assignment, attribution, and the
	// auto-updater guard are all emitted by the shared IdentityEnvVars builder
	// at the top of this function (runtime-neutral; kept in lockstep with the
	// lambda_microvm path). GIT_AUTHOR_EMAIL and other pod-only env continue
	// below.

	// OpenZiti per-pod access (epic ka-0000000990): point the agent at the
	// broker and its pod-local Ziti identity. Added before the spec.Env loop so
	// a per-bead env override can still win if ever needed. No-op when
	// spec.ZitiAccess is nil.
	envVars = append(envVars, m.buildZitiAgentEnv(spec)...)

	// Go soft memory limit, derived from the container's memory limit. Skipped
	// when spec.Env sets GOMEMLIMIT itself, so a project or role can override it.
	if _, set := spec.Env["GOMEMLIMIT"]; !set {
		if v := goMemLimitFor(m.buildResources(spec)); v != "" {
			envVars = append(envVars, corev1.EnvVar{Name: "GOMEMLIMIT", Value: v})
		}
	}

	// Add plain env vars from spec.
	for k, v := range spec.Env {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	// Add secret-sourced env vars.
	for _, se := range spec.SecretEnv {
		ref := &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: se.SecretName},
			Key:                  se.SecretKey,
		}
		if se.Optional {
			optional := true
			ref.Optional = &optional
		}
		envVars = append(envVars, corev1.EnvVar{
			Name:      se.EnvName,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: ref},
		})
	}

	// Daemon token from secret for agent→daemon authentication.
	if spec.DaemonTokenSecret != "" {
		secretRef := corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: spec.DaemonTokenSecret},
				Key:                  "token",
			},
		}
		// Optional refresh token co-located in the same Secret under
		// "refresh_token". Only present when provisionAgentToken issued the
		// pair (PER_AGENT_TOKENS path); the SecretKeyRef is marked optional
		// so older Secrets without the key don't fail pod creation.
		refreshTrue := true
		refreshRef := corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: spec.DaemonTokenSecret},
				Key:                  "refresh_token",
				Optional:             &refreshTrue,
			},
		}
		envVars = append(envVars,
			corev1.EnvVar{Name: "BEADS_DAEMON_TOKEN", ValueFrom: &secretRef},
			// BEADS_AUTH_TOKEN is read by the kd CLI to authenticate
			// requests to the beads daemon HTTP API.
			corev1.EnvVar{Name: "BEADS_AUTH_TOKEN", ValueFrom: &secretRef},
			// BEADS_REFRESH_TOKEN, when present, lets long-lived clients
			// (kd, gb, controller-side beadsapi) auto-refresh on a 401
			// expired-token challenge instead of dying mid-run.
			corev1.EnvVar{Name: "BEADS_REFRESH_TOKEN", ValueFrom: &refreshRef},
		)
	}

	// gVisor thaw-epoch rendezvous (ka-0000004017): point the in-pod gb + coop
	// thaw watchers at the marker dir. Only on runsc pods (where the hostPath
	// volume/mount is wired); unset elsewhere so the watchers stay no-ops.
	if spec.RuntimeClassName == RuntimeClassRunsc && !spec.NoThawState {
		envVars = append(envVars, corev1.EnvVar{Name: EnvThawStateDir, Value: MountThawState})
	}

	// Resolved K8s RuntimeClass, surfaced so coop can report it in its coopmux
	// registration metadata (k8s.runtime_class) and the dashboard can show
	// which sandbox an agent runs under. Unset means the cluster default.
	if spec.RuntimeClassName != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "BOAT_RUNTIME_CLASS", Value: spec.RuntimeClassName})
	}

	// Runtime coverage instrumentation (C2, ka-0000000508): point the Go
	// runtime (GOCOVERDIR) and the coop LLVM runtime (LLVM_PROFILE_FILE) at a
	// per-pod subtree of the shared coverage PVC. No-op unless coverage is
	// active (enabled + a shared-PVC claim wired via the chart).
	envVars = append(envVars, m.buildCoverageEnv(spec)...)

	return envVars
}

// buildCoverageEnv returns the coverage env vars for the agent container, or
// nil when coverage is inactive. Both binaries write into a per-pod subtree of
// the shared coverage PVC so counters from concurrent agents never collide:
//
//   - GARDENIO_AGENT_COVERAGE = 1                                (entrypoint gate)
//   - GOCOVERDIR             = <mount>/agents/<pod>/go
//   - LLVM_PROFILE_FILE      = <mount>/agents/<pod>/coop-%p-%m.profraw
//
// GARDENIO_AGENT_COVERAGE=1 is the signal the agent entrypoint keys on to
// mkdir -p both parent dirs before exec — the LLVM runtime does NOT create
// them and silently drops the .profraw otherwise (images/agent/entrypoint.sh).
// The pod name is composed from the spec (identical to the pod's
// metadata.name, manager.go CreateAgentPod), so no downward-API lookup is
// needed — the value is fully known at build time. The %p/%m specifiers are
// expanded by the LLVM runtime (pid + module signature) so multiple coop
// invocations in one pod each get a distinct profraw.
func (m *K8sManager) buildCoverageEnv(spec AgentPodSpec) []corev1.EnvVar {
	if !m.Coverage.active() || spec.LandFenced {
		return nil
	}
	base := fmt.Sprintf("%s/agents/%s", m.Coverage.mountPath(), spec.PodName())
	return []corev1.EnvVar{
		{Name: "GARDENIO_AGENT_COVERAGE", Value: "1"},
		{Name: "GOCOVERDIR", Value: base + "/go"},
		{Name: "LLVM_PROFILE_FILE", Value: base + "/coop-%p-%m.profraw"},
	}
}

// GoMemLimitPercent is the share of the agent container's memory limit given to
// each Go process as GOMEMLIMIT (gd-zBUZ9bwCKl). Compiling one test binary for
// a big package is the agent pod's heaviest step: measured on a 3Gi crew pod,
// cmd/compile reached 1.77Gi on kbeads/internal/server and passed 2.27Gi on
// controller/internal/bridge with no limit. Add Claude Code (0.35-0.9Gi) and
// kernel memory and the pod is OOM-killed (exit 137). With GOMEMLIMIT=1400MiB
// those compiles peaked at 1.44Gi and 1.72Gi. The compile takes up to twice as
// long, but it finishes. The limit is soft, so a process whose live heap needs
// more memory still gets it.
const GoMemLimitPercent = 45

// goMemLimitFor returns the GOMEMLIMIT value for a container with these
// resources: GoMemLimitPercent of its memory limit, in MiB. It returns "" when
// the container has no memory limit.
func goMemLimitFor(res corev1.ResourceRequirements) string {
	limit, ok := res.Limits[corev1.ResourceMemory]
	if !ok || limit.Value() <= 0 {
		return ""
	}
	mib := limit.Value() * GoMemLimitPercent / 100 / (1 << 20)
	if mib <= 0 {
		return ""
	}
	return fmt.Sprintf("%dMiB", mib)
}

// buildResources returns resource requirements. Uses spec.Resources if provided,
// otherwise falls back to defaults.
func (m *K8sManager) buildResources(spec AgentPodSpec) corev1.ResourceRequirements {
	if spec.Resources != nil {
		return *spec.Resources
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(DefaultCPURequest),
			corev1.ResourceMemory: resource.MustParse(DefaultMemoryRequest),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(DefaultCPULimit),
			corev1.ResourceMemory: resource.MustParse(DefaultMemoryLimit),
		},
	}
}

// applyVPAMemoryStamp consults the VPA recommender (when enabled) and rewrites
// spec.Resources.Requests[memory] to the right-sized, clamped value for this
// pod's (project, role) class. It is intentionally conservative:
//
//   - It only ever touches the memory REQUEST. CPU and the memory LIMIT are
//     left exactly as resolved by the default/project/role merge — the limit
//     stays generous for burst headroom (gVisor OOMs at the limit, not the
//     request).
//   - It never lowers the request below the stamper's safe floor (clamp guards
//     this), and Recommend returns ok=false on any miss/error so the existing
//     request survives. Net effect: enabling the flag can only right-size a
//     class that already has a mature recommendation; nothing regresses to
//     under-provisioning.
//
// This is spawn-time application only — it mutates the desired spec before the
// pod is created and never resizes a running pod.
func (m *K8sManager) applyVPAMemoryStamp(ctx context.Context, spec *AgentPodSpec) {
	if !m.VPAStamper.Enabled() {
		return
	}

	// Resolve the current memory limit so the stamped request can be clamped to
	// it (request must never exceed limit). buildResources reflects the fully
	// merged defaults/overrides, so it is the source of truth here.
	resources := m.buildResources(*spec)
	var memLimit *resource.Quantity
	if l, ok := resources.Limits[corev1.ResourceMemory]; ok {
		memLimit = &l
	}

	stamped, ok := m.VPAStamper.Recommend(ctx, *spec, memLimit)
	if !ok {
		return
	}

	// Materialize spec.Resources (it may be nil if every level used defaults)
	// and overwrite only the memory request.
	if spec.Resources == nil {
		spec.Resources = &corev1.ResourceRequirements{}
	}
	if spec.Resources.Requests == nil {
		spec.Resources.Requests = corev1.ResourceList{}
	}
	if spec.Resources.Limits == nil {
		// Preserve the resolved limits so materializing Resources here doesn't
		// silently drop the default/override memory + CPU limits.
		spec.Resources.Limits = resources.Limits
	}
	if _, ok := spec.Resources.Requests[corev1.ResourceCPU]; !ok {
		// Same for the CPU request — keep the resolved value when we had to
		// build the Requests list from scratch.
		if c, has := resources.Requests[corev1.ResourceCPU]; has {
			spec.Resources.Requests[corev1.ResourceCPU] = c
		}
	}
	spec.Resources.Requests[corev1.ResourceMemory] = stamped
}

func restartPolicyForMode(mode string) corev1.RestartPolicy {
	if mode == "job" {
		return corev1.RestartPolicyNever
	}
	return corev1.RestartPolicyAlways
}

func intPtr(i int64) *int64 { return &i }
func boolPtr(b bool) *bool  { return &b }
