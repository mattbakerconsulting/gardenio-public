package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// microvmSuspendOnStop reports whether a MicroVM agent's graceful stop should
// SUSPEND the VM (leave the bead open as a resume target) rather than terminate
// it. It is the teardown-side twin of the spawn-side isMicrovmSuspendEligible
// (podspec.go): the master switch MICROVM_SUSPEND_ENABLED is on AND the agent is
// a thread agent (bound to a Slack thread, so a later re-mention has something to
// resume). It reads the thread binding from the agent bead's
// slack_thread_channel/slack_thread_ts fields — the SAME metadata the spawn path
// stamped SLACK_THREAD_* + BOAT_FREEZE_ELIGIBLE from — so the in-VM `gb stop`
// suspend decision and this controller teardown decision never drift. The caller
// gates this on event.Type==AgentStop; kill/done never reach here.
func microvmSuspendOnStop(cfg *config.Config, event subscriber.Event) bool {
	return cfg.MicrovmSuspendEnabled &&
		event.Metadata["slack_thread_channel"] != "" &&
		event.Metadata["slack_thread_ts"] != ""
}

// microvmTeardowner is the slice of *microvm.Lifecycle the lambda_microvm
// stop/kill/done teardown branch needs. Isolating it behind an interface lets
// the suspend-vs-terminate decision (microvmTeardown) be unit-tested without a
// live AWS backend — the same seam step 3's resume endpoint uses (microvmResumer).
type microvmTeardowner interface {
	Available() bool
	Suspend(ctx context.Context, view microvm.AgentBeadView, reason string) error
	Terminate(ctx context.Context, view microvm.AgentBeadView, reason string) error
}

// microvmTeardownOutcome is what microvmTeardown decided for a stop/kill/done event.
type microvmTeardownOutcome int

const (
	// microvmTeardownNotApplicable: the event is not for a live MicroVM agent
	// (backend off, or execution_target != lambda_microvm). The caller falls
	// through to the K8s pod-teardown machinery unchanged.
	microvmTeardownNotApplicable microvmTeardownOutcome = iota
	// microvmTeardownSuspended: a freeze-eligible thread agent's graceful stop
	// SUSPENDED the VM and LEFT THE BEAD OPEN as the resume target. The caller
	// emits session-close metrics and returns WITHOUT closing the bead.
	microvmTeardownSuspended
	// microvmTeardownTerminated: the VM was terminated (kill/done, a non-eligible
	// stop, or a suspend that failed and fell back to terminate). The caller emits
	// session-close metrics and returns.
	microvmTeardownTerminated
)

// microvmTeardown performs the lambda_microvm runtime teardown for a
// stop/kill/done event and reports what it did, so the caller can emit the
// shared session-close metrics + return once. A MicroVM agent has no pod to
// freeze/delete: stop/kill/done maps to Lifecycle.Terminate (TerminateMicrovm +
// per-run secret/credential cleanup, bead-ID idempotency token) — EXCEPT a
// freeze-eligible thread agent's graceful AgentStop, which SUSPENDS the VM and
// leaves the bead open at agent_state=stopping as the resume target (plan
// ka-0000004518, step 2 ka-0000007189) — the native analog of the gVisor freeze
// path. Only AgentStop suspends: AgentKill (forced failure) and AgentDone (work
// complete) have no wake to serve and terminate. The suspend gate mirrors the
// spawn-side isMicrovmSuspendEligible exactly (master switch + thread binding) so
// the in-VM BOAT_FREEZE_ELIGIBLE signal and this teardown decision never drift;
// off (the default) every MicroVM stop terminates byte-for-byte as before. A
// suspend that fails falls through to Terminate so a half-suspended runtime with
// an open bead can't leak (the leak GC backstops either outcome).
func microvmTeardown(ctx context.Context, logger *slog.Logger, cfg *config.Config, event subscriber.Event, agentBeadID string, life microvmTeardowner, resumeMetrics *gardeniometrics.ResumeMetrics) microvmTeardownOutcome {
	if !life.Available() ||
		event.Metadata["execution_target"] != podmanager.ExecutionTargetLambdaMicroVM {
		return microvmTeardownNotApplicable
	}
	view := microvm.AgentBeadView{
		ID:              agentBeadID,
		AgentState:      event.Metadata["agent_state"],
		ExecutionTarget: event.Metadata["execution_target"],
		RuntimeID:       event.Metadata["runtime_id"],
		RuntimeState:    event.Metadata["runtime_state"],
	}
	reason := string(event.Type)

	if event.Type == subscriber.AgentStop && microvmSuspendOnStop(cfg, event) {
		if err := life.Suspend(ctx, view, reason); err != nil {
			// Suspend failed — fall through to Terminate so the stop still tears the
			// VM down rather than leaking a half-suspended runtime with an open bead
			// the reconciler would keep reviving. The leak GC backstops either way.
			// Count it as a standby-entry attempt that fell back to terminate, so the
			// suspend/fallback ratio quantifies how often standby entry itself fails
			// (step 6, ka-0000002861).
			logger.Error("lambda_microvm: suspend on graceful stop failed, terminating instead (leak GC backstops)",
				"bead", agentBeadID, "runtime_id", view.RuntimeID, "error", err)
			resumeMetrics.RecordMicrovmSuspend(event.Role, gardeniometrics.MicrovmSuspendOutcomeFallbackTerminated)
		} else {
			// Suspended: the bead stays OPEN at agent_state=stopping (the resume
			// target). Per-run credentials + Ziti leases are deliberately NOT revoked
			// (they must survive standby; see Lifecycle.Suspend). The caller emits
			// session-close metrics and returns WITHOUT closing the bead or touching
			// the K8s teardown machinery.
			logger.Info("lambda_microvm: agent runtime suspended on graceful stop (bead left open as resume target)",
				"bead", agentBeadID, "runtime_id", view.RuntimeID)
			resumeMetrics.RecordMicrovmSuspend(event.Role, gardeniometrics.MicrovmSuspendOutcomeSuspended)
			return microvmTeardownSuspended
		}
	}

	if err := life.Terminate(ctx, view, reason); err != nil {
		logger.Error("lambda_microvm: terminate on teardown failed (leak GC backstops)",
			"bead", agentBeadID, "runtime_id", view.RuntimeID, "reason", reason, "error", err)
	} else {
		logger.Info("lambda_microvm: agent runtime terminated on teardown",
			"bead", agentBeadID, "runtime_id", view.RuntimeID, "reason", reason)
	}
	// Per-run credentials (beads token, git token) are revoked inside
	// Lifecycle.Terminate via the credential revoker — the single owner of the
	// NORMAL-TERMINATION exit path — so no direct revoke here.
	return microvmTeardownTerminated
}

// handleNonK8sSpawn handles a spawn for an agent whose resolved execution target
// is not the Kubernetes pod backend. For lambda_microvm (epic ka-0000005010) it
// dispatches to the MicroVM lifecycle's Launch — creating the per-run secret +
// idempotent RunMicrovm (C4) AND persisting runtime_id/runtime_state/… onto the
// agent bead so later reconcile passes drive GetMicrovm off it (C5). It fails
// closed (marks the agent failed) when:
//   - the target is lambda_microvm but no MicroVM backend is available
//     (default-off / misconfigured — never fall through to a pod), or
//   - the target is any other unrecognized non-Kubernetes value.
//
// This guarantees the handoff invariant that one spawn event never produces a
// Kubernetes pod for a MicroVM-targeted agent, and never a MicroVM for a
// Kubernetes agent.
func handleNonK8sSpawn(ctx context.Context, logger *slog.Logger, cfg *config.Config, daemon, machineDaemon *beadsapi.Client, k8sClient kubernetes.Interface, microvmLifecycle *microvm.Lifecycle, microvmSessionVendor *microvm.SessionVendor, agentBeadID string, spec podmanager.AgentPodSpec, microvmLaunchMetrics *gardeniometrics.MicrovmLaunchMetrics) error {
	target := spec.ExecutionTarget

	if target == podmanager.ExecutionTargetLambdaMicroVM && microvmLifecycle.Available() {
		// dispatchStart stamps the controller-owned launch window (task
		// ka-0000003938): everything from here to backend.Launch returning a
		// live handle — per-run credential resolution + SM writes + broker lease
		// + RunMicrovm accept. Observed as gardenio_microvm_launch_duration_seconds
		// on BOTH the failed- and successful-launch exits below.
		dispatchStart := time.Now()
		agentSpec := microvmAgentSpecFromPodSpec(agentBeadID, spec)
		// Credential delivery (bug ka-0000007063): the runtime-neutral translation
		// above forwards NO beads token and NO Claude credential — both are
		// injected on the K8s path via Secret env-refs that never reach spec.Env.
		// Source them controller-side and fold them into the AgentSpec so
		// backend.go writes them ONLY into the per-run, task-scoped secret. Without
		// this the in-VM agent cannot authenticate beads writes (C7 item 4) or run
		// claude (C7 item 3). See microvm_credentials.go.
		beadsAccess, beadsRefresh := resolveMicrovmBeadsToken(ctx, logger, machineDaemon, cfg, agentBeadID, spec.AgentName, spec.Project)
		if cfg.PerAgentTokens && agentBeadID != "" && beadsAccess == "" {
			// Fail closed (P7 Task 1): never fall back to the shared token.
			logger.Error("refusing microvm launch: per-agent token provisioning failed",
				"bead", agentBeadID, "agent", spec.AgentName, "project", spec.Project)
			return fmt.Errorf("per-agent token provisioning failed for %s", spec.AgentName)
		}
		if !cfg.PerAgentTokens && beadsAccess == "" {
			// Shared-token mode with no shared token in the process: the
			// controller is on its machine principal (gd-BDirD4F7Iq), which is
			// never handed to a satellite.
			logger.Error("refusing microvm launch: no beads token to deliver (shared-token mode needs BEADS_DAEMON_TOKEN; enable agents.perAgentTokens)",
				"bead", agentBeadID, "agent", spec.AgentName, "project", spec.Project)
			return fmt.Errorf("no beads token to deliver to microvm %s: enable agents.perAgentTokens", spec.AgentName)
		}
		claudeToken := resolveMicrovmClaudeOAuthToken(ctx, logger, cfg, k8sClient, spec)
		applyMicrovmCredentials(&agentSpec, beadsAccess, beadsRefresh, claudeToken)
		// NATS contract (task ka-0000006995): the translation strips the pod's
		// cluster-DNS NATS env; re-deliver the pod contract with the in-VM URL
		// (local Ziti proxy listener) plus the agent NATS token read from the
		// same Secret the pod path projects as COOP_NATS_TOKEN_FILE.
		applyMicrovmNats(&agentSpec, resolveMicrovmNatsToken(ctx, logger, cfg, k8sClient, spec), microvmNatsUser(cfg))
		// Slack-bridge auth (ka-0000007184): the pod path injects
		// SLACK_BRIDGE_TOKEN as a Secret env-ref that never reaches spec.Env;
		// re-deliver it via the per-run secret so in-VM gb slack calls
		// authenticate. The bridge URL itself is image-owned (stamped by the
		// entrypoint iff the image's proxy set has the slackbridge listener).
		applyMicrovmSlackBridge(&agentSpec, resolveMicrovmSlackBridgeToken(ctx, logger, cfg, k8sClient, spec))
		// Coopmux broker auth (ka-0000006120): the pod path injects
		// COOP_BROKER_TOKEN / COOP_MUX_TOKEN as Secret env-refs that never
		// reach spec.Env; re-deliver them via the per-run secret so the in-VM
		// gb init credential wait authenticates instead of 401-ing through its
		// full 90s timeout (the dominant term in time-to-working-agent).
		applyMicrovmCoopmux(&agentSpec, resolveMicrovmCoopmuxToken(ctx, logger, cfg, k8sClient, spec))
		// Git credentials (ka-0000004762): mint a per-run project-scoped GitLab
		// token (factory configured) or fall back to the project's shared git
		// secrets; either way the values ride ONLY the per-run task-scoped
		// secret. Minted-token coordinates (id + project path, not the value)
		// are persisted onto the agent bead so the timeout/leak-GC exit paths
		// can revoke after a controller restart.
		gitCred := resolveMicrovmGitCredentials(ctx, logger, newMicrovmGitMinter(cfg, logger), k8sClient, spec)
		applyMicrovmGitCredentials(&agentSpec, gitCred)
		persistMicrovmGitToken(ctx, logger, daemon, agentBeadID, gitCred)
		// Bead-scoped S3 session credentials (S4 ka-0000006612): mint a
		// short-lived credential narrowed to coop/sessions/<bead>/* and fold it
		// into the per-run secret so the in-VM coop can persist/restore its
		// session. The MINIMAL execution role grants no sessions-bucket access
		// itself; this is the ONLY session-S3 authority a satellite gets. No-op
		// when vending is disabled (nil vendor). Nil interface (not a typed-nil)
		// so resolveMicrovmSessionCredentials's guard fires correctly.
		if microvmSessionVendor != nil {
			sessionCreds := resolveMicrovmSessionCredentials(ctx, logger, microvmSessionVendor, agentBeadID)
			applyMicrovmSessionCredentials(&agentSpec, sessionCreds, microvmSessionVendor.Region())
		}
		// Lifecycle.Launch runs the C4 backend launch AND persists the C5 runtime
		// metadata (execution_target/runtime_id/runtime_account/region/started_at)
		// onto the agent bead, so the reconciler recognises it as a MicroVM agent
		// and reconciles it by runtime status thereafter.
		handle, err := microvmLifecycle.Launch(ctx, agentSpec)
		if err != nil {
			// Launch already rolled back any per-run secret it wrote. This is
			// the FAILED-LAUNCH exit path: revoke every per-run credential
			// minted above (per-agent beads token, per-run git token) so a
			// failed launch leaves nothing live, then mark the agent failed so
			// the control plane doesn't wait on a runtime that never came up.
			microvmLaunchMetrics.RecordLaunch(time.Since(dispatchStart).Seconds(),
				gardeniometrics.MicrovmLaunchFailed, spec.Project, spec.Role)
			microvmLifecycle.RevokeCredentials(ctx, agentBeadID, "failed launch")
			logger.Error("lambda_microvm: launch failed; marking agent failed",
				"bead", agentBeadID, "project", spec.Project, "role", spec.Role, "error", err)
			markAgentFailed(ctx, logger, daemon, agentBeadID, target,
				fmt.Errorf("lambda_microvm launch failed: %w", err))
			return err
		}
		launchSeconds := time.Since(dispatchStart).Seconds()
		microvmLaunchMetrics.RecordLaunch(launchSeconds, gardeniometrics.MicrovmLaunchSuccess, spec.Project, spec.Role)
		logger.Info("lambda_microvm: agent launched",
			"bead", agentBeadID, "microvm_id", handle.MicrovmID,
			"project", spec.Project, "role", spec.Role,
			"launch_dispatch_seconds", launchSeconds)
		return nil
	}

	// probe (ExecutionTargetProbe) is a recognized target, but its lifecycle is
	// CLI-driven (`gb probe run <script-bead>`), not the async agent spawn door:
	// a probe is a one-off scripted pod that reports done/failed by exit code and
	// has no agent bead, coop, or credentials. So an agent bead that resolves to
	// probe must fail closed here with an accurate pointer rather than the generic
	// "unrecognized" message — the same fail-closed contract, but diagnosable.
	if podmanager.IsProbeTarget(target) {
		reason := "execution_target=probe is driven by `gb probe run <script-bead>`, not the agent spawn door"
		logger.Warn("agent resolved to the probe execution target; failing closed (probe pods are CLI-driven)",
			"bead", agentBeadID, "execution_target", target, "reason", reason)
		markAgentFailed(ctx, logger, daemon, agentBeadID, target, errors.New(reason))
		return nil
	}

	// lambda_microvm requested but backend unavailable, OR an unrecognized
	// non-Kubernetes target: fail closed rather than spawning a pod.
	reason := "MicroVM backend unavailable (default-off or misconfigured)"
	if target != podmanager.ExecutionTargetLambdaMicroVM {
		reason = "unrecognized non-kubernetes execution target"
	}
	logger.Warn("agent resolved to a non-kubernetes execution target; failing closed instead of spawning a pod",
		"bead", agentBeadID, "execution_target", target, "reason", reason)
	markAgentFailed(ctx, logger, daemon, agentBeadID, target, errors.New(reason))
	return nil
}

// runMicrovmLeakGC runs one lambda_microvm leak-GC sweep (C5, ka-0000001837).
// It builds the live set from the CURRENT open agent beads — the runtime_ids of
// open MicroVM agents, plus every open agent bead ID — and hands it to the
// lifecycle GC, which terminates any runtime/secret no live bead claims (and
// which is older than the GC min-age grace). It also collects the open,
// controller-suspended MicroVM thread-agent beads as standby candidates so the
// GC can enforce the max-standby-age cap (step 4, ka-0000006202): a SUSPENDED VM
// is legitimately claimed by its open resume-target bead (so the leak loop
// leaves it alone), and the cap is the only controller-side seam that bounds how
// long that standby may bill. Fail-safe: if the daemon list fails, GC is skipped
// entirely this pass (an empty live set would otherwise look like "everything is
// leaked" and mass-terminate live runtimes).
func runMicrovmLeakGC(ctx context.Context, logger *slog.Logger, daemon *beadsapi.Client, lifecycle *microvm.Lifecycle) {
	beads, err := daemon.ListAgentBeadsLean(ctx)
	if err != nil {
		logger.Warn("microvm GC: failed to list agent beads; skipping sweep (fail-safe)", "error", err)
		return
	}
	liveRuntimeIDs := make(map[string]bool)
	liveBeadIDs := make(map[string]bool, len(beads))
	var standby []microvm.StandbyCandidate
	for _, b := range beads {
		liveBeadIDs[b.ID] = true
		rid := b.Metadata["runtime_id"]
		if rid != "" {
			liveRuntimeIDs[rid] = true
		}
		// A standby candidate is an open MicroVM agent the controller suspended:
		// runtime in SUSPENDED/SUSPENDING with a runtime_suspended_at anchor. It
		// stays claimed by liveRuntimeIDs above (standby != leak); the GC's
		// max-standby-age cap decides whether it has outlived its standby budget.
		if rid != "" &&
			b.Metadata["execution_target"] == microvm.ExecutionTargetLambdaMicroVM &&
			isSuspendedRuntimeState(b.Metadata["runtime_state"]) &&
			b.Metadata["runtime_suspended_at"] != "" {
			standby = append(standby, microvm.StandbyCandidate{
				BeadID:             b.ID,
				RuntimeID:          rid,
				AgentState:         b.AgentState,
				RuntimeSuspendedAt: b.Metadata["runtime_suspended_at"],
			})
		}
	}
	stats, err := lifecycle.GC(ctx, liveRuntimeIDs, liveBeadIDs, standby, time.Now())
	if err != nil {
		// A list error inside GC is a hard stop that reaped nothing — log and move on.
		logger.Warn("microvm GC: sweep aborted on enumeration error", "error", err)
		return
	}
	if stats.RuntimesReaped > 0 || stats.SecretsDeleted > 0 || stats.StandbyReaped > 0 {
		logger.Info("microvm GC: reaped leaked resources",
			"runtimes_scanned", stats.RuntimesScanned, "runtimes_reaped", stats.RuntimesReaped,
			"secrets_scanned", stats.SecretsScanned, "secrets_deleted", stats.SecretsDeleted,
			"standby_reaped", stats.StandbyReaped)
	}
}

// isSuspendedRuntimeState reports whether a persisted runtime_state marks a VM
// as in controller-driven standby (SUSPENDING → SUSPENDED, both Live). Used to
// pick out standby candidates for the max-standby-age cap.
func isSuspendedRuntimeState(state string) bool {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUSPENDED", "SUSPENDING":
		return true
	default:
		return false
	}
}

// markAgentFailed hard-fails a non-Kubernetes agent bead. It records BOTH
// agent_state=failed AND a human-legible failure_reason (the cause), so the
// operator's failed card renders "Failed: <cause>" via the bridge's
// failureReasonTLDR fallback instead of a bare metadata-only card with no
// explanation (ka-0000006530 — a MicroVM Launch failure surfaced as an
// inscrutable "failed spawn error"). A nil cause degrades to agent_state-only
// (still recognized by the card as "Agent failed before an explicit wrap-up").
func markAgentFailed(ctx context.Context, logger *slog.Logger, daemon *beadsapi.Client, agentBeadID, target string, cause error) {
	if agentBeadID == "" || daemon == nil {
		return
	}
	if cause == nil {
		if err := daemon.UpdateAgentState(ctx, agentBeadID, "failed"); err != nil {
			logger.Warn("failed to mark non-k8s agent as failed",
				"bead", agentBeadID, "execution_target", target, "error", err)
		}
		return
	}
	if err := daemon.MarkAgentFailed(ctx, agentBeadID, cause.Error()); err != nil {
		logger.Warn("failed to mark non-k8s agent as failed",
			"bead", agentBeadID, "execution_target", target, "cause", cause, "error", err)
	}
}

// microvmClusterInternalEnvKeys are the env vars the pod path points at
// cluster-internal service DNS (e.g. nats://gardenio-nats:4222,
// http://gardenio-coopmux:...) that the MicroVM image OWNS itself, pointing them
// at its local Ziti proxy listeners on 127.0.0.1 (nfd!15: Beads 18080, Coopmux
// 19800, NATS 14222, LiteLLM 14000; Slack bridge 18090 added by
// ka-0000007184). Forwarding the pod's cluster-internal
// values would clobber the image's local-proxy config with hostnames the
// MicroVM cannot resolve, breaking overlay connectivity. COOP_HOST=0.0.0.0 and
// any POD_IP-derived callback are K8s-only. COOP_NATS_TOKEN_FILE points at the
// pod's tmpfs Secret mount (W5, ka-0000006585) — a path that does not exist in
// the MicroVM, so forwarding it would leave coop reading a phantom file every
// reconnect; the NATS token is delivered inline via the C4 per-run secret
// instead. These keys are stripped from the forwarded environment; the NATS
// trio (BEADS_NATS_URL, COOP_NATS_URL, COOP_NATS_FORWARD) is then re-stamped
// with the in-VM local-proxy contract by applyMicrovmNats (ka-0000006995).
var microvmClusterInternalEnvKeys = map[string]struct{}{
	"BEADS_HTTP_ADDR":      {},
	"BEADS_GRPC_ADDR":      {},
	"BEADS_NATS_URL":       {},
	"COOP_BROKER_URL":      {},
	"COOP_MUX_URL":         {},
	"COOP_NATS_URL":        {},
	"COOP_NATS_FORWARD":    {},
	"COOP_NATS_TOKEN_FILE": {},
	"COOP_HOST":            {},
	"ANTHROPIC_BASE_URL":   {},
	"BEADS_E2E_HTTP_ADDR":  {},
	// The image owns SLACK_BRIDGE_URL (ka-0000007184): its entrypoint stamps
	// http://127.0.0.1:18090 only when its baked ZITI_PROXY_SERVICES set
	// carries the gardenio-prod-slackbridge listener (same self-synchronizing
	// pattern as ANTHROPIC_BASE_URL/litellm). Forwarding the pod path's
	// cluster-DNS value would clobber that with a hostname the VM cannot
	// resolve — gb slack would fail with a DNS timeout on every image.
	// Stripping it yields the working local proxy on images with the listener
	// and a legible "SLACK_BRIDGE_URL is not set" on images without it. The
	// bridge auth token is re-delivered by applyMicrovmSlackBridge (on the pod
	// path it is a Secret env-ref that never rides spec.Env).
	"SLACK_BRIDGE_URL": {},
	// Same contract as SLACK_BRIDGE_URL: the pod-path value is cluster-internal
	// DNS (http://<release>-jira-bridge:8091) a MicroVM cannot resolve.
	// Stripping yields a legible "JIRA_BRIDGE_URL is not set" from gb jira
	// reply until a Ziti listener for the jira-bridge exists in the VM image.
	"JIRA_BRIDGE_URL": {},
	// The plan-gardener role injects this in-cluster viewer Service base for
	// gb plan add-step (role bead ka-0000003712 env). A MicroVM cannot resolve
	// it; forwarding it would turn every add-step into a DNS timeout. Stripped,
	// gb plan falls back to the public BEADS_VIEWER_URL and surfaces its
	// explicit "this is the AVA/OIDC edge" error until a beads-viewer Ziti
	// listener exists in the VM image (ka-0000003026 follow-up).
	"BEADS_VIEWER_INTERNAL_URL": {},
}

// coopmuxNatsRelayPrefix is the NATS subject prefix coopmux subscribes on for
// out-of-band session discovery (`{prefix}.session.>`). It MUST match the
// coopmux deployment's --nats-relay-prefix (helm values coopmux.natsRelay.prefix,
// default "coop.mux") or the MicroVM's announce/status/state publishes land on a
// subject nobody is listening to. The K8s pod path never sets these prefixes
// because in-cluster agents register over HTTP (POD_IP callback); the MicroVM
// path is outbound-only and relies on this relay, so the controller sets it
// explicitly (mirrors the value gb agent-start's --local path uses).
//
// This is threaded through COOP_NATS_RELAY_PREFIX — the coopmux discovery relay
// only (`{relay_prefix}.session.*`). It is DELIBERATELY separate from
// COOP_NATS_PREFIX (the general event / message-forward space, "coop.events.>"):
// forcing both onto "coop.mux" is what caused the prefix collision fixed in
// ka-0000005381 — coop's event publisher would emit coop.mux.state /
// coop.mux.account.exhausted, subjects nobody subscribes to and that the NATS
// agent grant (publish scoped to coop.events.> + coop.mux.session.>) rejects
// with an authorization violation.
const coopmuxNatsRelayPrefix = "coop.mux"

// coopEventsPrefix is the general NATS event / message-forward subject prefix
// (coop's event publisher N1 + forwarder N3). It matches coop's own default and
// the NATS agent grant (`coop.events.>`). Set explicitly on the MicroVM path so
// a stray forwarded COOP_NATS_PREFIX can never drag general events onto the
// relay's coop.mux space (ka-0000005381).
const coopEventsPrefix = "coop.events"

// microvmOutboundOnlyEnv are the registration-mode env vars the controller
// stamps onto every MicroVM agent so coop registers OUTBOUND only. A MicroVM is
// reached solely over a Ziti egress overlay: the control plane (coopmux running
// in EKS) has no address it can dial back into, so the K8s HTTP self-
// registration path — which advertises a POD_IP/127.0.0.1 callback coopmux then
// health-polls, screen-polls, and pushes credentials to — cannot work and would
// only get the session health-check-evicted. Instead:
//   - COOP_OUTBOUND_ONLY suppresses coop's inbound HTTP self-registration and
//     drops the callback URL from its NATS announce (coop/mux_client.rs);
//   - COOP_NATS_RELAY makes coop publish announce/heartbeat/status/state OUTBOUND
//     over the relay, which coopmux treats as a first-class transport it never
//     dials inbound (coop/mux nats_sub.rs, upstream/health.rs);
//   - COOP_NATS_RELAY_PREFIX aligns the relay's session subjects with coopmux's
//     subscription (`coop.mux.session.>`), WITHOUT dragging coop's general event
//     publisher onto that space;
//   - COOP_NATS_PREFIX pins the general event / message-forward space to
//     "coop.events" (coop's own default) so state / usage / account.exhausted /
//     messages.* stay on the subjects the beads-viewer subscribes to and the
//     NATS agent grant allows. Pinning both explicitly is the ka-0000005381
//     fix: the previous single-prefix wiring set COOP_NATS_PREFIX=coop.mux,
//     which pushed the event publisher's state/account.exhausted onto
//     coop.mux.* — subjects nobody subscribes to and that the agent grant
//     (publish scoped to coop.events.> + coop.mux.session.>) rejects.
//
// The image owns COOP_NATS_URL (its local Ziti proxy listener) and the inline
// NATS token, so the relay has a transport to ride. Nothing here requires a pod
// IP, projected SA token, Secret/PVC mount, downward-API metadata, or pod
// deletion (nfd!15 "Gardenio-owned changes #6"). Forwarded env from the pod spec
// does NOT override these — they are applied last.
var microvmOutboundOnlyEnv = map[string]string{
	"COOP_OUTBOUND_ONLY":     "1",
	"COOP_NATS_RELAY":        "1",
	"COOP_NATS_RELAY_PREFIX": coopmuxNatsRelayPrefix,
	"COOP_NATS_PREFIX":       coopEventsPrefix,
}

// microvmAgentSpecFromPodSpec translates the assembled (runtime-neutral inputs
// of the) Kubernetes AgentPodSpec into the microvm.AgentSpec the MicroVM backend
// consumes. It reuses the SAME project/role/task/env selection the pod path
// already computed (nfd!15 "Gardenio-owned changes #2" — one runtime-neutral
// spec, no hard-coded project/secret), but (1) strips the cluster-internal
// service endpoints the MicroVM image supplies itself via its local Ziti proxy
// listeners (microvmClusterInternalEnvKeys) and (2) stamps the outbound-only
// registration mode (microvmOutboundOnlyEnv) so coop never advertises a
// K8s-only inbound callback (C6, ka-0000005563). Account/model selection,
// tokens, Git config, and correlation IDs are forwarded as-is.
func microvmAgentSpecFromPodSpec(agentBeadID string, spec podmanager.AgentPodSpec) microvm.AgentSpec {
	// agentBeadID is the authoritative bead ID the caller resolved (it may fall
	// back to a label-constructed ID when event.BeadID was empty on older
	// daemons). Pin it onto the spec so IdentityEnvVars stamps the correct
	// BOAT_AGENT_BEAD_ID / KD_AGENT_ID regardless of what spec.BeadID carried.
	spec.BeadID = agentBeadID
	env := make(map[string]string, len(spec.Env)+len(microvmOutboundOnlyEnv))
	for k, v := range spec.Env {
		if _, skip := microvmClusterInternalEnvKeys[k]; skip {
			continue
		}
		env[k] = v
	}
	// Agent identity / correlation / task assignment / attribution — the SAME
	// runtime-neutral set the Kubernetes pod path emits via
	// podmanager.buildEnvVars, sourced from the shared podmanager.IdentityEnvVars
	// builder so the two backends can never drift (ka-0000000401). Without these
	// the in-VM `gb init` has no BOAT_AGENT_BEAD_ID: it boots but cannot identify
	// its own bead, set agent_state, or hydrate/claim BOAT_TASK_ID — the agent is
	// invisible to the control plane and never claims work. The K8s-only env
	// (HOME, XDG_STATE_HOME PVC path, POD_IP, sidecar/service DNS, secret mounts)
	// is intentionally NOT here — the MicroVM image owns its own HOME/state and
	// reaches services over its local Ziti proxy listeners. Applied after the
	// forwarded spec.Env so these authoritative identity values always win.
	maps.Copy(env, podmanager.IdentityEnvVars(spec))
	// Applied last so the outbound-only registration mode is authoritative — a
	// stray COOP_NATS_RELAY/COOP_OUTBOUND_ONLY on the pod spec can never flip a
	// MicroVM back onto the inbound-callback path.
	maps.Copy(env, microvmOutboundOnlyEnv)
	return microvm.AgentSpec{
		BeadID:      agentBeadID,
		Project:     spec.Project,
		Role:        spec.Role,
		AgentName:   spec.AgentName,
		TaskID:      spec.TaskID,
		Environment: env,
	}
}
