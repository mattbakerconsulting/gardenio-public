package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/statusreporter"
	"gardenio/controller/internal/subscriber"
)

// recordFreezePayload fans a workspace payload-completeness verdict (ka-0000006933
// finding #4) out to the two gardenio_freeze_payload_* counters. Called ONLY from
// the freeze-success branch (a successfully-persisted, non-discarded snapshot) so
// a snapshot the abort-discard defer later discards is never counted (M3). Each
// genuine-incomplete reason increments payload_incomplete once; each benign
// dangling-symlink-only leg increments payload_symlink_skip once. A clean freeze
// increments neither. runsc-only (the freeze path gates on RuntimeClassRunsc, and
// the runtime label is hardcoded to match the sibling RecordFreezeLatency calls
// above — if a runc checkpoint path is ever added, update both recorders together).
func recordFreezePayload(freezeMetrics *gardeniometrics.FreezeMetrics, v podmanager.PayloadVerdict, role string) {
	rt := gardeniometrics.FreezeRuntimeRunsc
	if v.WorkCopyFailed {
		freezeMetrics.RecordFreezePayloadIncomplete(gardeniometrics.FreezePayloadReasonWorkCopy, role, rt)
	}
	if v.TmpCopyFailed {
		freezeMetrics.RecordFreezePayloadIncomplete(gardeniometrics.FreezePayloadReasonTmpCopy, role, rt)
	}
	if v.GoferFailed {
		freezeMetrics.RecordFreezePayloadIncomplete(gardeniometrics.FreezePayloadReasonGofer, role, rt)
	}
	if v.GoferMalformed {
		freezeMetrics.RecordFreezePayloadIncomplete(gardeniometrics.FreezePayloadReasonGoferMalformed, role, rt)
	}
	if v.WorkSymlinkSkip {
		freezeMetrics.RecordFreezePayloadSymlinkSkip(gardeniometrics.FreezePayloadLegWork, role, rt)
	}
	if v.TmpSymlinkSkip {
		freezeMetrics.RecordFreezePayloadSymlinkSkip(gardeniometrics.FreezePayloadLegTmp, role, rt)
	}
}

// terminalReportPhase returns the synthetic pod phase to report on a session
// close (AgentDone/AgentKill/AgentStop), or "" to report nothing.
//
// The one non-obvious case is a SUCCESSFULLY-frozen graceful stop (frozen=true,
// only ever set on AgentStop). A freeze leaves the agent bead deliberately OPEN
// at agent_state=stopping — the name-keyed frozen-awaiting-thaw signal
// suspendAgent (cmd/gb/stop.go) set and every wake path (bridge
// respawnThreadAgent, wakePlanAssistant, the reconciler thaw exemptions) keys
// on. The "Stopped" phase maps to agent_state=done (statusreporter
// PhaseToAgentState), so reporting it would OVERWRITE that signal and defeat the
// freeze: the wake path would then see a plain done bead, cold-start a
// differently-named pod instead of minting the same-name resume bead, and the
// name-keyed thaw (the sole RecordResumeLatency call site) would never fire —
// resume-latency emits zero samples (bug ka-0000007439). So a frozen stop
// reports NOTHING here; the bead is closed later on a successful thaw or by the
// reconciler once its snapshot is gone. Every other close — a non-frozen stop
// (freeze failed / not runsc), AgentDone, AgentKill — reports its phase exactly
// as before.
func terminalReportPhase(eventType subscriber.EventType, frozen bool) string {
	switch eventType {
	case subscriber.AgentKill:
		return "Failed"
	case subscriber.AgentStop:
		if frozen {
			return "" // keep the bead open at agent_state=stopping (thaw target)
		}
		return "Stopped"
	default: // AgentDone
		return "Succeeded"
	}
}

// countActiveAgentPods counts the live agent pods the event door's caps see:
// those in the release namespace and in every tenant land namespace, so a
// land's pods count toward the fleet-wide CoopMaxPods (gd-NDJIOutLsv).
// projectCount is the share of them in project. A land namespace that is
// gone, or not bound to the controller, has no pods to count.
func countActiveAgentPods(ctx context.Context, cfg *config.Config, pods podmanager.Manager, project string) (activeCount, projectCount int, err error) {
	for i, ns := range append([]string{cfg.Namespace}, tenantLandNamespaces(cfg)...) {
		existing, err := pods.ListAgentPods(ctx, ns, map[string]string{
			podmanager.LabelApp: podmanager.LabelAppValue,
		})
		if i > 0 && (apierrors.IsNotFound(err) || apierrors.IsForbidden(err)) {
			continue
		}
		if err != nil {
			return 0, 0, fmt.Errorf("listing pods in %s for max check: %w", ns, err)
		}
		for _, p := range existing {
			if _, ok := p.Labels[podmanager.LabelAgent]; ok &&
				p.Status.Phase != corev1.PodFailed &&
				p.Status.Phase != corev1.PodSucceeded {
				activeCount++
				if p.Labels[podmanager.LabelProject] == project {
					projectCount++
				}
			}
		}
	}
	return activeCount, projectCount, nil
}

// handleEvent translates a beads lifecycle event into K8s pod operations.
func handleEvent(ctx context.Context, logger *slog.Logger, cfg *config.Config, event subscriber.Event, pods podmanager.Manager, status statusreporter.Reporter, k8sClient kubernetes.Interface, daemon, machineDaemon *beadsapi.Client, ft *freezeThawLifecycle, microvmLifecycle *microvm.Lifecycle, microvmSessionVendor *microvm.SessionVendor, eventMetrics *gardeniometrics.EventMetrics, resumeMetrics *gardeniometrics.ResumeMetrics, freezeMetrics *gardeniometrics.FreezeMetrics, spendReconciler *sessionSpendReconciler, microvmLaunchMetrics *gardeniometrics.MicrovmLaunchMetrics, tokMetrics *tokenMetrics, ghApp *githubAppIssuer) error {
	// eventReceivedAt anchors the revocation-latency histogram: for the
	// synchronous despawn / stuck-restart revoke paths, latency is measured
	// from the moment the controller observed the despawn event to revocation
	// completion (telemetry-plan §1.3 "from agent-done to revocation completion").
	eventReceivedAt := time.Now()
	logger.Info("handling beads event",
		"type", event.Type, "project", event.Project, "role", event.Role,
		"agent", event.AgentName, "bead", event.BeadID)

	// Use the canonical bead ID from the event. Fall back to constructing
	// from labels for backwards compatibility with older daemon versions.
	agentBeadID := event.BeadID
	if agentBeadID == "" {
		agentBeadID = fmt.Sprintf("%s-%s-%s-%s", event.Mode, event.Project, event.Role, event.AgentName)
	}

	switch event.Type {
	case subscriber.AgentSpawn:
		// Enforce max pods limit on event-driven spawns (same guard as reconciler).
		if cfg.CoopMaxPods > 0 || cfg.CoopMaxPodsPerProject > 0 {
			activeCount, projectCount, err := countActiveAgentPods(ctx, cfg, pods, event.Project)
			if err != nil {
				return err
			}
			if cfg.CoopMaxPods > 0 && activeCount >= cfg.CoopMaxPods {
				logger.Warn("max concurrent pods reached, rejecting spawn event",
					"limit", cfg.CoopMaxPods, "active", activeCount, "agent", event.AgentName)
				return fmt.Errorf("max concurrent pods (%d) reached, cannot spawn %s", cfg.CoopMaxPods, event.AgentName)
			}
			if cfg.CoopMaxPodsPerProject > 0 && projectCount >= cfg.CoopMaxPodsPerProject {
				logger.Warn("max concurrent pods per project reached, rejecting spawn event",
					"limit", cfg.CoopMaxPodsPerProject, "project", event.Project,
					"active", projectCount, "agent", event.AgentName)
				return fmt.Errorf("max pods per project (%d) reached for %s, cannot spawn %s",
					cfg.CoopMaxPodsPerProject, event.Project, event.AgentName)
			}
		}

		// Lands spawn wall (Track B §3.5, step 8.3): a tenant land that is
		// frozen, has tenant spawns off, or whose purse is at the wall or
		// unknown starts nothing, a resume (thaw) included. The bead is left
		// open at agent_state=budget_exceeded with the reason; the reconciler
		// re-decides it each pass. Operator lands never reach a daemon read.
		// A tenant land past its spawns per hour (step 10.4) is refused the
		// same way with spawn_rate.
		if r := spawnAdmission(ctx, cfg, asSpawnWallDaemon(daemon), landSpawnRate, event.Project, event.Role, agentBeadID, time.Now()); r.Refused() {
			recordSpawnWallRefusal(ctx, logger, asSpawnWallDaemon(daemon), event.Project, event.AgentName, agentBeadID, event.Metadata, r)
			return nil
		}

		spec := buildAgentPodSpec(cfg, event)

		// Execution-backend routing (epic ka-0000005010): an agent resolved to a
		// non-Kubernetes backend must never reach the pod path — the handoff
		// contract requires one spawn event to create exactly one runtime. Route
		// lambda_microvm to the MicroVM backend (C4); any other non-Kubernetes
		// target, or a nil/unavailable MicroVM backend, fails closed (marks the
		// agent failed) rather than silently spawning a pod against the wrong
		// backend.
		if !podmanager.IsKubernetesTarget(spec.ExecutionTarget) {
			return handleNonK8sSpawn(ctx, logger, cfg, daemon, machineDaemon, k8sClient, microvmLifecycle, microvmSessionVendor, agentBeadID, spec, microvmLaunchMetrics)
		}

		// gVisor wake path (epic ka-0000007417, task ka-0000003688): if this
		// agent is pinned to the runsc RuntimeClass and a snapshot from a prior
		// freeze exists, restore it through the placeholder-Pod-B lifecycle
		// instead of cold-starting. A thaw failure falls through to the normal
		// CreateAgentPod below — a cold start is always safe (rollback path).
		if ft.shouldThaw(ctx, spec) {
			// Resume-latency stopwatch (task ka-0000003577): t0 is the moment the
			// wake path accepts this spawn event; we observe the elapsed seconds
			// the moment the agent is responsive (thaw returns having gated on the
			// C3 rehydration acks — gb re-credentialed + coop reconnected NATS,
			// the first proof-of-life). Only a successful thaw observes; a thaw
			// failure cold-starts and its latency is the harness's R0/R1 domain,
			// not double-counted here.
			thawStart := time.Now()
			res, resumePath, err := ft.thaw(ctx, spec, agentBeadID)
			switch {
			case err != nil && res != nil:
				// Partial success (ka-0000003688 heads-up): the restore landed a
				// live Pod B but rehydration fail-closed (a required gb/coop ack
				// never arrived within the deadline — design C5). We must NOT
				// fall straight through to a cold CreateAgentPod: Pod B is a
				// real, running pod and would be orphaned alongside the
				// cold-started pod. Tear down Pod B first, then cold-start — a
				// cold start is always safe and reachable because the snapshot is
				// left intact.
				logger.Error("thaw restored but rehydration failed; tearing down Pod B before cold start",
					"agent", event.AgentName, "bead", agentBeadID,
					"podB", res.PlaceholderPod, "error", err)
				// Detach this teardown from the inbound (leader-election) ctx: a
				// controller leader-change / pod roll is precisely what cancels ctx
				// mid-thaw, and a cancelled DeleteAgentPod no-ops, leaking the live
				// Pod B's on-node runsc containers (bug ka-0000004088 problem 1). Pod
				// B is a committed, running pod — its teardown must complete even as
				// the process is shutting down, so it runs on a short detached ctx
				// bounded only by its own timeout. Mirrors the manager's detached
				// post-commit critical section (ka-0000001355, harden-thaw).
				tdCtx, tdCancel := context.WithTimeout(context.WithoutCancel(ctx), thawTeardownTimeout)
				if derr := pods.DeleteAgentPod(tdCtx, res.PlaceholderPod, res.Snapshot.Namespace); derr != nil {
					logger.Error("failed to delete un-rehydrated Pod B; cold start may orphan it",
						"agent", event.AgentName, "podB", res.PlaceholderPod, "error", derr)
				}
				tdCancel()
				// fall through to cold start below.
			case err != nil:
				logger.Error("thaw failed, falling back to cold start",
					"agent", event.AgentName, "bead", agentBeadID, "error", err)
				// fall through to cold start below.
			default:
				resumeSecs := time.Since(thawStart).Seconds()
				// thaw-warm grafts onto a pre-Ready placeholder (R4, the <1s
				// target); thaw-fresh creates Pod B first (R3). Map the path the
				// thaw actually took to its regime and observe the same sample the
				// bench harness logs per-run. runsc-only — the thaw path never
				// serves a runc agent (shouldThaw gates on RuntimeClassRunsc).
				regime := gardeniometrics.ResumeRegimeR3
				if resumePath == gardeniometrics.ResumePathThawWarm {
					regime = gardeniometrics.ResumeRegimeR4
				}
				resumeMetrics.RecordResumeLatency(resumeSecs, regime, event.Role,
					gardeniometrics.ResumeRuntimeRunsc, resumePath)
				logger.Info("agent thawed from snapshot",
					"agent", event.AgentName, "bead", agentBeadID,
					"podB", res.PlaceholderPod, "restoredPID", res.RestoredPID,
					"rehydrated", res.Rehydrated, "imageReaped", res.ImageReaped,
					"resumePath", resumePath, "regime", regime,
					"resumeLatencyMs", resumeSecs*1000)
				ft.reportThawed(ctx, status, agentBeadID, res)
				return nil
			}
		}

		// Fail-closed credential guard (ka-0000007608, guard #2): refuse to spawn
		// a crew/gardener pod that would have no Claude OAuth credential. An empty
		// project (today) silently early-returns applyAccountConfig, injecting no
		// CLAUDE_CODE_OAUTH_TOKEN, and verifySecretsExist then has nothing to
		// check — so a loginless pod spawns. Catch that here, and the parallel
		// case of a referenced gardenio-claude-oauth-<user> secret that is absent,
		// before CreateAgentPod. Direct-provider-auth projects (no OAuth
		// SecretEnv) are allowed unchanged. Runs after the thaw block: a thawed
		// agent is a re-spawn of a pod that already proved its credentials.
		if r := evaluateSpawnCredentials(ctx, k8sClient, logger, spec); r.refuse {
			recordSpawnRefusal(ctx, logger, daemon, spec, agentBeadID, r)
			return nil
		}

		// Pool-exhaustion escape hatch (ka-0000002560): applyAccountConfig set
		// SpawnBlocked because EVERY account in this project's pool is
		// simultaneously capacity-exhausted, so there is no healthy key to bake.
		// Declining here (rather than spawning onto a known-dead account) means
		// the reconciler re-attempts on the next sync cycle once a rate-limit
		// window resets. Transient: the bead is left spawnable and no escalation
		// is filed — the pool recovers on its own. Mark the bead queued so its
		// state reflects the wait instead of silently looking idle.
		if spec.SpawnBlocked {
			logger.Warn("declining spawn — project account pool fully exhausted (reconciler will re-attempt)",
				"agent", event.AgentName, "project", event.Project, "role", event.Role,
				"bead", agentBeadID, "reason", spec.SpawnBlockReason)
			if agentBeadID != "" {
				if err := daemon.UpdateAgentState(ctx, agentBeadID, "queued"); err != nil {
					logger.Warn("failed to mark pool-exhausted agent as queued",
						"bead", agentBeadID, "error", err)
				}
			}
			return nil
		}

		// Per-agent tokens: generate a project-scoped token for this agent.
		tokenMinted := false
		// Fail closed (P7 Task 1): falling back to the shared token would hand
		// this agent the primary credential the flag exists to keep from it.
		if cfg.PerAgentTokens && agentBeadID != "" {
			secretName, minted, err := provisionAgentToken(ctx, logger, machineDaemon, k8sClient, agentBeadID, event.AgentName, event.Project, spec.Namespace, tokMetrics)
			if err != nil {
				logger.Error("refusing spawn: per-agent token provisioning failed",
					"agent", event.AgentName, "bead", agentBeadID, "error", err)
				return fmt.Errorf("per-agent token provisioning failed for %s: %w", event.AgentName, err)
			}
			spec.DaemonTokenSecret = secretName
			tokenMinted = minted
		}

		// GitHub App per-agent installation token (fail closed): a GitHub-hosted
		// project that cannot get its own token is refused, never spawned with the
		// shared git credential. The reconciler retries on its next sync.
		if refuse, reason := ghApp.applyToSpec(ctx, &spec, agentBeadID); refuse {
			releaseMintedAgentToken(ctx, logger, daemon, k8sClient, agentBeadID, spec.Namespace, tokMetrics, tokenMinted)
			recordGithubAppRefusal(ctx, logger, daemon, spec, agentBeadID, reason)
			return nil
		}

		stampResolvedHarness(ctx, logger, daemon, agentBeadID, spec)
		claimSpawnTask(ctx, logger, machineDaemon, agentBeadID, spec)
		if err := pods.CreateAgentPod(ctx, spec); err != nil {
			ghApp.revokeIfCreated(ctx, agentBeadID, spec.Namespace, revocationTypeDespawn)
			releaseMintedAgentToken(ctx, logger, daemon, k8sClient, agentBeadID, spec.Namespace, tokMetrics, tokenMinted)
			return err
		}
		// Optimistically bump the per-account live-pod count so back-to-back
		// spawns within this sync window keep spreading (ka-0000004861).
		cfg.BumpAccountLiveCount(spec.ResolvedAccount)
		// E2.4: a pod was created for this agent — count the spawn. Prewarmed
		// pool pods are still spawns (they consume a pod slot); the lifecycle
		// counter tracks pod creation, not assignment. session_id is empty for
		// non-OOM events to keep cardinality bounded.
		eventMetrics.RecordLifecycleEvent(gardeniometrics.LifecycleSpawned, event.Project, event.Role, "")
		// Phase 0 (ka-0000002142): count which applyAccountConfig layer produced
		// this pod's account, now that we have actually created it. spec.AccountLayer
		// is the winning-layer enum stamped during buildAgentPodSpec.
		eventMetrics.RecordAccountLayer(spec.AccountLayer, event.Project, event.Role)
		// ka-0000001543: snapshot the agent's cumulative LiteLLM spend now so the
		// session-close hook can compute the session-cost delta. Stashed on the
		// agent bead so it survives a controller restart between spawn and close.
		// Best-effort and non-blocking — never fails a spawn.
		if spendReconciler != nil {
			spendReconciler.captureBaseline(ctx, agentBeadID, event.AgentName, event.Project, event.Role)
		}
		// Stamp pod_name onto the agent bead immediately at create time so the
		// Slack "agent" coopmux link is valid on the very first card render,
		// instead of waiting up to ~60s for the next SyncAll-with-IP tick
		// (ka-0000000553). coop derives the coopmux session id from the pod
		// hostname (= K8s pod name) and the metadata.pod_name note takes
		// priority, so the deterministic spec.PodName() — which is exactly the
		// name CreateAgentPod just used (manager.go:1076) — is the correct,
		// already-known value here. The notes write is a full replace, and
		// SyncAll rewrites the same set (backend + pod_name + pod_namespace)
		// once the pod has an IP, so this early write is safe.
		// The thaw/warm-claim paths (whose real pod name differs from the
		// canonical formula) return earlier and never reach this block.
		_ = status.ReportBackendMetadata(ctx, agentBeadID, statusreporter.BackendMetadata{
			PodName:   spec.PodName(),
			Namespace: spec.Namespace,
		})
		// Report spawning status to beads. For prewarmed pods we pass spec.Prewarmed
		// so ReportPodStatus suppresses the spawning write — otherwise the bead's
		// agent_state flips prewarmed→spawning, the in-pod standbyWait reads that
		// as "assignment received" and exits standby, Claude starts, and the bead
		// lands at "working" instead of staying "prewarmed" (ka-0000005019).
		_ = status.ReportPodStatus(ctx, agentBeadID, statusreporter.PodStatus{
			PodName:   spec.PodName(),
			Namespace: spec.Namespace,
			Phase:     string("Pending"),
			Ready:     false,
		}, spec.Prewarmed)
		return nil

	case subscriber.AgentDone, subscriber.AgentKill, subscriber.AgentStop:
		// lambda_microvm teardown (C5, ka-0000001837): a MicroVM agent has no pod
		// to freeze/delete, so microvmTeardown owns the suspend-vs-terminate
		// decision (see its doc). When it applies we emit the shared session-close
		// metrics and return here — the K8s pod-teardown machinery below (freeze,
		// DeleteAgentPod, token/Ziti revoke) is all pod-specific and skipped. The
		// bead itself is closed by gb stop / the reconciler on the terminate path;
		// on the suspend path the bead is deliberately LEFT OPEN as the resume
		// target (no close here).
		if outcome := microvmTeardown(ctx, logger, cfg, event, agentBeadID, microvmLifecycle, resumeMetrics); outcome != microvmTeardownNotApplicable {
			emitSessionCloseMetrics(ctx, logger, daemon, eventMetrics, event, agentBeadID, spendReconciler)
			return nil
		}
		agentName := strings.TrimPrefix(event.AgentName, event.Project+"-")
		podName := fmt.Sprintf("%s-%s-%s-%s", event.Mode, event.Project, event.Role, agentName)
		// Resolve the teardown namespace the same way spawn did, so a
		// runsc-pinned agent (whose pod lives in the privileged gVisor
		// namespace, bug ka-0000004301) is frozen and deleted in the right
		// namespace. Without this, shouldFreeze Gets the pod in cfg.Namespace,
		// finds nothing, and silently skips the freeze — the agent would never
		// dehydrate on graceful stop. buildAgentPodSpec is a pure constructor
		// (cache reads, no I/O); for every non-runsc agent it resolves to
		// cfg.Namespace exactly as namespaceFromEvent did before.
		ns := buildAgentPodSpec(cfg, event).Namespace

		// frozen records whether this graceful stop SUCCESSFULLY checkpointed the
		// agent (freeze() persisted a snapshot). It gates the terminal status write
		// below: a frozen agent's bead must stay OPEN at agent_state=stopping — the
		// name-keyed frozen-awaiting-thaw signal suspendAgent (cmd/gb/stop.go) set
		// and every wake path (bridge respawnThreadAgent, wakePlanAssistant, the
		// reconciler thaw exemptions) keys on — NOT be overwritten to "done". The
		// pre-fix ReportPodStatus(phase="Stopped") did exactly that overwrite, which
		// suspendAgent explicitly warned "would defeat the freeze": the wake path
		// then saw a plain done bead, grafted a differently-named pool pod instead of
		// minting the same-name resume bead, and the name-keyed thaw (the sole
		// RecordResumeLatency call site) never fired — bug ka-0000007439.
		frozen := false

		// gVisor dehydrate path (epic ka-0000007417, task ka-0000003688): on a
		// graceful stop of a runsc-pinned agent, checkpoint it before teardown
		// so a later spawn can thaw it. Only AgentStop (intentional, graceful
		// despawn) freezes — AgentDone (work complete) and AgentKill (forced
		// failure) tear down normally; a snapshot of a finished/failed agent has
		// no wake to serve. `runsc checkpoint --leave-running` keeps Pod A alive,
		// so the DeleteAgentPod below still runs to free the slot; the persisted
		// snapshot is what the wake path restores. A freeze failure is logged and
		// falls through to the normal delete (cold start on next spawn).
		//
		// TEARDOWN-SAFETY ORDERING (ka-0000003702, fix C): freeze() is synchronous
		// and now returns success ONLY after BOTH the OCI bundle AND the 3.0G
		// checkpoint image are written + host-ns re-verified (FT_BUNDLE_OK +
		// FT_IMAGE_OK) at the CONTROLLER-OWNED safe paths under /var/lib/gardenio
		// (outside the containerd / pod-lifecycle GC zone). Because the
		// DeleteAgentPod below only runs after freeze() returns, Pod A teardown is
		// effectively DEFERRED until the image+bundle are safely relocated — so the
		// teardown can no longer reap them out from under a not-yet-thawed
		// snapshot. If either verify fails, freeze() returns an error, NO snapshot
		// is persisted, and the next spawn cold-starts cleanly (never thaws behind
		// a reaped image).
		if event.Type == subscriber.AgentStop && ft.shouldFreeze(ctx, k8sClient, podName, ns) {
			// Freeze-latency stopwatch (task ka-0000005147): t0 is the moment the
			// freeze is requested; the TOTAL is the elapsed wall-clock the moment
			// freeze() returns having persisted the Snapshot — that persist is the
			// first point a later spawn can thaw, so it IS the freeze's
			// proof-of-durability (fast-interruptible-freeze.md §C1). Only a
			// successful freeze observes; a freeze failure falls through to normal
			// teardown and is not bucketed (its latency is a discarded partial).
			freezeStart := time.Now()
			_, timing, ferr := ft.freeze(ctx, event.AgentName, agentBeadID, podName, ns)
			switch {
			case errors.Is(ferr, podmanager.ErrFreezeAborted):
				// C2 interruptible freeze (ka-0000005834): an inbound user message
				// arrived mid-checkpoint and aborted the freeze. Because the
				// checkpoint is `--leave-running`, Pod A is still alive and Ready —
				// it must STAY up to serve that message (the <10s safety net). So we
				// ABANDON this graceful stop entirely: NO DeleteAgentPod (the whole
				// point — keep the warm agent), NO token revoke, NO "Stopped" status
				// write, NO session-close metrics. The snapshot, if one raced to
				// persistence, was already discarded inside Freeze (discardAbortedFreeze).
				// The agent simply keeps running as if the stop never happened; its
				// next genuine idle window will re-attempt the freeze.
				logger.Info("freeze aborted by inbound-message interrupt; leaving agent live, abandoning graceful stop",
					"agent", event.AgentName, "bead", agentBeadID, "pod", podName)
				freezeMetrics.RecordFreezeInterrupt(event.Role, gardeniometrics.FreezeRuntimeRunsc)
				return nil
			case errors.Is(ferr, podmanager.ErrFreezeWatchersNotArmed):
				// Pre-freeze guard (ka-0000006667): the pod's in-pod rehydration
				// watchers never armed, so a checkpoint would be un-thawable. This is
				// NOT a failure — it is a deliberate skip. Fall through to normal
				// teardown; the next spawn cold-starts cleanly (the same outcome a
				// doomed thaw would have reached, minus the wasted checkpoint).
				logger.Info("freeze skipped on graceful stop: in-pod rehydration watchers not armed, proceeding with normal teardown (cold start next spawn)",
					"agent", event.AgentName, "bead", agentBeadID, "pod", podName)
			case errors.Is(ferr, podmanager.ErrFreezeAlreadySnapshotted):
				// Duplicate-Freeze idempotency guard (ka-0000000194): a snapshot
				// already exists and Pod A is gone/Terminating, so this second
				// checkpoint was refused to protect the good snapshot from being
				// overwritten by a degenerate one. NOT a failure — the existing good
				// snapshot stands; fall through to the normal teardown. This is the
				// defense-in-depth backstop to the watcher-side AgentStop dedup.
				logger.Info("freeze skipped on graceful stop: snapshot already exists and source pod is gone/Terminating, keeping good snapshot (duplicate-checkpoint guard)",
					"agent", event.AgentName, "bead", agentBeadID, "pod", podName)
			case errors.Is(ferr, podmanager.ErrFreezeAlreadyThawed):
				// Re-freeze guard (ka-0000001878): the agent's snapshot was already
				// consumed by a prior freeze→thaw cycle (image reaped, bundle backing
				// the live thawed Pod B). A path-b checkpoint is node-local and
				// single-shot, so re-freezing would clobber the live bundle; Freeze
				// refused and DISCARDED the stale record. NOT a failure — fall through
				// to the normal teardown so this stop degrades to a clean cold start
				// on the next spawn. This is the explicit re-freeze guard; in practice
				// it is rarely reached because the thawed pod is named
				// "<canonical>-thaw-b" and shouldFreeze's canonical-name Get usually
				// misses it first — but the guard makes the safety asserted rather than
				// incidental, covering any seam that resolves the real Pod B name.
				logger.Info("freeze skipped on graceful stop: snapshot already consumed by a prior freeze→thaw cycle, discarded stale record, cold start next spawn (re-freeze guard)",
					"agent", event.AgentName, "bead", agentBeadID, "pod", podName)
			case ferr != nil:
				logger.Error("freeze failed on graceful stop, proceeding with normal teardown",
					"agent", event.AgentName, "bead", agentBeadID, "pod", podName, "error", ferr)
			default:
				// The freeze persisted a snapshot: the bead stays a live thaw target.
				// Suppress the "Stopped"→agent_state=done write below so the
				// frozen-awaiting-thaw signal (agent_state=stopping) survives for the
				// wake path (bug ka-0000007439).
				frozen = true
				freezeSecs := time.Since(freezeStart).Seconds()
				// runsc-only — shouldFreeze gates on RuntimeClassRunsc. Record the
				// headline TOTAL plus the per-step attribution (runsc-checkpoint vs.
				// Step-3 relocation copy vs. persist) the node + Go sub-timers
				// captured, so the dashboard learns WHERE the freeze seconds go
				// before C3 optimizes. A zero sub-timer (sentinel parse miss) still
				// records a 0s sample rather than being dropped — the bucketed count
				// stays consistent with the total's count.
				freezeMetrics.RecordFreezeLatency(freezeSecs,
					gardeniometrics.FreezeStepTotal, event.Role, gardeniometrics.FreezeRuntimeRunsc)
				freezeMetrics.RecordFreezeLatency(timing.Checkpoint.Seconds(),
					gardeniometrics.FreezeStepCheckpoint, event.Role, gardeniometrics.FreezeRuntimeRunsc)
				freezeMetrics.RecordFreezeLatency(timing.Relocation.Seconds(),
					gardeniometrics.FreezeStepRelocation, event.Role, gardeniometrics.FreezeRuntimeRunsc)
				freezeMetrics.RecordFreezeLatency(timing.Persist.Seconds(),
					gardeniometrics.FreezeStepPersist, event.Role, gardeniometrics.FreezeRuntimeRunsc)
				// Workspace payload-completeness counters (ka-0000006933 finding #4).
				// Recorded HERE, in the freeze-success branch — NOT inside Freeze() — so
				// a snapshot the abort-discard defer later discards (an inbound interrupt
				// racing completion) is never counted (ferr!=nil takes a different case
				// above). timing.Payload carries the verdict the Go side already
				// classified; this fans it out to the two counters: payload_incomplete
				// (the genuine-failure ALERT metric) + payload_symlink_skip (the
				// quantified benign dangling-symlink noise the alert deliberately excludes).
				recordFreezePayload(freezeMetrics, timing.Payload, event.Role)
				logger.Info("agent frozen on graceful stop",
					"agent", event.AgentName, "bead", agentBeadID, "pod", podName,
					"freezeLatencyMs", freezeSecs*1000,
					"checkpointMs", timing.Checkpoint.Milliseconds(),
					"relocationMs", timing.Relocation.Milliseconds(),
					"persistMs", timing.Persist.Milliseconds())
			}
		}

		err := pods.DeleteAgentPod(ctx, podName, ns)
		// Per-agent tokens: revoke fleet token and delete K8s Secret.
		if cfg.PerAgentTokens && agentBeadID != "" {
			revokeAgentToken(ctx, logger, daemon, k8sClient, agentBeadID, ns, tokMetrics, revocationTypeDespawn, eventReceivedAt)
		}
		ghApp.revoke(ctx, agentBeadID, ns, revocationTypeDespawn, eventReceivedAt)
		// OpenZiti per-pod access (epic ka-0000000990): release the agent's
		// task-scoped overlay lease early by task_id. Best-effort — the lease
		// TTL is the hard backstop, so a revoke failure never blocks teardown.
		// No-op unless the broker + controller token are configured. Detached
		// from the inbound ctx so a controller roll mid-teardown still releases
		// the lease (same rationale as the Pod-B teardown above).
		if agentBeadID != "" {
			revokeCtx, revokeCancel := context.WithTimeout(context.WithoutCancel(ctx), zitiRevokeTimeout)
			_ = revokeZitiLease(revokeCtx, logger, cfg, agentBeadID)
			revokeCancel()
		}
		// Clear backend metadata so stale Coop URLs don't linger.
		_ = status.ReportBackendMetadata(ctx, agentBeadID, statusreporter.BackendMetadata{})
		// Report the terminal pod phase to beads regardless of delete error — unless
		// this was a successful freeze, which must leave the bead OPEN at
		// agent_state=stopping (see terminalReportPhase / bug ka-0000007439).
		if phase := terminalReportPhase(event.Type, frozen); phase != "" {
			_ = status.ReportPodStatus(ctx, agentBeadID, statusreporter.PodStatus{
				PodName:   podName,
				Namespace: ns,
				Phase:     phase,
				Ready:     false,
			}, false)
		}

		// E2.4/E2.6: emit the lifecycle event + session-duration observation on
		// session close. AgentStop is a graceful despawn ("stopped"); AgentDone
		// (work complete) and AgentKill (forced teardown) are "despawned". The
		// session-duration outcome distinguishes done/stopped/failed.
		emitSessionCloseMetrics(ctx, logger, daemon, eventMetrics, event, agentBeadID, spendReconciler)
		return err

	case subscriber.AgentStuck:
		// "stuck" lifecycle event (ka-0000000550): the stagnation heuristic
		// (statusreporter checkStagnation) flagged this agent as unresponsive
		// and it is about to be restarted. One event per stuck detection — the
		// reporter clears its stagnation window on publish, so a re-detection
		// after the restart is a genuinely new stuck episode. Emitted before
		// the closed-bead early-return below so a stuck flagged during the
		// close race still counts (it was detected as stuck either way).
		eventMetrics.RecordLifecycleEvent(gardeniometrics.LifecycleStuck, event.Project, event.Role, "")
		// Delete and recreate the pod to restart the agent.
		agentName := strings.TrimPrefix(event.AgentName, event.Project+"-")
		podName := fmt.Sprintf("%s-%s-%s-%s", event.Mode, event.Project, event.Role, agentName)
		// Resolve the namespace via the spec so a runsc-pinned agent (pod in the
		// privileged gVisor namespace, bug ka-0000004301) is deleted and
		// recreated in the right namespace; non-runsc agents resolve to
		// cfg.Namespace unchanged.
		ns := buildAgentPodSpec(cfg, event).Namespace

		// Skip recreate when the bead has already been closed (ka-0000005268).
		// SyncAll can publish a stuck event for a pod whose bead just closed
		// (gb stop) before the K8s grace period deletes the pod. The cached
		// project/role entries may also have been torn down, so
		// buildAgentPodSpec produces a spec with empty Image and the API
		// rejects it with "spec.containers[0].image: Required value". Just
		// clean up the pod and return — no point recreating a closed agent.
		if agentBeadID != "" && beadIsClosed(ctx, daemon, agentBeadID) {
			logger.Info("skipping stuck recreate for closed bead",
				"agent", event.AgentName, "bead", agentBeadID, "pod", podName)
			if err := pods.DeleteAgentPod(ctx, podName, ns); err != nil {
				logger.Warn("failed to delete pod for closed stuck bead",
					"pod", podName, "error", err)
			}
			if cfg.PerAgentTokens {
				// Closed-bead cleanup is a terminal despawn, not a restart.
				revokeAgentToken(ctx, logger, daemon, k8sClient, agentBeadID, ns, tokMetrics, revocationTypeDespawn, eventReceivedAt)
			}
			ghApp.revoke(ctx, agentBeadID, ns, revocationTypeDespawn, eventReceivedAt)
			// Ziti lease (epic ka-0000000990): closed bead is terminal — release
			// any overlay lease for this task. Best-effort; no-op if unconfigured.
			if agentBeadID != "" {
				rCtx, rCancel := context.WithTimeout(context.WithoutCancel(ctx), zitiRevokeTimeout)
				_ = revokeZitiLease(rCtx, logger, cfg, agentBeadID)
				rCancel()
			}
			return nil
		}

		// Non-destructive remedy FIRST (ka-0000001648): crew/job pods use
		// emptyDir, so the delete+recreate below wipes the workspace — an
		// unrecoverable data-loss event when the stuck detection is a false
		// positive (on 2026-07-30 it destroyed an uncommitted FSM-485 e2e repro
		// spec). Ask coop to restart the wedged agent child IN PLACE
		// (force-SIGHUP + respawn with `--resume`) so the pod, its emptyDir
		// workspace, and the conversation all survive. This is viable because
		// the stuck heuristic only fires while coop's HTTP server is reachable
		// (statusreporter.checkStagnation polls it every sweep) — a stuck agent
		// is a wedged CHILD, not a dead coop. Only when the in-place restart
		// fails (coop genuinely unreachable) do we fall through to the
		// destructive delete+recreate, the sole remedy for a pod whose coop is
		// gone. Toggle off with STUCK_RESTART_IN_PLACE=false.
		if cfg.StuckRestartInPlace &&
			tryStuckRestartInPlace(ctx, logger, pods, daemon, nil, podName, ns, event.AgentName, agentBeadID) {
			return nil
		}

		if err := pods.DeleteAgentPod(ctx, podName, ns); err != nil {
			logger.Warn("failed to delete stuck pod (may not exist)", "pod", podName, "error", err)
		}
		// Per-agent tokens: revoke old token before recreating.
		if cfg.PerAgentTokens && agentBeadID != "" {
			revokeAgentToken(ctx, logger, daemon, k8sClient, agentBeadID, ns, tokMetrics, revocationTypeStuckRestart, eventReceivedAt)
		}
		ghApp.revoke(ctx, agentBeadID, ns, revocationTypeStuckRestart, eventReceivedAt)
		// Ziti lease (epic ka-0000000990): release the OLD pod's overlay lease
		// before the recreate mints a fresh one for the same task_id, so the
		// orphaned identity does not linger until its TTL. Best-effort.
		if agentBeadID != "" {
			rCtx, rCancel := context.WithTimeout(context.WithoutCancel(ctx), zitiRevokeTimeout)
			_ = revokeZitiLease(rCtx, logger, cfg, agentBeadID)
			rCancel()
		}
		// Lands spawn wall, as at fresh spawn (gd-Ld5r7Ok2rK): the recreate is a
		// new pod with fresh tokens, so a frozen or paused tenant land (or one
		// the wall refuses for any other reason) does not get it. The stuck pod
		// is already gone; the bead waits at budget_exceeded and the
		// reconciler re-decides it each pass.
		if r := spawnAdmission(ctx, cfg, asSpawnWallDaemon(daemon), landSpawnRate, event.Project, event.Role, agentBeadID, time.Now()); r.Refused() {
			recordSpawnWallRefusal(ctx, logger, asSpawnWallDaemon(daemon), event.Project, event.AgentName, agentBeadID, event.Metadata, r)
			return nil
		}
		spec := buildAgentPodSpec(cfg, event)
		// Harness misconfiguration (gd-helNySn0WA): same fail-closed guard as the
		// fresh-spawn path. Without it a misconfigured codex agent would silently
		// come back as a Claude pod. Refuse the recreate and mark the bead failed
		// (harness refusals file no escalation; the reason names the bead edit).
		if spec.HarnessError != "" {
			recordSpawnRefusal(ctx, logger, daemon, spec, agentBeadID, spawnRefusal{
				refuse:    true,
				permanent: true,
				harness:   true,
				reason:    "invalid agent harness: " + spec.HarnessError,
			})
			return nil
		}
		// Pool-exhaustion escape hatch (ka-0000002560): the stuck pod was already
		// deleted above, so recreating onto a known-dead key (every pool account
		// capacity-exhausted) would just respawn a doomed pod. Decline; the
		// reconciler's create-missing path re-attempts next sync once a window
		// resets. Mark queued so the bead state reflects the wait.
		if spec.SpawnBlocked {
			logger.Warn("declining stuck-restart recreate — project account pool fully exhausted (reconciler will re-attempt)",
				"agent", event.AgentName, "project", event.Project, "role", event.Role,
				"bead", agentBeadID, "reason", spec.SpawnBlockReason)
			if agentBeadID != "" {
				if err := daemon.UpdateAgentState(ctx, agentBeadID, "queued"); err != nil {
					logger.Warn("failed to mark pool-exhausted agent as queued",
						"bead", agentBeadID, "error", err)
				}
			}
			return nil
		}
		// Per-agent tokens: provision fresh token for recreated pod (fail closed).
		tokenMinted := false
		if cfg.PerAgentTokens && agentBeadID != "" {
			secretName, minted, err := provisionAgentToken(ctx, logger, machineDaemon, k8sClient, agentBeadID, event.AgentName, event.Project, spec.Namespace, tokMetrics)
			if err != nil {
				logger.Error("refusing stuck-restart recreate: per-agent token provisioning failed",
					"agent", event.AgentName, "bead", agentBeadID, "error", err)
				return fmt.Errorf("per-agent token provisioning failed for %s: %w", event.AgentName, err)
			}
			spec.DaemonTokenSecret = secretName
			tokenMinted = minted
		}
		// GitHub App token for the recreated pod (fail closed, as at fresh spawn).
		if refuse, reason := ghApp.applyToSpec(ctx, &spec, agentBeadID); refuse {
			releaseMintedAgentToken(ctx, logger, daemon, k8sClient, agentBeadID, spec.Namespace, tokMetrics, tokenMinted)
			recordGithubAppRefusal(ctx, logger, daemon, spec, agentBeadID, reason)
			return nil
		}
		stampResolvedHarness(ctx, logger, daemon, agentBeadID, spec)
		claimSpawnTask(ctx, logger, machineDaemon, agentBeadID, spec)
		if err := pods.CreateAgentPod(ctx, spec); err != nil {
			ghApp.revokeIfCreated(ctx, agentBeadID, spec.Namespace, revocationTypeStuckRestart)
			releaseMintedAgentToken(ctx, logger, daemon, k8sClient, agentBeadID, spec.Namespace, tokMetrics, tokenMinted)
			return err
		}
		cfg.BumpAccountLiveCount(spec.ResolvedAccount)
		// Phase 0 (ka-0000002142): count the winning applyAccountConfig layer for
		// this stuck-restart recreate, mirroring the fresh-spawn path above.
		eventMetrics.RecordAccountLayer(spec.AccountLayer, event.Project, event.Role)
		// Report restarting status. Pass spec.Prewarmed so a prewarmed pod's
		// stuck-restart does not flip agent_state away from "prewarmed".
		_ = status.ReportPodStatus(ctx, agentBeadID, statusreporter.PodStatus{
			PodName:   spec.PodName(),
			Namespace: spec.Namespace,
			Phase:     string("Pending"),
			Ready:     false,
			Message:   "restarted due to stuck detection",
		}, spec.Prewarmed)
		return nil

	case subscriber.AgentUpdate:
		// Metadata updates are handled by the reconciler during periodic sync.
		// The one immediate action is the garden's restart-in-place request for a
		// stale gardener (gd-LUmZlr6CKM, farm_restart.go), answered on the bead.
		if req := farmRestartPending(event.Metadata); req != "" {
			agentName := strings.TrimPrefix(event.AgentName, event.Project+"-")
			podName := fmt.Sprintf("%s-%s-%s-%s", event.Mode, event.Project, event.Role, agentName)
			ns := buildAgentPodSpec(cfg, event).Namespace
			handleFarmRestart(ctx, logger, pods, daemon, nil, agentBeadID, req,
				event.Metadata[beadsapi.FieldRestartRequestedBy], podName, ns, event.AgentName, time.Now())
		}
		return nil

	case subscriber.ConfigUpdated:
		// Running pods are left in place: a config edit reaches every agent at
		// its next session boot via gb-init and prime.sh. The rolling restart
		// that used to live here hard-deleted pods mid-turn and was removed
		// (ka-0000002053, decision ka-0000003611).
		logger.Info("config bead updated; agents pick it up at their next session boot",
			"project", event.Project, "role", event.Role, "config_bead", event.BeadID,
			"title", event.Metadata["title"])
		return nil

	default:
		logger.Warn("unknown event type", "type", event.Type)
		return nil
	}
}

// emitSessionCloseMetrics records the E2.4 lifecycle event and the E2.6
// session-duration observation when an agent session closes. The lifecycle
// kind and the duration `outcome` are derived from the close event type:
//   - AgentStop  → lifecycle "stopped", outcome "stopped" (graceful despawn)
//   - AgentDone  → lifecycle "despawned", outcome "done"  (work complete)
//   - AgentKill  → lifecycle "despawned", outcome "failed" (forced teardown)
//
// Session duration is closed_at − created_at, fetched from the agent bead via
// the daemon (the SSE close payload does not carry timestamps). A fetch
// failure or unparseable timestamps drop the histogram observation but still
// allow the lifecycle counter to increment — the counter never needs the
// daemon round-trip. session_id is left empty on these events (the FSM
// drill-down label is reserved for oom_killed).
func emitSessionCloseMetrics(ctx context.Context, logger *slog.Logger, daemon *beadsapi.Client, eventMetrics *gardeniometrics.EventMetrics, event subscriber.Event, agentBeadID string, spendReconciler *sessionSpendReconciler) {
	if eventMetrics == nil {
		return
	}

	var lifecycle, outcome string
	switch event.Type {
	case subscriber.AgentStop:
		lifecycle, outcome = gardeniometrics.LifecycleStopped, gardeniometrics.OutcomeStopped
	case subscriber.AgentKill:
		lifecycle, outcome = gardeniometrics.LifecycleDespawned, gardeniometrics.OutcomeFailed
	default: // AgentDone
		lifecycle, outcome = gardeniometrics.LifecycleDespawned, gardeniometrics.OutcomeDone
	}

	eventMetrics.RecordLifecycleEvent(lifecycle, event.Project, event.Role, "")

	if agentBeadID == "" {
		return
	}
	bead, err := daemon.GetBead(ctx, agentBeadID)
	if err != nil || bead == nil {
		logger.Debug("session-duration: failed to fetch bead for timestamps",
			"bead", agentBeadID, "error", err)
		return
	}

	// ka-0000008189: an INVOLUNTARY mid-work death ("killed mid-work") is
	// invisible in the event-type-derived `outcome` label above — a stop-gate
	// cooldown involuntary close arrives as AgentDone and would read
	// outcome="done". The honest signal is the agent bead's close_reason, which
	// hook.go / init_agent_loop.go stamp with the `_involuntary` marker only when
	// a claimed task was still in flight. Count it on its own series so its
	// rate()/increase() proves the regression is (or is not) fixed. No-op for a
	// clean idle-out, graceful stop, or genuine crash (family classifier returns
	// not-ok). close_reason lives in the bead's typed fields map.
	if bead.Fields != nil {
		if eventMetrics.RecordInvoluntaryDeath(bead.Fields["close_reason"], event.Project, event.Role) {
			logger.Info("involuntary agent death recorded",
				"bead", agentBeadID, "project", event.Project, "role", event.Role,
				"close_reason", bead.Fields["close_reason"])
		}
	}

	// ka-0000001543: reconcile authoritative per-session LiteLLM cost from the
	// agent tag's baseline-delta and write total_cost_usd onto the agent bead.
	// Done here because the bead (carrying the start baseline) is already in
	// scope and the master key is available controller-side. Best-effort.
	if spendReconciler != nil {
		spendReconciler.reconcileAtClose(ctx, bead, event.AgentName)
	}

	if bead.CreatedAt.IsZero() {
		return
	}
	// closed_at is an RFC3339 string field on the detail; fall back to now when
	// it is absent or unparseable (the close just happened).
	closedAt := time.Now()
	if bead.ClosedAt != "" {
		if t, perr := time.Parse(time.RFC3339, bead.ClosedAt); perr == nil {
			closedAt = t
		}
	}
	seconds := closedAt.Sub(bead.CreatedAt).Seconds()
	eventMetrics.RecordSessionDuration(seconds, event.Project, event.Role, outcome)
}
