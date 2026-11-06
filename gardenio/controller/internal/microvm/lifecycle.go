package microvm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// This file implements C5 lifecycle reconciliation for lambda_microvm agents
// (epic ka-0000005010, Change C5). The Kubernetes reconciler treats an agent
// bead with no pod as stale and reaps it; a MicroVM agent legitimately has NO
// pod, so it MUST be exempted from that rule and reconciled instead against its
// runtime status (GetMicrovm) via AgentRuntime.Status. This coordinator is the
// backend-neutral seam the K8s reconciler and stale reaper call to:
//
//   - recognise a MicroVM agent bead (IsMicrovmAgent),
//   - reconcile a live/terminal runtime against its bead (Reconcile),
//   - tear a runtime down on stop/cancel (Terminate),
//   - sweep leaked runtimes + per-run secrets whose owning bead is gone (GC).
//
// It owns NO K8s knowledge; the reconciler passes it plain agent-bead views.

// ExecutionTargetLambdaMicroVM is the resolved execution_target value that
// selects the MicroVM backend. Duplicated here (rather than importing
// podmanager) to keep this package free of the K8s dependency graph; the
// reconciler passes the resolved value and the two constants are guarded equal
// by a test.
const ExecutionTargetLambdaMicroVM = "lambda_microvm"

// agentBeadView is the minimal, backend-neutral projection of an agent bead the
// lifecycle coordinator needs. The reconciler builds it from beadsapi.AgentBead;
// keeping it local avoids a dependency cycle and pins exactly what the
// coordinator reads.
type AgentBeadView struct {
	// ID is the agent bead ID (the AWS client token + secret-path segment).
	ID string
	// AgentState is the bead's agent_state field.
	AgentState string
	// ExecutionTarget is the resolved backend for this bead (from the
	// execution_target field/metadata). Empty means the default Kubernetes path.
	ExecutionTarget string
	// RuntimeID is the persisted AWS MicroVM id (runtime_id), empty until Launch
	// stamped it or if this bead never launched a runtime.
	RuntimeID string
	// RuntimeState is the last-persisted runtime_state, so Reconcile only writes
	// when the observed state actually changed.
	RuntimeState string
	// AgentSecretARN is the per-run agent-config secret ARN, when known, so
	// Terminate can revoke it without re-deriving. Empty falls back to the
	// backend's deterministic secret name.
	AgentSecretARN string
	// RuntimeSuspendedAt is the RFC3339 timestamp the Suspend path stamped when
	// the VM was put into standby (runtime_suspended_at), cleared on Resume. Leak
	// GC measures standby age from this to enforce the max-standby-age cap; empty
	// means "not currently suspended by us" (or an older bead suspended before
	// the field existed — GC then falls back to the runtime StartedAt).
	RuntimeSuspendedAt string
}

// BeadRuntimeStore is the narrow bead-write surface the lifecycle coordinator
// drives: persist runtime metadata, close a bead, and read back a bead's
// persisted runtime_id so Launch can refuse a re-mint (below). Satisfied by
// *beadsapi.Client (UpdateBeadFields + CloseBead + RuntimeID). Returns only
// primitives so this package stays free of the beads-daemon type graph, the same
// boundary AgentBeadView keeps.
type BeadRuntimeStore interface {
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
	// RuntimeID reads the bead's persisted runtime_id field — the AWS MicroVM id
	// a prior successful Launch stamped (PersistLaunch). Empty when the bead has
	// launched no runtime yet. Launch consults it to bind-once the per-run
	// identity: a bead that already carries a runtime_id must NOT be re-launched
	// (which would re-mint its Ziti lease + overwrite its per-run secrets).
	RuntimeID(ctx context.Context, beadID string) (string, error)
}

// CredentialRevoker revokes the per-run credentials minted for one agent run
// (per-agent beads token, per-run git token, and — once ka-0000001924 lands —
// the per-run Ziti identity). The lifecycle coordinator invokes it on every
// exit path where the runtime is going or gone, so a terminated satellite's
// credentials die with it rather than riding out their TTL backstop:
//
//   - normal termination / stop / cancel  → Terminate
//   - timeout / runtime self-terminated   → Reconcile (terminal-close branch)
//   - leak GC (owning bead gone)          → GC (per-run secret sweep)
//   - failed launch                       → the spawn door (cmd/controller
//     handleNonK8sSpawn) via Lifecycle.RevokeCredentials — a launch that
//     failed never reaches Terminate/Reconcile/GC with a live runtime.
//
// Implementations MUST be idempotent and best-effort: a repeat revoke for the
// same bead (teardown racing a reconcile pass) and a revoke for a bead that
// never minted anything are both no-ops, and failures are logged, never
// propagated — every minted credential has its own expiry as the backstop.
// The implementation lives controller-side (cmd/controller); this package
// stays free of beads-daemon and GitLab knowledge.
type CredentialRevoker interface {
	RevokeAgentCredentials(ctx context.Context, beadID, reason string)
}

// Lifecycle drives C5 reconciliation for MicroVM agents. It is constructed with
// a ready *Backend and a bead store; when the backend is nil (default-off /
// unavailable) it is a no-op that reports no bead as a MicroVM agent, so the
// reconciler's normal Kubernetes path is unchanged.
type Lifecycle struct {
	backend *Backend
	store   BeadRuntimeStore
	logger  *slog.Logger

	// revoker, when set, is invoked on every exit path (terminate, terminal
	// reconcile-close, leak GC, and the spawn door's failed-launch cleanup via
	// RevokeCredentials) so per-run credentials die with the runtime. Nil means
	// no per-run credentials need lifecycle revocation (their TTLs backstop).
	revoker CredentialRevoker

	// gcMinAge is the minimum runtime/secret age before leak GC will tear it
	// down, so a resource created moments before its bead-write (or before the
	// bead has surfaced in the daemon list) is never mistaken for a leak.
	gcMinAge time.Duration

	// --- Resume rehydration boundary (step 5, rehydrate.go) ---

	// rehydrateEnabled gates the fail-closed proof-of-life rendezvous. When
	// false, Resume keeps the step-3 behavior (stamp working immediately, no
	// gate) — the documented rollback. Set from config.MicrovmResumeRehydrateEnabled.
	rehydrateEnabled bool

	// rehydrateDeadline bounds the proof-of-life wait (0 → DefaultResumeRehydrateDeadline).
	rehydrateDeadline time.Duration

	// prober reads the resumed VM's proof-of-life (presence last_seen advancing
	// over the overlay). Nil until SetLivenessProber wires it; when nil the gate
	// can never prove life so every resume fails closed (safe: cold-start).
	prober LivenessProber

	// rehydration tracks in-flight resume rehydrations per agent so a duplicate
	// resume dedups and the resume-status endpoint can report progress.
	rehydration *rehydrationRegistry

	// nowFunc is overridable in tests; defaults to time.Now.
	nowFunc func() time.Time

	// maxStandbyAge caps how long a controller-suspended MicroVM thread agent may
	// sit in SUSPENDED standby before leak GC terminates its VM and closes the
	// (still-open) resume-target bead as done (step 4, ka-0000006202). <=0 (the
	// default) disables the cap, so a claimed SUSPENDED VM is left alone
	// indefinitely and the AWS IdlePolicy SuspendedDurationSeconds auto-terminate
	// backstop (step 1) is the only standby-billing limit. Measured from the
	// runtime_suspended_at the Suspend path stamps (cleared on Resume), so it
	// mirrors AWS's SuspendedDurationSeconds semantics rather than total
	// VM-wall-clock age.
	maxStandbyAge time.Duration
}

// now returns the current time via the overridable clock (time.Now in prod).
func (l *Lifecycle) now() time.Time {
	if l != nil && l.nowFunc != nil {
		return l.nowFunc()
	}
	return time.Now()
}

// defaultGCMinAge is the grace period a leaked runtime/secret must exceed before
// GC reaps it. Comfortably longer than the launch→bead-write window and one
// reconcile cycle, so an in-flight spawn is never reaped.
const defaultGCMinAge = 10 * time.Minute

// NewLifecycle builds a lifecycle coordinator. A nil backend yields a no-op
// coordinator (IsMicrovmAgent always false); the caller wires it only when the
// MicroVM backend is available.
func NewLifecycle(backend *Backend, store BeadRuntimeStore, logger *slog.Logger) *Lifecycle {
	if logger == nil {
		logger = slog.Default()
	}
	return &Lifecycle{
		backend:     backend,
		store:       store,
		logger:      logger,
		gcMinAge:    defaultGCMinAge,
		rehydration: newRehydrationRegistry(),
		nowFunc:     time.Now,
	}
}

// SetResumeRehydration configures the fail-closed resume rehydration boundary
// (step 5). Wired by the controller from config: enabled toggles the gate
// (false = step-3 stamp-working-immediately rollback), deadline bounds the
// proof-of-life wait (<=0 → DefaultResumeRehydrateDeadline). The LivenessProber
// is wired separately via SetLivenessProber (it needs the daemon roster client).
// Nil-safe.
func (l *Lifecycle) SetResumeRehydration(enabled bool, deadline time.Duration) {
	if l == nil {
		return
	}
	l.rehydrateEnabled = enabled
	l.rehydrateDeadline = deadline
}

// Available reports whether the MicroVM backend is wired. When false the
// coordinator is inert and the reconciler behaves exactly as before.
func (l *Lifecycle) Available() bool { return l != nil && l.backend != nil }

// SetCredentialRevoker installs the per-run credential revoker the lifecycle
// invokes on every exit path. Wired by the controller after construction (the
// revoker needs the daemon client + config the controller owns). Nil-safe.
func (l *Lifecycle) SetCredentialRevoker(r CredentialRevoker) {
	if l != nil {
		l.revoker = r
	}
}

// SetMaxStandbyAge installs the controller-side standby cap: a SUSPENDED thread
// agent VM still claimed by an open bead is left alone as intentional standby
// until it exceeds this age, at which point leak GC terminates it and closes the
// bead as done (step 4, ka-0000006202). <=0 disables the cap (the default), so a
// claimed SUSPENDED VM is never reaped by GC — only the AWS IdlePolicy
// SuspendedDurationSeconds backstop bounds standby. Wired by the controller from
// MICROVM_MAX_STANDBY_AGE. Nil-safe.
func (l *Lifecycle) SetMaxStandbyAge(d time.Duration) {
	if l != nil {
		l.maxStandbyAge = d
	}
}

// RevokeCredentials revokes the per-run credentials for one agent bead via the
// installed revoker. Exposed for the spawn door's failed-launch cleanup — the
// one exit path that never reaches Terminate/Reconcile/GC. Best-effort no-op
// when no revoker is installed.
func (l *Lifecycle) RevokeCredentials(ctx context.Context, beadID, reason string) {
	if l == nil || l.revoker == nil {
		return
	}
	l.revoker.RevokeAgentCredentials(ctx, beadID, reason)
}

// IsMicrovmAgent reports whether the bead is a lambda_microvm agent this
// coordinator owns. False for every bead when the backend is unavailable, so
// the K8s reconciler keeps reaping ordinary podless beads unchanged.
func (l *Lifecycle) IsMicrovmAgent(view AgentBeadView) bool {
	if !l.Available() {
		return false
	}
	return strings.TrimSpace(view.ExecutionTarget) == ExecutionTargetLambdaMicroVM
}

// Launch realizes a MicroVM agent (backend.Launch) and persists its runtime
// metadata onto the agent bead so later reconcile passes recognise it and drive
// GetMicrovm off runtime_id. It is the single spawn-door entry point for the
// MicroVM backend. On a launch error nothing is persisted (Launch already
// rolled back any per-run secret); the caller marks the agent failed. On
// success the runtime is live regardless of whether the metadata write lands,
// so a persist failure is logged (not returned as a launch failure) — but leak
// GC would reap a runtime whose bead never recorded its id, so the write is
// retried best-effort and its failure surfaced to the caller for logging.
//
// Bind-once identity guard (warm-pool plan ka-0000004145, step ka-0000006503).
// The per-run Ziti lease + 2 Secrets Manager secrets are minted keyed to the
// bead ID at the FIRST Launch (backend.go), and the warm pool's whole premise is
// that assignment reuses that SAME bead so the identity binding is preserved with
// NO re-Launch and NO re-mint (AssignPrewarmed only flips agent_state). But
// backend.Launch is idempotent ONLY at the AWS layer — RunMicrovm's ClientToken
// collapses duplicate VMs, while the identity mint is deliberately NOT idempotent:
// BrokerClient.IssueLease "mints a NEW identity every call" (broker.go) and the
// identity secret is overwritten in place. So if a spawn event is re-emitted for a
// bead that already launched — an SSE reconnect replaying its `created`, or its own
// status→in_progress re-spawn (subscriber/sse.go) — routing back through here would
// re-mint the identity and orphan the live VM's existing lease. Refuse that:
// consult the bead's persisted runtime_id and, if it already carries one, return
// the reconstructed handle WITHOUT calling backend.Launch (zero IssueLease, zero
// secret writes). A read error fails OPEN — the bead ID is still the AWS
// ClientToken so a genuine duplicate VM is collapsed anyway, and leak GC backstops
// a stray lease — so a transient daemon blip never wedges a real first launch.
func (l *Lifecycle) Launch(ctx context.Context, spec AgentSpec) (RuntimeHandle, error) {
	if !l.Available() {
		return RuntimeHandle{}, fmt.Errorf("microvm: lifecycle backend unavailable")
	}
	if l.store != nil {
		// Authoritative fresh read, not event metadata: a replayed `created`
		// event carries the bead's original (empty) fields, so only a live read
		// sees a runtime_id a prior Launch persisted since.
		if rid, rerr := l.store.RuntimeID(ctx, spec.BeadID); rerr != nil {
			l.logger.Warn("microvm: could not read runtime_id before launch; proceeding (ClientToken + leak GC backstop)",
				"bead", spec.BeadID, "error", rerr)
		} else if strings.TrimSpace(rid) != "" {
			// Already launched: the per-run identity is minted and bound to this
			// bead. Re-launching would re-mint it. Return the existing binding.
			l.logger.Info("microvm: bead already has a runtime; skipping re-launch to preserve per-run identity binding (no re-mint)",
				"bead", spec.BeadID, "runtime_id", rid)
			return l.handleFor(AgentBeadView{ID: spec.BeadID, RuntimeID: rid}), nil
		}
	}
	handle, err := l.backend.Launch(ctx, spec)
	if err != nil {
		return RuntimeHandle{}, err
	}
	if perr := l.PersistLaunch(ctx, spec.BeadID, handle); perr != nil {
		l.logger.Warn("microvm: launched but failed to persist runtime metadata (leak GC backstops)",
			"bead", spec.BeadID, "runtime_id", handle.MicrovmID, "error", perr)
	}
	return handle, nil
}

// PersistLaunch records the runtime metadata a successful Launch produced onto
// the agent bead: execution_target, runtime_id, the AWS account/region, and the
// launch time. This is what lets the reconciler recognise the agent as a
// MicroVM agent on later passes and drive GetMicrovm off runtime_id. Best-effort
// from the caller's view — a write failure is returned so the spawn door can log
// it, but the runtime is already live regardless.
func (l *Lifecycle) PersistLaunch(ctx context.Context, beadID string, handle RuntimeHandle) error {
	if !l.Available() || l.store == nil {
		return nil
	}
	fields := map[string]string{
		"execution_target": ExecutionTargetLambdaMicroVM,
		"runtime_id":       handle.MicrovmID,
		"runtime_state":    "PENDING",
		"runtime_region":   l.backend.Region(),
	}
	if acct := l.backend.LauncherAccount(); acct != "" {
		fields["runtime_account"] = acct
	}
	if !handle.StartedAt.IsZero() {
		fields["runtime_started_at"] = handle.StartedAt.UTC().Format(time.RFC3339)
	}
	if err := l.store.UpdateBeadFields(ctx, beadID, fields); err != nil {
		return fmt.Errorf("microvm: persist launch metadata for %s: %w", beadID, err)
	}
	return nil
}

// handleFor rebuilds a RuntimeHandle for a bead from its persisted metadata so
// Status/Terminate can act without the in-memory launch handle (the controller
// may have restarted since Launch). Secret names are deterministic on the bead
// ID, so nothing beyond the view is needed.
func (l *Lifecycle) handleFor(view AgentBeadView) RuntimeHandle {
	secretARN := strings.TrimSpace(view.AgentSecretARN)
	if secretARN == "" {
		// Fall back to the deterministic per-run secret name so Terminate can
		// still revoke it after a controller restart lost the launch handle.
		secretARN = l.backend.agentSecretName(view.ID)
	}
	handle := RuntimeHandle{
		BeadID:            view.ID,
		MicrovmID:         strings.TrimSpace(view.RuntimeID),
		AgentSecretARN:    secretARN,
		IdentitySecretARN: l.backend.cfg.StaticIdentitySecretARN,
		IsPerRunIdentity:  false,
	}
	if l.backend.PerRunIdentityEnabled() {
		// Per-run identity regime (ka-0000001924): Terminate must revoke the
		// bead's identity secret + broker leases. Deterministic name — safe
		// even for a bead launched under the static regime (deleting a
		// never-created secret and revoking zero leases are idempotent no-ops).
		handle.IdentitySecretARN = l.backend.identitySecretName(view.ID)
		handle.IsPerRunIdentity = true
	}
	return handle
}

// ReconcileOutcome is the result of reconciling one MicroVM agent bead against
// its runtime.
type ReconcileOutcome struct {
	// Closed is true when the coordinator closed the bead (the runtime is gone
	// or terminal), so the reconciler drops it from the desired set.
	Closed bool
	// Live is true when the runtime is still doing (or could resume) work, so
	// the reconciler leaves the bead open and does NOT reap it.
	Live bool
	// State is the raw runtime state observed (persisted as runtime_state).
	State string
}

// Reconcile queries the runtime status for a MicroVM agent bead and converges
// the bead:
//
//   - runtime Live  → persist runtime_state, leave the bead open (Live=true).
//   - runtime gone/terminal → revoke the run's per-run secrets + Ziti identity
//     leases (the timeout row of the revocation matrix — a max-duration expiry
//     reaches a terminal state without any AgentStop/Done event), then close
//     the bead (Closed=true) with a runtime-derived agent_state.
//   - transient Status error → return the error; the reconciler leaves the bead
//     untouched this pass (never reap on an inconclusive read).
//
// A bead with no persisted runtime_id has no runtime to reconcile against yet
// (launch in flight, or the bead-write raced): treated as Live so it is left
// alone until a runtime id appears or GC ages it out.
func (l *Lifecycle) Reconcile(ctx context.Context, view AgentBeadView) (ReconcileOutcome, error) {
	if !l.Available() {
		return ReconcileOutcome{Live: true}, nil
	}
	if strings.TrimSpace(view.RuntimeID) == "" {
		// No runtime yet — launch may be in flight. Do not reap.
		return ReconcileOutcome{Live: true}, nil
	}

	handle := l.handleFor(view)
	status, err := l.backend.Status(ctx, handle)
	if err != nil {
		return ReconcileOutcome{}, fmt.Errorf("microvm: reconcile status for %s: %w", view.ID, err)
	}

	if status.Live {
		// Persist the observed state so the beads-viewer/operator sees the live
		// runtime state. Best-effort: a write failure does not change the verdict.
		if l.store != nil && status.State != "" && status.State != view.RuntimeState {
			if err := l.store.UpdateBeadFields(ctx, view.ID, map[string]string{"runtime_state": status.State}); err != nil {
				l.logger.Warn("microvm: failed to persist runtime_state",
					"bead", view.ID, "state", status.State, "error", err)
			}
		}
		return ReconcileOutcome{Live: true, State: status.State}, nil
	}

	// Terminal or gone: the runtime is not coming back, so everything per-run
	// must die with it. This is the TIMEOUT exit path (a MicroVM that hit
	// MaximumDurationInSeconds, or otherwise self-terminated) — no stop/cancel
	// event ever fires for it, so this is the only place its credentials and
	// Ziti identity get eagerly revoked.
	//
	// Two revocation seams fire here, both idempotent and best-effort:
	//   - RevokeCredentials — the controller-side per-run credentials (beads
	//     token, git token).
	//   - backend.Terminate — the per-run secrets + broker identity leases
	//     (ka-0000001924); the VM itself is already gone, so this is purely
	//     cleanup. Leak GC and the broker TTL reaper backstop any failure, so
	//     neither may block closing the bead.
	l.RevokeCredentials(ctx, view.ID, "runtime terminal: "+strings.ToLower(status.State))
	if err := l.backend.Terminate(ctx, handle, "reconcile: runtime terminal/gone"); err != nil {
		l.logger.Warn("microvm: per-run cleanup on terminal reconcile failed (leak GC backstops)",
			"bead", view.ID, "runtime_id", view.RuntimeID, "error", err)
	}

	// Close the bead. Derive a benign agent_state — a MicroVM that terminated
	// on its own (task complete, max-duration) is "done", not a crash, so the
	// Slack card does not read ❌.
	closeState := microvmCloseState(view.AgentState)
	closeReason := fmt.Sprintf("microvm_runtime_%s: no live runtime for %s",
		strings.ToLower(status.State), view.RuntimeID)
	l.logger.Info("closing MicroVM agent bead: runtime terminal/gone",
		"bead", view.ID, "runtime_id", view.RuntimeID,
		"runtime_state", status.State, "exists", status.Exists, "close_state", closeState)
	if l.store != nil {
		if err := l.store.CloseBead(ctx, view.ID, map[string]string{
			"agent_state":   closeState,
			"runtime_state": status.State,
			"close_reason":  closeReason,
		}); err != nil {
			return ReconcileOutcome{State: status.State}, fmt.Errorf("microvm: close terminal bead %s: %w", view.ID, err)
		}
	}
	return ReconcileOutcome{Closed: true, State: status.State}, nil
}

// Terminate tears down the runtime for a MicroVM agent on stop/cancel and cleans
// up its per-run secret. Idempotent (a gone runtime / deleted secret is
// success). The retry idempotency token is the bead ID (same as Launch), so a
// re-terminate is safe. No-op when the coordinator is unavailable.
func (l *Lifecycle) Terminate(ctx context.Context, view AgentBeadView, reason string) error {
	if !l.Available() {
		return nil
	}
	handle := l.handleFor(view)
	l.logger.Info("terminating MicroVM agent runtime",
		"bead", view.ID, "runtime_id", handle.MicrovmID, "reason", reason)
	// Revoke per-run credentials regardless of how the AWS teardown fares: the
	// decision to end this run is already made, and if TerminateMicrovm fails
	// the leak GC reaps the runtime later — the credentials must not outlive
	// either outcome. This is the NORMAL-TERMINATION exit path (done/stop/kill).
	l.RevokeCredentials(ctx, view.ID, "terminate: "+reason)
	if err := l.backend.Terminate(ctx, handle, reason); err != nil {
		return fmt.Errorf("microvm: terminate runtime for %s: %w", view.ID, err)
	}
	// Best-effort: reflect the terminal runtime state on the bead. The bead
	// close itself is owned by the caller (AgentDone/Stop/Kill teardown).
	if l.store != nil {
		if err := l.store.UpdateBeadFields(ctx, view.ID, map[string]string{"runtime_state": "TERMINATED"}); err != nil {
			l.logger.Debug("microvm: failed to stamp TERMINATED runtime_state (bead may be closing)",
				"bead", view.ID, "error", err)
		}
	}
	return nil
}

// Resume wakes a SUSPENDED MicroVM agent back to RUNNING in place — the native
// analog of the gVisor name-keyed thaw, driven by the next message for a
// suspended thread agent (step 3, plan ka-0000004518 / epic ka-0000004689). It
// rebuilds the handle from the bead's persisted runtime_id and calls
// backend.Resume (ResumeMicrovm), then flips the bead out of frozen-awaiting-thaw.
//
// The agent_state it stamps depends on the step-5 rehydration boundary
// (rehydrate.go, docs/microvm-resume-rehydration.md):
//
//   - rehydration gate ON (default): stamp agent_state=RESUMING, NOT working.
//     The VM is coming back but has NOT yet proven it re-established its identity
//     and overlay after the fresh warm-boot the AWS resume hook triggered. The
//     caller then runs AwaitRehydration, which flips the bead to working only
//     once the resumed VM's own egress heartbeat re-crosses the overlay to beads
//     (proof-of-life), or fails closed to resume_failed. This is what stops the
//     agent serving the user on a half-rehydrated identity.
//   - rehydration gate OFF (rollback): stamp agent_state=working immediately, the
//     step-3 behavior — no proof-of-life gate.
//
// runtime_state=PENDING either way (the VM is coming up; the reconciler converges
// it to RUNNING).
//
// CRITICAL — resume-in-place, never close+remint: unlike gVisor (whose snapshot
// is node-local and name-keyed, so the bridge safely closes the old bead and
// mints a same-name one), a MicroVM's identity lives in runtime_id ON THE BEAD.
// Closing the bead would fire AgentDone -> Lifecycle.Terminate and destroy the
// suspended VM, and a fresh mint has no runtime_id so it would Launch a brand-new
// RunMicrovm — either way the suspended VM is abandoned and continuity is lost
// (the exact bug the gVisor guard test protects against). So the caller MUST
// resume this same open bead, and this method leaves the bead OPEN.
//
// Idempotent like Terminate (backend.Resume tolerates a gone/wrong-state VM as a
// no-op success), so a duplicate message or a race with the reconciler never
// surfaces spuriously. Unlike Terminate it does NOT revoke per-run credentials —
// the run is continuing, and the session/Ziti/NATS material must survive standby
// (step 2 leaves it intact on suspend). A missing runtime_id is a programming
// error the caller guards, but backend.Resume treats an empty id as a no-op so
// this is still safe. No-op when the coordinator is unavailable.
func (l *Lifecycle) Resume(ctx context.Context, view AgentBeadView) error {
	if !l.Available() {
		return nil
	}
	handle := l.handleFor(view)
	l.logger.Info("resuming MicroVM agent runtime",
		"bead", view.ID, "runtime_id", handle.MicrovmID, "rehydrate_gate", l.rehydrateEnabled)
	if err := l.backend.Resume(ctx, handle); err != nil {
		return fmt.Errorf("microvm: resume runtime for %s: %w", view.ID, err)
	}
	// Flip the bead out of frozen-awaiting-thaw. With the rehydration gate on the
	// agent is NOT yet live — it lands at RESUMING and AwaitRehydration owns the
	// working/resume_failed transition; with the gate off it lands at working
	// immediately (step-3 rollback). The bead stays OPEN either way — closing it
	// would terminate the very VM we just resumed. Clear runtime_suspended_at so
	// the standby-age clock (step 4) resets — the VM is no longer in standby, and
	// a later re-suspend re-stamps a fresh anchor.
	resumedState := AgentStateWorking
	if l.rehydrateEnabled {
		resumedState = AgentStateResuming
	}
	if l.store != nil {
		if err := l.store.UpdateBeadFields(ctx, view.ID, map[string]string{
			"runtime_state":        "PENDING",
			"agent_state":          resumedState,
			"runtime_suspended_at": "",
		}); err != nil {
			// Best-effort: the VM is already resuming regardless. A stale
			// agent_state=stopping would only make a redundant resume a no-op.
			l.logger.Warn("microvm: resumed runtime but failed to stamp resumed state",
				"bead", view.ID, "runtime_id", handle.MicrovmID, "state", resumedState, "error", err)
		}
	}
	return nil
}

// Suspend puts the MicroVM for a freeze-eligible thread agent into standby on a
// graceful stop (AgentStop) instead of terminating it, so a later re-mention can
// resume the SAME VM with all memory + disk state intact (the native analog of
// the gVisor `runsc checkpoint --leave-running` freeze; plan ka-0000004518, step
// 2 ka-0000007189). It is the deliberate counterpart to Terminate on the
// suspend-on-stop branch:
//
//   - It calls backend.Suspend (idempotent SuspendMicrovm — a gone/wrong-state VM
//     is a success) rather than backend.Terminate, so the VM survives as the
//     resume target.
//   - It does NOT revoke per-run credentials or Ziti identity leases. Those MUST
//     survive standby so the resumed agent keeps working without a re-mint —
//     session creds default to 12h to cover suspended wall-clock (config.go
//     MicrovmSessionCredentialDurationSeconds). Revoking here would strand the
//     resumed VM on dead credentials, defeating the whole hot-resume path.
//   - It LEAVES THE BEAD OPEN. The caller (handleEvent suspend branch) does not
//     close it; the bead stays at agent_state=stopping — the same
//     suspended-awaiting-resume signal the gVisor freeze uses — so step 3's
//     resume-on-message finds it and calls backend.Resume. Only runtime_state is
//     stamped (best-effort) so the beads-viewer/operator sees standby; the
//     reconciler converges the precise AWS state (SUSPENDING→SUSPENDED, both Live)
//     on its next pass.
//
// Idempotent (bead-ID token, like Terminate/Launch), so a duplicate AgentStop or
// a race with the reconciler re-suspends harmlessly. No-op when the coordinator
// is unavailable.
func (l *Lifecycle) Suspend(ctx context.Context, view AgentBeadView, reason string) error {
	if !l.Available() {
		return nil
	}
	handle := l.handleFor(view)
	l.logger.Info("suspending MicroVM agent runtime (standby, resume target)",
		"bead", view.ID, "runtime_id", handle.MicrovmID, "reason", reason)
	// NOTE: unlike Terminate, we deliberately do NOT call RevokeCredentials here —
	// the per-run credentials + Ziti leases must outlive the standby window so the
	// resumed VM stays authenticated (12h session creds backstop; see above).
	if err := l.backend.Suspend(ctx, handle); err != nil {
		return fmt.Errorf("microvm: suspend runtime for %s: %w", view.ID, err)
	}
	// Best-effort: reflect the standby runtime state on the still-open bead, and
	// stamp runtime_suspended_at as the anchor leak GC measures max-standby-age
	// from (step 4, ka-0000006202) — mirroring AWS's SuspendedDurationSeconds,
	// which counts from suspend, not from VM start. The reconciler reconciles the
	// precise AWS state (SUSPENDING→SUSPENDED) later; a write failure here does
	// not change the suspend verdict (GC then conservatively leaves the VM alone
	// until the AWS IdlePolicy backstop reaps it).
	if l.store != nil {
		if err := l.store.UpdateBeadFields(ctx, view.ID, map[string]string{
			"runtime_state":        "SUSPENDED",
			"runtime_suspended_at": time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			l.logger.Debug("microvm: failed to stamp SUSPENDED runtime_state (bead may be updating)",
				"bead", view.ID, "error", err)
		}
	}
	return nil
}

// GCStats summarises one leak-GC sweep.
type GCStats struct {
	RuntimesScanned int
	RuntimesReaped  int
	SecretsScanned  int
	SecretsDeleted  int
	// StandbyReaped counts SUSPENDED thread-agent VMs terminated for exceeding
	// the max-standby-age cap (step 4, ka-0000006202) — their beads are closed as
	// done. Distinct from RuntimesReaped, which counts leaked runtimes whose
	// owning bead was already gone.
	StandbyReaped int
}

// StandbyCandidate is one open MicroVM thread-agent bead the controller
// suspended into standby (agent_state=stopping, runtime_state SUSPENDED),
// carried into GC so the max-standby-age cap can reap a VM that was suspended on
// `gb stop` but no re-mention ever resumed (step 4, ka-0000006202). The
// controller builds these from the same open-agent-bead list that yields
// liveRuntimeIDs/liveBeadIDs; a SUSPENDED VM is legitimately claimed by its open
// resume-target bead (so the runtime leak loop leaves it alone), and this is the
// one seam that bounds how long that standby may bill.
type StandbyCandidate struct {
	// BeadID is the still-open resume-target agent bead — the VM's identity
	// anchor and the bead GC closes when the cap is exceeded.
	BeadID string
	// RuntimeID is the persisted AWS MicroVM id to terminate.
	RuntimeID string
	// AgentState is the bead's prior agent_state (stopping for a suspended
	// standby), mapped through microvmCloseState so a benign standby reap closes
	// the bead as done rather than failed.
	AgentState string
	// RuntimeSuspendedAt is the RFC3339 anchor the Suspend path stamped
	// (runtime_suspended_at), cleared on Resume. Standby age is now−this, so the
	// cap mirrors AWS's SuspendedDurationSeconds (which counts from suspend, not
	// from VM start). Empty/unparseable ⇒ GC cannot age it and leaves it to the
	// AWS IdlePolicy backstop.
	RuntimeSuspendedAt string
}

// GC sweeps leaked runtimes and per-run secrets whose owning bead is gone, and
// enforces the max-standby-age cap on suspended thread-agent VMs.
//
// liveRuntimeIDs is the set of runtime_ids carried by CURRENTLY-OPEN MicroVM
// agent beads (the reconciler's desired set); liveBeadIDs is the set of open
// agent bead IDs. A runtime not claimed by any live bead's runtime_id, and older
// than gcMinAge, is a leak — TerminateMicrovm it. A per-run secret whose
// gardenio:agent-bead tag names no live bead, and older than gcMinAge, is a leak
// — force-delete it. The age gate protects an in-flight launch whose bead-write
// or daemon-list has not caught up.
//
// standby is the set of open, controller-suspended thread-agent beads (step 4,
// ka-0000006202). A SUSPENDED VM is legitimately claimed by its still-open
// resume-target bead, so the runtime leak loop above LEAVES IT ALONE — that is
// exactly the "standby != leak" guarantee. This loop is the one seam that bounds
// standby: a candidate whose runtime_suspended_at is older than maxStandbyAge
// (when the cap is enabled) is terminated and its bead closed as done, a
// belt-and-suspenders companion to the AWS IdlePolicy SuspendedDurationSeconds
// auto-terminate backstop. When maxStandbyAge<=0 the cap is disabled and every
// claimed SUSPENDED VM is left to that AWS backstop.
//
// GC is intentionally conservative: it only ever reaps a leaked resource that is
// BOTH unclaimed AND aged (and a standby VM only once past the cap), and it
// treats an enumeration error as a hard stop (return the error, reap nothing) so
// a partial list can never mass-delete live work.
func (l *Lifecycle) GC(ctx context.Context, liveRuntimeIDs, liveBeadIDs map[string]bool, standby []StandbyCandidate, now time.Time) (GCStats, error) {
	var stats GCStats
	if !l.Available() {
		return stats, nil
	}

	runtimes, err := l.backend.ListRuntimes(ctx)
	if err != nil {
		return stats, fmt.Errorf("microvm: GC list runtimes: %w", err)
	}
	stats.RuntimesScanned = len(runtimes)
	for _, rt := range runtimes {
		if !rt.Live {
			continue // already terminating/terminated — AWS reaps it
		}
		if liveRuntimeIDs[rt.MicrovmID] {
			continue // claimed by an open bead
		}
		if !rt.StartedAt.IsZero() && now.Sub(rt.StartedAt) < l.gcMinAge {
			continue // too young — launch/bead-write may be in flight
		}
		l.logger.Warn("microvm GC: terminating leaked runtime (no live bead claims it)",
			"runtime_id", rt.MicrovmID, "state", rt.State,
			"started_at", rt.StartedAt, "age", now.Sub(rt.StartedAt).Round(time.Second))
		if err := l.backend.TerminateRuntimeByID(ctx, rt.MicrovmID, "leak_gc: no live bead"); err != nil {
			l.logger.Warn("microvm GC: failed to terminate leaked runtime",
				"runtime_id", rt.MicrovmID, "error", err)
			continue
		}
		stats.RuntimesReaped++
	}

	secrets, err := l.backend.ListPerRunSecrets(ctx)
	if err != nil {
		return stats, fmt.Errorf("microvm: GC list secrets: %w", err)
	}
	stats.SecretsScanned = len(secrets)
	// Leaked beads whose broker identity leases must be revoked too — deduped,
	// since a bead leaks up to two secrets (agent-config + ziti-identity).
	leakedBeads := map[string]bool{}
	for _, s := range secrets {
		if s.BeadID == "" {
			continue // untagged — not ours to reap
		}
		if liveBeadIDs[s.BeadID] {
			continue // owning bead still open
		}
		if !s.CreatedDate.IsZero() && now.Sub(s.CreatedDate) < l.gcMinAge {
			continue // too young — launch may be mid-flight
		}
		l.logger.Warn("microvm GC: deleting leaked per-run secret (owning bead gone)",
			"secret", s.Name, "bead", s.BeadID,
			"created", s.CreatedDate, "age", now.Sub(s.CreatedDate).Round(time.Second))
		if err := l.backend.DeletePerRunSecret(ctx, s.ARN); err != nil {
			l.logger.Warn("microvm GC: failed to delete leaked secret",
				"secret", s.Name, "bead", s.BeadID, "error", err)
			continue
		}
		stats.SecretsDeleted++
		leakedBeads[s.BeadID] = true
	}

	// The LEAK-GC exit path: each leaked bead's per-run credentials (beads
	// token, git token — a Terminate that never ran, or a crash between mint
	// and launch) and broker identity leases (ka-0000001924) are orphans —
	// revoke them now rather than letting them ride out their TTL backstops.
	// Keyed purely by bead ID and deduped (a bead leaks up to two secrets);
	// idempotent, failures only logged.
	for beadID := range leakedBeads {
		l.RevokeCredentials(ctx, beadID, "leak_gc: owning bead gone")
		if err := l.backend.RevokeIdentityLeases(ctx, beadID); err != nil {
			l.logger.Warn("microvm GC: failed to revoke leaked identity leases (broker TTL reaper backstops)",
				"bead", beadID, "error", err)
		}
	}

	// The MAX-STANDBY-AGE cap (step 4, ka-0000006202). A SUSPENDED thread-agent
	// VM is intentional standby — claimed by an open resume-target bead, so the
	// leak loop above never touched it. This is the belt-and-suspenders that
	// bounds how long that standby may bill when the cap is enabled: reap the VM
	// (Terminate — full teardown incl. per-run secrets + Ziti leases + creds,
	// because unlike a resume this run is over) and close the still-open bead as
	// done. Only fires when the cap is configured; disabled (<=0) leaves standby
	// to the AWS IdlePolicy SuspendedDurationSeconds backstop.
	if l.maxStandbyAge > 0 {
		l.reapExpiredStandby(ctx, standby, now, &stats)
	}

	if stats.RuntimesReaped > 0 || stats.SecretsDeleted > 0 || stats.StandbyReaped > 0 {
		l.logger.Info("microvm GC sweep complete",
			"runtimes_scanned", stats.RuntimesScanned, "runtimes_reaped", stats.RuntimesReaped,
			"secrets_scanned", stats.SecretsScanned, "secrets_deleted", stats.SecretsDeleted,
			"standby_reaped", stats.StandbyReaped)
	}
	return stats, nil
}

// reapExpiredStandby terminates any suspended thread-agent VM whose standby has
// exceeded maxStandbyAge and closes its still-open bead as done (step 4,
// ka-0000006202). Caller guards l.maxStandbyAge>0. Standby age is measured from
// runtime_suspended_at (the Suspend anchor, cleared on Resume) so it mirrors
// AWS's SuspendedDurationSeconds; a candidate with no parseable anchor is left
// alone (the AWS IdlePolicy backstop reaps it) rather than reaped on a guess.
func (l *Lifecycle) reapExpiredStandby(ctx context.Context, standby []StandbyCandidate, now time.Time, stats *GCStats) {
	for _, c := range standby {
		runtimeID := strings.TrimSpace(c.RuntimeID)
		beadID := strings.TrimSpace(c.BeadID)
		if runtimeID == "" || beadID == "" {
			continue // nothing to reap / nothing to close
		}
		anchor := strings.TrimSpace(c.RuntimeSuspendedAt)
		if anchor == "" {
			continue // no standby anchor — defer to the AWS IdlePolicy backstop
		}
		suspendedAt, err := time.Parse(time.RFC3339, anchor)
		if err != nil {
			l.logger.Warn("microvm GC: unparseable runtime_suspended_at; leaving standby VM to AWS backstop",
				"bead", beadID, "runtime_id", runtimeID, "runtime_suspended_at", anchor, "error", err)
			continue
		}
		age := now.Sub(suspendedAt)
		if age < l.maxStandbyAge {
			continue // still within the standby cap — intentional standby, leave it
		}
		l.logger.Warn("microvm GC: terminating over-cap suspended standby VM and closing bead",
			"bead", beadID, "runtime_id", runtimeID,
			"suspended_at", suspendedAt, "standby_age", age.Round(time.Second),
			"max_standby_age", l.maxStandbyAge)

		// Full teardown: this run is over (no resume came), so per-run secrets,
		// Ziti leases, and credentials must all die with the VM — exactly the
		// Terminate path, not the Suspend path. Best-effort like the leak loop.
		view := AgentBeadView{ID: beadID, RuntimeID: runtimeID, AgentState: c.AgentState}
		l.RevokeCredentials(ctx, beadID, "standby_cap: suspended past max-standby-age")
		if err := l.backend.Terminate(ctx, l.handleFor(view), "standby_cap: suspended past max-standby-age"); err != nil {
			l.logger.Warn("microvm GC: failed to terminate over-cap standby VM (leak loop backstops next pass)",
				"bead", beadID, "runtime_id", runtimeID, "error", err)
			continue
		}
		// Close the resume-target bead as done — a standby that timed out is a
		// benign end-of-life, not a crash (microvmCloseState preserves an
		// already-failed prior state). Clear runtime_suspended_at for tidiness.
		if l.store != nil {
			if err := l.store.CloseBead(ctx, beadID, map[string]string{
				"agent_state":          microvmCloseState(c.AgentState),
				"runtime_state":        "TERMINATED",
				"runtime_suspended_at": "",
				"close_reason":         "microvm_standby_cap: suspended past max-standby-age " + l.maxStandbyAge.String(),
			}); err != nil {
				l.logger.Warn("microvm GC: terminated over-cap standby VM but failed to close bead",
					"bead", beadID, "runtime_id", runtimeID, "error", err)
				// The VM is gone; the leak loop closes/reaps residue next pass.
				continue
			}
		}
		stats.StandbyReaped++
	}
}

// microvmCloseState maps a MicroVM agent's prior agent_state to the state to
// close its bead in. A crash/failure is preserved as failed; everything else is
// a benign "done" (the runtime finished or was cancelled), matching the stale
// reaper's reapCloseState discipline (ka-0000001336).
func microvmCloseState(prior string) string {
	switch prior {
	case "failed", "rate_limited", "budget_exceeded":
		return prior
	default:
		return "done"
	}
}
