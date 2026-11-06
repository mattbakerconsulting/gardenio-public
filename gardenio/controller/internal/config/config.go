// Package config provides controller configuration from environment variables.
package config

import (
	"encoding/json"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Config holds controller configuration. Values come from env vars or defaults.
type Config struct {
	// --- Kubernetes ---

	// Namespace is the K8s namespace to operate in (env: NAMESPACE).
	Namespace string

	// Env is the deploy environment name (env: GARDENIO_ENV), e.g. "prod",
	// "staging", "test". It is the cross-estate coarse join key the FSM
	// correlation contract mandates (FSM_CORRELATION_CONTRACT.md §1.1) and is
	// attached to the gardenio_agent_lifecycle_events_total series so the
	// composite OOM↔ECS alert can join on it. Empty when unset; values MUST
	// match the ACME estate's env values exactly (no "production" vs "prod").
	Env string

	// KubeConfig is the path to kubeconfig file (env: KUBECONFIG).
	// Empty means use in-cluster config.
	KubeConfig string

	// --- Beads Daemon ---

	// BeadsGRPCAddr is the beads daemon gRPC address, host:port (env: BEADS_GRPC_ADDR).
	BeadsGRPCAddr string

	// BeadsHTTPAddr is the beads daemon HTTP address, host:port (env: BEADS_HTTP_ADDR).
	BeadsHTTPAddr string

	// BeadsE2EHTTPAddr is the beads daemon HTTP address for the e2e-isolated
	// namespace (env: BEADS_E2E_HTTP_ADDR). Passed to agent pods so e2e tests
	// create spawn events in an isolated beads instance.
	BeadsE2EHTTPAddr string

	// BeadsTokenSecret is the K8s secret containing the daemon auth token (env: BEADS_TOKEN_SECRET).
	// The controller reads the token value from this secret at startup for its own API calls,
	// and passes the secret name to agent pods for secretKeyRef injection.
	BeadsTokenSecret string

	// --- NATS ---

	// NatsURL is the NATS server URL for event bus (env: NATS_URL).
	// Passed to agent pods as BEADS_NATS_URL and COOP_NATS_URL.
	// Also used by the controller for subscribing to account exhaustion events.
	NatsURL string

	// NatsToken is the NATS auth token for controller-side subscriptions
	// (env: NATS_TOKEN). Read directly from the environment.
	NatsToken string

	// NatsUser is the NATS username for controller-side connections
	// (env: NATS_USER). Under nats.auth.subjectIsolation the server declares
	// multi-user auth whose entries only support user/password — the chart
	// sets NATS_USER=infra and NatsToken is sent as that user's password
	// (natsauth.Options). Empty preserves legacy bare-token auth.
	NatsUser string

	// NatsTokenSecret is the K8s secret containing the NATS auth token (env: NATS_TOKEN_SECRET).
	// W5 (ka-0000006585): projected into agent pods as a tmpfs-backed file at
	// /etc/nats-token/token; consumers read it via $COOP_NATS_TOKEN_FILE
	// instead of an env-var.
	NatsTokenSecret string

	// NatsAgentUserSecret is the K8s secret containing the NATS agent token
	// (env: NATS_AGENT_USER_SECRET). When set, takes precedence over
	// NatsTokenSecret for agent pod NATS token projection. The restricted
	// agent token limits agents to publishing on their own event subjects
	// (NATS subject isolation). The secret must have a "token" key.
	NatsAgentUserSecret string

	// --- Agent Pods ---

	// CoopImage is the default container image for agent pods (env: COOP_IMAGE).
	CoopImage string

	// InitCloneImage is the image used by the init-clone init container
	// (env: INIT_CLONE_IMAGE). When unset, the podmanager falls back to its
	// compiled-in default. Pinning this via the Helm chart lets operators
	// roll new init-clone images without re-cutting a controller release.
	InitCloneImage string

	// AgentStartupProbeFailureThreshold overrides the agent container's coop
	// StartupProbe failureThreshold (env: AGENT_STARTUP_PROBE_FAILURE_THRESHOLD).
	// The startup budget is failureThreshold × periodSeconds(5s); the compiled
	// default of 60 gives coop 300s to become ready. A slow- or contended-egress
	// cluster where cold-start work (workspace clone, credential attach) can run
	// long needs more headroom without re-cutting a controller release; pinning
	// this via the Helm chart (agents.startupProbeFailureThreshold) buys it.
	// <= 0 (unset) keeps the compiled default (ka-0000000649).
	AgentStartupProbeFailureThreshold int

	// AgentInitCloneJitterMaxSeconds staggers the init-clone COLD network clone
	// by a random 0..N seconds so pods spawned together do not all saturate NAT
	// egress at the same instant — a clone-storm (env:
	// AGENT_INIT_CLONE_JITTER_MAX_SECONDS). The sleep runs in the init container
	// BEFORE coop, so it never consumes the agent StartupProbe budget, and only
	// on a cold clone (warm-PVC pods that just fetch are unaffected). < 0 (env
	// unset) keeps the podmanager compiled default; 0 disables it; > 0 sets the
	// window. Follow-up hardening to the prod-gardenio-eks clone-storm incident
	// (ka-0000000995, follow-up to ka-0000001925).
	AgentInitCloneJitterMaxSeconds int

	// DindImage is the image used by the Docker-in-Docker sidecar (env:
	// DIND_IMAGE). When unset, the podmanager falls back to its compiled-in
	// default ("docker:dind"). Air-gap blocker P4.3 (ka-0000007177): pinning
	// this via the Helm chart (agents.dindImage) lets an air-gapped box use a
	// ref the in-box registry mirror can resolve.
	DindImage string

	// AgentPullPolicy overrides the agent-container ImagePullPolicy (env:
	// AGENT_PULL_POLICY; one of Always/IfNotPresent/Never). When unset, the
	// podmanager defaults to PullAlways. Air-gap blocker P4.3 (ka-0000007177):
	// a single-box install sets IfNotPresent so a seeded image is reused with
	// no registry round-trip on every agent spawn.
	AgentPullPolicy string

	// PrewarmedPriorityClass is the name of a low (negative) PriorityClass to
	// stamp on prewarmed/standby agent pods and warm placeholders (env:
	// PREWARMED_PRIORITY_CLASS). The Helm chart renders the `gardenio-prewarmed`
	// PriorityClass and sets this env ONLY when that class exists, so the
	// controller never references a missing class. A lower priority than real
	// (assigned) agents — which keep the cluster-default priority 0 — lets a
	// cold agent spawn preempt an idle pool member when nodes are tight, instead
	// of the parked pool starving cold spawns (ka-0000000722). Empty leaves pool
	// pods at default priority.
	PrewarmedPriorityClass string

	// InteractivePriorityClass is the name of a positive-value PriorityClass to
	// stamp on interactive-role agent pods — thread/dm/gardener (env:
	// INTERACTIVE_PRIORITY_CLASS). The Helm chart renders the
	// `gardenio-system-critical` PriorityClass (shared with the postgres/beads/
	// nats/vmsingle platform pods) and sets this env ONLY when that class
	// exists, so the controller never references a missing class. Lets a gardener
	// or human-waiting thread/dm pod preempt a batch crew pod (default priority
	// 0) when the on-demand node pool is fully packed, instead of sitting
	// Pending indefinitely behind bursty crew pods (gd-Vtnt5lAGeQ, 2026-09-22
	// gardener-concurrency-0 incident). Empty leaves interactive pods at default
	// priority (legacy behavior, same admission ordering as any other pod).
	InteractivePriorityClass string

	// ScheduledPriorityClass is the name of the PriorityClass to stamp on a
	// gardener pod that a schedule or fire-now started (env:
	// SCHEDULED_PRIORITY_CLASS). The chart renders `gardenio-scheduled` at the
	// interactive value with preemptionPolicy Never, so such a pod is queued
	// ahead of crews but never evicts one (gd-Y2KaNE233g). Empty leaves those
	// pods on InteractivePriorityClass.
	ScheduledPriorityClass string

	// AgentPriorityClass is the name of the lowest-tier PriorityClass to stamp
	// on every non-interactive, non-prewarmed agent pod (env:
	// AGENT_PRIORITY_CLASS). The Helm chart renders `gardenio-agent` (value 0,
	// preemptionPolicy Never) and sets this env ONLY when that class exists, so
	// a crew pod never evicts anything and every infra pod outranks it
	// (gd-2qjxTVMBkC). Empty leaves those pods at default priority.
	AgentPriorityClass string

	// AgentCoverageEnabled turns on runtime coverage instrumentation for
	// dynamically built agent pods (env: GARDENIO_AGENT_COVERAGE; C2 of the
	// e2e-coverage epic, ka-0000000508). When true (and a claim is wired), the
	// podmanager adds GOCOVERDIR + LLVM_PROFILE_FILE to the agent container and
	// mounts the shared coverage PVC. The Helm chart sets this env ONLY when
	// global.coverage.enabled, so a normal deploy leaves it false — a full
	// no-op. Default false.
	AgentCoverageEnabled bool

	// AgentCoverageClaim is the name of the shared ReadWriteMany coverage PVC
	// the agent pods mount when coverage is enabled (env:
	// GARDENIO_AGENT_COVERAGE_CLAIM). It embeds the Helm release name
	// ({fullname}-coverage) and so is not derivable at runtime — the chart
	// passes it explicitly. Empty disables the wiring even when Enabled (there
	// would be nowhere for the counters to land).
	AgentCoverageClaim string

	// AgentCoverageMount overrides where the coverage PVC is mounted in agent
	// pods (env: GARDENIO_AGENT_COVERAGE_MOUNT). Empty uses the podmanager's
	// compiled default (/data/coverage), which matches global.coverage.mountPath.
	// Set this only if an operator overrides that chart value so the two halves
	// (mount + GOCOVERDIR/LLVM_PROFILE_FILE root) stay in agreement.
	AgentCoverageMount string

	// ImagePullSecrets is a comma-separated list of K8s Secret names for pulling
	// private container images in agent pods (env: IMAGE_PULL_SECRETS).
	ImagePullSecrets []string

	// CoopServiceAccount is the K8s ServiceAccount to use for agent pods (env: COOP_SERVICE_ACCOUNT).
	// When set, all agent pods use this SA unless overridden by bead metadata.
	CoopServiceAccount string

	// CoopMaxPods is the maximum number of agent pods that can exist
	// simultaneously (env: COOP_MAX_PODS). 0 means unlimited. Default: 10.
	// When the limit is reached, new pods are queued until existing ones finish.
	CoopMaxPods int

	// CoopMaxPodsPerProject is the maximum number of agent pods per project
	// (env: COOP_MAX_PODS_PER_PROJECT). 0 means no per-project limit. Default: 6.
	CoopMaxPodsPerProject int

	// CoopMaxPodsGVisor is a separate concurrency budget for pods that land in
	// the gVisor namespace (cfg.GVisorNamespace) — runsc freeze/thaw agents and
	// the runsc prewarmed sub-pool (env: COOP_MAX_PODS_GVISOR). gVisor pods run
	// in their own PSA-privileged namespace but, prior to this, were counted
	// against the gardenio per-project cap (CoopMaxPodsPerProject) of the project
	// that spawned them. With that cap saturated by the thread pool + active
	// agents, a runsc warm sub-pool could never stand up even at min_size=1
	// (ka-0000004152). Counting gVisor pods against this distinct budget — never
	// the per-project cap — lets a runsc pool schedule independently. gVisor pods
	// still count toward the global CoopMaxPods ceiling. 0 means no separate
	// gVisor ceiling: gVisor pods are then bounded only by the global cap and are
	// exempt from the per-project cap. Default: 0.
	CoopMaxPodsGVisor int

	// CoopRateLimitWindow is the sliding window for pod creation rate limiting
	// (env: COOP_RATE_LIMIT_WINDOW). Default: 5m.
	CoopRateLimitWindow time.Duration

	// CoopRateLimitMax is the max pods that can be created within the rate limit
	// window before the reconciler pauses creation (env: COOP_RATE_LIMIT_MAX).
	// 0 means no rate limiting. Default: 20.
	CoopRateLimitMax int

	// CoopCircuitBreakerThreshold is the max pod creations within
	// CoopCircuitBreakerWindow before the circuit breaker trips and halts
	// all creation until cooldown elapses (env: COOP_CIRCUIT_BREAKER_THRESHOLD).
	// 0 means disabled. Default: 20.
	CoopCircuitBreakerThreshold int

	// CoopCircuitBreakerWindow is the rolling window over which creations
	// are counted for the circuit breaker (env: COOP_CIRCUIT_BREAKER_WINDOW).
	// Default: 2m.
	CoopCircuitBreakerWindow time.Duration

	// CoopCircuitBreakerCooldown is how long the circuit breaker stays open
	// after tripping before auto-resetting (env: COOP_CIRCUIT_BREAKER_COOLDOWN).
	// Default: 5m.
	CoopCircuitBreakerCooldown time.Duration

	// CoopBurstLimit is the maximum number of pods to create in a single
	// reconciliation pass (env: COOP_BURST_LIMIT). Default: 3.
	// This prevents memory pressure from simultaneous pod initialization.
	CoopBurstLimit int

	// SpawnGuardRateMax is the maximum number of agent-bead spawns in
	// SpawnGuardRateWindow, aggregated across all sources in this process
	// (env: SPAWN_GUARD_RATE_MAX). 0 disables the soft cap. Default: 30.
	SpawnGuardRateMax int

	// SpawnGuardRateWindow is the sliding window for SpawnGuardRateMax
	// (env: SPAWN_GUARD_RATE_WINDOW). Default: 1m.
	SpawnGuardRateWindow time.Duration

	// SpawnGuardTripThreshold is the hard-ceiling spawn count within
	// SpawnGuardTripWindow that trips the runaway-protection circuit breaker
	// (env: SPAWN_GUARD_TRIP_THRESHOLD). 0 disables the breaker. Default: 120.
	SpawnGuardTripThreshold int

	// SpawnGuardTripWindow is the rolling window for SpawnGuardTripThreshold
	// (env: SPAWN_GUARD_TRIP_WINDOW). Default: 1m.
	SpawnGuardTripWindow time.Duration

	// SpawnGuardTripCooldown is how long the runaway-protection breaker stays
	// open after tripping (env: SPAWN_GUARD_TRIP_COOLDOWN). Default: 5m.
	SpawnGuardTripCooldown time.Duration

	// CoopQueueExpiryTimeout is how long a bead can remain deferred (queued)
	// before the reconciler auto-closes it (env: COOP_QUEUE_EXPIRY_TIMEOUT).
	// 0 means no expiry. Default: 1h.
	CoopQueueExpiryTimeout time.Duration

	// CoopSyncInterval is how often to reconcile pod statuses with beads (env: COOP_SYNC_INTERVAL).
	// Default: 60s.
	CoopSyncInterval time.Duration

	// AgentStorageClass is the default StorageClass for agent workspace PVCs
	// (env: AGENT_STORAGE_CLASS). When set, crew-mode pods use this unless
	// overridden by a project bead's storage_class label.
	AgentStorageClass string

	// ClaudeModel is the Claude model ID for agent pods (env: CLAUDE_MODEL).
	// Injected as CLAUDE_MODEL env var. When empty, Claude Code uses its default.
	ClaudeModel string

	// AgentDefaultFableModel, when non-empty, is injected as
	// ANTHROPIC_DEFAULT_FABLE_MODEL into every agent pod (env:
	// AGENT_DEFAULT_FABLE_MODEL, default "claude-fable-5-1-oauth"). It fixes the
	// Claude Code harness `fable` alias, which otherwise resolves — via its
	// bundled model catalog — to a bare "claude-sonnet-5" id that the fleet's
	// LiteLLM virtual-key ACL rejects (401 "key not allowed to access model …
	// Tried to access claude-sonnet-5"), so `--model=fable`, Agent-tool
	// model:"fable", and any fable-pinned crew/formula are dead on arrival
	// (ka-0000003175). The harness reads ANTHROPIC_DEFAULT_FABLE_MODEL first when
	// resolving the alias (and it also flips the fable-availability gate true), so
	// pinning it to the ACL-allowed "-oauth" model GROUP that LiteLLM actually
	// serves routes fable requests correctly. Must be the "-oauth" group, not the
	// bare "claude-fable-5-1" alias key: the virtual-key ACL is checked against the
	// LITERAL request model name BEFORE model_group_alias resolution. Set empty in
	// an overlay to disable injection (escape hatch when the harness default is
	// already correct). The default tracks the CURRENT fable tier: Claude Fable
	// 5.1 (claude-fable-5-1, launched 2026-09-01) replaced fable-5 as every
	// `fable` selector's target in ka-0000002744; claude-fable-5-oauth stays a
	// defined LiteLLM group, so rollback is a config flip.
	AgentDefaultFableModel string

	// AgentDefaultOpusModel, when non-empty, is injected as
	// ANTHROPIC_DEFAULT_OPUS_MODEL into every agent pod (env:
	// AGENT_DEFAULT_OPUS_MODEL, default "claude-opus-5-5-oauth"). It is the
	// fleet's default-Opus knob: the harness `opus` alias resolves through it
	// to an ACL-allowed LiteLLM group. Empty disables injection.
	AgentDefaultOpusModel string

	// AgentDefaultHaikuModel, when non-empty, is injected as
	// ANTHROPIC_DEFAULT_HAIKU_MODEL into every agent pod (env:
	// AGENT_DEFAULT_HAIKU_MODEL, default "anthropic/claude-haiku-4-5"). It fixes
	// the Claude Code harness `haiku` alias, which otherwise resolves — via its
	// bundled model catalog — to the DATED snapshot id "claude-haiku-4-5-20251001"
	// that the fleet's LiteLLM virtual-key ACL rejects (401 "key not allowed to
	// access model … Tried to access claude-haiku-4-5-20251001"), so
	// `--model=haiku`, Agent-tool model:"haiku", and any haiku-pinned crew/formula
	// are dead on arrival — and, because haiku is the harness's small-model for
	// WebFetch and similar ops, those paths 401 silently too (ka-0000005599). The
	// harness reads ANTHROPIC_DEFAULT_HAIKU_MODEL first when resolving the alias,
	// so pinning it to the ACL-allowed model GROUP that LiteLLM actually serves
	// routes haiku requests correctly. Must be "anthropic/claude-haiku-4-5" (the
	// rolling pass-through group in the ACL / litellm model_list), NOT the dated
	// "…-20251001" snapshot the harness picks on its own: the virtual-key ACL is
	// checked against the LITERAL request model name BEFORE model_group_alias
	// resolution. (Unlike the fable pin's "-oauth" group, the haiku group carries
	// the "anthropic/" pass-through prefix — mirror the ACL literal, not fable's
	// shape.) Set empty in an overlay to disable injection (escape hatch when the
	// harness default is already correct).
	AgentDefaultHaikuModel string

	// DefaultHarness is the agent CLI used when no agent/role/project sets a
	// harness (env: AGENT_DEFAULT_HARNESS; "claude", "codex" or "gemini"). Defaults to
	// "claude" so existing roles are unchanged. An invalid value fails every
	// spawn that falls through to it, with a readable error.
	DefaultHarness string

	// GeminiHarnessEnabled lets a spawn resolve to the gemini harness (env:
	// GEMINI_HARNESS_ENABLED, default false; ADR 0005 §7). The gemini value is
	// accepted by the enum and validation from phase 1, but the config, hooks,
	// stop gate and coop driver that make a Gemini pod work land in phases 2-3,
	// so until this is turned on a spawn that resolves to gemini is refused with
	// a readable reason instead of booting a pod that cannot work.
	GeminiHarnessEnabled bool

	// AgentVmagentURL, when non-empty, is injected as VMAGENT_URL into every
	// agent pod (env: AGENT_VMAGENT_URL). It is the cluster-internal vmagent
	// ingest URL (e.g. "http://gardenio-vmagent:8429"). gb prime, gb session, and
	// the session-resume check all gate their metric pushes on VMAGENT_URL; the
	// controller never set it on agent pods before, so those series never landed
	// in VictoriaMetrics (epic ka-0000003733, Part B). When empty the agent pod
	// gets no VMAGENT_URL and the pushes stay silently skipped — today's behavior.
	AgentVmagentURL string

	// ClaudeTeamsEnabled enables Claude Code Agent Teams mode for agent pods
	// (env: CLAUDE_TEAMS_ENABLED). When true, the team lead session can spawn
	// and coordinate teammate sessions within the same pod.
	ClaudeTeamsEnabled bool

	// ClaudeTeammateMode is the teammate display mode (env: CLAUDE_TEAMMATE_MODE).
	// Valid values: "tmux" (each teammate in a tmux pane) or "in-process".
	// Default: "tmux".
	ClaudeTeammateMode string

	// ClaudeTeamsMaxTeammates is the maximum number of teammate sessions per pod
	// (env: CLAUDE_TEAMS_MAX_TEAMMATES). 0 means use Claude Code's built-in default.
	ClaudeTeamsMaxTeammates int

	// ClaudeTeams resource overrides: when teams mode is enabled, agent pods
	// need more memory and CPU because each teammate runs its own Claude Code
	// session (Node.js process + context window). These override the default
	// agent pod resources only when ClaudeTeamsEnabled is true.
	ClaudeTeamsCPURequest    string // env: CLAUDE_TEAMS_CPU_REQUEST
	ClaudeTeamsCPULimit      string // env: CLAUDE_TEAMS_CPU_LIMIT
	ClaudeTeamsMemoryRequest string // env: CLAUDE_TEAMS_MEMORY_REQUEST
	ClaudeTeamsMemoryLimit   string // env: CLAUDE_TEAMS_MEMORY_LIMIT

	// --- Channel Delivery ---

	// GardenioChannel enables MCP channel-based nudge delivery for agent pods
	// (env: GARDENIO_CHANNEL). When true, agent pods start the gardenio-channel
	// MCP server for reliable message delivery via Claude Code's channels protocol.
	GardenioChannel bool

	// PerAgentTokens enables per-agent scoped beads daemon tokens
	// (env: PER_AGENT_TOKENS). When true, each agent gets a unique
	// fleet token scoped to its project instead of sharing the global
	// BEADS_DAEMON_TOKEN. This prevents cross-project data access
	// if an agent token is leaked.
	PerAgentTokens bool

	// LandsTenantSpawns lets tenant lands spawn agents at all
	// (env: LANDS_TENANT_SPAWNS, lands.tenantSpawns; default off). Off, the
	// Lands spawn wall (internal/lands/wall) refuses every tenant spawn;
	// operator lands are unaffected. It stays off until Phase 10's fences ship
	// (Lands Track B spec §3.3 step 5).
	LandsTenantSpawns bool

	// LandsSpawnsPerHour caps how many agents one tenant land may start in
	// any hour (env: LANDS_SPAWNS_PER_HOUR, default 12; 0 turns the cap off).
	// Past it the Lands spawn wall refuses with spawn_rate until the oldest
	// spawn leaves the window (Track B spec §5.4, step 10.4). Operator lands
	// are unaffected.
	LandsSpawnsPerHour int

	// LandsRuntimeClass is the RuntimeClass the land fence puts every tenant
	// land pod under (env: LANDS_RUNTIME_CLASS, landNamespaces.runtimeClassName).
	// Empty, the default, gives tenant pods no runtimeClassName: the node's
	// default runtime (runc; owner ruling 2026-11-01, gd-iyZom5u11A). "runsc"
	// runs them under gVisor. The land-pods admission policy holds the same
	// value.
	LandsRuntimeClass string

	// LandsBridgeURL is the slack-bridge's land listener (env:
	// LANDS_BRIDGE_URL), which serves only the per-agent-token routes gb say
	// and gb thread use. The land fence gives a tenant pod this as
	// SLACK_BRIDGE_URL; empty, a tenant pod gets no bridge URL, as its land's
	// NetworkPolicy reaches only that listener (gd-pXik57wZA9).
	LandsBridgeURL string

	// GithubAppEnabled turns on per-agent GitHub App installation tokens
	// (env: GITHUB_APP_ENABLED, default false; epic gd-BHLDnjX1Hw). When true,
	// every agent whose project git URL is on github.com gets a token minted at
	// spawn, scoped to its repo(s), and the spawn is REFUSED if minting fails
	// (fail closed — no fallback to the shared git credential).
	GithubAppEnabled bool
	// GithubAppID is the numeric App id (env: GITHUB_APP_ID).
	GithubAppID string
	// GithubAppInstallationID pins the installation (env:
	// GITHUB_APP_INSTALLATION_ID). 0 = look it up per repo via the App JWT.
	GithubAppInstallationID int64
	// GithubAppPrivateKeyFile is the path of the mounted PEM private key (env:
	// GITHUB_APP_PRIVATE_KEY_FILE). A FILE, never an env var, so the key cannot
	// leak via /proc/<pid>/environ or `kubectl describe`.
	GithubAppPrivateKeyFile string
	// GithubAppAPIURL overrides the GitHub API base URL (env:
	// GITHUB_APP_API_URL; GHES / tests). Empty = https://api.github.com.
	GithubAppAPIURL string

	// FreezeThawEnabled opts the controller into the gVisor freeze/thaw agent
	// lifecycle (env: FREEZE_THAW_ENABLED, default false; epic ka-0000007417,
	// task ka-0000003688). When true, agents pinned to the runsc RuntimeClass
	// (podmanager.RuntimeClassRunsc) are checkpointed via
	// FreezeThawManager.Freeze on graceful stop/dehydrate and restored via
	// .Thaw on spawn/wake instead of going through the cold-start lifecycle.
	// Non-runsc agents are never touched. When false the verbs have no callers
	// and every agent uses today's create/delete lifecycle — this is the
	// rollback switch (epic success criterion 5: disable flag → cold-start
	// fallback). Freeze/thaw failures also fall back to the normal path so a
	// flipped flag degrades gracefully rather than wedging an agent.
	FreezeThawEnabled bool

	// SnapshotS3Bucket names the S3 bucket the OFF-NODE SnapshotTransport
	// (podmanager.S3SnapshotTransport, Δ5-B2 ka-0000006160) uploads the heavy
	// checkpoint image + OCI bundle to at freeze and pulls back at thaw, so a
	// frozen agent can thaw on a DIFFERENT node than it froze on (env:
	// FREEZE_SNAPSHOT_S3_BUCKET). EMPTY (default) leaves the node-local no-op
	// transport installed — freeze/thaw stay pinned to the origin node exactly as
	// in B1, and an off-node thaw degrades to a cold start. This is the merge-safe
	// zero-blast-radius default: landing the transport changes nothing in prod
	// until an operator names the bucket AND the terraform IRSA grant + bucket
	// lifecycle are applied. Reuses CoopS3Region/CoopS3Endpoint for the S3 client
	// (region/path-style endpoint), the same way the rest of the controller does.
	SnapshotS3Bucket string

	// SnapshotS3Prefix is the key prefix under which snapshot payloads live
	// (env: FREEZE_SNAPSHOT_S3_PREFIX, default podmanager.DefaultSnapshotS3Prefix
	// = "ftsnap"). The terraform S3 lifecycle-expiry rule and the IRSA policy
	// resource ARN must target this same prefix.
	SnapshotS3Prefix string

	// FreezeThawRoleAllowlist narrows freeze-eligibility to a canary set of
	// agent roles (env: FREEZE_THAW_ROLE_ALLOWLIST, comma-separated; default
	// empty; WS3 widening task ka-0000000138, epic ka-0000004429). It exists
	// because freeze-eligibility is otherwise a GLOBAL predicate —
	// FreezeThawEnabled AND runtime_class==runsc, with no per-role dimension
	// (see isFreezeEligible / shouldFreeze). Prod already runs
	// FreezeThawEnabled=true for the gvisor-poc canary, so the instant a
	// second, already-populated role (plan-gardener, thread) is pinned to runsc,
	// EVERY instance of it would freeze at once — there is no "route this role
	// onto runsc but keep it cold-start first" step. That is exactly the
	// widen-one-role-at-a-time footgun the rollout must avoid.
	//
	// Semantics:
	//   - EMPTY (default): NO role filter. Freeze-eligibility stays exactly as
	//     before — any runsc agent is eligible when FreezeThawEnabled. This is
	//     the zero-blast-radius default that makes the flag merge-safe.
	//   - NON-EMPTY: an agent is freeze-eligible ONLY if its role is in this
	//     set (AND it is runsc AND FreezeThawEnabled). A runsc agent whose role
	//     is not listed keeps today's cold-start lifecycle — it schedules on the
	//     gVisor pool but is never checkpointed. This lets the operator opt the
	//     gvisor-poc canary + one real role (e.g. plan-gardener) into freezing
	//     while every other runsc role stays cold, then widen the list one role
	//     at a time. ROLLBACK: shrink/clear the list (env change, no code
	//     redeploy) → the removed role reverts to cold-start on its next stop.
	//
	// Applied at BOTH freeze seams so the in-pod `gb stop` suspend decision
	// (spawn-time BOAT_FREEZE_ELIGIBLE) and the controller's own shouldFreeze
	// gate never drift.
	FreezeThawRoleAllowlist []string

	// PilotFreezeClass names the SINGLE opt-in agent role selected for the
	// bounded gVisor freeze/thaw PRODUCTION PILOT (env: PILOT_FREEZE_CLASS,
	// default empty; epic ka-0000006277 step 5, task ka-0000006164). The pilot
	// selects ONE agent class that most benefits from retained working memory —
	// a long-lived, HITL-heavy gardener (the chosen class is "plan-gardener": it
	// cold-boots on every cron fire today and cold-replays after a `gb decision`
	// yield, so cross-fire working memory is the headline win). Thread agents,
	// though HITL-heavy, run on the microvm plane (SuspendMicrovm, plan
	// ka-0000004689) — a different execution target — so they are NOT candidates
	// for this pod-plane runsc pilot.
	//
	// This is the SINGLE feature flag that scopes the pilot's blast radius to one
	// class. It is deliberately a superset knob over the two lower-level dials
	// (per-role RuntimeClass pinning + FreezeThawRoleAllowlist): setting it opts
	// the named class fully in with ONE env var, so the operator does not have to
	// coordinate a role-bead RuntimeClass edit AND an allowlist entry. When it
	// matches an agent's role AND FreezeThawEnabled is on, the spawn path
	// (applyPilotFreezeClass) pins that class's pods to the runsc RuntimeClass —
	// which routes them onto the dedicated gVisor node pool, into GVisorNamespace,
	// and marks them freeze-eligible (isFreezeEligible / markFreezeEligible). The
	// class is also always freeze-ALLOWED (FreezeRoleAllowed) so the single flag
	// is self-contained even when a non-empty FreezeThawRoleAllowlist is in force
	// for other roles.
	//
	// Semantics:
	//   - EMPTY (default): pilot OFF. No class is routed onto runsc by this flag;
	//     every role keeps whatever project/role/agent RuntimeClass resolved and
	//     the default runc path is unchanged. This is the merge-safe, zero-blast-
	//     radius default — landing the code changes nothing in prod until an
	//     operator names the class.
	//   - NON-EMPTY (e.g. "plan-gardener"): ONLY that one role is force-pinned to
	//     runsc + freeze-eligible; all other roles are untouched and stay on runc.
	//
	// ROLLBACK / OPT-OUT (epic success criterion 5): clear PILOT_FREEZE_CLASS (an
	// env change, no code redeploy) — the pilot class reverts to whatever
	// project/role resolved (runc) and to today's stateless-restartable cold-start
	// lifecycle on its NEXT spawn. Nothing is stranded: the flag only sets the
	// spawn-time RuntimeClass, an already-running pod is unaffected until it next
	// spawns, and the controller's own shouldFreeze gate re-reads the LIVE pod's
	// RuntimeClass — so a pilot-class pod that started under the flag but whose
	// flag was since cleared simply cold-starts on its next stop rather than
	// freezing. Clearing FreezeThawEnabled is the coarser second rollback switch:
	// the pilot pin is gated on it, so either lever fully reverts the pilot.
	//
	// Precedence: applied after project/role RuntimeClass resolution but BEFORE
	// the per-agent runtime_class metadata override, so an explicit per-agent pin
	// still wins (project < role < pilot < agent).
	PilotFreezeClass string

	// FreezeInterruptEnabled opts the controller into the interruptible-freeze
	// abort path (env: FREEZE_INTERRUPT_ENABLED, default false; Phase 6 epic
	// ka-0000008235, task ka-0000005834 "C2"). When true AND FreezeThawEnabled is
	// also on, a freeze in flight when a user message arrives for that agent is
	// ABORTED: the controller cancels the checkpoint exec, discards the partial
	// (or just-persisted) snapshot, and leaves the still-live Pod A (`runsc
	// checkpoint --leave-running` keeps it running) to serve the message — the
	// <10s safety net. When false the freeze always runs to completion exactly as
	// before (the in-flight registry is never populated, AbortFreeze is a no-op):
	// this is the rollback switch (flip-off = the abort seam disappears, zero
	// blast radius). Only meaningful when FreezeThawEnabled is also true — on its
	// own there is no freeze path to interrupt.
	FreezeInterruptEnabled bool

	// FreezePostPutDelay artificially widens the window between a successful
	// snapshot persist and the abort-verdict read inside Freeze (env:
	// FREEZE_POST_PUT_DELAY, default 0; TOCTOU e2e test seam, task
	// ka-0000000996). On the real substrate the post-Put window is microseconds
	// wide, so the abort-races-completion e2e cannot reliably land an interrupt
	// inside it; a positive delay holds the freeze just-persisted long enough for
	// the test to observe the snapshot ConfigMap, fire POST
	// /api/v1/freeze/interrupt, and prove the discard path on live infra. Default
	// 0 is today's behaviour verbatim — NEVER set in production. Only meaningful
	// when FreezeInterruptEnabled (and so FreezeThawEnabled) is also true.
	FreezePostPutDelay time.Duration

	// FreezeArmedGuardEnabled opts the controller into the pre-freeze
	// watcher-liveness guard (env: FREEZE_ARMED_GUARD_ENABLED, default false; bug
	// ka-0000006667). When true AND FreezeThawEnabled is also on, Freeze refuses
	// to checkpoint a runsc pod whose in-pod rehydration watchers (coop + gb)
	// have not yet armed their thaw-state sentinels — a snapshot taken before the
	// watchers are live has no consumer to ack the post-thaw rehydration gate, so
	// it would fail-close to a cold start anyway. Skipping the freeze yields the
	// same cold start minus a wasted 3.0G checkpoint and a guaranteed-doomed thaw.
	// Fail-safe: a node-probe error proceeds with the freeze (today's behaviour).
	// Default-off is the rollback switch; only meaningful when FreezeThawEnabled
	// is also true.
	FreezeArmedGuardEnabled bool

	// FreezeArmedWait is the bounded window the pre-freeze armed guard WAITS for
	// the in-pod rehydration watchers to arm before deciding (env:
	// FREEZE_ARMED_WAIT, default 0; bug ka-0000006667 direction [A]). Only
	// consulted when FreezeArmedGuardEnabled is also on. Zero (default) keeps the
	// single-shot probe: if the sentinels are absent the instant the freeze is
	// requested, Freeze refuses immediately. A positive value turns the guard
	// into a deterministic gate — Freeze polls the sentinels until every required
	// consumer (coop + gb) has armed, then checkpoints a provably-thawable
	// snapshot whose post-thaw rehydration ack is deterministic; or the window
	// elapses, and it refuses with ErrFreezeWatchersNotArmed exactly as the
	// single-shot probe would have. A runsc pod is k8s-health-ready well before
	// its agent loop finishes booting and arms; waiting out that window is what
	// makes the freeze gate on watcher liveness (proof the gb/coop watchers are
	// live) rather than racing the checkpoint against a slow agent boot. Only
	// meaningful when FreezeArmedGuardEnabled (and so FreezeThawEnabled) is true.
	FreezeArmedWait time.Duration

	// ThawRehydrateDeadline bounds the post-restore credential/NATS rehydration
	// hand-off (env: THAW_REHYDRATE_DEADLINE; default
	// podmanager.DefaultRehydrateDeadline). After a verified thaw the controller
	// writes a thaw-epoch marker and waits for the agent's gb+coop acks within
	// this budget; if either consumer fails to ack OK in time the thaw
	// fails-closed (rehydration-design C5, ka-0000004017) and the agent is not
	// marked live on a half-rehydrated identity. Zero defers to the manager's
	// built-in default.
	ThawRehydrateDeadline time.Duration

	// PostThawTokenRefreshEnabled opts the controller into the post-thaw
	// credential refresher (bug ka-0000000518; env:
	// POST_THAW_TOKEN_REFRESH_ENABLED, default false). A thawed agent's SA/IRSA
	// token warm copy is a static snapshot outside kubelet rotation (thaw step
	// [4] runsc-deletes Pod B so the kubelet stops rotating), so ~1h post-thaw the
	// overlaid token expires and the agent's k8s/AWS calls 401. When true (and
	// FreezeThawEnabled), a periodic loop re-mints each live thawed agent's tokens
	// via the k8s TokenRequest API and re-overlays them into the warm copy.
	// Default-off is the rollback switch (zero blast radius: the loop never runs,
	// no extra RBAC is exercised); only meaningful when FreezeThawEnabled is also
	// true. Requires the serviceaccounts/token create grant the helm chart adds
	// under the same guard.
	PostThawTokenRefreshEnabled bool

	// PostThawTokenRefreshInterval is how often the post-thaw token refresher runs
	// (env: POST_THAW_TOKEN_REFRESH_INTERVAL, default 30m). It must be comfortably
	// below the projected-token TTL (~1h) so each thawed agent is re-minted well
	// before its current token expires. Only consulted when
	// PostThawTokenRefreshEnabled is true.
	PostThawTokenRefreshInterval time.Duration

	// ThawCreateRestorePodReadyTimeout bounds the Ready-wait for a FRESH
	// create-then-restore placeholder Pod B (env:
	// THAW_CREATE_RESTORE_POD_READY_TIMEOUT; default 0 →
	// podmanager.DefaultCreateRestorePodReadyTimeout). The warm-B-MISS fallback
	// mints Pod B and re-runs the cold init-clone of the workspace volume inside
	// the readiness window (the gVisor checkpoint restores the agent rootfs
	// overlay but NOT the separate VolumeWorkspace repo mount). A repo-heavy cold
	// clone (~2m50s on prod) over the 180s default Pod-Ready budget made the
	// create-path thaw time out and cold-start before the runsc restore ran
	// (ka-0000004449). This scopes a generous clone-aware budget to that one seam
	// without touching the 180s budget the warm-B/teardown waits use. Zero defers
	// to the manager's built-in default. Only meaningful when FreezeThawEnabled.
	ThawCreateRestorePodReadyTimeout time.Duration

	// ThawHardDeadline is the safety cap on the DETACHED post-commit critical
	// section of a thaw — barrier restore + rehydration hand-off + image reap
	// (env: THAW_HARD_DEADLINE; default 0 → manager computes runscTimeout +
	// rehydrateDeadline + margin). Once Pod B is committed, that section runs on a
	// ctx decoupled from the inbound (leader-election) ctx's cancellation so a
	// controller leader-change / pod roll mid-thaw cannot abort an in-flight
	// rehydrate before its real deadline (the ka-0000004088 ~6.8s "context
	// canceled" confound) or cancel the Pod B teardown (orphaned on-node runsc
	// containers). This caps that detached section so a wedged node exec can never
	// wedge the goroutine forever. Only meaningful when FreezeThawEnabled is true.
	ThawHardDeadline time.Duration

	// FreezeHardDeadline is the safety cap on the whole FREEZE node exec — runsc
	// checkpoint + Step-3 relocation copy + pre-checkpoint gofer/rootfs copies
	// (env: FREEZE_HARD_DEADLINE; default 0 → manager computes runscTimeout + a
	// per-container relocation copy budget sized to the pod's container count, so
	// multi-sidecar agents get proportional headroom instead of a flat margin —
	// ka-0000007927). Every runsc call is already RUNSC_TIMEOUT-bounded and the
	// read-only image probes are HMNTT-bounded, but the MUTATING host-ns copies are
	// left unbounded on the node (bounding a mutating copy risks a partial-copy
	// un-thawable snapshot). This cap makes a wedge in any unbounded node op surface
	// as a freeze-exec timeout so Freeze() fails closed (no snapshot persisted, Pod
	// A left live, next spawn cold-starts) instead of hanging the freeze goroutine
	// (bug ka-0000004931). A non-zero value is an unconditional, count-independent
	// operator override. Only meaningful when FreezeThawEnabled is true.
	FreezeHardDeadline time.Duration

	// FreezeHardDeadlineInvalid reports that FREEZE_HARD_DEADLINE was SET to a
	// value that will NOT take effect as an override — it failed to parse (env
	// parsing silently drops it to the 0 "compute default" sentinel) or parsed to
	// a non-positive duration (which the manager's `> 0` override guard treats as
	// "unset"). Purely diagnostic: the controller echoes it at startup so a
	// fat-fingered override (e.g. "5" without a unit, or "-4m") is visible instead
	// of silently reverting to the computed cap. It never changes the effective
	// deadline — FreezeHardDeadline already carries whatever the value parses to
	// (task ka-0000006783).
	FreezeHardDeadlineInvalid bool

	// ThawDrainTimeout bounds how long graceful shutdown (SIGTERM / leader-loss)
	// waits for in-flight thaws' detached critical sections to finish before the
	// controller process exits (env: THAW_DRAIN_TIMEOUT; default 90s). The
	// terminationGracePeriodSeconds on the controller Deployment MUST exceed this
	// or the kubelet SIGKILLs the process mid-drain, re-introducing the abandoned-
	// restore failure this hardening closes (ka-0000001355). Zero disables the
	// wait (exit immediately on shutdown — pre-hardening behaviour). Only
	// meaningful when FreezeThawEnabled is true.
	ThawDrainTimeout time.Duration

	// GVisorNamespace is the K8s namespace that runsc (gVisor) freeze/thaw
	// agents and the per-node gvisor-probe pods live in (env: GVISOR_NAMESPACE,
	// default empty). Empty disables the split: runsc agents and the probe
	// executor fall back to cfg.Namespace, preserving today's single-namespace
	// behaviour (bug ka-0000004301, epic ka-0000008224).
	//
	// Why a separate namespace: a runsc agent pod carries a thaw-state hostPath
	// (volumes.go:177) and the freeze/thaw node-exec runs against a privileged
	// hostPID/hostPath gvisor-probe pod. Both are forbidden by PodSecurity
	// `baseline`, which the main `gardenio` namespace enforces — and managed EKS
	// does not expose the apiserver PodSecurity AdmissionConfiguration, so a
	// cluster-level `exemptions.runtimeClasses: [runsc]` is not available.
	// Routing runsc agents into a dedicated PSA-privileged namespace is the only
	// way to admit them without relaxing the security posture of the main
	// namespace where every other agent runs.
	//
	// When set, the controller (1) routes a runtime_class=runsc agent pod into
	// this namespace, (2) points the freeze/thaw ProbeNodeExecutor at it, and
	// (3) extends the status-reporter pod sweep to include it so the runsc
	// agent's bead still receives phase updates. Non-runsc agents are never
	// affected. Only meaningful when FreezeThawEnabled is also true; on its own
	// it just relocates runsc agents.
	GVisorNamespace string

	// VPAMemoryStampEnabled turns on reco-only VPA memory right-sizing at spawn
	// (env: VPA_MEMORY_STAMP_ENABLED, default false — rollback switch,
	// ka-0000004606). When true the controller reads the VPA recommendation for
	// each crew pod's (project, role) class and stamps a clamped memory request
	// at CREATE. When false the request stays at its default/project/role value.
	// This is recommendation-only application; it never installs a mutating
	// webhook and never resizes a running pod (in-place is gated separately,
	// ka-0000003138).
	VPAMemoryStampEnabled bool

	// VPAObjectNameTemplate is the VPA object-name pattern used for the
	// recommendation lookup, with {project} and {role} placeholders (env:
	// VPA_OBJECT_NAME_TEMPLATE). Empty uses the single live recommender object
	// (gardenio-agents-reco). This is the seam matching the scoping crew's
	// VPA-object-naming contract (ka-0000001210) — flip to a per-class template
	// once per-class objects land, with no code change.
	VPAObjectNameTemplate string

	// VPAMemoryFloor / VPAMemoryMax bound the stamped memory request (env:
	// VPA_MEMORY_FLOOR, VPA_MEMORY_MAX; Kubernetes quantity strings). Empty uses
	// the built-in safe defaults (1500Mi floor, 16Gi max). The floor guarantees
	// a freshly-stamped pod is never under-provisioned below a safe value even
	// if the recommendation is immature/low.
	VPAMemoryFloor string
	VPAMemoryMax   string

	// WarmPlaceholderPoolEnabled opts the controller into the warm runsc
	// placeholder ("warm-B") pool (env: WARM_PLACEHOLDER_POOL_ENABLED, default
	// false; Phase 5 sub-second resume, epic ka-0000002590, task ka-0000006550,
	// plan doc C5). When true, the poolmanager maintains a per-node pool of
	// already-Ready runsc placeholder pods (the buildPlaceholderPodB shape) so a
	// thaw can graft its checkpoint onto a warm placeholder instead of paying the
	// Create + waitPodReady kubelet-admit cost on the hot path (the only
	// multi-second step), landing total resume at restore-cost (~300ms) +
	// rehydrate. When false the pool is never created and Thaw keeps today's
	// create-then-restore path — this is the rollback switch (flip-off =
	// zero blast radius). Only meaningful when FreezeThawEnabled is also true: the
	// pool exists to serve thaws, and a runsc placeholder cannot schedule without
	// the gVisor substrate. On its own it does nothing.
	WarmPlaceholderPoolEnabled bool

	// WarmPlaceholderPoolMinSize is the target number of Ready runsc placeholder
	// pods the warm-B pool keeps per gVisor node (env:
	// WARM_PLACEHOLDER_POOL_MIN_SIZE, default 0). The reconciler creates
	// placeholders up to this count per node and refills after each thaw claim.
	// Zero (the default) keeps the pool empty even when
	// WarmPlaceholderPoolEnabled is true — set a positive value to actually
	// pre-provision warm placeholders. Per-node because a snapshot's checkpoint
	// image is node-local (Snapshot.Node), so a warm placeholder can only serve a
	// thaw whose snapshot lives on the same node. Only meaningful when
	// WarmPlaceholderPoolEnabled is also true.
	WarmPlaceholderPoolMinSize int

	// WarmPlaceholderStaleDrainRate caps how many not-in-use stale-image warm
	// placeholders the pool cycles per reconcile pass on a controller upgrade
	// (env: WARM_PLACEHOLDER_STALE_DRAIN_RATE, default 0 → poolmanager's
	// DefaultStaleDrainRate). A controller upgrade leaves idle warm pods running
	// the OLD agent image; the pool recycles them so a later thaw never grafts
	// onto a stale-image sandbox (epic ka-0000002868). This bounds the per-pass
	// blast radius so a fleet-wide roll converges over several passes instead of
	// nuking the pool at once. Only meaningful when WarmPlaceholderPoolEnabled.
	WarmPlaceholderStaleDrainRate int

	// WarmPlaceholderNodeFloor is the number of cheap legacy (blind bare-/work)
	// warm placeholders the pool keeps on EVERY gVisor node, IN ADDITION to the
	// per-(node,topology) snapshot buckets (env: WARM_PLACEHOLDER_NODE_FLOOR,
	// default 0 → off, today's behavior). It exists to keep a warm gVisor node
	// always present (ka-0000000239 / WS1): the snapshot-topology fill only warms
	// nodes that already hold a snapshot, so a gVisor node with no snapshot drops
	// to zero warm pods, goes Karpenter-`WhenEmpty`, and is consolidated — leaving
	// the next thaw to pay cold node provisioning (the >10s path). A small
	// per-node floor of cheap (100m) sleep placeholders keeps every gVisor node
	// non-empty so Karpenter never scales the warm substrate to zero, and gives
	// new/no-snapshot runsc agents a co-resident warm pod to claim. These floor
	// placeholders are unioned with (not a fallback for) the snapshot buckets, so
	// concurrent-thaw headroom (WarmPlaceholderPoolMinSize per topology) is
	// unaffected. Zero keeps the legacy-only-as-fallback behavior. Only meaningful
	// when WarmPlaceholderPoolEnabled.
	WarmPlaceholderNodeFloor int

	// WarmPlaceholderMaxConcurrent caps the TOTAL number of live warm placeholder
	// pods the pool may hold across ALL (node,topology) buckets at once (env:
	// WARM_PLACEHOLDER_MAX_CONCURRENT, default 0 → poolmanager's
	// DefaultMaxConcurrent of 10). Each warm placeholder is an ephemeral runsc pod
	// that holds real node capacity, so an unbounded per-bucket fill across many
	// buckets/nodes can mint far more ephemeral pods than the cluster's ephemeral
	// budget — the exact footprint that exhausted the on-demand vCPU quota with 223
	// leaked stacks (ka-0000004129). The cap bounds the pool's global footprint:
	// after a thaw consumes a member, replenishment refills toward MinSize only up
	// to this ceiling, so depth is maintained without ever exceeding the concurrent
	// cap. Only meaningful when WarmPlaceholderPoolEnabled.
	WarmPlaceholderMaxConcurrent int

	// --- Secrets & Credentials ---
	//
	// Per-project secrets (Claude OAuth, git credentials, GitHub/GitLab tokens,
	// Mezmo key, AWS credentials, etc.) are declared on project beads
	// via the "secrets" JSON field and auto-wired by applyCommonConfig +
	// secretreconciler. Only controller-level secrets remain here.

	// ClaudeOAuthSecret is the K8s secret containing Claude OAuth credentials (env: CLAUDE_OAUTH_SECRET).
	// Mounted as ~/.claude/.credentials.json in agent pods for Max/Corp accounts.
	// This is a volume mount (not SecretEnv), so it cannot be handled by config beads.
	ClaudeOAuthSecret string

	// SlackViewSessionSecret is the K8s Secret (key storage-state.json) holding
	// the Slack test user's Playwright storageState (env:
	// SLACK_VIEW_SESSION_SECRET). Empty disables the mount. podspec.go projects it
	// as an OPTIONAL volume, so a Secret that does not exist yet never blocks pod
	// creation; the agent entrypoint exports SLACK_VIEW_STORAGE_STATE only when
	// the file is actually present.
	SlackViewSessionSecret string

	// SlackViewSessionRoles is the comma-separated list of agent roles that get
	// the Slack viewer session mounted (env: SLACK_VIEW_SESSION_ROLES, default
	// "thread"). The session is a real login for the test workspace, so it
	// is not mounted into every pod.
	SlackViewSessionRoles string

	// AgentMachineClientMounts lists the machine client ids a role bead's
	// machine_client may mount into its pods (env:
	// GARDENIO_AGENT_MACHINE_CLIENT_MOUNTS, comma-separated, chart value
	// agents.machineClientMounts). A role naming any other id gets no mount.
	// Escalation routing Task 15, C5.
	AgentMachineClientMounts []string

	// AgentOIDCDiscoveryURL and AgentOIDCCASecret reach a machine-client pod
	// as GARDENIO_OIDC_DISCOVERY_URL and, through a mounted ca.crt,
	// GARDENIO_OIDC_CA_FILE: agent pods cannot dial the public edge serving
	// the issuer, so --as-machine discovers it at the in-cluster Keycloak
	// Service (env GARDENIO_AGENT_OIDC_DISCOVERY_URL, and
	// GARDENIO_AGENT_OIDC_CA_SECRET naming the Secret with key ca.crt).
	AgentOIDCDiscoveryURL string
	AgentOIDCCASecret     string

	// VerifierServiceAccount is the read-only ServiceAccount a role bead's
	// verifier=true runs its pods as, with the API token mounted (env:
	// GARDENIO_VERIFIER_SERVICE_ACCOUNT, set by the chart when
	// agents.verifier.enabled renders that SA). Empty: verifier is ignored, so
	// a role can carry the flag before the SA exists. P9.2, gd-6ujfeTZfNL.
	VerifierServiceAccount string

	// --- S3 Session Persistence ---

	// CoopS3Bucket is the S3 bucket for coop session artifact persistence
	// (env: COOP_S3_BUCKET). When set, agent pods upload transcripts,
	// recordings, and session logs to S3 for cross-pod recall.
	CoopS3Bucket string

	// CoopS3Prefix is the S3 key prefix for session artifacts (env: COOP_S3_PREFIX).
	// Default (in coop): "coop/sessions".
	CoopS3Prefix string

	// CoopS3ImagesBucket is the bucket gb image upload writes
	// <prefix>/images/... to (env: COOP_S3_IMAGES_BUCKET, chart
	// objectStore.images.bucket, gd-CmjVAU3vgd). Empty keeps images in
	// CoopS3Bucket.
	CoopS3ImagesBucket string

	// CoopS3Region is the AWS region for the S3 bucket (env: COOP_S3_REGION).
	// When empty, coop uses the default AWS region chain.
	CoopS3Region string

	// CoopS3Endpoint overrides the S3 service endpoint (env: COOP_S3_ENDPOINT).
	// When set (e.g. an in-box MinIO URL like http://minio.gardenio.svc:9000),
	// S3 clients target that endpoint with path-style addressing instead of
	// AWS S3. When empty, the default AWS S3 endpoint and virtual-hosted-style
	// addressing are used, unchanged.
	CoopS3Endpoint string

	// CoopS3CredentialsSecret is the K8s Secret name carrying the
	// object-store (MinIO) access/secret key pair (env:
	// COOP_S3_CREDENTIALS_SECRET), endpoint mode only. podspec.go projects it
	// onto agent pods as a tmpfs-backed Secret volume (podmanager.
	// ObjectStoreCredentialsSecret) rather than an env value — see
	// gd-W8OwRoHLLn. Empty in AWS/IRSA mode, where credentials come from the
	// ServiceAccount web-identity token instead.
	//
	// It must name the least-privilege agent Secret (chart default
	// "<release>-minio-agent-credentials", gd-urSiLLwPIz), never the MinIO
	// root Secret: podspec.go refuses any name on the forbidden-secret
	// deny-list, which includes "minio-credentials".
	CoopS3CredentialsSecret string

	// --- Coopmux ---

	// CoopmuxURL is the URL of the coopmux service (env: COOPMUX_URL).
	// When set, agent pods register with coopmux for credential distribution and
	// terminal multiplexing.
	CoopmuxURL string

	// CoopmuxTokenSecret is the K8s secret containing the coopmux auth token (env: COOPMUX_TOKEN_SECRET).
	// Injected as COOP_BROKER_TOKEN and COOP_MUX_TOKEN in agent pods.
	CoopmuxTokenSecret string

	// CoopmuxLandTokenKeyFile is the file holding the key a tenant land's
	// coopmux token is minted under, the one coopmux verifies it with (env:
	// COOP_MUX_LAND_TOKEN_KEY_FILE, internal/lands/landtoken). A tenant pod
	// carries its land's token instead of CoopmuxTokenSecret; unset, a tenant
	// pod gets no coopmux token and does not register (gd-ApgYe740ps).
	CoopmuxLandTokenKeyFile string

	// NatsLandCalloutIssuerSeedFile is the file holding the account seed the
	// NATS server's auth_callout.issuer names (env:
	// NATS_LAND_CALLOUT_ISSUER_SEED_FILE, the chart's
	// nats.auth.subjectIsolation.landCallout). Set with
	// CoopmuxLandTokenKeyFile and NatsURL, the controller answers the
	// server's auth callout for tenant lands and a tenant pod relays its coop
	// events as its land's own NATS user (internal/lands/natsland,
	// gd-sS7bSXOdGs); unset, a tenant pod carries no NATS.
	NatsLandCalloutIssuerSeedFile string

	// LandsAccountTemplate names the account bead a tenant land's lands-<land>
	// account is copied from (env: LANDS_ACCOUNT_TEMPLATE, the lands
	// provisioner's). Its credential is the one fleet Secret a tenant pod may
	// read besides the land's own (landCredential, gd-NXTrUXD1Sq).
	LandsAccountTemplate string

	// --- OpenZiti Access Broker (epic ka-0000000990) ---
	//
	// Per-pod OpenZiti overlay access via the network-fabric ziti-access-broker.
	// All fields default empty/off; a pod only gets Ziti wiring when a project
	// or role bead opts in (ziti_access=true) AND ZitiAccessBrokerURL is set.

	// ZitiAccessBrokerURL is the in-cluster base URL of the ziti-access-broker
	// (env: ZITI_ACCESS_BROKER_URL). When empty, per-pod Ziti access is fully
	// disabled regardless of bead metadata, and the controller skips
	// revoke-on-exit. This is the master on/off switch.
	ZitiAccessBrokerURL string

	// ZitiAccessTokenAudience is the projected ServiceAccount-token audience the
	// broker requires (env: ZITI_ACCESS_TOKEN_AUDIENCE, default
	// "gardenio-ziti-broker"). Empty falls back to the podmanager default.
	ZitiAccessTokenAudience string

	// ZitiTunnelerImage is the ziti-edge-tunnel image the per-pod tunnel sidecar
	// runs (env: ZITI_TUNNELER_IMAGE). Required for the tunneler enrollment
	// shape; a pod opting into Ziti access without a configured tunneler image
	// is rejected at spec-build time with an actionable error.
	ZitiTunnelerImage string

	// ZitiAccessDefaultTTL is the default lease TTL requested when a bead does
	// not specify one (env: ZITI_ACCESS_DEFAULT_TTL, e.g. "15m"). Empty lets the
	// broker apply its own default.
	ZitiAccessDefaultTTL string

	// ZitiAccessRevokeOnExit, when true (env: ZITI_ACCESS_REVOKE_ON_EXIT,
	// default true), makes the controller call DELETE /lease?task_id=<beadID>
	// when an agent reaches a terminal state. The controller must be listed in
	// the broker's REVOKER_SUBJECTS for this to succeed; it is best-effort
	// (the lease TTL is the hard backstop).
	ZitiAccessRevokeOnExit bool

	// ZitiAccessControllerTokenFile is the path to the controller's OWN
	// projected broker-audience token used to authenticate revoke calls
	// (env: ZITI_ACCESS_CONTROLLER_TOKEN_FILE). Mounted by the chart when
	// zitiAccess.revokeOnExit is enabled. Empty disables controller revoke.
	ZitiAccessControllerTokenFile string

	// --- Lambda MicroVM Backend (epic ka-0000005010, Change C3) ---
	//
	// Cross-account AWS identity for the lambda_microvm execution backend. The
	// controller assumes a Fabric-owned launcher role in acct 345678901234 to
	// RunMicrovm/GetMicrovm/TerminateMicrovm + write per-run Secrets Manager
	// values. Two launcher-hop origins exist (ka-0000006687):
	//   - MicrovmControllerRoleARN set (production): the hop originates from
	//     the dedicated gardenio-prod-microvm-controller role, whose OIDC trust
	//     pins the controller SA only.
	//   - unset (POC/rollback, operator-accepted Sky Bennett 2026-07-17): the
	//     controller rides the shared gardenio-prod-agent IRSA identity — any
	//     pod on that role could in principle assume the launcher. Refused in
	//     production mode (MicrovmProductionMode).
	// All fields default empty/off; the MicroVM backend is unusable (and the
	// credential factory is never constructed) unless both MicrovmLauncherRoleARN
	// and MicrovmRegion are set.

	// MicrovmLauncherRoleARN is the Fabric-owned cross-account IAM role the
	// controller assumes to drive Lambda MicroVMs + per-run Secrets Manager
	// (env: MICROVM_LAUNCHER_ROLE_ARN, e.g.
	// arn:aws:iam::345678901234:role/ziti-microvm-poc-gardenio-launcher). The
	// controller reaches it via an stscreds assume-role provider layered on its
	// own web-identity (IRSA) credentials — its base identity (the shared
	// gardenio-prod-agent role) is trusted by the launcher's trust policy for this
	// POC. Empty disables the MicroVM backend regardless of any bead's
	// execution_target.
	MicrovmLauncherRoleARN string

	// MicrovmRegion is the AWS region the Lambda MicroVM + per-run Secrets
	// Manager clients target after the assume-role hop (env: MICROVM_REGION,
	// e.g. "us-east-2"). It is INDEPENDENT of the controller's own region
	// (COOP_S3_REGION / the default chain) because the MicroVM image and launcher
	// live in us-east-2 while the controller's cluster + sessions bucket are in
	// us-east-1. Empty disables the MicroVM backend.
	MicrovmRegion string

	// MicrovmAssumeRoleSessionName is the STS session name stamped on the
	// assume-role call (env: MICROVM_ASSUME_ROLE_SESSION_NAME, default
	// "gardenio-microvm-controller"). Surfaces in the launcher account's
	// CloudTrail as the caller identity for every RunMicrovm/secret write, so a
	// stable, self-describing name keeps the cross-account audit trail legible.
	MicrovmAssumeRoleSessionName string

	// MicrovmSecretPrefix is the Secrets Manager name prefix the per-run
	// identity + runtime-credential secrets are written under (env:
	// MICROVM_SECRET_PREFIX, default "ziti-microvm-agents/"). The launcher's
	// Secrets Manager grant is scoped to exactly this prefix, so it MUST match
	// the Fabric-staged boundary; consumed by the C4 backend, carried here so the
	// whole MicroVM config lives in one place. A trailing slash is normalized on.
	MicrovmSecretPrefix string

	// MicrovmImageIdentifier is the approved MicroVM image the launcher may run
	// (env: MICROVM_IMAGE_IDENTIFIER, e.g.
	// arn:aws:lambda:us-east-2:345678901234:microvm-image:ziti-microvm-poc). The
	// launcher's RunMicrovm grant is scoped to exactly this image, so it MUST
	// match the Fabric-staged boundary. Required for the C4 backend; empty leaves
	// RunMicrovm unusable even when the C3 identity is otherwise enabled.
	MicrovmImageIdentifier string

	// MicrovmImageVersion pins the image version RunMicrovm launches (env:
	// MICROVM_IMAGE_VERSION, e.g. "7.0"). Empty lets AWS pick the image's active
	// version; the POC pins "7.0" (the version with all four proxy listeners) so
	// a Fabric-side active-version bump can't silently change what agents run.
	MicrovmImageVersion string

	// MicrovmExecutionRoleARN is the execution role the Lambda service assumes
	// INSIDE the MicroVM (env: MICROVM_EXECUTION_ROLE_ARN, e.g.
	// arn:aws:iam::345678901234:role/ziti-microvm-poc-execution). The launcher's
	// iam:PassRole grant is scoped to exactly this role. Required for the C4
	// backend; RunMicrovm passes it as ExecutionRoleArn.
	MicrovmExecutionRoleARN string

	// MicrovmStaticIdentitySecretARN is the shared, pre-enrolled Ziti identity
	// secret reused for the single-concurrency POC (env:
	// MICROVM_STATIC_IDENTITY_SECRET_ARN, e.g.
	// arn:aws:secretsmanager:us-east-2:345678901234:secret:ziti-microvm-poc/ziti-identity-kCfNSS).
	// The MicroVM image reads its Ziti identity from this secret ARN, passed in
	// the run-hook payload as identitySecretArn. It supports only ONE concurrent
	// MicroVM. DEPRECATED (ka-0000001924): ignored whenever MicrovmZitiBrokerURL
	// is set (per-run identities); kept only as config-gated rollback until the
	// per-run path passes the production E2E gate (ka-0000000183).
	MicrovmStaticIdentitySecretARN string

	// MicrovmGitLabBaseURL is the GitLab instance the per-run MicroVM git
	// credential minter targets (env: MICROVM_GITLAB_BASE_URL, default
	// "https://gitlab.com"). Only consulted when MicrovmGitFactoryToken is set.
	MicrovmGitLabBaseURL string

	// MicrovmGitFactoryToken is the privileged GitLab token (api scope,
	// Maintainer on the target projects) the controller uses to MINT per-run
	// project access tokens for MicroVM satellites (env:
	// MICROVM_GIT_FACTORY_TOKEN, sourced from a K8s Secret ref in the chart —
	// agents.microvm.gitCredentials.factoryTokenSecret). Empty (default)
	// disables minting: the satellite falls back to the project's shared git
	// secret (same K8s Secret the pod path projects), delivered via the per-run
	// task-scoped secret, and revocation is delivery-vehicle-only. The value
	// itself must never be logged and never delivered to any runtime.
	MicrovmGitFactoryToken string

	// MicrovmZitiBrokerURL is the ziti-access-broker base endpoint the controller
	// mints per-run MicroVM Ziti identities from (env: MICROVM_ZITI_BROKER_URL).
	// When set, every lambda_microvm launch mints a unique short-lived identity
	// carrying role #gardenio-microvm-agents (fail-closed: broker errors abort the
	// launch), delivered via the per-run ziti-identity secret, and revoked on
	// every exit path; MicrovmStaticIdentitySecretARN is ignored. Empty keeps the
	// deprecated static-identity POC path (ka-0000001924).
	MicrovmZitiBrokerURL string

	// MicrovmZitiBrokerServerID is the bound X-Ziti-Broker-Server-Id header value
	// inside the signed STS caller-verification proof (env:
	// MICROVM_ZITI_BROKER_SERVER_ID). Must match the broker's configured server
	// id; empty defaults to the broker URL's host.
	MicrovmZitiBrokerServerID string

	// MicrovmZitiBrokerEnroll requests broker-side enrollment (contract Option B:
	// the broker returns enrolled identity JSON, zero image change) instead of
	// the default enrollment-JWT delivery (Option A: the VM enrolls at boot and
	// the private key never leaves it) (env: MICROVM_ZITI_BROKER_ENROLL, default
	// false). Set only if the Ziti controller client API is unreachable from a
	// MicroVM over INTERNET_EGRESS.
	MicrovmZitiBrokerEnroll bool

	// MicrovmLogGroup is the CloudWatch Logs log group MicroVM runtime
	// stdout/stderr is delivered to (env: MICROVM_LOG_GROUP, e.g.
	// "/aws/lambda/microvms/ziti-microvm-poc" — the Fabric-staged group whose
	// WriteRuntimeLogs grant the execution role already holds, nfd!15). Set,
	// RunMicrovm carries a per-run CloudWatch Logging member; empty leaves
	// Logging unset (no runtime logs — the pre-logging POC behavior).
	MicrovmLogGroup string

	// MicrovmLogStreamPrefix prefixes the per-run log stream inside
	// MicrovmLogGroup (env: MICROVM_LOG_STREAM_PREFIX, default "agent/"): each
	// agent bead logs to "<prefix><beadID>", so streams never interleave across
	// concurrent agents. Ignored when MicrovmLogGroup is empty.
	MicrovmLogStreamPrefix string

	// MicrovmControllerRoleARN is the dedicated controller-only IRSA role the
	// launcher hop originates from (env: MICROVM_CONTROLLER_ROLE_ARN, e.g.
	// arn:aws:iam::123456789012:role/gardenio-prod-microvm-controller — already
	// trusted by the Fabric launcher per network-fabric-deployment!15). When
	// set, the controller exchanges its projected SA token for THIS role via
	// AssumeRoleWithWebIdentity and assumes the launcher from it, retiring the
	// POC's shared gardenio-prod-agent trust. Empty keeps the POC/rollback chain
	// (default credential chain → launcher); required in production mode.
	MicrovmControllerRoleARN string

	// MicrovmExpectedLauncherAccount pins the AWS account the assumed launcher
	// identity must resolve to (env: MICROVM_EXPECTED_LAUNCHER_ACCOUNT). When
	// set, the startup GetCallerIdentity verification refuses the backend on
	// mismatch, so a stale/wrong launcher ARN fails closed at boot. Empty skips
	// the pin; required in production mode.
	MicrovmExpectedLauncherAccount string

	// MicrovmProductionMode declares the MicroVM deployment production (env:
	// MICROVM_PRODUCTION_MODE, default false; chart
	// agents.microvm.productionMode). It converts every remaining POC
	// assumption into a fail-closed startup error (ka-0000006687): the static
	// POC identity must be unset, the ziti-access-broker (per-run identities)
	// must be configured, the dedicated controller role must be set, and the
	// launcher account must be pinned. Off keeps POC/rollback semantics.
	MicrovmProductionMode bool

	// MicrovmMaximumDurationSeconds caps a MicroVM's wall-clock lifetime (env:
	// MICROVM_MAXIMUM_DURATION_SECONDS; chart agents.microvm.maximumDurationSeconds).
	// <=0 keeps the backend's compiled default (3600s — the POC helper value).
	// Long-lived roles (gardener / plan-gardener iterations regularly run for
	// hours) need this raised toward the epic's 8h ceiling; the per-run Ziti
	// identity lease is automatically sized to this cap plus slack, so raising
	// it never strands an identity before its VM (ka-0000003026).
	MicrovmMaximumDurationSeconds int

	// MicrovmSessionVendorRoleARN is the S2-provisioned role the controller
	// assumes via AssumeRoleWithWebIdentity to mint bead-scoped S3 session
	// credentials for a MicroVM satellite (env: MICROVM_SESSION_VENDOR_ROLE_ARN;
	// chart agents.microvm.sessionVendorRoleARN, e.g.
	// arn:aws:iam::123456789012:role/gardenio-prod-microvm-session-vendor). The
	// vendor role's OIDC trust pins the controller SA subject, and its ceiling
	// policy scopes to coop/sessions/* CRUD + ViaService KMS; the controller
	// narrows each mint to coop/sessions/<bead-id>/* via an inline session policy
	// (S4 ka-0000006612, internal/microvm/session_vendor.go). Empty disables S3
	// session-credential vending: a satellite then gets no sessions-bucket
	// credential (exactly as before S4), since the MINIMAL execution role grants
	// it none. Also gated on a configured sessions bucket (CoopS3Bucket).
	MicrovmSessionVendorRoleARN string

	// MicrovmSessionCredentialDurationSeconds is the requested lifetime of a
	// minted bead-scoped S3 credential (env:
	// MICROVM_SESSION_CREDENTIAL_DURATION_SECONDS; chart
	// agents.microvm.sessionCredentialDurationSeconds). Clamped to STS's
	// [900, 43200] window. <=0 uses the 12h default (the vendor role's max
	// session duration), so a single mint covers the VM's 8h wall-clock ceiling
	// plus suspended wall-clock with no refresh/re-mint on resume. Raise the
	// vendor role's max_session_duration if a longer lifetime is ever needed.
	MicrovmSessionCredentialDurationSeconds int

	// MicrovmIdleAutoResumeEnabled sets the AWS IdlePolicy AutoResumeEnabled flag
	// on every RunMicrovm launch (env: MICROVM_IDLE_AUTO_RESUME_ENABLED, default
	// false; chart agents.microvm.idlePolicy.autoResumeEnabled). It is only
	// consulted when MicrovmIdlePolicyEnabled is true. NOTE (topology): our
	// MicroVMs are egress-only (only INTERNET_EGRESS), so the AWS idle clock —
	// which counts inbound requests through the proxy endpoint — never advances
	// and auto-resume-on-inbound cannot fire in-topology; resume is
	// controller-driven. The flag is still set for forward-compatibility, but
	// SuspendedDurationSeconds is the operative standby-billing backstop.
	MicrovmIdleAutoResumeEnabled bool

	// MicrovmIdleMaxIdleDurationSeconds is the AWS IdlePolicy MaxIdleDurationSeconds
	// (env: MICROVM_IDLE_MAX_IDLE_DURATION_SECONDS; chart
	// agents.microvm.idlePolicy.maxIdleDurationSeconds): the maximum time a
	// MicroVM may remain idle before AWS auto-suspends it. Required by the SDK
	// whenever an IdlePolicy is set. Egress-only topology means this clock does
	// not advance in-topology (see MicrovmIdleAutoResumeEnabled), but the SDK
	// still requires a value.
	MicrovmIdleMaxIdleDurationSeconds int

	// MicrovmIdleSuspendedDurationSeconds is the AWS IdlePolicy
	// SuspendedDurationSeconds (env: MICROVM_IDLE_SUSPENDED_DURATION_SECONDS;
	// chart agents.microvm.idlePolicy.suspendedDurationSeconds): the maximum time
	// a MicroVM may remain SUSPENDED before AWS auto-terminates it. Required by
	// the SDK whenever an IdlePolicy is set. This is the operative knob in our
	// egress-only topology: the standby-billing auto-terminate backstop that
	// reaps a MicroVM the controller suspended but never resumed.
	MicrovmIdleSuspendedDurationSeconds int

	// MicrovmIdlePolicyEnabled is the master switch for setting an AWS IdlePolicy
	// on RunMicrovm (env: MICROVM_IDLE_POLICY_ENABLED, default false; chart
	// agents.microvm.idlePolicy.enabled). Off (the default) leaves
	// RunMicrovmInput.IdlePolicy unset, preserving the pre-idle-policy launch
	// shape byte-for-byte. When on, all three MicrovmIdle* values above are sent
	// as one IdlePolicy (all three are required by the SDK together).
	MicrovmIdlePolicyEnabled bool

	// MicrovmMaxStandbyAge caps how long a controller-suspended MicroVM thread
	// agent may sit in SUSPENDED standby before leak GC terminates its VM and
	// closes the (still-open) resume-target bead as done (env:
	// MICROVM_MAX_STANDBY_AGE, e.g. "24h"; chart
	// agents.microvm.suspendOnStop.maxStandbyAge). <=0 (the default) disables the
	// cap — standby then relies solely on the AWS IdlePolicy
	// SuspendedDurationSeconds auto-terminate backstop (step 1). This is the
	// belt-and-suspenders controller-side cap of the suspend/resume plan (step 4,
	// ka-0000006202): it reaps a VM that was suspended on `gb stop` but never
	// resumed by a re-mention, so a never-resumed agent cannot bill standby
	// forever even when no IdlePolicy is configured. Standby age is measured from
	// the runtime_suspended_at the Suspend path stamps (cleared on Resume), NOT
	// from the VM's total wall-clock age, so it mirrors AWS's
	// SuspendedDurationSeconds semantics.
	MicrovmMaxStandbyAge time.Duration

	// MicrovmSuspendEnabled is the master switch for the native suspend-on-stop
	// path (env: MICROVM_SUSPEND_ENABLED, default false; chart
	// agents.microvm.suspendOnStop.enabled). Off (the default) preserves today's
	// behaviour byte-for-byte: a MicroVM thread agent's `gb stop` closes its bead
	// (done) and the controller Terminates the VM. When on, a freeze-eligible
	// MicroVM THREAD agent's graceful stop instead SUSPENDS the VM
	// (SuspendMicrovm) and leaves the bead open at agent_state=stopping as the
	// resume target — the native analog of gVisor freeze/thaw (plan ka-0000004518,
	// epic ka-0000004689). AgentKill/AgentDone and non-thread agents always
	// Terminate regardless of this flag. The controller uses it in two seams that
	// MUST agree: (1) it injects the in-VM BOAT_FREEZE_ELIGIBLE signal for a
	// suspend-eligible MicroVM thread agent so its `gb stop` transitions to
	// stopping (AgentStop) rather than closing the bead, and (2) it branches the
	// teardown to Suspend rather than Terminate on that AgentStop.
	MicrovmSuspendEnabled bool

	// MicrovmResumeRehydrateEnabled gates the fail-closed resume rehydration
	// rendezvous (env: MICROVM_RESUME_REHYDRATE_ENABLED, default true; chart
	// agents.microvm.resumeRehydrate.enabled). This is the step-5 correctness
	// boundary (plan ka-0000004518): after ResumeMicrovm, the controller waits
	// bounded for the resumed VM to prove life via its egress heartbeat (presence
	// last_seen advancing) before marking agent_state=working — and fails closed
	// (terminate VM + agent_state=resume_failed) if proof never arrives, so a
	// half-rehydrated VM (dead Ziti lease, failed re-credentialing) never serves
	// the user. Off is the step-3 rollback: Resume stamps agent_state=working
	// immediately with no proof-of-life gate. See docs/microvm-resume-rehydration.md.
	MicrovmResumeRehydrateEnabled bool

	// MicrovmResumeRehydrateDeadline bounds the resume rehydration proof-of-life
	// wait (env: MICROVM_RESUME_REHYDRATE_DEADLINE_SECONDS; chart
	// agents.microvm.resumeRehydrate.deadlineSeconds; default 90s). A MicroVM
	// resume restarts the ENTIRE userspace (process spawn + OpenZiti proxy
	// re-tunnel + Secrets Manager reads + STS + first heartbeat over the overlay),
	// so it wants a more generous budget than the gVisor in-process 10s reconnect;
	// the VM disk survives suspend so there is no cold repo re-clone. Comfortably
	// under the 15-min presence reaper. <=0 uses the built-in default. Only
	// meaningful when MicrovmResumeRehydrateEnabled is true.
	MicrovmResumeRehydrateDeadline time.Duration

	// --- Account Rotation Restarts ---

	// AccountRotationRestartEnabled gates the account-exhaustion pod-restart
	// paths (accountRotationRestarter.Reconcile and ReconcileStrandedRecovered)
	// (env: ACCOUNT_ROTATION_RESTART_ENABLED, default true). Operator kill
	// switch for incident response: deleting a pod destroys its in-flight
	// session context, so when restart storms are suspected of eating work
	// (ka-0000005463) this turns the pod-deleting failover off without a
	// code change. Stale-vkey restarts stay on independently — see
	// StaleVKeyRestartEnabled.
	AccountRotationRestartEnabled bool

	// StaleVKeyRestartEnabled gates accountRotationRestarter.ReconcileStaleVKeys
	// (env: STALE_VKEY_RESTART_ENABLED, default true). Separate from
	// AccountRotationRestartEnabled because a rotated-out virtual key is a
	// terminal 401-crashloop (ka-0000003704) that no window reset or in-process
	// credential switch recovers, so it should normally stay on even while the
	// exhaustion-driven restarts are disabled.
	StaleVKeyRestartEnabled bool

	// SpreadServedModelGateEnabled gates the spread-pool served-model check
	// (env: SPREAD_SERVED_MODEL_GATE_ENABLED, default true). When true, the
	// servedModelSync loop runs and the spread selector excludes any project
	// account whose baked model is NOT in LiteLLM's served-model set — a
	// target/undeployed-model account (valid key, unserved model) can never be
	// baked as agent cognition and crash-loop the pod (ka-0000002790). Operator
	// kill switch: set false to stop the sync loop and disable the gate without
	// a code change (the gate is also fail-open when the served set is unknown).
	SpreadServedModelGateEnabled bool

	// --- Leader Election ---

	// LeaderElection enables K8s lease-based leader election (env: ENABLE_LEADER_ELECTION).
	// When true, only the leader replica reconciles; others wait passively.
	// Required for running multiple replicas safely.
	LeaderElection bool

	// LeaderElectionID is the name of the Lease resource used for leader election
	// (env: LEADER_ELECTION_ID). Default: "agents-leader".
	LeaderElectionID string

	// LeaderElectionIdentity is the unique identity of this controller instance
	// (env: POD_NAME). Typically set from the Kubernetes downward API.
	// Default: hostname.
	LeaderElectionIdentity string

	// Slack notifications are now handled by the standalone slack-bridge
	// binary (cmd/slack-bridge). Slack config fields removed — see bd-8x8fy.

	// --- ExternalSecret Reconciliation ---

	// ExternalSecretStoreName is the SecretStore name for auto-reconciled ExternalSecrets
	// (env: EXTERNAL_SECRET_STORE_NAME). Default: "secretstore".
	ExternalSecretStoreName string

	// ExternalSecretStoreKind is the SecretStore kind for auto-reconciled ExternalSecrets
	// (env: EXTERNAL_SECRET_STORE_KIND). Default: "ClusterSecretStore".
	ExternalSecretStoreKind string

	// ExternalSecretRefreshInterval is the refresh interval for auto-reconciled ExternalSecrets
	// (env: EXTERNAL_SECRET_REFRESH_INTERVAL). Default: "15m".
	ExternalSecretRefreshInterval string

	// --- Prewarmed Agent Pool ---
	//
	// Pool size, role, mode, and enabled/disabled are configured per-project
	// via the "prewarmed_pool" JSON field on project beads (see beadsapi.PrewarmedPoolConfig).
	// Only the reconcile interval is controller-level config.

	// PrewarmedPoolInterval is how often the multi-pool reconciler runs
	// (env: PREWARMED_POOL_INTERVAL). Default: 30s.
	PrewarmedPoolInterval time.Duration

	// PrewarmedMicrovmMaxAge caps how long an UNASSIGNED lambda_microvm pool
	// member may sit in standby before the reconciler recycles it (closes the
	// bead → the VM tears down) and mints a fresh replacement (env:
	// PREWARMED_MICROVM_MAX_AGE, e.g. "20m"; chart
	// agents.prewarmedMicrovmMaxAge). This is the Phase-4 recycle-before-cap
	// knob (ka-0000003944): a MicroVM member burns a MaximumDurationInSeconds
	// wall-clock cap (MicrovmMaximumDurationSeconds) and its per-run Ziti lease +
	// SM/S3 credentials expire on TTLs (S3 ~12h, beads PAT 24h, git ~48h) — all
	// anchored at Launch. Recycling well under the cap guarantees an assigned
	// member always has ample remaining run budget and unexpired credentials,
	// and bounds idle standby billing. <=0 (the default) means "derive from the
	// cap": recycleAge falls back to half of the effective
	// MicrovmMaximumDurationSeconds, which self-adjusts as the cap is raised
	// (ka-0000002537 3600s→8h30m) and always sits far under every credential TTL.
	// Applies ONLY to lambda_microvm members; K8s pool members carry no
	// wall-clock cap and are never age-recycled. Recycles are rate-capped per
	// pass by the same drainRate as the stale-image recycler.
	PrewarmedMicrovmMaxAge time.Duration

	// StandbyPoll is how often a prewarmed agent pod polls its own bead for
	// the prewarmed→assigning state flip while parked in entrypoint standby
	// (env: STANDBY_POLL, injected pod-side as BOAT_STANDBY_POLL). This is the
	// dominant term in user-perceived warm-pool spawn latency: the controller
	// flips the bead in ~150ms, but the pod only discovers the assignment on
	// its next poll tick. The pod is idle and a GetBead on its own bead is
	// cheap, so a sub-second interval keeps end-to-end warm spawn genuinely
	// sub-second instead of the up-to-5s worst case the pod-side 5s default
	// produced (ka-0000003812). Default: 500ms.
	StandbyPoll time.Duration

	// --- Queue Age Alerting ---
	//
	// The queue-age warn threshold used to live here as CoopQueuedWarnAge
	// (env: COOP_QUEUED_WARN_AGE). It moved to the ConfigWatcher as the
	// pilot of the env→config-bead campaign (ka-0000003317 / epic
	// ka-0000006607) — see reconciler.Reconciler.queuedWarnAge and the
	// /config/tunables admin endpoint.

	// --- Upgrade Drain ---

	// UpgradeDrainTimeout is how long to wait for an agent to reach idle state
	// after being nudged before force-deleting the pod (env: COOP_UPGRADE_DRAIN_TIMEOUT).
	// Default: 5m.
	UpgradeDrainTimeout time.Duration

	// ToolLatencySlowThreshold is the cutoff over which the P2 HOOK_EVENTS
	// consumer (plan gardenio/plans/agent-tool-latency-attribution.md, bead
	// gd-sWwPnwkZtw) writes a full-argv slow_tool_calls ledger row for a tool
	// call (env: TOOL_LATENCY_SLOW_THRESHOLD). Default: 2s.
	ToolLatencySlowThreshold time.Duration

	// NodeDrainNudgeEnabled makes the reconciler route agents onto the graceful
	// commit+push drain nudge when their NODE is going away (WS1,
	// ka-0000007969). Every agent pod carries
	// karpenter.sh/do-not-disrupt and no NodePool sets terminationGracePeriod,
	// so Karpenter never voluntarily evicts a running agent; a node that is
	// nonetheless cordoned+tainted karpenter.sh/disrupted (or already carries a
	// DeletionTimestamp) is being FORCE-reclaimed by AWS (spot / health /
	// interruption-queue) in a way do-not-disrupt does not stop. That is an
	// unambiguous "this pod is about to be SIGKILL'd" signal, so the reconciler
	// pre-emptively fires the same commit+push+gb-stop nudge account rotation
	// uses, giving the agent its UpgradeDrainTimeout window to land WIP
	// before the node drain reaches it. Off by default (opt-in via Helm) so the
	// behaviour is a no-op until an operator enables it and node visibility is
	// wired. (env: COOP_NODE_DRAIN_NUDGE). Default: false.
	NodeDrainNudgeEnabled bool

	// --- Auth Failover ---

	// AuthExhaustedSweepInterval is how often the controller runs
	// SweepStaleExhausted to return recovered/cooled-down accounts to the healthy
	// spread pool (env: AUTH_EXHAUSTED_SWEEP_INTERVAL). This is the sole recovery
	// cadence now that switchback is abolished (ka-0000008240); it replaces the
	// former AUTH_SWITCHBACK_INTERVAL, which drove the same sweep during the
	// backward-compat window. Default: 5m. Floored to at least one sync cycle
	// while the sync loop runs so recovery can never silently stop.
	AuthExhaustedSweepInterval time.Duration

	// AuthFailoverCooldown is the minimum time between failover events for the
	// same project, preventing flapping (env: AUTH_FAILOVER_COOLDOWN).
	// Default: 5m. Set to 0 to disable cooldown.
	AuthFailoverCooldown time.Duration

	// AuthMaxFailoverDuration is how long an exhausted account waits before the
	// cooldown_until-elapsed backstop in SweepStaleExhausted returns it to the
	// healthy spread pool without a fresh recovering probe
	// (env: AUTH_MAX_FAILOVER_DURATION). It sets the default cooldown_until
	// horizon (computeCooldownUntil) for accounts with no billing_period_hours.
	// This is the primary-free replacement for the old switchback maxFailoverDur
	// auto-reset safety net (ka-0000008240). Default: 24h.
	AuthMaxFailoverDuration time.Duration

	// --- LiteLLM Proxy ---

	// LiteLLMURL is the URL of the external LiteLLM proxy service (env: LITELLM_URL).
	// When an account bead has a base_url, it is used directly. When a project references
	// an account but the account has no base_url, this URL is used as the default.
	LiteLLMURL string

	// LiteLLMMasterKey is the admin/master key for LiteLLM (env: LITELLM_MASTER_KEY).
	// Used by the spend sync loop to query /key/info for per-account spend.
	LiteLLMMasterKey string

	// LiteLLMTagSpendSource is where the controller reads lifetime per-tag
	// spend (env: LITELLM_TAG_SPEND_SOURCE, gd-Um9gnz9FuW): "spend-tags"
	// (GET /spend/tags, a LiteLLM_SpendLogs scan), "daily" (the
	// LiteLLM_DailyTagSpend aggregate, which spend-log retention leaves
	// intact) or "auto" (the default: both, until they agree, then daily).
	LiteLLMTagSpendSource string

	// LiteLLMKeyRotationInterval is how often the key-ACL widen loop scans
	// account beads to widen each virtual key's model allow-list (env:
	// LITELLM_KEY_ROTATION_INTERVAL, default 6h). The name predates the
	// deletion of the dormant key-rotation half (gd-OLYrS5E6F8).
	LiteLLMKeyRotationInterval time.Duration

	// LiteLLMAdminSecret is the name of the K8s secret containing the LiteLLM admin
	// API key (env: LITELLM_ADMIN_SECRET, default: "litellm-gardenio-key"). The secret
	// must have a "token" key. Used at startup to sync model_group_alias entries that
	// map Claude Code's internal model names to the correct LiteLLM model groups.
	LiteLLMAdminSecret string

	// --- Slack Bridge ---

	// SlackBridgeURL is the URL of the slack-bridge HTTP service (env: SLACK_BRIDGE_URL).
	// Injected as SLACK_BRIDGE_URL in agent pods so gb slack commands can reach the bridge.
	SlackBridgeURL string

	// SlackBridgeTokenSecret is the name of a K8s Secret containing the bridge
	// agent bearer token under key "token" (env: SLACK_BRIDGE_TOKEN_SECRET).
	// Injected into agent pods as SLACK_BRIDGE_TOKEN so `gb slack` calls to
	// /api/slack/* and /api/permission/* authenticate to the bridge. When
	// empty, the token is not injected and the bridge's agent endpoints will
	// reject agent traffic (fail-closed) unless the bridge itself is also
	// running without SLACK_BRIDGE_AGENT_TOKEN (local dev).
	SlackBridgeTokenSecret string

	// --- Jira Bridge ---

	// JiraBridgeURL is the URL of the jira-bridge HTTP service (env:
	// JIRA_BRIDGE_URL). Injected as JIRA_BRIDGE_URL in agent pods so
	// `gb jira reply` can reach the bridge (feature F2, plan ka-0000007401).
	JiraBridgeURL string

	// JiraBridgeTokenSecret is the name of a K8s Secret containing the
	// jira-bridge agent bearer token under key "token" (env:
	// JIRA_BRIDGE_TOKEN_SECRET). Injected into agent pods as
	// JIRA_BRIDGE_TOKEN so `gb jira reply` authenticates to /api/jira/reply
	// when the bridge sets JIRA_BRIDGE_AGENT_TOKEN. Mirrors
	// SlackBridgeTokenSecret.
	JiraBridgeTokenSecret string

	// AgentSecretAllowlist lists the K8s Secrets that project, role and
	// account beads may put into an agent pod (env: AGENT_SECRET_ALLOWLIST,
	// comma-separated path.Match globs such as "*-git-credentials" or
	// "gardenio-claude-oauth-*"). Bead-supplied names are otherwise checked only
	// by the {project}-/{role}- prefix, which every controller-only Secret in
	// the release namespace (gardenio-machine-gardenio-controller,
	// gardenio-keycloak-runtime-admin, ...) passes for project "gardenio" or a
	// role named after its leading words (gd-q8nBnOP65d). Empty = no allow-list;
	// only the controller's forbidden-secret markers apply. Secrets the
	// controller's own config names (SLACK_BRIDGE_TOKEN_SECRET etc.) are not
	// bead-supplied and are not checked against it.
	AgentSecretAllowlist []string

	// --- Beads Viewer ---

	// BeadsViewerURL is the public base URL of the read-only beads-viewer,
	// including any path prefix (e.g. https://gardenio.example.com/beads)
	// (env: BEADS_VIEWER_URL). Injected into agent pods as BEADS_VIEWER_URL so
	// `gb bundle`, `gb image upload`, and widget-preview links an agent posts
	// resolve to absolute, clickable URLs on the real viewer host. Empty = not
	// injected; gb then falls back to its built-in default host. See bug
	// ka-0000003798: without this, agent-posted bundle links were hostless
	// "/bundle/<id>" strings that Slack showed as plain text and 404'd on prod.
	BeadsViewerURL string

	// BeadsViewerInternalURL is the in-cluster beads-viewer Service base URL
	// with NO base-path/edge prefix (e.g.
	// http://gardenio-beads-viewer.gardenio-system.svc.cluster.local:8095)
	// (env: BEADS_VIEWER_INTERNAL_URL). Injected into agent pods as
	// BEADS_VIEWER_INTERNAL_URL so `gb plan create` / `gb plan add-step` POST
	// straight to the viewer pod out-of-the-box, without a hand-supplied
	// --viewer-url. Empty = not injected; gb then falls back to the public
	// BEADS_VIEWER_URL, which is the AVA/OIDC edge and makes plan writes fail
	// with a redirect error. See ka-0000000379: previously this was set only on
	// the plan-gardener role bead's env, so every other role's pods lacked it.
	BeadsViewerInternalURL string

	// GardenioDefaultHost is the bare gardenio host (no scheme/path, e.g.
	// gardenio.example.com) injected into agent pods as GARDENIO_DEFAULT_HOST
	// (env: GARDENIO_DEFAULT_HOST). The gb CLI inside a pod reads it to derive
	// its zero-config daemon/viewer hosts (cmd/gb/main.go resolveGardenioHost),
	// overriding the COMPILED default without a per-command flag. Empty = not
	// injected; gb falls back to its built-in compiled default host. The chart
	// derives this from the single `unifiedHost` knob via the
	// gardenio.link.defaultGardenioHost helper, so it stays "" (inert) until an
	// operator flips unifiedHost on or sets agents.defaultGardenioHost — the
	// gated cutover this is COUPLED to (ka-0000004747). See task ka-0000002401:
	// without this, new-prod agent pods set GARDENIO_DEFAULT_HOST nowhere and gb
	// handed humans legacy-host links off the compiled fallback.
	GardenioDefaultHost string

	// --- Grafana ---

	// GrafanaURL is the in-cluster Grafana Service URL (env: GRAFANA_URL).
	// Injected into agent pods as GRAFANA_URL so `gb grafana render` resolves a
	// base without the agent hand-discovering the svc address. Empty = not
	// injected (the agent must pass --grafana-url or set GRAFANA_BASE_URL).
	// See bug ka-0000005842.
	GrafanaURL string

	// GrafanaAuthProxyHeader is the auth-proxy identity header name Grafana
	// trusts (env: GRAFANA_AUTH_PROXY_HEADER, e.g. X-Auth-Request-Email).
	// Injected into agent pods so `gb grafana render` can present the
	// in-cluster auth-proxy identity. Empty = not injected.
	GrafanaAuthProxyHeader string

	// --- Service logs (`gb logs`) ---

	// LogsURL is the in-cluster VictoriaLogs Service BASE URL (env:
	// GARDENIO_LOGS_URL). Injected into agent pods as GARDENIO_LOGS_URL so
	// `gb logs <component>` resolves the log store without the agent
	// hand-discovering the svc address — the same "inject the in-cluster
	// observability base by env" affordance as GrafanaURL/VMAGENT_URL. It is the
	// READ side of the pod-log shippers (which WRITE into the same store), so it
	// is release-relative and wired by the chart to the in-chart vlsingle
	// Service. Empty = not injected (the agent must pass --url or set
	// GARDENIO_LOGS_URL), and gb logs then falls back to the well-known prod
	// Service name. See design ka-0000005579, task ka-0000005209.
	LogsURL string

	// GrafanaEnvAPIKeySecret is the K8s Secret holding env-side Grafana Editor
	// tokens (env: GRAFANA_ENV_API_KEY_SECRET, keys token-{env}). When set,
	// each environment in GrafanaEnvList is injected into agent pods as
	// GRAFANA_API_KEY_<ENV> sourced from this Secret. Distinct from GrafanaURL
	// (in-cluster gardenio Grafana); this reaches the ACME environment Grafana
	// (lens.acme.net etc.). Empty = not injected. See task ka-0000002337.
	GrafanaEnvAPIKeySecret string

	// GrafanaEnvList is the ordered set of environments (env:
	// GRAFANA_ENV_LIST, comma-separated, e.g. "devel,perf,prod") whose Grafana
	// token + base URL are wired into agent pods. Empty = none.
	GrafanaEnvList []string

	// GrafanaEnvBaseURLs maps an environment name to its non-secret Grafana
	// base URL, injected as GRAFANA_BASE_URL_<ENV>. A missing or empty entry
	// omits that env var. Sourced from per-env GRAFANA_BASE_URL_<ENV> vars.
	GrafanaEnvBaseURLs map[string]string

	// --- Stale Agent Reaper ---

	// StaleAgentTimeout is how long an agent bead can go without an update
	// before it is considered stale and its pod is terminated
	// (env: STALE_AGENT_TIMEOUT). Default: 2h. Set to 0 to disable.
	StaleAgentTimeout time.Duration

	// RuntimeCeilingByRole is the ABSOLUTE runtime ceiling per agent primary
	// role, measured from the bead's created_at — NOT from updated_at. It is
	// the reap-side complement to the lease-side age bound (bug
	// ka-0000001095 / MR !6050): the lease bound is non-destructive (a stale
	// holder simply stops counting so the lane fires again) but leaves a
	// genuinely-wedged pod alive, burning spend until an operator notices
	// ($1.01 / 547m on ka-0000001538).
	//
	// The existing StaleAgentTimeout reaper keys every reap on updated_at
	// staleness, an IDLE proxy; a wedged agent that keeps churning (a re-arm/
	// nudge treadmill, a tight self-loop) keeps updated_at fresh every cycle
	// (SyncAll bumps it while coop is non-idle, ka-0000003615) and so never
	// trips the 2h idle rule while its runtime grows to 7-9h — exactly the two
	// confirmed outages (469m/547m). A created_at ceiling is immune to that
	// churn.
	//
	// Originally scoped to spawned_by=trigger:* only (bug ka-0000000716, 6h
	// default from the measured trigger-lease distribution: STRICTLY GREATER
	// than the 3h lease-RELEASE threshold so we only reap what is provably
	// long-dead, 3x above the legitimate long-run cluster, and below the two
	// confirmed outages). Extended to every role (gd-X2eSin2J80): crew/thread/
	// plan-gardener agents — nearly the whole fleet — had no runtime ceiling at
	// all. The per-role defaults below are starting points to be tuned against
	// Phase B age-metric data (gd-5MTDWjwNjm), not final thresholds:
	//
	//   - crew:         6h  (env: STALEREAPER_RUNTIME_MAX_CREW)
	//   - thread:       2h  (env: STALEREAPER_RUNTIME_MAX_THREAD)
	//   - plan-gardener: 12h (env: STALEREAPER_RUNTIME_MAX_PLAN_GARDENER)
	//
	// A role with no entry (or an entry of 0) has no ceiling. This reap is
	// DESTRUCTIVE (it kills a live pod), so it also carries an MR-mid-flight
	// guard and ships behind an operator-armed dry-run tunable
	// (STALEREAPER_RUNTIME_REAP, renamed from STALEREAPER_TRIGGER_RUNTIME_REAP).
	RuntimeCeilingByRole map[string]time.Duration

	// StuckAgentTimeout is how long a Running+Ready agent pod's coop session
	// can report agent_state=working without advancing any liveness signal
	// (since_seq, screen_seq, context_tokens, cost, turns) before the
	// controller publishes an AgentStuck event to restart the pod
	// (env: STUCK_AGENT_TIMEOUT). 0 disables the watchdog entirely.
	//
	// DEFAULT: 0 (DISABLED). The heuristic is off by default until it has
	// demonstrated a false-positive-free record (ka-0000000084). It previously
	// defaulted to 15m and hard-killed a live monorepo-repro agent that was
	// mid-Playwright-run: the pod was healthy (602Mi of an 8Gi request, no
	// OOM, no eviction) and simply had not printed to its terminal for 16m,
	// and the kill destroyed an uncommitted e2e repro spec because crew pods
	// are emptyDir. A watchdog that cannot distinguish "quiet" from "wedged"
	// costs more work than the wedges it clears, so it is opt-in until proven.
	//
	// Re-enable deliberately (e.g. STUCK_AGENT_TIMEOUT=15m) once the liveness
	// signals are trustworthy — see the costUSD note on stagnationEntry.
	StuckAgentTimeout time.Duration

	// StuckLongLivedMultiplier scales StuckAgentTimeout for long-lived
	// orchestrator roles (gardener / plan-gardener), which legitimately produce
	// no terminal output for long stretches while orchestrating child agents or
	// hosting a Workflow fan-out. Without the longer grace window the stuck
	// heuristic hard-kills these live agents mid-turn — the dominant
	// involuntary-death vector (bug ka-0000002709). Default: 4 (60m at the
	// default base). A value <= 1 disables the role bias
	// (env: STUCK_LONG_LIVED_MULTIPLIER).
	StuckLongLivedMultiplier float64

	// StuckRestartInPlace makes the AgentStuck remedy NON-DESTRUCTIVE: instead
	// of deleting and recreating the pod (which wipes the emptyDir workspace for
	// crew/job pods, an unrecoverable data-loss event — ka-0000001648), the
	// controller first asks coop to restart the wedged agent child IN PLACE via
	// POST /api/v1/session/restart. That force-SIGHUPs the child and respawns it
	// with `--resume`, preserving the pod, its emptyDir workspace, and the
	// conversation. The destructive delete+recreate remains as the fallback for
	// when coop is itself unreachable (the pod genuinely needs recreation).
	//
	// This is the precondition for re-enabling the watchdog at all: a false
	// positive under in-place restart costs a conversation resume, not a lost
	// workspace. Enabled by default; set STUCK_RESTART_IN_PLACE=false to force
	// the legacy delete+recreate remedy (env: STUCK_RESTART_IN_PLACE).
	StuckRestartInPlace bool

	// --- PVC Garbage Collection ---

	// PVCGCInterval is how often the PVC garbage collector runs
	// (env: PVC_GC_INTERVAL). Default: 5m. Set to 0 to disable.
	PVCGCInterval time.Duration

	// PVCGCTTL is the minimum age of an orphaned PVC before it is deleted
	// (env: PVC_GC_TTL). Default: 24h.
	PVCGCTTL time.Duration

	// --- PV Garbage Collection ---

	// PVGCInterval is how often the PV garbage collector runs
	// (env: PV_GC_INTERVAL). Default: 5m. Set to 0 to disable.
	PVGCInterval time.Duration

	// PVGCTTL is the minimum age of a Released PV before it is deleted
	// (env: PV_GC_TTL). Default: 24h.
	PVGCTTL time.Duration

	// PVGCNamespaces is the comma-separated list of namespaces whose PVs
	// are eligible for garbage collection
	// (env: PV_GC_NAMESPACES). Default: the controller's own namespace.
	PVGCNamespaces []string

	// --- Orphaned Pending PVC Observability (ka-0000002737) ---

	// PVCObsInterval is how often the orphaned-Pending-PVC observability
	// collector lists cluster PVCs+pods and publishes gardenio_orphaned_pending_
	// pvcs (+ max-age / requested-bytes / per-PVC age) — the cost-leak signal
	// from the day-1 cost-gardener survey (PVCs stuck Pending for months with no
	// consumer, e.g. observability/tempo-0 ~200d, dolt/redis-master-0 ~148d).
	// (env: PVC_OBS_INTERVAL). Default: 0 (disabled). Opt-in because it needs a
	// cluster-scoped persistentvolumeclaims:list + pods:list grant the controller
	// SA holds only when the chart provisions it (agents.pvcObs.enabled); the
	// deployment sets this env only in lockstep with that RBAC.
	PVCObsInterval time.Duration

	// PVCObsThreshold is how long a PVC must sit Pending with no consumer before
	// it counts as orphaned (env: PVC_OBS_THRESHOLD). Default: 0 → the collector
	// falls back to pvcobs.DefaultThreshold (7d). Raise/lower via the chart
	// (agents.pvcObs.threshold) without a controller change.
	PVCObsThreshold time.Duration

	// --- Node Lifecycle Events (Track D, ka-0000001509) ---

	// NodeLifecycleInterval is how often the node-lifecycle collector lists
	// cluster Nodes and publishes gardenio_node_lifecycle_events_total{reason=...}
	// — the OOMKill/node-lifecycle Slack-alert signal Matt asked for
	// (ka-0000006448). (env: NODE_LIFECYCLE_INTERVAL). Default: 0 (disabled).
	// Opt-in because it needs a cluster-scoped nodes:list/watch grant the
	// controller SA holds only when the chart provisions it
	// (controller.nodeLifecycle.enabled); the deployment sets this env only in
	// lockstep with that RBAC.
	NodeLifecycleInterval time.Duration

	// NodeLifecycleNodeSelector scopes the node-lifecycle sweep to the nodepool
	// agents run on — the metric must measure only churn that can cold-resume an
	// agent, not ephemeral-pool/reaper churn on the consolidated prod cluster
	// (ka-0000003804). (env: NODE_LIFECYCLE_NODE_SELECTOR). Default: "" (list
	// every cluster node — the explicit opt-out). The chart ships
	// karpenter.sh/nodepool=agent-pool as the default (agents.nodeLifecycle
	// .nodeSelector), rendered alongside NODE_LIFECYCLE_INTERVAL.
	NodeLifecycleNodeSelector string

	// NodeLifecycleIdlePodNamespaces are the namespaces the node-lifecycle
	// collector LISTs agent pods in to derive the idle-node gauge
	// gardenio_agent_idle_node_count — agent-pool nodes hosting zero active agent
	// pods (P1b-follow-up idle-node detection, ka-0000001398, the unblocked half
	// of the utilization ask). (env: NODE_LIFECYCLE_IDLE_POD_NAMESPACES, a comma-
	// separated list.) Default: empty (idle-node detection DISABLED — the gauge
	// is not registered or emitted, so no alert evaluates against a phantom
	// series). The chart ships the release namespace (+ the gVisor namespace when
	// configured), where every agent pod carries app.kubernetes.io/name=gardenio;
	// it reuses the controller's EXISTING namespace-scoped pods:list RBAC, so no
	// new grant is needed. Only consulted when NodeLifecycleInterval > 0.
	NodeLifecycleIdlePodNamespaces []string

	// --- Advice Expiration Sweeper ---

	// AdviceSweepInterval is how often the sweeper scans open advice beads
	// and closes those whose expires_at timestamp is in the past
	// (env: GARDENIO_ADVICE_SWEEP_INTERVAL). Default: 5m. Set to 0 to disable.
	AdviceSweepInterval time.Duration

	// AdviceCompactionEnabled gates the supersession-compaction pass of the
	// advice sweeper (R3, ka-0000006409): closing open advice that another open
	// advice bead explicitly supersedes. The expiry pass always runs; this only
	// controls the newer supersession-close mutation so an operator can disable
	// it without disabling expiry (env: GARDENIO_ADVICE_COMPACTION). Default: true.
	AdviceCompactionEnabled bool

	// --- Durable Spend Settlement Sweep ---

	// SpendSweepInterval is how often the durable settlement sweep re-attempts
	// LiteLLM spend settlement for closed-but-unsettled agent beads
	// (env: GARDENIO_SPEND_SWEEP_INTERVAL). Default: 30m. Set to 0 to disable.
	SpendSweepInterval time.Duration

	// --- Cost-Audit Cross-Check Sweep (C2) ---

	// XCheckSweepInterval is how often the cost-audit cross-check sweep
	// recomputes its conservation pairs and republishes
	// gardenio_xcheck_value / gardenio_xcheck_agreement
	// (env: GARDENIO_XCHECK_SWEEP_INTERVAL). Default: 15m. Set to 0 to disable.
	XCheckSweepInterval time.Duration

	// SessionReaperInterval is how often the session reaper closes open session
	// beads whose agent bead is closed or missing and republishes
	// gardenio_orphan_open_sessions (gd-c6muILNedN)
	// (env: GARDENIO_SESSION_REAPER_INTERVAL). Default: 10m. Set to 0 to disable.
	SessionReaperInterval time.Duration

	// ClaudeStatusPollInterval is how often the Claude status-page poller
	// refreshes gardenio_claude_status_* — leg B of the Claude-outage detector
	// (epic ka-0000000487), the corroborating half that answers "does the
	// provider admit it?" next to leg A's "are our agents failing?"
	// (env: GARDENIO_CLAUDE_STATUS_POLL_INTERVAL). Default: 60s against a static
	// CDN-fronted JSON endpoint. Set to 0 to disable.
	ClaudeStatusPollInterval time.Duration

	// ClaudeStatusURL overrides the status endpoint the poller reads
	// (env: GARDENIO_CLAUDE_STATUS_URL). Default: the status.claude.com
	// /api/v2/summary.json summary endpoint. Override only to point at a stub
	// (tests, air-gapped envs); status.anthropic.com also works since the client
	// follows its 302, but the canonical host avoids depending on that redirect.
	ClaudeStatusURL string

	// ClaudeStatusFallbackURLs is an ordered, comma-separated list of alternate
	// status endpoints the poller tries — in order — ONLY when the primary URL's
	// fetch fails (env: GARDENIO_CLAUDE_STATUS_FALLBACK_URLS). It closes the
	// "poller has no fallback" single-point-of-failure the leg-B dark bugs keep
	// hitting (ka-0000008266 registry, ka-0000007040 provider TLS cert). Default
	// EMPTY: as of 2026-08-14 no alternate host serves summary.json
	// unauthenticated with a valid cert (the canonical Statuspage slug
	// tymt9n04zgry.statuspage.io was deactivated on custom-domain migration and
	// 401s "page is inactive"), so a fallback is wired in the moment Anthropic
	// exposes a valid-cert endpoint — no redeploy needed.
	ClaudeStatusFallbackURLs []string

	// --- Postgres efficiency report (gd-ylgk1jBhLi) ---

	// PgBudgetReportHour is the UTC hour of the daily Postgres efficiency
	// report to the land's #machines, which also files tasks for budget
	// breaches (env: GARDENIO_PG_BUDGET_REPORT_HOUR). Negative, the default,
	// disables it; the chart sets it with postgres.exporter.report.
	PgBudgetReportHour int
	// PgBudgetLand is the land whose #machines gets the report and whose
	// project the tasks are filed in (env: GARDENIO_PG_BUDGET_LAND).
	PgBudgetLand string
	// PgBudgetEpic, when set, parents the filed tasks
	// (env: GARDENIO_PG_BUDGET_EPIC).
	PgBudgetEpic string
	// PgBudgets is vmalert.postgresBudgets as JSON, so the report judges a
	// day by the thresholds the alert rules use (env: GARDENIO_PG_BUDGETS;
	// empty is pgbudget.DefaultBudgets).
	PgBudgets string

	// MetricsQueryURL is the vmsingle instant-query base URL the cross-check
	// sweep reads its PromQL-backed pairs from (env: VMSINGLE_QUERY_URL,
	// defaulting to the in-cluster gardenio-vmsingle Service). Mirrors the
	// beads-viewer's VMSINGLE_QUERY_URL wiring so both read the same upstream.
	// Empty disables the PromQL-backed pairs (the bead-scan pairs still run).
	MetricsQueryURL string

	// --- Spend-Logs Sweep (C3, stateless per-agent attribution) ---

	// SpendLogsSweepInterval is how often the spend-logs sweep recomputes
	// per-agent cost from LiteLLM /spend/logs and republishes
	// gardenio_agent_spend_logs_usd + the spend_logs_cost_usd bead fields
	// (env: GARDENIO_SPENDLOGS_SWEEP_INTERVAL). Default: 30m. Set to 0 to
	// disable. Each cycle pages the window's raw request rows (~130 pages /
	// ~100 MB per fleet-day at ka-0000001899 probe volume), hence the slower
	// default cadence than the 5m gauge pollers.
	SpendLogsSweepInterval time.Duration

	// SpendLogsSweepLookback is the projection window length: only agent
	// beads created inside the window are attributed (full-life coverage —
	// see spendlogs_sweep.go). Longer windows track longer-lived agents but
	// page proportionally more rows per cycle
	// (env: GARDENIO_SPENDLOGS_SWEEP_LOOKBACK). Default: 48h.
	SpendLogsSweepLookback time.Duration

	// --- Bridge Fleet Token Rotator ---

	// BridgeTokenRotationEnabled is the master switch for the bridge fleet
	// token rotator. When false, the rotator's RunLoop is a no-op
	// (env: BRIDGE_TOKEN_ROTATION_ENABLED). Default: false.
	BridgeTokenRotationEnabled bool

	// BridgeTokenRotationDryRun, when true, logs "would rotate" decisions but
	// does not write to the daemon or K8s. Used to verify candidate detection
	// in preprod before flipping live (env: BRIDGE_TOKEN_ROTATION_DRY_RUN).
	// Default: true (safe default: observe-only).
	BridgeTokenRotationDryRun bool

	// BridgeTokenRotationInterval controls how often the rotator checks each
	// fleet's remaining TTL (env: BRIDGE_TOKEN_ROTATION_INTERVAL). Default: 1h.
	BridgeTokenRotationInterval time.Duration

	// BridgeTokenRotationThreshold is the remaining-TTL window at which a
	// rotation fires (env: BRIDGE_TOKEN_ROTATION_THRESHOLD). Default: 336h (14d).
	BridgeTokenRotationThreshold time.Duration

	// BridgeTokenRotationAccessTTL is the TTL requested for newly minted
	// access tokens (env: BRIDGE_TOKEN_ROTATION_ACCESS_TTL). Default: 720h (30d).
	BridgeTokenRotationAccessTTL time.Duration

	// BridgeTokenRotationRefreshTTL is the TTL requested for the companion
	// refresh tokens (env: BRIDGE_TOKEN_ROTATION_REFRESH_TTL). Default: 1440h (60d).
	BridgeTokenRotationRefreshTTL time.Duration

	// BridgeTokenRotationSecret is the K8s Secret name that holds the per-fleet
	// access+refresh tokens (env: BRIDGE_TOKEN_ROTATION_SECRET).
	// Default: bridge-fleet-tokens.
	BridgeTokenRotationSecret string

	// BridgeTokenRotationFleetMapKey is the secret key that holds the
	// BEADS_FLEET_MAP env value (env: BRIDGE_TOKEN_ROTATION_FLEET_MAP_KEY).
	// Default: fleet-map.
	BridgeTokenRotationFleetMapKey string

	// BridgeTokenRotationFleets lists the fleets the rotator manages
	// (env: BRIDGE_TOKEN_ROTATION_FLEETS, comma-separated).
	// Default: slack-bridge,gitlab-bridge,jira-bridge.
	BridgeTokenRotationFleets []string

	// BridgeTokenRotationPrivilegedFleets lists the fleets whose rotated
	// tokens are designated PRIVILEGED service-fleet identities — they may
	// bypass per-bead ownership for system-managed graph writes
	// (produces/triggered-by deps, mr_url back-fill). Must be a subset of
	// BridgeTokenRotationFleets (env: BRIDGE_TOKEN_ROTATION_PRIVILEGED_FLEETS,
	// comma-separated). Default: gitlab-bridge. See ka-0000001946.
	BridgeTokenRotationPrivilegedFleets []string

	// --- Controller ---

	// LogLevel controls log verbosity: debug, info, warn, error (env: LOG_LEVEL).
	LogLevel string

	// --- Runtime (not from env) ---

	// ProjectCacheMu protects ProjectCache for concurrent read/write access.
	// The periodic sync goroutine writes via refreshProjectCache while the
	// event loop goroutine reads via buildAgentPodSpec.
	ProjectCacheMu sync.RWMutex

	// ProjectCache maps project name → metadata, populated at runtime from project beads
	// in the daemon. Not parsed from env. Protected by ProjectCacheMu.
	ProjectCache map[string]ProjectCacheEntry

	// AccountCache maps account name → settings, populated at runtime from type:account
	// config beads. Protected by ProjectCacheMu (refreshed together with projects).
	AccountCache map[string]AccountCacheEntry

	// AccountLiveCountsMu protects AccountLiveCounts for concurrent read/write.
	// The periodic sync goroutine replaces the snapshot via refreshAccountLiveCounts
	// while the spawn path reads it (and optimistically bumps it) for spread
	// account selection.
	AccountLiveCountsMu sync.RWMutex

	// AccountLiveCounts maps account name → number of running agent pods currently
	// baked with that account's LiteLLM key. Snapshot refreshed once per periodic
	// sync cycle from the live pod list, and optimistically incremented at spawn so
	// a burst of spawns within one window still spreads across the pool. Read by
	// selectAccount. Protected by AccountLiveCountsMu.
	AccountLiveCounts map[string]int

	// RoleCacheMu protects RoleCache for concurrent read/write access.
	// The periodic sync goroutine writes via refreshRoleCache while the
	// event loop goroutine reads via buildAgentPodSpec.
	RoleCacheMu sync.RWMutex

	// RoleCache maps role name → overrides, populated at runtime from type:role
	// config beads in the daemon. Not parsed from env. Protected by RoleCacheMu.
	RoleCache map[string]RoleCacheEntry

	// ServedModelsMu protects ServedModels for concurrent read/write access.
	// The served-model sync goroutine replaces the snapshot via SetServedModels
	// while the spawn path reads it (SnapshotServedModels) for the spread-pool
	// served-model gate (ka-0000002790).
	ServedModelsMu sync.RWMutex

	// ServedModels is the set of model names LiteLLM currently serves (the union
	// of /v1/models group ids and model_group_alias keys), refreshed periodically
	// by servedModelSync. It is the authoritative "what an agent can actually
	// call" set that the spread selector checks an account's baked model against:
	// an account whose model is NOT served can never satisfy a first inference
	// turn, so baking it as cognition crash-loops the pod (the glm-bedrock
	// incident, ka-0000002790). nil means UNKNOWN (the sync has not run or is
	// disabled) — the gate is fail-open on nil, so an unknown set never excludes
	// any account. Protected by ServedModelsMu.
	ServedModels map[string]bool
}

// AccountCacheEntry holds account settings from daemon type:account config beads.
// Projects reference an account by name to inherit LLM provider routing config.
type AccountCacheEntry struct {
	Provider string // Provider name (e.g. "anthropic", "openai")
	BaseURL  string // API endpoint override (e.g. LiteLLM proxy URL)
	// ProxyURL, when set, points ANTHROPIC_BASE_URL at a fronting proxy (e.g.
	// claudemore) INSTEAD of BaseURL. Empty = legacy direct-to-litellm routing.
	// Clearing it is the claudemore instant-rollback lever (ka-0000007035).
	ProxyURL   string
	Credential string // Token source: "coopmux:<name>", "secret:<name>", "env:<var>"
	Model      string // Default model for agents using this account
	MaxBudget  string // Per-session budget hint
	Fallback   string // Fallback account name when budget is exhausted
	LiteLLMKey string // LiteLLM virtual key for budget tracking
	Status     string // Rotation lifecycle: "active" (default), "exhausted", "cooldown"

	// ThrottleStatus is the live rate-limit signal from the probe: "" (healthy),
	// "allowed_warning" (near limit — usable but de-prioritized for spread),
	// "rejected" (currently rate-limited — excluded from spread). Distinct from
	// Status, which tracks the slower budget-exhaustion lifecycle.
	ThrottleStatus string

	// ThrottleResetAt is the probe-written RFC3339 timestamp at which this
	// account's binding rate-limit window resets (field throttle_reset_at).
	// Empty when unknown. Used only for the operator-facing "blocking new
	// spawns until <reset>" message the pool-exhaustion escape hatch emits when
	// every account in a project pool is simultaneously exhausted (ka-0000002560).
	ThrottleResetAt string

	// Utilization5h / Utilization7d are the probe-written fractional
	// utilizations ("0.0500") of the account's 5-hour and 7-day rate-limit
	// windows; ThrottleReset5hAt / ThrottleReset7dAt are the RFC3339 reset
	// times of those windows; RepresentativeClaim ("five_hour" | "seven_day")
	// names the BINDING window. The spread selector's capacity+reset bonus
	// (ka-0000001522) reads these to bias new pods toward accounts with unused
	// capacity in a window that resets soon (use-it-or-lose-it). Raw strings,
	// parsed where used — empty when the probe has not run.
	Utilization5h       string
	Utilization7d       string
	ThrottleReset5hAt   string
	ThrottleReset7dAt   string
	RepresentativeClaim string

	// SpawnWeight is the operator-set manual spread bias (field spawn_weight,
	// numeric string). It is subtracted from the account's effective load in
	// the spread ranking: positive values attract new pods (the account ranks
	// as if it carried that many fewer live pods); negative values repel them.
	// It biases RANKING only — health gating is untouched (a rejected /
	// exhausted / keyless account stays excluded regardless). Unlike the
	// capped auto heuristic, this knob is deliberately uncapped: an operator
	// setting spawn_weight=50 has chosen to dogpile that account
	// (ka-0000001522). Empty when unset.
	SpawnWeight string

	// BillingType is how the provider bills this account (bead field
	// billing_type): "subscription" (Claude Max OAuth — our fleet) or "metered"
	// (pay-as-you-go API credits). Empty means subscription, matching the
	// probe-side billingType() default. The spawn-path overage gate keys off
	// this: for a subscription account the overage headers describe pay-as-you-go
	// availability only and say NOTHING about whether the subscription quota can
	// serve, so no overage reason may block a spawn (ka-0000003769,
	// ka-0000004929, and the 2026-08-28 org_level_disabled outage).
	BillingType string

	// OverageDisabledReason is the probe's reason the account's overage/billing
	// is blocked, e.g. "out_of_credits" or "org_spend_cap_reached". A non-empty
	// value means the account cannot bill successful requests even though it may
	// be throttle-allowed and have a litellm_key — so it must be excluded from
	// the spawn account pool (ka-0000003769). Empty means billing is allowed.
	OverageDisabledReason string

	// ProbeAuthStatus is the read-only probe's terminal-auth verdict, distinct
	// from the transient rate-limit ThrottleStatus (field probe_auth_status):
	// "rejected" when the probe got a 401/403 — the account can no longer
	// authenticate at all (revoked OAuth token, or "your organization has
	// disabled Claude subscription access for Claude Code"), which a rolling
	// rate-limit window will NEVER heal; "ok" when the probe authenticated
	// cleanly (200 or a 429 throttle, both of which prove the credential is
	// live). Empty when the probe has not run or last saw a transient
	// infrastructure error (network/5xx) — those must NOT condemn an account.
	//
	// Why it exists (ka-0000002085): pool status="healthy" is not sufficient to
	// pick a rotation switch-target. During the 2026-06-22 incident a manual
	// switch onto matthewbaker1110 (pool-healthy, 5h util=0) hit a HARD error —
	// the account's org had Claude Code access disabled — and the live session
	// died. Gating switch-target selection (selectAccount / fallbackTier /
	// isHealthyAuthorized) on probe_auth_status != "rejected" stops the fleet
	// rotating a live session onto a subscription-dead account.
	ProbeAuthStatus string

	// TotalCostUSD and SpendAtLastReset are the controller-synced cumulative
	// spend ($) on this account's LiteLLM virtual key. spend_sync.RunLoop polls
	// LiteLLM /key/info every 5min and writes total_cost_usd; the account-reset
	// path snapshots spend_at_last_reset / last_reset_at. Their difference is
	// the spend SINCE the last reset (the billing-period spend, mirroring gb
	// usage's since_reset). The spawn path compares this against MaxBudget to
	// gate on budget headroom BELOW the provider's hard ceiling, so a capped
	// account degrades gracefully (redirect to its fallback) instead of 429-ing
	// every agent on turn 1 (ka-0000005505). Empty when the field is unset.
	TotalCostUSD     string
	SpendAtLastReset string

	// Labels carries the raw bead labels (e.g. "project:<name>") so the spawn-time
	// spread selector can enumerate a project's authorized account pool without a
	// second daemon round-trip.
	Labels []string

	// ExclusiveToRole, when set, reserves this account to a single role: only
	// agents whose role matches may be credentialed against it. The spread
	// selector excludes it from every other role's pool, and the exhaustion
	// resolver never offers it as a fallback to a non-matching role; an agent
	// pinned to it (e.g. via a role account pin) stays on it even when exhausted
	// — an exclusive account never spills onto the general pool. This is the
	// data-driven replacement for the hardcoded Bedrock partition (ka-0000005223,
	// policy ka-0000002660): a new exclusive account needs no controller change.
	ExclusiveToRole string

	// ModelGroupLimits is the probe-written per-model-group rate-limit JSON
	// (account bead field model_group_limits, ka-0000003806): Anthropic splits
	// subscription limits per model family, so an account can be Fable-exhausted
	// while its aggregate 5h/7d and Opus window are healthy. The fable-aware
	// landing gate (selectAccount) parses this to exclude an account whose
	// window for the SPEC'S model group is exhausted. Raw JSON string, parsed
	// where used — empty when the probe has not recorded a per-group split (older
	// accounts, or a group Anthropic has not separated).
	ModelGroupLimits string
}

// SnapshotAccountLiveCounts returns a copy of the current per-account live-pod
// count snapshot, safe to read without holding the lock. Used by the spawn-time
// spread selector (ka-0000004861).
func (c *Config) SnapshotAccountLiveCounts() map[string]int {
	c.AccountLiveCountsMu.RLock()
	defer c.AccountLiveCountsMu.RUnlock()
	out := make(map[string]int, len(c.AccountLiveCounts))
	for k, v := range c.AccountLiveCounts {
		out[k] = v
	}
	return out
}

// SetAccountLiveCounts replaces the per-account live-pod count snapshot. Called
// once per periodic sync cycle from the freshly listed agent pods.
func (c *Config) SetAccountLiveCounts(counts map[string]int) {
	c.AccountLiveCountsMu.Lock()
	c.AccountLiveCounts = counts
	c.AccountLiveCountsMu.Unlock()
}

// SnapshotServedModels returns a copy of the current LiteLLM served-model set,
// safe to read without holding the lock. Returns nil when the set is UNKNOWN
// (the served-model sync has not run or is disabled) so the spread selector can
// distinguish "no data → do not gate" from "known-empty". Used by the spread-
// pool served-model gate (ka-0000002790).
func (c *Config) SnapshotServedModels() map[string]bool {
	c.ServedModelsMu.RLock()
	defer c.ServedModelsMu.RUnlock()
	if c.ServedModels == nil {
		return nil
	}
	out := make(map[string]bool, len(c.ServedModels))
	for k, v := range c.ServedModels {
		out[k] = v
	}
	return out
}

// SetServedModels replaces the LiteLLM served-model set. Called once per served-
// model sync cycle from the freshly fetched /v1/models list. A nil or empty set
// is NOT published (the caller guards this) so a transient fetch failure never
// collapses the set to known-empty and starts wrongly excluding accounts.
func (c *Config) SetServedModels(models map[string]bool) {
	c.ServedModelsMu.Lock()
	c.ServedModels = models
	c.ServedModelsMu.Unlock()
}

// BumpAccountLiveCount optimistically increments the live-pod count for an
// account at spawn time, so back-to-back spawns within a single sync window see
// the just-placed pod and continue to spread rather than all landing on the
// same least-loaded account (ka-0000004861). The next periodic
// SetAccountLiveCounts reconciles the snapshot against ground truth. A no-op for
// the empty account name.
func (c *Config) BumpAccountLiveCount(account string) {
	if account == "" {
		return
	}
	c.AccountLiveCountsMu.Lock()
	if c.AccountLiveCounts == nil {
		c.AccountLiveCounts = make(map[string]int)
	}
	c.AccountLiveCounts[account]++
	c.AccountLiveCountsMu.Unlock()
}

// IsPilotFreezeClass reports whether role is the single opt-in gVisor
// freeze/thaw pilot class (PilotFreezeClass). Matching mirrors
// FreezeRoleAllowed's comma→dash normalization, so a class named either as a raw
// bead role ("plan-gardener") or via a label-sanitized value matches at both the
// spawn seam and the controller's shouldFreeze seam. An empty PilotFreezeClass
// (pilot off) or empty role never matches. This is a pure name check — it does
// NOT consult FreezeThawEnabled; the runsc-pin call site (applyPilotFreezeClass)
// gates on FreezeThawEnabled so the pilot pin only takes effect when the freeze
// machinery is live.
func (c *Config) IsPilotFreezeClass(role string) bool {
	if c.PilotFreezeClass == "" || role == "" {
		return false
	}
	norm := func(s string) string {
		return strings.ReplaceAll(strings.TrimSpace(s), ",", "-")
	}
	return norm(role) == norm(c.PilotFreezeClass)
}

// FreezeRoleAllowed reports whether an agent's role is permitted to
// freeze/thaw under the WS3 canary allowlist (FreezeThawRoleAllowlist). An
// empty allowlist means "no role filter" — every role is allowed, preserving
// the pre-WS3 behaviour where freeze-eligibility keyed only on runsc +
// FreezeThawEnabled. A non-empty allowlist narrows freezing to exactly the
// listed roles, so a runsc agent whose role is absent keeps the cold-start
// lifecycle even though the substrate and lifecycle flags are on. This is the
// per-role dial that lets the operator widen freeze/thaw one role at a time
// (task ka-0000000138).
//
// Matching is exact on the whole role value, tolerant of the comma→dash
// sanitization K8s labels apply: the spawn-time seam sees the raw bead role
// ("plan-gardener", "thread", or a multi-role "crew,thread"), while the
// controller's shouldFreeze seam reads the pod's gardenio.io/role LABEL, which
// is SanitizeRole'd ("crew-thread"). Normalizing both sides (comma→dash) makes
// an allowlist entry authored either way match at both seams. Single-component
// roles like the WS3 targets ("plan-gardener", "thread", "gvisor-poc") are
// unaffected by the normalization. An empty role never matches a non-empty
// allowlist (nothing to match against).
func (c *Config) FreezeRoleAllowed(role string) bool {
	// The pilot class is always freeze-allowed: PILOT_FREEZE_CLASS is a
	// self-contained opt-in (see PilotFreezeClass), so selecting the class must
	// permit its freeze even when a non-empty FreezeThawRoleAllowlist narrows
	// every OTHER role. This never widens beyond the one named class.
	if c.IsPilotFreezeClass(role) {
		return true
	}
	if len(c.FreezeThawRoleAllowlist) == 0 {
		return true
	}
	if role == "" {
		return false
	}
	norm := func(s string) string {
		return strings.ReplaceAll(strings.TrimSpace(s), ",", "-")
	}
	roleNorm := norm(role)
	for _, allowed := range c.FreezeThawRoleAllowlist {
		if roleNorm == norm(allowed) {
			return true
		}
	}
	return false
}

// AgentSecretAllowed reports whether a project, role or account bead may put
// the K8s Secret secretName into an agent pod (see AgentSecretAllowlist). An
// empty allow-list allows every name. Matching is case-insensitive; a malformed
// pattern matches nothing.
func (c *Config) AgentSecretAllowed(secretName string) bool {
	if len(c.AgentSecretAllowlist) == 0 {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(secretName))
	if name == "" {
		return false
	}
	for _, pattern := range c.AgentSecretAllowlist {
		if ok, err := path.Match(strings.ToLower(pattern), name); err == nil && ok {
			return true
		}
	}
	return false
}

// ProjectCacheEntry holds project metadata from daemon project beads.
type ProjectCacheEntry struct {
	Prefix        string // e.g., "kd", "bot"
	GitURL        string // e.g., "https://gardenio/kbeads.git"
	DefaultBranch string // e.g., "main"

	// Per-project pod customization (from project bead labels).
	Image          string // Override agent image for this project
	StorageClass   string // Override PVC storage class
	ServiceAccount string // Override K8s ServiceAccount for this project's agents
	// RuntimeClassName pins this project's pods to a K8s RuntimeClass (e.g.
	// "runsc" for gVisor); empty = cluster default. Role and per-agent
	// overrides supersede it (precedence: global < project < role < agent).
	RuntimeClassName string
	DockerEnabled    bool // Enable Docker-in-Docker sidecar for this project's agents
	PostgresEnabled  bool // Enable PostgreSQL sidecar for this project's agents
	NeedsK8sAPI      bool // Mount ServiceAccount token (default false; see ka-0000006344)

	// OpenZiti per-pod overlay access (epic ka-0000000990). Mirrors the
	// beadsapi.ProjectInfo Ziti* fields.
	ZitiAccessEnabled bool
	ZitiServiceID     string
	ZitiBind          bool
	ZitiTTL           string
	ZitiOptional      bool

	// ExecutionTarget selects the runtime backend for this project's agents
	// (""/"kubernetes" = pod path; "lambda_microvm" = AWS Lambda MicroVM backend,
	// epic ka-0000005010). Role and per-agent overrides supersede it
	// (global < project < role < agent). Mirrors beadsapi.ProjectInfo.ExecutionTarget.
	ExecutionTarget string

	// SessionResume opts this project's agents in/out of coop --resume on pod
	// restart. nil = unset (default true when WorkspaceStorage is configured).
	SessionResume *bool

	// Ultracode opts this project's agents in/out of ultracode/workflow mode.
	// nil = unset (defer to the pod's resolved claude-settings); &true / &false
	// = explicit opt-in/out. Role and per-agent overrides supersede it
	// (global < project < role < agent). Mirrors beadsapi.ProjectInfo.Ultracode.
	Ultracode *bool

	// Tier 1: resource overrides (Kubernetes quantity strings, e.g. "500m", "1Gi").
	// Zero value means "use the global default".
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string

	// EnvOverrides holds extra env vars to inject into agent pods for this project.
	// Applied before controller-level config; pod-level metadata takes precedence.
	EnvOverrides map[string]string

	// Account references a type:account bead by name. Resolved at pod creation
	// time to configure ANTHROPIC_BASE_URL, model, and LiteLLM headers.
	Account string

	// Model overrides the model for this project's agents independent of the
	// account, applied after applyAccountConfig (precedence global < project <
	// role < agent). Empty = inherit the account's default model. Lets one
	// account back multiple models without cloning account beads (ka-0000005488).
	Model string

	// Harness selects the agent CLI ("claude", "codex" or "gemini") for this project's
	// agents. Empty = inherit Config.DefaultHarness. Role and agent override it
	// (resolved by resolveHarness in cmd/controller/podspec.go).
	Harness string

	// Per-project secret overrides (merged with globals at pod creation).
	Secrets []beadsapi.SecretEntry
	// Per-project plain env vars (non-secret config like JIRA_BASE_URL).
	EnvVars []beadsapi.EnvEntry
	// Multi-repo definitions (primary + reference repos).
	Repos []beadsapi.RepoEntry
	// Per-project prewarmed agent pool config (nil = disabled for this project).
	PrewarmedPool *beadsapi.PrewarmedPoolConfig

	// LandKind mirrors beadsapi.ProjectInfo.LandKind ("" / "operator" /
	// "tenant"). The spawn wall skips its live reads for a cached operator land.
	LandKind string
	// Namespace mirrors beadsapi.ProjectInfo.Namespace: a tenant land's
	// land-<land>, where the land fence places its pods (podspec.go).
	Namespace string
	// NatsEpoch mirrors beadsapi.ProjectInfo.NatsEpoch, the tenant land's
	// NATS password epoch (landNatsEpoch).
	NatsEpoch string

	// GithubApp and GithubInstallationID mirror beadsapi.ProjectInfo: which
	// GitHub App mints this project's tokens and its pinned installation
	// (cmd/controller/github_app_credentials.go resolveGithubApp).
	GithubApp            string
	GithubInstallationID int64
}

// IsOperatorLand mirrors beadsapi.ProjectInfo.IsOperatorLand.
func (e ProjectCacheEntry) IsOperatorLand() bool {
	return e.LandKind == "" || e.LandKind == beadsapi.LandKindOperator
}

// IsTenantLand reports a land_kind=tenant project. Any other land_kind than
// operator counts, so a kind this controller does not know is fenced too.
func (e ProjectCacheEntry) IsTenantLand() bool { return !e.IsOperatorLand() }

// RoleCacheEntry holds role-level pod overrides from daemon type:role config beads.
// These sit between project-level and agent-level specificity in the layering order:
// global < project < role < agent.
type RoleCacheEntry struct {
	// Per-role pod customization (mirrors ProjectCacheEntry fields).
	Image            string // Override agent image for this role
	StorageClass     string // Override PVC storage class
	ServiceAccount   string // Override K8s ServiceAccount
	RuntimeClassName string // Pin pods to a K8s RuntimeClass (e.g. "runsc" for gVisor); empty = cluster default
	DockerEnabled    bool   // Enable Docker-in-Docker sidecar
	PostgresEnabled  bool   // Enable PostgreSQL sidecar
	NeedsK8sAPI      bool   // Mount ServiceAccount token (default false; see ka-0000006344)
	MachineClient    string // Machine client whose secret is mounted, if allowlisted (AgentMachineClientMounts)
	Verifier         bool   // Run as VerifierServiceAccount with its token, unless ServiceAccount is set (P9.2)

	// OpenZiti per-pod overlay access (epic ka-0000000990). Mirrors the
	// beadsapi.RoleInfo Ziti* fields; role takes precedence over project.
	ZitiAccessEnabled bool
	ZitiServiceID     string
	ZitiBind          bool
	ZitiTTL           string
	ZitiOptional      bool

	// ExecutionTarget selects the runtime backend for agents with this role
	// (""/"kubernetes" = pod path; "lambda_microvm" = AWS Lambda MicroVM backend,
	// epic ka-0000005010). Role takes precedence over project; per-agent metadata
	// overrides both. Mirrors beadsapi.RoleInfo.ExecutionTarget.
	ExecutionTarget string

	// SessionResume opts agents with this role in/out of coop --resume on pod
	// restart. nil = unset (falls back to project setting); role takes
	// precedence over project when both are set.
	SessionResume *bool

	// Ultracode opts agents with this role in/out of ultracode/workflow mode.
	// nil = unset (falls back to the project setting); &true / &false = explicit
	// opt-in/out. Role takes precedence over project; per-agent metadata
	// overrides both (global < project < role < agent). Mirrors
	// beadsapi.RoleInfo.Ultracode — this is where the gardenio-microvms role opts
	// its role-default agents into ultracode (ka-0000001714).
	Ultracode *bool

	// Resource overrides (Kubernetes quantity strings, e.g. "500m", "1Gi").
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string

	// Per-role secret overrides (merged with project secrets at pod creation).
	Secrets []beadsapi.SecretEntry
	// Per-role plain env vars (non-secret config).
	EnvVars []beadsapi.EnvEntry
	// Per-role extra env vars from env_json field.
	EnvOverrides map[string]string
	// Per-role Claude config overrides (layered between project and agent).
	ClaudeConfig map[string]json.RawMessage

	// Account references a type:account bead by name. When set, overrides the
	// project's account during applyAccountConfig. Used to pin a role (e.g.
	// fsm-triage) to a specific LLM provider regardless of which project the
	// agent runs in.
	Account string

	// Model overrides the model for agents with this role independent of the
	// account, applied after applyAccountConfig (precedence global < project <
	// role < agent; role supersedes project). Empty = inherit the account's
	// default model. Lets one account back multiple models without cloning
	// account beads — e.g. thread-fable = {account: gardenio, model:
	// claude-fable-5-oauth} (ka-0000005488).
	Model string

	// Harness selects the agent CLI ("claude", "codex" or "gemini") for agents with this
	// role. Empty = inherit the project's harness. Role supersedes project.
	Harness string

	// AccountWhitelist restricts which accounts a pod with this role may use.
	// When non-empty, the project account pool is intersected with this list
	// before spread selection; an empty intersection is fail-closed (no account
	// is baked) so a role pinned to a restricted pool can never be credentialed
	// against an account outside it. Empty/nil imposes no restriction. Mirrors
	// the gb-init resolveProjectAccount whitelist filter, which became a runtime
	// no-op once the controller pins BOAT_ACCOUNT (ka-0000003061).
	AccountWhitelist []string

	// NoAccountSpill opts a role OUT of the role-pin exhaustion spill
	// (ka-0000001307). Normally, when a role's pinned account is
	// capacity-exhausted at spawn time and the project has other healthy
	// accounts, applyAccountConfig spills to the healthy spread pool instead of
	// parking the agent queued indefinitely. A role that MUST stay on its pinned
	// account (e.g. a cost/model-locked lane where landing on a Claude Max
	// account would be wrong) sets no_account_spill=true to keep the old
	// block-and-wait behavior. Exclusive-account roles (exclusive_to_role) never
	// spill regardless — this flag is for a plain (non-exclusive) role pin.
	// Empty/false = spill allowed (the safer default). Parsed from the
	// "no_account_spill" field on role beads.
	NoAccountSpill bool
}

// Parse reads configuration from environment variables.
func Parse() *Config {
	return &Config{
		// Kubernetes
		Namespace:  envOr("NAMESPACE", "gardenio"),
		Env:        os.Getenv("GARDENIO_ENV"),
		KubeConfig: os.Getenv("KUBECONFIG"),

		// Beads Daemon
		BeadsGRPCAddr:    envOr("BEADS_GRPC_ADDR", "localhost:9090"),
		BeadsHTTPAddr:    envOr("BEADS_HTTP_ADDR", "localhost:8080"),
		BeadsE2EHTTPAddr: os.Getenv("BEADS_E2E_HTTP_ADDR"),
		BeadsTokenSecret: os.Getenv("BEADS_TOKEN_SECRET"),

		// NATS Event Bus
		NatsURL:             os.Getenv("NATS_URL"),
		NatsToken:           os.Getenv("NATS_TOKEN"),
		NatsUser:            os.Getenv("NATS_USER"),
		NatsTokenSecret:     os.Getenv("NATS_TOKEN_SECRET"),
		NatsAgentUserSecret: os.Getenv("NATS_AGENT_USER_SECRET"),

		// Agent Pods
		CoopImage:      os.Getenv("COOP_IMAGE"),
		InitCloneImage: os.Getenv("INIT_CLONE_IMAGE"),

		AgentStartupProbeFailureThreshold: envIntOr("AGENT_STARTUP_PROBE_FAILURE_THRESHOLD", 0),
		// -1 sentinel = env unset -> use the podmanager compiled default; a
		// value of 0 explicitly disables the clone-storm jitter (ka-0000000995).
		AgentInitCloneJitterMaxSeconds: envIntOr("AGENT_INIT_CLONE_JITTER_MAX_SECONDS", -1),
		DindImage:                      os.Getenv("DIND_IMAGE"),
		AgentPullPolicy:                os.Getenv("AGENT_PULL_POLICY"),
		PrewarmedPriorityClass:         os.Getenv("PREWARMED_PRIORITY_CLASS"),
		InteractivePriorityClass:       os.Getenv("INTERACTIVE_PRIORITY_CLASS"),
		ScheduledPriorityClass:         os.Getenv("SCHEDULED_PRIORITY_CLASS"),
		AgentPriorityClass:             os.Getenv("AGENT_PRIORITY_CLASS"),
		AgentCoverageEnabled:           envBoolOr("GARDENIO_AGENT_COVERAGE", false),
		AgentCoverageClaim:             os.Getenv("GARDENIO_AGENT_COVERAGE_CLAIM"),
		AgentCoverageMount:             os.Getenv("GARDENIO_AGENT_COVERAGE_MOUNT"),
		ImagePullSecrets:               parseCommaSeparated(os.Getenv("IMAGE_PULL_SECRETS")),
		CoopServiceAccount:             os.Getenv("COOP_SERVICE_ACCOUNT"),
		CoopMaxPods:                    envIntOr("COOP_MAX_PODS", 10),
		CoopMaxPodsPerProject:          envIntOr("COOP_MAX_PODS_PER_PROJECT", 6),
		CoopMaxPodsGVisor:              envIntOr("COOP_MAX_PODS_GVISOR", 0),
		CoopBurstLimit:                 envIntOr("COOP_BURST_LIMIT", 3),
		CoopRateLimitWindow:            envDurationOr("COOP_RATE_LIMIT_WINDOW", 5*time.Minute),
		CoopRateLimitMax:               envIntOr("COOP_RATE_LIMIT_MAX", 20),
		CoopCircuitBreakerThreshold:    envIntOr("COOP_CIRCUIT_BREAKER_THRESHOLD", 20),
		CoopCircuitBreakerWindow:       envDurationOr("COOP_CIRCUIT_BREAKER_WINDOW", 2*time.Minute),
		CoopCircuitBreakerCooldown:     envDurationOr("COOP_CIRCUIT_BREAKER_COOLDOWN", 5*time.Minute),
		SpawnGuardRateMax:              envIntOr("SPAWN_GUARD_RATE_MAX", 30),
		SpawnGuardRateWindow:           envDurationOr("SPAWN_GUARD_RATE_WINDOW", time.Minute),
		SpawnGuardTripThreshold:        envIntOr("SPAWN_GUARD_TRIP_THRESHOLD", 120),
		SpawnGuardTripWindow:           envDurationOr("SPAWN_GUARD_TRIP_WINDOW", time.Minute),
		SpawnGuardTripCooldown:         envDurationOr("SPAWN_GUARD_TRIP_COOLDOWN", 5*time.Minute),
		CoopQueueExpiryTimeout:         envDurationOr("COOP_QUEUE_EXPIRY_TIMEOUT", time.Hour),
		CoopSyncInterval:               envDurationOr("COOP_SYNC_INTERVAL", 60*time.Second),
		AgentStorageClass:              os.Getenv("AGENT_STORAGE_CLASS"),
		ClaudeModel:                    os.Getenv("CLAUDE_MODEL"),
		AgentDefaultFableModel:         envLookupOr("AGENT_DEFAULT_FABLE_MODEL", "claude-fable-5-1-oauth"),
		AgentDefaultOpusModel:          envLookupOr("AGENT_DEFAULT_OPUS_MODEL", "claude-opus-5-5-oauth"),
		AgentDefaultHaikuModel:         envLookupOr("AGENT_DEFAULT_HAIKU_MODEL", "anthropic/claude-haiku-4-5"),
		DefaultHarness:                 envLookupOr("AGENT_DEFAULT_HARNESS", "claude"),
		GeminiHarnessEnabled:           envBoolOr("GEMINI_HARNESS_ENABLED", false),
		AgentVmagentURL:                os.Getenv("AGENT_VMAGENT_URL"),
		ClaudeTeamsEnabled:             envBoolOr("CLAUDE_TEAMS_ENABLED", false),
		ClaudeTeammateMode:             envOr("CLAUDE_TEAMMATE_MODE", ""),
		ClaudeTeamsMaxTeammates:        envIntOr("CLAUDE_TEAMS_MAX_TEAMMATES", 0),
		ClaudeTeamsCPURequest:          os.Getenv("CLAUDE_TEAMS_CPU_REQUEST"),
		ClaudeTeamsCPULimit:            os.Getenv("CLAUDE_TEAMS_CPU_LIMIT"),
		ClaudeTeamsMemoryRequest:       os.Getenv("CLAUDE_TEAMS_MEMORY_REQUEST"),
		ClaudeTeamsMemoryLimit:         os.Getenv("CLAUDE_TEAMS_MEMORY_LIMIT"),

		// Channel Delivery
		GardenioChannel: envBoolOr("GARDENIO_CHANNEL", false),

		// Per-Agent Tokens
		PerAgentTokens:     envBoolOr("PER_AGENT_TOKENS", false),
		LandsTenantSpawns:  envBoolOr("LANDS_TENANT_SPAWNS", false),
		LandsSpawnsPerHour: envIntOr("LANDS_SPAWNS_PER_HOUR", 12),
		LandsRuntimeClass:  strings.TrimSpace(os.Getenv("LANDS_RUNTIME_CLASS")),
		LandsBridgeURL:     strings.TrimSpace(os.Getenv("LANDS_BRIDGE_URL")),

		// GitHub App per-agent installation tokens (default off)
		GithubAppEnabled:        envBoolOr("GITHUB_APP_ENABLED", false),
		GithubAppID:             strings.TrimSpace(os.Getenv("GITHUB_APP_ID")),
		GithubAppInstallationID: int64(envIntOr("GITHUB_APP_INSTALLATION_ID", 0)),
		GithubAppPrivateKeyFile: envOr("GITHUB_APP_PRIVATE_KEY_FILE", "/etc/gardenio/github-app/private-key"),
		GithubAppAPIURL:         strings.TrimSpace(os.Getenv("GITHUB_APP_API_URL")),

		// gVisor freeze/thaw lifecycle (Phase 4, default off — rollback switch)
		FreezeThawEnabled:                envBoolOr("FREEZE_THAW_ENABLED", false),
		SnapshotS3Bucket:                 strings.TrimSpace(os.Getenv("FREEZE_SNAPSHOT_S3_BUCKET")),
		SnapshotS3Prefix:                 strings.TrimSpace(os.Getenv("FREEZE_SNAPSHOT_S3_PREFIX")),
		FreezeThawRoleAllowlist:          envCSVOr("FREEZE_THAW_ROLE_ALLOWLIST", nil),
		PilotFreezeClass:                 strings.TrimSpace(os.Getenv("PILOT_FREEZE_CLASS")),
		FreezeInterruptEnabled:           envBoolOr("FREEZE_INTERRUPT_ENABLED", false),
		FreezePostPutDelay:               envDurationOr("FREEZE_POST_PUT_DELAY", 0),
		FreezeArmedGuardEnabled:          envBoolOr("FREEZE_ARMED_GUARD_ENABLED", false),
		FreezeArmedWait:                  envDurationOr("FREEZE_ARMED_WAIT", 0),
		ThawRehydrateDeadline:            envDurationOr("THAW_REHYDRATE_DEADLINE", 0),
		PostThawTokenRefreshEnabled:      envBoolOr("POST_THAW_TOKEN_REFRESH_ENABLED", false),
		PostThawTokenRefreshInterval:     envDurationOr("POST_THAW_TOKEN_REFRESH_INTERVAL", 30*time.Minute),
		ThawCreateRestorePodReadyTimeout: envDurationOr("THAW_CREATE_RESTORE_POD_READY_TIMEOUT", 0),
		ThawHardDeadline:                 envDurationOr("THAW_HARD_DEADLINE", 0),
		FreezeHardDeadline:               envDurationOr("FREEZE_HARD_DEADLINE", 0),
		FreezeHardDeadlineInvalid:        envDurationSetButIneffective("FREEZE_HARD_DEADLINE"),
		ThawDrainTimeout:                 envDurationOr("THAW_DRAIN_TIMEOUT", 90*time.Second),
		GVisorNamespace:                  os.Getenv("GVISOR_NAMESPACE"),

		// Reco-only VPA memory stamp (default off — rollback switch, ka-0000004606)
		VPAMemoryStampEnabled: envBoolOr("VPA_MEMORY_STAMP_ENABLED", false),
		VPAObjectNameTemplate: os.Getenv("VPA_OBJECT_NAME_TEMPLATE"),
		VPAMemoryFloor:        os.Getenv("VPA_MEMORY_FLOOR"),
		VPAMemoryMax:          os.Getenv("VPA_MEMORY_MAX"),

		// Warm runsc placeholder ("warm-B") pool (Phase 5, default off — rollback switch)
		WarmPlaceholderPoolEnabled:    envBoolOr("WARM_PLACEHOLDER_POOL_ENABLED", false),
		WarmPlaceholderPoolMinSize:    envIntOr("WARM_PLACEHOLDER_POOL_MIN_SIZE", 0),
		WarmPlaceholderStaleDrainRate: envIntOr("WARM_PLACEHOLDER_STALE_DRAIN_RATE", 0),
		WarmPlaceholderNodeFloor:      envIntOr("WARM_PLACEHOLDER_NODE_FLOOR", 0),
		WarmPlaceholderMaxConcurrent:  envIntOr("WARM_PLACEHOLDER_MAX_CONCURRENT", 0),

		// Secrets & Credentials (only controller-level; per-project secrets via config beads)
		ClaudeOAuthSecret: os.Getenv("CLAUDE_OAUTH_SECRET"),

		SlackViewSessionSecret: os.Getenv("SLACK_VIEW_SESSION_SECRET"),
		SlackViewSessionRoles:  envOr("SLACK_VIEW_SESSION_ROLES", "thread"),

		AgentMachineClientMounts: parseCommaSeparated(os.Getenv("GARDENIO_AGENT_MACHINE_CLIENT_MOUNTS")),
		AgentOIDCDiscoveryURL:    os.Getenv("GARDENIO_AGENT_OIDC_DISCOVERY_URL"),
		AgentOIDCCASecret:        os.Getenv("GARDENIO_AGENT_OIDC_CA_SECRET"),
		VerifierServiceAccount:   strings.TrimSpace(os.Getenv("GARDENIO_VERIFIER_SERVICE_ACCOUNT")),

		// S3 Session Persistence
		CoopS3Bucket:            os.Getenv("COOP_S3_BUCKET"),
		CoopS3Prefix:            os.Getenv("COOP_S3_PREFIX"),
		CoopS3ImagesBucket:      strings.TrimSpace(os.Getenv("COOP_S3_IMAGES_BUCKET")),
		CoopS3Region:            os.Getenv("COOP_S3_REGION"),
		CoopS3Endpoint:          os.Getenv("COOP_S3_ENDPOINT"),
		CoopS3CredentialsSecret: os.Getenv("COOP_S3_CREDENTIALS_SECRET"),

		// Coopmux
		CoopmuxURL:         os.Getenv("COOPMUX_URL"),
		CoopmuxTokenSecret: os.Getenv("COOPMUX_TOKEN_SECRET"),
		// The same variable coopmux reads (landtoken.EnvKeyFile).
		CoopmuxLandTokenKeyFile:       os.Getenv("COOP_MUX_LAND_TOKEN_KEY_FILE"),
		NatsLandCalloutIssuerSeedFile: os.Getenv("NATS_LAND_CALLOUT_ISSUER_SEED_FILE"),
		LandsAccountTemplate:          os.Getenv("LANDS_ACCOUNT_TEMPLATE"),

		// OpenZiti Access Broker (epic ka-0000000990)
		ZitiAccessBrokerURL:           os.Getenv("ZITI_ACCESS_BROKER_URL"),
		ZitiAccessTokenAudience:       os.Getenv("ZITI_ACCESS_TOKEN_AUDIENCE"),
		ZitiTunnelerImage:             os.Getenv("ZITI_TUNNELER_IMAGE"),
		ZitiAccessDefaultTTL:          os.Getenv("ZITI_ACCESS_DEFAULT_TTL"),
		ZitiAccessRevokeOnExit:        envBoolOr("ZITI_ACCESS_REVOKE_ON_EXIT", true),
		ZitiAccessControllerTokenFile: os.Getenv("ZITI_ACCESS_CONTROLLER_TOKEN_FILE"),

		// Lambda MicroVM Backend (epic ka-0000005010, Change C3) — cross-account
		// AWS identity. All default empty/off; the backend is unusable unless
		// MicrovmLauncherRoleARN + MicrovmRegion are both set by the chart.
		MicrovmLauncherRoleARN:                  os.Getenv("MICROVM_LAUNCHER_ROLE_ARN"),
		MicrovmRegion:                           os.Getenv("MICROVM_REGION"),
		MicrovmAssumeRoleSessionName:            envOr("MICROVM_ASSUME_ROLE_SESSION_NAME", "gardenio-microvm-controller"),
		MicrovmSecretPrefix:                     envOr("MICROVM_SECRET_PREFIX", "ziti-microvm-agents/"),
		MicrovmImageIdentifier:                  os.Getenv("MICROVM_IMAGE_IDENTIFIER"),
		MicrovmImageVersion:                     os.Getenv("MICROVM_IMAGE_VERSION"),
		MicrovmExecutionRoleARN:                 os.Getenv("MICROVM_EXECUTION_ROLE_ARN"),
		MicrovmStaticIdentitySecretARN:          os.Getenv("MICROVM_STATIC_IDENTITY_SECRET_ARN"),
		MicrovmGitLabBaseURL:                    envOr("MICROVM_GITLAB_BASE_URL", "https://gitlab.com"),
		MicrovmGitFactoryToken:                  strings.TrimSpace(os.Getenv("MICROVM_GIT_FACTORY_TOKEN")),
		MicrovmZitiBrokerURL:                    os.Getenv("MICROVM_ZITI_BROKER_URL"),
		MicrovmZitiBrokerServerID:               os.Getenv("MICROVM_ZITI_BROKER_SERVER_ID"),
		MicrovmZitiBrokerEnroll:                 envBoolOr("MICROVM_ZITI_BROKER_ENROLL", false),
		MicrovmLogGroup:                         os.Getenv("MICROVM_LOG_GROUP"),
		MicrovmLogStreamPrefix:                  envOr("MICROVM_LOG_STREAM_PREFIX", "agent/"),
		MicrovmControllerRoleARN:                os.Getenv("MICROVM_CONTROLLER_ROLE_ARN"),
		MicrovmExpectedLauncherAccount:          os.Getenv("MICROVM_EXPECTED_LAUNCHER_ACCOUNT"),
		MicrovmProductionMode:                   envBoolOr("MICROVM_PRODUCTION_MODE", false),
		MicrovmMaximumDurationSeconds:           envIntOr("MICROVM_MAXIMUM_DURATION_SECONDS", 0),
		MicrovmSessionVendorRoleARN:             os.Getenv("MICROVM_SESSION_VENDOR_ROLE_ARN"),
		MicrovmSessionCredentialDurationSeconds: envIntOr("MICROVM_SESSION_CREDENTIAL_DURATION_SECONDS", 0),
		MicrovmIdlePolicyEnabled:                envBoolOr("MICROVM_IDLE_POLICY_ENABLED", false),
		MicrovmIdleAutoResumeEnabled:            envBoolOr("MICROVM_IDLE_AUTO_RESUME_ENABLED", false),
		MicrovmIdleMaxIdleDurationSeconds:       envIntOr("MICROVM_IDLE_MAX_IDLE_DURATION_SECONDS", 0),
		MicrovmIdleSuspendedDurationSeconds:     envIntOr("MICROVM_IDLE_SUSPENDED_DURATION_SECONDS", 0),
		MicrovmMaxStandbyAge:                    envDurationOr("MICROVM_MAX_STANDBY_AGE", 0),
		MicrovmSuspendEnabled:                   envBoolOr("MICROVM_SUSPEND_ENABLED", false),
		MicrovmResumeRehydrateEnabled:           envBoolOr("MICROVM_RESUME_REHYDRATE_ENABLED", true),
		MicrovmResumeRehydrateDeadline:          time.Duration(envIntOr("MICROVM_RESUME_REHYDRATE_DEADLINE_SECONDS", 0)) * time.Second,

		// Leader Election
		AccountRotationRestartEnabled: envBoolOr("ACCOUNT_ROTATION_RESTART_ENABLED", true),
		StaleVKeyRestartEnabled:       envBoolOr("STALE_VKEY_RESTART_ENABLED", true),

		SpreadServedModelGateEnabled: envBoolOr("SPREAD_SERVED_MODEL_GATE_ENABLED", true),

		LeaderElection:         envBoolOr("ENABLE_LEADER_ELECTION", false),
		LeaderElectionID:       envOr("LEADER_ELECTION_ID", "agents-leader"),
		LeaderElectionIdentity: envOr("POD_NAME", hostname()),

		// Slack config removed — handled by standalone slack-bridge (bd-8x8fy).

		// Auth Failover
		AuthExhaustedSweepInterval: envDurationOr("AUTH_EXHAUSTED_SWEEP_INTERVAL", 5*time.Minute),
		AuthFailoverCooldown:       envDurationOr("AUTH_FAILOVER_COOLDOWN", 5*time.Minute),
		AuthMaxFailoverDuration:    envDurationOr("AUTH_MAX_FAILOVER_DURATION", 24*time.Hour),

		// LiteLLM Proxy
		LiteLLMURL:       os.Getenv("LITELLM_URL"),
		LiteLLMMasterKey: os.Getenv("LITELLM_MASTER_KEY"),

		LiteLLMTagSpendSource: os.Getenv("LITELLM_TAG_SPEND_SOURCE"),

		LiteLLMKeyRotationInterval: envDurationOr("LITELLM_KEY_ROTATION_INTERVAL", 6*time.Hour),

		LiteLLMAdminSecret: envOr("LITELLM_ADMIN_SECRET", "litellm-gardenio-key"),

		// Slack Bridge
		SlackBridgeURL:         os.Getenv("SLACK_BRIDGE_URL"),
		SlackBridgeTokenSecret: os.Getenv("SLACK_BRIDGE_TOKEN_SECRET"),

		// Jira Bridge (agent-facing reply endpoint, F2 ka-0000000065)
		JiraBridgeURL:         os.Getenv("JIRA_BRIDGE_URL"),
		JiraBridgeTokenSecret: os.Getenv("JIRA_BRIDGE_TOKEN_SECRET"),

		AgentSecretAllowlist: envCSVOr("AGENT_SECRET_ALLOWLIST", nil),

		// Beads Viewer public base URL, injected into agent pods so gb-emitted
		// bundle/image/widget links are absolute + clickable (bug ka-0000003798).
		BeadsViewerURL: os.Getenv("BEADS_VIEWER_URL"),

		// Beads Viewer in-cluster Service base, injected into agent pods so
		// `gb plan create/add-step` POST straight to the viewer pod without a
		// hand-supplied --viewer-url (ka-0000000379).
		BeadsViewerInternalURL: os.Getenv("BEADS_VIEWER_INTERNAL_URL"),

		// Bare gardenio host injected into agent pods as GARDENIO_DEFAULT_HOST so
		// the in-pod gb CLI derives daemon/viewer hosts from the unified host
		// instead of its compiled default (ka-0000002401). Gated: empty until
		// unifiedHost is on or an explicit override is set in the chart.
		GardenioDefaultHost: os.Getenv("GARDENIO_DEFAULT_HOST"),

		// Grafana (in-cluster render affordance for `gb grafana render`)
		GrafanaURL:             os.Getenv("GRAFANA_URL"),
		GrafanaAuthProxyHeader: os.Getenv("GRAFANA_AUTH_PROXY_HEADER"),

		// Service logs (in-cluster VictoriaLogs base for `gb logs <component>`)
		LogsURL: os.Getenv("GARDENIO_LOGS_URL"),

		// Grafana env-side token bundle (task ka-0000002337): per-env Editor
		// tokens for the ACME environment Grafana, injected into agent pods.
		GrafanaEnvAPIKeySecret: os.Getenv("GRAFANA_ENV_API_KEY_SECRET"),
		GrafanaEnvList:         parseCommaSeparated(os.Getenv("GRAFANA_ENV_LIST")),
		GrafanaEnvBaseURLs:     loadGrafanaEnvBaseURLs(parseCommaSeparated(os.Getenv("GRAFANA_ENV_LIST"))),

		// Prewarmed Agent Pool (per-project config via project beads; only interval is controller-level)
		PrewarmedPoolInterval:  envDurationOr("PREWARMED_POOL_INTERVAL", 30*time.Second),
		PrewarmedMicrovmMaxAge: envDurationOr("PREWARMED_MICROVM_MAX_AGE", 0),
		StandbyPoll:            envDurationOr("STANDBY_POLL", 500*time.Millisecond),

		// Queue Age Alerting: migrated to ConfigWatcher
		// (see reconciler.Reconciler.queuedWarnAge) — no direct env read here.

		// Upgrade Drain
		UpgradeDrainTimeout: envDurationOr("COOP_UPGRADE_DRAIN_TIMEOUT", 5*time.Minute),

		// Tool-latency slow-call ledger threshold (P2, gd-sWwPnwkZtw)
		ToolLatencySlowThreshold: envDurationOr("TOOL_LATENCY_SLOW_THRESHOLD", 2*time.Second),
		// Node-drain graceful nudge is off by default; opt-in via Helm once node
		// visibility is wired so a forced node reclaim drains agents cleanly
		// instead of hard-killing them mid-turn (WS1, ka-0000007969).
		NodeDrainNudgeEnabled: envBoolOr("COOP_NODE_DRAIN_NUDGE", false),

		// ExternalSecret Reconciliation
		ExternalSecretStoreName:       envOr("EXTERNAL_SECRET_STORE_NAME", "secretstore"),
		ExternalSecretStoreKind:       envOr("EXTERNAL_SECRET_STORE_KIND", "ClusterSecretStore"),
		ExternalSecretRefreshInterval: envOr("EXTERNAL_SECRET_REFRESH_INTERVAL", "15m"),

		// Stale Agent Reaper
		StaleAgentTimeout: envDurationOr("STALE_AGENT_TIMEOUT", 2*time.Hour),
		RuntimeCeilingByRole: map[string]time.Duration{
			"crew":          envDurationOr("STALEREAPER_RUNTIME_MAX_CREW", 6*time.Hour),
			"thread":        envDurationOr("STALEREAPER_RUNTIME_MAX_THREAD", 2*time.Hour),
			"plan-gardener": envDurationOr("STALEREAPER_RUNTIME_MAX_PLAN_GARDENER", 12*time.Hour),
		},
		// Disabled by default (ka-0000000084) — see StuckAgentTimeout docs.
		StuckAgentTimeout:        envDurationOr("STUCK_AGENT_TIMEOUT", 0),
		StuckLongLivedMultiplier: envFloatOr("STUCK_LONG_LIVED_MULTIPLIER", 4),
		// Non-destructive restart is the default remedy (ka-0000001648): try an
		// in-place coop session restart before deleting+recreating the pod.
		StuckRestartInPlace: envBoolOr("STUCK_RESTART_IN_PLACE", true),

		// PVC Garbage Collection
		PVCGCInterval: envDurationOr("PVC_GC_INTERVAL", 5*time.Minute),
		PVCGCTTL:      envDurationOr("PVC_GC_TTL", 24*time.Hour),

		// PV Garbage Collection
		PVGCInterval:   envDurationOr("PV_GC_INTERVAL", 5*time.Minute),
		PVGCTTL:        envDurationOr("PV_GC_TTL", 24*time.Hour),
		PVGCNamespaces: envCSVOr("PV_GC_NAMESPACES", nil),

		PVCObsInterval:  envDurationOr("PVC_OBS_INTERVAL", 0),
		PVCObsThreshold: envDurationOr("PVC_OBS_THRESHOLD", 0),

		NodeLifecycleInterval:          envDurationOr("NODE_LIFECYCLE_INTERVAL", 0),
		NodeLifecycleNodeSelector:      envOr("NODE_LIFECYCLE_NODE_SELECTOR", ""),
		NodeLifecycleIdlePodNamespaces: envCSVOr("NODE_LIFECYCLE_IDLE_POD_NAMESPACES", nil),

		// Advice Expiration Sweeper
		AdviceSweepInterval:     envDurationOr("GARDENIO_ADVICE_SWEEP_INTERVAL", 5*time.Minute),
		AdviceCompactionEnabled: envBoolOr("GARDENIO_ADVICE_COMPACTION", true),

		// Durable Spend Settlement Sweep
		SpendSweepInterval: envDurationOr("GARDENIO_SPEND_SWEEP_INTERVAL", 30*time.Minute),

		// Cost-Audit Cross-Check Sweep (C2)
		XCheckSweepInterval: envDurationOr("GARDENIO_XCHECK_SWEEP_INTERVAL", 15*time.Minute),
		MetricsQueryURL:     envOr("VMSINGLE_QUERY_URL", "http://gardenio-vmsingle:8428"),

		// Postgres efficiency report (gd-ylgk1jBhLi)
		PgBudgetReportHour: envIntOr("GARDENIO_PG_BUDGET_REPORT_HOUR", -1),
		PgBudgetLand:       envOr("GARDENIO_PG_BUDGET_LAND", "gardenio"),
		PgBudgetEpic:       os.Getenv("GARDENIO_PG_BUDGET_EPIC"),
		PgBudgets:          os.Getenv("GARDENIO_PG_BUDGETS"),

		// Session reaper (gd-c6muILNedN)
		SessionReaperInterval: envDurationOr("GARDENIO_SESSION_REAPER_INTERVAL", 10*time.Minute),

		// Claude status-page poller (leg B, epic ka-0000000487)
		ClaudeStatusPollInterval: envDurationOr("GARDENIO_CLAUDE_STATUS_POLL_INTERVAL", time.Minute),
		ClaudeStatusURL:          envOr("GARDENIO_CLAUDE_STATUS_URL", ""),
		ClaudeStatusFallbackURLs: parseCommaSeparated(os.Getenv("GARDENIO_CLAUDE_STATUS_FALLBACK_URLS")),

		// Spend-Logs Sweep (C3, stateless per-agent attribution)
		SpendLogsSweepInterval: envDurationOr("GARDENIO_SPENDLOGS_SWEEP_INTERVAL", 30*time.Minute),
		SpendLogsSweepLookback: envDurationOr("GARDENIO_SPENDLOGS_SWEEP_LOOKBACK", 48*time.Hour),

		// Bridge Fleet Token Rotator (default OFF; opt-in per cluster)
		BridgeTokenRotationEnabled:     envBoolOr("BRIDGE_TOKEN_ROTATION_ENABLED", false),
		BridgeTokenRotationDryRun:      envBoolOr("BRIDGE_TOKEN_ROTATION_DRY_RUN", true),
		BridgeTokenRotationInterval:    envDurationOr("BRIDGE_TOKEN_ROTATION_INTERVAL", time.Hour),
		BridgeTokenRotationThreshold:   envDurationOr("BRIDGE_TOKEN_ROTATION_THRESHOLD", 14*24*time.Hour),
		BridgeTokenRotationAccessTTL:   envDurationOr("BRIDGE_TOKEN_ROTATION_ACCESS_TTL", 720*time.Hour),
		BridgeTokenRotationRefreshTTL:  envDurationOr("BRIDGE_TOKEN_ROTATION_REFRESH_TTL", 60*24*time.Hour),
		BridgeTokenRotationSecret:      envOr("BRIDGE_TOKEN_ROTATION_SECRET", "bridge-fleet-tokens"),
		BridgeTokenRotationFleetMapKey: envOr("BRIDGE_TOKEN_ROTATION_FLEET_MAP_KEY", "fleet-map"),
		BridgeTokenRotationFleets:      envCSVOr("BRIDGE_TOKEN_ROTATION_FLEETS", []string{"slack-bridge", "gitlab-bridge", "jira-bridge"}),
		// gitlab-bridge writes produces/triggered-by deps + mr_url back-fill
		// across cross-owner beads — it needs the privileged ownership bypass
		// (ka-0000001946). slack-bridge / jira-bridge stay ordinary.
		BridgeTokenRotationPrivilegedFleets: envCSVOr("BRIDGE_TOKEN_ROTATION_PRIVILEGED_FLEETS", []string{"gitlab-bridge"}),

		// Controller
		LogLevel: envOr("LOG_LEVEL", "info"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envLookupOr returns the env var's value when the key is SET (even to the
// empty string), and fallback only when the key is entirely UNSET. Unlike
// envOr, this preserves an explicit empty value as a meaningful signal — used
// by AGENT_DEFAULT_FABLE_MODEL, whose empty value is the escape hatch that
// disables the fable-alias pin (a helm overlay setting the value to "" must be
// able to turn injection off, not silently re-inherit the baked default).
func envLookupOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envBoolOr(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return fallback
}

func envFloatOr(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// envDurationSetButIneffective reports whether key was set in the environment to
// a value that will NOT survive as a positive override: it either failed to
// parse (envDurationOr silently drops it to the fallback) or parsed to a
// non-positive duration (which downstream `> 0` override guards treat as unset).
// Used to surface a fat-fingered FREEZE_HARD_DEADLINE at startup rather than
// letting it silently revert to the computed default (task ka-0000006783). An
// unset or empty env var is not "ineffective" — it is the documented default —
// so this returns false for it.
func envDurationSetButIneffective(key string) bool {
	v := os.Getenv(key)
	if v == "" {
		return false
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return true
	}
	return d <= 0
}

func envCSVOr(key string, fallback []string) []string {
	if v := os.Getenv(key); v != "" {
		return parseCommaSeparated(v)
	}
	return fallback
}

// loadGrafanaEnvBaseURLs reads the non-secret per-environment Grafana base URL
// for each env in the bundle from GRAFANA_BASE_URL_<ENV> (env uppercased).
// Envs with no value set are omitted. See task ka-0000002337.
func loadGrafanaEnvBaseURLs(envs []string) map[string]string {
	if len(envs) == 0 {
		return nil
	}
	urls := make(map[string]string, len(envs))
	for _, env := range envs {
		if v := os.Getenv("GRAFANA_BASE_URL_" + strings.ToUpper(env)); v != "" {
			urls[env] = v
		}
	}
	return urls
}

func parseCommaSeparated(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			result = append(result, v)
		}
	}
	return result
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
