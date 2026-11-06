// Command controller is the Gardenio K8s controller — a thin reactive bridge
// between beads lifecycle events and Kubernetes pod operations.
//
// Architecture: Beads IS the control plane. This controller watches beads
// events via BD Daemon and translates them to pod create/delete operations.
// It does NOT use controller-runtime or CRD reconciliation loops.
//
// See docs/design/k8s-crd-schema.md and docs/design/k8s-reconciliation-loops.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"k8s.io/client-go/dynamic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/bridgetokenrotator"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/coopmux"
	"gardenio/controller/internal/covflush"
	"gardenio/controller/internal/healthsnapshot"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/nodelifecycle"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/poolmanager"
	"gardenio/controller/internal/pvcgc"
	"gardenio/controller/internal/pvcobs"
	"gardenio/controller/internal/pvgc"
	"gardenio/controller/internal/reconciler"
	"gardenio/controller/internal/secretreconciler"
	"gardenio/controller/internal/statusreporter"
	"gardenio/controller/internal/subscriber"
	"gardenio/controller/internal/tracing"
)

var (
	version = "dev"
	commit  = "unknown"
)

// thawTeardownTimeout bounds the detached teardown of an un-rehydrated Pod B on
// the partial-success thaw path. It runs on a ctx decoupled from the inbound
// (leader-election) ctx so a controller roll mid-thaw cannot cancel the delete
// and orphan Pod B's on-node runsc containers (bug ka-0000004088 problem 1;
// ka-0000001355 harden-thaw). A pod delete + finalizer settle is sub-second in
// practice; 30s is a generous backstop.
const thawTeardownTimeout = 30 * time.Second

// machineWarmTimeout bounds the startup wait for the first machine token.
const machineWarmTimeout = 90 * time.Second

// controllerMachineRoles are the gardenio-machine-* realm roles the controller's
// service account must carry (kbeads machine_grants.go). A token missing one is
// re-fetched after 30s instead of at half-life, so a chart that adds a role and
// rolls the controller together does not pin a role-less token.
var controllerMachineRoles = []string{
	"gardenio-machine-accounts",
	"gardenio-machine-token-mint",
	"gardenio-machine-controller-ownership",
	"gardenio-machine-controller-config-seed",
}

// controllerMachineReclaimTypes are the issue types the stale reaper may
// release a dead agent's claim on (reopen, clear assignee, flag) when it runs
// as the machine principal: the issue types in the Ownership (task) and
// ReclaimOwnership (bug, feature, chore, epic; gd-RZJk5gspFv) of
// gardenio-machine-controller-ownership (kbeads machine_grants.go).
// TestControllerMachineReclaimTypes_MatchGrant keeps the two in step.
var controllerMachineReclaimTypes = []string{"task", "bug", "feature", "chore", "epic"}

func main() {
	defer covflush.Flush()
	bridge.NudgeSender = "controller" // from: label on the mail bridge.NudgeAgent sends

	// Subcommand dispatch. The controller binary doubles as the perf-traces
	// relay (epic ka-0000000598, task ka-0000004875) so the chart reuses the
	// controller image instead of shipping a new one. Default (no subcommand)
	// runs the controller.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "perf-traces-relay":
			runPerfTracesRelay()
			return
		}
	}

	cfg := config.Parse()

	logger := setupLogger(cfg.LogLevel)
	logger.Info("starting gardenio controller",
		"beads_http", cfg.BeadsHTTPAddr,
		"namespace", cfg.Namespace)

	shutdownTracing, err := tracing.Init(context.Background(), logger, tracing.Options{
		ServiceName:    "gardenio-controller",
		ServiceVersion: version,
		Environment:    cfg.Env,
	})
	if err != nil {
		logger.Error("failed to initialize tracing", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			logger.Warn("tracing shutdown error", "error", err)
		}
	}()

	k8sClient, err := buildK8sClient(cfg.KubeConfig)
	if err != nil {
		logger.Error("failed to create K8s client", "error", err)
		os.Exit(1)
	}

	// Build dynamic client for ExternalSecret reconciliation.
	k8sCfg, err := buildK8sConfig(cfg.KubeConfig)
	if err != nil {
		logger.Error("failed to build K8s config for dynamic client", "error", err)
		os.Exit(1)
	}
	dynClient, err := dynamic.NewForConfig(k8sCfg)
	if err != nil {
		logger.Error("failed to create dynamic K8s client", "error", err)
		os.Exit(1)
	}
	secretRec := secretreconciler.New(
		dynClient, cfg.Namespace,
		cfg.ExternalSecretStoreName,
		cfg.ExternalSecretStoreKind,
		cfg.ExternalSecretRefreshInterval,
		logger,
	)
	// Materialize only Secrets an agent pod may mount (gd-q8nBnOP65d): the
	// same forbidden markers and AGENT_SECRET_ALLOWLIST the pod spec applies.
	secretRec.SetSecretFilter(func(name string) bool {
		return !isForbiddenPodSecret(name) && cfg.AgentSecretAllowed(name)
	})
	// Mirror managed ExternalSecrets into the gVisor namespace too, so runsc
	// agents / prewarmed-pool standbys routed there (routeGVisorNamespace) find
	// their project/role secrets already synced instead of stalling pod
	// admission on "secret(s) not yet synced" (ka-0000007922).
	if cfg.GVisorNamespace != "" {
		secretRec.SetExtraNamespaces(cfg.GVisorNamespace)
	}

	// Lambda MicroVM backend (epic ka-0000005010, C3 identity + C4 backend).
	// Default-off: StartupBackend is a no-op (returns nil) unless the chart pins
	// both the launcher role ARN and the MicroVM region. When enabled, on a
	// healthy pod it assumes the Fabric launcher role from the controller's own
	// default (IRSA) identity, verifies the cross-account hop (logging
	// GetCallerIdentity), and builds the RunMicrovm/secret-write backend the
	// AgentSpawn door dispatches lambda_microvm agents to. Best-effort: a
	// construction/verification/config failure is logged inside StartupBackend
	// and the controller continues with a nil backend (the MicroVM path stays
	// unavailable and the spawn door fails closed) — it is not a fatal boot
	// dependency for the Kubernetes backend.
	microvmBackend, _ := microvm.StartupBackend(context.Background(), logger, microvm.StartupConfig{
		Identity: microvm.Options{
			LauncherRoleARN:   cfg.MicrovmLauncherRoleARN,
			Region:            cfg.MicrovmRegion,
			SessionName:       cfg.MicrovmAssumeRoleSessionName,
			ControllerRoleARN: cfg.MicrovmControllerRoleARN,
		},
		Backend: microvm.BackendConfig{
			Region:                  cfg.MicrovmRegion,
			ImageIdentifier:         cfg.MicrovmImageIdentifier,
			ImageVersion:            cfg.MicrovmImageVersion,
			ExecutionRoleARN:        cfg.MicrovmExecutionRoleARN,
			SecretPrefix:            cfg.MicrovmSecretPrefix,
			StaticIdentitySecretARN: cfg.MicrovmStaticIdentitySecretARN,
			MaximumDurationSeconds:  int32(cfg.MicrovmMaximumDurationSeconds),
			LogGroup:                cfg.MicrovmLogGroup,
			LogStreamPrefix:         cfg.MicrovmLogStreamPrefix,
			ProductionMode:          cfg.MicrovmProductionMode,
			ExpectedLauncherAccount: cfg.MicrovmExpectedLauncherAccount,
			// Optional AWS IdlePolicy (MICROVM_IDLE_*). Nil unless explicitly
			// enabled, so the launch shape stays byte-for-byte identical to the
			// pre-idle-policy path (plan ka-0000004518).
			IdlePolicy: microvm.NewIdlePolicy(
				cfg.MicrovmIdlePolicyEnabled,
				cfg.MicrovmIdleAutoResumeEnabled,
				cfg.MicrovmIdleMaxIdleDurationSeconds,
				cfg.MicrovmIdleSuspendedDurationSeconds,
			),
		},
		// Per-run Ziti identities (ka-0000001924): when the broker URL is set,
		// every launch mints a unique short-lived identity (fail-closed) and the
		// deprecated static identity above is ignored.
		Broker: microvm.BrokerOptions{
			URL:      cfg.MicrovmZitiBrokerURL,
			ServerID: cfg.MicrovmZitiBrokerServerID,
			Enroll:   cfg.MicrovmZitiBrokerEnroll,
		},
	})

	// Bead-scoped S3 session-credential vendor (S4 ka-0000006612). Built once
	// (its anonymous STS client is safe for concurrent use). Default-off: nil
	// unless the S2 vendor role AND a sessions bucket are configured. When a
	// MicroVM launches, handleNonK8sSpawn mints a short-lived credential scoped
	// to coop/sessions/<bead-id>/* and folds it into the per-run secret so the
	// in-VM coop can persist/restore its session; the MINIMAL execution role
	// grants no sessions-bucket access itself. A build failure is logged and
	// leaves a nil vendor — satellites then run without S3 session persistence
	// rather than failing to launch.
	microvmSessionVendor, err := microvm.NewSessionVendor(context.Background(), microvmSessionVendorOptions(cfg))
	if err != nil {
		logger.Warn("lambda_microvm: session-credential vendor unavailable; satellites will run without S3 session persistence",
			"vendor_role", cfg.MicrovmSessionVendorRoleARN, "bucket", cfg.CoopS3Bucket, "error", err)
		microvmSessionVendor = nil
	} else if microvmSessionVendor != nil {
		logger.Info("lambda_microvm: bead-scoped S3 session-credential vendor ready",
			"vendor_role", cfg.MicrovmSessionVendorRoleARN, "bucket", cfg.CoopS3Bucket,
			"region", microvmSessionVendor.Region())
	}

	// Machine principal (gd-s7aPIf0Bgp, gd-BDirD4F7Iq): with
	// BEADS_OIDC_TOKEN_URL/CLIENT_ID/CLIENT_SECRET_FILE set (chart
	// agents.machinePrincipal) the controller authenticates EVERY daemon call
	// and the SSE event stream as the Keycloak service account of client
	// gardenio-controller, and holds no BEADS_DAEMON_TOKEN. Its roles are what
	// kbeads admits: gardenio-machine-accounts (update/account, no allow-list
	// entry), -token-mint (/v1/fleet-tokens, design D3), -controller-ownership
	// (the typed replacement for the static-token ownership bypass on the
	// reaper / sweeper / settle writes) and -controller-config-seed
	// (EnsureConfigs). Unset, the token source is nil and the client falls back
	// to the static token: today's behavior, and the rollback.
	machineTS, machineEnabled, err := beadsapi.MachineTokenSourceFromEnv(context.Background(), logger, beadsapi.WithExpectedRoles(controllerMachineRolesFor(os.Getenv)...))
	if err != nil {
		logger.Error("machine principal misconfigured", "error", err)
		os.Exit(1)
	}
	if !machineEnabled {
		machineTS = nil
	}

	watcher := subscriber.NewSSEWatcher(subscriber.SSEConfig{
		BeadsHTTPAddr: cfg.BeadsHTTPAddr,
		Token:         os.Getenv("BEADS_DAEMON_TOKEN"),
		TokenSource:   machineTS,
		Topics:        "beads.bead.*",
		Namespace:     cfg.Namespace,
		CoopImage:     cfg.CoopImage,
		BeadsGRPCAddr: cfg.BeadsGRPCAddr,
	}, logger)
	logger.Info("using SSE transport for beads events",
		"beads_http", cfg.BeadsHTTPAddr)
	pods := podmanager.New(k8sClient, logger)
	pods.InitCloneImageOverride = cfg.InitCloneImage
	// A tenant pod's coopmux token is minted per land (gd-ApgYe740ps).
	pods.MintLandCoopmuxToken = landCoopmuxTokenMinter(cfg)
	// And its NATS password, when tenant lands have NATS users (gd-sS7bSXOdGs).
	pods.MintLandNatsToken = landNatsTokenMinter(cfg)
	// Agent coop StartupProbe budget override (ka-0000000649): <= 0 keeps the
	// compiled default; a slow-egress cluster widens the cold-start window via
	// agents.startupProbeFailureThreshold without a controller re-release.
	pods.StartupProbeFailureThresholdOverride = cfg.AgentStartupProbeFailureThreshold
	// Clone-storm stagger (ka-0000000995, follow-up to ka-0000001925): the
	// init-clone container sleeps a random 0..N seconds before a cold network
	// clone so a batch of pods does not all saturate NAT egress at once. env
	// unset (-1 sentinel) -> compiled default; 0 disables; >0 sets the window.
	pods.InitCloneJitterMaxSeconds = podmanager.DefaultInitCloneJitterMaxSeconds
	if cfg.AgentInitCloneJitterMaxSeconds >= 0 {
		pods.InitCloneJitterMaxSeconds = cfg.AgentInitCloneJitterMaxSeconds
	}
	// Air-gap overrides (P4.3, ka-0000007177): DinD image + agent pull policy.
	// Both default to the compiled-in behavior when their env is unset, so
	// cloud deploys are unchanged.
	pods.DindImageOverride = cfg.DindImage
	switch corev1.PullPolicy(cfg.AgentPullPolicy) {
	case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
		pods.AgentPullPolicy = corev1.PullPolicy(cfg.AgentPullPolicy)
	case "":
		// unset → podmanager keeps its PullAlways default
	default:
		logger.Warn("ignoring invalid AGENT_PULL_POLICY; using default PullAlways",
			"value", cfg.AgentPullPolicy,
			"valid", "Always|IfNotPresent|Never")
	}
	// Low-priority class for prewarmed/standby pods so a cold real agent
	// preempts an idle pool member under node pressure (ka-0000000722). Empty
	// unless the chart rendered the class and set PREWARMED_PRIORITY_CLASS.
	pods.PrewarmedPriorityClass = cfg.PrewarmedPriorityClass
	// Positive-value class for interactive-role pods (thread/dm/gardener) so they
	// can preempt default-priority batch crew pods under node pressure
	// (gd-Vtnt5lAGeQ). Empty unless the chart rendered the shared
	// gardenio-system-critical class and set INTERACTIVE_PRIORITY_CLASS.
	pods.InteractivePriorityClass = cfg.InteractivePriorityClass
	// Same value as interactive but never preempting, for a gardener a schedule
	// or fire-now started (gd-Y2KaNE233g). Empty unless the chart rendered
	// gardenio-scheduled and set SCHEDULED_PRIORITY_CLASS.
	pods.ScheduledPriorityClass = cfg.ScheduledPriorityClass
	// Lowest-tier, never-preempting class for every other agent pod
	// (gd-2qjxTVMBkC). Empty unless the chart rendered gardenio-agent and set
	// AGENT_PRIORITY_CLASS.
	pods.AgentPriorityClass = cfg.AgentPriorityClass
	// Runtime coverage instrumentation for agent pods (C2, ka-0000000508): the
	// chart wires these only when global.coverage.enabled, so a normal deploy
	// leaves Enabled=false — a full no-op. When active, the podmanager adds
	// GOCOVERDIR + LLVM_PROFILE_FILE and mounts the shared coverage PVC.
	pods.Coverage = podmanager.CoverageConfig{
		Enabled:   cfg.AgentCoverageEnabled,
		ClaimName: cfg.AgentCoverageClaim,
		MountPath: cfg.AgentCoverageMount,
	}

	// Reco-only VPA memory stamp (ka-0000004606): when enabled, the pod manager
	// reads the VPA recommendation for each crew pod's (project, role) class and
	// stamps a clamped memory request at spawn. Default OFF (rollback switch);
	// reuses the dynamic client already built for ExternalSecret reconciliation.
	pods.VPAStamper = podmanager.NewVPAMemoryStamper(dynClient, podmanager.VPAMemoryStamperConfig{
		Enabled:            cfg.VPAMemoryStampEnabled,
		ObjectNameTemplate: cfg.VPAObjectNameTemplate,
		Floor:              cfg.VPAMemoryFloor,
		Max:                cfg.VPAMemoryMax,
	}, logger)

	// gVisor freeze/thaw lifecycle (epic ka-0000007417, task ka-0000003688).
	// Nil unless cfg.FreezeThawEnabled — see freezethaw_lifecycle.go. The probe
	// executor needs the raw rest.Config (k8sCfg) for the SPDY exec stream into
	// the per-node privileged probe pod.
	// Warm-placeholder pool churn metrics (ka-0000002975). Constructed up front
	// so it can be both handed to the pool (drainStale records cycled-stale
	// counts) and registered on the :9092 registry below. Nil-safe: a no-op when
	// the pool is disabled.
	poolMetrics := gardeniometrics.NewPoolMetrics(cfg.Env)

	// Rate-limit probe liveness metrics (ka-0000001889): tick counter, stall
	// detections, recovered panics, and the watchdog's staleness gauge. Registered
	// on the :9092 registry below and passed to the probe via run().
	probeMetrics := gardeniometrics.NewProbeMetrics(cfg.Env)

	var ft *freezeThawLifecycle
	if cfg.FreezeThawEnabled {
		// The gvisor-probe pods (privileged hostPID/hostPath) live in the
		// PSA-privileged gVisor namespace when one is configured, NOT in the
		// baseline-enforced main namespace (bug ka-0000004301). Fall back to
		// cfg.Namespace when unset to preserve single-namespace behaviour.
		probeNS := cfg.Namespace
		if cfg.GVisorNamespace != "" {
			probeNS = cfg.GVisorNamespace
		}
		nodeExec := podmanager.NewProbeNodeExecutor(k8sClient, k8sCfg, probeNS)
		ft = newFreezeThawLifecycle(cfg, k8sClient, nodeExec, logger)

		// Warm runsc placeholder ("warm-B") pool (C2/C3, ka-0000005266 /
		// ka-0000004263). Default-off: NewPlaceholderPool returns nil unless
		// WARM_PLACEHOLDER_POOL_ENABLED and a positive MinSize are set, so the
		// wake path stays on the create-then-restore thaw with zero blast radius.
		// When enabled, the pool keeps MinSize Ready placeholders per gVisor node
		// and the wake path grafts thaws onto them (sub-second resume).
		if cfg.WarmPlaceholderPoolEnabled {
			pool := poolmanager.NewPlaceholderPool(k8sClient, poolmanager.PlaceholderPoolConfig{
				Namespace:         probeNS,
				Image:             cfg.CoopImage,
				MinSize:           cfg.WarmPlaceholderPoolMinSize,
				NodeFloor:         cfg.WarmPlaceholderNodeFloor,
				StaleDrainRate:    cfg.WarmPlaceholderStaleDrainRate,
				MaxConcurrent:     cfg.WarmPlaceholderMaxConcurrent,
				CycleRecorder:     poolMetrics,
				ClaimRecorder:     poolMetrics,
				PriorityClassName: cfg.PrewarmedPriorityClass,
				ImagePullSecrets:  cfg.ImagePullSecrets,
			}, logger.With("component", "warm-placeholder-pool"))
			// Make the pool topology- and repo-aware (ka-0000002219): drive its
			// fills from existing snapshot templates so a warm graft matches the
			// snapshot's mount shape + repo, instead of a blind bare-/work pod.
			pool.SetSnapshotProvider(ft.snapshotProvider())
			ft.setPlaceholderPool(pool)
		}
	}

	// Daemon client for HTTP access (used by reconciler, status reporter, and
	// bridge). With the machine principal enabled it IS the machine client:
	// there is no static token left in this process to authenticate with, and
	// the SSE stream above uses the same source.
	daemon, err := beadsapi.New(beadsapi.Config{
		HTTPAddr:    cfg.BeadsHTTPAddr,
		Token:       os.Getenv("BEADS_DAEMON_TOKEN"),
		TokenSource: machineTS,
	})
	if err != nil {
		logger.Error("failed to create beads daemon client", "error", err)
		os.Exit(1)
	}
	defer daemon.Close()
	// machineDaemon is the client for the controller's own-principal calls
	// (account-bead writes, fleet-token minting). It is daemon itself: one
	// credential, so a deployment cannot half-migrate. The separate parameter
	// stays because the call sites document which writes need which grant.
	machineDaemon := daemon
	if machineEnabled {
		logger.Info("machine principal enabled", "client", os.Getenv("BEADS_OIDC_CLIENT_ID"))
		// Prime the first token before the startup reads (config seed, project /
		// role caches) that now ride it. Bounded: a Keycloak that is still
		// reconciling the client (gd-pj5hT3V1la) must not hold the pod hostage,
		// and a missing Secret mount fails every call closed either way.
		warmCtx, cancelWarm := context.WithTimeout(context.Background(), machineWarmTimeout)
		if err := beadsapi.WarmMachineToken(warmCtx, machineTS); err != nil {
			logger.Warn("machine token warm-up did not complete; daemon calls fail closed (auth_unavailable) until it does", "error", err)
		}
		cancelWarm()
	} else {
		logger.Info("machine principal disabled")
	}
	// C5 lifecycle coordinator (epic ka-0000005010, ka-0000001837): persists
	// runtime metadata at launch, reconciles MicroVM agents by runtime status
	// (not pod presence), terminates on stop/cancel, and sweeps leaked
	// runtimes/secrets. A nil backend yields an inert coordinator, so the
	// Kubernetes path is unchanged when the MicroVM backend is default-off.
	microvmLifecycle := microvm.NewLifecycle(microvmBackend, daemon, logger)
	// Per-run credential revocation on every exit path (ka-0000004762): the
	// lifecycle invokes this on terminate, terminal reconcile-close (timeout)
	// and leak GC; the spawn door invokes it on failed launch. It revokes the
	// per-agent beads token and the per-run git token; the future per-run Ziti
	// identity (ka-0000001924) slots in here too.
	microvmLifecycle.SetCredentialRevoker(newMicrovmCredentialRevoker(logger, daemon, cfg))
	// Resume rehydration correctness boundary (step 5, plan ka-0000004518): after
	// a ResumeMicrovm, the fail-closed proof-of-life rendezvous waits for the
	// resumed VM's own egress heartbeat to re-cross the OpenZiti overlay to beads
	// (presence roster) before marking agent_state=working — else it terminates
	// the un-rehydratable VM and marks resume_failed, so a half-rehydrated VM
	// never serves the user. The gate is default-on; MICROVM_RESUME_REHYDRATE_ENABLED
	// =false is the step-3 rollback (stamp working immediately). See
	// docs/microvm-resume-rehydration.md.
	microvmLifecycle.SetResumeRehydration(cfg.MicrovmResumeRehydrateEnabled, cfg.MicrovmResumeRehydrateDeadline)
	microvmLifecycle.SetLivenessProber(microvmRosterProber{roster: daemon})
	// Max-standby-age cap (step 4, ka-0000006202): a SUSPENDED thread-agent VM is
	// intentional standby (its open bead is the resume target), so leak GC leaves
	// it alone UNLESS its standby exceeds this cap — then GC terminates the VM and
	// closes the bead as done. <=0 (default) disables the cap; the AWS IdlePolicy
	// SuspendedDurationSeconds backstop is then the only standby-billing limit.
	microvmLifecycle.SetMaxStandbyAge(cfg.MicrovmMaxStandbyAge)
	// Human-operator invariant (epic ka-0000004835, task ka-0000006293): the
	// client-level system-operator default is REMOVED. Previously (R3,
	// ka-0000002887) the controller stamped `gardenio-controller` as a
	// client-wide X-System-Operator default so every operator-less write bound
	// the `system:gardenio-controller` sentinel row. The operator directive
	// (decision ka-0000006432) is to eliminate system-operator sentinels: a
	// missing human is a loud refusal, not a silent fallback to a system
	// identity.
	//
	// Attribution now flows entirely from per-call context:
	//   - Human-origin writes (bridge / scheduler / mention / trigger handlers)
	//     carry the resolved human via WithOperator (X-Operated-By:
	//     slack:U…/oidc:…@…). These already set it per call; nothing here
	//     backstops them.
	//   - TRUE machine-origin writes (statusreporter pod-phase updates,
	//     reconciler housekeeping, project/role/account cache writebacks) must
	//     carry an explicit per-call WithSystemOperator so the daemon binds the
	//     narrowly-scoped machine sink — NOT a client-wide default that would
	//     silently absorb writes that *should* have carried a human.
	//
	// The daemon-side resolver collapse (system:* -> one system:machine sink,
	// drop fleet terminal branch) lands in ka-0000006425; this change is
	// gardener-gated to merge alongside it so service writes don't hard-400
	// before the daemon can absorb genuine machine-origin writes (the
	// fail-closed-amplification risk that broke gasbox, ka-0000005858 RC3).
	status := statusreporter.NewHTTPReporter(daemon, k8sClient, cfg.Namespace, logger)
	// Also sweep the gVisor namespace so runsc freeze/thaw agents running there
	// get bead phase/coop_url updates (bug ka-0000004301). No-op when unset.
	if cfg.GVisorNamespace != "" {
		status.SetExtraNamespaces(cfg.GVisorNamespace)
	}
	// And the tenant land namespaces, where the land fence places tenant pods
	// (Lands step 10.2), re-read from the project cache every sweep so lands
	// that open or close are picked up (gd-Vuu1J7y4J0).
	if landNamespacesEnabled(os.Getenv) {
		status.SetNamespaceSource(func() []string { return tenantLandNamespaces(cfg) })
	}

	// Event-driven controller metrics (E2.4/E2.5/E2.6, timeseries plan §2.E2).
	// These counters/histogram are incremented from real lifecycle transitions
	// rather than polled; they are registered on the controller :9092 registry
	// below and shared with handleEvent (lifecycle + session duration) and the
	// status reporter (pod phase transitions + OOM detection).
	eventMetrics := gardeniometrics.NewEventMetrics(cfg.Env)
	status.SetEventRecorder(eventMetrics)

	// Per-agent spend counter (E3 spend-as-counter, ka-0000003262). Incremented
	// once per session by reconcileAtClose (reconcile_session_spend.go) from the
	// LiteLLM-reconciled session cost; registered on the :9092 registry below.
	// (A per-turn NATS spend forwarder once fed this counter but was structurally
	// dead — coop emits no response_cost — and was removed in I1 ka-0000000717.)
	spendMetrics := gardeniometrics.NewSpendMetrics()

	// Cost-audit cross-check gauges (C2, plan ka-0000003747). The periodic
	// xcheckSweep (started in run()) republishes gardenio_xcheck_value /
	// gardenio_xcheck_agreement — the corroboration score for each conservation
	// pair — onto this :9092 registry.
	xcheckMetrics := gardeniometrics.NewXCheckMetrics(cfg.Env)

	// Resume-latency histogram (Phase 5 sub-second resume, task ka-0000003577).
	// Observed from the gVisor wake path below: spawn-event-accepted -> thaw
	// rehydration acked (coop-ready). Feeds the dashboard with the same samples
	// bench-resume.sh logs per-run to CSV. Registered on the :9092 registry.
	resumeMetrics := gardeniometrics.NewResumeMetrics(cfg.Env)

	// Freeze-latency histogram (Phase 6 fast+interruptible freeze, epic
	// ka-0000008235, task ka-0000005147). Observed from the gVisor freeze path
	// below: freeze-requested -> checkpoint image persisted + Snapshot recorded,
	// broken out per step (runsc-checkpoint vs. Step-3 relocation copy vs. persist)
	// so C3 optimization is data-driven. Feeds the dashboard with the same samples
	// bench-freeze.sh logs per-run to CSV. Registered on the :9092 registry.
	freezeMetrics := gardeniometrics.NewFreezeMetrics(cfg.Env)

	// lambda_microvm launch-dispatch histogram (task ka-0000003938, thread
	// "plan gardeners on MicroVM"). Observed from the MicroVM spawn door below:
	// spawn-door-enters-MicroVM-branch -> backend.Launch returns a live handle.
	// The controller-owned half of MicroVM spin-up (per-run credential
	// resolution + SM writes + broker lease + RunMicrovm accept); the in-VM
	// phases run in the Fabric-owned VM and land in launcher-account CloudWatch.
	// Registered on the :9092 registry.
	microvmLaunchMetrics := gardeniometrics.NewMicrovmLaunchMetrics(cfg.Env)

	// Tool-latency histograms (P2 HOOK_EVENTS consumer, plan
	// gardenio/plans/agent-tool-latency-attribution.md, bead gd-sWwPnwkZtw).
	// Populated by the durable JetStream consumer started below
	// (startToolLatencyConsumer) from every agent PostToolUse hook event.
	// Registered on the :9092 registry.
	toolLatencyMetrics := gardeniometrics.NewToolLatencyMetrics(cfg.Env)

	// Register bead types, views, and context configs with the daemon.
	if err := bridge.EnsureConfigs(context.Background(), daemon, logger); err != nil {
		logger.Warn("failed to ensure beads configs (will retry on next sync)", "error", err)
	}

	// Populate project, account, and role caches from daemon beads.
	cfg.ProjectCache = make(map[string]config.ProjectCacheEntry)
	cfg.AccountCache = make(map[string]config.AccountCacheEntry)
	cfg.RoleCache = make(map[string]config.RoleCacheEntry)
	refreshProjectCache(context.Background(), logger, daemon, cfg)
	refreshRoleCache(context.Background(), logger, daemon, cfg)

	// Create coopmux client for credential sync (nil if COOPMUX_URL is not set).
	muxClient := coopmux.New(coopmux.Config{
		URL:       cfg.CoopmuxURL,
		AuthToken: os.Getenv("COOP_MUX_AUTH_TOKEN"),
	})
	if muxClient != nil {
		logger.Info("coopmux credential sync enabled", "url", cfg.CoopmuxURL)
	}

	rec := reconciler.New(daemon, pods, cfg, logger, BuildSpecFromBeadInfo)
	// Also reconcile actual pods in the gVisor namespace so runsc freeze/thaw
	// agents participate in desired-vs-actual orphan cleanup and drift
	// recreation (ka-0000008076). No-op when unset.
	if cfg.GVisorNamespace != "" {
		rec.SetExtraNamespaces(cfg.GVisorNamespace)
	}
	// And the tenant land namespaces, so a land's pods count toward the pod
	// caps and are reconciled in place (gd-NDJIOutLsv).
	if landNamespacesEnabled(os.Getenv) {
		rec.SetNamespaceSource(func() []string { return tenantLandNamespaces(cfg) })
	}
	rec.SetBeadCloser(daemon)
	rec.SetStateUpdater(daemon)
	rec.SetFailedAgentMarker(daemon)
	rec.SetHarnessStamper(daemon)
	rec.SetSpawnTaskClaimer(machineDaemon)
	// Drain nudges (node reclaim, account rotation) go out as
	// mail and drain readiness comes from the agent bead, not the pod.
	rec.UpgradeTracker().SetMessenger(drainMessenger{Client: daemon, logger: logger})
	// C5 lambda_microvm lifecycle (epic ka-0000005010, ka-0000001837): reconcile
	// MicroVM agents by runtime status instead of pod presence so they are never
	// reaped as podless leaks nor cold-started as pods. Inert (IsMicrovmAgent
	// always false) when the MicroVM backend is default-off/unavailable.
	if microvmLifecycle.Available() {
		rec.SetMicrovmReconciler(microvmLifecycle)
	}
	// Exempt frozen-awaiting-thaw agents from the leaked-bead reaper: a frozen
	// agent has a terminal agent_state and no pod but a persisted snapshot, so
	// closing its bead would race the in-flight thaw (bug ka-0000000114). Only
	// wired when freeze/thaw is enabled (ft is nil otherwise).
	if ft != nil {
		rec.SetSnapshotChecker(ft.hasSnapshot)
		// Exempt the restored placeholder Pod B from the terminal-pod reaper
		// while its thaw is still rehydrating: Pod B flips to phase=Failed seconds
		// after the restore graft, and reaping it mid-rehydration tears the netns
		// down so both watcher acks fail ENETUNREACH (bug ka-0000002058).
		rec.SetThawChecker(ft.isThawing)
	}

	// Runtime-tunable config watcher (env→config-bead pilot, ka-0000003317).
	// Polls type=config beads titled "tunables" labeled component:controller.
	// The reconciler reads COOP_QUEUED_WARN_AGE through the watcher, so an
	// update to the config bead takes effect without a pod restart.
	tunables, err := beadsapi.NewConfigWatcher(daemon, "controller",
		beadsapi.WithLogger(logger.With("component", "config-watcher")))
	if err != nil {
		logger.Error("failed to create config watcher", "error", err)
		os.Exit(1)
	}
	// Start synchronously; a transient error on first load is tolerated — the
	// poll loop will retry and the reconciler falls back to the env/compiled
	// layer until beads become available.
	if err := tunables.Start(context.Background()); err != nil {
		logger.Warn("config watcher initial load failed (will retry)", "error", err)
	}
	rec.SetTunables(tunables)

	// Create stale agent reaper (nil if disabled).
	staleReaper := statusreporter.NewStaleReaper(daemon, pods, cfg.Namespace, cfg.StaleAgentTimeout, logger)
	if staleReaper != nil {
		// Route stale runsc agents' pod deletion to the gVisor namespace
		// (ka-0000008076). No-op when unset.
		staleReaper.SetGVisorNamespace(cfg.GVisorNamespace)
		// Attach the config watcher so the open-run-instance close gate
		// (STALEREAPER_CLOSE_OPEN_RUNINSTANCES, now default ON — an operator can
		// set it false as a kill-switch back to dry-run) and the runtime-ceiling
		// reap gate (STALEREAPER_RUNTIME_REAP, default OFF/dry-run) can be
		// flipped via a config-bead update without a restart (ka-0000000075,
		// ka-0000006054, ka-0000000716).
		staleReaper.SetTunables(tunables)
		// Arm the per-role runtime ceiling (bug ka-0000000716, extended to every
		// role by gd-X2eSin2J80): the reap-side spend-stopper complement to the
		// non-destructive lease-side age bound (!6050). A positive ceiling for a
		// role runs the watchdog in dry-run (log-only) for that role until an
		// operator flips STALEREAPER_RUNTIME_REAP; a role absent or zero in the
		// map disables it entirely for that role.
		staleReaper.SetRuntimeCeilings(cfg.RuntimeCeilingByRole)
		// Exempt frozen-awaiting-thaw agents (agent_state=stopping with a
		// persisted snapshot) from reaping, mirroring the reconciler's snapshot
		// exemption — reaping one destroys the thaw-target signal and forces the
		// next wake to cold-start (ka-0000002104). Only wired when freeze/thaw
		// is enabled (ft is nil otherwise).
		if ft != nil {
			staleReaper.SetSnapshotChecker(ft.hasSnapshot)
		}
		// On the machine principal, release stranded claims only on the types
		// its ownership grant covers: anything else is a 403 every pass
		// (gd-ozX2LfXsrv). The static token bypasses ownership, so it keeps
		// every issue type.
		if machineTS != nil {
			staleReaper.SetReclaimTypes(controllerMachineReclaimTypes)
		}
		logger.Info("stale agent reaper enabled", "timeout", cfg.StaleAgentTimeout)
	}

	// Tracker for the GET /healthz/snapshot endpoint. Components below
	// (reconciler periodic loop, stale reaper, rotation notifier) write into
	// it; the snapshot handler reads from it. See internal/healthsnapshot.
	snapshotTracker := healthsnapshot.NewTracker()

	// Create PVC garbage collector for orphaned workspace volumes.
	var pvcCollector *pvcgc.Collector
	if cfg.PVCGCInterval > 0 {
		pvcCollector = pvcgc.New(k8sClient, cfg.Namespace, cfg.PVCGCTTL, logger.With("component", "pvcgc"))
		// Also GC orphaned workspace PVCs left behind by runsc freeze/thaw
		// agents in the gVisor namespace (ka-0000008076). No-op when unset.
		if cfg.GVisorNamespace != "" {
			pvcCollector.SetExtraNamespaces(cfg.GVisorNamespace)
		}
		logger.Info("PVC garbage collection enabled", "interval", cfg.PVCGCInterval, "ttl", cfg.PVCGCTTL)
	}

	// Create PV garbage collector for Released PVs stuck due to CSI migration.
	var pvCollector *pvgc.Collector
	if cfg.PVGCInterval > 0 {
		pvNS := cfg.PVGCNamespaces
		if len(pvNS) == 0 {
			pvNS = []string{cfg.Namespace}
		}
		pvCollector = pvgc.New(k8sClient, pvNS, cfg.PVGCTTL, logger.With("component", "pvgc"))
		logger.Info("PV garbage collection enabled", "interval", cfg.PVGCInterval, "ttl", cfg.PVGCTTL, "namespaces", pvNS)
	}

	// Orphaned-Pending-PVC observability collector (ka-0000002737): publishes a
	// live series for PVCs stuck Pending for months with no consumer — a silent
	// storage/EBS cost leak the GC collectors do not touch (foreign namespaces).
	// Opt-in — only constructed when the interval is set, which the chart does in
	// lockstep with the cluster-scoped PVC+pod list RBAC (agents.pvcObs.enabled).
	var pvcObsCollector *pvcobs.Collector
	if cfg.PVCObsInterval > 0 {
		pvcObsCollector = pvcobs.New(k8sClient, cfg.PVCObsThreshold, cfg.PVCObsInterval, logger.With("component", "pvcobs"))
		logger.Info("orphaned-pending-PVC observability collector enabled", "interval", cfg.PVCObsInterval, "threshold", cfg.PVCObsThreshold)
	}

	// Create the node-lifecycle collector (Track D, ka-0000001509): publishes
	// gardenio_node_lifecycle_events_total{reason=...} so the AgentNodeLifecycleEvents
	// Slack alert has a live series for node reclaim/disruption/deletion churn —
	// the node-lifecycle half of Matt's OOM+node-lifecycle alerting ask
	// (ka-0000006448). Opt-in — only constructed when the interval is set, which
	// the chart does in lockstep with the cluster-scoped nodes:list/watch RBAC
	// (agents.nodeLifecycle.enabled).
	var nodeLifecycleCollector *nodelifecycle.Collector
	if cfg.NodeLifecycleInterval > 0 {
		nodeLifecycleCollector = nodelifecycle.New(k8sClient, cfg.Env, cfg.NodeLifecycleInterval, cfg.NodeLifecycleNodeSelector, cfg.NodeLifecycleIdlePodNamespaces, logger.With("component", "nodelifecycle"))
		logger.Info("node-lifecycle collector enabled", "interval", cfg.NodeLifecycleInterval, "nodeSelector", cfg.NodeLifecycleNodeSelector, "idlePodNamespaces", cfg.NodeLifecycleIdlePodNamespaces)
	}

	// Slack notifications, decision watcher, and mail watcher are now handled
	// by the standalone slack-bridge binary (cmd/slack-bridge). The controller
	// only handles K8s pod lifecycle operations. See bd-8x8fy.

	// Create multi-pool manager. Pool config comes from project beads
	// (prewarmed_pool JSON field), so the manager is always created but only
	// reconciles projects that have a pool enabled.
	pool := poolmanager.New(daemon, cfg, logger.With("component", "poolmanager"))
	// Wire the stale-image recycle metric (ka-0000001689): prewarmed_agents_cycled_total.
	// poolMetrics is constructed above and registered on the :9092 registry below.
	pool.SetCycleRecorder(poolMetrics)
	// Wire the assignment hit/empty/error metric (ka-0000000550): prewarmed_assigns_total.
	pool.SetAssignRecorder(poolMetrics)

	// Phase 3b/D10b: the scheduler top loop has been removed from this
	// binary; cmd/trigger-engine owns scheduled-gardener firing and its own
	// spawnguard instance.

	// Set up Prometheus metrics collector for fleet cost/usage.
	// Lean agent roster: the 30 s collector reads only counters and identity
	// keys, never fields.prompt / wrapup (gd-zXdZq4Traf).
	metricsCollector := gardeniometrics.New(beadsapi.LeanAgentLister{Client: daemon}, logger, 30*time.Second)
	// Count reconcile-pass outcomes on the shared registry as
	// gardenio_reconcile_total{controller,result} for prod SLO S2 (ka-0000001213).
	rec.SetReconcileRecorder(metricsCollector)
	// Count drain episodes as gardenio_agent_lifecycle_events_total{event="drained"}
	// (ka-0000000550). eventMetrics is registered on the same registry below.
	rec.SetLifecycleRecorder(eventMetrics)

	// Leg-B Claude status-page series (ka-0000000487). Built here — NOT at
	// package var-init — so they can register on promRegistry below, the ONLY
	// registry :9092 serves. The old promauto version registered on the global
	// DefaultRegisterer BEFORE the swap below, so the controller's series landed
	// on a registry :9092 never served (leg B dark) while the four bridge
	// binaries that import this package exported phantom zero series and made
	// ClaudeStatusPollerStale fire forever (ka-0000008266). Built and registered
	// ONLY when the poller is enabled: a disabled poller must export nothing so
	// the deadman's absent() clause — not a stale phantom last_check=0 — is what
	// reports leg B as off. Threaded into the poller in run().
	var claudeStatusMetrics *bridge.ClaudeStatusMetrics
	if cfg.ClaudeStatusPollInterval > 0 {
		claudeStatusMetrics = bridge.NewClaudeStatusMetrics()
	}

	promRegistry := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = promRegistry
	prometheus.DefaultGatherer = promRegistry
	promRegistry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	promRegistry.MustRegister(collectors.NewGoCollector())
	if err := metricsCollector.Register(promRegistry); err != nil {
		logger.Error("failed to register metrics", "error", err)
		os.Exit(1)
	}
	if err := eventMetrics.Register(promRegistry); err != nil {
		logger.Error("failed to register event metrics", "error", err)
		os.Exit(1)
	}
	if claudeStatusMetrics != nil {
		if err := claudeStatusMetrics.Register(promRegistry); err != nil {
			logger.Error("failed to register claude status metrics", "error", err)
			os.Exit(1)
		}
	}
	promRegistry.MustRegister(landSpawnRefusals)
	promRegistry.MustRegister(beadsapi.ListRequestsCollector())
	if err := spendMetrics.Register(promRegistry); err != nil {
		logger.Error("failed to register spend metrics", "error", err)
		os.Exit(1)
	}
	if err := xcheckMetrics.Register(promRegistry); err != nil {
		logger.Error("failed to register xcheck metrics", "error", err)
		os.Exit(1)
	}
	if err := resumeMetrics.Register(promRegistry); err != nil {
		logger.Error("failed to register resume metrics", "error", err)
		os.Exit(1)
	}
	if err := freezeMetrics.Register(promRegistry); err != nil {
		logger.Error("failed to register freeze metrics", "error", err)
		os.Exit(1)
	}
	if err := microvmLaunchMetrics.Register(promRegistry); err != nil {
		logger.Error("failed to register microvm launch metrics", "error", err)
		os.Exit(1)
	}
	if err := toolLatencyMetrics.Register(promRegistry); err != nil {
		logger.Error("failed to register tool latency metrics", "error", err)
		os.Exit(1)
	}
	if err := poolMetrics.Register(promRegistry); err != nil {
		logger.Error("failed to register pool metrics", "error", err)
		os.Exit(1)
	}
	if err := probeMetrics.Register(promRegistry); err != nil {
		logger.Error("failed to register probe metrics", "error", err)
		os.Exit(1)
	}
	if err := rec.DeferredMetrics().Register(promRegistry); err != nil {
		logger.Error("failed to register deferred metrics", "error", err)
		os.Exit(1)
	}
	if err := rec.IdleMetrics().Register(promRegistry); err != nil {
		logger.Error("failed to register idle-agent measurement metrics", "error", err)
		os.Exit(1)
	}
	// Agent-token lifecycle metrics (provisions/revocations + revoke-latency
	// SLO histogram). Raw token id / provider id NEVER appear as labels.
	tokMetrics := newTokenMetrics()
	if err := tokMetrics.Register(promRegistry); err != nil {
		logger.Error("failed to register token metrics", "error", err)
		os.Exit(1)
	}
	// Bridge fleet-token rotator metrics (ka-0000002353 plan item 3). Registered
	// only when the rotator is enabled — a disabled rotator exports nothing and
	// BridgeTokenRotationOverdue's absent() clause reports it as off, rather than
	// a phantom stale expiry gauge. Passed into run() and handed to the rotator.
	var rotatorMetrics *bridgetokenrotator.Metrics
	if cfg.BridgeTokenRotationEnabled {
		rotatorMetrics = bridgetokenrotator.NewMetrics()
		if err := rotatorMetrics.Register(promRegistry); err != nil {
			logger.Error("failed to register bridge fleet-token rotator metrics", "error", err)
			os.Exit(1)
		}
	}
	if pvcObsCollector != nil {
		if err := pvcObsCollector.Register(promRegistry); err != nil {
			logger.Error("failed to register orphaned-pending-PVC observability metrics", "error", err)
			os.Exit(1)
		}
	}
	if nodeLifecycleCollector != nil {
		if err := nodeLifecycleCollector.Register(promRegistry); err != nil {
			logger.Error("failed to register node-lifecycle metric", "error", err)
			os.Exit(1)
		}
	}

	// Start lightweight health/version HTTP server.
	healthAddr := os.Getenv("HEALTH_LISTEN_ADDR")
	if healthAddr == "" {
		healthAddr = ":8091"
	}
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":"%s"}`, version)
	})
	healthMux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"version":    version,
			"commit":     commit,
			"agentImage": cfg.CoopImage,
			"namespace":  cfg.Namespace,
		})
	})
	registerAutodestructEndpoints(healthMux, rec, logger)
	// Shared bearer-auth for the state-mutating :8091 endpoints (pool/assign,
	// freeze/interrupt, microvm/resume). Opt-in via CONTROLLER_API_TOKEN — a
	// pass-through no-op (with a startup warning) when unset, so the default
	// posture is byte-identical to before. See api_auth.go.
	apiAuth := newControllerAPIAuth(logger)
	// ChatGPT-plan device login (ADR 0007). Strict bearer auth (off without
	// CONTROLLER_API_TOKEN); served only by the leader, which sets the broker.
	registerCodexLoginEndpoints(healthMux, codexLoginBrokers)
	healthMux.HandleFunc("POST /api/v1/pool/assign", apiAuth(handlePoolAssign(pool, logger)))
	healthMux.HandleFunc("GET /config/tunables", handleTunables(tunables))
	// C2 interruptible freeze (ka-0000005834): the message-ingress seam (bridge
	// bot_mentions) POSTs here when a user message targets an agent that may be
	// mid-freeze, so the controller can abort the checkpoint and keep the agent
	// live to serve the message. No-op (404 "not freezing") when the agent has no
	// freeze in flight. Inert unless FreezeInterruptEnabled (ft.abortFreeze gates
	// on the registry, which is empty when the flag is off).
	healthMux.HandleFunc("POST /api/v1/freeze/interrupt", apiAuth(handleFreezeInterrupt(ft, logger)))

	// Re-nudge mid-THAW coordination (ka-0000005830, design ka-0000005125): the
	// message-ingress seam (bridge bot_mentions) GETs here to learn whether a
	// thaw of the targeted agent is in flight. When thawing, the bridge BUFFERS
	// the nudge (rather than racing a half-restored agent or injecting a
	// duplicate respawn) until the thaw releases the freshly-credentialed Pod B.
	// Reports thawing:false — so the buffer is a no-op and routing is
	// byte-identical to today — whenever the lifecycle is nil (freeze/thaw off),
	// mirroring how freeze/interrupt 404s when off. Zero blast radius by default.
	healthMux.HandleFunc("GET /api/v1/thaw/status", handleThawStatus(ft, logger))

	// MicroVM resume-on-message (step 3, plan ka-0000004518): the message-ingress
	// seam (bridge respawnThreadAgent) POSTs here when the next message arrives for
	// a SUSPENDED MicroVM thread agent. This is the native analog of the gVisor
	// name-keyed thaw — but a MicroVM cannot be resumed by closing + reminting the
	// bead (that fires AgentDone -> Terminate and destroys the suspended VM). So
	// the bridge asks the controller to ResumeMicrovm the SAME open bead's
	// runtime_id in place. Returns 200 {"resumed":false} (the routing-unchanged
	// signal) whenever the MicroVM backend is off or the bead is not a suspended
	// MicroVM agent, so the bridge falls back to its normal path with zero blast
	// radius by default.
	healthMux.HandleFunc("POST /api/v1/microvm/resume", apiAuth(handleMicrovmResume(microvmLifecycle, daemon, resumeMetrics, logger)))

	// MicroVM resume rehydration status (step 5, plan ka-0000004518): reports
	// whether the fail-closed proof-of-life rendezvous is in flight for an agent
	// after a resume (the MicroVM analog of GET /api/v1/thaw/status). Reports
	// rehydrating:false when the backend is off or the rendezvous has settled.
	healthMux.HandleFunc("GET /api/v1/microvm/resume/status", handleMicrovmResumeStatus(microvmLifecycle, logger))

	// Health snapshot endpoint backs the gardenio health page in beads-viewer.
	// Returns controller-side signals (pod counts by phase, recent reaper
	// actions, recent rotation events, last reconcile times) as JSON shaped
	// to match the R5 chart-catalog contract per tile. See epic
	// ka-0000000152 and gardenio/docs/research/beads-viewer-health-page.md.
	snapshotter := healthsnapshot.NewSnapshotter(snapshotTracker, pods, cfg.Namespace)
	healthMux.Handle("GET /healthz/snapshot", healthsnapshot.NewHandler(snapshotter, healthsnapshot.DefaultCacheTTL))
	healthSrv := &http.Server{
		Addr:              healthAddr,
		Handler:           healthMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("starting health/version server", "addr", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health server failed", "error", err)
		}
	}()

	// Start metrics server on a separate internal-only port.
	metricsAddr := os.Getenv("METRICS_LISTEN_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":9092"
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.HandlerFor(promRegistry, promhttp.HandlerOpts{}))
	metricsSrv := &http.Server{
		Addr:              metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("starting metrics server", "addr", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Start metrics collection in background.
	go metricsCollector.Start(ctx)

	// Start the orphaned-Pending-PVC observability collector (ka-0000002737).
	// No-op when disabled (pvcObsCollector is nil unless PVC_OBS_INTERVAL > 0).
	if pvcObsCollector != nil {
		go pvcObsCollector.RunLoop(ctx)
	}

	// Start the node-lifecycle collector (Track D). No-op when disabled
	// (nodeLifecycleCollector is nil unless NODE_LIFECYCLE_INTERVAL > 0).
	if nodeLifecycleCollector != nil {
		go nodeLifecycleCollector.RunLoop(ctx)
	}

	runFn := func(ctx context.Context) {
		if err := run(ctx, logger, cfg, k8sClient, watcher, pods, status, rec, daemon, machineDaemon, secretRec, pool, muxClient, pvcCollector, pvCollector, staleReaper, snapshotTracker, ft, microvmLifecycle, microvmSessionVendor, eventMetrics, spendMetrics, resumeMetrics, freezeMetrics, probeMetrics, xcheckMetrics, microvmLaunchMetrics, claudeStatusMetrics, tokMetrics, rotatorMetrics, toolLatencyMetrics); err != nil {
			logger.Error("controller stopped", "error", err)
			waitForCodexRotations(logger)
			os.Exit(1)
		}
	}

	if cfg.LeaderElection {
		runLeaderElection(ctx, logger, cfg, k8sClient, runFn)
	} else {
		runFn(ctx)
	}
	// run returned because ctx was cancelled (SIGTERM): let a Codex rotation that
	// already holds a fresh single-use refresh token reach the Secret first.
	waitForCodexRotations(logger)
}

func buildK8sConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func buildK8sClient(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := buildK8sConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("building k8s config: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}

func setupLogger(level string) *slog.Logger {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
}
