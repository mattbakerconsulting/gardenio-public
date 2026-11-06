package microvm

// rehydrate.go — the MicroVM resume rehydration correctness boundary (step 5,
// plan ka-0000004518 / epic ka-0000004689; task ka-0000001608). This is the
// MicroVM analog of the gVisor fail-closed thaw-epoch rendezvous
// (podmanager/freezethaw_rehydrate.go, design ka-0000001515) — but the mechanism
// differs fundamentally because the topology is different. See
// docs/microvm-resume-rehydration.md for the full design.
//
// # Why this is NOT the gVisor rendezvous
//
// The gVisor thaw resumes a FROZEN process tree: main() never re-runs, so every
// in-RAM credential and long-lived TCP 4-tuple is stale. The controller pushes a
// marker onto a hostPath the in-pod watchers poll and reads their acks back over
// the same hostPath (NodeExecutor). None of that is possible for a MicroVM:
//
//   - ResumeMicrovm is IDENTIFIER-ONLY (no payload) — the controller cannot
//     inject a marker/epoch into the VM on resume.
//   - MicroVMs are EGRESS-ONLY (only INTERNET_EGRESS) — no hostPath, no
//     NodeExecutor, no dial-in.
//
// But the signal channel exists natively and is stronger than the marker: on
// ResumeMicrovm, AWS fires the in-VM `resume` lifecycle hook, which
// RUNTIME.start()s the agent — tearing down + restarting OpenZiti + the agent
// process and RE-READING all secrets fresh from Secrets Manager
// (images/microvm-agent/entrypoint.py). So MicroVM rehydration is a fresh
// warm-boot, not a stale-RAM poison-and-reconnect: the resume hook already IS
// the "re-credential + reconnect" signal, and there is no stale RAM to poison.
//
// # The correctness boundary this file owns
//
// What the controller cannot assume is that the fresh boot SUCCEEDED (the resume
// hook can 503, the overlay can fail to re-establish, or the Ziti identity lease
// can have expired during standby). So after backend.Resume, AwaitRehydration
// waits BOUNDED for the resumed VM to PROVE LIFE via its own egress heartbeat
// reaching the beads daemon (presence last_seen advancing past the resume
// instant). That single egress ack proves identity + overlay + reachability at
// once — a fresh boot cannot partially satisfy them. On proof: agent_state
// flips to working (the VM is live; the user's message may flow). On timeout:
// FAIL CLOSED — terminate the un-rehydratable VM and stamp resume_failed, so the
// agent never serves the user on a half-rehydrated identity and the next message
// cold-starts fresh.

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Agent-state values owned by the resume rehydration boundary.
const (
	// AgentStateResuming is held for the duration of the rehydration rendezvous:
	// backend.Resume succeeded and the VM is coming back, but it has NOT yet
	// proven life, so it is not yet servable. Deliberately distinct from
	// "working" (live) and from "stopping" (the suspended-awaiting-resume signal
	// the bridge's resume-in-place guard keys on) so a concurrent message never
	// re-resumes a VM already mid-rehydration.
	AgentStateResuming = "resuming"

	// AgentStateWorking is the live state: proof-of-life arrived within the
	// deadline, so the resumed VM re-established its identity + overlay and may
	// serve the user.
	AgentStateWorking = "working"

	// AgentStateResumeFailed is the fail-closed terminal-ish state: proof-of-life
	// never arrived, so the VM was terminated and its per-run material revoked.
	// Not "stopping", so the bridge falls the next message through to a normal
	// cold respawn; the step-4 reconciler also converges it (terminal → close).
	AgentStateResumeFailed = "resume_failed"
)

// DefaultResumeRehydrateDeadline bounds the proof-of-life wait. A MicroVM resume
// restarts the ENTIRE userspace (process spawn + OpenZiti proxy re-tunnel +
// Secrets Manager reads + STS + first heartbeat over the overlay), so it wants a
// more generous budget than the gVisor in-process 10s reconnect. The VM disk
// survives suspend so there is no cold repo re-clone (the dominant gVisor cost).
// Comfortably under the 15-min presence reaper.
const DefaultResumeRehydrateDeadline = 90 * time.Second

// resumeRehydratePollInterval is how often AwaitRehydration re-reads the
// proof-of-life signal. Matches the gVisor thawepoch poll cadence (~1s): fast
// enough that a healthy resume releases promptly, cheap enough for a 90s window.
const resumeRehydratePollInterval = time.Second

// LivenessProber reports whether a MicroVM agent has PROVEN LIFE since a given
// instant — i.e. its own egress heartbeat has re-crossed the OpenZiti overlay to
// the beads daemon and advanced its presence last_seen past `since`. This is the
// M1 proof-of-life ack: because the heartbeat can only egress over a
// freshly-enrolled identity + fresh NATS token + live overlay, its arrival
// proves the resumed VM re-established ALL of them at once.
//
// It is an interface (not a direct beads-daemon dependency) for the same reason
// AgentBeadView is: this package stays free of the beads-daemon/HTTP graph. The
// controller wires a presence-roster-backed adapter; tests supply a fake.
type LivenessProber interface {
	// ProvenLiveSince reports whether agentName has been seen (presence
	// last_seen) at or after `since`. A read error is returned so the caller can
	// distinguish "not yet proven" (retry under the deadline) from a transient
	// failure (also retryable, but logged). agentName is the VM's agent NAME
	// (the presence-tracker actor key), not the bead ID.
	ProvenLiveSince(ctx context.Context, agentName string, since time.Time) (bool, error)
}

// rehydrationState is the settled outcome of one resume rehydration rendezvous,
// tracked in the in-flight registry so the resume-status endpoint can report
// in-flight vs terminal and so a duplicate resume dedups onto the live one.
type rehydrationState struct {
	// done is closed when the rendezvous settles (working or resume_failed).
	done chan struct{}
	// released is true iff proof-of-life arrived within the deadline (working).
	// Only read after done is closed.
	released bool
}

// rehydrationRegistry tracks in-flight resume rehydrations per agent name, so a
// duplicate resume (a second message racing the first) attaches to the live
// rendezvous instead of starting a second, and the resume-status endpoint can
// report progress. Mirrors the FreezeThawManager single-flight discipline
// (freezethaw.go thawSingleflight) but for the resume path.
type rehydrationRegistry struct {
	mu       sync.Mutex
	inflight map[string]*rehydrationState
}

func newRehydrationRegistry() *rehydrationRegistry {
	return &rehydrationRegistry{inflight: map[string]*rehydrationState{}}
}

// begin registers a new in-flight rehydration for agentName, returning the
// state and true. If one is already in flight it returns the existing state and
// false (the caller must NOT start a second rendezvous — attach to this one).
func (r *rehydrationRegistry) begin(agentName string) (*rehydrationState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.inflight[agentName]; ok {
		return st, false
	}
	st := &rehydrationState{done: make(chan struct{})}
	r.inflight[agentName] = st
	return st, true
}

// settle marks the rendezvous for agentName done with the given release verdict
// and removes it from the in-flight set. Idempotent-safe: a missing entry is a
// no-op (the caller always pairs begin+settle for the entry it created).
func (r *rehydrationRegistry) settle(agentName string, released bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.inflight[agentName]
	if !ok {
		return
	}
	st.released = released
	close(st.done)
	delete(r.inflight, agentName)
}

// inFlight reports whether a rehydration for agentName is currently in flight.
// The resume-status endpoint reports this; a `resuming` bead with an in-flight
// entry is (correctly) not re-resumed by a concurrent message.
func (r *rehydrationRegistry) inFlight(agentName string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.inflight[agentName]
	return ok
}

// RehydrationOutcome is the result of AwaitRehydration.
type RehydrationOutcome struct {
	// Released is true iff proof-of-life arrived within the deadline (the agent
	// was marked working). False means the rendezvous fail-closed (the VM was
	// terminated and the bead marked resume_failed).
	Released bool
	// Elapsed is how long the rendezvous took to settle.
	Elapsed time.Duration
	// Reason is a short human-readable settlement reason for logs/telemetry.
	Reason string
}

// SetLivenessProber wires the proof-of-life prober the resume rehydration
// rendezvous polls. Called by the controller after construction (the prober
// needs the daemon roster client the controller owns). Nil-safe; when unset,
// AwaitRehydration cannot prove life and every resume fails closed under the
// deadline (safe: a cold-start is always correct). Tests assign a fake.
func (l *Lifecycle) SetLivenessProber(p LivenessProber) {
	if l != nil {
		l.prober = p
	}
}

// ResumeRehydrateEnabled reports whether the fail-closed rehydration boundary is
// active. When false, Resume keeps the step-3 behavior (stamp working
// immediately, no proof-of-life gate) — the documented rollback.
func (l *Lifecycle) ResumeRehydrateEnabled() bool {
	return l != nil && l.rehydrateEnabled
}

// resumeAgentName derives the presence-tracker actor key (the agent NAME) for a
// bead. The controller passes it explicitly on the resume path (it has the name
// from the bridge request); this fallback keeps AwaitRehydration usable from
// callers that only carry the view.
func resumeRehydrateDeadline(configured time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return DefaultResumeRehydrateDeadline
}

// AwaitRehydration is the controller-side fail-closed proof-of-life gate (the
// gVisor C5 analog, adapted to the egress-only MicroVM topology). It is invoked
// AFTER backend.Resume has succeeded and the bead has been stamped `resuming`
// (Resume does both). It polls the LivenessProber until the resumed VM's own
// egress heartbeat has re-crossed the overlay to beads (presence last_seen ≥
// resumeAt = M1), bounded by the deadline:
//
//   - proof within deadline → stamp agent_state=working (the VM is live; the
//     user's message may now flow to it). Returns Released=true.
//   - deadline exceeded → FAIL CLOSED (M2): terminate the un-rehydratable VM
//     (revoking its per-run secrets + Ziti identity leases via backend.Terminate)
//     and stamp agent_state=resume_failed, so the next message cold-starts a
//     fresh VM. Returns Released=false.
//
// agentName is the presence-tracker actor key (the VM's agent name); resumeAt is
// the instant backend.Resume returned (proof must post-date it, so a pre-resume
// heartbeat from before the suspend can never be mistaken for proof of the new
// boot). The rendezvous dedups per agentName via the in-flight registry: a
// duplicate resume attaches to the live rendezvous rather than starting a
// second. No-op success (Released=true) when the coordinator is unavailable or
// the gate is disabled — the caller already handled the working stamp in that
// case.
func (l *Lifecycle) AwaitRehydration(ctx context.Context, view AgentBeadView, agentName string, resumeAt time.Time) RehydrationOutcome {
	if !l.Available() || !l.rehydrateEnabled {
		// Gate off or backend unavailable: Resume already stamped working
		// (step-3 behavior). Nothing to await.
		return RehydrationOutcome{Released: true, Reason: "rehydration gate disabled"}
	}

	st, started := l.rehydration.begin(agentName)
	if !started {
		// A rehydration for this agent is already in flight — attach to it rather
		// than run a second gate against the same VM.
		select {
		case <-st.done:
			return RehydrationOutcome{Released: st.released, Reason: "attached to in-flight rehydration"}
		case <-ctx.Done():
			return RehydrationOutcome{Released: false, Reason: "context cancelled awaiting in-flight rehydration"}
		}
	}

	deadline := resumeRehydrateDeadline(l.rehydrateDeadline)
	start := l.now()
	outcome := l.awaitProofOfLife(ctx, view, agentName, resumeAt, deadline)
	outcome.Elapsed = l.now().Sub(start)
	l.rehydration.settle(agentName, outcome.Released)
	return outcome
}

// awaitProofOfLife runs the bounded poll loop and applies the terminal bead
// transition (working on proof, resume_failed + terminate on timeout). Split
// from AwaitRehydration so the registry begin/settle bookkeeping stays a thin
// wrapper.
func (l *Lifecycle) awaitProofOfLife(ctx context.Context, view AgentBeadView, agentName string, resumeAt time.Time, deadline time.Duration) RehydrationOutcome {
	l.logger.Info("microvm resume: awaiting proof-of-life rehydration",
		"bead", view.ID, "agent", agentName, "runtime_id", view.RuntimeID, "deadline", deadline)

	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	tick := time.NewTicker(resumeRehydratePollInterval)
	defer tick.Stop()

	// Evaluate immediately — a fast healthy resume can prove life before the
	// first tick.
	if l.proofSeen(dctx, agentName, resumeAt) {
		return l.markRehydrated(ctx, view, agentName)
	}
	for {
		select {
		case <-dctx.Done():
			if ctx.Err() != nil {
				// Parent cancellation (controller shutdown / leader loss), not a
				// deadline expiry. Do NOT fail-close-terminate the VM on a
				// controller roll — leave it for the next reconcile/resume to
				// settle. Report not-released without tearing the VM down.
				l.logger.Warn("microvm resume: rehydration aborted by controller shutdown; VM left for reconcile",
					"bead", view.ID, "agent", agentName, "runtime_id", view.RuntimeID)
				return RehydrationOutcome{Released: false, Reason: "controller shutdown"}
			}
			return l.failClosed(ctx, view, agentName, deadline)
		case <-tick.C:
			if l.proofSeen(dctx, agentName, resumeAt) {
				return l.markRehydrated(ctx, view, agentName)
			}
		}
	}
}

// proofSeen reads the LivenessProber once, treating a read error as "not yet
// proven" (retryable under the deadline) so a transient beads/roster blip never
// short-circuits the gate into a spurious fail-close mid-window.
func (l *Lifecycle) proofSeen(ctx context.Context, agentName string, resumeAt time.Time) bool {
	if l.prober == nil {
		return false
	}
	live, err := l.prober.ProvenLiveSince(ctx, agentName, resumeAt)
	if err != nil {
		l.logger.Debug("microvm resume: proof-of-life read failed (retrying under deadline)",
			"agent", agentName, "error", err)
		return false
	}
	return live
}

// markRehydrated is the RELEASE path: proof-of-life arrived, so stamp the bead
// working. The VM is live and the user's message may now flow to it.
func (l *Lifecycle) markRehydrated(ctx context.Context, view AgentBeadView, agentName string) RehydrationOutcome {
	l.logger.Info("microvm resume: proof-of-life confirmed; marking agent live",
		"bead", view.ID, "agent", agentName, "runtime_id", view.RuntimeID)
	if l.store != nil {
		if err := l.store.UpdateBeadFields(ctx, view.ID, map[string]string{
			"agent_state":   AgentStateWorking,
			"runtime_state": "RUNNING",
		}); err != nil {
			// Best-effort: the VM is live regardless. A stale `resuming` only
			// costs a redundant (idempotent) resume on the next message.
			l.logger.Warn("microvm resume: rehydrated but failed to stamp working state",
				"bead", view.ID, "agent", agentName, "error", err)
		}
	}
	return RehydrationOutcome{Released: true, Reason: "proof-of-life confirmed"}
}

// failClosed is the FAIL-CLOSED path (M2): proof-of-life never arrived within
// the deadline, so the resumed VM is un-rehydratable (resume hook 503, overlay
// re-establish failure, or an expired Ziti lease). Terminate the VM — revoking
// its per-run secrets + Ziti identity leases so nothing leaks — and stamp
// resume_failed so the next message cold-starts fresh. The agent is NEVER left
// live on a half-rehydrated identity.
func (l *Lifecycle) failClosed(ctx context.Context, view AgentBeadView, agentName string, deadline time.Duration) RehydrationOutcome {
	l.logger.Error("microvm resume: proof-of-life NOT confirmed within deadline; failing closed (terminating VM, cold-start next message)",
		"bead", view.ID, "agent", agentName, "runtime_id", view.RuntimeID, "deadline", deadline)

	// Terminate on a fresh context: the deadline ctx is expired, and teardown
	// must run even though the wait budget is spent (mirrors the gVisor
	// freshcontext discipline in freezethaw_rehydrate.go graceWait).
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), awsOpTimeout+resumeRehydratePollInterval)
	defer cancel()
	if err := l.Terminate(tctx, view, "resume rehydration failed: no proof-of-life"); err != nil {
		// The VM may linger; leak GC + the broker TTL reaper backstop it. Still
		// mark the bead resume_failed so the agent is never treated as live.
		l.logger.Warn("microvm resume: fail-closed terminate errored (leak GC backstops)",
			"bead", view.ID, "agent", agentName, "runtime_id", view.RuntimeID, "error", err)
	}
	if l.store != nil {
		if err := l.store.UpdateBeadFields(tctx, view.ID, map[string]string{
			"agent_state":   AgentStateResumeFailed,
			"runtime_state": "TERMINATED",
		}); err != nil {
			l.logger.Warn("microvm resume: failed to stamp resume_failed state",
				"bead", view.ID, "agent", agentName, "error", err)
		}
	}
	return RehydrationOutcome{Released: false, Reason: fmt.Sprintf("no proof-of-life within %s", deadline)}
}

// IsRehydrating reports whether a resume rehydration for agentName is in flight.
// The resume-status endpoint consults it (mirrors freezeThawLifecycle.isThawing
// for the gVisor path). Safe on a nil/unavailable coordinator → false.
func (l *Lifecycle) IsRehydrating(agentName string) bool {
	if l == nil || l.rehydration == nil {
		return false
	}
	return l.rehydration.inFlight(agentName)
}
